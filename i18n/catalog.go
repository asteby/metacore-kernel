package i18n

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Catalog is an in-memory Translator built from plain message maps
// (lang → key → text). It is the kernel's batteries-included bundle: apps and
// addons declare their strings next to the code (or embed JSON/YAML files, see
// LoadCatalogFS) and layer the result over a host bundle with Compose.
//
// Lookup walks a fallback chain. For a request in "es-MX" with the default
// fallback the chain is es-MX → es → en: the exact tag, then each shorter
// prefix of the tag, then the catalog's fallback languages (each expanded the
// same way). Tags are matched case-insensitively and "_" equals "-".
//
// Messages support simple named interpolation: "Hola {name}" rendered with
// Translate(ctx, key, map[string]any{"name": "Ana"}) or with key/value pairs
// Translate(ctx, key, "name", "Ana"). Placeholders without an argument are
// left as written. There are no plural rules.
//
// A Catalog is safe for concurrent use; Add/Merge may run while requests are
// being translated.
type Catalog struct {
	mu       sync.RWMutex
	msgs     map[string]map[string]string // normalized lang → key → text
	fallback []string                     // normalized fallback languages
}

// DefaultFallbackLanguage is the last language a Catalog tries when the
// request's own chain has no entry for a key.
const DefaultFallbackLanguage = "en"

// NewCatalog builds a Catalog from lang → key → text. The map is copied. The
// fallback chain ends in DefaultFallbackLanguage ("en"); change it with
// WithFallback.
func NewCatalog(msgs map[string]map[string]string) *Catalog {
	c := &Catalog{
		msgs:     map[string]map[string]string{},
		fallback: []string{DefaultFallbackLanguage},
	}
	c.Merge(msgs)
	return c
}

// WithFallback replaces the languages tried after the request's own chain, in
// order (e.g. WithFallback("es", "en") for a Spanish-first app). Passing none
// disables the fallback: only the request language and its prefixes are tried.
// Returns c for chaining.
func (c *Catalog) WithFallback(langs ...string) *Catalog {
	fb := make([]string, 0, len(langs))
	for _, l := range langs {
		if n := normalizeLang(l); n != "" {
			fb = append(fb, n)
		}
	}
	c.mu.Lock()
	c.fallback = fb
	c.mu.Unlock()
	return c
}

// Add merges one language's messages. A key already present is overwritten.
// Empty languages and keys are ignored. Returns c for chaining.
func (c *Catalog) Add(lang string, msgs map[string]string) *Catalog {
	l := normalizeLang(lang)
	if l == "" || len(msgs) == 0 {
		return c
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	dst := c.msgs[l]
	if dst == nil {
		dst = make(map[string]string, len(msgs))
		c.msgs[l] = dst
	}
	for k, v := range msgs {
		if k != "" {
			dst[k] = v
		}
	}
	return c
}

// Merge adds every language of msgs (see Add). Returns c for chaining.
func (c *Catalog) Merge(msgs map[string]map[string]string) *Catalog {
	for lang, kv := range msgs {
		c.Add(lang, kv)
	}
	return c
}

// Messages returns a copy of the catalog as lang → key → text, with the
// normalized (lower-case) language tags.
func (c *Catalog) Messages() map[string]map[string]string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]map[string]string, len(c.msgs))
	for l, kv := range c.msgs {
		cp := make(map[string]string, len(kv))
		for k, v := range kv {
			cp[k] = v
		}
		out[l] = cp
	}
	return out
}

// Languages returns the normalized language tags that hold at least one
// message, sorted.
func (c *Catalog) Languages() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.msgs))
	for l, kv := range c.msgs {
		if len(kv) > 0 {
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

// Len returns the total number of messages across languages.
func (c *Catalog) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	n := 0
	for _, kv := range c.msgs {
		n += len(kv)
	}
	return n
}

// Lookup returns the raw (uninterpolated) message for key in lang, walking
// the fallback chain. ok is false when no language in the chain has the key.
func (c *Catalog) Lookup(lang, key string) (string, bool) {
	if c == nil || key == "" {
		return "", false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, l := range LanguageChain(lang, c.fallback...) {
		if v, ok := c.msgs[l][key]; ok {
			return v, true
		}
	}
	return "", false
}

// Translate implements Translator: it looks key up for the language stored
// in ctx (WithLanguage) and interpolates args. A missing key returns key
// unchanged, per the Translator contract.
func (c *Catalog) Translate(ctx context.Context, key string, args ...any) string {
	v, ok := c.Lookup(LanguageFromContext(ctx), key)
	if !ok {
		return key
	}
	return Interpolate(v, args...)
}

// LanguageChain returns the normalized lookup order for lang: the tag, each
// shorter prefix ("zh-hant-tw" → "zh-hant" → "zh"), then every fallback
// language expanded the same way. Duplicates are dropped.
func LanguageChain(lang string, fallback ...string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(tag string) {
		for t := normalizeLang(tag); t != ""; {
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
			i := strings.LastIndexByte(t, '-')
			if i <= 0 {
				break
			}
			t = t[:i]
		}
	}
	add(lang)
	for _, f := range fallback {
		add(f)
	}
	return out
}

// normalizeLang lower-cases a BCP-47 tag and turns "_" into "-".
func normalizeLang(lang string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(lang), "_", "-"))
}

// Interpolate replaces {name} placeholders in msg. args is either a single
// map (map[string]any or map[string]string) or alternating name/value pairs
// ("name", "Ana", "count", 3). Placeholders with no matching argument are
// left untouched; with no args msg is returned as is.
func Interpolate(msg string, args ...any) string {
	if len(args) == 0 || !strings.Contains(msg, "{") {
		return msg
	}
	vals := map[string]string{}
	switch m := args[0].(type) {
	case map[string]any:
		for k, v := range m {
			vals[k] = fmt.Sprint(v)
		}
	case map[string]string:
		for k, v := range m {
			vals[k] = v
		}
	default:
		for i := 0; i+1 < len(args); i += 2 {
			if k, ok := args[i].(string); ok {
				vals[k] = fmt.Sprint(args[i+1])
			}
		}
	}
	if len(vals) == 0 {
		return msg
	}
	var b strings.Builder
	b.Grow(len(msg))
	for i := 0; i < len(msg); {
		if msg[i] == '{' {
			if j := strings.IndexByte(msg[i+1:], '}'); j >= 0 {
				name := msg[i+1 : i+1+j]
				if v, ok := vals[name]; ok {
					b.WriteString(v)
					i += j + 2
					continue
				}
			}
		}
		b.WriteByte(msg[i])
		i++
	}
	return b.String()
}

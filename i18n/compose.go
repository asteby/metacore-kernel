package i18n

import (
	"context"
	"strings"
)

// Compose layers translators: for each key the first translator whose
// result is a hit wins. A result counts as a miss when it equals the key
// (the Translator contract for "not found") or is empty. nil translators
// are skipped. When every layer misses the key is returned unchanged.
//
// Put the most specific layer first, e.g. an app's own catalog over a
// shared host bundle:
//
//	tr := i18n.Compose(appCatalog, hostBundle)
func Compose(translators ...Translator) Translator {
	layers := make([]Translator, 0, len(translators))
	for _, t := range translators {
		if t != nil {
			layers = append(layers, t)
		}
	}
	return composed(layers)
}

type composed []Translator

func (c composed) Translate(ctx context.Context, key string, args ...any) string {
	for _, t := range c {
		if v := t.Translate(ctx, key, args...); v != "" && v != key {
			return v
		}
	}
	return key
}

// HumanizeMissing wraps tr so a key it cannot translate becomes a readable
// label instead of the raw key: Humanize(key, lang) for the language in ctx
// ("models.branches.modal.fields.postal_code" → "Código postal" in Spanish,
// "Postal code" otherwise). A nil tr humanizes every key. Keys that do not
// look like dotted/snake identifiers (containing spaces) are returned as is.
func HumanizeMissing(tr Translator) Translator {
	return TranslatorFunc(func(ctx context.Context, key string, args ...any) string {
		if tr != nil {
			if v := tr.Translate(ctx, key, args...); v != "" && v != key {
				return v
			}
		}
		if key == "" || strings.ContainsAny(key, " \t\n") {
			return key
		}
		return Humanize(key, LanguageFromContext(ctx))
	})
}

// ScopeModelMessages expands the model-relative keys of msgs (lang → key →
// text) into full metadata keys: a key that does not already start with
// prefix becomes prefix + model + "." + key, so "modal.fields.name" declared
// by the "branches" model becomes "models.branches.modal.fields.name". An
// empty prefix means DefaultModelKeyPrefix ("models."). The input is not
// modified.
func ScopeModelMessages(prefix, model string, msgs map[string]map[string]string) map[string]map[string]string {
	if prefix == "" {
		prefix = DefaultModelKeyPrefix
	}
	out := make(map[string]map[string]string, len(msgs))
	for lang, kv := range msgs {
		dst := make(map[string]string, len(kv))
		for k, v := range kv {
			if k == "" {
				continue
			}
			if !strings.HasPrefix(k, prefix) && model != "" {
				k = prefix + model + "." + k
			}
			dst[k] = v
		}
		out[lang] = dst
	}
	return out
}

// DefaultModelKeyPrefix is the namespace of model metadata keys
// ("models.<model>.table.title"). It mirrors metadata.DefaultI18nKeyPrefix.
const DefaultModelKeyPrefix = "models."

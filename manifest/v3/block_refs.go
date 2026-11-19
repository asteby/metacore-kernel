package v3

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
)

// Named, reusable UI blocks (contributions.blocks[]).
//
// A declarative block (a map, a live list, a health strip...) is DEFINED ONCE
// in contributions.blocks[] and REFERENCED from every surface that shows it: a
// sidebar page body, a dashboard widget (DashboardWidget.Block) or another
// named block. The reference node is
//
//	{"type": "ref", "ref": "<key>" | "<addon>.<key>"}
//
// and exists ONLY in the manifest: ExpandBlockRefs replaces it with the
// referenced block BEFORE genui/v2 sees the document, so neither the genui
// schema nor the SDK renderers nor the browser know about "ref". The host
// expands when it serves the catalog/pages; manifest validation uses the very
// same function, so what validates is exactly what is served.
//
// Expansion rules (one engine, no second implementation):
//   - a ref node is only allowed inside a "blocks" array (root, grid, tabs[],
//     section, ...) and carries nothing but "type" and "ref";
//   - the node is replaced by the referenced block's own top-level blocks
//     (spliced in place); its "data" sources are merged into the host
//     document's "data" (the same name with a different definition is an
//     error: names are shared, rename one);
//   - a block with a "permission" is spliced wrapped in a genui "section" that
//     carries that permission, so expansion never widens who sees it;
//   - refs nest (a block may reference another) up to MaxBlockRefDepth, and a
//     cycle is an error;
//   - "<addon>.<key>" requires <addon> in compatibility.requires. Its
//     definition lives in another manifest, so it is expanded only when the
//     caller supplies that catalog (ForeignBlocks, already expanded); the
//     validator checks the addon is required and substitutes a placeholder.
const (
	// MaxContributionBlocks caps the named blocks one addon declares.
	MaxContributionBlocks = 64
	// MaxBlockRefDepth caps the nesting of refs (a block referencing a block
	// referencing a block ...). 3 allows page -> composite -> leaf.
	MaxBlockRefDepth = 3
	// MaxBlockUIBytes caps one named block body (same tile budget as a widget).
	MaxBlockUIBytes = MaxWidgetUIBytes
)

var blockRefRe = regexp.MustCompile(`^([a-z][a-z0-9_-]*\.)?[a-z][a-z0-9_]*$`)

// BlockDef is one named, reusable block (contributions.blocks[]).
type BlockDef struct {
	// Key is the block's id, unique within the addon, snake_case. Another
	// addon references it as "<addon>.<key>". Required.
	Key string `json:"key"`
	// Title is an optional i18n key describing the block (catalogs, tooling).
	Title string `json:"title,omitempty"`
	// Permission optionally gates the block wherever it is referenced: the
	// expansion wraps it in a section carrying this permission.
	Permission string `json:"permission,omitempty"`
	// UI is the genui/v2 document of the block (same contract as PageDef.UI,
	// i18n:"keys", at most MaxBlockUIBytes). Its top-level blocks are what a
	// ref splices in; its data sources travel with them. Required.
	UI json.RawMessage `json:"ui"`
}

// ForeignBlocks is the catalog of blocks of OTHER addons a host knows, keyed by
// addon key then block key. Entries must already be expanded (their own refs
// resolved) by the host that owns them.
type ForeignBlocks map[string]map[string]BlockDef

// blockPlaceholder stands in for a foreign ref during validation (its
// definition is not in this manifest) so the rest of the document still
// validates.
var blockPlaceholder = map[string]any{"type": "text", "text": map[string]any{"$t": "ref.placeholder"}}

type blockExpander struct {
	self        string
	requires    map[string]struct{}
	own         map[string]BlockDef
	foreign     ForeignBlocks
	placeholder bool // validation: foreign refs become a placeholder
	unresolved  bool // a foreign ref was left in place (no catalog)
	seen        int  // ref nodes met (lets doc() return the input untouched)
	errs        []string
}

func newBlockExpander(m *Manifest, foreign ForeignBlocks, placeholder bool) *blockExpander {
	x := &blockExpander{
		self:        m.Metadata.Key,
		requires:    map[string]struct{}{},
		own:         map[string]BlockDef{},
		foreign:     foreign,
		placeholder: placeholder,
	}
	for _, r := range m.Compatibility.Requires {
		x.requires[r.Key] = struct{}{}
	}
	if m.Contributions != nil {
		for _, b := range m.Contributions.Blocks {
			if _, dup := x.own[b.Key]; !dup {
				x.own[b.Key] = b
			}
		}
	}
	return x
}

// ExpandBlockRefs returns a copy of m with every block reference resolved:
// ref nodes in page bodies and in named blocks are spliced, and a widget with
// block:"<ref>" gets that block as its ui (and block cleared). m is not
// modified. foreign may be nil; then refs to other addons are left as they
// are (and the widget keeps its block). It is idempotent: expanding an
// expanded manifest returns an equal one. The error lists every problem
// (unknown ref, cycle, depth, addon not required, data name clash).
func ExpandBlockRefs(m *Manifest, foreign ForeignBlocks) (*Manifest, error) {
	em, errs := expandBlockRefs(m, foreign, false)
	if len(errs) > 0 {
		return em, errors.New(strings.Join(errs, "; "))
	}
	return em, nil
}

func expandBlockRefs(m *Manifest, foreign ForeignBlocks, placeholder bool) (*Manifest, []string) {
	if m == nil || m.Contributions == nil {
		return m, nil
	}
	x := newBlockExpander(m, foreign, placeholder)
	em := *m
	c := *m.Contributions
	em.Contributions = &c

	c.Blocks = append([]BlockDef(nil), m.Contributions.Blocks...)
	for i := range c.Blocks {
		id := x.self + "." + c.Blocks[i].Key
		c.Blocks[i].UI = x.doc(fmt.Sprintf("contributions.blocks[%s].ui", c.Blocks[i].Key), c.Blocks[i].UI, []string{id})
	}
	c.Pages = append([]PageDef(nil), m.Contributions.Pages...)
	for i := range c.Pages {
		c.Pages[i].UI = x.doc(fmt.Sprintf("contributions.pages[%s].ui", c.Pages[i].Key), c.Pages[i].UI, nil)
	}
	c.Dashboard = append([]DashboardWidget(nil), m.Contributions.Dashboard...)
	for i := range c.Dashboard {
		w := &c.Dashboard[i]
		where := fmt.Sprintf("contributions.dashboard[%s]", w.Key)
		switch {
		case w.Kind != "block":
		case w.Block != "" && !hasJSON(w.UI):
			// The widget IS a reference: expand a one-node document.
			syn, _ := json.Marshal(map[string]any{
				"version": "genui/v2", "i18n": "keys",
				"blocks": []any{map[string]any{"type": "ref", "ref": w.Block}},
			})
			x.unresolved = false
			out := x.doc(where+".block", syn, nil)
			if !x.unresolved {
				w.UI, w.Block = out, ""
			}
			x.unresolved = false
		case hasJSON(w.UI) && w.Block == "":
			w.UI = x.doc(where+".ui", w.UI, nil)
		}
	}
	return &em, x.errs
}

// doc expands the refs of one genui document, returning raw untouched when it
// has none (or is not a JSON object: genui reports that itself).
func (x *blockExpander) doc(where string, raw json.RawMessage, stack []string) json.RawMessage {
	if !hasJSON(raw) {
		return raw
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return raw
	}
	root, ok := v.(map[string]any)
	if !ok {
		return raw
	}
	before := x.seen
	x.walk(where, root, root, stack)
	if x.seen == before {
		return raw
	}
	out, err := json.Marshal(root)
	if err != nil {
		return raw
	}
	return out
}

func (x *blockExpander) walk(where string, root map[string]any, node any, stack []string) {
	switch t := node.(type) {
	case map[string]any:
		for k, v := range t {
			if arr, ok := v.([]any); ok && k == "blocks" {
				t[k] = x.blocks(where, root, arr, stack)
				continue
			}
			x.walk(where, root, v, stack)
		}
	case []any:
		for _, it := range t {
			x.walk(where, root, it, stack)
		}
	}
}

func (x *blockExpander) blocks(where string, root map[string]any, arr []any, stack []string) []any {
	out := make([]any, 0, len(arr))
	for _, it := range arr {
		if n, ok := it.(map[string]any); ok && n["type"] == "ref" {
			x.seen++
			out = append(out, x.resolve(where, root, n, stack)...)
			continue
		}
		x.walk(where, root, it, stack)
		out = append(out, it)
	}
	return out
}

func (x *blockExpander) resolve(where string, root, node map[string]any, stack []string) []any {
	for k := range node {
		if k != "type" && k != "ref" {
			x.errs = append(x.errs, fmt.Sprintf("%s: a ref node only accepts type and ref (found %q)", where, k))
			return nil
		}
	}
	ref, _ := node["ref"].(string)
	if !blockRefRe.MatchString(ref) {
		x.errs = append(x.errs, fmt.Sprintf("%s: ref %q must be \"<key>\" or \"<addon>.<key>\" (snake_case key)", where, ref))
		return nil
	}
	addon, key := "", ref
	if i := strings.Index(ref, "."); i >= 0 {
		addon, key = ref[:i], ref[i+1:]
	}

	var def BlockDef
	id := x.self + "." + key
	if addon == "" || addon == x.self {
		d, ok := x.own[key]
		if !ok {
			x.errs = append(x.errs, fmt.Sprintf("%s: ref %q does not match any contributions.blocks[].key", where, ref))
			return nil
		}
		def = d
	} else {
		if _, ok := x.requires[addon]; !ok {
			x.errs = append(x.errs, fmt.Sprintf("%s: ref %q points to addon %q, which is not in compatibility.requires", where, ref, addon))
			return nil
		}
		if x.foreign == nil {
			if x.placeholder {
				return []any{blockPlaceholder}
			}
			x.unresolved = true
			return []any{node}
		}
		d, ok := x.foreign[addon][key]
		if !ok {
			x.errs = append(x.errs, fmt.Sprintf("%s: ref %q: addon %q declares no block %q", where, ref, addon, key))
			return nil
		}
		def, id = d, ref
	}

	for _, s := range stack {
		if s == id {
			x.errs = append(x.errs, fmt.Sprintf("%s: ref cycle %s -> %s", where, strings.Join(stack, " -> "), id))
			return nil
		}
	}
	if len(stack) >= MaxBlockRefDepth {
		x.errs = append(x.errs, fmt.Sprintf("%s: ref %q nests deeper than %d levels", where, ref, MaxBlockRefDepth))
		return nil
	}

	body := def.UI
	if addon == "" || addon == x.self {
		body = x.doc(where, def.UI, append(append([]string(nil), stack...), id))
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var sub map[string]any
	if err := dec.Decode(&sub); err != nil {
		x.errs = append(x.errs, fmt.Sprintf("%s: block %q has no readable ui document", where, ref))
		return nil
	}
	if data, ok := sub["data"].(map[string]any); ok {
		rd, _ := root["data"].(map[string]any)
		if rd == nil {
			rd = map[string]any{}
			root["data"] = rd
		}
		for name, src := range data {
			if prev, exists := rd[name]; exists && !reflect.DeepEqual(prev, src) {
				x.errs = append(x.errs, fmt.Sprintf("%s: block %q defines data source %q differently from the document; rename one", where, ref, name))
				continue
			}
			rd[name] = src
		}
	}
	blocks, _ := sub["blocks"].([]any)
	if def.Permission != "" && len(blocks) > 0 {
		return []any{map[string]any{"type": "section", "permission": def.Permission, "blocks": blocks}}
	}
	return blocks
}

// validateBlockRefs enforces the named-block contract on the ORIGINAL manifest
// and returns the expansion the rest of the validation runs on: block
// definitions (key, permission, ui), the one-origin rule of block widgets
// (exactly one of ui|block) and every expansion error.
func validateBlockRefs(m *Manifest) (*Manifest, []string) {
	em, errs := expandBlockRefs(m, nil, true)
	if m.Contributions == nil {
		return em, errs
	}
	blocks := m.Contributions.Blocks
	if len(blocks) > MaxContributionBlocks {
		errs = append(errs, fmt.Sprintf("contributions.blocks has %d entries; the maximum is %d", len(blocks), MaxContributionBlocks))
	}
	seen := map[string]struct{}{}
	for i, b := range blocks {
		id := b.Key
		if id == "" {
			id = fmt.Sprintf("#%d", i)
		}
		where := fmt.Sprintf("contributions.blocks[%s]", id)
		switch {
		case b.Key == "":
			errs = append(errs, where+".key is required")
		case !pageKeyRe.MatchString(b.Key):
			errs = append(errs, fmt.Sprintf("%s.key %q must be snake_case (^[a-z][a-z0-9_]*$)", where, b.Key))
		default:
			if _, dup := seen[b.Key]; dup {
				errs = append(errs, fmt.Sprintf("%s.key %q is duplicated within the addon", where, b.Key))
			}
			seen[b.Key] = struct{}{}
		}
		if b.Permission != "" && !PermissionKeyRe.MatchString(b.Permission) {
			errs = append(errs, fmt.Sprintf("%s.permission %q is not a valid permission key (want e.g. \"<addon>.<resource>.<action>\")", where, b.Permission))
		}
		if len(b.UI) > MaxBlockUIBytes {
			errs = append(errs, fmt.Sprintf("%s.ui is %d bytes; the maximum is %d", where, len(b.UI), MaxBlockUIBytes))
			continue
		}
		errs = append(errs, validateUIDoc(where, em.Contributions.Blocks[i].UI, MaxPageUIBytes)...)
	}
	for i, w := range m.Contributions.Dashboard {
		id := w.Key
		if id == "" {
			id = fmt.Sprintf("#%d", i)
		}
		where := fmt.Sprintf("contributions.dashboard[%s]", id)
		switch {
		case w.Kind == "block" && w.Block == "" && !hasJSON(w.UI):
			errs = append(errs, where+" is kind=block and needs exactly one of ui (inline genui/v2 document) or block (a contributions.blocks[] reference)")
		case w.Block == "":
		case w.Kind != "block":
			errs = append(errs, fmt.Sprintf("%s.block is only allowed for kind=block (got kind=%s)", where, w.Kind))
		case hasJSON(w.UI):
			errs = append(errs, where+" declares both ui and block; a block widget has exactly one origin (inline ui OR a reference to contributions.blocks[])")
		}
	}
	return em, errs
}

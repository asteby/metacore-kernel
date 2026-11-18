package genuiv2

import (
	"sort"

	v1 "github.com/asteby/metacore-kernel/genui"
)

// V1BlockTypes are the blocks that exist with the same shape in genui/v1.
var V1BlockTypes = []string{"text", "kpi", "card", "table", "links", "timeline", "chart", "confirm"}

// keepVerbatim: scalars where null/"" carry meaning; never pruned by Upgrade.
var keepVerbatim = map[string]bool{"rows": true, "params": true}

func dropEmpty(v any) any {
	switch t := v.(type) {
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = dropEmpty(x)
		}
		return out
	case obj:
		out := obj{}
		for k, x := range t {
			if forbidden(k) {
				continue
			}
			if x == nil || x == "" {
				continue
			}
			if l, ok := x.([]any); ok && len(l) == 0 {
				continue
			}
			if keepVerbatim[k] {
				out[k] = x
			} else {
				out[k] = dropEmpty(x)
			}
		}
		return out
	}
	return v
}

func upgradeRaw(spec obj) obj {
	body := dropEmpty(spec).(obj)
	out := obj{"version": Version}
	for k, x := range body {
		if k != "version" {
			out[k] = x
		}
	}
	return out
}

// Upgrade turns an already validated genui/v1 document (generic JSON) into
// genui/v2: the 8 v1 blocks exist in v2 with the same shape, so only
// `version` changes and the empty fields v1 tolerated are dropped. It does not
// validate the result; use ParseAny for untrusted input.
func Upgrade(spec map[string]any) Doc { return upgradeRaw(spec) }

// UpgradeV1 upgrades a typed v1 spec.
func UpgradeV1(s *v1.Spec) (Doc, error) {
	raw, err := marshal(s)
	if err != nil {
		return nil, err
	}
	x, err := decodeText(raw)
	if err != nil {
		return nil, err
	}
	o, ok := normalize(x).(obj)
	if !ok {
		return nil, fail(CodeInvalidDocument, "", "spec v1 invalido")
	}
	return upgradeRaw(o), nil
}

// ParseResult is the outcome of ParseAny.
type ParseResult struct {
	Doc Doc
	// Source is the version the input declared: "genui/v1" or "genui/v2".
	Source string
}

// ParseAny is the single entry for untrusted specs of any version. A v1 goes
// through the v1 validator and, upgraded, through the strict v2 validator with
// the same options; the result is always a normalised v2 document.
func ParseAny(in any, opts Options) (ParseResult, error) {
	v, err := prepare(in)
	if err != nil {
		return ParseResult{}, err
	}
	if v["version"] == VersionV1 {
		_, v1err := v1.FromAny(v)
		up := upgradeRaw(v)
		if err := validatePrepared(up, opts); err != nil {
			return ParseResult{}, err
		}
		if v1err != nil {
			return ParseResult{}, fail(CodeInvalidDocument, "", "%s", v1err.Error())
		}
		return ParseResult{Doc: up, Source: VersionV1}, nil
	}
	if err := validatePrepared(v, opts); err != nil {
		return ParseResult{}, err
	}
	return ParseResult{Doc: v, Source: Version}, nil
}

// Caps are the client's capabilities (what its GenUIProvider registered).
type Caps struct {
	Versions []string `json:"versions"`
	// Blocks supported (v2 only). nil = every core block.
	Blocks []string `json:"blocks,omitempty"`
	// Verbs supported (v2 only; v1 confirm actions count as emit). nil = all.
	Verbs []string `json:"verbs,omitempty"`
}

// DowngradeReport counts what Downgrade changed.
type DowngradeReport struct {
	Replaced int `json:"replaced"`
	Dropped  int `json:"dropped"`
}

// DowngradeResult carries the degraded document: a v2 Doc, or a v1 document
// (generic JSON) when the client only speaks v1.
type DowngradeResult struct {
	Spec   map[string]any
	Report DowngradeReport
}

const emptyText = "Contenido no disponible en este cliente."

var (
	actionKeys = map[string]bool{"actions": true, "row_actions": true, "bulk_actions": true, "submit": true, "action": true}
	dataKeys   = map[string]bool{"rows": true, "inline": true, "data": true, "state": true, "props": true, "params": true, "values": true, "filters": true, "args": true}
)

func isActionObj(x any) bool {
	o, ok := x.(obj)
	if !ok {
		return false
	}
	_, isStr := o["do"].(string)
	return isStr
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func copyObj(o obj) obj {
	out := make(obj, len(o))
	for k, v := range o {
		out[k] = v
	}
	return out
}

// Downgrade degrades a v2 document to what a client can render. The input is
// validated first. A block the client does not support becomes its `fallback`
// (a text block), or is dropped without one; a container whose children are
// all dropped uses its own fallback; an action with an unsupported verb or
// pointing to a dropped overlay/form is dropped. A v1-only client gets a valid
// genui/v1 document. Without caps v1 is assumed. The result is revalidated.
func Downgrade(in Doc, caps *Caps) (DowngradeResult, error) {
	pre, err := Parse(in, Options{AllowExternalLinks: true})
	if err != nil {
		return DowngradeResult{}, err
	}
	spec := pre
	versions := []string{VersionV1}
	if caps != nil && len(caps.Versions) > 0 {
		versions = caps.Versions
	}
	wantsV2 := contains(versions, Version)
	if !wantsV2 && !contains(versions, VersionV1) {
		return DowngradeResult{}, fail(CodeUnsupportedVersion, "", "el cliente no soporta %v", versions)
	}
	report := DowngradeReport{}
	var blocks map[string]bool
	var verbs map[string]bool
	if caps != nil && caps.Blocks != nil {
		blocks = map[string]bool{}
		for _, b := range caps.Blocks {
			blocks[b] = true
		}
	}
	if caps != nil && caps.Verbs != nil {
		verbs = map[string]bool{}
		for _, b := range caps.Verbs {
			verbs[b] = true
		}
	}
	verbOk := func(v string) bool { return verbs == nil || verbs[v] }

	useFallback := func(b obj, out *[]obj) {
		fb, _ := b["fallback"].(string)
		if fb == "" {
			report.Dropped++
			return
		}
		t := obj{"type": "text", "text": fb}
		if wantsV2 {
			if id, ok := b["id"]; ok {
				t["id"] = id
			}
			if w, ok := b["when"]; ok {
				t["when"] = w
			}
		}
		report.Replaced++
		*out = append(*out, t)
	}

	var filterActions func(v any, key string, keep func(obj) bool) any
	filterActions = func(v any, key string, keep func(obj) bool) any {
		switch t := v.(type) {
		case []any:
			isActs := actionKeys[key]
			out := make([]any, 0, len(t))
			for _, x := range t {
				if xo, ok := x.(obj); ok && isActs {
					if _, hasID := xo["id"].(string); isActionObj(xo) || hasID {
						if !keep(xo) {
							report.Dropped++
							continue
						}
					}
				}
				out = append(out, filterActions(x, key, keep))
			}
			return out
		case obj:
			out := obj{}
			for k, x := range t {
				if dataKeys[k] {
					out[k] = x
					continue
				}
				if actionKeys[k] && isActionObj(x) && !keep(x.(obj)) {
					report.Dropped++
					continue
				}
				nx := filterActions(x, k, keep)
				if nl, ok := nx.([]any); ok && len(nl) == 0 && actionKeys[k] {
					if ol, ok := x.([]any); ok && len(ol) > 0 {
						continue
					}
				}
				out[k] = nx
			}
			return out
		}
		return v
	}
	verbKeep := func(a obj) bool {
		if isActionObj(a) {
			return verbOk(str(a["do"]))
		}
		return verbOk("emit")
	}
	v1Ok := func(b obj) obj {
		if has(b, "when") {
			return nil
		}
		rest := obj{}
		for k, x := range b {
			if k != "id" && k != "fallback" {
				rest[k] = x
			}
		}
		if !contains(V1BlockTypes, str(rest["type"])) {
			return nil
		}
		if _, err := v1.FromAny(obj{"version": VersionV1, "blocks": []any{rest}}); err != nil {
			return nil
		}
		return rest
	}

	var mapBlocks func(list []any, nested bool) []obj
	mapBlocks = func(list []any, nested bool) []obj {
		var out []obj
		for _, raw := range list {
			b := raw.(obj)
			typ := str(b["type"])
			supported := (blocks == nil || blocks[typ]) && !(typ == "confirm" && !verbOk("emit"))
			if wantsV2 && supported {
				nb := copyObj(b)
				if kids, ok := b["blocks"].([]any); ok {
					mk := mapBlocks(kids, true)
					if len(mk) == 0 {
						useFallback(b, &out)
						continue
					}
					nb["blocks"] = toAny(mk)
				}
				if tabs, ok := b["tabs"].([]any); ok {
					var kept []any
					for _, t := range tabs {
						to := copyObj(t.(obj))
						if _, ok := to["blocks"]; !ok { // opcion custom (component): no tiene hijos que degradar
							kept = append(kept, to)
							continue
						}
						mk := mapBlocks(to["blocks"].([]any), true)
						if len(mk) > 0 {
							to["blocks"] = toAny(mk)
							kept = append(kept, to)
						}
					}
					if len(kept) == 0 {
						useFallback(b, &out)
						continue
					}
					nb["tabs"] = kept
				}
				out = append(out, nb)
				continue
			}
			if !wantsV2 && !nested && supported {
				if r := v1Ok(filterActions(b, "", verbKeep).(obj)); r != nil {
					out = append(out, r)
					continue
				}
			}
			useFallback(b, &out)
		}
		return out
	}

	top := mapBlocks(spec["blocks"].([]any), false)
	if !wantsV2 && len(top) > 12 {
		report.Dropped += len(top) - 12
		top = top[:12]
	}
	if len(top) == 0 {
		top = []obj{{"type": "text", "text": emptyText, "tone": "muted"}}
	}

	if !wantsV2 {
		out := obj{"version": VersionV1, "blocks": toAny(top)}
		if t, ok := spec["title"].(string); ok && t != "" {
			out["title"] = t
		}
		if _, err := v1.FromAny(out); err != nil {
			return DowngradeResult{}, fail(CodeInvalidDocument, "", "%s", err.Error())
		}
		return DowngradeResult{Spec: out, Report: report}, nil
	}

	out := copyObj(spec)
	out["blocks"] = toAny(top)
	if ovs, ok := spec["overlays"].(obj); ok {
		ov := obj{}
		keys := make([]string, 0, len(ovs))
		for k := range ovs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			o := copyObj(ovs[k].(obj))
			kids := mapBlocks(o["blocks"].([]any), true)
			if len(kids) > 0 {
				o["blocks"] = toAny(kids)
				ov[k] = o
			}
		}
		if len(ov) > 0 {
			out["overlays"] = ov
		} else {
			delete(out, "overlays")
		}
	}
	liveForms := map[string]bool{}
	var collect func(v any)
	collect = func(v any) {
		switch t := v.(type) {
		case []any:
			for _, x := range t {
				collect(x)
			}
		case obj:
			if (t["type"] == "form" || t["type"] == "wizard") && str(t["id"]) != "" {
				liveForms[str(t["id"])] = true
			}
			for k, x := range t {
				if !dataKeys[k] {
					collect(x)
				}
			}
		}
	}
	collect(out["blocks"])
	collect(out["overlays"])
	live := asObj(out["overlays"])
	filtered := filterActions(out, "", func(a obj) bool {
		if !isActionObj(a) {
			return verbOk("emit")
		}
		verb := str(a["do"])
		if !verbOk(verb) {
			return false
		}
		switch {
		case verb == "open":
			return has(live, str(a["overlay"]))
		case verb == "close" && has(a, "overlay"):
			return has(live, str(a["overlay"]))
		case verb == "submit":
			return liveForms[str(a["form"])]
		}
		return true
	}).(obj)
	res, err := Parse(filtered, Options{AllowExternalLinks: true})
	if err != nil {
		return DowngradeResult{}, err
	}
	return DowngradeResult{Spec: res, Report: report}, nil
}

func toAny(l []obj) []any {
	out := make([]any, len(l))
	for i, x := range l {
		out[i] = x
	}
	return out
}

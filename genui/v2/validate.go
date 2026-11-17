// Package genuiv2 is the Go side of the genui/v2 contract (ADR 0001 of
// metacore-sdk). The JSON Schema in schema/genui-v2.schema.json is the single
// source of truth: this package interprets it directly (the same closed subset
// of JSON Schema as the TS validator) and adds, by hand, the few rules a
// schema cannot express (reference grammar, depth and totals, unique ids,
// overlay/form targets, link safety). The shared conformance vectors in
// testdata/conformance pin both implementations to the same behaviour.
//
// A document is plain JSON (map[string]any); there are no structs to drift.
package genuiv2

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	v1 "github.com/asteby/metacore-kernel/genui"
)

//go:embed schema/genui-v2.schema.json
var schemaJSON []byte

// SchemaJSON returns the embedded JSON Schema (for structured outputs, editors, hub).
func SchemaJSON() []byte { return append([]byte(nil), schemaJSON...) }

// Doc is a validated genui/v2 document.
type Doc = map[string]any

// Options tunes validation.
type Options struct {
	// AllowExternalLinks permits literal http(s) links. Off by default: only the chat turns it on.
	AllowExternalLinks bool
}

type obj = map[string]any

var (
	schemaOnce sync.Once
	rootSchema obj
	defs       obj
	blockTypes map[string]bool
)

func loadSchema() {
	schemaOnce.Do(func() {
		if err := json.Unmarshal(schemaJSON, &rootSchema); err != nil {
			panic("genui/v2: esquema embebido invalido: " + err.Error())
		}
		defs = rootSchema["$defs"].(obj)
		blockTypes = map[string]bool{}
		for name := range defs {
			if strings.HasPrefix(name, "block_") {
				blockTypes[strings.TrimPrefix(name, "block_")] = true
			}
		}
	})
}

func has(o obj, k string) bool { _, ok := o[k]; return ok }
func isObj(v any) bool         { _, ok := v.(obj); return ok }
func str(v any) string         { s, _ := v.(string); return s }

func kindOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case []any:
		return "array"
	case obj:
		return "object"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		return "number"
	}
	return "unknown"
}

func matchesType(t any, v any) bool {
	var types []any
	if l, ok := t.([]any); ok {
		types = l
	} else {
		types = []any{t}
	}
	k := kindOf(v)
	for _, x := range types {
		switch x {
		case "number", "integer":
			f, ok := v.(float64)
			if ok && !math.IsInf(f, 0) && !math.IsNaN(f) && (x == "number" || f == math.Trunc(f)) {
				return true
			}
		default:
			if x == k {
				return true
			}
		}
	}
	return false
}

var lastKeyRe = regexp.MustCompile(`([A-Za-z_]+)(\[\d+\])?$`)

func lastKey(path string) string {
	m := lastKeyRe.FindStringSubmatch(path)
	if m == nil {
		return ""
	}
	return m[1]
}

var limitByKey = map[string]string{
	"blocks": CodeLimitChildren, "tabs": CodeLimitTabs, "steps": CodeLimitSteps, "overlays": CodeLimitOverlays,
	"data": CodeLimitSources, "columns": CodeLimitColumns, "rows": CodeLimitRows, "inline": CodeLimitRows,
	"series": CodeLimitSeries, "fields": CodeLimitFields, "items": CodeLimitItems, "markdown": CodeLimitMarkdown,
	"actions": CodeLimitActions, "row_actions": CodeLimitActions, "bulk_actions": CodeLimitActions,
}

func limitFor(path, fallback string) string {
	if c, ok := limitByKey[lastKey(path)]; ok {
		return c
	}
	return fallback
}

func resolve(s obj) obj {
	cur := s
	for {
		r, ok := cur["$ref"].(string)
		if !ok || !strings.HasPrefix(r, "#/$defs/") {
			return cur
		}
		next, ok := defs[strings.TrimPrefix(r, "#/$defs/")].(obj)
		if !ok {
			panic("genui/v2: esquema: " + r + " no existe")
		}
		cur = next
	}
}

type ctx struct {
	doc        obj
	opts       Options
	blockCount int
	blockIDs   map[string]string
	idOrder    []string
	bindings   int
	actions    int
	cur        int
	submits    []submitRef
}

type submitRef struct{ form, path string }

// compareUTF8 orders by UTF-8 bytes (= code points); Go's string comparison does exactly that.
func sortedKeys(o obj, path string) []string {
	keys := make([]string, 0, len(o))
	for k := range o {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if path == "$" && has(o, "blocks") {
		out := keys[:0:0]
		for _, k := range keys {
			if k != "blocks" {
				out = append(out, k)
			}
		}
		return append(out, "blocks")
	}
	return keys
}

func asObj(v any) obj { o, _ := v.(obj); return o }

// check interprets the schema node against value. It panics with *Error on the first failure.
func (c *ctx) check(value any, schema obj, path string, depth int) {
	refName := ""
	if r, ok := schema["$ref"].(string); ok {
		refName = strings.TrimPrefix(r, "#/$defs/")
	}
	s := resolve(schema)
	if s["x-genui"] == "block" {
		c.checkBlock(value, path, depth)
		return
	}

	if vo, ok := value.(obj); ok && has(vo, "$ref") {
		_, hasEnum := s["enum"]
		_, hasConst := s["const"]
		t, hasType := s["type"]
		if hasEnum || hasConst || (hasType && !matchesType(t, obj{})) {
			panic(fail(CodeRefNotAllowed, path, "%s: $ref no permitido en esta posicion", path))
		}
	}
	if k, ok := s["const"]; ok && !jsonEqual(value, k) {
		panic(fail(CodeEnum, path, "%s: debe ser %v", path, k))
	}
	if en, ok := s["enum"].([]any); ok {
		found := false
		for _, e := range en {
			if jsonEqual(e, value) {
				found = true
				break
			}
		}
		if !found {
			panic(fail(CodeEnum, path, "%s: valor no permitido", path))
		}
	}

	if disc, ok := s["x-discriminator"].(string); ok {
		vo, isO := value.(obj)
		if !isO {
			panic(fail(CodeType, path, "%s: se esperaba object, llego %s", path, kindOf(value)))
		}
		if !has(vo, disc) {
			panic(fail(CodeRequired, path, "%s: falta %s", path, disc))
		}
		var alt obj
		for _, a := range s["oneOf"].([]any) {
			if cst, ok := asObj(resolve(a.(obj))["properties"])[disc]; ok {
				if cv, ok := cst.(obj)["const"]; ok && jsonEqual(cv, vo[disc]) {
					alt = a.(obj)
					break
				}
			}
		}
		if alt == nil {
			panic(fail(CodeEnum, path+"."+disc, "%s.%s: valor no permitido", path, disc))
		}
		c.check(value, alt, path, depth)
	} else if _, ok := s["anyOf"]; ok {
		c.checkAlternatives(value, s, s["anyOf"].([]any), true, path, depth)
	} else if _, ok := s["oneOf"]; ok {
		c.checkAlternatives(value, s, s["oneOf"].([]any), false, path, depth)
	}

	if t, ok := s["type"]; ok && !matchesType(t, value) {
		panic(fail(CodeType, path, "%s: se esperaba %v, llego %s", path, t, kindOf(value)))
	}
	switch v := value.(type) {
	case string:
		if np, _ := s["x-no-proto"].(bool); np && hasForbiddenSegment(v, ".:@") {
			panic(fail(CodeForbiddenKey, path, "%s: segmento prohibido", path))
		}
		if ml, ok := s["maxLength"].(float64); ok && utf8.RuneCountInString(v) > int(ml) {
			code, _ := s["x-error"].(string)
			if code == "" {
				code = CodeLimitLength
				if lastKey(path) == "markdown" {
					code = CodeLimitMarkdown
				}
			}
			panic(fail(code, path, "%s: excede %v caracteres", path, ml))
		}
		if p, ok := s["pattern"].(string); ok && !compilePattern(p).match(v) {
			code, _ := s["x-error"].(string)
			if code == "" {
				code = CodePattern
			}
			panic(fail(code, path, "%s: formato invalido", path))
		}
	case float64:
		mn, hasMn := s["minimum"].(float64)
		mx, hasMx := s["maximum"].(float64)
		if (hasMn && v < mn) || (hasMx && v > mx) {
			panic(fail(CodeType, path, "%s: fuera de rango", path))
		}
	case []any:
		if mx, ok := s["maxItems"].(float64); ok && len(v) > int(mx) {
			panic(fail(limitFor(path, CodeLimitItems), path, "%s: maximo %v elementos", path, mx))
		}
		if mn, ok := s["minItems"].(float64); ok && len(v) < int(mn) {
			panic(fail(CodeRequired, path, "%s: minimo %v elementos", path, mn))
		}
		if it, ok := s["items"].(obj); ok {
			for i, x := range v {
				c.check(x, it, fmt.Sprintf("%s[%d]", path, i), depth)
			}
		}
	case obj:
		c.checkObject(v, s, path, depth)
	}

	switch refName {
	case "action":
		c.actions++
		if c.actions > MaxActions {
			panic(fail(CodeLimitActions, path, "maximo %d acciones", MaxActions))
		}
		c.cur++
		c.actionRule(value.(obj), path)
	case "ref":
		c.bindings++
		if c.bindings > MaxBindings {
			panic(fail(CodeLimitBindings, path, "maximo %d vinculaciones", MaxBindings))
		}
	case "bindingPath":
		c.refRule(value.(string), path)
	case "source":
		name := strings.TrimPrefix(value.(string), "data.")
		d, ok := c.doc["data"].(obj)
		if !ok || !has(d, name) {
			panic(fail(CodeUnknownTarget, path, "%s: la fuente %v no esta declarada en data", path, value))
		}
	case "link":
		l := value.(string)
		if !strings.HasPrefix(l, "/") && !c.opts.AllowExternalLinks {
			panic(fail(CodeExternalLink, path, "%s: los enlaces externos estan apagados en esta superficie", path))
		}
		if !v1.SafeLink(l) {
			panic(fail(CodeUnsafeLink, path, "%s: enlace no seguro", path))
		}
	}
}

func (c *ctx) checkObject(value obj, s obj, path string, depth int) {
	keys := sortedKeys(value, path)
	if mx, ok := s["maxProperties"].(float64); ok && len(keys) > int(mx) {
		panic(fail(limitFor(path, CodeLimitProperties), path, "%s: demasiadas propiedades (max. %v)", path, mx))
	}
	if mn, ok := s["minProperties"].(float64); ok && len(keys) < int(mn) {
		panic(fail(CodeRequired, path, "%s: faltan propiedades", path))
	}
	if req, ok := s["required"].([]any); ok {
		for _, r := range req {
			if !has(value, r.(string)) {
				panic(fail(CodeRequired, path, "%s: falta %v", path, r))
			}
		}
	}
	props := asObj(s["properties"])
	addl, addlIsObj := s["additionalProperties"].(obj)
	pn := asObj(s["propertyNames"])
	for _, k := range keys {
		sub := path + "." + k
		_, declared := props[k]
		if strings.HasPrefix(k, "$") && !declared && addlIsObj {
			panic(fail(CodeRefNotAllowed, sub, "%s: clave $ no permitida en esta posicion", sub))
		}
		if np, _ := pn["x-no-proto"].(bool); np && hasForbiddenSegment(k, ".:@") {
			panic(fail(CodeForbiddenKey, sub, "%s: segmento prohibido", sub))
		}
		if p, ok := pn["pattern"].(string); ok && !compilePattern(p).match(k) {
			panic(fail(CodePattern, sub, "%s: nombre invalido", sub))
		}
		switch {
		case declared:
			c.check(value[k], props[k].(obj), sub, depth)
		case s["additionalProperties"] == false:
			panic(fail(CodeFieldNotAllowed, sub, "%s: campo no permitido", sub))
		case addlIsObj:
			c.check(value[k], addl, sub, depth)
		}
	}
}

type snapshot struct{ bindings, actions, cur, submits, blockCount, ids int }

func (c *ctx) checkAlternatives(value any, s obj, alts []any, anyOf bool, path string, depth int) {
	errs := make([]*Error, len(alts))
	ok := 0
	for i, a := range alts {
		snap := snapshot{c.bindings, c.actions, c.cur, len(c.submits), c.blockCount, len(c.idOrder)}
		e := c.try(value, a.(obj), path, depth)
		if e == nil {
			ok++
			if anyOf {
				break
			}
			continue
		}
		c.bindings, c.actions, c.cur, c.blockCount = snap.bindings, snap.actions, snap.cur, snap.blockCount
		c.submits = c.submits[:snap.submits]
		for _, id := range c.idOrder[snap.ids:] {
			delete(c.blockIDs, id)
		}
		c.idOrder = c.idOrder[:snap.ids]
		errs[i] = e
	}
	if !anyOf && ok > 1 {
		panic(fail(CodeInvalidBlock, path, "%s: coincide con mas de una alternativa", path))
	}
	if ok == 0 {
		idx := -1
		for i, a := range alts {
			r := resolve(a.(obj))
			t, hasT := r["type"]
			if !hasT || !matchesType(t, value) {
				continue
			}
			if vo, isO := value.(obj); isO {
				if req, ok := r["required"].([]any); ok {
					all := true
					for _, k := range req {
						if !has(vo, k.(string)) {
							all = false
						}
					}
					if !all {
						continue
					}
				}
			}
			idx = i
			break
		}
		if idx < 0 {
			for i, a := range alts {
				if t, ok := resolve(a.(obj))["type"]; ok && matchesType(t, value) {
					idx = i
					break
				}
			}
		}
		if idx < 0 {
			panic(fail(CodeType, path, "%s: tipo invalido (%s)", path, kindOf(value)))
		}
		panic(errs[idx])
	}
}

func (c *ctx) try(value any, schema obj, path string, depth int) (e *Error) {
	defer func() {
		if r := recover(); r != nil {
			if pe, ok := r.(*Error); ok {
				e = pe
				return
			}
			panic(r)
		}
	}()
	c.check(value, schema, path, depth)
	return nil
}

func (c *ctx) checkBlock(raw any, path string, depth int) {
	if depth > MaxDepth {
		panic(fail(CodeLimitDepth, path, "%s: profundidad maxima %d", path, MaxDepth))
	}
	b, ok := raw.(obj)
	if !ok {
		panic(fail(CodeType, path, "%s: el bloque debe ser un objeto", path))
	}
	typ, isStr := b["type"].(string)
	if !isStr || !blockTypes[typ] {
		panic(fail(CodeUnknownBlock, path, "%s: tipo de bloque desconocido %v", path, b["type"]))
	}
	c.blockCount++
	if c.blockCount > MaxBlocks {
		panic(fail(CodeLimitBlocks, path, "maximo %d bloques", MaxBlocks))
	}
	if id, ok := b["id"].(string); ok {
		if _, dup := c.blockIDs[id]; dup {
			panic(fail(CodeDuplicateID, path+".id", "%s: id duplicado %s", path, id))
		}
		c.blockIDs[id] = typ
		c.idOrder = append(c.idOrder, id)
	}
	outer := c.cur
	c.cur = 0
	c.check(b, defs["block_"+typ].(obj), path, depth+1)
	own := c.cur
	c.cur = outer
	if own > MaxActionsPerBlock {
		panic(fail(CodeLimitActions, path, "%s: maximo %d acciones por bloque", path, MaxActionsPerBlock))
	}
	c.blockRules(b, typ, path)
}

func lenOf(v any) int { l, _ := v.([]any); return len(l) }

func (c *ctx) blockRules(b obj, typ, path string) {
	present := func(k string) bool { return has(b, k) }
	switch typ {
	case "table":
		cols := lenOf(b["columns"])
		switch {
		case present("source"):
			if present("rows") {
				panic(fail(CodeInvalidBlock, path, "%s: table usa rows o source, no ambos", path))
			}
			if cols > MaxSourceTableColumns {
				panic(fail(CodeLimitColumns, path, "%s: maximo %d columnas con source", path, MaxSourceTableColumns))
			}
		case cols > MaxInlineTableColumns:
			panic(fail(CodeLimitColumns, path, "%s: maximo %d columnas en linea", path, MaxInlineTableColumns))
		case present("rows") && lenOf(b["rows"]) > MaxInlineTableRows:
			panic(fail(CodeLimitRows, path, "%s: maximo %d filas en linea", path, MaxInlineTableRows))
		}
	case "chart":
		if present("source") && present("rows") {
			panic(fail(CodeInvalidBlock, path, "%s: chart usa rows o source, no ambos", path))
		}
	case "stats", "timeline":
		if present("source") && present("items") {
			panic(fail(CodeInvalidBlock, path, "%s: %s usa items o source, no ambos", path, typ))
		}
		if !present("source") && !present("items") {
			panic(fail(CodeRequired, path, "%s: %s requiere items o source", path, typ))
		}
	case "map":
		if present("source") && present("points") {
			panic(fail(CodeInvalidBlock, path, "%s: map usa points o source, no ambos", path))
		}
		if !present("source") && !present("points") {
			panic(fail(CodeRequired, path, "%s: map requiere points o source", path))
		}
		if present("fields") && !present("source") {
			panic(fail(CodeInvalidBlock, path, "%s: fields solo aplica con source", path))
		}
		seen := map[string]bool{}
		pts, _ := b["points"].([]any)
		for _, p := range pts {
			k := str(asObj(p)["key"])
			if seen[k] {
				panic(fail(CodeDuplicateID, path, "%s: punto duplicado %s", path, k))
			}
			seen[k] = true
		}
	case "metric":
		if present("source") && present("value") {
			panic(fail(CodeInvalidBlock, path, "%s: metric usa value o source, no ambos", path))
		}
		if !present("source") && !present("value") {
			panic(fail(CodeRequired, path, "%s: metric requiere value o source", path))
		}
		if present("source") != present("field") {
			panic(fail(CodeInvalidBlock, path, "%s: source y field van juntos", path))
		}
		if present("spark_field") && !present("source") {
			panic(fail(CodeInvalidBlock, path, "%s: spark_field solo aplica con source", path))
		}
		if present("spark") && present("spark_field") {
			panic(fail(CodeInvalidBlock, path, "%s: metric usa spark o spark_field, no ambos", path))
		}
	case "diagram":
		ids := map[string]bool{}
		for _, n := range b["nodes"].([]any) {
			id := str(asObj(n)["id"])
			if ids[id] {
				panic(fail(CodeDuplicateID, path, "%s: nodo duplicado %s", path, id))
			}
			ids[id] = true
		}
		edges, _ := b["edges"].([]any)
		for _, e := range edges {
			eo := asObj(e)
			if !ids[str(eo["from"])] || !ids[str(eo["to"])] {
				panic(fail(CodeUnknownTarget, path, "%s: la arista %v->%v apunta a un nodo inexistente", path, eo["from"], eo["to"]))
			}
		}
	case "tabs":
		keys := map[string]bool{}
		for _, t := range b["tabs"].([]any) {
			k := str(asObj(t)["key"])
			if keys[k] {
				panic(fail(CodeDuplicateID, path, "%s: pestana duplicada %s", path, k))
			}
			keys[k] = true
		}
	case "form", "wizard":
		keys := map[string]bool{}
		fields, _ := b["fields"].([]any)
		for _, f := range fields {
			k := str(asObj(f)["key"])
			if keys[k] {
				panic(fail(CodeDuplicateID, path, "%s: campo duplicado %s", path, k))
			}
			keys[k] = true
		}
		var steps []any
		if typ == "wizard" {
			steps, _ = b["steps"].([]any)
		} else {
			steps, _ = asObj(b["layout"])["steps"].([]any)
		}
		for _, st := range steps {
			for _, k := range asObj(st)["fields"].([]any) {
				if !keys[k.(string)] {
					panic(fail(CodeUnknownTarget, path, "%s: el paso usa un campo inexistente %v", path, k))
				}
			}
		}
		for _, f := range fields {
			if d, ok := asObj(f)["depends_on"].(string); ok && !keys[d] {
				panic(fail(CodeUnknownTarget, path, "%s: depends_on apunta a un campo inexistente %s", path, d))
			}
		}
		hasFields := len(fields) > 0
		if typ == "form" && asObj(b["layout"])["mode"] == "steps" && len(steps) == 0 {
			panic(fail(CodeRequired, path, "%s: layout.mode steps requiere steps", path))
		}
		if typ == "form" && !hasFields {
			panic(fail(CodeRequired, path, "%s: form requiere fields", path))
		}
		if typ == "wizard" && !present("ref") && !(len(steps) > 0 && hasFields) {
			panic(fail(CodeRequired, path, "%s: wizard requiere steps y fields, o ref", path))
		}
	}
}

var ctxKeys = map[string]bool{"locale": true, "currency": true, "time_zone": true, "branch_id": true}

func (c *ctx) refRule(ref, path string) {
	parts := strings.Split(ref, ".")
	if parts[0] == "ctx" && (len(parts) != 2 || !ctxKeys[parts[1]]) {
		panic(fail(CodeBadRef, path, "%s: ctx solo expone locale, currency, time_zone, branch_id", path))
	}
	if parts[0] == "data" {
		d, ok := c.doc["data"].(obj)
		if !ok || !has(d, parts[1]) {
			panic(fail(CodeBadRef, path, "%s: la fuente %s no esta declarada en data", path, parts[1]))
		}
	}
}

func (c *ctx) actionRule(a obj, path string) {
	verb := str(a["do"])
	overlays := asObj(c.doc["overlays"])
	switch verb {
	case "navigate":
		r := asObj(a["route"])
		switch {
		case has(r, "path"):
			if has(r, "name") || has(r, "model") || has(r, "id") || has(r, "filters") || has(r, "key") {
				panic(fail(CodeInvalidAction, path, "%s: route usa path o name, no ambos", path))
			}
		case r["name"] == "record":
			if !has(r, "model") || !has(r, "id") {
				panic(fail(CodeInvalidAction, path, "%s: route record requiere model e id", path))
			}
		case r["name"] == "list":
			if !has(r, "model") {
				panic(fail(CodeInvalidAction, path, "%s: route list requiere model", path))
			}
		case r["name"] == "page":
			if !has(r, "key") {
				panic(fail(CodeInvalidAction, path, "%s: route page requiere key", path))
			}
		default:
			panic(fail(CodeInvalidAction, path, "%s: route necesita path o name", path))
		}
	case "open", "close":
		if verb == "open" || has(a, "overlay") {
			if !has(overlays, str(a["overlay"])) {
				panic(fail(CodeUnknownOverlay, path, "%s: overlay inexistente %v", path, a["overlay"]))
			}
		}
	case "refresh":
		if !has(asObj(c.doc["data"]), str(a["source"])) {
			panic(fail(CodeUnknownTarget, path, "%s: la fuente %v no esta declarada en data", path, a["source"]))
		}
	case "submit":
		c.submits = append(c.submits, submitRef{str(a["form"]), path})
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

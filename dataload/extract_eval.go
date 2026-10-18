package dataload

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// --- Plantillas -------------------------------------------------------------------------------

// template es "texto {campo} texto {campo|filtro|filtro(arg)}". Los campos
// ausentes se escriben vacíos. Filtros: slug, upper, lower, decimal (un entero
// "3" pasa a "3.0"), safe (cambia | y ; por espacio) y span(otro) ("2010" si
// ambos extremos son iguales, "2010-2012" si no).
type template struct {
	parts []tmplPart
}

type tmplPart struct {
	lit     string
	name    string
	filters []tmplFilter
}

type tmplFilter struct{ name, arg string }

var filterNames = map[string]bool{"slug": true, "upper": true, "lower": true, "decimal": true, "safe": true, "span": true}

func parseTemplate(s string) (*template, error) {
	t := &template{}
	for len(s) > 0 {
		i := strings.IndexByte(s, '{')
		if i < 0 {
			t.parts = append(t.parts, tmplPart{lit: s})
			break
		}
		if i > 0 {
			t.parts = append(t.parts, tmplPart{lit: s[:i]})
		}
		j := strings.IndexByte(s[i:], '}')
		if j < 0 {
			return nil, fmt.Errorf("plantilla con { sin cerrar: %q", s)
		}
		body := s[i+1 : i+j]
		s = s[i+j+1:]
		segs := strings.Split(body, "|")
		p := tmplPart{name: strings.TrimSpace(segs[0])}
		if p.name == "" {
			return nil, fmt.Errorf("plantilla con {} vacío")
		}
		for _, f := range segs[1:] {
			f = strings.TrimSpace(f)
			flt := tmplFilter{name: f}
			if k := strings.IndexByte(f, '('); k > 0 && strings.HasSuffix(f, ")") {
				flt = tmplFilter{name: f[:k], arg: strings.TrimSpace(f[k+1 : len(f)-1])}
			}
			if !filterNames[flt.name] {
				return nil, fmt.Errorf("filtro %q desconocido en la plantilla", flt.name)
			}
			if flt.name == "span" && flt.arg == "" {
				return nil, fmt.Errorf("el filtro span necesita el otro campo: span(campo)")
			}
			p.filters = append(p.filters, flt)
		}
		t.parts = append(t.parts, p)
	}
	return t, nil
}

func (t *template) render(get func(string) (string, bool)) string {
	var b strings.Builder
	for _, p := range t.parts {
		if p.name == "" {
			b.WriteString(p.lit)
			continue
		}
		v, _ := get(p.name)
		for _, f := range p.filters {
			switch f.name {
			case "slug":
				v = slugify(v)
			case "upper":
				v = strings.ToUpper(v)
			case "lower":
				v = strings.ToLower(v)
			case "decimal":
				if v != "" && strings.Trim(v, "0123456789") == "" {
					v += ".0"
				}
			case "safe":
				v = strings.NewReplacer("|", " ", ";", " ").Replace(v)
			case "span":
				if other, ok := get(f.arg); ok && other != "" && other != v && v != "" {
					v = v + "-" + other
				}
			}
		}
		b.WriteString(v)
	}
	return b.String()
}

func validPost(p string) bool {
	switch p {
	case "collapse", "slug", "upper", "lower":
		return true
	}
	return false
}

func applyPost(ops []string, s string) string {
	for _, op := range ops {
		switch op {
		case "collapse":
			s = strings.Join(strings.Fields(s), " ")
		case "slug":
			s = slugify(s)
		case "upper":
			s = strings.ToUpper(s)
		case "lower":
			s = strings.ToLower(s)
		}
	}
	return s
}

var reNotSlug = regexp.MustCompile(`[^a-z0-9.]+`)

// slugify: sin acentos, minúsculas, todo lo que no sea letra, dígito o punto pasa a "-".
func slugify(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(s) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		b.WriteRune(r)
	}
	out := reNotSlug.ReplaceAllString(strings.ToLower(strings.TrimSpace(b.String())), "-")
	return strings.Trim(out, "-")
}

// --- Evaluación de una fila -------------------------------------------------------------------

// Extraction es el resultado de aplicar el documento a una fila.
type Extraction struct {
	Review []ReviewItem
	// Status: "ok" o "revisar".
	Status string
}

// Blocking dice si algún motivo bloquea la carga automática.
func (x Extraction) Blocking() bool { return x.Status == SeverityRevisar }

// Codes devuelve los códigos emitidos, ordenados y sin repetir.
func (x Extraction) Codes() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range x.Review {
		if !seen[r.Code] {
			seen[r.Code] = true
			out = append(out, r.Code)
		}
	}
	sort.Strings(out)
	return out
}

type rowState struct {
	e      *Extractor
	prof   Profile
	raw    map[string]any
	rec    map[string]any
	vars   map[string]any
	pieces map[string]string // variables de texto "en proceso" (restos de entity/tokens/prefix)
	work   string
	title  string
	items  []ReviewItem
	seen   map[string]bool
	codes  map[string]bool
}

func (s *rowState) issue(code, field, detail string) {
	if code == "" {
		return
	}
	key := code + "\x00" + field
	if s.seen[key] {
		return
	}
	s.seen[key] = true
	s.codes[code] = true
	if detail == "" {
		detail = code
	}
	s.items = append(s.items, ReviewItem{Code: code, Field: field, Severity: s.e.severity[code], Detail: detail})
}

func (s *rowState) get(name string) (string, bool) {
	if v, ok := s.vars[name]; ok {
		return valueText(v), true
	}
	if v, ok := getPath(s.rec, name); ok && v != nil {
		return valueText(v), true
	}
	return "", false
}

func (s *rowState) isSet(name string) bool {
	if v, ok := s.vars[name]; ok {
		return !isEmptyValue(v)
	}
	if v, ok := getPath(s.rec, name); ok {
		return !isEmptyValue(v)
	}
	return false
}

func (s *rowState) number(name string) (float64, bool) {
	var v any
	if x, ok := s.vars[name]; ok {
		v = x
	} else if x, ok := getPath(s.rec, name); ok {
		v = x
	}
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}

func isEmptyValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(t) == ""
	case bool:
		return !t
	case []string:
		return len(t) == 0
	}
	return false
}

func valueText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case int:
		return strconv.Itoa(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		if t {
			return "true"
		}
		return ""
	case []string:
		return strings.Join(t, ",")
	}
	return fmt.Sprint(v)
}

// Apply aplica el documento a una fila ya mapeada por el perfil: lee rec en
// text.source, escribe en rec lo que declaran outputs y categories y devuelve
// los motivos de revisión. raw es la fila ORIGINAL (para rawPattern).
func (e *Extractor) Apply(prof Profile, raw map[string]any, rec map[string]any) Extraction {
	s := &rowState{e: e, prof: prof, raw: raw, rec: rec, vars: map[string]any{}, pieces: map[string]string{}, seen: map[string]bool{}, codes: map[string]bool{}}
	src, _ := getPath(rec, e.Doc.Text.Source)
	text, ok := src.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return Extraction{Status: "ok"}
	}
	s.work = collapseText(applyClean(e.norm, text))
	s.title = s.work

	for i := range e.rules {
		s.runRule(&e.rules[i])
	}
	s.title = s.dropTokens(s.title)
	s.applyCategories()
	for _, c := range e.checks {
		s.runCheck(c)
	}
	s.writeOutputs()

	x := Extraction{Review: s.items, Status: "ok"}
	for _, it := range s.items {
		if it.Severity == SeverityRevisar {
			x.Status = SeverityRevisar
		}
	}
	return x
}

func collapseText(s string) string { return strings.Join(strings.Fields(s), " ") }

func cutAt(text string, loc []int) string {
	return collapseText(text[:loc[0]] + " " + text[loc[1]:])
}

func (s *rowState) dropTokens(text string) string {
	drop := s.e.Doc.Text.DropTokens
	if len(drop) == 0 {
		return text
	}
	var keep []string
	for _, tok := range strings.Fields(text) {
		skip := false
		for _, d := range drop {
			if tok == d {
				skip = true
				break
			}
		}
		if !skip {
			keep = append(keep, tok)
		}
	}
	return strings.Join(keep, " ")
}

func (s *rowState) runRule(r *compiledRule) {
	switch r.Kind {
	case RuleCapture:
		loc := r.re.FindStringSubmatchIndex(s.work)
		if loc == nil {
			return
		}
		group := func(name string) (string, bool) {
			i := r.idx[name]
			if loc[2*i] < 0 {
				return "", false
			}
			return s.work[loc[2*i]:loc[2*i+1]], true
		}
		names := make([]string, 0, len(r.into))
		for f := range r.into {
			names = append(names, f)
		}
		sort.Strings(names)
		for _, field := range names {
			spec := r.into[field]
			v, present := group(spec.Group)
			if !present {
				continue
			}
			if isNullValue(spec.Null, v) {
				continue
			}
			switch spec.As {
			case "int":
				if n, err := strconv.Atoi(v); err == nil {
					s.vars[field] = n
				}
			case "flag":
				s.vars[field] = true
			case "list":
				var parts []string
				for _, p := range regexp.MustCompile(spec.Split).Split(v, -1) {
					if p = strings.TrimSpace(p); p != "" {
						parts = append(parts, p)
					}
				}
				s.vars[field] = parts
			default:
				s.vars[field] = v
			}
		}
		for _, ri := range r.ReviewIf {
			if l, ok := s.vars[ri.Into].([]string); ok && ri.When == "listLengthGt1" && len(l) > 1 {
				s.issue(ri.Code, ri.Into, ri.Detail)
			}
		}
		if r.Consume {
			s.work = cutAt(s.work, loc)
		}
		if r.Title == "remove" {
			if tl := r.re.FindStringIndex(s.title); tl != nil {
				s.title = cutAt(s.title, tl)
			}
		}
	case RuleEntity:
		s.runEntity(r)
	case RuleTokens:
		text := s.piece(r.From)
		dict := s.e.vocabDict[r.Dictionary]
		var found, kept []string
		for _, tok := range strings.Fields(text) {
			parts := splitNonEmpty(tok, "/") // "TRASERO/" (barra pegada) también cuenta
			all := len(parts) > 0
			for _, p := range parts {
				if _, ok := dict[p]; !ok {
					all = false
					break
				}
			}
			if all {
				for _, p := range parts {
					found = append(found, dict[p])
				}
			} else {
				kept = append(kept, tok)
			}
		}
		s.pieces[r.From] = strings.Join(kept, " ")
		vals := uniqueStrings(found)
		if len(r.Order) > 0 {
			var ordered []string
			for _, o := range r.Order {
				for _, v := range vals {
					if v == o {
						ordered = append(ordered, v)
					}
				}
			}
			vals = ordered
		}
		if len(vals) > 0 {
			join := r.Join
			if join == "" {
				join = "/"
			}
			s.vars[r.intoTxt] = strings.Join(vals, join)
		}
	case RulePrefix:
		text := s.piece(r.From)
		dict := append([]string(nil), s.e.vocabTxt[r.Dictionary]...)
		sort.SliceStable(dict, func(i, j int) bool {
			wi, wj := len(strings.Fields(dict[i])), len(strings.Fields(dict[j]))
			if wi != wj {
				return wi > wj
			}
			return len(dict[i]) > len(dict[j])
		})
		for _, p := range dict {
			if text == p || strings.HasPrefix(text, p+" ") {
				s.vars[r.intoTxt] = p
				if rest := strings.TrimSpace(text[len(p):]); rest != "" && r.Rest != "" {
					s.vars[r.Rest] = rest
				}
				return
			}
		}
		if text != "" {
			s.vars[r.intoTxt] = text
			s.issue(r.UnknownCode, r.intoTxt, "el texto no está en el vocabulario del documento")
		}
	case RuleSet:
		for _, req := range r.Requires {
			if !s.isSet(req) {
				return
			}
		}
		out := r.tmpl.render(s.get)
		out = applyPost(r.Post, out)
		if strings.TrimSpace(out) != "" {
			s.vars[r.Field] = out
		}
	}
}

func isNullValue(nulls []string, v string) bool {
	for _, n := range nulls {
		if v == n {
			return true
		}
	}
	return false
}

func splitNonEmpty(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// piece devuelve el texto en proceso de una variable; si ninguna regla entity lo
// fijó, es el texto de trabajo completo.
func (s *rowState) piece(name string) string {
	if p, ok := s.pieces[name]; ok {
		return p
	}
	return s.work
}

func (s *rowState) runEntity(r *compiledRule) {
	ev := s.e.vocabEnt[r.Vocab]
	loc := r.re.FindStringSubmatchIndex(s.work)
	restVar := r.RestGroup
	if r.RestVariable != "" {
		restVar = r.RestVariable
	}
	if loc == nil {
		s.pieces[restVar] = s.dropTokens(s.work)
		return
	}
	grp := func(name string) string {
		i := r.idx[name]
		if name == "" || loc[2*i] < 0 {
			return ""
		}
		return s.work[loc[2*i]:loc[2*i+1]]
	}
	s.pieces[restVar] = s.dropTokens(grp(r.RestGroup))
	ent := ev.byAlias[grp(r.EntityGroup)]
	detail := s.dropTokens(grp(r.DetailGroup))
	if em := r.Emit; em != nil {
		if em.ID != "" {
			s.vars[em.ID] = ent.ID
		}
		if em.Label != "" {
			s.vars[em.Label] = ent.Label
		}
		if em.Detail != "" && detail != "" {
			s.vars[em.Detail] = detail
		}
	}
	g := r.Guards
	if g == nil {
		return
	}
	switch {
	case detail == "":
		s.issue(g.SuspectCode, "entity", "se reconoció la entidad pero no su detalle")
	case g.MaxTokens > 0 && len(strings.Fields(detail)) > g.MaxTokens:
		s.issue(g.SuspectCode, "entity", "detalle demasiado largo: "+detail)
	case r.suspect != nil && r.suspect.MatchString(detail):
		s.issue(g.SuspectCode, "entity", "detalle con calificadores: "+detail)
	}
	if ev.anyRe != nil {
		if m := ev.anyRe.FindStringSubmatch(detail); m != nil {
			allowed := false
			for _, a := range g.AllowIn[ent.ID] {
				if a == m[1] {
					allowed = true
				}
			}
			if !allowed {
				s.issue(g.AmbiguousCode, "entity", "otra entidad aparece dentro del detalle")
			}
		}
	}
}

func (s *rowState) applyCategories() {
	c := s.e.Doc.Categories
	if c == nil {
		return
	}
	src, _ := s.get(c.Source)
	cc, ok := s.e.cats[src]
	if !ok || cc.ID == nil {
		s.issue(c.UnmappedCode, c.Into, fmt.Sprintf("categoría de origen sin equivalente: %s", orEmpty(src)))
		if ok {
			s.applySet(cc.Set)
		}
		return
	}
	id := *cc.ID
	for i, rf := range cc.Refine {
		if v, _ := s.get(rf.Field); v != "" && cc.refine[i].MatchString(v) {
			id = rf.ID
		}
	}
	setPathValue(s.rec, c.Into, id)
	s.applySet(cc.Set)
}

func orEmpty(s string) string {
	if s == "" {
		return "(vacía)"
	}
	return s
}

func (s *rowState) applySet(m map[string]any) {
	for k, v := range m {
		s.vars[k] = v
	}
}

func (s *rowState) runCheck(c compiledCheck) {
	detail := c.Detail
	switch c.Type {
	case CheckSum:
		total, ok := s.number(c.Total)
		if !ok {
			return
		}
		sum := 0.0
		for _, p := range c.Parts {
			n, ok := s.number(p)
			if !ok {
				return
			}
			sum += n
		}
		if math.Abs(sum-total) > c.Tolerance {
			s.issue(c.Code, c.Total, orDetail(detail, "el total no coincide con la suma de sus partes"))
		}
	case CheckGreater:
		l, ok1 := s.number(c.Left)
		r, ok2 := s.number(c.Right)
		if ok1 && ok2 && !(l > r) {
			s.issue(c.Code, c.Left, orDetail(detail, c.Left+" no es mayor que "+c.Right))
		}
	case CheckRawPattern:
		for _, f := range c.Fields {
			if cell, ok := s.rawCell(f); ok && c.re.MatchString(strings.TrimSpace(cell)) {
				s.issue(c.Code, f, orDetail(detail, "el origen no trae el dato"))
			}
		}
	case CheckWholeNumber:
		for _, f := range c.Fields {
			if n, ok := s.number(f); ok && n != math.Trunc(n) {
				s.issue(c.Code, f, orDetail(detail, "el valor no es un entero"))
			}
		}
	case CheckNonNegative:
		for _, f := range c.Fields {
			if n, ok := s.number(f); ok && n < 0 {
				s.issue(c.Code, f, orDetail(detail, "el valor es negativo"))
			}
		}
	case CheckRange:
		from, ok1 := s.number(c.From)
		to, ok2 := s.number(c.To)
		if !ok1 || !ok2 {
			return
		}
		invalid := from > to
		if c.Min != nil && (from < float64(*c.Min) || to < float64(*c.Min)) {
			invalid = true
		}
		if c.Max != nil && (from > float64(*c.Max) || to > float64(*c.Max)) {
			invalid = true
		}
		if c.MaxSpan > 0 && to-from > float64(c.MaxSpan) {
			invalid = true
		}
		if invalid {
			s.issue(c.Code, c.From, orDetail(detail, fmt.Sprintf("%v-%v fuera de rango o invertido", from, to)))
		}
	case CheckWhen:
		for _, cond := range c.All {
			if s.isSet(cond.Field) != (cond.Is == "set") {
				return
			}
		}
		s.issue(c.Code, c.Field, orDetail(detail, c.ID))
	}
}

func orDetail(d, def string) string {
	if d != "" {
		return d
	}
	return def
}

// rawCell lee la celda ORIGINAL (antes de limpieza y de values) del campo del perfil.
func (s *rowState) rawCell(target string) (string, bool) {
	f, ok := s.prof.field(target)
	if !ok {
		return "", false
	}
	want := map[string]bool{}
	for _, h := range f.headers() {
		want[normHeader(h)] = true
	}
	for h, v := range s.raw {
		if want[normHeader(h)] {
			if v == nil {
				return "", false
			}
			return stringifyCell(v), true
		}
	}
	return "", false
}

func (s *rowState) blocked(unless []string) bool {
	for _, c := range unless {
		if s.codes[c] {
			return true
		}
	}
	return false
}

func (s *rowState) writeOutputs() {
	o := s.e.Doc.Outputs
	if o.Title != "" && s.title != "" {
		setPathValue(s.rec, o.Title, s.title)
	}
	attrTarget := o.AttributesTarget
	if attrTarget == "" {
		attrTarget = "attributes"
	}
	attrs := map[string]any{}
	for _, name := range s.e.attrNames {
		a := o.Attributes[name]
		if s.blocked(a.Unless) {
			continue
		}
		var v any
		if x, ok := s.vars[a.Field]; ok {
			v = x
		} else if x, ok := getPath(s.rec, a.Field); ok {
			v = x
		}
		switch t := v.(type) {
		case nil:
			continue
		case bool:
			if !t {
				continue
			}
			attrs[name] = true
		case []string:
			if len(t) > 0 {
				attrs[name] = strings.Join(t, ",")
			}
		case string:
			if strings.TrimSpace(t) != "" {
				attrs[name] = t
			}
		default:
			attrs[name] = v
		}
	}

	for _, l := range s.e.outputs {
		if s.blocked(l.Unless) {
			continue
		}
		if !s.allSet(l.Requires) || s.anySet(l.RequiresNot) {
			continue
		}
		var lines []string
		if l.Each != nil {
			from, ok1 := s.number(l.Each.From)
			to, ok2 := s.number(l.Each.To)
			span := l.Each.MaxSpan
			if span == 0 {
				span = 200
			}
			if !ok1 || !ok2 || from > to || int(to-from) > span {
				continue
			}
			for n := int(from); n <= int(to); n++ {
				as, val := l.Each.As, strconv.Itoa(n)
				lines = append(lines, l.tmpl.render(func(name string) (string, bool) {
					if name == as {
						return val, true
					}
					return s.get(name)
				}))
			}
		} else {
			lines = []string{l.tmpl.render(s.get)}
		}
		s.append(l, lines, attrs, attrTarget)
	}
	if len(attrs) > 0 {
		cur, _ := getPath(s.rec, attrTarget)
		merged, _ := cur.(map[string]any)
		if merged == nil {
			merged = map[string]any{}
		}
		for k, v := range attrs {
			merged[k] = v
		}
		setPathValue(s.rec, attrTarget, merged)
	}
	for f := range s.e.drop {
		deletePath(s.rec, f)
	}
}

func (s *rowState) allSet(names []string) bool {
	for _, n := range names {
		if !s.isSet(n) {
			return false
		}
	}
	return true
}

func (s *rowState) anySet(names []string) bool {
	for _, n := range names {
		if s.isSet(n) {
			return true
		}
	}
	return false
}

// append escribe las líneas renderizadas. Un destino bajo attributes.* va al mapa
// de atributos (que se funde al final) para no pelear con la salida de Attributes.
func (s *rowState) append(l compiledList, lines []string, attrs map[string]any, attrTarget string) {
	if l.Join != "" {
		text := strings.Join(lines, l.Join)
		if sub, ok := strings.CutPrefix(l.Target, attrTarget+"."); ok {
			if prev, ok := attrs[sub].(string); ok && prev != "" {
				text = prev + l.Join + text
			}
			attrs[sub] = text
			return
		}
		if prev, ok := getPath(s.rec, l.Target); ok {
			if ps, _ := prev.(string); ps != "" {
				text = ps + l.Join + text
			}
		}
		setPathValue(s.rec, l.Target, text)
		return
	}
	var list []any
	if prev, ok := getPath(s.rec, l.Target); ok {
		list, _ = prev.([]any)
	}
	for _, ln := range lines {
		list = append(list, ln)
	}
	setPathValue(s.rec, l.Target, list)
}

func setPathValue(rec map[string]any, path string, v any) { setRecordPath(rec, path, v) }

// --- Comprobaciones de conjunto ----------------------------------------------------------------

// RowFacts son los datos de una fila que las comprobaciones de conjunto necesitan.
type RowFacts struct {
	Record map[string]any
}

// DatasetIssue es un motivo de revisión que nace de comparar filas entre sí.
type DatasetIssue struct {
	Index int // posición en la lista recibida
	Item  ReviewItem
}

// CheckDataset aplica las comprobaciones de conjunto. Cada fila que comparte un
// valor repetido (la primera incluida) recibe el motivo.
func (e *Extractor) CheckDataset(rows []RowFacts) []DatasetIssue {
	var out []DatasetIssue
	for _, c := range e.Doc.DatasetChecks {
		byValue := map[string][]int{}
		for i, r := range rows {
			if r.Record == nil {
				continue
			}
			v, ok := getPath(r.Record, c.Field)
			if !ok || v == nil || strings.TrimSpace(valueText(v)) == "" {
				continue
			}
			k := valueText(v)
			byValue[k] = append(byValue[k], i)
		}
		for _, idxs := range byValue {
			if len(idxs) < 2 {
				continue
			}
			for _, i := range idxs {
				out = append(out, DatasetIssue{Index: i, Item: ReviewItem{
					Code: c.Code, Field: c.Field, Severity: e.severity[c.Code], Detail: orDetail(c.Detail, "valor repetido en el archivo"),
				}})
			}
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Index != out[b].Index {
			return out[a].Index < out[b].Index
		}
		return out[a].Item.Code < out[b].Item.Code
	})
	return out
}

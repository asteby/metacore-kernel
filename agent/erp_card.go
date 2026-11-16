package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ERP-Card is the compact, model-readable notation the copilot uses to see a
// data model: one header line (real name, Spanish label, synonyms) and one
// line of fields (real name, type, label, flags). ERPCardLegend explains the
// notation once, in the cached system prompt; cards travel in tool results and
// in the per-turn context block. Cards are deterministic: the same metadata
// always renders the same bytes, so they cache and diff cleanly.

// ERPCardLegend is the fixed explanation of the ERP-Card notation (~150
// tokens). It never changes at runtime, so it belongs to the cached prefix.
const ERPCardLegend = `Notación ERP-Card: modelo "Etiqueta" (sinónimos) y luego campo:tipo "Etiqueta" separados por |.
Tipos: str | text | int | num | money(c)=importe en la moneda del campo c | bool | date | dt=fecha-hora | json | id.
->M many2one a M (guarda el id) | []M one2many | <>M many2many | enum(a|b) valores válidos (valor=Etiqueta) | * requerido | ro solo lectura.
Todo modelo tiene id y created_at. Usa SIEMPRE el nombre real del campo (antes de :), nunca la etiqueta.`

// CatalogField is one field of a model as the semantic catalog sees it.
type CatalogField struct {
	// Name is the real column name (what queries use).
	Name string
	// Label is the localized UI label ("" when it adds nothing to Name).
	Label string
	// Type is the canonical card type: str, text, int, num, money, bool,
	// date, dt, json, id, img (relations and enums use Rel / Enum).
	Type string
	// Rel is "m2o", "o2m" or "m2m" for relations; Ref is the target model.
	Rel string
	Ref string
	// Currency names the currency field of a money field ("" = none).
	Currency string
	// Enum lists the accepted values of a select-like field.
	Enum                           []EnumOption
	Required, Readonly, Filterable bool
}

// EnumOption is one accepted value of an enum field and its UI label.
type EnumOption struct {
	Value string
	Label string
}

// CatalogModel is the host-neutral, typed view of one model the catalog
// renders as an ERP-Card and indexes for retrieval.
type CatalogModel struct {
	// Name is the key the tools accept (the table when there is one).
	Name string
	// Key / ModelKey are the registry keys (aliases for resolution).
	Key, ModelKey string
	// Label is the localized title ("Pedidos de venta").
	Label string
	// Synonyms are extra names the user may use ("pedido", "orden").
	Synonyms []string
	// Description is free text from the metadata (indexed, not rendered).
	Description string
	Fields      []CatalogField
	// SearchIn are the free-text search columns.
	SearchIn []string
	// Vector reports semantic search support (vector_search).
	Vector bool
}

// CatalogLabeler localizes a field label (ops' FieldLabel satisfies it). The
// optional EnumValueLabel method is used when the labeler also has it.
type CatalogLabeler interface {
	FieldLabel(model, key, raw string) string
}

type enumLabeler interface {
	EnumValueLabel(model, field, value string) string
}

// ModelSynonymSource is an optional extension of ModelCatalog: hosts that
// keep curated synonyms per model expose them here.
type ModelSynonymSource interface {
	ModelSynonyms(modelKey string) []string
}

// cardSkipColumns never appear on a card: tenant/system plumbing and secrets.
// id and created_at are implicit (the legend says every model has them).
var cardSkipColumns = map[string]bool{
	"id": true, "organization_id": true, "created_at": true, "updated_at": true, "deleted_at": true,
	"created_by_id": true, "updated_by_id": true, "embedding": true, "password": true,
	"password_hash": true, "token": true, "secret": true, "api_key": true,
}

// rawSchemaField is the union of the column/field shapes hosts put in
// ModelMeta.Schema (modelbase.ColumnDef, modelbase.FieldDef, manifest column
// specs, or their JSON round-trip).
type rawSchemaField struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Field       string `json:"field"`
	Label       string `json:"label"`
	Type        string `json:"type"`
	Ref         string `json:"ref"`
	Multiple    bool   `json:"multiple"`
	Required    bool   `json:"required"`
	Readonly    bool   `json:"readonly"`
	Filterable  bool   `json:"filterable"`
	Generated   string `json:"generated"`
	Description string `json:"description"`
	Options     []struct {
		Value any    `json:"value"`
		Label string `json:"label"`
	} `json:"options"`
}

func (r rawSchemaField) key() string {
	for _, k := range []string{r.Key, r.Name, r.Field} {
		if k = strings.TrimSpace(k); k != "" {
			return k
		}
	}
	return ""
}

type rawRelation struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Through string `json:"through"`
	Label   string `json:"label"`
}

// decodeSchemaList turns a schema value (typed slice, []any, []map) into
// rawSchemaField entries through one JSON round-trip.
func decodeSchemaList[T any](v any) []T {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out []T
	if err := json.Unmarshal(b, &out); err != nil {
		return nil
	}
	return out
}

// BuildCatalogModel derives the typed catalog view of m from the metadata the
// host already passes through its ModelCatalog (Schema "columns", "fields",
// "filters", "relations", "synonyms", "description"). labels is optional.
func BuildCatalogModel(cat ModelCatalog, m ModelMeta, labels CatalogLabeler) CatalogModel {
	name := m.Key
	if m.Table != "" {
		name = m.Table
	}
	cm := CatalogModel{Name: name, Key: m.Key, ModelKey: m.ModelKey, SearchIn: append([]string(nil), m.SearchIn...), Vector: m.VectorEnabled}
	if cat != nil {
		cm.Label = ModelLabel(cat, name)
	} else {
		cm.Label = humanizeSnake(name)
	}
	if foldText(cm.Label) == foldText(humanizeSnake(name)) && m.Label != "" && !strings.Contains(m.Label, ".") {
		cm.Label = m.Label
	}

	generated := map[string]bool{}
	for _, g := range m.GeneratedColumns {
		generated[g] = true
	}
	index := map[string]int{}
	add := func(r rawSchemaField) {
		key := r.key()
		if key == "" || !isValidField(key) || cardSkipColumns[strings.ToLower(key)] {
			return
		}
		i, seen := index[key]
		if !seen {
			i = len(cm.Fields)
			index[key] = i
			cm.Fields = append(cm.Fields, CatalogField{Name: key})
		}
		f := &cm.Fields[i]
		if f.Label == "" && strings.TrimSpace(r.Label) != "" {
			f.Label = fieldLabel(cat, labels, name, key, r.Label)
		}
		if f.Ref == "" && strings.TrimSpace(r.Ref) != "" {
			f.Ref = r.Ref
			f.Rel = "m2o"
			if r.Multiple {
				f.Rel = "m2m"
			}
		}
		if f.Type == "" {
			f.Type = canonicalFieldType(r.Type)
		}
		if len(f.Enum) == 0 && len(r.Options) > 0 {
			for _, o := range r.Options {
				v := strings.TrimSpace(fmt.Sprint(o.Value))
				if v == "" || o.Value == nil {
					continue
				}
				f.Enum = append(f.Enum, EnumOption{Value: v, Label: enumLabel(cat, labels, name, key, v, o.Label)})
			}
		}
		f.Required = f.Required || r.Required
		f.Readonly = f.Readonly || r.Readonly || strings.TrimSpace(r.Generated) != "" || generated[key]
		f.Filterable = f.Filterable || r.Filterable
	}
	for _, r := range decodeSchemaList[rawSchemaField](m.Schema["columns"]) {
		add(r)
	}
	for _, r := range decodeSchemaList[rawSchemaField](m.Schema["fields"]) {
		add(r)
	}
	for _, c := range m.Columns {
		add(rawSchemaField{Key: c})
	}
	// Table filters mark their column filterable.
	for _, f := range decodeSchemaList[struct {
		Key    string `json:"key"`
		Column string `json:"column"`
	}](m.Schema["filters"]) {
		col := f.Column
		if col == "" {
			col = f.Key
		}
		if i, ok := index[col]; ok {
			cm.Fields[i].Filterable = true
		}
	}
	finishFields(cat, &cm)

	for _, r := range decodeSchemaList[rawRelation](m.Schema["relations"]) {
		if r.Name == "" || r.Through == "" || !isValidField(r.Name) {
			continue
		}
		if _, dup := index[r.Name]; dup {
			continue
		}
		rel := "o2m"
		if strings.Contains(strings.ToLower(r.Kind), "many_to_many") {
			rel = "m2m"
		}
		index[r.Name] = len(cm.Fields)
		cm.Fields = append(cm.Fields, CatalogField{Name: r.Name, Rel: rel, Ref: r.Through, Label: fieldLabel(cat, labels, name, r.Name, r.Label), Readonly: true})
	}

	cm.Description = schemaString(m.Schema["description"])
	cm.Synonyms = modelSynonyms(cat, m, cm)
	return cm
}

// finishFields fills what the raw metadata leaves implicit: relation targets
// of *_id columns, money currency fields, text type defaults, and drops labels
// that only repeat the name.
func finishFields(cat ModelCatalog, cm *CatalogModel) {
	currency := ""
	for _, c := range []string{"currency_id", "currency", "currency_code", "moneda_id", "moneda"} {
		for _, f := range cm.Fields {
			if f.Name == c {
				currency = c
				break
			}
		}
		if currency != "" {
			break
		}
	}
	for i := range cm.Fields {
		f := &cm.Fields[i]
		if f.Ref == "" && strings.HasSuffix(f.Name, "_id") && cat != nil {
			base := strings.TrimSuffix(f.Name, "_id")
			if target := ResolveModelKey(cat, base); target != "" && !strings.Contains(target, ".") {
				if _, ok := cat.Lookup(target); ok {
					f.Ref, f.Rel = target, "m2o"
				}
			}
		}
		if f.Ref != "" && cat != nil {
			if target := ResolveModelKey(cat, f.Ref); target != "" && !strings.Contains(target, ".") {
				f.Ref = target
			}
		}
		if f.Type == "" {
			f.Type = "str"
			if strings.HasSuffix(f.Name, "_id") {
				f.Type = "id"
			}
		}
		if f.Type == "money" && f.Name != currency {
			f.Currency = currency
		}
		if f.Label != "" && foldText(f.Label) == foldText(humanizeSnake(f.Name)) {
			f.Label = ""
		}
	}
}

// canonicalFieldType maps the host's UI/DDL types onto the card vocabulary.
func canonicalFieldType(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "":
		return ""
	case "integer", "int", "bigint", "smallint", "int64", "int32", "serial":
		return "int"
	case "number", "float", "decimal", "numeric", "double", "real", "percent", "percentage", "bar":
		return "num"
	case "currency", "money", "monetary", "price", "amount":
		return "money"
	case "boolean", "bool", "checkbox", "switch", "toggle":
		return "bool"
	case "date":
		return "date"
	case "datetime", "timestamp", "timestamptz", "time", "date_time":
		return "dt"
	case "json", "jsonb", "array", "object", "tags":
		return "json"
	case "uuid":
		return "id"
	case "textarea", "richtext", "markdown", "html", "longtext":
		return "text"
	case "image", "file", "avatar", "attachment":
		return "img"
	}
	return "str"
}

// fieldLabel localizes a raw label: the host labeler, then the catalog's
// i18n, then (for an unresolved i18n key) nothing.
func fieldLabel(cat ModelCatalog, labels CatalogLabeler, model, key, raw string) string {
	raw = strings.TrimSpace(raw)
	if labels != nil {
		if l := strings.TrimSpace(labels.FieldLabel(model, key, raw)); l != "" && !looksLikeI18nKey(l) {
			return l
		}
	}
	if raw == "" {
		return ""
	}
	if cat != nil {
		if t := strings.TrimSpace(cat.Translate(raw)); t != "" && !looksLikeI18nKey(t) {
			return t
		}
	}
	if looksLikeI18nKey(raw) {
		return ""
	}
	return raw
}

func enumLabel(cat ModelCatalog, labels CatalogLabeler, model, field, value, raw string) string {
	if el, ok := labels.(enumLabeler); ok && labels != nil {
		if l := strings.TrimSpace(el.EnumValueLabel(model, field, value)); l != "" && l != value && !looksLikeI18nKey(l) {
			return l
		}
	}
	return fieldLabel(cat, nil, model, field, raw)
}

func looksLikeI18nKey(s string) bool {
	return strings.Contains(s, ".") && !strings.Contains(s, " ")
}

func schemaString(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case []string:
		return strings.Join(t, ", ")
	}
	return ""
}

func schemaStrings(v any) []string {
	switch t := v.(type) {
	case string:
		var out []string
		for _, s := range strings.Split(t, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return t
	case []any:
		var out []string
		for _, s := range t {
			if str, ok := s.(string); ok && strings.TrimSpace(str) != "" {
				out = append(out, strings.TrimSpace(str))
			}
		}
		return out
	}
	return nil
}

// modelSynonyms derives the extra names of a model deterministically: curated
// ones (Schema "synonyms", ModelSynonymSource), then the singular/plural of
// its label. Names already equal to the model name or label are skipped.
func modelSynonyms(cat ModelCatalog, m ModelMeta, cm CatalogModel) []string {
	var cands []string
	cands = append(cands, schemaStrings(m.Schema["synonyms"])...)
	if src, ok := cat.(ModelSynonymSource); ok && cat != nil {
		cands = append(cands, src.ModelSynonyms(m.Key)...)
		if cm.Name != m.Key {
			cands = append(cands, src.ModelSynonyms(cm.Name)...)
		}
	}
	if m.Title != "" && cat != nil {
		if t := strings.TrimSpace(cat.Translate(m.Title)); t != "" && !looksLikeI18nKey(t) {
			cands = append(cands, t)
		}
	}
	if cm.Label != "" {
		cands = append(cands, strings.ToLower(inflectFirstWord(cm.Label, singularES)))
	}
	seen := map[string]bool{foldText(cm.Name): true, foldText(cm.Label): true, foldText(humanizeSnake(cm.Name)): true}
	var out []string
	for _, c := range cands {
		c = strings.TrimSpace(c)
		f := foldText(c)
		if c == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, c)
	}
	return out
}

// CardOptions bound the size of a rendered card.
type CardOptions struct {
	// MaxFields keeps the most informative fields (required, title-like,
	// relations, enums, money, dates) and notes how many were left out.
	// Zero renders every field.
	MaxFields int
	// MaxSynonyms caps the header synonyms (default 4).
	MaxSynonyms int
	// MaxEnum caps the enum values shown per field (default 8).
	MaxEnum int
}

// titleLikeFields are the columns that usually name a record.
var titleLikeFields = map[string]bool{
	"name": true, "title": true, "label": true, "folio": true, "code": true, "number": true,
	"sku": true, "email": true, "display_name": true, "full_name": true, "reference": true,
}

func fieldPriority(f CatalogField, searchIn map[string]bool) int {
	p := 0
	if titleLikeFields[f.Name] {
		p += 4
	}
	if f.Required {
		p += 3
	}
	switch {
	case f.Rel == "m2o":
		p += 2
	case len(f.Enum) > 0, f.Type == "money":
		p += 2
	case f.Type == "date" || f.Type == "dt" || f.Type == "num" || f.Type == "int":
		p++
	case f.Rel != "", f.Type == "json", f.Type == "img":
		p -= 2
	}
	if searchIn[f.Name] {
		p++
	}
	if f.Filterable {
		p++
	}
	return p
}

// RenderCard renders m as an ERP-Card.
func RenderCard(m CatalogModel, opt CardOptions) string {
	if opt.MaxSynonyms <= 0 {
		opt.MaxSynonyms = 4
	}
	if opt.MaxEnum <= 0 {
		opt.MaxEnum = 8
	}
	var b strings.Builder
	b.WriteString(m.Name)
	if m.Label != "" {
		b.WriteString(` "` + m.Label + `"`)
	}
	if syn := m.Synonyms; len(syn) > 0 {
		if len(syn) > opt.MaxSynonyms {
			syn = syn[:opt.MaxSynonyms]
		}
		b.WriteString(" (" + strings.Join(syn, ", ") + ")")
	}
	if m.Vector {
		b.WriteString(" [vector_search]")
	}

	fields := m.Fields
	omitted := 0
	if opt.MaxFields > 0 && len(fields) > opt.MaxFields {
		search := map[string]bool{}
		for _, s := range m.SearchIn {
			search[s] = true
		}
		idx := make([]int, len(fields))
		for i := range idx {
			idx[i] = i
		}
		sort.SliceStable(idx, func(a, b int) bool {
			return fieldPriority(fields[idx[a]], search) > fieldPriority(fields[idx[b]], search)
		})
		keep := idx[:opt.MaxFields]
		sort.Ints(keep)
		picked := make([]CatalogField, 0, len(keep))
		for _, i := range keep {
			picked = append(picked, fields[i])
		}
		omitted = len(fields) - len(picked)
		fields = picked
	}
	parts := make([]string, 0, len(fields)+1)
	for _, f := range fields {
		parts = append(parts, renderCardField(f, opt.MaxEnum))
	}
	if omitted > 0 {
		parts = append(parts, fmt.Sprintf("+%d campos (describe_model)", omitted))
	}
	if len(parts) > 0 {
		b.WriteString("\n  " + strings.Join(parts, " | "))
	}
	return b.String()
}

func renderCardField(f CatalogField, maxEnum int) string {
	var b strings.Builder
	b.WriteString(f.Name)
	b.WriteByte(':')
	switch {
	case f.Rel == "m2o":
		b.WriteString("->" + f.Ref)
	case f.Rel == "o2m":
		b.WriteString("[]" + f.Ref)
	case f.Rel == "m2m":
		b.WriteString("<>" + f.Ref)
	case len(f.Enum) > 0:
		vals := make([]string, 0, len(f.Enum))
		for i, o := range f.Enum {
			if i >= maxEnum {
				vals = append(vals, "…")
				break
			}
			v := o.Value
			if o.Label != "" && foldText(o.Label) != foldText(o.Value) && foldText(o.Label) != foldText(humanizeSnake(o.Value)) {
				v += "=" + o.Label
			}
			vals = append(vals, v)
		}
		b.WriteString("enum(" + strings.Join(vals, "|") + ")")
	case f.Type == "money" && f.Currency != "":
		b.WriteString("money(" + f.Currency + ")")
	default:
		b.WriteString(f.Type)
	}
	if f.Label != "" {
		b.WriteString(` "` + f.Label + `"`)
	}
	if f.Required {
		b.WriteByte('*')
	}
	if f.Readonly {
		b.WriteString(" ro")
	}
	return b.String()
}

// withoutFields returns m without the fields hidden by mask (column read
// permissions), so a card never reveals a column the caller cannot read.
func (m CatalogModel) withoutFields(mask columnMask) CatalogModel {
	if len(mask) == 0 {
		return m
	}
	out := m
	out.Fields = make([]CatalogField, 0, len(m.Fields))
	for _, f := range m.Fields {
		if !mask.denied(f.Name) {
			out.Fields = append(out.Fields, f)
		}
	}
	out.SearchIn = mask.allowed(m.SearchIn)
	return out
}

// field finds a field by exact name.
func (m CatalogModel) field(name string) (CatalogField, bool) {
	for _, f := range m.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return CatalogField{}, false
}

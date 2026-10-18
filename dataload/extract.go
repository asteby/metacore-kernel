package dataload

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Etapa extract: un documento declarativo de reglas que lee campos de texto de
// cada fila (regex RE2), captura a campos, normaliza, aplica reglas en orden y
// marca "revisar" lo que no se puede leer con certeza. Es hermano del Profile:
// el Profile mapea columnas (y puede cargarse solo); el documento de extracción
// se enlaza por profile.name + profile.version.
//
// El motor no conoce ningún giro. Los vocabularios (marcas, tipos de pieza,
// categorías...), las plantillas de etiquetas y los códigos de revisión viven en el
// documento. Formato: "metacore.dataload.extract/v1".
//
//	{
//	  "schema": "metacore.dataload.extract/v1",
//	  "profile": {"name": "...", "version": "..."},
//	  "text":  {"source": "<campo del registro>", "normalize": ["upper","collapse_spaces"], "dropTokens": ["-"]},
//	  "rules": [ ... en orden: capture | entity | tokens | prefix | set ... ],
//	  "vocab": { "<nombre>": lista de textos | diccionario alias->valor | lista de {id,label,aliases} },
//	  "categories": { ... },
//	  "outputs": { ... },
//	  "checks": [ ... ], "datasetChecks": [ ... ],
//	  "review": {"revisar": [códigos], "info": [códigos]}
//	}
//
// Los patrones usan el subconjunto común de RE2 y JS: sin lookaround ni
// retrovínculos; grupos con nombre (?<n>...). Los campos que una regla extrae
// ("variables") se leen en las plantillas con {nombre}.
const ExtractSchema = "metacore.dataload.extract/v1"

// Severidades de revisión.
const (
	// SeverityRevisar bloquea la carga automática de la fila: queda en espera de
	// una decisión explícita (ver ReviewStore).
	SeverityRevisar = "revisar"
	// SeverityInfo solo informa; la fila se carga normal.
	SeverityInfo = "info"
)

// ProfileRef enlaza un documento de extracción con la versión del perfil que complementa.
type ProfileRef struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ExtractDoc es el documento de extracción tal como se guarda y se hashea.
type ExtractDoc struct {
	Schema        string         `json:"schema"`
	Profile       ProfileRef     `json:"profile"`
	Text          ExtractText    `json:"text"`
	Rules         []ExtractRule  `json:"rules,omitempty"`
	Vocab         map[string]any `json:"vocab,omitempty"`
	Categories    *ExtractCats   `json:"categories,omitempty"`
	Outputs       ExtractOutputs `json:"outputs"`
	Checks        []ExtractCheck `json:"checks,omitempty"`
	DatasetChecks []DatasetCheck `json:"datasetChecks,omitempty"`
	Review        ReviewCodes    `json:"review"`
}

// ExtractText dice de dónde se lee y cómo se normaliza el texto de trabajo.
type ExtractText struct {
	// Source es el campo del registro (ruta con puntos) que se lee.
	Source string `json:"source"`
	// Normalize son reglas de limpieza del paquete (upper, collapse_spaces, trim...).
	Normalize []string `json:"normalize,omitempty"`
	// DropTokens son palabras sueltas que se descartan del título y de las piezas
	// leídas (p. ej. "-" como relleno del origen).
	DropTokens []string `json:"dropTokens,omitempty"`
}

// ReviewCodes asigna severidad a cada código que el documento puede emitir.
type ReviewCodes struct {
	Revisar []string `json:"revisar,omitempty"`
	Info    []string `json:"info,omitempty"`
}

// Tipos de regla.
const (
	RuleCapture = "capture" // un patrón con grupos con nombre que se asignan a campos
	RuleEntity  = "entity"  // un patrón con {{vocab}}: reconoce una entidad del vocabulario más su detalle
	RuleTokens  = "tokens"  // saca de una variable palabras de un diccionario
	RulePrefix  = "prefix"  // el prefijo más largo de un vocabulario de textos
	RuleSet     = "set"     // compone un campo con una plantilla
)

// ExtractRule es una regla; los campos que aplican dependen de Kind.
type ExtractRule struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`

	// capture y entity
	Pattern string `json:"pattern,omitempty"`

	// capture
	// Consume quita lo leído del texto de trabajo para las reglas siguientes.
	Consume bool `json:"consume,omitempty"`
	// Title: "remove" quita lo leído del título; "keep" (o vacío) lo deja.
	Title string `json:"title,omitempty"`
	// Into (capture): campo -> CaptureInto. Into (tokens, prefix): nombre del campo destino.
	Into     json.RawMessage `json:"into,omitempty"`
	ReviewIf []ReviewIf      `json:"reviewIf,omitempty"`

	// entity
	Vocab        string        `json:"vocab,omitempty"`
	EntityGroup  string        `json:"entityGroup,omitempty"`
	DetailGroup  string        `json:"detailGroup,omitempty"`
	RestGroup    string        `json:"restGroup,omitempty"`
	Emit         *EntityEmit   `json:"emit,omitempty"`
	Guards       *EntityGuards `json:"guards,omitempty"`
	RestVariable string        `json:"restVariable,omitempty"`

	// tokens y prefix
	From       string   `json:"from,omitempty"`
	Dictionary string   `json:"dictionary,omitempty"`
	Order      []string `json:"order,omitempty"`
	Join       string   `json:"join,omitempty"`
	Rest       string   `json:"rest,omitempty"`
	// UnknownCode se emite cuando prefix no encuentra ninguna entrada del vocabulario.
	UnknownCode string `json:"unknownCode,omitempty"`

	// set
	Field    string   `json:"field,omitempty"`
	Template string   `json:"template,omitempty"`
	Requires []string `json:"requires,omitempty"`
	// Post son operaciones sobre el resultado: collapse, slug, upper, lower.
	Post []string `json:"post,omitempty"`
}

// CaptureInto asigna un grupo del patrón a un campo.
type CaptureInto struct {
	Group string `json:"group"`
	// As: text (default), int, flag (verdadero si el grupo participó) o list (con Split).
	As    string `json:"as,omitempty"`
	Split string `json:"split,omitempty"`
	// Null son valores del origen que significan "sin dato": el campo no se escribe.
	Null []string `json:"null,omitempty"`
}

// ReviewIf emite un código cuando se cumple una condición sobre lo capturado.
type ReviewIf struct {
	// When: listLengthGt1 (la lista del campo Into trae más de un elemento).
	When   string `json:"when"`
	Into   string `json:"into"`
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// EntityEmit nombra los campos que escribe una regla entity al reconocer la entidad.
type EntityEmit struct {
	ID     string `json:"id,omitempty"`     // id del vocabulario
	Label  string `json:"label,omitempty"`  // etiqueta del vocabulario
	Detail string `json:"detail,omitempty"` // texto del detalle (sin los tokens descartados)
}

// EntityGuards son las condiciones para dar por cierto el detalle de una entidad.
type EntityGuards struct {
	// MaxTokens: más palabras que esto vuelve sospechoso el detalle (0 = sin tope).
	MaxTokens int `json:"maxTokens,omitempty"`
	// RejectPattern: un detalle que lo cumple es sospechoso (calificadores).
	RejectPattern string `json:"rejectPattern,omitempty"`
	// AllowIn: por id de entidad, alias de OTRAS entidades que sí pueden aparecer en su detalle.
	AllowIn map[string][]string `json:"allowIn,omitempty"`
	// SuspectCode se emite con detalle vacío, largo o rechazado; AmbiguousCode cuando
	// el detalle contiene otra entidad del vocabulario.
	SuspectCode   string `json:"suspectCode,omitempty"`
	AmbiguousCode string `json:"ambiguousCode,omitempty"`
}

// ExtractCats traduce una categoría de origen a la del destino.
type ExtractCats struct {
	// Source es el campo con la categoría de origen; Into el campo destino.
	Source string `json:"source"`
	Into   string `json:"into"`
	// UnmappedCode se emite cuando la categoría no existe o mapea a null.
	UnmappedCode string                `json:"unmappedCode,omitempty"`
	Map          map[string]CategoryTo `json:"map"`
}

// CategoryTo es el destino de una categoría de origen. ID null = sin equivalente.
type CategoryTo struct {
	ID     *string          `json:"id"`
	Refine []CategoryRefine `json:"refine,omitempty"`
	// Set escribe variables fijas cuando la categoría coincide (p. ej. marcar
	// "sin entidad es normal" para los checks).
	Set map[string]any `json:"set,omitempty"`
}

// CategoryRefine cambia el id si el campo (variable) cumple el patrón.
type CategoryRefine struct {
	Field string `json:"field"`
	Match string `json:"match"`
	ID    string `json:"id"`
}

// ExtractOutputs dice qué se escribe en el registro.
type ExtractOutputs struct {
	// Title es el campo del registro que recibe el título limpio.
	Title string `json:"title,omitempty"`
	// Attributes: nombre -> variable (o campo del registro) | {field, unless}.
	Attributes map[string]AttrOut `json:"attributes,omitempty"`
	// AttributesTarget es el mapa del registro donde van; default "attributes".
	AttributesTarget string `json:"attributesTarget,omitempty"`
	// Lists son salidas de lista o de texto unido (etiquetas, tablas).
	Lists []ListOut `json:"lists,omitempty"`
	// Drop quita campos intermedios del registro al terminar.
	Drop []string `json:"drop,omitempty"`
}

// AttrOut es un atributo de salida; admite la forma corta (solo el nombre del campo).
type AttrOut struct {
	Field string `json:"field"`
	// Unless omite el atributo si la fila emitió alguno de esos códigos.
	Unless []string `json:"unless,omitempty"`
}

// UnmarshalJSON admite "campo" o {"field":"campo","unless":[...]}.
func (a *AttrOut) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*a = AttrOut{Field: s}
		return nil
	}
	type plain AttrOut
	var p plain
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return err
	}
	*a = AttrOut(p)
	return nil
}

// MarshalJSON usa la forma corta cuando no hay condiciones.
func (a AttrOut) MarshalJSON() ([]byte, error) {
	if len(a.Unless) == 0 {
		return json.Marshal(a.Field)
	}
	type plain AttrOut
	return json.Marshal(plain(a))
}

// ListOut agrega una cadena renderizada a un destino del registro.
type ListOut struct {
	// Target es la ruta del registro ("tags", "attributes.compatibilidad").
	Target   string `json:"target"`
	Template string `json:"template"`
	// Each repite la plantilla para cada entero de From a To (inclusive).
	Each *EachRange `json:"each,omitempty"`
	// Join: vacío = el destino es una lista de textos; con valor, un solo texto unido.
	Join string `json:"join,omitempty"`
	// Requires: variables que deben existir. RequiresNot: que NO deben existir.
	Requires    []string `json:"requires,omitempty"`
	RequiresNot []string `json:"requiresNot,omitempty"`
	// Unless omite la salida si la fila emitió alguno de esos códigos.
	Unless []string `json:"unless,omitempty"`
}

// EachRange recorre un rango entero declarado por dos variables.
type EachRange struct {
	As   string `json:"as"`
	From string `json:"from"`
	To   string `json:"to"`
	// MaxSpan evita expansiones absurdas; 0 = 200.
	MaxSpan int `json:"maxSpan,omitempty"`
}

// Tipos de comprobación por fila.
const (
	CheckSum         = "sum"         // total ≈ suma de partes (tolerance)
	CheckGreater     = "greater"     // left > right
	CheckRawPattern  = "rawPattern"  // la celda ORIGINAL de la columna cumple el patrón
	CheckWholeNumber = "wholeNumber" // los campos numéricos son enteros
	CheckNonNegative = "nonNegative" // los campos numéricos no son negativos
	CheckRange       = "range"       // from <= to y ambos dentro de [min, max], con maxSpan
	CheckWhen        = "when"        // todas las condiciones de All se cumplen
)

// ExtractCheck es una comprobación por fila; los campos que aplican dependen de Type.
type ExtractCheck struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Code string `json:"code"`
	// Detail es el motivo mostrado a la persona; vacío = un texto estándar del tipo.
	Detail string `json:"detail,omitempty"`

	// sum
	Total     string   `json:"total,omitempty"`
	Parts     []string `json:"parts,omitempty"`
	Tolerance float64  `json:"tolerance,omitempty"`
	// greater
	Left  string `json:"left,omitempty"`
	Right string `json:"right,omitempty"`
	// rawPattern (sin distinguir mayúsculas), wholeNumber, nonNegative
	Fields  []string `json:"fields,omitempty"`
	Pattern string   `json:"pattern,omitempty"`
	// range
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Min     *int   `json:"min,omitempty"`
	Max     *int   `json:"max,omitempty"`
	MaxSpan int    `json:"maxSpan,omitempty"`
	// when
	All []Condition `json:"all,omitempty"`
	// Field es el campo que se muestra como origen del motivo en las
	// comprobaciones when (las demás usan los suyos).
	Field string `json:"field,omitempty"`
}

// Condition mira si una variable (o campo del registro) existe.
type Condition struct {
	Field string `json:"field"`
	// Is: "set" (existe y no está vacía) o "unset".
	Is string `json:"is"`
}

// DatasetCheck es una comprobación sobre todo el archivo.
type DatasetCheck struct {
	ID   string `json:"id"`
	Type string `json:"type"` // unique
	// Field es la ruta del registro cuyo valor no puede repetirse.
	Field  string `json:"field"`
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// ReviewItem es un motivo por el que una fila necesita (o merece) la mirada de una persona.
type ReviewItem struct {
	Code     string `json:"code"`
	Field    string `json:"field,omitempty"`
	Severity string `json:"severity"`
	// Detail es el texto del motivo: el que escribió el autor del documento o, si
	// no escribió ninguno, el texto por omisión de DetailCode.
	Detail string `json:"detail,omitempty"`
	// DetailCode y DetailParams identifican el motivo por omisión del motor para
	// traducirlo (vacíos si el documento trae su propio detail).
	DetailCode   Code   `json:"detailCode,omitempty"`
	DetailParams Params `json:"detailParams,omitempty"`
}

// ParseExtract decodifica y valida un documento de extracción (rechaza campos
// desconocidos, como ParseProfile) y lo compila.
func ParseExtract(b []byte) (*Extractor, error) {
	var d ExtractDoc
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return nil, wrapErr(err, CodeExtractParse)
	}
	return NewExtractor(d)
}

// Extractor es un documento de extracción validado y con sus patrones compilados.
type Extractor struct {
	Doc  ExtractDoc
	hash string

	norm      []string
	severity  map[string]string
	rules     []compiledRule
	vocabTxt  map[string][]string
	vocabDict map[string]map[string]string
	vocabEnt  map[string]*entityVocab
	cats      map[string]*compiledCat
	outputs   []compiledList
	attrNames []string
	checks    []compiledCheck
	drop      map[string]bool
}

// Hash es la huella estable del documento (JSON canónico).
func (e *Extractor) Hash() string { return e.hash }

// Profile devuelve el perfil al que se enlaza el documento.
func (e *Extractor) Profile() ProfileRef { return e.Doc.Profile }

// CheckProfile exige que el documento corresponda a esta versión del perfil.
func (e *Extractor) CheckProfile(p Profile) error {
	if e.Doc.Profile.Name != p.Name || e.Doc.Profile.Version != p.Version {
		return newErr(CodeExtractProfileMismatch, "extract", e.Doc.Profile.Name+"@"+e.Doc.Profile.Version, "profile", p.Name+"@"+p.Version)
	}
	return nil
}

type entityEntry struct {
	ID, Label string
	Aliases   []string
}

type entityVocab struct {
	byAlias map[string]entityEntry
	alt     string // alternativas ordenadas, largas primero, escapadas
	anyRe   *regexp.Regexp
}

type compiledRule struct {
	ExtractRule
	re      *regexp.Regexp
	into    map[string]CaptureInto
	intoTxt string
	suspect *regexp.Regexp
	tmpl    *template
	idx     map[string]int // nombre de grupo -> índice
}

type compiledCat struct {
	CategoryTo
	refine []*regexp.Regexp
}

type compiledList struct {
	ListOut
	tmpl *template
}

type compiledCheck struct {
	ExtractCheck
	re *regexp.Regexp
}

// NewExtractor valida y compila un documento.
func NewExtractor(d ExtractDoc) (*Extractor, error) {
	e := &Extractor{
		Doc: d, severity: map[string]string{}, vocabTxt: map[string][]string{}, vocabDict: map[string]map[string]string{},
		vocabEnt: map[string]*entityVocab{}, cats: map[string]*compiledCat{}, drop: map[string]bool{},
	}
	var errs []Message
	bad := func(code Code, kv ...any) { errs = append(errs, msg(code, kv...)) }

	if d.Schema != ExtractSchema {
		bad(CodeExtractSchema, "schema", d.Schema, "want", ExtractSchema)
	}
	if d.Profile.Name == "" {
		bad(CodeExtractMissing, "where", "", "field", "profile.name")
	}
	if d.Text.Source == "" {
		bad(CodeExtractMissing, "where", "", "field", "text.source")
	}
	for _, r := range d.Text.Normalize {
		if !validCleanRule(r) {
			bad(CodeExtractAt, "where", "text.normalize", "detail", cleanRuleError(r))
		}
	}
	e.norm = d.Text.Normalize

	for _, c := range d.Review.Revisar {
		e.severity[c] = SeverityRevisar
	}
	for _, c := range d.Review.Info {
		if e.severity[c] == SeverityRevisar {
			bad(CodeExtractCodeTwoLevels, "code", c)
		}
		e.severity[c] = SeverityInfo
	}
	known := func(where, code string) {
		if code != "" && e.severity[code] == "" {
			bad(CodeExtractCodeNoSeverity, "where", where, "code", code)
		}
	}

	e.compileVocab(&errs)

	ids := map[string]bool{}
	for i, r := range d.Rules {
		where := fmt.Sprintf("rules[%d] %q", i, r.ID)
		if r.ID == "" {
			bad(CodeExtractMissing, "where", fmt.Sprintf("rules[%d]: ", i), "field", "id")
		} else if ids[r.ID] {
			bad(CodeExtractDuplicateID, "where", where)
		}
		ids[r.ID] = true
		cr := compiledRule{ExtractRule: r, idx: map[string]int{}}
		e.compileRule(&cr, where, bad, known)
		e.rules = append(e.rules, cr)
	}

	if c := d.Categories; c != nil {
		if c.Source == "" || c.Into == "" {
			bad(CodeExtractMissing, "where", "categories: ", "field", "source/into")
		}
		known("categories", c.UnmappedCode)
		for src, to := range c.Map {
			cc := &compiledCat{CategoryTo: to}
			for j, rf := range to.Refine {
				re, err := compileRE2(rf.Match)
				if err != nil {
					bad(CodeExtractAt, "where", fmt.Sprintf("categories.%s.refine[%d]", src, j), "detail", err)
				}
				if rf.Field == "" || rf.ID == "" {
					bad(CodeExtractMissing, "where", fmt.Sprintf("categories.%s.refine[%d]: ", src, j), "field", "field/id")
				}
				cc.refine = append(cc.refine, re)
			}
			e.cats[src] = cc
		}
	}

	e.compileOutputs(&errs, known)
	e.compileChecks(&errs, known)
	for i, c := range d.DatasetChecks {
		if c.Type != "unique" || c.Field == "" {
			bad(CodeExtractDatasetUnique, "where", fmt.Sprintf("datasetChecks[%d]", i))
		}
		known(fmt.Sprintf("datasetChecks[%d]", i), c.Code)
	}
	for _, f := range d.Outputs.Drop {
		e.drop[f] = true
	}

	if err := detailsErr(errs, CodeExtractInvalid, "name", d.Profile.Name); err != nil {
		return nil, err
	}
	e.hash = canonicalHash(d)
	return e, nil
}

func canonicalHash(v any) string {
	b, _ := json.Marshal(v)
	var generic any
	_ = json.Unmarshal(b, &generic)
	b, _ = json.Marshal(generic) // los mapas salen con llaves ordenadas
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// compileRE2 compila con regexp de Go (RE2). Los grupos con nombre (?<n>...) los
// acepta Go 1.22+.
func compileRE2(p string) (*regexp.Regexp, error) {
	if p == "" {
		return nil, newErr(CodePatternEmpty)
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return nil, newErr(CodePatternInvalid, "pattern", p, "error", err.Error())
	}
	return re, nil
}

func (e *Extractor) compileVocab(errs *[]Message) {
	bad := func(code Code, kv ...any) { *errs = append(*errs, msg(code, kv...)) }
	names := make([]string, 0, len(e.Doc.Vocab))
	for n := range e.Doc.Vocab {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		switch v := e.Doc.Vocab[name].(type) {
		case map[string]any:
			dict := make(map[string]string, len(v))
			for k, val := range v {
				s, ok := val.(string)
				if !ok {
					bad(CodeVocabValueText, "vocab", name, "key", k)
					continue
				}
				dict[k] = s
			}
			e.vocabDict[name] = dict
		case []any:
			if len(v) == 0 {
				bad(CodeVocabEmpty, "vocab", name)
				continue
			}
			switch v[0].(type) {
			case string:
				list := make([]string, 0, len(v))
				for _, it := range v {
					s, ok := it.(string)
					if !ok {
						bad(CodeVocabMixed, "vocab", name)
						break
					}
					list = append(list, s)
				}
				e.vocabTxt[name] = list
			case map[string]any:
				ev := &entityVocab{byAlias: map[string]entityEntry{}}
				for i, it := range v {
					m, ok := it.(map[string]any)
					if !ok {
						bad(CodeVocabEntityShape, "vocab", fmt.Sprintf("%s[%d]", name, i))
						continue
					}
					en := entityEntry{ID: str(m["id"]), Label: str(m["label"])}
					if arr, ok := m["aliases"].([]any); ok {
						for _, a := range arr {
							en.Aliases = append(en.Aliases, str(a))
						}
					}
					if en.ID == "" || len(en.Aliases) == 0 {
						bad(CodeVocabEntityMissing, "vocab", fmt.Sprintf("%s[%d]", name, i))
						continue
					}
					if en.Label == "" {
						en.Label = en.ID
					}
					for _, a := range en.Aliases {
						if prev, dup := ev.byAlias[a]; dup {
							bad(CodeVocabAliasDup, "vocab", name, "alias", a, "first", prev.ID, "second", en.ID)
						}
						ev.byAlias[a] = en
					}
				}
				aliases := make([]string, 0, len(ev.byAlias))
				for a := range ev.byAlias {
					aliases = append(aliases, a)
				}
				sort.Slice(aliases, func(i, j int) bool {
					if len(aliases[i]) != len(aliases[j]) {
						return len(aliases[i]) > len(aliases[j])
					}
					return aliases[i] < aliases[j]
				})
				esc := make([]string, len(aliases))
				for i, a := range aliases {
					esc[i] = regexp.QuoteMeta(a)
				}
				ev.alt = strings.Join(esc, "|")
				if ev.alt != "" {
					ev.anyRe = regexp.MustCompile(`(?:^|\s)(` + ev.alt + `)(?:\s|$)`)
				}
				e.vocabEnt[name] = ev
			default:
				bad(CodeVocabUnsupported, "vocab", name)
			}
		default:
			bad(CodeVocabShape, "vocab", name)
		}
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func (e *Extractor) compileRule(cr *compiledRule, where string, bad func(Code, ...any), known func(string, string)) {
	r := cr.ExtractRule
	switch r.Kind {
	case RuleCapture:
		re, err := compileRE2(r.Pattern)
		if err != nil {
			bad(CodeExtractAt, "where", where, "detail", err)
			return
		}
		cr.re = re
		for i, n := range re.SubexpNames() {
			if n != "" {
				cr.idx[n] = i
			}
		}
		if r.Title != "" && r.Title != "remove" && r.Title != "keep" {
			bad(CodeExtractTitle, "where", where)
		}
		if len(r.Into) > 0 {
			if err := decodeStrict(r.Into, &cr.into); err != nil {
				bad(CodeExtractAt, "where", where+": into", "detail", err.Error())
			}
			for field, spec := range cr.into {
				if _, ok := cr.idx[spec.Group]; !ok {
					bad(CodeExtractNoGroup, "where", where+": into."+field, "group", spec.Group)
				}
				switch spec.As {
				case "", "text", "int", "flag":
				case "list":
					if spec.Split == "" {
						bad(CodeExtractMissing, "where", where+": into."+field+": list → ", "field", "split")
					} else if _, err := compileRE2(spec.Split); err != nil {
						bad(CodeExtractAt, "where", where+": into."+field+": split", "detail", err)
					}
				default:
					bad(CodeExtractUnknown, "where", where+": into."+field, "what", "as", "value", spec.As)
				}
			}
		}
		for _, ri := range r.ReviewIf {
			if ri.When != "listLengthGt1" {
				bad(CodeExtractUnknown, "where", where, "what", "reviewIf.when", "value", ri.When)
			}
			if _, ok := cr.into[ri.Into]; !ok {
				bad(CodeExtractReviewIfInto, "where", where, "field", ri.Into)
			}
			known(where, ri.Code)
		}
	case RuleEntity:
		ev := e.vocabEnt[r.Vocab]
		if ev == nil {
			bad(CodeExtractEntityVocab, "where", where, "vocab", r.Vocab)
			return
		}
		if !strings.Contains(r.Pattern, "{{vocab}}") {
			bad(CodeExtractVocabPlaceholder, "where", where)
			return
		}
		re, err := compileRE2(strings.ReplaceAll(r.Pattern, "{{vocab}}", ev.alt))
		if err != nil {
			bad(CodeExtractAt, "where", where, "detail", err)
			return
		}
		cr.re = re
		for i, n := range re.SubexpNames() {
			if n != "" {
				cr.idx[n] = i
			}
		}
		for _, g := range []string{r.EntityGroup, r.RestGroup} {
			if _, ok := cr.idx[g]; !ok {
				bad(CodeExtractNoGroup, "where", where, "group", g)
			}
		}
		if r.DetailGroup != "" {
			if _, ok := cr.idx[r.DetailGroup]; !ok {
				bad(CodeExtractNoGroup, "where", where, "group", r.DetailGroup)
			}
		}
		if g := r.Guards; g != nil {
			if g.RejectPattern != "" {
				if cr.suspect, err = compileRE2(g.RejectPattern); err != nil {
					bad(CodeExtractAt, "where", where+": guards.rejectPattern", "detail", err)
				}
			}
			known(where, g.SuspectCode)
			known(where, g.AmbiguousCode)
		}
	case RuleTokens, RulePrefix:
		if r.From == "" || r.Dictionary == "" {
			bad(CodeExtractMissing, "where", where+": ", "field", "from/dictionary")
			return
		}
		if err := json.Unmarshal(r.Into, &cr.intoTxt); err != nil || cr.intoTxt == "" {
			bad(CodeExtractIntoName, "where", where)
		}
		if r.Kind == RuleTokens && e.vocabDict[r.Dictionary] == nil {
			bad(CodeExtractDictionary, "where", where, "dictionary", r.Dictionary)
		}
		if r.Kind == RulePrefix {
			if e.vocabTxt[r.Dictionary] == nil {
				bad(CodeExtractTextList, "where", where, "dictionary", r.Dictionary)
			}
			known(where, r.UnknownCode)
		}
	case RuleSet:
		if r.Field == "" || r.Template == "" {
			bad(CodeExtractMissing, "where", where+": ", "field", "field/template")
			return
		}
		t, err := parseTemplate(r.Template)
		if err != nil {
			bad(CodeExtractAt, "where", where, "detail", err)
		}
		cr.tmpl = t
		for _, p := range r.Post {
			if !validPost(p) {
				bad(CodeExtractUnknown, "where", where, "what", "post (collapse, slug, upper, lower)", "value", p)
			}
		}
	default:
		bad(CodeExtractRuleKind, "where", where, "kind", r.Kind)
	}
}

func decodeStrict(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func (e *Extractor) compileOutputs(errs *[]Message, known func(string, string)) {
	o := e.Doc.Outputs
	bad := func(code Code, kv ...any) { *errs = append(*errs, msg(code, kv...)) }
	names := make([]string, 0, len(o.Attributes))
	for n, a := range o.Attributes {
		if a.Field == "" {
			bad(CodeExtractMissing, "where", "outputs.attributes."+n+": ", "field", "field")
		}
		for _, c := range a.Unless {
			known("outputs.attributes."+n, c)
		}
		names = append(names, n)
	}
	sort.Strings(names)
	e.attrNames = names
	for i, l := range o.Lists {
		where := fmt.Sprintf("outputs.lists[%d]", i)
		if l.Target == "" || l.Template == "" {
			bad(CodeExtractMissing, "where", where+": ", "field", "target/template")
			continue
		}
		t, err := parseTemplate(l.Template)
		if err != nil {
			bad(CodeExtractAt, "where", where, "detail", err)
		}
		if l.Each != nil && (l.Each.As == "" || l.Each.From == "" || l.Each.To == "") {
			bad(CodeExtractMissing, "where", where+": each → ", "field", "as/from/to")
		}
		for _, c := range l.Unless {
			known(where, c)
		}
		e.outputs = append(e.outputs, compiledList{ListOut: l, tmpl: t})
	}
}

func (e *Extractor) compileChecks(errs *[]Message, known func(string, string)) {
	bad := func(code Code, kv ...any) { *errs = append(*errs, msg(code, kv...)) }
	ids := map[string]bool{}
	for i, c := range e.Doc.Checks {
		where := fmt.Sprintf("checks[%d] %q", i, c.ID)
		if c.ID == "" || ids[c.ID] {
			bad(CodeExtractDuplicateID, "where", where)
		}
		ids[c.ID] = true
		known(where, c.Code)
		if c.Code == "" {
			bad(CodeExtractMissing, "where", where+": ", "field", "code")
		}
		cc := compiledCheck{ExtractCheck: c}
		switch c.Type {
		case CheckSum:
			if c.Total == "" || len(c.Parts) == 0 {
				bad(CodeExtractMissing, "where", where+": sum → ", "field", "total/parts")
			}
		case CheckGreater:
			if c.Left == "" || c.Right == "" {
				bad(CodeExtractMissing, "where", where+": greater → ", "field", "left/right")
			}
		case CheckRawPattern:
			// Se compara sin distinguir mayúsculas: el origen escribe "#N/D" y "#n/d".
			re, err := compileRE2("(?i)" + c.Pattern)
			if err != nil || len(c.Fields) == 0 {
				bad(CodeExtractRawPattern, "where", where, "detail", fmt.Sprint(err))
			}
			cc.re = re
		case CheckWholeNumber, CheckNonNegative:
			if len(c.Fields) == 0 {
				bad(CodeExtractMissing, "where", where+": "+c.Type+" → ", "field", "fields")
			}
		case CheckRange:
			if c.From == "" || c.To == "" {
				bad(CodeExtractMissing, "where", where+": range → ", "field", "from/to")
			}
		case CheckWhen:
			if len(c.All) == 0 {
				bad(CodeExtractMissing, "where", where+": when → ", "field", "all")
			}
			for _, cond := range c.All {
				if cond.Field == "" || (cond.Is != "set" && cond.Is != "unset") {
					bad(CodeExtractCondition, "where", where)
				}
			}
		default:
			bad(CodeExtractUnknown, "where", where, "what", "type", "value", c.Type)
		}
		e.checks = append(e.checks, cc)
	}
}

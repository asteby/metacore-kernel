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
// sabores...), las plantillas de etiquetas y los códigos de revisión viven en el
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
	Detail   string `json:"detail,omitempty"`
}

// ParseExtract decodifica y valida un documento de extracción (rechaza campos
// desconocidos, como ParseProfile) y lo compila.
func ParseExtract(b []byte) (*Extractor, error) {
	var d ExtractDoc
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("extracción inválida: %w", err)
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
		return fmt.Errorf("extracción %s@%s no corresponde al perfil %s@%s", e.Doc.Profile.Name, e.Doc.Profile.Version, p.Name, p.Version)
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
	var errs []string
	bad := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	if d.Schema != ExtractSchema {
		bad("schema %q no soportado (se esperaba %s)", d.Schema, ExtractSchema)
	}
	if d.Profile.Name == "" {
		bad("falta profile.name")
	}
	if d.Text.Source == "" {
		bad("falta text.source")
	}
	for _, r := range d.Text.Normalize {
		if !validCleanRule(r) {
			bad("text.normalize: %s", cleanRuleError(r))
		}
	}
	e.norm = d.Text.Normalize

	for _, c := range d.Review.Revisar {
		e.severity[c] = SeverityRevisar
	}
	for _, c := range d.Review.Info {
		if e.severity[c] == SeverityRevisar {
			bad("el código %q está en revisar e info", c)
		}
		e.severity[c] = SeverityInfo
	}
	known := func(where, code string) {
		if code != "" && e.severity[code] == "" {
			bad("%s: el código %q no tiene severidad en review", where, code)
		}
	}

	e.compileVocab(&errs)

	ids := map[string]bool{}
	for i, r := range d.Rules {
		where := fmt.Sprintf("rules[%d] %q", i, r.ID)
		if r.ID == "" {
			bad("rules[%d]: falta id", i)
		} else if ids[r.ID] {
			bad("%s: id repetido", where)
		}
		ids[r.ID] = true
		cr := compiledRule{ExtractRule: r, idx: map[string]int{}}
		e.compileRule(&cr, where, bad, known)
		e.rules = append(e.rules, cr)
	}

	if c := d.Categories; c != nil {
		if c.Source == "" || c.Into == "" {
			bad("categories: faltan source o into")
		}
		known("categories", c.UnmappedCode)
		for src, to := range c.Map {
			cc := &compiledCat{CategoryTo: to}
			for j, rf := range to.Refine {
				re, err := compileRE2(rf.Match)
				if err != nil {
					bad("categories.%s.refine[%d]: %v", src, j, err)
				}
				if rf.Field == "" || rf.ID == "" {
					bad("categories.%s.refine[%d]: faltan field o id", src, j)
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
			bad("datasetChecks[%d]: solo se soporta type unique con field", i)
		}
		known(fmt.Sprintf("datasetChecks[%d]", i), c.Code)
	}
	for _, f := range d.Outputs.Drop {
		e.drop[f] = true
	}

	if len(errs) > 0 {
		sort.Strings(errs)
		return nil, fmt.Errorf("extracción %q: %s", d.Profile.Name, strings.Join(errs, "; "))
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
		return nil, fmt.Errorf("patrón vacío")
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return nil, fmt.Errorf("patrón %q: %v", p, err)
	}
	return re, nil
}

func (e *Extractor) compileVocab(errs *[]string) {
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
					*errs = append(*errs, fmt.Sprintf("vocab.%s: el valor de %q debe ser texto", name, k))
					continue
				}
				dict[k] = s
			}
			e.vocabDict[name] = dict
		case []any:
			if len(v) == 0 {
				*errs = append(*errs, fmt.Sprintf("vocab.%s: vacío", name))
				continue
			}
			switch v[0].(type) {
			case string:
				list := make([]string, 0, len(v))
				for _, it := range v {
					s, ok := it.(string)
					if !ok {
						*errs = append(*errs, fmt.Sprintf("vocab.%s: mezcla textos y objetos", name))
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
						*errs = append(*errs, fmt.Sprintf("vocab.%s[%d]: debe ser objeto {id,label,aliases}", name, i))
						continue
					}
					en := entityEntry{ID: str(m["id"]), Label: str(m["label"])}
					if arr, ok := m["aliases"].([]any); ok {
						for _, a := range arr {
							en.Aliases = append(en.Aliases, str(a))
						}
					}
					if en.ID == "" || len(en.Aliases) == 0 {
						*errs = append(*errs, fmt.Sprintf("vocab.%s[%d]: faltan id o aliases", name, i))
						continue
					}
					if en.Label == "" {
						en.Label = en.ID
					}
					for _, a := range en.Aliases {
						if prev, dup := ev.byAlias[a]; dup {
							*errs = append(*errs, fmt.Sprintf("vocab.%s: el alias %q lo usan %s y %s", name, a, prev.ID, en.ID))
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
				*errs = append(*errs, fmt.Sprintf("vocab.%s: elementos no soportados", name))
			}
		default:
			*errs = append(*errs, fmt.Sprintf("vocab.%s: debe ser lista o diccionario", name))
		}
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func (e *Extractor) compileRule(cr *compiledRule, where string, bad func(string, ...any), known func(string, string)) {
	r := cr.ExtractRule
	switch r.Kind {
	case RuleCapture:
		re, err := compileRE2(r.Pattern)
		if err != nil {
			bad("%s: %v", where, err)
			return
		}
		cr.re = re
		for i, n := range re.SubexpNames() {
			if n != "" {
				cr.idx[n] = i
			}
		}
		if r.Title != "" && r.Title != "remove" && r.Title != "keep" {
			bad("%s: title debe ser remove o keep", where)
		}
		if len(r.Into) > 0 {
			if err := decodeStrict(r.Into, &cr.into); err != nil {
				bad("%s: into: %v", where, err)
			}
			for field, spec := range cr.into {
				if _, ok := cr.idx[spec.Group]; !ok {
					bad("%s: into.%s: el patrón no tiene el grupo %q", where, field, spec.Group)
				}
				switch spec.As {
				case "", "text", "int", "flag":
				case "list":
					if spec.Split == "" {
						bad("%s: into.%s: list necesita split", where, field)
					} else if _, err := compileRE2(spec.Split); err != nil {
						bad("%s: into.%s: split: %v", where, field, err)
					}
				default:
					bad("%s: into.%s: as %q desconocido", where, field, spec.As)
				}
			}
		}
		for _, ri := range r.ReviewIf {
			if ri.When != "listLengthGt1" {
				bad("%s: reviewIf.when %q desconocido", where, ri.When)
			}
			if _, ok := cr.into[ri.Into]; !ok {
				bad("%s: reviewIf.into %q no es un campo de into", where, ri.Into)
			}
			known(where, ri.Code)
		}
	case RuleEntity:
		ev := e.vocabEnt[r.Vocab]
		if ev == nil {
			bad("%s: vocab %q inexistente o no es una lista de entidades {id,label,aliases}", where, r.Vocab)
			return
		}
		if !strings.Contains(r.Pattern, "{{vocab}}") {
			bad("%s: el patrón debe llevar {{vocab}}", where)
			return
		}
		re, err := compileRE2(strings.ReplaceAll(r.Pattern, "{{vocab}}", ev.alt))
		if err != nil {
			bad("%s: %v", where, err)
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
				bad("%s: el patrón no tiene el grupo %q", where, g)
			}
		}
		if r.DetailGroup != "" {
			if _, ok := cr.idx[r.DetailGroup]; !ok {
				bad("%s: el patrón no tiene el grupo %q", where, r.DetailGroup)
			}
		}
		if g := r.Guards; g != nil {
			if g.RejectPattern != "" {
				if cr.suspect, err = compileRE2(g.RejectPattern); err != nil {
					bad("%s: guards.rejectPattern: %v", where, err)
				}
			}
			known(where, g.SuspectCode)
			known(where, g.AmbiguousCode)
		}
	case RuleTokens, RulePrefix:
		if r.From == "" || r.Dictionary == "" {
			bad("%s: faltan from o dictionary", where)
			return
		}
		if err := json.Unmarshal(r.Into, &cr.intoTxt); err != nil || cr.intoTxt == "" {
			bad("%s: into debe ser el nombre del campo destino", where)
		}
		if r.Kind == RuleTokens && e.vocabDict[r.Dictionary] == nil {
			bad("%s: dictionary %q no es un diccionario alias->valor", where, r.Dictionary)
		}
		if r.Kind == RulePrefix {
			if e.vocabTxt[r.Dictionary] == nil {
				bad("%s: dictionary %q no es una lista de textos", where, r.Dictionary)
			}
			known(where, r.UnknownCode)
		}
	case RuleSet:
		if r.Field == "" || r.Template == "" {
			bad("%s: faltan field o template", where)
			return
		}
		t, err := parseTemplate(r.Template)
		if err != nil {
			bad("%s: %v", where, err)
		}
		cr.tmpl = t
		for _, p := range r.Post {
			if !validPost(p) {
				bad("%s: post %q desconocido (collapse, slug, upper, lower)", where, p)
			}
		}
	default:
		bad("%s: tipo de regla desconocido %q", where, r.Kind)
	}
}

func decodeStrict(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func (e *Extractor) compileOutputs(errs *[]string, known func(string, string)) {
	o := e.Doc.Outputs
	bad := func(f string, a ...any) { *errs = append(*errs, fmt.Sprintf(f, a...)) }
	names := make([]string, 0, len(o.Attributes))
	for n, a := range o.Attributes {
		if a.Field == "" {
			bad("outputs.attributes.%s: falta el campo", n)
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
			bad("%s: faltan target o template", where)
			continue
		}
		t, err := parseTemplate(l.Template)
		if err != nil {
			bad("%s: %v", where, err)
		}
		if l.Each != nil && (l.Each.As == "" || l.Each.From == "" || l.Each.To == "") {
			bad("%s: each necesita as, from y to", where)
		}
		for _, c := range l.Unless {
			known(where, c)
		}
		e.outputs = append(e.outputs, compiledList{ListOut: l, tmpl: t})
	}
}

func (e *Extractor) compileChecks(errs *[]string, known func(string, string)) {
	bad := func(f string, a ...any) { *errs = append(*errs, fmt.Sprintf(f, a...)) }
	ids := map[string]bool{}
	for i, c := range e.Doc.Checks {
		where := fmt.Sprintf("checks[%d] %q", i, c.ID)
		if c.ID == "" || ids[c.ID] {
			bad("%s: id vacío o repetido", where)
		}
		ids[c.ID] = true
		known(where, c.Code)
		if c.Code == "" {
			bad("%s: falta code", where)
		}
		cc := compiledCheck{ExtractCheck: c}
		switch c.Type {
		case CheckSum:
			if c.Total == "" || len(c.Parts) == 0 {
				bad("%s: sum necesita total y parts", where)
			}
		case CheckGreater:
			if c.Left == "" || c.Right == "" {
				bad("%s: greater necesita left y right", where)
			}
		case CheckRawPattern:
			// Se compara sin distinguir mayúsculas: el origen escribe "#N/D" y "#n/d".
			re, err := compileRE2("(?i)" + c.Pattern)
			if err != nil || len(c.Fields) == 0 {
				bad("%s: rawPattern necesita fields y un patrón válido (%v)", where, err)
			}
			cc.re = re
		case CheckWholeNumber, CheckNonNegative:
			if len(c.Fields) == 0 {
				bad("%s: %s necesita fields", where, c.Type)
			}
		case CheckRange:
			if c.From == "" || c.To == "" {
				bad("%s: range necesita from y to", where)
			}
		case CheckWhen:
			if len(c.All) == 0 {
				bad("%s: when necesita all", where)
			}
			for _, cond := range c.All {
				if cond.Field == "" || (cond.Is != "set" && cond.Is != "unset") {
					bad("%s: condición inválida (is: set|unset)", where)
				}
			}
		default:
			bad("%s: tipo %q desconocido", where, c.Type)
		}
		e.checks = append(e.checks, cc)
	}
}

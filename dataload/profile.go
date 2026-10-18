package dataload

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/asteby/metacore-kernel/importer"
	"github.com/asteby/metacore-kernel/modelbase"
)

// Profile es el mapeo declarativo de una fuente hacia una entidad. Es solo
// datos (JSON): no contiene código ni lógica de un giro concreto.
type Profile struct {
	// Name y Version identifican el perfil; entran en la llave de idempotencia,
	// de modo que cambiar el perfil produce una corrida distinta.
	Name    string `json:"name"`
	Version string `json:"version"`
	// Entity es el tipo de registro destino (lo interpreta el Target).
	Entity string `json:"entity"`
	// Key son los campos destino cuyo valor forma la identidad natural del
	// registro (p. ej. ["external_id"] o ["sku"]). Obligatorio: sin identidad no
	// hay diff ni idempotencia.
	Key    []string `json:"key"`
	Fields []Field  `json:"fields"`
	// Defaults se escriben en todo registro; una celda explícita gana.
	Defaults map[string]any `json:"defaults,omitempty"`
	// Clean son reglas de limpieza aplicadas a todo campo de texto antes de las
	// propias de cada campo.
	Clean []string `json:"clean,omitempty"`
	// IgnoreHeaders son columnas de la fuente que se descartan y se cuentan
	// (nombre exacto sin distinguir mayúsculas, o prefijo con "*" final:
	// "Meta: _wcct_*").
	IgnoreHeaders []string `json:"ignoreHeaders,omitempty"`
	// MaxRows tope de filas por corrida; 0 = DefaultMaxRows.
	MaxRows int `json:"maxRows,omitempty"`
}

// DefaultMaxRows es el tope de filas por corrida si el perfil no dice otro.
const DefaultMaxRows = 50000

// Field mapea una o más columnas de la fuente a un campo destino.
type Field struct {
	// Target es la ruta destino (admite puntos: "price.amount").
	Target string `json:"target"`
	// From son los encabezados aceptados; el primero es el canónico.
	From     []string `json:"from"`
	Type     string   `json:"type,omitempty"` // text|number|boolean|date|email
	Required bool     `json:"required,omitempty"`
	// Clean son reglas de limpieza propias del campo.
	Clean []string `json:"clean,omitempty"`
	// Values traduce valores textuales (tras limpiar, sin distinguir
	// mayúsculas) antes de convertir el tipo: {"publish":"true","draft":"false"}.
	Values map[string]string `json:"values,omitempty"`
	// Transform es el nombre de una transformación registrada con
	// importer.RegisterTransform. Las de medios no se aceptan aquí: usa Media.
	Transform string `json:"transform,omitempty"`
	// Media marca que la celda trae una o más URLs de medios. No se descargan al
	// planificar: se encolan tras aplicar y se respaldan por sha256.
	Media bool `json:"media,omitempty"`
	// Sep separa varias URLs en una celda Media; vacío = coma, punto y coma,
	// barra vertical y salto de línea.
	Sep string `json:"sep,omitempty"`
	// Default se usa cuando la celda viene vacía.
	Default any `json:"default,omitempty"`
	// Split separa la celda (ya limpia) en una lista de textos sin vacíos:
	// "creatine, ares" con Split "," da ["creatine","ares"].
	Split string `json:"split,omitempty"`
	// Derive calcula el valor a partir de otras columnas de la fila (ver Derive).
	// Con Derive el campo no lleva From: lo alimenta el cálculo.
	Derive *Derive `json:"derive,omitempty"`
}

// Reglas de limpieza disponibles (nombres estables, parte del contrato del perfil).
const (
	CleanTrim            = "trim"              // quita espacios en los extremos
	CleanCollapseSpaces  = "collapse_spaces"   // espacios repetidos -> uno
	CleanUnescapeNewline = "unescape_newlines" // "\n" literal (doble escape) -> salto real
	CleanStripHTML       = "strip_html"        // elimina etiquetas y decodifica entidades básicas
	CleanLower           = "lower"
	CleanUpper           = "upper"
	CleanTrimQuotes      = "trim_quotes" // comillas tipográficas -> rectas y quita las de los extremos
	CleanNullIfEmptyish  = "null_if_emptyish"
	// CleanSanitizeHTML conserva solo etiquetas de texto seguras (p, br, ul, ol, li,
	// strong, b, em, i, h2-h4, a con http/https), sin atributos salvo href; quita
	// scripts, estilos y comentarios.
	CleanSanitizeHTML = "sanitize_html"
	// CleanCollapseBlankLines deja a lo más una línea en blanco seguida.
	CleanCollapseBlankLines = "collapse_blank_lines"
	// CleanAutoParagraphs envuelve en <p> cada bloque de texto que no sea ya HTML de bloque.
	CleanAutoParagraphs = "auto_paragraphs"
	// CleanDropLines es parametrizada: "drop_lines:<regex>" elimina las líneas
	// que coinciden (la regex se compila sin distinguir mayúsculas si empieza con (?i)).
	CleanDropLines = "drop_lines"
)

var (
	reTag      = regexp.MustCompile(`(?s)<[^>]*>`)
	reSpaces   = regexp.MustCompile(`[ \t\x{00A0}]+`)
	reBlankRun = regexp.MustCompile(`\n{3,}`)
	htmlRepl   = strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#039;", "'", "&#39;", "'")
	quoteRepl  = strings.NewReplacer("‘", "'", "’", "'", "“", `"`, "”", `"`)
)

// applyClean aplica las reglas en orden. Una regla desconocida es error de
// validación del perfil (ver Validate), nunca se ignora en silencio.
func applyClean(rules []string, s string) string {
	for _, r := range rules {
		if arg, ok := strings.CutPrefix(r, CleanDropLines+":"); ok {
			s = dropLines(arg, s)
			continue
		}
		switch r {
		case CleanTrim:
			s = strings.TrimSpace(s)
		case CleanCollapseSpaces:
			s = strings.TrimSpace(reSpaces.ReplaceAllString(s, " "))
		case CleanUnescapeNewline:
			s = strings.NewReplacer(`\r\n`, "\n", `\n`, "\n", `\r`, "\n", `\t`, "\t").Replace(s)
		case CleanStripHTML:
			s = strings.NewReplacer("<br>", "\n", "<br/>", "\n", "<br />", "\n", "</p>", "\n\n", "</li>", "\n").Replace(s)
			s = htmlRepl.Replace(reTag.ReplaceAllString(s, ""))
			s = reBlankRun.ReplaceAllString(strings.TrimSpace(s), "\n\n")
		case CleanLower:
			s = strings.ToLower(s)
		case CleanUpper:
			s = strings.ToUpper(s)
		case CleanTrimQuotes:
			s = strings.Trim(quoteRepl.Replace(strings.TrimSpace(s)), `"`)
		case CleanSanitizeHTML:
			s = sanitizeHTML(s)
		case CleanCollapseBlankLines:
			s = strings.TrimSpace(reBlankRun.ReplaceAllString(s, "\n\n"))
		case CleanAutoParagraphs:
			s = autoParagraphs(s)
		case CleanNullIfEmptyish:
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "n/a", "na", "null", "none", "-", "--", "undefined":
				s = ""
			}
		}
	}
	return s
}

var knownClean = map[string]bool{
	CleanTrim: true, CleanCollapseSpaces: true, CleanUnescapeNewline: true, CleanStripHTML: true,
	CleanLower: true, CleanUpper: true, CleanTrimQuotes: true, CleanNullIfEmptyish: true,
	CleanSanitizeHTML: true, CleanCollapseBlankLines: true, CleanAutoParagraphs: true,
}

// ParseProfile decodifica y valida un perfil JSON. Rechaza campos desconocidos
// para que un error de captura no pase como "campo ignorado".
func ParseProfile(b []byte) (Profile, error) {
	var p Profile
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Profile{}, fmt.Errorf("perfil inválido: %w", err)
	}
	if err := p.Validate(); err != nil {
		return Profile{}, err
	}
	return p, nil
}

// Validate revisa los errores de autoría del perfil.
func (p Profile) Validate() error {
	var errs []string
	if p.Name == "" {
		errs = append(errs, "falta name")
	}
	if p.Entity == "" {
		errs = append(errs, "falta entity")
	}
	if len(p.Key) == 0 {
		errs = append(errs, "falta key (identidad natural)")
	}
	targets := map[string]bool{}
	for i, f := range p.Fields {
		if f.Target == "" || (len(f.From) == 0 && f.Derive == nil) {
			errs = append(errs, fmt.Sprintf("fields[%d]: faltan target o from", i))
			continue
		}
		if f.Derive != nil {
			if len(f.From) > 0 {
				errs = append(errs, fmt.Sprintf("%q: con derive no se usa from", f.Target))
			}
			if f.Media {
				errs = append(errs, fmt.Sprintf("%q: derive y media son excluyentes", f.Target))
			}
			if err := f.Derive.validate(); err != nil {
				errs = append(errs, fmt.Sprintf("%q: %v", f.Target, err))
			}
		}
		if targets[f.Target] {
			errs = append(errs, fmt.Sprintf("target %q repetido", f.Target))
		}
		targets[f.Target] = true
		if f.Media && f.Transform != "" {
			errs = append(errs, fmt.Sprintf("%q: Media y Transform son excluyentes", f.Target))
		}
		if strings.HasPrefix(f.Transform, "media_url") {
			errs = append(errs, fmt.Sprintf("%q: usa media:true; las transformaciones media_url descargan al planificar", f.Target))
		}
		for _, r := range append(append([]string{}, p.Clean...), f.Clean...) {
			if !validCleanRule(r) {
				errs = append(errs, cleanRuleError(r))
			}
		}
	}
	for _, k := range p.Key {
		if !targets[k] {
			errs = append(errs, fmt.Sprintf("key %q no es un target de fields", k))
		}
	}
	for _, spec := range p.importSpec().Validate() {
		errs = append(errs, spec.Error())
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("perfil %q: %s", p.Name, strings.Join(errs, "; "))
	}
	return nil
}

// Hash es la huella estable del perfil resuelto (JSON canónico: los mapas se
// serializan con llaves ordenadas).
func (p Profile) Hash() string {
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (p Profile) maxRows() int {
	if p.MaxRows > 0 {
		return p.MaxRows
	}
	return DefaultMaxRows
}

func (p Profile) field(target string) (Field, bool) {
	for _, f := range p.Fields {
		if f.Target == target {
			return f, true
		}
	}
	return Field{}, false
}

// importSpec proyecta el perfil a la especificación del importador del kernel
// para reutilizar su coerción de tipos, alias de encabezados y transformaciones
// registradas. Los campos Media se tratan como texto: no llevan Transform.
func (p Profile) importSpec() modelbase.ImportSpec {
	spec := modelbase.ImportSpec{MaxRows: p.maxRows(), Defaults: p.Defaults}
	for _, f := range p.Fields {
		col := modelbase.ImportColumn{Key: f.Target, Type: f.Type, Required: f.Required, Transform: f.Transform}
		if f.Media {
			col.Required, col.Transform = false, "" // las URLs no entran al registro; ver normalizeRow
		}
		if h := f.headers(); len(h) > 0 {
			col.Header = h[0]
			col.Aliases = h[1:]
		}
		spec.Columns = append(spec.Columns, col)
	}
	return spec
}

// Overrides son los ajustes de UNA organización sobre un perfil base. Se guardan
// con la corrida (no en el código ni en el addon) y se aplican con Resolve.
type Overrides struct {
	Org string `json:"org,omitempty"`
	// IgnoreHeaders se suman a los del perfil (p. ej. columnas "Meta:" de plugins).
	IgnoreHeaders []string `json:"ignoreHeaders,omitempty"`
	// Rename agrega encabezados alternativos a un campo existente: target -> encabezados.
	Rename map[string][]string `json:"rename,omitempty"`
	// Fields agrega campos nuevos o reemplaza por completo el de igual target.
	Fields []Field `json:"fields,omitempty"`
	// Drop elimina campos por target.
	Drop []string `json:"drop,omitempty"`
	// Defaults se suman a los del perfil (ganan los del override).
	Defaults map[string]any `json:"defaults,omitempty"`
	// Clean reemplaza las reglas globales de limpieza si no es nil.
	Clean []string `json:"clean,omitempty"`
	// CleanAdd agrega reglas de limpieza al final de las de un campo existente
	// (target -> reglas): p. ej. quitar el aviso legal de la descripción.
	CleanAdd map[string][]string `json:"cleanAdd,omitempty"`
}

// Resolve devuelve una copia del perfil con los overrides aplicados. No muta p.
func (p Profile) Resolve(o *Overrides) (Profile, error) {
	out := p
	out.Fields = append([]Field(nil), p.Fields...)
	out.IgnoreHeaders = append([]string(nil), p.IgnoreHeaders...)
	out.Defaults = map[string]any{}
	for k, v := range p.Defaults {
		out.Defaults[k] = v
	}
	if o == nil {
		return out, out.Validate()
	}
	out.IgnoreHeaders = append(out.IgnoreHeaders, o.IgnoreHeaders...)
	for k, v := range o.Defaults {
		out.Defaults[k] = v
	}
	if o.Clean != nil {
		out.Clean = o.Clean
	}
	drop := map[string]bool{}
	for _, t := range o.Drop {
		drop[t] = true
	}
	kept := out.Fields[:0:0]
	for _, f := range out.Fields {
		if drop[f.Target] {
			continue
		}
		if extra, ok := o.Rename[f.Target]; ok {
			f.From = append(append([]string(nil), f.From...), extra...)
		}
		if extra, ok := o.CleanAdd[f.Target]; ok {
			f.Clean = append(append([]string(nil), f.Clean...), extra...)
		}
		kept = append(kept, f)
	}
	out.Fields = kept
	for t := range o.Rename {
		if _, ok := out.field(t); !ok {
			return Profile{}, fmt.Errorf("override: rename de %q, que no existe en el perfil %q", t, p.Name)
		}
	}
	for t := range o.CleanAdd {
		if _, ok := out.field(t); !ok {
			return Profile{}, fmt.Errorf("override: cleanAdd de %q, que no existe en el perfil %q", t, p.Name)
		}
	}
	for _, nf := range o.Fields {
		replaced := false
		for i := range out.Fields {
			if out.Fields[i].Target == nf.Target {
				out.Fields[i], replaced = nf, true
			}
		}
		if !replaced {
			out.Fields = append(out.Fields, nf)
		}
	}
	return out, out.Validate()
}

// Hash de los overrides, estable; entra en la llave de idempotencia vía el perfil resuelto.
func (o *Overrides) Hash() string {
	if o == nil {
		return ""
	}
	b, _ := json.Marshal(o)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (p Profile) headerIgnored(h string) bool {
	lh := strings.ToLower(strings.TrimSpace(h))
	for _, pat := range p.IgnoreHeaders {
		pat = strings.ToLower(strings.TrimSpace(pat))
		if strings.HasSuffix(pat, "*") {
			if strings.HasPrefix(lh, strings.TrimSuffix(pat, "*")) {
				return true
			}
		} else if pat == lh {
			return true
		}
	}
	return false
}

// normalizeRow limpia, traduce valores y separa los medios de una fila cruda.
// Devuelve la fila lista para importer.BuildRecordWithDeps, las URLs de medios
// por campo destino y cuántas columnas ignoradas se descartaron.
func (p Profile) normalizeRow(raw map[string]any) (row map[string]any, media map[string][]string, ignored int) {
	raw = p.withDerived(raw)
	idx := map[string]Field{}
	for _, f := range p.Fields {
		for _, h := range f.headers() {
			idx[normHeader(h)] = f
		}
		idx[normHeader(f.Target)] = f
	}
	row = make(map[string]any, len(raw))
	for h, v := range raw {
		if p.headerIgnored(h) {
			ignored++
			continue
		}
		f, ok := idx[normHeader(h)]
		if !ok {
			continue
		}
		s := stringifyCell(v)
		s = applyClean(append(append([]string{}, p.Clean...), f.Clean...), s)
		if len(f.Values) > 0 {
			for from, to := range f.Values {
				if strings.EqualFold(strings.TrimSpace(from), strings.TrimSpace(s)) {
					s = to
					break
				}
			}
		}
		if strings.TrimSpace(s) == "" && f.Default != nil {
			s = fmt.Sprint(f.Default)
		}
		if f.Media {
			if urls := splitMedia(s, f.Sep); len(urls) > 0 {
				if media == nil {
					media = map[string][]string{}
				}
				media[f.Target] = urls
			}
			continue
		}
		row[h] = s
	}
	return row, media, ignored
}

func normHeader(h string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(h), "*")))
}

func stringifyCell(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		if s := string(b); !strings.HasPrefix(s, "[") && !strings.HasPrefix(s, "{") {
			return strings.Trim(s, `"`)
		}
		return string(b)
	}
}

func splitMedia(s, sep string) []string {
	if sep != "" {
		s = strings.ReplaceAll(s, sep, "\n")
	} else {
		s = strings.NewReplacer(",", "\n", ";", "\n", "|", "\n").Replace(s)
	}
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(s, "\n") {
		part = strings.TrimSpace(part)
		if part != "" && !seen[part] {
			seen[part] = true
			out = append(out, part)
		}
	}
	return out
}

// buildRecord convierte una fila cruda en registro destino.
func (p Profile) buildRecord(spec modelbase.ImportSpec, raw map[string]any, deps *importer.TransformDeps, i int) (rec map[string]any, media map[string][]string, ignored int, issues []importer.RowIssue) {
	row, media, ignored := p.normalizeRow(raw)
	rec, issues = importer.BuildRecordWithDeps(spec, row, deps, i)
	p.applySplits(rec)
	return rec, media, ignored, issues
}

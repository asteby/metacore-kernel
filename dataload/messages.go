package dataload

import (
	"context"
	"embed"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/asteby/metacore-kernel/i18n"
	"github.com/asteby/metacore-kernel/importer"
)

// Mensajes del motor. El motor no escribe textos para personas: todo error,
// problema de fila o aviso lleva un Code estable (lo que ops/addons guardan,
// filtran y traducen) y sus parámetros. El texto sale de un catálogo de claves
// (locales/*.json, embebido) que se puede ampliar o reemplazar con
// i18n.Compose sin tocar el motor. Los códigos son contrato: no se renombran;
// uno nuevo se agrega a code.go y a cada catálogo (la prueba lo exige).

// Code es un código estable de mensaje ("dataload.<área>.<motivo>").
type Code string

// Params son los valores que interpola el texto del código ({name}).
type Params map[string]any

// Message es un aviso localizable: código + parámetros, sin texto fijo.
type Message struct {
	Code   Code   `json:"code"`
	Params Params `json:"params,omitempty"`
}

// NewMessage arma un Message con código y pares nombre/valor, para paquetes
// que extienden el motor (dataload/batch) y no pueden usar el constructor
// interno.
func NewMessage(code Code, kv ...any) Message { return msg(code, kv...) }

// NewError arma un error del motor con código estable y parámetros.
func NewError(code Code, kv ...any) *Error { return newErr(code, kv...) }

// WrapError envuelve una causa con un código del motor.
func WrapError(cause error, code Code, kv ...any) *Error { return wrapErr(cause, code, kv...) }

// msg arma un Message a partir de pares nombre/valor.
func msg(code Code, kv ...any) Message {
	m := Message{Code: code}
	if len(kv) > 1 {
		m.Params = Params{}
		for i := 0; i+1 < len(kv); i += 2 {
			k, ok := kv[i].(string)
			if !ok {
				continue
			}
			// Los parámetros viajan en JSON (historial, API): un error del motor se
			// guarda como su Message y cualquier otro como texto.
			switch v := kv[i+1].(type) {
			case *Error:
				m.Params[k] = v.Message
			case error:
				m.Params[k] = v.Error()
			default:
				m.Params[k] = v
			}
		}
	}
	return m
}

// Text devuelve el mensaje en lang (cadena de respaldo del catálogo si falta).
func (m Message) Text(lang string) string { return renderCode(Messages(), lang, m.Code, m.Params) }

// Localize devuelve el mensaje en el idioma del contexto (i18n.WithLanguage).
func (m Message) Localize(ctx context.Context) string { return m.Text(i18n.LanguageFromContext(ctx)) }

// String es el texto en el idioma por omisión del catálogo.
func (m Message) String() string { return m.Text("") }

// Error es un error del motor con código estable. errors.Is compara por código,
// así que los centinelas (ErrRunNotFound, ErrLeaseLost...) siguen funcionando
// aunque el error lleve parámetros o una causa.
type Error struct {
	Message
	// Details son los problemas puntuales (p. ej. cada error de autoría de un perfil).
	Details []Message `json:"details,omitempty"`
	// Cause es el error de abajo (almacén, red, destino), si lo hay.
	Cause error `json:"-"`
}

func newErr(code Code, kv ...any) *Error { return &Error{Message: msg(code, kv...)} }

// wrapErr envuelve una causa con un código del motor.
func wrapErr(cause error, code Code, kv ...any) *Error {
	e := newErr(code, kv...)
	e.Cause = cause
	return e
}

// detailsErr arma un error con su lista de problemas (vacía = nil).
func detailsErr(details []Message, code Code, kv ...any) error {
	if len(details) == 0 {
		return nil
	}
	sort.SliceStable(details, func(i, j int) bool { return details[i].String() < details[j].String() })
	e := newErr(code, kv...)
	e.Details = details
	return e
}

func (e *Error) Error() string { return e.text("") }

// text rinde el error completo (detalles y causas del motor incluidas) en lang.
func (e *Error) text(lang string) string {
	var b strings.Builder
	b.WriteString(e.Message.Text(lang))
	for i, d := range e.Details {
		if i == 0 {
			b.WriteString(": ")
		} else {
			b.WriteString("; ")
		}
		b.WriteString(d.Text(lang))
	}
	if e.Cause != nil {
		b.WriteString(": ")
		b.WriteString(localizeErr(lang, e.Cause))
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Cause }

// Is compara por código: errors.Is(err, ErrRunNotFound).
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// ErrorCode devuelve el código del primer error del motor en la cadena ("" si no hay).
func ErrorCode(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// ErrorMessage devuelve el código y parámetros del primer error del motor en la
// cadena; ok=false si err no viene del motor.
func ErrorMessage(err error) (Message, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e.Message, true
	}
	return Message{}, false
}

// Localize traduce un error al idioma del contexto. Las partes que no vienen del
// motor (un error del almacén, de la red) se dejan como las escribió su autor.
func Localize(ctx context.Context, err error) string {
	return localizeErr(i18n.LanguageFromContext(ctx), err)
}

func localizeErr(lang string, err error) string {
	if err == nil {
		return ""
	}
	if e, ok := err.(*Error); ok {
		return e.text(lang)
	}
	return err.Error()
}

//go:embed locales/*.json
var localeFS embed.FS

var (
	catalogOnce sync.Once
	catalog     *i18n.Catalog
)

// DefaultLanguage es el idioma con que se rinden Error() y String() cuando nadie
// pide otro; ops traduce con Localize o con su propio catálogo.
const DefaultLanguage = "es"

// Messages es el catálogo embebido de textos del motor (es y en). Se puede
// componer con uno propio (i18n.Compose) para agregar idiomas o cambiar textos;
// las claves son los Code.
func Messages() *i18n.Catalog {
	catalogOnce.Do(func() {
		c, err := i18n.LoadCatalogFS(localeFS, "locales/*.json")
		if err != nil {
			panic("dataload: locales: " + err.Error()) // solo si el paquete se compiló con un catálogo roto
		}
		catalog = c.WithFallback(DefaultLanguage, "en")
	})
	return catalog
}

func renderCode(c *i18n.Catalog, lang string, code Code, p Params) string {
	t, ok := c.Lookup(lang, string(code))
	if !ok {
		return string(code)
	}
	if len(p) == 0 {
		return t
	}
	// Un parámetro que es a su vez un mensaje o un error del motor se rinde en el
	// mismo idioma.
	vals := make(map[string]any, len(p))
	for k, v := range p {
		switch x := v.(type) {
		case Message:
			vals[k] = x.Text(lang)
		case error:
			vals[k] = localizeErr(lang, x)
		case map[string]any: // un Message que ya pasó por JSON (leído del historial)
			if c, ok := x["code"].(string); ok {
				sub := Message{Code: Code(c)}
				if ps, ok := x["params"].(map[string]any); ok {
					sub.Params = ps
				}
				vals[k] = sub.Text(lang)
			} else {
				vals[k] = v
			}
		default:
			vals[k] = v
		}
	}
	return i18n.Interpolate(t, vals)
}

// rowIssue arma un problema de fila con código, parámetros y el texto por omisión.
func rowIssue(row int, column string, code Code, kv ...any) importer.RowIssue {
	m := msg(code, kv...)
	return importer.RowIssue{Row: row, Column: column, Code: string(code), Params: m.Params, Message: m.String()}
}

// LocalizeIssue traduce un problema de fila al idioma del contexto. Sin código
// (un problema de otro origen) devuelve su Message tal cual.
func LocalizeIssue(ctx context.Context, is importer.RowIssue) string {
	if is.Code == "" {
		return is.Message
	}
	return renderCode(Messages(), i18n.LanguageFromContext(ctx), Code(is.Code), is.Params)
}

// Localize traduce el motivo al idioma del contexto. El detail que escribió el
// autor del documento se respeta tal cual (es dato, no texto del motor).
func (it ReviewItem) Localize(ctx context.Context) string {
	if it.DetailCode == "" {
		return it.Detail
	}
	return renderCode(Messages(), i18n.LanguageFromContext(ctx), it.DetailCode, it.DetailParams)
}

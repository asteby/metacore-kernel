package diag

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	reCode  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)
	reCause = regexp.MustCompile(`^[0-9]{1,6}$`)
	rePath  = regexp.MustCompile(`^[a-z][a-z0-9_]*(\[[0-9]{1,3}\])?(\.[a-z][a-z0-9_]*(\[[0-9]{1,3}\])?){0,8}$`)
	reRun   = regexp.MustCompile(`[A-Za-z0-9_.\[\]]+`)
	reKey   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,29}$`)
)

const (
	maxProps       = 8
	maxPropLen     = 80
	maxProviderCds = 8
	maxCauses      = 8
	maxShapeKeys   = 12
	maxScanString  = 2000
	maxBodyParse   = 256 << 10
	maxWalkNodes   = 2000
	maxWalkDepth   = 6
)

// pathAdjacent: un token pegado a estos caracteres forma parte de un email,
// URL, ruta de archivo, dominio, query o texto codificado, no es un campo.
const pathAdjacent = "@/\\%#&~+*-"

// notPathTail: ultimo tramo que delata dominio o nombre de archivo.
var notPathTail = map[string]bool{
	"com": true, "net": true, "org": true, "io": true, "mx": true, "co": true, "app": true, "dev": true,
	"info": true, "biz": true, "edu": true, "gov": true, "us": true, "es": true, "ar": true, "br": true,
	"cl": true, "pe": true, "uk": true, "de": true, "fr": true, "eu": true, "ai": true, "me": true,
	"json": true, "xml": true, "html": true, "htm": true, "php": true, "js": true, "txt": true, "pdf": true,
	"png": true, "jpg": true, "jpeg": true, "gif": true, "svg": true, "css": true, "csv": true, "zip": true,
	"go": true, "py": true, "rb": true, "exe": true, "dll": true, "log": true, "tmp": true,
}

// camelToSnake convierte userID/firstName en user_id/first_name. Mecanico: no
// introduce ni copia texto que no estuviera.
func camelToSnake(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			if i > 0 {
				p := s[i-1]
				if (p >= 'a' && p <= 'z') || (p >= '0' && p <= '9') {
					b.WriteByte('_')
				}
			}
			b.WriteByte(c + 32)
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// validPath aplica el regex de rutas, el largo, la regla de digitos y la de
// tramos. single permite un unico tramo sin '_' (solo cuando la clave que lo
// contiene ya declara que el valor es un campo).
func validPath(p string, single bool) bool {
	if len(p) == 0 || len(p) > maxPropLen || !rePath.MatchString(p) || longDigitRun(p, 5) {
		return false
	}
	segs := strings.Split(p, ".")
	if len(segs) > 1 && notPathTail[bareSeg(segs[len(segs)-1])] {
		return false
	}
	if len(segs) == 1 {
		if single {
			return true
		}
		// Una palabra suelta es indistinguible de una frase: solo con '_' o indice.
		return strings.ContainsAny(p, "_[")
	}
	for _, sg := range segs {
		if len(bareSeg(sg)) < 2 {
			return false
		}
	}
	return true
}

func bareSeg(sg string) string {
	if i := strings.IndexByte(sg, '['); i >= 0 {
		return sg[:i]
	}
	return sg
}

// ExtractPaths saca de un texto libre (cuerpo de error) los tokens con forma de
// ruta de campo. Las frases NUNCA se copian y no se extrae ningun valor: un
// token se acepta solo si pasa el regex de rutas, no esta pegado a
// @ / \ % # & ~ + * - y no es dominio ni nombre de archivo.
func ExtractPaths(text string) []string { return ExtractPathsScrubbed(text, nil) }

// valueCues: la palabra que precede a un token y delata que el token es un
// VALOR (un nombre, un usuario, un id), no un campo: "user john_doe", "name
// maria.lopez", "value juan.perez".
var valueCues = map[string]bool{
	"user": true, "username": true, "usuario": true, "customer": true, "cliente": true, "client": true,
	"name": true, "nombre": true, "named": true, "email": true, "correo": true, "mail": true, "id": true,
	"value": true, "valor": true, "by": true, "from": true, "hello": true, "hola": true, "dear": true,
	"mr": true, "mrs": true, "sr": true, "sra": true, "dr": true, "host": true, "ip": true,
	"account": true, "cuenta": true, "holder": true, "titular": true, "payer": true, "buyer": true,
	"seller": true, "owner": true, "contact": true, "contacto": true, "as": true, "with": true, "de": true, "del": true, "por": true,
}

// prevWord devuelve la palabra (minusculas) justo antes de text[s:], saltando
// espacios, comillas y dos puntos.
func prevWord(text string, s int) string {
	i := s
	for i > 0 && strings.IndexByte(" \t\n'\"`:(", text[i-1]) >= 0 {
		i--
	}
	j := i
	for j > 0 && ((text[j-1] >= 'a' && text[j-1] <= 'z') || (text[j-1] >= 'A' && text[j-1] <= 'Z')) {
		j--
	}
	return strings.ToLower(text[j:i])
}

// ExtractPathsScrubbed es ExtractPaths borrando antes por valor los secretos de
// sc (un secreto conocido dentro del mensaje no puede acabar como "ruta").
func ExtractPathsScrubbed(text string, sc *Scrubber) []string {
	if len(text) > maxScanString {
		text = text[:maxScanString]
	}
	text = sc.Redact(text)
	var out []string
	seen := map[string]bool{}
	for _, loc := range reRun.FindAllStringIndex(text, -1) {
		s, e := loc[0], loc[1]
		tok := text[s:e]
		// El punto final de una oracion no es parte del token.
		for strings.HasSuffix(tok, ".") {
			tok = tok[:len(tok)-1]
		}
		if tok == "" || tok[0] < 'a' || tok[0] > 'z' {
			continue
		}
		if s > 0 && strings.IndexByte(pathAdjacent, text[s-1]) >= 0 {
			continue
		}
		if e < len(text) && strings.IndexByte(pathAdjacent, text[e]) >= 0 {
			continue
		}
		// Un '.' o ':' pegado delante tambien indica dominio/host (a.b.c).
		if s > 0 && text[s-1] == '.' {
			continue
		}
		if valueCues[prevWord(text, s)] {
			continue
		}
		// "(maria.lopez)": un token entre parentesis es una aclaracion sobre un
		// valor, no el nombre de un campo.
		if s > 0 && e < len(text) && text[s-1] == '(' && text[e] == ')' {
			continue
		}
		p := camelToSnake(tok)
		if !validPath(p, false) || seen[p] || sc.Contains(p) {
			continue
		}
		seen[p] = true
		out = append(out, p)
		if len(out) >= maxProps {
			break
		}
	}
	return out
}

// pathValue interpreta el valor de una clave que por convencion nombra un campo
// (field, path, param, pointer...): "/data/attributes/email", "$.payer.email".
func pathValue(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) > 120 {
		return "", false
	}
	s = strings.Trim(s, "`'\"")
	s = strings.TrimPrefix(s, "#")
	s = strings.TrimPrefix(s, "$")
	s = strings.TrimPrefix(s, ".")
	s = strings.Trim(s, "/")
	if i := strings.IndexByte(s, '/'); i > 0 && fsRoots[strings.ToLower(s[:i])] {
		return "", false
	}
	s = strings.ReplaceAll(s, "/", ".")
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return "", false
	}
	s = camelToSnake(s)
	if !validPath(s, true) {
		return "", false
	}
	return s, true
}

var pathKeys = map[string]bool{
	"field": true, "fields": true, "path": true, "param": true, "parameter": true, "property": true,
	"pointer": true, "attribute": true, "loc": true,
}

// fsRoots: primer tramo de una ruta de archivo; un valor asi no es un puntero
// de campo aunque venga en una clave de ruta.
var fsRoots = map[string]bool{
	"home": true, "users": true, "usr": true, "var": true, "etc": true, "opt": true, "tmp": true, "root": true,
	"mnt": true, "srv": true, "proc": true, "windows": true, "bin": true, "dev": true, "lib": true,
}

var codeKeys = map[string]bool{
	"code": true, "error": true, "error_code": true, "err_code": true, "errorcode": true, "type": true,
	"error_type": true, "reason": true, "kind": true, "status": true, "error_id": true,
}

// strongCodeKeys nombran un codigo de error sin ambiguedad: cualquier valor con
// forma de codigo se acepta. En las demas (error, type, reason, kind, status)
// el valor puede ser texto: solo se acepta con '_' (cc_rejected_other_reason) o
// si es una palabra generica de estado (rejected, forbidden...).
var strongCodeKeys = map[string]bool{
	"code": true, "error_code": true, "err_code": true, "errorcode": true, "error_type": true, "error_id": true,
}

var genericStatusWords = map[string]bool{
	"error": true, "fail": true, "failed": true, "failure": true, "invalid": true, "expired": true, "rejected": true,
	"declined": true, "denied": true, "refused": true, "unauthorized": true, "forbidden": true, "timeout": true,
	"conflict": true, "duplicate": true, "unavailable": true, "pending": true, "approved": true, "cancelled": true,
	"canceled": true, "unknown": true, "internal": true, "notfound": true, "success": true,
}

// messageKeys: claves cuyo valor es un mensaje de error en lenguaje natural. Es
// el UNICO texto libre del que se intentan sacar rutas de campo; en cualquier
// otra clave el valor es un dato (nombre, usuario, dominio) y no se toca.
var messageKeys = map[string]bool{
	"message": true, "messages": true, "msg": true, "detail": true, "details": true, "description": true,
	"error_description": true, "error_message": true, "title": true, "errors": true,
}

// providerCode acepta el valor de una clave de codigo como codigo de proveedor.
func providerCode(key, raw string) (string, bool) {
	c := strings.ToLower(strings.TrimSpace(raw))
	if !reCode.MatchString(c) || longDigitRun(c, 5) {
		return "", false
	}
	if strongCodeKeys[key] || strings.Contains(c, "_") || genericStatusWords[c] {
		return c, true
	}
	return "", false
}

// BodyFacts es lo UNICO que el kernel retiene de un cuerpo de error ajeno.
type BodyFacts struct {
	ProviderCodes []string
	Causes        []string
	Props         []string
	Shape         string
}

// ExtractBody reduce un cuerpo de respuesta de error a codigos, causas
// numericas, rutas de campo y esqueleto de claves/tipos. Sin valores. httpStatus
// se usa solo para no repetir el status como causa.
func ExtractBody(body []byte, httpStatus int) BodyFacts {
	return ExtractBodyScrubbed(body, httpStatus, nil)
}

// ExtractBodyScrubbed es ExtractBody borrando antes por valor los secretos de sc.
func ExtractBodyScrubbed(body []byte, httpStatus int, sc *Scrubber) BodyFacts {
	var f BodyFacts
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		f.ProviderCodes = []string{"empty_body"}
		return f
	}
	if len(body) > maxBodyParse || (body[0] != '{' && body[0] != '[') {
		f.ProviderCodes = []string{"non_json_body"}
		return f
	}
	var root any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		f.ProviderCodes = []string{"non_json_body"}
		return f
	}
	w := &walker{status: httpStatus, sc: sc}
	w.walk(root, "", 0)
	f.ProviderCodes = w.codes.list(maxProviderCds)
	f.Causes = w.causes.list(maxCauses)
	f.Props = w.props.list(maxProps)
	if obj, ok := root.(map[string]any); ok {
		f.Shape = shapeOf(obj)
	}
	return f
}

type orderedSet struct {
	seen map[string]bool
	vals []string
}

func (o *orderedSet) add(v string) {
	if o.seen == nil {
		o.seen = map[string]bool{}
	}
	if !o.seen[v] {
		o.seen[v] = true
		o.vals = append(o.vals, v)
	}
}

func (o *orderedSet) list(max int) []string {
	if len(o.vals) > max {
		return o.vals[:max]
	}
	return o.vals
}

type walker struct {
	status               int
	sc                   *Scrubber
	nodes                int
	codes, causes, props orderedSet
}

func (w *walker) walk(v any, key string, depth int) {
	w.nodes++
	if w.nodes > maxWalkNodes || depth > maxWalkDepth {
		return
	}
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			w.walk(x[k], camelToSnake(k), depth+1)
		}
	case []any:
		for _, e := range x {
			w.walk(e, key, depth+1)
		}
	case string:
		if codeKeys[key] {
			if c, ok := providerCode(key, x); ok {
				w.codes.add(c)
			} else if reCause.MatchString(strings.TrimSpace(x)) && strings.TrimSpace(x) != strconv.Itoa(w.status) {
				w.causes.add(strings.TrimSpace(x))
			}
		}
		if pathKeys[key] {
			if p, ok := pathValue(x); ok {
				w.props.add(p)
			}
		}
		if messageKeys[key] {
			for _, p := range ExtractPathsScrubbed(x, w.sc) {
				w.props.add(p)
			}
		}
	case json.Number:
		if codeKeys[key] {
			s := x.String()
			if reCause.MatchString(s) && s != strconv.Itoa(w.status) {
				w.causes.add(s)
			}
		}
	}
}

func jsonType(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case json.Number, float64:
		return "number"
	case bool:
		return "bool"
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	}
	return "null"
}

// shapeOf es el esqueleto de las claves de primer nivel: "clave:tipo,...".
func shapeOf(obj map[string]any) string {
	pairs := map[string]string{}
	for k, v := range obj {
		sk := camelToSnake(k)
		if !reKey.MatchString(sk) || longDigitRun(sk, 5) {
			continue
		}
		pairs[sk] = jsonType(v)
	}
	keys := make([]string, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > maxShapeKeys {
		keys = keys[:maxShapeKeys]
	}
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + ":" + pairs[k]
	}
	return strings.Join(parts, ",")
}

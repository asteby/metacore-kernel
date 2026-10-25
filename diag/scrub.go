package diag

import (
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	xnorm "golang.org/x/text/unicode/norm"
)

// Scrubber borra por VALOR los secretos que el host conoce de una invocacion
// (ajustes, credenciales de conector, cabeceras Authorization de http_request):
// cualquier aparicion literal desaparece, venga de donde venga. Es seguro para
// uso concurrente y el cero valor es usable. Un *Scrubber nil no borra nada.
//
// Es PEREZOSO: Add/AddSettings/AddHeaders solo guardan los valores crudos (una
// invocacion exitosa, la inmensa mayoria, nunca paga por las variantes); las
// variantes (url-escaped, base64, hex...) se calculan la primera vez que se
// consulta, o sea solo cuando hay un fallo que sanear.
type Scrubber struct {
	mu      sync.RWMutex
	pending []rawSecret // valores crudos aun sin expandir
	secrets []string    // variantes exactas, de mayor a menor largo
	loose   []string    // formas compactadas (sin separadores, en minusculas) de >= looseMin
	seen    map[string]struct{}
}

type rawSecret struct {
	v   string
	min int
}

// NewScrubber crea un Scrubber con secretos iniciales.
func NewScrubber(secrets ...string) *Scrubber {
	s := &Scrubber{}
	s.Add(secrets...)
	return s
}

const (
	minSecretLen  = 4 // credenciales explicitas
	minSettingLen = 5 // valores de ajustes alfanumericos: evita borrar "true" o "mx" por todos lados
	minPinLen     = 4 // ajustes solo-digitos (un pin): se borran desde 4
	looseMin      = 8 // largo minimo para buscar el secreto partido por separadores
)

// Add registra secretos (y, al consultar, sus variantes: sin prefijo
// Bearer/Basic/Token, decodificado de Basic, url-escaped, base64/base64url, hex
// y en minusculas/partido por espacios mediante la busqueda laxa).
func (s *Scrubber) Add(secrets ...string) {
	if s == nil || len(secrets) == 0 {
		return
	}
	s.mu.Lock()
	for _, v := range secrets {
		s.pending = append(s.pending, rawSecret{v, minSecretLen})
	}
	s.mu.Unlock()
}

// settingNoise: valores de ajustes que no son secretos y que borrar por todos
// lados solo estorba.
var settingNoise = map[string]bool{"true": true, "false": true, "null": true, "none": true, "yes": true}

// AddSettings registra los valores de los ajustes de la invocacion (todos:
// el host no distingue cuales son sensibles). Solo guarda; no calcula nada.
func (s *Scrubber) AddSettings(m map[string]string) {
	if s == nil || len(m) == 0 {
		return
	}
	s.mu.Lock()
	if cap(s.pending)-len(s.pending) < len(m) {
		np := make([]rawSecret, len(s.pending), len(s.pending)+len(m))
		copy(np, s.pending)
		s.pending = np
	}
	for _, v := range m {
		s.pending = append(s.pending, rawSecret{v, minSettingLen})
	}
	s.mu.Unlock()
}

// AddHeaders registra los valores de cabeceras de peticion sensibles. Se
// tratan como sensibles todas salvo las claramente inocuas (Content-Type,
// Accept, User-Agent...), porque un secreto puede viajar en cualquier cabecera
// personalizada (X-Api-Key, X-Idempotency-Key...).
func (s *Scrubber) AddHeaders(h map[string]string) {
	for k, v := range h {
		switch strings.ToLower(k) {
		case "content-type", "accept", "accept-language", "accept-encoding", "user-agent", "content-length":
			continue
		}
		s.Add(v)
	}
}

func allDigits(v string) bool {
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return false
		}
	}
	return v != ""
}

// isLooseSkip: separadores y caracteres de formato que se ignoran al buscar un
// secreto partido ("AB CD-EF.GH").
func isLooseSkip(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '-', '_', '.', ',':
		return true
	}
	return unicode.Is(unicode.Cf, r)
}

// compactForm: forma comparable de un secreto: sin separadores ni caracteres de
// formato y en minusculas.
func compactForm(v string) string {
	var b strings.Builder
	for _, r := range v {
		if isLooseSkip(r) {
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// materialize expande los crudos pendientes. Llamar con s.mu tomado en escritura.
func (s *Scrubber) materialize() {
	if len(s.pending) == 0 {
		return
	}
	if s.seen == nil {
		s.seen = make(map[string]struct{}, len(s.pending)*4)
		for _, e := range s.secrets {
			s.seen[e] = struct{}{}
		}
	}
	addVar := func(v string) {
		if _, ok := s.seen[v]; ok {
			return
		}
		s.seen[v] = struct{}{}
		s.secrets = append(s.secrets, v)
	}
	looseSeen := map[string]struct{}{}
	for _, l := range s.loose {
		looseSeen[l] = struct{}{}
	}
	for _, raw := range s.pending {
		v := strings.TrimSpace(raw.v)
		min := raw.min
		if min == minSettingLen {
			if allDigits(v) {
				min = minPinLen
			}
			if settingNoise[strings.ToLower(v)] {
				continue
			}
		}
		bases := []string{v}
		for _, p := range []string{"Bearer ", "Basic ", "Token "} {
			if len(v) > len(p) && strings.EqualFold(v[:len(p)], p) {
				rest := strings.TrimSpace(v[len(p):])
				bases = append(bases, rest)
				if p == "Basic " {
					if dec, err := base64.StdEncoding.DecodeString(rest); err == nil {
						bases = append(bases, string(dec))
						for _, part := range strings.SplitN(string(dec), ":", 2) {
							bases = append(bases, part)
						}
					}
				}
			}
		}
		for _, b := range bases {
			if len(b) < min {
				continue
			}
			addVar(b)
			if q := url.QueryEscape(b); q != b {
				addVar(q)
			}
			addVar(base64.StdEncoding.EncodeToString([]byte(b)))
			addVar(base64.RawStdEncoding.EncodeToString([]byte(b)))
			addVar(base64.URLEncoding.EncodeToString([]byte(b)))
			addVar(base64.RawURLEncoding.EncodeToString([]byte(b)))
			addVar(hex.EncodeToString([]byte(b)))
			if lb := strings.ToLower(b); lb != b {
				addVar(lb)
			}
			if c := compactForm(b); len(c) >= looseMin {
				if _, ok := looseSeen[c]; !ok {
					looseSeen[c] = struct{}{}
					s.loose = append(s.loose, c)
				}
			}
		}
	}
	s.pending = nil
	// Los largos primero: un secreto que contiene a otro se borra entero.
	sort.SliceStable(s.secrets, func(i, j int) bool { return len(s.secrets[i]) > len(s.secrets[j]) })
	sort.SliceStable(s.loose, func(i, j int) bool { return len(s.loose[i]) > len(s.loose[j]) })
}

// ready expande lo pendiente y vuelve con el RLock tomado (el llamador hace
// RUnlock).
func (s *Scrubber) ready() {
	s.mu.RLock()
	if len(s.pending) == 0 {
		return
	}
	s.mu.RUnlock()
	s.mu.Lock()
	s.materialize()
	s.mu.Unlock()
	s.mu.RLock()
}

// Contains reporta si str contiene algun secreto literal.
func (s *Scrubber) Contains(str string) bool {
	if s == nil || str == "" {
		return false
	}
	s.ready()
	defer s.mu.RUnlock()
	for _, sec := range s.secrets {
		if strings.Contains(str, sec) {
			return true
		}
	}
	return false
}

// Redact sustituye cada secreto por [redacted]: primero la aparicion literal
// (y sus variantes codificadas) y luego la laxa, que encuentra el secreto
// aunque venga en otras mayusculas o partido por espacios, guiones, puntos...
func (s *Scrubber) Redact(str string) string {
	if s == nil || str == "" {
		return str
	}
	s.ready()
	defer s.mu.RUnlock()
	for _, sec := range s.secrets {
		if strings.Contains(str, sec) {
			str = strings.ReplaceAll(str, sec, "[redacted]")
		}
	}
	if len(s.loose) == 0 {
		return str
	}
	return redactLoose(str, s.loose)
}

// redactLoose borra las apariciones de los secretos compactados en str
// comparando contra la forma compactada de str y mapeando de vuelta al rango
// original.
func redactLoose(str string, loose []string) string {
	var cb strings.Builder
	var start, end []int // por byte de la forma compactada: rango en str
	cb.Grow(len(str))
	for i, r := range str {
		if isLooseSkip(r) {
			continue
		}
		w := utf8.RuneLen(r)
		if r == utf8.RuneError {
			w = 1
		}
		n0 := cb.Len()
		cb.WriteRune(unicode.ToLower(r))
		for k := n0; k < cb.Len(); k++ {
			start = append(start, i)
			end = append(end, i+w)
		}
	}
	c := cb.String()
	type span struct{ a, b int }
	var spans []span
	for _, sec := range loose {
		for off := 0; off+len(sec) <= len(c); {
			i := strings.Index(c[off:], sec)
			if i < 0 {
				break
			}
			a, b := off+i, off+i+len(sec)
			spans = append(spans, span{start[a], end[b-1]})
			off = b
		}
	}
	if len(spans) == 0 {
		return str
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].a < spans[j].a })
	var out strings.Builder
	pos := 0
	for _, sp := range spans {
		if sp.b <= pos {
			continue
		}
		if sp.a > pos {
			out.WriteString(str[pos:sp.a])
		}
		out.WriteString("[redacted]")
		pos = sp.b
	}
	out.WriteString(str[pos:])
	return out.String()
}

var (
	reURL      = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.\-]*://[^\s"'<>)\]]+`)
	reBareURL  = regexp.MustCompile(`(?i)\b[a-z0-9](?:[a-z0-9\-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9\-]*[a-z0-9])?)*\.[a-z]{2,24}(?::\d{1,5})?[/?#][^\s"'<>)\]]*`)
	reJWT      = regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{5,}\.[A-Za-z0-9_\-]{5,}\.?[A-Za-z0-9_\-]*`)
	reEmail    = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9\-]+(?:\.[A-Za-z0-9\-]+)*\.[A-Za-z]{2,}`)
	reAuth     = regexp.MustCompile(`(?i)\b(bearer|basic|token|apikey|api[_-]?key)\s+[A-Za-z0-9._~+/=\-]{4,}`)
	reSecretKV = regexp.MustCompile(`(?i)([\w.\-]*(?:password|passwd|passphrase|pwd|secret|api[_-]?key|apikey|token|authorization|credential|private[_-]?key|signature|cookie|bearer|auth)[\w.\-]*["']?\s*[=:]\s*)(?:(?:bearer|basic|token|digest|apikey)\s+)?(?:\[redacted\]|"[^"]*"|'[^']*'|[^\s,;&"'}\]]+)`)
	rePAN      = regexp.MustCompile(`\d(?:[^\dA-Za-z]{0,2}\d){12,18}`)
	rePANShape = regexp.MustCompile(`\d{4}(?:[^\dA-Za-z]{1,2}\d{4}){3}`)
	reIPv4     = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	reIPv6     = regexp.MustCompile(`(?i)(?:[0-9a-f]{1,4}:){7}[0-9a-f]{1,4}|(?:[0-9a-f]{1,4}:){1,7}:(?:[0-9a-f]{1,4}(?::[0-9a-f]{1,4}){0,6})?|::(?:[0-9a-f]{1,4}:){0,6}[0-9a-f]{1,4}`)
	rePhone    = regexp.MustCompile(`\+\d[\d ().\-]{7,}\d|\(?\d{2,3}\)?[ .\-]\d{3,4}[ .\-]\d{4}\b`)
	reDigits   = regexp.MustCompile(`\d{6,}`)
	reToken    = regexp.MustCompile(`[A-Za-z0-9_\-+/=]{24,}`)
)

// MaxBreadcrumbLen es el largo maximo (runas) de una linea de breadcrumb.
const MaxBreadcrumbLen = 160

// dropFormat quita los caracteres Unicode de formato (categoria Cf: ancho
// cero, marcas bidi, soft hyphen...) con los que se parte un email o un token
// para que no lo reconozca una regex.
func dropFormat(r rune) rune {
	if unicode.Is(unicode.Cf, r) {
		return -1
	}
	return r
}

// ScrubText limpia un texto libre (linea de log del guest, mensaje): normaliza
// (NFKC, sin caracteres de formato), borra secretos por valor y luego URLs
// (queda el host), JWT, emails, credenciales (cabecera o clave=valor), PAN
// (Luhn), IPs, telefonos, runs largos de digitos y tokens opacos. Es
// idempotente. Se usa para breadcrumbs; los campos estructurados usan
// allowlist en Sanitize y no pasan texto libre.
func ScrubText(str string, sc *Scrubber) string {
	str = strings.Map(dropFormat, xnorm.NFKC.String(strings.ToValidUTF8(str, "\uFFFD")))
	// Hasta un punto fijo: una pasada puede dejar al descubierto lo que otra
	// regla ocultaba (un PAN pegado a un run de digitos, por ejemplo).
	for i := 0; i < 6; i++ {
		next := scrubOnce(str, sc)
		if next == str {
			break
		}
		str = next
	}
	return str
}

func urlToHost(m string) string {
	probe := m
	if !strings.Contains(m, "://") {
		probe = "https://" + m
	}
	if u, err := url.Parse(probe); err == nil && safeHost(u.Hostname()) {
		if strings.Contains(m, "://") {
			return u.Scheme + "://" + u.Hostname()
		}
		return strings.ToLower(u.Hostname())
	}
	return "[url]"
}

// safeHost: solo un nombre de host o IP "normal". url.Parse decodifica el
// porcentaje del host ("%80" -> byte suelto) y eso rompe la idempotencia.
func safeHost(h string) bool {
	if h == "" {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == ':') {
			return false
		}
	}
	return true
}

func scrubOnce(str string, sc *Scrubber) string {
	str = sc.Redact(str)
	str = reURL.ReplaceAllStringFunc(str, urlToHost)
	str = reBareURL.ReplaceAllStringFunc(str, urlToHost)
	str = reJWT.ReplaceAllString(str, "[jwt]")
	str = reEmail.ReplaceAllString(str, "[email]")
	str = reAuth.ReplaceAllString(str, "[redacted]")
	str = reSecretKV.ReplaceAllString(str, "${1}[redacted]")
	str = redactPAN(str)
	str = reIPv4.ReplaceAllString(str, "[ip]")
	str = reIPv6.ReplaceAllString(str, "[ip]")
	str = rePhone.ReplaceAllString(str, "[phone]")
	str = reDigits.ReplaceAllString(str, "[num]")
	str = reToken.ReplaceAllStringFunc(str, func(m string) string {
		if looksOpaque(m) {
			return "[token]"
		}
		return m
	})
	return str
}

// redactPAN reemplaza cada numero de tarjeta plausible: una ventana de 13..19
// digitos (con hasta 2 separadores no alfanumericos entre digitos) que cumple
// Luhn, aunque venga pegada a otros digitos, o 4 grupos de 4 digitos.
func redactPAN(str string) string {
	str = rePAN.ReplaceAllStringFunc(str, func(m string) string {
		pos := make([]int, 0, 19)
		for i := 0; i < len(m); i++ {
			if m[i] >= '0' && m[i] <= '9' {
				pos = append(pos, i)
			}
		}
		if len(pos) <= 19 {
			// Un solo candidato: si alguna ventana es una tarjeta, se borra todo,
			// sin dejar de sobra los digitos que la rodean.
			for i := 0; i < len(pos); i++ {
				for l := 19; l >= 13; l-- {
					if i+l <= len(pos) && luhnValid(digitsOnly(m[pos[i]:pos[i+l-1]+1])) {
						return "[pan]"
					}
				}
			}
			return m
		}
		var out strings.Builder
		last := 0
		for i := 0; i < len(pos); {
			hit := 0
			for l := 19; l >= 13; l-- {
				if i+l <= len(pos) && luhnValid(digitsOnly(m[pos[i]:pos[i+l-1]+1])) {
					hit = l
					break
				}
			}
			if hit == 0 {
				i++
				continue
			}
			out.WriteString(m[last:pos[i]])
			out.WriteString("[pan]")
			last = pos[i+hit-1] + 1
			i += hit
		}
		out.WriteString(m[last:])
		return out.String()
	})
	return rePANShape.ReplaceAllString(str, "[pan]")
}

// looksOpaque: token largo con digitos (claves, hashes, ids) o largo y de
// mayusculas/minusculas mezcladas (base64).
func looksOpaque(m string) bool {
	hasDigit, hasUpper, hasLower := false, false, false
	for _, r := range m {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r >= 'a' && r <= 'z':
			hasLower = true
		}
	}
	return hasDigit || (len(m) >= 32 && hasUpper && hasLower)
}

func digitsOnly(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// luhnValid: 13..19 digitos con checksum Luhn (numero de tarjeta plausible).
func luhnValid(d string) bool {
	if len(d) < 13 || len(d) > 19 {
		return false
	}
	sum, alt := 0, false
	for i := len(d) - 1; i >= 0; i-- {
		n := int(d[i] - '0')
		if alt {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		alt = !alt
	}
	return sum%10 == 0
}

// longDigitRun reporta si s tiene un tramo de mas de max digitos seguidos.
func longDigitRun(s string, max int) bool {
	run := 0
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			run++
			if run > max {
				return true
			}
		} else {
			run = 0
		}
	}
	return false
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

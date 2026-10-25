package diag

import (
	"encoding/base64"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// Scrubber borra por VALOR los secretos que el host conoce de una invocacion
// (ajustes, credenciales de conector, cabeceras Authorization de http_request):
// cualquier aparicion literal desaparece, venga de donde venga. Es seguro para
// uso concurrente y el cero valor es usable. Un *Scrubber nil no borra nada.
type Scrubber struct {
	mu      sync.RWMutex
	secrets []string
}

// NewScrubber crea un Scrubber con secretos iniciales.
func NewScrubber(secrets ...string) *Scrubber {
	s := &Scrubber{}
	s.Add(secrets...)
	return s
}

const (
	minSecretLen  = 4 // credenciales explicitas
	minSettingLen = 6 // valores de ajustes: evita borrar "true" o "mx" por todos lados
)

// Add registra secretos (y sus variantes: sin prefijo Bearer/Basic/Token,
// decodificado de Basic, url-escaped y base64).
func (s *Scrubber) Add(secrets ...string) { s.add(minSecretLen, secrets...) }

func (s *Scrubber) add(min int, secrets ...string) {
	if s == nil {
		return
	}
	var more []string
	for _, v := range secrets {
		v = strings.TrimSpace(v)
		for _, p := range []string{"Bearer ", "Basic ", "Token ", "bearer ", "basic ", "token "} {
			if strings.HasPrefix(v, p) {
				rest := strings.TrimSpace(v[len(p):])
				more = append(more, rest)
				if strings.EqualFold(strings.TrimSpace(p), "basic") {
					if raw, err := base64.StdEncoding.DecodeString(rest); err == nil {
						more = append(more, string(raw))
						for _, part := range strings.SplitN(string(raw), ":", 2) {
							more = append(more, part)
						}
					}
				}
			}
		}
		more = append(more, v)
	}
	var variants []string
	for _, v := range more {
		if len(v) < min {
			continue
		}
		variants = append(variants, v)
		if q := url.QueryEscape(v); q != v {
			variants = append(variants, q)
		}
		variants = append(variants, base64.StdEncoding.EncodeToString([]byte(v)), base64.RawStdEncoding.EncodeToString([]byte(v)))
	}
	if len(variants) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := make(map[string]bool, len(s.secrets))
	for _, e := range s.secrets {
		seen[e] = true
	}
	for _, v := range variants {
		if !seen[v] {
			seen[v] = true
			s.secrets = append(s.secrets, v)
		}
	}
	// Los largos primero: un secreto que contiene a otro se borra entero.
	sort.SliceStable(s.secrets, func(i, j int) bool { return len(s.secrets[i]) > len(s.secrets[j]) })
}

// AddSettings registra los valores de los ajustes de la invocacion (todos:
// el host no distingue cuales son sensibles).
func (s *Scrubber) AddSettings(m map[string]string) {
	for _, v := range m {
		s.add(minSettingLen, v)
	}
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

// Contains reporta si str contiene algun secreto literal.
func (s *Scrubber) Contains(str string) bool {
	if s == nil || str == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sec := range s.secrets {
		if strings.Contains(str, sec) {
			return true
		}
	}
	return false
}

// Redact sustituye cada secreto literal por [redacted].
func (s *Scrubber) Redact(str string) string {
	if s == nil || str == "" {
		return str
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sec := range s.secrets {
		if strings.Contains(str, sec) {
			str = strings.ReplaceAll(str, sec, "[redacted]")
		}
	}
	return str
}

var (
	reURL    = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.\-]*://[^\s"'<>)\]]+`)
	reJWT    = regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{5,}\.[A-Za-z0-9_\-]{5,}\.?[A-Za-z0-9_\-]*`)
	reEmail  = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9\-]+(?:\.[A-Za-z0-9\-]+)*\.[A-Za-z]{2,}`)
	reAuth   = regexp.MustCompile(`(?i)\b(bearer|basic|token|apikey|api[_-]key)\s+[A-Za-z0-9._~+/=\-]{4,}`)
	rePAN    = regexp.MustCompile(`\d(?:[ \-]?\d){12,18}`)
	reDigits = regexp.MustCompile(`\d{6,}`)
	reToken  = regexp.MustCompile(`[A-Za-z0-9_\-+/=]{24,}`)
)

// MaxBreadcrumbLen es el largo maximo (runas) de una linea de breadcrumb.
const MaxBreadcrumbLen = 160

// ScrubText limpia un texto libre (linea de log del guest, mensaje): borra
// secretos por valor y luego URLs (queda el host), JWT, emails, credenciales
// en cabecera, PAN (Luhn), runs largos de digitos y tokens opacos. Es
// idempotente. Se usa para breadcrumbs; los campos estructurados usan
// allowlist en Sanitize y no pasan texto libre.
func ScrubText(str string, sc *Scrubber) string {
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

func scrubOnce(str string, sc *Scrubber) string {
	str = sc.Redact(str)
	str = reURL.ReplaceAllStringFunc(str, func(m string) string {
		if u, err := url.Parse(m); err == nil && u.Host != "" {
			return u.Scheme + "://" + u.Hostname()
		}
		return "[url]"
	})
	str = reJWT.ReplaceAllString(str, "[jwt]")
	str = reEmail.ReplaceAllString(str, "[email]")
	str = reAuth.ReplaceAllString(str, "[redacted]")
	str = rePAN.ReplaceAllStringFunc(str, func(m string) string {
		if luhnValid(digitsOnly(m)) {
			return "[pan]"
		}
		return m
	})
	str = reDigits.ReplaceAllString(str, "[num]")
	str = reToken.ReplaceAllStringFunc(str, func(m string) string {
		if looksOpaque(m) {
			return "[token]"
		}
		return m
	})
	return str
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

package importer

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// number_mixed: números escritos con separadores de miles y decimales mezclados
// ("$1.059,32", "1,059.32", "99,9", "1.059.32", "1 059,32"). Es una
// transformación genérica registrada como las demás; no conoce ninguna moneda
// ni origen. Regla de oro: lo ambiguo es error de fila, nunca se adivina.
//
// Se acepta, tras quitar un signo, símbolos de moneda ($, MXN, USD, EUR, €) y
// espacios de los extremos:
//
//   - enteros sin separadores: 1059
//   - un solo separador seguido de 1-2 dígitos o de 4 o más: decimal (99,9  3.5  0.059)
//   - un solo separador seguido de EXACTAMENTE 3 dígitos y precedido de 1-3
//     dígitos que no empiezan en 0: AMBIGUO (1.059 puede ser 1059 o 1,059).
//     Si hay más de 3 dígitos antes (1234.567) solo puede ser decimal.
//   - ambos separadores: el último es el decimal (aparece una vez) y el otro
//     agrupa miles en bloques de 3 (1.059,32 / 1,059.32)
//   - el mismo separador repetido con bloques de 3: miles (1.059.320)
//   - el mismo separador repetido cuyo último bloque tiene 1-2 dígitos: el último
//     es el decimal (1.059.32), siempre que los anteriores sean miles válidos
//   - espacio o apóstrofo como separador de miles (1 059,32), con bloques de 3
//
// Todo lo demás (letras, bloques de miles incompletos, separadores mezclados
// dentro de los miles) es inválido.
const TransformNumberMixed = "number_mixed"

// Errores tipificados de number_mixed; se envuelven con %w.
var (
	// ErrNumberAmbiguous: el texto admite dos lecturas (p. ej. "1.059").
	ErrNumberAmbiguous = errors.New("numero_ambiguo")
	// ErrNumberInvalid: el texto no tiene la forma de un número reconocible.
	ErrNumberInvalid = errors.New("numero_invalido")
)

func init() {
	RegisterRawTransform(TransformNumberMixed, func(_ TransformContext, raw string) (any, error) {
		return ParseMixedNumber(raw)
	})
}

var currencyTokens = []string{"MXN", "USD", "EUR", "$", "€"}

// ParseMixedNumber implementa number_mixed. Ver TransformNumberMixed.
func ParseMixedNumber(raw string) (float64, error) {
	s := strings.TrimSpace(strings.ReplaceAll(raw, " ", " "))
	upper := strings.ToUpper(s)
	for changed := true; changed; {
		changed = false
		for _, c := range currencyTokens {
			if strings.HasPrefix(upper, c) {
				s, upper, changed = strings.TrimSpace(s[len(c):]), strings.TrimSpace(upper[len(c):]), true
			} else if strings.HasSuffix(upper, c) {
				s, upper, changed = strings.TrimSpace(s[:len(s)-len(c)]), strings.TrimSpace(upper[:len(upper)-len(c)]), true
			}
		}
	}
	neg := false
	switch {
	case strings.HasPrefix(s, "-"):
		neg, s = true, strings.TrimSpace(s[1:])
	case strings.HasPrefix(s, "+"):
		s = strings.TrimSpace(s[1:])
	}
	bad := func(why string) (float64, error) {
		return 0, fmt.Errorf("%w: %q %s", ErrNumberInvalid, raw, why)
	}
	if s == "" {
		return bad("no trae dígitos")
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9') && !strings.ContainsRune(".,' ", r) {
			return bad("trae caracteres que no son de un número")
		}
	}
	if s[0] < '0' || s[0] > '9' {
		if s[0] != '.' && s[0] != ',' {
			return bad("empieza con un separador")
		}
	}

	dots, commas := strings.Count(s, "."), strings.Count(s, ",")
	var intPart, frac string
	switch {
	case dots > 0 && commas > 0:
		last := strings.LastIndexAny(s, ".,")
		dec := s[last]
		if strings.Count(s, string(dec)) != 1 {
			return bad("repite el separador decimal")
		}
		other := byte('.')
		if dec == '.' {
			other = ','
		}
		if strings.LastIndexByte(s, other) > last {
			return bad("el separador de miles va después del decimal")
		}
		intPart, frac = s[:last], s[last+1:]
		if frac == "" {
			return bad("termina en separador")
		}
	case dots+commas == 1:
		i := strings.IndexAny(s, ".,")
		before, after := s[:i], s[i+1:]
		if after == "" {
			return bad("termina en separador")
		}
		if len(after) == 3 && !strings.ContainsAny(before, " '") && len(before) >= 1 && len(before) <= 3 && before[0] != '0' {
			return 0, fmt.Errorf("%w: %q puede leerse como miles o como decimal", ErrNumberAmbiguous, raw)
		}
		if len(after) == 3 && strings.ContainsAny(before, " '") {
			// "1 059,234": el espacio ya fijó los miles, así que el separador es decimal.
			intPart, frac = before, after
			break
		}
		intPart, frac = before, after
	case dots+commas > 1:
		sep := "."
		if commas > 0 {
			sep = ","
		}
		groups := strings.Split(s, sep)
		last := groups[len(groups)-1]
		switch {
		case len(last) == 3:
			intPart = s // todos miles
		case len(last) >= 1 && len(last) <= 2:
			intPart, frac = strings.Join(groups[:len(groups)-1], sep), last
		default:
			return bad("el último bloque no tiene 3 dígitos ni es decimal de 1-2")
		}
	default:
		intPart = s
	}

	digits, err := groupedDigits(intPart)
	if err != nil {
		return bad(err.Error())
	}
	if frac != "" && strings.Trim(frac, "0123456789") != "" {
		return bad("la parte decimal no es numérica")
	}
	if digits == "" {
		if frac == "" {
			return bad("no trae dígitos")
		}
		digits = "0"
	}
	text := digits
	if frac != "" {
		text += "." + frac
	}
	v, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return bad("fuera de rango")
	}
	if neg {
		v = -v
	}
	return v, nil
}

// groupedDigits valida la parte entera: sin separadores son solo dígitos; con
// separadores todos son el mismo carácter, el primer bloque trae 1-3 dígitos y
// el resto exactamente 3. Devuelve los dígitos sin separadores.
func groupedDigits(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	sep := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			continue
		}
		if sep == 0 {
			sep = c
		} else if sep != c && !(isSpaceLike(sep) && isSpaceLike(c)) {
			return "", errors.New("mezcla separadores de miles distintos")
		}
	}
	if sep == 0 {
		return s, nil
	}
	var parts []string
	cur := strings.Builder{}
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			cur.WriteByte(s[i])
			continue
		}
		parts = append(parts, cur.String())
		cur.Reset()
	}
	parts = append(parts, cur.String())
	if len(parts[0]) < 1 || len(parts[0]) > 3 {
		return "", errors.New("el primer bloque de miles debe tener 1-3 dígitos")
	}
	for _, p := range parts[1:] {
		if len(p) != 3 {
			return "", errors.New("los bloques de miles deben tener 3 dígitos")
		}
	}
	return strings.Join(parts, ""), nil
}

func isSpaceLike(c byte) bool { return c == ' ' || c == '\'' }

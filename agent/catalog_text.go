package agent

import (
	"strings"
	"unicode"
)

// Text helpers of the semantic catalog: accent-insensitive Spanish folding,
// tokenization, a light stemmer and edit distance. All deterministic and
// allocation-light; nothing here calls a model.

// foldText lower-cases s and strips Spanish diacritics (á→a, ñ→n, ü→u).
func foldText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		switch r {
		case 'á', 'à', 'ä', 'â':
			r = 'a'
		case 'é', 'è', 'ë', 'ê':
			r = 'e'
		case 'í', 'ì', 'ï', 'î':
			r = 'i'
		case 'ó', 'ò', 'ö', 'ô':
			r = 'o'
		case 'ú', 'ù', 'ü', 'û':
			r = 'u'
		case 'ñ':
			r = 'n'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// catalogStopwords are folded Spanish/English words that carry no catalog
// meaning in a user request ("¿cuántos pedidos tengo de hoy?").
var catalogStopwords = map[string]bool{
	"a": true, "al": true, "como": true, "con": true, "cual": true, "cuales": true, "cuando": true,
	"cuanto": true, "cuanta": true, "cuantos": true, "cuantas": true, "de": true, "del": true,
	"dame": true, "dime": true, "donde": true, "el": true, "ella": true, "en": true, "es": true,
	"esta": true, "este": true, "estos": true, "estas": true, "hay": true, "la": true, "las": true,
	"le": true, "lo": true, "los": true, "me": true, "mi": true, "mis": true, "muestra": true,
	"muestrame": true, "necesito": true, "o": true, "para": true, "por": true, "que": true,
	"quiero": true, "se": true, "si": true, "sin": true, "su": true, "sus": true, "tengo": true,
	"tenemos": true, "un": true, "una": true, "unos": true, "unas": true, "y": true, "ver": true,
	"lista": true, "listar": true, "todos": true, "todas": true, "hoy": true,
	"ayer": true, "mes": true, "semana": true, "the": true, "of": true, "and": true, "for": true,
	"model": true, "field": true, "nav": true, "title": true, "id": true, "puedes": true,
	"favor": true, "ultimo": true, "ultimos": true, "ultima": true, "ultimas": true,
}

// catalogTokens splits s into folded word tokens (snake_case, camelCase,
// dotted keys and punctuation all separate words). Stopwords are dropped.
func catalogTokens(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) == 0 {
			return
		}
		w := foldText(string(cur))
		cur = cur[:0]
		if len(w) < 2 || catalogStopwords[w] {
			return
		}
		out = append(out, w)
	}
	prevLower := false
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			// camelCase boundary: salesOrder -> sales order
			if unicode.IsUpper(r) && prevLower {
				flush()
			}
			cur = append(cur, r)
			prevLower = unicode.IsLower(r) || unicode.IsDigit(r)
		default:
			flush()
			prevLower = false
		}
	}
	flush()
	return out
}

// stemLite reduces a folded token to a crude stem so singular/plural and
// gender variants meet: clientes/cliente -> client, sucursales/sucursal ->
// sucursal, facturas/factura -> factur, productos/product -> product.
func stemLite(w string) string {
	if len(w) <= 3 {
		return w
	}
	switch {
	case strings.HasSuffix(w, "ces") && len(w) > 4:
		w = w[:len(w)-3] + "z"
	case strings.HasSuffix(w, "ies") && len(w) > 4:
		w = w[:len(w)-3] + "y"
	case strings.HasSuffix(w, "s"):
		w = w[:len(w)-1]
	}
	if len(w) > 4 {
		switch w[len(w)-1] {
		case 'a', 'e', 'o':
			w = w[:len(w)-1]
		}
	}
	return w
}

// catalogStems tokenizes and stems s.
func catalogStems(s string) []string {
	toks := catalogTokens(s)
	for i, t := range toks {
		toks[i] = stemExpand(stemLite(t))
	}
	return toks
}

// stemVerbNouns maps verb stems users type onto the noun stem models are
// labeled with ("vendimos" -> venta, "compramos" -> compra).
var stemVerbNouns = []struct{ prefix, stem string }{
	{"vend", "vent"},
	{"compr", "compr"},
	{"factur", "factur"},
	{"cobr", "cobr"},
}

func stemExpand(stem string) string {
	for _, v := range stemVerbNouns {
		if strings.HasPrefix(stem, v.prefix) {
			return v.stem
		}
	}
	return stem
}

// editDistance is the Levenshtein distance between a and b (runes).
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// singularES is the regular Spanish singular of one
// word. It only derives synonyms, never resolves keys.
func singularES(w string) string {
	lw := foldText(w)
	switch {
	case len(lw) > 4 && strings.HasSuffix(lw, "ces"):
		return w[:len(w)-3] + "z"
	case len(lw) > 4 && strings.HasSuffix(lw, "es") && strings.IndexByte("lrndjy", lw[len(lw)-3]) >= 0:
		return w[:len(w)-2]
	case len(lw) > 3 && strings.HasSuffix(lw, "s"):
		return w[:len(w)-1]
	}
	return w
}

// inflectFirstWord applies fn to the first word of a label ("Pedidos de
// venta" -> "Pedido de venta").
func inflectFirstWord(label string, fn func(string) string) string {
	label = strings.TrimSpace(label)
	if label == "" {
		return ""
	}
	i := strings.IndexByte(label, ' ')
	if i < 0 {
		return fn(label)
	}
	return fn(label[:i]) + label[i:]
}

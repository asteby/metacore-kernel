package agent

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"

	"github.com/asteby/metacore-kernel/agent/tools"
)

// Word caps of the model-provided turn fields (also the fallback lengths).
const (
	memoryTitleWords   = 8
	memorySummaryWords = 20
	memoryFactRunes    = 160
	memoryMaxTurnFacts = 6
	memoryDigestRunes  = 180
	memoryMaxDigests   = 8
	memoryStoredRunes  = 4000
	// memoryIndexLineRunes caps an index line (about 20 tokens).
	memoryIndexLineRunes = 80
)

// memoryIndexHeader opens the conversation index.
const memoryIndexHeader = "[Memoria de la conversación] Turnos anteriores resumidos (#n · tema → resultado). " +
	"Si necesitas el detalle o los datos de uno, usa recall_turn; no repitas una consulta cuyo resultado ya está aquí."

// firstWords returns the first n words of s on one line, with "…" when cut.
func firstWords(s string, n int) string {
	words := strings.Fields(s)
	if len(words) <= n {
		return strings.Join(words, " ")
	}
	return strings.Join(words[:n], " ") + "…"
}

// oneLine collapses whitespace and caps s at max runes ("…" when cut).
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if max > 0 && len([]rune(s)) > max {
		return string([]rune(s)[:max-1]) + "…"
	}
	return s
}

// turnTitle is the model's title, or the start of the user's message.
func turnTitle(modelTitle, userMessage string) string {
	if t := firstWords(modelTitle, memoryTitleWords); t != "" {
		return t
	}
	if t := firstWords(userMessage, memoryTitleWords); t != "" {
		return t
	}
	return "(sin texto)"
}

// turnSummary is the model's summary, or the first sentence of the answer.
func turnSummary(modelSummary, answer string) string {
	if s := firstWords(modelSummary, memorySummaryWords); s != "" {
		return s
	}
	a := strings.Join(strings.Fields(stripChatMarkup(answer)), " ")
	if i := strings.IndexAny(a, ".!?\n"); i > 0 {
		a = a[:i+1]
	}
	if s := firstWords(a, memorySummaryWords); s != "" {
		return s
	}
	return "(sin respuesta)"
}

// stripChatMarkup drops the chat emphasis/bullet marks so summaries stay plain.
func stripChatMarkup(s string) string {
	return strings.NewReplacer("*", "", "_", "", "•", "", "`", "").Replace(s)
}

// cleanFacts normalizes the model's facts: one line each, capped, deduped.
func cleanFacts(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range in {
		f = oneLine(f, memoryFactRunes)
		k := strings.ToLower(f)
		if f == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, f)
		if len(out) == memoryMaxTurnFacts {
			break
		}
	}
	return out
}

// stringList reads a []string out of a JSON-decoded tool parameter.
func stringList(v interface{}) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		if t != "" {
			return []string{t}
		}
	}
	return nil
}

// renderIndex renders the index of turns (ascending) within budget tokens.
// When it does not fit, the oldest lines are dropped and counted. It is a
// pure function of its input: the same turns always render the same bytes.
func renderIndex(turns []TurnRecord, budget int) string {
	if len(turns) == 0 {
		return ""
	}
	lines := make([]string, len(turns))
	for i, t := range turns {
		lines[i] = indexLine(t)
	}
	used := EstimateTokens(memoryIndexHeader) + 20 // room for the omission note
	first := len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		n := EstimateTokens(lines[i]) + 1
		if budget > 0 && used+n > budget {
			break
		}
		used += n
		first = i
	}
	var sb strings.Builder
	sb.WriteString(memoryIndexHeader)
	if first > 0 {
		fmt.Fprintf(&sb, "\n(#%d–#%d omitidos por espacio; búscalos con recall_turn y query)", turns[0].Turn, turns[first-1].Turn)
	}
	for _, l := range lines[first:] {
		sb.WriteString("\n")
		sb.WriteString(l)
	}
	return sb.String()
}

// indexLine is "#n · title → summary".
func indexLine(t TurnRecord) string {
	return oneLine("#"+strconv.Itoa(t.Turn)+" · "+oneLine(t.Title, 50)+" → "+t.Summary, memoryIndexLineRunes)
}

// renderFacts renders the durable facts, in the given (stable) order, as a
// numbered list within budget tokens. Facts past the budget are left out.
func renderFacts(facts []MemoryFact, budget int) string {
	if len(facts) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("[Memoria del usuario] Datos y preferencias durables (respétalos; forget_fact si dejan de valer):")
	used := EstimateTokens(sb.String())
	for i, f := range facts {
		line := "\n" + strconv.Itoa(i+1) + ". " + oneLine(f.Text, memoryFactRunes)
		n := EstimateTokens(line)
		if budget > 0 && used+n > budget {
			break
		}
		used += n
		sb.WriteString(line)
	}
	return sb.String()
}

// renderTurnData renders what a turn left (facts and tool digests) within
// budget tokens, headed by label.
func renderTurnData(label string, t TurnRecord, budget int) string {
	items := append(append([]string(nil), t.Facts...), t.ToolDigests...)
	if len(items) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(label)
	used := EstimateTokens(label)
	for _, it := range items {
		line := "\n- " + it
		n := EstimateTokens(line)
		if budget > 0 && used+n > budget {
			break
		}
		used += n
		sb.WriteString(line)
	}
	return sb.String()
}

// memoryInternalTools never get a digest: they are memory plumbing, not data.
var memoryInternalTools = map[string]bool{
	"final_answer": true, "recall_turn": true, "remember_fact": true, "forget_fact": true,
}

// digestToolCall is a deterministic one-line digest of a tool call:
// "query_data(model=orders, limit=10) → 23 filas; total=4500". It never
// calls a model.
func digestToolCall(call tools.ToolCall, res tools.ToolResult) string {
	var sb strings.Builder
	sb.WriteString(call.ToolName)
	sb.WriteString("(")
	sb.WriteString(digestArgs(call.Parameters))
	sb.WriteString(") → ")
	if !res.Success {
		sb.WriteString("error: " + oneLine(res.Error, 60))
	} else {
		sb.WriteString(digestResult(res.Result))
	}
	return oneLine(sb.String(), memoryDigestRunes)
}

// digestArgs renders up to four arguments, sorted by name; long values are cut.
func digestArgs(params map[string]interface{}) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, 4)
	for _, k := range keys {
		if len(parts) == 4 {
			parts = append(parts, "…")
			break
		}
		v := compactValue(params[k], 40)
		if v == "" {
			continue
		}
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, ", ")
}

// compactValue renders a value compactly within max runes.
func compactValue(v interface{}, max int) string {
	var s string
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		s = t
	case bool, float64, float32, int, int64, int32, json.Number:
		s = fmt.Sprint(t)
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		s = string(raw)
	}
	return oneLine(s, max)
}

// rowListKeys are the result keys that usually hold the returned rows.
var rowListKeys = []string{"data", "rows", "records", "items", "results"}

// rowCountKeys are the result keys that usually hold a total.
var rowCountKeys = []string{"total", "count", "total_count", "rows_count"}

// digestResult summarizes a tool result: row count and up to three numbers.
func digestResult(r interface{}) string {
	switch t := r.(type) {
	case nil:
		return "ok"
	case []interface{}:
		return strconv.Itoa(len(t)) + " filas"
	case []map[string]interface{}:
		return strconv.Itoa(len(t)) + " filas"
	case string:
		return oneLine(t, 60)
	case map[string]interface{}:
		var parts []string
		counted := map[string]bool{}
		for _, k := range rowCountKeys {
			if n, ok := asNumber(t[k]); ok {
				parts = append(parts, k+"="+formatNumber(n))
				counted[k] = true
				break
			}
		}
		for _, k := range rowListKeys {
			if l, ok := t[k].([]interface{}); ok {
				parts = append(parts, strconv.Itoa(len(l))+" filas")
				break
			}
			if l, ok := t[k].([]map[string]interface{}); ok {
				parts = append(parts, strconv.Itoa(len(l))+" filas")
				break
			}
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		nums := 0
		for _, k := range keys {
			if counted[k] || nums == 3 {
				continue
			}
			if n, ok := asNumber(t[k]); ok {
				parts = append(parts, k+"="+formatNumber(n))
				nums++
			}
		}
		if len(parts) == 0 {
			if s, ok := t["status"].(string); ok && s != "" {
				return s
			}
			if s, ok := t["message"].(string); ok && s != "" {
				return oneLine(s, 60)
			}
			return "ok"
		}
		return strings.Join(parts, "; ")
	}
	return "ok"
}

func asNumber(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func formatNumber(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', 2, 64)
}

// memoryStopwords are frequent Spanish words ignored by the lexical match.
var memoryStopwords = map[string]bool{
	"para": true, "como": true, "cual": true, "cuales": true, "cuanto": true, "cuantos": true,
	"cuantas": true, "donde": true, "esta": true, "este": true, "estos": true, "estas": true,
	"esos": true, "esas": true, "ese": true, "esa": true, "eso": true, "pero": true, "porque": true,
	"sobre": true, "tiene": true, "tienen": true, "hay": true, "fue": true, "son": true, "que": true,
	"los": true, "las": true, "del": true, "una": true, "unos": true, "unas": true, "con": true,
	"por": true, "mas": true, "muy": true, "todo": true, "todos": true, "dame": true, "dime": true,
	"puedes": true, "quiero": true, "necesito": true, "antes": true, "dijiste": true, "hola": true,
	"gracias": true, "favor": true, "entonces": true, "tambien": true, "algo": true, "cosa": true,
}

var accentStripper = transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)

// significantWords lowercases, strips accents and keeps distinct words of at
// least four letters that are not stopwords.
func significantWords(s string) map[string]bool {
	if folded, _, err := transform.String(accentStripper, strings.ToLower(s)); err == nil {
		s = folded
	} else {
		s = strings.ToLower(s)
	}
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len([]rune(w)) < 4 || memoryStopwords[w] {
			continue
		}
		out[w] = true
	}
	return out
}

// lexicalScore is the share of the query's significant words found in the
// turn (title, summary, facts, user message), and how many were shared.
func lexicalScore(query map[string]bool, t TurnRecord) (float64, int) {
	if len(query) == 0 {
		return 0, 0
	}
	doc := significantWords(t.Title + " " + t.Summary + " " + strings.Join(t.Facts, " ") + " " + t.UserMessage)
	shared := 0
	for w := range query {
		if doc[w] {
			shared++
		}
	}
	return float64(shared) / float64(len(query)), shared
}

// cosine is the cosine similarity of a and b (0 when sizes differ).
func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// bestLexicalTurn finds the turn that best matches query among candidates
// (ties go to the most recent turn); ok is false below the threshold.
func bestLexicalTurn(query string, candidates []TurnRecord, threshold float64) (TurnRecord, bool) {
	q := significantWords(query)
	var best TurnRecord
	bestScore, found := 0.0, false
	for _, t := range candidates {
		score, shared := lexicalScore(q, t)
		if shared < 2 && !(len(q) == 1 && shared == 1) {
			continue
		}
		if score >= threshold && score >= bestScore {
			best, bestScore, found = t, score, true
		}
	}
	return best, found
}

// bestSemanticTurn finds the most similar turn by embedding (ties go to the
// most recent turn); ok is false below the threshold or without vectors.
func bestSemanticTurn(vec []float32, candidates []TurnRecord, threshold float64) (TurnRecord, bool) {
	var best TurnRecord
	bestScore, found := 0.0, false
	for _, t := range candidates {
		s := cosine(vec, t.Embedding)
		if s >= threshold && s >= bestScore {
			best, bestScore, found = t, s, true
		}
	}
	return best, found
}

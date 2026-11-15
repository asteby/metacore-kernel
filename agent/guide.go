package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/asteby/metacore-kernel/agent/contextmgr"
)

// GuideDescriptor is the safe, compact catalog published by the UI bundles
// loaded in the user's browser (host built-ins, addon manifests and the
// synthetic CRUD tours derived from the nav + model metadata). It
// intentionally excludes routes/selectors.
type GuideDescriptor struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Intents     []string `json:"intents"`
}

type SuggestedGuide struct {
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	Confidence float64 `json:"confidence"`
}

const (
	maxGuideCatalog   = 100
	maxGuideTextRunes = 160
)

// GuideTurn carries one chat turn's guide catalog into the agent loop and the
// guide the model decided to start (if any) back out to the handler.
//
// Teach-vs-do is a language decision, so the model makes it — not a verb list.
// The catalog is whatever the browser advertises, so new models, nav entries
// and addon guides show up here with no backend change.
type GuideTurn struct {
	catalog []GuideDescriptor
	mu      sync.Mutex
	chosen  *SuggestedGuide
}

// HasCatalog reports whether the browser advertised any guide this turn.
func (t *GuideTurn) HasCatalog() bool { return t != nil && len(t.catalog) > 0 }

// NewGuideTurn sanitizes the client catalog (it is untrusted input that ends
// up in a tool description): drops incomplete rows, dedupes, caps size/length.
func NewGuideTurn(catalog []GuideDescriptor) *GuideTurn {
	seen := map[string]bool{}
	clean := make([]GuideDescriptor, 0, len(catalog))
	for _, g := range catalog {
		id := strings.TrimSpace(g.ID)
		title := clipGuideText(g.Title)
		if id == "" || title == "" || seen[id] || len(id) > 128 {
			continue
		}
		seen[id] = true
		clean = append(clean, GuideDescriptor{ID: id, Title: title, Description: clipGuideText(g.Description)})
		if len(clean) == maxGuideCatalog {
			break
		}
	}
	return &GuideTurn{catalog: clean}
}

func clipGuideText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxGuideTextRunes {
		s = string(r[:maxGuideTextRunes]) + "…"
	}
	return s
}

// Chosen is the guide start_guide selected during the turn, or nil.
func (t *GuideTurn) Chosen() *SuggestedGuide {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.chosen
}

func (t *GuideTurn) find(id string) *GuideDescriptor {
	for i := range t.catalog {
		if t.catalog[i].ID == id {
			return &t.catalog[i]
		}
	}
	return nil
}

type guideTurnKey struct{}

// WithGuideTurn attaches the turn's guide catalog to ctx.
func WithGuideTurn(ctx context.Context, turn *GuideTurn) context.Context {
	return context.WithValue(ctx, guideTurnKey{}, turn)
}

// GuideTurnFrom returns the turn attached by WithGuideTurn, or nil.
func GuideTurnFrom(ctx context.Context) *GuideTurn {
	turn, _ := ctx.Value(guideTurnKey{}).(*GuideTurn)
	return turn
}

// The guide tools have FIXED definitions: the catalog the browser sends
// changes per user, screen and deploy, and listing it in a tool description
// (or as an enum) changed the tools block on every turn, which defeats the
// provider's prompt cache and cost thousands of tokens. The model finds a
// guide with search_guides and starts it with start_guide; the id is checked
// against this turn's catalog when the tool runs.

// maxGuideSearchResults bounds what search_guides returns.
const maxGuideSearchResults = 8

// maxGuideLineRunes clips the description in a search_guides line.
const maxGuideLineRunes = 90

// StartGuideTool lets the model start a visual walkthrough. It is registered
// whenever the browser attached a guide turn; guide_id must be one of the
// turn's guides, so the model can never start an invented one.
type StartGuideTool struct{ turn *GuideTurn }

func NewStartGuideTool(turn *GuideTurn) *StartGuideTool { return &StartGuideTool{turn: turn} }

func (t *StartGuideTool) Name() string     { return "start_guide" }
func (t *StartGuideTool) Category() string { return "workflow" }

func (t *StartGuideTool) Description() string {
	return "Inicia una guía visual paso a paso en la pantalla del usuario (ilumina el menú, los botones y los campos). " +
		"Úsala SOLO cuando el usuario quiere aprender o que le muestres cómo se hace algo en la interfaz. " +
		"Si pide que TÚ hagas la acción (crear, registrar, modificar… aunque todavía no dé los datos), NO la uses: usa las tools de datos. " +
		"Obtén antes el guide_id exacto con search_guides; no inventes ids."
}

func (t *StartGuideTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"guide_id": map[string]interface{}{
			"type":        "string",
			"description": "Id exacto de la guía, tal como lo devuelve search_guides.",
			"required":    true,
		},
	}
}

func (t *StartGuideTool) Execute(_ context.Context, params map[string]interface{}, _ *contextmgr.ConversationContext) (interface{}, error) {
	id, _ := params["guide_id"].(string)
	id = strings.TrimSpace(id)
	guide := t.turn.find(id)
	if guide == nil {
		return nil, unknownGuideError(t.turn, id)
	}
	t.turn.mu.Lock()
	t.turn.chosen = &SuggestedGuide{ID: guide.ID, Title: guide.Title, Confidence: 1}
	t.turn.mu.Unlock()
	return map[string]interface{}{
		"started": true,
		"title":   guide.Title,
		"note":    "La guía ya se muestra en pantalla. Responde en una sola frase corta; no repitas los pasos.",
	}, nil
}

// unknownGuideError explains a bad guide_id and offers the closest guides.
func unknownGuideError(turn *GuideTurn, id string) error {
	if !turn.HasCatalog() {
		return fmt.Errorf("guía %q no disponible: en esta pantalla no hay guías visuales; explica los pasos en texto", id)
	}
	hits := turn.Search(id, 3)
	if len(hits) == 0 {
		return fmt.Errorf("guía %q no existe en esta sesión; busca el id exacto con search_guides", id)
	}
	lines := make([]string, len(hits))
	for i, g := range hits {
		lines[i] = g.ID + " (" + g.Title + ")"
	}
	return fmt.Errorf("guía %q no existe en esta sesión. Quizá quisiste: %s. Usa un id exacto o busca con search_guides",
		id, strings.Join(lines, "; "))
}

// SearchGuidesTool finds guides of this turn's catalog by keywords.
type SearchGuidesTool struct{ turn *GuideTurn }

func NewSearchGuidesTool(turn *GuideTurn) *SearchGuidesTool { return &SearchGuidesTool{turn: turn} }

func (t *SearchGuidesTool) Name() string     { return "search_guides" }
func (t *SearchGuidesTool) Category() string { return "workflow" }

func (t *SearchGuidesTool) Description() string {
	return "Busca guías visuales de la interfaz por palabras clave (p. ej. \"crear cliente\", \"facturas\") y devuelve hasta 8 con su id. " +
		"Úsala antes de start_guide cuando el usuario quiere que le enseñes cómo hacer algo en pantalla."
}

func (t *SearchGuidesTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"query": map[string]interface{}{
			"type":        "string",
			"description": "Qué quiere aprender el usuario, en pocas palabras (acción y pantalla o módulo).",
			"required":    true,
		},
	}
}

func (t *SearchGuidesTool) Execute(_ context.Context, params map[string]interface{}, _ *contextmgr.ConversationContext) (interface{}, error) {
	query, _ := params["query"].(string)
	if !t.turn.HasCatalog() {
		return map[string]interface{}{
			"guides": []string{},
			"note":   "No hay guías visuales en esta pantalla; explica los pasos en texto.",
		}, nil
	}
	hits := t.turn.Search(query, maxGuideSearchResults)
	if len(hits) == 0 {
		return map[string]interface{}{
			"guides": []string{},
			"note":   "Ninguna guía coincide. Prueba con otras palabras (la acción y el nombre de la pantalla o módulo).",
		}, nil
	}
	lines := make([]string, len(hits))
	for i, g := range hits {
		line := g.ID + " — " + g.Title
		if g.Description != "" {
			line += ": " + clipRunes(g.Description, maxGuideLineRunes)
		}
		lines[i] = line
	}
	return map[string]interface{}{
		"guides": lines,
		"note":   "Inicia la adecuada con start_guide usando el id exacto (lo que va antes de \" — \").",
	}, nil
}

// Search ranks the turn's guides against query by keyword overlap on id,
// title and description (case- and accent-insensitive, Spanish stopwords
// ignored, plural/derived forms matched by shared stem) and returns at most
// limit guides with a positive score, best first; ties keep catalog order.
func (t *GuideTurn) Search(query string, limit int) []GuideDescriptor {
	if t == nil || limit <= 0 {
		return nil
	}
	terms := guideTerms(query)
	if len(terms) == 0 {
		return nil
	}
	type scored struct {
		g     GuideDescriptor
		score float64
		pos   int
	}
	var hits []scored
	for i, g := range t.catalog {
		fields := []struct {
			words  []string
			weight float64
		}{
			{guideTerms(g.Title), 3},
			{guideTerms(g.ID), 2},
			{guideTerms(g.Description), 1},
		}
		score := 0.0
		for _, q := range terms {
			best := 0.0
			for _, f := range fields {
				for _, w := range f.words {
					if m := termMatch(q, w) * f.weight; m > best {
						best = m
					}
				}
			}
			score += best
		}
		if score > 0 {
			hits = append(hits, scored{g, score, i})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].pos < hits[j].pos
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]GuideDescriptor, len(hits))
	for i, h := range hits {
		out[i] = h.g
	}
	return out
}

// termMatch scores a query word against a field word: 1 when equal, 0.7 when
// they share a stem (one is a prefix of the other, or a common prefix of at
// least 4 letters covering most of the shorter: cliente/clientes,
// crear/creacion, factura/facturacion), 0 otherwise.
func termMatch(q, w string) float64 {
	if q == w {
		return 1
	}
	short := len(q)
	if len(w) < short {
		short = len(w)
	}
	if short < 4 {
		return 0
	}
	common := 0
	for common < short && q[common] == w[common] {
		common++
	}
	if common == short || (common >= 4 && float64(common) >= 0.75*float64(short)) {
		return 0.7
	}
	return 0
}

// guideStopwords are Spanish filler words that carry no search intent.
var guideStopwords = map[string]bool{
	"al": true, "como": true, "con": true, "de": true, "del": true, "el": true, "en": true,
	"es": true, "la": true, "las": true, "lo": true, "los": true, "me": true, "mi": true, "mis": true,
	"para": true, "por": true, "que": true, "se": true, "su": true, "sus": true, "un": true,
	"una": true, "uno": true, "unos": true, "unas": true, "hago": true, "hacer": true,
	"puedo": true, "quiero": true, "ensename": true, "muestrame": true, "guia": true, "synth": true,
}

// guideFold removes Spanish accents from lower-cased text.
var guideFold = strings.NewReplacer("á", "a", "é", "e", "í", "i", "ó", "o", "ú", "u", "ü", "u", "ñ", "n")

// guideTerms normalizes s into search words: lower-cased, accents folded,
// split on anything that is not a letter or digit (ids split on . _ -),
// one-letter words and stopwords dropped.
func guideTerms(s string) []string {
	s = guideFold.Replace(strings.ToLower(s))
	words := strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	out := words[:0]
	for _, w := range words {
		if len(w) < 2 || guideStopwords[w] {
			continue
		}
		out = append(out, w)
	}
	return out
}

func clipRunes(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// ParallelSafe: the search only reads the turn's catalog.
func (t *SearchGuidesTool) ParallelSafe() bool { return true }

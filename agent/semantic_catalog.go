package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/agent/tools"
	"github.com/google/uuid"
)

// The semantic catalog bundle (CopilotConfig.SemanticCatalog) cuts the calls
// and tokens of a data turn:
//
//   - the system prompt gains the fixed ERP-Card legend and one rule (stable,
//     so it stays in the cached prefix);
//   - before the first model call, the cards of the models that match the
//     user's message are retrieved (no model call) and appended to the turn
//     context block, after the user's message — never to the system prompt;
//   - catalog_search, list_models, describe_model and query_data are served
//     by the kernel: cards instead of schema JSON, a one-line-per-model list,
//     and a single query tool (filters, aggregates, pagination) whose rows
//     come back as a Markdown table.
//
// With the right card already in context, a lookup is one query_data call
// and the answer (2 model calls) instead of list_models -> describe_model ->
// query_data -> answer (4).

// SemanticCatalogOptions tune the bundle. Every field is optional.
type SemanticCatalogOptions struct {
	// CatalogOptions tune the index (labels, synonyms, embedder, refresh).
	CatalogOptions
	// Index shares one index between copilots (presets). Default: one index
	// per copilot over CRUD's catalog.
	Index *CatalogIndex
	// PrefetchK is how many cards the prefetch adds (default 3; negative
	// disables the prefetch).
	PrefetchK int
	// PrefetchTokenBudget caps the prefetch block (default 800 tokens).
	PrefetchTokenBudget int
	// PrefetchTimeout bounds the retrieval (embedder included; default 1.5s).
	PrefetchTimeout time.Duration
	// StrictSchema renders query_data's schema for strict structured outputs.
	StrictSchema bool
	// KeepHostTools keeps the host's list_models / describe_model /
	// query_data instead of the kernel ones (catalog_search is still added).
	KeepHostTools bool
}

// Prefetch defaults.
const (
	DefaultPrefetchK           = 3
	DefaultPrefetchTokenBudget = 800
	DefaultPrefetchTimeout     = 1500 * time.Millisecond
)

// CatalogPromptClause is the stable system-prompt section of the bundle: the
// ERP-Card legend and how to use the per-turn cards.
const CatalogPromptClause = "\n\nDATOS — catálogo semántico.\n" + ERPCardLegend + "\n" +
	"Cada turno puede traer [Catálogo relevante] con las tarjetas de los modelos que coinciden con el mensaje: si el modelo está ahí, llama query_data directo (sin list_models ni describe_model). " +
	"Si no está, usa catalog_search. Conteos y totales: aggregates en query_data. Meta: una consulta y luego la respuesta."

// prefetchHeader opens the prefetch block.
const prefetchHeader = "[Catálogo relevante] Modelos que coinciden con el mensaje (ERP-Card). Si el que necesitas está aquí, consulta directo con query_data:"

// catalogIndex returns the bundle's index (nil when the bundle is off).
func (s *CopilotService) catalogIndex() *CatalogIndex {
	if !s.cfg.SemanticCatalog {
		return nil
	}
	return s.catalog
}

// CatalogIndex exposes the copilot's semantic catalog (nil when off), for
// hosts that render cards in their own tools.
func (s *CopilotService) CatalogIndex() *CatalogIndex { return s.catalogIndex() }

// initSemanticCatalog validates and builds the bundle at construction.
func (s *CopilotService) initSemanticCatalog() error {
	if !s.cfg.SemanticCatalog {
		return nil
	}
	if s.cfg.CRUD == nil {
		return fmt.Errorf("agent: CopilotConfig.SemanticCatalog needs CopilotConfig.CRUD")
	}
	s.catalog = s.cfg.Catalog.Index
	if s.catalog == nil {
		s.catalog = NewCatalogIndex(s.cfg.CRUD.cfg.Catalog, s.cfg.Catalog.CatalogOptions)
	}
	return nil
}

// catalogPromptClause is "" when the bundle is off.
func (s *CopilotService) catalogPromptClause() string {
	if s.catalogIndex() == nil {
		return ""
	}
	return CatalogPromptClause
}

// catalogTools are the kernel data tools of the bundle for one turn.
func (s *CopilotService) catalogTools() []tools.BaseTool {
	ix := s.catalogIndex()
	if ix == nil {
		return nil
	}
	maxResult, _ := s.turnLimits()
	budget := DefaultCompactMaxBytes
	if maxResult > 0 && maxResult-400 < budget {
		budget = max(maxResult-400, 1000)
	}
	ct := CatalogTools{CRUD: s.cfg.CRUD, Index: ix, StrictSchema: s.cfg.Catalog.StrictSchema, MaxResultBytes: budget}
	all := ct.Tools()
	if !s.cfg.Catalog.KeepHostTools {
		return all
	}
	return all[:1] // catalog_search only
}

// CatalogPrefetch is the per-turn prefetch block for message: the cards of
// the models it matches, within the token budget, or "" (bundle off,
// chitchat, nothing relevant). It never calls a language model.
func (s *CopilotService) CatalogPrefetch(ctx context.Context, orgID, userID uuid.UUID, message string) string {
	ix := s.catalogIndex()
	opt := s.cfg.Catalog
	if ix == nil || opt.PrefetchK < 0 || strings.TrimSpace(message) == "" || isChitchat(message) {
		return ""
	}
	k := opt.PrefetchK
	if k == 0 {
		k = DefaultPrefetchK
	}
	budget := opt.PrefetchTokenBudget
	if budget <= 0 {
		budget = DefaultPrefetchTokenBudget
	}
	timeout := opt.PrefetchTimeout
	if timeout <= 0 {
		timeout = DefaultPrefetchTimeout
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	identity := &AgentIdentity{OrganizationID: orgID, UserID: userID}
	return BuildPrefetchBlock(ctx, s.cfg.CRUD, identity, ix, message, k, budget)
}

// BuildPrefetchBlock retrieves the top-k cards for message that identity may
// read and packs them under budget tokens (a card that does not fit is
// retried with fewer fields, then dropped). "" when nothing matches.
func BuildPrefetchBlock(ctx context.Context, crud *CRUDService, identity *AgentIdentity, ix *CatalogIndex, message string, k, budget int) string {
	if crud == nil || ix == nil {
		return ""
	}
	hits := ix.Search(ctx, message, k, func(m CatalogModel) bool {
		_, ok := crud.visibleModel(identity, m)
		return ok
	})
	if len(hits) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(prefetchHeader)
	used := EstimateTokens(prefetchHeader)
	added := 0
	for _, h := range hits {
		m, _ := crud.visibleModel(identity, h.Model)
		for _, fields := range []int{ix.opts.CardFields, 10, 6} {
			card := RenderCard(m, CardOptions{MaxFields: fields, MaxEnum: 6})
			if n := EstimateTokens(card) + 1; used+n <= budget {
				b.WriteString("\n" + card)
				used += n
				added++
				break
			}
		}
	}
	if added == 0 {
		return ""
	}
	return b.String()
}

// turnContextFor is the turn context of a copilot turn: today's date and,
// with the bundle, the prefetched cards.
func (s *CopilotService) turnContextFor(ctx context.Context, orgID, userID uuid.UUID, message string) string {
	tc := s.TurnContext(orgID, userID)
	if block := s.CatalogPrefetch(ctx, orgID, userID, message); block != "" {
		if tc != "" {
			tc += "\n\n"
		}
		tc += block
	}
	return tc
}

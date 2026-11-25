package agent

import (
	"context"
	"fmt"
	"github.com/asteby/metacore-kernel/provenance"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/agent/tools"
)

// Context metadata keys the copilot uses to hand attachment data to the loop.
const (
	// MetaImagePaths holds local image paths for model vision (consumed once).
	MetaImagePaths = "aby_image_paths"
	// MetaImageURLs holds the public URLs of attached images, for image fields
	// on create/update (vision alone does not set them).
	MetaImageURLs = "aby_image_urls"
)

// ProcessResult is the transport-level outcome of a turn.
type ProcessResult struct {
	Message       string
	Images        []ImageResult
	Audio         []AudioResult
	ShouldDisable bool
	Metadata      map[string]interface{}
	Proposals     []*MutationProposal
	Embeds        []ChatEmbed
}

// AudioResult is an audio clip to deliver.
type AudioResult struct{ URL string }

// ImageResult is an image to deliver.
type ImageResult struct {
	URL     string
	Caption string
}

// ToProcessResult maps an orchestrator response to a ProcessResult.
func ToProcessResult(response *AgentResponse) *ProcessResult {
	result := &ProcessResult{
		Message:       response.Message,
		Images:        make([]ImageResult, len(response.Images)),
		Audio:         make([]AudioResult, len(response.Audio)),
		ShouldDisable: response.ShouldDisable,
		Metadata:      response.Metadata,
		Embeds:        response.Embeds,
	}
	for i, img := range response.Images {
		result.Images[i] = ImageResult{URL: img.URL, Caption: img.Caption}
	}
	for i, a := range response.Audio {
		result.Audio[i] = AudioResult{URL: a.URL}
	}
	return result
}

// CopilotConfig wires the embedded copilot to the host.
type CopilotConfig struct {
	Orchestrator *AgentOrchestrator
	// CRUD applies approved proposals. Required for ApplyProposal.
	CRUD *CRUDService
	// Capabilities renders the authoritative capability clause. Optional, and
	// only used when InjectCapabilities is set.
	Capabilities *CapabilityRegistry
	// InjectCapabilities appends the capability clause (a JSON list of up to
	// 120 model/action entries) to the system prompt. Off by default: it cost
	// 1-3.6k tokens and many permission checks per turn, and permissions are
	// enforced by the tools anyway.
	InjectCapabilities bool
	// Brand supplies the persona (base prompt, mascot name, platform name,
	// tone addendum) per org. Required.
	Brand BrandProvider
	// Clock gives the org-local "now". Optional; UTC when nil.
	Clock ClockZone
	// ClockInSystemPrompt restores the legacy clock: NowClause (minute
	// precision) inside the system prompt. It changes the prompt every minute,
	// so no provider can cache it. Off by default: today's date (day
	// precision) travels in the turn context after the user's message.
	ClockInSystemPrompt bool
	// ClockTimeOfDay adds the local time (HH:MM) to the turn context.
	ClockTimeOfDay bool
	// MaxToolResultBytes caps each tool result fed back to the model during
	// copilot turns. Zero takes DefaultCopilotMaxToolResultBytes; negative
	// defers to the orchestrator's MaxToolResultBytes.
	MaxToolResultBytes int
	// HistoryTokenBudget bounds the history sent each turn (newest first, by
	// EstimateTokens; the user's message is always kept). Zero takes
	// DefaultCopilotHistoryTokenBudget; negative sends the whole loaded history.
	HistoryTokenBudget int
	// LayeredMemory replaces the raw history with layered memory: a recent
	// window of exchanges, an index of the older turns at the head of the
	// history, the recall_turn/remember_fact/forget_fact tools, durable
	// per-user facts and related-turn prefetch in the turn context (see
	// memory.go). Off by default. HistoryTokenBudget still caps the window.
	LayeredMemory bool
	// Memory tunes LayeredMemory (store, embedder, budgets). Zero values take
	// the defaults; without Store, memory is process-local.
	Memory MemoryConfig
	// Conversations reads earlier turns' attachments. Optional.
	Conversations ConversationStore
	// Proposals is the pending-mutation store (default GlobalProposalStore; NewGormProposalStore persists them).
	Proposals ProposalRepository
	// PrepareContext stamps the context with what the model layer needs for
	// this turn (org AI runtime, billing identity, live mode). Optional.
	PrepareContext func(ctx context.Context, orgID, userID uuid.UUID, live bool) context.Context
	// Attachments tunes attachment enrichment. Optional.
	Attachments AttachmentOptions
	// RecordTitle names a created record for the proposal embed. Optional;
	// the default uses the first of name/title/label/code.
	RecordTitle func(created map[string]any, modelTitle string) string
	// LiveSuffix is appended to the system prompt in voice mode.
	LiveSuffix string
	// TurnTools adds per-org/per-user tools for one turn on top of the
	// orchestrator registry (for example the organisation's dynamic tools).
	// Optional.
	TurnTools func(ctx context.Context, orgID, userID uuid.UUID) []tools.BaseTool
	// ProposalSummary phrases what the agent proposed, for the provenance of
	// an approved proposal ("Propuso crear Hallazgo «…»"). Optional; the
	// default is "<action> <model title>".
	ProposalSummary func(p *MutationProposal) string
	// ProposalOrigin labels where approvals happen ("Borrador aprobado en el
	// chat"). Optional.
	ProposalOrigin string
	// SemanticCatalog turns on the semantic catalog bundle (see
	// semantic_catalog.go): the ERP-Card legend in the system prompt, the
	// per-turn card prefetch in the turn context, and the kernel's
	// catalog_search / list_models / describe_model / query_data tools.
	// Requires CRUD. Off by default.
	SemanticCatalog bool
	// Catalog tunes the semantic catalog bundle. Optional.
	Catalog SemanticCatalogOptions
}

// CopilotService runs turns of the embedded copilot and applies the changes
// the user approves in chat. Storage is separate from any business inbox:
// history comes from the copilot thread and the agent is transient.
type CopilotService struct {
	cfg     CopilotConfig
	memory  *copilotMemory // nil unless LayeredMemory
	catalog *CatalogIndex  // semantic catalog (SemanticCatalog)
}

// Copilot defaults (see CopilotConfig).
const (
	DefaultCopilotMaxToolResultBytes = 8_000
	DefaultCopilotHistoryTokenBudget = 4_000
)

// DefaultLiveSuffix is the voice-mode instruction.
const DefaultLiveSuffix = "\n\nMODO LIVE (voz): responde en 1–3 frases cortas y hablables. Sigue usando tools y previews de aprobación; no narres schemas ni dumps técnicos."

// NewCopilotService validates cfg and builds the service.
func NewCopilotService(cfg CopilotConfig) (*CopilotService, error) {
	if cfg.Orchestrator == nil {
		return nil, fmt.Errorf("agent: CopilotConfig.Orchestrator is required")
	}
	if cfg.Brand == nil {
		return nil, fmt.Errorf("agent: CopilotConfig.Brand is required")
	}
	if cfg.Proposals == nil {
		cfg.Proposals = GlobalProposalStore
	}
	if cfg.LiveSuffix == "" {
		cfg.LiveSuffix = DefaultLiveSuffix
	}
	svc := &CopilotService{cfg: cfg}
	if cfg.LayeredMemory {
		svc.memory = newCopilotMemory(cfg.Memory)
	}
	if err := svc.initSemanticCatalog(); err != nil {
		return nil, err
	}
	return svc, nil
}

// BrandClause tells the model which commercial name to use (white-label):
// never an internal codename.
func BrandClause(p Personality) string {
	name := strings.TrimSpace(p.PlatformName)
	lower := strings.ToLower(name)
	internal := name == ""
	for _, n := range p.InternalNames {
		if strings.ToLower(strings.TrimSpace(n)) == lower {
			internal = true
		}
	}
	if internal {
		return "\nNombre comercial: no uses un nombre de producto; di \"el sistema\" o \"la plataforma\"."
	}
	return "\nNombre comercial de la plataforma: " + name +
		". Si hace falta nombrar el producto, usa SOLO ese nombre (nunca un nombre interno del producto)."
}

// PersonalityClause appends the vertical's own tone/domain addendum.
func PersonalityClause(p Personality) string {
	text := strings.TrimSpace(p.Addendum)
	if text == "" {
		return ""
	}
	return "\n\nPERSONALIDAD Y CONTEXTO DE LA VERTICAL:\n" + text
}

// NowClause is the "current date/time" line in the org's zone, so "hoy"
// matches the business's local day. It is minute-precise, so a prompt that
// carries it changes every minute; the copilot only uses it with
// CopilotConfig.ClockInSystemPrompt (see TodayClause).
func NowClause(now time.Time) string {
	return "\nFecha y hora actual (" + now.Location().String() + "): " + now.Format("Monday 2006-01-02 15:04") +
		". Usa esta fecha/hora como \"hoy\" y \"ahora\" — ya está en la zona horaria del negocio; NUNCA inventes la fecha ni la conviertas a otra zona."
}

var spanishWeekdays = [...]string{"domingo", "lunes", "martes", "miércoles", "jueves", "viernes", "sábado"}

// TodayClause is the turn-context line with today's date (day precision) in
// the org's zone, and the local time when withTime is set. It goes after the
// user's message, never in the system prompt, so the cached prefix survives.
func TodayClause(now time.Time, withTime bool) string {
	when := spanishWeekdays[now.Weekday()] + " " + now.Format("2006-01-02")
	if withTime {
		when += ", " + now.Format("15:04")
	}
	return "[Contexto del turno] Hoy es " + when + " (zona " + now.Location().String() + "). " +
		"Usa esta fecha como \"hoy\" — ya está en la zona horaria del negocio; NUNCA inventes la fecha ni la conviertas a otra zona."
}

// SystemPrompt assembles the copilot prompt for a user: the host's base prompt
// followed by the brand and tone clauses (plus the legacy clock and the
// capability clause when configured). It only holds content that is stable
// across turns, so the provider can cache it.
func (s *CopilotService) SystemPrompt(orgID, userID uuid.UUID, live bool) (prompt string, p Personality) {
	p = s.cfg.Brand.PersonalityFor(orgID)
	prompt = p.SystemPrompt + BrandClause(p) + PersonalityClause(p)
	if s.cfg.ClockInSystemPrompt {
		prompt += NowClause(s.now(orgID, userID))
	}
	if s.cfg.InjectCapabilities && s.cfg.Capabilities != nil {
		prompt += s.cfg.Capabilities.PromptClause(orgID, userID)
	}
	prompt += s.catalogPromptClause()
	if live {
		prompt += s.cfg.LiveSuffix
	}
	return prompt, p
}

// TurnContext is the per-turn block attached after the user's message:
// today's date in the org's zone ("" with the legacy ClockInSystemPrompt).
func (s *CopilotService) TurnContext(orgID, userID uuid.UUID) string {
	if s.cfg.ClockInSystemPrompt {
		return ""
	}
	return TodayClause(s.now(orgID, userID), s.cfg.ClockTimeOfDay)
}

func (s *CopilotService) now(orgID, userID uuid.UUID) time.Time {
	if s.cfg.Clock != nil {
		return s.cfg.Clock.Now(orgID, userID)
	}
	return time.Now().UTC()
}

// turnLimits resolves the copilot's tool result cap and history budget into
// Turn values (0 = orchestrator policy / no budget).
func (s *CopilotService) turnLimits() (maxResultBytes, historyBudget int) {
	switch {
	case s.cfg.MaxToolResultBytes == 0:
		maxResultBytes = DefaultCopilotMaxToolResultBytes
	case s.cfg.MaxToolResultBytes > 0:
		maxResultBytes = s.cfg.MaxToolResultBytes
	}
	switch {
	case s.cfg.HistoryTokenBudget == 0:
		historyBudget = DefaultCopilotHistoryTokenBudget
	case s.cfg.HistoryTokenBudget > 0:
		historyBudget = s.cfg.HistoryTokenBudget
	}
	return maxResultBytes, historyBudget
}

// ProcessCopilotMessage runs one copilot turn.
func (s *CopilotService) ProcessCopilotMessage(ctx context.Context, orgID, userID, sessionID uuid.UUID, userMessage string, attachments []ChatAttachment, live bool) (*ProcessResult, error) {
	if s.cfg.PrepareContext != nil {
		ctx = s.cfg.PrepareContext(ctx, orgID, userID, live)
	}
	mgr := s.cfg.Orchestrator.contextMgr
	// Seed the shared cache BEFORE the orchestrator asks for it, so its
	// GetContext(sessionID) finds a context whose HistorySource keeps reading
	// the copilot thread instead of the business messages table.
	convCtx := mgr.GetCopilotContext(sessionID, orgID, userID)
	if convCtx.Metadata == nil {
		convCtx.Metadata = map[string]interface{}{}
	} else {
		delete(convCtx.Metadata, MetaImagePaths)
		delete(convCtx.Metadata, MetaImageURLs)
	}

	if len(attachments) == 0 {
		if prev := LastUserAttachments(s.cfg.Conversations, sessionID); len(prev) > 0 {
			attachments = prev
			log.Printf("📎 copilot: reusing %d attachment(s) from earlier turn", len(prev))
		}
	}
	enriched, imagePaths, imageURLs := EnrichMessageWithAttachmentsOpts(userMessage, attachments, s.cfg.Attachments)
	if len(imagePaths) > 0 {
		convCtx.Metadata[MetaImagePaths] = imagePaths
	}
	if len(imageURLs) > 0 {
		convCtx.Metadata[MetaImageURLs] = imageURLs
	}
	mgr.SaveContext(convCtx)

	prompt, _ := s.SystemPrompt(orgID, userID, live)
	maxResult, historyBudget := s.turnLimits()
	turn := Turn{
		ConversationID:     sessionID,
		AgentID:            uuid.Nil, // transient agent: no tools row to look up
		UserMessage:        enriched,
		SystemPrompt:       prompt,
		TurnContext:        s.turnContextFor(ctx, orgID, userID, userMessage),
		ExtraTools:         s.turnTools(ctx, orgID, userID),
		MaxToolResultBytes: maxResult,
		HistoryTokenBudget: historyBudget,
	}
	var mem *memoryTurn
	if s.memory != nil {
		mem = s.memory.beginTurn(ctx, sessionID, orgID, userID, userMessage)
		turn.TurnContext = joinTurnContext(turn.TurnContext, mem.contextBlock)
		turn.ExtraTools = append(turn.ExtraTools, s.memory.tools()...)
		turn.HistoryExchanges = mem.window
		turn.HistoryPreamble = mem.preamble
		turn.OnToolResult = mem.observe
	}
	response, err := s.cfg.Orchestrator.ProcessMessage(ctx, turn)
	if err != nil {
		return nil, fmt.Errorf("copilot processing failed: %w", err)
	}
	if mem != nil {
		mem.finish(ctx, response.Message)
	}
	result := ToProcessResult(response)
	result.Proposals = s.cfg.Proposals.ListPending(sessionID, userID)
	return result, nil
}

// turnTools are the kernel catalog tools (SemanticCatalog) followed by the
// host's per-turn tools, which win on a name clash.
func (s *CopilotService) turnTools(ctx context.Context, orgID, userID uuid.UUID) []tools.BaseTool {
	out := s.catalogTools()
	if s.cfg.TurnTools != nil {
		out = append(out, s.cfg.TurnTools(ctx, orgID, userID)...)
	}
	return out
}

// ApplyProposal executes or rejects a pending mutation after the user decides
// in chat. A proposal can only be decided by its own org and user.
func (s *CopilotService) ApplyProposal(orgID, userID, proposalID uuid.UUID, approve bool) (*MutationProposal, map[string]any, error) {
	return s.ApplyProposalContext(context.Background(), orgID, userID, proposalID, approve)
}

// ApplyProposalContext is ApplyProposal bound to ctx. The write is stamped as
// "person with agent": provenance assisted, actor the agent, authorized_by and
// on_behalf_of the person who approved, ref the proposal and the session.
func (s *CopilotService) ApplyProposalContext(ctx context.Context, orgID, userID, proposalID uuid.UUID, approve bool) (*MutationProposal, map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	store := s.cfg.Proposals
	p, ok := store.Get(proposalID)
	if !ok || p.OrganizationID != orgID || p.UserID != userID {
		return nil, nil, fmt.Errorf("proposal not found")
	}
	if p.Status != "pending" {
		// Double-click / voice+click: the same decision is success so the UI can sync.
		if approve && p.Status == "approved" {
			return p, map[string]any{"status": "already_applied", "model": p.Model, "record_id": p.RecordID}, nil
		}
		if !approve && p.Status == "rejected" {
			return p, nil, nil
		}
		return nil, nil, fmt.Errorf("proposal already %s", p.Status)
	}
	if !approve {
		store.SetStatus(proposalID, "rejected")
		p.Status = "rejected"
		return p, nil, nil
	}
	if s.cfg.CRUD == nil {
		return nil, nil, fmt.Errorf("agent: CopilotConfig.CRUD is required to apply proposals")
	}

	identity := &AgentIdentity{OrganizationID: orgID, UserID: userID}
	ctx = provenance.WithContext(ctx, s.ProposalProvenance(p, userID))
	var result map[string]any
	var err error
	switch p.Action {
	case "create":
		created, cerr := s.cfg.CRUD.CreateContext(ctx, identity, MutateRequest{Model: p.Model, Data: cloneMap(p.Data)})
		err, result = cerr, created
		if created != nil {
			if id, ok := created["id"]; ok {
				p.RecordID = fmt.Sprint(id)
				if p.Embed != nil {
					p.Embed["id"] = p.RecordID
					p.Embed["title"] = s.recordTitle(created, p.ModelTitle)
				}
			}
		}
	case "update":
		err = s.cfg.CRUD.UpdateContext(ctx, identity, MutateRequest{Model: p.Model, RecordID: p.RecordID, Data: cloneMap(p.Data)})
		result = map[string]any{"status": "updated", "model": p.Model, "record_id": p.RecordID}
	case "delete":
		err = s.cfg.CRUD.DeleteContext(ctx, identity, MutateRequest{Model: p.Model, RecordID: p.RecordID})
		result = map[string]any{"status": "deleted", "model": p.Model, "record_id": p.RecordID}
	default:
		return nil, nil, fmt.Errorf("unknown action %s", p.Action)
	}
	if err != nil {
		return nil, nil, err
	}
	store.SetStatus(proposalID, "approved")
	p.Status = "approved"
	return p, result, nil
}

// ProposalProvenance is the provenance of applying p once userID approved it.
func (s *CopilotService) ProposalProvenance(p *MutationProposal, userID uuid.UUID) provenance.Provenance {
	agentName := ""
	if s.cfg.Brand != nil {
		agentName = s.cfg.Brand.PersonalityFor(p.OrganizationID).Name
	}
	summary := ""
	if s.cfg.ProposalSummary != nil {
		summary = s.cfg.ProposalSummary(p)
	} else {
		title := p.ModelTitle
		if title == "" {
			title = p.Model
		}
		summary = strings.TrimSpace(p.Action + " " + title)
	}
	return provenance.Assisted(agentName, userID, provenance.ViaChat).
		WithRef(provenance.RefProposal, p.ID.String()).
		WithSession(p.SessionID).
		WithSummary(summary).
		WithOrigin(s.cfg.ProposalOrigin)
}

// ApplyProposalsBatch applies or rejects many pending proposals (same
// org/user). It stops on the first error and returns those done before it.
func (s *CopilotService) ApplyProposalsBatch(orgID, userID uuid.UUID, ids []uuid.UUID, approve bool) (applied []*MutationProposal, err error) {
	return s.ApplyProposalsBatchContext(context.Background(), orgID, userID, ids, approve)
}

// ApplyProposalsBatchContext is ApplyProposalsBatch bound to ctx.
func (s *CopilotService) ApplyProposalsBatchContext(ctx context.Context, orgID, userID uuid.UUID, ids []uuid.UUID, approve bool) (applied []*MutationProposal, err error) {
	applied = make([]*MutationProposal, 0, len(ids))
	for _, id := range ids {
		p, _, aerr := s.ApplyProposalContext(ctx, orgID, userID, id, approve)
		if aerr != nil {
			return applied, aerr
		}
		if p != nil {
			applied = append(applied, p)
		}
	}
	return applied, nil
}

func (s *CopilotService) recordTitle(created map[string]any, modelTitle string) string {
	if s.cfg.RecordTitle != nil {
		return s.cfg.RecordTitle(created, modelTitle)
	}
	for _, k := range []string{"name", "title", "label", "code", "folio"} {
		if v, ok := created[k].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return modelTitle
}

func cloneMap(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

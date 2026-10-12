package agent

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
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
	// Capabilities renders the authoritative capability clause. Optional.
	Capabilities *CapabilityRegistry
	// Brand supplies the persona (base prompt, mascot name, platform name,
	// tone addendum) per org. Required.
	Brand BrandProvider
	// Clock gives the org-local "now". Optional; UTC when nil.
	Clock ClockZone
	// Conversations reads earlier turns' attachments. Optional.
	Conversations ConversationStore
	// Proposals is the pending-mutation store (default GlobalProposalStore).
	Proposals *ProposalStore
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
}

// CopilotService runs turns of the embedded copilot and applies the changes
// the user approves in chat. Storage is separate from any business inbox:
// history comes from the copilot thread and the agent is transient.
type CopilotService struct {
	cfg CopilotConfig
}

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
	return &CopilotService{cfg: cfg}, nil
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
// matches the business's local day.
func NowClause(now time.Time) string {
	return "\nFecha y hora actual (" + now.Location().String() + "): " + now.Format("Monday 2006-01-02 15:04") +
		". Usa esta fecha/hora como \"hoy\" y \"ahora\" — ya está en la zona horaria del negocio; NUNCA inventes la fecha ni la conviertas a otra zona."
}

// SystemPrompt assembles the copilot prompt for a user: the host's base prompt
// followed by the brand, tone, clock and capability clauses.
func (s *CopilotService) SystemPrompt(orgID, userID uuid.UUID, live bool) (prompt string, p Personality) {
	p = s.cfg.Brand.PersonalityFor(orgID)
	now := time.Now().UTC()
	if s.cfg.Clock != nil {
		now = s.cfg.Clock.Now(orgID, userID)
	}
	prompt = p.SystemPrompt + BrandClause(p) + PersonalityClause(p) + NowClause(now)
	if s.cfg.Capabilities != nil {
		prompt += s.cfg.Capabilities.PromptClause(orgID, userID)
	}
	if live {
		prompt += s.cfg.LiveSuffix
	}
	return prompt, p
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
	response, err := s.cfg.Orchestrator.ProcessMessage(ctx, Turn{
		ConversationID: sessionID,
		AgentID:        uuid.Nil, // transient agent: no tools row to look up
		UserMessage:    enriched,
		SystemPrompt:   prompt,
	})
	if err != nil {
		return nil, fmt.Errorf("copilot processing failed: %w", err)
	}
	result := ToProcessResult(response)
	result.Proposals = s.cfg.Proposals.ListPending(sessionID, userID)
	return result, nil
}

// ApplyProposal executes or rejects a pending mutation after the user decides
// in chat. A proposal can only be decided by its own org and user.
func (s *CopilotService) ApplyProposal(orgID, userID, proposalID uuid.UUID, approve bool) (*MutationProposal, map[string]any, error) {
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
	var result map[string]any
	var err error
	switch p.Action {
	case "create":
		created, cerr := s.cfg.CRUD.Create(identity, MutateRequest{Model: p.Model, Data: cloneMap(p.Data)})
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
		err = s.cfg.CRUD.Update(identity, MutateRequest{Model: p.Model, RecordID: p.RecordID, Data: cloneMap(p.Data)})
		result = map[string]any{"status": "updated", "model": p.Model, "record_id": p.RecordID}
	case "delete":
		err = s.cfg.CRUD.Delete(identity, MutateRequest{Model: p.Model, RecordID: p.RecordID})
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

// ApplyProposalsBatch applies or rejects many pending proposals (same
// org/user). It stops on the first error and returns those done before it.
func (s *CopilotService) ApplyProposalsBatch(orgID, userID uuid.UUID, ids []uuid.UUID, approve bool) (applied []*MutationProposal, err error) {
	applied = make([]*MutationProposal, 0, len(ids))
	for _, id := range ids {
		p, _, aerr := s.ApplyProposal(orgID, userID, id, approve)
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

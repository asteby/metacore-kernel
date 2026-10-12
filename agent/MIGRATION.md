# Adopting the agent runtime (stage 1 + stage 2)

Everything here lives in `github.com/asteby/metacore-kernel/agent/...` and
`.../vector`. The kernel never imports an app's models or services: each
app implements the small interfaces below and passes them in.

Stage 1 (v0.199.0) shipped: `agent` contracts, `agent/contextmgr`,
`agent/tools`, chat embeds, charts, guides, proposals, provider-error
classification, message queue, egress guard.
Stage 2 (this change) adds the runtime that glues them together.

## 1. Interfaces an app implements

Signatures are exact. "Required" means the constructor fails or the feature
is dead without it.

### Package `agent` (contracts.go and neighbours)

| Interface | Methods | Required by | Notes |
|---|---|---|---|
| `PermissionChecker` | `Can(orgID, userID uuid.UUID, model, action string) bool` | `CRUDService` (required unless `DevAllowAll`) | Wrap your `PermissionService` + `ModelActionPermissionNames` candidates. `action` is `index`, `create`, `update`, `delete`. Return false on lookup errors (fail closed). |
| `ModuleGate` | `IsEnabled(orgID uuid.UUID, moduleKey string) bool`<br>`OwnerOf(model string) string`<br>`Enabled(orgID uuid.UUID) ([]ModuleInfo, error)` | `CRUDService.Gate`, `CapabilityRegistry.Gate`, `InstalledModulesContext` (all optional) | `OwnerOf` returns `""` for core models. Failure policy of `IsEnabled` is yours (ops fails open on a DB error, as before). |
| `ModelCatalog` | `Lookup(key string) (ModelMeta, bool)`<br>`All() []ModelMeta`<br>`AliasSource(key string) (string, bool)`<br>`Hints(lower string) []string`<br>`Translate(raw string) string`<br>`NavIcon(modelKey string) string` | `CRUDService` (required), `AgentOrchestrator.Catalog` (optional) | Fill `ModelMeta` from `models.GetModelInstance` / `GetDynamicMetadata`. `Hints` is where the ERP LLM aliases live (`"cliente"` -> `["customers","Customer"]`); return nil if none. `Translate` returns `raw` when there is no entry. `NavIcon` reads addon navigation. |
| `ModelRegistry` (stage 1) | `ResolveModelKey(name string) string`<br>`ModelLabel(modelKey string) string`<br>`FieldLabel(model, key, raw string) string`<br>`EnumValueLabel(model, field, value string) string` | `EmbedBuilder`, `AgentOrchestrator.Models` | Use `agent.CatalogRegistry{Catalog: cat, Labels: yours}` to get the first two for free from `ModelCatalog`; you only write `FieldLabel`/`EnumValueLabel`. |
| `SemanticIndex` | `VectorSearch(modelKey, query string, orgID uuid.UUID, limit int) ([]vector.SearchResult, error)`<br>`EnqueueRecord(modelKey string, recordID uuid.UUID)` | `CRUDService.Index` (optional) | `*vector.Pipeline` implements it. |
| `NamedPermissionChecker` | `HasPermission(orgID, userID uuid.UUID, permission string) (bool, error)` | `CapabilityRegistry.Named` (optional) | Checks a literal name (`"sales.create"`) declared by a manifest. Without it, declared capabilities that name a permission are dropped (fail closed). |
| `DeclaredCapabilityProvider` | `DeclaredCapabilities(orgID uuid.UUID) ([]CapabilityDescriptor, error)` | `CapabilityRegistry.Declared` (optional) | Read `AddonManifestRecord.AgentCapabilities` and map to descriptors (`Owner` = addon key). The kernel validates kind/risk/confirmation and filters by enabled module + permission. |
| `ChatModel` | `ChatWithFunctions(ctx, messages []map[string]interface{}, systemPrompt string, tools []map[string]interface{}) (map[string]interface{}, error)` | `NewAIServiceAdapter` | Your `AIService.GenerateChatWithFunctionCalling`. The returned map is OpenAI-shaped (`choices[0].message.{content,tool_calls}`). Alternatively implement `agent.AIService` directly (see below). |
| `AIService` | `GenerateChatWithTools(ctx, messages []map[string]interface{}, systemPrompt string, tools []map[string]interface{}) (*AIResponse, error)` | `AgentOrchestrator`, `SmartOrchestrator` | `NewAIServiceAdapter(chatModel)` already is one. |
| `BrandProvider` | `PersonalityFor(orgID uuid.UUID) Personality` | `CopilotService` (required) | `Personality{Name, Description, SystemPrompt, PlatformName, InternalNames, Addendum}`. `SystemPrompt` is the **full base prompt, ERP rules included** (this is where `CopilotSystemPrompt(mascot)` goes); `PlatformName`, `Addendum` come from OrgBranding then PlatformConfig; `InternalNames` = `["ops","0ps","asteby","erp ops","erp 0ps"]`. |
| `ClockZone` | `Now(orgID, userID uuid.UUID) time.Time` | `CopilotService.Clock` (optional, UTC when nil) | Return `time.Now().In(orgLocation)`; the zone name is printed to the model. |
| `ConversationStore` | `LastUserMessageMetadata(conversationID uuid.UUID, limit int) ([]json.RawMessage, error)` | `LastUserAttachments`, `CopilotService.Conversations` (optional) | Newest-first `copilot_messages.metadata` of role `user`. |
| `PromptSection` / `PromptSectionFunc` | `Section(orgID uuid.UUID) string` | `CatalogContext.Sections` | One per host block: currency (`CurrencyClause`), product-catalog counts, contact data capture, commerce guidance. |
| `SpeechSynthesizer` | `TextToSpeech(ctx, text string) (audioURL string, err error)` | `NewSendAudioTool` | Your `DeepgramService`. |
| `ImageGenerator`, `VectorStore`, `EmbeddingService`, `KnowledgeService` | unchanged from stage 1 | | |
| `AgentDirectory` | `AgentsFor(orchestratorID uuid.UUID) ([]AgentProfile, error)` | `SmartOrchestrator` | Load `Orchestrator.Agents` with tools; `AgentProfile.Tools` are tool names. |
| `AgentRunner` | `RunAgent(ctx, userMessage string, conversationID uuid.UUID, agent AgentProfile) (*AgentResponse, error)` | `SmartOrchestrator` | Build the specialist's prompt + `ExtraTools`, then call `AgentOrchestrator.ProcessMessage`. |

Plain data the host builds: `ModelMeta`, `ToolDefinition` (+ `ToolInputParam`)
from `models.AgentTool`, `AgentProfile`, `OrchestratorRef`, `Personality`.

### Package `agent/airuntime`

| Interface | Methods | Notes |
|---|---|---|
| `ConfigStore` | `OrgConfig(orgID uuid.UUID) (Config, error)`<br>`UserAISource(userID uuid.UUID) string` | Map `models.OrgAIConfig` <-> `airuntime.Config` (a missing row returns the zero `Config` and nil). `UserAISource` returns `"local"` or `"inherit"`. |
| `SeatDetector` (optional) | `ReadySeat() string` | `claude.LocalSeats{}` implements it (first logged-in CLI wins). |
| `ProviderStore` | `ActiveWithKeys() ([]Provider, error)`<br>`ResolveCreds(name string) (Provider, string, error)` | Over `ai_providers`. Decrypt keys here; return `airuntime.ErrProviderNotConfigured` when missing. Feed `Provider` rows to `PlanTools` / `PlanSimple`. |

### Package `agent/claude`

| Interface | Methods | Notes |
|---|---|---|
| `CredentialStore` | `CurrentEndpoint(orgID uuid.UUID) (baseURL, model string, err error)`<br>`SaveClaudeToken(orgID, userID uuid.UUID, baseURL, model, token string) error` | `CurrentEndpoint` returns the stored values only when the org is already in `claude_code` mode, else empty strings. `SaveClaudeToken` = `UpsertOrgAIConfig(... ModeClaudeCode ...)`; run `airuntime.Normalize` first. |

### Package `agent/convruntime`

| Interface | Methods | Notes |
|---|---|---|
| `Subscriber` | `SubscribeAll(event, subscriberKey string, handler Handler)`<br>`Unsubscribe(subscriberKey string)` | Adapt `services.EventBus` (`Handler` is `func(ctx, orgID uuid.UUID, payload any) error`; your adapter drops the `*gorm.DB` argument). |
| `Conversations` | `Load(ctx, id uuid.UUID) (Conversation, error)` | Return `convruntime.ErrConversationNotFound` for a missing row. `Conversation{ID, OrganizationID, ProcessType, ProcessActive}`. |
| `Dispatcher` | `Dispatch(ctx, conv Conversation, content, messageID string)` | Re-load whatever your engine needs (device, contact) and call `agent.Service.EnqueueMessage`. |

### Package `vector`

`NewPipeline(db, embedder, source, opts)` needs `vector.Embedder`
(`GenerateEmbedding(text) ([]float32, error)` and `GenerateBatchEmbeddings`)
and a `vector.ModelSource`:

```go
VectorConfig(modelKey string) (ModelConfig, bool)
AllVectorConfigs() map[string]ModelConfig
TableName(modelKey string) string
```

Your `RemoteEmbeddingService.GenerateEmbeddings` becomes `GenerateEmbedding`
(or use `vector.NewEnvRemoteEmbedder()`).

## 2. Constructors (replace the app's wiring)

```go
reg := tools.NewToolRegistry()               // agent/tools
reg.Register(agent.NewFinalAnswerTool())     // + GetConversationContext, SendImage,
                                             //   HandoffToHuman, SendAudio(speech),
                                             //   SearchKnowledge(ks), ShowChart, ...
// host tools stay yours: query_erp / product_search / contact tools.

crud, err := agent.NewCRUDService(agent.CRUDConfig{
    DB: db, Catalog: catalog, Permissions: perms, Gate: gate, Index: pipeline,
    NormalizeValue: stockLineNormalizer, // optional host hook
})

orch := agent.NewAgentOrchestrator(agent.OrchestratorConfig{
    Tools: reg, Context: contextmgr.NewManager(db),
    AI: agent.NewAIServiceAdapter(chatModel),
    Models: agent.CatalogRegistry{Catalog: catalog, Labels: labels},
    Catalog: catalog,
})

copilot, err := agent.NewCopilotService(agent.CopilotConfig{
    Orchestrator: orch, CRUD: crud, Brand: brand, Clock: clock,
    Conversations: convStore,
    Capabilities: &agent.CapabilityRegistry{CRUD: crud, Gate: gate, Declared: manifests, Named: perms2},
    PrepareContext: func(ctx context.Context, org, user uuid.UUID, live bool) context.Context {
        // services.WithAbyOrgInfo, WithOrgAIRuntime (airuntime.With), WithAbyLive...
        return ctx
    },
    Attachments: agent.AttachmentOptions{RecordHint: stockHint},
})
```

## 3. Steps

1. `go get github.com/asteby/metacore-kernel@<this release>`.
2. **contextmgr**: replace `meta-core/services/contextmgr` with
   `metacore-kernel/agent/contextmgr` (stage 1). Tools take
   `*contextmgr.ConversationContext` from the kernel.
3. **Tools**: your tools already satisfy `tools.BaseTool` (same five methods).
   Replace `agent.Tool`, `agent.ToolRegistry`, `agent.ToolCall`,
   `agent.ToolResult`, `ToolWithMetadata` with the `agent/tools` types. The
   kernel registry gained `Clone()`, `ToOpenAIFormat()` and now treats
   `action` / `creative` categories as action tools (so `generate_image`
   base64 never reaches chat text). Delete the app's `tools.go` registry and
   the generic tools (`final_answer`, `send_image`, `handoff_to_human`,
   `get_conversation_context`, `send_audio`, `search_knowledge`); keep
   `UpdateContactInfoTool`.
4. **Catalog**: write the `ModelCatalog` adapter. Move
   `resolveModelKey/Candidates/Label/Icon` and the alias map out of the app:
   the algorithm is `agent.ResolveModelKey` / `ModelLabel` / `ModelIcon`; the
   aliases go in `Hints`; `heuristicModelIcon` is gone (default `"database"`),
   so return the icon you want from `NavIcon` (or keep the heuristic there).
5. **CRUD**: delete `crud_service.go`; build `agent.NewCRUDService`. Move
   `normalizeAdjustmentLines` into the `NormalizeValue` hook. Tool wrappers
   that called `crud.Query/Create/...` keep working (same method names; the
   constructor now returns an error, `VectorSearch` returns `any`).
6. **Orchestrator**: delete `orchestrator.go` and `smart_orchestrator.go`.
   `ProcessMessage(ctx, userMessage, conv, agent)` becomes
   `ProcessMessage(ctx, agent.Turn{ConversationID, AgentID, UserMessage, SystemPrompt, ExtraTools})`.
   What the old method did before the loop is now yours, done once per turn:
   stamp `ctx` (org info, `airuntime.With(ctx, rt)` from
   `Resolver.Load(orgID, userID)`), load `AgentTool` rows and turn each into
   `agent.NewDynamicTool(&agent.ToolDefinition{...})` for `ExtraTools`, and
   build the system prompt with `CatalogContext.Build(orgID)` +
   contact context + the commerce guidance (keep these ERP strings in ops).
   The tools list and closing rules are appended by the kernel
   (`AppendToolsSection`).
7. **Copilot**: replace `Service.ProcessCopilotMessage`, `ApplyProposal(s)`,
   `copilotBrandClause/PersonalityClause/NowClause/CapabilityClause` and
   `capability.go` with `CopilotService` + `CapabilityRegistry`. Image
   metadata keys are `agent.MetaImagePaths` / `agent.MetaImageURLs`
   (`"aby_image_paths"` / `"aby_image_urls"`, unchanged on the wire).
   `GlobalProposalStore` is still the default store.
8. **Model layer**: keep `services/ai_service.go` (langchaingo and hub stay in
   the app: the kernel has no LLM-library dependency). Replace its strategy
   building by `airuntime.PlanTools` / `PlanSimple` and a switch over
   `Step.Backend` (`org-endpoint`, `hub` + `Tier`, `claude-code`, `provider`);
   use `airuntime.ExecuteWithFallback`, `RetryOnceOnRateLimit`,
   `StripThinkTags`, `TierForModel`. Replace `OrgAIRuntime`,
   `WithOrgAIRuntime`, `OrgAIRuntimeFrom` and the mode constants with
   `airuntime.*` (`models.OrgAIModeX` -> `airuntime.ModeX`; keep
   `models.OrgAIConfig` as the GORM row and map it to `airuntime.Config`;
   validate submissions with `airuntime.Normalize`).
9. **Claude**: delete `claude_code_*.go`, `claude_direct_*.go`,
   `claude_connect*.go`, `local_agent_seats.go`. Use
   `claude.NewClaudeConnectManager(credStore)`, `claude.GetClaudeBinaryStatus`,
   `claude.InstallClaudeBinary`, `claude.ResolveClaudeRunner(rt)`,
   `claude.DefaultClaudeDirectRunner()` and the transports
   (see section 6 for the transport constructors). `claude.LocalSeats{}` is the
   `airuntime.SeatDetector`.
10. **Semantic search**: replace `SemanticPipeline` with
    `vector.NewPipeline(db, embedder, source, vector.PipelineOptions{})`; run
    `vector.MigrateSQL` (idempotent) or keep your AutoMigrate of the same
    table. `BackfillAll`, `BackfillModel`, `ReembedAll`,
    `VectorizableModels` keep their names.
11. **convruntime**: replace `services/convruntime` with
    `convruntime.Wire(convruntime.Config{Bus, DB, Conversations, Dispatcher})`.
    Keep the app's `flow` engine and `agent.Service` (WhatsApp device queue):
    they stay in the app and are called from your `Dispatcher`.
12. Run the app's tests; delete the ported tests that the kernel now owns
    (`aby_*_test.go`, `attachments_test.go`, `claude_*_test.go`,
    `convruntime/driver_test.go`).

## 4. Behaviour changes to know about

* **Permissions fail closed.** The old CRUD allowed everything when no
  `PermissionService` was wired. `NewCRUDService` now refuses to build without
  a `PermissionChecker`; `DevAllowAll: true` restores the old dev behaviour.
* **Delete** adds `deleted_at IS NULL` and writes a UTC timestamp parameter
  instead of `NOW()`; `Aggregate` returns the SQL error instead of swallowing
  it; text-search columns are validated as identifiers.
* **Vector pipeline**: enqueue never blocks (a full queue drops and logs);
  `VectorSearch` requires an org and re-filters the records by it; the
  record read uses `Take` (the old `First` on a map ordered by a missing
  primary key); `embeddings (entity_type, entity_id)` must be unique for the
  upsert (`MigrateSQL` creates the index).
* **Orchestrator** metadata `agent_id` is whatever `Turn.AgentID` says
  (copilot: `uuid.Nil`). `heuristicModelIcon` and the built-in alias map are
  gone (see step 4). `DynamicTool` defaults an empty method to POST.
* **Smart routing** returns an error instead of running a zero-value agent
  when no agent could be selected.
* **Adapter**: a malformed `choices[0]` no longer panics.
* **Claude**: hard-coded `/home/ubuntu/...` lookup paths (Link's fnm alias)
  were removed; use `CLAUDE_CODE_BIN` or PATH. `claude.BinaryPath` is a
  variable: assign it at start-up if your install layout differs.
  New direct dependencies: `github.com/creack/pty` (darwin) and
  `github.com/UserExistsError/conpty` (windows).

## 5. Not moved (stays in the app, by design)

* ERP prompt text and LLM aliases; `query_erp_tool`, `product_search`,
  `UpdateContactInfoTool`, commerce guidance in the prompt.
* `services/ai_service.go` (langchaingo clients) and `services/hubai`;
  `AIProvider` / `OrgAIConfig` GORM models, their admin UI metadata, key
  encryption (`utils.Decrypt`) and handlers.
* `agent.Service` (business WhatsApp path: devices, flow engine, debounce
  wiring, typing indicators) and `flow`.
* `ClaudeBinaryStatus` HTTP handlers and Spanish error mapping.

## 6. Exported names added for hosts

`claude.NewDirectHTTPClient(rt airuntime.Runtime) *http.Client`,
`claude.NewCodeHTTPClient(timeout time.Duration) *http.Client` and the
constants `claude.DirectBaseURL` / `claude.DirectToken` are the exported
spellings of what used to be package-private in `services`
(`createLLM` uses them for the `openai-proxy` provider).

// Package agent is the reusable runtime for conversational copilots built on
// the metacore kernel: tool registry, conversation context, chat embeds
// (record/count/chart/image cards), provider-error classification, outbound
// message queue, guided-tour suggestions and the egress guard for tool SQL.
//
// The package follows the kernel's dependency-inversion rule: it never imports
// a host's models or services. Everything host-specific is an interface in
// contracts.go (PermissionChecker, ModuleGate, ModelRegistry, ModelCatalog,
// BrandProvider, SemanticIndex, ChatModel, ConversationStore, ClockZone...)
// or a small injected port (ImageGenerator, SpeechSynthesizer, vector.Store,
// vector.Embedder).
//
// The runtime in this package:
//
//	AgentOrchestrator  the tool-using reasoning loop (ProcessMessage(Turn))
//	SmartOrchestrator  AI routing between specialist agents
//	CopilotService     the embedded copilot: prompt assembly, turn, proposals
//	CRUDService        permission- and module-gated, org-scoped data access
//	CapabilityRegistry what a user may do (CRUD + manifest-declared)
//	DynamicTool        tenant-configured webhook/API tools behind the egress guard
//	AIServiceAdapter   OpenAI-shaped model replies -> AIResponse
//	attachments        spreadsheet/image enrichment of the user message
//
// What deliberately stays in the host: the ERP system prompt and LLM aliases
// (ModelCatalog.Hints), tools that know the host's schema (query_erp,
// product_search, contact tools), the langchaingo/hub client behind ChatModel,
// the AI-provider and org-config tables (behind airuntime.ConfigStore and
// ProviderStore), and the business WhatsApp service. See MIGRATION.md for the
// exact list of interfaces each app implements.
//
// Sub-packages:
//
//	agent/contextmgr  per-conversation state (in-memory cache + gorm history)
//	agent/tools       ToolRegistry, BaseTool and the calculator tool
//	agent/airuntime   per-org AI mode, Runtime on the context, fallback planner
//	agent/claude      Claude Code CLI as a backend: direct runner, transports,
//	                  OAuth token, setup-token connect flow, local CLI seats
//	agent/convruntime bus-driven delivery of inbound messages to an engine
package agent

package flow

import (
	"time"

	"github.com/google/uuid"
)

// FlowStatus represents the lifecycle state of a flow.
type FlowStatus string

const (
	FlowStatusDraft    FlowStatus = "draft"
	FlowStatusActive   FlowStatus = "active"
	FlowStatusPaused   FlowStatus = "paused"
	FlowStatusArchived FlowStatus = "archived"
)

// TriggerType identifies how a flow is entered. The kernel ships with a handful
// of generic triggers (manual, webhook, schedule, event). Apps may define
// additional trigger types (keyword, welcome, menu, fallback, etc.) and plug
// them into their own trigger service — the engine itself treats TriggerType
// as an opaque string.
type TriggerType string

const (
	TriggerManual   TriggerType = "manual"
	TriggerWebhook  TriggerType = "webhook"
	TriggerSchedule TriggerType = "schedule"
	TriggerEventType TriggerType = "event"
	TriggerAPI      TriggerType = "api"
)

// NodeType identifies a NodeExecutor in the engine's Registry. Kernel built-ins
// (see registry.go) use the values below. Apps may register arbitrary
// additional types (e.g. "message", "ai_chat", "create_ticket") by calling
// Engine.RegisterNode.
type NodeType string

const (
	NodeTypeTrigger       NodeType = "trigger"
	NodeTypeHTTPRequest   NodeType = "http_request"
	NodeTypeWebhook       NodeType = "webhook"
	NodeTypeCondition     NodeType = "condition"
	NodeTypeSwitch        NodeType = "switch"
	NodeTypeDelay         NodeType = "delay"
	NodeTypeWait          NodeType = "wait"
	NodeTypeLoop          NodeType = "loop"
	NodeTypeFilter        NodeType = "filter"
	NodeTypeSetVariable   NodeType = "set_variable"
	NodeTypeTransformData NodeType = "transform_data"
	NodeTypeSplit         NodeType = "split"
	NodeTypeMerge         NodeType = "merge"
	NodeTypeErrorHandler  NodeType = "error_handler"
	NodeTypeNote          NodeType = "note"
)

// ExecutionStatus reports lifecycle of a FlowExecution.
type ExecutionStatus string

const (
	ExecutionPending   ExecutionStatus = "pending"
	ExecutionRunning   ExecutionStatus = "running"
	ExecutionCompleted ExecutionStatus = "completed"
	ExecutionFailed    ExecutionStatus = "failed"
	ExecutionCancelled ExecutionStatus = "cancelled"
	ExecutionPaused    ExecutionStatus = "paused"
)

// Flow is the executable DAG passed to the engine. It is deliberately a plain
// Go struct — apps map their own persistence model (GORM record, YAML, etc.)
// into a *Flow before calling ExecuteFlow. The kernel never persists Flows on
// its own; that is the host's responsibility via the optional Store.
type Flow struct {
	ID             uuid.UUID
	OrganizationID uuid.UUID
	Name           string
	Status         FlowStatus
	TriggerType    TriggerType
	TriggerConfig  map[string]interface{}

	Nodes []FlowNode
	Edges []FlowEdge

	// Variables are flow-level defaults that seed ExecutionContext.
	Variables []FlowVariable
	Settings  map[string]interface{}
}

// FlowNode is a vertex in the DAG. Config lives under Data.Config, interpreted
// by the NodeExecutor registered for Type.
type FlowNode struct {
	ID   string
	Type NodeType
	Data FlowNodeData
}

// FlowNodeData is the configuration bundle for a single node. Both keys are
// kept (Config for regular nodes, TriggerConfig for trigger nodes) to match
// the existing editor contract used by the ops/link frontends.
type FlowNodeData struct {
	Label           string
	Description     string
	TriggerType     string
	TriggerConfig   map[string]interface{}
	Config          map[string]interface{}
	InputVariables  []string
	OutputVariables []FlowVariable
	Timeout         int
	RetryCount      int
	RetryDelay      int
	ContinueOnError bool
	Notes           string
}

// FlowEdge is a directed connection between nodes. SourceHandle is the output
// handle on the source node (used by branching nodes such as Condition or
// Switch); Condition evaluates the edge at runtime against the ExecutionContext.
type FlowEdge struct {
	ID           string
	Source       string
	Target       string
	SourceHandle string
	TargetHandle string
	Condition    *EdgeCondition
}

// EdgeCondition is evaluated by the engine against context variables when the
// edge is traversed. Supported operators are listed in context.go.
type EdgeCondition struct {
	Field    string
	Operator string
	Value    interface{}
}

// FlowVariable declares a named variable on the flow (global default) or as a
// node's declared output. Value may be any JSON-compatible value.
type FlowVariable struct {
	Name        string
	Type        string
	Value       interface{}
	Description string
	Source      string
	IsSecret    bool
}

// FlowExecution is the runtime record of a single flow run. Apps that need to
// persist executions map this struct to their own model inside a Store
// implementation.
type FlowExecution struct {
	ID             uuid.UUID
	FlowID         uuid.UUID
	OrganizationID uuid.UUID
	Status         ExecutionStatus
	StartedAt      *time.Time
	CompletedAt    *time.Time
	Duration       int64
	TriggerType    TriggerType
	TriggerData    map[string]interface{}

	// Opaque app context carried through the execution (e.g. contact_id,
	// conversation_id for link). The engine never inspects this — node
	// executors do.
	AppContext map[string]interface{}

	Variables      map[string]interface{}
	NodeExecutions []NodeExecution

	ErrorMessage string
	ErrorNode    string
}

// NodeExecution is the per-node trace recorded into the FlowExecution log.
type NodeExecution struct {
	NodeID      string
	NodeType    NodeType
	Status      ExecutionStatus
	StartedAt   time.Time
	CompletedAt *time.Time
	Duration    int64
	Input       map[string]interface{}
	Output      map[string]interface{}
	Error       string
}

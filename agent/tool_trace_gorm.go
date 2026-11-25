package agent

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/asteby/metacore-kernel/agent/contextmgr"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ToolTracesTable is the table of GormToolTraceStore.
const ToolTracesTable = "agent_tool_traces"

// ToolTracesDDLPostgres is the Postgres schema of GormToolTraceStore.
const ToolTracesDDLPostgres = `
CREATE TABLE IF NOT EXISTS agent_tool_traces (
    id              uuid        PRIMARY KEY,
    organization_id uuid        NOT NULL,
    user_id         uuid        NOT NULL,
    conversation_id uuid        NOT NULL,
    call_id         text        NOT NULL DEFAULT '',
    tool            text        NOT NULL,
    iteration       integer     NOT NULL DEFAULT 0,
    started_at      timestamptz NOT NULL,
    duration_ms     bigint      NOT NULL DEFAULT 0,
    attempts        integer     NOT NULL DEFAULT 0,
    parallel        boolean     NOT NULL DEFAULT false,
    success         boolean     NOT NULL DEFAULT false,
    error           text        NOT NULL DEFAULT '',
    timed_out       boolean     NOT NULL DEFAULT false,
    result_bytes    integer     NOT NULL DEFAULT 0,
    truncated       boolean     NOT NULL DEFAULT false
);
CREATE INDEX IF NOT EXISTS idx_agent_tool_traces_org_started ON agent_tool_traces (organization_id, started_at);
`

type toolTraceRow struct {
	ID             uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	OrganizationID uuid.UUID `gorm:"column:organization_id;type:uuid;not null;index:idx_agent_tool_traces_org_started"`
	UserID         uuid.UUID `gorm:"column:user_id;type:uuid;not null"`
	ConversationID uuid.UUID `gorm:"column:conversation_id;type:uuid;not null"`
	CallID         string    `gorm:"column:call_id;type:text;not null"`
	Tool           string    `gorm:"column:tool;type:text;not null"`
	Iteration      int       `gorm:"column:iteration;not null"`
	StartedAt      time.Time `gorm:"column:started_at;not null;index:idx_agent_tool_traces_org_started"`
	DurationMS     int64     `gorm:"column:duration_ms;not null"`
	Attempts       int       `gorm:"column:attempts;not null"`
	Parallel       bool      `gorm:"column:parallel;not null"`
	Success        bool      `gorm:"column:success;not null"`
	Error          string    `gorm:"column:error;type:text;not null"`
	TimedOut       bool      `gorm:"column:timed_out;not null"`
	ResultBytes    int       `gorm:"column:result_bytes;not null"`
	Truncated      bool      `gorm:"column:truncated;not null"`
}

func (toolTraceRow) TableName() string { return ToolTracesTable }

// MigrateToolTraces creates or updates the agent_tool_traces table.
func MigrateToolTraces(db *gorm.DB) error { return db.AutoMigrate(&toolTraceRow{}) }

// ToolTraceHook is the signature of OrchestratorConfig.OnToolTrace.
type ToolTraceHook = func(ctx context.Context, convCtx *contextmgr.ConversationContext, tr ToolTrace)

// GormToolTraceStore persists tool traces. A host wires it with
//
//	st := agent.NewGormToolTraceStore(db, 256)
//	cfg.OnToolTrace = st.Hook()
//	defer st.Close()
//
// The hook only enqueues (OnToolTrace runs on the loop goroutine); a worker
// writes the rows. When the queue is full the trace is dropped and logged,
// never blocking the agent loop.
type GormToolTraceStore struct {
	db   *gorm.DB
	ch   chan toolTraceRow
	done chan struct{}
	once sync.Once
}

// NewGormToolTraceStore starts the writer; queue is the buffer size (<=0 = 256).
func NewGormToolTraceStore(db *gorm.DB, queue int) *GormToolTraceStore {
	if queue <= 0 {
		queue = 256
	}
	s := &GormToolTraceStore{db: db, ch: make(chan toolTraceRow, queue), done: make(chan struct{})}
	go s.run()
	return s
}

func (s *GormToolTraceStore) run() {
	defer close(s.done)
	for row := range s.ch {
		if err := s.db.Create(&row).Error; err != nil {
			slog.Error("agent: tool trace write", "tool", row.Tool, "err", err)
		}
	}
}

// Hook returns the function to assign to OrchestratorConfig.OnToolTrace.
func (s *GormToolTraceStore) Hook() ToolTraceHook {
	return func(_ context.Context, cc *contextmgr.ConversationContext, tr ToolTrace) {
		row := toolTraceRow{
			ID: uuid.New(), CallID: tr.CallID, Tool: tr.Tool, Iteration: tr.Iteration, StartedAt: tr.StartedAt,
			DurationMS: tr.DurationMS, Attempts: tr.Attempts, Parallel: tr.Parallel, Success: tr.Success,
			Error: tr.Error, TimedOut: tr.TimedOut, ResultBytes: tr.ResultBytes, Truncated: tr.Truncated,
		}
		if row.StartedAt.IsZero() {
			row.StartedAt = time.Now().UTC()
		}
		if cc != nil {
			row.OrganizationID, row.UserID, row.ConversationID = cc.OrganizationID, cc.UserID, cc.ConversationID
		}
		select {
		case s.ch <- row:
		default:
			slog.Warn("agent: tool trace queue full, dropped", "tool", tr.Tool)
		}
	}
}

// Close flushes the queue and stops the writer. Hook must not be used after.
func (s *GormToolTraceStore) Close() {
	s.once.Do(func() { close(s.ch) })
	<-s.done
}

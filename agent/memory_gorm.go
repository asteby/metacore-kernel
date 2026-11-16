package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Tables of GormTurnStore. Hosts that own their migrations can create them
// with MemoryDDLPostgres instead of MigrateTurnMemory.
const (
	MemoryTurnsTable = "copilot_memory_turns"
	MemoryFactsTable = "copilot_memory_facts"
)

// MemoryDDLPostgres is the Postgres schema of GormTurnStore, for hosts that
// run their own migrations (goose/SQL). It matches what MigrateTurnMemory
// creates through AutoMigrate.
const MemoryDDLPostgres = `
CREATE TABLE IF NOT EXISTS copilot_memory_turns (
    session_id      uuid        NOT NULL,
    turn            integer     NOT NULL,
    organization_id uuid        NOT NULL,
    user_id         uuid        NOT NULL,
    title           text        NOT NULL DEFAULT '',
    summary         text        NOT NULL DEFAULT '',
    facts           text        NOT NULL DEFAULT '[]', -- JSON array of strings
    tool_digests    text        NOT NULL DEFAULT '[]', -- JSON array of strings
    user_message    text        NOT NULL DEFAULT '',
    answer          text        NOT NULL DEFAULT '',
    embedding       text        NOT NULL DEFAULT '',   -- JSON array of floats ('' without embedder)
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, turn)
);
CREATE INDEX IF NOT EXISTS idx_copilot_memory_turns_org_user ON copilot_memory_turns (organization_id, user_id);

CREATE TABLE IF NOT EXISTS copilot_memory_facts (
    id              uuid        PRIMARY KEY,
    organization_id uuid        NOT NULL,
    user_id         uuid        NOT NULL,
    text            text        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_copilot_memory_facts_org_user ON copilot_memory_facts (organization_id, user_id, created_at);
`

// memoryTurnRow is the row of MemoryTurnsTable.
type memoryTurnRow struct {
	SessionID      uuid.UUID `gorm:"column:session_id;type:uuid;primaryKey"`
	Turn           int       `gorm:"column:turn;primaryKey;autoIncrement:false"`
	OrganizationID uuid.UUID `gorm:"column:organization_id;type:uuid;not null;index:idx_copilot_memory_turns_org_user"`
	UserID         uuid.UUID `gorm:"column:user_id;type:uuid;not null;index:idx_copilot_memory_turns_org_user"`
	Title          string    `gorm:"column:title;type:text;not null;default:''"`
	Summary        string    `gorm:"column:summary;type:text;not null;default:''"`
	Facts          string    `gorm:"column:facts;type:text;not null;default:'[]'"`
	ToolDigests    string    `gorm:"column:tool_digests;type:text;not null;default:'[]'"`
	UserMessage    string    `gorm:"column:user_message;type:text;not null;default:''"`
	Answer         string    `gorm:"column:answer;type:text;not null;default:''"`
	Embedding      string    `gorm:"column:embedding;type:text;not null;default:''"`
	CreatedAt      time.Time `gorm:"column:created_at;not null"`
}

func (memoryTurnRow) TableName() string { return MemoryTurnsTable }

// memoryFactRow is the row of MemoryFactsTable.
type memoryFactRow struct {
	ID             uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	OrganizationID uuid.UUID `gorm:"column:organization_id;type:uuid;not null;index:idx_copilot_memory_facts_org_user"`
	UserID         uuid.UUID `gorm:"column:user_id;type:uuid;not null;index:idx_copilot_memory_facts_org_user"`
	Text           string    `gorm:"column:text;type:text;not null"`
	CreatedAt      time.Time `gorm:"column:created_at;not null;index:idx_copilot_memory_facts_org_user"`
}

func (memoryFactRow) TableName() string { return MemoryFactsTable }

// MigrateTurnMemory creates or updates the layered-memory tables. GORM
// AutoMigrate is additive (creates tables, adds columns/indexes, never
// drops), so it is safe at every boot.
func MigrateTurnMemory(db *gorm.DB) error {
	return db.AutoMigrate(&memoryTurnRow{}, &memoryFactRow{})
}

// GormTurnStore is the SQL TurnMemoryStore (Postgres in production; any
// GORM dialect works).
type GormTurnStore struct{ db *gorm.DB }

// NewGormTurnStore builds a store over db. Run MigrateTurnMemory (or
// MemoryDDLPostgres) first.
func NewGormTurnStore(db *gorm.DB) *GormTurnStore { return &GormTurnStore{db: db} }

func marshalStrings(v []string) string {
	if len(v) == 0 {
		return "[]"
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}

// SaveTurn upserts the turn (session_id, turn).
func (s *GormTurnStore) SaveTurn(ctx context.Context, rec TurnRecord) error {
	emb := ""
	if len(rec.Embedding) > 0 {
		raw, _ := json.Marshal(rec.Embedding)
		emb = string(raw)
	}
	row := memoryTurnRow{
		SessionID: rec.SessionID, Turn: rec.Turn, OrganizationID: rec.OrganizationID, UserID: rec.UserID,
		Title: rec.Title, Summary: rec.Summary, Facts: marshalStrings(rec.Facts), ToolDigests: marshalStrings(rec.ToolDigests),
		UserMessage: rec.UserMessage, Answer: rec.Answer, Embedding: emb, CreatedAt: rec.CreatedAt,
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now().UTC()
	}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error
}

// ListTurns returns the session's turns by number.
func (s *GormTurnStore) ListTurns(ctx context.Context, sessionID uuid.UUID) ([]TurnRecord, error) {
	var rows []memoryTurnRow
	if err := s.db.WithContext(ctx).Where("session_id = ?", sessionID).Order("turn ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]TurnRecord, len(rows))
	for i, r := range rows {
		rec := TurnRecord{
			SessionID: r.SessionID, OrganizationID: r.OrganizationID, UserID: r.UserID, Turn: r.Turn,
			Title: r.Title, Summary: r.Summary, UserMessage: r.UserMessage, Answer: r.Answer, CreatedAt: r.CreatedAt,
		}
		_ = json.Unmarshal([]byte(r.Facts), &rec.Facts)
		_ = json.Unmarshal([]byte(r.ToolDigests), &rec.ToolDigests)
		if r.Embedding != "" {
			_ = json.Unmarshal([]byte(r.Embedding), &rec.Embedding)
		}
		out[i] = rec
	}
	return out, nil
}

// ListFacts returns the user's facts in stable order.
func (s *GormTurnStore) ListFacts(ctx context.Context, orgID, userID uuid.UUID) ([]MemoryFact, error) {
	var rows []memoryFactRow
	if err := s.db.WithContext(ctx).
		Where("organization_id = ? AND user_id = ?", orgID, userID).
		Order("created_at ASC").Order("id ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]MemoryFact, len(rows))
	for i, r := range rows {
		out[i] = MemoryFact{ID: r.ID, OrganizationID: r.OrganizationID, UserID: r.UserID, Text: r.Text, CreatedAt: r.CreatedAt}
	}
	sortFacts(out)
	return out, nil
}

// SaveFact upserts the fact by ID.
func (s *GormTurnStore) SaveFact(ctx context.Context, f MemoryFact) error {
	if f.ID == uuid.Nil {
		f.ID = uuid.New()
	}
	if f.CreatedAt.IsZero() {
		f.CreatedAt = time.Now().UTC()
	}
	row := memoryFactRow{ID: f.ID, OrganizationID: f.OrganizationID, UserID: f.UserID, Text: f.Text, CreatedAt: f.CreatedAt}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error
}

// DeleteFact removes the fact if it belongs to org+user.
func (s *GormTurnStore) DeleteFact(ctx context.Context, orgID, userID, id uuid.UUID) error {
	return s.db.WithContext(ctx).
		Where("id = ? AND organization_id = ? AND user_id = ?", id, orgID, userID).
		Delete(&memoryFactRow{}).Error
}

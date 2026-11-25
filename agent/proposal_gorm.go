package agent

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ProposalsTable is the table of GormProposalStore.
const ProposalsTable = "agent_mutation_proposals"

// ProposalsDDLPostgres is the Postgres schema of GormProposalStore, for hosts
// that run their own migrations. It matches what MigrateProposals creates.
const ProposalsDDLPostgres = `
CREATE TABLE IF NOT EXISTS agent_mutation_proposals (
    id              uuid        PRIMARY KEY,
    organization_id uuid        NOT NULL,
    user_id         uuid        NOT NULL,
    session_id      uuid        NOT NULL,
    status          text        NOT NULL DEFAULT 'pending',
    payload         text        NOT NULL,  -- JSON of MutationProposal
    created_at      timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_agent_mutation_proposals_session ON agent_mutation_proposals (session_id, user_id, status);
`

// ProposalRepository is the pending-mutation store the copilot talks to. The
// in-memory ProposalStore and GormProposalStore both implement it.
type ProposalRepository interface {
	Put(p *MutationProposal)
	Get(id uuid.UUID) (*MutationProposal, bool)
	ListPending(sessionID, userID uuid.UUID) []*MutationProposal
	SetStatus(id uuid.UUID, status string) (*MutationProposal, bool)
}

var (
	_ ProposalRepository = (*ProposalStore)(nil)
	_ ProposalRepository = (*GormProposalStore)(nil)
)

type proposalRow struct {
	ID             uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	OrganizationID uuid.UUID `gorm:"column:organization_id;type:uuid;not null"`
	UserID         uuid.UUID `gorm:"column:user_id;type:uuid;not null;index:idx_agent_mutation_proposals_session"`
	SessionID      uuid.UUID `gorm:"column:session_id;type:uuid;not null;index:idx_agent_mutation_proposals_session"`
	Status         string    `gorm:"column:status;type:text;not null;default:pending;index:idx_agent_mutation_proposals_session"`
	Payload        string    `gorm:"column:payload;type:text;not null"`
	CreatedAt      time.Time `gorm:"column:created_at;not null"`
}

func (proposalRow) TableName() string { return ProposalsTable }

// MigrateProposals creates or updates the proposals table (additive, safe at
// every boot).
func MigrateProposals(db *gorm.DB) error { return db.AutoMigrate(&proposalRow{}) }

// GormProposalStore persists pending mutations so a human approval survives a
// restart. The ProposalRepository methods carry no error, so a failing write
// or read is logged (slog) and surfaces as "not found".
type GormProposalStore struct{ db *gorm.DB }

// NewGormProposalStore builds a store over db. Run MigrateProposals (or
// ProposalsDDLPostgres) first.
func NewGormProposalStore(db *gorm.DB) *GormProposalStore { return &GormProposalStore{db: db} }

func (s *GormProposalStore) Put(p *MutationProposal) {
	raw, err := json.Marshal(p)
	if err != nil {
		slog.Error("agent: proposal marshal", "id", p.ID, "err", err)
		return
	}
	created := p.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	status := p.Status
	if status == "" {
		status = "pending"
	}
	row := proposalRow{ID: p.ID, OrganizationID: p.OrganizationID, UserID: p.UserID, SessionID: p.SessionID,
		Status: status, Payload: string(raw), CreatedAt: created}
	if err := s.db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error; err != nil {
		slog.Error("agent: proposal put", "id", p.ID, "err", err)
	}
}

func (s *GormProposalStore) decode(r proposalRow) (*MutationProposal, bool) {
	var p MutationProposal
	if err := json.Unmarshal([]byte(r.Payload), &p); err != nil {
		slog.Error("agent: proposal decode", "id", r.ID, "err", err)
		return nil, false
	}
	p.Status = r.Status // the column is the source of truth for the status
	return &p, true
}

func (s *GormProposalStore) Get(id uuid.UUID) (*MutationProposal, bool) {
	var r proposalRow
	if err := s.db.Where("id = ?", id).Take(&r).Error; err != nil {
		if err != gorm.ErrRecordNotFound {
			slog.Error("agent: proposal get", "id", id, "err", err)
		}
		return nil, false
	}
	return s.decode(r)
}

func (s *GormProposalStore) ListPending(sessionID, userID uuid.UUID) []*MutationProposal {
	var rows []proposalRow
	if err := s.db.Where("session_id = ? AND user_id = ? AND status = ?", sessionID, userID, "pending").
		Order("created_at ASC").Find(&rows).Error; err != nil {
		slog.Error("agent: proposal list", "session", sessionID, "err", err)
		return []*MutationProposal{}
	}
	out := make([]*MutationProposal, 0, len(rows))
	for _, r := range rows {
		if p, ok := s.decode(r); ok {
			out = append(out, p)
		}
	}
	return out
}

func (s *GormProposalStore) SetStatus(id uuid.UUID, status string) (*MutationProposal, bool) {
	res := s.db.Model(&proposalRow{}).Where("id = ?", id).Update("status", status)
	if res.Error != nil {
		slog.Error("agent: proposal set status", "id", id, "err", res.Error)
		return nil, false
	}
	if res.RowsAffected == 0 {
		return nil, false
	}
	return s.Get(id)
}

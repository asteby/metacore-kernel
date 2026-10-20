package legacybridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Tablas del puente.
const (
	TableRefs         = "bridge_refs"
	TableCursors      = "bridge_cursors"
	TableOwnership    = "bridge_ownership"
	TableOwnershipLog = "bridge_ownership_log"
	TablePhases       = "bridge_phases"
	TablePending      = "bridge_pending"
	TableCommands     = "bridge_commands"
)

// Migrate crea las tablas bridge_* si no existen. Es idempotente; en Postgres
// corre bajo un candado consultivo.
func Migrate(ctx context.Context, db *gorm.DB) error {
	pg := db.Dialector.Name() == "postgres"
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if pg {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", advisoryKey("legacybridge.migrate")).Error; err != nil {
				return err
			}
		}
		for _, q := range Schema(db.Dialector.Name()) {
			if err := tx.Exec(q).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("legacybridge: migrate: %w", err)
	}
	return nil
}

// Schema devuelve el DDL idempotente para el dialecto ("postgres"; cualquier
// otro recibe el de SQLite). El JSON se guarda como texto: el puente no
// consulta dentro de él.
func Schema(dialect string) []string {
	ts := "DATETIME"
	if dialect == "postgres" {
		ts = "TIMESTAMPTZ"
	}
	return []string{
		`CREATE TABLE IF NOT EXISTS ` + TableRefs + ` (
			org_id         TEXT NOT NULL,
			system         TEXT NOT NULL,
			entity         TEXT NOT NULL,
			external_id    TEXT NOT NULL,
			local_id       TEXT NOT NULL,
			owner          TEXT NOT NULL DEFAULT 'legacy',
			source_version TEXT NOT NULL DEFAULT '',
			last_hash      TEXT NOT NULL DEFAULT '',
			created_in     TEXT NOT NULL DEFAULT 'legacy',
			state          TEXT NOT NULL DEFAULT 'synced',
			updated_at     ` + ts + ` NOT NULL,
			PRIMARY KEY (org_id, system, entity, external_id)
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_bridge_refs_local ON ` + TableRefs + ` (org_id, system, entity, local_id)`,
		`CREATE TABLE IF NOT EXISTS ` + TableCursors + ` (
			org_id     TEXT NOT NULL,
			system     TEXT NOT NULL,
			stream     TEXT NOT NULL,
			watermark  TEXT NOT NULL,
			updated_at ` + ts + ` NOT NULL,
			PRIMARY KEY (org_id, system, stream)
		)`,
		`CREATE TABLE IF NOT EXISTS ` + TableOwnership + ` (
			org_id     TEXT NOT NULL,
			system     TEXT NOT NULL,
			entity     TEXT NOT NULL,
			branch     TEXT NOT NULL,
			phase      TEXT NOT NULL,
			owner      TEXT NOT NULL,
			note       TEXT NOT NULL DEFAULT '',
			updated_by TEXT NOT NULL DEFAULT '',
			updated_at ` + ts + ` NOT NULL,
			PRIMARY KEY (org_id, system, entity, branch, phase),
			CONSTRAINT bridge_ownership_owner_check CHECK (owner IN ('legacy', 'local'))
		)`,
		`CREATE TABLE IF NOT EXISTS ` + TableOwnershipLog + ` (
			id         TEXT NOT NULL PRIMARY KEY,
			org_id     TEXT NOT NULL,
			system     TEXT NOT NULL,
			entity     TEXT NOT NULL,
			branch     TEXT NOT NULL,
			phase      TEXT NOT NULL,
			old_owner  TEXT NOT NULL DEFAULT '',
			new_owner  TEXT NOT NULL,
			note       TEXT NOT NULL DEFAULT '',
			actor      TEXT NOT NULL DEFAULT '',
			created_at ` + ts + ` NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_bridge_ownership_log_org ON ` + TableOwnershipLog + ` (org_id, system, created_at)`,
		`CREATE TABLE IF NOT EXISTS ` + TablePhases + ` (
			org_id     TEXT NOT NULL,
			system     TEXT NOT NULL,
			phase      TEXT NOT NULL,
			updated_by TEXT NOT NULL DEFAULT '',
			updated_at ` + ts + ` NOT NULL,
			PRIMARY KEY (org_id, system)
		)`,
		`CREATE TABLE IF NOT EXISTS ` + TablePending + ` (
			org_id       TEXT NOT NULL,
			system       TEXT NOT NULL,
			entity       TEXT NOT NULL,
			external_id  TEXT NOT NULL,
			envelope     TEXT NOT NULL,
			missing      TEXT NOT NULL DEFAULT '[]',
			attempts     INTEGER NOT NULL DEFAULT 0,
			first_seen   ` + ts + ` NOT NULL,
			next_attempt ` + ts + ` NOT NULL,
			PRIMARY KEY (org_id, system, entity, external_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_bridge_pending_due ON ` + TablePending + ` (next_attempt)`,
		`CREATE TABLE IF NOT EXISTS ` + TableCommands + ` (
			id              TEXT NOT NULL PRIMARY KEY,
			org_id          TEXT NOT NULL,
			system          TEXT NOT NULL,
			kind            TEXT NOT NULL,
			aggregate_key   TEXT NOT NULL,
			seq             BIGINT NOT NULL,
			idempotency_key TEXT NOT NULL,
			payload         TEXT NOT NULL,
			entity          TEXT NOT NULL DEFAULT '',
			local_id        TEXT NOT NULL DEFAULT '',
			status          TEXT NOT NULL DEFAULT 'pending',
			attempts        INTEGER NOT NULL DEFAULT 0,
			next_attempt_at ` + ts + ` NOT NULL,
			last_error      TEXT NOT NULL DEFAULT '',
			response        TEXT NOT NULL DEFAULT '',
			created_at      ` + ts + ` NOT NULL,
			updated_at      ` + ts + ` NOT NULL,
			CONSTRAINT bridge_commands_status_check CHECK (status IN ('pending', 'sent', 'dead', 'discarded'))
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_bridge_commands_idem ON ` + TableCommands + ` (org_id, system, idempotency_key)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_bridge_commands_seq ON ` + TableCommands + ` (org_id, system, aggregate_key, seq)`,
		`CREATE INDEX IF NOT EXISTS idx_bridge_commands_open ON ` + TableCommands + ` (status, next_attempt_at) WHERE status IN ('pending', 'dead')`,
		`CREATE INDEX IF NOT EXISTS idx_bridge_commands_org ON ` + TableCommands + ` (org_id, system, status, created_at)`,
	}
}

func advisoryKey(s string) int64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return int64(h.Sum64())
}

// GormStore implementa Store sobre las tablas bridge_*. Usa la transacción del
// contexto (WithTx / GormAtomic) cuando la hay.
type GormStore struct {
	db  *gorm.DB
	pg  bool
	Now func() time.Time
}

var _ Store = (*GormStore)(nil)

// NewGormStore crea el almacén. Si migrate es true corre Migrate.
func NewGormStore(ctx context.Context, db *gorm.DB, migrate bool) (*GormStore, error) {
	if db == nil {
		return nil, errors.New("legacybridge: NewGormStore requiere *gorm.DB")
	}
	if migrate {
		if err := Migrate(ctx, db); err != nil {
			return nil, err
		}
	}
	return &GormStore{db: db, pg: db.Dialector.Name() == "postgres", Now: time.Now}, nil
}

func (s *GormStore) conn(ctx context.Context) *gorm.DB { return DB(ctx, s.db) }
func (s *GormStore) now() time.Time                    { return s.Now().UTC() }

// --- refs ---

type refRow struct {
	OrgID         string
	System        string
	Entity        string
	ExternalID    string
	LocalID       string
	Owner         string
	SourceVersion string
	LastHash      string
	CreatedIn     string
	State         string
	UpdatedAt     time.Time
}

func (r refRow) ref() Ref {
	return Ref{Org: r.OrgID, System: r.System, Entity: r.Entity, ExternalID: r.ExternalID, LocalID: r.LocalID,
		Owner: Owner(r.Owner), SourceVersion: r.SourceVersion, LastHash: r.LastHash, CreatedIn: Owner(r.CreatedIn),
		State: RefState(r.State), UpdatedAt: r.UpdatedAt}
}

const refCols = `org_id, system, entity, external_id, local_id, owner, source_version, last_hash, created_in, state, updated_at`

func (s *GormStore) oneRef(ctx context.Context, where string, args ...any) (Ref, bool, error) {
	var rows []refRow
	if err := s.conn(ctx).Raw(`SELECT `+refCols+` FROM `+TableRefs+` WHERE `+where+` LIMIT 1`, args...).Scan(&rows).Error; err != nil {
		return Ref{}, false, err
	}
	if len(rows) == 0 {
		return Ref{}, false, nil
	}
	return rows[0].ref(), true, nil
}

func (s *GormStore) GetRef(ctx context.Context, org, system, entity, ext string) (Ref, bool, error) {
	return s.oneRef(ctx, `org_id = ? AND system = ? AND entity = ? AND external_id = ?`, org, system, entity, ext)
}

func (s *GormStore) GetRefByLocal(ctx context.Context, org, system, entity, local string) (Ref, bool, error) {
	return s.oneRef(ctx, `org_id = ? AND system = ? AND entity = ? AND local_id = ?`, org, system, entity, local)
}

func (s *GormStore) PutRef(ctx context.Context, r Ref) error {
	if err := r.validate(); err != nil {
		return err
	}
	other, ok, err := s.GetRefByLocal(ctx, r.Org, r.System, r.Entity, r.LocalID)
	if err != nil {
		return err
	}
	if ok && other.ExternalID != r.ExternalID {
		return ErrRefConflict
	}
	r = r.withDefaults()
	if r.UpdatedAt.IsZero() {
		r.UpdatedAt = s.now()
	}
	return s.conn(ctx).Exec(`INSERT INTO `+TableRefs+` (`+refCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (org_id, system, entity, external_id) DO UPDATE SET
			local_id = excluded.local_id, owner = excluded.owner, source_version = excluded.source_version,
			last_hash = excluded.last_hash, state = excluded.state, updated_at = excluded.updated_at`,
		r.Org, r.System, r.Entity, r.ExternalID, r.LocalID, string(r.Owner), r.SourceVersion, r.LastHash,
		string(r.CreatedIn), string(r.State), r.UpdatedAt).Error
}

func (s *GormStore) ListRefs(ctx context.Context, org, system, entity, after string, limit int) ([]Ref, error) {
	var rows []refRow
	if err := s.conn(ctx).Raw(`SELECT `+refCols+` FROM `+TableRefs+
		` WHERE org_id = ? AND system = ? AND entity = ? AND external_id > ? ORDER BY external_id LIMIT ?`,
		org, system, entity, after, clampRefLimit(limit)).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]Ref, len(rows))
	for i, r := range rows {
		out[i] = r.ref()
	}
	return out, nil
}

// Lock toma pg_advisory_xact_lock(key) en la transacción del contexto (solo
// Postgres; SQLite ya serializa las escrituras).
func (s *GormStore) Lock(ctx context.Context, key string) error {
	tx, ok := TxFrom(ctx)
	if !ok || !s.pg {
		return nil
	}
	return tx.Exec(`SELECT pg_advisory_xact_lock(?)`, advisoryKey(key)).Error
}

// --- cursores ---

func (s *GormStore) GetCursor(ctx context.Context, org, system, stream string) (string, error) {
	var rows []struct{ Watermark string }
	err := s.conn(ctx).Raw(`SELECT watermark FROM `+TableCursors+` WHERE org_id = ? AND system = ? AND stream = ?`, org, system, stream).Scan(&rows).Error
	if err != nil || len(rows) == 0 {
		return "", err
	}
	return rows[0].Watermark, nil
}

func (s *GormStore) SetCursor(ctx context.Context, org, system, stream, cursor string) error {
	return s.conn(ctx).Exec(`INSERT INTO `+TableCursors+` (org_id, system, stream, watermark, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (org_id, system, stream) DO UPDATE SET watermark = excluded.watermark, updated_at = excluded.updated_at`,
		org, system, stream, cursor, s.now()).Error
}

// --- propiedad ---

type ruleRow struct {
	OrgID     string
	System    string
	Entity    string
	Branch    string
	Phase     string
	Owner     string
	Note      string
	UpdatedBy string
	UpdatedAt time.Time
}

func (s *GormStore) Rules(ctx context.Context, org, system string) ([]OwnershipRule, error) {
	var rows []ruleRow
	err := s.conn(ctx).Raw(`SELECT org_id, system, entity, branch, phase, owner, note, updated_by, updated_at FROM `+TableOwnership+
		` WHERE org_id = ? AND system = ? ORDER BY entity, branch, phase`, org, system).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]OwnershipRule, len(rows))
	for i, r := range rows {
		out[i] = OwnershipRule{Org: r.OrgID, System: r.System, Entity: r.Entity, Branch: r.Branch, Phase: r.Phase,
			Owner: Owner(r.Owner), Note: r.Note, UpdatedBy: r.UpdatedBy, UpdatedAt: r.UpdatedAt}
	}
	return out, nil
}

func (s *GormStore) PutRule(ctx context.Context, r OwnershipRule) error {
	r, err := r.Normalize()
	if err != nil {
		return err
	}
	now := s.now()
	return GormAtomic(s.db)(ctx, func(ctx context.Context) error {
		db := s.conn(ctx)
		var prev []struct{ Owner string }
		if err := db.Raw(`SELECT owner FROM `+TableOwnership+` WHERE org_id = ? AND system = ? AND entity = ? AND branch = ? AND phase = ?`,
			r.Org, r.System, r.Entity, r.Branch, r.Phase).Scan(&prev).Error; err != nil {
			return err
		}
		old := ""
		if len(prev) > 0 {
			old = prev[0].Owner
		}
		if err := db.Exec(`INSERT INTO `+TableOwnership+` (org_id, system, entity, branch, phase, owner, note, updated_by, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (org_id, system, entity, branch, phase) DO UPDATE SET
				owner = excluded.owner, note = excluded.note, updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
			r.Org, r.System, r.Entity, r.Branch, r.Phase, string(r.Owner), r.Note, r.UpdatedBy, now).Error; err != nil {
			return err
		}
		return db.Exec(`INSERT INTO `+TableOwnershipLog+` (id, org_id, system, entity, branch, phase, old_owner, new_owner, note, actor, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			uuid.NewString(), r.Org, r.System, r.Entity, r.Branch, r.Phase, old, string(r.Owner), r.Note, r.UpdatedBy, now).Error
	})
}

func (s *GormStore) Phase(ctx context.Context, org, system string) (string, error) {
	var rows []struct{ Phase string }
	err := s.conn(ctx).Raw(`SELECT phase FROM `+TablePhases+` WHERE org_id = ? AND system = ?`, org, system).Scan(&rows).Error
	if err != nil || len(rows) == 0 {
		return "", err
	}
	return rows[0].Phase, nil
}

func (s *GormStore) SetPhase(ctx context.Context, org, system, phase, actor string) error {
	return s.conn(ctx).Exec(`INSERT INTO `+TablePhases+` (org_id, system, phase, updated_by, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (org_id, system) DO UPDATE SET phase = excluded.phase, updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
		org, system, phase, actor, s.now()).Error
}

// --- en espera ---

type pendingRow struct {
	OrgID       string
	System      string
	Entity      string
	ExternalID  string
	Envelope    string
	Missing     string
	Attempts    int
	FirstSeen   time.Time
	NextAttempt time.Time
}

func (r pendingRow) pending() (PendingEnvelope, error) {
	p := PendingEnvelope{Org: r.OrgID, System: r.System, Attempts: r.Attempts, FirstSeen: r.FirstSeen, NextAttempt: r.NextAttempt}
	if err := json.Unmarshal([]byte(r.Envelope), &p.Envelope); err != nil {
		return p, fmt.Errorf("legacybridge: envelope en espera ilegible (%s|%s): %w", r.Entity, r.ExternalID, err)
	}
	_ = json.Unmarshal([]byte(r.Missing), &p.Missing)
	return p, nil
}

const pendingCols = `org_id, system, entity, external_id, envelope, missing, attempts, first_seen, next_attempt`

func (s *GormStore) PutPending(ctx context.Context, p PendingEnvelope) error {
	env, err := json.Marshal(p.Envelope)
	if err != nil {
		return err
	}
	missing, _ := json.Marshal(p.Missing)
	return s.conn(ctx).Exec(`INSERT INTO `+TablePending+` (`+pendingCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (org_id, system, entity, external_id) DO UPDATE SET
			envelope = excluded.envelope, missing = excluded.missing, attempts = excluded.attempts, next_attempt = excluded.next_attempt`,
		p.Org, p.System, p.Envelope.Entity, p.Envelope.ExternalID, string(env), string(missing), p.Attempts,
		p.FirstSeen.UTC(), p.NextAttempt.UTC()).Error
}

func (s *GormStore) GetPending(ctx context.Context, org, system, entity, ext string) (PendingEnvelope, bool, error) {
	var rows []pendingRow
	if err := s.conn(ctx).Raw(`SELECT `+pendingCols+` FROM `+TablePending+` WHERE org_id = ? AND system = ? AND entity = ? AND external_id = ?`,
		org, system, entity, ext).Scan(&rows).Error; err != nil || len(rows) == 0 {
		return PendingEnvelope{}, false, err
	}
	p, err := rows[0].pending()
	return p, err == nil, err
}

func (s *GormStore) DeletePending(ctx context.Context, org, system, entity, ext string) error {
	return s.conn(ctx).Exec(`DELETE FROM `+TablePending+` WHERE org_id = ? AND system = ? AND entity = ? AND external_id = ?`,
		org, system, entity, ext).Error
}

func (s *GormStore) DuePending(ctx context.Context, now time.Time, limit int) ([]PendingEnvelope, error) {
	var rows []pendingRow
	if err := s.conn(ctx).Raw(`SELECT `+pendingCols+` FROM `+TablePending+` WHERE next_attempt <= ? ORDER BY next_attempt LIMIT ?`,
		now.UTC(), limit).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]PendingEnvelope, 0, len(rows))
	for _, r := range rows {
		p, err := r.pending()
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func (s *GormStore) CountPending(ctx context.Context, org, system string) (int64, error) {
	var n int64
	err := s.conn(ctx).Raw(`SELECT COUNT(*) FROM `+TablePending+` WHERE org_id = ? AND system = ?`, org, system).Scan(&n).Error
	return n, err
}

// --- comandos ---

type commandRow struct {
	ID             string
	OrgID          string
	System         string
	Kind           string
	AggregateKey   string
	Seq            int64
	IdempotencyKey string
	Payload        string
	Entity         string
	LocalID        string
	Status         string
	Attempts       int
	NextAttemptAt  time.Time
	LastError      string
	Response       string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (r commandRow) command() Command {
	c := Command{ID: r.ID, Org: r.OrgID, System: r.System, Kind: r.Kind, AggregateKey: r.AggregateKey, Seq: r.Seq,
		IdempotencyKey: r.IdempotencyKey, Payload: json.RawMessage(r.Payload), Entity: r.Entity, LocalID: r.LocalID,
		Status: CommandStatus(r.Status), Attempts: r.Attempts, NextAttemptAt: r.NextAttemptAt, LastError: r.LastError,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
	if r.Response != "" {
		c.Response = json.RawMessage(r.Response)
	}
	return c
}

const commandCols = `id, org_id, system, kind, aggregate_key, seq, idempotency_key, payload, entity, local_id, status, attempts, next_attempt_at, last_error, response, created_at, updated_at`

func (s *GormStore) commands(ctx context.Context, where string, args ...any) ([]Command, error) {
	var rows []commandRow
	if err := s.conn(ctx).Raw(`SELECT `+commandCols+` FROM `+TableCommands+` WHERE `+where, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]Command, len(rows))
	for i, r := range rows {
		out[i] = r.command()
	}
	return out, nil
}

func (s *GormStore) InsertCommand(ctx context.Context, c Command) (Command, bool, error) {
	if !json.Valid(c.Payload) {
		return Command{}, false, errors.New("legacybridge: payload del comando no es JSON")
	}
	var out Command
	var created bool
	err := GormAtomic(s.db)(ctx, func(ctx context.Context) error {
		db := s.conn(ctx)
		if s.pg {
			// Serializa la asignación de Seq por agregado.
			if err := db.Exec(`SELECT pg_advisory_xact_lock(?)`, advisoryKey(c.Org+"|"+c.System+"|"+c.AggregateKey)).Error; err != nil {
				return err
			}
		}
		if got, err := s.commands(ctx, `org_id = ? AND system = ? AND idempotency_key = ? LIMIT 1`, c.Org, c.System, c.IdempotencyKey); err != nil {
			return err
		} else if len(got) == 1 {
			out = got[0]
			return nil
		}
		var seq int64
		if err := db.Raw(`SELECT COALESCE(MAX(seq), 0) FROM `+TableCommands+` WHERE org_id = ? AND system = ? AND aggregate_key = ?`,
			c.Org, c.System, c.AggregateKey).Scan(&seq).Error; err != nil {
			return err
		}
		now := s.now()
		c.ID, c.Seq, c.Status, c.Attempts, c.CreatedAt, c.UpdatedAt = uuid.NewString(), seq+1, CommandPending, 0, now, now
		if c.NextAttemptAt.IsZero() {
			c.NextAttemptAt = now
		}
		if err := db.Exec(`INSERT INTO `+TableCommands+` (`+commandCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			c.ID, c.Org, c.System, c.Kind, c.AggregateKey, c.Seq, c.IdempotencyKey, string(c.Payload), c.Entity, c.LocalID,
			string(c.Status), c.Attempts, c.NextAttemptAt.UTC(), c.LastError, "", c.CreatedAt, c.UpdatedAt).Error; err != nil {
			return err
		}
		out, created = c, true
		return nil
	})
	return out, created, err
}

func (s *GormStore) GetCommand(ctx context.Context, id string) (Command, bool, error) {
	got, err := s.commands(ctx, `id = ?`, id)
	if err != nil || len(got) == 0 {
		return Command{}, false, err
	}
	return got[0], true, nil
}

func (s *GormStore) HeadCommand(ctx context.Context, a AggregateRef) (Command, bool, error) {
	got, err := s.commands(ctx, `org_id = ? AND system = ? AND aggregate_key = ? AND status IN ('pending', 'dead') ORDER BY seq LIMIT 1`,
		a.Org, a.System, a.AggregateKey)
	if err != nil || len(got) == 0 {
		return Command{}, false, err
	}
	return got[0], true, nil
}

func (s *GormStore) UpdateCommand(ctx context.Context, c Command) error {
	res := s.conn(ctx).Exec(`UPDATE `+TableCommands+` SET status = ?, attempts = ?, next_attempt_at = ?, last_error = ?, response = ?, created_at = ?, updated_at = ? WHERE id = ?`,
		string(c.Status), c.Attempts, c.NextAttemptAt.UTC(), c.LastError, string(c.Response), c.CreatedAt.UTC(), c.UpdatedAt.UTC(), c.ID)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrCommandNotFound
	}
	return nil
}

func (s *GormStore) ListCommands(ctx context.Context, f CommandFilter) ([]Command, error) {
	where, args := `1 = 1`, []any{}
	if f.Org != "" {
		where += ` AND org_id = ?`
		args = append(args, f.Org)
	}
	if f.System != "" {
		where += ` AND system = ?`
		args = append(args, f.System)
	}
	if f.Status != "" {
		where += ` AND status = ?`
		args = append(args, string(f.Status))
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	return s.commands(ctx, where+` ORDER BY created_at DESC, seq LIMIT ?`, append(args, limit)...)
}

func (s *GormStore) DueAggregates(ctx context.Context, now time.Time, limit int) ([]AggregateRef, error) {
	// Cabeza = menor seq abierto (pending o dead) del agregado; solo cuentan las
	// cabezas pending vencidas (una cabeza dead bloquea su agregado).
	var rows []struct {
		OrgID        string
		System       string
		AggregateKey string
	}
	err := s.conn(ctx).Raw(`SELECT c.org_id, c.system, c.aggregate_key FROM `+TableCommands+` c
		WHERE c.status = 'pending' AND c.next_attempt_at <= ?
		  AND NOT EXISTS (SELECT 1 FROM `+TableCommands+` p
		                  WHERE p.org_id = c.org_id AND p.system = c.system AND p.aggregate_key = c.aggregate_key
		                    AND p.status IN ('pending', 'dead') AND p.seq < c.seq)
		ORDER BY c.next_attempt_at LIMIT ?`, now.UTC(), limit).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]AggregateRef, len(rows))
	for i, r := range rows {
		out[i] = AggregateRef{Org: r.OrgID, System: r.System, AggregateKey: r.AggregateKey}
	}
	return out, nil
}

func (s *GormStore) CountCommands(ctx context.Context, org, system string, status CommandStatus) (int64, error) {
	var n int64
	q := `SELECT COUNT(*) FROM ` + TableCommands + ` WHERE org_id = ? AND system = ?`
	args := []any{org, system}
	if status != "" {
		q += ` AND status = ?`
		args = append(args, string(status))
	}
	err := s.conn(ctx).Raw(q, args...).Scan(&n).Error
	return n, err
}

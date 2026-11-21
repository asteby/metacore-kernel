// Package ledger is the generic balance-and-movement primitive behind stock,
// wallets, cash drawers and payment balances: cells that hold a running
// balance, append-only entries that record every change with the balance that
// resulted, idempotent postings, reversal by counter-entry, reconciliation and
// a post-commit event.
//
// A module owns the meaning (what an account is, which buckets exist, what the
// sign convention is, which floor applies); the ledger owns the mechanics that
// are easy to get subtly wrong and were being rewritten per module:
//
//   - one cell per (org, ledger, account, bucket), created race-safely;
//   - cells locked FOR UPDATE in a canonical order, so concurrent postings
//     touching the same cells in opposite order cannot deadlock;
//   - idempotency by key (unique index + double check under the cell locks): a
//     retried posting returns the original result with Replayed set;
//   - a floor per line (default 0): a posting that would dip below fails with
//     CodeInsufficient and writes nothing;
//   - reversal posts the exact inverse and links both ways; reversing twice is
//     refused;
//   - Reconcile compares every cell with the sum of its entries;
//   - the event is published after commit, one per posting, and an optional
//     in-transaction hook lets the module enqueue an outbox job atomically.
//
// Usage:
//
//	stock := ledger.New("inventory.stock", ledger.Config{Emit: publish})
//	err := stock.Run(ctx, db, func(s *ledger.Session) error {
//	    _, err := s.Post(ledger.Posting{
//	        Org: org, Key: nativekit.Key("sale", saleID, "line-1"), Kind: "sale",
//	        Lines: []ledger.Line{{Account: sku + "@" + wh, Bucket: "quantity", Delta: qty.Neg()}},
//	    })
//	    return err
//	})
//
// # Stability
//
// New, Config, Ledger, Session, Post, Reverse, Balance, Balances, History,
// Reconcile and the table models are stable under the kernel's semver. New
// capabilities arrive as new optional fields and methods.
package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/asteby/metacore-kernel/nativekit"
	"github.com/asteby/metacore-kernel/nativekit/dec"
)

// DefaultBucket is used when a Line does not name one.
const DefaultBucket = "balance"

// Stable error codes of the ledger.
const (
	CodeInsufficient     nativekit.Code = "ledger.insufficient"
	CodeInvalidPosting   nativekit.Code = "ledger.invalid_posting"
	CodeOverflow         nativekit.Code = "ledger.overflow"
	CodePostingNotFound  nativekit.Code = "ledger.posting_not_found"
	CodeAlreadyReversed  nativekit.Code = "ledger.already_reversed"
	CodeCannotReverse    nativekit.Code = "ledger.cannot_reverse"
	CodeKeyReused        nativekit.Code = nativekit.CodeIdempotencyConflict
	codeDuplicateInFlght nativekit.Code = nativekit.CodeIdempotencyInFlight
)

// Line is the change to one cell.
type Line struct {
	Account string
	// Bucket defaults to DefaultBucket.
	Bucket string
	// Delta is signed and never zero.
	Delta dec.D
	// Floor is the lowest balance this line may leave the cell at. Nil means
	// zero unless the ledger was configured with AllowNegative; set it below
	// zero to model a credit limit (Floor = -limit).
	Floor *dec.D
}

// NewPosting is the input of Post.
type NewPosting struct {
	Org uuid.UUID
	// Key makes the posting idempotent; empty disables it.
	Key string
	// Fingerprint (optional) binds the key to a payload digest; reusing the
	// key with another digest fails with nativekit.CodeIdempotencyConflict.
	Fingerprint string
	// Kind classifies the posting ("sale", "adjustment", "transfer", ...).
	Kind string
	// RefKind / RefID point at the business document that caused it.
	RefKind, RefID string
	Actor          string // opaque user id, never a name
	Memo           string
	Meta           map[string]any
	Lines          []Line
	OccurredAt     time.Time // zero = now

	reverses *uuid.UUID
}

// Result is what Post and Reverse return.
type Result struct {
	Posting Posting
	Entries []Entry
	// Replayed is true when Key was already applied: nothing changed and the
	// result is the original one.
	Replayed bool
}

// Event is what Config.Emit receives after commit, once per posting.
type Event struct {
	Ledger    string
	Org       uuid.UUID
	PostingID uuid.UUID
	Kind      string
	RefKind   string
	RefID     string
	Reversal  bool
	Cells     []CellChange
}

// CellChange is one touched cell in an Event.
type CellChange struct {
	Account string
	Bucket  string
	Delta   dec.D
	Balance dec.D
}

// Config tunes a Ledger.
type Config struct {
	// Emit is called after commit with one Event per posting. Failures are the
	// callback's to handle: they never fail the posting. Nil = no events.
	Emit func(ctx context.Context, ev Event)
	// OnPost runs inside the transaction after the posting is written. Return
	// an error to abort the whole operation. Use it to enqueue an outbox job
	// atomically with the balance change.
	OnPost func(ctx context.Context, tx *gorm.DB, ev Event) error
	// AllowNegative lets balances go below zero by default (overdraft style
	// ledgers). Per-line Floor still wins.
	AllowNegative bool
	// TablePrefix keeps this ledger in its own tables (<prefix>cells,
	// <prefix>postings, <prefix>entries) instead of the shared metacore_ledger_*
	// ones, so a module can adopt the ledger without migrating its data into
	// them. Letters, digits and underscore only. Create them with Ledger.Migrate.
	TablePrefix string
	// Now is injectable for tests.
	Now func() time.Time
}

// Ledger is a named ledger. It is cheap, stateless and safe for concurrent use;
// create one per logical ledger at boot.
type Ledger struct {
	name string
	cfg  Config

	tCells, tPostings, tEntries string
}

// New creates the ledger called name ("inventory.stock", "customer.wallet").
// The name partitions the shared tables, so two modules never see each other's
// cells.
func New(name string, cfg Config) *Ledger {
	if name == "" {
		panic("ledger: empty name")
	}
	l := &Ledger{name: name, cfg: cfg}
	l.tCells, l.tPostings, l.tEntries = tableNames(cfg.TablePrefix)
	return l
}

// Name is the ledger's partition key.
func (l *Ledger) Name() string { return l.name }

func (l *Ledger) now() time.Time {
	if l.cfg.Now != nil {
		return l.cfg.Now().UTC()
	}
	return time.Now().UTC()
}

// Session posts to a ledger inside an operation's transaction.
type Session struct {
	l  *Ledger
	op *nativekit.Op
}

// Session binds the ledger to an operation. The post-commit event fires through
// the operation's AfterCommit hooks, so it is only published if the whole
// operation commits.
func (l *Ledger) Session(op *nativekit.Op) *Session { return &Session{l: l, op: op} }

// Run is the common case: one transaction, one session.
func (l *Ledger) Run(ctx context.Context, db *gorm.DB, fn func(s *Session) error) error {
	return nativekit.Run(ctx, db, func(op *nativekit.Op) error { return fn(l.Session(op)) })
}

// Tx exposes the operation's transaction so the module can write its own rows
// atomically with the postings.
func (s *Session) Tx() *gorm.DB { return s.op.Tx() }

type cellKey struct{ account, bucket string }

// Post applies the lines atomically. See the package comment for the
// guarantees.
func (s *Session) Post(p NewPosting) (Result, error) {
	if err := validate(p); err != nil {
		return Result{}, err
	}
	tx := s.op.Tx()
	ctx := s.op.Context()

	if r, ok, err := s.replay(p); err != nil || ok {
		return r, err
	}

	// Lock every distinct cell in canonical order, creating the missing ones.
	keys := distinct(p.Lines)
	cells := make(map[cellKey]*Cell, len(keys))
	for _, k := range keys {
		c, err := s.lockCell(p.Org, k)
		if err != nil {
			return Result{}, err
		}
		cells[k] = c
	}
	// Double check under the locks: a concurrent twin that held them first has
	// committed its posting by now.
	if r, ok, err := s.replay(p); err != nil || ok {
		return r, err
	}

	now := p.OccurredAt
	if now.IsZero() {
		now = s.l.now()
	}
	post := Posting{
		ID: uuid.New(), OrgID: p.Org, Ledger: s.l.name, Fingerprint: p.Fingerprint, Kind: p.Kind,
		RefKind: p.RefKind, RefID: p.RefID, Actor: strings.TrimSpace(p.Actor), Memo: p.Memo,
		ReversesID: p.reverses, OccurredAt: now, CreatedAt: s.l.now(),
	}
	if p.Key != "" {
		k := p.Key
		post.IdemKey = &k
	}
	if len(p.Meta) > 0 {
		b, err := json.Marshal(p.Meta)
		if err != nil {
			return Result{}, nativekit.Wrap(CodeInvalidPosting, "meta is not JSON-encodable", err)
		}
		post.Meta = string(b)
	}

	entries := make([]Entry, 0, len(p.Lines))
	changes := make([]CellChange, 0, len(p.Lines))
	for i, ln := range p.Lines {
		k := cellKey{ln.Account, bucketOf(ln)}
		c := cells[k]
		next, ok := c.Balance.AddChecked(ln.Delta)
		if !ok {
			return Result{}, nativekit.E(CodeOverflow, "balance overflow", map[string]any{"account": ln.Account, "bucket": k.bucket})
		}
		if floor, has := s.l.floorOf(ln); has && next < floor {
			return Result{}, nativekit.E(CodeInsufficient, "insufficient balance", map[string]any{
				"account": ln.Account, "bucket": k.bucket,
				"balance": c.Balance.String(), "delta": ln.Delta.String(), "floor": floor.String(),
			})
		}
		c.Balance = next
		c.Version++
		entries = append(entries, Entry{
			ID: uuid.New(), PostingID: post.ID, OrgID: p.Org, Ledger: s.l.name, Account: ln.Account, Bucket: k.bucket,
			Seq: i, Delta: ln.Delta, BalanceAfter: next, CreatedAt: post.CreatedAt,
		})
		changes = append(changes, CellChange{Account: ln.Account, Bucket: k.bucket, Delta: ln.Delta, Balance: next})
	}
	for _, k := range keys {
		c := cells[k]
		if err := tx.Table(s.l.tCells).Where("id = ?", c.ID).
			Updates(map[string]any{"balance": c.Balance, "version": c.Version, "updated_at": s.l.now()}).Error; err != nil {
			return Result{}, err
		}
	}
	if err := tx.Table(s.l.tPostings).Create(&post).Error; err != nil {
		if isUnique(err) && p.Key != "" {
			return Result{}, nativekit.E(codeDuplicateInFlght, "a posting with this key is being applied", map[string]any{"key": p.Key})
		}
		return Result{}, err
	}
	if err := tx.Table(s.l.tEntries).Create(&entries).Error; err != nil {
		return Result{}, err
	}

	ev := Event{Ledger: s.l.name, Org: p.Org, PostingID: post.ID, Kind: p.Kind, RefKind: p.RefKind, RefID: p.RefID,
		Reversal: p.reverses != nil, Cells: changes}
	if s.l.cfg.OnPost != nil {
		if err := s.l.cfg.OnPost(ctx, tx, ev); err != nil {
			return Result{}, err
		}
	}
	if s.l.cfg.Emit != nil {
		emit := s.l.cfg.Emit
		s.op.AfterCommit(func(ctx context.Context) { emit(ctx, ev) })
	}
	return Result{Posting: post, Entries: entries}, nil
}

// ReverseInput is the input of Reverse.
type ReverseInput struct {
	Org       uuid.UUID
	PostingID uuid.UUID
	// Key makes the reversal idempotent. Empty defaults to a key derived from
	// the posting id, which also guarantees a posting is reversed at most once.
	Key   string
	Actor string
	Memo  string
	// AllowNegative lets the reversal dip below the lines' floors (reversing an
	// income after the goods were already consumed). Default: the floors apply.
	AllowNegative bool
}

// Reverse posts the exact inverse of a posting and links both. A reversal
// cannot itself be reversed (post a new correcting posting instead), and a
// posting is reversed at most once.
func (s *Session) Reverse(in ReverseInput) (Result, error) {
	tx := s.op.Tx()
	var orig Posting
	err := tx.Table(s.l.tPostings).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND org_id = ? AND ledger = ?", in.PostingID, in.Org, s.l.name).Take(&orig).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Result{}, nativekit.E(CodePostingNotFound, "posting not found", map[string]any{"posting_id": in.PostingID.String()})
	}
	if err != nil {
		return Result{}, err
	}
	key := in.Key
	if key == "" {
		key = nativekit.Key("reverse", orig.ID.String())
	}
	if orig.ReversesID != nil {
		return Result{}, nativekit.E(CodeCannotReverse, "a reversal cannot be reversed", map[string]any{"posting_id": orig.ID.String()})
	}
	if orig.ReversedBy != nil {
		var prev Posting
		if err := tx.Table(s.l.tPostings).Where("id = ?", *orig.ReversedBy).Take(&prev).Error; err == nil && prev.IdemKey != nil && *prev.IdemKey == key {
			r, _, rerr := s.replay(NewPosting{Org: in.Org, Key: key})
			return r, rerr
		}
		return Result{}, nativekit.E(CodeAlreadyReversed, "posting already reversed", map[string]any{"posting_id": orig.ID.String()})
	}
	var entries []Entry
	if err := tx.Table(s.l.tEntries).Where("posting_id = ?", orig.ID).Order("seq").Find(&entries).Error; err != nil {
		return Result{}, err
	}
	lines := make([]Line, 0, len(entries))
	for _, e := range entries {
		ln := Line{Account: e.Account, Bucket: e.Bucket, Delta: e.Delta.Neg()}
		if in.AllowNegative {
			floor := dec.D(-1 << 62)
			ln.Floor = &floor
		}
		lines = append(lines, ln)
	}
	id := orig.ID
	r, err := s.Post(NewPosting{
		Org: in.Org, Key: key, Kind: "reversal", RefKind: orig.RefKind, RefID: orig.RefID,
		Actor: in.Actor, Memo: in.Memo, Lines: lines, reverses: &id,
	})
	if err != nil || r.Replayed {
		return r, err
	}
	if err := tx.Table(s.l.tPostings).Where("id = ?", orig.ID).Update("reversed_by", r.Posting.ID).Error; err != nil {
		return Result{}, err
	}
	return r, nil
}

// replay returns the stored result when p.Key was already applied.
func (s *Session) replay(p NewPosting) (Result, bool, error) {
	if p.Key == "" {
		return Result{}, false, nil
	}
	tx := s.op.Tx()
	var prev Posting
	err := tx.Table(s.l.tPostings).Where("org_id = ? AND ledger = ? AND idem_key = ?", p.Org, s.l.name, p.Key).Take(&prev).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	if p.Fingerprint != "" && prev.Fingerprint != "" && p.Fingerprint != prev.Fingerprint {
		return Result{}, false, nativekit.E(CodeKeyReused, "idempotency key reused with a different payload", map[string]any{"key": p.Key})
	}
	var entries []Entry
	if err := tx.Table(s.l.tEntries).Where("posting_id = ?", prev.ID).Order("seq").Find(&entries).Error; err != nil {
		return Result{}, false, err
	}
	return Result{Posting: prev, Entries: entries, Replayed: true}, true, nil
}

func validate(p NewPosting) error {
	bad := func(why string) error { return nativekit.E(CodeInvalidPosting, why) }
	if p.Org == uuid.Nil {
		return bad("org is required")
	}
	if len(p.Lines) == 0 {
		return bad("a posting needs at least one line")
	}
	for _, ln := range p.Lines {
		if strings.TrimSpace(ln.Account) == "" {
			return bad("line account is required")
		}
		if ln.Delta.IsZero() {
			return bad("line delta must not be zero")
		}
	}
	return nil
}

func bucketOf(ln Line) string {
	if ln.Bucket == "" {
		return DefaultBucket
	}
	return ln.Bucket
}

func (l *Ledger) floorOf(ln Line) (dec.D, bool) {
	if ln.Floor != nil {
		return *ln.Floor, true
	}
	if l.cfg.AllowNegative {
		return 0, false
	}
	return 0, true
}

// distinct returns the lines' cells deduplicated in canonical lock order.
func distinct(lines []Line) []cellKey {
	raw := make([]string, 0, len(lines))
	byRaw := make(map[string]cellKey, len(lines))
	for _, ln := range lines {
		k := cellKey{ln.Account, bucketOf(ln)}
		r := k.account + "\x00" + k.bucket
		raw = append(raw, r)
		byRaw[r] = k
	}
	out := make([]cellKey, 0, len(raw))
	for _, r := range nativekit.SortedKeys(raw...) {
		out = append(out, byRaw[r])
	}
	return out
}

// lockCell returns the cell locked FOR UPDATE, creating it empty when missing
// (the unique index makes concurrent creation safe).
func (s *Session) lockCell(org uuid.UUID, k cellKey) (*Cell, error) {
	tx := s.op.Tx()
	var c Cell
	find := func() error {
		return tx.Table(s.l.tCells).Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("org_id = ? AND ledger = ? AND account = ? AND bucket = ?", org, s.l.name, k.account, k.bucket).Take(&c).Error
	}
	err := find()
	if err == nil {
		return &c, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	nc := Cell{ID: uuid.New(), OrgID: org, Ledger: s.l.name, Account: k.account, Bucket: k.bucket}
	if err := tx.Table(s.l.tCells).Clauses(clause.OnConflict{DoNothing: true}).Create(&nc).Error; err != nil {
		return nil, err
	}
	if err := find(); err != nil {
		return nil, err
	}
	return &c, nil
}

func isUnique(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "unique") || strings.Contains(m, "duplicate key")
}

package dynamic

import (
	"context"
	"errors"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type RxSession struct {
	modelbase.BaseUUIDModel
	State string `json:"state"`
}

func (RxSession) TableName() string                    { return "rx_sessions" }
func (RxSession) DefineTable() modelbase.TableMetadata { return modelbase.TableMetadata{Title: "S"} }
func (RxSession) DefineModal() modelbase.ModalMetadata { return modelbase.ModalMetadata{Title: "S"} }

type RxOrder struct {
	modelbase.BaseUUIDModel
	Total float64 `json:"total"`
}

func (RxOrder) TableName() string                    { return "rx_orders" }
func (RxOrder) DefineTable() modelbase.TableMetadata { return modelbase.TableMetadata{Title: "O"} }
func (RxOrder) DefineModal() modelbase.ModalMetadata { return modelbase.ModalMetadata{Title: "O"} }

type RxPayment struct {
	modelbase.BaseUUIDModel
	SessionID string  `json:"session_id"`
	OrderID   string  `json:"order_id"`
	Amount    float64 `json:"amount"`
	Status    string  `json:"status"`
}

func (RxPayment) TableName() string                    { return "rx_payments" }
func (RxPayment) DefineTable() modelbase.TableMetadata { return modelbase.TableMetadata{Title: "P"} }
func (RxPayment) DefineModal() modelbase.ModalMetadata { return modelbase.ModalMetadata{Title: "P"} }

var rxRules = []manifest.CrossRuleDef{
	{Kind: "ref_state", ErrorKey: "pos.session_closed", Ref: "session_id", Parent: "rx_sessions", Require: map[string]any{"state": "open"}},
	{Kind: "sum_lte", ErrorKey: "pos.overpay", Ref: "order_id", Parent: "rx_orders", Sum: "amount", Max: "total", Where: map[string]any{"status": []any{"completed"}}},
}

func rxSetup(t *testing.T) (*Service, *gorm.DB, *fakeUser, string, string, string) {
	t.Helper()
	db := setupTestDB(t)
	for _, ddl := range []string{
		`CREATE TABLE rx_sessions (id TEXT PRIMARY KEY, organization_id TEXT, created_at DATETIME, updated_at DATETIME, deleted_at DATETIME, created_by_id TEXT, state TEXT)`,
		`CREATE TABLE rx_orders (id TEXT PRIMARY KEY, organization_id TEXT, created_at DATETIME, updated_at DATETIME, deleted_at DATETIME, created_by_id TEXT, total REAL)`,
		`CREATE TABLE rx_payments (id TEXT PRIMARY KEY, organization_id TEXT, created_at DATETIME, updated_at DATETIME, deleted_at DATETIME, created_by_id TEXT, session_id TEXT, order_id TEXT, amount REAL, status TEXT)`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatal(err)
		}
	}
	org := uuid.New()
	open, closed, order := uuid.NewString(), uuid.NewString(), uuid.NewString()
	db.Exec(`INSERT INTO rx_sessions (id, organization_id, state) VALUES (?, ?, 'open'), (?, ?, 'closed')`, open, org.String(), closed, org.String())
	db.Exec(`INSERT INTO rx_orders (id, organization_id, total) VALUES (?, ?, 500)`, order, org.String())
	modelbase.Register("rx_sessions", func() modelbase.ModelDefiner { return &RxSession{} })
	modelbase.Register("rx_orders", func() modelbase.ModelDefiner { return &RxOrder{} })
	modelbase.Register("rx_payments", func() modelbase.ModelDefiner { return &RxPayment{} })
	svc := New(Config{
		DB:       db,
		Metadata: metadata.New(metadata.Config{CacheTTL: -1}),
		ConstraintResolver: func(_ context.Context, model string) (*ModelConstraints, bool) {
			if model == "rx_payments" {
				return &ModelConstraints{Rules: rxRules}, true
			}
			return nil, false
		},
	})
	return svc, db, newUser(org), open, closed, order
}

func rxCount(db *gorm.DB) int64 {
	var n int64
	db.Table("rx_payments").Count(&n)
	return n
}

func wantRuleKey(t *testing.T, err error, key string) {
	t.Helper()
	var ce *ConstraintError
	if !errors.Is(err, ErrConstraintViolation) || !errors.As(err, &ce) || ce.ErrorKey != key {
		t.Fatalf("want constraint_violation %q, got %v", key, err)
	}
}

func TestCrossRecord_CreateOnClosedSessionRejected(t *testing.T) {
	svc, db, user, open, closed, order := rxSetup(t)
	ctx := context.Background()
	_, err := svc.Create(ctx, "rx_payments", user, map[string]any{"session_id": closed, "order_id": order, "amount": 10.0, "status": "completed"})
	wantRuleKey(t, err, "pos.session_closed")
	if rxCount(db) != 0 {
		t.Fatal("rejected create must not insert")
	}
	if _, err := svc.Create(ctx, "rx_payments", user, map[string]any{"session_id": open, "order_id": order, "amount": 10.0, "status": "completed"}); err != nil {
		t.Fatalf("open session must accept: %v", err)
	}
}

func TestCrossRecord_OverpayAcrossRowsRejectedAndCancelledIgnored(t *testing.T) {
	svc, db, user, open, _, order := rxSetup(t)
	ctx := context.Background()
	mk := func(amount float64, status string) error {
		_, err := svc.Create(ctx, "rx_payments", user, map[string]any{"session_id": open, "order_id": order, "amount": amount, "status": status})
		return err
	}
	if err := mk(300, "completed"); err != nil {
		t.Fatal(err)
	}
	if err := mk(9000, "cancelled"); err != nil { // outside where: does not count
		t.Fatalf("cancelled payment must not count toward the cap: %v", err)
	}
	wantRuleKey(t, mk(300, "completed"), "pos.overpay") // 600 > 500
	if err := mk(200, "completed"); err != nil {        // exactly 500
		t.Fatalf("payment up to the total must pass: %v", err)
	}
	if got := rxCount(db); got != 3 {
		t.Fatalf("rows = %d, want 3 (rejected one rolled back)", got)
	}
}

func TestCrossRecord_UpdateRaisingAmountRejectedUnrelatedEditAllowed(t *testing.T) {
	svc, db, user, open, _, order := rxSetup(t)
	ctx := context.Background()
	p, err := svc.Create(ctx, "rx_payments", user, map[string]any{"session_id": open, "order_id": order, "amount": 400.0, "status": "completed"})
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.MustParse(p["id"].(string))
	_, err = svc.Update(ctx, "rx_payments", user, id, map[string]any{"amount": 900.0})
	wantRuleKey(t, err, "pos.overpay")
	var amt float64
	db.Table("rx_payments").Select("amount").Where("id = ?", id.String()).Scan(&amt)
	if amt != 400 {
		t.Fatalf("update must roll back, amount = %v", amt)
	}
	// Closing the session later must not block an unrelated edit of the payment.
	db.Exec(`UPDATE rx_sessions SET state='closed' WHERE id = ?`, open)
	if _, err := svc.Update(ctx, "rx_payments", user, id, map[string]any{"status": "cancelled"}); err != nil {
		t.Fatalf("edit that does not touch ref/sum must pass: %v", err)
	}
}

func TestCrossRecord_CrossTenantParentNotFound(t *testing.T) {
	svc, _, _, open, _, order := rxSetup(t)
	other := newUser(uuid.New())
	_, err := svc.Create(context.Background(), "rx_payments", other, map[string]any{"session_id": open, "order_id": order, "amount": 1.0, "status": "completed"})
	wantRuleKey(t, err, "pos.session_closed")
}

// The wasm data_mutate path adapts the same evaluator through CrossRecordCompute.
func TestCrossRecordCompute_SkipsDeletesAndEvaluatesWrites(t *testing.T) {
	_, db, user, _, closed, order := rxSetup(t)
	fn := CrossRecordCompute(
		func(tbl string) []manifest.CrossRuleDef {
			if tbl == "rx_payments" {
				return rxRules
			}
			return nil
		},
		func(tbl string) string { return tbl },
		func(model string) (string, error) { return model, nil },
	)
	ctx := context.Background()
	row := map[string]any{"id": uuid.NewString(), "session_id": closed, "order_id": order, "amount": 1.0, "status": "completed"}
	err := fn(ctx, db, user.GetOrganizationID(), "rx_payments", "created", row)
	wantRuleKey(t, err, "pos.session_closed")
	if err := fn(ctx, db, user.GetOrganizationID(), "rx_payments", "deleted", row); err != nil {
		t.Fatalf("deletes are exempt: %v", err)
	}
	if err := fn(ctx, db, user.GetOrganizationID(), "other", "created", row); err != nil {
		t.Fatalf("tables without rules are inert: %v", err)
	}
}

func rxSetupSkip(t *testing.T, onMissing string) (*Service, *gorm.DB, *fakeUser, string, string) {
	t.Helper()
	svc, db, user, open, _, order := rxSetup(t)
	rules := make([]manifest.CrossRuleDef, len(rxRules))
	copy(rules, rxRules)
	rules[1].OnMissingParent = onMissing
	svc.constraints = func(_ context.Context, model string) (*ModelConstraints, bool) {
		if model == "rx_payments" {
			return &ModelConstraints{Rules: rules}, true
		}
		return nil, false
	}
	return svc, db, user, open, order
}

func TestCrossRecord_MissingParentRejectedByDefault(t *testing.T) {
	for _, mode := range []string{"", "reject"} {
		svc, db, user, open, _ := rxSetupSkip(t, mode)
		_, err := svc.Create(context.Background(), "rx_payments", user, map[string]any{"session_id": open, "order_id": uuid.NewString(), "amount": 10.0, "status": "completed"})
		wantRuleKey(t, err, "pos.overpay")
		if rxCount(db) != 0 {
			t.Fatalf("mode %q: rejected create must not insert", mode)
		}
	}
}

func TestCrossRecord_MissingParentSkippedWhenOptedIn(t *testing.T) {
	svc, db, user, open, order := rxSetupSkip(t, "skip")
	ctx := context.Background()
	if _, err := svc.Create(ctx, "rx_payments", user, map[string]any{"session_id": open, "order_id": uuid.NewString(), "amount": 10.0, "status": "completed"}); err != nil {
		t.Fatalf("missing parent with skip must pass: %v", err)
	}
	// Parent present: the cap still applies.
	if _, err := svc.Create(ctx, "rx_payments", user, map[string]any{"session_id": open, "order_id": order, "amount": 400.0, "status": "completed"}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Create(ctx, "rx_payments", user, map[string]any{"session_id": open, "order_id": order, "amount": 200.0, "status": "completed"})
	wantRuleKey(t, err, "pos.overpay")
	if got := rxCount(db); got != 2 {
		t.Fatalf("rows = %d, want 2 (overpay rolled back)", got)
	}
}

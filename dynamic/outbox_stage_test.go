package dynamic

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// metaRecorder is a Publisher that keeps the Meta each publication carried.
type metaRecorder struct {
	mu    sync.Mutex
	metas []Meta
	ctxs  []context.Context
	names []string
}

func (r *metaRecorder) Publish(ctx context.Context, _ string, event string, _ uuid.UUID, _ any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.metas = append(r.metas, MetaFromContext(ctx))
	r.ctxs = append(r.ctxs, ctx)
	r.names = append(r.names, event)
	return nil
}

// X1.1: a crash between the outbox write and the publication is recovered by
// the relay, and the re-publication carries the SAME identity (event id,
// aggregate, sequence, causation, actor) as the first attempt would have, which
// is what lets the dispatch ledger collapse it.
func TestOutbox_GuestEventCrashBetweenPersistAndPublishIsRelayed(t *testing.T) {
	rec := &metaRecorder{}
	svc := newOutboxTestService(t, rec)
	pub := NewOutboxPublisher(svc.db, rec)
	pub.OnPersisted(func() error { return fmt.Errorf("simulated crash") })

	org := uuid.New()
	ev := StagedEvent{
		AddonKey: "warehouse", Name: "warehouse.stock_picked", OrgID: org,
		Payload:       []byte(`{"sku":"A"}`),
		AggregateType: "Pick", AggregateID: "p-1",
		CausationID: "cause-1", CorrelationID: "corr-1", ActorID: "actor-1", BranchID: "branch-1",
	}
	pub.Publish(context.Background(), nil, []StagedEvent{ev})
	if len(rec.metas) != 0 {
		t.Fatalf("crash must prevent publication, got %d", len(rec.metas))
	}
	var row EventOutbox
	if err := svc.db.First(&row).Error; err != nil || row.PublishedAt != nil || row.Sequence != 1 {
		t.Fatalf("want one unpublished sequenced row, err=%v row=%+v", err, row)
	}

	// Fresh rows are not touched by the relay (it could race the in-line publish).
	svc.RelayOutboxOnce(context.Background())
	if len(rec.metas) != 0 {
		t.Fatal("relay must skip rows younger than its min age")
	}
	svc.db.Model(&EventOutbox{}).Where("id = ?", row.ID).Update("created_at", time.Now().UTC().Add(-time.Minute))
	svc.RelayOutboxOnce(context.Background())

	if len(rec.metas) != 1 {
		t.Fatalf("relay published %d times, want 1", len(rec.metas))
	}
	m := rec.metas[0]
	if m.ID != row.EventID || m.ID == "" || m.AggregateType != "Pick" || m.AggregateID != "p-1" || m.Sequence != 1 || m.CausationID != "cause-1" {
		t.Fatalf("relayed meta lost its identity: %+v (row %+v)", m, row)
	}
	if ActorIDFromContext(rec.ctxs[0]) != "actor-1" || BranchIDFromContext(rec.ctxs[0]) != "branch-1" || CorrelationIDFromContext(rec.ctxs[0]) != "corr-1" {
		t.Fatal("relayed ctx lost actor/branch/correlation")
	}
	if err := svc.db.First(&row, "id = ?", row.ID).Error; err != nil || row.PublishedAt == nil {
		t.Fatalf("relay must stamp published_at: %v", err)
	}
	// Second sweep: nothing left to do.
	svc.RelayOutboxOnce(context.Background())
	if len(rec.metas) != 1 {
		t.Fatal("a published row must not be relayed again")
	}
}

func TestOutbox_AggregateSequenceIsMonotonicPerAggregate(t *testing.T) {
	rec := &metaRecorder{}
	svc := newOutboxTestService(t, rec)
	pub := NewOutboxPublisher(svc.db, rec)
	org := uuid.New()
	mk := func(agg string) StagedEvent {
		return StagedEvent{AddonKey: "a", Name: "a.changed", OrgID: org, Payload: []byte(`{}`), AggregateType: "T", AggregateID: agg}
	}
	pub.Publish(context.Background(), nil, []StagedEvent{mk("x"), mk("x"), mk("y"), mk("x")})
	var got []string
	for _, m := range rec.metas {
		got = append(got, fmt.Sprintf("%s=%d", m.AggregateID, m.Sequence))
	}
	want := []string{"x=1", "x=2", "y=1", "x=3"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("sequences = %v, want %v", got, want)
	}
	// A different org has its own counter.
	pub.Publish(context.Background(), nil, []StagedEvent{{AddonKey: "a", Name: "a.changed", OrgID: uuid.New(), Payload: []byte(`{}`), AggregateType: "T", AggregateID: "x"}})
	if last := rec.metas[len(rec.metas)-1]; last.Sequence != 1 {
		t.Fatalf("other org sequence = %d, want 1", last.Sequence)
	}
}

func TestOutbox_CanonicalEventCarriesEnvelopeIdentity(t *testing.T) {
	rec := &metaRecorder{}
	svc := newOutboxTestService(t, rec)
	user := newUser(uuid.New())
	id := uuid.NewString()
	svc.publishCanonical(context.Background(), "dyn_items", "updated", user, id, nil, map[string]any{"x": 1})
	svc.publishCanonical(context.Background(), "dyn_items", "updated", user, id, nil, map[string]any{"x": 2})
	if len(rec.metas) != 2 {
		t.Fatalf("publications = %d", len(rec.metas))
	}
	var ce CanonicalEvent
	var row EventOutbox
	svc.db.Order("created_at").First(&row)
	_ = json.Unmarshal([]byte(row.Payload), &ce)
	if rec.metas[0].ID != ce.OccurrenceID || rec.metas[0].AggregateID != id || rec.metas[0].Sequence != 1 || rec.metas[1].Sequence != 2 {
		t.Fatalf("canonical meta = %+v / %+v (occurrence %s)", rec.metas[0], rec.metas[1], ce.OccurrenceID)
	}
}

// A guest-declared aggregate counts in its emitter's own namespace: another
// addon (or a canonical model) with the same type/id never advances it.
func TestOutbox_GuestAggregateSequenceIsNamespacedPerEmitter(t *testing.T) {
	rec := &metaRecorder{}
	svc := newOutboxTestService(t, rec)
	pub := NewOutboxPublisher(svc.db, rec)
	org := uuid.New()
	mk := func(addon string) StagedEvent {
		return StagedEvent{AddonKey: addon, Name: addon + ".changed", OrgID: org, Payload: []byte(`{}`), AggregateType: "Order", AggregateID: "1"}
	}
	pub.Publish(context.Background(), nil, []StagedEvent{mk("a"), mk("b"), mk("b"), mk("a")})
	var got []string
	for _, m := range rec.metas {
		got = append(got, fmt.Sprint(m.Sequence))
	}
	if fmt.Sprint(got) != "[1 1 2 2]" {
		t.Fatalf("sequences = %v, want [1 1 2 2]", got)
	}
	// A canonical counter for the same (type, id) is untouched.
	seq, err := nextAggregateSequence(context.Background(), svc.db, org, "Order", "1")
	if err != nil || seq != 1 {
		t.Fatalf("canonical counter = %d (%v), want 1", seq, err)
	}
}

// An unarmed publisher must not migrate inside the caller's tx on a
// one-connection pool; a failed arm retries later instead of staying off.
func TestOutbox_UnarmedPublishInTxDoesNotHangAndArmRetries(t *testing.T) {
	rec := &metaRecorder{}
	svc := newOutboxTestService(t, rec)
	pub := NewOutboxPublisher(svc.db, rec) // not armed
	ctx, hook := WithCommitHook(context.Background())
	tx := svc.db.Begin()
	done := make(chan struct{})
	go func() {
		pub.Publish(ctx, tx, []StagedEvent{{AddonKey: "a", Name: "a.x", OrgID: uuid.New(), Payload: []byte(`{}`)}})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("HANG: Publish in a tx blocked")
	}
	if err := tx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	hook.Run(context.Background())
	if len(rec.metas) != 1 {
		t.Fatalf("published %d, want 1 after commit", len(rec.metas))
	}

	// Let the async arm attempt of pub finish so it cannot contend with p2.
	for i := 0; i < 500 && (!pub.armed.Load() || pub.arming.Load()); i++ {
		time.Sleep(10 * time.Millisecond)
	}

	// Arm failure is not permanent: backoff, then a later call succeeds.
	p2 := NewOutboxPublisher(svc.db, rec)
	p2.armFails, p2.retryAt = 3, time.Now().Add(time.Hour) // as after failed migrations
	if p2.Arm() {
		t.Fatal("Arm must respect the backoff window")
	}
	p2.retryAt = time.Now().Add(-time.Second)
	armed := false
	for i := 0; i < 50 && !armed; i++ {
		if armed = p2.Arm(); !armed {
			p2.retryAt = time.Now().Add(-time.Second) // transient lock: keep retrying
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !armed {
		t.Fatal("Arm must retry after the backoff instead of staying off forever")
	}
}

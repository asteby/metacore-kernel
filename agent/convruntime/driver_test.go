package convruntime

import (
	"context"
	"errors"
	"sync"
	"testing"

	kdynamic "github.com/asteby/metacore-kernel/dynamic"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type fakeConvs struct {
	rows map[uuid.UUID]Conversation
	err  error
}

func (f fakeConvs) Load(_ context.Context, id uuid.UUID) (Conversation, error) {
	if f.err != nil {
		return Conversation{}, f.err
	}
	c, ok := f.rows[id]
	if !ok {
		return Conversation{}, ErrConversationNotFound
	}
	return c, nil
}

type call struct {
	conv    Conversation
	content string
	msgID   string
}

type fakeDispatcher struct {
	mu    sync.Mutex
	calls []call
}

func (f *fakeDispatcher) Dispatch(_ context.Context, c Conversation, content, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{c, content, id})
}

func (f *fakeDispatcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type fakeBus struct {
	event, key string
	handler    Handler
	unsub      []string
}

func (b *fakeBus) SubscribeAll(e, k string, h Handler) { b.event, b.key, b.handler = e, k, h }
func (b *fakeBus) Unsubscribe(k string)                { b.unsub = append(b.unsub, k) }

func newDriver(t *testing.T) (*driver, *fakeDispatcher, fakeConvs) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&ProcessedMessage{}); err != nil {
		t.Fatal(err)
	}
	convs := fakeConvs{rows: map[uuid.UUID]Conversation{}}
	disp := &fakeDispatcher{}
	return &driver{db: db, conv: convs, disp: disp}, disp, convs
}

func seed(c fakeConvs, org uuid.UUID, processType string, active bool) uuid.UUID {
	id := uuid.New()
	c.rows[id] = Conversation{ID: id, OrganizationID: org, ProcessType: processType, ProcessActive: active}
	return id
}

func inbound(msgID, convID uuid.UUID, sender, content string) *kdynamic.CanonicalEvent {
	return &kdynamic.CanonicalEvent{
		ID: msgID.String(), Model: "Message", Action: "created", AddonKey: "link_inbox",
		After: map[string]any{"id": msgID.String(), "sender_type": sender, "conversation_id": convID.String(), "content": content},
	}
}

func TestDispatchesContactMessageForAgentAndFlow(t *testing.T) {
	d, disp, convs := newDriver(t)
	org := uuid.New()
	for _, pt := range []string{"agent", "flow"} {
		msg := uuid.New()
		_ = d.handle(context.Background(), org, inbound(msg, seed(convs, org, pt, true), "contact", "hola"))
		last := disp.calls[len(disp.calls)-1]
		if last.content != "hola" || last.msgID != msg.String() || last.conv.ProcessType != pt {
			t.Fatalf("%s: %+v", pt, last)
		}
	}
	if disp.count() != 2 {
		t.Fatalf("got %d", disp.count())
	}
}

func TestFilters(t *testing.T) {
	d, disp, convs := newDriver(t)
	org := uuid.New()
	agentConv := seed(convs, org, "agent", true)
	for _, s := range []string{"agent", "flow", "bot", "user", ""} {
		_ = d.handle(context.Background(), org, inbound(uuid.New(), agentConv, s, "eco"))
	}
	for _, pt := range []string{"user", "orchestrator", ""} {
		_ = d.handle(context.Background(), org, inbound(uuid.New(), seed(convs, org, pt, true), "contact", "hi"))
	}
	_ = d.handle(context.Background(), org, inbound(uuid.New(), seed(convs, org, "agent", false), "contact", "hi"))
	_ = d.handle(context.Background(), org, inbound(uuid.New(), agentConv, "contact", ""))
	_ = d.handle(context.Background(), org, inbound(uuid.New(), uuid.New(), "contact", "ghost conversation"))
	_ = d.handle(context.Background(), org, map[string]any{"sender_type": "contact"})
	_ = d.handle(context.Background(), org, &kdynamic.CanonicalEvent{ID: "x"})
	if disp.count() != 0 {
		t.Fatalf("nothing should dispatch, got %d", disp.count())
	}
}

func TestTenantGuardDropsForeignOrg(t *testing.T) {
	d, disp, convs := newDriver(t)
	convID := seed(convs, uuid.New(), "agent", true)
	_ = d.handle(context.Background(), uuid.New(), inbound(uuid.New(), convID, "contact", "hola"))
	if disp.count() != 0 {
		t.Fatal("an event from another org must not reach the conversation")
	}
}

func TestDedupsRepeatedDelivery(t *testing.T) {
	d, disp, convs := newDriver(t)
	org := uuid.New()
	ev := inbound(uuid.New(), seed(convs, org, "agent", true), "contact", "una vez")
	for i := 0; i < 3; i++ {
		_ = d.handle(context.Background(), org, ev)
	}
	var n int64
	d.db.Model(&ProcessedMessage{}).Count(&n)
	if disp.count() != 1 || n != 1 {
		t.Fatalf("dispatches=%d ledger=%d", disp.count(), n)
	}
}

func TestFallsBackToAfterIDAndSurvivesLoaderErrors(t *testing.T) {
	d, disp, convs := newDriver(t)
	org := uuid.New()
	convID := seed(convs, org, "agent", true)
	msg := uuid.New()
	ev := inbound(msg, convID, "contact", "hola")
	ev.ID = "" // no event id: use after.id
	_ = d.handle(context.Background(), org, ev)
	if disp.count() != 1 || disp.calls[0].msgID != msg.String() {
		t.Fatalf("%+v", disp.calls)
	}
	d.conv = fakeConvs{err: errors.New("db down")}
	if err := d.handle(context.Background(), org, inbound(uuid.New(), convID, "contact", "x")); err != nil {
		t.Fatal("handler must never break the bus fanout")
	}
	if disp.count() != 1 {
		t.Fatal("loader error must drop the message")
	}
}

func TestWire(t *testing.T) {
	db, _ := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	bus := &fakeBus{}
	ok := Config{Bus: bus, DB: db, Conversations: fakeConvs{}, Dispatcher: &fakeDispatcher{}}
	for name, c := range map[string]Config{
		"bus": {DB: db, Conversations: ok.Conversations, Dispatcher: ok.Dispatcher},
		"db":  {Bus: bus, Conversations: ok.Conversations, Dispatcher: ok.Dispatcher},
		"cv":  {Bus: bus, DB: db, Dispatcher: ok.Dispatcher},
		"dp":  {Bus: bus, DB: db, Conversations: ok.Conversations},
	} {
		if _, err := Wire(c); err == nil {
			t.Errorf("nil %s must be rejected", name)
		}
	}
	cancel, err := Wire(ok)
	if err != nil {
		t.Fatal(err)
	}
	if bus.event != DefaultEvent || bus.key != DefaultSubscriberKey || bus.handler == nil {
		t.Fatalf("%+v", bus)
	}
	cancel()
	if len(bus.unsub) != 1 || bus.unsub[0] != DefaultSubscriberKey {
		t.Fatal("cancel must unsubscribe")
	}
	ok.Event, ok.SubscriberKey = "x.Message.created", "mine"
	if _, err := Wire(ok); err != nil || bus.event != "x.Message.created" || bus.key != "mine" {
		t.Fatal("overrides ignored")
	}
}

func TestUUIDFromAny(t *testing.T) {
	id := uuid.New()
	for _, v := range []any{id, id.String()} {
		if got, err := uuidFromAny(v); err != nil || got != id {
			t.Fatalf("%v %v", got, err)
		}
	}
	for _, v := range []any{uuid.Nil, "", "nope", 5, nil} {
		if _, err := uuidFromAny(v); err == nil {
			t.Fatalf("%v must fail", v)
		}
	}
}

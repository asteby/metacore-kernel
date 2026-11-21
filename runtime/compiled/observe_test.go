package compiled

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/nativekit/observe"
)

func TestDispatcherObservesEveryCall(t *testing.T) {
	var got []observe.Sample
	obs := observe.New(func(_ context.Context, s observe.Sample) { got = append(got, s) })
	org := uuid.New()
	inv := &fakeInvoker{out: `{"success":true}`}
	d := New(inv, nil)
	d.Observe(obs)

	if _, err := d.Call(context.Background(), nil, org, "demo", "compiled:Post", nil); err != nil {
		t.Fatal(err)
	}
	inv.out = `{"success":false,"error":{"code":"x","message":"no"}}`
	if _, err := d.Call(context.Background(), nil, org, "demo", "Post", nil); err != nil {
		t.Fatal(err)
	}
	inv.out, inv.err = "", errors.New("boom")
	_, _ = d.CallRaw(context.Background(), nil, org, "demo", "Post", nil)
	d2 := New(inv, func(context.Context, uuid.UUID, string) (Installation, error) { return Installation{}, ErrNotInstalled })
	d2.Observe(obs)
	_, _ = d2.CallRaw(context.Background(), nil, org, "demo", "Post", nil)

	want := []observe.Outcome{observe.OutcomeOK, observe.OutcomeDeclined, observe.OutcomeError, observe.OutcomeNotInstalled}
	if len(got) != len(want) {
		t.Fatalf("samples = %d want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Outcome != w || got[i].Module != "demo" || got[i].Action != "Post" || got[i].Kind != observe.KindCompiled || got[i].Org != org {
			t.Errorf("sample %d = %+v want outcome %s", i, got[i], w)
		}
	}
}

func TestDispatcherWithoutObserverIsUnchanged(t *testing.T) {
	d := New(&fakeInvoker{out: `{"success":true}`}, nil)
	if _, err := d.Call(context.Background(), nil, uuid.New(), "demo", "Post", nil); err != nil {
		t.Fatal(err)
	}
	var nilD *Dispatcher
	nilD.Observe(nil) // must not panic
}

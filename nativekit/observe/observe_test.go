package observe_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/asteby/metacore-kernel/nativekit"
	"github.com/asteby/metacore-kernel/nativekit/observe"
)

func TestClassify(t *testing.T) {
	cases := map[observe.Outcome]error{
		observe.OutcomeOK:           nil,
		observe.OutcomeError:        errors.New("db down"),
		observe.OutcomeDeclined:     fmt.Errorf("w: %w", nativekit.E("stock.negative", "no")),
		observe.OutcomeInvalid:      nativekit.E(nativekit.CodeInvalid, "bad"),
		observe.OutcomeForbidden:    nativekit.E(nativekit.CodeForbidden, "no"),
		observe.OutcomeNotInstalled: fmt.Errorf("w: %w", observe.ErrNotInstalled),
	}
	for want, err := range cases {
		if got := observe.Classify(err); got != want {
			t.Errorf("%v -> %s want %s", err, got, want)
		}
	}
	if observe.Classify(nativekit.E(nativekit.CodeInternal, "x")) != observe.OutcomeError {
		t.Error("internal is a system error")
	}
}

func TestNilObserverIsNoOp(t *testing.T) {
	var o *observe.Observer
	v, err := observe.Track(context.Background(), o, observe.KindNative, "m", "a", func() (int, error) { return 7, nil })
	if v != 7 || err != nil {
		t.Fatal(v, err)
	}
	o.Add(func(context.Context, observe.Sample) {})
}

func TestSinkPanicDoesNotBreakCallerOrOtherSinks(t *testing.T) {
	n := 0
	o := observe.New(func(context.Context, observe.Sample) { panic("bad sink") }, func(context.Context, observe.Sample) { n++ })
	if _, err := observe.Track(context.Background(), o, observe.KindNative, "m", "a", func() (int, error) { return 0, nil }); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("later sinks must still run")
	}
}

func TestPrometheusSinkStableNamesAndLowCardinality(t *testing.T) {
	reg := prometheus.NewRegistry()
	o := observe.New(observe.PrometheusSink(reg), observe.PrometheusSink(reg)) // twice: tolerated
	for i := 0; i < 3; i++ {
		_, _ = observe.Track(context.Background(), o, observe.KindNative, "wallet", "topup", func() (int, error) { return 0, nil })
	}
	sp := o.Start(observe.KindNative, "wallet", "spend").With(uuid.New(), "user-1")
	sp.End(context.Background(), nativekit.E("ledger.insufficient", "no"))

	exp := `
# HELP metacore_action_total Invocations of compiled actions and native module operations, by kind, module, action and outcome.
# TYPE metacore_action_total counter
metacore_action_total{action="spend",kind="native",module="wallet",outcome="declined"} 2
metacore_action_total{action="topup",kind="native",module="wallet",outcome="ok"} 6
`
	// Registered twice means both sinks feed the same collectors: counts double.
	if err := testutil.GatherAndCompare(reg, strings.NewReader(exp), "metacore_action_total"); err != nil {
		t.Fatal(err)
	}
}

func TestAuditSinkFiltersAndForwardsWithoutLabelsLeak(t *testing.T) {
	var recs []observe.AuditRecord
	org := uuid.New()
	o := observe.New(observe.AuditSink(
		func(s observe.Sample) bool { return s.Action != "read" },
		func(_ context.Context, r observe.AuditRecord) { recs = append(recs, r) }))
	o.Start(observe.KindCompiled, "demo", "read").End(context.Background(), nil)
	o.Start(observe.KindCompiled, "demo", "post").With(org, "u1").End(context.Background(), nil)
	if len(recs) != 1 || recs[0].Org != org || recs[0].Actor != "u1" || recs[0].Action != "post" || recs[0].Outcome != observe.OutcomeOK {
		t.Fatalf("records %+v", recs)
	}
}

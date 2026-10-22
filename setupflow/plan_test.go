package setupflow

import (
	"fmt"
	"testing"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

func mailFlow() v3.SetupFlow {
	return v3.SetupFlow{Key: "main", Title: "t", Steps: []v3.SetupStep{
		{Key: "node", Kind: "check"},
		{Key: "settings", Kind: "form", DependsOn: []string{"node"}},
		{Key: "cloudflare", Kind: "form", Optional: true, DependsOn: []string{"settings"},
			SkipWhen: &v3.SetupSkipWhen{Setting: "dns_mode", Equals: "manual"}},
		{Key: "records", Kind: "records", DependsOn: []string{"settings"}},
		{Key: "apply", Kind: "action", Optional: true, DependsOn: []string{"cloudflare", "records"},
			SkipWhen: &v3.SetupSkipWhen{Setting: "dns_mode", In: []string{"manual", "none"}}},
		{Key: "dns_check", Kind: "check", DependsOn: []string{"records"}},
	}}
}

func settings(m map[string]string) SettingLookup {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func states(p FlowPlan) string {
	s := ""
	for _, st := range p.Steps {
		s += fmt.Sprintf("%s=%s ", st.Key, st.State)
	}
	return s
}

func TestPlan_FreshFlow(t *testing.T) {
	p := Plan(mailFlow(), nil, settings(nil))
	if fmt.Sprint(p.Next) != "[node]" {
		t.Fatalf("next = %v (%s)", p.Next, states(p))
	}
	if p.Steps[1].State != StepWaiting || fmt.Sprint(p.Steps[1].WaitingOn) != "[node]" {
		t.Fatalf("settings must wait on node: %+v", p.Steps[1])
	}
	if p.RequiredTotal != 4 || p.RequiredDone != 0 || p.Complete || p.Percent() != 0 {
		t.Fatalf("progress = %+v", p)
	}
}

func TestPlan_SkipAndOptionalSemantics(t *testing.T) {
	st := map[string]Status{"node": StatusOK, "settings": StatusOK}
	p := Plan(mailFlow(), st, settings(map[string]string{"dns_mode": "manual"}))
	// cloudflare + apply are skipped (manual), records and dns_check become reachable.
	if p.Steps[2].State != StepSkipped || p.Steps[4].State != StepSkipped {
		t.Fatalf("skip_when ignored: %s", states(p))
	}
	if fmt.Sprint(p.Next) != "[records dns_check]" && fmt.Sprint(p.Next) != "[records]" {
		t.Fatalf("next = %v", p.Next)
	}
	// dns_check depends on records (required): waiting until records done.
	if p.Steps[5].State != StepWaiting {
		t.Fatalf("dns_check should wait on records: %s", states(p))
	}
	st["records"] = StatusOK
	st["dns_check"] = StatusPending
	p = Plan(mailFlow(), st, settings(map[string]string{"dns_mode": "manual"}))
	if p.Steps[5].State != StepInProgress || p.Complete {
		t.Fatalf("pending dns_check must be in progress: %s", states(p))
	}
	st["dns_check"] = StatusWarn
	p = Plan(mailFlow(), st, settings(map[string]string{"dns_mode": "manual"}))
	if !p.Complete || p.Percent() != 100 || len(p.Next) != 0 {
		t.Fatalf("warn counts as done, flow complete: %s %+v", states(p), p)
	}
}

func TestPlan_OptionalNeverBlocks(t *testing.T) {
	st := map[string]Status{"node": StatusOK, "settings": StatusOK, "records": StatusOK}
	// dns_mode=cloudflare: cloudflare (optional, not skipped) is unfinished, yet
	// apply (depends on it) is not blocked by it, and the flow can complete.
	p := Plan(mailFlow(), st, settings(map[string]string{"dns_mode": "cloudflare"}))
	if p.Steps[2].State != StepReady || p.Steps[4].State != StepReady {
		t.Fatalf("optional steps must be offered, not block: %s", states(p))
	}
	st["dns_check"] = StatusOK
	p = Plan(mailFlow(), st, settings(map[string]string{"dns_mode": "cloudflare"}))
	if !p.Complete {
		t.Fatalf("optional steps must not block completion: %s", states(p))
	}
}

func TestPlan_FailedStepStaysActionable(t *testing.T) {
	st := map[string]Status{"node": StatusError}
	p := Plan(mailFlow(), st, nil)
	if p.Steps[0].State != StepFailed || fmt.Sprint(p.Next) != "[node]" {
		t.Fatalf("failed step must be offered again: %s next=%v", states(p), p.Next)
	}
	if p.Steps[1].State != StepWaiting {
		t.Fatalf("dependents wait on a failed step: %s", states(p))
	}
}

func TestSkipMatches(t *testing.T) {
	look := settings(map[string]string{"a": "x", "empty": ""})
	cases := []struct {
		s    *v3.SetupSkipWhen
		want bool
	}{
		{nil, false},
		{&v3.SetupSkipWhen{Setting: "a", Equals: "x"}, true},
		{&v3.SetupSkipWhen{Setting: "a", Equals: "y"}, false},
		{&v3.SetupSkipWhen{Setting: "a", In: []string{"y", "x"}}, true},
		{&v3.SetupSkipWhen{Setting: "a", In: []string{"y"}}, false},
		{&v3.SetupSkipWhen{Setting: "missing", Equals: "x"}, false},
		{&v3.SetupSkipWhen{Setting: "empty"}, false},
	}
	for i, c := range cases {
		if got := SkipMatches(c.s, look); got != c.want {
			t.Errorf("case %d = %v", i, got)
		}
	}
	if SkipMatches(&v3.SetupSkipWhen{Setting: "a", Equals: "x"}, nil) {
		t.Error("nil lookup never skips")
	}
}

func TestPlan_TerminatesOnCyclicInput(t *testing.T) {
	f := v3.SetupFlow{Key: "m", Title: "t", Steps: []v3.SetupStep{
		{Key: "a", DependsOn: []string{"b"}}, {Key: "b", DependsOn: []string{"a"}},
	}}
	p := Plan(f, nil, nil) // must not hang; both wait on each other
	if len(p.Next) != 0 || p.Complete {
		t.Fatalf("cyclic flow: %+v", p)
	}
}

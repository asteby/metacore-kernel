package setfields

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 11, 8, 12, 0, 0, 0, time.FixedZone("x", 3600))

func retireSpec() Spec {
	return Spec{
		StateField:    "status",
		RequiresState: []string{"active"},
		Set: map[string]any{
			"status":         "retired",
			"retired_at":     "$now",
			"retired_by_id":  "$actor.id",
			"retired_reason": "$payload.reason",
			"copy_name":      "$record.name",
			"literal":        "$$money",
			"cleared":        nil,
			"count":          3,
		},
	}
}

func TestApply_ResolvesEveryTokenKind(t *testing.T) {
	in := Input{
		Record:  map[string]any{"status": "active", "name": "Tienda A", "cleared": "x"},
		Payload: map[string]any{"reason": "baja"},
		ActorID: "actor-1",
		Now:     fixedNow,
	}
	res, err := Apply(retireSpec(), in)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := map[string]any{
		"status":         "retired",
		"retired_at":     fixedNow.UTC(),
		"retired_by_id":  "actor-1",
		"retired_reason": "baja",
		"copy_name":      "Tienda A",
		"literal":        "$money",
		"cleared":        nil,
		"count":          3,
	}
	if !reflect.DeepEqual(res.Updates, want) {
		t.Fatalf("updates = %#v\nwant     %#v", res.Updates, want)
	}
	if res.Before["status"] != "active" || res.Before["cleared"] != "x" || res.After["status"] != "retired" {
		t.Fatalf("before/after wrong: %#v %#v", res.Before, res.After)
	}
	if got := res.Updates["retired_at"].(time.Time).Location(); got != time.UTC {
		t.Fatalf("$now must be UTC, got %v", got)
	}
}

func TestApply_RequiresStateFailsClosed(t *testing.T) {
	in := Input{Record: map[string]any{"status": "retired"}, Payload: map[string]any{"reason": "x"}, ActorID: "a", Now: fixedNow}
	_, err := Apply(retireSpec(), in)
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("want ErrInvalidState, got %v", err)
	}
	var se *StateError
	if !errors.As(err, &se) || se.Current != "retired" || se.Field != "status" {
		t.Fatalf("StateError detail wrong: %#v", se)
	}
	// A row with no state at all is not "active" either.
	if _, err := Apply(retireSpec(), Input{Record: map[string]any{}, ActorID: "a"}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("missing state must fail, got %v", err)
	}
}

func TestApply_NoGateWhenRequiresStateEmpty(t *testing.T) {
	spec := Spec{Set: map[string]any{"notes": "hi"}}
	if _, err := Apply(spec, Input{}); err != nil {
		t.Fatalf("no gate expected: %v", err)
	}
}

func TestResolve_Errors(t *testing.T) {
	cases := []struct {
		name string
		set  map[string]any
		in   Input
		want error
	}{
		{"empty", map[string]any{}, Input{}, ErrEmptySet},
		{"managed id", map[string]any{"id": "x"}, Input{}, ErrManagedColumn},
		{"managed org", map[string]any{"organization_id": "x"}, Input{}, ErrManagedColumn},
		{"managed audit", map[string]any{"updated_at": "$now"}, Input{}, ErrManagedColumn},
		{"unknown token", map[string]any{"a": "$wat"}, Input{}, ErrBadToken},
		{"object value", map[string]any{"a": map[string]any{"x": 1}}, Input{}, ErrBadToken},
		{"no actor", map[string]any{"a": "$actor.id"}, Input{}, ErrMissingValue},
		{"missing payload", map[string]any{"a": "$payload.reason"}, Input{Payload: map[string]any{}}, ErrMissingValue},
		{"null payload", map[string]any{"a": "$payload.reason"}, Input{Payload: map[string]any{"reason": nil}}, ErrMissingValue},
		{"managed spelling", map[string]any{"OrganizationID": "x"}, Input{}, ErrManagedColumn},
		{"missing record col", map[string]any{"a": "$record.name"}, Input{Record: map[string]any{}}, ErrMissingValue},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Resolve(c.set, c.in); !errors.Is(err, c.want) {
				t.Fatalf("want %v, got %v", c.want, err)
			}
		})
	}
}

func TestResolve_NowDefaultsToServerClock(t *testing.T) {
	before := time.Now().UTC().Add(-time.Second)
	out, err := Resolve(map[string]any{"at": "$now"}, Input{})
	if err != nil {
		t.Fatal(err)
	}
	if got := out["at"].(time.Time); got.Before(before) || got.Location() != time.UTC {
		t.Fatalf("bad default now: %v", got)
	}
}

func TestColumns_Sorted(t *testing.T) {
	got := Columns(map[string]any{"b": 1, "a": 2, "c": 3})
	if !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("got %v", got)
	}
}

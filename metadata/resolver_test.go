package metadata

import (
	"context"
	"errors"
	"testing"

	"github.com/asteby/metacore-kernel/modelbase"
)

// TestModelResolver_ResolvesModelNotInRegistry verifies that a model reachable
// ONLY through Config.ModelResolver (never registered in the global modelbase
// registry) is resolved by GetTable / GetModal.
func TestModelResolver_ResolvesModelNotInRegistry(t *testing.T) {
	const key = "resolver_only_model"

	// Sanity: the model must NOT be in the global registry, otherwise the
	// test would pass for the wrong reason.
	if _, ok := modelbase.Get(key); ok {
		t.Fatalf("precondition failed: %q unexpectedly present in modelbase registry", key)
	}

	want := &fakeModel{key: key, title: "Resolver Only"}
	svc := New(Config{
		CacheTTL: -1, // disable cache so each call exercises computeTable
		ModelResolver: func(modelKey string) (modelbase.ModelDefiner, bool) {
			if modelKey == key {
				return want, true
			}
			return nil, false
		},
	})

	table, err := svc.GetTable(context.Background(), key)
	if err != nil {
		t.Fatalf("GetTable via resolver: %v", err)
	}
	if table.Title != "Resolver Only" {
		t.Fatalf("GetTable title: got %q want %q", table.Title, "Resolver Only")
	}

	modal, err := svc.GetModal(context.Background(), key)
	if err != nil {
		t.Fatalf("GetModal via resolver: %v", err)
	}
	if modal.Title != "Resolver Only" {
		t.Fatalf("GetModal title: got %q want %q", modal.Title, "Resolver Only")
	}
}

// TestModelResolver_FallsBackToModelbase verifies that when the resolver
// returns ok==false the lookup falls through to modelbase.Get.
func TestModelResolver_FallsBackToModelbase(t *testing.T) {
	const registeredKey = "resolver_fallback_registered"

	modelbase.Register(registeredKey, func() modelbase.ModelDefiner {
		return &fakeModel{key: registeredKey, title: "From Registry"}
	})

	resolverCalls := 0
	svc := New(Config{
		CacheTTL: -1,
		ModelResolver: func(modelKey string) (modelbase.ModelDefiner, bool) {
			resolverCalls++
			return nil, false // never resolves; forces modelbase fallback
		},
	})

	table, err := svc.GetTable(context.Background(), registeredKey)
	if err != nil {
		t.Fatalf("GetTable fallback: %v", err)
	}
	if table.Title != "From Registry" {
		t.Fatalf("GetTable title: got %q want %q", table.Title, "From Registry")
	}
	if resolverCalls == 0 {
		t.Fatalf("resolver was never consulted; it must run before modelbase.Get")
	}
}

// TestModelResolver_NilPreservesHistoricalBehaviour verifies that without a
// ModelResolver the service is strictly modelbase-only: registered models
// resolve, unknown models return ErrModelNotFound.
func TestModelResolver_NilPreservesHistoricalBehaviour(t *testing.T) {
	const registeredKey = "resolver_nil_registered"

	modelbase.Register(registeredKey, func() modelbase.ModelDefiner {
		return &fakeModel{key: registeredKey, title: "No Resolver"}
	})

	svc := New(Config{CacheTTL: -1}) // ModelResolver left nil

	table, err := svc.GetTable(context.Background(), registeredKey)
	if err != nil {
		t.Fatalf("GetTable without resolver: %v", err)
	}
	if table.Title != "No Resolver" {
		t.Fatalf("GetTable title: got %q want %q", table.Title, "No Resolver")
	}

	if _, err := svc.GetTable(context.Background(), "definitely_unregistered_model"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("expected ErrModelNotFound for unknown model without resolver, got %v", err)
	}
}

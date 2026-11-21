package modelalias

import (
	"errors"
	"testing"
)

func TestNormalizeIgnoresCaseAndSeparators(t *testing.T) {
	for _, s := range []string{"branch-cash-desk", "branch_cash_desk", "BranchCashDesk", "Branch Cash Desk"} {
		if Normalize(s) != "branchcashdesk" {
			t.Errorf("%q -> %q", s, Normalize(s))
		}
	}
}

func TestResolveKeyTableAndAliases(t *testing.T) {
	x := New()
	if err := x.Add("BranchCashDesk", "branch_cash_desks", []string{"cash-desk", "caja"}); err != nil {
		t.Fatal(err)
	}
	if err := x.Add("Order", "orders", []string{"sale"}); err != nil {
		t.Fatal(err)
	}
	for slug, want := range map[string]string{
		"branch-cash-desk":  "BranchCashDesk",
		"branch_cash_desks": "BranchCashDesk",
		"BranchCashDesk":    "BranchCashDesk",
		"cash-desks":        "BranchCashDesk", // plural of an alias
		"Caja":              "BranchCashDesk",
		"order":             "Order", // singular of the table
		"sales":             "Order",
	} {
		if got, ok := x.Resolve(slug); !ok || got != want {
			t.Errorf("%q -> %q,%v want %q", slug, got, ok, want)
		}
	}
	if _, ok := x.Resolve("invoice"); ok {
		t.Error("unknown slug must not resolve")
	}
}

func TestPluralVariantsCoverIesAndEs(t *testing.T) {
	x := New()
	_ = x.Add("Category", "categories", nil)
	_ = x.Add("Address", "addresses", nil)
	for slug, want := range map[string]string{"category": "Category", "categories": "Category", "address": "Address", "addresses": "Address"} {
		if got, ok := x.Resolve(slug); !ok || got != want {
			t.Errorf("%q -> %q,%v", slug, got, ok)
		}
	}
}

func TestConflictIsRejectedAndLeavesIndexUntouched(t *testing.T) {
	x := New()
	_ = x.Add("Order", "orders", []string{"sale"})
	err := x.Add("Quote", "quotes", []string{"sale", "estimate"})
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Owner != "Order" || ce.Claimant != "Quote" {
		t.Fatalf("err = %v", err)
	}
	if _, ok := x.Resolve("estimate"); ok {
		t.Fatal("a rejected Add must not register any of its names")
	}
	// An alias may not shadow another model's key or table.
	if err := x.Add("Report", "reports", []string{"orders"}); err == nil {
		t.Fatal("alias equal to another model's table must conflict")
	}
}

func TestReAddReplacesPreviousNames(t *testing.T) {
	x := New()
	_ = x.Add("Order", "orders", []string{"old-name"})
	if err := x.Add("Order", "orders", []string{"new-name"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := x.Resolve("old-name"); ok {
		t.Fatal("stale alias survived an upgrade")
	}
	if k, ok := x.Resolve("new-name"); !ok || k != "Order" {
		t.Fatal("new alias missing")
	}
	x.Remove("Order")
	if _, ok := x.Resolve("orders"); ok {
		t.Fatal("Remove must forget every name")
	}
}

func TestAmbiguousVariantDoesNotGuess(t *testing.T) {
	x := New()
	_ = x.Add("A", "a_tbl", []string{"bus"})
	_ = x.Add("B", "b_tbl", []string{"bues"})
	if _, ok := x.Resolve("bu"); ok {
		t.Fatal("a slug whose variants point at two models must not resolve")
	}
	if k, ok := x.Resolve("bus"); !ok || k != "A" {
		t.Fatal("exact match must win")
	}
}

func TestCanonicalPrefersKnownSlug(t *testing.T) {
	x := New()
	_ = x.Add("Order", "orders", []string{"sale"})
	known := func(s string) bool { return s == "sale" } // a model literally keyed "sale" exists
	if got := Canonical("sale", known, x); got != "sale" {
		t.Fatalf("the canonical lookup must win, got %q", got)
	}
	if got := Canonical("sales", func(string) bool { return false }, x); got != "Order" {
		t.Fatalf("got %q", got)
	}
	if got := Canonical("nothing", func(string) bool { return false }, x); got != "nothing" {
		t.Fatal("unknown slug stays untouched")
	}
	if got := Canonical("x", nil, nil); got != "x" {
		t.Fatal("nil resolver is a no-op")
	}
}

package manifest

import (
	"testing"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

func TestFromV3_CarriesSnapshotsAndGrant(t *testing.T) {
	snaps := mapModelSnapshots([]v3.Snapshot{{From: "product_id", Fields: map[string]string{"min_price": "min_price_at_sale"}}})
	if len(snaps) != 1 || snaps[0].From != "product_id" || snaps[0].Fields["min_price"] != "min_price_at_sale" {
		t.Fatalf("snapshots = %+v", snaps)
	}
	if mapModelSnapshots(nil) != nil || mapColumnGrant(nil) != nil {
		t.Fatal("ausentes deben quedar nil")
	}
	g := mapColumnGrant(&v3.GrantBinding{Policy: "p", Sets: "flag", ContextEq: map[string]string{"k": "c"}, ContextGte: map[string]string{"a": "x - y"}})
	if g == nil || g.Policy != "p" || g.Sets != "flag" || g.ContextEq["k"] != "c" || g.ContextGte["a"] != "x - y" {
		t.Fatalf("grant = %+v", g)
	}
}

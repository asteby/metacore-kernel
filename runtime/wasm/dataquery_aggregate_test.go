package wasm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

type aggData struct {
	Rows      []map[string]any `json:"rows"`
	Truncated bool             `json:"truncated"`
}

func aggResult(t *testing.T, raw []byte) (aggData, dqrEnvelope) {
	t.Helper()
	env := unmarshalDataQuery(t, raw)
	var d aggData
	if env.Success {
		var wrap struct {
			Data aggData `json:"data"`
		}
		if err := json.Unmarshal(raw, &wrap); err != nil {
			t.Fatalf("data unmarshal: %v", err)
		}
		d = wrap.Data
	}
	return d, env
}

// Grouped aggregate: the host keeps the org scope and the soft-delete filter,
// applies the guest filters, casts sum/avg to a JSON number and asks for one
// extra group to detect truncation.
func TestDataQueryAggregate_GroupedOverdue(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()
	orgID := uuid.New()

	expectProbe(mock, `"stock"`, "id", "organization_id", "customer_id", "amount_due", "due_date", "deleted_at")
	mock.ExpectQuery(`SELECT "customer_id", CAST\(SUM\("amount_due"\) AS double precision\) AS "overdue", COUNT\(\*\) AS "n", MIN\("due_date"\) AS "oldest" ` +
		`FROM "stock" WHERE organization_id = \$1 AND "amount_due" > \$2 AND "due_date" < \$3 AND deleted_at IS NULL ` +
		`GROUP BY "customer_id" ORDER BY "overdue" DESC LIMIT 51`).
		WithArgs(orgID, int64(0), "2026-09-29").
		WillReturnRows(sqlmock.NewRows([]string{"customer_id", "overdue", "n", "oldest"}).
			AddRow("c1", 1500.5, int64(2), "2026-08-01").
			AddRow("c2", 99.0, int64(1), "2026-09-01"))

	inv := testInvocation(gdb, nil, orgID, stockReadEnforcer(), nil)
	out := executeDataQueryRecords(context.Background(), inv, []byte(`{
		"table": "stock",
		"where": {"amount_due": {"gt": 0}, "due_date": {"lt": "2026-09-29"}},
		"order_by": "overdue", "order_dir": "desc",
		"aggregate": {
			"group_by": ["customer_id"],
			"select": [
				{"fn": "sum", "col": "amount_due", "as": "overdue"},
				{"fn": "count", "as": "n"},
				{"fn": "min", "col": "due_date", "as": "oldest"}
			]
		}
	}`))
	d, env := aggResult(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if len(d.Rows) != 2 || d.Truncated {
		t.Fatalf("want 2 rows, not truncated; got %#v", d)
	}
	if d.Rows[0]["customer_id"] != "c1" || d.Rows[0]["overdue"] != 1500.5 || d.Rows[0]["n"] != float64(2) {
		t.Fatalf("unexpected first row %#v", d.Rows[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

// Without group_by the whole filtered set folds into one row.
func TestDataQueryAggregate_NoGroupBy(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()
	orgID := uuid.New()
	expectProbe(mock, `"stock"`, "id", "organization_id", "customer_id", "amount_due")
	mock.ExpectQuery(`SELECT CAST\(SUM\("amount_due"\) AS double precision\) AS "total" FROM "stock" WHERE organization_id = \$1 AND "customer_id" = \$2 LIMIT 51`).
		WithArgs(orgID, "c1").
		WillReturnRows(sqlmock.NewRows([]string{"total"}).AddRow(nil))

	inv := testInvocation(gdb, nil, orgID, stockReadEnforcer(), nil)
	d, env := aggResult(t, executeDataQueryRecords(context.Background(), inv, []byte(`{
		"table": "stock", "where": {"customer_id": "c1"},
		"aggregate": {"select": [{"fn": "sum", "col": "amount_due", "as": "total"}]}
	}`)))
	if !env.Success || len(d.Rows) != 1 || d.Rows[0]["total"] != nil {
		t.Fatalf("want one row with a null total (empty set), got %#v %#v", d, env.Error)
	}
}

// More groups than the limit: the surplus is dropped and truncated is set.
func TestDataQueryAggregate_Truncated(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()
	orgID := uuid.New()
	expectProbe(mock, `"stock"`, "id", "organization_id", "customer_id")
	mock.ExpectQuery(`GROUP BY "customer_id" LIMIT 3`).
		WillReturnRows(sqlmock.NewRows([]string{"customer_id", "n"}).
			AddRow("a", int64(1)).AddRow("b", int64(1)).AddRow("c", int64(1)))

	inv := testInvocation(gdb, nil, orgID, stockReadEnforcer(), nil)
	d, env := aggResult(t, executeDataQueryRecords(context.Background(), inv, []byte(`{
		"table": "stock", "limit": 2,
		"aggregate": {"group_by": ["customer_id"], "select": [{"fn": "count", "as": "n"}]}
	}`)))
	if !env.Success || len(d.Rows) != 2 || !d.Truncated {
		t.Fatalf("want 2 rows + truncated, got %#v %#v", d, env.Error)
	}
}

func TestDataQueryAggregate_RejectsBadRequests(t *testing.T) {
	cases := map[string]string{
		"unknown fn":          `{"table":"stock","aggregate":{"select":[{"fn":"median","col":"x","as":"m"}]}}`,
		"sum without col":     `{"table":"stock","aggregate":{"select":[{"fn":"sum","as":"m"}]}}`,
		"no select":           `{"table":"stock","aggregate":{"group_by":["a"]}}`,
		"injection in col":    `{"table":"stock","aggregate":{"select":[{"fn":"sum","col":"x); DROP TABLE t;--","as":"m"}]}}`,
		"injection in alias":  `{"table":"stock","aggregate":{"select":[{"fn":"count","as":"m\" FROM x;--"}]}}`,
		"host-managed col":    `{"table":"stock","aggregate":{"select":[{"fn":"count","col":"organization_id","as":"m"}]}}`,
		"group by deleted_at": `{"table":"stock","aggregate":{"group_by":["deleted_at"],"select":[{"fn":"count","as":"m"}]}}`,
		"alias collides":      `{"table":"stock","aggregate":{"group_by":["a"],"select":[{"fn":"count","as":"a"}]}}`,
		"too many groups":     `{"table":"stock","aggregate":{"group_by":["a","b","c","d"],"select":[{"fn":"count","as":"m"}]}}`,
		"order_by not output": `{"table":"stock","order_by":"zzz","aggregate":{"select":[{"fn":"count","as":"m"}]}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			gdb, mock, cleanup := newMockGorm(t)
			defer cleanup()
			inv := testInvocation(gdb, nil, uuid.New(), stockReadEnforcer(), nil)
			env := unmarshalDataQuery(t, executeDataQueryRecords(context.Background(), inv, []byte(body)))
			if env.Success || env.Error == nil || env.Error.Code != "invalid_request" {
				t.Fatalf("want invalid_request, got %#v", env)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("no DB work expected: %v", err)
			}
		})
	}
}

// The aggregate is gated by the same db:read grant as a plain read.
func TestDataQueryAggregate_NeedsReadGrant(t *testing.T) {
	gdb, _, cleanup := newMockGorm(t)
	defer cleanup()
	inv := testInvocation(gdb, nil, uuid.New(), stockReadEnforcer(), nil)
	env := unmarshalDataQuery(t, executeDataQueryRecords(context.Background(), inv, []byte(
		`{"table":"payments","aggregate":{"select":[{"fn":"count","as":"n"}]}}`)))
	if env.Success || env.Error == nil || env.Error.Code != "forbidden" || !strings.Contains(env.Error.Message, "payments") {
		t.Fatalf("want forbidden on payments, got %#v", env)
	}
}

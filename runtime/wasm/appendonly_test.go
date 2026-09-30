package wasm

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func ledgerOnly(table string) bool { return table == "stock_ledger" }

// An update or delete on an append-only table is refused before any DB work
// (no BEGIN is expected on the mock) with the stable `append_only` code.
func TestExecuteDataMutate_AppendOnlyRefusesUpdateDeleteInc(t *testing.T) {
	orgID := uuid.New()
	rowID := uuid.NewString()
	for name, body := range map[string]string{
		"update": `{"op":"update","table":"stock_ledger","model":"StockLedger","id":"` + rowID + `","data":{"note":"edit"}}`,
		"inc":    `{"op":"update","table":"stock_ledger","model":"StockLedger","id":"` + rowID + `","inc":{"quantity":1}}`,
		"delete": `{"op":"delete","table":"stock_ledger","model":"StockLedger","id":"` + rowID + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			gdb, mock, cleanup := newMockGorm(t)
			defer cleanup()
			bus, getEvents, _ := captureBus(t, "*")
			inv := testInvocation(gdb, bus, orgID, multiWriteEnforcer("stock_ledger"), nil)
			inv.appendOnly = ledgerOnly

			env := unmarshalMutate(t, executeDataMutate(context.Background(), inv, []byte(body)))
			if env.Success || env.Error == nil || env.Error.Code != "append_only" {
				t.Fatalf("want append_only refusal, got %#v", env)
			}
			if !strings.Contains(env.Error.Message, "append-only") {
				t.Fatalf("message should explain the ledger, got %q", env.Error.Message)
			}
			if len(getEvents()) != 0 {
				t.Fatal("a refused mutation must publish no event")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("no DB work expected: %v", err)
			}
		})
	}
}

// A batch that mixes a create with an update of the ledger is refused whole:
// nothing reaches the database.
func TestExecuteDataBatch_AppendOnlyRefusesWholeBatch(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()
	orgID := uuid.New()
	bus, _, _ := captureBus(t, "*")
	inv := testInvocation(gdb, bus, orgID, multiWriteEnforcer("stock_ledger", "stock"), nil)
	inv.appendOnly = ledgerOnly

	out := executeDataBatch(context.Background(), inv, []byte(`{"mutations":[
		{"op":"create","table":"stock","model":"Stock","data":{"quantity":1}},
		{"op":"update","table":"stock_ledger","model":"StockLedger","id":"`+uuid.NewString()+`","data":{"note":"x"}}
	]}`))
	env := unmarshalBatch(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "append_only" {
		t.Fatalf("want append_only refusal, got %s", out)
	}
	if !strings.Contains(env.Error.Message, "mutations[1]") {
		t.Fatalf("message should point at the offending mutation, got %q", env.Error.Message)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("no DB work expected: %v", err)
	}
}

// Creates on the ledger and writes to other tables are unaffected.
func TestRefuseAppendOnly_CreateAndOtherTablesPass(t *testing.T) {
	inv := &invocation{appendOnly: ledgerOnly}
	for _, req := range []*dataMutateRequest{
		{Op: "create", Table: "stock_ledger"},
		{Op: "update", Table: "stock"},
		{Op: "delete", Table: "stock"},
	} {
		if code, err := refuseAppendOnly(inv, req); err != nil || code != "" {
			t.Fatalf("%s %s: unexpected refusal %q %v", req.Op, req.Table, code, err)
		}
	}
	if code, err := refuseAppendOnly(&invocation{}, &dataMutateRequest{Op: "delete", Table: "stock_ledger"}); err != nil || code != "" {
		t.Fatalf("no resolver must not refuse: %q %v", code, err)
	}
}

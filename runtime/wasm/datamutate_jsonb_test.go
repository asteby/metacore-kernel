package wasm

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func TestDecodeDataMutateColsBindsJSONDocuments(t *testing.T) {
	id := uuid.New()
	got, err := decodeDataMutateCols(map[string]json.RawMessage{
		"payload": json.RawMessage(`{"lines":[{"sku":"X","qty":1}]}`),
		"tags":    json.RawMessage(`["a","b"]`),
		"ref":     json.RawMessage(`{"$uuid":"` + id.String() + `"}`),
		"n":       json.RawMessage(`3`),
	})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["payload"] != `{"lines":[{"sku":"X","qty":1}]}` || got["tags"] != `["a","b"]` {
		t.Fatalf("json documents must bind as their JSON text: %#v", got)
	}
	if got["ref"] != id || got["n"] != int64(3) {
		t.Fatalf("markers and scalars keep their typed decoding: %#v", got)
	}
	if _, err := decodeDataMutateCols(map[string]json.RawMessage{"ref": json.RawMessage(`{"$uuid":"nope"}`)}); err == nil {
		t.Fatal("a malformed marker must still be rejected")
	}
}

// End to end: a create with a jsonb object reaches the INSERT instead of
// dying as invalid_request (channel_orders dead deliveries).
func TestExecuteDataMutate_CreateWithJSONBObject(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	orgID := uuid.New()
	rowID := uuid.NewString()
	bus, _, _ := captureBus(t, "inventory.Stock.created")

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT \* FROM "stock" LIMIT 0`).WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id"}))
	mock.ExpectQuery(`INSERT INTO "stock" \("created_at", "id", "organization_id", "payload", "updated_at"\)`).
		WithArgs(sqlmock.AnyArg(), rowID, orgID, `{"a":[1,2]}`, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id"}).AddRow(rowID, orgID.String()))
	mock.ExpectCommit()

	inv := testInvocation(gdb, bus, orgID, stockWriteEnforcer(), nil)
	out := executeDataMutate(context.Background(), inv, []byte(`{
		"op": "create", "table": "stock", "model": "Stock", "id": "`+rowID+`",
		"data": {"payload": {"a":[1,2]}}
	}`))
	if env := unmarshalMutate(t, out); !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

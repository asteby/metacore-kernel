package batch_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing/fstest"

	"github.com/asteby/metacore-kernel/dataload/batch"
	"github.com/google/uuid"
)

// A producer writes its files and manifest.json last; the consumer declares a
// Contract and runs Process on every poll.
func Example() {
	body := []byte(`[{"id":1},{"id":2}]`)
	sum := sha256.Sum256(body)
	manifest, _ := json.Marshal(batch.Manifest{Files: map[string]string{"items.json": hex.EncodeToString(sum[:])}})
	dir := fstest.MapFS{
		"items.json":    {Data: body},
		"manifest.json": {Data: manifest},
	}

	contract := batch.Contract{Files: []batch.FileSpec{{
		Name: "items.json",
		Import: func(_ context.Context, f batch.File) (batch.FileReport, error) {
			var rows []map[string]any
			if err := json.Unmarshal(f.Body, &rows); err != nil {
				return batch.FileReport{}, err
			}
			return batch.FileReport{Counts: map[string]int{"items": len(rows)}}, nil
		},
	}}}
	ledger := batch.NewMemoryLedger()
	scope := batch.Scope{OrgID: uuid.New(), Kind: "items-export"}

	for i := 0; i < 2; i++ {
		res, err := batch.Process(context.Background(), dir, contract, ledger, scope)
		fmt.Println(res.Imported, res.Skipped, res.Counts["items"], err)
	}
	// Output:
	// true false 2 <nil>
	// false true 0 <nil>
}

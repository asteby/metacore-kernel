package sqlpoll

import (
	"context"
	"strings"
	"testing"
	"time"

	lb "github.com/asteby/metacore-kernel/legacybridge"
)

// Regresión (revisión r9): una llave de texto con collation no binaria
// (utf8mb4_general_ci en MariaDB; NOCASE aquí) ordenaba distinto que el
// desempate del cursor y, con un empate de updated_at en el borde del lote, el
// cursor se atascaba en silencio: duplicados para siempre y filas posteriores
// sin replicar. Con key_type "text" se ordena y compara en binario.
func codesSource(t *testing.T, keyType string) (*Source, *env) {
	t.Helper()
	db := legacyDB(t)
	exec(t, db, `CREATE TABLE codes (code TEXT COLLATE NOCASE PRIMARY KEY, name TEXT, updated_at DATETIME)`)
	for _, k := range []string{"a", "B", "c", "D", "e", "F"} {
		exec(t, db, `INSERT INTO codes VALUES (?, ?, ?)`, k, "n-"+k, stamp(1))
	}
	exec(t, db, `INSERT INTO codes VALUES ('g', 'posterior', ?)`, stamp(30))
	p := Profile{System: "7l", Streams: []Stream{{Table: "codes", Entity: "codes", Key: "code", KeyType: keyType, UpdatedAt: "updated_at",
		Columns: []Column{{From: "name", To: "name"}}}}}
	s, err := New(db, SQLite, p)
	if err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return t0.Add(time.Hour) }
	s.Settle = 5 * time.Second
	s.PageSize = 3
	return s, newEnv(t)
}

func TestSync_TextKeyWithCaseInsensitiveCollationDoesNotStall(t *testing.T) {
	s, e := codesSource(t, KeyText)
	ctx := context.Background()
	reps, err := s.Sync(ctx, org, e.store, e.ap)
	if err != nil || reps[0].Error != "" {
		t.Fatalf("sync: %v %+v", err, reps)
	}
	for _, k := range []string{"a", "B", "c", "D", "e", "F", "g"} {
		if _, ok := e.sink.get("codes", "7l:codes:"+k); !ok {
			t.Fatalf("no replicado: %s", k)
		}
	}
	if reps[0].Counts[lb.OutcomeApplied] != 7 || reps[0].Counts[lb.OutcomeDuplicate] != 0 {
		t.Fatalf("conteos = %+v", reps[0].Counts)
	}
	// Reconcile ordena igual y no ve diferencias.
	rep, err := s.Reconcile(ctx, org, "codes", e.store, func(b lb.Batch) error { _, err := e.ap.Ingest(ctx, org, b); return err })
	if err != nil || rep.Diffs() != 0 || rep.Scanned != 7 {
		t.Fatalf("reconcile = %+v %v", rep, err)
	}
}

func TestSync_TextKeyDeclaredAsIntegerReportsInsteadOfStalling(t *testing.T) {
	s, e := codesSource(t, "")
	reps, err := s.Sync(context.Background(), org, e.store, e.ap)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reps[0].Error, "key_type") {
		t.Fatalf("el error debe nombrar key_type: %+v", reps[0])
	}
	if cur, _ := e.store.GetCursor(context.Background(), org, "7l", "codes"); cur != "" {
		t.Fatalf("cursor = %s", cur)
	}
}

func TestProfile_RejectsUnknownKeyType(t *testing.T) {
	p := Profile{System: "7l", Streams: []Stream{{Table: "t", Entity: "t", KeyType: "uuid", Columns: []Column{{From: "a", To: "a"}}}}}
	if _, err := p.Normalize(); err == nil {
		t.Fatal("key_type desconocido debe rechazarse")
	}
}

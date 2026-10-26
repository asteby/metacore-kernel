package wasm

import (
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/guest"
	"github.com/google/uuid"
)

// El sobre que fabrica el host (sourceEnvelope) lo entiende guest.ClassifyHostError:
// ata el contrato host<->guest de DG1.1/DG1.4 sin una base de datos.
func TestGuestClassifiesHostSourceUnavailableEnvelope(t *testing.T) {
	a := &sourceAbsence{
		Source: "fiscal_documents", Reason: ReasonAddonNotInstalled, SQLState: sqlStateUndefinedTable,
		Info: SourceInfo{Owners: []string{"fiscal_mexico"}},
	}
	body := sourceUnavailableBody("relation \"x\" does not exist (SQLSTATE 42P01)", a, true, "")
	env := sourceEnvelope("customers", body, dataMutateMeta("customers", uuid.Nil, time.Now(), ""))
	out := guest.ClassifyHostError(env)
	if out.Err != nil || out.Absent == nil {
		t.Fatalf("outcome = %+v", out)
	}
	got := out.Absent
	if got.Reason != guest.ReasonAddonNotInstalled || got.Source != "fiscal_documents" || !got.Degradable ||
		got.SQLState != "42P01" || len(got.Owners) != 1 || got.Owners[0] != "fiscal_mexico" {
		t.Fatalf("absence = %+v", *got)
	}
	if guest.CodeSourceUnavailable != CodeSourceUnavailable {
		t.Fatal("código desalineado")
	}
	for _, r := range [][2]string{
		{guest.ReasonAddonNotInstalled, ReasonAddonNotInstalled}, {guest.ReasonAddonDisabled, ReasonAddonDisabled},
		{guest.ReasonTableMissing, ReasonTableMissing}, {guest.ReasonSchemaMismatch, ReasonSchemaMismatch},
	} {
		if r[0] != r[1] {
			t.Fatalf("razón desalineada: %v", r)
		}
	}
}

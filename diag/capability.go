package diag

import (
	"context"
	"errors"
	"time"

	"github.com/asteby/metacore-kernel/capability"
)

// OnCapabilityFailure convierte un fallo del Executor en diagnostico. Se cablea
// con Executor.OnFailure = rec.OnCapabilityFailure. Corre en la goroutine del
// Executor y no bloquea.
func (r *Recorder) OnCapabilityFailure(ctx context.Context, f capability.Failure) {
	if !r.Enabled() {
		return
	}
	// El runtime (wasm) ya registro este mismo fallo si el proveedor es un addon
	// wasm: no se duplica.
	if r.hostRecordedSince(f.Org, f.Provider, CorrelationFrom(ctx), r.opt.Now().Add(-f.Duration-time.Second)) {
		return
	}
	// Aqui no hay secretos conocidos por valor (el Executor no ve ajustes ni
	// cabeceras del guest): el saneado es solo por patron y allowlist; lo que se
	// guarda de un fallo de capacidad es codigo, operacion y rutas de campo.
	sc := NewScrubber()
	d := Diagnostic{
		Org: f.Org, AddonKey: f.Provider, Kind: KindCapability, Operation: f.Capability,
		CorrelationID: CorrelationFrom(ctx), Severity: SeverityError, Code: f.Code,
		Indeterminate: f.Indeterminate, Subject: SubjectFrom(ctx),
		Retryable: f.Code == "timeout",
	}
	if f.Code == "" || f.Code == "provider_error" {
		d.Code = "provider_error"
	}
	coded := f.Reported
	if coded == nil {
		_ = errors.As(f.Err, &coded)
	}
	if coded != nil {
		d.ProviderCode = []string{coded.Code}
		d.Props = ExtractPathsScrubbed(coded.Message, sc)
	}
	if f.Code == "canceled" {
		d.Severity = SeverityWarning
	}
	r.Record(d, sc)
}

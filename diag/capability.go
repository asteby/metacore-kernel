package diag

import (
	"context"
	"errors"

	"github.com/asteby/metacore-kernel/capability"
)

// OnCapabilityFailure convierte un fallo del Executor en diagnostico. Se cablea
// con Executor.OnFailure = rec.OnCapabilityFailure. Corre en la goroutine del
// Executor y no bloquea.
func (r *Recorder) OnCapabilityFailure(ctx context.Context, f capability.Failure) {
	if !r.Enabled() {
		return
	}
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
		d.Props = ExtractPaths(coded.Message)
	}
	if f.Code == "canceled" {
		d.Severity = SeverityWarning
	}
	r.Record(d, nil)
}

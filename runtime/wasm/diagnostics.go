package wasm

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/diag"
)

// faultError marks a runtime failure with the stable diagnostic code it maps
// to. It is transparent: Error() is the wrapped message verbatim and Unwrap
// keeps errors.Is/As (sys.ExitError, context errors…) working, so nothing that
// matches on the old errors changes.
type faultError struct {
	code string
	err  error
}

func (e *faultError) Error() string { return e.err.Error() }
func (e *faultError) Unwrap() error { return e.err }

// WithDiagnostics enables automatic diagnostics: every invocation that ends in
// failure (trap, timeout, busy pool, memory, panic, a {success:false} envelope)
// leaves a sanitized diag.Diagnostic through rec, with the failed outbound HTTP
// calls and the last log lines of the guest as context. The guest does nothing
// and the manifest does not change. nil (the default) disables it; a Recorder
// switched off with KERNEL_DIAGNOSTICS=off is inert.
func (h *Host) WithDiagnostics(rec *diag.Recorder) *Host {
	h.diag = rec
	return h
}

type diagBagKey struct{}

func diagBagFrom(ctx context.Context) *diag.Bag {
	b, _ := ctx.Value(diagBagKey{}).(*diag.Bag)
	return b
}

// beginDiag opens the capture bag for one invocation and returns the context
// carrying it plus the finisher to defer. With diagnostics off it returns the
// same ctx and a no-op finisher (rec != nil is the only extra cost).
func (h *Host) beginDiag(ctx context.Context, orgID uuid.UUID, addonKey, funcName string, settings map[string]string) (context.Context, func(out *[]byte, err *error, rec any)) {
	if !h.diag.Enabled() {
		return ctx, func(_ *[]byte, _ *error, rec any) {
			if rec != nil {
				panic(rec)
			}
		}
	}
	bag := diag.NewBag()
	bag.Scrubber.AddSettings(settings)
	if s := diag.SubjectFrom(ctx); s.Type != "" {
		bag.SetSubject(s.Type, s.ID)
	}
	ctx = context.WithValue(ctx, diagBagKey{}, bag)
	invID := diag.InvocationIDFrom(ctx)
	corr := diag.CorrelationFrom(ctx)
	if corr == "" {
		corr = invID
	}
	org := orgID
	if org == uuid.Nil {
		org = orgIDFrom(ctx)
	}
	return ctx, func(out *[]byte, errp *error, rec any) {
		func() {
			defer func() { _ = recover() }() // un fallo del diagnostico nunca toca al guest
			d := diag.Diagnostic{
				Org: org, AddonKey: addonKey, Operation: funcName, CorrelationID: corr, InvocationID: invID,
				Severity: diag.SeverityError,
			}
			switch {
			case rec != nil:
				d.Kind, d.Code, d.Severity = diag.KindRuntime, "panic", diag.SeverityCritical
			case *errp != nil:
				d.Kind = diag.KindRuntime
				d.Code, d.Severity = classifyRuntime(*errp)
			default:
				env, ok := diag.FromEnvelope(*out)
				if !ok {
					return
				}
				d = mergeEnvelope(d, env)
			}
			bag.Apply(&d)
			h.diag.Record(d, bag.Scrubber)
		}()
		if rec != nil {
			panic(rec)
		}
	}
}

func mergeEnvelope(d, env diag.Diagnostic) diag.Diagnostic {
	d.Kind, d.Code, d.Props = diag.KindEnvelope, env.Code, env.Props
	return d
}

// classifyRuntime maps a runtime error to a stable code and severity.
func classifyRuntime(err error) (string, diag.Severity) {
	var nl *AddonNotLoadedError
	if errors.As(err, &nl) {
		return "addon_not_loaded", diag.SeverityWarning
	}
	var f *faultError
	if errors.As(err, &f) {
		switch f.code {
		case "canceled":
			return "canceled", diag.SeverityWarning
		case "pool_busy":
			return "pool_busy", diag.SeverityError
		}
		return f.code, diag.SeverityError
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "not in backend.exports"), strings.Contains(msg, "missing from module"):
		return "export_missing", diag.SeverityError
	case errors.Is(err, context.Canceled):
		return "canceled", diag.SeverityWarning
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", diag.SeverityError
	case strings.Contains(msg, "module closed"):
		return "module_closed", diag.SeverityError
	}
	return "runtime_error", diag.SeverityError
}

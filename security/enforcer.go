package security

import (
	"fmt"
	"log"
	"os"
	"runtime"
	"sync/atomic"
)

// Mode controls how an Enforcer reacts to capability violations.
type Mode int

const (
	// ModeShadow logs violations but never blocks. This is the default
	// during rollout so we can observe real traffic before flipping enforce.
	ModeShadow Mode = iota
	// ModeEnforce returns errors on violations. Callers should map the error
	// to a 403 response.
	ModeEnforce
)

// String returns a human-readable name for logs.
func (m Mode) String() string {
	switch m {
	case ModeEnforce:
		return "enforce"
	default:
		return "shadow"
	}
}

// ModeFromEnv reads METACORE_ENFORCE — "1", "true", "yes" flip to enforce;
// anything else (including unset) stays in shadow.
func ModeFromEnv() Mode {
	v := os.Getenv("METACORE_ENFORCE")
	switch v {
	case "1", "true", "TRUE", "yes", "YES":
		return ModeEnforce
	default:
		return ModeShadow
	}
}

// Enforcer wraps a Capabilities resolver and applies per-call checks in
// either shadow or enforce mode. It is safe for concurrent use — Mode is
// stored atomically so operators can toggle it at runtime.
type Enforcer struct {
	mode atomic.Int32
	// LookupCapabilities returns the compiled capabilities for an addon. A
	// nil return means "unknown addon" and is treated as a violation.
	LookupCapabilities func(addonKey string) *Capabilities
	// Logger receives violation lines. Defaults to the standard logger.
	Logger *log.Logger
	// OnViolation, when set, is called for every violation regardless of mode.
	// Useful for wiring metrics (e.g. incrementing metacore.capability.violation).
	OnViolation func(addonKey, kind, target, caller string, err error)
}

// NewEnforcer builds an Enforcer defaulting to shadow mode unless the
// METACORE_ENFORCE env var is set.
func NewEnforcer(lookup func(addonKey string) *Capabilities) *Enforcer {
	e := &Enforcer{LookupCapabilities: lookup}
	e.SetMode(ModeFromEnv())
	return e
}

// SetMode switches mode atomically.
func (e *Enforcer) SetMode(m Mode) { e.mode.Store(int32(m)) }

// Mode returns the current mode.
func (e *Enforcer) Mode() Mode { return Mode(e.mode.Load()) }

// CheckCapability evaluates whether addonKey is allowed to perform
// (kind, target). Kinds follow manifest conventions:
//
//	"db:read", "db:write", "http:fetch", "event:emit", "event:subscribe"
//
// In ModeShadow a violation is logged and nil is returned. In ModeEnforce
// the violation is logged AND returned as an error — the caller turns it
// into a 403.
func (e *Enforcer) CheckCapability(addonKey, kind, target string) error {
	var err error
	if e.LookupCapabilities == nil {
		err = fmt.Errorf("enforcer: no capability resolver wired")
	} else {
		caps := e.LookupCapabilities(addonKey)
		if caps == nil {
			err = fmt.Errorf("addon %q not registered", addonKey)
		} else {
			switch kind {
			case "db:read":
				err = caps.CanReadModel(target)
			case "db:write":
				err = caps.CanWriteModel(target)
			case "http:fetch":
				err = caps.CanFetch(target)
			case "event:emit":
				err = caps.CanEmit(target)
			case "event:subscribe":
				err = caps.CanSubscribe(target)
			default:
				err = fmt.Errorf("unknown capability kind %q", kind)
			}
		}
	}
	if err == nil {
		return nil
	}

	caller := callerLocation(2)
	logger := e.Logger
	if logger == nil {
		logger = log.Default()
	}
	mode := e.Mode()
	logger.Printf("metacore.capability.violation mode=%s addon=%s kind=%s target=%s caller=%s err=%v",
		mode, addonKey, kind, target, caller, err)
	if e.OnViolation != nil {
		e.OnViolation(addonKey, kind, target, caller, err)
	}
	if mode == ModeEnforce {
		return err
	}
	return nil
}

// callerLocation returns "file:line" for the frame skip levels up the stack
// from this helper. Uses runtime.Caller so we don't depend on debug symbols.
func callerLocation(skip int) string {
	_, file, line, ok := runtime.Caller(skip + 1)
	if !ok {
		return "unknown"
	}
	return fmt.Sprintf("%s:%d", file, line)
}

package native

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Service is the fully resolved desired state passed to a host supervisor only
// after bundle signature, entitlement and compatibility validation succeed.
type Service struct {
	AddonKey       string
	Version        string
	InstallationID uuid.UUID
	OrganizationID uuid.UUID
	ArtifactRoot   string
	Spec           Spec
}

type State string

const (
	StatePending  State = "pending"
	StateStarting State = "starting"
	StateHealthy  State = "healthy"
	StateDegraded State = "degraded"
	StateStopped  State = "stopped"
	StateFailed   State = "failed"
)

type Status struct {
	State      State
	Revision   string
	Message    string
	ObservedAt time.Time
}

// Supervisor reconciles native services. Ensure and Stop must be idempotent;
// callers may replay them after crashes. Implementations must sandbox the
// process and must never use a shell to interpret Entrypoint or Args.
type Supervisor interface {
	Ensure(ctx context.Context, service Service) (Status, error)
	Status(ctx context.Context, service Service) (Status, error)
	Stop(ctx context.Context, service Service) error
}

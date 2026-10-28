package automation

import (
	"time"
)

// Policy bounds what automations may do in a host. Zero values take the
// defaults from DefaultPolicy.
type Policy struct {
	// Enabled turns the whole feature on. When false every mutation returns
	// ErrDisabled and the scheduler does nothing.
	Enabled bool
	// MaxPerUser caps live (non-archived) automations + tools per person.
	MaxPerUser int
	// MinInterval is the minimum time between two scheduled fires (cron,
	// interval) and the default cooldown of event triggers.
	MinInterval time.Duration
	// MaxConsecutiveFailures moves an automation to "error" (auto-pause).
	MaxConsecutiveFailures int
	// MaxTokensPerRun / MaxCostUSDPerRun cap a single run (spec limits can
	// only lower them).
	MaxTokensPerRun  int
	MaxCostUSDPerRun float64
	// DailyBudgetUSD caps model spend per organisation per UTC day across all
	// automations. 0 = no model spend allowed.
	DailyBudgetUSD float64
	// MaxRunsPerDay caps runs per automation per day.
	MaxRunsPerDay int
	// DefaultModel is the cheap model used by summarize steps.
	DefaultModel string
	// DefaultTimezone applies when a trigger has none.
	DefaultTimezone string
	// Channels the host can deliver through.
	Channels []string
	// AutoApprovePersonalReminders lets a trivial personal reminder requested
	// by its own beneficiary start active without a separate confirmation.
	AutoApprovePersonalReminders bool
	// AssistantName labels agent-assisted authorship ("Asistente").
	AssistantName string
	// MaxOnceHorizon bounds how far in the future a one-shot can be set.
	MaxOnceHorizon time.Duration
	// RunTimeout bounds a single run.
	RunTimeout time.Duration
	// EventLabels names the host's domain events in descriptions
	// ("support.ticket.opened" → "se abra un ticket").
	EventLabels map[string]string
}

// DefaultPolicy is conservative: 20 automations per person, 15 minutes minimum
// frequency, three failures before auto-pause, no model spend unless the host
// sets a budget.
func DefaultPolicy() Policy {
	return Policy{
		Enabled:                      true,
		MaxPerUser:                   20,
		MinInterval:                  15 * time.Minute,
		MaxConsecutiveFailures:       3,
		MaxTokensPerRun:              1500,
		MaxCostUSDPerRun:             0.02,
		DailyBudgetUSD:               0,
		MaxRunsPerDay:                48,
		DefaultModel:                 "",
		DefaultTimezone:              "UTC",
		Channels:                     []string{ChannelInApp, ChannelPush},
		AutoApprovePersonalReminders: true,
		AssistantName:                "Asistente",
		MaxOnceHorizon:               366 * 24 * time.Hour,
		RunTimeout:                   2 * time.Minute,
	}
}

func (p Policy) withDefaults() Policy {
	d := DefaultPolicy()
	if p.MaxPerUser <= 0 {
		p.MaxPerUser = d.MaxPerUser
	}
	if p.MinInterval <= 0 {
		p.MinInterval = d.MinInterval
	}
	if p.MaxConsecutiveFailures <= 0 {
		p.MaxConsecutiveFailures = d.MaxConsecutiveFailures
	}
	if p.MaxTokensPerRun <= 0 {
		p.MaxTokensPerRun = d.MaxTokensPerRun
	}
	if p.MaxCostUSDPerRun <= 0 {
		p.MaxCostUSDPerRun = d.MaxCostUSDPerRun
	}
	if p.MaxRunsPerDay <= 0 {
		p.MaxRunsPerDay = d.MaxRunsPerDay
	}
	if p.DefaultTimezone == "" {
		p.DefaultTimezone = d.DefaultTimezone
	}
	if len(p.Channels) == 0 {
		p.Channels = d.Channels
	}
	if p.AssistantName == "" {
		p.AssistantName = d.AssistantName
	}
	if p.MaxOnceHorizon <= 0 {
		p.MaxOnceHorizon = d.MaxOnceHorizon
	}
	if p.RunTimeout <= 0 {
		p.RunTimeout = d.RunTimeout
	}
	return p
}

func (p Policy) channelAllowed(ch string) bool {
	for _, c := range p.Channels {
		if c == ch {
			return true
		}
	}
	return false
}

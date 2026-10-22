package setupflow

import (
	"errors"
	"fmt"
	"regexp"
)

// HealthStatus is the overall health of an addon installation.
type HealthStatus string

const (
	HealthOK    HealthStatus = "ok"
	HealthWarn  HealthStatus = "warn"
	HealthError HealthStatus = "error"
	// HealthUnknown is assigned by the host when no probe result exists yet or
	// the probe itself failed; handlers must not return it.
	HealthUnknown HealthStatus = "unknown"
)

// Rank orders statuses by severity (unknown < ok < warn < error) so a host can
// fold several results into the worst one.
func (s HealthStatus) Rank() int {
	switch s {
	case HealthOK:
		return 1
	case HealthWarn:
		return 2
	case HealthError:
		return 3
	}
	return 0
}

// Metric is one numeric health indicator (queue depth, delivery rate).
type Metric struct {
	Key   string  `json:"key"`
	Value float64 `json:"value"`
	// Unit is optional display unit ("%", "msgs", "s").
	Unit string `json:"unit,omitempty"`
}

// Issue is one concrete problem behind a non-ok health.
type Issue struct {
	// Severity is warn or error.
	Severity   HealthStatus   `json:"severity"`
	MessageKey string         `json:"message_key"`
	Params     map[string]any `json:"params,omitempty"`
	Fix        *Fix           `json:"fix,omitempty"`
}

// HealthResult is what a contributions.health handler returns.
type HealthResult struct {
	Status     HealthStatus   `json:"status"`
	SummaryKey string         `json:"summary_key"`
	Params     map[string]any `json:"params,omitempty"`
	Metrics    []Metric       `json:"metrics,omitempty"`
	Issues     []Issue        `json:"issues,omitempty"`
}

var metricKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)

// Validate checks a health result a handler returned.
func (h HealthResult) Validate() error {
	switch h.Status {
	case HealthOK, HealthWarn, HealthError:
	default:
		return fmt.Errorf("status %q must be ok|warn|error", h.Status)
	}
	if !messageKeyRe.MatchString(h.SummaryKey) || len(h.SummaryKey) > MaxMessageKeyLen {
		return fmt.Errorf("summary_key %q is not a valid i18n key", h.SummaryKey)
	}
	if err := checkParams("params", h.Params); err != nil {
		return err
	}
	if len(h.Metrics) > MaxMetrics {
		return fmt.Errorf("at most %d metrics", MaxMetrics)
	}
	seen := map[string]struct{}{}
	for i, m := range h.Metrics {
		if !metricKeyRe.MatchString(m.Key) {
			return fmt.Errorf("metrics[%d].key %q must match %s", i, m.Key, metricKeyRe)
		}
		if _, dup := seen[m.Key]; dup {
			return fmt.Errorf("metrics[%d].key %q is duplicated", i, m.Key)
		}
		seen[m.Key] = struct{}{}
		if m.Value != m.Value || m.Value > 1e15 || m.Value < -1e15 {
			return fmt.Errorf("metrics[%d].value is not a finite number", i)
		}
	}
	if len(h.Issues) > MaxIssues {
		return fmt.Errorf("at most %d issues", MaxIssues)
	}
	worst := HealthOK
	for i, is := range h.Issues {
		if is.Severity != HealthWarn && is.Severity != HealthError {
			return fmt.Errorf("issues[%d].severity %q must be warn|error", i, is.Severity)
		}
		if !messageKeyRe.MatchString(is.MessageKey) || len(is.MessageKey) > MaxMessageKeyLen {
			return fmt.Errorf("issues[%d].message_key %q is not a valid i18n key", i, is.MessageKey)
		}
		if err := checkParams(fmt.Sprintf("issues[%d].params", i), is.Params); err != nil {
			return err
		}
		if is.Fix != nil {
			if err := is.Fix.Validate(); err != nil {
				return fmt.Errorf("issues[%d]: %w", i, err)
			}
		}
		if is.Severity.Rank() > worst.Rank() {
			worst = is.Severity
		}
	}
	if worst.Rank() > h.Status.Rank() {
		return errors.New("status is better than its worst issue: an error issue needs status error, a warn issue at least warn")
	}
	return nil
}

// ParseHealthResult decodes and validates a health handler response (envelope
// or bare HealthResult).
func ParseHealthResult(raw []byte) (HealthResult, error) {
	data, err := unwrap(raw)
	if err != nil {
		return HealthResult{}, err
	}
	var r HealthResult
	if err := decodeStrict(data, &r); err != nil {
		return HealthResult{}, fmt.Errorf("setupflow: invalid health result: %w", err)
	}
	if err := r.Validate(); err != nil {
		return HealthResult{}, fmt.Errorf("setupflow: invalid health result: %w", err)
	}
	return r, nil
}

// Worst folds several statuses into the most severe one (HealthUnknown when
// the list is empty).
func Worst(statuses ...HealthStatus) HealthStatus {
	out := HealthUnknown
	for _, s := range statuses {
		if s.Rank() > out.Rank() {
			out = s
		}
	}
	return out
}

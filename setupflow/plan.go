package setupflow

import (
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// StepState is the planner's view of one step.
type StepState string

const (
	// StepDone: a recorded status of ok or warn.
	StepDone StepState = "done"
	// StepSkipped: skip_when matched; counts as satisfied.
	StepSkipped StepState = "skipped"
	// StepFailed: the recorded status is error or blocked.
	StepFailed StepState = "failed"
	// StepInProgress: the recorded status is pending or running.
	StepInProgress StepState = "in_progress"
	// StepReady: not finished and every required dependency is satisfied.
	StepReady StepState = "ready"
	// StepWaiting: a required dependency is not satisfied yet.
	StepWaiting StepState = "waiting"
)

// StepPlan describes one step of a flow.
type StepPlan struct {
	Key      string
	State    StepState
	Optional bool
	// Status is the recorded status ("" when never run).
	Status Status
	// WaitingOn lists the unsatisfied required dependencies (StepWaiting only).
	WaitingOn []string
}

// FlowPlan is the planner's view of a whole flow.
type FlowPlan struct {
	Steps []StepPlan
	// Next lists the actionable step keys (ready, failed or in progress and
	// with satisfied dependencies), in declaration order.
	Next []string
	// RequiredTotal / RequiredDone count the non-optional, non-skipped steps.
	RequiredTotal int
	RequiredDone  int
	// Complete is true when every required step is done.
	Complete bool
}

// Percent is the completion of the required steps, 0..100 (100 when there are
// none).
func (p FlowPlan) Percent() int {
	if p.RequiredTotal == 0 {
		return 100
	}
	return p.RequiredDone * 100 / p.RequiredTotal
}

// SettingLookup returns the current value of an addon setting (as the string
// skip_when compares against) and whether it is set.
type SettingLookup func(key string) (string, bool)

// SkipMatches reports whether a step's skip_when holds for the current
// settings. A nil skip_when, or an unset setting, never skips.
func SkipMatches(s *v3.SetupSkipWhen, lookup SettingLookup) bool {
	if s == nil || lookup == nil {
		return false
	}
	v, ok := lookup(s.Setting)
	if !ok {
		return false
	}
	if len(s.In) > 0 {
		for _, c := range s.In {
			if c == v {
				return true
			}
		}
		return false
	}
	return s.Equals != "" && v == s.Equals
}

// Plan evaluates a flow. statuses maps step key -> the last recorded Status
// (form/action/records/custom steps get theirs from the host: e.g. ok once the
// required settings are saved). Rules:
//
//   - a step whose skip_when matches is skipped, whatever was recorded;
//   - optional steps never block their dependents and never block completion;
//   - skipped and done steps satisfy dependents;
//   - a step is ready when every non-optional dependency is satisfied, waiting
//     otherwise.
//
// The flow is assumed valid (manifest validation rejects cycles and unknown
// keys); Plan still terminates on any input.
func Plan(flow v3.SetupFlow, statuses map[string]Status, lookup SettingLookup) FlowPlan {
	byKey := make(map[string]int, len(flow.Steps))
	for i, s := range flow.Steps {
		byKey[s.Key] = i
	}
	states := make([]StepState, len(flow.Steps))
	for i, s := range flow.Steps {
		st := statuses[s.Key]
		switch {
		case SkipMatches(s.SkipWhen, lookup):
			states[i] = StepSkipped
		case st.Done():
			states[i] = StepDone
		case st == StatusError || st == StatusBlocked:
			states[i] = StepFailed
		case st == StatusPending || st == StatusRunning:
			states[i] = StepInProgress
		}
	}
	satisfied := func(key string) bool {
		i, ok := byKey[key]
		if !ok {
			return true // unknown refs are a manifest error, not a blocker
		}
		return flow.Steps[i].Optional || states[i] == StepDone || states[i] == StepSkipped
	}
	var plan FlowPlan
	for i, s := range flow.Steps {
		sp := StepPlan{Key: s.Key, Optional: s.Optional, Status: statuses[s.Key], State: states[i]}
		var waiting []string
		for _, d := range s.DependsOn {
			if !satisfied(d) {
				waiting = append(waiting, d)
			}
		}
		if states[i] != StepDone && states[i] != StepSkipped {
			if len(waiting) > 0 {
				sp.State = StepWaiting
				sp.WaitingOn = waiting
			} else {
				if sp.State == "" {
					sp.State = StepReady
				}
				plan.Next = append(plan.Next, s.Key)
			}
		}
		if !s.Optional && states[i] != StepSkipped {
			plan.RequiredTotal++
			if states[i] == StepDone {
				plan.RequiredDone++
			}
		}
		plan.Steps = append(plan.Steps, sp)
	}
	plan.Complete = plan.RequiredDone == plan.RequiredTotal
	return plan
}

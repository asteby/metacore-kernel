// Package aitest provides test doubles for the ai.complete host pipeline
// (capability.AIGate): a scripted provider, an in-memory usage meter and a
// static policy source. Hosts and addons use them to test their AI paths
// without a hub, Sparks or a database.
package aitest

import (
	"context"
	"fmt"
	"sync"

	"github.com/asteby/metacore-kernel/capability"
	"github.com/google/uuid"
)

// Provider is a scripted capability.AIProvider. Each call pops the next
// Result (the last one repeats); Calls records what reached it.
type Provider struct {
	mu      sync.Mutex
	Results []capability.AIResult
	Err     error
	Calls   []capability.AICall
}

// NewProvider answers every call with res.
func NewProvider(res ...capability.AIResult) *Provider { return &Provider{Results: res} }

// OK is a successful answer with content and cost.
func OK(content string, costSparks int64) capability.AIResult {
	return capability.AIResult{
		Status: capability.AIStatusOK, Content: content, CostSparks: costSparks,
		Usage: capability.AIUsage{InputTokens: 10, OutputTokens: 5}, Model: "test-model", RequestID: "req_test",
	}
}

// Complete implements capability.AIProvider.
func (p *Provider) Complete(_ context.Context, call capability.AICall) (capability.AIResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Calls = append(p.Calls, call)
	if p.Err != nil {
		return capability.AIResult{}, p.Err
	}
	if len(p.Results) == 0 {
		return OK("ok", 1), nil
	}
	res := p.Results[0]
	if len(p.Results) > 1 {
		p.Results = p.Results[1:]
	}
	return res, nil
}

// CallCount is how many calls reached the provider.
func (p *Provider) CallCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.Calls)
}

// Last is the last call that reached the provider.
func (p *Provider) Last() capability.AICall {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.Calls) == 0 {
		return capability.AICall{}
	}
	return p.Calls[len(p.Calls)-1]
}

// Meter is an in-memory capability.AIUsageMeter.
type Meter struct {
	mu      sync.Mutex
	spent   map[string]int64
	Records []capability.AIUsageRecord
	// Err, when set, fails Spent (to test the fail-closed path).
	Err error
}

// NewMeter builds an empty meter.
func NewMeter() *Meter { return &Meter{spent: map[string]int64{}} }

func key(org uuid.UUID, addon, feature, period string) string {
	return fmt.Sprintf("%s|%s|%s|%s", org, addon, feature, period)
}

// Add pre-loads spend (feature "" = the addon total only).
func (m *Meter) Add(org uuid.UUID, addon, feature, period string, sparks int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spent[key(org, addon, "", period)] += sparks
	if feature != "" {
		m.spent[key(org, addon, feature, period)] += sparks
	}
}

// Spent implements capability.AIUsageMeter.
func (m *Meter) Spent(_ context.Context, org uuid.UUID, addon, feature, period string) (int64, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return 0, 0, m.Err
	}
	return m.spent[key(org, addon, "", period)], m.spent[key(org, addon, feature, period)], nil
}

// Record implements capability.AIUsageMeter.
func (m *Meter) Record(_ context.Context, rec capability.AIUsageRecord) error {
	m.mu.Lock()
	m.Records = append(m.Records, rec)
	m.mu.Unlock()
	if rec.CostSparks > 0 {
		m.Add(rec.Org, rec.Addon, rec.Feature, rec.Period, rec.CostSparks)
	}
	return nil
}

// Policies is a static capability.AIPolicySource keyed by addon (any org).
type Policies map[string]capability.AIPolicy

// AIPolicy implements capability.AIPolicySource.
func (p Policies) AIPolicy(_ context.Context, _ uuid.UUID, addon string) (capability.AIPolicy, error) {
	pol, ok := p[addon]
	if !ok {
		return capability.AIPolicy{}, fmt.Errorf("%w: addon %q is not installed", capability.ErrAIForbidden, addon)
	}
	return pol, nil
}

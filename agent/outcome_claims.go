package agent

import (
	"regexp"
	"sync"

	"github.com/asteby/metacore-kernel/agent/tools"
)

// Hosts extend what the orchestrator treats as "claiming a tool's outcome"
// without touching the tool: another language for show_chart, or phrases
// specific to their product. Registered patterns add to the ones the tool
// declares itself (tools.OutcomeClaimer); they never replace them.
var (
	claimMu    sync.RWMutex
	extraClaim = map[string]map[string][]*regexp.Regexp{} // tool -> language -> patterns
)

// RegisterOutcomeClaims adds claim patterns for a tool under a language code
// (primary BCP-47 subtag: "es", "en", "pt", "fr"...). Safe for concurrent use;
// call it at startup. Nil patterns are ignored.
func RegisterOutcomeClaims(tool, lang string, patterns ...*regexp.Regexp) {
	claimMu.Lock()
	defer claimMu.Unlock()
	if extraClaim[tool] == nil {
		extraClaim[tool] = map[string][]*regexp.Regexp{}
	}
	for _, re := range patterns {
		if re != nil {
			extraClaim[tool][lang] = append(extraClaim[tool][lang], re)
		}
	}
}

// claimPatterns is every pattern, in every language, under which a reply claims
// the outcome of t: what the tool declares plus what the host registered. The
// conversation language is not known here, so a claim in any declared language
// counts; adding a language is data, not code.
func claimPatterns(t tools.BaseTool) []*regexp.Regexp {
	var out []*regexp.Regexp
	if c, ok := tools.As[tools.OutcomeClaimer](t); ok {
		for _, res := range c.OutcomeClaims() {
			out = append(out, res...)
		}
	}
	claimMu.RLock()
	defer claimMu.RUnlock()
	for _, res := range extraClaim[t.Name()] {
		out = append(out, res...)
	}
	return out
}

package notifications

import (
	"regexp"
	"strings"
)

// Render expands a template string against vars using a tiny subset of the
// Mustache spec: plain interpolation plus boolean sections.  It is intended
// for short notification messages, not full document templating, and never
// returns an error — malformed input is rendered as best-effort literal text.
//
// Supported syntax:
//
//   - {{var}}              → vars["var"]; absent or empty renders nothing.
//   - {{#var}}body{{/var}} → body (with vars expanded inside) when vars["var"]
//     is "truthy"; the whole block is removed otherwise.
//   - {{^var}}body{{/var}} → inverse: body is rendered only when vars["var"]
//     is "falsy".
//
// Truthy/falsy follow Mustache semantics adapted to map[string]string:
// the empty string and the literal "false" are falsy; everything else is
// truthy.  The map being absent of the key is also falsy.
//
// Sections may nest.  A close tag {{/x}} always pairs with the nearest
// matching unclosed open tag {{#x}} or {{^x}}, and an unmatched open tag
// is left in place so the bug is visible in the output instead of silently
// truncating the template.
//
// What is intentionally NOT supported (use a real Mustache library if you
// need it): partials, lambdas, dotted-name lookups, list iteration, custom
// delimiters, comments, HTML escaping.
func Render(tmpl string, vars map[string]string) string {
	if tmpl == "" {
		return ""
	}
	out := renderNode(parse(tmpl), vars)
	// Collapse runs of 3+ newlines (which sections frequently leave behind
	// when their bodies vanish) down to a single blank line so chat clients
	// don't render a wall of whitespace.
	out = multiNewlineRE.ReplaceAllString(out, "\n\n")
	return strings.TrimSpace(out)
}

var multiNewlineRE = regexp.MustCompile(`\n{3,}`)

// truthy mirrors Mustache's section semantics over our string-only var map.
// Empty string and the literal "false" are falsy; everything else truthy.
func truthy(v string, present bool) bool {
	if !present {
		return false
	}
	if v == "" || v == "false" {
		return false
	}
	return true
}

// node is a single template fragment.  Either a literal text run, a variable
// reference, or a section that owns a list of child nodes.
type node struct {
	kind     nodeKind
	text     string
	name     string
	inverted bool
	children []node
}

type nodeKind int

const (
	nodeText nodeKind = iota
	nodeVar
	nodeSection
)

// renderNode walks a parsed template and emits the final string.
func renderNode(nodes []node, vars map[string]string) string {
	var b strings.Builder
	for _, n := range nodes {
		switch n.kind {
		case nodeText:
			b.WriteString(n.text)
		case nodeVar:
			b.WriteString(vars[n.name])
		case nodeSection:
			val, ok := vars[n.name]
			show := truthy(val, ok)
			if n.inverted {
				show = !show
			}
			if show {
				b.WriteString(renderNode(n.children, vars))
			}
		}
	}
	return b.String()
}

// parse scans tmpl into a flat list of nodes (with sections holding their own
// children).  The grammar is tiny enough that a hand-rolled scanner is easier
// to audit than a regex soup; see template_test.go for the contract.
func parse(tmpl string) []node {
	nodes, _, _ := parseUntil(tmpl, 0, "")
	return nodes
}

// parseUntil consumes tmpl starting at pos until it either runs out of input
// or finds a closing tag {{/closeName}}.  Returns the parsed children, the
// index immediately past the closing tag (or len(tmpl) if EOF), and whether
// the close tag was actually found.  The closed flag is the only way a
// caller can distinguish "section closed cleanly at EOF" from "section never
// closed" since both produce pos == len(tmpl).
func parseUntil(tmpl string, pos int, closeName string) (out []node, next int, closed bool) {
	for pos < len(tmpl) {
		open := strings.Index(tmpl[pos:], "{{")
		if open == -1 {
			out = append(out, node{kind: nodeText, text: tmpl[pos:]})
			return out, len(tmpl), false
		}
		open += pos
		if open > pos {
			out = append(out, node{kind: nodeText, text: tmpl[pos:open]})
		}
		close := strings.Index(tmpl[open:], "}}")
		if close == -1 {
			// Unclosed tag: emit the rest as literal text so the bug is visible
			// in the output instead of being silently swallowed.
			out = append(out, node{kind: nodeText, text: tmpl[open:]})
			return out, len(tmpl), false
		}
		close += open
		raw := tmpl[open+2 : close]
		next = close + 2

		switch {
		case strings.HasPrefix(raw, "/"):
			name := strings.TrimSpace(raw[1:])
			if name == closeName {
				return out, next, true
			}
			// Stray close tag (no matching open at this nesting level): keep
			// it literal and move on.
			out = append(out, node{kind: nodeText, text: tmpl[open:next]})
		case strings.HasPrefix(raw, "#") || strings.HasPrefix(raw, "^"):
			inverted := raw[0] == '^'
			name := strings.TrimSpace(raw[1:])
			children, after, found := parseUntil(tmpl, next, name)
			if !found {
				// No matching {{/name}} found.  Render the open tag literal so
				// the missing close is obvious in the output, then fold the
				// already-parsed children into the current level so their
				// content (and any vars) still appears.
				out = append(out, node{kind: nodeText, text: tmpl[open:next]})
				out = append(out, children...)
				return out, len(tmpl), false
			}
			out = append(out, node{
				kind:     nodeSection,
				name:     name,
				inverted: inverted,
				children: children,
			})
			pos = after
			continue
		default:
			out = append(out, node{kind: nodeVar, name: strings.TrimSpace(raw)})
		}
		pos = next
	}
	return out, pos, closeName == ""
}

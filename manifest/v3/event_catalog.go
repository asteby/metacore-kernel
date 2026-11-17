package v3

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Event catalog (RFC 0003 §2.4 / §4). An addon declares the events it emits
// (and optionally the ones it consumes) in the top-level, optional `events`
// block. The declaration is DATA for the integrations panel: it lets an
// operator pick an event grouped by addon and lets the UI build a mapping
// editor from payload_schema. It grants no capability and runs no code: the
// bus keeps its own rules (docs/events-bus.md).

// Sensitivity classifies the personal-data exposure of an event payload so the
// host can warn before routing it outside the platform.
type Sensitivity string

const (
	SensitivityPublic   Sensitivity = "public"
	SensitivityInternal Sensitivity = "internal"
	SensitivityPersonal Sensitivity = "personal"
)

// Bounds applied at load time. They keep a catalog entry small enough to ship
// to the browser on every panel open.
const (
	MaxEventsPerAddon        = 200
	MaxEventPayloadSchemaLen = 16 << 10 // serialized bytes
	MaxEventSchemaDepth      = 8
	MaxEventSchemaNodes      = 500
)

// EventsDecl is the manifest `events` block.
type EventsDecl struct {
	// Emits are the events this addon publishes on the bus.
	Emits []EmittedEvent `json:"emits,omitempty"`
	// Consumes are the events this addon reacts to (informative).
	Consumes []ConsumedEvent `json:"consumes,omitempty"`
}

// EmittedEvent declares one event an addon emits.
type EmittedEvent struct {
	// Name is "<domain>.<entity>.<action>", e.g. "sales.order.created".
	Name string `json:"name"`
	// Description is an i18n key (resolved by the host catalog), not prose.
	Description string `json:"description,omitempty"`
	// Aggregate is the entity the event sequence belongs to (snake_case).
	Aggregate string `json:"aggregate,omitempty"`
	// PayloadSchema is a bounded JSON Schema of the event payload. Only local
	// "#/..." references are allowed.
	PayloadSchema json.RawMessage `json:"payload_schema,omitempty"`
	// Sensitivity defaults to "internal" when omitted.
	Sensitivity Sensitivity `json:"sensitivity,omitempty"`
	// Since is the addon version (semver) that started emitting it.
	Since string `json:"since,omitempty"`
}

// ConsumedEvent declares one event an addon reacts to.
type ConsumedEvent struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

var (
	// Names exactly as the bus publishes them (no mapping): 1 to 3 dot segments,
	// first/last lowercase snake, the middle one (only with 3) PascalCase or snake.
	eventNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(.([A-Za-z][A-Za-z0-9_]*.)?[a-z][a-z0-9_]*)?$`)
	eventIDRe   = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	eventI18nRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,199}$`)
	eventVerRe  = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)
)

// validateEvents enforces what the JSON schema cannot express.
func validateEvents(m *Manifest, warnOnly func(rule, msg string)) []string {
	if m.Events == nil {
		return nil
	}
	var errs []string
	e := m.Events
	if len(e.Emits) > MaxEventsPerAddon {
		errs = append(errs, fmt.Sprintf("events.emits has %d entries; the maximum is %d", len(e.Emits), MaxEventsPerAddon))
	}
	seen := make(map[string]struct{}, len(e.Emits))
	for i, ev := range e.Emits {
		where := fmt.Sprintf("events.emits[%d]", i)
		if ev.Name != "" {
			where = fmt.Sprintf("events.emits[%s]", ev.Name)
		}
		if !eventNameRe.MatchString(ev.Name) {
			errs = append(errs, fmt.Sprintf("%s.name %q is not a published event name (forms: event, domain.event, addon.Model.action; first and last segment lowercase snake_case)", where, ev.Name))
		} else if _, dup := seen[ev.Name]; dup {
			errs = append(errs, fmt.Sprintf("%s.name is duplicated within the addon", where))
		} else {
			seen[ev.Name] = struct{}{}
			if owner, crud := isCRUDEventName(ev.Name); crud {
				if owner != m.Metadata.Key {
					errs = append(errs, fmt.Sprintf("%s.name %q is a canonical CRUD event of addon %q; an addon can only declare its own (%q)", where, ev.Name, owner, m.Metadata.Key))
				}
			} else if !eventCovered(m.Capabilities, ev.Name) {
				warnOnly(RuleEventEmitCapability, fmt.Sprintf("%s.name %q is not covered by an event:emit capability of this addon; its guest could not publish it", where, ev.Name))
			}
		}
		if ev.Description != "" && !eventI18nRe.MatchString(ev.Description) {
			errs = append(errs, fmt.Sprintf("%s.description %q must be an i18n key, not prose", where, ev.Description))
		}
		if ev.Aggregate != "" && !eventIDRe.MatchString(ev.Aggregate) {
			errs = append(errs, fmt.Sprintf("%s.aggregate %q must be snake_case", where, ev.Aggregate))
		}
		switch ev.Sensitivity {
		case "", SensitivityPublic, SensitivityInternal, SensitivityPersonal:
		default:
			errs = append(errs, fmt.Sprintf("%s.sensitivity %q is not one of public|internal|personal", where, ev.Sensitivity))
		}
		if ev.Since != "" && !eventVerRe.MatchString(ev.Since) {
			errs = append(errs, fmt.Sprintf("%s.since %q must be a semver version", where, ev.Since))
		}
		if len(ev.PayloadSchema) > 0 {
			errs = append(errs, validateEventPayloadSchema(where+".payload_schema", ev.PayloadSchema)...)
		}
	}
	seenC := make(map[string]struct{}, len(e.Consumes))
	for i, ev := range e.Consumes {
		where := fmt.Sprintf("events.consumes[%d]", i)
		if !eventNameRe.MatchString(ev.Name) {
			errs = append(errs, fmt.Sprintf("%s.name %q is not a published event name", where, ev.Name))
		} else if _, dup := seenC[ev.Name]; dup {
			errs = append(errs, fmt.Sprintf("%s.name %q is duplicated", where, ev.Name))
		} else {
			seenC[ev.Name] = struct{}{}
		}
		if ev.Description != "" && !eventI18nRe.MatchString(ev.Description) {
			errs = append(errs, fmt.Sprintf("%s.description %q must be an i18n key, not prose", where, ev.Description))
		}
	}
	return errs
}

// validateEventPayloadSchema bounds a payload schema: size, depth, node count,
// object root, and no reference that leaves the document ($ref must be a local
// "#/..." pointer; $id, $anchor, $dynamicRef and $recursiveRef are rejected).
func validateEventPayloadSchema(where string, raw json.RawMessage) []string {
	if len(raw) > MaxEventPayloadSchemaLen {
		return []string{fmt.Sprintf("%s is %d bytes; the maximum is %d", where, len(raw), MaxEventPayloadSchemaLen)}
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return []string{fmt.Sprintf("%s is not valid JSON: %v", where, err)}
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return []string{fmt.Sprintf("%s must be a JSON Schema object", where)}
	}
	if t, ok := root["type"].(string); ok && t != "object" {
		return []string{fmt.Sprintf("%s root type must be \"object\" (the payload), got %q", where, t)}
	}
	var errs []string
	nodes := 0
	var walk func(v any, depth int)
	walk = func(v any, depth int) {
		if len(errs) > 0 {
			return
		}
		nodes++
		if nodes > MaxEventSchemaNodes {
			errs = append(errs, fmt.Sprintf("%s has more than %d nodes", where, MaxEventSchemaNodes))
			return
		}
		if depth > MaxEventSchemaDepth {
			errs = append(errs, fmt.Sprintf("%s is nested deeper than %d levels", where, MaxEventSchemaDepth))
			return
		}
		switch t := v.(type) {
		case map[string]any:
			for k, c := range t {
				switch k {
				case "$ref":
					if s, _ := c.(string); !strings.HasPrefix(s, "#/") {
						errs = append(errs, fmt.Sprintf("%s: $ref %v must be a local \"#/...\" pointer (no remote references)", where, c))
						return
					}
				case "$id", "$dynamicRef", "$recursiveRef", "$anchor":
					errs = append(errs, fmt.Sprintf("%s: %q is not allowed", where, k))
					return
				}
				walk(c, depth+1)
			}
		case []any:
			for _, c := range t {
				walk(c, depth+1)
			}
		}
	}
	walk(root, 0)
	return errs
}

// CatalogEntry is one row of the aggregated event catalog.
type CatalogEntry struct {
	Addon         string          `json:"addon"`
	AddonName     string          `json:"addon_name,omitempty"`
	AddonVersion  string          `json:"addon_version,omitempty"`
	Name          string          `json:"name"`
	Description   string          `json:"description,omitempty"`
	Aggregate     string          `json:"aggregate,omitempty"`
	PayloadSchema json.RawMessage `json:"payload_schema,omitempty"`
	Sensitivity   Sensitivity     `json:"sensitivity"`
	Since         string          `json:"since,omitempty"`
}

// CatalogFilter narrows Catalog. Zero value = everything.
type CatalogFilter struct {
	// Addons keeps only these addon keys (empty = all).
	Addons []string
	// Sensitivity keeps only these levels (empty = all).
	Sensitivity []Sensitivity
}

// Catalog aggregates the events emitted by the given manifests (typically the
// ones installed for an organization). Pure: no I/O, input not mutated. Output
// is sorted by addon then name so it is stable for the UI and for tests. A
// missing sensitivity is reported as "internal". Nil manifests and manifests
// without an events block are skipped.
func Catalog(manifests []*Manifest, f CatalogFilter) []CatalogEntry {
	addons := make(map[string]struct{}, len(f.Addons))
	for _, a := range f.Addons {
		addons[a] = struct{}{}
	}
	levels := make(map[Sensitivity]struct{}, len(f.Sensitivity))
	for _, s := range f.Sensitivity {
		levels[s] = struct{}{}
	}
	out := []CatalogEntry{}
	for _, m := range manifests {
		if m == nil || m.Events == nil {
			continue
		}
		if len(addons) > 0 {
			if _, ok := addons[m.Metadata.Key]; !ok {
				continue
			}
		}
		for _, ev := range m.Events.Emits {
			s := ev.Sensitivity
			if s == "" {
				s = SensitivityInternal
			}
			if len(levels) > 0 {
				if _, ok := levels[s]; !ok {
					continue
				}
			}
			out = append(out, CatalogEntry{
				Addon: m.Metadata.Key, AddonName: m.Metadata.Name, AddonVersion: m.Metadata.Version,
				Name: ev.Name, Description: ev.Description, Aggregate: ev.Aggregate,
				PayloadSchema: ev.PayloadSchema, Sensitivity: s, Since: ev.Since,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Addon != out[j].Addon {
			return out[i].Addon < out[j].Addon
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// isCRUDEventName reports the host's canonical CRUD shape
// "<addon_key>.<Model>.<action>": three segments whose middle one starts with
// an uppercase letter (the host builds it from the owner addon key and the
// model key, dynamic/canonical_events.go).
func isCRUDEventName(name string) (addonKey string, ok bool) {
	parts := strings.Split(name, ".")
	if len(parts) != 3 || parts[1] == "" || parts[1][0] < 'A' || parts[1][0] > 'Z' {
		return "", false
	}
	return parts[0], true
}

// eventCovered mirrors the bus rule (events.eventMatches, capability glob):
// exact name, "*", or a trailing ".*" prefix.
func eventCovered(caps []Capability, name string) bool {
	for _, c := range caps {
		if c.Kind != "event:emit" {
			continue
		}
		p := c.Target
		if p == "*" || p == name {
			return true
		}
		if strings.HasSuffix(p, ".*") {
			prefix := strings.TrimSuffix(p, ".*")
			if name == prefix || strings.HasPrefix(name, prefix+".") {
				return true
			}
		}
	}
	return false
}

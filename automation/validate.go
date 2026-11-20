package automation

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var templateVarRe = regexp.MustCompile(`\{\{\s*([A-Za-z_$][A-Za-z0-9_.$]*)\s*\}\}`)

// rootVars are context roots a step may reference besides earlier step ids.
var rootVars = map[string]bool{"$trigger": true, "$now": true, "$today": true, "inputs": true, "event": true, "count": true, "lista": true, "item": true}

// validateSpec checks a parsed spec semantically for kind, as identity, at now.
// It returns the effective trigger location for scheduling.
func (s *Service) validateSpec(ctx context.Context, kind string, spec *Spec, id Identity, now time.Time) error {
	p := s.policy
	switch kind {
	case KindAutomation:
		if spec.Trigger == nil {
			return invalid("falta el disparador (trigger)")
		}
		if len(spec.Inputs) > 0 {
			return invalid("only dynamic tools declare inputs")
		}
	case KindTool:
		if spec.Trigger != nil {
			return invalid("a dynamic tool has no trigger")
		}
	default:
		return invalid("tipo desconocido %q", kind)
	}
	if err := s.validateTrigger(spec.Trigger, now); err != nil {
		return err
	}

	// Catalog snapshot for this identity (permission-filtered by the host).
	allowed := map[string]CatalogTool{}
	if s.catalog != nil {
		for _, t := range s.catalog.List(ctx, id) {
			allowed[t.Name] = t
		}
	}

	seen := map[string]bool{}
	for _, in := range spec.Inputs {
		if seen["inputs."+in.Name] {
			return invalid("input duplicado %q", in.Name)
		}
		seen["inputs."+in.Name] = true
	}
	deliveries := 0
	for i, st := range spec.Steps {
		if seen[st.ID] {
			return invalid("paso duplicado %q", st.ID)
		}
		ref := func(path string) error {
			if path == "" {
				return nil
			}
			root := strings.SplitN(path, ".", 2)[0]
			if rootVars[root] || seen[root] || strings.HasPrefix(root, "$") {
				return nil
			}
			return invalid("el paso %q usa %q, que no es un paso anterior", st.ID, path)
		}
		refTemplate := func(tpl string) error {
			for _, m := range templateVarRe.FindAllStringSubmatch(tpl, -1) {
				if err := ref(m[1]); err != nil {
					return err
				}
			}
			return nil
		}
		switch st.Type {
		case StepTool:
			tool, ok := allowed[st.Tool]
			if !ok {
				return invalid("tool %q is not allowed for automations", st.Tool)
			}
			if err := checkArgs(st.ID, tool, st.Args, refTemplate); err != nil {
				return err
			}
		case StepFilter:
			if err := ref(st.From); err != nil {
				return err
			}
		case StepCondition:
			if err := ref(st.If.Field); err != nil {
				return err
			}
		case StepTemplate:
			if err := refTemplate(st.Template); err != nil {
				return err
			}
		case StepSummarize:
			if kind == KindTool && st.Mode == "llm" {
				return invalid("a dynamic tool does not summarize with AI (the agent using it already does)")
			}
			if err := ref(st.From); err != nil {
				return err
			}
			if err := refTemplate(st.Template); err != nil {
				return err
			}
		case StepDeliver:
			if kind == KindTool {
				return invalid("a dynamic tool does not deliver notifications (step %q)", st.ID)
			}
			deliveries++
			for _, ch := range st.Channels {
				if !p.channelAllowed(ch) {
					return invalid("channel %q is not available", ch)
				}
			}
			for _, tpl := range []string{st.Title, st.Body, st.Link} {
				if err := refTemplate(tpl); err != nil {
					return err
				}
			}
			if err := ref(st.ItemsFrom); err != nil {
				return err
			}
			if st.Link != "" && !strings.HasPrefix(st.Link, "/") {
				return invalid("el enlace de la entrega debe ser una ruta interna (/…)")
			}
		}
		_ = i
		seen[st.ID] = true
	}
	if deliveries > 2 {
		return invalid("at most two delivery steps")
	}
	if kind == KindAutomation && deliveries == 0 {
		return invalid("the automation delivers nothing: add a deliver step")
	}
	return nil
}

func (s *Service) validateTrigger(t *Trigger, now time.Time) error {
	if t == nil {
		return nil
	}
	p := s.policy
	switch t.Type {
	case TriggerCron:
		loc, err := loadLocation(t.Timezone, p.DefaultTimezone)
		if err != nil {
			return err
		}
		c, err := ParseCron(t.Cron)
		if err != nil {
			return invalid("%v", err)
		}
		if c.Next(now, loc).IsZero() {
			return invalid("the cron expression %q never fires", t.Cron)
		}
		if gap := c.MinGap(now, loc, 300); gap < p.MinInterval {
			return invalid("the minimum frequency is every %s (the expression fires every %s)", humanDuration(p.MinInterval), humanDuration(gap))
		}
	case TriggerInterval:
		d, err := time.ParseDuration(t.Every)
		if err != nil || d <= 0 {
			return invalid("invalid interval %q", t.Every)
		}
		if d < p.MinInterval {
			return invalid("the minimum frequency is every %s", humanDuration(p.MinInterval))
		}
	case TriggerOnce:
		loc, err := loadLocation(t.Timezone, p.DefaultTimezone)
		if err != nil {
			return err
		}
		at, err := parseOnce(t.At, loc)
		if err != nil {
			return err
		}
		if !at.After(now) {
			return invalid("the date %s has already passed", at.In(loc).Format("02/01/2006 15:04"))
		}
		if at.Sub(now) > p.MaxOnceHorizon {
			return invalid("the date is too far away (maximum %s)", humanDuration(p.MaxOnceHorizon))
		}
	case TriggerEvent:
		if t.Cooldown != "" {
			d, err := time.ParseDuration(t.Cooldown)
			if err != nil || d < 0 {
				return invalid("invalid cooldown %q", t.Cooldown)
			}
		}
		for _, w := range t.Where {
			if strings.HasPrefix(w.Field, "$") {
				return invalid("las condiciones del evento solo leen su payload")
			}
		}
	}
	return nil
}

// checkArgs validates literal arguments against the tool's parameter
// declaration. Templated strings ("{{inputs.x}}") are checked for references
// only; their type is checked at run time.
func checkArgs(stepID string, tool CatalogTool, args map[string]any, refTemplate func(string) error) error {
	for name, v := range args {
		decl, ok := tool.Parameters[name].(map[string]any)
		if !ok {
			return invalid("%s: tool %q has no parameter %q", stepID, tool.Name, name)
		}
		if s, isStr := v.(string); isStr && templateVarRe.MatchString(s) {
			if err := refTemplate(s); err != nil {
				return err
			}
			continue
		}
		if err := checkType(name, decl, v); err != nil {
			return invalid("%s: %v", stepID, err)
		}
	}
	for name, d := range tool.Parameters {
		decl, _ := d.(map[string]any)
		if req, _ := decl["required"].(bool); req {
			if _, ok := args[name]; !ok {
				return invalid("%s: missing required parameter %q of %q", stepID, name, tool.Name)
			}
		}
	}
	return nil
}

func checkType(name string, decl map[string]any, v any) error {
	typ, _ := decl["type"].(string)
	switch typ {
	case "integer":
		f, ok := v.(float64)
		if !ok || f != float64(int64(f)) {
			if _, isInt := v.(int); !isInt {
				return fmt.Errorf("%q debe ser entero", name)
			}
		}
	case "number":
		switch v.(type) {
		case float64, int, int64:
		default:
			return fmt.Errorf("%q must be numeric", name)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%q debe ser verdadero/falso", name)
		}
	case "string", "":
		if _, ok := v.(string); !ok {
			return fmt.Errorf("%q debe ser texto", name)
		}
	}
	if enum, ok := decl["enum"].([]any); ok && len(enum) > 0 {
		for _, e := range enum {
			if fmt.Sprint(e) == fmt.Sprint(v) {
				return nil
			}
		}
		return fmt.Errorf("%q no admite %v", name, v)
	}
	if enum, ok := decl["enum"].([]string); ok && len(enum) > 0 {
		for _, e := range enum {
			if e == fmt.Sprint(v) {
				return nil
			}
		}
		return fmt.Errorf("%q no admite %v", name, v)
	}
	return nil
}

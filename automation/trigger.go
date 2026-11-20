package automation

import (
	"fmt"
	"strings"
	"time"
)

// onceLayouts are accepted for trigger.at besides RFC 3339 (interpreted in the
// trigger timezone).
var onceLayouts = []string{"2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02T15:04:05"}

func loadLocation(name, fallback string) (*time.Location, error) {
	if name == "" {
		name = fallback
	}
	if name == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, invalid("zona horaria desconocida %q", name)
	}
	return loc, nil
}

// parseOnce parses trigger.at.
func parseOnce(at string, loc *time.Location) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, at); err == nil {
		return t, nil
	}
	for _, l := range onceLayouts {
		if t, err := time.ParseInLocation(l, at, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, invalid("invalid date and time %q (use YYYY-MM-DDTHH:MM)", at)
}

// NextRun computes the next fire strictly after `after` for a scheduled
// trigger. ok=false means the trigger never fires again (a past one-shot, an
// event trigger, or an impossible cron).
func NextRun(t *Trigger, after time.Time, defaultTZ string) (next time.Time, ok bool, err error) {
	if t == nil {
		return time.Time{}, false, nil
	}
	switch t.Type {
	case TriggerCron:
		loc, err := loadLocation(t.Timezone, defaultTZ)
		if err != nil {
			return time.Time{}, false, err
		}
		c, err := ParseCron(t.Cron)
		if err != nil {
			return time.Time{}, false, invalid("%v", err)
		}
		n := c.Next(after, loc)
		return n, !n.IsZero(), nil
	case TriggerInterval:
		d, err := time.ParseDuration(t.Every)
		if err != nil || d <= 0 {
			return time.Time{}, false, invalid("invalid interval %q", t.Every)
		}
		return after.Add(d), true, nil
	case TriggerOnce:
		loc, err := loadLocation(t.Timezone, defaultTZ)
		if err != nil {
			return time.Time{}, false, err
		}
		at, err := parseOnce(t.At, loc)
		if err != nil {
			return time.Time{}, false, err
		}
		if !at.After(after) {
			return time.Time{}, false, nil
		}
		return at, true, nil
	case TriggerEvent:
		return time.Time{}, false, nil
	}
	return time.Time{}, false, invalid("disparador desconocido %q", t.Type)
}

var weekdaysES = []string{"domingo", "lunes", "martes", "miércoles", "jueves", "viernes", "sábado"}

// DescribeTrigger renders a short Spanish description ("Cada día a las 07:00
// (America/Mexico_City)") for cards and lists. Unknown shapes fall back to the
// raw expression.
func DescribeTrigger(t *Trigger, defaultTZ string) string {
	return describeTrigger(t, defaultTZ, nil)
}

var opLabels = map[string]string{
	"eq": "es", "ne": "no es", "contains": "contiene", "not_contains": "no contiene", "gt": "mayor que",
	"gte": "al menos", "lt": "menor que", "lte": "como máximo", "in": "es", "is_empty": "está vacío", "is_not_empty": "tiene valor",
}

func describeValue(v any) string {
	if list, ok := v.([]any); ok {
		parts := make([]string, 0, len(list))
		for _, x := range list {
			parts = append(parts, fmt.Sprint(x))
		}
		return strings.Join(parts, " o ")
	}
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func describeTrigger(t *Trigger, defaultTZ string, eventLabels map[string]string) string {
	if t == nil {
		return "Bajo demanda (herramienta)"
	}
	tz := t.Timezone
	if tz == "" {
		tz = defaultTZ
	}
	switch t.Type {
	case TriggerOnce:
		loc, err := loadLocation(t.Timezone, defaultTZ)
		if err != nil {
			return "Una vez: " + t.At
		}
		at, err := parseOnce(t.At, loc)
		if err != nil {
			return "Una vez: " + t.At
		}
		return "Una vez, el " + at.In(loc).Format("02/01/2006 a las 15:04") + " (" + tz + ")"
	case TriggerInterval:
		d, err := time.ParseDuration(t.Every)
		if err != nil {
			return "Cada " + t.Every
		}
		return "Cada " + humanDuration(d)
	case TriggerEvent:
		s := "Cuando ocurra «" + t.Event + "»"
		if l, ok := eventLabels[t.Event]; ok && l != "" {
			s = "Cuando " + l
		}
		if len(t.Where) > 0 {
			parts := make([]string, 0, len(t.Where))
			for _, w := range t.Where {
				op := opLabels[w.Op]
				if op == "" {
					op = w.Op
				}
				parts = append(parts, strings.TrimSpace(fmt.Sprintf("%s %s %s", w.Field, op, describeValue(w.Value))))
			}
			s += " (" + strings.Join(parts, "; ") + ")"
		}
		return s
	case TriggerCron:
		f := strings.Fields(t.Cron)
		if len(f) == 5 && isNum(f[0]) && isNum(f[1]) && f[3] == "*" {
			hm := pad2(f[1]) + ":" + pad2(f[0])
			switch {
			case f[2] == "*" && f[4] == "*":
				return "Cada día a las " + hm + " (" + tz + ")"
			case f[2] == "*" && f[4] == "1-5":
				return "De lunes a viernes a las " + hm + " (" + tz + ")"
			case f[2] == "*" && isNum(f[4]):
				n := atoi(f[4]) % 7
				return "Cada " + weekdaysES[n] + " a las " + hm + " (" + tz + ")"
			case isNum(f[2]) && f[4] == "*":
				return "El día " + f[2] + " de cada mes a las " + hm + " (" + tz + ")"
			}
		}
		return "Programada: " + t.Cron + " (" + tz + ")"
	}
	return t.Type
}

func humanDuration(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0:
		n := int(d / (24 * time.Hour))
		if n == 1 {
			return "día"
		}
		return fmt.Sprintf("%d días", n)
	case d%time.Hour == 0:
		n := int(d / time.Hour)
		if n == 1 {
			return "hora"
		}
		return fmt.Sprintf("%d horas", n)
	case d%time.Minute == 0:
		return fmt.Sprintf("%d minutos", int(d/time.Minute))
	}
	return d.String()
}

func isNum(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}

func pad2(s string) string {
	if len(s) == 1 {
		return "0" + s
	}
	return s
}

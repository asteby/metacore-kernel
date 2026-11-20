package automation

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron is a parsed standard 5-field expression: minute hour day-of-month month
// day-of-week. Fields accept "*", lists ("1,15"), ranges ("1-5"), steps
// ("*/15", "8-18/2") and, for day-of-week, 0-7 (0 and 7 are Sunday). As in
// Vixie cron, when both day-of-month and day-of-week are restricted a day
// matches if EITHER does.
type Cron struct {
	expr                          string
	minute, hour, dom, month, dow uint64
	domStar, dowStar              bool
}

// ParseCron parses a 5-field expression. Macros @hourly, @daily, @weekly and
// @monthly are accepted.
func ParseCron(expr string) (*Cron, error) {
	e := strings.TrimSpace(expr)
	switch e {
	case "@hourly":
		e = "0 * * * *"
	case "@daily", "@midnight":
		e = "0 0 * * *"
	case "@weekly":
		e = "0 0 * * 0"
	case "@monthly":
		e = "0 0 1 * *"
	}
	f := strings.Fields(e)
	if len(f) != 5 {
		return nil, fmt.Errorf("cron %q: expected 5 fields (minute hour day month weekday)", expr)
	}
	c := &Cron{expr: expr}
	var err error
	if c.minute, _, err = cronField(f[0], 0, 59); err != nil {
		return nil, fmt.Errorf("cron %q minuto: %w", expr, err)
	}
	if c.hour, _, err = cronField(f[1], 0, 23); err != nil {
		return nil, fmt.Errorf("cron %q hora: %w", expr, err)
	}
	if c.dom, c.domStar, err = cronField(f[2], 1, 31); err != nil {
		return nil, fmt.Errorf("cron %q day: %w", expr, err)
	}
	if c.month, _, err = cronField(f[3], 1, 12); err != nil {
		return nil, fmt.Errorf("cron %q mes: %w", expr, err)
	}
	if c.dow, c.dowStar, err = cronField(f[4], 0, 7); err != nil {
		return nil, fmt.Errorf("cron %q weekday: %w", expr, err)
	}
	if c.dow&(1<<7) != 0 {
		c.dow |= 1
		c.dow &^= 1 << 7
	}
	return c, nil
}

// String returns the original expression.
func (c *Cron) String() string { return c.expr }

func cronField(s string, lo, hi int) (bits uint64, star bool, err error) {
	if s == "" {
		return 0, false, fmt.Errorf("empty field")
	}
	for _, part := range strings.Split(s, ",") {
		rng, stepS, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			step, err = strconv.Atoi(stepS)
			if err != nil || step <= 0 || step > hi {
				return 0, false, fmt.Errorf("invalid step %q", part)
			}
		}
		var a, b int
		switch {
		case rng == "*":
			a, b = lo, hi
			if !hasStep {
				star = true
			}
		case strings.Contains(rng, "-"):
			x, y, _ := strings.Cut(rng, "-")
			if a, err = strconv.Atoi(x); err != nil {
				return 0, false, fmt.Errorf("invalid range %q", part)
			}
			if b, err = strconv.Atoi(y); err != nil {
				return 0, false, fmt.Errorf("invalid range %q", part)
			}
		default:
			if a, err = strconv.Atoi(rng); err != nil {
				return 0, false, fmt.Errorf("invalid value %q", part)
			}
			b = a
			if hasStep {
				b = hi
			}
		}
		if a < lo || b > hi || a > b {
			return 0, false, fmt.Errorf("%q fuera de %d-%d", part, lo, hi)
		}
		for v := a; v <= b; v += step {
			bits |= 1 << uint(v)
		}
	}
	return bits, star, nil
}

func (c *Cron) dayMatches(t time.Time) bool {
	d := c.dom&(1<<uint(t.Day())) != 0
	w := c.dow&(1<<uint(t.Weekday())) != 0
	if c.domStar || c.dowStar {
		return d && w
	}
	return d || w
}

// Next returns the first instant strictly after `after` that matches, in loc
// wall-clock time. It returns the zero time if nothing matches within five
// years (for example "0 0 30 2 *").
func (c *Cron) Next(after time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	t := after.In(loc).Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		if c.month&(1<<uint(t.Month())) == 0 {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc)
			continue
		}
		if !c.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
			continue
		}
		if c.hour&(1<<uint(t.Hour())) == 0 {
			n := time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, loc)
			if !n.After(t) { // DST fall-back: wall clock repeats an hour
				n = t.Add(time.Hour).Truncate(time.Hour)
			}
			t = n
			continue
		}
		if c.minute&(1<<uint(t.Minute())) == 0 {
			t = t.Add(time.Minute)
			continue
		}
		// Strictly later in wall-clock time too: when DST falls back the same
		// local minute happens twice; fire only on its first occurrence.
		if t.After(after) && wall(t) > wall(after.In(loc)) {
			return t
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}
}

// MinGap returns the smallest distance between consecutive fires over the
// next `samples` occurrences starting at from. Used to enforce the minimum
// frequency.
func (c *Cron) MinGap(from time.Time, loc *time.Location, samples int) time.Duration {
	prev := c.Next(from, loc)
	if prev.IsZero() {
		return 0
	}
	min := time.Duration(1<<63 - 1)
	for i := 0; i < samples; i++ {
		n := c.Next(prev, loc)
		if n.IsZero() {
			break
		}
		if g := n.Sub(prev); g < min {
			min = g
		}
		prev = n
	}
	return min
}

// wall encodes the local wall-clock minute as a sortable integer.
func wall(t time.Time) int64 {
	return int64(t.Year())*100000000 + int64(t.Month())*1000000 + int64(t.Day())*10000 + int64(t.Hour())*100 + int64(t.Minute())
}

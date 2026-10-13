// Package dates provides lenient date/time column types for compiled GORM
// models that are written through the generic dynamic CRUD (/api/data/:model)
// or by an agent tool.
//
// The SDK form posts a date field as "YYYY-MM-DD", but time.Time only accepts
// RFC 3339 when decoding JSON, so creating a row with a bare date failed with
// "invalid request body". [Time] and [Date] accept both shapes on input
// (JSON and database scan) and serialize to a single canonical shape:
//
//   - Time  → timestamptz, serialized as RFC 3339 in UTC.
//   - Date  → date, serialized as "YYYY-MM-DD".
//
// The zero value serializes as JSON null and is stored as SQL NULL, so a
// non-pointer field behaves like a nullable column. Use *Time / *Date when the
// distinction between "absent" and "null" matters to the caller.
//
//	type Visit struct {
//	    ID          uuid.UUID
//	    ScheduledOn dates.Date  // "2026-10-13"
//	    StartedAt   *dates.Time // "2026-10-13T16:00:00Z" or "2026-10-13"
//	}
package dates

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DateLayout is the canonical calendar-day layout ("YYYY-MM-DD").
const DateLayout = "2006-01-02"

// ErrInvalid is wrapped by every parse failure, so callers can map it to a
// validation error with errors.Is.
var ErrInvalid = errors.New("dates: invalid date")

// inputLayouts are tried in order. Layouts without a zone are read as UTC.
var inputLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04:05",
	DateLayout,
}

// Parse reads raw as RFC 3339, "YYYY-MM-DDTHH:MM[:SS]", "YYYY-MM-DD HH:MM:SS"
// or "YYYY-MM-DD". Values without a zone are interpreted as UTC. An empty
// string returns the zero time and no error.
func Parse(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	for _, layout := range inputLayouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%w %q: use YYYY-MM-DD or RFC 3339", ErrInvalid, raw)
}

func scan(src any) (time.Time, error) {
	switch v := src.(type) {
	case nil:
		return time.Time{}, nil
	case time.Time:
		return v, nil
	case string:
		return Parse(v)
	case []byte:
		return Parse(string(v))
	}
	return time.Time{}, fmt.Errorf("%w: cannot scan %T", ErrInvalid, src)
}

func unmarshal(b []byte) (time.Time, error) {
	if string(b) == "null" {
		return time.Time{}, nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return time.Time{}, fmt.Errorf("%w: must be a JSON string", ErrInvalid)
	}
	return Parse(s)
}

// Time is an instant (timestamptz) that accepts "YYYY-MM-DD" or RFC 3339 and
// serializes as RFC 3339 in UTC. The zero value serializes as null.
type Time struct{ time.Time }

// Now returns the current instant in UTC.
func Now() Time { return Time{time.Now().UTC()} }

// NewTime wraps a time.Time.
func NewTime(t time.Time) Time { return Time{t} }

// UnmarshalJSON implements json.Unmarshaler.
func (t *Time) UnmarshalJSON(b []byte) (err error) { t.Time, err = unmarshal(b); return }

// MarshalJSON implements json.Marshaler.
func (t Time) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(t.Time.UTC().Format(time.RFC3339))
}

// Scan implements sql.Scanner.
func (t *Time) Scan(src any) (err error) { t.Time, err = scan(src); return }

// Value implements driver.Valuer. The zero value is stored as NULL.
func (t Time) Value() (driver.Value, error) {
	if t.IsZero() {
		return nil, nil
	}
	return t.Time, nil
}

// GormDataType tells GORM the column type.
func (Time) GormDataType() string { return "timestamptz" }

// Ptr returns the wrapped time as *time.Time, or nil for a nil or zero value.
func (t *Time) Ptr() *time.Time {
	if t == nil || t.IsZero() {
		return nil
	}
	v := t.Time
	return &v
}

// Date is a calendar day (date) that accepts "YYYY-MM-DD" or RFC 3339 and
// serializes as "YYYY-MM-DD". The zero value serializes as null.
type Date struct{ time.Time }

// NewDate wraps a time.Time.
func NewDate(t time.Time) Date { return Date{t} }

// UnmarshalJSON implements json.Unmarshaler.
func (d *Date) UnmarshalJSON(b []byte) (err error) { d.Time, err = unmarshal(b); return }

// MarshalJSON implements json.Marshaler.
func (d Date) MarshalJSON() ([]byte, error) {
	if d.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(d.Time.Format(DateLayout))
}

// Scan implements sql.Scanner.
func (d *Date) Scan(src any) (err error) { d.Time, err = scan(src); return }

// Value implements driver.Valuer. The zero value is stored as NULL.
func (d Date) Value() (driver.Value, error) {
	if d.IsZero() {
		return nil, nil
	}
	return d.Time, nil
}

// GormDataType tells GORM the column type.
func (Date) GormDataType() string { return "date" }

// Ptr returns the wrapped time as *time.Time, or nil for a nil or zero value.
func (d *Date) Ptr() *time.Time {
	if d == nil || d.IsZero() {
		return nil
	}
	v := d.Time
	return &v
}

// TimePtr returns the time.Time of a *Time, or nil. Same as t.Ptr().
func TimePtr(t *Time) *time.Time { return t.Ptr() }

// DatePtr returns the time.Time of a *Date, or nil. Same as d.Ptr().
func DatePtr(d *Date) *time.Time { return d.Ptr() }

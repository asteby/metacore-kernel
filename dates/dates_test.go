package dates

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type row struct {
	At  *Time `json:"at"`
	Day Date  `json:"day"`
}

func TestUnmarshalAcceptsDateOnlyAndRFC3339(t *testing.T) {
	var r row
	if err := json.Unmarshal([]byte(`{"at":"2026-10-11","day":"2026-10-05"}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.At == nil || !r.At.Equal(time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("date only: %v", r.At)
	}
	if err := json.Unmarshal([]byte(`{"at":"2026-10-11T07:30:00Z","day":"2026-10-05T00:00:00Z"}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.At.Hour() != 7 || r.Day.Format(DateLayout) != "2026-10-05" {
		t.Errorf("RFC 3339: %v %v", r.At, r.Day)
	}
	if err := json.Unmarshal([]byte(`{"at":"2026-10-11T07:30"}`), &r); err != nil || r.At.Minute() != 30 {
		t.Errorf("datetime-local: %v %v", r.At, err)
	}
	for _, bad := range []string{`{"at":"yesterday"}`, `{"at":5}`} {
		err := json.Unmarshal([]byte(bad), &r)
		if err == nil || !errors.Is(err, ErrInvalid) {
			t.Errorf("want ErrInvalid for %s, got %v", bad, err)
		}
	}
}

func TestEmptyAndNull(t *testing.T) {
	var r row
	if err := json.Unmarshal([]byte(`{"at":"","day":null}`), &r); err != nil {
		t.Fatal(err)
	}
	if !r.At.IsZero() || !r.Day.IsZero() {
		t.Error("empty and null must be zero")
	}
	out, _ := json.Marshal(row{})
	if string(out) != `{"at":null,"day":null}` {
		t.Errorf("zero must serialize as null: %s", out)
	}
}

func TestMarshal(t *testing.T) {
	at := NewTime(time.Date(2026, 10, 11, 1, 0, 0, 0, time.FixedZone("CST", -6*3600)))
	out, _ := json.Marshal(row{At: &at, Day: NewDate(time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC))})
	if string(out) != `{"at":"2026-10-11T07:00:00Z","day":"2026-10-05"}` {
		t.Errorf("marshal: %s", out)
	}
}

func TestValueScan(t *testing.T) {
	var d Date
	if err := d.Scan(time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)); err != nil || d.Day() != 5 {
		t.Errorf("Scan time: %v %v", d, err)
	}
	if err := d.Scan("2026-10-06"); err != nil || d.Day() != 6 {
		t.Errorf("Scan string: %v %v", d, err)
	}
	if err := d.Scan(nil); err != nil || !d.IsZero() {
		t.Errorf("Scan nil: %v %v", d, err)
	}
	if err := d.Scan(42); !errors.Is(err, ErrInvalid) {
		t.Errorf("Scan int: %v", err)
	}
	if v, _ := (Time{}).Value(); v != nil {
		t.Errorf("zero must store NULL, got %v", v)
	}
	if TimePtr(nil) != nil || DatePtr(&Date{}) != nil {
		t.Error("helpers return nil for empty")
	}
	now := Now()
	if p := now.Ptr(); p == nil || !p.Equal(now.Time) {
		t.Error("Ptr must return the wrapped instant")
	}
}

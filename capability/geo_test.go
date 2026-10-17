package capability

import (
	"strings"
	"testing"
)

func TestGeoContractsRegistered(t *testing.T) {
	for key, timeout := range map[string]any{GeoGeocode: GeocodeTimeout, GeoRoute: RouteTimeout} {
		c := mustContract(t, key)
		if !c.IsQuery() || c.Fanout() || c.Idempotent || c.PinRequired || c.Input == nil || c.Output == nil {
			t.Errorf("%s: want a one-provider, non-mutating query with typed input/output, got %+v", key, c)
		}
		if c.Timeout != timeout {
			t.Errorf("%s: timeout %v, want %v", key, c.Timeout, timeout)
		}
	}
	if got := mustContract(t, GeoRoute).Required; strings.Join(got, ",") != "waypoints" {
		t.Fatalf("geo.route required = %v", got)
	}
	if got := mustContract(t, GeoGeocode).Required; len(got) != 0 {
		t.Fatalf("geo.geocode has no individually required input (query or postal_code), got %v", got)
	}
}

func TestGeoGeocodeOutput(t *testing.T) {
	c := mustContract(t, GeoGeocode)
	cases := map[string]struct {
		out  string
		want []string
	}{
		"centroid":      {`{"results":[{"lat":19.4326,"lng":-99.1332,"precision":"postal_code","postal_code":"06000","country":"MX","bbox":[-99.2,19.4,-99.1,19.5]}]}`, nil},
		"no match":      {`{"results":[]}`, nil},
		"lat range":     {`{"results":[{"lat":91,"lng":0,"precision":"street"}]}`, []string{"results[0].lat: want latitude"}},
		"lng range":     {`{"results":[{"lat":0,"lng":-181,"precision":"street"}]}`, []string{"results[0].lng: want longitude"}},
		"string coords": {`{"results":[{"lat":"19.4","lng":"-99.1","precision":"street"}]}`, []string{"results[0].lat", "results[0].lng"}},
		"precision":     {`{"results":[{"lat":0,"lng":0,"precision":"exact"}]}`, []string{"results[0].precision"}},
		"bbox numbers":  {`{"results":[{"lat":0,"lng":0,"precision":"region","bbox":["a"]}]}`, []string{"results[0].bbox[0]: want number"}},
		"unknown field": {`{"results":[],"provider":"x"}`, []string{"provider: unknown field"}},
	}
	for name, tc := range cases {
		got := c.ValidateOutput([]byte(tc.out))
		if len(tc.want) == 0 && len(got) != 0 {
			t.Errorf("%s: unexpected errors %v", name, got)
		}
		for _, w := range tc.want {
			if !strings.Contains(strings.Join(got, "; "), w) {
				t.Errorf("%s: want %q in %v", name, w, got)
			}
		}
	}
}

func TestGeoRouteInputAndOutput(t *testing.T) {
	c := mustContract(t, GeoRoute)
	in := map[string]any{
		"waypoints": []any{map[string]any{"lat": 19.4, "lng": -99.1}, map[string]any{"lat": 20.6, "lng": -103.3}},
		"profile":   "truck",
	}
	if errs := c.CheckInput(in); len(errs) != 0 {
		t.Fatalf("valid route input rejected: %v", errs)
	}
	in["waypoints"] = []any{map[string]any{"lat": 19.4, "lng": "-99.1"}}
	in["profile"] = "plane"
	errs := strings.Join(c.CheckInput(in), "; ")
	for _, w := range []string{"waypoints[0].lng: want longitude", "profile"} {
		if !strings.Contains(errs, w) {
			t.Errorf("want %q in %s", w, errs)
		}
	}
	ok := `{"distance_m":540000,"duration_s":21600.5,"legs":[{"distance_m":540000,"duration_s":21600.5}],"geometry":{"type":"LineString","coordinates":[[-99.1,19.4],[-103.3,20.6]]}}`
	if errs := c.ValidateOutput([]byte(ok)); len(errs) != 0 {
		t.Fatalf("valid route output rejected: %v", errs)
	}
	if errs := c.ValidateOutput([]byte(`{"distance_m":"540 km"}`)); len(errs) != 2 {
		t.Fatalf("want distance type + missing duration, got %v", errs)
	}
}

func TestTrackingEventCoordinatesAreOptional(t *testing.T) {
	track := mustContract(t, CommerceShippingTrack)
	// Existing providers (no coordinates) keep validating.
	if errs := track.ValidateOutput([]byte(`{"status":"in_transit","events":[{"at":"2026-10-16T10:00:00Z","status":"in_transit","location":"CDMX"}]}`)); len(errs) != 0 {
		t.Fatalf("event without coordinates rejected: %v", errs)
	}
	if errs := track.ValidateOutput([]byte(`{"status":"in_transit","events":[{"at":"2026-10-16T10:00:00Z","status":"in_transit","location":"CDMX","lat":19.43,"lng":-99.13}]}`)); len(errs) != 0 {
		t.Fatalf("event with coordinates rejected: %v", errs)
	}
	errs := track.ValidateOutput([]byte(`{"status":"in_transit","events":[{"at":"2026-10-16T10:00:00Z","status":"in_transit","lat":"19.43","lng":200}]}`))
	if len(errs) != 2 {
		t.Fatalf("want lat type + lng range errors, got %v", errs)
	}
}

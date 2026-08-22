package dispatch

import "testing"

func TestGuestEnvelopeError(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`{"success":true}`, ""},
		{`{"success":true,"data":{"skipped":"awaiting_pick"}}`, ""},
		{`{"success":false,"error":{"code":"invalid_request","message":"column \"id\" is host-stamped"}}`,
			`invalid_request: column "id" is host-stamped`},
		{`{"success":false}`, "success=false"},
		{`not-json`, ""},
		{``, ""},
	}
	for _, c := range cases {
		got := guestEnvelopeError([]byte(c.in))
		if got != c.want {
			t.Errorf("guestEnvelopeError(%s) = %q, want %q", c.in, got, c.want)
		}
	}
}

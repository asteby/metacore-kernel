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
		// message_key from the host rides along for the operator / UI.
		{`{"success":false,"error":{"code":"forbidden","message":"cross-tenant write refused","message_key":"wasm.host.error.forbidden"}}`,
			`forbidden: cross-tenant write refused [wasm.host.error.forbidden]`},
		// A host envelope pasted verbatim into the guest message is unwrapped
		// (tire_warranty / channel_orders dead deliveries read as raw JSON).
		{`{"success":false,"error":{"code":"db_error","message":"upsert channel_order: {\"error\":{\"code\":\"invalid_request\",\"message\":\"data: column \\\"payload\\\": unsupported object arg\"},\"meta\":{\"addon\":\"channel_orders\"},\"success\":false}"}}`,
			`db_error: upsert channel_order: host invalid_request: data: column "payload": unsupported object arg`},
		{`{"success":false,"error":{"code":"db_error","message":"{\"error\":{\"code\":\"db_error\",\"message\":\"ERROR: null value in column \\\"customer_id\\\"\"},\"success\":false} (retry later)"}}`,
			`db_error: host db_error: ERROR: null value in column "customer_id" (retry later)`},
		// Something that only looks like an envelope stays as-is.
		{`{"success":false,"error":{"code":"x","message":"bad {\"error\" oops"}}`, `x: bad {"error" oops`},
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

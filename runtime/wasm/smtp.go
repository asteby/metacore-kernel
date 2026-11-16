package wasm

// smtp.go — the `smtp_send` host import (ABI 1.20): a guest hands the host one ready-made RFC 5322
// message and the host delivers it over SMTP through the ORGANIZATION'S OWN mail server (company
// mailbox, Office 365, Google Workspace, a relay…), instead of only through an HTTP email API.
//
// The guest sandbox has no raw network (only http_request), so the TCP session lives in the host,
// where every egress rule is in one place:
//   - capability `smtp:send` (a fixed host glob, or `connector:<c>.<credential>` so the host comes
//     from the org's connector, never from the guest);
//   - the SSRF rules of HTTP (loopback, private ranges, metadata refused; netguard.DialControl
//     re-checks the resolved IP at connect time);
//   - submission ports only, TLS mandatory, bounded recipients/size/time (the wait does not count
//     against the guest's budget, like http_request).
//
// The session itself is in runtime/wasm/smtpsend. Response: {"ok":true,"message":...} or the
// standard error envelope (bad_request, forbidden, transport, smtp_tls, smtp_auth, smtp_rejected,
// plus smtp_code when the server answered).

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/asteby/metacore-kernel/runtime/wasm/smtpsend"
	"github.com/google/uuid"
	"github.com/tetratelabs/wazero/api"
)

// smtpConnectorHosts resolves the org's smtp:send connector grants to concrete hosts for this
// invocation (same shape as connectorFetchHosts).
func smtpConnectorHosts(ctx context.Context, inv *invocation) []string {
	refs := inv.caps.SMTPHostRefs()
	if len(refs) == 0 || inv.connectors == nil || inv.orgID == uuid.Nil {
		return nil
	}
	var hosts []string
	for _, ref := range refs {
		if inv.caps.CanReadConnector(ref.Connector) != nil {
			continue
		}
		creds, err := scopedConnectorGet(ctx, inv, ref.Connector)
		if err != nil {
			continue
		}
		for _, v := range creds {
			inv.bag.AddSecrets(v)
		}
		if h := strings.ToLower(strings.TrimSpace(creds[ref.Credential])); h != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

func smtpResultEnvelope(res smtpsend.Result) []byte {
	if res.Code == "" {
		b, _ := json.Marshal(map[string]any{"ok": true, "message": res.Message})
		return b
	}
	out := map[string]any{"error": res.Code, "message": res.Message, "message_key": hostErrorMessageKey(res.Code)}
	if res.SMTPCode != 0 {
		out["smtp_code"] = res.SMTPCode
	}
	b, _ := json.Marshal(out)
	return b
}

// doSMTP is the smtp_send implementation.
func doSMTP(ctx context.Context, mod api.Module, inv *invocation, raw []byte) uint64 {
	var req smtpsend.Request
	if err := json.Unmarshal(raw, &req); err != nil {
		return writeToGuest(ctx, mod, jsonError("bad_request", "request must be a JSON object: "+err.Error()))
	}
	if req.Password != "" {
		inv.bag.AddSecrets(req.Password)
	}
	msg, bad := smtpsend.Validate(&req)
	if bad != "" {
		return writeToGuest(ctx, mod, jsonError("bad_request", bad))
	}
	if err := inv.caps.CanSMTPHosts(req.Host, smtpConnectorHosts(ctx, inv)); err != nil {
		return writeToGuest(ctx, mod, jsonError("forbidden", err.Error()))
	}
	var res smtpsend.Result
	inv.deadline.excluding(func() { res = smtpsend.Deliver(ctx, &req, msg) })
	return writeToGuest(ctx, mod, smtpResultEnvelope(res))
}

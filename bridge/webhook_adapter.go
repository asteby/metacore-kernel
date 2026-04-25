// webhook_adapter.go bridges the legacy "hook URL" pattern (where an addon
// declares `manifest.hooks["model::action"] = "https://..."`) to the
// kernel's signed dispatcher. The host wraps its existing unsigned
// interceptor — the bridge takes the URL and produces a signed
// ActionInterceptor that POSTs to it with the right HMAC + host-context
// headers.
//
// Behaviour:
//   - If an installation row exists in metacore_installations for the
//     (org, addon) pair AND the dispatcher's secret resolver returns a
//     secret, the outbound POST carries HMAC headers.
//   - Otherwise SignedWebhookInterceptor delegates to the supplied
//     fallback interceptor (typically the host's pre-bridge unsigned
//     behaviour) so we never regress addons that haven't been enrolled.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/asteby/metacore-kernel/installer"
	"github.com/asteby/metacore-kernel/security"
	"gorm.io/gorm"
)

// SignedWebhookInterceptor produces an ActionInterceptor that routes
// through the WebhookDispatcher when a kernel installation is registered
// for (org, addon). If no signing context is resolvable it delegates to
// fallback — host code passes its existing unsigned interceptor here so
// retro-compat is the default, never a regression.
//
// fallback may be nil. When it is and no signing context is resolvable,
// the interceptor returns (nil, nil) — i.e. the action is treated as a
// host-local pass.
func SignedWebhookInterceptor(db *gorm.DB, disp *security.WebhookDispatcher, addonKey, webhookURL string, fallback ActionInterceptor) ActionInterceptor {
	return func(ctx *ActionContext, recordID interface{}, payload map[string]interface{}) (interface{}, error) {
		if db == nil || disp == nil {
			if fallback != nil {
				return fallback(ctx, recordID, payload)
			}
			return nil, nil
		}
		var inst installer.Installation
		err := db.Where("organization_id = ? AND addon_key = ?", ctx.OrgID, addonKey).First(&inst).Error
		if err != nil || inst.SecretHash == "" {
			if fallback != nil {
				return fallback(ctx, recordID, payload)
			}
			return nil, nil
		}

		body := map[string]interface{}{
			"addon_key":       addonKey,
			"org_id":          ctx.OrgID.String(),
			"installation_id": inst.ID.String(),
			"record_id":       recordID,
			"payload":         payload,
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("signed webhook marshal: %w", err)
		}

		reqCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		reqCtx = security.WithEvent(reqCtx, eventNameFromPayload(payload))

		resp, err := disp.Dispatch(reqCtx, inst.ID, webhookURL, http.MethodPost, raw)
		if err != nil {
			// Fall back to unsigned so a signing glitch doesn't take the
			// addon offline while we roll out. The dispatcher already
			// logged details.
			if fallback != nil {
				return fallback(ctx, recordID, payload)
			}
			return nil, err
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("signed webhook read: %w", err)
		}
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("signed webhook returned status %d: %s", resp.StatusCode, string(respBody))
		}
		var out interface{}
		if err := json.Unmarshal(respBody, &out); err != nil {
			return string(respBody), nil
		}
		return out, nil
	}
}

// eventNameFromPayload best-efforts extracts an event label for the
// X-Metacore-Event header. Empty string means "no event metadata".
func eventNameFromPayload(payload map[string]interface{}) string {
	if payload == nil {
		return ""
	}
	if ev, ok := payload["event"].(string); ok {
		return ev
	}
	if ev, ok := payload["_event"].(string); ok {
		return ev
	}
	return ""
}

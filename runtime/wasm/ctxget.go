package wasm

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/google/uuid"
)

// CtxGetEnvelopeVersion is the wire-format version of the ctx_get JSON
// envelope (docs/wasm-abi.md § 20).
const CtxGetEnvelopeVersion = 1

// ctx_get scopes. Each one is unlocked by the manifest capability `ctx:<scope>`.
const (
	ctxScopeUser      = "user"
	ctxScopeRoles     = "roles"
	ctxScopeOrgConfig = "org_config"
)

var ctxAllScopes = []string{ctxScopeUser, ctxScopeRoles, ctxScopeOrgConfig}

// HostContextScopes tells the embedder's ContextProviderFn which slices the
// guest is entitled to (and asked for), so a provider can skip the queries for
// the rest instead of loading everything and letting the kernel discard it.
type HostContextScopes struct {
	User      bool
	Roles     bool
	OrgConfig bool
}

// OrgConfig is the closed, allow-listed slice of the organization the guest may
// read. It is NOT a view of the `organizations` row: no secrets, no billing, no
// arbitrary columns. Pointers distinguish "unset" (null) from a real zero — a
// tax_rate of 0 is a legitimate exempt org, an absent one is "not configured".
type OrgConfig struct {
	CurrencyCode string   `json:"currency_code"`
	TaxRate      *float64 `json:"tax_rate"`
	TaxIncluded  *bool    `json:"tax_included"`
	Locale       string   `json:"locale"`
	Timezone     string   `json:"timezone"`
}

// HostContext is what the embedder's provider returns for the acting
// (org, user). Fields outside the requested HostContextScopes may be left zero.
type HostContext struct {
	UserEmail string
	Roles     []string
	Org       OrgConfig
}

// ContextProviderFn resolves the execution context of an invocation: who is
// acting (userID is uuid.Nil for system-driven work such as an unattended event
// delivery) and the org's config. It is the port the embedder (ops) implements;
// the kernel owns the capability gate, the envelope and the size bounds.
type ContextProviderFn func(ctx context.Context, orgID, userID uuid.UUID, want HostContextScopes) (*HostContext, error)

type ctxGetRequest struct {
	// Scopes optionally narrows the answer. Empty = every scope the addon
	// declared. Asking for a scope the addon did NOT declare is `forbidden`
	// (loud, not silently empty) so a missing manifest capability is found in
	// the first test run rather than as a mysterious null in production.
	Scopes []string `json:"scopes"`
}

// executeCtxGet implements `metacore_host.ctx_get`: a read-only view of the
// invocation's execution context (the env.user / env.company of an Odoo
// module), gated per slice by the `ctx:user|roles|org_config` capabilities.
// All failures surface inside the JSON envelope; wire shape `{success,data,meta}`.
func executeCtxGet(ctx context.Context, inv *invocation, reqJSON []byte) []byte {
	start := time.Now()
	addonKey := ""
	orgID := uuid.Nil
	if inv != nil {
		addonKey = inv.addonKey
		orgID = inv.orgID
	}
	fail := func(code, msg string) []byte {
		return ctxGetErr(addonKey, code, msg, orgID, start)
	}
	if inv == nil {
		return fail("invalid_request", "invocation context missing")
	}
	if orgID == uuid.Nil {
		return fail("no_active_org", "invocation has no bound orgID")
	}

	var req ctxGetRequest
	if len(reqJSON) > 0 {
		if err := json.Unmarshal(reqJSON, &req); err != nil {
			return fail("invalid_request", "malformed request JSON: "+err.Error())
		}
	}

	// Capability gate, hard-enforced (like connector_get, independent of the
	// enforcer's shadow mode): the import is new, so no legacy guest depends on
	// permissive behaviour, and what it exposes is identity/config.
	granted := map[string]bool{}
	for _, s := range ctxAllScopes {
		if inv.caps.CanReadContext(s) == nil {
			granted[s] = true
		}
	}
	want := map[string]bool{}
	if len(req.Scopes) == 0 {
		want = granted
	} else {
		for _, s := range req.Scopes {
			known := false
			for _, k := range ctxAllScopes {
				known = known || k == s
			}
			if !known {
				return fail("invalid_request", "unknown scope "+s)
			}
			if !granted[s] {
				inv.logf("audit ctx_get addon=%s scope=%s org=%s decision=denied", addonKey, s, orgID)
				return fail("forbidden", "addon lacks capability ctx:"+s)
			}
			want[s] = true
		}
	}

	data := map[string]any{"org_id": orgID.String()}
	if len(want) > 0 {
		if inv.ctxProvider == nil {
			return fail("context_unavailable", "host has no context provider configured")
		}
		userID := uuid.Nil
		if id, err := uuid.Parse(dynamic.ActorIDFromContext(ctx)); err == nil {
			userID = id
		}
		hc, err := inv.ctxProvider(ctx, orgID, userID, HostContextScopes{
			User: want[ctxScopeUser], Roles: want[ctxScopeRoles], OrgConfig: want[ctxScopeOrgConfig],
		})
		if err != nil {
			return fail("context_error", err.Error())
		}
		if hc == nil {
			hc = &HostContext{}
		}
		if want[ctxScopeUser] {
			if userID == uuid.Nil {
				data["user_id"] = nil
			} else {
				data["user_id"] = userID.String()
			}
			data["user_email"] = hc.UserEmail
		}
		if want[ctxScopeRoles] {
			roles := append([]string(nil), hc.Roles...)
			sort.Strings(roles)
			if roles == nil {
				roles = []string{}
			}
			data["roles"] = roles
		}
		if want[ctxScopeOrgConfig] {
			data["org"] = hc.Org
		}
	}

	env, _ := json.Marshal(map[string]any{
		"success": true,
		"data":    data,
		"meta":    ctxGetMeta(addonKey, orgID, start),
	})
	return env
}

func ctxGetMeta(addonKey string, orgID uuid.UUID, start time.Time) map[string]any {
	meta := map[string]any{
		"addon":           addonKey,
		"durationMs":      time.Since(start).Milliseconds(),
		"envelopeVersion": CtxGetEnvelopeVersion,
	}
	if orgID != uuid.Nil {
		meta["orgId"] = orgID.String()
	}
	return meta
}

func ctxGetErr(addonKey, code, message string, orgID uuid.UUID, start time.Time) []byte {
	b, _ := json.Marshal(map[string]any{
		"success": false,
		"error":   map[string]any{"code": code, "message": message},
		"meta":    ctxGetMeta(addonKey, orgID, start),
	})
	return b
}

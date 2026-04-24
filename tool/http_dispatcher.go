package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
)

// HTTPDispatcher implements Tool by POST-ing the validated params to the
// addon-declared endpoint, relying on security.WebhookDispatcher for HMAC
// signing derived from the Installation's secret.
//
// Construct one per installed tool: HTTPDispatcher{Inst, Definition, Dispatcher}.
type HTTPDispatcher struct {
	// Inst binds the tool to its installation (tenant, secret lookup key).
	Inst Installation
	// Definition is the declared manifest.ToolDef.
	Definition manifest.ToolDef
	// Dispatcher signs and sends the HTTP call. Share one across all tools.
	Dispatcher *security.WebhookDispatcher
}

// ID implements Tool.
func (h *HTTPDispatcher) ID() string { return h.Definition.ID }

// AddonKey implements Tool.
func (h *HTTPDispatcher) AddonKey() string { return h.Inst.AddonKey }

// Def implements Tool.
func (h *HTTPDispatcher) Def() manifest.ToolDef { return h.Definition }

// Execute validates params, POSTs to the resolved endpoint, and shapes the
// addon response into a tool.Result. Non-2xx responses surface the body so
// hosts can log it, but Success=false.
func (h *HTTPDispatcher) Execute(ctx context.Context, params map[string]any) (Result, error) {
	cleaned, verrs := Validate(h.Definition.InputSchema, params)
	if len(verrs) > 0 {
		return Result{
			Success: false,
			Error:   summarizeValidation(verrs),
		}, nil
	}

	url, err := resolveEndpoint(h.Inst.BaseURL, h.Definition.Endpoint)
	if err != nil {
		return Result{}, err
	}

	body, err := json.Marshal(map[string]any{
		"addon_key":       h.Inst.AddonKey,
		"tool_id":         h.Definition.ID,
		"installation_id": h.Inst.ID,
		"org_id":          h.Inst.OrgID,
		"parameters":      cleaned,
	})
	if err != nil {
		return Result{}, fmt.Errorf("tool.HTTPDispatcher: marshal body: %w", err)
	}

	method := h.Definition.Method
	if method == "" {
		method = http.MethodPost
	}

	ctx = security.WithEvent(ctx, "tool:"+h.Definition.ID)

	status, resp, err := h.Dispatcher.DispatchAndRead(ctx, h.Inst.ID, url, method, body)
	if err != nil {
		return Result{}, err
	}

	if status >= 200 && status < 300 {
		return unpack(resp, true)
	}
	// Addon failed — surface the body as .Error so hosts can log it verbatim.
	return Result{
		Success: false,
		Error:   fmt.Sprintf("addon returned %d: %s", status, string(resp)),
	}, nil
}

// resolveEndpoint joins a relative ToolDef.Endpoint to the installation's
// BaseURL. Absolute endpoints pass through unchanged.
func resolveEndpoint(base, endpoint string) (string, error) {
	if endpoint == "" {
		return "", fmt.Errorf("tool.HTTPDispatcher: endpoint is empty")
	}
	if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
		return endpoint, nil
	}
	if base == "" {
		return "", fmt.Errorf("tool.HTTPDispatcher: endpoint %q is relative but Installation.BaseURL is empty", endpoint)
	}
	base = strings.TrimRight(base, "/")
	if !strings.HasPrefix(endpoint, "/") {
		endpoint = "/" + endpoint
	}
	return base + endpoint, nil
}

// unpack decodes the addon response. Addons may return either a bare JSON
// object (treated as .Data) or a {success, data, error, metadata} envelope.
func unpack(body []byte, fallbackSuccess bool) (Result, error) {
	if len(body) == 0 {
		return Result{Success: fallbackSuccess}, nil
	}
	var envelope struct {
		Success  *bool          `json:"success"`
		Data     any            `json:"data"`
		Error    string         `json:"error"`
		Metadata map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && (envelope.Success != nil || envelope.Error != "" || envelope.Metadata != nil) {
		r := Result{
			Data:     envelope.Data,
			Error:    envelope.Error,
			Metadata: envelope.Metadata,
		}
		if envelope.Success != nil {
			r.Success = *envelope.Success
		} else {
			r.Success = envelope.Error == ""
		}
		return r, nil
	}
	// No envelope recognized — treat the whole body as opaque data.
	var raw any
	if err := json.Unmarshal(body, &raw); err != nil {
		return Result{Success: fallbackSuccess, Data: string(body)}, nil
	}
	return Result{Success: fallbackSuccess, Data: raw}, nil
}

func summarizeValidation(errs []ValidationError) string {
	parts := make([]string, 0, len(errs))
	for _, e := range errs {
		parts = append(parts, e.Param+": "+e.Reason)
	}
	return "invalid params: " + strings.Join(parts, "; ")
}

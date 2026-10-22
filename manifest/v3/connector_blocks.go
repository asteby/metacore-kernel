package v3

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Connector setup blocks (connectors[].form_layout.sections[].blocks).
//
// A connector's form_layout turns its credentials into a wizard, but a real
// "connect this provider" step needs more than inputs: the URL the operator
// must paste in the provider's panel (computed by the host for THIS
// organization), instructions with links to that panel, and buttons that
// verify the result (test the credentials, create a sandbox order). Blocks
// declare those pieces once in the manifest; the host fills the computed
// values and runs the actions; the SDK renders them the same for every addon.
//
// Blocks are optional and additive: a section without blocks renders exactly
// as before. They are only valid inside a CONNECTOR's form_layout (a model
// form or an edge-device pairing form has no per-org webhook nor connector
// context to run an action with), and both validators reject them elsewhere.

// Block kinds.
const (
	// FormBlockWebhookURL shows the public URL of one of the addon's inbound
	// webhooks (webhooks[].key) for the current organization, read-only with
	// a copy button. The host computes it; the manifest never hardcodes it.
	FormBlockWebhookURL = "webhook_url"
	// FormBlockInstructions is help text with optional https links (e.g. to
	// the provider's developer panel). Nothing to compute or run.
	FormBlockInstructions = "instructions"
	// FormBlockAction is a verification button bound to a wasm export of the
	// addon. The host runs it with the org's STORED connector credentials and
	// the SDK shows the structured result (see FormBlockResult) with retry.
	FormBlockAction = "action"
)

// Block limits keep a step renderable on a phone.
const (
	MaxFormBlocksPerSection = 8
	MaxFormBlockLinks       = 4
)

// FormBlock is one non-input element of a connector setup step.
type FormBlock struct {
	// Key identifies the block, unique across the connector's form_layout.
	// The host addresses an action block by it
	// (POST /integrations/<connector>/blocks/<key>/run).
	Key string `json:"key"`
	// Kind is webhook_url | instructions | action.
	Kind string `json:"kind"`
	// Title / Description: literal text or i18n keys.
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	// Webhook (kind=webhook_url) is the webhooks[].key whose URL is shown.
	Webhook string `json:"webhook,omitempty"`
	// Links (kind=instructions or webhook_url) point to external panels. Only
	// https URLs are accepted.
	Links []FormBlockLink `json:"links,omitempty"`
	// Export (kind=action) is the wasm export the host invokes. It joins the
	// addon's export whitelist (backend.exports) like a test_export.
	Export string `json:"export,omitempty"`
	// Label (kind=action) is the button text (literal or i18n key).
	Label string `json:"label,omitempty"`
}

// FormBlockLink is a labelled https link of a block.
type FormBlockLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// FormBlockResult documents what an action block's export answers inside the
// kernel envelope {success, data}. data:
//
//	{"ok": true, "status": "ok", "message": "Orden de prueba creada",
//	 "details": [{"key":"id","label":"ID","value":"123-abc","kind":"id"},
//	             {"key":"link","label":"Pagar en sandbox","value":"https://…","kind":"url"}]}
//
// status uses the setup vocabulary (ok | warn | error | pending); when absent
// it is derived from ok. kind is text | id | url (ids get a copy button, urls
// a link). The host passes the envelope through; it never invents success.
type FormBlockResult struct {
	OK      bool                 `json:"ok"`
	Status  string               `json:"status,omitempty"`
	Message string               `json:"message,omitempty"`
	Details []FormBlockResultRow `json:"details,omitempty"`
}

// FormBlockResultRow is one labelled value of an action result.
type FormBlockResultRow struct {
	Key   string `json:"key,omitempty"`
	Label string `json:"label,omitempty"`
	Value string `json:"value"`
	Kind  string `json:"kind,omitempty"`
}

var formBlockKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// FormLayoutHasBlocks reports whether any section of fl declares blocks.
func FormLayoutHasBlocks(fl *FormLayout) bool {
	if fl == nil {
		return false
	}
	for _, s := range fl.Sections {
		if len(s.Blocks) > 0 {
			return true
		}
	}
	return false
}

// FormLayoutBlockExports lists the wasm exports of fl's action blocks (for the
// export whitelist).
func FormLayoutBlockExports(fl *FormLayout) []string {
	if fl == nil {
		return nil
	}
	var out []string
	for _, s := range fl.Sections {
		for _, b := range s.Blocks {
			if b.Kind == FormBlockAction && b.Export != "" {
				out = append(out, b.Export)
			}
		}
	}
	return out
}

// ValidateConnectorBlocks checks the blocks of one connector's form_layout.
// where is the error prefix ("connectors[0]"); webhooks is the set of the
// addon's webhooks[].key. Shared by the v3 validator and the legacy
// manifest.Manifest.Validate so a manifest fails identically on both.
func ValidateConnectorBlocks(where string, fl *FormLayout, webhooks map[string]struct{}) []string {
	if fl == nil {
		return nil
	}
	var errs []string
	seen := map[string]struct{}{}
	for si, s := range fl.Sections {
		if len(s.Blocks) > MaxFormBlocksPerSection {
			errs = append(errs, fmt.Sprintf("%s.form_layout.sections[%d].blocks has %d blocks (max %d)", where, si, len(s.Blocks), MaxFormBlocksPerSection))
		}
		for bi, b := range s.Blocks {
			at := fmt.Sprintf("%s.form_layout.sections[%d].blocks[%d]", where, si, bi)
			if !formBlockKeyRe.MatchString(b.Key) {
				errs = append(errs, fmt.Sprintf("%s.key %q must match %s", at, b.Key, formBlockKeyRe))
			} else if _, dup := seen[b.Key]; dup {
				errs = append(errs, fmt.Sprintf("%s.key %q is duplicated in the connector's form_layout", at, b.Key))
			}
			seen[b.Key] = struct{}{}
			errs = append(errs, validateBlockLinks(at, b.Links)...)
			switch b.Kind {
			case FormBlockWebhookURL:
				if b.Webhook == "" {
					errs = append(errs, at+".webhook is required for kind webhook_url")
				} else if _, ok := webhooks[b.Webhook]; !ok {
					errs = append(errs, fmt.Sprintf("%s.webhook %q is not a webhooks[].key of this addon", at, b.Webhook))
				}
				if b.Export != "" || b.Label != "" {
					errs = append(errs, at+": export/label only apply to kind action")
				}
			case FormBlockInstructions:
				if strings.TrimSpace(b.Description) == "" && len(b.Links) == 0 {
					errs = append(errs, at+": kind instructions needs a description or links")
				}
				if b.Webhook != "" || b.Export != "" || b.Label != "" {
					errs = append(errs, at+": webhook/export/label do not apply to kind instructions")
				}
			case FormBlockAction:
				if b.Export == "" {
					errs = append(errs, at+".export is required for kind action")
				} else if !setupSymbolRe.MatchString(b.Export) {
					errs = append(errs, fmt.Sprintf("%s.export %q must match %s", at, b.Export, setupSymbolRe))
				}
				if b.Webhook != "" {
					errs = append(errs, at+": webhook only applies to kind webhook_url")
				}
			default:
				errs = append(errs, fmt.Sprintf("%s.kind %q is not one of webhook_url|instructions|action", at, b.Kind))
			}
		}
	}
	return errs
}

func validateBlockLinks(at string, links []FormBlockLink) []string {
	var errs []string
	if len(links) > MaxFormBlockLinks {
		errs = append(errs, fmt.Sprintf("%s.links has %d links (max %d)", at, len(links), MaxFormBlockLinks))
	}
	for li, l := range links {
		if strings.TrimSpace(l.Label) == "" {
			errs = append(errs, fmt.Sprintf("%s.links[%d].label is empty", at, li))
		}
		u, err := url.Parse(l.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			errs = append(errs, fmt.Sprintf("%s.links[%d].url %q must be an absolute https URL", at, li, l.URL))
		}
	}
	return errs
}

// validateFormBlocks is the v3 validator's entry: connector blocks are
// checked against the addon's webhooks; any other form_layout must not
// declare blocks.
func validateFormBlocks(m *Manifest) []string {
	webhooks := make(map[string]struct{}, len(m.Webhooks))
	for _, w := range m.Webhooks {
		webhooks[w.Key] = struct{}{}
	}
	var errs []string
	for ci, c := range m.Connectors {
		errs = append(errs, ValidateConnectorBlocks(fmt.Sprintf("connectors[%d]", ci), c.FormLayout, webhooks)...)
	}
	for mi, mod := range m.Models {
		errs = append(errs, rejectBlocks(fmt.Sprintf("models[%d]", mi), mod.FormLayout)...)
	}
	for di, d := range m.EdgeDevices {
		errs = append(errs, rejectBlocks(fmt.Sprintf("edge_devices[%d]", di), d.FormLayout)...)
	}
	return errs
}

// rejectBlocks reports blocks declared on a form_layout that is not a
// connector's (models, edge-device pairing).
func rejectBlocks(where string, fl *FormLayout) []string {
	if !FormLayoutHasBlocks(fl) {
		return nil
	}
	return []string{where + ".form_layout: blocks are only supported in connectors[].form_layout"}
}

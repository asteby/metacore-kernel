package v3

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
)

// Setting.auto values: a connector credential the host generates.
const (
	// SettingAutoStorefrontBearer is the bearer secret a storefront sends to the
	// addon's webhooks with verify "bearer" (secret_ref "<connector>.<key>"). It
	// replaces "invent a 32+ char secret and paste it in two places".
	SettingAutoStorefrontBearer = "storefront_bearer"

	// SettingAutoWebhookSigning is the secret the host generates to sign and
	// verify inbound webhooks with verify "hmac-sha256" (secret_ref
	// "<connector>.<key>"). It replaces "invent a 32+ char secret and paste it
	// in two places".
	SettingAutoWebhookSigning = "webhook_signing"

	// AutoSecretPrefix marks a host-generated secret so a leaked one is
	// recognisable in logs and secret scanners.
	AutoSecretPrefix = "mcs_"
	// AutoSecretBytes is the entropy of a generated secret (256 bits).
	AutoSecretBytes = 32
)

// IsKnownSettingAuto reports whether v is a supported Setting.auto value.
func IsKnownSettingAuto(v string) bool {
	return v == SettingAutoStorefrontBearer || v == SettingAutoWebhookSigning
}

// GenerateAutoSecret returns a fresh random secret for an auto credential:
// AutoSecretPrefix + 43 base64url characters (256 bits from crypto/rand).
func GenerateAutoSecret() (string, error) {
	b := make([]byte, AutoSecretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auto secret: %w", err)
	}
	return AutoSecretPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// validateSettingAuto checks one Setting.auto declaration. where is the list
// path ("connectors[0].credentials"), w the item path.
func validateSettingAuto(where, w string, s Setting) []string {
	var errs []string
	if !strings.HasPrefix(where, "connectors[") {
		errs = append(errs, w+".auto is only valid on a connector credential")
	}
	if !IsKnownSettingAuto(s.Auto) {
		errs = append(errs, fmt.Sprintf("%s.auto %q is not supported (supported: %s, %s)", w, s.Auto, SettingAutoStorefrontBearer, SettingAutoWebhookSigning))
	}
	if s.Type != "secret" {
		errs = append(errs, w+".auto requires type secret")
	}
	if s.Default != nil {
		errs = append(errs, w+".auto cannot declare a default")
	}
	if s.VisibleWhen != nil {
		errs = append(errs, w+".auto cannot declare visible_when (the host owns the value)")
	}
	return errs
}

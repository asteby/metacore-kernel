package licensing

import (
	"crypto/ed25519"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/licensing/sigv2"
	"github.com/google/uuid"
)

// NewConfigFromEnv reads the standard metacore licensing environment into a
// Config so an existing host (ops today) migrates onto this primitive without
// changing its deployment. The embedder must still set Store (the only
// non-env-derivable field) before calling New; any field can be overridden
// afterwards.
//
//	LICENSING_ENFORCE       "1"|"true"|"yes"|"on" → gates active (default off)
//	LICENSING_GRACE_DAYS    integer days of post-expiry grace (default 14)
//	OPS_LICENSE_PUBKEY      hub Ed25519 license pubkey (hex); else fetched
//	OPS_LICENSE_TOKEN       bootstrap token (env source)
//	OPS_LICENSE_FILE        path to a file holding a bootstrap token
//	OPS_INSTANCE_ID         hub-assigned instance UUID (renew handshake)
//	OPS_INSTANCE_PRIV_KEY   hex Ed25519 private key (renew handshake)
//	OPS_VERSION             build version reported on each check-in
//	HUB_BASE_URL            hub base (default https://hub.asteby.com)
//	LICENSING_REQUIRE_V2    "1"|"true"|… → refuse the legacy key-less token
//	                        format (downgrade protection; default off)
//	LICENSING_LEASE_COUNTER "1"|"true"|… → renew echoes the lease counter so the
//	                        hub can flag cloned identities (default off)
func NewConfigFromEnv() Config {
	cfg := Config{
		Enforce:    isTruthy(os.Getenv("LICENSING_ENFORCE")),
		HubBaseURL: strings.TrimSpace(os.Getenv("HUB_BASE_URL")),
		PubKeyHex:  strings.TrimSpace(os.Getenv("OPS_LICENSE_PUBKEY")),
		Version:    strings.TrimSpace(os.Getenv("OPS_VERSION")),

		RequireV2:    isTruthy(os.Getenv("LICENSING_REQUIRE_V2")),
		LeaseCounter: isTruthy(os.Getenv("LICENSING_LEASE_COUNTER")),
	}
	if v := strings.TrimSpace(os.Getenv("LICENSING_GRACE_DAYS")); v != "" {
		if d, err := strconv.Atoi(v); err == nil && d >= 0 {
			cfg.Grace = time.Duration(d) * 24 * time.Hour
		}
	}
	if raw := strings.TrimSpace(os.Getenv("OPS_INSTANCE_ID")); raw != "" {
		if id, err := uuid.Parse(raw); err == nil {
			cfg.InstanceID = id
		}
	}
	if raw := strings.TrimSpace(os.Getenv("OPS_INSTANCE_PRIV_KEY")); raw != "" {
		if b, err := hex.DecodeString(raw); err == nil && len(b) == ed25519.PrivateKeySize {
			cfg.InstancePriv = ed25519.PrivateKey(b)
		}
	}
	if tok, src := bootstrapTokenFromEnv(); tok != "" {
		cfg.BootstrapToken = tok
		cfg.BootstrapSource = src
	}
	return cfg
}

// bootstrapTokenFromEnv resolves a provisioning token from the environment: an
// inline token wins over a file path.
func bootstrapTokenFromEnv() (token, source string) {
	if t := strings.TrimSpace(os.Getenv("OPS_LICENSE_TOKEN")); t != "" {
		return t, "env"
	}
	if p := strings.TrimSpace(os.Getenv("OPS_LICENSE_FILE")); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(b)), "file"
		}
	}
	return "", ""
}

func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// StatusTrackerConfigFromEnv reads the revocation-list policy from the
// environment (keys must be supplied by the caller):
//
//	LICENSING_STATUS_STALE_POLICY  warn (default) | readonly | block | ignore
//	LICENSING_STATUS_MARGIN_HOURS  hours past valid_until still treated as
//	                               fresh (default 168 = 7 days; 0 → default)
func StatusTrackerConfigFromEnv(keys sigv2.Keyring) sigv2.TrackerConfig {
	cfg := sigv2.TrackerConfig{
		Keys:   keys,
		Policy: sigv2.ParseStalePolicy(os.Getenv("LICENSING_STATUS_STALE_POLICY")),
	}
	if v := strings.TrimSpace(os.Getenv("LICENSING_STATUS_MARGIN_HOURS")); v != "" {
		if h, err := strconv.Atoi(v); err == nil && h > 0 {
			cfg.Margin = time.Duration(h) * time.Hour
		}
	}
	return cfg
}

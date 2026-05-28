// central_trust.go — Central marketplace trust anchor (Let's Encrypt-style).
//
// Background — why this exists.
//
// The historical kernel trust model required hosts to enumerate every
// trusted Ed25519 publisher key via MARKETPLACE_PUBKEY / MARKETPLACE_PUBKEYS.
// In practice the marketplace (hub) has MANY publishing developers (one
// keypair per registered developer), so listing them statically is both
// impractical and stale the moment a new developer registers. The fix is
// the same pattern Let's Encrypt uses for TLS: the marketplace operator
// COUNTER-signs every served bundle with ONE long-lived "marketplace"
// keypair and publishes that pubkey at a well-known URL. Consumers trust
// ONE key (or fetch it on boot) instead of the union of every publisher.
//
// The original publisher signature (manifest.Signature) is still recorded
// for provenance and audit, but the installer's trust anchor is the
// CENTRAL marketplace signature carried alongside the bundle bytes.
//
// Configuration matrix the installer honours, in priority order:
//
//   1. MARKETPLACE_PUBKEY / MARKETPLACE_PUBKEYS env (manual override) —
//      used as-is. Customer-on-VPS or air-gapped deployments that pin a
//      specific key for reproducible builds set this.
//   2. MARKETPLACE_URL env (default https://hub.asteby.com) — when no
//      pubkey env is set, the installer fetches GET
//      {MARKETPLACE_URL}/v1/marketplace/pubkey at construction time and
//      caches the returned hex. This is the default SaaS path; new hosts
//      need no extra configuration.
//   3. Nothing configured + ALLOW_UNSIGNED_BUNDLES=true — sideloading
//      escape hatch (unchanged from the pre-central-anchor behaviour).
//   4. Nothing configured + ALLOW_UNSIGNED_BUNDLES unset — Install
//      refuses every bundle (fail-closed, unchanged).
//
// The fetch is best-effort: a non-2xx response or network error logs a
// warning and leaves PublicKeys empty so the existing fail-closed path
// catches the misconfiguration at the first Install attempt rather than
// crashing the host on boot. Operators with strict boot-time invariants
// can set MARKETPLACE_PUBKEY explicitly and the fetch is skipped.
package installer

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// DefaultMarketplaceURL is the production hub. Overridden by the
// MARKETPLACE_URL env var. Kept as a package-level var (not a const) so
// tests can swap it without juggling env vars.
var DefaultMarketplaceURL = "https://hub.asteby.com"

// marketplacePubKeyResponse mirrors the JSON shape served by
// hub/backend/internal/api/marketplace_pubkey.go. Defensive: extra fields
// are ignored, the "pubkey" key is the one we need.
type marketplacePubKeyResponse struct {
	PubKey    string `json:"pubkey"`
	Algorithm string `json:"algorithm"`
	FetchedAt string `json:"fetched_at,omitempty"`
}

// FetchCentralPubKey performs the HTTP GET against
// {baseURL}/v1/marketplace/pubkey and decodes the response. The caller
// owns the timeout policy via ctx. Returns the hex-encoded pubkey from
// the JSON body; algorithm is verified to be ed25519 (case-insensitive)
// because the kernel only knows how to verify Ed25519 today.
//
// Exported so a host that wants to refresh the trust anchor at runtime
// (key rotation without a restart) can drive it without re-deriving the
// URL conventions.
func FetchCentralPubKey(ctx context.Context, baseURL string) (string, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return "", fmt.Errorf("installer: empty marketplace URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/marketplace/pubkey", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "metacore-installer/1")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("installer: GET marketplace pubkey: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusServiceUnavailable {
		// Hub has the endpoint but no signing seed is configured — the
		// operator hasn't finished setting up the trust anchor yet. The
		// caller treats this as "no key fetched" and the installer's
		// fail-closed gate trips on the first install.
		return "", fmt.Errorf("installer: marketplace pubkey not configured at %s (HTTP 503)", baseURL)
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("installer: marketplace pubkey HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out marketplacePubKeyResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("installer: decode marketplace pubkey: %w", err)
	}
	alg := strings.ToLower(strings.TrimSpace(out.Algorithm))
	if alg != "" && alg != "ed25519" {
		return "", fmt.Errorf("installer: marketplace pubkey algorithm %q not supported (only ed25519)", out.Algorithm)
	}
	hexKey := strings.TrimSpace(out.PubKey)
	if hexKey == "" {
		return "", fmt.Errorf("installer: marketplace pubkey response missing pubkey field")
	}
	raw, err := hex.DecodeString(hexKey)
	if err != nil {
		return "", fmt.Errorf("installer: marketplace pubkey is not hex: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return "", fmt.Errorf("installer: marketplace pubkey has %d bytes, want %d",
			len(raw), ed25519.PublicKeySize)
	}
	return hexKey, nil
}

// loadCentralPubKeyIfNeeded augments envKeys with the central marketplace
// pubkey when the operator did NOT supply MARKETPLACE_PUBKEY[S] but DID
// leave MARKETPLACE_URL pointing at a hub (defaulting to
// DefaultMarketplaceURL). The fetch is best-effort: a failure logs a
// warning and returns envKeys unchanged so the existing fail-closed
// behaviour catches the misconfiguration at the first Install rather
// than crashing the host at boot.
//
// Called exactly once from New(); subsequent re-fetches are the host's
// responsibility (e.g. on a rotation event) via the exported
// FetchCentralPubKey + Installer.AppendTrustedPubKey.
func loadCentralPubKeyIfNeeded(envKeys []ed25519.PublicKey) []ed25519.PublicKey {
	if len(envKeys) > 0 {
		// Operator pinned at least one key explicitly. Respect the pin —
		// the central fetch is for hosts that want zero-config trust.
		return envKeys
	}
	baseURL := strings.TrimSpace(os.Getenv("MARKETPLACE_URL"))
	if baseURL == "" {
		baseURL = DefaultMarketplaceURL
	}
	if baseURL == "" {
		// Both env and default are empty (a test that clears the default
		// for an offline-only scenario). Skip silently.
		return envKeys
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hexKey, err := FetchCentralPubKey(ctx, baseURL)
	if err != nil {
		// Log and continue — the kernel still fails closed on the first
		// Install when PublicKeys is empty, so a misconfiguration is
		// observable rather than silent.
		slog.Warn("installer.marketplace_pubkey_fetch_failed",
			"url", baseURL,
			"err", err.Error(),
			"hint", "set MARKETPLACE_PUBKEY explicitly or check network reachability")
		return envKeys
	}
	raw, err := hex.DecodeString(hexKey)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		// FetchCentralPubKey already validated this; defensive.
		slog.Warn("installer.marketplace_pubkey_invalid",
			"url", baseURL,
			"err", "decoded pubkey failed length/hex sanity")
		return envKeys
	}
	slog.Info("installer.marketplace_pubkey_loaded",
		"url", baseURL,
		"pubkey", hexKey)
	return append(envKeys, ed25519.PublicKey(raw))
}

// AppendTrustedPubKey is the runtime knob a host calls when the
// marketplace operator rotates the central key (or to add a sideloaded
// developer pubkey to a hot Installer without a restart). The kernel
// preserves the "verify under ANY trusted key" semantics so old in-flight
// bundles signed by the previous key continue to verify until the host
// drops it on the next deploy.
func (i *Installer) AppendTrustedPubKey(hexKey string) error {
	raw, err := hex.DecodeString(strings.TrimSpace(hexKey))
	if err != nil {
		return fmt.Errorf("installer: trusted pubkey is not hex: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return fmt.Errorf("installer: trusted pubkey has %d bytes, want %d",
			len(raw), ed25519.PublicKeySize)
	}
	i.PublicKeys = append(i.PublicKeys, ed25519.PublicKey(raw))
	return nil
}

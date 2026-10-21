package licensing

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/asteby/metacore-kernel/licensing/sigv2"
	"github.com/google/uuid"
)

// DefaultGrace is how long an EXPIRED instance license keeps the instance in a
// degraded-but-writable state before writes are blocked. Chosen generously so
// a lapsed renewal (e.g. a payment hiccup) never hard-locks a customer without
// warning. Overridable via Config.Grace / LICENSING_GRACE_DAYS.
const DefaultGrace = 14 * 24 * time.Hour

// DefaultRevokeGrace is how long an instance keeps operating after the hub
// first refuses a renew as "license revoked", before it turns read-only. It
// protects a customer from a mistaken revocation (the issuer can fix it within
// the window) without leaving revocation toothless. Overridable via
// Config.RevokeGrace / LICENSING_REVOKE_GRACE_HOURS.
const DefaultRevokeGrace = 72 * time.Hour

// ErrRevoked is returned by the renew handshake when the hub refuses the
// license as revoked. Distinct from any other renew failure (network, bad
// signature, instance not paired), which never starts the revoke clock.
var ErrRevoked = errors.New("licensing: license revoked by the hub")

// DefaultRecheck is the background re-verification + renew cadence — the
// heartbeat that doubles as fleet telemetry and makes remote revocation
// effective within one lease window.
const DefaultRecheck = 6 * time.Hour

// DefaultHubBaseURL is the metacore marketplace/trust-anchor host.
const DefaultHubBaseURL = "https://hub.asteby.com"

// renewThreshold: renew when a non-leased license is within this window of
// expiry. Leased licenses renew on every cycle regardless.
const renewThreshold = 30 * 24 * time.Hour

// Config configures a Service. The embedder supplies a LicenseStore and,
// typically, an enforce flag + hub pubkey; everything else has a sensible
// default. Use NewConfigFromEnv to populate it from the standard env vars.
type Config struct {
	// Enforce turns the gates from advisory to blocking.
	Enforce bool
	// Grace is the post-expiry degraded window. Zero → DefaultGrace.
	Grace time.Duration
	// HubBaseURL is the trust-anchor host. Empty → DefaultHubBaseURL.
	HubBaseURL string
	// PubKeyHex is the hub Ed25519 license pubkey (hex). Empty → fetched from
	// HubBaseURL/v1/license/pubkey and cached. Bake it for offline appliances.
	PubKeyHex string
	// InstanceID + InstancePriv are the instance identity for the renew
	// (check-in) handshake. Absent → the service verifies + gates but never
	// renews (offline appliance running on its local window).
	InstanceID   uuid.UUID
	InstancePriv ed25519.PrivateKey
	// BootstrapToken is a token adopted on first Boot when the store is empty
	// (env/file provisioning). Empty → nothing to bootstrap.
	BootstrapToken string
	// BootstrapSource labels where BootstrapToken came from ("env"/"file").
	BootstrapSource string
	// Version is the build version reported on each check-in (fleet tracking).
	Version string

	// Keys are EXTRA trusted keys (typically a sigv2.KeySet) used to verify
	// v2 "asteby1." tokens whose kid is not the legacy hub key. Nil → only the
	// legacy hub key (PubKeyHex) is trusted, exactly as before.
	Keys sigv2.Keyring
	// RequireV2 refuses the legacy key-less token format (downgrade
	// protection). Default false: both formats verify.
	RequireV2 bool
	// LeaseCounter makes renew echo the monotonic lease counter carried by the
	// current token (field "lease_ctr"), so the hub can detect two copies of
	// one identity. Default false: no "lease_ctr" is sent. (The renew body does
	// always carry "token_formats" and "version", whatever this flag says.)
	LeaseCounter bool
	// RetireLegacyKey, together with Keys, stops trusting the hub's legacy
	// pubkey (kid "legacy" / its derived kid): only the key set verifies. This
	// is what lets a key-set rotation actually retire the old hub key. Without
	// Keys it is ignored (there would be nothing left to trust). Default false:
	// the legacy key keeps verifying behind the key set, as before.
	RetireLegacyKey bool
	// Store persists the singleton license Record. Required in practice; a nil
	// store yields a verify-only service that never persists.
	Store LicenseStore

	// Recheck overrides the background loop cadence. Zero → DefaultRecheck.
	Recheck time.Duration
	HTTP    *http.Client
	Logger  *slog.Logger
	// Now overrides the clock (tests). Nil → time.Now().UTC.
	Now func() time.Time

	// RevokeEnforce makes a hub revocation take effect: after RevokeGrace the
	// State becomes status "revoked" (not Valid, not InGrace), which with
	// Enforce on blocks writes (read-only). Off by default: the revocation is
	// still recorded and exposed on State (Revoked, RevokedAt,
	// RevokeGraceUntil) but nothing changes for the instance.
	RevokeEnforce bool
	// RevokeGrace is the window from the first observed revocation to
	// read-only. Zero → DefaultRevokeGrace, unless RevokeGraceSet is true
	// (then zero means "read-only immediately").
	RevokeGrace    time.Duration
	RevokeGraceSet bool
}

// Service loads, verifies, caches, re-checks and renews the instance license.
// The zero value is not usable — construct via New.
type Service struct {
	store   LicenseStore
	log     *slog.Logger
	http    *http.Client
	hubBase string

	enforce       bool
	grace         time.Duration
	recheck       time.Duration
	revokeEnforce bool
	revokeGrace   time.Duration

	instanceID   uuid.UUID
	instancePriv ed25519.PrivateKey
	version      string

	bootstrapToken  string
	bootstrapSource string

	extraKeys    sigv2.Keyring
	pol          sigv2.Policy
	leaseCounter bool
	retireLegacy bool

	now func() time.Time

	mu        sync.RWMutex
	pubKeyHex string
	state     *State
	// revokedMem mirrors Record.RevokedAt for stores that do not persist it
	// (an embedder not yet updated for the field): the anchor then survives
	// every refresh cycle of THIS process, and only a restart resets it.
	revokedMem *time.Time
}

// New builds a Service from Config, filling defaults. It does not touch the
// store or the network; call Boot then Start.
func New(cfg Config) *Service {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	grace := cfg.Grace
	if grace <= 0 {
		grace = DefaultGrace
	}
	recheck := cfg.Recheck
	if recheck <= 0 {
		recheck = DefaultRecheck
	}
	revokeGrace := cfg.RevokeGrace
	if revokeGrace <= 0 && !cfg.RevokeGraceSet {
		revokeGrace = DefaultRevokeGrace
	}
	base := strings.TrimSpace(cfg.HubBaseURL)
	if base == "" {
		base = DefaultHubBaseURL
	}
	src := cfg.BootstrapSource
	if src == "" && strings.TrimSpace(cfg.BootstrapToken) != "" {
		src = "config"
	}
	s := &Service{
		store:           cfg.Store,
		log:             log,
		http:            hc,
		hubBase:         strings.TrimRight(base, "/"),
		enforce:         cfg.Enforce,
		grace:           grace,
		recheck:         recheck,
		revokeEnforce:   cfg.RevokeEnforce,
		revokeGrace:     revokeGrace,
		instanceID:      cfg.InstanceID,
		instancePriv:    cfg.InstancePriv,
		version:         strings.TrimSpace(cfg.Version),
		bootstrapToken:  strings.TrimSpace(cfg.BootstrapToken),
		bootstrapSource: src,
		pubKeyHex:       strings.TrimSpace(cfg.PubKeyHex),
		extraKeys:       cfg.Keys,
		pol:             sigv2.Policy{RequireV2: cfg.RequireV2},
		leaseCounter:    cfg.LeaseCounter,
		retireLegacy:    cfg.RetireLegacyKey && cfg.Keys != nil,
		now:             now,
	}
	// Seed an empty snapshot so Current() never returns nil before Boot.
	s.store2(&State{Enforced: s.enforce, Status: "missing", Entitlements: []string{}})
	return s
}

// Enforced reports whether gates are active.
func (s *Service) Enforced() bool { return s.enforce }

// Current returns the latest snapshot. Never nil.
func (s *Service) Current() *State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

func (s *Service) store2(st *State) {
	s.mu.Lock()
	s.state = st
	s.mu.Unlock()
}

// Boot performs the first load: adopt a persisted token if present, otherwise
// bootstrap from Config.BootstrapToken, verify, and publish the snapshot. It
// never returns an error that should stop startup — a bad/missing license
// produces a degraded snapshot, and enforcement decides consequences.
func (s *Service) Boot(ctx context.Context) {
	row, err := s.load(ctx)
	if err != nil {
		s.log.Warn("licensing: load persisted token failed", "err", err)
	}
	if row == nil && s.bootstrapToken != "" {
		if adopted, aerr := s.persist(ctx, s.bootstrapToken, s.bootstrapSource); aerr != nil {
			s.log.Warn("licensing: bootstrap token rejected", "source", s.bootstrapSource, "err", aerr)
		} else {
			row = adopted
		}
	}
	s.recompute(ctx, row)
}

// Start launches the background re-check + renew loop. Cancel ctx to stop.
func (s *Service) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(s.recheck)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.RefreshNow(ctx)
			}
		}
	}()
}

// RefreshNow re-verifies the persisted token and attempts a renewal when it is
// nearing expiry (or already leased/in grace) and the instance is linked +
// online.
func (s *Service) RefreshNow(ctx context.Context) {
	row, err := s.load(ctx)
	if err != nil {
		s.log.Warn("licensing: refresh load failed", "err", err)
	}
	if row != nil && s.shouldRenew(row) {
		if renewed, rerr := s.renew(ctx); rerr != nil {
			if errors.Is(rerr, ErrRevoked) {
				row = s.markRevoked(ctx, row)
			} else {
				s.log.Info("licensing: renew skipped/failed", "err", rerr)
			}
		} else if renewed != "" {
			if updated, perr := s.persist(ctx, renewed, "renew"); perr == nil {
				row = updated
				s.log.Info("licensing: license renewed", "expires_at", row.ExpiresAt)
			}
		}
	}
	s.recompute(ctx, row)
}

// Activate verifies a pasted token, persists it on success, and republishes
// the snapshot. Returns the fresh State or a verification error.
func (s *Service) Activate(ctx context.Context, token string) (*State, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("empty license token")
	}
	pub, err := s.pubKey(ctx)
	if err != nil {
		return nil, err
	}
	// Accept a currently-valid token; also accept an already-expired but
	// signature-authentic token (an admin may paste a renewal that the clock
	// or a stale copy makes look expired) — the snapshot will show its status.
	if _, verr := s.verify(token, pub, s.now()); verr != nil && !errors.Is(verr, ErrExpired) {
		return nil, verr
	}
	row, err := s.persist(ctx, token, "admin")
	if err != nil {
		return nil, err
	}
	s.recompute(ctx, row)
	return s.Current(), nil
}

// recompute verifies row's token and swaps in a fresh snapshot.
func (s *Service) recompute(ctx context.Context, row *Record) {
	st := s.compute(ctx, row)
	s.applyRevocation(ctx, st, row)
	s.store2(st)
}

// compute builds the snapshot for row from its token alone (no revocation).
func (s *Service) compute(ctx context.Context, row *Record) *State {
	now := s.now()
	st := &State{Enforced: s.enforce, Entitlements: []string{}, LastCheckedAt: &now}
	if row == nil || row.Token == "" {
		st.Status = "missing"
		st.Reason = "no license installed"
		return st
	}
	pub, err := s.pubKey(ctx)
	if err != nil {
		// Can't fetch the pubkey and none baked: cannot verify. Fall back to
		// the persisted snapshot's window so an offline instance still runs on
		// its last decoded claims (signature was checked at persist time).
		s.log.Warn("licensing: pubkey unavailable, using persisted snapshot", "err", err)
		s.fillFromRow(st, row, now)
		return st
	}
	claims, verr := s.verify(row.Token, pub, now)
	switch {
	case verr == nil:
		s.fillValid(st, claims, now)
		s.markValidated(ctx, row, now)
	case errors.Is(verr, ErrExpired) && claims != nil:
		s.fillExpired(st, claims, now)
	default:
		st.Status = "invalid"
		st.Reason = verr.Error()
	}
	return st
}

// markRevoked records the FIRST observed revocation on the persisted Record
// (never moves it later: the grace clock must not reset on every cycle or on a
// restart) and returns the updated row.
func (s *Service) markRevoked(ctx context.Context, row *Record) *Record {
	if row == nil {
		return row
	}
	at, readOK := s.revokeAnchor(ctx, row)
	if at != nil {
		row.RevokedAt = at
		// The anchor came from the in-process mirror, and the side channel read
		// fine yet holds nothing: an earlier read or write failed, so the real
		// first-observed instant lives only in memory. Persist it now (and on
		// every later cycle until the write sticks) or a restart would restart
		// the grace.
		if readOK && s.store != nil && s.anchorOnlyInMemory(at) {
			s.saveAnchor(ctx, at)
		}
		return row
	}
	now := s.now()
	// The anchor is compared later against the hub-clock iat of re-pasted
	// tokens: a box clock set behind the hub's must not leave the anchor
	// earlier than the token we already hold, or pasting that very token
	// again would look like a "newer" one and lift the revocation.
	if row.IssuedAt != nil && row.IssuedAt.After(now) {
		now = *row.IssuedAt
	}
	s.mu.Lock()
	s.revokedMem = &now
	s.mu.Unlock()
	row.RevokedAt = &now
	s.log.Warn("licensing: hub refused the renew as revoked", "revoke_grace", s.revokeGrace.String(), "enforced", s.revokeEnforce)
	if !readOK {
		// The stored anchor could not be read: it may well exist. Writing a
		// fresh one now would overwrite the real (older) first-observed
		// instant and restart the grace, so only keep it in memory.
		return row
	}
	if s.store != nil {
		if err := s.store.Save(ctx, row); err != nil {
			s.log.Warn("licensing: persist revocation failed", "err", err)
		}
		s.saveAnchor(ctx, &now)
	}
	return row
}

// anchorOnlyInMemory reports whether at is the in-process mirror itself (not a
// copy read from the Record or the store).
func (s *Service) anchorOnlyInMemory(at *time.Time) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.revokedMem != nil && s.revokedMem == at
}

// revokeAnchor resolves the first-observed revocation instant from, in order:
// the Record itself, the store's RevokeAnchorStore side channel (for stores
// whose Record cannot carry the field, e.g. a GORM table), and the in-process
// mirror. Nil when the instance is not revoked.
//
// The bool is false when the side channel failed to read: the answer is then
// unknown (not "not revoked"), and callers must not overwrite or clear the
// stored anchor on the strength of it.
func (s *Service) revokeAnchor(ctx context.Context, row *Record) (*time.Time, bool) {
	if row != nil && row.RevokedAt != nil {
		return row.RevokedAt, true
	}
	readOK := true
	if as, ok := s.store.(RevokeAnchorStore); ok {
		if at, err := as.LoadRevokeAnchor(ctx); err != nil {
			readOK = false
			s.log.Warn("licensing: load revoke anchor failed", "err", err)
		} else if at != nil {
			return at, true
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.revokedMem, readOK
}

// saveAnchor writes (or clears, with nil) the anchor through the optional
// RevokeAnchorStore side channel. A no-op for stores that do not implement it.
func (s *Service) saveAnchor(ctx context.Context, at *time.Time) {
	if as, ok := s.store.(RevokeAnchorStore); ok {
		if err := as.SaveRevokeAnchor(ctx, at); err != nil {
			s.log.Warn("licensing: persist revoke anchor failed", "err", err)
		}
	}
}

// applyRevocation overlays the revocation onto a computed snapshot. The
// informational fields are always filled; the posture (Status/Valid/InGrace)
// only changes when the revoke policy is enforced AND the revoke grace elapsed.
func (s *Service) applyRevocation(ctx context.Context, st *State, row *Record) {
	if row != nil && row.RevokedAt == nil {
		row.RevokedAt, _ = s.revokeAnchor(ctx, row)
	}
	if row == nil || row.RevokedAt == nil || !st.Configured {
		return
	}
	revokedAt := *row.RevokedAt
	until := revokedAt.Add(s.revokeGrace)
	st.Revoked = true
	st.RevokedAt = &revokedAt
	st.RevokeGraceUntil = &until
	if !s.revokeEnforce {
		return
	}
	if s.now().Before(until) {
		// Warning window: still operable, but the UI must say so.
		st.Reason = "license revoked by the issuer; read-only from " + until.UTC().Format(time.RFC3339)
		return
	}
	st.Valid = false
	st.InGrace = false
	st.Stale = false
	st.Status = "revoked"
	st.Reason = "license revoked by the issuer"
}

// fillCommon copies the identity + entitlement fields shared by every posture.
func fillCommon(st *State, c *Claims) {
	st.Configured = true
	st.OrgID = c.CustomerID.String()
	st.Plan = c.Plan
	if len(c.Presets) > 0 {
		st.Preset = c.Presets[0]
	}
	st.Entitlements = c.Entitlements()
	st.Wildcard = c.Wildcard()
	st.MaxOfflineHours = c.MaxOfflineHours
	ia, ea := c.IssuedAt, c.ExpiresAt
	st.IssuedAt, st.ExpiresAt = &ia, &ea
}

func (s *Service) fillValid(st *State, c *Claims, now time.Time) {
	fillCommon(st, c)
	st.Valid = true
	st.Status = "valid"
	st.DaysRemaining = daysBetween(now, c.ExpiresAt)
	// Lease posture: still operable (Valid stays true so writes are not
	// blocked) but flagged — the UI nags and the renew loop must succeed.
	if c.LeaseExpired(now) {
		st.Stale = true
		st.Status = "stale"
		st.Reason = "missed check-in window (lease " + strconv.Itoa(c.MaxOfflineHours) + "h); renew required"
	}
}

func (s *Service) fillExpired(st *State, c *Claims, now time.Time) {
	fillCommon(st, c)
	// Grace: the signed claim wins; the config knob is the fallback for tokens
	// minted without one.
	graceUntil := c.ExpiresAt.Add(s.grace)
	if c.GraceDays > 0 {
		graceUntil = c.GraceUntil()
	}
	st.GraceUntil = &graceUntil
	if now.Before(graceUntil) {
		st.InGrace = true
		st.Status = "grace"
		st.DaysRemaining = daysBetween(now, graceUntil)
	} else {
		st.Status = "expired"
	}
	st.Reason = "license expired at " + c.ExpiresAt.UTC().Format(time.RFC3339)
}

// fillFromRow reconstructs a snapshot from the persisted decoded claims when
// the pubkey is unavailable (offline). The signature was verified at persist
// time, so this trusts the stored window.
func (s *Service) fillFromRow(st *State, row *Record, now time.Time) {
	st.Configured = true
	st.OrgID = row.OrgID
	st.Preset = row.Preset
	st.Entitlements = append([]string{}, row.EntitledAddons...)
	st.Wildcard = row.Wildcard()
	st.IssuedAt, st.ExpiresAt = row.IssuedAt, row.ExpiresAt
	if row.ExpiresAt == nil {
		st.Status = "invalid"
		st.Reason = "persisted license missing expiry"
		return
	}
	switch {
	case now.Before(*row.ExpiresAt):
		st.Valid = true
		st.Status = "valid"
		st.DaysRemaining = daysBetween(now, *row.ExpiresAt)
	default:
		graceUntil := row.ExpiresAt.Add(s.grace)
		st.GraceUntil = &graceUntil
		if now.Before(graceUntil) {
			st.InGrace = true
			st.Status = "grace"
			st.DaysRemaining = daysBetween(now, graceUntil)
		} else {
			st.Status = "expired"
		}
	}
}

// ---- persistence ----

func (s *Service) load(ctx context.Context) (*Record, error) {
	if s.store == nil {
		return nil, nil
	}
	return s.store.Load(ctx)
}

// persist verifies the signature (against the pubkey) enough to decode claims,
// then upserts the singleton record. It rejects tokens whose signature does not
// verify — but accepts an authentic-yet-expired token so a renewal chain never
// loses the current entitlement snapshot.
func (s *Service) persist(ctx context.Context, token, source string) (*Record, error) {
	if s.store == nil {
		return nil, errors.New("no license store configured")
	}
	pub, err := s.pubKey(ctx)
	if err != nil {
		return nil, err
	}
	claims, verr := s.decode(token, pub)
	if verr != nil {
		return nil, verr
	}
	now := s.now()
	ia, ea := claims.IssuedAt, claims.ExpiresAt
	preset := ""
	if len(claims.Presets) > 0 {
		preset = claims.Presets[0]
	}
	row := &Record{
		Token:          token,
		OrgID:          claims.CustomerID.String(),
		Preset:         preset,
		EntitledAddons: claims.Entitlements(),
		IssuedAt:       &ia,
		ExpiresAt:      &ea,
		ActivatedAt:    &now,
		ValidatedAt:    &now,
		Source:         source,
	}
	// A standing revocation is lifted ONLY by evidence the issuer reinstated
	// the license: a successful renew (source "renew": the hub answered 200)
	// or a token issued after the revocation was first observed. Re-pasting
	// the same (or any older) token must NOT restart the revoke grace.
	var keep *time.Time
	anchorKnown := true
	if source != "renew" {
		prev, _ := s.load(ctx)
		var at *time.Time
		at, anchorKnown = s.revokeAnchor(ctx, prev)
		if at != nil && !ia.After(*at) {
			keep = at
		}
		if !anchorKnown {
			// Unreadable anchor (transient store failure): neither lift nor
			// restart the revocation. Leave the stored anchor untouched.
			keep = at
		}
	}
	row.RevokedAt = keep
	if err := s.store.Save(ctx, row); err != nil {
		return nil, err
	}
	if anchorKnown {
		s.mu.Lock()
		s.revokedMem = keep
		s.mu.Unlock()
		s.saveAnchor(ctx, keep)
	}
	return row, nil
}

func (s *Service) markValidated(ctx context.Context, row *Record, now time.Time) {
	if s.store == nil || row == nil {
		return
	}
	row.ValidatedAt = &now
	if err := s.store.Save(ctx, row); err != nil {
		s.log.Debug("licensing: mark validated failed", "err", err)
	}
}

// ---- verification (both wire formats) ----

// ring builds the keyring for one verification: the extra keys first (a kid in
// the key set decides), then the legacy hub key.
func (s *Service) ring(pubKeyHex string) (sigv2.Keyring, error) {
	if s.retireLegacy && s.extraKeys != nil {
		return s.extraKeys, nil
	}
	legacy, err := legacyRing(pubKeyHex)
	if err != nil {
		if s.extraKeys != nil && errors.Is(err, ErrNoPubKey) {
			return s.extraKeys, nil
		}
		return nil, err
	}
	if s.extraKeys == nil {
		return legacy, nil
	}
	return sigv2.Rings{s.extraKeys, legacy}, nil
}

func (s *Service) verify(token, pubKeyHex string, now time.Time) (*Claims, error) {
	r, err := s.ring(pubKeyHex)
	if err != nil {
		return nil, err
	}
	return VerifyWith(token, r, s.pol, now)
}

func (s *Service) decode(token, pubKeyHex string) (*Claims, error) {
	r, err := s.ring(pubKeyHex)
	if err != nil {
		return nil, err
	}
	return decodeAndVerifyWith(token, r, s.pol, s.now())
}

// ---- pubkey resolution ----

func (s *Service) pubKey(ctx context.Context) (string, error) {
	s.mu.RLock()
	cached := s.pubKeyHex
	s.mu.RUnlock()
	if cached != "" {
		return cached, nil
	}
	// Fetch from the hub once and cache. Appliances should bake PubKeyHex so
	// they never take this network path.
	url := s.hubBase + "/v1/license/pubkey"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := s.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: fetch hub pubkey: %v", ErrNoPubKey, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: hub pubkey status %d", ErrNoPubKey, resp.StatusCode)
	}
	var body struct {
		Algorithm string `json:"algorithm"`
		PublicKey string `json:"public_key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("%w: decode hub pubkey: %v", ErrNoPubKey, err)
	}
	if strings.TrimSpace(body.PublicKey) == "" {
		return "", ErrNoPubKey
	}
	s.mu.Lock()
	s.pubKeyHex = strings.TrimSpace(body.PublicKey)
	s.mu.Unlock()
	return s.pubKeyHex, nil
}

// ---- renewal ----

// isRevokedBody tells a revocation 403 from the other 403s the renew can
// return (instance not paired, id mismatch). The current hub sends
// {"code":"license_revoked"}; older hubs only say "license revoked; renewal
// refused" in the error text.
func isRevokedBody(b []byte) bool {
	var body struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &body) == nil {
		if body.Code == "license_revoked" {
			return true
		}
		return strings.Contains(strings.ToLower(body.Error), "license revoked")
	}
	return strings.Contains(strings.ToLower(string(b)), "license revoked")
}

func (s *Service) shouldRenew(row *Record) bool {
	if s.instancePriv == nil || s.instanceID == uuid.Nil || row == nil || row.ExpiresAt == nil {
		return false
	}
	// A leased license renews on EVERY cycle — the renew is the mandatory
	// check-in that keeps it from going stale, regardless of how far the
	// expiry window still reaches.
	if st := s.Current(); st != nil && (st.Stale || st.MaxOfflineHours > 0) {
		return true
	}
	// With the revoke policy on, check in every cycle: a revocation must be
	// noticed within one recheck even if the token window is still far away.
	if s.revokeEnforce {
		return true
	}
	return s.now().Add(renewThreshold).After(*row.ExpiresAt)
}

// TokenFormatEnvelopeV2 is the token_formats value announcing that this kernel
// verifies the v2 "asteby1." envelope (sent on every renew).
const TokenFormatEnvelopeV2 = "asteby1"

// renewResponse mirrors the hub POST /v1/licenses/renew contract (see
// docs/licensing.md). The instance signs the request with its Ed25519 key so
// the hub authenticates it via the durable instance-signature path.
type renewResponse struct {
	LicenseToken string     `json:"license_token"`
	ExpiresAt    *time.Time `json:"expires_at"`
}

func (s *Service) renew(ctx context.Context) (string, error) {
	if s.instancePriv == nil || s.instanceID == uuid.Nil {
		return "", errors.New("instance not linked (no instance key); cannot renew")
	}
	// Body matches the hub's renewLicenseRequest: the instance names itself
	// (verified against the signature) and reports its build version so the
	// hub's check-in registry can track the fleet's version spread.
	payload := map[string]any{
		"instance_id": s.instanceID.String(),
		// This kernel verifies the v2 envelope: announce it so the hub may
		// answer with an "asteby1." token. A kernel without this field gets the
		// legacy two-part token no matter what the hub flag says.
		"token_formats": []string{TokenFormatEnvelopeV2},
	}
	if s.version != "" {
		payload["version"] = s.version
	}
	var body []byte
	var err error
	if s.leaseCounter {
		// Additive and opt-in: the counter lives in the SIGNED claims of the
		// token we hold, so no host store has to learn a new column.
		payload["lease_ctr"] = s.currentLeaseCounter(ctx)
		body, err = json.Marshal(payload)
	} else {
		body, err = json.Marshal(payload)
	}
	if err != nil {
		return "", err
	}
	url := s.hubBase + "/v1/licenses/renew"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	s.signRequest(req, body)

	resp, err := s.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if resp.StatusCode == http.StatusForbidden && isRevokedBody(b) {
			return "", ErrRevoked
		}
		return "", fmt.Errorf("hub renew status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out renewResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.LicenseToken), nil
}

// currentLeaseCounter reads the counter from the persisted token's claims (0
// when absent or unreadable: an unknown counter is reported as 0).
func (s *Service) currentLeaseCounter(ctx context.Context) uint64 {
	row, err := s.load(ctx)
	if err != nil || row == nil || row.Token == "" {
		return 0
	}
	pub, err := s.pubKey(ctx)
	if err != nil {
		return 0
	}
	c, err := s.decode(row.Token, pub)
	if err != nil || c == nil {
		return 0
	}
	return c.Counter
}

// signRequest adds the instance-signature headers over the exact body bytes,
// matching the hub's verifyInstanceSignature: message = sha256(body||"\n"||ts).
func (s *Service) signRequest(req *http.Request, body []byte) {
	if s.instancePriv == nil {
		return
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	h := sha256.New()
	h.Write(body)
	h.Write([]byte("\n"))
	h.Write([]byte(ts))
	sig := ed25519.Sign(s.instancePriv, h.Sum(nil))
	pub := s.instancePriv.Public().(ed25519.PublicKey)
	req.Header.Set("X-Signature", "ed25519:"+base64.StdEncoding.EncodeToString(sig))
	req.Header.Set("X-Signature-Timestamp", ts)
	req.Header.Set("X-Instance-Pubkey", hex.EncodeToString(pub))
}

// ---- helpers ----

func daysBetween(now, future time.Time) int {
	d := future.Sub(now)
	if d <= 0 {
		return 0
	}
	return int(d.Hours() / 24)
}

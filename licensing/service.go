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

	"github.com/google/uuid"
)

// DefaultGrace is how long an EXPIRED instance license keeps the instance in a
// degraded-but-writable state before writes are blocked. Chosen generously so
// a lapsed renewal (e.g. a payment hiccup) never hard-locks a customer without
// warning. Overridable via Config.Grace / LICENSING_GRACE_DAYS.
const DefaultGrace = 14 * 24 * time.Hour

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
	// Store persists the singleton license Record. Required in practice; a nil
	// store yields a verify-only service that never persists.
	Store LicenseStore

	// Recheck overrides the background loop cadence. Zero → DefaultRecheck.
	Recheck time.Duration
	HTTP    *http.Client
	Logger  *slog.Logger
	// Now overrides the clock (tests). Nil → time.Now().UTC.
	Now func() time.Time
}

// Service loads, verifies, caches, re-checks and renews the instance license.
// The zero value is not usable — construct via New.
type Service struct {
	store   LicenseStore
	log     *slog.Logger
	http    *http.Client
	hubBase string

	enforce bool
	grace   time.Duration
	recheck time.Duration

	instanceID   uuid.UUID
	instancePriv ed25519.PrivateKey
	version      string

	bootstrapToken  string
	bootstrapSource string

	now func() time.Time

	mu        sync.RWMutex
	pubKeyHex string
	state     *State
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
		instanceID:      cfg.InstanceID,
		instancePriv:    cfg.InstancePriv,
		version:         strings.TrimSpace(cfg.Version),
		bootstrapToken:  strings.TrimSpace(cfg.BootstrapToken),
		bootstrapSource: src,
		pubKeyHex:       strings.TrimSpace(cfg.PubKeyHex),
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
			s.log.Info("licensing: renew skipped/failed", "err", rerr)
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
	if _, verr := Verify(token, pub, s.now()); verr != nil && !errors.Is(verr, ErrExpired) {
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
	now := s.now()
	st := &State{Enforced: s.enforce, Entitlements: []string{}, LastCheckedAt: &now}
	if row == nil || row.Token == "" {
		st.Status = "missing"
		st.Reason = "no license installed"
		s.store2(st)
		return
	}
	pub, err := s.pubKey(ctx)
	if err != nil {
		// Can't fetch the pubkey and none baked: cannot verify. Fall back to
		// the persisted snapshot's window so an offline instance still runs on
		// its last decoded claims (signature was checked at persist time).
		s.log.Warn("licensing: pubkey unavailable, using persisted snapshot", "err", err)
		s.fillFromRow(st, row, now)
		s.store2(st)
		return
	}
	claims, verr := Verify(row.Token, pub, now)
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
	s.store2(st)
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
	claims, verr := decodeAndVerifySig(token, pub)
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
	if err := s.store.Save(ctx, row); err != nil {
		return nil, err
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
	return s.now().Add(renewThreshold).After(*row.ExpiresAt)
}

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
	payload := map[string]string{
		"instance_id": s.instanceID.String(),
	}
	if s.version != "" {
		payload["version"] = s.version
	}
	body, err := json.Marshal(payload)
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
		return "", fmt.Errorf("hub renew status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out renewResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.LicenseToken), nil
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

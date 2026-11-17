package inbound

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Defaults.
const (
	DefaultMaxBody     = 1 << 20 // 1 MiB
	DefaultLease       = 60 * time.Second
	DefaultExecTimeout = 15 * time.Second
	DefaultPrefix      = "/api/inbound/"
	maxSchemaDetails   = 10
)

// Config wires a Handler.
type Config struct {
	Contracts *Registry
	// Verifiers maps a contract's Auth profile name to its Verifier.
	Verifiers map[string]Verifier
	Receipts  ReceiptStore
	// Executor runs model routes (use *DynamicExecutor); Commands runs
	// command routes. At least the one the contracts need must be set.
	Executor Executor
	Commands *CommandRegistry

	MaxBodyBytes int64         // default 1 MiB
	Lease        time.Duration // processing reservation lease, default 60 s
	ExecTimeout  time.Duration // default 15 s
	// PreAuthLimiter is keyed by client IP and runs before anything else;
	// Limiter is keyed by organization and runs after authentication.
	// Both optional (nil = unlimited; the host's edge limiter may cover it).
	PreAuthLimiter Limiter
	Limiter        Limiter
	// ClientIP extracts the caller address for PreAuthLimiter. Default is the
	// host part of RemoteAddr (set it when behind a trusted proxy).
	ClientIP func(*http.Request) string
	// TrustedProxies are the reverse-proxy networks whose X-Forwarded-For is
	// honored by the default ClientIP (see ClientIPFrom). Empty = XFF ignored.
	TrustedProxies []netip.Prefix
	// DefaultAuth is the profile used to authenticate requests for contracts
	// that do not exist, so an unknown contract answers 401 to anyone who
	// cannot sign and 404 only to an authenticated caller. Empty = unknown
	// contracts always answer 401.
	DefaultAuth string
	// Prefix is the mount prefix, default "/api/inbound/".
	Prefix string
	Logger *slog.Logger
	Now    func() time.Time
}

// Handler is the generic POST /api/inbound/{org}/{contract} endpoint.
type Handler struct {
	cfg Config
}

// New validates the wiring and returns the handler. Every registered contract
// must reference a known auth profile, and its route must have an executor or
// a registered command; a bad contract is a startup error, not a runtime 500.
func New(cfg Config) (*Handler, error) {
	if cfg.Contracts == nil || cfg.Receipts == nil {
		return nil, errors.New("inbound: Contracts and Receipts are required")
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = DefaultMaxBody
	}
	if cfg.Lease <= 0 {
		cfg.Lease = DefaultLease
	}
	if cfg.ExecTimeout <= 0 {
		cfg.ExecTimeout = DefaultExecTimeout
	}
	if cfg.Prefix == "" {
		cfg.Prefix = DefaultPrefix
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	h := &Handler{cfg: cfg}
	return h, h.Check()
}

// Check re-validates the registered contracts against the wiring. Call it
// after loading contracts at runtime.
func (h *Handler) Check() error {
	var errs []error
	for _, c := range h.cfg.Contracts.all() {
		if err := h.checkOne(context.Background(), c); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Receipts exposes the log for a query endpoint on the host.
func (h *Handler) Receipts(ctx context.Context, org uuid.UUID, f ReceiptFilter) ([]Receipt, error) {
	return h.cfg.Receipts.List(ctx, org, f)
}

// ---------------------------------------------------------------- errors ---

// Error codes of the uniform error body.
const (
	CodeMethodNotAllowed  = "method_not_allowed"
	CodeNotFound          = "not_found"
	CodeUnknownContract   = "unknown_contract"
	CodeUnauthorized      = "unauthorized"
	CodeTooLarge          = "payload_too_large"
	CodeUnsupportedMedia  = "unsupported_media_type"
	CodeMalformedJSON     = "malformed_json"
	CodeInvalidSchema     = "invalid_schema"
	CodeMissingEventKey   = "invalid_event_key"
	CodeRateLimited       = "rate_limited"
	CodeIdempotencyClash  = "idempotency_conflict"
	CodeInProgress        = "in_progress"
	CodeInternal          = "internal_error"
	CodeContractMisconfig = "contract_misconfigured"
	CodeScopeRequired     = "scope_required"
)

// ErrorBody is the uniform error envelope: {"error": {...}}.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail is the content of ErrorBody. Details only carry JSON pointers
// and rule names, never payload values.
type ErrorDetail struct {
	Code    string       `json:"code"`
	Message string       `json:"message"`
	Details []FieldIssue `json:"details,omitempty"`
}

// FieldIssue points at the offending field and the failed schema keyword.
type FieldIssue struct {
	Path string `json:"path"`
	Rule string `json:"rule"`
}

// SuccessBody is the 200 envelope.
type SuccessBody struct {
	Status    string `json:"status"` // applied | replayed
	ReceiptID string `json:"receipt_id"`
	RecordID  string `json:"record_id,omitempty"`
	Created   bool   `json:"created,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string, details ...FieldIssue) {
	writeJSON(w, status, ErrorBody{Error: ErrorDetail{Code: code, Message: msg, Details: details}})
}

// ----------------------------------------------------------------- serve ---

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeErr(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "use POST")
		return
	}
	if l := h.cfg.PreAuthLimiter; l != nil && !l.Allow(LimiterKeyForIP(h.clientIP(r))) {
		w.Header().Set("Retry-After", "1")
		writeErr(w, http.StatusTooManyRequests, CodeRateLimited, "too many requests")
		return
	}
	org, key, ok := h.parsePath(r.URL.Path)
	if !ok {
		writeErr(w, http.StatusNotFound, CodeNotFound, "not found")
		return
	}
	version := 0
	badVersion := false
	if v := r.Header.Get("X-Contract-Version"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			badVersion = true
		}
		version = n
	}

	if r.ContentLength > h.cfg.MaxBodyBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, CodeTooLarge, "payload too large")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.cfg.MaxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, http.StatusRequestEntityTooLarge, CodeTooLarge, "payload too large")
		} else {
			writeErr(w, http.StatusBadRequest, CodeMalformedJSON, "could not read body")
		}
		return
	}

	// Authenticate BEFORE revealing whether the contract exists: a caller that
	// cannot sign gets 401 for known and unknown contracts alike.
	var c *compiled
	if !badVersion {
		c, _ = h.cfg.Contracts.get(key, version)
	}
	profile := h.cfg.DefaultAuth
	if c != nil {
		profile = c.Auth
	}
	ver, okv := h.cfg.Verifiers[profile]
	if !okv {
		if c != nil {
			h.cfg.Logger.Error("inbound: contract references missing auth profile", "contract", c.Key, "profile", c.Auth)
			writeErr(w, http.StatusInternalServerError, CodeContractMisconfig, "contract is not available")
			return
		}
		writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "authentication failed")
		return
	}
	vreq := VerifyRequest{Org: org, Method: r.Method, Path: r.URL.Path, Header: r.Header, Body: body}
	var commit func() error
	if pv, isPrepared := ver.(PreparedVerifier); isPrepared {
		commit, err = pv.Prepare(r.Context(), vreq)
	} else {
		err = ver.Verify(r.Context(), vreq)
	}
	if err != nil {
		h.cfg.Logger.Warn("inbound: auth rejected", "org", org, "profile", profile, "reason", err.Error())
		writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "authentication failed")
		return
	}
	if c == nil {
		writeErr(w, http.StatusNotFound, CodeUnknownContract, "unknown contract")
		return
	}

	// Cheap checks and the per-org limiter run BEFORE the nonce is consumed, so
	// a 415/422/429 never burns a nonce.
	if l := h.cfg.Limiter; l != nil && !l.Allow(org.String()) {
		w.Header().Set("Retry-After", "1")
		writeErr(w, http.StatusTooManyRequests, CodeRateLimited, "too many requests")
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeErr(w, http.StatusUnsupportedMediaType, CodeUnsupportedMedia, "Content-Type must be application/json")
		return
	}

	payload, err := decodeJSON(body)
	if err != nil {
		if errors.Is(err, errNumberRange) {
			writeErr(w, http.StatusUnprocessableEntity, CodeInvalidSchema, "payload does not satisfy the contract schema",
				FieldIssue{Path: "/", Rule: "number_range"})
			return
		}
		writeErr(w, http.StatusBadRequest, CodeMalformedJSON, "body is not valid JSON")
		return
	}
	sum := sha256.Sum256(body)
	base := Receipt{OrgID: org, Contract: c.Key, Version: c.Version, BodyHash: hex.EncodeToString(sum[:])}

	if verr := c.schema.Validate(payload); verr != nil {
		h.reject(r.Context(), base, CodeInvalidSchema)
		writeErr(w, http.StatusUnprocessableEntity, CodeInvalidSchema, "payload does not satisfy the contract schema", schemaIssues(verr)...)
		return
	}
	evKey, err := eventKey(c.tmpl, payload, r.Header)
	if err != nil {
		h.reject(r.Context(), base, CodeMissingEventKey)
		writeErr(w, http.StatusUnprocessableEntity, CodeMissingEventKey, "event key could not be derived")
		return
	}
	op := Operation{Org: org, Contract: c.Key, EventKey: evKey, Route: c.Route, Values: applyMapping(c.Route, payload)}
	if sc := c.Route.Scope; sc != nil {
		op.Branch = scopeValue(payload, sc.Branch)
		op.Entity = scopeValue(payload, sc.Entity)
		op.RequireBranch = sc.Branch != ""
		op.RequireEntity = sc.Entity != ""
		if (op.RequireBranch && op.Branch == "") || (op.RequireEntity && op.Entity == "") {
			h.reject(r.Context(), base, CodeScopeRequired)
			writeErr(w, http.StatusUnprocessableEntity, CodeScopeRequired, "the event must name its branch and entity")
			return
		}
	}

	// Last gate before side effects: consume the nonce.
	if commit != nil {
		if err := commit(); err != nil {
			h.cfg.Logger.Warn("inbound: auth rejected", "org", org, "profile", profile, "reason", err.Error())
			writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "authentication failed")
			return
		}
	}

	base.KeyHash = HashKey(org, c.Key, evKey)
	if c.Log.KeyInClear {
		base.EventKey = evKey
	}
	cur, claimed, err := h.cfg.Receipts.Begin(r.Context(), base, h.cfg.Lease)
	if err != nil {
		h.cfg.Logger.Error("inbound: receipt begin", "err_type", fmt.Sprintf("%T", err))
		writeErr(w, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}
	if !claimed {
		h.answerExisting(w, cur, base.BodyHash)
		return
	}

	out, xerr := h.execute(r.Context(), c, op)
	if xerr != nil {
		h.fail(w, r.Context(), cur, xerr)
		return
	}
	h.finish(r.Context(), cur, StatusApplied, out.RecordID, "")
	writeJSON(w, http.StatusOK, SuccessBody{Status: "applied", ReceiptID: cur.ID.String(), RecordID: out.RecordID, Created: out.Created})
}

// reject logs a rejected event. The store caps retained rejected rows per
// organization, so a client cannot grow the log without bound.
func (h *Handler) reject(ctx context.Context, base Receipt, code string) {
	base.Status, base.ErrorCode = StatusRejected, code
	if e := h.cfg.Receipts.Record(ctx, base); e != nil {
		h.cfg.Logger.Error("inbound: record rejected receipt", "err_type", fmt.Sprintf("%T", e))
	}
}

// finish closes a claimed receipt, fenced by the owning attempt when the store
// supports it: a slow worker whose lease expired cannot overwrite the one that
// re-claimed the event.
func (h *Handler) finish(ctx context.Context, cur Receipt, status, recordID, code string) {
	ctx = context.WithoutCancel(ctx)
	if af, ok := h.cfg.Receipts.(AttemptFinisher); ok {
		won, e := af.FinishAttempt(ctx, cur.ID, cur.Attempts, status, recordID, code)
		if e != nil {
			h.cfg.Logger.Error("inbound: receipt finish", "err_type", fmt.Sprintf("%T", e))
		} else if !won {
			h.cfg.Logger.Warn("inbound: receipt no longer owned by this attempt", "receipt", cur.ID, "attempt", cur.Attempts)
		}
		return
	}
	if e := h.cfg.Receipts.Finish(ctx, cur.ID, status, recordID, code); e != nil {
		h.cfg.Logger.Error("inbound: receipt finish", "err_type", fmt.Sprintf("%T", e))
	}
}

func scopeValue(payload any, path string) string {
	if path == "" {
		return ""
	}
	v, ok := lookup(payload, path)
	if !ok {
		return ""
	}
	s, _ := scalarString(v)
	return s
}

func (h *Handler) answerExisting(w http.ResponseWriter, cur Receipt, bodyHash string) {
	switch cur.Status {
	case StatusApplied:
		if cur.BodyHash != bodyHash {
			writeErr(w, http.StatusConflict, CodeIdempotencyClash, "event key already used with a different payload")
			return
		}
		writeJSON(w, http.StatusOK, SuccessBody{Status: "replayed", ReceiptID: cur.ID.String(), RecordID: cur.RecordID})
	default: // processing within lease
		w.Header().Set("Retry-After", "5")
		writeErr(w, http.StatusConflict, CodeInProgress, "event is being processed")
	}
}

func (h *Handler) execute(ctx context.Context, c *compiled, op Operation) (out Outcome, err error) {
	ctx, cancel := context.WithTimeout(ctx, h.cfg.ExecTimeout)
	defer cancel()
	defer func() {
		if rec := recover(); rec != nil {
			h.cfg.Logger.Error("inbound: executor panic", "contract", c.Key, "panic_type", fmt.Sprintf("%T", rec))
			out, err = Outcome{}, &ExecError{Kind: KindInternal, Code: "internal"}
		}
	}()
	if c.Route.Command != "" {
		fn, ok := h.cfg.Commands.get(c.Route.Command)
		if !ok {
			return Outcome{}, &ExecError{Kind: KindInternal, Code: "command_missing"}
		}
		return fn(ctx, op)
	}
	if h.cfg.Executor == nil {
		return Outcome{}, &ExecError{Kind: KindInternal, Code: "executor_unavailable"}
	}
	return h.cfg.Executor.Execute(ctx, op)
}

func (h *Handler) fail(w http.ResponseWriter, ctx context.Context, cur Receipt, err error) {
	id := cur.ID
	var ee *ExecError
	if !errors.As(err, &ee) {
		ee = &ExecError{Kind: KindInternal, Code: "internal", Err: err}
	}
	code := ee.Code
	if ee.Kind == KindInternal {
		code = "internal"
		// Types and codes only: error text may contain personal data.
		h.cfg.Logger.Error("inbound: execution failed", "receipt", id, "exec_code", ee.Code, "err_type", fmt.Sprintf("%T", ee.Err))
	}
	h.finish(ctx, cur, StatusFailed, "", code)
	switch ee.Kind {
	case KindInvalid:
		writeErr(w, http.StatusUnprocessableEntity, ee.Code, "the event was rejected by the target model")
	case KindConflict:
		writeErr(w, http.StatusConflict, ee.Code, "the event conflicts with the current state")
	case KindNotFound:
		writeErr(w, http.StatusNotFound, ee.Code, "target record not found")
	case KindForbidden:
		writeErr(w, http.StatusForbidden, ee.Code, "the event is outside the allowed scope")
	default:
		writeErr(w, http.StatusInternalServerError, CodeInternal, "internal error")
	}
}

func (h *Handler) clientIP(r *http.Request) string {
	if h.cfg.ClientIP != nil {
		return h.cfg.ClientIP(r)
	}
	return ClientIPFrom(r, h.cfg.TrustedProxies)
}

func (h *Handler) parsePath(p string) (uuid.UUID, string, bool) {
	rest, ok := strings.CutPrefix(p, h.cfg.Prefix)
	if !ok {
		return uuid.Nil, "", false
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 {
		return uuid.Nil, "", false
	}
	org, err := uuid.Parse(parts[0])
	if err != nil || !keyRe.MatchString(parts[1]) {
		return uuid.Nil, "", false
	}
	return org, parts[1], true
}

func decodeJSON(body []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data")
	}
	if err := checkNumbers(v); err != nil {
		return nil, err
	}
	return v, nil
}

var errNumberRange = errors.New("number out of range")

// checkNumbers rejects numbers that do not fit a float64 (1e999, which decodes
// to +Inf) so they never reach the mapping or the model as garbage.
func checkNumbers(v any) error {
	switch t := v.(type) {
	case json.Number:
		if _, err := t.Int64(); err == nil {
			return nil
		}
		f, err := t.Float64()
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return errNumberRange
		}
	case map[string]any:
		for _, x := range t {
			if err := checkNumbers(x); err != nil {
				return err
			}
		}
	case []any:
		for _, x := range t {
			if err := checkNumbers(x); err != nil {
				return err
			}
		}
	}
	return nil
}

// RegisterContract validates a contract against the wiring (auth profile,
// executor/command, and the target model via ContractChecker) and registers it
// only if everything passes. Use it for runtime loads instead of
// Registry.Register + Check.
func (h *Handler) RegisterContract(ctx context.Context, c Contract) error {
	cc, err := compile(c)
	if err != nil {
		return err
	}
	if err := h.checkOne(ctx, cc); err != nil {
		return err
	}
	h.cfg.Contracts.put(cc)
	return nil
}

func (h *Handler) checkOne(ctx context.Context, c *compiled) error {
	var errs []error
	if _, ok := h.cfg.Verifiers[c.Auth]; !ok {
		errs = append(errs, fmt.Errorf("contract %s v%d: unknown auth profile %q", c.Key, c.Version, c.Auth))
	}
	if c.Route.Command != "" {
		if _, ok := h.cfg.Commands.get(c.Route.Command); !ok {
			errs = append(errs, fmt.Errorf("contract %s v%d: command %q not registered", c.Key, c.Version, c.Route.Command))
		}
	} else if h.cfg.Executor == nil {
		errs = append(errs, fmt.Errorf("contract %s v%d: no Executor for model routes", c.Key, c.Version))
	} else if cc, ok := h.cfg.Executor.(ContractChecker); ok {
		if err := cc.CheckContract(ctx, c.Contract); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

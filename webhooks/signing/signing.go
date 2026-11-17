package signing

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Profile es un perfil de firma de la lista cerrada.
type Profile string

const (
	HMACSimple            Profile = "hmac_simple"
	HMACCanonicalV1       Profile = "hmac_canonical_v1"
	Bearer                Profile = "bearer"
	BearerHMACCanonicalV1 Profile = "bearer+hmac_canonical_v1"
)

// Profiles lista los perfiles soportados.
func Profiles() []Profile {
	return []Profile{HMACSimple, HMACCanonicalV1, Bearer, BearerHMACCanonicalV1}
}

// Cabeceras por omision.
const (
	DefaultSimpleHeader    = "X-Signature"
	DefaultTimestampHeader = "X-7L-Timestamp"
	DefaultNonceHeader     = "X-7L-Nonce"
	DefaultSignatureHeader = "X-7L-Signature"
	// DefaultMaxSkew es la ventana de reloj del contrato de V2 (+-300 s).
	DefaultMaxSkew = 300 * time.Second
)

// Spec es la declaracion (dato puro, serializable, sin secretos) de un perfil.
type Spec struct {
	Profile Profile `json:"profile"`
	// SecretRef nombra el secreto HMAC (no es el secreto). Lo resuelve el host.
	SecretRef string `json:"secret_ref,omitempty"`
	// KeyRef nombra el token Bearer (no es el token). Lo resuelve el host.
	KeyRef string `json:"key_ref,omitempty"`

	// Header: cabecera de hmac_simple (por omision X-Signature).
	Header string `json:"header,omitempty"`
	// Prefix: prefijo del valor de hmac_simple (p. ej. "sha256=").
	Prefix string `json:"prefix,omitempty"`
	// TimestampHeader / NonceHeader / SignatureHeader: nombres de las cabeceras
	// de hmac_canonical_v1 (por omision las de V2: X-7L-*).
	TimestampHeader string `json:"timestamp_header,omitempty"`
	NonceHeader     string `json:"nonce_header,omitempty"`
	SignatureHeader string `json:"signature_header,omitempty"`
}

// Secrets son los valores YA resueltos. Nunca se persisten ni se registran.
type Secrets struct {
	HMACKey     string
	BearerToken string
}

// Request es lo que se firma: metodo, ruta (sin query) y cuerpo exacto.
type Request struct {
	Method string
	Path   string
	Body   []byte
}

// PathOf devuelve la ruta tal como se firma: con "/" inicial y sin query.
func PathOf(u *url.URL) string {
	p := u.EscapedPath()
	if p == "" {
		return "/"
	}
	return p
}

// Errores de validacion/verificacion. CodeOf mapea los de verificacion a los
// codigos de V2.
var (
	ErrUnknownProfile = errors.New("signing: unknown profile")
	ErrBadSpec        = errors.New("signing: invalid spec")
	ErrMissingSecret  = errors.New("signing: missing secret")

	ErrUnauthenticated = &Error{code: "unauthenticated", msg: "missing or invalid bearer token"}
	ErrBadSignature    = &Error{code: "invalid_signature", msg: "signature mismatch"}
	ErrStaleTimestamp  = &Error{code: "stale_timestamp", msg: "timestamp outside window"}
	ErrNonceReused     = &Error{code: "nonce_reused", msg: "nonce already seen"}
	ErrBadRequest      = &Error{code: "bad_request", msg: "malformed signature headers"}
)

// Error es un fallo de verificacion con codigo estable.
type Error struct {
	code string
	msg  string
}

func (e *Error) Error() string { return "signing: " + e.msg }

// Code es el codigo estable ("invalid_signature", "stale_timestamp"...).
func (e *Error) Code() string { return e.code }

// CodeOf devuelve el codigo de un error de verificacion ("" si no lo es).
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.code
	}
	return ""
}

var (
	headerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)
	nonceRe    = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)
	refRe      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$`)
	reserved   = map[string]bool{
		"host": true, "content-length": true, "content-type": true, "transfer-encoding": true,
		"connection": true, "user-agent": true, "accept-encoding": true, "expect": true,
		"te": true, "trailer": true, "upgrade": true, "cookie": true, "proxy-authorization": true,
		"authorization": true,
	}
)

// ValidRef indica si ref tiene forma de referencia a secreto (no de valor).
func ValidRef(ref string) bool { return refRe.MatchString(ref) }

func (s Spec) needsHMAC() bool {
	return s.Profile == HMACSimple || s.Profile == HMACCanonicalV1 || s.Profile == BearerHMACCanonicalV1
}
func (s Spec) needsBearer() bool { return s.Profile == Bearer || s.Profile == BearerHMACCanonicalV1 }
func (s Spec) canonical() bool {
	return s.Profile == HMACCanonicalV1 || s.Profile == BearerHMACCanonicalV1
}

// Validate comprueba la declaracion (se llama AL GUARDAR): perfil conocido,
// referencias presentes y con forma de referencia, cabeceras legales y no
// reservadas.
func (s Spec) Validate() error {
	known := false
	for _, p := range Profiles() {
		if s.Profile == p {
			known = true
		}
	}
	if !known {
		return fmt.Errorf("%w: %q", ErrUnknownProfile, s.Profile)
	}
	if s.needsHMAC() {
		if s.SecretRef == "" {
			return fmt.Errorf("%w: profile %s requires secret_ref", ErrBadSpec, s.Profile)
		}
		if !ValidRef(s.SecretRef) {
			return fmt.Errorf("%w: secret_ref must be a reference, not a value", ErrBadSpec)
		}
	} else if s.SecretRef != "" {
		return fmt.Errorf("%w: profile %s takes no secret_ref", ErrBadSpec, s.Profile)
	}
	if s.needsBearer() {
		if s.KeyRef == "" {
			return fmt.Errorf("%w: profile %s requires key_ref", ErrBadSpec, s.Profile)
		}
		if !ValidRef(s.KeyRef) {
			return fmt.Errorf("%w: key_ref must be a reference, not a value", ErrBadSpec)
		}
	} else if s.KeyRef != "" {
		return fmt.Errorf("%w: profile %s takes no key_ref", ErrBadSpec, s.Profile)
	}
	if s.Profile != HMACSimple && (s.Header != "" || s.Prefix != "") {
		return fmt.Errorf("%w: header/prefix only apply to hmac_simple", ErrBadSpec)
	}
	if !s.canonical() && (s.TimestampHeader != "" || s.NonceHeader != "" || s.SignatureHeader != "") {
		return fmt.Errorf("%w: timestamp/nonce/signature headers only apply to hmac_canonical_v1", ErrBadSpec)
	}
	for _, h := range []string{s.Header, s.TimestampHeader, s.NonceHeader, s.SignatureHeader} {
		if h == "" {
			continue
		}
		if !headerName.MatchString(h) || reserved[strings.ToLower(h)] {
			return fmt.Errorf("%w: illegal header name %q", ErrBadSpec, h)
		}
	}
	if strings.ContainsAny(s.Prefix, "\r\n") || len(s.Prefix) > 32 {
		return fmt.Errorf("%w: illegal prefix", ErrBadSpec)
	}
	if s.canonical() {
		th, nh, sh := s.tsHeader(), s.nonceHeader(), s.sigHeader()
		if strings.EqualFold(th, nh) || strings.EqualFold(th, sh) || strings.EqualFold(nh, sh) {
			return fmt.Errorf("%w: canonical headers must be distinct", ErrBadSpec)
		}
	}
	return nil
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
func (s Spec) simpleHeader() string { return orDefault(s.Header, DefaultSimpleHeader) }
func (s Spec) tsHeader() string     { return orDefault(s.TimestampHeader, DefaultTimestampHeader) }
func (s Spec) nonceHeader() string  { return orDefault(s.NonceHeader, DefaultNonceHeader) }
func (s Spec) sigHeader() string    { return orDefault(s.SignatureHeader, DefaultSignatureHeader) }

// NewNonce genera un nonce valido (22 caracteres base64url).
func NewNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Canonical devuelve la cadena firmada por hmac_canonical_v1.
func Canonical(ts, nonce, method, path string, body []byte) string {
	sum := sha256.Sum256(body)
	return strings.Join([]string{ts, nonce, strings.ToUpper(method), path, hex.EncodeToString(sum[:])}, ".")
}

func mac(key string, msg []byte) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write(msg)
	return hex.EncodeToString(m.Sum(nil))
}

// Sign devuelve las cabeceras que firman req segun spec. now y nonce se
// inyectan (nonce vacio = NewNonce()). Ningun error incluye valores de secreto.
func Sign(spec Spec, sec Secrets, req Request, now time.Time, nonce string) (map[string]string, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if spec.needsHMAC() && sec.HMACKey == "" {
		return nil, fmt.Errorf("%w: hmac key", ErrMissingSecret)
	}
	if spec.needsBearer() && sec.BearerToken == "" {
		return nil, fmt.Errorf("%w: bearer token", ErrMissingSecret)
	}
	h := map[string]string{}
	if spec.needsBearer() {
		h["Authorization"] = "Bearer " + sec.BearerToken
	}
	switch {
	case spec.Profile == HMACSimple:
		h[spec.simpleHeader()] = spec.Prefix + mac(sec.HMACKey, req.Body)
	case spec.canonical():
		if nonce == "" {
			nonce = NewNonce()
		}
		if !nonceRe.MatchString(nonce) {
			return nil, fmt.Errorf("%w: nonce must be 16-64 chars [A-Za-z0-9_-]", ErrBadRequest)
		}
		ts := strconv.FormatInt(now.Unix(), 10)
		h[spec.tsHeader()] = ts
		h[spec.nonceHeader()] = nonce
		h[spec.sigHeader()] = mac(sec.HMACKey, []byte(Canonical(ts, nonce, req.Method, req.Path, req.Body)))
	}
	return h, nil
}

// NonceRecorder es el anti-replay del receptor. *security.NonceCache lo
// satisface: CheckAndRecord devuelve error si el nonce ya se vio.
type NonceRecorder interface {
	CheckAndRecord(nonce string) error
}

// VerifyOptions ajusta Verify.
type VerifyOptions struct {
	// Now: reloj del receptor (cero = time.Now()).
	Now time.Time
	// MaxSkew: ventana +-; cero = DefaultMaxSkew (300 s).
	MaxSkew time.Duration
	// Nonces: si no es nil, un nonce repetido se rechaza (ErrNonceReused).
	Nonces NonceRecorder
}

// Verify es el espejo de Sign. Devuelve nil si la peticion es autentica. Los
// fallos de autenticacion son *Error con codigo estable (CodeOf).
func Verify(spec Spec, sec Secrets, req Request, hdr http.Header, o VerifyOptions) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	if spec.needsBearer() {
		if sec.BearerToken == "" {
			return fmt.Errorf("%w: bearer token", ErrMissingSecret)
		}
		got, ok := strings.CutPrefix(hdr.Get("Authorization"), "Bearer ")
		if !ok || !eq(got, sec.BearerToken) {
			return ErrUnauthenticated
		}
	}
	if spec.needsHMAC() && sec.HMACKey == "" {
		return fmt.Errorf("%w: hmac key", ErrMissingSecret)
	}
	switch {
	case spec.Profile == HMACSimple:
		got := hdr.Get(spec.simpleHeader())
		if got == "" {
			return ErrBadRequest
		}
		if !eq(spec.Prefix+mac(sec.HMACKey, req.Body), got) {
			return ErrBadSignature
		}
	case spec.canonical():
		ts, nonce, sig := hdr.Get(spec.tsHeader()), hdr.Get(spec.nonceHeader()), hdr.Get(spec.sigHeader())
		if ts == "" || nonce == "" || sig == "" || !nonceRe.MatchString(nonce) {
			return ErrBadRequest
		}
		tsv, err := strconv.ParseInt(ts, 10, 64)
		if err != nil {
			return ErrBadRequest
		}
		now := o.Now
		if now.IsZero() {
			now = time.Now()
		}
		skew := o.MaxSkew
		if skew <= 0 {
			skew = DefaultMaxSkew
		}
		d := now.Sub(time.Unix(tsv, 0))
		if d < 0 {
			d = -d
		}
		if d > skew {
			return ErrStaleTimestamp
		}
		want := mac(sec.HMACKey, []byte(Canonical(ts, nonce, req.Method, req.Path, req.Body)))
		if !eq(want, strings.ToLower(sig)) {
			return ErrBadSignature
		}
		if o.Nonces != nil {
			if err := o.Nonces.CheckAndRecord(nonce); err != nil {
				return ErrNonceReused
			}
		}
	}
	return nil
}

func eq(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

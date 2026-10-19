package brand

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Brand pack: the signed INSTANCE identity the platform (hub) issues to one
// customer's instances. Contract: hub docs/BRAND-PACK-CONTRACT.md.
//
// The hub serializes the Pack to exact JSON bytes, signs
// sha256(PackDomain || bytes) with the marketplace Ed25519 key and ships
// an Envelope. Verification works on those RAW bytes: they are never
// canonicalized nor re-serialized, so any JSON encoder on the issuing side
// stays compatible as long as it signs what it sends.

const (
	// PackSpec is the only pack/envelope format this kernel understands. A
	// higher spec is rejected (ErrSpec) and the host keeps its last pack.
	PackSpec = 1
	// PackKind is the fixed "kind" of a brand pack document.
	PackKind = "brand-pack"
	// PackDomain separates brand-pack signatures from release/bundle
	// signatures made with the same marketplace key (those sign the raw
	// sha256 of the artifact), so one can never be replayed as the other.
	PackDomain = "metacore-brand-pack/v1\n"

	// Asset and pack size limits (bytes of the decoded asset / raw pack).
	MaxLogoBytes    = 256 << 10
	MaxIconBytes    = 128 << 10
	MaxFaviconBytes = 64 << 10
	MaxPackBytes    = 600 << 10

	// MaxImageDim bounds each side of a PNG asset.
	MaxImageDim = 4096

	// Asset types accepted inside a pack (by magic bytes, never by label).
	TypePNG = "image/png"
	TypeSVG = "image/svg+xml"

	// Asset kinds, as keyed in Pack.Assets.
	AssetLogo    = "logo"
	AssetIcon    = "icon"
	AssetFavicon = "favicon"
)

// Typed verification errors. Verify wraps them with detail; test with
// errors.Is.
var (
	// ErrBadSignature: the signature does not verify, the envelope sha256
	// does not match the pack bytes, or the pack is not decodable.
	ErrBadSignature = errors.New("brand: pack signature does not verify")
	// ErrSpec: envelope/pack spec or kind is not one this kernel supports.
	ErrSpec = errors.New("brand: unsupported pack spec")
	// ErrMalformed: the signed bytes are not a valid pack document (bad
	// JSON, envelope/pack version disagreement, invalid name/color).
	ErrMalformed = errors.New("brand: malformed pack")
	// ErrAssetDigest: an asset's sha256 does not match its decoded bytes.
	ErrAssetDigest = errors.New("brand: asset sha256 mismatch")
	// ErrAssetInvalid: an asset is not a safe PNG/SVG, or its declared type
	// disagrees with its content.
	ErrAssetInvalid = errors.New("brand: invalid asset")
	// ErrTooLarge: the pack or one of its assets exceeds its size limit.
	ErrTooLarge = errors.New("brand: pack or asset too large")
	// ErrRollback: the pack version is not greater than the current one
	// (an equal version is rejected too, whatever its sha256).
	ErrRollback = errors.New("brand: pack version is not newer than the current one")
	// ErrCustomerMismatch: the pack was issued for another customer.
	ErrCustomerMismatch = errors.New("brand: pack issued for another customer")
)

var colorRe = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

// PackAsset is one artwork inside a pack. Data is standard base64 with
// padding (RFC 4648 §4); SHA256 is the hex sha256 of the decoded bytes.
type PackAsset struct {
	Type   string `json:"type"`
	SHA256 string `json:"sha256"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Data   string `json:"data"`

	// Bytes holds the decoded, verified data. Filled by Verify; never
	// serialized.
	Bytes []byte `json:"-"`
}

// PackAssets keys each optional asset by kind.
type PackAssets struct {
	Logo    *PackAsset `json:"logo,omitempty"`
	Icon    *PackAsset `json:"icon,omitempty"`
	Favicon *PackAsset `json:"favicon,omitempty"`
}

// Pack is the signed document. Field order matches the hub serialization.
type Pack struct {
	Spec         int        `json:"spec"`
	Kind         string     `json:"kind"`
	CustomerID   string     `json:"customer_id"`
	Version      int64      `json:"version"`
	IssuedAt     string     `json:"issued_at"`
	Name         string     `json:"name"`
	PrimaryColor string     `json:"primary_color"`
	AccentColor  string     `json:"accent_color"`
	MascotName   string     `json:"mascot_name"`
	Locked       bool       `json:"locked"`
	Assets       PackAssets `json:"assets"`
}

// Envelope is what travels on the wire (the `brand` field of renew/activate
// and the body of POST /v1/licenses/brand).
type Envelope struct {
	Spec    int    `json:"spec"`
	Version int64  `json:"version"`
	SHA256  string `json:"sha256"` // hex sha256 of the raw pack bytes
	Pack    string `json:"pack"`   // base64url, no padding, of the raw bytes
	Sig     string `json:"sig"`    // base64url, no padding, Ed25519 signature
}

// VerifyOptions carries the instance-side context a pack is checked against.
type VerifyOptions struct {
	// CurrentVersion is the version of the pack the host already applied
	// (0 = none). Only a strictly greater version is accepted.
	CurrentVersion int64
	// ExpectedCustomerID is the license's customer (claim cid /
	// licensing.State.OrgID). Required: an empty value fails closed with
	// ErrCustomerMismatch. Compared case-insensitively.
	ExpectedCustomerID string
	// MaxPack overrides MaxPackBytes for the raw pack (0 = MaxPackBytes).
	MaxPack int
}

// Digest is what the marketplace key signs: sha256(PackDomain || raw).
func Digest(raw []byte) [32]byte {
	h := sha256.New()
	h.Write([]byte(PackDomain))
	h.Write(raw)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// Sign builds the envelope for raw pack bytes. The version is read from the
// bytes themselves, so the envelope can never disagree with what it signs.
// Used by issuers (hub) and tests; instances only Verify.
func Sign(priv ed25519.PrivateKey, raw []byte) (*Envelope, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, errors.New("brand: invalid ed25519 private key")
	}
	var head struct {
		Version int64 `json:"version"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	digest := Digest(raw)
	sum := sha256.Sum256(raw)
	return &Envelope{
		Spec:    PackSpec,
		Version: head.Version,
		SHA256:  hex.EncodeToString(sum[:]),
		Pack:    base64.RawURLEncoding.EncodeToString(raw),
		Sig:     base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, digest[:])),
	}, nil
}

// Verify authenticates an envelope against the marketplace public key and
// the instance context, and returns the parsed pack (assets decoded into
// PackAsset.Bytes) plus the raw signed bytes, which the host should persist
// as-is to re-verify later. Steps, in order: envelope spec, size, decode,
// sha256, signature over Digest(raw), parse raw bytes, spec/kind, version
// agreement, customer, monotonic version, fields, assets.
func Verify(env *Envelope, pub ed25519.PublicKey, opts VerifyOptions) (*Pack, []byte, error) {
	if env == nil {
		return nil, nil, fmt.Errorf("%w: nil envelope", ErrMalformed)
	}
	if len(pub) != ed25519.PublicKeySize {
		return nil, nil, fmt.Errorf("%w: invalid public key", ErrBadSignature)
	}
	if env.Spec != PackSpec {
		return nil, nil, fmt.Errorf("%w: envelope spec %d", ErrSpec, env.Spec)
	}
	maxPack := opts.MaxPack
	if maxPack <= 0 {
		maxPack = MaxPackBytes
	}
	if base64.RawURLEncoding.DecodedLen(len(env.Pack)) > maxPack {
		return nil, nil, fmt.Errorf("%w: pack exceeds %d bytes", ErrTooLarge, maxPack)
	}
	raw, err := base64.RawURLEncoding.DecodeString(env.Pack)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: pack is not base64url: %v", ErrBadSignature, err)
	}
	if len(raw) > maxPack {
		return nil, nil, fmt.Errorf("%w: pack exceeds %d bytes", ErrTooLarge, maxPack)
	}
	sum := sha256.Sum256(raw)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), strings.TrimSpace(env.SHA256)) {
		return nil, nil, fmt.Errorf("%w: envelope sha256 does not match pack bytes", ErrBadSignature)
	}
	sig, err := base64.RawURLEncoding.DecodeString(env.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, nil, fmt.Errorf("%w: malformed signature", ErrBadSignature)
	}
	digest := Digest(raw)
	if !ed25519.Verify(pub, digest[:], sig) {
		return nil, nil, ErrBadSignature
	}

	var p Pack
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if p.Spec != PackSpec {
		return nil, nil, fmt.Errorf("%w: pack spec %d", ErrSpec, p.Spec)
	}
	if p.Kind != PackKind {
		return nil, nil, fmt.Errorf("%w: kind %q", ErrSpec, p.Kind)
	}
	if p.Version != env.Version {
		return nil, nil, fmt.Errorf("%w: envelope version %d != pack version %d", ErrMalformed, env.Version, p.Version)
	}
	want := strings.TrimSpace(opts.ExpectedCustomerID)
	if want == "" || !strings.EqualFold(want, strings.TrimSpace(p.CustomerID)) {
		return nil, nil, fmt.Errorf("%w: pack customer %q", ErrCustomerMismatch, p.CustomerID)
	}
	if p.Version <= opts.CurrentVersion {
		return nil, nil, fmt.Errorf("%w: version %d <= current %d", ErrRollback, p.Version, opts.CurrentVersion)
	}
	if err := checkFields(&p); err != nil {
		return nil, nil, err
	}
	for _, a := range []struct {
		kind string
		ptr  *PackAsset
	}{{AssetLogo, p.Assets.Logo}, {AssetIcon, p.Assets.Icon}, {AssetFavicon, p.Assets.Favicon}} {
		if a.ptr == nil {
			continue
		}
		if err := verifyAsset(a.kind, a.ptr); err != nil {
			return nil, nil, err
		}
	}
	return &p, raw, nil
}

func checkFields(p *Pack) error {
	if n := utf8.RuneCountInString(strings.TrimSpace(p.Name)); n < 1 || n > 64 {
		return fmt.Errorf("%w: name must be 1-64 characters", ErrMalformed)
	}
	if utf8.RuneCountInString(p.MascotName) > 64 {
		return fmt.Errorf("%w: mascot_name exceeds 64 characters", ErrMalformed)
	}
	for _, c := range []string{p.PrimaryColor, p.AccentColor} {
		if c != "" && !colorRe.MatchString(c) {
			return fmt.Errorf("%w: color %q is not #RRGGBB", ErrMalformed, c)
		}
	}
	return nil
}

// MaxAssetBytes returns the size limit of an asset kind (0 = unknown kind).
func MaxAssetBytes(kind string) int {
	switch kind {
	case AssetLogo:
		return MaxLogoBytes
	case AssetIcon:
		return MaxIconBytes
	case AssetFavicon:
		return MaxFaviconBytes
	}
	return 0
}

func verifyAsset(kind string, a *PackAsset) error {
	limit := MaxAssetBytes(kind)
	if base64.StdEncoding.DecodedLen(len(a.Data)) > limit+3 {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrTooLarge, kind, limit)
	}
	data, err := base64.StdEncoding.DecodeString(a.Data)
	if err != nil {
		return fmt.Errorf("%w: %s data is not base64: %v", ErrAssetInvalid, kind, err)
	}
	if len(data) == 0 {
		return fmt.Errorf("%w: %s is empty", ErrAssetInvalid, kind)
	}
	if len(data) > limit {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrTooLarge, kind, limit)
	}
	sum := sha256.Sum256(data)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), strings.TrimSpace(a.SHA256)) {
		return fmt.Errorf("%w: %s", ErrAssetDigest, kind)
	}
	typ, err := ValidateAsset(a.Type, data)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrAssetInvalid, kind, err)
	}
	a.Type = typ
	a.Bytes = data
	return nil
}

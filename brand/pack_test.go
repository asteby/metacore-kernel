package brand

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"os"
	"strings"
	"testing"
)

// goldenVector mirrors hub backend/internal/api/testdata/brand_pack_vector.json
// (copied byte for byte into testdata/).
type goldenVector struct {
	SeedHex         string   `json:"seed_hex"`
	PublicKeyHex    string   `json:"public_key_hex"`
	Domain          string   `json:"domain"`
	PackJSON        string   `json:"pack_json"`
	PackB64URL      string   `json:"pack_b64url"`
	SHA256          string   `json:"sha256"`
	SignedDigestHex string   `json:"signed_digest_hex"`
	SigB64URL       string   `json:"sig_b64url"`
	Envelope        Envelope `json:"envelope"`
}

const goldenCustomer = "22222222-2222-4222-8222-222222222222"

func loadVector(t *testing.T) (goldenVector, ed25519.PrivateKey) {
	t.Helper()
	b, err := os.ReadFile("testdata/brand_pack_vector.json")
	if err != nil {
		t.Fatal(err)
	}
	var v goldenVector
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	seed, err := hex.DecodeString(v.SeedHex)
	if err != nil {
		t.Fatal(err)
	}
	return v, ed25519.NewKeyFromSeed(seed)
}

func pubOf(priv ed25519.PrivateKey) ed25519.PublicKey { return priv.Public().(ed25519.PublicKey) }

func TestPack_GoldenVector(t *testing.T) {
	v, priv := loadVector(t)
	if got := hex.EncodeToString(pubOf(priv)); got != v.PublicKeyHex {
		t.Fatalf("public key = %s, want %s", got, v.PublicKeyHex)
	}
	if v.Domain != PackDomain {
		t.Fatalf("domain = %q, want %q", v.Domain, PackDomain)
	}
	raw := []byte(v.PackJSON)
	d := Digest(raw)
	if got := hex.EncodeToString(d[:]); got != v.SignedDigestHex {
		t.Fatalf("digest = %s, want %s", got, v.SignedDigestHex)
	}

	env, err := Sign(priv, raw)
	if err != nil {
		t.Fatal(err)
	}
	if *env != v.Envelope {
		t.Fatalf("envelope mismatch:\n got %+v\nwant %+v", *env, v.Envelope)
	}
	if env.Pack != v.PackB64URL || env.SHA256 != v.SHA256 || env.Sig != v.SigB64URL {
		t.Fatal("envelope fields differ from the vector")
	}

	p, gotRaw, err := Verify(&v.Envelope, pubOf(priv), VerifyOptions{ExpectedCustomerID: goldenCustomer})
	if err != nil {
		t.Fatalf("verify golden: %v", err)
	}
	if !bytes.Equal(gotRaw, raw) {
		t.Fatal("Verify must return the raw signed bytes untouched")
	}
	if p.Version != 3 || p.Name != "God's Nutrition" || !p.Locked || p.PrimaryColor != "#0a7d3b" {
		t.Fatalf("pack = %+v", p)
	}
	if p.Assets.Logo == nil || p.Assets.Logo.Type != TypeSVG || !bytes.HasPrefix(p.Assets.Logo.Bytes, []byte("<svg")) {
		t.Fatalf("logo not decoded: %+v", p.Assets.Logo)
	}
	if p.Assets.Icon != nil || p.Assets.Favicon != nil {
		t.Fatal("absent assets must stay nil")
	}
}

// encodePack serializes like the hub: no HTML escaping, no trailing newline.
func encodePack(t *testing.T, p any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p); err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

func mkAsset(typ string, data []byte) *PackAsset {
	sum := sha256.Sum256(data)
	return &PackAsset{Type: typ, SHA256: hex.EncodeToString(sum[:]), Width: 1, Height: 1,
		Data: base64.StdEncoding.EncodeToString(data)}
}

func basePack() Pack {
	return Pack{Spec: 1, Kind: PackKind, CustomerID: goldenCustomer, Version: 4,
		IssuedAt: "2026-10-18T12:00:00Z", Name: "Acme", PrimaryColor: "#112233", Locked: true}
}

func signed(t *testing.T, priv ed25519.PrivateKey, p any) *Envelope {
	t.Helper()
	env, err := Sign(priv, encodePack(t, p))
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func opts() VerifyOptions { return VerifyOptions{ExpectedCustomerID: goldenCustomer} }

func TestVerify_AlteredByteRejected(t *testing.T) {
	v, priv := loadVector(t)
	raw := []byte(v.PackJSON)
	raw[bytes.Index(raw, []byte("Nutri"))] = 'M' // flip one byte, keep JSON valid
	sum := sha256.Sum256(raw)
	env := v.Envelope
	env.Pack = base64.RawURLEncoding.EncodeToString(raw)
	env.SHA256 = hex.EncodeToString(sum[:])
	if _, _, err := Verify(&env, pubOf(priv), opts()); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("altered byte: err = %v, want ErrBadSignature", err)
	}
	// Envelope sha256 that disagrees with the bytes.
	env = v.Envelope
	env.SHA256 = strings.Repeat("0", 64)
	if _, _, err := Verify(&env, pubOf(priv), opts()); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("bad envelope sha: err = %v", err)
	}
	// Wrong key.
	other := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	if _, _, err := Verify(&v.Envelope, pubOf(other), opts()); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("wrong key: err = %v", err)
	}
}

func TestVerify_SignatureWithoutDomainRejected(t *testing.T) {
	v, priv := loadVector(t)
	raw := []byte(v.PackJSON)
	plain := sha256.Sum256(raw) // what a release signature covers
	env := v.Envelope
	env.Sig = base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, plain[:]))
	if _, _, err := Verify(&env, pubOf(priv), opts()); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("no domain: err = %v, want ErrBadSignature", err)
	}
}

func TestVerify_AssetDigestMismatch(t *testing.T) {
	_, priv := loadVector(t)
	p := basePack()
	a := mkAsset(TypeSVG, []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`))
	a.SHA256 = strings.Repeat("ab", 32)
	p.Assets.Icon = a
	if _, _, err := Verify(signed(t, priv, p), pubOf(priv), opts()); !errors.Is(err, ErrAssetDigest) {
		t.Fatalf("err = %v, want ErrAssetDigest", err)
	}
}

func TestVerify_SpecAndKind(t *testing.T) {
	_, priv := loadVector(t)
	p := basePack()
	p.Spec = 2
	if _, _, err := Verify(signed(t, priv, p), pubOf(priv), opts()); !errors.Is(err, ErrSpec) {
		t.Fatalf("pack spec 2: err = %v", err)
	}
	env := signed(t, priv, basePack())
	env.Spec = 2
	if _, _, err := Verify(env, pubOf(priv), opts()); !errors.Is(err, ErrSpec) {
		t.Fatalf("envelope spec 2: err = %v", err)
	}
	p = basePack()
	p.Kind = "release"
	if _, _, err := Verify(signed(t, priv, p), pubOf(priv), opts()); !errors.Is(err, ErrSpec) {
		t.Fatalf("kind: err = %v", err)
	}
}

func TestVerify_Rollback(t *testing.T) {
	_, priv := loadVector(t)
	env := signed(t, priv, basePack()) // version 4
	for _, cur := range []int64{4, 9} {
		o := opts()
		o.CurrentVersion = cur
		if _, _, err := Verify(env, pubOf(priv), o); !errors.Is(err, ErrRollback) {
			t.Fatalf("current %d: err = %v, want ErrRollback", cur, err)
		}
	}
	// Same version, different content (other sha) is still a rollback.
	p := basePack()
	p.Name = "Acme 2"
	o := opts()
	o.CurrentVersion = 4
	if _, _, err := Verify(signed(t, priv, p), pubOf(priv), o); !errors.Is(err, ErrRollback) {
		t.Fatalf("equal version other sha: err = %v", err)
	}
	o.CurrentVersion = 3
	if _, _, err := Verify(env, pubOf(priv), o); err != nil {
		t.Fatalf("newer version must pass: %v", err)
	}
	// Envelope version that lies about the pack.
	env2 := *env
	env2.Version = 99
	if _, _, err := Verify(&env2, pubOf(priv), opts()); !errors.Is(err, ErrMalformed) {
		t.Fatalf("version disagreement: err = %v", err)
	}
}

func TestVerify_CustomerMismatch(t *testing.T) {
	v, priv := loadVector(t)
	for _, cid := range []string{"33333333-3333-4333-8333-333333333333", ""} {
		_, _, err := Verify(&v.Envelope, pubOf(priv), VerifyOptions{ExpectedCustomerID: cid})
		if !errors.Is(err, ErrCustomerMismatch) {
			t.Fatalf("cid %q: err = %v", cid, err)
		}
	}
	if _, _, err := Verify(&v.Envelope, pubOf(priv), VerifyOptions{ExpectedCustomerID: strings.ToUpper(goldenCustomer)}); err != nil {
		t.Fatalf("uuid compare must ignore case: %v", err)
	}
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestVerify_SizeLimits(t *testing.T) {
	_, priv := loadVector(t)
	big := append([]byte(`<svg xmlns="http://www.w3.org/2000/svg"><!--`), bytes.Repeat([]byte("x"), MaxFaviconBytes)...)
	big = append(big, []byte(`--></svg>`)...)
	p := basePack()
	p.Assets.Favicon = mkAsset(TypeSVG, big)
	if _, _, err := Verify(signed(t, priv, p), pubOf(priv), opts()); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("favicon > 64 KiB: err = %v", err)
	}
	// The same bytes fit as a logo.
	p.Assets.Favicon, p.Assets.Logo = nil, mkAsset(TypeSVG, big)
	if _, _, err := Verify(signed(t, priv, p), pubOf(priv), opts()); err != nil {
		t.Fatalf("logo of %d bytes must pass: %v", len(big), err)
	}
	o := opts()
	o.MaxPack = 1024
	if _, _, err := Verify(signed(t, priv, p), pubOf(priv), o); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("pack > MaxPack: err = %v", err)
	}
}

func TestVerify_PNGAndBadAssets(t *testing.T) {
	_, priv := loadVector(t)
	good := pngBytes(t, 32, 32)
	p := basePack()
	p.Assets.Icon = mkAsset(TypePNG, good)
	pk, _, err := Verify(signed(t, priv, p), pubOf(priv), opts())
	if err != nil {
		t.Fatalf("png: %v", err)
	}
	if !bytes.Equal(pk.Assets.Icon.Bytes, good) {
		t.Fatal("icon bytes not decoded")
	}

	cases := map[string]*PackAsset{
		"declared svg, content png": mkAsset(TypeSVG, good),
		"png with trailing bytes":   mkAsset(TypePNG, append(append([]byte{}, good...), "<script>"...)),
		"gif":                       mkAsset("image/gif", []byte("GIF89a....")),
		"svg script":                mkAsset(TypeSVG, []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)),
	}
	for name, a := range cases {
		p := basePack()
		p.Assets.Icon = a
		if _, _, err := Verify(signed(t, priv, p), pubOf(priv), opts()); !errors.Is(err, ErrAssetInvalid) {
			t.Errorf("%s: err = %v, want ErrAssetInvalid", name, err)
		}
	}
}

func TestValidateSVG(t *testing.T) {
	ok := []string{
		`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1 1"/>`,
		`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"><defs><linearGradient id="g"/></defs><rect fill="url(#g)"/><use href="#g"/></svg>`,
		`<svg xmlns="http://www.w3.org/2000/svg"><style>.a{fill:#000}</style></svg>`,
	}
	for _, s := range ok {
		if err := ValidateSVG([]byte(s)); err != nil {
			t.Errorf("valid SVG rejected: %v\n%s", err, s)
		}
	}
	bad := []string{
		`<svg xmlns="http://www.w3.org/2000/svg"><script>x</script></svg>`,
		`<svg xmlns="http://www.w3.org/2000/svg"><s:script xmlns:s="x">x</s:script></svg>`,
		`<svg xmlns="http://www.w3.org/2000/svg"><foreignObject/></svg>`,
		`<svg xmlns="http://www.w3.org/2000/svg" onload="x()"/>`,
		`<svg xmlns="http://www.w3.org/2000/svg"><a href="https://evil"/></svg>`,
		`<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink"><image xlink:href="http://x/a.png"/></svg>`,
		`<svg xmlns="http://www.w3.org/2000/svg"><a href="#x" title="java&#x09;script:1"/></svg>`,
		`<svg xmlns="http://www.w3.org/2000/svg"><style>@import url(http://x)</style></svg>`,
		`<svg xmlns="http://www.w3.org/2000/svg"><rect style="fill:url(http://x/#a)"/></svg>`,
		`<svg xmlns="http://www.w3.org/2000/svg"><set attributeName="href" to="javascript:x"/></svg>`,
		`<!DOCTYPE svg [<!ENTITY x "y">]><svg xmlns="http://www.w3.org/2000/svg"/>`,
		`<?xml-stylesheet href="x"?><svg xmlns="http://www.w3.org/2000/svg"/>`,
		`<html/>`,
		`<svg xmlns="http://www.w3.org/2000/svg">`,
	}
	for _, s := range bad {
		if err := ValidateSVG([]byte(s)); err == nil {
			t.Errorf("unsafe SVG accepted:\n%s", s)
		}
	}
}

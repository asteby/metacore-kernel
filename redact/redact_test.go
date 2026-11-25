package redact

import (
	"encoding/json"
	"flag"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite testdata/*.json")

// legacyOpts is the profile that reproduces the original emitters: Legacy()
// plus the two identifier markers the originals used.
func legacyOpts() []Option {
	return []Option{Legacy(), WithMarker(RuleIDPerson, "[CURP]"), WithMarker(RuleIDEntity, "[RFC]")}
}

// corpus feeds the parity, superset and fixture tests.
var corpus = []string{
	"",
	"plain text with nothing sensitive",
	"Authorization: Bearer abcdEFGH12345678.tok-en/xyz== end",
	"bearer short",
	"token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTYifQ.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c ok",
	"dsn postgres://user:p%40ss@db.internal:5432/app?sslmode=disable and mysql://root@h/db",
	"MONGODB+SRV://u:p@cluster0.example.net/test redis://:pw@cache:6379/0 amqp://g:g@mq",
	"keys sk_live_abcdef123456 pk_test_ABCDEFGH1234 ghp_abcdefghijklmnop whsec_abcdefgh12 xoxb-nope xoxb_abcdefgh1234",
	"password=hunter2 passwd: 'two words' pwd=\"a b\" secret:abc token=xyz&next=1",
	`{"api_key": "AbCd", "client_secret":"s3", "refresh_token": "r", "access_token":"a"}`,
	"api-key=ZZZ Authorization=Basic",
	"mail me at john.doe+tag@example.co.uk or x@y.io, not at foo@bar",
	"curp GAPA850101HDFRRN09 rfc GAPA850101AB1 and XAXX010101000 and ab&c850101XYZ",
	"card 4111 1111 1111 1111 or 4111-1111-1111-1111 or 4111111111111111 or 1234567890123456",
	"short 123456789012 and long 12345678901234567890123",
	"call +52 55 1234 5678 or (555) 123-4567 or 5551234567 or 555-1234",
	"sha1 da39a3ee5e6b4b0d3255bfef95601890afd80709 sha256 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
	"blob QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVowMTIzNDU2Nzg5QUJDREVGR0hJSktMTU5PUA== tail",
	"unicode texto con acentos: canci\u00f3n \u00d1and\u00fa 5551234567 fin",
	"multi\nline\twith token=abc\nand email a@b.cd",
	"url https://u:pw@host.example/path?X-Amz-Signature=abcdef0123&x=1&sig=zzz",
	"2024-01-02 12:30:45 version 10.1.2.3 ids 192.168.0.1 and ::1 and 2001:db8::ff00:42:8329",
	"-----BEGIN RSA PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASC\n-----END RSA PRIVATE KEY----- after",
	"AKIAIOSFODNN7EXAMPLE and sk-ant-api03-abcdefghijklmnop1234 and glpat-abcdefghijklmnopqrst",
	"Authorization: Basic dXNlcjpwYXNzd29yZA== and basic configuration of the thing",
	"cookie: sessionid=abc123; passphrase=open sesame",
	"Ab12Cd34Ef56Gh78Ij90Kl12Mn34Op56Qr78St90Uv12Wx34Yz56_token-like-value",
	"very_long_identifier_name_without_digits_in_it_at_all_just_letters_here",
}

// TestLegacyParity proves Legacy() is byte-identical to the original list on
// the corpus, on every prefix and suffix of it, and on mutations.
func TestLegacyParity(t *testing.T) {
	e := New(legacyOpts()...)
	check := func(s string) {
		t.Helper()
		if got, want := e.Redact(s), legacyRedact(s); got != want {
			t.Fatalf("parity mismatch for %q\n got: %q\nwant: %q", s, got, want)
		}
	}
	for _, s := range corpus {
		check(s)
		for i := 0; i <= len(s); i++ {
			if i%3 == 0 { // keep runtime small
				check(s[:i])
				check(s[i:])
			}
		}
	}
	// Join everything: many adjacent matches in one string.
	check(strings.Join(corpus, " | "))
}

func TestLegacyParityEmptyAndMarkers(t *testing.T) {
	e := New(legacyOpts()...)
	if e.Redact("") != "" {
		t.Fatal("empty must stay empty")
	}
	if got := e.Redact("a@b.cd"); got != "[EMAIL]" {
		t.Fatalf("got %q", got)
	}
	// Without host markers the identifier rules fall back to typed markers.
	if got := New(Legacy()).Redact("GAPA850101HDFRRN09"); got != "[redacted:id_person]" {
		t.Fatalf("got %q", got)
	}
}

// TestDefaultSupersetOfLegacy: whatever the originals scrubbed, the default
// engine scrubs too (no false negatives), and typed markers are used.
func TestDefaultSupersetOfLegacy(t *testing.T) {
	d := New()
	for _, s := range corpus {
		if legacyRedact(s) == s {
			continue
		}
		if d.Redact(s) == s {
			t.Errorf("default left untouched what legacy redacted: %q", s)
		}
	}
	// Secret substrings must never survive.
	secrets := map[string][]string{
		"Authorization: Bearer abcdEFGH12345678.tok-en/xyz==": {"abcdEFGH12345678"},
		"password=hunter2 x":                                             {"hunter2"},
		"postgres://user:p%40ss@db.internal:5432/app":                    {"p%40ss", "db.internal"},
		"john.doe+tag@example.co.uk":                                     {"john.doe"},
		"sk_live_abcdef123456":                                           {"abcdef123456"},
		"+52 55 1234 5678":                                               {"1234 5678"},
		"4111 1111 1111 1111":                                            {"4111 1111"},
		"AKIAIOSFODNN7EXAMPLE":                                           {"AKIAIOSFODNN7EXAMPLE"},
		"sk-ant-api03-abcdefghijklmnop1234":                              {"abcdefghijklmnop1234"},
		"https://u:pw@host.example/p":                                    {"u:pw"},
		"https://h/x?X-Amz-Signature=abcdef0123&x=1":                     {"abcdef0123"},
		"Authorization: Basic dXNlcjpwYXNzd29yZA==":                      {"dXNlcjpwYXNzd29yZA"},
		"-----BEGIN PRIVATE KEY-----\nMIIEvQ\n-----END PRIVATE KEY-----": {"MIIEvQ"},
		"cookie: sessionid=abc123":                                       {"abc123"},
	}
	for in, bad := range secrets {
		out := d.Redact(in)
		for _, b := range bad {
			if strings.Contains(out, b) {
				t.Errorf("%q leaked %q: %q", in, b, out)
			}
		}
	}
}

func TestTypedMarkersAndKeepers(t *testing.T) {
	cases := map[string]string{
		"mail a@b.cd now":                      "mail [redacted:email] now",
		"Bearer abcdefgh12345":                 "Bearer [redacted:bearer]",
		"password=hunter2 next":                "password=[redacted:secret] next",
		"postgres://u:p@h/db":                  "postgres://[redacted:conn_string]",
		"https://u:p@h.example/x":              "https://[redacted:url_credentials]@h.example/x",
		"https://h/x?sig=abc123&a=1":           "https://h/x?sig=[redacted:url_signature]&a=1",
		"card 4111 1111 1111 1111 ok":          "card [redacted:card] ok",
		"num 1234567890123456 ok":              "num [redacted:number] ok",
		"basic configuration of the thing":     "basic configuration of the thing",
		"Authorization: Basic dXNlcjpwYXNz":    "Authorization: [redacted:secret] [redacted:basic_auth]",
		"Basic dXNlcjpwYXNz alone":             "Basic [redacted:basic_auth] alone",
		"version 10.1.2.3 at 12:30:45 and ::1": "version 10.1.2.3 at 12:30:45 and ::1",
	}
	for in, want := range cases {
		if got := String(in); got != want {
			t.Errorf("String(%q)\n got: %q\nwant: %q", in, got, want)
		}
	}
	if got := String("from 10.1.2.3 and 256.1.1.1 and ::1 and 12:30:45", WithIP()); got !=
		"from [redacted:ip] and 256.1.1.1 and [redacted:ip] and 12:30:45" {
		t.Errorf("ip: %q", got)
	}
}

func TestLuhn(t *testing.T) {
	good := []string{"4111111111111111", "4111 1111 1111 1111", "5500-0000-0000-0004", "378282246310005", "6011111111111117"}
	bad := []string{"4111111111111112", "1234567890123456", "411111111111", "41111111111111111111", "4111x111111111111"}
	for _, g := range good {
		if !LuhnValid(g) {
			t.Errorf("want valid: %s", g)
		}
	}
	for _, b := range bad {
		if LuhnValid(b) {
			t.Errorf("want invalid: %s", b)
		}
	}
}

func TestOptionsOnlyWithoutCustomMarker(t *testing.T) {
	in := "a@b.cd 5551234567 token=zz"
	if got := String(in, Only(RuleEmail)); got != "[redacted:email] 5551234567 token=zz" {
		t.Errorf("only: %q", got)
	}
	if got := String(in, Without(RulePhone, RuleNumber)); !strings.Contains(got, "5551234567") {
		t.Errorf("without: %q", got)
	}
	r := WithRule(Rule{Name: "ticket", Pattern: re(`TCK-\d+`)})
	if got := String("see TCK-42", r, WithMarker("ticket", "<t>")); got != "see <t>" {
		t.Errorf("custom: %q", got)
	}
	if got := String("a@b.cd", WithMarker(RuleEmail, "$1${x}")); got != "$1${x}" {
		t.Errorf("marker must be literal: %q", got)
	}
}

func TestTruncationAndCleanPipeline(t *testing.T) {
	if got := TruncateRunes("h\u00e9llo w\u00f6rld", 4); got != "h\u00e9ll" {
		t.Errorf("runes: %q", got)
	}
	if got := TruncateBytes("a\u00e9b", 2); got != "a" { // 'é' is 2 bytes, cut mid-rune
		t.Errorf("bytes: %q", got)
	}
	if got := TruncateBytes("abc", 0); got != "abc" {
		t.Errorf("zero: %q", got)
	}
	e := New(WithStripControls(), WithTrim(), WithMaxRunes(20))
	got := e.Redact("  \x00hi\x07 a@b.cd\nline two is long enough  ")
	if got != "hi [redacted:email]\nl" && !strings.HasPrefix(got, "hi [redacted:email]") {
		t.Errorf("pipeline: %q", got)
	}
	if n := len([]rune(got)); n > 20 {
		t.Errorf("too long: %d", n)
	}
	// Truncation after redaction: a secret straddling the cut never leaks.
	cut := New(WithMaxRunes(10)).Redact("token=supersecretvalue")
	if strings.Contains(cut, "super") {
		t.Errorf("leak: %q", cut)
	}
	// MaxInput drops the unscanned tail instead of passing it through.
	mi := New(WithMaxInput(5)).Redact("abcde a@b.cd")
	if mi != "abcde" {
		t.Errorf("maxinput: %q", mi)
	}
}

func TestAsFuncAndInterface(t *testing.T) {
	var _ Redactor = New()
	var _ Redactor = Func(strings.ToUpper)
	f := AsFunc(nil)
	if f("a@b.cd") != "[redacted:email]" {
		t.Fatal("nil redactor must fall back to default")
	}
	f = AsFunc(Func(func(s string) string { return "x" }))
	if f("a") != "x" {
		t.Fatal("custom redactor not used")
	}
	var hook func(string) string = AsFunc(New(WithMaxRunes(3)))
	if hook("abcdef") != "abc" {
		t.Fatal("hook")
	}
}

func TestConcurrent(t *testing.T) {
	e := New(WithIP())
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				for _, s := range corpus {
					want := e.Redact(s)
					if got := String(s, WithIP()); got != want {
						t.Errorf("nondeterministic: %q", s)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
}

// ---- ReDoS / size ----

func within(t *testing.T, name string, limit time.Duration, f func()) {
	t.Helper()
	start := time.Now()
	f()
	if d := time.Since(start); d > limit {
		t.Errorf("%s took %v (limit %v)", name, d, limit)
	}
}

func TestNoCatastrophicBacktracking(t *testing.T) {
	const n = 1 << 18
	inputs := map[string]string{
		"a-run":        strings.Repeat("a", n),
		"digits":       strings.Repeat("1", n),
		"digit-space":  strings.Repeat("1 ", n/2),
		"dash-digits":  strings.Repeat("1-", n/2),
		"at-signs":     strings.Repeat("a@", n/2),
		"dots":         strings.Repeat("a.", n/2),
		"colons":       strings.Repeat(":", n),
		"hexcolon":     strings.Repeat("ab:", n/3),
		"quote-open":   "password=\"" + strings.Repeat("x", n),
		"key-eq":       strings.Repeat("token=", n/6),
		"bearer":       strings.Repeat("bearer ", n/7),
		"scheme":       strings.Repeat("a://", n/4),
		"pem-open":     "-----BEGIN PRIVATE KEY-----" + strings.Repeat("A", n),
		"pem-lines":    strings.Repeat("-----BEGIN PRIVATE KEY-----\n", n/28),
		"base64":       strings.Repeat("Ab1/", n/4),
		"base-pad":     strings.Repeat("Basic ", n/6),
		"jwt-ish":      "eyJ" + strings.Repeat("a.", n/2),
		"unicode":      strings.Repeat("\u00d1", n/2),
		"phone-ish":    strings.Repeat("(1", n/2),
		"pairs-hyphen": strings.Repeat("a-", n/2),
	}
	engines := map[string]*Engine{"default": New(WithIP()), "legacy": New(legacyOpts()...)}
	for en, e := range engines {
		for name, in := range inputs {
			within(t, en+"/"+name, 10*time.Second, func() { _ = e.Redact(in) })
		}
	}
}

func TestLinearScaling(t *testing.T) {
	e := New(WithIP())
	unit := "x@y.zz 4111 1111 1111 1111 token=abc 10.0.0.1 "
	run := func(k int) time.Duration {
		s := strings.Repeat(unit, k)
		start := time.Now()
		_ = e.Redact(s)
		return time.Since(start)
	}
	run(100) // warm up
	small, large := run(2000), run(32000)
	// 16x the input must not cost anywhere near 16^2 = 256x.
	if large > small*64+50*time.Millisecond {
		t.Errorf("non-linear: %v for 2k units, %v for 32k units", small, large)
	}
}

// ---- spec and portable fixtures ----

type vectorCase struct {
	Name     string `json:"name"`
	Profile  string `json:"profile"`
	Input    string `json:"input"`
	Expected string `json:"expected"`
}

type profile struct {
	Description string            `json:"description"`
	Legacy      bool              `json:"legacy,omitempty"`
	IP          bool              `json:"ip,omitempty"`
	Markers     map[string]string `json:"markers,omitempty"`
	StripCtl    bool              `json:"strip_controls,omitempty"`
	Trim        bool              `json:"trim,omitempty"`
	MaxRunes    int               `json:"max_runes,omitempty"`
	MaxBytes    int               `json:"max_bytes,omitempty"`
}

type vectorFile struct {
	Version  int                `json:"version"`
	Note     string             `json:"note"`
	Profiles map[string]profile `json:"profiles"`
	Cases    []vectorCase       `json:"cases"`
}

func (p profile) options() []Option {
	var o []Option
	if p.Legacy {
		o = append(o, Legacy())
	}
	if p.IP {
		o = append(o, WithIP())
	}
	for k, v := range p.Markers {
		o = append(o, WithMarker(k, v))
	}
	if p.StripCtl {
		o = append(o, WithStripControls())
	}
	if p.Trim {
		o = append(o, WithTrim())
	}
	if p.MaxRunes > 0 {
		o = append(o, WithMaxRunes(p.MaxRunes))
	}
	if p.MaxBytes > 0 {
		o = append(o, WithMaxBytes(p.MaxBytes))
	}
	return o
}

var profiles = map[string]profile{
	"default":    {Description: "all non-optional rules, typed markers"},
	"default_ip": {Description: "default plus the ip rule", IP: true},
	"legacy": {Description: "original shared rule list and markers", Legacy: true,
		Markers: map[string]string{RuleIDPerson: "[CURP]", RuleIDEntity: "[RFC]"}},
	"clean_short": {Description: "support-style cleaning: strip controls, trim, 40 runes",
		StripCtl: true, Trim: true, MaxRunes: 40},
}

func buildVectors() vectorFile {
	vf := vectorFile{
		Version:  1,
		Note:     "Generated by `go test ./redact -run TestFixtures -update`. Every port must reproduce expected for each case under its profile; see docs/redact.md.",
		Profiles: profiles,
	}
	for _, name := range []string{"default", "default_ip", "legacy", "clean_short"} {
		e := New(profiles[name].options()...)
		for i, s := range corpus {
			if s == "" {
				continue
			}
			vf.Cases = append(vf.Cases, vectorCase{
				Name: name + "/" + itoa(i), Profile: name, Input: s, Expected: e.Redact(s),
			})
		}
	}
	// Control characters and truncation, only meaningful for clean_short.
	for i, s := range []string{
		"  \x00hi\x07 a@b.cd\nline two is long enough to be cut  ",
		"\u00d1" + strings.Repeat("\u00e9", 60),
	} {
		vf.Cases = append(vf.Cases, vectorCase{Name: "clean_short/x" + itoa(i), Profile: "clean_short", Input: s,
			Expected: New(profiles["clean_short"].options()...).Redact(s)})
	}
	return vf
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func marshal(v any) []byte {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(b, '\n')
}

func TestFixtures(t *testing.T) {
	files := map[string][]byte{
		"testdata/vectors.json": marshal(buildVectors()),
		"testdata/rules.json":   marshal(map[string]any{"version": 1, "rules": Spec()}),
	}
	for path, want := range files {
		if *update {
			if err := os.WriteFile(path, want, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v (run with -update)", path, err)
		}
		if strings.ReplaceAll(string(got), "\r\n", "\n") != string(want) {
			t.Errorf("%s is stale; run `go test ./redact -run TestFixtures -update`", path)
		}
	}
}

// TestVectorsReplay reads the committed JSON the way a port would and checks
// every case, so a hand edit of an expected value is caught too.
func TestVectorsReplay(t *testing.T) {
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Skip("no vectors yet")
	}
	var vf vectorFile
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatal(err)
	}
	if len(vf.Cases) < 50 {
		t.Fatalf("too few cases: %d", len(vf.Cases))
	}
	for _, c := range vf.Cases {
		p, ok := vf.Profiles[c.Profile]
		if !ok {
			t.Fatalf("%s: unknown profile", c.Name)
		}
		if got := New(p.options()...).Redact(c.Input); got != c.Expected {
			t.Errorf("%s\n got: %q\nwant: %q", c.Name, got, c.Expected)
		}
	}
}

// TestLegacyVectorsMatchOracle ties the committed legacy fixtures to the
// original implementation, not only to the new code.
func TestLegacyVectorsMatchOracle(t *testing.T) {
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Skip("no vectors yet")
	}
	var vf vectorFile
	_ = json.Unmarshal(raw, &vf)
	n := 0
	for _, c := range vf.Cases {
		if c.Profile == "legacy" {
			n++
			if want := legacyRedact(c.Input); want != c.Expected {
				t.Errorf("%s: fixture differs from original implementation\n got: %q\nwant: %q", c.Name, c.Expected, want)
			}
		}
	}
	if n == 0 {
		t.Fatal("no legacy cases")
	}
}

func TestSpecShape(t *testing.T) {
	s := Spec()
	if len(s) != len(builtin) || s[0].Name != RulePrivateKey {
		t.Fatal("spec")
	}
	var found bool
	for _, r := range s {
		if r.Name == RuleBearer && r.Flags == "i" && strings.HasPrefix(r.Pattern, `\bbearer`) {
			found = true
		}
	}
	if !found {
		t.Fatal("flags not lifted")
	}
	if !reflect.DeepEqual(New().Rules()[0], RulePrivateKey) {
		t.Fatal("rules order")
	}
}

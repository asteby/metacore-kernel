package diag

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/asteby/metacore-kernel/capability"
)

// --- B1: el recorte de una linea cruda no puede partir un secreto conocido ---

func crumbsOf(b *Bag) []string {
	d := Diagnostic{Org: uuid.New(), AddonKey: "acme", Code: "x"}
	b.Apply(&d)
	return Sanitize(d, b.Scrubber).Breadcrumbs
}

func TestBagLog_TruncationNeverSplitsKnownSecret(t *testing.T) {
	secret := "AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHH"
	// Caso minimo del revisor.
	b := NewBag()
	b.AddSecrets(secret)
	b.Log("https://a.io/?" + strings.Repeat("x", 600) + " k=" + secret)
	for _, c := range crumbsOf(b) {
		if strings.Contains(c, secret[:8]) {
			t.Fatalf("leak: %q", c)
		}
	}
	// Barrido de desplazamientos, incluidos los que cruzan el tope en memoria.
	for _, pad := range []int{0, 100, 159, 160, 300, 639, 640, 641, 700, 2000, 8100, 8150, 8170, 8180, 8190, 8192, 8200, 9000} {
		b := NewBag()
		b.AddSecrets(secret)
		b.Log(strings.Repeat("x", pad) + " " + secret + " tail")
		for _, c := range crumbsOf(b) {
			for i := 0; i+8 <= len(secret); i++ {
				if strings.Contains(c, secret[i:i+8]) {
					t.Fatalf("pad=%d partial secret survived: %q", pad, c)
				}
			}
		}
	}
}

func FuzzBagLogKnownSecretOffset(f *testing.F) {
	f.Add(0, "https://a.io/?q=")
	f.Add(8185, "k=")
	f.Fuzz(func(t *testing.T, pad int, prefix string) {
		if pad < 0 || pad > 9000 || len(prefix) > 50 {
			return
		}
		secret := "QwErTyUiOpAsDfGhJkLzXcVbNm"
		b := NewBag()
		b.AddSecrets(secret)
		b.Log(prefix + strings.Repeat("x", pad) + " " + secret)
		for _, c := range crumbsOf(b) {
			for i := 0; i+10 <= len(secret); i++ {
				if strings.Contains(c, secret[i:i+10]) {
					t.Fatalf("partial secret leaked pad=%d prefix=%q: %q", pad, prefix, c)
				}
			}
		}
	})
}

// --- 1) extraccion: nunca valores ---

func TestExtractBody_DataKeysYieldNoProps(t *testing.T) {
	bodies := []string{
		`{"payer":{"first_name":"juan_perez","username":"maria.lopez"}}`,
		`{"customer":"cus_9f8a","empresa":"cliente.empresa.shop","path_info":"/home/juan/docs"}`,
		`{"errors":[{"source":"juanita_perez","note":"perez_gomez"}],"name":"maria_lopez"}`,
	}
	for _, b := range bodies {
		f := ExtractBody([]byte(b), 400)
		if len(f.Props) != 0 {
			t.Errorf("%s => props %v", b, f.Props)
		}
	}
	for _, b := range []string{`{"path":"/home/juan/docs"}`, `{"location":"/home/juan/docs","target":"perezgomez","source":"juanita"}`} {
		if f := ExtractBody([]byte(b), 400); len(f.Props) != 0 {
			t.Errorf("%s => props %v", b, f.Props)
		}
	}
	if d, _ := FromEnvelope([]byte(`{"success":false,"error":{"code":"x_y","message":"pago de maria_lopez (maria.lopez) rechazado"}}`)); len(d.Props) != 0 {
		t.Errorf("envelope props %v", d.Props)
	}
	// reason/status con un nombre propio no son codigo de proveedor.
	for _, b := range []string{`{"reason":"carlos"}`, `{"status":"maria","kind":"juan"}`, `{"error":"pedro"}`} {
		if f := ExtractBody([]byte(b), 400); len(f.ProviderCodes) != 0 {
			t.Errorf("%s => codes %v", b, f.ProviderCodes)
		}
	}
	// Los codigos legitimos siguen saliendo.
	f := ExtractBody([]byte(`{"error":"bad_request","status":"rejected","code":"invalid","reason":"cc_rejected_other"}`), 400)
	for _, w := range []string{"bad_request", "rejected", "invalid", "cc_rejected_other"} {
		if !has(f.ProviderCodes, w) {
			t.Errorf("missing code %q in %v", w, f.ProviderCodes)
		}
	}
	// Las rutas de campo salen de claves de mensaje y de claves de ruta.
	f = ExtractBody([]byte(`{"detail":"invalid parameter payer.email","errors":[{"field":"items[0].unit_price"}]}`), 400)
	if !has(f.Props, "payer.email") || !has(f.Props, "items[0].unit_price") {
		t.Errorf("props=%v", f.Props)
	}
}

func TestExtractPaths_ValueCues(t *testing.T) {
	for _, in := range []string{
		"user john_doe (id user_77) at 10.0.0.5",
		"Value juan.perez is not valid",
		"customer maria_lopez rejected",
		"username: maria.lopez taken",
	} {
		if got := ExtractPaths(in); len(got) != 0 {
			t.Errorf("%q => %v", in, got)
		}
	}
	if got := ExtractPaths("Value juan.perez is not valid for field payer.name"); !has(got, "payer.name") || has(got, "juan.perez") {
		t.Errorf("got %v", got)
	}
}

func TestExtractPathsScrubbed_DropsKnownSecrets(t *testing.T) {
	sc := NewScrubber("key_live_abcdef")
	if got := ExtractPathsScrubbed("bad key_live_abcdef in payer.email", sc); has(got, "key_live_abcdef") || !has(got, "payer.email") {
		t.Errorf("got %v", got)
	}
}

// --- capability: sin Scrubber nulo y sin doble registro ---

func newSQLiteDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:diagrev_%s?mode=memory&cache=shared&_busy_timeout=5000", randHex())),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := db.DB()
	s.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = s.Close() })
	if err := Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestOnCapabilityFailure_UsesScrubberAndSkipsWhatHostRecorded(t *testing.T) {
	db := newSQLiteDB(t)
	rec, st := mkRec(t, db, nil, Options{})
	org := uuid.New()
	sc := NewScrubber("tok_live_secretvalue")
	ctx := WithScrubber(context.Background(), sc)
	rec.OnCapabilityFailure(ctx, capability.Failure{Org: org, Capability: "commerce.shipping.rates", Provider: "slowpost", Code: "provider_error",
		Reported: capability.NewCodedError("bad_gateway", "tok_live_secretvalue rejected for rate.zone")})
	// El host del runtime registra un fallo wasm del addon "wasmco" y luego el
	// Executor reporta el mismo fallo: solo debe quedar el del host.
	rec.Record(Diagnostic{Org: org, AddonKey: "wasmco", Kind: KindEnvelope, Operation: "pay", Code: "provider_error", CorrelationID: "c1"}, nil)
	rec.OnCapabilityFailure(WithCorrelation(context.Background(), "c1"), capability.Failure{Org: org, Capability: "commerce.payments.charge", Provider: "wasmco", Code: "provider_error", Duration: 5 * time.Millisecond})
	// Correlacion distinta: es otro fallo.
	rec.OnCapabilityFailure(WithCorrelation(context.Background(), "c2"), capability.Failure{Org: org, Capability: "commerce.payments.charge", Provider: "wasmco", Code: "timeout", Duration: 5 * time.Millisecond})
	rec.Flush(context.Background())
	gs, _, _ := st.List(context.Background(), Filter{Org: org})
	kinds := map[string]int{}
	for _, g := range gs {
		kinds[g.AddonKey+"/"+string(g.Kind)]++
		if g.AddonKey == "slowpost" {
			_, evs, _ := st.Get(context.Background(), org, g.ID)
			if p := evs[0].Diagnostic.Props; has(p, "tok_live_secretvalue") || !has(p, "rate.zone") {
				t.Errorf("props=%v", p)
			}
		}
	}
	if kinds["wasmco/envelope"] != 1 || kinds["wasmco/capability"] != 1 || kinds["slowpost/capability"] != 1 || len(gs) != 3 {
		t.Fatalf("groups=%v", kinds)
	}
}

// --- 2) huecos de saneado verificados por el revisor ---

func TestScrubText_ReviewGaps(t *testing.T) {
	secret := "S3cr3tKeyValue"
	sc := NewScrubber(secret)
	sc.AddSettings(map[string]string{"pin": "12345", "api_user": "acme_merchant"})
	cases := []struct{ in, leak string }{
		{"card 4111.1111.1111.1111 declined", "4111"},
		{"card 4111/1111/1111/1111 declined", "4111"},
		{"card 4111  1111  1111  1111 declined", "4111"},
		{"card 4111 1111-1111.1111 declined", "4111"},
		{"card ４１１１１１１１１１１１１１１１ declined", "１１１"},
		{"order 12 4111 1111 1111 1111 declined", "4111"},
		{"card 4111 1111 1111 1112 declined", "4111"}, // 4x4 sin Luhn valido
		{"url api.example.com/v1/customers?email=foo%40bar.com", "customers"},
		{"url api.example.com/v1/customers?email=foo%40bar.com", "foo"},
		{"ip 189.203.44.12 y 2806:2f0:9000::1 y ::1", "189.203"},
		{"ip 189.203.44.12 y 2806:2f0:9000::1 y ::1", "2806"},
		{"phone +52 55 1234 5678 invalid", "1234"},
		{"phone 55 1234 5678 invalid", "1234"},
		{"email juan​.perez@gmail.com failed", "perez"},
		{"email juan.perez​@gmail.com failed", "juan.perez"},
		{"email juan.perez＠gmail.com failed", "juan.perez"},
		{"password=CorrectHorseBatteryStaple", "CorrectHorse"},
		{`{"password":"CorrectHorseBatteryStaple"}`, "CorrectHorse"},
		{"api_key: abcdefabcdefabcdefabcdefabcdef", "abcdefabcdef"},
		{"secret = hunter two", "hunter"},
		{"Authorization: zk_" + "live_abcdefghijklmnopqrstuvwxyz", "abcdefghij"},
		{"Authorization: Bearer abcdefghijklmnopqrstuvwxyz", "abcdefghij"},
		{"x-api-key=" + hex.EncodeToString([]byte(secret)), "3372"},
		{"k=" + strings.Join(strings.Split(secret, ""), " "), "S 3"},
		{"k=" + base64.URLEncoding.EncodeToString([]byte(secret)), "UzNj"},
		{"k=" + strings.ToLower(secret), "s3cr3t"},
		{"k=S3CR3TKEY value", "S3CR3T"},
		{"pin 12345 user", "12345"},
		{"api_user acme_merchant", "acme_merchant"},
	}
	for _, c := range cases {
		out := ScrubText(c.in, sc)
		if strings.Contains(out, c.leak) {
			t.Errorf("%q -> %q still has %q", c.in, out, c.leak)
		}
		if again := ScrubText(out, sc); again != out {
			t.Errorf("not idempotent: %q -> %q", out, again)
		}
	}
	// Lo que NO debe romperse: host de URL, hora, fecha, ruta de campo.
	keep := ScrubText("GET https://api.x.com/v1/pay at 12:30:45 on 2026-10-24 field payer.email", NewScrubber())
	for _, w := range []string{"https://api.x.com", "12:30:45", "2026-10-24", "payer.email"} {
		if !strings.Contains(keep, w) {
			t.Errorf("over-scrubbed %q: %q", w, keep)
		}
	}
	if !strings.Contains(ScrubText("see api.example.com/v1/x", nil), "api.example.com") {
		t.Error("host of schemeless URL should survive")
	}
}

func TestSanitize_StripsFormatCharsFromBreadcrumbs(t *testing.T) {
	d := Diagnostic{Org: uuid.New(), AddonKey: "acme", Code: "x", Breadcrumbs: []string{"mail ana​.l@example.com now"}}
	o := Sanitize(d, nil)
	if len(o.Breadcrumbs) != 1 || strings.Contains(o.Breadcrumbs[0], "ana") {
		t.Fatalf("crumbs=%q", o.Breadcrumbs)
	}
}

func TestScrubber_SmallKnownSettingsAndLaziness(t *testing.T) {
	sc := NewScrubber()
	sc.AddSettings(map[string]string{"pin": "98765", "flag": "true", "mx": "mx", "name": "acme_corp"})
	// AddSettings solo guarda crudos: nada se expande hasta la primera consulta.
	if len(sc.secrets) != 0 || len(sc.pending) != 4 {
		t.Fatalf("not lazy: secrets=%d pending=%d", len(sc.secrets), len(sc.pending))
	}
	if !sc.Contains("pin 98765") || !sc.Contains("x acme_corp") {
		t.Fatal("known settings must be scrubbed")
	}
	if sc.Contains("true mx") {
		t.Fatal("noise values must not be scrubbed")
	}
	if len(sc.pending) != 0 {
		t.Fatal("pending not materialized")
	}
	// Se pueden anadir mas despues de consultar.
	sc.Add("later-secret")
	if !sc.Contains("a later-secret b") || !sc.Contains("98765") {
		t.Fatal("incremental add broken")
	}
}

func FuzzScrubTextPAN(f *testing.F) {
	f.Add(" ", "-", ".", "")
	f.Add("  ", "/", "_", "12 ")
	f.Fuzz(func(t *testing.T, s1, s2, s3, pre string) {
		for _, s := range []string{s1, s2, s3} {
			if len(s) > 2 {
				return
			}
			for _, r := range s {
				if (r >= '0' && r <= '9') || r > 127 || r < 0x20 || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
					return
				}
			}
		}
		if len(pre) > 6 {
			return
		}
		out := ScrubText("card "+pre+"4111"+s1+"1111"+s2+"1111"+s3+"1111 declined", nil)
		if strings.Count(out, "1111") >= 3 {
			t.Fatalf("PAN survived %q %q %q pre=%q: %q", s1, s2, s3, pre, out)
		}
	})
}

// --- 3) la base colgada no cuelga el cierre ---

func TestRecorder_CloseReturnsWhenStoreHangs(t *testing.T) {
	db := newSQLiteDB(t)
	// Toda escritura Raw se queda esperando hasta que su contexto vence.
	if err := db.Callback().Raw().Replace("gorm:raw", func(tx *gorm.DB) {
		<-tx.Statement.Context.Done()
		_ = tx.AddError(tx.Statement.Context.Err())
	}); err != nil {
		t.Fatal(err)
	}
	on := true
	rec := NewRecorder(NewStore(db), Options{Enabled: &on, FlushEvery: 20 * time.Millisecond, FlushTimeout: 150 * time.Millisecond})
	rec.Start(context.Background())
	org := uuid.New()
	rec.Record(Diagnostic{Org: org, AddonKey: "acme", Code: "boom"}, nil)
	time.Sleep(60 * time.Millisecond) // el tick ya esta dentro de apply

	start := time.Now()
	done := make(chan struct{})
	go func() { rec.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close se quedo colgado con la base sin responder")
	}
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("Close tardo %v", d)
	}
	// Flush directo tambien vuelve a tiempo y devuelve lo pendiente a la cola.
	rec2 := NewRecorder(NewStore(db), Options{Enabled: &on, FlushTimeout: 100 * time.Millisecond})
	rec2.Record(Diagnostic{Org: org, AddonKey: "acme", Code: "boom2"}, nil)
	s2 := time.Now()
	rec2.Flush(context.Background())
	if time.Since(s2) > time.Second {
		t.Fatalf("Flush tardo %v", time.Since(s2))
	}
	rec2.mu.Lock()
	n := len(rec2.pending)
	rec2.mu.Unlock()
	if n != 1 {
		t.Fatalf("pendiente perdido: %d", n)
	}
}

// --- 4) prefiltro del sobre ---

func TestFromEnvelope_Prefilter(t *testing.T) {
	for _, raw := range []string{
		`{"success":false,"error":{"code":"x_y","message":"payer.email"}}`,
		"{\n  \"success\": false,\n  \"error\": {\"code\": \"x_y\"}\n}",
	} {
		if _, ok := FromEnvelope([]byte(raw)); !ok {
			t.Errorf("not recognized: %q", raw)
		}
	}
	for _, raw := range []string{`{"success":true,"data":{"a":1}}`, `{"data":{"ok":false}}`, `{"success":true}`} {
		if _, ok := FromEnvelope([]byte(raw)); ok {
			t.Errorf("false positive: %q", raw)
		}
	}
}

// Prueba manual contra un Postgres real que se congela (docker pause):
//
//	DIAG_PAUSE_CONTAINER=<contenedor> TEST_POSTGRES_DSN=... go test ./diag -run PausedPostgres
func TestRecorder_CloseReturnsWithPausedPostgres(t *testing.T) {
	name := os.Getenv("DIAG_PAUSE_CONTAINER")
	if name == "" {
		t.Skip("DIAG_PAUSE_CONTAINER not set")
	}
	db := pgSchema(t)
	if err := Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	on := true
	rec := NewRecorder(NewStore(db), Options{Enabled: &on, FlushEvery: 100 * time.Millisecond, FlushTimeout: time.Second})
	rec.Start(context.Background())
	org := uuid.New()
	rec.Record(Diagnostic{Org: org, AddonKey: "acme", Code: "warm"}, nil)
	time.Sleep(300 * time.Millisecond)
	if out, err := exec.Command("docker", "pause", name).CombinedOutput(); err != nil {
		t.Fatalf("pause: %v %s", err, out)
	}
	defer exec.Command("docker", "unpause", name).Run()
	rec.Record(Diagnostic{Org: org, AddonKey: "acme", Code: "during"}, nil)
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	done := make(chan struct{})
	go func() { rec.Close(); close(done) }()
	select {
	case <-done:
		t.Logf("Close returned in %v with Postgres paused", time.Since(start))
	case <-time.After(10 * time.Second):
		t.Fatal("Close colgado con Postgres congelado")
	}
}

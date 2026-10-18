package dataload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/importer"
	"github.com/asteby/metacore-kernel/netguard"
)

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func TestBackup_StoresImmutableOriginalBySHA256WithManifest(t *testing.T) {
	r := newRig(t)
	e, err := r.queue.Backup.Backup(context.Background(), "org-a", r.origin.URL("/img/a.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	want := jpegBytes("a")
	if e.SHA256 != sha(want) || e.Bytes != int64(len(want)) || e.ContentType != "image/jpeg" || e.Status != EntryOK {
		t.Fatalf("entrada = %+v", e)
	}
	if !bytes.Equal(r.store.Objects[e.StoredName], want) {
		t.Fatal("el original guardado no es idéntico al descargado")
	}
	if !strings.Contains(e.StoredName, e.SHA256) {
		t.Fatalf("el nombre debe estar direccionado por sha256: %s", e.StoredName)
	}
	if e.SourceURL != r.origin.URL("/img/a.jpg") || e.FetchedAt.IsZero() || e.VerifiedAt.IsZero() {
		t.Fatalf("manifiesto incompleto: %+v", e)
	}
	got, ok, _ := r.manifest.ByURL(context.Background(), "org-a", e.SourceURL)
	if !ok || !reflect.DeepEqual(got, e) {
		t.Fatal("el manifiesto no quedó guardado")
	}
}

func TestBackup_SameURLAgainDoesNotTouchNetworkOrStore(t *testing.T) {
	r := newRig(t)
	u := r.origin.URL("/img/a.jpg")
	_, _ = r.queue.Backup.Backup(context.Background(), "org-a", u)
	_, err := r.queue.Backup.Backup(context.Background(), "org-a", u)
	if err != nil || r.origin.Hits("/img/a.jpg") != 1 || r.store.Puts != 1 {
		t.Fatalf("err=%v hits=%d puts=%d", err, r.origin.Hits("/img/a.jpg"), r.store.Puts)
	}
}

func TestBackup_SameContentUnderTwoURLsIsWrittenOnce(t *testing.T) {
	r := newRig(t)
	a, _ := r.queue.Backup.Backup(context.Background(), "org-a", r.origin.URL("/same/1.jpg"))
	b, err := r.queue.Backup.Backup(context.Background(), "org-a", r.origin.URL("/same/2.jpg"))
	if err != nil || a.StoredName != b.StoredName || r.store.Puts != 1 {
		t.Fatalf("err=%v puts=%d", err, r.store.Puts)
	}
	all, _ := r.manifest.List(context.Background(), "org-a")
	if len(all) != 2 {
		t.Fatalf("el manifiesto debe listar ambas URLs de origen, got %d", len(all))
	}
}

func TestBackup_OriginalIsNeverOverwritten(t *testing.T) {
	r := newRig(t)
	e, _ := r.queue.Backup.Backup(context.Background(), "org-a", r.origin.URL("/same/1.jpg"))
	r.store.Objects[e.StoredName] = []byte("alterado") // simula corrupción en el almacén
	// Otra URL con el mismo contenido: el almacén dice "ya existe", no se reescribe y la verificación lo delata.
	_, err := r.queue.Backup.Backup(context.Background(), "org-a", r.origin.URL("/same/2.jpg"))
	if !errors.Is(err, ErrBackupCorrupt) {
		t.Fatalf("debía detectar la corrupción, got %v", err)
	}
	if string(r.store.Objects[e.StoredName]) != "alterado" {
		t.Fatal("el original existente no debe reescribirse en silencio")
	}
	bad, _, _ := r.manifest.ByURL(context.Background(), "org-a", r.origin.URL("/same/2.jpg"))
	if bad.Status != EntryCorrupt {
		t.Fatalf("el manifiesto debe dejar constancia: %+v", bad)
	}
}

func TestBackup_DoesNotStoreTruncatedOrNonImage(t *testing.T) {
	r := newRig(t)
	_, err := r.queue.Backup.Backup(context.Background(), "org-a", r.origin.URL("/notimage"))
	if !errors.Is(err, importer.ErrMediaType) || r.store.Puts != 0 {
		t.Fatalf("err=%v puts=%d", err, r.store.Puts)
	}
	r.queue.Backup.Fetch.Policy.MaxBytes = 8
	_, err = r.queue.Backup.Backup(context.Background(), "org-a", r.origin.URL("/img/a.jpg"))
	if !errors.Is(err, importer.ErrMediaTooLarge) || r.store.Puts != 0 {
		t.Fatalf("err=%v puts=%d", err, r.store.Puts)
	}
}

func TestVerifyAll_ReportsMissingAndCorrupt(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	a, _ := r.queue.Backup.Backup(ctx, "org-a", r.origin.URL("/img/a.jpg"))
	b, _ := r.queue.Backup.Backup(ctx, "org-a", r.origin.URL("/img/b.jpg"))
	_, _ = r.queue.Backup.Backup(ctx, "org-a", r.origin.URL("/img/c.jpg"))
	rep, err := r.queue.Backup.VerifyAll(ctx, "org-a")
	if err != nil || !rep.Healthy() || rep.OK != 3 {
		t.Fatalf("%v %+v", err, rep)
	}
	delete(r.store.Objects, a.StoredName)
	r.store.Objects[b.StoredName] = []byte("x")
	rep, _ = r.queue.Backup.VerifyAll(ctx, "org-a")
	if rep.Healthy() || len(rep.Missing) != 1 || len(rep.Corrupt) != 1 || rep.OK != 1 {
		t.Fatalf("rep = %+v", rep)
	}
	if e, _, _ := r.manifest.ByURL(ctx, "org-a", a.SourceURL); e.Status != EntryMissing {
		t.Fatalf("estado del manifiesto = %s", e.Status)
	}
}

func TestBackup_ReDownloadsWhenManifestSaysMissing(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	a, _ := r.queue.Backup.Backup(ctx, "org-a", r.origin.URL("/img/a.jpg"))
	delete(r.store.Objects, a.StoredName)
	_, _ = r.queue.Backup.VerifyAll(ctx, "org-a")
	if _, err := r.queue.Backup.Backup(ctx, "org-a", a.SourceURL); err != nil {
		t.Fatal(err)
	}
	if r.origin.Hits("/img/a.jpg") != 2 || !bytes.Equal(r.store.Objects[a.StoredName], jpegBytes("a")) {
		t.Fatal("debía restaurar el respaldo faltante")
	}
}

func TestExportManifest_ListsAllFields(t *testing.T) {
	r := newRig(t)
	_, _ = r.queue.Backup.Backup(context.Background(), "org-a", r.origin.URL("/img/a.jpg"))
	b, err := ExportManifest(context.Background(), r.manifest, "org-a")
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	if err := json.Unmarshal(b, &out); err != nil || len(out) != 1 {
		t.Fatalf("%v %s", err, b)
	}
	for _, k := range []string{"sourceUrl", "sha256", "bytes", "contentType", "fetchedAt", "storedName", "status"} {
		if _, ok := out[0][k]; !ok {
			t.Errorf("falta %q en el manifiesto", k)
		}
	}
	if empty, _ := ExportManifest(context.Background(), r.manifest, "otra"); strings.TrimSpace(string(empty)) != "[]" {
		t.Errorf("manifiesto vacío = %s", empty)
	}
}

func enqueue(t *testing.T, r *rig, urls ...string) {
	t.Helper()
	media := map[string][]string{"images": urls}
	if _, err := r.queue.Enqueue(context.Background(), r.queue.jobsFor("org-a", "item", "run1", "k1", media)); err != nil {
		t.Fatal(err)
	}
}

func TestMediaQueue_EnqueueIsIdempotent(t *testing.T) {
	r := newRig(t)
	enqueue(t, r, r.origin.URL("/img/a.jpg"))
	n, _ := r.queue.Enqueue(context.Background(), r.queue.jobsFor("org-a", "item", "run2", "k1", map[string][]string{"images": {r.origin.URL("/img/a.jpg")}}))
	if n != 0 {
		t.Fatalf("un trabajo repetido no debe duplicarse, added=%d", n)
	}
}

func TestMediaQueue_RetriesTransientFailuresWithBackoffThenSucceeds(t *testing.T) {
	r := newRig(t)
	r.origin.Flaky("/img/a.jpg", 2)
	enqueue(t, r, r.origin.URL("/img/a.jpg"))
	ctx := context.Background()

	_, _ = r.queue.ProcessOnce(ctx, 10)
	jobs, _ := r.jobs.List(ctx, "org-a", MediaJobFilter{})
	if j := jobs[0]; j.State != MediaPending || j.Attempts != 1 || !strings.Contains(j.LastError, "503") {
		t.Fatalf("tras el 1er fallo: %+v", j)
	}
	if n, _ := r.queue.ProcessOnce(ctx, 10); n != 0 {
		t.Fatal("no debe reintentar antes de que venza la espera")
	}
	r.clk.Advance(3 * time.Second) // 2s del primer retraso
	_, _ = r.queue.ProcessOnce(ctx, 10)
	jobs, _ = r.jobs.List(ctx, "org-a", MediaJobFilter{})
	if jobs[0].Attempts != 2 || jobs[0].State != MediaPending {
		t.Fatalf("tras el 2º fallo: %+v", jobs[0])
	}
	r.clk.Advance(9 * time.Second) // 8s del segundo
	_, _ = r.queue.ProcessOnce(ctx, 10)
	jobs, _ = r.jobs.List(ctx, "org-a", MediaJobFilter{})
	if jobs[0].State != MediaDone || jobs[0].Attempts != 3 || jobs[0].SHA256 != sha(jpegBytes("a")) {
		t.Fatalf("final: %+v", jobs[0])
	}
}

func TestMediaQueue_GivesUpAfterMaxAttemptsAndRetryFailedRequeues(t *testing.T) {
	r := newRig(t)
	r.queue.MaxAttempts = 2
	r.origin.Flaky("/img/a.jpg", 100)
	enqueue(t, r, r.origin.URL("/img/a.jpg"))
	ctx := context.Background()
	r.drain(t, "org-a")
	st, _ := r.queue.Stats(ctx, "org-a", "")
	if st.Dead != 1 {
		t.Fatalf("stats = %+v", st)
	}
	r.origin.Flaky("/img/a.jpg", 0)
	n, _ := r.queue.RetryFailed(ctx, "org-a", "run1")
	if n != 1 {
		t.Fatalf("requeue = %d", n)
	}
	r.drain(t, "org-a")
	if st, _ = r.queue.Stats(ctx, "org-a", ""); st.Done != 1 || st.Dead != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestMediaQueue_DefinitiveFailuresAreNotRetried(t *testing.T) {
	r := newRig(t)
	enqueue(t, r, r.origin.URL("/nope.jpg"), r.origin.URL("/notimage"))
	_, _ = r.queue.ProcessOnce(context.Background(), 10)
	jobs, _ := r.jobs.List(context.Background(), "org-a", MediaJobFilter{})
	for _, j := range jobs {
		if j.State != MediaDead || j.Attempts != 1 {
			t.Errorf("%s debía morir al primer intento: %+v", j.URL, j)
		}
	}
	if r.origin.Hits("/nope.jpg") != 1 {
		t.Errorf("un 404 no se reintenta, hits=%d", r.origin.Hits("/nope.jpg"))
	}
}

func TestMediaQueue_DefaultClientBlocksInternalDestinations(t *testing.T) {
	r := newRig(t)
	r.queue.Backup.Fetch.HTTPClient = nil // cliente por defecto: netguard
	enqueue(t, r, r.origin.URL("/img/a.jpg"))
	_, _ = r.queue.ProcessOnce(context.Background(), 10)
	jobs, _ := r.jobs.List(context.Background(), "org-a", MediaJobFilter{})
	if jobs[0].State != MediaDead || !strings.Contains(jobs[0].LastError, netguard.ErrBlockedEgress.Error()) {
		t.Fatalf("loopback debía bloquearse: %+v", jobs[0])
	}
	if r.store.Puts != 0 {
		t.Fatal("no debe guardarse nada de un destino bloqueado")
	}
}

func TestMediaQueue_AllowHostsPolicyIsEnforced(t *testing.T) {
	r := newRig(t)
	r.queue.Backup.Fetch.Policy.AllowHosts = []string{"cdn.ejemplo.com"}
	enqueue(t, r, r.origin.URL("/img/a.jpg"))
	_, _ = r.queue.ProcessOnce(context.Background(), 10)
	jobs, _ := r.jobs.List(context.Background(), "org-a", MediaJobFilter{})
	if jobs[0].State != MediaDead || !strings.Contains(jobs[0].LastError, netguard.ErrHostNotAllowed.Error()) {
		t.Fatalf("job = %+v", jobs[0])
	}
	if r.origin.Hits("/img/a.jpg") != 0 {
		t.Fatal("no debe ni conectarse a un host fuera de la lista")
	}
}

func TestMediaQueue_AttachesOrderedRefsOnlyWhenFieldSettles(t *testing.T) {
	r := newRig(t)
	r.origin.Flaky("/img/b.jpg", 1) // la 2ª imagen tarda un reintento
	enqueue(t, r, r.origin.URL("/img/a.jpg"), r.origin.URL("/img/b.jpg"), r.origin.URL("/img/c.jpg"))
	ctx := context.Background()
	_, _ = r.queue.ProcessOnce(ctx, 10)
	if r.sink.Attachs != 0 {
		t.Fatal("no debe ligar mientras quede un medio pendiente")
	}
	r.clk.Advance(5 * time.Second)
	_, _ = r.queue.ProcessOnce(ctx, 10)
	refs := r.sink.Get("org-a", "item", "k1", "images")
	if len(refs) != 3 || r.sink.Attachs != 1 {
		t.Fatalf("refs=%v attachs=%d", refs, r.sink.Attachs)
	}
	for i, tag := range []string{"a", "b", "c"} {
		if refs[i].Index != i || refs[i].SHA256 != sha(jpegBytes(tag)) || refs[i].SourceURL == "" || refs[i].StoredName == "" {
			t.Errorf("ref %d = %+v", i, refs[i])
		}
	}
}

func TestMediaQueue_FailedMediaDoesNotBlockTheRest(t *testing.T) {
	r := newRig(t)
	enqueue(t, r, r.origin.URL("/img/a.jpg"), r.origin.URL("/nope.jpg"), r.origin.URL("/img/c.jpg"))
	r.drain(t, "org-a")
	refs := r.sink.Get("org-a", "item", "k1", "images")
	if len(refs) != 2 || refs[0].SHA256 != sha(jpegBytes("a")) || refs[1].SHA256 != sha(jpegBytes("c")) || refs[1].Index != 1 {
		t.Fatalf("refs = %+v", refs)
	}
	// Al reintentar y recuperarse, se vuelve a ligar el conjunto completo.
}

type derivSpy struct {
	calls []ManifestEntry
	err   error
}

func (d *derivSpy) Derive(_ context.Context, e ManifestEntry) error {
	d.calls = append(d.calls, e)
	return d.err
}

func TestMediaQueue_DeriverRunsAfterBackupAndItsFailureKeepsTheOriginal(t *testing.T) {
	r := newRig(t)
	spy := &derivSpy{err: errors.New("sin libwebp")}
	r.queue.Deriver = spy
	enqueue(t, r, r.origin.URL("/img/a.jpg"))
	r.drain(t, "org-a")
	jobs, _ := r.jobs.List(context.Background(), "org-a", MediaJobFilter{})
	if len(spy.calls) != 1 || jobs[0].State != MediaDone || jobs[0].DeriveError == "" {
		t.Fatalf("calls=%d job=%+v", len(spy.calls), jobs[0])
	}
	if !bytes.Equal(r.store.Objects[spy.calls[0].StoredName], jpegBytes("a")) {
		t.Fatal("el original debe quedar intacto")
	}
}

func TestMediaQueue_ExpiredLeaseIsReclaimed(t *testing.T) {
	r := newRig(t)
	enqueue(t, r, r.origin.URL("/img/a.jpg"))
	ctx := context.Background()
	got, _ := r.jobs.Claim(ctx, r.clk.Now(), 5, time.Minute) // un worker que "murió"
	if len(got) != 1 {
		t.Fatal("no reclamó")
	}
	if n, _ := r.queue.ProcessOnce(ctx, 5); n != 0 {
		t.Fatal("el arrendamiento vigente no debe tomarse")
	}
	r.clk.Advance(6 * time.Minute)
	if n, _ := r.queue.ProcessOnce(ctx, 5); n != 1 {
		t.Fatal("el arrendamiento vencido debía reclamarse")
	}
}

func TestMediaQueue_ShutdownReleasesJobWithoutBurningAnAttempt(t *testing.T) {
	r := newRig(t)
	enqueue(t, r, r.origin.URL("/img/a.jpg"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = r.queue.ProcessOnce(ctx, 5)
	jobs, _ := r.jobs.List(context.Background(), "org-a", MediaJobFilter{})
	if jobs[0].State != MediaPending || jobs[0].Attempts != 0 {
		t.Fatalf("job = %+v", jobs[0])
	}
}

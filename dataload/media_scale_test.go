package dataload

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// vorigin es un origen de imágenes con versiones: ETag, 304, Range y cortes de conexión.
type vorigin struct {
	*httptest.Server
	mu        sync.Mutex
	body      map[string][]byte
	etag      map[string]string
	cutFirst  map[string]int
	cut       map[string]bool
	reqs      []vreq
	delay     time.Duration
	active    map[string]int
	maxActive map[string]int
	down      bool
}

type vreq struct {
	Path, Range, IfNoneMatch string
	Status                   int
}

func jpegN(tag string, n int) []byte {
	return append(jpegBytes(tag), bytes.Repeat([]byte("x"), n)...)
}

func newVOrigin(t *testing.T) *vorigin {
	t.Helper()
	o := &vorigin{body: map[string][]byte{}, etag: map[string]string{}, cutFirst: map[string]int{}, cut: map[string]bool{}, active: map[string]int{}, maxActive: map[string]int{}}
	o.Server = httptest.NewServer(http.HandlerFunc(o.serve))
	t.Cleanup(o.Close)
	return o
}

func (o *vorigin) set(path string, body []byte, etag string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.body[path], o.etag[path] = body, etag
}

func (o *vorigin) URL(path string) string { return o.Server.URL + path }

func (o *vorigin) requests(path string) []vreq {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []vreq
	for _, r := range o.reqs {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (o *vorigin) maxConcurrent(prefix string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.maxActive[prefix]
}

func (o *vorigin) serve(w http.ResponseWriter, r *http.Request) {
	prefix := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)[0]
	o.mu.Lock()
	if o.down {
		o.mu.Unlock()
		panic(http.ErrAbortHandler)
	}
	o.active[prefix]++
	if o.active[prefix] > o.maxActive[prefix] {
		o.maxActive[prefix] = o.active[prefix]
	}
	body, etag := o.body[r.URL.Path], o.etag[r.URL.Path]
	delay := o.delay
	cutAt := o.cutFirst[r.URL.Path]
	doCut := cutAt > 0 && !o.cut[r.URL.Path]
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		o.active[prefix]--
		o.mu.Unlock()
	}()
	if delay > 0 {
		time.Sleep(delay)
	}
	rec := func(code int) {
		o.mu.Lock()
		o.reqs = append(o.reqs, vreq{r.URL.Path, r.Header.Get("Range"), r.Header.Get("If-None-Match"), code})
		o.mu.Unlock()
	}
	if body == nil {
		rec(404)
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "image/jpeg")
	if etag != "" {
		h.Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			rec(304)
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	if rg := r.Header.Get("Range"); rg != "" && etag != "" && r.Header.Get("If-Range") == etag {
		var start int
		if _, err := fmt.Sscanf(rg, "bytes=%d-", &start); err == nil && start < len(body) {
			h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(body)-1, len(body)))
			h.Set("Content-Length", strconv.Itoa(len(body)-start))
			rec(206)
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(body[start:])
			return
		}
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	rec(200)
	if doCut {
		o.mu.Lock()
		o.cut[r.URL.Path] = true
		o.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body[:cutAt])
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}
	_, _ = w.Write(body)
}

func enqueueURLs(t *testing.T, r *rig, org, field string, urls ...string) {
	t.Helper()
	var jobs []MediaJob
	for i, u := range urls {
		jobs = append(jobs, r.queue.jobsFor(org, "item", "run-"+org, fmt.Sprintf("k%d", i), map[string][]string{field: {u}})...)
	}
	if _, err := r.queue.Enqueue(context.Background(), jobs); err != nil {
		t.Fatal(err)
	}
}

func TestMediaQueue_PerOrgConcurrencyCapHoldsWhileOtherOrgsRunFreely(t *testing.T) {
	r := newRig(t)
	o := newVOrigin(t)
	o.delay = 40 * time.Millisecond
	var urlsA, urlsB []string
	for i := 0; i < 6; i++ {
		pa, pb := fmt.Sprintf("/a/%d.jpg", i), fmt.Sprintf("/b/%d.jpg", i)
		o.set(pa, jpegN("a"+strconv.Itoa(i), 100), "")
		o.set(pb, jpegN("b"+strconv.Itoa(i), 100), "")
		urlsA, urlsB = append(urlsA, o.URL(pa)), append(urlsB, o.URL(pb))
	}
	r.queue.Workers = 8
	r.queue.Limits = StaticOrgLimits(OrgMediaLimits{}, map[string]OrgMediaLimits{"org-a": {MaxConcurrent: 1}})
	enqueueURLs(t, r, "org-a", "images", urlsA...)
	enqueueURLs(t, r, "org-b", "images", urlsB...)

	n, err := r.queue.ProcessOnce(context.Background(), 100)
	if err != nil || n != 12 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := o.maxConcurrent("a"); got != 1 {
		t.Fatalf("org-a tiene tope 1 y llegó a %d simultáneos", got)
	}
	if got := o.maxConcurrent("b"); got < 2 {
		t.Fatalf("org-b no tiene tope propio y debería haber paralelizado (máx %d)", got)
	}
	for _, org := range []string{"org-a", "org-b"} {
		if st, _ := r.queue.Stats(context.Background(), org, ""); st.Done != 6 {
			t.Fatalf("%s: %+v", org, st)
		}
	}
	if r.queue.ActiveForOrg("org-a") != 0 {
		t.Fatal("los lugares reservados deben liberarse")
	}
}

func TestMediaQueue_CapHoldsAcrossConcurrentPasses(t *testing.T) {
	r := newRig(t)
	o := newVOrigin(t)
	o.delay = 30 * time.Millisecond
	var urls []string
	for i := 0; i < 8; i++ {
		p := fmt.Sprintf("/a/%d.jpg", i)
		o.set(p, jpegN("c"+strconv.Itoa(i), 50), "")
		urls = append(urls, o.URL(p))
	}
	r.queue.Limits = StaticOrgLimits(OrgMediaLimits{MaxConcurrent: 2}, nil)
	enqueueURLs(t, r, "org-a", "images", urls...)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ { // tres bucles de worker a la vez, un solo tope
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = r.queue.ProcessOnce(context.Background(), 3) }()
	}
	wg.Wait()
	if got := o.maxConcurrent("a"); got > 2 {
		t.Fatalf("el tope de la organización es por cola, no por pasada: %d", got)
	}
}

func TestRoundRobinByOrg_InterleavesWithoutReorderingWithinAnOrg(t *testing.T) {
	var jobs []MediaJob
	for i := 0; i < 4; i++ {
		jobs = append(jobs, MediaJob{ID: fmt.Sprintf("a%d", i), Org: "a"})
	}
	jobs = append(jobs, MediaJob{ID: "b0", Org: "b"}, MediaJob{ID: "c0", Org: "c"}, MediaJob{ID: "b1", Org: "b"})
	var ids []string
	for _, j := range roundRobinByOrg(jobs) {
		ids = append(ids, j.ID)
	}
	if strings.Join(ids, ",") != "a0,b0,c0,a1,b1,a2,a3" {
		t.Fatalf("orden = %v", ids)
	}
}

func TestMediaQueue_PerOrgRetryCap(t *testing.T) {
	r := newRig(t)
	r.queue.Limits = StaticOrgLimits(OrgMediaLimits{}, map[string]OrgMediaLimits{"org-a": {MaxAttempts: 2}})
	r.origin.Flaky("/img/x.jpg", 100)
	r.origin.Flaky("/img/y.jpg", 100)
	enqueueURLs(t, r, "org-a", "images", r.origin.URL("/img/x.jpg"))
	enqueueURLs(t, r, "org-b", "images", r.origin.URL("/img/y.jpg"))
	for i := 0; i < 8; i++ {
		_, _ = r.queue.ProcessOnce(context.Background(), 10)
		r.clk.Advance(15 * time.Minute)
	}
	ja, _ := r.jobs.List(context.Background(), "org-a", MediaJobFilter{})
	jb, _ := r.jobs.List(context.Background(), "org-b", MediaJobFilter{})
	if ja[0].State != MediaDead || ja[0].Attempts != 2 {
		t.Fatalf("org-a debía rendirse a los 2 intentos: %+v", ja[0])
	}
	if jb[0].State != MediaDead || jb[0].Attempts != 5 {
		t.Fatalf("org-b usa el tope de la cola (5): %+v", jb[0])
	}
}

func TestMediaQueue_ConditionalRevalidationWith304KeepsTheOriginalAndTouchesNothing(t *testing.T) {
	r := newRig(t)
	o := newVOrigin(t)
	o.set("/p/a.jpg", jpegN("v1", 500), `"v1"`)
	u := o.URL("/p/a.jpg")
	enqueueURLs(t, r, "org-a", "images", u)
	r.drain(t, "org-a")
	first, _, _ := r.manifest.ByURL(context.Background(), "org-a", u)
	if first.ETag != `"v1"` || first.ValidatedAt.IsZero() {
		t.Fatalf("el manifiesto guarda los validadores: %+v", first)
	}
	puts := r.store.Puts

	r.clk.Advance(48 * time.Hour)
	n, err := r.queue.Revalidate(context.Background(), "org-a", MediaJobFilter{}, RevalidateConditional)
	if err != nil || n != 1 {
		t.Fatalf("%v %d", err, n)
	}
	r.drain(t, "org-a")
	reqs := o.requests("/p/a.jpg")
	if len(reqs) != 2 || reqs[1].IfNoneMatch != `"v1"` || reqs[1].Status != 304 {
		t.Fatalf("segunda petición condicional: %+v", reqs)
	}
	after, _, _ := r.manifest.ByURL(context.Background(), "org-a", u)
	if after.SHA256 != first.SHA256 || !after.FetchedAt.Equal(first.FetchedAt) || !after.ValidatedAt.After(first.ValidatedAt) || r.store.Puts != puts || len(after.Revisions) != 0 {
		t.Fatalf("un 304 solo renueva ValidatedAt: %+v", after)
	}
	jobs, _ := r.jobs.List(context.Background(), "org-a", MediaJobFilter{})
	if jobs[0].State != MediaDone || jobs[0].Outcome != OutcomeNotModified || jobs[0].Revalidate != RevalidateNone {
		t.Fatalf("job = %+v", jobs[0])
	}
}

func TestMediaQueue_ChangedOriginStoresANewOriginalWithAnotherSHA256AndKeepsTheOldOne(t *testing.T) {
	r := newRig(t)
	o := newVOrigin(t)
	o.set("/p/a.jpg", jpegN("v1", 500), `"v1"`)
	u := o.URL("/p/a.jpg")
	enqueueURLs(t, r, "org-a", "images", u)
	r.drain(t, "org-a")
	old, _, _ := r.manifest.ByURL(context.Background(), "org-a", u)
	if got := r.sink.Get("org-a", "item", "k0", "images"); len(got) != 1 || got[0].SHA256 != old.SHA256 {
		t.Fatalf("sink inicial: %+v", got)
	}

	o.set("/p/a.jpg", jpegN("v2-nuevo", 700), `"v2"`)
	r.clk.Advance(time.Hour)
	if _, err := r.queue.Revalidate(context.Background(), "org-a", MediaJobFilter{}, RevalidateConditional); err != nil {
		t.Fatal(err)
	}
	r.drain(t, "org-a")

	cur, _, _ := r.manifest.ByURL(context.Background(), "org-a", u)
	if cur.SHA256 == old.SHA256 || cur.SHA256 != sha(jpegN("v2-nuevo", 700)) || cur.ETag != `"v2"` {
		t.Fatalf("debe apuntar al original nuevo: %+v", cur)
	}
	if len(cur.Revisions) != 1 || cur.Revisions[0].SHA256 != old.SHA256 || cur.Revisions[0].StoredName != old.StoredName {
		t.Fatalf("la revisión anterior queda documentada: %+v", cur.Revisions)
	}
	if !bytes.Equal(r.store.Objects[old.StoredName], jpegN("v1", 500)) {
		t.Fatal("el original anterior debe seguir intacto en el almacén")
	}
	if !bytes.Equal(r.store.Objects[cur.StoredName], jpegN("v2-nuevo", 700)) || cur.StoredName == old.StoredName {
		t.Fatal("el original nuevo se guarda aparte")
	}
	refs := r.sink.Get("org-a", "item", "k0", "images")
	if len(refs) != 1 || refs[0].SHA256 != cur.SHA256 {
		t.Fatalf("el registro debe apuntar al original nuevo: %+v", refs)
	}
	rep, _ := r.queue.Backup.VerifyAll(context.Background(), "org-a")
	if !rep.Healthy() {
		t.Fatalf("%+v", rep)
	}
}

func TestBackuper_ExplicitRefetchSavesANewOriginalOnlyWhenTheContentChanged(t *testing.T) {
	r := newRig(t)
	o := newVOrigin(t)
	o.set("/p/a.jpg", jpegN("v1", 300), "") // sin validadores: solo sirve el refetch explícito
	u := o.URL("/p/a.jpg")
	b := r.queue.Backup
	ctx := context.Background()
	first, err := b.Backup(ctx, "org-a", u)
	if err != nil {
		t.Fatal(err)
	}
	puts := r.store.Puts

	e, res, err := b.Refetch(ctx, "org-a", u)
	if err != nil || res.Outcome != OutcomeUnchanged || e.SHA256 != first.SHA256 || r.store.Puts != puts || len(e.Revisions) != 0 {
		t.Fatalf("mismo contenido: %v %+v", err, res)
	}
	o.set("/p/a.jpg", jpegN("v2", 300), "")
	e, res, err = b.Refetch(ctx, "org-a", u)
	if err != nil || res.Outcome != OutcomeReplaced || e.SHA256 == first.SHA256 || r.store.Puts != puts+1 || len(e.Revisions) != 1 {
		t.Fatalf("contenido nuevo: %v %+v", err, res)
	}
	// Un segundo cambio acumula la historia.
	o.set("/p/a.jpg", jpegN("v3", 300), "")
	e, _, _ = b.Refetch(ctx, "org-a", u)
	if len(e.Revisions) != 2 || e.Revisions[0].SHA256 != first.SHA256 {
		t.Fatalf("revisiones = %+v", e.Revisions)
	}
	// Conditional sin validadores guardados degrada a bajar de nuevo.
	o.set("/p/a.jpg", jpegN("v4", 300), "")
	_, res, _ = b.Ensure(ctx, "org-a", u, RevalidateConditional)
	if res.Outcome != OutcomeReplaced {
		t.Fatalf("outcome = %s", res.Outcome)
	}
}

func TestBackuper_RevalidatePolicyOnlyAppliesPastMaxAge(t *testing.T) {
	r := newRig(t)
	o := newVOrigin(t)
	o.set("/p/a.jpg", jpegN("v1", 300), `"v1"`)
	u := o.URL("/p/a.jpg")
	b := r.queue.Backup
	b.Revalidate = RevalidatePolicy{Mode: RevalidateConditional, MaxAge: 24 * time.Hour}
	ctx := context.Background()
	if _, _, err := b.Ensure(ctx, "org-a", u, RevalidateNone); err != nil {
		t.Fatal(err)
	}
	r.clk.Advance(time.Hour)
	_, res, _ := b.Ensure(ctx, "org-a", u, RevalidateNone)
	if res.Outcome != OutcomeFresh || len(o.requests("/p/a.jpg")) != 1 {
		t.Fatalf("dentro de MaxAge no se toca la red: %s", res.Outcome)
	}
	r.clk.Advance(48 * time.Hour)
	_, res, _ = b.Ensure(ctx, "org-a", u, RevalidateNone)
	if res.Outcome != OutcomeNotModified || len(o.requests("/p/a.jpg")) != 2 {
		t.Fatalf("pasado MaxAge se revalida: %s", res.Outcome)
	}
	// Y la validación renovada reinicia el reloj.
	_, res, _ = b.Ensure(ctx, "org-a", u, RevalidateNone)
	if res.Outcome != OutcomeFresh {
		t.Fatalf("outcome = %s", res.Outcome)
	}
	// Sin política (valor cero) nunca revalida, como siempre.
	b.Revalidate = RevalidatePolicy{}
	r.clk.Advance(1000 * time.Hour)
	if _, res, _ = b.Ensure(ctx, "org-a", u, RevalidateNone); res.Outcome != OutcomeFresh {
		t.Fatalf("outcome = %s", res.Outcome)
	}
}

func TestBackuper_RevalidationFailureKeepsTheGoodOriginalInsteadOfLosingTheMedia(t *testing.T) {
	r := newRig(t)
	o := newVOrigin(t)
	o.set("/p/a.jpg", jpegN("v1", 300), `"v1"`)
	u := o.URL("/p/a.jpg")
	enqueueURLs(t, r, "org-a", "images", u)
	r.drain(t, "org-a")
	old, _, _ := r.manifest.ByURL(context.Background(), "org-a", u)

	o.mu.Lock()
	o.down = true // el sitio de origen se cae
	o.mu.Unlock()
	if _, err := r.queue.Revalidate(context.Background(), "org-a", MediaJobFilter{}, RevalidateRefetch); err != nil {
		t.Fatal(err)
	}
	r.drain(t, "org-a")
	jobs, _ := r.jobs.List(context.Background(), "org-a", MediaJobFilter{})
	cur, _, _ := r.manifest.ByURL(context.Background(), "org-a", u)
	if jobs[0].State != MediaDone || jobs[0].Outcome != OutcomeStale || !strings.Contains(jobs[0].LastError, "revalidación") {
		t.Fatalf("job = %+v", jobs[0])
	}
	if cur.SHA256 != old.SHA256 || cur.Status != EntryOK {
		t.Fatal("el original respaldado sigue vigente")
	}
	if refs := r.sink.Get("org-a", "item", "k0", "images"); len(refs) != 1 || refs[0].SHA256 != old.SHA256 {
		t.Fatalf("la galería no pierde la imagen: %+v", refs)
	}
}

func TestMediaQueue_ResumesACutDownloadWithRangeInsteadOfStartingOver(t *testing.T) {
	r := newRig(t)
	o := newVOrigin(t)
	big := jpegN("grande", 20000)
	o.set("/p/big.jpg", big, `"g1"`)
	o.cutFirst["/p/big.jpg"] = 7000
	r.queue.Backup.Fetch.Policy.MaxAttempts = 3
	u := o.URL("/p/big.jpg")
	enqueueURLs(t, r, "org-a", "images", u)
	r.drain(t, "org-a")
	e, ok, _ := r.manifest.ByURL(context.Background(), "org-a", u)
	if !ok || e.SHA256 != sha(big) || e.Bytes != int64(len(big)) {
		t.Fatalf("el original debe quedar completo e idéntico: %+v", e)
	}
	reqs := o.requests("/p/big.jpg")
	if len(reqs) != 2 || reqs[1].Range != "bytes=7000-" || reqs[1].Status != 206 {
		t.Fatalf("la segunda petición debe reanudar con Range: %+v", reqs)
	}
	if !bytes.Equal(r.store.Objects[e.StoredName], big) {
		t.Fatal("bytes guardados distintos")
	}
}

func TestMediaQueue_RevalidateValidatesModeAndOnlyTouchesFinishedJobs(t *testing.T) {
	r := newRig(t)
	o := newVOrigin(t)
	o.set("/p/a.jpg", jpegN("v1", 100), `"v1"`)
	enqueueURLs(t, r, "org-a", "images", o.URL("/p/a.jpg"), o.URL("/p/ausente.jpg"))
	if _, err := r.queue.Revalidate(context.Background(), "org-a", MediaJobFilter{}, "otra-cosa"); err == nil {
		t.Fatal("modo desconocido")
	}
	if n, _ := r.queue.Revalidate(context.Background(), "org-a", MediaJobFilter{}, RevalidateConditional); n != 0 {
		t.Fatalf("lo que no terminó no se revalida: %d", n)
	}
	for i := 0; i < 3; i++ {
		_, _ = r.queue.ProcessOnce(context.Background(), 10)
		r.clk.Advance(15 * time.Minute)
	}
	// /ausente.jpg dio 404: murió. Solo el que terminó se revalida.
	if n, err := r.queue.Revalidate(context.Background(), "org-a", MediaJobFilter{}, RevalidateConditional); err != nil || n != 1 {
		t.Fatalf("%v %d", err, n)
	}
	// Otra organización no se toca.
	if n, _ := r.queue.Revalidate(context.Background(), "org-b", MediaJobFilter{}, RevalidateRefetch); n != 0 {
		t.Fatalf("%d", n)
	}
}

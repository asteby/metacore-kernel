package dataload

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/importer"
)

// partialRig arma una cola cuyo Backuper guarda las descargas cortadas y que
// hace un solo intento de red por pasada (la reanudación ocurre entre pasadas).
func partialRig(t *testing.T) (*rig, *vorigin, *MemoryPartialStore) {
	r := newRig(t)
	o := newVOrigin(t)
	ps := NewMemoryPartialStore()
	r.queue.Backup.Partials = ps
	r.queue.Backup.Fetch.Policy.MaxAttempts = 1
	return r, o, ps
}

// Punto 3: los bytes de un intento cortado sobreviven al reintento de la cola
// (aquí, en OTRO proceso) y el siguiente intento pide solo lo que falta.
func TestMediaPartial_RetryInAnotherProcessResumesWithRange(t *testing.T) {
	r, o, ps := partialRig(t)
	ctx := context.Background()
	big := jpegN("parcial", 30000)
	o.set("/p/big.jpg", big, `"v1"`)
	o.cutFirst["/p/big.jpg"] = 9000
	u := o.URL("/p/big.jpg")
	enqueueURLs(t, r, "org-a", "images", u)

	if _, err := r.queue.ProcessOnce(ctx, 10); err != nil {
		t.Fatal(err)
	}
	p, ok, _ := ps.LoadPartial(ctx, "org-a", u)
	if !ok || len(p.Data) != 9000 || p.ETag != `"v1"` {
		t.Fatalf("lo parcial debe guardarse: ok=%v len=%d etag=%q", ok, len(p.Data), p.ETag)
	}
	if st, _ := r.queue.Stats(ctx, "org-a", ""); st.Pending != 1 {
		t.Fatalf("el trabajo queda para reintento: %+v", st)
	}
	if _, ok, _ := r.manifest.ByURL(ctx, "org-a", u); ok {
		t.Fatal("un parcial nunca entra al manifiesto")
	}

	r.clk.Advance(time.Minute)
	other := secondQueue(r, r.jobs) // otro proceso, misma persistencia
	if _, err := other.ProcessOnce(ctx, 10); err != nil {
		t.Fatal(err)
	}
	reqs := o.requests("/p/big.jpg")
	if len(reqs) != 2 || reqs[1].Range != "bytes=9000-" || reqs[1].Status != 206 {
		t.Fatalf("el reintento debe pedir solo lo que falta: %+v", reqs)
	}
	e, ok, _ := r.manifest.ByURL(ctx, "org-a", u)
	if !ok || e.SHA256 != sha(big) || !bytes.Equal(r.store.Objects[e.StoredName], big) {
		t.Fatalf("original completo e idéntico: %+v", e)
	}
	if _, ok, _ := ps.LoadPartial(ctx, "org-a", u); ok {
		t.Fatal("al terminar, lo parcial se borra")
	}
}

// Punto 3: si el origen cambió entre intentos, If-Range hace que mande el archivo
// completo nuevo; nunca se mezclan bytes de dos versiones.
func TestMediaPartial_ChangedOriginBetweenRetriesNeverMixesVersions(t *testing.T) {
	r, o, ps := partialRig(t)
	ctx := context.Background()
	v1, v2 := jpegN("uno", 20000), jpegN("dos", 21000)
	o.set("/p/x.jpg", v1, `"v1"`)
	o.cutFirst["/p/x.jpg"] = 5000
	u := o.URL("/p/x.jpg")
	enqueueURLs(t, r, "org-a", "images", u)
	_, _ = r.queue.ProcessOnce(ctx, 10)
	if _, ok, _ := ps.LoadPartial(ctx, "org-a", u); !ok {
		t.Fatal("parcial guardado")
	}

	o.set("/p/x.jpg", v2, `"v2"`)
	r.clk.Advance(time.Minute)
	_, _ = r.queue.ProcessOnce(ctx, 10)
	reqs := o.requests("/p/x.jpg")
	if len(reqs) != 2 || reqs[1].Range != "bytes=5000-" || reqs[1].Status != 200 {
		t.Fatalf("pidió continuar pero el origen mandó la versión nueva completa: %+v", reqs)
	}
	e, _, _ := r.manifest.ByURL(ctx, "org-a", u)
	if e.SHA256 != sha(v2) || !bytes.Equal(r.store.Objects[e.StoredName], v2) {
		t.Fatal("se guardó una mezcla de versiones")
	}
	if _, ok, _ := ps.LoadPartial(ctx, "org-a", u); ok {
		t.Fatal("lo parcial viejo se descarta")
	}
}

// Punto 3: un parcial sin validador fuerte no se puede continuar: se ignora.
func TestMediaPartial_WeakValidatorIsNeverResumed(t *testing.T) {
	o := newVOrigin(t)
	body := jpegN("debil", 4000)
	o.set("/w.jpg", body, `W/"w1"`)
	deps := &importer.TransformDeps{HTTPClient: o.Client()}
	f, err := importer.FetchMediaWith(deps, o.URL("/w.jpg"), importer.FetchOptions{Resume: &importer.Partial{Data: body[:1000], ETag: `W/"w1"`}})
	if err != nil || !bytes.Equal(f.Data, body) || f.ResumedBytes != 0 {
		t.Fatalf("err=%v resumed=%d", err, f.ResumedBytes)
	}
	if reqs := o.requests("/w.jpg"); len(reqs) != 1 || reqs[0].Range != "" {
		t.Fatalf("sin Range con validador débil: %+v", reqs)
	}
	if (importer.Partial{Data: []byte("x"), ETag: `W/"a"`}).Resumable() || !(importer.Partial{Data: []byte("x"), ETag: `"a"`}).Resumable() {
		t.Fatal("Resumable")
	}
}

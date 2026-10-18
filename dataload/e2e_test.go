package dataload

import (
	"context"
	"testing"
)

const e2eProfileJSON = `{
  "name": "catalogo-csv", "version": "1", "entity": "item", "key": ["external_id"],
  "clean": ["trim"],
  "ignoreHeaders": ["Meta: _wcct_*"],
  "fields": [
    {"target":"external_id","from":["ID"],"required":true},
    {"target":"name","from":["Nombre"],"required":true,"clean":["trim_quotes","collapse_spaces"]},
    {"target":"price","from":["Precio"],"type":"number"},
    {"target":"description","from":["Descripción"],"clean":["unescape_newlines","strip_html"]},
    {"target":"published","from":["Publicado"],"type":"boolean","values":{"-1":"0"}},
    {"target":"images","from":["Imágenes"],"media":true}
  ]
}`

// Recorrido completo: perfil JSON -> dry-run -> aplicar -> cola de medios con
// respaldo -> repetir (idempotente, sin descargar de nuevo) -> auditar el respaldo.
func TestEndToEnd_ProfileDryRunApplyBackupAndIdempotentRerun(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	prof, err := ParseProfile([]byte(e2eProfileJSON))
	if err != nil {
		t.Fatal(err)
	}
	body := csvRows(r.origin)
	rq := RunRequest{Org: "org-a", Actor: "u1", Profile: prof, Source: NewCSVSource("catalogo.csv", body)}

	plan, _, err := r.runner.DryRun(ctx, rq)
	if err != nil || plan.Counts[ActionCreate] != 3 || plan.IgnoredColumns != 3 {
		t.Fatalf("dry-run: %v %+v", err, plan.Counts)
	}
	if r.target.Len("org-a", "item") != 0 || r.origin.Hits("/img/a.jpg") != 0 {
		t.Fatal("el dry-run tocó el destino o la red")
	}

	run := startAndWait(t, r, rq)
	if run.State != RunSucceeded || run.PlanHash != plan.PlanHash {
		t.Fatalf("el apply debe ejecutar el plan mostrado: %+v", run)
	}
	r.drain(t, "org-a")

	for path, tag := range map[string]string{"/img/a.jpg": "a", "/img/b.jpg": "b", "/img/c.jpg": "c"} {
		e, ok, _ := r.manifest.ByURL(ctx, "org-a", r.origin.URL(path))
		if !ok || e.SHA256 != sha(jpegBytes(tag)) || e.Status != EntryOK {
			t.Fatalf("%s sin respaldo: %+v", path, e)
		}
	}
	if len(r.sink.Get("org-a", "item", "1", "images")) != 2 || len(r.sink.Get("org-a", "item", "2", "images")) != 1 {
		t.Fatal("las galerías no se ligaron")
	}
	if r.store.Puts != 3 {
		t.Fatalf("puts = %d", r.store.Puts)
	}

	// Segunda corrida con la misma entrada y también forzada: nada cambia, nada se descarga.
	rq.Force = true
	again := startAndWait(t, r, rq)
	r.drain(t, "org-a")
	if again.Progress.Created+again.Progress.Updated != 0 || again.Progress.Unchanged != 3 {
		t.Fatalf("again = %+v", again.Progress)
	}
	for _, p := range []string{"/img/a.jpg", "/img/b.jpg", "/img/c.jpg"} {
		if r.origin.Hits(p) != 1 {
			t.Errorf("%s se descargó %d veces", p, r.origin.Hits(p))
		}
	}
	if r.store.Puts != 3 {
		t.Fatalf("puts tras repetir = %d", r.store.Puts)
	}

	// El sitio de origen desaparece: el respaldo sigue íntegro y verificable.
	r.origin.Close()
	rep, err := r.queue.Backup.VerifyAll(ctx, "org-a")
	if err != nil || !rep.Healthy() || rep.OK != 3 {
		t.Fatalf("verificación: %v %+v", err, rep)
	}
}

// Reinicio del proceso: otro Runner y otra cola sobre la misma persistencia
// continúan sin duplicar ni perder trabajo.
func TestEndToEnd_NewProcessOverSamePersistenceResumes(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	body := csvRows(r.origin)
	rq := req(r, body)
	run := startAndWait(t, r, rq)
	_, _ = r.queue.ProcessOnce(ctx, 1) // el "proceso anterior" alcanzó a hacer un solo medio

	r2 := &Runner{Ledger: r.ledger, Target: r.target, Media: r.queue, Now: r.clk.Now}
	defer r2.Close()
	replay, err := r2.Start(ctx, rq)
	if err != nil || replay.ReplayOf != run.ID {
		t.Fatalf("el historial persistente debe resolver el replay: %v %+v", err, replay)
	}
	r.drain(t, "org-a")
	st, _ := r.queue.Stats(ctx, "org-a", "")
	if st.Done != 3 || st.Dead != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

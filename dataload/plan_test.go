package dataload

import (
	"bytes"
	"context"
	"io"
	"testing"
)

func planReq(r *rig, body []byte, ov *Overrides) PlanRequest {
	return PlanRequest{Org: "org-a", Profile: catalogProfile(), Overrides: ov, Source: NewCSVSource("catalogo.csv", body), Target: r.target}
}

func TestPlan_DryRunWritesNothingAndDownloadsNothing(t *testing.T) {
	r := newRig(t)
	p := mustPlan(t, planReq(r, csvRows(r.origin), nil))
	if p.Counts[ActionCreate] != 3 || r.target.Batches() != 0 || r.target.Len("org-a", "item") != 0 {
		t.Fatalf("el dry-run escribió: %+v applies=%d", p.Counts, r.target.Batches())
	}
	if r.origin.Hits("/img/a.jpg") != 0 || r.store.Puts != 0 {
		t.Fatal("el dry-run descargó medios")
	}
	if p.MediaURLs != 3 {
		t.Fatalf("debía contar 3 URLs de medios, got %d", p.MediaURLs)
	}
}

func TestPlan_AppliesCleaningAndTypes(t *testing.T) {
	r := newRig(t)
	p := mustPlan(t, planReq(r, csvRows(r.origin), nil))
	first := p.Rows[0].Record
	if first["name"] != "WHEY PROTEIN" {
		t.Errorf("name = %q", first["name"])
	}
	if first["price"] != 549.5 {
		t.Errorf("price = %v", first["price"])
	}
	if first["description"] != "Línea uno\nLínea dos" {
		t.Errorf("description = %q", first["description"])
	}
	if p.Rows[1].Record["published"] != false {
		t.Errorf("published (-1 -> 0) = %v", p.Rows[1].Record["published"])
	}
	if _, ok := first["images"]; ok {
		t.Error("las URLs de medios no deben ir en el registro")
	}
	if len(p.Rows[0].Media["images"]) != 2 {
		t.Errorf("media = %v", p.Rows[0].Media)
	}
}

func TestPlan_CountsIgnoredHeaders(t *testing.T) {
	r := newRig(t)
	p := mustPlan(t, planReq(r, csvRows(r.origin), &Overrides{IgnoreHeaders: []string{"Meta: _wcct_*"}}))
	if p.IgnoredColumns != 3 {
		t.Fatalf("IgnoredColumns = %d, quería 3 (una por fila)", p.IgnoredColumns)
	}
}

func TestPlan_InvalidAndDuplicateRowsAreReportedNotWritten(t *testing.T) {
	r := newRig(t)
	body := csvRows(r.origin, "1,REPETIDO,10,,1,,x", ",SIN ID,10,,1,,x", "9,,10,,1,,x")
	p := mustPlan(t, planReq(r, body, &Overrides{Duplicates: DuplicatesFirst}))
	if p.Counts[ActionDuplicate] != 1 || p.Counts[ActionInvalid] != 2 || p.Counts[ActionCreate] != 3 {
		t.Fatalf("counts = %v", p.Counts)
	}
	if p.Rows[3].Action != ActionDuplicate || p.Rows[3].Issues[0].Row != 4 || p.Rows[3].Issues[0].Code != string(CodeRowDuplicate) {
		t.Fatalf("duplicado mal reportado: %+v", p.Rows[3])
	}
}

// Por omisión (decisión del dueño, reversible con duplicates: "first"): si una
// identidad se repite, NINGUNA de sus filas se carga sola; la primera va a
// revisión con las demás como referencia y las demás quedan duplicate. Mismo
// resultado en memoria y en streaming.
func TestPlan_DuplicateKeyHoldsAllRowsAndSendsFirstToReview(t *testing.T) {
	r := newRig(t)
	body := csvRows(r.origin, "1,BUJE TRASERO,310,,1,,x", "2,OTRO,1,,1,,x", "1,TERCERA,5,,1,,x")
	check := func(name string, rows []RowPlan, counts map[Action]int) {
		t.Helper()
		if counts[ActionReview] != 2 || counts[ActionDuplicate] != 3 || counts[ActionCreate] != 1 {
			t.Fatalf("%s: counts = %v", name, counts)
		}
		first := rows[0]
		if first.Action != ActionReview || first.Outcome != ActionCreate || len(first.Review) != 1 ||
			first.Review[0].Code != ReviewCodeDuplicateKey || first.Review[0].DetailParams["rows"] != "4, 6" {
			t.Fatalf("%s: primera fila = %+v", name, first)
		}
		if rows[3].Action != ActionDuplicate || rows[3].Issues[0].Code != string(CodeRowDuplicateHeld) {
			t.Fatalf("%s: repetida = %+v", name, rows[3])
		}
		for _, c := range (&Plan{Rows: rows}).Changes() {
			if c.Key == "1" || c.Key == "2" {
				t.Fatalf("%s: una identidad repetida no debe escribirse: %+v", name, c)
			}
		}
	}
	p := mustPlan(t, planReq(r, body, nil))
	check("memoria", p.Rows, p.Counts)
	var streamed []RowPlan
	sp, err := StreamPlan(context.Background(), PlanRequest{Org: "org-a", Profile: catalogProfile(), Target: r.target,
		Source: NewCSVStreamSource("c", func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil })},
		StreamOptions{OnPage: func(_ context.Context, page []RowPlan) error { streamed = append(streamed, page...); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	check("streaming", streamed, sp.Counts)
	if sp.PlanHash != p.PlanHash {
		t.Fatal("la huella del plan debe ser la misma en memoria y en streaming")
	}
	bad := catalogProfile()
	bad.Duplicates = "ultima"
	if err := bad.Validate(); err == nil {
		t.Fatal("una política desconocida debe rechazarse")
	}
}

func TestPlan_UpdateShowsFieldDiffAndUnchangedIsDetected(t *testing.T) {
	r := newRig(t)
	body := csvRows(r.origin)
	run, _ := r.runner.Start(context.Background(), RunRequest{Org: "org-a", Profile: catalogProfile(), Source: NewCSVSource("c", body)})
	if _, err := r.runner.Wait(context.Background(), "org-a", run.ID); err != nil {
		t.Fatal(err)
	}
	changed := csvRows(r.origin)
	changed = []byte(string(changed[:0]) + replaceOnce(string(body), "549.50", "599"))
	p := mustPlan(t, planReq(r, changed, nil))
	if p.Counts[ActionUpdate] != 1 || p.Counts[ActionUnchanged] != 2 {
		t.Fatalf("counts = %v", p.Counts)
	}
	d := p.Rows[0].Diffs
	if len(d) != 1 || d[0].Field != "price" || d[0].Old != 549.5 || d[0].New != 599.0 {
		t.Fatalf("diff = %+v", d)
	}
}

func TestPlan_DiffWithoutStoredHashComparesFields(t *testing.T) {
	r := newRig(t)
	_ = r.target.Apply(context.Background(), "org-a", "item", []Change{{Key: "3", Record: map[string]any{"external_id": "3", "name": "PREWORKOUT", "price": 399.0, "published": true}}})
	p := mustPlan(t, planReq(r, csvRows(r.origin), nil))
	if p.Rows[2].Action != ActionUnchanged {
		// published true en el destino, la fila 3 trae 1 -> true; sin hash se compara por campos
		t.Fatalf("fila 3 = %+v", p.Rows[2])
	}
}

func TestPlan_MediaChangeIsAnUpdate(t *testing.T) {
	r := newRig(t)
	body := csvRows(r.origin)
	run, _ := r.runner.Start(context.Background(), RunRequest{Org: "org-a", Profile: catalogProfile(), Source: NewCSVSource("c", body)})
	_, _ = r.runner.Wait(context.Background(), "org-a", run.ID)
	next := replaceOnce(string(body), "/img/c.jpg", "/img/c2.jpg")
	p := mustPlan(t, planReq(r, []byte(next), nil))
	if p.Counts[ActionUpdate] != 1 || p.Rows[1].Action != ActionUpdate {
		t.Fatalf("counts = %v", p.Counts)
	}
}

func TestPlan_TooManyRows(t *testing.T) {
	r := newRig(t)
	req := planReq(r, csvRows(r.origin), nil)
	req.Profile.MaxRows = 2
	if _, err := BuildPlan(context.Background(), req); err == nil {
		t.Fatal("debía rechazar por exceso de filas")
	}
}

func TestPlan_HashIsDeterministic(t *testing.T) {
	r := newRig(t)
	a := mustPlan(t, planReq(r, csvRows(r.origin), nil))
	b := mustPlan(t, planReq(r, csvRows(r.origin), nil))
	if a.PlanHash == "" || a.PlanHash != b.PlanHash || a.InputHash != b.InputHash {
		t.Fatal("el plan debe ser determinista")
	}
}

func replaceOnce(s, old, new string) string {
	i := indexOf(s, old)
	if i < 0 {
		panic("no está: " + old)
	}
	return s[:i] + new + s[i+len(old):]
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

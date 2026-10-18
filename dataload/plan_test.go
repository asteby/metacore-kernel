package dataload

import (
	"context"
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
	p := mustPlan(t, planReq(r, body, nil))
	if p.Counts[ActionDuplicate] != 1 || p.Counts[ActionInvalid] != 2 || p.Counts[ActionCreate] != 3 {
		t.Fatalf("counts = %v", p.Counts)
	}
	if p.Rows[3].Action != ActionDuplicate || p.Rows[3].Issues[0].Row != 4 {
		t.Fatalf("duplicado mal reportado: %+v", p.Rows[3])
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

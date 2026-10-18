package dataload

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Recorrido estilo "lista de proveedor": un CSV con números de formato mixto, descripciones de
// las que hay que extraer datos y filas que se mandan a revisar.

type listaRow struct{ sku, cat, desc, cost, cdmx, edomex, unit, iva, pub string }

func listaCSV(rows ...listaRow) []byte {
	var b strings.Builder
	b.WriteString("Articulo,Linea/MARCA,Categoria,Descripcion,U.Med.,Precio,CDMX,EDO MEX,Unitario,IVA,PRECIO PÚBLICO\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "%s,Proveedor,%s,%q,st,%q,%q,%q,%q,%q,%q\n", r.sku, r.cat, r.desc, r.cost, r.cdmx, r.edomex, r.unit, r.iva, r.pub)
	}
	return []byte(b.String())
}

func listaRows() []listaRow {
	return []listaRow{
		{"P1", "FRENOS", "BALATA DISCO DELANTERO CERAMICA NISSAN ZETA 1.6 L4 2010 2012", "$500,00", "3", "1", "913,21", "146,11", "$1.059,32"},
		{"P2", "SUSPENSION Y DIRECCION", "AMORTIGUADOR TRASERO HONDA CIVIC 1.8 L4 2012 2014", "400.00", "10", "0", "913.21", "146.11", "1,059.32"},
		{"P3", "AFINACION", "FILTRO ACEITE UNIVERSAL", "50,5", "7", "2", "86,12", "13,78", "99,9"},
		{"P4", "QUIMICOS", "ACEITE MOTOR 10W40 SINTETICO", "100", "9", "9", "", "", "148"},
		{"P5", "SUSPENSION Y DIRECCION", "AMORTIGUADOR TRASERO MAZDA 3 - - 2008 2013", "100", "1", "1", "", "", "300"},
		{"P6", "FRENOS", "GATO HIDRAULICO 2 TON", "100", "1", "1", "", "", "300"},
		{"P7", "FRENOS", "BUJE DELANTERO NISSAN ZETA 1.6 L4 2010 2012", "100", "#N/D", "1", "", "", "300"},
		{"P8", "FRENOS", "BUJE DELANTERO NISSAN ZETA 1.6 L4 2010 2012", "100", "1", "1", "", "", "1.059"},
		{"D1", "FRENOS", "BUJE DELANTERO NISSAN ZETA 1.6 L4 2010 2012", "100", "1", "1", "", "", "300"},
		{"D1", "FRENOS", "BUJE TRASERO NISSAN ZETA 1.6 L4 2010 2012", "100", "1", "1", "", "", "310"},
		{"P10", "SUSPENSION Y DIRECCION", "BIELETA DELANTERA FORD RANGER EXCEPTO 4X4 2.3 L4 2005 2010", "100", "1", "1", "", "", "300"},
	}
}

func listaRequest(t *testing.T, rows ...listaRow) RunRequest {
	t.Helper()
	prof, ex := loadLista(t)
	if len(rows) == 0 {
		rows = listaRows()
	}
	return RunRequest{Org: "org-a", Actor: "u1", Profile: prof, Extract: ex, Source: NewCSVSource("lista.csv", listaCSV(rows...))}
}

func rowOf(p *Plan, key string) RowPlan {
	for _, r := range p.Rows {
		if r.Key == key {
			return r
		}
	}
	return RowPlan{}
}

func TestProtekStyle_MixedNumbersExtractionAndReviewRowsInThePlan(t *testing.T) {
	r := newRig(t)
	req := listaRequest(t)
	plan, run, err := r.runner.DryRun(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	want := map[Action]int{ActionCreate: 4, ActionReview: 5, ActionInvalid: 1, ActionDuplicate: 1}
	for a, n := range want {
		if plan.Counts[a] != n {
			t.Errorf("%s = %d, quería %d (todo: %v)", a, plan.Counts[a], n, plan.Counts)
		}
	}
	// Números mixtos: los tres formatos dan el mismo precio.
	for _, k := range []string{"P1", "P2"} {
		if p := rowOf(plan, k).Record["price"]; p != 1059.32 {
			t.Errorf("%s price = %v", k, p)
		}
	}
	if rowOf(plan, "P3").Record["price"] != 99.9 || rowOf(plan, "P4").Record["price"] != 148.0 {
		t.Error("99,9 y 148 mal leídos")
	}
	// Extracción: tabla de compatibilidad y etiquetas.
	p1 := rowOf(plan, "P1")
	if a := p1.Record["attributes"].(map[string]any); a["compatibilidad"] != "Nissan|ZETA|2010-2012|1.6 L4|" || a["pieza"] != "BALATA DISCO" {
		t.Errorf("P1 attrs = %v", a)
	}
	if len(p1.Record["tags"].([]any)) != 3 || p1.Action != ActionCreate {
		t.Errorf("P1 = %+v", p1)
	}
	// Ambiguo => error de fila, nunca un valor adivinado.
	p8 := plan.Rows[7] // las filas inválidas no tienen identidad: se ubican por número de fila
	if p8.Action != ActionInvalid || p8.Record != nil || !strings.Contains(p8.Issues[0].Message, "numero_ambiguo") {
		t.Errorf("P8 = %+v", p8)
	}
	// Filas revisar: fuera de la carga, con su motivo y lo que harían.
	reasons := map[string]string{"P5": "motor_desconocido", "P6": "vehiculo_no_reconocido", "P7": "stock_nd", "D1": "sku_duplicado", "P10": "modelo_sospechoso"}
	for k, code := range reasons {
		row := rowOf(plan, k)
		if row.Action != ActionReview || row.Outcome != ActionCreate {
			t.Errorf("%s = %s/%s", k, row.Action, row.Outcome)
			continue
		}
		found := false
		for _, it := range row.Review {
			found = found || (it.Code == code && it.Severity == SeverityRevisar && it.Detail != "")
		}
		if !found {
			t.Errorf("%s sin motivo %s: %+v", k, code, row.Review)
		}
	}
	for _, c := range plan.Changes() {
		if contains([]string{"P5", "P6", "P7", "P10", "D1"}, c.Key) {
			t.Errorf("%s no debe estar en la carga automática", c.Key)
		}
	}
	if len(plan.Changes()) != 4 || len(plan.Reviews()) != 5 {
		t.Errorf("changes=%d reviews=%d", len(plan.Changes()), len(plan.Reviews()))
	}
	// El historial de la simulación ya lista las filas con motivo.
	if run.Progress.Review != 5 || len(run.Review) != 5 || run.ExtractHash == "" {
		t.Errorf("run = %+v", run.Progress)
	}
}

func TestInfoOnlyRowsLoadNormallyWithTheirNotes(t *testing.T) {
	r := newRig(t)
	plan, _, err := r.runner.DryRun(context.Background(), listaRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"P3", "P4"} {
		row := rowOf(plan, k)
		if row.Action != ActionCreate || len(row.Review) == 0 || row.Blocking() {
			t.Errorf("%s = %s %+v", k, row.Action, row.Review)
		}
	}
}

func TestRunner_ApplyLoadsOnlyCleanRowsAndReportsReviewRowsInTheLedger(t *testing.T) {
	r := newRig(t)
	r.runner.Reviews = NewMemoryReviewStore()
	run := startAndWait(t, r, listaRequest(t))
	if run.State != RunSucceeded || run.Progress.Created != 4 || run.Progress.Review != 5 || run.Progress.Invalid != 1 || run.Progress.Duplicates != 1 {
		t.Fatalf("run = %+v", run.Progress)
	}
	if r.target.Len("org-a", "product") != 4 {
		t.Fatalf("el destino debe tener solo las 4 filas limpias, tiene %d", r.target.Len("org-a", "product"))
	}
	for _, k := range []string{"P5", "P6", "P7", "D1", "P10"} {
		if _, ok := r.target.Get("org-a", "product", k); ok {
			t.Errorf("%s se escribió sin decisión", k)
		}
	}
	got, _ := r.ledger.Get(context.Background(), "org-a", run.ID)
	if len(got.Review) != 5 || got.Review[0].Items[0].Detail == "" {
		t.Fatalf("el historial debe traer cada fila con su motivo: %+v", got.Review)
	}
	pend, _ := r.runner.Reviews.List(context.Background(), "org-a", ReviewFilter{State: ReviewPending})
	if len(pend) != 5 || pend[0].Record == nil {
		t.Fatalf("pendientes = %d", len(pend))
	}
	byCode, _ := r.runner.Reviews.List(context.Background(), "org-a", ReviewFilter{Code: "motor_desconocido"})
	if len(byCode) != 1 || byCode[0].Key != "P5" {
		t.Fatalf("filtro por código: %+v", byCode)
	}
}

func TestReview_ApprovedRowsAreAppliedLaterByExplicitDecisionAndRejectedStayOut(t *testing.T) {
	r := newRig(t)
	r.runner.Reviews = NewMemoryReviewStore()
	ctx := context.Background()
	startAndWait(t, r, listaRequest(t))

	p5, _, _ := r.runner.Reviews.Get(ctx, "org-a", "product", "P5")
	p6, _, _ := r.runner.Reviews.Get(ctx, "org-a", "product", "P6")
	if _, err := r.runner.Decide(ctx, "org-a", "", nil); err == nil {
		t.Fatal("una decisión sin responsable no vale")
	}
	if _, err := r.runner.Decide(ctx, "org-a", "ana", []ReviewDecision{
		{Entity: "product", Key: "P5", Hash: p5.Hash, Approve: true, Note: "el proveedor confirmó la cilindrada por teléfono"},
		{Entity: "product", Key: "P6", Hash: p6.Hash, Approve: false, Note: "no es refacción"},
	}); err != nil {
		t.Fatal(err)
	}
	// Decidir no escribe nada.
	if _, ok := r.target.Get("org-a", "product", "P5"); ok {
		t.Fatal("Decide no debe escribir en el destino")
	}
	run, err := r.runner.ApplyApproved(ctx, "org-a", "ana", "product")
	if err != nil || run.State != RunSucceeded || run.Mode != ModeReview || run.Progress.Created != 1 || run.Actor != "ana" {
		t.Fatalf("%v %+v", err, run)
	}
	got, ok := r.target.Get("org-a", "product", "P5")
	if !ok || got.Record["price"] != 300.0 || got.Hash != p5.Hash {
		t.Fatalf("se debe escribir el registro exacto que se revisó: %+v", got)
	}
	if _, ok := r.target.Get("org-a", "product", "P6"); ok {
		t.Fatal("lo rechazado no se escribe")
	}
	e5, _, _ := r.runner.Reviews.Get(ctx, "org-a", "product", "P5")
	e6, _, _ := r.runner.Reviews.Get(ctx, "org-a", "product", "P6")
	if e5.State != ReviewApplied || e5.DecidedBy != "ana" || e5.AppliedRun != run.ID || e6.State != ReviewRejected {
		t.Fatalf("estados: %s %s", e5.State, e6.State)
	}
	// Reaplicar no repite nada.
	again, err := r.runner.ApplyApproved(ctx, "org-a", "ana", "product")
	if err != nil || again.Progress.Total != 0 {
		t.Fatalf("%v %+v", err, again.Progress)
	}
	// Una corrida posterior con el mismo archivo ya ve P5 como unchanged y no la vuelve a pedir.
	req := listaRequest(t)
	req.Force = true
	second := startAndWait(t, r, req)
	if second.Progress.Review != 4 || second.Progress.Unchanged != 5 {
		t.Fatalf("segunda corrida: %+v", second.Progress)
	}
	if !errors.Is(func() error {
		_, err := r.runner.Decide(ctx, "org-a", "ana", []ReviewDecision{{Entity: "product", Key: "P5", Hash: p5.Hash, Approve: true}})
		return err
	}(), ErrReviewClosed) {
		t.Fatal("una fila ya aplicada no admite otra decisión")
	}
}

func TestReview_DecisionIsBoundToTheRowVersionAndSupersededWhenTheSourceIsFixed(t *testing.T) {
	r := newRig(t)
	r.runner.Reviews = NewMemoryReviewStore()
	ctx := context.Background()
	startAndWait(t, r, listaRequest(t))
	old, _, _ := r.runner.Reviews.Get(ctx, "org-a", "product", "P7")

	// El proveedor corrige P7 y P6 en una lista nueva.
	rows := listaRows()
	for i := range rows {
		switch rows[i].sku {
		case "P7":
			rows[i].cdmx, rows[i].pub = "#N/D", "310" // sigue dudosa, pero el registro cambió
		case "P6":
			rows[i].desc = "BUJE TRASERO TOYOTA YARIS 1.5 L4 2007 2012"
		}
	}
	startAndWait(t, r, listaRequest(t, rows...))

	if _, err := r.runner.Decide(ctx, "org-a", "ana", []ReviewDecision{{Entity: "product", Key: "P7", Hash: old.Hash, Approve: true}}); !errors.Is(err, ErrReviewStale) {
		t.Fatalf("la decisión sobre la versión vieja debe rechazarse: %v", err)
	}
	cur, _, _ := r.runner.Reviews.Get(ctx, "org-a", "product", "P7")
	if cur.Hash == old.Hash || cur.State != ReviewPending {
		t.Fatalf("la fila cambiada vuelve a pendiente: %+v", cur)
	}
	if _, err := r.runner.Decide(ctx, "org-a", "ana", []ReviewDecision{{Entity: "product", Key: "P7", Hash: cur.Hash, Approve: true}}); err != nil {
		t.Fatal(err)
	}
	p6, _, _ := r.runner.Reviews.Get(ctx, "org-a", "product", "P6")
	if p6.State != ReviewSuperseded {
		t.Fatalf("P6 se resolvió sola al corregir la fuente: %s", p6.State)
	}
	if _, ok := r.target.Get("org-a", "product", "P6"); !ok {
		t.Fatal("P6 corregida se carga en automático")
	}
	if _, err := r.runner.Decide(ctx, "org-a", "ana", []ReviewDecision{{Entity: "product", Key: "NOPE", Hash: "x", Approve: true}}); !errors.Is(err, ErrReviewNotFound) {
		t.Fatalf("%v", err)
	}
}

func TestReview_DecideIsAllOrNothing(t *testing.T) {
	r := newRig(t)
	r.runner.Reviews = NewMemoryReviewStore()
	ctx := context.Background()
	startAndWait(t, r, listaRequest(t))
	p5, _, _ := r.runner.Reviews.Get(ctx, "org-a", "product", "P5")
	_, err := r.runner.Decide(ctx, "org-a", "ana", []ReviewDecision{
		{Entity: "product", Key: "P5", Hash: p5.Hash, Approve: true},
		{Entity: "product", Key: "P6", Hash: "otra-huella", Approve: true},
	})
	if !errors.Is(err, ErrReviewStale) {
		t.Fatal(err)
	}
	after, _, _ := r.runner.Reviews.Get(ctx, "org-a", "product", "P5")
	if after.State != ReviewPending {
		t.Fatalf("no se registra ninguna decisión: %s", after.State)
	}
}

func TestPlan_ExtractIsPartOfTheRunKey(t *testing.T) {
	r := newRig(t)
	req := listaRequest(t)
	first := startAndWait(t, r, req)
	// Mismo archivo y perfil, otro documento de extracción => otra corrida (no es replay).
	_, ex2 := loadLista(t)
	ex2.Doc.Rules[0].Consume = !ex2.Doc.Rules[0].Consume
	ex3, err := NewExtractor(ex2.Doc)
	if err != nil {
		t.Fatal(err)
	}
	req.Extract = ex3
	second := startAndWait(t, r, req)
	if second.ReplayOf != "" || second.Key == first.Key || second.ExtractHash == first.ExtractHash {
		t.Fatalf("el documento de extracción debe entrar en la llave: %+v", second)
	}
	// Sin extracción la llave es la de siempre.
	req.Extract = nil
	if got := fingerprint("h", nil); got != "h" {
		t.Fatal(got)
	}
}

func TestPlan_LookupIsChunkedForLargeBatches(t *testing.T) {
	prof, ex := loadLista(t)
	var rows []map[string]any
	for i := 0; i < 25; i++ {
		rows = append(rows, map[string]any{"Articulo": fmt.Sprintf("K%02d", i), "Linea/MARCA": "x", "Descripcion": "BUJE DELANTERO NISSAN ZETA 1.6 L4 2010 2012", "Categoria": "FRENOS", "PRECIO PÚBLICO": "10"})
	}
	ct := &countingTarget{MemoryTarget: NewMemoryTarget()}
	plan, err := BuildPlan(context.Background(), PlanRequest{Org: "o", Profile: prof, Extract: ex, Source: NewRowsSource("x", rows), Target: ct, LookupChunk: 10})
	if err != nil || plan.Counts[ActionCreate] != 25 {
		t.Fatalf("%v %v", err, plan.Counts)
	}
	if len(ct.sizes) != 3 || ct.sizes[0] != 10 || ct.sizes[2] != 5 {
		t.Fatalf("consultas = %v", ct.sizes)
	}
}

type countingTarget struct {
	*MemoryTarget
	sizes []int
}

func (c *countingTarget) Lookup(ctx context.Context, org, entity string, keys []string) (map[string]Existing, error) {
	c.sizes = append(c.sizes, len(keys))
	return c.MemoryTarget.Lookup(ctx, org, entity, keys)
}

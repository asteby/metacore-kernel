package dataload

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/asteby/metacore-kernel/importer"
)

// Ejecución de una corrida con una StreamSource: el archivo no se carga a
// memoria. La primera pasada (scan) detecta duplicados y valores repetidos y da
// la huella; la segunda planifica por páginas y escribe cada página en lotes con
// checkpoint, igual que el camino en memoria. Si el perfil declara ApplyOrder,
// la segunda pasada solo planifica y la escritura se hace en una pasada por cada
// rango de orden (padres antes que variantes), así el orden global se respeta sin
// tener las filas en memoria.

// pageSize de las corridas en streaming (0 = DefaultPageSize), redondeado hacia
// arriba a un múltiplo de BatchSize para que los lotes salgan completos.
func (r *Runner) pageSize() int {
	p := DefaultPageSize
	if r.PageSize > 0 {
		p = r.PageSize
	}
	b := r.batch()
	return (p + b - 1) / b * b
}

func (r *Runner) executeStream(ctx context.Context, req RunRequest, src StreamSource, run *Run, base Progress, marker MarkingTarget, save func() bool) error {
	sp, err := newStreamPlanner(PlanRequest{Org: req.Org, Profile: req.Profile, Overrides: req.Overrides, Source: src, Target: r.Target, Extract: req.Extract})
	if err != nil {
		return err
	}
	if err := sp.scan(ctx, r.pageSize()); err != nil {
		return err
	}
	// La llave y la huella de la corrida se calcularon con otra lectura: si el
	// archivo cambió desde entonces no se aplica nada.
	if sp.summary.InputHash != run.InputHash {
		return newErr(CodeSourceChanged, "source", src.Name())
	}
	if req.FailOnInvalid && (sp.summary.Invalid > 0 || sp.summary.Duplicates > 0) {
		return newErr(CodeRunInvalidRows, "invalid", sp.summary.Invalid, "duplicates", sp.summary.Duplicates)
	}
	entity := sp.prof.Entity
	w, err := r.newBatchWriter(ctx, req.Org, entity, run, &base, marker, save)
	if err != nil {
		return err
	}

	// planned son los conteos del plan, acumulados página por página.
	planned := map[Action]int{}
	refresh := func() {
		p := &run.Progress
		p.Total = planned[ActionCreate] + planned[ActionUpdate] + base.Done
		p.Unchanged = max(0, planned[ActionUnchanged]-base.Done)
		p.Invalid, p.Duplicates, p.Review = planned[ActionInvalid], planned[ActionDuplicate], planned[ActionReview]
	}
	run.Issues, run.Review = nil, nil
	run.Progress = Progress{Done: base.Done, Created: base.Created, Updated: base.Updated}
	if !save() {
		return errStopped
	}

	ordered := len(sp.prof.ApplyOrder) > 0
	tiers := map[string][]int{}
	inReview := map[string]bool{}
	tmp := &Plan{applyOrder: sp.prof.ApplyOrder}
	onPage := func(ctx context.Context, page []RowPlan) error {
		for _, rp := range page {
			planned[rp.Action]++
			for _, is := range rp.Issues {
				if len(run.Issues) < MaxRunIssues {
					run.Issues = append(run.Issues, is)
				}
			}
			if rp.Action == ActionReview {
				inReview[rp.Key] = true
				if len(run.Review) < MaxRunIssues {
					run.Review = append(run.Review, ReviewNote{Row: rp.Row, Key: rp.Key, Outcome: rp.Outcome, Items: rp.Review})
				}
			}
			if ordered && (rp.Action == ActionCreate || rp.Action == ActionUpdate) {
				rank := tmp.rank(Change{Record: rp.Record})
				tiers[rankKey(rank)] = rank
			}
		}
		if err := r.holdReviewRows(ctx, *run, req.Org, entity, page); err != nil {
			return err
		}
		refresh()
		var only func(RowPlan) bool
		if !ordered {
			if err := w.write(pageChanges(page)); err != nil {
				return err
			}
		} else {
			// Lo que se escribirá en las pasadas por orden encola sus medios ahí,
			// después de existir en el destino.
			only = func(rp RowPlan) bool { return rp.Action == ActionUnchanged }
		}
		n, err := r.enqueueMedia(ctx, req.Org, entity, run.ID, page, only)
		if err != nil {
			return err
		}
		run.Progress.MediaQueued += n
		if !save() {
			return errStopped
		}
		return nil
	}
	plan, err := sp.plan(ctx, nil, onPage)
	if err != nil {
		return err
	}
	run.PlanHash = plan.PlanHash

	if ordered {
		keys := make([]string, 0, len(tiers))
		for k := range tiers {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(a, b int) bool { return lessRank(tiers[keys[a]], tiers[keys[b]]) })
		for _, k := range keys {
			want := tiers[k]
			only := func(rp *RowPlan) bool {
				return rp.Action == "" && rankKey(tmp.rank(Change{Record: rp.Record})) == rankKey(want)
			}
			write := func(ctx context.Context, page []RowPlan) error {
				changes := pageChanges(page)
				if err := w.write(changes); err != nil {
					return err
				}
				n, err := r.enqueueMedia(ctx, req.Org, entity, run.ID, page, func(rp RowPlan) bool { return rp.Action == ActionCreate || rp.Action == ActionUpdate })
				if err != nil {
					return err
				}
				run.Progress.MediaQueued += n
				return nil
			}
			if _, err := sp.plan(ctx, only, write); err != nil {
				return err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return contextError(ctx, err)
	}
	// Pendientes de revisión que esta corrida ya resolvió: la identidad vino en
	// una fila válida (está en la primera pasada) y no volvió a pedir revisión.
	return r.supersedeReviews(ctx, req.Org, entity, func(key string) bool {
		_, ok := sp.first[key]
		return ok && !inReview[key]
	})
}

// pageChanges son las escrituras de una página, en orden de fila.
func pageChanges(page []RowPlan) []Change {
	var out []Change
	for _, rp := range page {
		if rp.Action == ActionCreate || rp.Action == ActionUpdate {
			out = append(out, Change{Key: rp.Key, Action: rp.Action, Record: rp.Record, Hash: rp.Hash})
		}
	}
	return out
}

func rankKey(rank []int) string {
	parts := make([]string, len(rank))
	for i, n := range rank {
		parts[i] = fmt.Sprint(n)
	}
	return strings.Join(parts, ",")
}

func lessRank(a, b []int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// dryRunStream es DryRun con una StreamSource: el plan no trae Rows (para el diff
// por fila está StreamPlan con OnPage); los problemas y motivos sí se guardan en
// la corrida, hasta MaxRunIssues.
func (r *Runner) dryRunStream(ctx context.Context, req RunRequest, run *Run) (*Plan, error) {
	var issues []importer.RowIssue
	var notes []ReviewNote
	plan, err := StreamPlan(ctx, PlanRequest{Org: req.Org, Profile: req.Profile, Overrides: req.Overrides, Source: req.Source, Target: r.Target, Extract: req.Extract},
		StreamOptions{PageSize: r.pageSize(), OnPage: func(_ context.Context, page []RowPlan) error {
			for _, rp := range page {
				for _, is := range rp.Issues {
					if len(issues) < MaxRunIssues {
						issues = append(issues, is)
					}
				}
				if rp.Action == ActionReview && len(notes) < MaxRunIssues {
					notes = append(notes, ReviewNote{Row: rp.Row, Key: rp.Key, Outcome: rp.Outcome, Items: rp.Review})
				}
			}
			return nil
		}})
	if err != nil {
		return nil, err
	}
	run.Issues, run.Review = issues, notes
	return plan, nil
}

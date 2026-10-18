package dataload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/asteby/metacore-kernel/importer"
)

// Action es lo que el plan hará con una fila.
type Action string

const (
	ActionCreate    Action = "create"
	ActionUpdate    Action = "update"
	ActionUnchanged Action = "unchanged"
	ActionInvalid   Action = "invalid"   // la fila no pasó validación; no se escribe
	ActionDuplicate Action = "duplicate" // misma identidad que una fila anterior del archivo; gana la primera
)

// Existing es lo que el destino ya tiene para una identidad.
type Existing struct {
	// Record son los campos actuales, con las mismas rutas que el perfil.
	Record map[string]any
	// Hash es la huella que el destino guardó la última vez que aplicó una
	// Change (opcional). Si coincide con la nueva, la fila es unchanged sin
	// comparar campo por campo.
	Hash string
}

// Change es una escritura que el destino debe aplicar de forma idempotente
// (upsert por Key).
type Change struct {
	Key    string
	Action Action // create o update
	Record map[string]any
	Hash   string
}

// Target es la persistencia del destino; la implementan ops/addons.
type Target interface {
	// Lookup devuelve lo existente para las identidades pedidas (las ausentes no
	// aparecen en el mapa). No debe escribir.
	Lookup(ctx context.Context, org, entity string, keys []string) (map[string]Existing, error)
	// Apply hace upsert de un lote. Debe ser idempotente por Key y atómico por
	// lote; un error deja el lote sin aplicar.
	Apply(ctx context.Context, org, entity string, changes []Change) error
}

// FieldDiff es un campo que cambia en una actualización.
type FieldDiff struct {
	Field string `json:"field"`
	Old   any    `json:"old"`
	New   any    `json:"new"`
}

// RowPlan es el plan de una fila de la fuente.
type RowPlan struct {
	Row    int                 `json:"row"` // 1-based sobre las filas de datos
	Key    string              `json:"key,omitempty"`
	Action Action              `json:"action"`
	Hash   string              `json:"hash,omitempty"`
	Record map[string]any      `json:"record,omitempty"`
	Media  map[string][]string `json:"media,omitempty"` // campo -> URLs por respaldar
	Diffs  []FieldDiff         `json:"diffs,omitempty"`
	Issues []importer.RowIssue `json:"issues,omitempty"`
}

// Plan es el resultado del dry-run: qué pasaría, sin escribir nada.
type Plan struct {
	Org            string         `json:"org"`
	Entity         string         `json:"entity"`
	Profile        string         `json:"profile"`
	ProfileVersion string         `json:"profileVersion"`
	ProfileHash    string         `json:"profileHash"`
	Source         string         `json:"source"`
	InputHash      string         `json:"inputHash"`
	PlanHash       string         `json:"planHash"`
	Rows           []RowPlan      `json:"rows"`
	Counts         map[Action]int `json:"counts"`
	IgnoredColumns int            `json:"ignoredColumns"`
	MediaURLs      int            `json:"mediaUrls"`

	applyOrder []OrderRule
}

// PlanRequest reúne lo necesario para planificar.
type PlanRequest struct {
	Org       string
	Profile   Profile
	Overrides *Overrides
	Source    Source
	Target    Target
}

// BuildPlan lee la fuente, aplica el perfil (con los overrides de la org),
// valida y compara contra el destino. Nunca escribe y nunca descarga medios.
func BuildPlan(ctx context.Context, req PlanRequest) (*Plan, error) {
	if req.Org == "" {
		return nil, fmt.Errorf("dataload: falta org")
	}
	prof, err := req.Profile.Resolve(req.Overrides)
	if err != nil {
		return nil, err
	}
	rows, err := req.Source.Rows(ctx)
	if err != nil {
		return nil, fmt.Errorf("dataload: no se pudo leer %s: %w", req.Source.Name(), err)
	}
	if len(rows) > prof.maxRows() {
		return nil, importer.ErrTooManyRows{Got: len(rows), Limit: prof.maxRows()}
	}
	spec := prof.importSpec()
	plan := &Plan{
		Org: req.Org, Entity: prof.Entity, Profile: prof.Name, ProfileVersion: prof.Version,
		ProfileHash: prof.Hash(), Source: req.Source.Name(), InputHash: inputHash(rows),
		Counts: map[Action]int{}, applyOrder: prof.ApplyOrder,
	}

	seen := map[string]int{}
	var keys []string
	for i, raw := range rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rp := RowPlan{Row: i + 1}
		rec, media, ignored, issues := prof.buildRecord(spec, raw, nil, i)
		plan.IgnoredColumns += ignored
		if len(issues) > 0 {
			for j := range issues {
				issues[j].Row = rp.Row
			}
			rp.Action, rp.Issues = ActionInvalid, issues
			plan.add(rp)
			continue
		}
		key, ok := identity(prof.Key, rec)
		if !ok {
			rp.Action = ActionInvalid
			rp.Issues = []importer.RowIssue{{Row: rp.Row, Message: "falta la identidad (" + strings.Join(prof.Key, ", ") + ")"}}
			plan.add(rp)
			continue
		}
		rp.Key, rp.Record, rp.Media = key, rec, media
		rp.Hash = recordHash(rec, media)
		if first, dup := seen[key]; dup {
			rp.Action = ActionDuplicate
			rp.Issues = []importer.RowIssue{{Row: rp.Row, Message: fmt.Sprintf("identidad repetida; se usa la fila %d", first)}}
			plan.add(rp)
			continue
		}
		seen[key] = rp.Row
		keys = append(keys, key)
		plan.add(rp)
	}

	existing := map[string]Existing{}
	if len(keys) > 0 {
		if existing, err = req.Target.Lookup(ctx, req.Org, prof.Entity, keys); err != nil {
			return nil, fmt.Errorf("dataload: consulta al destino: %w", err)
		}
	}
	plan.Counts = map[Action]int{}
	h := sha256.New()
	for i := range plan.Rows {
		rp := &plan.Rows[i]
		if rp.Action == "" {
			ex, found := existing[rp.Key]
			switch {
			case !found:
				rp.Action = ActionCreate
			case ex.Hash != "" && ex.Hash == rp.Hash:
				rp.Action = ActionUnchanged
			default:
				rp.Diffs = diffRecords(ex.Record, rp.Record)
				if ex.Hash == "" && len(rp.Diffs) == 0 {
					rp.Action = ActionUnchanged
				} else {
					rp.Action = ActionUpdate
				}
			}
			fmt.Fprintf(h, "%s|%s|%s\n", rp.Key, rp.Action, rp.Hash)
		}
		plan.Counts[rp.Action]++
		for _, u := range rp.Media {
			plan.MediaURLs += len(u)
		}
	}
	plan.PlanHash = hex.EncodeToString(h.Sum(nil))
	return plan, nil
}

func (p *Plan) add(rp RowPlan) { p.Rows = append(p.Rows, rp) }

// Changes devuelve las escrituras del plan (create y update), en orden de fila
// y, si el perfil declara ApplyOrder, ordenadas por esas reglas (estable).
func (p *Plan) Changes() []Change {
	var out []Change
	for _, r := range p.Rows {
		if r.Action == ActionCreate || r.Action == ActionUpdate {
			out = append(out, Change{Key: r.Key, Action: r.Action, Record: r.Record, Hash: r.Hash})
		}
	}
	if len(p.applyOrder) > 0 {
		rank := func(c Change) []int {
			rs := make([]int, len(p.applyOrder))
			for i, o := range p.applyOrder {
				rs[i] = len(o.Rank)
				v, _ := getPath(c.Record, o.Field)
				s := fmt.Sprint(v)
				for j, want := range o.Rank {
					if s == want {
						rs[i] = j
						break
					}
				}
			}
			return rs
		}
		sort.SliceStable(out, func(a, b int) bool {
			ra, rb := rank(out[a]), rank(out[b])
			for i := range ra {
				if ra[i] != rb[i] {
					return ra[i] < rb[i]
				}
			}
			return false
		})
	}
	return out
}

// identity arma la identidad a partir de los campos clave; vacía = inválida.
func identity(keys []string, rec map[string]any) (string, bool) {
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v, ok := getPath(rec, k)
		s := strings.TrimSpace(fmt.Sprint(v))
		if !ok || v == nil || s == "" {
			return "", false
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "\x1f"), true
}

func getPath(m map[string]any, path string) (any, bool) {
	var cur any = m
	for _, seg := range strings.Split(path, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = mm[seg]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// recordHash es la huella canónica de lo que la fila quiere dejar en el destino
// (registro + URLs de medios).
func recordHash(rec map[string]any, media map[string][]string) string {
	b, _ := json.Marshal(struct {
		R map[string]any      `json:"r"`
		M map[string][]string `json:"m,omitempty"`
	}{rec, media})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// diffRecords compara solo los campos que el perfil escribe.
func diffRecords(old, next map[string]any) []FieldDiff {
	a, b := flatten("", old), flatten("", next)
	var out []FieldDiff
	for k, nv := range b {
		if ov, ok := a[k]; !ok || !equalJSON(ov, nv) {
			out = append(out, FieldDiff{Field: k, Old: a[k], New: nv})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Field < out[j].Field })
	return out
}

func flatten(prefix string, m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		if sub, ok := v.(map[string]any); ok {
			for sk, sv := range flatten(path, sub) {
				out[sk] = sv
			}
			continue
		}
		out[path] = v
	}
	return out
}

func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	if string(x) == string(y) {
		return true
	}
	// 10 (int) vs 10.0 (float64) y similares: compara por texto.
	return fmt.Sprint(a) == fmt.Sprint(b)
}

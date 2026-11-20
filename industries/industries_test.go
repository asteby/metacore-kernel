// Package industries is the multi-industry GATE of the kernel (audit task 8).
//
// Plan v2 / M3 requires that a vertical (restaurant, clinic...) operates end to
// end on the generic primitives with ZERO niche lines in kernel/SDK. This
// package proves it by loading two test-only v3 manifests for two unrelated
// industries (testdata/industries/*.manifest.json), validating them with the v3
// (Strict) and legacy validators, and EXECUTING the real dynamic.Service over
// in-memory SQLite: stage machine (K3), sequences (K4), request_approval (K5).
//
// It lives outside testdata/ because `go test ./...` skips testdata folders;
// only the manifests live there. What the kernel can NOT express yet is in
// docs/industries.md and in the t.Skip("backlog: ...") tests below.
package industries

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

// ---------------------------------------------------------------- models ---

type comanda struct {
	modelbase.BaseUUIDModel
	Status      string  `json:"status"`
	Mesa        string  `json:"mesa"`
	TurnoID     string  `json:"turno_id"`
	ComandaNo   string  `json:"comanda_no"`
	FolioTicket string  `json:"folio_ticket"`
	Total       float64 `json:"total"`
	PaidTotal   float64 `json:"paid_total"`
	DiscountPct float64 `json:"discount_pct"`
}

func (comanda) TableName() string { return "ind_comandas" }
func (comanda) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{Title: "Comandas", SearchColumns: []string{"mesa"}}
}
func (comanda) DefineModal() modelbase.ModalMetadata {
	return modelbase.ModalMetadata{Title: "Comanda"}
}

type cita struct {
	modelbase.BaseUUIDModel
	Status                  string  `json:"status"`
	PacienteID              string  `json:"paciente_id"`
	ConsentimientoFirmadoAt string  `json:"consentimiento_firmado_at"`
	Honorario               float64 `json:"honorario"`
	DescuentoHonorarioPct   float64 `json:"descuento_honorario_pct"`
	NotasPago               string  `json:"notas_pago"`
}

func (cita) TableName() string { return "ind_citas" }
func (cita) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{Title: "Citas", SearchColumns: []string{"paciente_id"}}
}
func (cita) DefineModal() modelbase.ModalMetadata { return modelbase.ModalMetadata{Title: "Cita"} }

type expediente struct {
	modelbase.BaseUUIDModel
	PacienteID string `json:"paciente_id"`
	Nota       string `json:"nota"`
	Folio      string `json:"folio"`
}

func (expediente) TableName() string { return "ind_expedientes" }
func (expediente) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{Title: "Expedientes", SearchColumns: []string{"paciente_id"}}
}
func (expediente) DefineModal() modelbase.ModalMetadata {
	return modelbase.ModalMetadata{Title: "Expediente"}
}

const ddlBase = `id TEXT PRIMARY KEY, organization_id TEXT, created_by_id TEXT,
	created_at DATETIME, updated_at DATETIME, deleted_at DATETIME`

var ddl = []string{
	`CREATE TABLE IF NOT EXISTS ind_comandas (` + ddlBase + `, status TEXT, mesa TEXT, turno_id TEXT,
		comanda_no TEXT, folio_ticket TEXT, total REAL, paid_total REAL, discount_pct REAL)`,
	`CREATE TABLE IF NOT EXISTS ind_citas (` + ddlBase + `, status TEXT, paciente_id TEXT,
		consentimiento_firmado_at TEXT, honorario REAL, descuento_honorario_pct REAL, notas_pago TEXT)`,
	`CREATE TABLE IF NOT EXISTS ind_expedientes (` + ddlBase + `, paciente_id TEXT, nota TEXT, folio TEXT)`,
}

func init() {
	modelbase.Register("ind_comandas", func() modelbase.ModelDefiner { return &comanda{} })
	modelbase.Register("ind_citas", func() modelbase.ModelDefiner { return &cita{} })
	modelbase.Register("ind_expedientes", func() modelbase.ModelDefiner { return &expediente{} })
}

// ------------------------------------------------------------------ user ---

type user struct {
	id, org uuid.UUID
}

func (u *user) GetID() uuid.UUID             { return u.id }
func (u *user) GetOrganizationID() uuid.UUID { return u.org }
func (u *user) GetEmail() string             { return "gate@example.com" }
func (u *user) GetRole() string              { return "" }
func (u *user) GetPasswordHash() string      { return "" }
func (u *user) SetEmail(string)              {}
func (u *user) SetName(string)               {}
func (u *user) SetPasswordHash(string)       {}
func (u *user) SetRole(string)               {}
func (u *user) SetOrganizationID(uuid.UUID)  {}

// ------------------------------------------------------------------- env ---

type env struct {
	t     *testing.T
	svc   *dynamic.Service
	db    *gorm.DB
	org   uuid.UUID
	roles map[uuid.UUID][]string
	clock time.Time
}

func readManifest(t *testing.T, file string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "industries", file))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// validate runs the v3 validator in Strict mode and the legacy validator over
// the host projection, and returns the projection.
func validate(t *testing.T, raw []byte) manifest.Manifest {
	t.Helper()
	warns, err := v3.ValidateWithOptions(raw, v3.Options{Strict: true})
	if err != nil {
		t.Fatalf("v3 Strict validation: %v", err)
	}
	if len(warns) > 0 {
		t.Fatalf("v3 Strict produced warnings: %v", warns)
	}
	m, err := v3.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	legacy := manifest.FromV3(m)
	if err := legacy.Validate("3.0.0"); err != nil {
		t.Fatalf("legacy validation: %v", err)
	}
	return legacy
}

func newEnv(t *testing.T, file string) *env {
	t.Helper()
	legacy := validate(t, readManifest(t, file))

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1) // in-memory SQLite is per connection
	for _, q := range ddl {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}

	e := &env{t: t, db: db, org: uuid.New(), roles: map[uuid.UUID][]string{},
		clock: time.Date(2026, 11, 19, 20, 0, 0, 0, time.UTC)}

	defs := map[string]manifest.ModelDefinition{}
	for _, d := range legacy.ModelDefinitions {
		defs[d.TableName] = d
		defs[d.ModelKey] = d
	}
	e.svc = dynamic.New(dynamic.Config{
		AllowUnconfiguredPermissions: true,
		DB:                           db,
		Metadata:                     metadata.New(metadata.Config{CacheTTL: -1}),
		SequenceClock:                func(context.Context, uuid.UUID) time.Time { return e.clock },
		ActorRolesResolver:           func(_ context.Context, u modelbase.AuthUser) []string { return e.roles[u.GetID()] },
		StageMachineResolver: func(_ context.Context, model string) (*dynamic.StageMachine, bool) {
			d, ok := defs[model]
			if !ok || len(d.Stages) == 0 {
				return nil, false
			}
			return &dynamic.StageMachine{Field: d.StageField, Stages: d.Stages, Transitions: d.Transitions, OnTransition: d.OnTransition}, true
		},
		SequenceResolver: func(_ context.Context, model string) (*dynamic.ModelSequences, bool) {
			d, ok := defs[model]
			if !ok || len(d.Sequences) == 0 {
				return nil, false
			}
			ms := &dynamic.ModelSequences{Sequences: d.Sequences, ColumnBindings: map[string]string{}, Model: d.ModelKey}
			for _, c := range d.Columns {
				if c.Sequence != "" {
					ms.ColumnBindings[c.Name] = c.Sequence
				}
			}
			return ms, true
		},
		ConstraintResolver: func(_ context.Context, model string) (*dynamic.ModelConstraints, bool) {
			d, ok := defs[model]
			if !ok {
				return nil, false
			}
			return dynamic.BuildModelConstraints(d)
		},
	})
	// The rule guards must be wired (fail-open otherwise).
	if un := e.svc.UnwiredRuleGuards(context.Background(), legacy.ModelDefinitions); len(un) != 0 {
		t.Fatalf("unwired rule guards: %v", un)
	}
	return e
}

func (e *env) user(roles ...string) *user {
	u := &user{id: uuid.New(), org: e.org}
	e.roles[u.GetID()] = roles
	return u
}

func (e *env) create(model string, u *user, in map[string]any) (map[string]any, error) {
	return e.svc.Create(context.Background(), model, u, in)
}

func (e *env) mustCreate(model string, u *user, in map[string]any) uuid.UUID {
	e.t.Helper()
	out, err := e.create(model, u, in)
	if err != nil {
		e.t.Fatalf("create %s: %v", model, err)
	}
	return uuid.MustParse(out["id"].(string))
}

func (e *env) update(model string, u *user, id uuid.UUID, in map[string]any) error {
	_, err := e.svc.Update(context.Background(), model, u, id, in)
	return err
}

func (e *env) mustUpdate(model string, u *user, id uuid.UUID, in map[string]any) {
	e.t.Helper()
	if err := e.update(model, u, id, in); err != nil {
		e.t.Fatalf("update %s %v: %v", model, in, err)
	}
}

func (e *env) get(model string, u *user, id uuid.UUID) map[string]any {
	e.t.Helper()
	out, err := e.svc.Get(context.Background(), model, u, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *env) approve(approver *user, err error, reason string) error {
	e.t.Helper()
	var ar *dynamic.ApprovalRequiredError
	if !errors.As(err, &ar) {
		e.t.Fatalf("want approval_required, got %v", err)
	}
	_, aerr := e.svc.ApproveRequest(context.Background(), approver, ar.Request.ID, reason)
	return aerr
}

func isLocked(err error) bool {
	var ve *dynamic.ValidationError
	if !errors.As(err, &ve) {
		return false
	}
	for _, issues := range ve.Fields {
		for _, is := range issues {
			if is.Code == dynamic.CodeRecordLocked {
				return true
			}
		}
	}
	return false
}

func isApproval(err error) bool {
	var ar *dynamic.ApprovalRequiredError
	return errors.As(err, &ar)
}

func count(t *testing.T, db *gorm.DB, table, where string) int64 {
	t.Helper()
	var n int64
	if err := db.Raw("SELECT COUNT(*) FROM " + table + " WHERE " + where).Scan(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

// noNiche asserts that a manifest carries no vertical/country-specific token.
func noNiche(t *testing.T, raw []byte) {
	t.Helper()
	low := strings.ToLower(string(raw))
	for _, tok := range []string{"mxn", "cfdi", "rfc", "llanta", "es-mx", "sat_", "iva", "pitsline", "7leguas"} {
		if strings.Contains(low, tok) {
			t.Errorf("manifest contains niche token %q: the gate must stay generic", tok)
		}
	}
}

// ------------------------------------------------------------ RESTAURANTE ---

func TestRestaurante_Manifest(t *testing.T) {
	raw := readManifest(t, "restaurante.manifest.json")
	validate(t, raw)
	noNiche(t, raw)
}

func TestRestaurante_FlujoFeliz(t *testing.T) {
	e := newEnv(t, "restaurante.manifest.json")
	mesero := e.user("mesero")

	id := e.mustCreate("ind_comandas", mesero, map[string]any{"status": "mesa", "mesa": "12", "turno_id": "T1"})
	// K4: la comanda se numera sola por turno y dia.
	if got := e.get("ind_comandas", mesero, id)["comanda_no"]; got != "T1-19-001" {
		t.Fatalf("comanda_no = %v, want T1-19-001", got)
	}
	e.mustUpdate("ind_comandas", mesero, id, map[string]any{"status": "comanda", "total": 250.0})
	e.mustUpdate("ind_comandas", mesero, id, map[string]any{"status": "cocina"})
	// K4 assign_on: el folio de ticket nace al entrar a cuenta, no antes.
	if got := e.get("ind_comandas", mesero, id)["folio_ticket"]; got != nil && got != "" {
		t.Fatalf("ticket assigned before cuenta: %v", got)
	}
	e.mustUpdate("ind_comandas", mesero, id, map[string]any{"status": "cuenta"})
	if got := e.get("ind_comandas", mesero, id)["folio_ticket"]; got != "T-000001" {
		t.Fatalf("folio_ticket = %v, want T-000001", got)
	}
	// K3 guard: no se cobra con pago insuficiente; con pago completo, si.
	if err := e.update("ind_comandas", mesero, id, map[string]any{"status": "pagada", "paid_total": 100.0}); !errors.Is(err, dynamic.ErrInvalidTransition) {
		t.Fatalf("underpaid charge: %v, want invalid_transition", err)
	}
	e.mustUpdate("ind_comandas", mesero, id, map[string]any{"status": "pagada", "paid_total": 250.0})
	if got := e.get("ind_comandas", mesero, id)["status"]; got != "pagada" {
		t.Fatalf("status = %v", got)
	}
}

func TestRestaurante_Rechazos(t *testing.T) {
	e := newEnv(t, "restaurante.manifest.json")
	mesero, cocinero, gerente := e.user("mesero"), e.user("cocinero"), e.user("gerente")

	id := e.mustCreate("ind_comandas", mesero, map[string]any{"status": "mesa", "turno_id": "T1"})

	// requires_fields: sin mesa no se abre la comanda.
	var ve *dynamic.ValidationError
	if err := e.update("ind_comandas", mesero, id, map[string]any{"status": "comanda"}); !errors.As(err, &ve) {
		t.Fatalf("open without mesa: %v, want validation error", err)
	}
	// rol sin permiso.
	if err := e.update("ind_comandas", cocinero, id, map[string]any{"status": "comanda", "mesa": "3"}); !errors.Is(err, dynamic.ErrForbidden) {
		t.Fatalf("cocinero opening: %v, want forbidden", err)
	}
	e.mustUpdate("ind_comandas", mesero, id, map[string]any{"status": "comanda", "mesa": "3", "total": 80.0})
	// transicion invalida: de comanda no se salta a cuenta ni a pagada.
	for _, to := range []string{"cuenta", "pagada", "mesa"} {
		if err := e.update("ind_comandas", gerente, id, map[string]any{"status": to}); !errors.Is(err, dynamic.ErrInvalidTransition) {
			t.Fatalf("comanda -> %s: %v, want invalid_transition", to, err)
		}
	}
	// Un numerico en 0 cuenta como "presente" para requires_fields (la columna
	// existe); "total > 0" se expresa con guard, no con requires_fields.
	id0 := e.mustCreate("ind_comandas", mesero, map[string]any{"status": "comanda", "mesa": "9", "turno_id": "T1"})
	if err := e.update("ind_comandas", mesero, id0, map[string]any{"status": "cocina"}); !errors.Is(err, dynamic.ErrInvalidTransition) {
		t.Fatalf("send empty comanda to kitchen: %v, want invalid_transition (guard total > 0)", err)
	}
}

func TestRestaurante_CancelacionConAprobacion(t *testing.T) {
	e := newEnv(t, "restaurante.manifest.json")
	mesero, gerente := e.user("mesero"), e.user("gerente")
	id := e.mustCreate("ind_comandas", mesero, map[string]any{"status": "cocina", "mesa": "5", "total": 90.0, "turno_id": "T1"})

	err := e.update("ind_comandas", mesero, id, map[string]any{"status": "cancelada"})
	if !isApproval(err) {
		t.Fatalf("cancel: %v, want approval_required", err)
	}
	if got := e.get("ind_comandas", mesero, id)["status"]; got != "cocina" {
		t.Fatalf("status moved before approval: %v", got)
	}
	// Un mesero no aprueba; el gerente sin motivo tampoco (reason_required).
	if aerr := e.approve(mesero, err, "x"); aerr == nil {
		t.Fatal("a mesero approved a cancellation")
	}
	if aerr := e.approve(gerente, err, ""); aerr == nil {
		t.Fatal("approval without reason must be refused (reason_required)")
	}
	if aerr := e.approve(gerente, err, "cliente se fue"); aerr != nil {
		t.Fatalf("approve: %v", aerr)
	}
	if got := e.get("ind_comandas", mesero, id)["status"]; got != "cancelada" {
		t.Fatalf("status after approval = %v, want cancelada", got)
	}
	// Etapa final y locked: ni edicion ni salida sin aprobacion.
	if err := e.update("ind_comandas", gerente, id, map[string]any{"total": 1.0}); !isLocked(err) {
		t.Fatalf("edit of a cancelled comanda: %v, want record_locked", err)
	}
	if err := e.update("ind_comandas", gerente, id, map[string]any{"status": "cocina"}); !errors.Is(err, dynamic.ErrInvalidTransition) {
		t.Fatalf("revive cancelled: %v, want invalid_transition", err)
	}
}

func TestRestaurante_PagadaLockedYReapertura(t *testing.T) {
	e := newEnv(t, "restaurante.manifest.json")
	mesero, gerente := e.user("mesero"), e.user("gerente")
	id := e.mustCreate("ind_comandas", mesero, map[string]any{"status": "cuenta", "mesa": "1", "total": 100.0, "turno_id": "T1"})
	e.mustUpdate("ind_comandas", mesero, id, map[string]any{"status": "pagada", "paid_total": 100.0})

	if err := e.update("ind_comandas", gerente, id, map[string]any{"total": 5.0}); !isLocked(err) {
		t.Fatalf("edit of a paid comanda: %v, want record_locked", err)
	}
	if err := e.svc.Delete(context.Background(), "ind_comandas", gerente, id); !isLocked(err) {
		t.Fatalf("delete of a paid comanda: %v, want record_locked", err)
	}
	// reopen + approval: queda aparcado, el gerente aprueba con motivo.
	err := e.update("ind_comandas", mesero, id, map[string]any{"status": "cuenta"})
	if !isApproval(err) {
		t.Fatalf("reopen: %v, want approval_required", err)
	}
	if got := e.get("ind_comandas", mesero, id)["status"]; got != "pagada" {
		t.Fatalf("reopened before approval: %v", got)
	}
	if aerr := e.approve(gerente, err, "error de captura"); aerr != nil {
		t.Fatalf("approve reopen: %v", aerr)
	}
	if got := e.get("ind_comandas", mesero, id)["status"]; got != "cuenta" {
		t.Fatalf("status after reopen = %v, want cuenta", got)
	}
}

func TestRestaurante_DescuentoAltoPideAprobacionYReplay(t *testing.T) {
	e := newEnv(t, "restaurante.manifest.json")
	mesero, gerente := e.user("mesero"), e.user("gerente")

	if _, err := e.create("ind_comandas", mesero, map[string]any{"status": "mesa", "turno_id": "T1", "discount_pct": 10.0}); err != nil {
		t.Fatalf("10%% discount must pass: %v", err)
	}
	_, err := e.create("ind_comandas", mesero, map[string]any{"status": "mesa", "turno_id": "T1", "mesa": "VIP", "discount_pct": 25.0})
	if !isApproval(err) {
		t.Fatalf("25%% discount: %v, want approval_required", err)
	}
	// Aparcada: nada escrito, y no consumio folio de comanda.
	if n := count(t, e.db, "ind_comandas", "mesa = 'VIP'"); n != 0 {
		t.Fatalf("parked write persisted (%d rows)", n)
	}
	if aerr := e.approve(mesero, err, ""); aerr == nil {
		t.Fatal("a mesero approved a discount")
	}
	if aerr := e.approve(gerente, err, ""); aerr != nil {
		t.Fatalf("approve: %v", aerr)
	}
	// Replay: la misma escritura se aplica UNA vez.
	if n := count(t, e.db, "ind_comandas", "mesa = 'VIP'"); n != 1 {
		t.Fatalf("approved write applied %d times, want 1", n)
	}
	// Una aprobacion ya consumida no se repite.
	var ar *dynamic.ApprovalRequiredError
	errors.As(err, &ar)
	if _, aerr := e.svc.ApproveRequest(context.Background(), gerente, ar.Request.ID, ""); aerr == nil {
		t.Fatal("approving twice must fail")
	}
	if n := count(t, e.db, "ind_comandas", "mesa = 'VIP'"); n != 1 {
		t.Fatalf("double approval duplicated the row (%d)", n)
	}
}

func TestRestaurante_FoliosPorTurnoYGaplessConcurrente(t *testing.T) {
	e := newEnv(t, "restaurante.manifest.json")
	mesero := e.user("mesero")

	// comanda_no: una serie por turno_id (scope column) y por dia (period day).
	mk := func(turno string) string {
		out, err := e.create("ind_comandas", mesero, map[string]any{"status": "mesa", "turno_id": turno})
		if err != nil {
			t.Fatal(err)
		}
		return out["comanda_no"].(string)
	}
	if a, b, c := mk("T1"), mk("T1"), mk("T2"); a != "T1-19-001" || b != "T1-19-002" || c != "T2-19-001" {
		t.Fatalf("series per turno: %s %s %s", a, b, c)
	}
	e.clock = e.clock.Add(24 * time.Hour)
	if got := mk("T1"); got != "T1-20-001" {
		t.Fatalf("a new day restarts the series: %s", got)
	}

	// ticket gapless: 24 cuentas concurrentes => T-000001..T-000024 sin huecos.
	const n = 24
	ids := make([]uuid.UUID, n)
	for i := range ids {
		ids[i] = e.mustCreate("ind_comandas", mesero, map[string]any{"status": "cocina", "mesa": fmt.Sprint(i), "total": 10.0, "turno_id": "T9"})
	}
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.update("ind_comandas", mesero, id, map[string]any{"status": "cuenta"}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent cuenta: %v", err)
	}
	var tickets []string
	for _, id := range ids {
		tickets = append(tickets, e.get("ind_comandas", mesero, id)["folio_ticket"].(string))
	}
	sort.Strings(tickets)
	for i, tk := range tickets {
		if want := fmt.Sprintf("T-%06d", i+1); tk != want {
			t.Fatalf("ticket #%d = %s, want %s (gapless)", i, tk, want)
		}
	}
}

// ------------------------------------------------------------- CONSULTORIO ---

func TestConsultorio_Manifest(t *testing.T) {
	raw := readManifest(t, "consultorio.manifest.json")
	validate(t, raw)
	noNiche(t, raw)
}

func TestConsultorio_FlujoFelizYRechazos(t *testing.T) {
	e := newEnv(t, "consultorio.manifest.json")
	recepcion, medico := e.user("recepcion"), e.user("medico")

	id := e.mustCreate("ind_citas", recepcion, map[string]any{"status": "solicitada", "paciente_id": "P1"})
	// transicion invalida: no se atiende una cita solo solicitada.
	if err := e.update("ind_citas", medico, id, map[string]any{"status": "atendida"}); !errors.Is(err, dynamic.ErrInvalidTransition) {
		t.Fatalf("solicitada -> atendida: %v, want invalid_transition", err)
	}
	e.mustUpdate("ind_citas", recepcion, id, map[string]any{"status": "confirmada"})
	// rol: recepcion no atiende.
	if err := e.update("ind_citas", recepcion, id, map[string]any{"status": "atendida", "consentimiento_firmado_at": "2026-11-19T10:00:00Z"}); !errors.Is(err, dynamic.ErrForbidden) {
		t.Fatalf("recepcion attending: %v, want forbidden", err)
	}
	// requires_fields: sin consentimiento firmado no se atiende.
	var ve *dynamic.ValidationError
	if err := e.update("ind_citas", medico, id, map[string]any{"status": "atendida"}); !errors.As(err, &ve) {
		t.Fatalf("attend without consent: %v, want validation error", err)
	}
	e.mustUpdate("ind_citas", medico, id, map[string]any{"status": "atendida", "consentimiento_firmado_at": "2026-11-19T10:00:00Z"})
	// guard honorario > 0: no se cobra con honorario 0.
	if err := e.update("ind_citas", recepcion, id, map[string]any{"status": "cobrada"}); !errors.Is(err, dynamic.ErrInvalidTransition) {
		t.Fatalf("charge with honorario 0: %v, want invalid_transition", err)
	}
	e.mustUpdate("ind_citas", recepcion, id, map[string]any{"status": "cobrada", "honorario": 800.0})
	// locked con editable_when_locked: notas_pago si, honorario no.
	e.mustUpdate("ind_citas", recepcion, id, map[string]any{"notas_pago": "pago en efectivo"})
	if err := e.update("ind_citas", recepcion, id, map[string]any{"honorario": 1.0}); !isLocked(err) {
		t.Fatalf("edit honorario of a charged cita: %v, want record_locked", err)
	}
	if err := e.update("ind_citas", recepcion, id, map[string]any{"status": "confirmada"}); !errors.Is(err, dynamic.ErrInvalidTransition) {
		t.Fatalf("leaving cobrada: %v, want invalid_transition", err)
	}
	// cancelar desde confirmada; cancelada es final+locked.
	c := e.mustCreate("ind_citas", recepcion, map[string]any{"status": "confirmada", "paciente_id": "P2"})
	e.mustUpdate("ind_citas", recepcion, c, map[string]any{"status": "cancelada"})
	if err := e.update("ind_citas", recepcion, c, map[string]any{"honorario": 5.0}); !isLocked(err) {
		t.Fatalf("edit of a cancelled cita: %v, want record_locked", err)
	}
}

func TestConsultorio_DescuentoHonorarioAprobacion(t *testing.T) {
	e := newEnv(t, "consultorio.manifest.json")
	recepcion, director := e.user("recepcion"), e.user("director")
	if _, err := e.create("ind_citas", recepcion, map[string]any{"status": "solicitada", "paciente_id": "P1", "descuento_honorario_pct": 20.0}); err != nil {
		t.Fatalf("20%% must pass: %v", err)
	}
	_, err := e.create("ind_citas", recepcion, map[string]any{"status": "solicitada", "paciente_id": "P-DESC", "descuento_honorario_pct": 50.0})
	if !isApproval(err) {
		t.Fatalf("50%% discount: %v, want approval_required", err)
	}
	if n := count(t, e.db, "ind_citas", "paciente_id = 'P-DESC'"); n != 0 {
		t.Fatalf("parked write persisted: %d", n)
	}
	if aerr := e.approve(recepcion, err, ""); aerr == nil {
		t.Fatal("recepcion approved a discount")
	}
	if aerr := e.approve(director, err, ""); aerr != nil {
		t.Fatalf("approve: %v", aerr)
	}
	if n := count(t, e.db, "ind_citas", "paciente_id = 'P-DESC'"); n != 1 {
		t.Fatalf("replay applied %d times, want 1", n)
	}
}

func TestConsultorio_ExpedienteGaplessPorPaciente(t *testing.T) {
	e := newEnv(t, "consultorio.manifest.json")
	medico := e.user("medico")

	// El folio del expediente es una serie por paciente (scope column, gapless).
	for i, want := range []string{"EXP-0001", "EXP-0002"} {
		out, err := e.create("ind_expedientes", medico, map[string]any{"paciente_id": "P1", "nota": fmt.Sprint(i)})
		if err != nil || out["folio"] != want {
			t.Fatalf("P1 #%d: %v (err %v), want %s", i, out["folio"], err, want)
		}
	}
	if out, err := e.create("ind_expedientes", medico, map[string]any{"paciente_id": "P2"}); err != nil || out["folio"] != "EXP-0001" {
		t.Fatalf("P2 owns its own series: %v (err %v)", out["folio"], err)
	}
	// Gapless: el folio solo sale por la columna, no por sequence_next suelto.
	if _, err := e.svc.NextSequenceScoped(context.Background(), medico, "ind_expedientes", "exp", "P1"); err == nil {
		t.Fatal("gapless sequence must refuse a standalone draw")
	}
	// Un INSERT fallido no quema folio.
	dup := uuid.NewString()
	if _, err := e.create("ind_expedientes", medico, map[string]any{"id": dup, "paciente_id": "P3"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.create("ind_expedientes", medico, map[string]any{"id": dup, "paciente_id": "P3"}); err == nil {
		t.Fatal("duplicate id must fail")
	}
	if out, err := e.create("ind_expedientes", medico, map[string]any{"paciente_id": "P3"}); err != nil || out["folio"] != "EXP-0002" {
		t.Fatalf("failed insert burnt a folio: %v (err %v)", out["folio"], err)
	}

	// Concurrencia: 40 expedientes sobre 2 pacientes => 1..20 por paciente.
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[string][]string{}
	for i := 0; i < 40; i++ {
		pac := fmt.Sprintf("C%d", i%2)
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := e.create("ind_expedientes", medico, map[string]any{"paciente_id": pac})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("concurrent create: %v", err)
				return
			}
			got[pac] = append(got[pac], out["folio"].(string))
		}()
	}
	wg.Wait()
	for pac, folios := range got {
		sort.Strings(folios)
		for i, f := range folios {
			if want := fmt.Sprintf("EXP-%04d", i+1); f != want {
				t.Fatalf("%s #%d = %s, want %s (gapless per patient)", pac, i, f, want)
			}
		}
	}
}

// ---------------------------------------------------------------- BACKLOG ---
//
// Flows the industries need that the current primitives CANNOT express. They
// stay as skipped tests (not hacks) so the gate documents the gap and a future
// primitive flips them on. See docs/industries.md.

func TestBacklog(t *testing.T) {
	for _, c := range []struct{ name, primitive string }{
		{"restaurante: split de cuenta, propinas y pagos multiples sobre un documento", "pagos/lineas de pago hijas con reparto (split) y propina como linea"},
		{"restaurante: ruteo de comanda a estacion de cocina (KDS)", "ruteo por linea a estaciones con estado propio por linea"},
		{"restaurante: modificadores y combos", "modificadores/combos de linea con precio y receta"},
		{"consultorio: agenda con slots/recursos sin empalmes", "recurso con calendario y exclusion de empalmes (constraint de rango/intervalo)"},
		{"consultorio: recordatorios relativos a la cita", "disparadores relativos a una fecha del registro (T-24h)"},
		{"consultorio: consentimiento con vigencia", "guard/when sobre fechas y now() (hoy solo numerico)"},
		{"guard por string: cobrar solo si metodo_pago = 'efectivo'", "guard/when no numerico: K3 evalua evalConstraintExpr sobre mergedNumericEnv"},
		{"lotes con caducidad y recetas (insumos de cocina)", "lotes/caducidad (FEFO) y explosion de recetas"},
		{"impuestos multiples o por pais", "impuestos por linea/pais (orgconfig.TaxRate es unico)"},
		{"moneda por documento y tipo de cambio", "moneda por documento con tipo de cambio"},
	} {
		t.Run(c.name, func(t *testing.T) { t.Skip("backlog: " + c.primitive) })
	}
}

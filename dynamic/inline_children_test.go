package dynamic

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

// Registros padre con filas hijas en una sola llamada (relaciones one_to_many
// con create_inline). Arnés: regla de descuento (ic_rules) con escalones
// (ic_tiers, mínimo 1), notas polimórficas con Scope (ic_notes) y un hijo cuyo
// crear está vedado (ic_secrets). Modelos reflect como los de un addon.

func icDefs() map[string]manifest.ModelDefinition {
	return map[string]manifest.ModelDefinition{
		"IcRule": {
			ModelKey: "IcRule", TableName: "ic_rules", OrgScoped: true,
			Columns: []manifest.ColumnDef{{Name: "name", Type: "string"}},
			Relations: []manifest.RelationDef{
				{Name: "tiers", Kind: "one_to_many", Through: "IcTier", ForeignKey: "rule_id", Embed: true, CreateInline: true, Min: 1},
				{Name: "notes", Kind: "one_to_many", Through: "IcNote", ForeignKey: "owner_id", Embed: true, CreateInline: true,
					Scope: map[string]string{"owner_model": "IcRule"}},
				{Name: "secrets", Kind: "one_to_many", Through: "IcSecret", ForeignKey: "rule_id", Embed: true, CreateInline: true},
				// Declarada pero sin create_inline: no admite _children.
				{Name: "audits", Kind: "one_to_many", Through: "IcTier", ForeignKey: "rule_id", Embed: true},
			},
		},
		"IcTier": {
			ModelKey: "IcTier", TableName: "ic_tiers", OrgScoped: true,
			Columns: []manifest.ColumnDef{
				{Name: "rule_id", Type: "uuid"}, {Name: "min_amount", Type: "decimal"}, {Name: "percent", Type: "decimal"},
			},
		},
		"IcNote": {
			ModelKey: "IcNote", TableName: "ic_notes", OrgScoped: true,
			Columns: []manifest.ColumnDef{
				{Name: "owner_id", Type: "uuid"}, {Name: "owner_model", Type: "string"}, {Name: "body", Type: "string"},
			},
		},
		"IcSecret": {
			ModelKey: "IcSecret", TableName: "ic_secrets", OrgScoped: true,
			Columns: []manifest.ColumnDef{{Name: "rule_id", Type: "uuid"}, {Name: "body", Type: "string"}},
		},
		// Hijos de OTRO addon (relation.addon): IcPrice es de «pricing» (registrado) e
		// IcGhost de «ghostaddon» (no registrado: su addon no está instalado).
		"IcProduct": {
			ModelKey: "IcProduct", TableName: "ic_products", OrgScoped: true,
			Columns: []manifest.ColumnDef{{Name: "name", Type: "string"}},
			Relations: []manifest.RelationDef{
				{Name: "prices", Kind: "one_to_many", Through: "IcPrice", ForeignKey: "product_id", Embed: true, CreateInline: true, Addon: "pricing", Widget: "market_prices"},
				{Name: "ghosts", Kind: "one_to_many", Through: "IcGhost", ForeignKey: "product_id", Embed: true, CreateInline: true, Addon: "ghostaddon"},
			},
		},
		"IcPrice": {
			ModelKey: "IcPrice", TableName: "ic_prices", OrgScoped: true,
			Columns: []manifest.ColumnDef{{Name: "product_id", Type: "uuid"}, {Name: "market", Type: "string"}, {Name: "price", Type: "decimal"}},
		},
	}
}

func newInlineService(t *testing.T, withResolver bool, hooks ...*HookRegistry) (*Service, *gorm.DB) {
	t.Helper()
	db := setupTestDB(t)
	defs := icDefs()
	types := map[string]reflect.Type{}
	for name, def := range defs {
		cols := "id TEXT PRIMARY KEY, organization_id TEXT, created_at DATETIME, updated_at DATETIME, deleted_at DATETIME, created_by_id TEXT, updated_by_id TEXT, deleted_by_id TEXT"
		for _, c := range def.Columns {
			sqlType := "TEXT"
			if c.Type == "decimal" {
				sqlType = "REAL"
			}
			cols += ", " + c.Name + " " + sqlType
		}
		mustExec(t, db, fmt.Sprintf("CREATE TABLE %s (%s)", def.TableName, cols))
		typ, err := BuildStructType(def)
		if err != nil {
			t.Fatal(err)
		}
		types[name] = typ
		table := def.TableName
		modelbase.Register(name, func() modelbase.ModelDefiner { return dlMeta{table: table} })
	}
	cfg := Config{
		AllowUnconfiguredPermissions: true,
		DB:                           db,
		Metadata:                     metadata.New(metadata.Config{CacheTTL: -1}),
		ModelResolver: func(_ context.Context, name string) (any, bool) {
			typ, ok := types[name]
			if !ok {
				return nil, false
			}
			return reflect.New(typ).Interface(), true
		},
		TableNameResolver: func(_ context.Context, name string) (string, bool) {
			d, ok := defs[name]
			return d.TableName, ok
		},
		// El crear de IcSecret está vedado a toda persona: el padre no debe
		// escribirse si se le mandan hijos de ese modelo.
		AccessPolicyResolver: func(_ context.Context, model string) (modelbase.AccessPolicy, bool) {
			if model == "IcSecret" {
				return modelbase.AccessPolicy{Create: modelbase.AccessNobody()}, true
			}
			return modelbase.AccessPolicy{}, false
		},
	}
	if len(hooks) > 0 {
		cfg.Hooks = hooks[0]
	}
	if withResolver {
		cfg.InlineRelationsResolver = func(_ context.Context, model string) ([]InlineRelation, bool) {
			d, ok := defs[model]
			if !ok {
				return nil, false
			}
			rels := DeriveInlineRelations(d)
			return rels, len(rels) > 0
		}
	}
	return New(cfg), db
}

func icRows(t *testing.T, db *gorm.DB, table, col, val string) []map[string]any {
	t.Helper()
	var rows []map[string]any
	db.Raw(fmt.Sprintf("SELECT * FROM %s WHERE %s = ? AND deleted_at IS NULL", table, col), val).Scan(&rows)
	return rows
}

func icCount(t *testing.T, db *gorm.DB, table string) int64 {
	t.Helper()
	var n int64
	db.Table(table).Count(&n)
	return n
}

func icValidation(t *testing.T, err error) *ValidationError {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	return ve
}

func TestDeriveInlineRelations_OnlyCreateInlineOneToMany(t *testing.T) {
	d := icDefs()["IcRule"]
	got := DeriveInlineRelations(d)
	names := []string{}
	for _, r := range got {
		names = append(names, r.Name)
	}
	if !reflect.DeepEqual(names, []string{"tiers", "notes", "secrets"}) {
		t.Fatalf("relations = %v (audits has no create_inline)", names)
	}
	if got[0].Model != "IcTier" || got[0].ForeignKey != "rule_id" || got[0].Min != 1 {
		t.Fatalf("tiers = %+v", got[0])
	}
	if got[1].Scope["owner_model"] != "IcRule" {
		t.Fatalf("notes scope = %+v", got[1].Scope)
	}
	// readonly y many_to_many no se derivan, aunque declaren create_inline.
	d.Relations = []manifest.RelationDef{
		{Name: "ro", Kind: "one_to_many", Through: "IcTier", ForeignKey: "rule_id", CreateInline: true, Readonly: true},
		{Name: "mm", Kind: "many_to_many", Through: "IcTier", ForeignKey: "rule_id", CreateInline: true},
	}
	if got := DeriveInlineRelations(d); len(got) != 0 {
		t.Fatalf("derived %+v", got)
	}
}

func TestCreate_WritesParentWithChildren(t *testing.T) {
	svc, db := newInlineService(t, true)
	user := newUser(uuid.New())
	id := uuid.NewString()
	out, err := svc.Create(context.Background(), "IcRule", user, map[string]any{
		"id": id, "name": "Mayoreo",
		"_children": map[string]any{
			"tiers": []any{
				map[string]any{"min_amount": 1000.0, "percent": 5.0},
				map[string]any{"min_amount": 5000.0, "percent": 10.0},
			},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if fmt.Sprint(out["id"]) != id {
		t.Fatalf("parent id = %v", out["id"])
	}
	if _, leaked := out["_children"]; leaked {
		t.Fatal("_children must not be served back as a column")
	}
	rows := icRows(t, db, "ic_tiers", "rule_id", id)
	if len(rows) != 2 {
		t.Fatalf("tiers = %d, want 2 (%v)", len(rows), rows)
	}
	for _, r := range rows {
		if fmt.Sprint(r["organization_id"]) != user.orgID.String() {
			t.Fatalf("tier lost the org scope: %v", r)
		}
	}
}

// El foreign_key y el id del hijo los pone el runtime: lo que mande el cliente
// no apunta a otro padre ni pisa otra fila.
func TestCreate_ChildForeignKeyAndIDAreRuntimeOwned(t *testing.T) {
	svc, db := newInlineService(t, true)
	user := newUser(uuid.New())
	id, other, forged := uuid.NewString(), uuid.NewString(), uuid.NewString()
	_, err := svc.Create(context.Background(), "IcRule", user, map[string]any{
		"id": id, "name": "R",
		"_children": map[string]any{"tiers": []any{
			map[string]any{"id": forged, "rule_id": other, "min_amount": 1.0, "percent": 1.0},
		}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if n := len(icRows(t, db, "ic_tiers", "rule_id", other)); n != 0 {
		t.Fatalf("a tier was attached to the foreign parent: %d", n)
	}
	rows := icRows(t, db, "ic_tiers", "rule_id", id)
	if len(rows) != 1 || fmt.Sprint(rows[0]["id"]) == forged {
		t.Fatalf("tiers = %v", rows)
	}
}

func TestCreate_ChildScopeIsStamped(t *testing.T) {
	svc, db := newInlineService(t, true)
	user := newUser(uuid.New())
	id := uuid.NewString()
	_, err := svc.Create(context.Background(), "IcRule", user, map[string]any{
		"id": id, "name": "R",
		"_children": map[string]any{
			"tiers": []any{map[string]any{"min_amount": 1.0, "percent": 1.0}},
			"notes": []any{map[string]any{"body": "hola", "owner_model": "Other"}},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	notes := icRows(t, db, "ic_notes", "owner_id", id)
	if len(notes) != 1 || fmt.Sprint(notes[0]["owner_model"]) != "IcRule" || fmt.Sprint(notes[0]["body"]) != "hola" {
		t.Fatalf("notes = %v (Scope must win over the client)", notes)
	}
}

func TestCreate_InlineMinNotMet(t *testing.T) {
	svc, db := newInlineService(t, true)
	user := newUser(uuid.New())
	for name, children := range map[string]any{
		"empty list": map[string]any{"tiers": []any{}},
		"missing":    map[string]any{},
		"null":       map[string]any{"tiers": nil},
	} {
		_, err := svc.Create(context.Background(), "IcRule", user, map[string]any{"name": "R", "_children": children})
		ve := icValidation(t, err)
		got := ve.Fields["_children.tiers"]
		if len(got) != 1 || got[0].Code != "min" || got[0].Params["min"] != 1 || got[0].Params["count"] != 0 {
			t.Fatalf("%s: _children.tiers = %+v", name, got)
		}
	}
	if n := icCount(t, db, "ic_rules"); n != 0 {
		t.Fatalf("a rejected create left %d rules", n)
	}
}

func TestCreate_InlineUnknownOrUndeclaredRelation(t *testing.T) {
	svc, db := newInlineService(t, true)
	user := newUser(uuid.New())
	good := []any{map[string]any{"min_amount": 1.0, "percent": 1.0}}
	_, err := svc.Create(context.Background(), "IcRule", user, map[string]any{
		"name": "R",
		"_children": map[string]any{
			"tiers":   good,
			"audits":  good, // existe pero no declara create_inline
			"missing": good, // no existe
		},
	})
	ve := icValidation(t, err)
	for _, k := range []string{"_children.audits", "_children.missing"} {
		if f := ve.Fields[k]; len(f) != 1 || f[0].Code != "invalid_relation" {
			t.Fatalf("%s = %+v", k, f)
		}
	}
	if _, bad := ve.Fields["_children.tiers"]; bad {
		t.Fatalf("a valid relation must not be flagged: %+v", ve.Fields)
	}
	if n := icCount(t, db, "ic_rules"); n != 0 {
		t.Fatalf("rejected create left %d rules", n)
	}
}

// Sin resolver el host no declaró ninguna relación inline: `_children` se
// rechaza en vez de perderse sin avisar.
func TestCreate_InlineWithoutResolverRejectsChildren(t *testing.T) {
	svc, db := newInlineService(t, false)
	user := newUser(uuid.New())
	_, err := svc.Create(context.Background(), "IcRule", user, map[string]any{
		"name": "R", "_children": map[string]any{"tiers": []any{map[string]any{"min_amount": 1.0}}},
	})
	ve := icValidation(t, err)
	if f := ve.Fields["_children.tiers"]; len(f) != 1 || f[0].Code != "invalid_relation" {
		t.Fatalf("errors = %+v", ve.Fields)
	}
	if n := icCount(t, db, "ic_rules"); n != 0 {
		t.Fatalf("left %d rules", n)
	}
}

func TestCreate_InlineBadShapes(t *testing.T) {
	svc, _ := newInlineService(t, true)
	user := newUser(uuid.New())
	ctx := context.Background()

	_, err := svc.Create(ctx, "IcRule", user, map[string]any{"name": "R", "_children": "nope"})
	if f := icValidation(t, err).Fields["_children"]; len(f) != 1 || f[0].Code != "invalid_type" {
		t.Fatalf("_children as string: %+v", f)
	}
	_, err = svc.Create(ctx, "IcRule", user, map[string]any{"name": "R", "_children": map[string]any{"tiers": map[string]any{"a": 1}}})
	if f := icValidation(t, err).Fields["_children.tiers"]; len(f) != 1 || f[0].Code != "invalid_type" {
		t.Fatalf("tiers as object: %+v", f)
	}
	_, err = svc.Create(ctx, "IcRule", user, map[string]any{"name": "R", "_children": map[string]any{"tiers": []any{
		map[string]any{"min_amount": 1.0}, "not an object", 7,
	}}})
	ve := icValidation(t, err)
	for _, k := range []string{"_children.tiers.1", "_children.tiers.2"} {
		if f := ve.Fields[k]; len(f) != 1 || f[0].Code != "invalid_type" {
			t.Fatalf("%s = %+v", k, f)
		}
	}
	// Hijos de hijos: no se escriben (no habría cómo deshacerlos).
	_, err = svc.Create(ctx, "IcRule", user, map[string]any{"name": "R", "_children": map[string]any{"tiers": []any{
		map[string]any{"min_amount": 1.0, "_children": map[string]any{"x": []any{}}},
	}}})
	if f := icValidation(t, err).Fields["_children.tiers.0._children"]; len(f) != 1 {
		t.Fatalf("nested _children not rejected: %+v", f)
	}
}

// Una fila inválida no deja el padre ni las filas anteriores: se deshace todo
// y el error cae en la fila (_children.<relación>.<índice>.<campo>).
func TestCreate_InlineFailureUndoesEverything(t *testing.T) {
	hooks := NewHookRegistry()
	hooks.RegisterBeforeCreate("IcNote", func(_ context.Context, _ HookContext, in map[string]any) error {
		if in["body"] == nil || in["body"] == "" {
			return NewValidationError().AddMessage("body", "Escribe la nota").Err()
		}
		return nil
	})
	svc, db := newInlineService(t, true, hooks)
	user := newUser(uuid.New())
	id := uuid.NewString()
	_, err := svc.Create(context.Background(), "IcRule", user, map[string]any{
		"id": id, "name": "R",
		"_children": map[string]any{
			// tiers se escribe primero; notes falla en su 2.ª fila: también se
			// deshacen los escalones de la otra relación.
			"tiers": []any{map[string]any{"min_amount": 1.0, "percent": 1.0}},
			"notes": []any{
				map[string]any{"body": "nota"},
				map[string]any{}, // sin body
			},
		},
	})
	ve := icValidation(t, err)
	if f := ve.Fields["_children.notes.1.body"]; len(f) != 1 {
		t.Fatalf("errors = %+v", ve.Fields)
	}
	for _, table := range []string{"ic_rules", "ic_tiers", "ic_notes"} {
		if n := icCount(t, db, table); n != 0 {
			t.Fatalf("%s kept %d rows after the failed create", table, n)
		}
	}
}

// Sin permiso de crear el hijo, el padre ni se escribe.
func TestCreate_InlineRespectsChildCreatePermission(t *testing.T) {
	svc, db := newInlineService(t, true)
	user := newUser(uuid.New())
	_, err := svc.Create(context.Background(), "IcRule", user, map[string]any{
		"name": "R",
		"_children": map[string]any{
			"tiers":   []any{map[string]any{"min_amount": 1.0, "percent": 1.0}},
			"secrets": []any{map[string]any{"body": "x"}},
		},
	})
	var ade *AccessDeniedError
	if !errors.As(err, &ade) || !errors.Is(err, ErrForbidden) || ade.Model != "IcSecret" {
		t.Fatalf("err = %v, want AccessDeniedError on IcSecret", err)
	}
	if n := icCount(t, db, "ic_rules") + icCount(t, db, "ic_tiers"); n != 0 {
		t.Fatalf("left %d rows", n)
	}
	// Una relación sin filas no exige el permiso.
	if _, err := svc.Create(context.Background(), "IcRule", user, map[string]any{
		"name": "R2",
		"_children": map[string]any{
			"tiers":   []any{map[string]any{"min_amount": 1.0, "percent": 1.0}},
			"secrets": []any{},
		},
	}); err != nil {
		t.Fatalf("empty forbidden relation must not block: %v", err)
	}
}

// Sin `_children` todo es como antes, también con el resolver cableado y con
// relaciones que declaran min (el mínimo solo aplica al enviar `_children`).
func TestCreate_InlineWithoutChildrenIsUnchanged(t *testing.T) {
	for _, withResolver := range []bool{true, false} {
		svc, db := newInlineService(t, withResolver)
		user := newUser(uuid.New())
		out, err := svc.Create(context.Background(), "IcRule", user, map[string]any{"name": "Solo"})
		if err != nil {
			t.Fatalf("resolver=%v: %v", withResolver, err)
		}
		if n := icCount(t, db, "ic_tiers"); n != 0 {
			t.Fatalf("tiers = %d", n)
		}
		if fmt.Sprint(out["name"]) != "Solo" {
			t.Fatalf("out = %v", out)
		}
	}
}

// El input original (el que reproduce una aprobación) conserva `_children`:
// el replay de un crear estacionado escribe el padre CON sus hijos.
func TestCreate_InlineSurvivesApprovalReplayInput(t *testing.T) {
	svc, db := newInlineService(t, true)
	user := newUser(uuid.New())
	in := map[string]any{
		"id": uuid.NewString(), "name": "R",
		"_children": map[string]any{"tiers": []any{map[string]any{"min_amount": 1.0, "percent": 2.0}}},
	}
	orig := cloneMap(in) // lo que openConstraintApproval guarda como payload
	if _, err := svc.Create(context.Background(), "IcRule", user, in); err != nil {
		t.Fatal(err)
	}
	if _, ok := orig["_children"]; !ok {
		t.Fatal("the parked input lost _children")
	}
	// Create no tocó las filas anidadas del input original: un replay las ve intactas.
	row := orig["_children"].(map[string]any)["tiers"].([]any)[0].(map[string]any)
	if _, has := row["rule_id"]; has || len(row) != 2 {
		t.Fatalf("posted child row was mutated: %v", row)
	}
	if n := icCount(t, db, "ic_tiers"); n != 1 {
		t.Fatalf("tiers = %d", n)
	}
	// El replay (mismo camino que ApprovalOpCreate: Create sobre el payload) crea otro padre con su hijo.
	orig["id"] = uuid.NewString()
	if _, err := svc.Create(context.Background(), "IcRule", user, cloneMap(orig)); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if n := icCount(t, db, "ic_tiers"); n != 2 {
		t.Fatalf("tiers after replay = %d", n)
	}
}

// Un create_inline hacia el modelo de OTRO addon (relation.addon) se deriva con
// su dueño y se escribe con el mismo Create del hijo: org, id y foreign_key los
// pone el runtime, y todo es todo-o-nada con el padre.
func TestDeriveInlineRelations_CarriesTheChildAddon(t *testing.T) {
	got := DeriveInlineRelations(icDefs()["IcProduct"])
	if len(got) != 2 || got[0].Name != "prices" || got[0].Addon != "pricing" || got[0].Model != "IcPrice" {
		t.Fatalf("relations = %+v", got)
	}
}

func TestCreate_InlineWritesChildrenOfAnotherAddon(t *testing.T) {
	svc, db := newInlineService(t, true)
	user := newUser(uuid.New())
	id := uuid.NewString()
	if _, err := svc.Create(context.Background(), "IcProduct", user, map[string]any{
		"id": id, "name": "Creatina",
		"_children": map[string]any{
			"prices": []any{
				map[string]any{"market": "mx", "price": 1399.0},
				map[string]any{"market": "us", "price": 69.99},
			},
		},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	rows := icRows(t, db, "ic_prices", "product_id", id)
	if len(rows) != 2 {
		t.Fatalf("prices = %d, want 2 (%v)", len(rows), rows)
	}
	for _, r := range rows {
		if fmt.Sprint(r["organization_id"]) != user.orgID.String() {
			t.Fatalf("a child of another addon lost the org scope: %v", r)
		}
	}
}

func TestCreate_InlineCrossAddonFailureUndoesTheParent(t *testing.T) {
	hooks := NewHookRegistry()
	hooks.RegisterBeforeCreate("IcPrice", func(_ context.Context, _ HookContext, in map[string]any) error {
		if in["market"] == nil || in["market"] == "" {
			return NewValidationError().AddMessage("market", "Elige el mercado").Err()
		}
		return nil
	})
	svc, db := newInlineService(t, true, hooks)
	user := newUser(uuid.New())
	_, err := svc.Create(context.Background(), "IcProduct", user, map[string]any{
		"name": "P",
		"_children": map[string]any{
			"prices": []any{map[string]any{"market": "mx", "price": 1.0}, map[string]any{"price": 2.0}},
		},
	})
	ve := icValidation(t, err)
	if f := ve.Fields["_children.prices.1.market"]; len(f) != 1 {
		t.Fatalf("the other addon's own validation must surface: %+v", ve.Fields)
	}
	for _, table := range []string{"ic_products", "ic_prices"} {
		if n := icCount(t, db, table); n != 0 {
			t.Fatalf("%s kept %d rows after the failed create", table, n)
		}
	}
}

// Si el addon dueño del hijo no está instalado (modelo sin registrar) el error
// es claro y por relación, y el padre no se escribe.
func TestCreate_InlineChildOfMissingAddonIsRefusedBeforeWriting(t *testing.T) {
	svc, db := newInlineService(t, true)
	user := newUser(uuid.New())
	_, err := svc.Create(context.Background(), "IcProduct", user, map[string]any{
		"name": "P",
		"_children": map[string]any{
			"prices": []any{map[string]any{"market": "mx", "price": 1.0}},
			"ghosts": []any{map[string]any{"x": 1.0}},
		},
	})
	ve := icValidation(t, err)
	f := ve.Fields["_children.ghosts"]
	if len(f) != 1 || f[0].Code != CodeInlineChildUnavailable {
		t.Fatalf("errors = %+v", ve.Fields)
	}
	if f[0].Params["addon"] != "ghostaddon" || f[0].Params["model"] != "IcGhost" {
		t.Fatalf("params = %+v", f[0].Params)
	}
	if n := icCount(t, db, "ic_products") + icCount(t, db, "ic_prices"); n != 0 {
		t.Fatalf("left %d rows", n)
	}
}

// Sin `id` en el padre (una tabla sin default de id, como la del arnés) el crear
// con hijos fija el id antes del insert: los hijos apuntan a él y el deshacer
// de un crear fallido sabe qué padre quitar.
func TestCreate_InlineAssignsTheParentIDWhenNoneIsGiven(t *testing.T) {
	svc, db := newInlineService(t, true)
	user := newUser(uuid.New())
	out, err := svc.Create(context.Background(), "IcProduct", user, map[string]any{
		"name":      "Sin id",
		"_children": map[string]any{"prices": []any{map[string]any{"market": "mx", "price": 1.0}}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := fmt.Sprint(out["id"])
	if id == "" || id == "<nil>" {
		t.Fatalf("parent id = %q, want one assigned", id)
	}
	if rows := icRows(t, db, "ic_prices", "product_id", id); len(rows) != 1 {
		t.Fatalf("prices of %s = %d, want 1", id, len(rows))
	}
}

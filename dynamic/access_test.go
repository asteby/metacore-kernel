package dynamic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/asteby/metacore-kernel/permission"
)

// ---------------------------------------------------------------------------
// Models
// ---------------------------------------------------------------------------

// accPromo is a storefront catalogue model: every buyer may read it, only
// "store.admin" may write it. It also validates itself (Validatable).
type accPromo struct {
	modelbase.BaseUUIDModel
	Title    string  `json:"title"`
	Discount float64 `json:"discount"`
}

func (accPromo) TableName() string { return "acc_promos" }
func (accPromo) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{Title: "Promos", Columns: []modelbase.ColumnDef{{Key: "title"}, {Key: "discount"}}}
}
func (accPromo) DefineModal() modelbase.ModalMetadata { return modelbase.ModalMetadata{Title: "Promo"} }
func (accPromo) DefineAccess() modelbase.AccessPolicy {
	return modelbase.AccessPublicReadStaffWrite("store.admin")
}
func (p *accPromo) Validate() map[string]string {
	errs := map[string]string{}
	if p.Discount < 0 || p.Discount > 100 {
		errs["discount"] = "must be between 0 and 100"
	}
	return errs
}

// accOpen declares no policy: the historical behaviour must hold.
type accOpen struct {
	modelbase.BaseUUIDModel
	Title string `json:"title"`
}

func (accOpen) TableName() string                     { return "acc_open" }
func (accOpen) DefineTable() modelbase.TableMetadata  { return modelbase.TableMetadata{Title: "Open"} }
func (accOpen) DefineModal() modelbase.ModalMetadata  { return modelbase.ModalMetadata{Title: "Open"} }

// accSettings is a per-org singleton with defaults.
type accSettings struct {
	modelbase.BaseUUIDModel
	modelbase.SingletonModel
	StoreName string `json:"store_name"`
	Currency  string `json:"currency"`
}

func (accSettings) TableName() string                    { return "acc_settings" }
func (accSettings) DefineTable() modelbase.TableMetadata { return modelbase.TableMetadata{Title: "Settings"} }
func (accSettings) DefineModal() modelbase.ModalMetadata { return modelbase.ModalMetadata{Title: "Settings"} }
func (accSettings) DefineAccess() modelbase.AccessPolicy {
	return modelbase.AccessPublicReadStaffWrite("store.admin")
}
func (accSettings) SingletonDefaults(context.Context) map[string]any {
	return map[string]any{"store_name": "Mi tienda", "currency": "MXN"}
}

// accReport is gated by a capability instead of a role.
type accReport struct {
	modelbase.BaseUUIDModel
	Title string `json:"title"`
}

func (accReport) TableName() string                    { return "acc_reports" }
func (accReport) DefineTable() modelbase.TableMetadata { return modelbase.TableMetadata{Title: "Reports"} }
func (accReport) DefineModal() modelbase.ModalMetadata { return modelbase.ModalMetadata{Title: "Report"} }
func (accReport) DefineAccess() modelbase.AccessPolicy {
	return modelbase.AccessPolicy{Default: modelbase.AccessCapabilities("reports.view")}
}

// accRule declares form rules the metadata validation schema enforces.
type accRule struct {
	modelbase.BaseUUIDModel
	Title string  `json:"title"`
	Stars float64 `json:"stars"`
	Sku   string  `json:"sku"`
}

func (accRule) TableName() string { return "acc_rules" }
func (accRule) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{Title: "Rules", Columns: []modelbase.ColumnDef{
		{Key: "sku", Validation: modelbase.Pattern(`^[A-Z]{3}-\d+$`)},
	}}
}
func (accRule) DefineModal() modelbase.ModalMetadata {
	return modelbase.ModalMetadata{Title: "Rule", Fields: []modelbase.FieldDef{
		{Key: "title", Type: "text", Required: true},
		{Key: "stars", Type: "number", Validation: modelbase.Range(1, 5)},
		{Key: "sku", Type: "text"},
	}}
}

var accTables = []string{"acc_promos", "acc_open", "acc_settings", "acc_reports", "acc_rules"}

var accExtraCols = map[string]string{
	"acc_promos":   "title TEXT, discount REAL",
	"acc_open":     "title TEXT",
	"acc_settings": "store_name TEXT, currency TEXT",
	"acc_reports":  "title TEXT",
	"acc_rules":    "title TEXT, stars REAL, sku TEXT",
}

func registerAccModels() {
	modelbase.Register("acc_promos", func() modelbase.ModelDefiner { return &accPromo{} })
	modelbase.Register("acc_open", func() modelbase.ModelDefiner { return &accOpen{} })
	modelbase.Register("acc_settings", func() modelbase.ModelDefiner { return &accSettings{} })
	modelbase.Register("acc_reports", func() modelbase.ModelDefiner { return &accReport{} })
	modelbase.Register("acc_rules", func() modelbase.ModelDefiner { return &accRule{} })
}

// ---------------------------------------------------------------------------
// Fixture: SQLite always, Postgres when TEST_POSTGRES_DSN is set.
// ---------------------------------------------------------------------------

type accFixture struct {
	svc    *Service
	db     *gorm.DB
	app    *fiber.App
	tables map[string]string // model → physical table
}

func eachAccDialect(t *testing.T, cfg func(*Config), fn func(t *testing.T, fx *accFixture)) {
	t.Run("SQLite", func(t *testing.T) {
		db := setupTestDB(t)
		tables := map[string]string{}
		for _, tb := range accTables {
			mustExec(t, db, fmt.Sprintf(`CREATE TABLE %s (id TEXT PRIMARY KEY, organization_id TEXT, created_by_id TEXT,
				created_at DATETIME, updated_at DATETIME, deleted_at DATETIME, %s)`, tb, accExtraCols[tb]))
			tables[tb] = tb
		}
		fn(t, newAccFixture(t, db, tables, cfg))
	})
	t.Run("Postgres", func(t *testing.T) {
		db, sfx := pgTestDB(t)
		tables := map[string]string{}
		for _, tb := range accTables {
			phys := tb + "_" + sfx
			cols := strings.ReplaceAll(accExtraCols[tb], "REAL", "double precision")
			mustExec(t, db, fmt.Sprintf(`CREATE TABLE %s (id uuid PRIMARY KEY, organization_id uuid, created_by_id uuid,
				created_at timestamptz, updated_at timestamptz, deleted_at timestamptz, %s)`, phys, cols))
			tables[tb] = phys
			t.Cleanup(func() { db.Exec("DROP TABLE IF EXISTS " + phys) })
		}
		fn(t, newAccFixture(t, db, tables, cfg))
	})
}

func newAccFixture(t *testing.T, db *gorm.DB, tables map[string]string, cfg func(*Config)) *accFixture {
	t.Helper()
	registerAccModels()
	c := Config{
		DB:       db,
		Metadata: metadata.New(metadata.Config{CacheTTL: -1}),
		TableNameResolver: func(_ context.Context, model string) (string, bool) {
			tb, ok := tables[model]
			return tb, ok
		},
	}
	if cfg != nil {
		cfg(&c)
	}
	svc := New(c)
	app := fiber.New()
	NewHandler(svc, headerUserResolver).Mount(app)
	return &accFixture{svc: svc, db: db, app: app, tables: tables}
}

// headerUserResolver builds the principal from test headers: X-Org, X-Role and
// X-Extra-Roles (comma-separated platform roles, like host.RoleResolver).
func headerUserResolver(c fiber.Ctx) modelbase.AuthUser {
	org, err := uuid.Parse(c.Get("X-Org"))
	if err != nil {
		return nil
	}
	uid, err := uuid.Parse(c.Get("X-User"))
	if err != nil {
		uid = uuid.New()
	}
	u := &fakeUser{id: uid, orgID: org, role: c.Get("X-Role")}
	extra := c.Get("X-Extra-Roles")
	if extra == "" {
		return u
	}
	return modelbase.WithRoles(u, func() []string { return strings.Split(extra, ",") })
}

type accCaller struct {
	org   uuid.UUID
	user  uuid.UUID
	role  string
	extra string
}

func (fx *accFixture) do(t *testing.T, who accCaller, method, path, body string) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Org", who.org.String())
	if who.user != uuid.Nil {
		req.Header.Set("X-User", who.user.String())
	}
	req.Header.Set("X-Role", who.role)
	req.Header.Set("X-Extra-Roles", who.extra)
	resp, err := fx.app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var env map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("%s %s: bad json %q", method, path, raw)
		}
	}
	return resp.StatusCode, env
}

func dataID(t *testing.T, env map[string]any) string {
	t.Helper()
	d, _ := env["data"].(map[string]any)
	id, _ := d["id"].(string)
	if id == "" {
		t.Fatalf("no data.id in %v", env)
	}
	return id
}

// ---------------------------------------------------------------------------
// 1. Access policy
// ---------------------------------------------------------------------------

func TestAccessPolicy_HandlerEnforcesPerAction(t *testing.T) {
	eachAccDialect(t, nil, func(t *testing.T, fx *accFixture) {
		store := uuid.New()
		// A buyer is "owner" of an auto-provisioned org — exactly the case
		// that let buyers write catalogue models before policies existed.
		buyer := accCaller{org: store, role: "owner"}
		admin := accCaller{org: store, role: "owner", extra: "store.admin"}

		st, env := fx.do(t, buyer, "POST", "/dynamic/acc_promos", `{"title":"2x1","discount":10}`)
		if st != fiber.StatusForbidden {
			t.Fatalf("buyer create: status %d, want 403 (%v)", st, env)
		}
		if env["code"] != "access_denied" || env["action"] != "create" || env["model"] != "acc_promos" {
			t.Fatalf("buyer create: body %v", env)
		}

		st, env = fx.do(t, admin, "POST", "/dynamic/acc_promos", `{"title":"2x1","discount":10}`)
		if st != fiber.StatusCreated {
			t.Fatalf("admin create: status %d (%v)", st, env)
		}
		id := dataID(t, env)

		if st, env = fx.do(t, buyer, "GET", "/dynamic/acc_promos", ""); st != fiber.StatusOK {
			t.Fatalf("buyer list: status %d (%v)", st, env)
		}
		if items, _ := env["data"].([]any); len(items) != 1 {
			t.Fatalf("buyer list: %v", env["data"])
		}
		if st, _ = fx.do(t, buyer, "GET", "/dynamic/acc_promos/"+id, ""); st != fiber.StatusOK {
			t.Fatalf("buyer get: status %d", st)
		}
		if st, _ = fx.do(t, buyer, "PUT", "/dynamic/acc_promos/"+id, `{"title":"x"}`); st != fiber.StatusForbidden {
			t.Fatalf("buyer update: status %d, want 403", st)
		}
		if st, _ = fx.do(t, buyer, "DELETE", "/dynamic/acc_promos/"+id, ""); st != fiber.StatusForbidden {
			t.Fatalf("buyer delete: status %d, want 403", st)
		}
		if st, env = fx.do(t, admin, "PUT", "/dynamic/acc_promos/"+id, `{"title":"3x2"}`); st != fiber.StatusOK {
			t.Fatalf("admin update: status %d (%v)", st, env)
		}
		if st, _ = fx.do(t, admin, "DELETE", "/dynamic/acc_promos/"+id, ""); st != fiber.StatusOK {
			t.Fatalf("admin delete: status %d", st)
		}

		// No policy → historical behaviour: any authenticated user writes.
		if st, env = fx.do(t, buyer, "POST", "/dynamic/acc_open", `{"title":"free"}`); st != fiber.StatusCreated {
			t.Fatalf("open model create: status %d (%v)", st, env)
		}
	})
}

func TestAccessPolicy_RoleResolverViaActorRoles(t *testing.T) {
	// ActorRolesResolver (the approvals role source) also feeds the policy.
	eachAccDialect(t, func(c *Config) {
		c.ActorRolesResolver = func(_ context.Context, u modelbase.AuthUser) []string {
			if u.GetRole() == "staff" {
				return []string{"STORE.ADMIN"} // case-insensitive
			}
			return nil
		}
	}, func(t *testing.T, fx *accFixture) {
		org := uuid.New()
		if st, env := fx.do(t, accCaller{org: org, role: "staff"}, "POST", "/dynamic/acc_promos", `{"title":"a"}`); st != fiber.StatusCreated {
			t.Fatalf("staff create: %d %v", st, env)
		}
		if st, _ := fx.do(t, accCaller{org: org, role: "owner"}, "POST", "/dynamic/acc_promos", `{"title":"a"}`); st != fiber.StatusForbidden {
			t.Fatalf("owner create: %d, want 403", st)
		}
	})
}

func TestAccessPolicy_OverrideAndResolver(t *testing.T) {
	// A host resolver wins over the model's DefineAccess.
	eachAccDialect(t, func(c *Config) {
		c.AccessPolicyResolver = func(_ context.Context, model string) (modelbase.AccessPolicy, bool) {
			if model == "acc_open" {
				return modelbase.AccessReadOnly(), true
			}
			return modelbase.AccessPolicy{}, false
		}
	}, func(t *testing.T, fx *accFixture) {
		who := accCaller{org: uuid.New(), role: "owner", extra: "store.admin"}
		if st, _ := fx.do(t, who, "POST", "/dynamic/acc_open", `{"title":"a"}`); st != fiber.StatusForbidden {
			t.Fatalf("read-only model create: %d, want 403", st)
		}
		if st, _ := fx.do(t, who, "GET", "/dynamic/acc_open", ""); st != fiber.StatusOK {
			t.Fatalf("read-only model list: %d, want 200", st)
		}
	})
}

func TestAccessPolicy_Capabilities(t *testing.T) {
	agent := uuid.New()
	store := permission.NewInMemoryStore(map[permission.Role][]permission.Capability{
		"analyst": {"reports.view", "acc_reports.read", "acc_reports.create"},
		"clerk":   {"acc_reports.read", "acc_reports.create"},
	}, nil)
	eachAccDialect(t, func(c *Config) {
		c.Permissions = permission.New(permission.Config{Store: store, CacheTTL: -1, SuperRoles: []permission.Role{}})
	}, func(t *testing.T, fx *accFixture) {
		org := uuid.New()
		if st, env := fx.do(t, accCaller{org: org, user: agent, role: "analyst"}, "POST", "/dynamic/acc_reports", `{"title":"q3"}`); st != fiber.StatusCreated {
			t.Fatalf("analyst create: %d %v", st, env)
		}
		st, env := fx.do(t, accCaller{org: org, role: "clerk"}, "GET", "/dynamic/acc_reports", "")
		if st != fiber.StatusForbidden || env["code"] != "access_denied" {
			t.Fatalf("clerk list: %d %v, want 403 access_denied", st, env)
		}
	})
}

func TestAccessPolicy_SystemCallerBypassesAndCan(t *testing.T) {
	eachAccDialect(t, nil, func(t *testing.T, fx *accFixture) {
		org := uuid.New()
		ctx := context.Background()
		if _, err := fx.svc.Create(ctx, "acc_promos", NewSystemCaller(org), map[string]any{"title": "seed"}); err != nil {
			t.Fatalf("system caller create: %v", err)
		}
		buyer := &fakeUser{id: uuid.New(), orgID: org, role: "owner"}
		if fx.svc.Can(ctx, buyer, "acc_promos", modelbase.AccessCreate) {
			t.Fatal("buyer must not be able to create")
		}
		if !fx.svc.Can(ctx, buyer, "acc_promos", modelbase.AccessList) {
			t.Fatal("buyer must be able to list")
		}
		admin := modelbase.WithRoles(buyer, func() []string { return []string{"store.admin"} })
		if !fx.svc.Can(ctx, admin, "acc_promos", modelbase.AccessDelete) {
			t.Fatal("admin must be able to delete")
		}
		var ade *AccessDeniedError
		_, err := fx.svc.Create(ctx, "acc_promos", buyer, map[string]any{"title": "x"})
		if !errors.As(err, &ade) || !errors.Is(err, ErrForbidden) {
			t.Fatalf("service create: %v, want AccessDeniedError wrapping ErrForbidden", err)
		}
	})
}

// ---------------------------------------------------------------------------
// 2. Singleton
// ---------------------------------------------------------------------------

func TestSingleton_CurrentCreateConflictAndSave(t *testing.T) {
	eachAccDialect(t, nil, func(t *testing.T, fx *accFixture) {
		org := uuid.New()
		admin := accCaller{org: org, role: "owner", extra: "store.admin"}

		st, env := fx.do(t, admin, "GET", "/dynamic/acc_settings/current", "")
		if st != fiber.StatusOK {
			t.Fatalf("current: %d %v", st, env)
		}
		meta, _ := env["meta"].(map[string]any)
		if meta["persisted"] != true || meta["singleton"] != true {
			t.Fatalf("current meta: %v", meta)
		}
		d, _ := env["data"].(map[string]any)
		if d["currency"] != "MXN" || d["store_name"] != "Mi tienda" {
			t.Fatalf("defaults not applied: %v", d)
		}
		id := dataID(t, env)

		// Reading again returns the same row, never a second one.
		_, env = fx.do(t, admin, "GET", "/dynamic/acc_settings/current", "")
		if dataID(t, env) != id {
			t.Fatal("current must be stable")
		}

		st, env = fx.do(t, admin, "POST", "/dynamic/acc_settings", `{"store_name":"otra"}`)
		if st != fiber.StatusConflict || env["code"] != "singleton_exists" {
			t.Fatalf("second create: %d %v, want 409 singleton_exists", st, env)
		}
		if dd, _ := env["data"].(map[string]any); dd["id"] != id {
			t.Fatalf("409 must carry the existing id: %v", env)
		}

		st, env = fx.do(t, admin, "PUT", "/dynamic/acc_settings/current", `{"store_name":"7 Leguas"}`)
		if st != fiber.StatusOK {
			t.Fatalf("save current: %d %v", st, env)
		}
		if d, _ := env["data"].(map[string]any); d["store_name"] != "7 Leguas" || d["id"] != id {
			t.Fatalf("save current: %v", env["data"])
		}

		// Another org's buyer (may read, may not create): unsaved defaults.
		other := accCaller{org: uuid.New(), role: "owner"}
		st, env = fx.do(t, other, "GET", "/dynamic/acc_settings/current", "")
		if st != fiber.StatusOK {
			t.Fatalf("buyer current: %d %v", st, env)
		}
		if m, _ := env["meta"].(map[string]any); m["persisted"] != false {
			t.Fatalf("buyer current must not persist: %v", env)
		}
		var n int64
		fx.db.Table(fx.tables["acc_settings"]).Where("organization_id = ?", other.org).Count(&n)
		if n != 0 {
			t.Fatalf("buyer read created %d rows", n)
		}
		if st, _ = fx.do(t, other, "PUT", "/dynamic/acc_settings/current", `{"store_name":"x"}`); st != fiber.StatusForbidden {
			t.Fatalf("buyer save current: %d, want 403", st)
		}

		// A non-singleton model has no /current.
		if st, _ = fx.do(t, admin, "GET", "/dynamic/acc_promos/current", ""); st != fiber.StatusNotFound {
			t.Fatalf("non-singleton current: %d, want 404", st)
		}
	})
}

func TestSingleton_MetadataFlag(t *testing.T) {
	registerAccModels()
	meta := metadata.New(metadata.Config{CacheTTL: -1})
	tbl, err := meta.GetTable(context.Background(), "acc_settings")
	if err != nil || !tbl.Singleton {
		t.Fatalf("acc_settings table metadata must carry singleton: %v %+v", err, tbl)
	}
	raw, _ := json.Marshal(tbl)
	if !strings.Contains(string(raw), `"singleton":true`) {
		t.Fatalf("wire payload: %s", raw)
	}
	tbl, _ = meta.GetTable(context.Background(), "acc_promos")
	if tbl.Singleton {
		t.Fatal("acc_promos is not a singleton")
	}
	modelbase.MarkSingleton("acc_marked")
	if !modelbase.IsSingletonModel("acc_marked", &accOpen{}) {
		t.Fatal("MarkSingleton must flag the key")
	}
}

// ---------------------------------------------------------------------------
// 3. Field validation
// ---------------------------------------------------------------------------

func fieldErrs(t *testing.T, env map[string]any, field string) []map[string]any {
	t.Helper()
	errs, _ := env["errors"].(map[string]any)
	list, _ := errs[field].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		m, _ := e.(map[string]any)
		out = append(out, m)
	}
	return out
}

func TestValidation_GoModelValidateMapsTo422(t *testing.T) {
	eachAccDialect(t, nil, func(t *testing.T, fx *accFixture) {
		admin := accCaller{org: uuid.New(), role: "owner", extra: "store.admin"}
		st, env := fx.do(t, admin, "POST", "/dynamic/acc_promos", `{"title":"x","discount":150}`)
		if st != fiber.StatusUnprocessableEntity {
			t.Fatalf("create: %d %v, want 422", st, env)
		}
		fe := fieldErrs(t, env, "discount")
		if len(fe) != 1 || fe[0]["code"] != CodeInvalid || fe[0]["message"] != "must be between 0 and 100" {
			t.Fatalf("errors: %v", env["errors"])
		}

		// Update validates the merged row.
		_, env = fx.do(t, admin, "POST", "/dynamic/acc_promos", `{"title":"ok","discount":5}`)
		id := dataID(t, env)
		st, env = fx.do(t, admin, "PUT", "/dynamic/acc_promos/"+id, `{"discount":-1}`)
		if st != fiber.StatusUnprocessableEntity || len(fieldErrs(t, env, "discount")) != 1 {
			t.Fatalf("update: %d %v, want 422", st, env)
		}
	})
}

func TestValidation_HooksReturnFieldErrors(t *testing.T) {
	hooks := NewHookRegistry()
	hooks.RegisterBeforeCreate("acc_open", func(_ context.Context, _ HookContext, in map[string]any) error {
		if in["title"] == "bad" {
			return modelbase.FieldErrors{"title": "not allowed"}
		}
		if in["title"] == "worse" {
			return NewValidationError().AddMessage("title", "nope").Add("title", "min", map[string]any{"min": 3}).Err()
		}
		return nil
	})
	eachAccDialect(t, func(c *Config) { c.Hooks = hooks }, func(t *testing.T, fx *accFixture) {
		who := accCaller{org: uuid.New(), role: "owner"}
		st, env := fx.do(t, who, "POST", "/dynamic/acc_open", `{"title":"bad"}`)
		if st != fiber.StatusUnprocessableEntity || fieldErrs(t, env, "title")[0]["message"] != "not allowed" {
			t.Fatalf("FieldErrors hook: %d %v", st, env)
		}
		st, env = fx.do(t, who, "POST", "/dynamic/acc_open", `{"title":"worse"}`)
		if st != fiber.StatusUnprocessableEntity || len(fieldErrs(t, env, "title")) != 2 {
			t.Fatalf("ValidationError hook: %d %v", st, env)
		}
		if st, _ = fx.do(t, who, "POST", "/dynamic/acc_open", `{"title":"fine"}`); st != fiber.StatusCreated {
			t.Fatalf("clean create: %d", st)
		}
	})
}

func TestValidation_MetadataSchema(t *testing.T) {
	eachAccDialect(t, func(c *Config) {
		c.ValidationSchemaResolver = MetadataValidationSchema(c.Metadata)
	}, func(t *testing.T, fx *accFixture) {
		who := accCaller{org: uuid.New(), role: "owner"}
		st, env := fx.do(t, who, "POST", "/dynamic/acc_rules", `{"stars":9,"sku":"abc"}`)
		if st != fiber.StatusUnprocessableEntity {
			t.Fatalf("create: %d %v, want 422", st, env)
		}
		if fe := fieldErrs(t, env, "title"); len(fe) != 1 || fe[0]["code"] != "required" {
			t.Fatalf("title: %v", env["errors"])
		}
		if fe := fieldErrs(t, env, "stars"); len(fe) != 1 || fe[0]["code"] != "max" {
			t.Fatalf("stars: %v", env["errors"])
		}
		if fe := fieldErrs(t, env, "sku"); len(fe) != 1 {
			t.Fatalf("sku (ColumnDef pattern fallback): %v", env["errors"])
		}
		if st, env = fx.do(t, who, "POST", "/dynamic/acc_rules", `{"title":"ok","stars":4,"sku":"ABC-12"}`); st != fiber.StatusCreated {
			t.Fatalf("valid create: %d %v", st, env)
		}
	})
}

func TestValidationErrorPublicAPI(t *testing.T) {
	if NewValidationError().Err() != nil {
		t.Fatal("empty accumulator must be a nil error")
	}
	ve := NewValidationError().Add("a", "required", nil).AddMessage("b", "bad")
	err := ve.Err()
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("must wrap ErrValidation: %v", err)
	}
	if ve.Fields["b"][0].Code != CodeInvalid || ve.Fields["b"][0].Message != "bad" {
		t.Fatalf("AddMessage: %+v", ve.Fields)
	}
	raw, _ := json.Marshal(ve)
	if !strings.Contains(string(raw), `"message":"bad"`) || strings.Contains(string(raw), `"message":""`) {
		t.Fatalf("wire: %s", raw)
	}
}

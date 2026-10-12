package agent

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type fakePerms struct{ deny map[string]bool }

func (f fakePerms) Can(_, _ uuid.UUID, model, action string) bool { return !f.deny[model+"."+action] }

type fakeGate struct {
	owners  map[string]string
	enabled map[string]bool
	mods    []ModuleInfo
}

func (f fakeGate) IsEnabled(_ uuid.UUID, key string) bool { return f.enabled[key] }
func (f fakeGate) OwnerOf(model string) string            { return f.owners[model] }
func (f fakeGate) Enabled(uuid.UUID) ([]ModuleInfo, error) {
	return f.mods, nil
}

func newCRUD(t *testing.T, mut func(*CRUDConfig)) (*CRUDService, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE customers (id text PRIMARY KEY, organization_id text, name text, email text, token text, created_at datetime DEFAULT CURRENT_TIMESTAMP, deleted_at datetime)`).Error; err != nil {
		t.Fatal(err)
	}
	cfg := CRUDConfig{
		DB:          db,
		Catalog:     testCatalog(),
		Permissions: fakePerms{deny: map[string]bool{}},
	}
	if mut != nil {
		mut(&cfg)
	}
	s, err := NewCRUDService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s, db
}

func TestNewCRUDServiceRequiresPorts(t *testing.T) {
	if _, err := NewCRUDService(CRUDConfig{Catalog: testCatalog()}); err != ErrNoPermissionChecker {
		t.Fatalf("got %v", err)
	}
	if _, err := NewCRUDService(CRUDConfig{Permissions: fakePerms{}}); err == nil {
		t.Fatal("catalog must be required")
	}
	if _, err := NewCRUDService(CRUDConfig{Catalog: testCatalog(), DevAllowAll: true}); err != nil {
		t.Fatal(err)
	}
}

func TestCheckPermissionGateThenPermission(t *testing.T) {
	org := uuid.New()
	gate := fakeGate{owners: map[string]string{"customers": "crm"}, enabled: map[string]bool{}}
	s, _ := newCRUD(t, func(c *CRUDConfig) {
		c.Gate = gate
		c.Permissions = fakePerms{deny: map[string]bool{"stock.index": true}}
	})
	id := &AgentIdentity{OrganizationID: org, UserID: uuid.New()}

	if err := s.CheckPermission(id, "customers", "index"); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("module not enabled must block, got %v", err)
	}
	gate.enabled["crm"] = true
	if err := s.CheckPermission(id, "customers", "index"); err != nil {
		t.Fatalf("enabled module + permission must pass: %v", err)
	}
	if err := s.CheckPermission(id, "stock", "index"); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("denied permission must block, got %v", err)
	}
}

func TestListModelsFiltersByPermissionAndDedupes(t *testing.T) {
	s, _ := newCRUD(t, func(c *CRUDConfig) { c.Permissions = fakePerms{deny: map[string]bool{"sales_orders.index": true}} })
	list, err := s.ListModels(&AgentIdentity{OrganizationID: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range list {
		got[e["model"].(string)], _ = e["title"].(string)
	}
	if len(got) != 2 || got["stock"] != "Existencias" || got["customers"] != "Clientes" {
		t.Fatalf("got %v", got)
	}
}

func TestQueryIsOrgScopedAndStripsSensitive(t *testing.T) {
	s, db := newCRUD(t, nil)
	a, b := uuid.New(), uuid.New()
	db.Exec(`INSERT INTO customers (id, organization_id, name, token) VALUES (?, ?, 'Ana', 'secret'), (?, ?, 'Beto', 'x')`, uuid.NewString(), a.String(), uuid.NewString(), b.String())
	res, err := s.Query(&AgentIdentity{OrganizationID: a}, QueryRequest{Model: "customer"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 1 || res.Records[0]["name"] != "Ana" {
		t.Fatalf("org scoping broken: %+v", res)
	}
	if _, ok := res.Records[0]["token"]; ok {
		t.Fatal("sensitive column leaked")
	}
	if _, err := s.Query(&AgentIdentity{OrganizationID: a}, QueryRequest{Model: "inventory.model.nothing"}); err == nil {
		t.Fatal("dotted unknown model must be rejected")
	}
	// invalid filter / order keys are ignored, not interpolated
	if _, err := s.Query(&AgentIdentity{OrganizationID: a}, QueryRequest{Model: "customers", Filters: map[string]any{"name; DROP TABLE customers": 1}, OrderBy: "name; DROP TABLE customers"}); err != nil {
		t.Fatal(err)
	}
	var n int64
	db.Table("customers").Count(&n)
	if n != 2 {
		t.Fatal("table was tampered with")
	}
}

func TestCreateUpdateDeleteScopedAndSanitized(t *testing.T) {
	s, db := newCRUD(t, nil)
	org, other := uuid.New(), uuid.New()
	id := &AgentIdentity{OrganizationID: org, UserID: uuid.New()}

	rec, err := s.Create(id, MutateRequest{Model: "customers", Data: map[string]any{
		"name": "Ana", "organization_id": other, "id": "hijack", "rel": map[string]any{"x": 1}, "gone": nil,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if rec["organization_id"] != org {
		t.Fatalf("caller must not choose the org: %v", rec["organization_id"])
	}
	rid := rec["id"].(uuid.UUID).String()

	if err := s.Update(&AgentIdentity{OrganizationID: other}, MutateRequest{Model: "customers", RecordID: rid, Data: map[string]any{"name": "Evil"}}); err == nil {
		t.Fatal("cross-org update must fail")
	}
	if err := s.Update(id, MutateRequest{Model: "customers", RecordID: rid, Data: map[string]any{"name": "Ana B", "deleted_at": "x"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(id, MutateRequest{Model: "customers", RecordID: rid, Data: map[string]any{"id": "z"}}); err == nil {
		t.Fatal("only system columns => nothing to update")
	}
	got, err := s.GetRecord(id, "customers", rid)
	if err != nil || got["name"] != "Ana B" {
		t.Fatalf("%v %v", got, err)
	}
	if err := s.Delete(&AgentIdentity{OrganizationID: other}, MutateRequest{Model: "customers", RecordID: rid}); err == nil {
		t.Fatal("cross-org delete must fail")
	}
	if err := s.Delete(id, MutateRequest{Model: "customers", RecordID: rid}); err != nil {
		t.Fatal(err)
	}
	var n int64
	db.Table("customers").Where("deleted_at IS NULL").Count(&n)
	if n != 0 {
		t.Fatal("soft delete did not hide the row")
	}
	if _, err := s.GetRecord(id, "customers", rid); err == nil {
		t.Fatal("deleted record must not load")
	}
}

func TestMutationPermissionDenied(t *testing.T) {
	s, _ := newCRUD(t, func(c *CRUDConfig) {
		c.Permissions = fakePerms{deny: map[string]bool{"customers.create": true, "customers.update": true, "customers.delete": true}}
	})
	id := &AgentIdentity{OrganizationID: uuid.New()}
	if _, err := s.Create(id, MutateRequest{Model: "customers", Data: map[string]any{"name": "x"}}); err == nil {
		t.Fatal("create must be denied")
	}
	if err := s.Update(id, MutateRequest{Model: "customers", RecordID: "1", Data: map[string]any{"name": "x"}}); err == nil {
		t.Fatal("update must be denied")
	}
	if err := s.Delete(id, MutateRequest{Model: "customers", RecordID: "1"}); err == nil {
		t.Fatal("delete must be denied")
	}
}

func TestAggregateValidation(t *testing.T) {
	s, db := newCRUD(t, nil)
	org := uuid.New()
	db.Exec(`INSERT INTO customers (id, organization_id, name) VALUES (?, ?, 'A'), (?, ?, 'B')`, uuid.NewString(), org.String(), uuid.NewString(), org.String())
	id := &AgentIdentity{OrganizationID: org}
	res, err := s.Aggregate(id, AggregateRequest{Model: "customers", Function: "count"})
	if err != nil {
		t.Fatal(err)
	}
	v := res.(map[string]any)["value"]
	if p, ok := v.(*any); ok { // some drivers scan into pointers
		v = *p
	}
	if v != int64(2) {
		t.Fatalf("count=%v", v)
	}
	if _, err := s.Aggregate(id, AggregateRequest{Model: "customers", Function: "sum"}); err == nil {
		t.Fatal("sum needs a field")
	}
	if _, err := s.Aggregate(id, AggregateRequest{Model: "customers", Function: "sum", Field: "x); DROP"}); err == nil {
		t.Fatal("bad field must be rejected")
	}
	if _, err := s.Aggregate(id, AggregateRequest{Model: "customers", Function: "median"}); err == nil {
		t.Fatal("unsupported function")
	}
}

func TestNormalizeValueHookAndJSONColumns(t *testing.T) {
	cat := testCatalog()
	cat.models[1].JSONColumns = []string{"meta"}
	cat.models[1].GeneratedColumns = []string{"available"}
	s, _ := newCRUD(t, func(c *CRUDConfig) {
		c.Catalog = cat
		c.NormalizeValue = func(_, key string, v any) any {
			if key == "name" {
				return strings.ToUpper(v.(string))
			}
			return v
		}
	})
	out := s.sanitizeMutationData("customers", map[string]any{
		"name": "ana", "available": 3, "meta": []any{1}, "lines": []any{1}, "created_at": "x",
	}, "update")
	if out["name"] != "ANA" {
		t.Fatalf("hook not applied: %v", out)
	}
	if _, ok := out["available"]; ok {
		t.Fatal("generated column must be dropped")
	}
	if _, ok := out["meta"]; !ok {
		t.Fatal("jsonb slice must survive update")
	}
	if _, ok := out["lines"]; ok {
		t.Fatal("non-json slice must be dropped on update")
	}
	if _, ok := out["created_at"]; ok {
		t.Fatal("system column must be dropped")
	}
	enc := map[string]any{"meta": []any{1}, "lines": []any{2}, "name": "x", "raw": "already"}
	encodeJSONColumns(enc, map[string]struct{}{"meta": {}, "raw": {}})
	if _, isStr := enc["meta"].(string); isStr {
		t.Fatal("meta should be wrapped in an expression")
	}
	if enc["name"] != "x" || enc["raw"] != "already" {
		t.Fatal("strings must be left alone")
	}
}

func TestCapabilityRegistry(t *testing.T) {
	org, user := uuid.New(), uuid.New()
	gate := fakeGate{
		owners:  map[string]string{"customers": "crm"},
		enabled: map[string]bool{"crm": true},
		mods:    []ModuleInfo{{Key: "crm", Name: "CRM"}},
	}
	s, _ := newCRUD(t, func(c *CRUDConfig) {
		c.Gate = gate
		c.Permissions = fakePerms{deny: map[string]bool{"customers.delete": true, "stock.index": true, "sales_orders.index": true}}
	})
	reg := &CapabilityRegistry{
		CRUD: s, Gate: gate,
		Declared: declaredFake{caps: []CapabilityDescriptor{
			{ID: "crm.guide.x", Owner: "crm", Title: "T", Description: "D", Kind: "guide", Risk: "low"},
			{ID: "off.guide", Owner: "off", Title: "T", Description: "D", Kind: "guide", Risk: "low"},
			{ID: "crm.bad.kind", Owner: "crm", Title: "T", Description: "D", Kind: "nope", Risk: "low"},
			{ID: "crm.tx.noconfirm", Owner: "crm", Title: "T", Description: "D", Kind: "transact", Risk: "low"},
			{ID: "crm.perm", Owner: "crm", Title: "T", Description: "D", Kind: "act", Risk: "low", Permission: "crm.run"},
		}},
	}
	caps, err := reg.List(org, user)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]CapabilityDescriptor{}
	for _, c := range caps {
		ids[c.ID] = c
	}
	for _, want := range []string{"crm.customers.index", "crm.customers.create", "crm.customers.update", "crm.guide.x"} {
		if _, ok := ids[want]; !ok {
			t.Errorf("missing %s in %v", want, keys(ids))
		}
	}
	for _, bad := range []string{"crm.customers.delete", "off.guide", "crm.bad.kind", "crm.tx.noconfirm", "crm.perm"} {
		if _, ok := ids[bad]; ok {
			t.Errorf("%s must be filtered out", bad)
		}
	}
	if !ids["crm.customers.create"].RequiresConfirmation || ids["crm.customers.index"].RequiresConfirmation {
		t.Error("confirmation flags wrong")
	}
	reg.Named = namedFake{"crm.run": true}
	caps, _ = reg.List(org, user)
	found := false
	for _, c := range caps {
		found = found || c.ID == "crm.perm"
	}
	if !found {
		t.Error("named permission granted but capability missing")
	}
	clause := reg.PromptClause(org, user)
	if !strings.Contains(clause, "crm.customers.create") || !strings.Contains(clause, `"confirm":true`) {
		t.Errorf("clause: %s", clause)
	}
	if (&CapabilityRegistry{}).PromptClause(org, user) != "" {
		t.Error("empty registry must render nothing")
	}
}

func keys(m map[string]CapabilityDescriptor) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

type declaredFake struct{ caps []CapabilityDescriptor }

func (d declaredFake) DeclaredCapabilities(uuid.UUID) ([]CapabilityDescriptor, error) {
	return d.caps, nil
}

type namedFake map[string]bool

func (n namedFake) HasPermission(_, _ uuid.UUID, p string) (bool, error) { return n[p], nil }

func TestInstalledModulesContextAndCatalogContext(t *testing.T) {
	org := uuid.New()
	gate := fakeGate{mods: []ModuleInfo{{Key: "crm", Name: "CRM", Description: "clientes", Capabilities: []string{"Clientes"}}, {Key: "k"}}}
	got := InstalledModulesContext(gate, org)
	for _, w := range []string{"**CRM**: clientes", "(entidades: Clientes)", "**k**"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in %q", w, got)
		}
	}
	if InstalledModulesContext(nil, org) != "" || InstalledModulesContext(fakeGate{}, org) != "" {
		t.Error("no modules => empty")
	}
	cc := CatalogContext{Gate: gate, Sections: []PromptSection{PromptSectionFunc(func(uuid.UUID) string { return "\nEXTRA" }), nil}}
	if !strings.HasSuffix(cc.Build(org), "\nEXTRA") {
		t.Error("host sections must follow modules")
	}
	if !strings.Contains(CurrencyClause("Peso", "$"), "Peso ($)") || !strings.Contains(CurrencyClause("", ""), "Dólar") {
		t.Error("currency clause")
	}
}

package diag

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type apiFixture struct {
	app  *fiber.App
	st   *Store
	rec  *Recorder
	orgA uuid.UUID
	orgB uuid.UUID
	ga   Group
	gb   Group
}

func newAPI(t *testing.T, db *gorm.DB, super bool) *apiFixture {
	t.Helper()
	rec, st := mkRec(t, db, nil, Options{})
	f := &apiFixture{st: st, rec: rec, orgA: uuid.New(), orgB: uuid.New()}
	ctx := context.Background()
	rec.Record(failure(f.orgA, func(d *Diagnostic) {
		d.Breadcrumbs = []string{"step one"}
		d.Subject = Subject{Type: "payment", ID: "p1"}
	}), nil)
	rec.Record(failure(f.orgB), nil)
	rec.Flush(ctx)
	ga, _, _ := st.List(ctx, Filter{Org: f.orgA})
	gb, _, _ := st.List(ctx, Filter{Org: f.orgB})
	f.ga, f.gb = ga[0], gb[0]

	h := NewHandler(st, HandlerOptions{
		IsSuperAdmin:  func(fiber.Ctx) bool { return super },
		KernelVersion: "v9.9.9",
		Versions:      func(fiber.Ctx) map[string]string { return map[string]string{"acme": "1.2.3"} },
		Manage: []fiber.Handler{func(c fiber.Ctx) error { // PermManage de pruebas
			if c.Get("X-Test-Manage") != "1" {
				return c.SendStatus(403)
			}
			return c.Next()
		}},
	})
	f.app = fiber.New()
	authed := func(c fiber.Ctx) error {
		if c.Get("X-Test-Org") == "" {
			return c.SendStatus(401)
		}
		o, _ := uuid.Parse(c.Get("X-Test-Org"))
		c.Locals("organization_id", o)
		return c.Next()
	}
	if err := h.Mount(f.app.Group("/api/diagnostics"), authed); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *apiFixture) do(t *testing.T, method, path string, org uuid.UUID) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if org != uuid.Nil {
		req.Header.Set("X-Test-Org", org.String())
	}
	req.Header.Set("X-Test-Manage", "1")
	res, err := f.app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return res.StatusCode, out
}

func TestHandler_MountFailsClosedWithoutMiddleware(t *testing.T) {
	h := NewHandler(NewStore(nil), HandlerOptions{})
	if err := h.Mount(fiber.New()); err == nil {
		t.Fatal("Mount without an access middleware must fail")
	}
	if err := h.Mount(fiber.New(), nil); err == nil {
		t.Fatal("nil middleware is no middleware")
	}
}

func TestHandler_API(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		f := newAPI(t, db, false)
		base := "/api/diagnostics"

		if code, _ := f.do(t, "GET", base+"/issues", uuid.Nil); code != 401 {
			t.Fatalf("unauthenticated: %d", code)
		}
		code, out := f.do(t, "GET", base+"/issues?addon=acme&severity=error&status=open&since=2000-01-01T00:00:00Z", f.orgA)
		data, _ := out["data"].([]any)
		if code != 200 || len(data) != 1 || out["success"] != true {
			t.Fatalf("list: %d %v", code, out)
		}
		if code, _ := f.do(t, "GET", base+"/issues?since=ayer", f.orgA); code != 400 {
			t.Fatalf("bad since: %d", code)
		}
		code, out = f.do(t, "GET", base+"/issues/"+f.ga.ID.String(), f.orgA)
		if code != 200 || !strings.Contains(toJSON(out), `"events"`) || !strings.Contains(toJSON(out), "step one") {
			t.Fatalf("get: %d %v", code, out)
		}
		if code, _ := f.do(t, "GET", base+"/issues/"+f.gb.ID.String(), f.orgA); code != 404 {
			t.Fatalf("org A reading org B's issue: %d", code)
		}
		if code, _ := f.do(t, "GET", base+"/issues/not-a-uuid", f.orgA); code != 400 {
			t.Fatalf("bad id: %d", code)
		}
		code, out = f.do(t, "GET", base+"/events?subject_type=payment&subject_id=p1", f.orgA)
		if ev, _ := out["data"].([]any); code != 200 || len(ev) != 1 {
			t.Fatalf("events: %d %v", code, out)
		}
		if code, _ := f.do(t, "GET", base+"/events", f.orgA); code != 400 {
			t.Fatalf("events without criteria: %d", code)
		}
		// Cruce de orgs: solo superadmin.
		if code, _ := f.do(t, "GET", base+"/issues?org_id="+f.orgB.String(), f.orgA); code != 403 {
			t.Fatalf("cross-org as admin: %d", code)
		}
		if code, _ := f.do(t, "POST", base+"/issues/"+f.gb.ID.String()+"/resolve", f.orgA); code != 404 {
			t.Fatalf("cross-org resolve: %d", code)
		}
		// resolve / mute
		code, out = f.do(t, "POST", base+"/issues/"+f.ga.ID.String()+"/resolve", f.orgA)
		if code != 200 || !strings.Contains(toJSON(out), `"resolved"`) {
			t.Fatalf("resolve: %d %v", code, out)
		}
		code, out = f.do(t, "POST", base+"/issues/"+f.ga.ID.String()+"/mute", f.orgA)
		if code != 200 || !strings.Contains(toJSON(out), `"muted"`) {
			t.Fatalf("mute: %d %v", code, out)
		}
		// export
		req := httptest.NewRequest("GET", base+"/export", nil)
		req.Header.Set("X-Test-Org", f.orgA.String())
		res, _ := f.app.Test(req)
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != 200 || !strings.Contains(res.Header.Get("Content-Disposition"), "attachment") {
			t.Fatalf("export: %d", res.StatusCode)
		}
		var bundle Bundle
		if err := json.Unmarshal(b, &bundle); err != nil {
			t.Fatal(err)
		}
		if bundle.KernelVersion != "v9.9.9" || bundle.Addons["acme"] != "1.2.3" || len(bundle.Issues) != 1 || len(bundle.Events) != 1 || bundle.Org != f.orgA {
			t.Fatalf("bundle=%+v", bundle)
		}
		low := strings.ToLower(string(b))
		for _, bad := range []string{"settings", "secret", "token", "password", f.orgB.String()} {
			if strings.Contains(low, bad) {
				t.Fatalf("bundle mentions %q", bad)
			}
		}
	})
}

func TestHandler_SuperAdminCrossOrg(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		f := newAPI(t, db, true)
		code, out := f.do(t, "GET", "/api/diagnostics/issues?org_id="+f.orgB.String(), f.orgA)
		data, _ := out["data"].([]any)
		if code != 200 || len(data) != 1 || !strings.Contains(toJSON(out), f.gb.ID.String()) {
			t.Fatalf("superadmin cross-org: %d %v", code, out)
		}
	})
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestHandler_ResolveMuteNeedManagePermission(t *testing.T) {
	db := newSQLiteDB(t)
	f := newAPI(t, db, false)
	url := "/api/diagnostics/issues/" + f.ga.ID.String()
	req := httptest.NewRequest("POST", url+"/resolve", nil)
	req.Header.Set("X-Test-Org", f.orgA.String()) // ve (PermView) pero no gestiona
	if res, _ := f.app.Test(req); res.StatusCode != 403 {
		t.Fatalf("view-only resolve: %d", res.StatusCode)
	}
	req = httptest.NewRequest("POST", url+"/mute", nil)
	req.Header.Set("X-Test-Org", f.orgA.String())
	if res, _ := f.app.Test(req); res.StatusCode != 403 {
		t.Fatalf("view-only mute: %d", res.StatusCode)
	}
	if code, _ := f.do(t, "GET", url, f.orgA); code != 200 {
		t.Fatalf("view-only read: %d", code)
	}
	// Sin middlewares Manage las rutas ni existen (falla cerrado).
	h := NewHandler(f.st, HandlerOptions{})
	app := fiber.New()
	pass := func(c fiber.Ctx) error { c.Locals("organization_id", f.orgA); return c.Next() }
	if err := h.Mount(app.Group("/d"), pass); err != nil {
		t.Fatal(err)
	}
	res, _ := app.Test(httptest.NewRequest("POST", "/d/issues/"+f.ga.ID.String()+"/resolve", nil))
	if res.StatusCode != 404 && res.StatusCode != 405 {
		t.Fatalf("resolve mounted without Manage: %d", res.StatusCode)
	}
}

func TestHandler_InternalErrorsAreGeneric500(t *testing.T) {
	db := newSQLiteDB(t)
	f := newAPI(t, db, false)
	sqlDB, _ := db.DB()
	_ = sqlDB.Close() // la base se cae
	req := httptest.NewRequest("GET", "/api/diagnostics/events?correlation_id=c1", nil)
	req.Header.Set("X-Test-Org", f.orgA.String())
	res, _ := f.app.Test(req)
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 500 {
		t.Fatalf("status=%d body=%s", res.StatusCode, b)
	}
	for _, leak := range []string{"sql", "database", "diag:", "closed"} {
		if strings.Contains(strings.ToLower(string(b)), leak) {
			t.Fatalf("internal text leaked (%q): %s", leak, b)
		}
	}
}

func TestBundle_NoNPlusOne(t *testing.T) {
	db := newSQLiteDB(t)
	rec, st := mkRec(t, db, nil, Options{})
	org := uuid.New()
	for i := 0; i < 30; i++ {
		rec.Record(failure(org, func(d *Diagnostic) { d.Code = fmt.Sprintf("code_%d", i) }), nil)
	}
	rec.Flush(context.Background())
	queries := 0
	cb := func(*gorm.DB) { queries++ }
	_ = db.Callback().Query().Before("gorm:query").Register("count_q", cb)
	_ = db.Callback().Row().Before("gorm:row").Register("count_r", cb)
	_ = db.Callback().Raw().Before("gorm:raw").Register("count_raw", cb)
	h := NewHandler(st, HandlerOptions{})
	b, err := h.Bundle(context.Background(), org, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Issues) != 30 || len(b.Events) != 30 {
		t.Fatalf("issues=%d events=%d", len(b.Issues), len(b.Events))
	}
	if queries > 4 {
		t.Fatalf("bundle ran %d queries for 30 groups", queries)
	}
}

func TestSchema_HasDiagRefIndex(t *testing.T) {
	for _, d := range []string{"postgres", "sqlite"} {
		if !strings.Contains(strings.Join(Schema(d), "\n"), "(org_id, diag_ref)") {
			t.Errorf("%s: missing (org_id, diag_ref) index", d)
		}
	}
}

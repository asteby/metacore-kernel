package marketplace_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/asteby/metacore-kernel/auth"
	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/installer"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/marketplace"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

var testOrgID = uuid.MustParse("11111111-1111-1111-1111-111111111111")
var testUserID = uuid.MustParse("22222222-2222-2222-2222-222222222222")
var testOrgID2 = uuid.MustParse("33333333-3333-3333-3333-333333333333")

func setupDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.Exec(`CREATE TABLE marketplace_installations (
		id TEXT PRIMARY KEY,
		organization_id TEXT NOT NULL,
		addon_key TEXT NOT NULL,
		name TEXT,
		category TEXT,
		version TEXT NOT NULL,
		bundle_url TEXT,
		status TEXT NOT NULL DEFAULT 'requested',
		error_message TEXT,
		requested_by_id TEXT NOT NULL,
		requested_at DATETIME,
		completed_at DATETIME
	)`).Error; err != nil {
		t.Fatalf("create schema: %v", err)
	}
	return db
}

func newTestApp(t *testing.T, db *gorm.DB) *fiber.App {
	t.Helper()
	app := fiber.New()
	h, err := marketplace.NewHandler(db)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	authMw := func(c fiber.Ctx) error {
		c.Locals(auth.LocalOrganizationID, testOrgID)
		c.Locals(auth.LocalUserID, testUserID)
		return c.Next()
	}
	h.Mount(app, authMw)
	return app
}

func newTestAppNoAuth(t *testing.T, db *gorm.DB) *fiber.App {
	t.Helper()
	app := fiber.New()
	h, err := marketplace.NewHandler(db)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	h.Mount(app)
	return app
}

func makeInstallReq(body map[string]any) *http.Request {
	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(body)
	req := httptest.NewRequest("POST", "/marketplace/install", &buf)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func readBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("json unmarshal: %v body=%s", err, string(b))
	}
	return out
}

// Tests

func TestInstall_LiteMode_Success(t *testing.T) {
	db := setupDB(t)
	app := newTestApp(t, db)

	req := makeInstallReq(map[string]any{
		"addonKey": "com.example.addon",
		"version":  "1.0.0",
	})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	if resp.StatusCode != 201 {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if body["success"] != true {
		t.Fatalf("expected success=true, got %v", body["success"])
	}
	data := body["data"].(map[string]any)
	if data["addon_key"] != "com.example.addon" {
		t.Fatalf("expected addon_key com.example.addon, got %v", data["addon_key"])
	}
	if data["status"] != "requested" {
		t.Fatalf("expected status requested, got %v", data["status"])
	}
	if data["organization_id"] != testOrgID.String() {
		t.Fatalf("expected org_id %s, got %v", testOrgID.String(), data["organization_id"])
	}
}

func TestInstall_LiteMode_DefaultVersion(t *testing.T) {
	db := setupDB(t)
	app := newTestApp(t, db)

	req := makeInstallReq(map[string]any{
		"addonKey": "com.example.addon",
	})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	if resp.StatusCode != 201 {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	data := body["data"].(map[string]any)
	if data["version"] != "latest" {
		t.Fatalf("expected version latest, got %v", data["version"])
	}
}

func TestInstall_MissingAddonKey(t *testing.T) {
	db := setupDB(t)
	app := newTestApp(t, db)

	req := makeInstallReq(map[string]any{
		"version": "1.0.0",
	})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if body["success"] != false {
		t.Fatalf("expected success=false, got %v", body["success"])
	}
}

func TestInstall_Unauthenticated(t *testing.T) {
	db := setupDB(t)
	app := newTestAppNoAuth(t, db)

	req := makeInstallReq(map[string]any{
		"addonKey": "com.example.addon",
		"version":  "1.0.0",
	})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if body["success"] != false {
		t.Fatalf("expected success=false, got %v", body["success"])
	}
}

func TestInstall_InvalidBody(t *testing.T) {
	db := setupDB(t)
	app := newTestApp(t, db)

	req := httptest.NewRequest("POST", "/marketplace/install", bytes.NewReader([]byte("not json")))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestInstall_FullMode_Bundle404(t *testing.T) {
	db := setupDB(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", 404)
	}))
	defer srv.Close()

	origAllow := os.Getenv("ALLOW_UNSIGNED_BUNDLES")
	os.Setenv("ALLOW_UNSIGNED_BUNDLES", "true")
	inst := installer.New(db, "test")
	os.Setenv("ALLOW_UNSIGNED_BUNDLES", origAllow)

	app := fiber.New()
	h, err := marketplace.NewHandler(db, marketplace.WithInstaller(inst))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	authMw := func(c fiber.Ctx) error {
		c.Locals(auth.LocalOrganizationID, testOrgID)
		c.Locals(auth.LocalUserID, testUserID)
		return c.Next()
	}
	h.Mount(app, authMw)

	req := makeInstallReq(map[string]any{
		"addonKey":  "com.example.addon",
		"version":   "1.0.0",
		"bundleURL": srv.URL + "/bundle.tar.gz",
	})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	if resp.StatusCode != 422 {
		t.Fatalf("expected 422, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if body["success"] != false {
		t.Fatalf("expected success=false, got %v", body["success"])
	}
	data := body["data"].(map[string]any)
	if data["status"] != "failed" {
		t.Fatalf("expected status failed, got %v", data["status"])
	}
}

func TestInstall_FullMode_BundleKeyMismatch(t *testing.T) {
	db := setupDB(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := &bundle.Bundle{
			Manifest: manifest.Manifest{
				Key:              "different.key",
				Version:          "1.0.0",
				Kernel:           ">=2.0",
				ModelDefinitions: []manifest.ModelDefinition{},
			},
		}
		var buf bytes.Buffer
		bundle.Write(&buf, b)
		w.Header().Set("Content-Type", "application/gzip")
		w.Write(buf.Bytes())
	}))
	defer srv.Close()

	origAllow := os.Getenv("ALLOW_UNSIGNED_BUNDLES")
	os.Setenv("ALLOW_UNSIGNED_BUNDLES", "true")
	inst := installer.New(db, "test")
	os.Setenv("ALLOW_UNSIGNED_BUNDLES", origAllow)

	app := fiber.New()
	h, err := marketplace.NewHandler(db, marketplace.WithInstaller(inst))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	authMw := func(c fiber.Ctx) error {
		c.Locals(auth.LocalOrganizationID, testOrgID)
		c.Locals(auth.LocalUserID, testUserID)
		return c.Next()
	}
	h.Mount(app, authMw)

	req := makeInstallReq(map[string]any{
		"addonKey":  "com.example.addon",
		"version":   "1.0.0",
		"bundleURL": srv.URL + "/bundle.tar.gz",
	})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	if resp.StatusCode != 422 {
		t.Fatalf("expected 422, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	data := body["data"].(map[string]any)
	if data["status"] != "failed" {
		t.Fatalf("expected status failed, got %v", data["status"])
	}
}

func TestList_Auth(t *testing.T) {
	db := setupDB(t)
	app := newTestApp(t, db)

	resp, err := app.Test(httptest.NewRequest("GET", "/marketplace/installs", nil))
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if body["success"] != true {
		t.Fatalf("expected success=true, got %v", body["success"])
	}
	data := body["data"].([]any)
	if data == nil {
		t.Fatalf("expected data array, got nil")
	}
}

func TestList_Unauthenticated(t *testing.T) {
	db := setupDB(t)
	app := newTestAppNoAuth(t, db)

	resp, err := app.Test(httptest.NewRequest("GET", "/marketplace/installs", nil))
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestList_FiltersByOrg(t *testing.T) {
	db := setupDB(t)

	db.Exec(`INSERT INTO marketplace_installations (id, organization_id, addon_key, version, status, requested_by_id, requested_at)
		VALUES (?, ?, 'org1.addon', '1.0.0', 'requested', ?, datetime('now'))`,
		uuid.New().String(), testOrgID.String(), testUserID.String())

	db.Exec(`INSERT INTO marketplace_installations (id, organization_id, addon_key, version, status, requested_by_id, requested_at)
		VALUES (?, ?, 'org2.addon', '1.0.0', 'requested', ?, datetime('now'))`,
		uuid.New().String(), testOrgID2.String(), testUserID.String())

	app := fiber.New()
	h, err := marketplace.NewHandler(db)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	authMw := func(c fiber.Ctx) error {
		c.Locals(auth.LocalOrganizationID, testOrgID)
		c.Locals(auth.LocalUserID, testUserID)
		return c.Next()
	}
	h.Mount(app, authMw)

	resp, err := app.Test(httptest.NewRequest("GET", "/marketplace/installs", nil))
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	data := body["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("expected 1 row for org %s, got %d", testOrgID.String(), len(data))
	}
	row := data[0].(map[string]any)
	if row["addon_key"] != "org1.addon" {
		t.Fatalf("expected org1.addon, got %v", row["addon_key"])
	}
}

func TestInstall_LiteMode_PersistsRow(t *testing.T) {
	db := setupDB(t)
	app := newTestApp(t, db)

	req := makeInstallReq(map[string]any{
		"addonKey": "com.example.addon",
		"version":  "1.0.0",
		"name":     "Example Addon",
		"category": "productivity",
	})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	if resp.StatusCode != 201 {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	var count int64
	db.Model(&marketplace.Installation{}).Count(&count)
	if count != 1 {
		t.Fatalf("expected 1 row in DB, got %d", count)
	}

	var row marketplace.Installation
	if err := db.First(&row).Error; err != nil {
		t.Fatalf("find row: %v", err)
	}
	if row.AddonKey != "com.example.addon" {
		t.Fatalf("expected addon_key com.example.addon, got %s", row.AddonKey)
	}
	if row.Status != "requested" {
		t.Fatalf("expected status requested, got %s", row.Status)
	}
}

func TestInstall_LiteMode_WithoutBundleURL(t *testing.T) {
	db := setupDB(t)
	app := newTestApp(t, db)

	req := makeInstallReq(map[string]any{
		"addonKey":  "com.example.addon",
		"version":   "1.0.0",
		"bundleURL": "",
	})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	if resp.StatusCode != 201 {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if body["success"] != true {
		t.Fatalf("expected success=true, got %v", body["success"])
	}
}

func TestInstall_FullMode_WithInstallerNoBundleURL(t *testing.T) {
	db := setupDB(t)

	origAllow := os.Getenv("ALLOW_UNSIGNED_BUNDLES")
	os.Setenv("ALLOW_UNSIGNED_BUNDLES", "true")
	inst := installer.New(db, "test")
	os.Setenv("ALLOW_UNSIGNED_BUNDLES", origAllow)

	app := fiber.New()
	h, err := marketplace.NewHandler(db, marketplace.WithInstaller(inst))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	authMw := func(c fiber.Ctx) error {
		c.Locals(auth.LocalOrganizationID, testOrgID)
		c.Locals(auth.LocalUserID, testUserID)
		return c.Next()
	}
	h.Mount(app, authMw)

	req := makeInstallReq(map[string]any{
		"addonKey":  "com.example.addon",
		"version":   "1.0.0",
		"bundleURL": "",
	})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	if resp.StatusCode != 201 {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
}

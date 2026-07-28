package marketplace_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

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
		completed_at DATETIME,
		previous_version TEXT,
		previous_versions TEXT,
		upgraded_at DATETIME,
		uninstalled_at DATETIME,
		purge_at DATETIME,
		data_policy TEXT,
		requires TEXT
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

// ─── uninstall / upgrade / rollback / discovery ───────────────────────────

// seedInstalled inserts a `installed` marketplace row for the test org so the
// uninstall/upgrade/rollback handlers have something to target. Returns the
// row id so the rollback test can look it up.
func seedInstalled(t *testing.T, db *gorm.DB, addonKey, version string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	if err := db.Exec(`INSERT INTO marketplace_installations
		(id, organization_id, addon_key, version, bundle_url, status,
		 requested_by_id, requested_at)
		VALUES (?, ?, ?, ?, '', 'installed', ?, ?)`,
		id.String(), testOrgID.String(), addonKey, version,
		testUserID.String(), now).Error; err != nil {
		t.Fatalf("seed installed: %v", err)
	}
	return id
}

func TestUninstall_DefaultPolicy_LiteMode(t *testing.T) {
	db := setupDB(t)
	seedInstalled(t, db, "com.example.addon", "1.0.0")
	app := newTestApp(t, db)

	req := makeJSONReq(t, "POST", "/marketplace/uninstall", map[string]any{
		"addonKey": "com.example.addon",
	})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if body["success"] != true {
		t.Fatalf("expected success=true, got %v", body)
	}
	data := body["data"].(map[string]any)
	if data["status"] != "uninstalled" {
		t.Fatalf("expected status uninstalled, got %v", data["status"])
	}
	if data["data_policy"] != "retain_30d" {
		t.Fatalf("expected data_policy retain_30d, got %v", data["data_policy"])
	}
	if data["purge_at"] == nil || data["purge_at"] == "" {
		t.Fatalf("expected purge_at set for retain_30d policy, got %v", data["purge_at"])
	}
}

func TestUninstall_DropNow_NoPurgeAt(t *testing.T) {
	db := setupDB(t)
	seedInstalled(t, db, "com.example.addon", "1.0.0")
	app := newTestApp(t, db)

	req := makeJSONReq(t, "POST", "/marketplace/uninstall", map[string]any{
		"addonKey":   "com.example.addon",
		"dataPolicy": "drop_now",
	})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	data := readBody(t, resp)["data"].(map[string]any)
	if data["data_policy"] != "drop_now" {
		t.Fatalf("expected data_policy drop_now, got %v", data["data_policy"])
	}
	if pa, ok := data["purge_at"]; ok && pa != nil && pa != "" {
		t.Fatalf("expected purge_at nil for drop_now, got %v", pa)
	}
}

func TestUninstall_RetainForever_NoPurgeAt(t *testing.T) {
	db := setupDB(t)
	seedInstalled(t, db, "com.example.addon", "1.0.0")
	app := newTestApp(t, db)

	req := makeJSONReq(t, "POST", "/marketplace/uninstall", map[string]any{
		"addonKey":   "com.example.addon",
		"dataPolicy": "retain_forever",
	})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	data := readBody(t, resp)["data"].(map[string]any)
	if data["data_policy"] != "retain_forever" {
		t.Fatalf("expected data_policy retain_forever, got %v", data["data_policy"])
	}
	if pa, ok := data["purge_at"]; ok && pa != nil && pa != "" {
		t.Fatalf("expected purge_at nil for retain_forever, got %v", pa)
	}
}

func TestUninstall_InvalidPolicy(t *testing.T) {
	db := setupDB(t)
	seedInstalled(t, db, "com.example.addon", "1.0.0")
	app := newTestApp(t, db)

	req := makeJSONReq(t, "POST", "/marketplace/uninstall", map[string]any{
		"addonKey":   "com.example.addon",
		"dataPolicy": "delete_everything",
	})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestUninstall_NotInstalled_Returns404(t *testing.T) {
	db := setupDB(t)
	app := newTestApp(t, db)

	req := makeJSONReq(t, "POST", "/marketplace/uninstall", map[string]any{
		"addonKey": "com.example.nope",
	})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestUninstall_Unauthenticated(t *testing.T) {
	db := setupDB(t)
	seedInstalled(t, db, "com.example.addon", "1.0.0")
	app := newTestAppNoAuth(t, db)

	req := makeJSONReq(t, "POST", "/marketplace/uninstall", map[string]any{
		"addonKey": "com.example.addon",
	})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

// seedInstalledWithRequires inserts an installed row whose Requires column
// declares the given dep keys. Drives the dep-block / cascade tests below.
func seedInstalledWithRequires(t *testing.T, db *gorm.DB, addonKey, name string, requires []string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	reqJSON, _ := json.Marshal(requires)
	if err := db.Exec(`INSERT INTO marketplace_installations
		(id, organization_id, addon_key, name, version, bundle_url, status,
		 requested_by_id, requested_at, requires)
		VALUES (?, ?, ?, ?, '1.0.0', '', 'installed', ?, ?, ?)`,
		id.String(), testOrgID.String(), addonKey, name,
		testUserID.String(), now, string(reqJSON)).Error; err != nil {
		t.Fatalf("seed installed with requires: %v", err)
	}
	return id
}

func TestUninstall_DepBlock_ReturnsConflictWithDependents(t *testing.T) {
	db := setupDB(t)
	// "customers" is the leaf; "pos" depends on it.
	seedInstalled(t, db, "customers", "1.0.0")
	seedInstalledWithRequires(t, db, "pos", "Point of Sale", []string{"customers"})
	app := newTestApp(t, db)

	req := makeJSONReq(t, "POST", "/marketplace/uninstall", map[string]any{
		"addonKey": "customers",
	})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	if resp.StatusCode != 409 {
		t.Fatalf("expected 409 conflict, got %d (body=%s)", resp.StatusCode, dumpBody(resp))
	}
	body := readBody(t, resp)
	if body["success"] != false {
		t.Fatalf("expected success=false, got %v", body["success"])
	}
	data, ok := body["data"].(map[string]any)
	if !ok {
		t.Fatalf("expected data object, got %v", body["data"])
	}
	deps, ok := data["dependents"].([]any)
	if !ok || len(deps) != 1 {
		t.Fatalf("expected one dependent, got %v", data["dependents"])
	}
	first := deps[0].(map[string]any)
	if first["key"] != "pos" {
		t.Fatalf("expected dependent key=pos, got %v", first["key"])
	}
	if first["name"] != "Point of Sale" {
		t.Fatalf("expected dependent name=Point of Sale, got %v", first["name"])
	}
}

func TestUninstall_NoDependents_Passes(t *testing.T) {
	db := setupDB(t)
	// Only one addon installed → no reverse deps possible.
	seedInstalled(t, db, "customers", "1.0.0")
	app := newTestApp(t, db)

	req := makeJSONReq(t, "POST", "/marketplace/uninstall", map[string]any{
		"addonKey": "customers",
	})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d (body=%s)", resp.StatusCode, dumpBody(resp))
	}
	data := readBody(t, resp)["data"].(map[string]any)
	if data["status"] != "uninstalled" {
		t.Fatalf("expected status=uninstalled, got %v", data["status"])
	}
}

func TestUninstall_ForceOverride_BypassesDepBlock(t *testing.T) {
	db := setupDB(t)
	seedInstalled(t, db, "customers", "1.0.0")
	seedInstalledWithRequires(t, db, "pos", "POS", []string{"customers"})
	app := newTestApp(t, db)

	req := makeJSONReq(t, "POST", "/marketplace/uninstall", map[string]any{
		"addonKey": "customers",
		"force":    true,
	})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 with force=true, got %d (body=%s)", resp.StatusCode, dumpBody(resp))
	}
	data := readBody(t, resp)["data"].(map[string]any)
	if data["status"] != "uninstalled" {
		t.Fatalf("expected status=uninstalled, got %v", data["status"])
	}
}

func TestUninstall_Cascade_UninstallsDependentsLeafFirst(t *testing.T) {
	db := setupDB(t)
	// Dep chain: invoicing → pos → customers
	// Cascade on "customers" should uninstall invoicing first, then pos, then
	// customers (leaf-first).
	seedInstalled(t, db, "customers", "1.0.0")
	seedInstalledWithRequires(t, db, "pos", "POS", []string{"customers"})
	seedInstalledWithRequires(t, db, "invoicing", "Invoicing", []string{"pos"})
	app := newTestApp(t, db)

	req := makeJSONReq(t, "POST", "/marketplace/uninstall", map[string]any{
		"addonKey": "customers",
		"cascade":  true,
	})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 with cascade=true, got %d (body=%s)", resp.StatusCode, dumpBody(resp))
	}
	body := readBody(t, resp)
	data := body["data"].(map[string]any)
	list, ok := data["uninstalled"].([]any)
	if !ok || len(list) != 3 {
		t.Fatalf("expected 3 uninstalled rows, got %v", data["uninstalled"])
	}
	// First entry must be the deepest leaf (no one depends on it: invoicing),
	// last entry must be the requested addon (customers).
	first := list[0].(map[string]any)
	if first["addon_key"] != "invoicing" {
		t.Fatalf("expected leaf-first uninstall (invoicing), got %v", first["addon_key"])
	}
	last := list[len(list)-1].(map[string]any)
	if last["addon_key"] != "customers" {
		t.Fatalf("expected requested addon last (customers), got %v", last["addon_key"])
	}
	primary, ok := data["primary"].(map[string]any)
	if !ok || primary["addon_key"] != "customers" {
		t.Fatalf("expected primary=customers, got %v", data["primary"])
	}
}

func TestUninstall_Cascade_NoDependents_StillUninstallsPrimary(t *testing.T) {
	db := setupDB(t)
	seedInstalled(t, db, "customers", "1.0.0")
	app := newTestApp(t, db)

	req := makeJSONReq(t, "POST", "/marketplace/uninstall", map[string]any{
		"addonKey": "customers",
		"cascade":  true,
	})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	data := readBody(t, resp)["data"].(map[string]any)
	list := data["uninstalled"].([]any)
	if len(list) != 1 {
		t.Fatalf("expected exactly 1 uninstall, got %d", len(list))
	}
	only := list[0].(map[string]any)
	if only["addon_key"] != "customers" {
		t.Fatalf("expected customers, got %v", only["addon_key"])
	}
}

func TestUpgrade_LiteMode_IdempotentVersionBump(t *testing.T) {
	db := setupDB(t)
	seedInstalled(t, db, "com.example.addon", "1.0.0")
	app := newTestApp(t, db)

	req := makeJSONReq(t, "POST", "/marketplace/upgrade", map[string]any{
		"addonKey":      "com.example.addon",
		"targetVersion": "1.1.0",
		"bundleURL":     "https://example.test/bundle.tar.gz",
	})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d (body=%s)", resp.StatusCode, dumpBody(resp))
	}
	body := readBody(t, resp)
	data := body["data"].(map[string]any)
	if data["version"] != "1.1.0" {
		t.Fatalf("expected version 1.1.0, got %v", data["version"])
	}
	if data["previous_version"] != "1.0.0" {
		t.Fatalf("expected previous_version 1.0.0, got %v", data["previous_version"])
	}
	if data["upgraded_at"] == nil || data["upgraded_at"] == "" {
		t.Fatalf("expected upgraded_at set, got %v", data["upgraded_at"])
	}

	// Re-issuing the same upgrade is idempotent — we land on the same target
	// version with the previous version pointing at 1.1.0 (the version
	// before this 2nd call).
	req2 := makeJSONReq(t, "POST", "/marketplace/upgrade", map[string]any{
		"addonKey":      "com.example.addon",
		"targetVersion": "1.2.0",
		"bundleURL":     "https://example.test/bundle.tar.gz",
	})
	resp2, err := app.Test(req2)
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	if resp2.StatusCode != 200 {
		t.Fatalf("expected 200 on 2nd upgrade, got %d", resp2.StatusCode)
	}
	data2 := readBody(t, resp2)["data"].(map[string]any)
	if data2["version"] != "1.2.0" {
		t.Fatalf("expected version 1.2.0, got %v", data2["version"])
	}
	// previous_versions should contain both 1.0.0 and 1.1.0 (oldest first).
	history := data2["previous_versions"].([]any)
	if len(history) != 2 {
		t.Fatalf("expected 2 history entries, got %d (%v)", len(history), history)
	}
	if history[0] != "1.0.0" || history[1] != "1.1.0" {
		t.Fatalf("unexpected history order: %v", history)
	}
}

func TestUpgrade_HistoryCappedAtThree(t *testing.T) {
	db := setupDB(t)
	seedInstalled(t, db, "com.example.addon", "1.0.0")
	app := newTestApp(t, db)

	// Five forward bumps — only the last three predecessors should remain.
	for _, v := range []string{"1.1.0", "1.2.0", "1.3.0", "1.4.0", "1.5.0"} {
		req := makeJSONReq(t, "POST", "/marketplace/upgrade", map[string]any{
			"addonKey":      "com.example.addon",
			"targetVersion": v,
			"bundleURL":     "https://example.test/bundle.tar.gz",
		})
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("upgrade to %s: %v", v, err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("upgrade to %s expected 200, got %d", v, resp.StatusCode)
		}
	}
	// Fetch final state via /installs to inspect the history.
	resp, err := app.Test(httptest.NewRequest("GET", "/marketplace/installs", nil))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	rows := readBody(t, resp)["data"].([]any)
	if len(rows) == 0 {
		t.Fatalf("no rows returned")
	}
	row := rows[0].(map[string]any)
	history := row["previous_versions"].([]any)
	if len(history) != 3 {
		t.Fatalf("expected history capped at 3, got %d (%v)", len(history), history)
	}
	if history[0] != "1.2.0" || history[2] != "1.4.0" {
		t.Fatalf("expected oldest=1.2.0 newest=1.4.0, got %v", history)
	}
}

func TestUpgrade_NotInstalled_Returns404(t *testing.T) {
	db := setupDB(t)
	app := newTestApp(t, db)

	req := makeJSONReq(t, "POST", "/marketplace/upgrade", map[string]any{
		"addonKey":      "com.example.nope",
		"targetVersion": "1.1.0",
		"bundleURL":     "https://example.test/bundle.tar.gz",
	})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestUpgrade_MissingTargetVersion(t *testing.T) {
	db := setupDB(t)
	seedInstalled(t, db, "com.example.addon", "1.0.0")
	app := newTestApp(t, db)

	req := makeJSONReq(t, "POST", "/marketplace/upgrade", map[string]any{
		"addonKey":  "com.example.addon",
		"bundleURL": "https://example.test/bundle.tar.gz",
	})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestRollback_LiteMode_RestoresPreviousVersion(t *testing.T) {
	db := setupDB(t)
	id := seedInstalled(t, db, "com.example.addon", "1.0.0")
	app := newTestApp(t, db)

	// Bump to 1.1.0 first so there's a history entry to roll back to.
	upReq := makeJSONReq(t, "POST", "/marketplace/upgrade", map[string]any{
		"addonKey":      "com.example.addon",
		"targetVersion": "1.1.0",
		"bundleURL":     "https://example.test/bundle.tar.gz",
	})
	resp, err := app.Test(upReq)
	if err != nil {
		t.Fatalf("seed upgrade: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("seed upgrade expected 200, got %d", resp.StatusCode)
	}

	// Roll back.
	rbReq := httptest.NewRequest("POST", "/marketplace/rollback/"+id.String(), nil)
	rbResp, err := app.Test(rbReq)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if rbResp.StatusCode != 200 {
		t.Fatalf("rollback expected 200, got %d (body=%s)", rbResp.StatusCode, dumpBody(rbResp))
	}
	body := readBody(t, rbResp)
	data := body["data"].(map[string]any)
	if data["version"] != "1.0.0" {
		t.Fatalf("expected rollback to land on 1.0.0, got %v", data["version"])
	}
	if data["previous_version"] != "1.1.0" {
		t.Fatalf("expected previous_version 1.1.0 after rollback, got %v", data["previous_version"])
	}
}

func TestRollback_NoHistory_Returns409(t *testing.T) {
	db := setupDB(t)
	id := seedInstalled(t, db, "com.example.addon", "1.0.0")
	app := newTestApp(t, db)

	rbReq := httptest.NewRequest("POST", "/marketplace/rollback/"+id.String(), nil)
	rbResp, err := app.Test(rbReq)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if rbResp.StatusCode != 409 {
		t.Fatalf("expected 409 (no history), got %d", rbResp.StatusCode)
	}
}

func TestRollback_OtherOrgInstallation_Returns404(t *testing.T) {
	db := setupDB(t)
	// Seed an installation owned by org2 — current session is org1.
	id := uuid.New()
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	if err := db.Exec(`INSERT INTO marketplace_installations
		(id, organization_id, addon_key, version, bundle_url, status,
		 requested_by_id, requested_at, previous_version)
		VALUES (?, ?, 'com.example.addon', '1.1.0', '', 'installed', ?, ?, '1.0.0')`,
		id.String(), testOrgID2.String(), testUserID.String(), now).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	app := newTestApp(t, db)

	rbReq := httptest.NewRequest("POST", "/marketplace/rollback/"+id.String(), nil)
	rbResp, err := app.Test(rbReq)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if rbResp.StatusCode != 404 {
		t.Fatalf("expected 404 (org isolation), got %d", rbResp.StatusCode)
	}
}

func TestRollback_InvalidUUID(t *testing.T) {
	db := setupDB(t)
	app := newTestApp(t, db)

	rbReq := httptest.NewRequest("POST", "/marketplace/rollback/not-a-uuid", nil)
	rbResp, err := app.Test(rbReq)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if rbResp.StatusCode != 400 {
		t.Fatalf("expected 400, got %d", rbResp.StatusCode)
	}
}

func TestDiscoverAddons_ListsInstalled(t *testing.T) {
	db := setupDB(t)
	seedInstalled(t, db, "com.example.addon", "1.0.0")
	seedInstalled(t, db, "com.example.other", "2.0.0")
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
	h.MountDiscovery(app, authMw)

	resp, err := app.Test(httptest.NewRequest("GET", "/api/addons", nil))
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	data := body["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("expected 2 addons, got %d (%v)", len(data), data)
	}
	for _, item := range data {
		row := item.(map[string]any)
		if row["has_update"] != false {
			t.Fatalf("expected has_update false without checker, got %v", row["has_update"])
		}
	}
}

func TestDiscoverAddons_HasUpdateWhenCheckerReportsNewer(t *testing.T) {
	db := setupDB(t)
	seedInstalled(t, db, "com.example.addon", "1.0.0")

	app := fiber.New()
	h, err := marketplace.NewHandler(db,
		marketplace.WithUpdateChecker(marketplace.UpdateCheckerFunc(
			func(_ context.Context, key string) (string, error) {
				if key == "com.example.addon" {
					return "1.5.0", nil
				}
				return "", nil
			})))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	authMw := func(c fiber.Ctx) error {
		c.Locals(auth.LocalOrganizationID, testOrgID)
		c.Locals(auth.LocalUserID, testUserID)
		return c.Next()
	}
	h.MountDiscovery(app, authMw)

	resp, err := app.Test(httptest.NewRequest("GET", "/api/addons", nil))
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	data := readBody(t, resp)["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("expected 1 row, got %d", len(data))
	}
	row := data[0].(map[string]any)
	if row["has_update"] != true {
		t.Fatalf("expected has_update=true, got %v", row["has_update"])
	}
	if row["latest_version"] != "1.5.0" {
		t.Fatalf("expected latest_version=1.5.0, got %v", row["latest_version"])
	}
}

func TestDiscoverAddons_FiltersOutUninstalled(t *testing.T) {
	db := setupDB(t)
	seedInstalled(t, db, "com.example.alive", "1.0.0")
	// Insert an uninstalled row that should not appear in discovery.
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	db.Exec(`INSERT INTO marketplace_installations
		(id, organization_id, addon_key, version, bundle_url, status,
		 requested_by_id, requested_at, uninstalled_at, data_policy)
		VALUES (?, ?, 'com.example.dead', '0.5.0', '', 'uninstalled', ?, ?, ?, 'drop_now')`,
		uuid.New().String(), testOrgID.String(), testUserID.String(), now, now)

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
	h.MountDiscovery(app, authMw)

	resp, err := app.Test(httptest.NewRequest("GET", "/api/addons", nil))
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	data := readBody(t, resp)["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("expected only the live addon, got %d (%v)", len(data), data)
	}
	row := data[0].(map[string]any)
	if row["addon_key"] != "com.example.alive" {
		t.Fatalf("expected com.example.alive, got %v", row["addon_key"])
	}
}

// ─── shared helpers for the new tests ─────────────────────────────────────

// makeJSONReq builds a request with the supplied body for the given method
// and path. Mirrors makeInstallReq but reusable across the new endpoints.
func makeJSONReq(t *testing.T, method, path string, body map[string]any) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		t.Fatalf("encode body: %v", err)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	return req
}

// dumpBody snapshots the response body for inclusion in t.Fatalf messages
// without consuming it for the assertion path. Safe for one-shot use.
func dumpBody(resp *http.Response) string {
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "<read error: " + err.Error() + ">"
	}
	return string(b)
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

// newTestAppWithInstaller mounts the handler with the kernel installer wired so
// runUninstall can consult metacore_installations as the source of truth. It
// also migrates the installer's authoritative table onto the same sqlite DB.
func newTestAppWithInstaller(t *testing.T, db *gorm.DB) *fiber.App {
	t.Helper()
	// Hand-create metacore_installations (the installer's source of truth).
	// AutoMigrate emits a Postgres `gen_random_uuid()` default that sqlite
	// rejects, so mirror setupDB's raw-DDL approach.
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS metacore_installations (
		id TEXT PRIMARY KEY,
		organization_id TEXT NOT NULL,
		addon_key TEXT NOT NULL,
		version TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'enabled',
		source TEXT NOT NULL,
		secret_hash TEXT,
		secret_enc TEXT,
		settings TEXT,
		manifest_hash TEXT,
		installed_at DATETIME,
		enabled_at DATETIME,
		disabled_at DATETIME
	)`).Error; err != nil {
		t.Fatalf("create metacore_installations: %v", err)
	}
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
	return app
}

// seedInstallerRow inserts a live row into metacore_installations (the
// installer's source of truth) WITHOUT any matching marketplace_installations
// bookkeeping row — mimicking an addon installed by bundle-sync.
func seedInstallerRow(t *testing.T, db *gorm.DB, addonKey string) {
	t.Helper()
	if err := db.Create(&installer.Installation{
		ID:             uuid.New(),
		OrganizationID: testOrgID,
		AddonKey:       addonKey,
		Version:        "1.0.0",
		Status:         "enabled",
		Source:         "bundle",
	}).Error; err != nil {
		t.Fatalf("seed installer row: %v", err)
	}
}

// TestUninstall_InstallerSourceOfTruth_NoBookkeepingRow covers the bundle-sync
// case: the addon is live in metacore_installations but has NO
// marketplace_installations row. Uninstall must NOT 404 — it runs the teardown
// and synthesizes the bookkeeping row as uninstalled. It must also be
// idempotent (a second call does not error or duplicate rows).
func TestUninstall_InstallerSourceOfTruth_NoBookkeepingRow(t *testing.T) {
	db := setupDB(t)
	app := newTestAppWithInstaller(t, db)
	seedInstallerRow(t, db, "link_inbox")

	// Sanity: no bookkeeping row exists yet.
	var before int64
	db.Table("marketplace_installations").Where("addon_key = ?", "link_inbox").Count(&before)
	if before != 0 {
		t.Fatalf("expected 0 bookkeeping rows before uninstall, got %d", before)
	}

	req := makeJSONReq(t, "POST", "/marketplace/uninstall", map[string]any{
		"addonKey": "link_inbox",
	})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 (installer is source of truth), got %d", resp.StatusCode)
	}
	data := readBody(t, resp)["data"].(map[string]any)
	if data["status"] != "uninstalled" {
		t.Fatalf("expected status uninstalled, got %v", data["status"])
	}

	// Teardown removed the authoritative row.
	var live int64
	db.Model(&installer.Installation{}).Where("addon_key = ?", "link_inbox").Count(&live)
	if live != 0 {
		t.Fatalf("expected metacore_installations row deleted, got %d", live)
	}

	// A bookkeeping row was synthesized as uninstalled.
	var after int64
	db.Table("marketplace_installations").Where("addon_key = ? AND status = ?", "link_inbox", "uninstalled").Count(&after)
	if after != 1 {
		t.Fatalf("expected 1 synthesized uninstalled bookkeeping row, got %d", after)
	}

	// Idempotent: second call finds the (now uninstalled) bookkeeping row and
	// re-runs the normal path without erroring or duplicating.
	resp2, err := app.Test(makeJSONReq(t, "POST", "/marketplace/uninstall", map[string]any{
		"addonKey": "link_inbox",
	}))
	if err != nil {
		t.Fatalf("second test request: %v", err)
	}
	if resp2.StatusCode != 200 {
		t.Fatalf("expected idempotent 200 on second uninstall, got %d", resp2.StatusCode)
	}
	var total int64
	db.Table("marketplace_installations").Where("addon_key = ?", "link_inbox").Count(&total)
	if total != 1 {
		t.Fatalf("expected exactly 1 bookkeeping row after idempotent re-run, got %d", total)
	}
}

// TestUninstall_NotInThirdTable_Returns404 confirms an addon absent from BOTH
// tables still yields a legitimate 404 even when the installer is wired.
func TestUninstall_NotInThirdTable_Returns404(t *testing.T) {
	db := setupDB(t)
	app := newTestAppWithInstaller(t, db)

	resp, err := app.Test(makeJSONReq(t, "POST", "/marketplace/uninstall", map[string]any{
		"addonKey": "com.example.ghost",
	}))
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404 for addon absent from all tables, got %d", resp.StatusCode)
	}
}

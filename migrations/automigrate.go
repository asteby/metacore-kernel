package migrations

import (
	"fmt"
	"reflect"
	"strings"

	"gorm.io/gorm"
)

// AutoMigrate runs a two-pass FK-safe auto-migration over the given models.
//
// Phase 1 creates tables with foreign-key constraints disabled so models can
// migrate in arbitrary order without failing on dangling references. Phase 2
// re-runs with constraints enabled so gorm adds them. The caller's original
// DisableForeignKeyConstraintWhenMigrating setting is restored before return.
//
// This is invoked explicitly from an app's migrate command — never at boot
// time — so the "safe in-place schema update" guarantee is the operator's to
// make.
func AutoMigrate(db *gorm.DB, models []any) error {
	if len(models) == 0 {
		return nil
	}
	original := db.Config.DisableForeignKeyConstraintWhenMigrating
	defer func() { db.Config.DisableForeignKeyConstraintWhenMigrating = original }()

	db.Config.DisableForeignKeyConstraintWhenMigrating = true
	if err := db.AutoMigrate(models...); err != nil {
		return fmt.Errorf("migrations: phase 1 (FK-disabled) auto-migrate: %w", err)
	}

	db.Config.DisableForeignKeyConstraintWhenMigrating = false
	if err := db.AutoMigrate(models...); err != nil {
		return fmt.Errorf("migrations: phase 2 (FK-enabled) auto-migrate: %w", err)
	}
	return nil
}

// AutoMigrateSorted topologically sorts models by their gorm `foreignKey:`
// struct tags and then runs AutoMigrate. The sort is a best-effort — cycles
// are broken silently — and redundant given the two-pass AutoMigrate, but
// deterministic ordering is useful for readable migration logs and for
// dialects that occasionally misbehave in phase 1.
func AutoMigrateSorted(db *gorm.DB, models map[string]any) error {
	return AutoMigrate(db, TopoSort(models))
}

// TopoSort orders the given model map by foreignKey dependencies discovered
// through reflection on gorm struct tags. Exposed so callers can inspect
// or log the order they would migrate in.
func TopoSort(models map[string]any) []any {
	typeToKey := make(map[string]string, len(models))
	for key, m := range models {
		typeToKey[structName(m)] = key
	}

	deps := make(map[string]map[string]struct{}, len(models))
	for key, m := range models {
		set := map[string]struct{}{}
		scanDeps(reflectElem(m), typeToKey, set)
		delete(set, key) // drop self-references
		deps[key] = set
	}

	sorted := make([]any, 0, len(models))
	visited := make(map[string]bool, len(models))
	visiting := make(map[string]bool, len(models))

	var visit func(string)
	visit = func(key string) {
		if visited[key] || visiting[key] {
			return
		}
		visiting[key] = true
		for dep := range deps[key] {
			visit(dep)
		}
		visiting[key] = false
		visited[key] = true
		sorted = append(sorted, models[key])
	}
	for key := range models {
		visit(key)
	}
	return sorted
}

func structName(m any) string {
	t := reflect.TypeOf(m)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t.Name()
}

func reflectElem(m any) reflect.Type {
	t := reflect.TypeOf(m)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t
}

func scanDeps(t reflect.Type, typeToKey map[string]string, out map[string]struct{}) {
	if t == nil || t.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)

		// Recurse into embedded structs (e.g. modelbase.BaseUUIDModel).
		if field.Anonymous {
			ft := field.Type
			if ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				scanDeps(ft, typeToKey, out)
			}
			continue
		}

		tag := field.Tag.Get("gorm")
		if !strings.Contains(tag, "foreignKey:") {
			continue
		}
		ft := field.Type
		if ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Slice {
			ft = ft.Elem()
			if ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
		}
		if depKey, ok := typeToKey[ft.Name()]; ok {
			out[depKey] = struct{}{}
		}
	}
}

// ResetDatabase drops every application-owned table in the connected schema.
// DESTRUCTIVE — only call behind an explicit operator flag plus whatever
// confirmation the app's UX demands. The kernel does not prompt; that is a
// responsibility of the caller.
//
// Postgres: drops and recreates the `public` schema (CASCADE). SQLite:
// iterates sqlite_master and drops every non-system table. Other dialects
// return ErrResetUnsupported.
func ResetDatabase(db *gorm.DB) error {
	switch db.Dialector.Name() {
	case "postgres":
		if err := db.Exec("DROP SCHEMA IF EXISTS public CASCADE").Error; err != nil {
			return fmt.Errorf("migrations: drop public schema: %w", err)
		}
		if err := db.Exec("CREATE SCHEMA public").Error; err != nil {
			return fmt.Errorf("migrations: recreate public schema: %w", err)
		}
		return nil
	case "sqlite":
		var names []string
		if err := db.Raw(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`).Scan(&names).Error; err != nil {
			return err
		}
		for _, n := range names {
			if !safeIdent(n) {
				continue
			}
			if err := db.Exec(`DROP TABLE IF EXISTS "` + n + `"`).Error; err != nil {
				return fmt.Errorf("migrations: drop table %s: %w", n, err)
			}
		}
		return nil
	default:
		return fmt.Errorf("%w: dialect %q", ErrResetUnsupported, db.Dialector.Name())
	}
}

func safeIdent(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r == '_' || (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
			continue
		}
		return false
	}
	return true
}

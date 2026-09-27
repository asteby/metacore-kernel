package dynamic

import (
	"fmt"
	"strings"

	"github.com/asteby/metacore-kernel/manifest"
)

// ModelTarget is where the host materializes a model another addon extends.
type ModelTarget struct {
	Schema string
	Table  string
	// AcceptsExtensions is whether the target's addon lists the model in
	// extension_points.model_extensions_accepted.
	AcceptsExtensions bool
}

// ModelTargetResolver resolves "<addon_key>.<ModelKey>" to its physical table.
// ok=false when the host does not know the model (not installed, or a host
// that does not track it): the extension table is then created without the
// physical FOREIGN KEY (a logical 1:1 the dynamic service still honours).
type ModelTargetResolver func(ref string) (ModelTarget, bool)

// idColumnDDL is the primary-key fragment. An extension table's id IS the
// target row's id, so it has no default: a row can only be written with the id
// of an existing target (enforced by the FOREIGN KEY when the target resolves).
func idColumnDDL(def manifest.ModelDefinition) string {
	if def.Extends != "" {
		return `"id" uuid PRIMARY KEY`
	}
	return `"id" uuid PRIMARY KEY DEFAULT gen_random_uuid()`
}

// extendsConstraintName is the stable name of an extension table's FOREIGN KEY.
func extendsConstraintName(table string) string {
	return "fk_" + table + "_extends"
}

// extendsForeignKeyStatement adds the 1:1 FOREIGN KEY (id → target.id, ON
// DELETE CASCADE) once. ALTER TABLE … ADD CONSTRAINT has no IF NOT EXISTS, so
// the guard looks the constraint up by name.
func extendsForeignKeyStatement(schema, table string, target ModelTarget) string {
	name := extendsConstraintName(table)
	return fmt.Sprintf(`DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = '%s' AND conrelid = '%s'::regclass) THEN
    ALTER TABLE %q.%q ADD CONSTRAINT %q FOREIGN KEY ("id") REFERENCES %q.%q ("id") ON DELETE CASCADE;
  END IF;
END $$`,
		name, strings.ReplaceAll(fmt.Sprintf("%q.%q", schema, table), "'", "''"),
		schema, table, name, target.Schema, target.Table)
}

// resolveExtendsTarget validates the resolved target of an extension table. A
// resolved target whose addon does not accept extensions is an install error;
// an unresolved one yields ok=false (no physical FK).
func resolveExtendsTarget(def manifest.ModelDefinition, resolve ModelTargetResolver) (ModelTarget, bool, error) {
	if def.Extends == "" || resolve == nil {
		return ModelTarget{}, false, nil
	}
	t, ok := resolve(def.Extends)
	if !ok || t.Table == "" {
		return ModelTarget{}, false, nil
	}
	if !t.AcceptsExtensions {
		return ModelTarget{}, false, fmt.Errorf("model %s extends %s, which its addon does not list in extension_points.model_extensions_accepted", def.ModelKey, def.Extends)
	}
	if t.Schema == "" {
		t.Schema = "public"
	}
	return t, true, nil
}

// searchKeySQL renders a SearchKeyDef as the immutable expression of a STORED
// generated column. Numeric parts go through trim_scale so 16.0 and 16 compose
// the same key. Normalized keys keep only the column values, uppercased and
// stripped of anything that is not a letter or a digit; exact keys concatenate
// literals and values as declared. Any NULL part makes the key NULL.
func searchKeySQL(sk *manifest.SearchKeyDef) (string, error) {
	var parts []string
	cols := 0
	for _, p := range sk.Parts {
		switch {
		case p.Column != "":
			cols++
			parts = append(parts, searchKeyColumnSQL(p))
		case p.Literal != "" && sk.Match == "exact":
			parts = append(parts, "'"+strings.ReplaceAll(p.Literal, "'", "''")+"'")
		}
	}
	if cols == 0 {
		return "", fmt.Errorf("search key has no column part")
	}
	expr := strings.Join(parts, " || ")
	if sk.Match == "exact" {
		return "(" + expr + ")", nil
	}
	return fmt.Sprintf(`upper(regexp_replace(%s, '[^A-Za-z0-9]', '', 'g'))`, expr), nil
}

func searchKeyColumnSQL(p manifest.SearchKeyPart) string {
	t := strings.ToLower(strings.TrimSpace(p.Type))
	switch {
	case t == "numeric" || t == "decimal" || strings.HasPrefix(t, "numeric("):
		return fmt.Sprintf(`trim_scale(%q)::text`, p.Column)
	default:
		return fmt.Sprintf(`%q::text`, p.Column)
	}
}

// NormalizeSearchKey applies the normalized search-key folding to typed text so
// it compares against the stored key: "205/55 r16" → "20555R16" minus the
// format's literal letters ("R") → "2055516". Literal letters are removed only
// when they sit between values, so a value that itself has letters survives.
func NormalizeSearchKey(sk *manifest.SearchKeyDef, typed string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(typed) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if sk == nil {
		return out
	}
	for _, p := range sk.Parts {
		lit := strings.ToUpper(p.Literal)
		if lit == "" {
			continue
		}
		var lb strings.Builder
		for _, r := range lit {
			if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
				lb.WriteRune(r)
			}
		}
		if l := lb.String(); l != "" {
			out = removeBetweenDigits(out, l)
		}
	}
	return out
}

// removeBetweenDigits drops occurrences of lit that sit between two digits
// ("20555R16" with lit "R" → "2055516"), leaving letters inside values alone.
func removeBetweenDigits(s, lit string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], lit) && i > 0 && isDigit(s[i-1]) && i+len(lit) < len(s) && isDigit(s[i+len(lit)]) {
			i += len(lit)
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

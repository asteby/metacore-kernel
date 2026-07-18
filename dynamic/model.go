// Package dynamic builds tables and GORM-compatible struct types from a
// ModelDefinition at runtime. Each addon gets its own Postgres schema
// (addon_<key>) so table names never collide with core or other addons.
package dynamic

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"
)

// QualifiedTable returns "<schema>.<table>" for the shared-isolation layout.
// For per-tenant addons, callers should build the schema via
// SchemaName(key, orgID, IsolationPerTenant) and join themselves.
func QualifiedTable(addonKey, table string) string {
	return SharedSchemaName(addonKey) + "." + table
}

// BaseFields are always present on an addon table.
type BaseFields struct {
	ID        uuid.UUID  `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	CreatedAt time.Time  `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt time.Time  `gorm:"autoUpdateTime" json:"updated_at"`
	DeletedAt *time.Time `gorm:",omitempty" json:"deleted_at,omitempty"`
}

// BuildStructType assembles a runtime struct type for a ModelDefinition.
// The result is suitable for GORM AutoMigrate and reflect.New for CRUD.
func BuildStructType(def manifest.ModelDefinition) (reflect.Type, error) {
	fields := []reflect.StructField{
		{
			Name: "ID",
			Type: reflect.TypeOf(uuid.UUID{}),
			Tag:  `json:"id" gorm:"type:uuid;primaryKey;default:gen_random_uuid()"`,
		},
	}
	if def.OrgScoped {
		fields = append(fields, reflect.StructField{
			Name: "OrganizationID",
			Type: reflect.TypeOf(uuid.UUID{}),
			Tag:  `json:"organization_id" gorm:"type:uuid;not null;index"`,
		})
	}
	for _, c := range def.Columns {
		rf, err := columnToField(c)
		if err != nil {
			return nil, fmt.Errorf("column %q: %w", c.Name, err)
		}
		fields = append(fields, rf)
	}
	fields = append(fields,
		reflect.StructField{Name: "CreatedAt", Type: reflect.TypeOf(time.Time{}), Tag: `json:"created_at" gorm:"autoCreateTime"`},
		reflect.StructField{Name: "UpdatedAt", Type: reflect.TypeOf(time.Time{}), Tag: `json:"updated_at" gorm:"autoUpdateTime"`},
	)
	if def.SoftDelete {
		fields = append(fields, reflect.StructField{
			Name: "DeletedAt",
			Type: reflect.TypeOf(&time.Time{}),
			Tag:  `json:"deleted_at,omitempty" gorm:"index"`,
		})
	}
	return reflect.StructOf(fields), nil
}

func columnToField(c manifest.ColumnDef) (reflect.StructField, error) {
	goType, gormType, err := columnGoType(c)
	if err != nil {
		return reflect.StructField{}, err
	}
	// A NULLABLE uuid column (an optional FK such as products.category_id) must be
	// a POINTER type. As a non-pointer uuid.UUID its zero value is the nil UUID
	// "00000000-…", which GORM writes verbatim on INSERT/UPDATE instead of NULL —
	// so leaving the picker empty violates the foreign key ("insert ... violates
	// foreign key constraint" / SQLSTATE 23503). No client-side normalization can
	// fix this: even an explicit JSON `null` unmarshals into a non-pointer
	// uuid.UUID as the nil UUID. As *uuid.UUID an unset value stays nil → GORM
	// writes NULL. Required uuids keep the value (NOT NULL) form.
	if goType == uuidType && !c.Required {
		goType = reflect.PtrTo(uuidType)
	}
	// The uuid "family of nullability": every OPTIONAL scalar column whose Go type
	// is a NON-POINTER value (timestamp → time.Time, numeric → float64, bool →
	// bool) has the exact same latent bug as the nullable uuid above. Their zero
	// values (0001-01-01T00:00:00Z / 0 / false) are indistinguishable from a real
	// "unset" and GORM writes them verbatim instead of NULL — so an empty optional
	// timestamp persists a bogus year-1 date, an empty numeric persists 0, an empty
	// bool persists false. Making them POINTERS lets an unset value stay nil → GORM
	// writes NULL.
	//
	// CONTRACT: pointer-ize ONLY when the column is !Required AND declares no
	// Default. When a Default is declared the VALUE form is kept: the column DEFAULT
	// covers "unset", preserving the semantics addons chose when they declared it
	// (a bool defaulting to false, a numeric defaulting to 0, etc.). string/text/
	// json/jsonb/int/bigint/vector are intentionally left as-is (empty string is a
	// legitimate value, jsonb has its own handling, ints have no ambiguous NULL vs
	// zero need here).
	if lit, ok := manifest.DefaultLiteral(c.Default); !c.Required && !(ok && lit != "") {
		switch goType {
		case timeType, float64Type, boolType:
			goType = reflect.PtrTo(goType)
		}
	}
	name := exportName(c.Name)
	tags := []string{fmt.Sprintf(`json:"%s"`, c.Name)}
	gormParts := []string{"type:" + gormType}
	// A Postgres STORED generated column is maintained by the database on every
	// write, so it MUST be excluded from INSERT/UPDATE — otherwise GORM sends the
	// field's zero value in the column list and Postgres rejects the write with
	// "cannot insert a non-DEFAULT value into column ...". GORM's read-only
	// permission ("->") keeps the column in SELECT/scan (so it still surfaces in
	// list/detail/export) while omitting it from every write. Generated columns
	// carry no NOT NULL / DEFAULT / index (validation forbids them), so we return
	// here before those parts are appended.
	if c.Generated != "" {
		gormParts = append(gormParts, "->")
		tags = append(tags, fmt.Sprintf(`gorm:"%s"`, strings.Join(gormParts, ";")))
		return reflect.StructField{
			Name: name,
			Type: goType,
			Tag:  reflect.StructTag(strings.Join(tags, " ")),
		}, nil
	}
	if c.Required {
		gormParts = append(gormParts, "not null")
	}
	if c.Index {
		gormParts = append(gormParts, "index")
	}
	if c.Unique {
		gormParts = append(gormParts, "uniqueIndex")
	}
	if lit, ok := manifest.DefaultLiteral(c.Default); ok && lit != "" {
		gormParts = append(gormParts, "default:"+lit)
	}
	tags = append(tags, fmt.Sprintf(`gorm:"%s"`, strings.Join(gormParts, ";")))
	return reflect.StructField{
		Name: name,
		Type: goType,
		Tag:  reflect.StructTag(strings.Join(tags, " ")),
	}, nil
}

func columnGoType(c manifest.ColumnDef) (reflect.Type, string, error) {
	switch strings.ToLower(c.Type) {
	case "string":
		size := c.Size
		if size == 0 {
			size = 255
		}
		return reflect.TypeOf(""), fmt.Sprintf("varchar(%d)", size), nil
	case "text":
		return reflect.TypeOf(""), "text", nil
	case "uuid":
		return reflect.TypeOf(uuid.UUID{}), "uuid", nil
	case "int", "integer":
		return reflect.TypeOf(int(0)), "integer", nil
	case "bigint":
		return reflect.TypeOf(int64(0)), "bigint", nil
	case "decimal", "numeric", "float", "double":
		return reflect.TypeOf(float64(0)), "numeric(18,4)", nil
	case "bool", "boolean":
		return reflect.TypeOf(false), "boolean", nil
	case "timestamp", "timestamptz", "datetime", "timestamp with time zone":
		return reflect.TypeOf(time.Time{}), "timestamptz", nil
	case "date":
		return reflect.TypeOf(time.Time{}), "date", nil
	case "jsonb", "json":
		// json.RawMessage (not map[string]any): a jsonb column must accept ANY
		// JSON value — an OBJECT (e.g. address, display_config) OR an ARRAY (e.g.
		// transfer/adjust line-items items[]/lines[]). map[string]any makes
		// json.Unmarshal reject arrays ("cannot unmarshal array into map"), which
		// surfaced as a 400 "invalid request body" on every line-items create.
		// RawMessage stores the raw JSON bytes and GORM passes them straight to
		// the jsonb column; toMap re-marshals to the original object/array.
		return reflect.TypeOf(json.RawMessage{}), "jsonb", nil
	case "vector":
		// pgvector embedding column. pgvector.Vector implements GORM's
		// Valuer/Scanner and driver serialization, so GORM reads/writes the
		// column natively. Dimension comes from ColumnDef.Size (a bare "vector"
		// leaves it unconstrained); the verbatim vector(N) form is handled below.
		if c.Size > 0 {
			return reflect.TypeOf(pgvector.Vector{}), fmt.Sprintf("vector(%d)", c.Size), nil
		}
		return reflect.TypeOf(pgvector.Vector{}), "vector", nil
	default:
		// Parameterized Postgres-native forms (e.g. numeric(6,2), varchar(120),
		// vector(768)) that v3 manifests declare verbatim — pass the validated
		// form through.
		if sqlType, ok := parameterizedColumnType(c.Type); ok {
			switch {
			case strings.HasPrefix(sqlType, "varchar"):
				return reflect.TypeOf(""), sqlType, nil
			case strings.HasPrefix(sqlType, "vector"):
				return reflect.TypeOf(pgvector.Vector{}), sqlType, nil
			default:
				return reflect.TypeOf(float64(0)), sqlType, nil
			}
		}
		return nil, "", fmt.Errorf("unknown column type %q", c.Type)
	}
}

// exportName converts snake_case to PascalCase for the Go struct field name.
func exportName(s string) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, "")
}

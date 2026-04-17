// Package dynamic builds tables and GORM-compatible struct types from a
// ModelDefinition at runtime. Each addon gets its own Postgres schema
// (addon_<key>) so table names never collide with core or other addons.
package dynamic

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
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
	name := exportName(c.Name)
	tags := []string{fmt.Sprintf(`json:"%s"`, c.Name)}
	gormParts := []string{"type:" + gormType}
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
	case "timestamp", "datetime":
		return reflect.TypeOf(time.Time{}), "timestamptz", nil
	case "date":
		return reflect.TypeOf(time.Time{}), "date", nil
	case "jsonb", "json":
		return reflect.TypeOf(map[string]any{}), "jsonb", nil
	default:
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

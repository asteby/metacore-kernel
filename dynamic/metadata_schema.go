package dynamic

import (
	"context"
	"fmt"
	"strings"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

// MetadataValidationSchema builds a ValidationSchemaResolver from the served
// metadata of Go models (DefineModal / DefineTable), so the rules a compiled
// model already declares for its form are also enforced server-side before
// the write — the same pass, codes and 422 body as manifest columns:
//
//   - FieldDef.Required (unless Readonly or with a DefaultValue) → required;
//   - FieldDef.Type "number" → invalid_type for a non-numeric value;
//   - static FieldDef.Options on a single-value select → invalid_option;
//   - FieldDef.Validation (regex / min / max / custom), falling back to the
//     ColumnDef.Validation of the same key → the declarative checks.
//
// Wire it as Config.ValidationSchemaResolver (host.AppConfig
// ValidateModelMetadata does it). A resolver of the host (addon manifests)
// can be chained in front with ChainValidationSchemas.
func MetadataValidationSchema(meta *metadata.Service) ValidationSchemaResolver {
	return func(ctx context.Context, model string) ([]manifest.ColumnDef, bool) {
		if meta == nil {
			return nil, false
		}
		modal, err := meta.GetModal(ctx, model)
		if err != nil || modal == nil {
			return nil, false
		}
		colRules := map[string]*modelbase.ValidationRule{}
		if table, err := meta.GetTable(ctx, model); err == nil && table != nil {
			for _, c := range table.Columns {
				if c.Validation != nil {
					colRules[c.Key] = c.Validation
				}
			}
		}
		cols := make([]manifest.ColumnDef, 0, len(modal.Fields))
		for _, f := range modal.Fields {
			if f.Key == "" || strings.Contains(f.Key, ".") {
				continue
			}
			col := manifest.ColumnDef{
				Name:     f.Key,
				Type:     metadataFieldType(f.Type),
				Required: f.Required && !f.Readonly,
				Default:  f.DefaultValue,
			}
			if len(f.Options) > 0 && !f.Multiple && f.OptionsSource == "" && f.Type == "select" {
				for _, o := range f.Options {
					col.Options = append(col.Options, manifest.Option{Value: fmt.Sprint(o.Value), Label: o.Label})
				}
			}
			rule := f.Validation
			if rule == nil {
				rule = colRules[f.Key]
			}
			if rule != nil {
				col.Validation = &manifest.ValidationRule{Regex: rule.Regex, Min: rule.Min, Max: rule.Max, Custom: rule.Custom}
			}
			if !col.Required && col.Validation == nil && len(col.Options) == 0 && col.Type != "number" {
				continue
			}
			cols = append(cols, col)
		}
		return cols, len(cols) > 0
	}
}

// ChainValidationSchemas returns a resolver that asks each resolver in order
// and answers with the first that declares columns. nil entries are skipped.
func ChainValidationSchemas(resolvers ...ValidationSchemaResolver) ValidationSchemaResolver {
	return func(ctx context.Context, model string) ([]manifest.ColumnDef, bool) {
		for _, r := range resolvers {
			if r == nil {
				continue
			}
			if cols, ok := r(ctx, model); ok && len(cols) > 0 {
				return cols, true
			}
		}
		return nil, false
	}
}

// metadataFieldType maps a form field type to the column type the validation
// pass understands (numbers are bounded by value, everything else by length).
func metadataFieldType(t string) string {
	switch strings.ToLower(t) {
	case "number", "integer", "int", "decimal", "currency", "money", "float":
		return "number"
	case "email", "url", "uuid", "text", "textarea", "string", "":
		return "string"
	default:
		return strings.ToLower(t)
	}
}

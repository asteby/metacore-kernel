// Package metakit holds small helpers for compiled (Go) models:
//
//   - metadata shorthands (Col, StatusCol, Text, Select, Opts…) that declare
//     TableMetadata / ModalMetadata without repeating the same struct literal in
//     every model;
//   - a partial-update guard (Guard, Install, Trusted) that validates
//     Updates(map) and map inserts — the writes BeforeSave cannot see — and
//     keeps columns outside the form read-only unless a domain action marks its
//     session as Trusted.
//
// It holds no domain logic.
//
// # Guard in three steps
//
//	// 1. once, at boot (after opening the DB)
//	if err := metakit.Install(db); err != nil { return err }
//
//	// 2. per model, in its package init / register func
//	metakit.Guard[Ticket]("tickets", func(before, after *Ticket) error {
//	    if before != nil && before.Status == "closed" && after.Status != "closed" {
//	        return errors.New("a closed ticket is reopened with its action")
//	    }
//	    return nil
//	})
//
//	// 3. a domain action that already checked its transition
//	metakit.Trusted(tx).Model(&t).Updates(map[string]any{"status": "closed"})
//
// Untrusted map writes that change a column outside DefineModal fail with a
// 422 whose field error code is protected_field (localized by the SDK); a rule
// error becomes an "_" field error carrying its message.
package metakit

import "github.com/asteby/metacore-kernel/modelbase"

// Opts turns value/label pairs into select options: Opts("open", "Open",
// "closed", "Closed"). A trailing odd value is ignored.
func Opts(pairs ...string) []modelbase.OptionDef {
	out := make([]modelbase.OptionDef, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, modelbase.OptionDef{Value: pairs[i], Label: pairs[i+1]})
	}
	return out
}

// Col declares a sortable text column.
func Col(key, label string) modelbase.ColumnDef {
	return modelbase.ColumnDef{Key: key, Label: label, Type: "text", Sortable: true}
}

// DateCol declares a sortable date column.
func DateCol(key, label string) modelbase.ColumnDef {
	return modelbase.ColumnDef{Key: key, Label: label, Type: "date", Sortable: true}
}

// StatusCol declares a status column (badge) with options, sortable and
// filterable.
func StatusCol(key, label string, opts []modelbase.OptionDef) modelbase.ColumnDef {
	return modelbase.ColumnDef{Key: key, Label: label, Type: "select", CellStyle: "status", Options: opts, Sortable: true, Filterable: true}
}

// SelectCol declares a sortable, filterable select column.
func SelectCol(key, label string, opts []modelbase.OptionDef) modelbase.ColumnDef {
	return modelbase.ColumnDef{Key: key, Label: label, Type: "select", Options: opts, Sortable: true, Filterable: true}
}

// Text declares a text form field.
func Text(key, label string, required bool) modelbase.FieldDef {
	return modelbase.FieldDef{Key: key, Label: label, Type: "text", Required: required}
}

// Area declares a long-text form field.
func Area(key, label string, required bool) modelbase.FieldDef {
	return modelbase.FieldDef{Key: key, Label: label, Type: "textarea", Required: required}
}

// Select declares a select form field.
func Select(key, label string, required bool, opts []modelbase.OptionDef) modelbase.FieldDef {
	return modelbase.FieldDef{Key: key, Label: label, Type: "select", Required: required, Options: opts}
}

// Date declares a date form field.
func Date(key, label string, required bool) modelbase.FieldDef {
	return modelbase.FieldDef{Key: key, Label: label, Type: "date", Required: required}
}

// Number declares a numeric form field.
func Number(key, label string, required bool) modelbase.FieldDef {
	return modelbase.FieldDef{Key: key, Label: label, Type: "number", Required: required}
}

// Bool declares a boolean form field.
func Bool(key, label string) modelbase.FieldDef {
	return modelbase.FieldDef{Key: key, Label: label, Type: "boolean"}
}

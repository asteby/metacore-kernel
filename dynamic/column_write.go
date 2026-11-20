package dynamic

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/asteby/metacore-kernel/modelbase"
)

// Column write permissions (manifest v3 columns[].write_permission →
// modelbase.ColumnDef.WritePermission).
//
// The write twin of ReadPermission: a column that declares a WritePermission
// can be CHANGED through the generic create/update only by callers holding that
// permission. It sits between an ungated column (anyone who may write the
// record) and Protected (nobody through generic writes):
//
//   - a caller without the permission who sends a different value is rejected
//     with *ColumnWriteDeniedError (403, code "column_write_denied");
//   - a value the caller merely echoes back unchanged (an edit form posts every
//     field) is dropped from the input, so the save does not fail;
//   - a system caller (NewSystemCaller) is never gated, and declared actions
//     write through their own path (data_mutate), which this gate does not touch.
//
// The permission decision is the same single one the read gate uses
// (holdsColumnPermission / Config.ColumnReadChecker), so a host wires one
// checker for both. The gate runs inside validateColumns, i.e. wherever the
// field-validation pass runs (Create, Update, extension tables) and therefore
// needs Config.ValidationSchemaResolver, exactly like Protected.

// ColumnWriteDeniedError reports a write that tried to change a column the
// caller may not write. It wraps ErrForbidden so it answers 403; the handler
// adds code "column_write_denied", the model and the column.
type ColumnWriteDeniedError struct {
	Model  string
	Column string
}

func (e *ColumnWriteDeniedError) Error() string {
	return fmt.Sprintf("permission denied: column %q of %s is not writable", e.Column, e.Model)
}

// Unwrap ties the error to ErrForbidden for errors.Is-based 403 mapping.
func (e *ColumnWriteDeniedError) Unwrap() error { return ErrForbidden }

// mayWriteColumn reports whether user may change a column gated by perm.
func (s *Service) mayWriteColumn(ctx context.Context, user modelbase.AuthUser, perm string) bool {
	if _, ok := user.(systemPrincipal); ok {
		return true
	}
	return s.holdsColumnPermission(ctx, user, strings.TrimSpace(perm))
}

// ColumnWriteMask lists the columns of model (sorted) that user may NOT change,
// so a host can serve them read-only in the caller's form metadata. A system
// caller yields none. It fails closed on a metadata error other than "model
// has no metadata": the error is returned with every gated column listed.
func (s *Service) ColumnWriteMask(ctx context.Context, model string, user modelbase.AuthUser) ([]string, error) {
	meta, err := s.meta.GetTable(ctx, model)
	if err != nil {
		return nil, fmt.Errorf("dynamic: column write gates of %s: %w", model, err)
	}
	var out []string
	held := map[string]bool{}
	for _, c := range meta.Columns {
		p := strings.TrimSpace(c.WritePermission)
		if p == "" || c.Key == "" {
			continue
		}
		ok, seen := held[p]
		if !seen {
			ok = s.mayWriteColumn(ctx, user, p)
			held[p] = ok
		}
		if !ok {
			out = append(out, c.Key)
		}
	}
	sort.Strings(out)
	return out, nil
}

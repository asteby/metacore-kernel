package inbox

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"gorm.io/gorm"
)

// PurgeBatch is how many rows one purge statement touches.
const PurgeBatch = 5000

// PurgeReport says what a purge did.
type PurgeReport struct {
	// Blanked is the number of rows whose headers and body_raw were removed
	// (older than RetainRaw).
	Blanked int64 `json:"blanked"`
	// Deleted is the number of rows removed entirely (older than RetainResult).
	Deleted int64 `json:"deleted"`
}

// Purge applies the retention policy: body_raw and headers go after RetainRaw
// (default 90 days); the result columns and the body hash stay RetainResult
// (default 24 months), after which the row and its attempts are deleted.
// Rows still awaiting attention (recibida, procesando, reintentar,
// requiere_revision, dead_letter) are never touched. Run it daily from the
// host; each call handles at most PurgeBatch rows per step, so loop until
// the report is zero for big backlogs.
func (s *Store) Purge(ctx context.Context) (PurgeReport, error) {
	var rep PurgeReport
	now := s.now()
	rawCut, resCut := now.Add(-s.cfg.RetainRaw), now.Add(-s.cfg.RetainResult)
	closed := []string{EstadoProcesada, EstadoDuplicada, EstadoIgnorada, EstadoReconciliadaAPI, EstadoResueltaManual}

	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if s.pg {
			// The append-only guard lets headers/body_raw change only here.
			if err := tx.Exec("SELECT set_config('metacore.inbox_purge', 'on', true)").Error; err != nil {
				return err
			}
		}
		ids := tx.Table(TableNotifications).Select("id").
			Where("received_at < ? AND purged_at IS NULL AND estado IN ?", rawCut, closed).Limit(PurgeBatch)
		res := tx.Table(TableNotifications).Where("id IN (?)", ids).
			Updates(map[string]any{"body_raw": nil, "headers": "{}", "purged_at": now})
		if res.Error != nil {
			return res.Error
		}
		rep.Blanked = res.RowsAffected

		old := tx.Table(TableNotifications).Select("id").
			Where("received_at < ? AND estado IN ?", resCut, closed).Limit(PurgeBatch)
		if err := tx.Exec("DELETE FROM "+TableAttempts+" WHERE notification_id IN (?)", old).Error; err != nil {
			return err
		}
		res = tx.Exec("DELETE FROM "+TableNotifications+" WHERE id IN (?)", old)
		rep.Deleted = res.RowsAffected
		return res.Error
	})
	return rep, err
}

// Export formats.
const (
	FormatCSV    = "csv"
	FormatNDJSON = "ndjson"
)

var csvHeader = []string{"id", "received_at", "provider", "route", "method", "source_ip", "auth", "auth_reason", "estado", "motivo",
	"intentos", "resource_kind", "resource_id", "event_id", "payment_id", "checkout_ref", "incident_id", "body_sha256", "body_redacted"}

// Export streams the journal as CSV or NDJSON in received_at order. It never
// includes body_raw or headers; body_redacted carries the masked payload.
// Callers gate it behind payments.notifications.export. It returns the number
// of rows written.
func (s *Store) Export(ctx context.Context, w io.Writer, f Filter, format string) (int, error) {
	if format != FormatCSV && format != FormatNDJSON {
		return 0, fmt.Errorf("inbox: unknown export format %q", format)
	}
	var cw *csv.Writer
	var enc *json.Encoder
	if format == FormatCSV {
		cw = csv.NewWriter(w)
		if err := cw.Write(csvHeader); err != nil {
			return 0, err
		}
	} else {
		enc = json.NewEncoder(w)
	}
	total := 0
	const page = 500
	for offset := 0; ; offset += page {
		var rows []Notification
		err := s.filtered(ctx, f).Select(notificationColumns).Order("received_at, id").Limit(page).Offset(offset).Find(&rows).Error
		if err != nil {
			return total, err
		}
		for _, n := range rows {
			n.Headers = ""
			if enc != nil {
				if err := enc.Encode(n); err != nil {
					return total, err
				}
			} else if err := cw.Write(csvRow(n)); err != nil {
				return total, err
			}
			total++
		}
		if len(rows) < page {
			break
		}
	}
	if cw != nil {
		cw.Flush()
		return total, cw.Error()
	}
	return total, nil
}

func csvRow(n Notification) []string {
	opt := func(p interface{ String() string }, ok bool) string {
		if !ok {
			return ""
		}
		return p.String()
	}
	pay, inc := "", ""
	if n.PaymentID != nil {
		pay = opt(n.PaymentID, true)
	}
	if n.IncidentID != nil {
		inc = opt(n.IncidentID, true)
	}
	row := []string{n.ID.String(), n.ReceivedAt.UTC().Format(time.RFC3339Nano), n.Provider, n.Route, n.Method, n.SourceIP,
		n.Auth, n.AuthReason, n.Estado, n.Motivo, strconv.Itoa(n.Intentos), n.ResourceKind, n.ResourceID, n.EventID,
		pay, n.CheckoutRef, inc, n.BodySHA256, string(n.BodyRedacted)}
	for i, c := range row {
		// Spreadsheet formula injection: provider-controlled text must not
		// open as a formula in Excel or Sheets.
		if c != "" && (c[0] == '=' || c[0] == '+' || c[0] == '-' || c[0] == '@') {
			row[i] = "'" + c
		}
	}
	return row
}

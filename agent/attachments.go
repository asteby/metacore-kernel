package agent

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/xuri/excelize/v2"
)

// ChatAttachment is a user-uploaded file or a shared record/view embed.
type ChatAttachment struct {
	URL           string `json:"url,omitempty"`
	Name          string `json:"name,omitempty"`
	MimeType      string `json:"mime_type,omitempty"`
	Size          int64  `json:"size,omitempty"`
	Type          string `json:"type,omitempty"` // record | view | (empty = file)
	Model         string `json:"model,omitempty"`
	ModelTitle    string `json:"model_title,omitempty"`
	ID            string `json:"id,omitempty"`
	Title         string `json:"title,omitempty"`
	Subtitle      string `json:"subtitle,omitempty"`
	Search        string `json:"search,omitempty"`
	FilterSummary string `json:"filter_summary,omitempty"`
}

// ResolveStoragePath maps a public /storage/... URL to a local disk path.
func ResolveStoragePath(url string) string {
	u := strings.TrimSpace(url)
	u = strings.TrimPrefix(u, "/")
	if strings.HasPrefix(u, "storage/") {
		// /storage/uploads/x → storage/public/uploads/x
		rest := strings.TrimPrefix(u, "storage/")
		return filepath.Join("storage", "public", rest)
	}
	if strings.HasPrefix(u, "storage/public/") {
		return u
	}
	return ""
}

func isImageAtt(a ChatAttachment) bool {
	mime := strings.ToLower(a.MimeType)
	name := strings.ToLower(a.Name + " " + a.URL)
	if strings.HasPrefix(mime, "image/") {
		return true
	}
	for _, ext := range []string{".jpg", ".jpeg", ".png", ".gif", ".webp"} {
		if strings.Contains(name, ext) {
			return true
		}
	}
	return false
}

func isSpreadsheetAtt(a ChatAttachment) bool {
	mime := strings.ToLower(a.MimeType)
	name := strings.ToLower(a.Name + " " + a.URL)
	if strings.Contains(mime, "sheet") || strings.Contains(mime, "excel") || mime == "text/csv" {
		return true
	}
	for _, ext := range []string{".xlsx", ".xls", ".csv"} {
		if strings.Contains(name, ext) {
			return true
		}
	}
	return false
}

func attachmentsFromCopilotMetadata(raw json.RawMessage) []ChatAttachment {
	if len(raw) == 0 {
		return nil
	}
	var wrap struct {
		Attachments []ChatAttachment `json:"attachments"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil || len(wrap.Attachments) == 0 {
		return nil
	}
	return wrap.Attachments
}

// LastUserAttachments returns the most recent file/record embeds the user
// attached in this copilot thread. A follow-up like "puedes reintentar?" has
// no files of its own; without this, the model honestly does not see the jpeg
// that is still visible in the UI.
func LastUserAttachments(store ConversationStore, conversationID uuid.UUID) []ChatAttachment {
	if store == nil || conversationID == uuid.Nil {
		return nil
	}
	rows, err := store.LastUserMessageMetadata(conversationID, 12)
	if err != nil {
		return nil
	}
	for _, meta := range rows {
		if atts := attachmentsFromCopilotMetadata(meta); len(atts) > 0 {
			return atts
		}
	}
	return nil
}

// AttachmentOptions tunes how attachments are folded into the message.
type AttachmentOptions struct {
	// RecordHint returns the instruction appended to a shared record. Empty
	// means the default generic hint. Hosts use it to add model-specific
	// guidance (for example which model carries the image field).
	RecordHint func(a ChatAttachment) string
	// ImageFieldHint is appended to the line describing an attached image,
	// telling the model where to put the URL on create/update. Defaults to
	// " (p. ej. data.image)".
	ImageFieldHint string
}

// EnrichMessageWithAttachments appends spreadsheet text extracts to the user
// message so Claude can act on Excel/CSV without a separate tool. Images are
// returned as local paths (vision) plus public URLs (for create_record image fields).
func EnrichMessageWithAttachments(message string, atts []ChatAttachment) (enriched string, imagePaths []string, imageURLs []string) {
	return EnrichMessageWithAttachmentsOpts(message, atts, AttachmentOptions{})
}

// EnrichMessageWithAttachmentsOpts is EnrichMessageWithAttachments with host
// hooks.
func EnrichMessageWithAttachmentsOpts(message string, atts []ChatAttachment, opts AttachmentOptions) (enriched string, imagePaths []string, imageURLs []string) {
	enriched = message
	if opts.ImageFieldHint == "" {
		opts.ImageFieldHint = " (p. ej. data.image)"
	}
	var docs []string
	for _, a := range atts {
		if a.Type == "record" || a.Type == "view" {
			label := a.Title
			if label == "" {
				label = a.ModelTitle
			}
			if label == "" {
				label = a.Model
			}
			if a.Type == "record" {
				hint := "úsalo como contexto; el usuario puede pedir actualizarlo/consultarlo"
				if opts.RecordHint != nil {
					if h := opts.RecordHint(a); h != "" {
						hint = h
					}
				}
				docs = append(docs, fmt.Sprintf(
					"[registro compartido: %s · model=%s id=%s — %s]",
					label, a.Model, a.ID, hint,
				))
			} else {
				docs = append(docs, fmt.Sprintf(
					"[vista compartida: %s · model=%s search=%s — consulta esta tabla/filtro]",
					label, a.Model, a.Search,
				))
			}
			continue
		}
		pubURL := strings.TrimSpace(a.URL)
		path := ResolveStoragePath(pubURL)
		onDisk := false
		if path != "" {
			if _, err := os.Stat(path); err == nil {
				onDisk = true
			}
		}
		label := a.Name
		if label == "" && path != "" {
			label = filepath.Base(path)
		}
		if label == "" {
			label = "adjunto"
		}
		if isImageAtt(a) {
			if onDisk {
				imagePaths = append(imagePaths, path)
			}
			if pubURL != "" {
				imageURLs = append(imageURLs, pubURL)
			}
			docs = append(docs, fmt.Sprintf(
				"[imagen adjunta: %s url=%s — visión + al create_record/update_record pon este URL en el campo de imagen del modelo%s]",
				label, pubURL, opts.ImageFieldHint,
			))
			continue
		}
		if !onDisk {
			if pubURL != "" {
				docs = append(docs, fmt.Sprintf("[archivo adjunto: %s url=%s]", label, pubURL))
			}
			continue
		}
		if isSpreadsheetAtt(a) {
			text, err := ExtractSpreadsheetPreview(path, 40)
			if err != nil {
				docs = append(docs, fmt.Sprintf("[archivo %s: no se pudo leer: %v]", label, err))
				continue
			}
			docs = append(docs, fmt.Sprintf("### Archivo adjunto: %s\n```\n%s\n```", label, text))
			continue
		}
		docs = append(docs, fmt.Sprintf("[archivo adjunto: %s (%s)]", label, a.MimeType))
	}
	if len(docs) == 0 {
		return enriched, imagePaths, imageURLs
	}
	enriched = strings.TrimSpace(enriched) + "\n\n" + strings.Join(docs, "\n\n")
	return enriched, imagePaths, imageURLs
}

// ExtractSpreadsheetPreview reads the first sheet (or CSV) into a compact TSV preview.
func ExtractSpreadsheetPreview(path string, maxRows int) (string, error) {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".csv" {
		f, err := os.Open(path)
		if err != nil {
			return "", err
		}
		defer f.Close()
		r := csv.NewReader(f)
		r.FieldsPerRecord = -1
		var lines []string
		for i := 0; i < maxRows; i++ {
			rec, err := r.Read()
			if err != nil {
				break
			}
			lines = append(lines, strings.Join(rec, "\t"))
		}
		if len(lines) == 0 {
			return "", fmt.Errorf("csv vacío")
		}
		return strings.Join(lines, "\n"), nil
	}

	f, err := excelize.OpenFile(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return "", fmt.Errorf("excel sin hojas")
	}
	rows, err := f.GetRows(sheets[0])
	if err != nil {
		return "", err
	}
	var lines []string
	for i, row := range rows {
		if i >= maxRows {
			lines = append(lines, fmt.Sprintf("… (%d filas más)", len(rows)-maxRows))
			break
		}
		lines = append(lines, strings.Join(row, "\t"))
	}
	return strings.Join(lines, "\n"), nil
}

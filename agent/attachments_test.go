package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

type fakeConvStore struct {
	rows []json.RawMessage
	err  error
	lim  int
}

func (f *fakeConvStore) LastUserMessageMetadata(_ uuid.UUID, limit int) ([]json.RawMessage, error) {
	f.lim = limit
	return f.rows, f.err
}

func TestResolveStoragePath(t *testing.T) {
	got := ResolveStoragePath("/storage/aby/x.png")
	want := filepath.Join("storage", "public", "aby", "x.png")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if ResolveStoragePath("https://evil/x.png") != "" || ResolveStoragePath("") != "" {
		t.Fatal("non-storage URLs must not map to disk")
	}
}

func TestExtractSpreadsheetPreviewCSV(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.csv")
	if err := os.WriteFile(path, []byte("sku,price\nA1,10\nA2,20\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	text, err := ExtractSpreadsheetPreview(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "sku\tprice") || !strings.Contains(text, "A1") {
		t.Fatalf("unexpected preview: %q", text)
	}
	if one, _ := ExtractSpreadsheetPreview(path, 1); strings.Contains(one, "A1") {
		t.Fatal("row cap ignored")
	}
}

func TestEnrichMessageWithAttachments(t *testing.T) {
	t.Chdir(t.TempDir())
	dir := filepath.Join("storage", "public", "aby")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "t.csv"), []byte("a,b\n1,2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "p.png"), []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}

	msg, imgs, urls := EnrichMessageWithAttachments("actualiza", []ChatAttachment{
		{URL: "/storage/aby/t.csv", Name: "t.csv", MimeType: "text/csv"},
		{URL: "/storage/aby/p.png", Name: "p.png", MimeType: "image/png"},
		{URL: "/storage/aby/missing.pdf", Name: "m.pdf", MimeType: "application/pdf"},
		{Type: "record", Model: "customers", ID: "7", Title: "Ana"},
		{Type: "view", Model: "customers", Search: "ana", ModelTitle: "Clientes"},
	})
	for _, w := range []string{"### Archivo adjunto: t.csv", "1\t2", "[imagen adjunta: p.png", "(p. ej. data.image)", "[archivo adjunto: m.pdf url=/storage/aby/missing.pdf]", "[registro compartido: Ana · model=customers id=7", "[vista compartida: Clientes"} {
		if !strings.Contains(msg, w) {
			t.Errorf("missing %q in:\n%s", w, msg)
		}
	}
	if len(imgs) != 1 || imgs[0] != filepath.Join("storage", "public", "aby", "p.png") || len(urls) != 1 || urls[0] != "/storage/aby/p.png" {
		t.Fatalf("imgs=%v urls=%v", imgs, urls)
	}

	out, _, _ := EnrichMessageWithAttachmentsOpts("x", []ChatAttachment{{Type: "record", Model: "stock", ID: "1", Title: "S"}, {URL: "/storage/aby/p.png", MimeType: "image/png"}},
		AttachmentOptions{
			RecordHint:     func(a ChatAttachment) string { return "HINT-" + a.Model },
			ImageFieldHint: " [campo image]",
		})
	if !strings.Contains(out, "HINT-stock") || !strings.Contains(out, "[campo image]") {
		t.Fatalf("hooks not applied: %s", out)
	}
	if got, _, _ := EnrichMessageWithAttachments("solo texto", nil); got != "solo texto" {
		t.Fatal("no attachments must leave the message alone")
	}
}

func TestLastUserAttachments(t *testing.T) {
	meta := json.RawMessage(`{"attachments":[{"url":"/storage/a.png","mime_type":"image/png"}]}`)
	store := &fakeConvStore{rows: []json.RawMessage{json.RawMessage(`{}`), json.RawMessage(`not json`), meta}}
	atts := LastUserAttachments(store, uuid.New())
	if len(atts) != 1 || atts[0].URL != "/storage/a.png" || store.lim != 12 {
		t.Fatalf("%v lim=%d", atts, store.lim)
	}
	if LastUserAttachments(nil, uuid.New()) != nil || LastUserAttachments(store, uuid.Nil) != nil {
		t.Fatal("nil store / nil conversation must yield nothing")
	}
	if LastUserAttachments(&fakeConvStore{err: errors.New("db")}, uuid.New()) != nil {
		t.Fatal("store errors must yield nothing")
	}
}

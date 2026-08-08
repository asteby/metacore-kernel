package importer

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/modelbase"
)

type memStore struct {
	files map[string][]byte
	ctype map[string]string
}

func (m *memStore) Put(_ context.Context, suggestedName string, r io.Reader, contentType string) (string, error) {
	if m.files == nil {
		m.files = map[string][]byte{}
		m.ctype = map[string]string{}
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	if len(data) > defaultMediaMaxBytes {
		return "", io.ErrUnexpectedEOF
	}
	name := suggestedName
	if name == "" {
		name = "blob"
	}
	m.files[name] = data
	m.ctype[name] = contentType
	return name, nil
}

func TestMediaURLTransformFetchesAndStores(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("fake-jpeg-bytes"))
	}))
	defer srv.Close()

	store := &memStore{}
	deps := &TransformDeps{HTTPClient: srv.Client(), Store: store}

	spec := modelbase.ImportSpec{
		Columns: []modelbase.ImportColumn{
			{Key: "user.name", Header: "Nombre", Required: true},
			{Key: "user.avatar", Header: "URL Foto", Transform: "media_url"},
		},
	}
	record, issues := BuildRecordWithDeps(spec, map[string]any{
		"Nombre":   "Dra. Ana",
		"URL Foto": srv.URL + "/avatar.jpg",
	}, deps, 0)
	if len(issues) > 0 {
		t.Fatalf("issues: %+v", issues)
	}
	user := record["user"].(map[string]any)
	if user["avatar"] != "avatar.jpg" {
		t.Fatalf("avatar: got %#v", user["avatar"])
	}
	if !bytes.Equal(store.files["avatar.jpg"], []byte("fake-jpeg-bytes")) {
		t.Fatalf("store content mismatch: %q", store.files["avatar.jpg"])
	}
}

func TestMediaURLListTransformJoinsFilenames(t *testing.T) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("png"))
	}))
	defer srv.Close()

	store := &memStore{}
	deps := &TransformDeps{HTTPClient: srv.Client(), Store: store}
	spec := modelbase.ImportSpec{
		Columns: []modelbase.ImportColumn{
			{Key: "name", Header: "Nombre", Required: true},
			{Key: "_gallery", Header: "URL Galería", Transform: "media_url_list"},
		},
	}
	record, issues := BuildRecordWithDeps(spec, map[string]any{
		"Nombre":      "X",
		"URL Galería": srv.URL + "/a.png | " + srv.URL + "/b.png",
	}, deps, 0)
	if len(issues) > 0 {
		t.Fatalf("issues: %+v", issues)
	}
	got, _ := record["_gallery"].(string)
	if got != "a.png|b.png" {
		t.Fatalf("gallery list: %q", got)
	}
	if n != 2 {
		t.Fatalf("expected 2 fetches, got %d", n)
	}
}

func TestMediaURLTransformRequiresStore(t *testing.T) {
	spec := modelbase.ImportSpec{
		Columns: []modelbase.ImportColumn{
			{Key: "user.name", Header: "Nombre", Required: true},
			{Key: "user.avatar", Header: "URL Foto", Transform: "media_url"},
		},
	}
	_, issues := BuildRecordWithDeps(spec, map[string]any{
		"Nombre":   "X",
		"URL Foto": "https://example.com/a.jpg",
	}, nil, 0)
	if len(issues) != 1 {
		t.Fatalf("want 1 issue, got %+v", issues)
	}
	if !strings.Contains(issues[0].Message, "TransformDeps.Store") {
		t.Fatalf("message: %s", issues[0].Message)
	}
}

func TestUnknownTransformFailsRow(t *testing.T) {
	spec := modelbase.ImportSpec{
		Columns: []modelbase.ImportColumn{
			{Key: "name", Header: "Nombre", Required: true, Transform: "no_such_xf"},
		},
	}
	_, issues := BuildRecord(spec, map[string]any{"Nombre": "X"})
	if len(issues) != 1 || !strings.Contains(issues[0].Message, "no registrado") {
		t.Fatalf("issues: %+v", issues)
	}
}

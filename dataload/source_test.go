package dataload

import (
	"context"
	"strings"
	"testing"
)

func TestCSVSource_BOMAndAccents(t *testing.T) {
	rows, err := NewCSVSource("a.csv", []byte("\xef\xbb\xbfAño,Nombre\n2026,Ana\n")).Rows(context.Background())
	if err != nil || len(rows) != 1 || rows[0]["Año"] != "2026" {
		t.Fatalf("got %v %v", rows, err)
	}
}

func TestJSONSource_ArrayAndDataEnvelope(t *testing.T) {
	for _, body := range []string{`[{"a":1}]`, `{"data":[{"a":1}]}`} {
		rows, err := NewJSONSource("x", []byte(body)).Rows(context.Background())
		if err != nil || len(rows) != 1 {
			t.Fatalf("%s: %v %v", body, rows, err)
		}
	}
}

func TestBundleSource_DottedPath(t *testing.T) {
	body := `{"catalog":{"items":[{"id":"1"},{"id":"2"}]}}`
	rows, err := NewBundleSource("b.json", []byte(body), "catalog.items").Rows(context.Background())
	if err != nil || len(rows) != 2 {
		t.Fatalf("got %v %v", rows, err)
	}
}

func TestBundleSource_BadPathsAreErrors(t *testing.T) {
	for path, want := range map[string]string{"nada": "no existe", "catalog": "no es un arreglo", "catalog.items.x": "no es un objeto"} {
		_, err := NewBundleSource("b", []byte(`{"catalog":{"items":[{"id":"1"}]}}`), path).Rows(context.Background())
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, quería %q", path, err, want)
		}
	}
	if _, err := NewBundleSource("b", []byte(`{"a":[1]}`), "a").Rows(context.Background()); err == nil {
		t.Error("un arreglo de escalares debía ser error")
	}
}

func TestSource_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewCSVSource("x", []byte("a\n1")).Rows(ctx); err == nil {
		t.Fatal("debía respetar el contexto cancelado")
	}
}

func TestInputHash_IndependentOfKeyOrderButNotRowOrder(t *testing.T) {
	a := []map[string]any{{"x": "1", "y": "2"}, {"x": "3"}}
	b := []map[string]any{{"y": "2", "x": "1"}, {"x": "3"}}
	c := []map[string]any{{"x": "3"}, {"x": "1", "y": "2"}}
	if inputHash(a) != inputHash(b) || inputHash(a) == inputHash(c) {
		t.Fatal("huella de entrada incorrecta")
	}
}

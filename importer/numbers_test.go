package importer

import (
	"errors"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/modelbase"
)

func TestNumberMixed_ReadsEveryUnambiguousFormat(t *testing.T) {
	ok := map[string]float64{
		"$1.059,32":     1059.32,
		"1.059,32":      1059.32,
		"1,059.32":      1059.32,
		"99,9":          99.9,
		"99.9":          99.9,
		"3.5":           3.5,
		"1.059.32":      1059.32, // mismo separador repetido, último bloque de 2: decimal
		"1,059,32":      1059.32,
		"1.059.320":     1059320, // bloques de 3: miles
		"1,059,320":     1059320,
		"1059":          1059,
		"0":             0,
		"148":           148,
		"MXN 1,200.50":  1200.5,
		"1,200.50 mxn":  1200.5,
		" $ 12.345,67 ": 12345.67,
		"-1.059,32":     -1059.32,
		"+7,5":          7.5,
		"1 059,32":      1059.32,
		"1 059":         1059,
		"0.059":         0.059, // un 0 inicial no puede ser un grupo de miles
		"0,500":         0.5,
		"1234.567":      1234.567, // 4 dígitos antes: no puede ser miles
		"3.14159":       3.14159,
		".5":            0.5,
		"1.5":           1.5,
		"1 059,32": 1059.32,
	}
	for in, want := range ok {
		got, err := ParseMixedNumber(in)
		if err != nil || got != want {
			t.Errorf("%q = %v, %v; quería %v", in, got, err, want)
		}
	}
}

func TestNumberMixed_AmbiguousIsAnErrorNeverAGuess(t *testing.T) {
	for _, in := range []string{"1.059", "1,059", "$12.345", "999,999", "$ 1.000"} {
		_, err := ParseMixedNumber(in)
		if !errors.Is(err, ErrNumberAmbiguous) {
			t.Errorf("%q debía ser ambiguo, got %v", in, err)
		}
	}
}

func TestNumberMixed_InvalidShapes(t *testing.T) {
	for _, in := range []string{"", "  ", "abc", "1.2.3.4x", "12,", ",", ".", "1,23.45,6", "1.05,3.4", "1.2345.678", "1..2", "1.059 234,5", "$", "--5", "1e5", "#N/D", "12 34", "1,2,3"} {
		_, err := ParseMixedNumber(in)
		if !errors.Is(err, ErrNumberInvalid) {
			t.Errorf("%q debía ser inválido, got %v", in, err)
		}
	}
}

func TestNumberMixed_IsRegisteredAndRunsBeforeTheTypeCheck(t *testing.T) {
	spec := modelbase.ImportSpec{Columns: []modelbase.ImportColumn{
		{Key: "sku", Header: "SKU", Required: true},
		{Key: "price", Header: "Precio", Type: "number", Transform: "number_mixed", Required: true},
	}}
	rec, issues := BuildRecord(spec, map[string]any{"SKU": "A1", "Precio": "$1.059,32"})
	if len(issues) != 0 || rec["price"] != 1059.32 {
		t.Fatalf("rec=%v issues=%v", rec, issues)
	}
	// Ambiguo: la fila falla con el motivo, no se escribe un valor adivinado.
	rec, issues = BuildRecord(spec, map[string]any{"SKU": "A2", "Precio": "1.059"})
	if rec != nil || len(issues) != 1 || issues[0].Column != "Precio" {
		t.Fatalf("rec=%v issues=%v", rec, issues)
	}
	if got := issues[0].Message; got == "" || !strings.Contains(got, "numero_ambiguo") {
		t.Fatalf("el motivo debe nombrar el código: %q", got)
	}
	// Los tipos normales siguen validándose antes de su transformación.
	_, issues = BuildRecord(modelbase.ImportSpec{Columns: []modelbase.ImportColumn{{Key: "n", Header: "N", Type: "number"}}}, map[string]any{"N": "1,5"})
	if len(issues) != 1 {
		t.Fatalf("un number sin transformación sigue exigiendo un número estricto: %v", issues)
	}
}

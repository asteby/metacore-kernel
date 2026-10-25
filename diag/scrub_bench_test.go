package diag

import (
	"fmt"
	"testing"
)

func settings(n int) map[string]string {
	m := make(map[string]string, n)
	for i := 0; i < n; i++ {
		m[fmt.Sprint("k", i)] = fmt.Sprintf("value-%06d-xxxx", i)
	}
	return m
}

// Ruta caliente: registrar ajustes en una invocacion exitosa no calcula variantes.
func BenchmarkScrubberAddSettings200(b *testing.B) {
	m := settings(200)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		NewScrubber().AddSettings(m)
	}
}

// Ruta de fallo: expandir y sanear 20 lineas con 200 ajustes.
func BenchmarkSanitizeBreadcrumbs200Settings(b *testing.B) {
	m := settings(200)
	line := "charging order for customer, step ok, retrying request after backoff "
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sc := NewScrubber()
		sc.AddSettings(m)
		d := Diagnostic{AddonKey: "acme", Code: "x"}
		for j := 0; j < 20; j++ {
			d.Breadcrumbs = append(d.Breadcrumbs, line)
		}
		Sanitize(d, sc)
	}
}

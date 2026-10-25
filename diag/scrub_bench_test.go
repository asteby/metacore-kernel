package diag

import (
	"fmt"
	"strings"
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

// ScrubText sobre entrada acotada: el costo por byte no debe crecer con el largo.
func BenchmarkScrubTextLen(b *testing.B) {
	for _, n := range []int{160, 320, 640, 1280, 2560} {
		line := strings.Repeat("1.1.1.1.", n/8)
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			sc := NewScrubber("zzzz-secret")
			b.SetBytes(int64(len(line)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				ScrubText(line, sc)
			}
		})
	}
}

// Caso adverso de la ruta de fallo: 20 lineas de ~8 KB (el tope de memoria de
// una linea es maxCrumbRaw; el resto se corta tras borrar por valor).
func BenchmarkSanitizeAdversarial20x8KB(b *testing.B) {
	line := strings.Repeat("1.1.1.1.", 1010)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		bag := NewBag()
		bag.AddSecrets("zzzz-secret")
		for j := 0; j < 20; j++ {
			bag.Log(line)
		}
		d := Diagnostic{AddonKey: "acme", Code: "x"}
		bag.Apply(&d)
		Sanitize(d, bag.Scrubber)
	}
}

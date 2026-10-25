package wasm

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// Costo de una invocacion EXITOSA con 20 ajustes: el diagnostico encendido debe
// quedar cerca del costo sin diagnostico (ruta caliente).
func benchInvokeOK(b *testing.B, withDiag bool) {
	// El guest de prueba tiene una memoria lineal fija y se agota tras ~2000
	// llamadas ("write 2 bytes @ 65536 out of range"): se instancia de nuevo cada
	// benchResetEvery llamadas, fuera del cronometro, para correr con el
	// benchtime por defecto.
	const benchResetEvery = 1000
	var f *diagFixture
	fresh := func() {
		if f != nil {
			_ = f.h.Close(context.Background())
		}
		f = newDiagFixture(b, 5000)
		if !withDiag {
			f.h.WithDiagnostics(nil)
		}
	}
	fresh()
	b.Cleanup(func() { _ = f.h.Close(context.Background()) })
	prev := newHTTPClient
	newHTTPClient = func() *http.Client {
		return &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) { return resp(200, `{}`), nil })}
	}
	b.Cleanup(func() { newHTTPClient = prev })
	settings := map[string]string{}
	for i := 0; i < 20; i++ {
		settings[fmt.Sprintf("k%02d_key", i)] = fmt.Sprintf("value-abcdef-%02d", i)
	}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i > 0 && i%benchResetEvery == 0 {
			b.StopTimer()
			fresh()
			b.StartTimer()
		}
		if _, err := f.h.InvokeFor(ctx, f.org, uuid.Nil, "diagaddon", "okafter", []byte(`{}`), settings); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkInvokeOK_DiagOff(b *testing.B) { benchInvokeOK(b, false) }
func BenchmarkInvokeOK_DiagOn(b *testing.B)  { benchInvokeOK(b, true) }

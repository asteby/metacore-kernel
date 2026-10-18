package importer

import (
	"bytes"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// wideHeaderCSV es la "bomba" de la revisión: encabezado de cols columnas y
// filas de una sola celda. Pesa poco en disco.
func wideHeaderCSV(cols, rows int) []byte {
	var b bytes.Buffer
	for i := 0; i < cols; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "c%d", i)
	}
	b.WriteByte('\n')
	for i := 0; i < rows; i++ {
		b.WriteString("x\n")
	}
	return b.Bytes()
}

func heapGrowth(f func()) uint64 {
	runtime.GC()
	var a, z runtime.MemStats
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&z)
	return z.TotalAlloc - a.TotalAlloc
}

// Antes cada fila reservaba un mapa del tamaño del encabezado: 1.5 MB de CSV
// pedían ~600 MB. Ahora el mapa es del tamaño de la fila.
func TestParseCSVWideHeaderDoesNotBlowUpMemory(t *testing.T) {
	body := wideHeaderCSV(200_000, 60)
	var rows []map[string]any
	grew := heapGrowth(func() {
		var err error
		rows, err = ParseCSV(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
	})
	if len(rows) != 60 || rows[0]["c0"] != "x" {
		t.Fatalf("filas = %d, %v", len(rows), rows[0])
	}
	if grew > 64<<20 {
		t.Fatalf("ParseCSV pidió %d MB para un archivo de %d KB", grew>>20, len(body)>>10)
	}
}

func TestParseCSVLimitedChecksHeaderBeforeRows(t *testing.T) {
	_, err := ParseCSVLimited(bytes.NewReader(wideHeaderCSV(200_000, 1000)), ParseLimits{MaxColumns: 512, MaxRows: 200_000})
	var pe ErrParseLimit
	if !errors.As(err, &pe) || pe.What != "columns" || pe.Row != 0 {
		t.Fatalf("esperaba tope de columnas en el encabezado, llegó %v", err)
	}
	_, err = ParseCSVLimited(strings.NewReader("a\n1\n2\n3\n"), ParseLimits{MaxRows: 2})
	if !errors.As(err, &pe) || pe.What != "rows" {
		t.Fatalf("esperaba tope de filas, llegó %v", err)
	}
	_, err = ParseCSVLimited(strings.NewReader("a\n"+strings.Repeat("z", 100)+"\n"), ParseLimits{MaxCellBytes: 10})
	if !errors.As(err, &pe) || pe.What != "cell_bytes" {
		t.Fatalf("esperaba tope de celda, llegó %v", err)
	}
	rows, err := ParseCSVLimited(strings.NewReader("\ufeffa,b\n1,2\n3\n"), ParseLimits{MaxColumns: 2, MaxRows: 2})
	if err != nil || len(rows) != 2 || rows[0]["a"] != "1" || rows[1]["b"] != nil {
		t.Fatalf("filas = %v, %v", rows, err)
	}
}

// `[{},{},...]` arma millones de mapas antes de contar con json.Unmarshal; la
// versión con topes corta en cuanto pasa MaxRows.
func TestParseJSONLimitedStopsAtMaxRows(t *testing.T) {
	body := []byte("[" + strings.TrimSuffix(strings.Repeat("{},", 2_000_000), ",") + "]")
	var err error
	grew := heapGrowth(func() { _, err = ParseJSONLimited(body, ParseLimits{MaxRows: 1000}) })
	var pe ErrParseLimit
	if !errors.As(err, &pe) || pe.What != "rows" {
		t.Fatalf("esperaba tope de filas, llegó %v", err)
	}
	if grew > 16<<20 {
		t.Fatalf("pidió %d MB antes de cortar", grew>>20)
	}
	rows, err := ParseJSONLimited([]byte(`{"meta":{"x":[1,2]},"data":[{"a":1.5,"b":"x"}]}`), ParseLimits{MaxRows: 10, MaxColumns: 2})
	if err != nil || len(rows) != 1 || rows[0]["a"] != 1.5 {
		t.Fatalf("sobre data: %v %v", rows, err)
	}
	if _, err := ParseJSONLimited([]byte(`[{"a":1,"b":2,"c":3}]`), ParseLimits{MaxColumns: 2}); !errors.As(err, &pe) || pe.What != "columns" {
		t.Fatalf("esperaba tope de columnas, llegó %v", err)
	}
	same, _ := ParseJSON([]byte(`[{"a":1,"n":{"k":2}}]`))
	lim, _ := ParseJSONLimited([]byte(`[{"a":1,"n":{"k":2}}]`), ParseLimits{})
	if fmt.Sprint(same) != fmt.Sprint(lim) {
		t.Fatalf("ParseJSONLimited difiere de ParseJSON: %v vs %v", lim, same)
	}
}

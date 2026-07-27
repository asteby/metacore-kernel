package importer

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/modelbase"
)

func doctorsLikeSpec() modelbase.ImportSpec {
	return modelbase.ImportSpec{
		Columns: []modelbase.ImportColumn{
			{Key: "user.name", Header: "Nombre completo", Required: true, Example: "Dra. Ana Pérez", Hint: "Nombre y apellidos"},
			{Key: "user.email", Header: "Email", Required: true, Type: "email", Aliases: []string{"Correo"}, Example: "ana@correo.com"},
			{Key: "user.password", Header: "Contraseña", Generator: "random_secret"},
			{Key: "years", Header: "Años", Type: "number"},
			{Key: "active", Header: "Activo", Type: "boolean"},
		},
		MaxRows:      10,
		Instructions: []string{"Una fila por doctor."},
	}
}

func TestBuildImportRecordResolvesAliasesAndDotPaths(t *testing.T) {
	record, issues := BuildRecord(doctorsLikeSpec(), map[string]any{
		"Nombre completo *": "Dr. Juan",
		"correo":            "juan@correo.com", // alias, lowercased
		"Años":              "12",
		"Activo":            "sí",
		"Columna extraña":   "se ignora",
	})

	if len(issues) > 0 {
		t.Fatalf("unexpected issues: %+v", issues)
	}
	user, ok := record["user"].(map[string]any)
	if !ok {
		t.Fatalf("dot-path must nest under user: %+v", record)
	}
	if user["email"] != "juan@correo.com" {
		t.Errorf("alias did not resolve: %+v", user)
	}
	if record["years"] != float64(12) {
		t.Errorf("number coercion: got %#v", record["years"])
	}
	if record["active"] != true {
		t.Errorf("boolean coercion: got %#v", record["active"])
	}
	if _, ok := record["Columna extraña"]; ok {
		t.Error("unknown columns must be dropped, not passed to Create")
	}
}

func TestBuildImportRecordRunsGeneratorForBlankCell(t *testing.T) {
	record, issues := BuildRecord(doctorsLikeSpec(), map[string]any{
		"Nombre completo": "Dr. Juan",
		"Email":           "juan@correo.com",
	})

	if len(issues) > 0 {
		t.Fatalf("unexpected issues: %+v", issues)
	}
	secret, _ := record["user"].(map[string]any)["password"].(string)
	if len(secret) != 32 {
		t.Fatalf("random_secret should fill the blank cell, got %q", secret)
	}
}

func TestBuildImportRecordReportsMissingRequiredAndBadTypes(t *testing.T) {
	_, issues := BuildRecord(doctorsLikeSpec(), map[string]any{
		"Email": "no-es-un-correo",
		"Años":  "doce",
	})

	if len(issues) != 3 {
		t.Fatalf("want 3 issues (missing name, bad email, bad number), got %d: %+v", len(issues), issues)
	}
	joined := ""
	for _, i := range issues {
		joined += i.Message + "|"
	}
	for _, want := range []string{"Nombre completo", "correo válido", "número"} {
		if !strings.Contains(joined, want) {
			t.Errorf("issues should mention %q: %s", want, joined)
		}
	}
}

func TestIsTemplateSampleRowSkipsTheGuideRows(t *testing.T) {
	spec := doctorsLikeSpec()

	example := map[string]any{"Nombre completo": "Dra. Ana Pérez", "Email": "ana@correo.com"}
	if !IsTemplateSampleRow(spec, example) {
		t.Error("the example row shipped in the template must be skipped")
	}
	hints := map[string]any{"Nombre completo": "Nombre y apellidos"}
	if !IsTemplateSampleRow(spec, hints) {
		t.Error("the hints row must be skipped")
	}
	real := map[string]any{"Nombre completo": "Dr. Juan", "Email": "juan@correo.com"}
	if IsTemplateSampleRow(spec, real) {
		t.Error("real data must NOT be skipped")
	}
}

func TestTemplateRoundTripsThroughTheParser(t *testing.T) {
	spec := doctorsLikeSpec()
	data, err := BuildTemplate(spec, "Médicos")
	if err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}

	rows, err := ParseXLSX(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseXLSX: %v", err)
	}
	// The template ships an example row and a hints row — both must parse and
	// both must be recognised as guide rows rather than data.
	if len(rows) != 2 {
		t.Fatalf("template should yield 2 guide rows, got %d: %+v", len(rows), rows)
	}
	for i, row := range rows {
		if !IsTemplateSampleRow(spec, row) {
			t.Errorf("guide row %d not recognised: %+v", i, row)
		}
	}
	// And every generated header must resolve back to its column.
	idx := spec.HeaderIndex()
	for header := range rows[0] {
		if _, ok := idx[normalizeHeader(header)]; !ok {
			t.Errorf("template header %q does not resolve back to a column", header)
		}
	}
}

func TestParseCSVAndXLSXAgreeOnTheSameData(t *testing.T) {
	csvRows, err := ParseCSV(strings.NewReader("Nombre completo,Email\nDr. Juan,juan@correo.com\n"))
	if err != nil {
		t.Fatalf("ParseCSV: %v", err)
	}
	if len(csvRows) != 1 || csvRows[0]["Email"] != "juan@correo.com" {
		t.Fatalf("csv parse: %+v", csvRows)
	}
}

func TestParseJSONBytesAcceptsArrayAndEnvelope(t *testing.T) {
	for _, body := range []string{
		`[{"Email":"a@b.com"}]`,
		`{"data":[{"Email":"a@b.com"}]}`,
	} {
		rows, err := ParseJSON([]byte(body))
		if err != nil {
			t.Fatalf("ParseJSON(%s): %v", body, err)
		}
		if len(rows) != 1 || rows[0]["Email"] != "a@b.com" {
			t.Errorf("ParseJSON(%s): %+v", body, rows)
		}
	}
}

func TestPrepareSeparatesValidRowsFromIssues(t *testing.T) {
	spec := doctorsLikeSpec()
	rows := []map[string]any{
		{"Nombre completo": "Dra. Ana Pérez", "Email": "ana@correo.com"}, // template example
		{"Nombre completo": "Dr. Juan", "Email": "juan@correo.com"},
		{"Nombre completo": "Dra. Eva"}, // missing required email
	}

	prepared, err := Prepare(spec, rows)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.Skipped != 1 {
		t.Errorf("Skipped: got %d want 1", prepared.Skipped)
	}
	if len(prepared.Records) != 1 {
		t.Fatalf("Records: got %d want 1", len(prepared.Records))
	}
	// Row numbers must point at the spreadsheet line, header included.
	if prepared.RowNumbers[0] != 3 {
		t.Errorf("RowNumbers: got %d want 3", prepared.RowNumbers[0])
	}
	if len(prepared.Issues) != 1 || prepared.Issues[0].Row != 4 {
		t.Errorf("Issues should point at row 4: %+v", prepared.Issues)
	}
}

func TestPrepareRejectsUploadsOverTheRowCap(t *testing.T) {
	spec := doctorsLikeSpec() // MaxRows: 10
	rows := make([]map[string]any, 11)
	for i := range rows {
		rows[i] = map[string]any{"Nombre completo": "Dr. X", "Email": "x@y.com"}
	}

	_, err := Prepare(spec, rows)

	var tooMany ErrTooManyRows
	if !errors.As(err, &tooMany) {
		t.Fatalf("want ErrTooManyRows, got %v", err)
	}
	if tooMany.Limit != 10 || tooMany.Got != 11 {
		t.Errorf("unexpected bounds: %+v", tooMany)
	}
}

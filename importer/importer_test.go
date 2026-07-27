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

func TestIsTemplateSampleRowUsesTheMarkerNotTheContent(t *testing.T) {
	spec := doctorsLikeSpec()

	marked := map[string]any{"Nombre completo": "Dra. Ana Pérez", TemplateMarkerHeader: "example"}
	if !IsTemplateSampleRow(spec, marked) {
		t.Error("a row carrying the template marker must be skipped")
	}

	// The critical case: a user whose real data happens to equal the example
	// must NOT be dropped. Matching on content used to discard it silently.
	lookalike := map[string]any{"Nombre completo": "Dra. Ana Pérez", "Email": "ana@correo.com"}
	if IsTemplateSampleRow(spec, lookalike) {
		t.Error("real data equal to the example must be imported, not discarded")
	}

	if IsTemplateSampleRow(spec, map[string]any{"Nombre completo": "Dr. Juan"}) {
		t.Error("an ordinary row must not be skipped")
	}
	// An emptied marker cell means the user reused the row for real data.
	if IsTemplateSampleRow(spec, map[string]any{"Nombre completo": "Dr. Juan", TemplateMarkerHeader: ""}) {
		t.Error("an empty marker must not skip the row")
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
	// And every generated header must resolve back to its column — except the
	// marker, which is bookkeeping rather than a data column.
	idx := spec.HeaderIndex()
	for header := range rows[0] {
		if normalizeHeader(header) == TemplateMarkerHeader {
			continue
		}
		if _, ok := idx[normalizeHeader(header)]; !ok {
			t.Errorf("template header %q does not resolve back to a column", header)
		}
	}
}

func TestParseCSVReadsHeadersAndRows(t *testing.T) {
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
		{"Nombre completo": "Dra. Ana Pérez", "Email": "ana@correo.com", TemplateMarkerHeader: "example"},
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

func TestRequiredIsEnforcedWhenTheGeneratorIsUnknown(t *testing.T) {
	spec := modelbase.ImportSpec{Columns: []modelbase.ImportColumn{
		{Key: "token", Header: "Token", Required: true, Generator: "typo_generator"},
	}}

	_, issues := BuildRecord(spec, map[string]any{})

	// A misspelled generator must not smuggle an empty required column past
	// validation and into an opaque database error later.
	if len(issues) != 1 {
		t.Fatalf("want the required column reported, got %+v", issues)
	}
}

func TestCSVRoundTripSurvivesExcelsBOM(t *testing.T) {
	spec := doctorsLikeSpec()

	// What Excel writes when it saves the downloaded template as CSV.
	csv := "\ufeffNombre completo *,Email *\nDr. Juan,juan@correo.com\n"
	rows, err := ParseCSV(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("ParseCSV: %v", err)
	}

	prepared, err := Prepare(spec, rows)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(prepared.Issues) != 0 {
		t.Fatalf("BOM broke header matching: %+v", prepared.Issues)
	}
	if len(prepared.Records) != 1 {
		t.Fatalf("want 1 record, got %d", len(prepared.Records))
	}
}

func TestWriteCSVWithBOMEmitsTheMarkExcelExpects(t *testing.T) {
	data, err := WriteCSVWithBOM([][]string{{"Años", "Contraseña"}})
	if err != nil {
		t.Fatalf("WriteCSVWithBOM: %v", err)
	}
	if !strings.HasPrefix(string(data), "\ufeff") {
		t.Error("CSV must start with the UTF-8 BOM or Excel mangles accents")
	}

	// And what we write must feed straight back into the parser.
	rows, err := ParseCSV(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseCSV: %v", err)
	}
	_ = rows
	headerRow, err := ParseCSV(strings.NewReader(string(data) + "12,secreto\n"))
	if err != nil {
		t.Fatalf("ParseCSV with data: %v", err)
	}
	if _, ok := headerRow[0]["Años"]; !ok {
		t.Errorf("accented header lost through the round-trip: %+v", headerRow[0])
	}
}

func TestDateCellsAreNormalisedAndAmbiguityRejected(t *testing.T) {
	spec := modelbase.ImportSpec{Columns: []modelbase.ImportColumn{
		{Key: "born_on", Header: "Fecha", Type: "date"},
	}}

	record, issues := BuildRecord(spec, map[string]any{"Fecha": "2026-04-03"})
	if len(issues) != 0 {
		t.Fatalf("ISO date rejected: %+v", issues)
	}
	if record["born_on"] != "2026-04-03" {
		t.Errorf("date should reach Create normalised, got %#v", record["born_on"])
	}

	// D/M/Y vs M/D/Y cannot be told apart, so it is refused rather than guessed.
	if _, issues := BuildRecord(spec, map[string]any{"Fecha": "03/04/2026"}); len(issues) != 1 {
		t.Errorf("ambiguous date must be rejected, got %+v", issues)
	}
}

func TestReadCappedRejectsRatherThanTruncating(t *testing.T) {
	// One byte over the limit. A truncating reader would return 16 MiB of
	// perfectly parseable CSV and silently drop the rest.
	oversized := bytes.Repeat([]byte("a"), int(MaxUploadBytes)+1)

	_, err := readCapped(bytes.NewReader(oversized))

	var tooLarge ErrUploadTooLarge
	if !errors.As(err, &tooLarge) {
		t.Fatalf("want ErrUploadTooLarge, got %v", err)
	}

	// Exactly at the limit is still accepted.
	atLimit := bytes.Repeat([]byte("a"), int(MaxUploadBytes))
	if _, err := readCapped(bytes.NewReader(atLimit)); err != nil {
		t.Errorf("a payload exactly at the limit must be accepted: %v", err)
	}
}

func TestRedactGeneratedKeepsSecretsOutOfThePreview(t *testing.T) {
	spec := doctorsLikeSpec() // user.password carries the random_secret generator
	prepared, err := Prepare(spec, []map[string]any{
		{"Nombre completo": "Dr. Juan", "Email": "juan@correo.com"},
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	preview := RedactGenerated(spec, prepared.Records)

	user := preview[0]["user"].(map[string]any)
	if user["password"] != "(generado)" {
		t.Errorf("generated secret leaked into the preview: %#v", user["password"])
	}
	// What the user typed stays visible — that is what makes a preview useful.
	if user["email"] != "juan@correo.com" {
		t.Errorf("user-supplied value should survive redaction: %#v", user["email"])
	}
	// And redaction must not corrupt the record about to be created.
	original := prepared.Records[0]["user"].(map[string]any)
	if len(original["password"].(string)) != 32 {
		t.Error("redaction mutated the record instead of copying it")
	}
}

func TestFlattenProducesDotPathKeys(t *testing.T) {
	flat := Flatten(map[string]any{
		"membership_level": "basic",
		"user":             map[string]any{"name": "Ana", "email": "ana@correo.com"},
	})

	if flat["user.name"] != "Ana" || flat["user.email"] != "ana@correo.com" {
		t.Fatalf("nested keys were not flattened: %+v", flat)
	}
	if flat["membership_level"] != "basic" {
		t.Errorf("top-level key lost: %+v", flat)
	}
	if _, still := flat["user"]; still {
		t.Error("the nested map must not survive alongside its flattened keys")
	}
}

func TestDefaultsReachEveryRecordButNeverOverrideACell(t *testing.T) {
	spec := modelbase.ImportSpec{
		Columns:  []modelbase.ImportColumn{{Key: "user.name", Header: "Nombre", Required: true}},
		Defaults: map[string]any{"user.role": "patient"},
	}

	record, issues := BuildRecord(spec, map[string]any{"Nombre": "Ana"})
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %+v", issues)
	}
	user := record["user"].(map[string]any)
	if user["role"] != "patient" {
		t.Errorf("hidden default did not reach the record: %+v", user)
	}

	// A default must never win over a value the file actually carries.
	spec.Columns = append(spec.Columns, modelbase.ImportColumn{Key: "user.role", Header: "Rol"})
	record, _ = BuildRecord(spec, map[string]any{"Nombre": "Ana", "Rol": "admin"})
	if got := record["user"].(map[string]any)["role"]; got != "admin" {
		t.Errorf("explicit cell must win over the default, got %#v", got)
	}
}

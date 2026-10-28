package batch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/asteby/metacore-kernel/dataload"
	"github.com/asteby/metacore-kernel/provenance"
	"github.com/google/uuid"
)

var scope = Scope{OrgID: uuid.MustParse("11111111-1111-1111-1111-111111111111"), Kind: "export"}

// mkBatch builds a directory with the given files and a manifest listing
// them (sha256 computed), generated at gen.
func mkBatch(files map[string]string, gen string) fstest.MapFS {
	fsys := fstest.MapFS{}
	m := Manifest{Files: map[string]string{}, GeneratedAt: gen, Producer: "test", Schema: "export/v1"}
	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
		m.Files[name] = sha([]byte(body))
	}
	raw, _ := json.Marshal(m)
	fsys["manifest.json"] = &fstest.MapFile{Data: raw}
	return fsys
}

type calls struct{ order []string }

func (c *calls) spec(name string, optional bool) FileSpec {
	return FileSpec{Name: name, Optional: optional, Import: func(_ context.Context, f File) (FileReport, error) {
		c.order = append(c.order, f.Name)
		return FileReport{Counts: map[string]int{"rows": len(f.Body)}, Warnings: nil}, nil
	}}
}

func contract(c *calls) Contract {
	return Contract{Files: []FileSpec{c.spec("a.json", false), c.spec("b.json", false), c.spec("geo.json", true)}}
}

func TestFullBatchImportsInOrder(t *testing.T) {
	c := &calls{}
	l := NewMemoryLedger()
	fsys := mkBatch(map[string]string{"b.json": "bb", "a.json": "a", "geo.json": "ggg"}, "2026-10-01T10:00:00Z")
	res, err := Process(context.Background(), fsys, contract(c), l, scope)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Imported || res.Skipped || strings.Join(c.order, ",") != "a.json,b.json,geo.json" {
		t.Fatalf("res=%+v order=%v", res, c.order)
	}
	if res.Counts["rows"] != 6 || res.Counts["files"] != 3 {
		t.Fatalf("counts = %v", res.Counts)
	}
	recs := l.Records(scope)
	if len(recs) != 1 || recs[0].ManifestSHA != res.ManifestSHA || recs[0].GeneratedAt == nil || recs[0].SameContent || len(recs[0].Files) != 3 {
		t.Fatalf("records = %+v", recs)
	}
	// the very same manifest is not processed twice
	res, err = Process(context.Background(), fsys, contract(c), l, scope)
	if err != nil || !res.Skipped || res.Imported || len(c.order) != 3 {
		t.Fatalf("second pass: %+v %v order=%v", res, err, c.order)
	}
}

func TestNoManifestIsNothing(t *testing.T) {
	c := &calls{}
	res, err := Process(context.Background(), fstest.MapFS{"a.json": {Data: []byte("x")}}, contract(c), NewMemoryLedger(), scope)
	if err != nil || res.Imported || res.Skipped || res.ManifestSHA != "" || len(c.order) != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestSHAMismatchDoesNotRecordAndRetries(t *testing.T) {
	c := &calls{}
	l := NewMemoryLedger()
	fsys := mkBatch(map[string]string{"a.json": "a", "b.json": "bb"}, "")
	fsys["b.json"] = &fstest.MapFile{Data: []byte("half written")}
	_, err := Process(context.Background(), fsys, contract(c), l, scope)
	if dataload.ErrorCode(err) != dataload.CodeBatchSHA256Mismatch {
		t.Fatalf("err = %v", err)
	}
	if len(c.order) != 0 {
		t.Fatalf("a.json was imported although b.json failed verification: %v", c.order)
	}
	if len(l.Records(scope)) != 0 {
		t.Fatal("a failed batch was recorded")
	}
	// the producer finishes the write: next revision imports
	fsys["b.json"] = &fstest.MapFile{Data: []byte("bb")}
	res, err := Process(context.Background(), fsys, contract(c), l, scope)
	if err != nil || !res.Imported {
		t.Fatalf("retry: %+v %v", res, err)
	}
}

func TestSameContentWithNewGeneratedAt(t *testing.T) {
	c := &calls{}
	l := NewMemoryLedger()
	files := map[string]string{"a.json": "a", "b.json": "bb"}
	if _, err := Process(context.Background(), mkBatch(files, "2026-10-01T10:00:00Z"), contract(c), l, scope); err != nil {
		t.Fatal(err)
	}
	res, err := Process(context.Background(), mkBatch(files, "2026-10-02T10:00:00Z"), contract(c), l, scope)
	if err != nil || !res.Skipped || !res.SameContent || res.Imported || len(c.order) != 2 {
		t.Fatalf("res=%+v err=%v order=%v", res, err, c.order)
	}
	recs := l.Records(scope)
	if len(recs) != 2 || !recs[1].SameContent {
		t.Fatalf("records = %+v", recs)
	}
	// and the new manifest is now Done: no recomputation
	res, _ = Process(context.Background(), mkBatch(files, "2026-10-02T10:00:00Z"), contract(c), l, scope)
	if !res.Skipped || res.SameContent {
		t.Fatalf("third pass: %+v", res)
	}
	// real change: imports again
	res, err = Process(context.Background(), mkBatch(map[string]string{"a.json": "a2", "b.json": "bb"}, "2026-10-03T10:00:00Z"), contract(c), l, scope)
	if err != nil || !res.Imported || len(c.order) != 4 {
		t.Fatalf("changed batch: %+v %v", res, err)
	}
}

func TestWarnings(t *testing.T) {
	c := &calls{}
	// unknown file in the manifest -> warning; optional absent -> none; required absent -> warning
	fsys := mkBatch(map[string]string{"a.json": "a", "extra.json": "e"}, "")
	res, err := Process(context.Background(), fsys, contract(c), NewMemoryLedger(), scope)
	if err != nil || !res.Imported {
		t.Fatalf("%+v %v", res, err)
	}
	var codes []dataload.Code
	for _, w := range res.Warnings {
		codes = append(codes, w.Code)
	}
	want := []dataload.Code{dataload.CodeBatchUnknownFile, dataload.CodeBatchMissingFile}
	if fmt.Sprint(codes) != fmt.Sprint(want) {
		t.Fatalf("warning codes = %v, want %v", codes, want)
	}
	if res.Warnings[0].Params["file"] != "extra.json" || res.Warnings[1].Params["file"] != "b.json" {
		t.Fatalf("warnings = %+v", res.Warnings)
	}
	if got := res.Warnings[1].Text("en"); !strings.Contains(got, "b.json") {
		t.Fatalf("localized text %q", got)
	}
}

func TestOptionalAbsentNoWarning(t *testing.T) {
	c := &calls{}
	res, err := Process(context.Background(), mkBatch(map[string]string{"a.json": "a", "b.json": "b"}, ""), contract(c), NewMemoryLedger(), scope)
	if err != nil || len(res.Warnings) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestMaliciousNamesAreNeverOpened(t *testing.T) {
	c := &calls{}
	fsys := mkBatch(map[string]string{"a.json": "a", "b.json": "b"}, "")
	var m Manifest
	_ = json.Unmarshal(fsys["manifest.json"].Data, &m)
	for _, bad := range []string{"../secret", "/etc/passwd", "sub/dir.json", `..\x`, ".."} {
		m.Files[bad] = sha([]byte("x"))
	}
	raw, _ := json.Marshal(m)
	fsys["manifest.json"] = &fstest.MapFile{Data: raw}
	opened := map[string]bool{}
	res, err := Process(context.Background(), spyFS{fsys, opened}, contract(c), NewMemoryLedger(), scope)
	if err != nil || !res.Imported {
		t.Fatalf("%+v %v", res, err)
	}
	for name := range opened {
		if name != "manifest.json" && name != "a.json" && name != "b.json" {
			t.Errorf("opened %q", name)
		}
	}
	unknown := 0
	for _, w := range res.Warnings {
		if w.Code == dataload.CodeBatchUnknownFile {
			unknown++
		}
	}
	if unknown != 5 {
		t.Fatalf("unknown warnings = %d, want 5", unknown)
	}
}

type spyFS struct {
	fstest.MapFS
	opened map[string]bool
}

func (s spyFS) Open(name string) (fs.File, error) { s.opened[name] = true; return s.MapFS.Open(name) }

func TestSizeLimits(t *testing.T) {
	c := &calls{}
	fsys := mkBatch(map[string]string{"a.json": "0123456789", "b.json": "b"}, "")
	ct := contract(c)
	ct.Files[0].MaxBytes = 5
	_, err := Process(context.Background(), fsys, ct, NewMemoryLedger(), scope)
	if dataload.ErrorCode(err) != dataload.CodeBatchTooLarge {
		t.Fatalf("err = %v", err)
	}
	if len(c.order) != 0 {
		t.Fatal("imported despite the size cap")
	}
	// global cap, and a manifest over its cap
	ct = contract(c)
	ct.MaxFileBytes = 5
	if _, err = Process(context.Background(), fsys, ct, NewMemoryLedger(), scope); dataload.ErrorCode(err) != dataload.CodeBatchTooLarge {
		t.Fatalf("global cap: %v", err)
	}
	ct = contract(c)
	ct.MaxManifestBytes = 10
	if _, err = Process(context.Background(), fsys, ct, NewMemoryLedger(), scope); dataload.ErrorCode(err) != dataload.CodeBatchManifestRead {
		t.Fatalf("manifest cap: %v", err)
	}
}

func TestImportFailureMidwayReimportsEverything(t *testing.T) {
	l := NewMemoryLedger()
	fail := true
	var order []string
	ct := Contract{Files: []FileSpec{
		{Name: "a.json", Import: func(_ context.Context, f File) (FileReport, error) {
			order = append(order, "a")
			return FileReport{}, nil
		}},
		{Name: "b.json", Import: func(_ context.Context, f File) (FileReport, error) {
			order = append(order, "b")
			if fail {
				return FileReport{}, errors.New("db down")
			}
			return FileReport{}, nil
		}},
		{Name: "c.json", Import: func(_ context.Context, f File) (FileReport, error) {
			order = append(order, "c")
			return FileReport{}, nil
		}},
	}}
	fsys := mkBatch(map[string]string{"a.json": "a", "b.json": "b", "c.json": "c"}, "")
	_, err := Process(context.Background(), fsys, ct, l, scope)
	if dataload.ErrorCode(err) != dataload.CodeBatchImportFailed || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("err = %v", err)
	}
	if strings.Join(order, "") != "ab" || len(l.Records(scope)) != 0 {
		t.Fatalf("order=%v records=%d", order, len(l.Records(scope)))
	}
	fail, order = false, nil
	res, err := Process(context.Background(), fsys, ct, l, scope)
	if err != nil || !res.Imported || strings.Join(order, "") != "abc" {
		t.Fatalf("retry: %+v %v order=%v", res, err, order)
	}
}

func TestInvalidManifests(t *testing.T) {
	c := &calls{}
	for name, body := range map[string]string{"not json": "{", "no files": `{"files":{}}`, "null": `null`} {
		fsys := fstest.MapFS{"manifest.json": {Data: []byte(body)}}
		_, err := Process(context.Background(), fsys, contract(c), NewMemoryLedger(), scope)
		code := dataload.ErrorCode(err)
		if code != dataload.CodeBatchManifestInvalid && code != dataload.CodeBatchManifestEmpty {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestContractValidation(t *testing.T) {
	for name, ct := range map[string]Contract{
		"nested":    {Files: []FileSpec{{Name: "sub/a.json"}}},
		"dotdot":    {Files: []FileSpec{{Name: ".."}}},
		"empty":     {Files: []FileSpec{{Name: ""}}},
		"duplicate": {Files: []FileSpec{{Name: "a"}, {Name: "a"}}},
		"manifest":  {Files: []FileSpec{{Name: "manifest.json"}}},
		"bad mname": {ManifestName: "x/m.json"},
	} {
		if _, err := Process(context.Background(), fstest.MapFS{}, ct, NewMemoryLedger(), scope); dataload.ErrorCode(err) != dataload.CodeBatchContractInvalid {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestCustomManifestNameAndProvenance(t *testing.T) {
	var got provenance.Provenance
	var gotOK bool
	ct := Contract{
		ManifestName: "done.json",
		Files: []FileSpec{{Name: "a.json", Import: func(ctx context.Context, f File) (FileReport, error) {
			got, gotOK = provenance.FromContext(ctx)
			return FileReport{Warnings: []string{"fila 3 ignorada"}}, nil
		}}},
		Provenance: func(file string, b BatchInfo) provenance.Provenance {
			return provenance.Integration("data-sync", provenance.ViaImport, file+"@"+b.ManifestSHA[:6])
		},
	}
	fsys := mkBatch(map[string]string{"a.json": "a"}, "")
	fsys["done.json"] = fsys["manifest.json"]
	delete(fsys, "manifest.json")
	res, err := Process(context.Background(), fsys, ct, NewMemoryLedger(), scope)
	if err != nil || !res.Imported {
		t.Fatalf("%+v %v", res, err)
	}
	if !gotOK || got.ActorName != "data-sync" {
		t.Fatalf("provenance not in the import context: %+v ok=%v", got, gotOK)
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Code != dataload.CodeBatchFileWarning || res.Warnings[0].Params["text"] != "fila 3 ignorada" {
		t.Fatalf("warnings = %+v", res.Warnings)
	}
}

func TestContentPrintIgnoresOrderAndCase(t *testing.T) {
	a := ContentPrint(map[string]string{"x": "ABC", "y": "def"})
	b := ContentPrint(map[string]string{"y": " DEF ", "x": "abc"})
	if a != b {
		t.Fatal("ContentPrint is not canonical")
	}
	if a == ContentPrint(map[string]string{"x": "abc", "y": "dee"}) {
		t.Fatal("ContentPrint ignores content")
	}
}

func TestContextCanceledStopsImport(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	l := NewMemoryLedger()
	n := 0
	ct := Contract{Files: []FileSpec{
		{Name: "a.json", Import: func(context.Context, File) (FileReport, error) { n++; cancel(); return FileReport{}, nil }},
		{Name: "b.json", Import: func(context.Context, File) (FileReport, error) { n++; return FileReport{}, nil }},
	}}
	_, err := Process(ctx, mkBatch(map[string]string{"a.json": "a", "b.json": "b"}, ""), ct, l, scope)
	if !errors.Is(err, context.Canceled) || n != 1 || len(l.Records(scope)) != 0 {
		t.Fatalf("err=%v n=%d", err, n)
	}
}

func TestScopesAreIndependent(t *testing.T) {
	c := &calls{}
	l := NewMemoryLedger()
	fsys := mkBatch(map[string]string{"a.json": "a", "b.json": "b"}, "")
	other := Scope{OrgID: uuid.New(), Kind: "export"}
	for _, s := range []Scope{scope, other} {
		res, err := Process(context.Background(), fsys, contract(c), l, s)
		if err != nil || !res.Imported {
			t.Fatalf("scope %v: %+v %v", s, res, err)
		}
	}
}

func TestSourceFor(t *testing.T) {
	f := File{Name: "p.csv", Body: []byte("id,name\n1,Ana\n2,Beto\n")}
	rows, err := SourceFor(f, "csv").Rows(context.Background())
	if err != nil || len(rows) != 2 || rows[0]["name"] != "Ana" {
		t.Fatalf("csv: %v %v", rows, err)
	}
	rows, err = SourceFor(File{Name: "p.json", Body: []byte(`[{"id":1}]`)}, "json").Rows(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("json: %v %v", rows, err)
	}
	rows, err = SourceFor(File{Name: "b.json", Body: []byte(`{"catalog":{"items":[{"id":1},{"id":2}]}}`)}, "bundle:catalog.items").Rows(context.Background())
	if err != nil || len(rows) != 2 {
		t.Fatalf("bundle: %v %v", rows, err)
	}
	src := SourceFor(f, "xml")
	if src.Name() != "batch:p.csv" {
		t.Fatalf("name = %q", src.Name())
	}
	if _, err := src.Rows(context.Background()); dataload.ErrorCode(err) != dataload.CodeBatchFormat {
		t.Fatalf("unknown format: %v", err)
	}
}

func TestParseTime(t *testing.T) {
	for _, s := range []string{"2026-10-01T10:00:00Z", "2026-10-01T10:00:00.123456789-06:00", "2026-10-01T10:00:00", "2026-10-01"} {
		if _, ok := parseTime(s); !ok {
			t.Errorf("%q not parsed", s)
		}
	}
	if _, ok := parseTime("ayer"); ok {
		t.Error("garbage parsed")
	}
	var zero time.Time
	if tt, ok := parseTime(""); ok || tt != zero {
		t.Error("empty parsed")
	}
}

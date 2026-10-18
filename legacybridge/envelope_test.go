package legacybridge

import (
	"strings"
	"testing"
)

func TestEnvelope_ValidateReportsEveryProblem(t *testing.T) {
	err := Envelope{Op: "merge", Facets: []Facet{{}}, Requires: []Dependency{{Entity: "x"}}}.Validate()
	if err == nil {
		t.Fatal("envelope vacío debe fallar")
	}
	for _, want := range []string{"source", "entity", "external_id", "source_version", "op", "facets[0]", "requires[0]"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("falta %q en %v", want, err)
		}
	}
	ok := Envelope{Source: "erp", Entity: "customers", ExternalID: "erp:clients:1", Op: OpUpsert, SourceVersion: ver(1, "a"),
		Aggregate: Aggregate{Root: map[string]any{"name": "A"}}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	ok.Aggregate.Root = nil
	if ok.Validate() == nil {
		t.Fatal("upsert sin root debe fallar")
	}
	ok.Op = OpDelete
	if err := ok.Validate(); err != nil {
		t.Fatalf("delete no necesita root: %v", err)
	}
}

func TestEnvelope_HashIgnoresVersionAndEmissionButNotContent(t *testing.T) {
	a := Envelope{Source: "erp", Entity: "e", ExternalID: "1", Op: OpUpsert, SourceVersion: ver(1, "x"),
		Aggregate: Aggregate{Root: map[string]any{"b": 2, "a": 1}, Children: map[string][]map[string]any{"lines": {{"q": 1}}}}}
	b := a
	b.SourceVersion = ver(9, "y")
	b.Aggregate.Root = map[string]any{"a": 1, "b": 2}
	if a.Hash() != b.Hash() {
		t.Fatal("la huella no debe depender de la versión ni del orden de claves")
	}
	b.Aggregate.Children = map[string][]map[string]any{"lines": {{"q": 2}}}
	if a.Hash() == b.Hash() {
		t.Fatal("un hijo distinto cambia la huella")
	}
}

func TestParseNDJSON(t *testing.T) {
	in := `{"source":"erp","entity":"e","external_id":"1","op":"upsert","source_version":"v1","aggregate":{"root":{"n":1.50}}}

{"source":"erp","entity":"e","external_id":"2","op":"delete","source_version":"v1"}
`
	got, err := ParseNDJSON(strings.NewReader(in))
	if err != nil || len(got) != 2 {
		t.Fatalf("got %d, %v", len(got), err)
	}
	if got[0].Aggregate.Root["n"].(interface{ String() string }).String() != "1.50" {
		t.Fatal("los números se conservan como json.Number (sin perder decimales)")
	}
	if _, err := ParseNDJSON(strings.NewReader("{nope}\n")); err == nil || !strings.Contains(err.Error(), "línea 1") {
		t.Fatalf("línea inválida: %v", err)
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		cand, stored string
		want         VersionOrder
	}{
		{ver(2, "a"), "", VersionNewer},
		{ver(2, "a"), ver(2, "a"), VersionSame},
		{ver(3, "a"), ver(2, "b"), VersionNewer},
		{ver(1, "a"), ver(2, "b"), VersionStale},
		{ver(2, "b"), ver(2, "a"), VersionNewer}, // mismo tick, otro contenido: se aplica
		{"2026-10-18T10:00:02.5Z#a", "2026-10-18T10:00:02Z#a", VersionNewer},
		{"b", "a", VersionNewer}, // no RFC3339: texto
	}
	for _, c := range cases {
		if got := CompareVersions(c.cand, c.stored); got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, quiero %d", c.cand, c.stored, got, c.want)
		}
	}
}

func TestSystemOf(t *testing.T) {
	if SystemOf("erp:clients:8") != "erp" || SystemOf("8") != "" {
		t.Fatal("SystemOf")
	}
}

func TestResolveOwner_MostSpecificRuleWins(t *testing.T) {
	rules := []OwnershipRule{
		{Entity: "*", Branch: "*", Phase: "*", Owner: OwnerLegacy},
		{Entity: "*", Branch: "b5", Owner: OwnerLocal},                    // sucursal cortada
		{Entity: "products", Branch: "", Owner: OwnerLegacy},              // maestros siguen en el legado
		{Entity: "crm.Lead", Owner: OwnerLocal},                           // CRM ya es del host
		{Entity: "products", Branch: "*", Phase: "F3", Owner: OwnerLocal}, // en F3 se mueven los maestros
	}
	cases := []struct {
		entity, branch, phase string
		want                  Owner
	}{
		{"sales.SalesOrder", "b1", "F1", OwnerLegacy},
		{"sales.SalesOrder", "b5", "F1", OwnerLocal},
		{"products", "b5", "F1", OwnerLegacy}, // entidad pesa más que sucursal
		{"products", "b5", "F3", OwnerLocal},
		{"crm.Lead", "b1", "F0", OwnerLocal},
	}
	for _, c := range cases {
		if got := ResolveOwner(rules, c.entity, c.branch, c.phase, OwnerLegacy); got != c.want {
			t.Errorf("%s/%s/%s = %s, quiero %s", c.entity, c.branch, c.phase, got, c.want)
		}
	}
	if ResolveOwner(nil, "x", "", "", OwnerLocal) != OwnerLocal {
		t.Fatal("sin reglas devuelve el default")
	}
	tie := []OwnershipRule{{Entity: "x", Owner: OwnerLegacy}, {Entity: "x", Owner: OwnerLocal}}
	if ResolveOwner(tie, "x", "", "", OwnerLegacy) != OwnerLocal {
		t.Fatal("en empate gana local")
	}
}

func TestOwnershipRule_Normalize(t *testing.T) {
	r, err := OwnershipRule{System: "erp", Entity: " ", Owner: OwnerLocal}.Normalize()
	if err != nil || r.Entity != "*" || r.Branch != "*" || r.Phase != "*" {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := (OwnershipRule{System: "erp", Owner: "both"}).Normalize(); err == nil {
		t.Fatal("dueño inválido")
	}
	if _, err := (OwnershipRule{Owner: OwnerLocal}).Normalize(); err == nil {
		t.Fatal("sin system")
	}
}

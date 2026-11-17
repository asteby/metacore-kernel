package webhooks

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// --- helpers --------------------------------------------------------------

func mustJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("bad json %q: %v", s, err)
	}
	return m
}

func sampleEvent(t *testing.T) *Event {
	return &Event{
		ID: "evt-1", Name: "customers.customer.created", OrgID: uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		OccurredAt: time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC), AggregateType: "customers.customer", AggregateID: "cus-1",
		Sequence: 3, Source: "kernel", BranchID: "br-1",
		Payload: mustJSON(t, `{"id":"cus-1","name":"  Ana  ","email":"ANA@Example.COM","age":31,"vip":true,
			"channel":"web","address":{"city":"CDMX"},"items":[{"sku":"A1"},{"sku":"B2"}],"note":null}`),
	}
}

func renderBody(t *testing.T, tr *Transform, ev *Event) (map[string]any, error) {
	t.Helper()
	c, err := compileTransform(tr)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return c.render(ev.view())
}

// --- transform ------------------------------------------------------------

func TestTransformSubstitutionConstantsTablesAndFunctions(t *testing.T) {
	tr := &Transform{
		Body: map[string]any{
			"external_reference": "{{event.aggregate_id}}",
			"name":               "{{payload.name | trim}}",
			"email":              "{{payload.email | lower}}",
			"city_upper":         "{{payload.address.city | upper}}",
			"age":                "{{payload.age}}", // tipo conservado: numero
			"vip":                "{{payload.vip}}", // tipo conservado: bool
			"address":            "{{payload.address}}",
			"channel":            "{{payload.channel | map:channels}}",
			"label":              "Cliente {{payload.name | trim}} ({{event.name}})",
			"first_sku":          "{{payload.items.0.sku}}",
			"nick":               "{{payload.nick | default:anon}}",
			"note":               "{{payload.note | default:sin nota}}",
			"missing":            "{{payload.nope}}",
			"currency":           "MXN",
			"fixed_n":            float64(7),
			"fixed_b":            true,
			"nested":             map[string]any{"seq": "{{event.sequence}}", "list": []any{"{{event.id}}", "k"}},
		},
		Tables: map[string]map[string]string{"channels": {"web": "b2c", "pos": "retail"}},
	}
	got, err := renderBody(t, tr, sampleEvent(t))
	if err != nil {
		t.Fatal(err)
	}
	want := mustJSON(t, `{
		"external_reference":"cus-1","name":"Ana","email":"ana@example.com","city_upper":"CDMX",
		"age":31,"vip":true,"address":{"city":"CDMX"},"channel":"b2c",
		"label":"Cliente Ana (customers.customer.created)","first_sku":"A1","nick":"anon","note":"sin nota",
		"missing":null,"currency":"MXN","fixed_n":7,"fixed_b":true,
		"nested":{"seq":3,"list":["evt-1","k"]}}`)
	if !reflect.DeepEqual(got, want) {
		gb, _ := json.MarshalIndent(got, "", " ")
		wb, _ := json.MarshalIndent(want, "", " ")
		t.Fatalf("got:\n%s\nwant:\n%s", gb, wb)
	}
}

func TestTransformTableFallbackAndMiss(t *testing.T) {
	tr := &Transform{
		Body:   map[string]any{"c": "{{payload.channel | map:ch}}"},
		Tables: map[string]map[string]string{"ch": {"pos": "retail", "*": "other"}},
	}
	got, err := renderBody(t, tr, sampleEvent(t))
	if err != nil || got["c"] != "other" {
		t.Fatalf("fallback: %v %v", got, err)
	}
	tr.Tables["ch"] = map[string]string{"pos": "retail"}
	if _, err := renderBody(t, tr, sampleEvent(t)); err == nil || !strings.Contains(err.Error(), "no entry") {
		t.Fatalf("expected table miss error, got %v", err)
	}
}

func TestTransformCompileErrors(t *testing.T) {
	cases := map[string]*Transform{
		"empty body":       {Body: map[string]any{}},
		"unknown function": {Body: map[string]any{"a": "{{payload.x | eval}}"}},
		"unknown table":    {Body: map[string]any{"a": "{{payload.x | map:nope}}"}},
		"bad table name":   {Body: map[string]any{"a": "{{payload.x | map:Bad Name}}"}},
		"unclosed":         {Body: map[string]any{"a": "{{payload.x"}},
		"bad root":         {Body: map[string]any{"a": "{{env.HOME}}"}},
		"bad event field":  {Body: map[string]any{"a": "{{event.password}}"}},
		"fn with arg":      {Body: map[string]any{"a": "{{payload.x | lower:1}}"}},
		"bad segment":      {Body: map[string]any{"a": "{{payload.a b}}"}},
		"unsupported":      {Body: map[string]any{"a": struct{}{}}},
	}
	for name, tr := range cases {
		if _, err := compileTransform(tr); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	// demasiado profundo
	deep := map[string]any{"x": "y"}
	for i := 0; i < 12; i++ {
		deep = map[string]any{"n": deep}
	}
	if _, err := compileTransform(&Transform{Body: deep}); err == nil {
		t.Error("deep body should fail")
	}
}

func TestTransformPathErrorsAreUnknownPath(t *testing.T) {
	_, err := compileTransform(&Transform{Body: map[string]any{"a": "{{event.nope}}"}})
	if !errors.Is(err, ErrUnknownPath) {
		t.Fatalf("want ErrUnknownPath, got %v", err)
	}
}

// --- filtro ---------------------------------------------------------------

func TestFilterOperators(t *testing.T) {
	root := sampleEvent(t).view()
	f := func(js string) *compiledFilter {
		t.Helper()
		var fl Filter
		if err := json.Unmarshal([]byte(js), &fl); err != nil {
			t.Fatal(err)
		}
		cf, err := compileFilter(&fl)
		if err != nil {
			t.Fatalf("%s: %v", js, err)
		}
		return cf
	}
	cases := []struct {
		js   string
		want bool
	}{
		{`{}`, true},
		{`{"all":[{"path":"payload.channel","eq":"web"}]}`, true},
		{`{"all":[{"path":"payload.channel","eq":"pos"}]}`, false},
		{`{"all":[{"path":"payload.age","eq":31}]}`, true},
		{`{"all":[{"path":"payload.age","eq":31.0}]}`, true},
		{`{"all":[{"path":"payload.age","eq":"31"}]}`, false}, // tipos estrictos
		{`{"all":[{"path":"payload.vip","eq":true}]}`, true},
		{`{"all":[{"path":"payload.note","eq":null}]}`, true},
		{`{"all":[{"path":"payload.channel","ne":"pos"}]}`, true},
		{`{"all":[{"path":"payload.nope","ne":"pos"}]}`, true},
		{`{"all":[{"path":"payload.channel","ne":"web"}]}`, false},
		{`{"all":[{"path":"event.branch_id","in":["br-9","br-1"]}]}`, true},
		{`{"all":[{"path":"event.branch_id","in":["br-9"]}]}`, false},
		{`{"all":[{"path":"payload.nope","in":["x"]}]}`, false},
		{`{"all":[{"path":"payload.email","exists":true}]}`, true},
		{`{"all":[{"path":"payload.nope","exists":true}]}`, false},
		{`{"all":[{"path":"payload.nope","exists":false}]}`, true},
		{`{"all":[{"path":"payload.note","exists":true}]}`, false}, // null = no existe
		{`{"all":[{"path":"payload.address.city","eq":"CDMX"}]}`, true},
		{`{"all":[{"path":"payload.items.1.sku","eq":"B2"}]}`, true},
		{`{"all":[{"path":"payload.channel","eq":"web"},{"path":"payload.vip","eq":false}]}`, false},
		{`{"any":[{"path":"payload.channel","eq":"pos"},{"path":"payload.vip","eq":true}]}`, true},
		{`{"any":[{"path":"payload.channel","eq":"pos"},{"path":"payload.vip","eq":false}]}`, false},
		{`{"all":[{"path":"payload.channel","eq":"web"}],"any":[{"path":"payload.vip","eq":false}]}`, false},
		{`{"all":[{"path":"payload.channel","eq":"web"}],"any":[{"path":"payload.vip","eq":true}]}`, true},
	}
	for _, c := range cases {
		if got := f(c.js).match(root); got != c.want {
			t.Errorf("%s = %v, want %v", c.js, got, c.want)
		}
	}
}

func TestFilterCompileErrors(t *testing.T) {
	bad := []string{
		`{"all":[{"path":"payload.a"}]}`,               // sin operador
		`{"all":[{"path":"payload.a","eq":1,"ne":2}]}`, // dos operadores
		`{"all":[{"path":"payload.a","in":[]}]}`,       // in vacio
		`{"all":[{"path":"nope.a","eq":1}]}`,           // raiz invalida
		`{"all":[{"path":"event.zzz","eq":1}]}`,        // campo de evento inexistente
		`{"all":[{"path":"payload.a b","eq":1}]}`,      // segmento invalido
	}
	for _, js := range bad {
		var fl Filter
		if err := json.Unmarshal([]byte(js), &fl); err != nil {
			t.Fatal(err)
		}
		if _, err := compileFilter(&fl); err == nil {
			t.Errorf("%s should fail", js)
		}
	}
	many := Filter{}
	for i := 0; i < 40; i++ {
		many.All = append(many.All, Condition{Path: "payload.a", Eq: json.RawMessage(`1`)})
	}
	if _, err := compileFilter(&many); err == nil {
		t.Error("too many conditions should fail")
	}
}

func TestFilterSurvivesJSONRoundTrip(t *testing.T) {
	in := Filter{All: []Condition{{Path: "payload.a", Eq: json.RawMessage(`null`)}, {Path: "payload.b", In: []any{"x", float64(2)}}}}
	b, _ := json.Marshal(in)
	var out Filter
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	cf, err := compileFilter(&out)
	if err != nil {
		t.Fatal(err)
	}
	if !cf.match(map[string]any{"payload": map[string]any{"a": nil, "b": float64(2)}}) {
		t.Fatal("round-tripped filter lost semantics")
	}
}

// --- esquema --------------------------------------------------------------

func TestSchemaPathValidation(t *testing.T) {
	src := NewSchemas().
		Register("customers.customer.created", "id", "name", "address.city", "items.*.sku").
		Register("customers.customer.updated", "id", "name")
	body := func(p string) []pathRef {
		r, err := parsePath(p)
		if err != nil {
			t.Fatal(err)
		}
		return []pathRef{r}
	}
	ok := []struct {
		events []string
		path   string
	}{
		{[]string{"customers.customer.created"}, "payload.id"},
		{[]string{"customers.customer.created"}, "payload.address.city"}, //
		{[]string{"customers.customer.created"}, "payload.items.0.sku"},  // indice casa con *
		{[]string{"customers.customer.created"}, "event.id"},             // sobre: siempre
		{[]string{"customers.*"}, "payload.name"},                        // existe en ambos
	}
	for _, c := range ok {
		if err := checkRefs(src, c.events, body(c.path)); err != nil {
			t.Errorf("%v %s: %v", c.events, c.path, err)
		}
	}
	bad := []struct {
		events []string
		path   string
		is     error
	}{
		{[]string{"customers.customer.created"}, "payload.nombre", ErrUnknownPath},
		{[]string{"customers.customer.created"}, "payload.address", ErrUnknownPath}, // subarbol: no es un escalar declarado
		{[]string{"customers.customer.created"}, "payload.items", ErrUnknownPath},
		{[]string{"customers.customer.created"}, "payload.address.zip", ErrUnknownPath},
		{[]string{"customers.customer.created"}, "payload.items.x.sku", ErrUnknownPath},
		{[]string{"customers.*"}, "payload.address.city", ErrUnknownPath}, // falta en updated
		{[]string{"orders.order.created"}, "payload.id", ErrNoSchema},     // evento sin esquema
		{[]string{"orders.*"}, "payload.id", ErrNoSchema},                 // patron sin eventos
	}
	for _, c := range bad {
		err := checkRefs(src, c.events, body(c.path))
		if !errors.Is(err, c.is) {
			t.Errorf("%v %s: got %v, want %v", c.events, c.path, err, c.is)
		}
	}
	// sin fuente de esquema: solo se admiten rutas event.*
	if err := checkRefs(nil, []string{"x.y"}, body("payload.id")); !errors.Is(err, ErrNoSchema) {
		t.Errorf("nil schema: %v", err)
	}
	if err := checkRefs(nil, []string{"x.y"}, body("event.id")); err != nil {
		t.Errorf("nil schema, event path: %v", err)
	}
}

// --- destino --------------------------------------------------------------

func TestValidateTarget(t *testing.T) {
	s := &Service{}
	good := []string{"https://api.example.com/hook", "https://sandbox.7leguas.com.mx/api/channels/v1/customers"}
	for _, u := range good {
		if err := s.validateTarget(u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	bad := []string{
		"http://api.example.com/hook", // no https
		"https://127.0.0.1/x", "https://10.0.0.5/x", "https://192.168.1.1/x", "https://169.254.169.254/latest",
		"https://[::1]/x", "https://[::ffff:127.0.0.1]/x",
		"https://2130706433/x",  // IP decimal
		"https://0x7f.0.0.1/x",  // IP hexa
		"https://localhost/x",   // sin punto
		"https://intranet/x",    // sin punto
		"https://db.internal/x", // espacio interno
		"https://svc.cluster.local/x",
		"https://printer.local/x",
		"https://user:pw@api.example.com/x", // credenciales
		"ftp://api.example.com/x", "https://api.example.com/x?token=abc", "https://api.example.com/x#frag",
		"https:///nohost",
		"not a url",
		"https://exa mple.com/x",
		"https://-bad.example.com/x",
	}
	for _, u := range bad {
		if err := s.validateTarget(u); !errors.Is(err, ErrBlockedTarget) {
			t.Errorf("%s: want ErrBlockedTarget, got %v", u, err)
		}
	}
	// allowlist
	s.allowHosts = []string{"*.7leguas.com.mx", "api.example.com"}
	if err := s.validateTarget("https://sandbox.7leguas.com.mx/x"); err != nil {
		t.Error(err)
	}
	if err := s.validateTarget("https://evil.example.net/x"); !errors.Is(err, ErrBlockedTarget) {
		t.Errorf("allowlist: %v", err)
	}
	// solo pruebas
	s2 := &Service{allowPrivate: true}
	if err := s2.validateTarget("http://127.0.0.1:9/x"); err != nil {
		t.Errorf("allowPrivate: %v", err)
	}
}

// --- sobre del bus --------------------------------------------------------

func TestEventFromBusEnvelope(t *testing.T) {
	org := uuid.New()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	payload := map[string]any{
		"id": "cus-1", "name": "Ana",
		"envelope": map[string]any{
			"id": "evt-9", "source": "kernel", "branch_id": "br-2", "actor_id": "u-1", "correlation_id": "corr-1",
			"aggregate": map[string]any{"type": "customers.customer", "id": "cus-1"}, "sequence": float64(4),
			"occurred_at": "2026-11-17T09:00:00Z",
		},
	}
	ev, err := EventFromBus(org, "customers.customer.created", payload, now)
	if err != nil {
		t.Fatal(err)
	}
	if ev.ID != "evt-9" || ev.AggregateID != "cus-1" || ev.AggregateType != "customers.customer" || ev.Sequence != 4 ||
		ev.BranchID != "br-2" || ev.Source != "kernel" || ev.ActorID != "u-1" || ev.CorrelationID != "corr-1" ||
		ev.OccurredAt.Format(time.RFC3339) != "2026-11-17T09:00:00Z" {
		t.Fatalf("event = %+v", ev)
	}
	if _, has := ev.Payload["envelope"]; has || ev.Payload["name"] != "Ana" {
		t.Fatalf("payload must drop the envelope: %v", ev.Payload)
	}
	// un Replay (generation N) es otra ocurrencia: no se deduplica contra la original
	payload["envelope"].(map[string]any)["generation"] = float64(2)
	ev2, _ := EventFromBus(org, "customers.customer.created", payload, now)
	if ev2.ID != "evt-9.g2" {
		t.Fatalf("generation id = %s", ev2.ID)
	}
}

func TestEventFromBusDerivedIDIsStable(t *testing.T) {
	org := uuid.New()
	t1 := time.Unix(1_800_000_000, 0)
	a, _ := EventFromBus(org, "x.y", json.RawMessage(`{"a":1}`), t1)
	b, _ := EventFromBus(org, "x.y", map[string]any{"a": 1}, t1)
	f, _ := EventFromBus(org, "x.y", map[string]any{"a": 1}, t1.Add(time.Hour))
	c, _ := EventFromBus(org, "x.y", map[string]any{"a": 2}, time.Now())
	d, _ := EventFromBus(uuid.New(), "x.y", map[string]any{"a": 1}, time.Now())
	if a.ID != b.ID || a.ID == f.ID || a.ID == c.ID || a.ID == d.ID || !strings.HasPrefix(a.ID, "h-") {
		t.Fatalf("ids: %s %s %s %s", a.ID, b.ID, c.ID, d.ID)
	}
	// payload que no es objeto
	e, err := EventFromBus(org, "x.y", []int{1, 2}, time.Now())
	if err != nil || e.Payload["value"] == nil {
		t.Fatalf("non-object payload: %v %v", e.Payload, err)
	}
	if _, err := EventFromBus(org, "x.y", make(chan int), time.Now()); err == nil {
		t.Fatal("non-JSON payload must error")
	}
}

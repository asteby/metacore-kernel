package batch

import "testing"

func TestJSNumber(t *testing.T) {
	cases := map[float64]string{1: "1", 1.5: "1.5", 0.1: "0.1", 1e-7: "1e-7", 1e21: "1e+21", 123456789012: "123456789012", -0.000001: "-0.000001", 23.4601989322: "23.4601989322"}
	for in, want := range cases {
		if got := jsNumber(in); got != want {
			t.Errorf("jsNumber(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestJSStringifyHashOrderAndEscapes(t *testing.T) {
	// sha256 of {"b":1,"a":"ñ\n","c":[1.5,null,true]} computed with Node.
	raw := []byte("{\n  \"b\": 1.0,\n  \"x\": \"fuera\",\n  \"a\": \"ñ\\n\",\n  \"c\": [1.5, null, true]\n}")
	got, err := JSStringifyHash(raw, "x")
	if err != nil {
		t.Fatal(err)
	}
	const want = "02c9943a7495014817b16418fc600cef44cd9493ce95cdc717d8f9c7a280d621"
	if got != want {
		t.Fatalf("hash %s", got)
	}
}

func TestJSStringifyHashRejectsTrailingAndInvalid(t *testing.T) {
	for _, raw := range []string{`{"a":1} {"b":2}`, `{"a":`, ``, `{"a":1e999}`} {
		if _, err := JSStringifyHash([]byte(raw)); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
}

func TestJSStringifyHashDropOnlyRoot(t *testing.T) {
	a, _ := JSStringifyHash([]byte(`{"x":1,"n":{"x":2}}`), "x")
	b, _ := JSStringifyHash([]byte(`{"n":{"x":2}}`))
	if a == "" || a != b {
		t.Fatalf("root-only drop failed: %s vs %s", a, b)
	}
}

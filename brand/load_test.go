package brand

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestLoadFSVantoraLike(t *testing.T) {
	src, err := LoadFS(vantoraPack())
	if err != nil {
		t.Fatal(err)
	}
	if src.Key != "vantora" || src.StorageNamespace != "vantora" || src.Assistant.Name != "Vanta" {
		t.Fatalf("source = %+v", src)
	}
	if src.Color != "#0033f0" || src.Theme.Background != "#ffffff" || src.Theme.Gradient[0] != "#00fcff" {
		t.Fatalf("colors not normalized: color=%q theme=%+v", src.Color, src.Theme)
	}
	if src.Icon.Width != 512 || src.Logo.Width != 1300 || src.Logo.Height != 400 {
		t.Fatalf("svg dimensions: icon=%dx%d logo=%dx%d", src.Icon.Width, src.Icon.Height, src.Logo.Width, src.Logo.Height)
	}
	if src.Favicon.Type != "image/png" || src.Favicon.Width != 48 || src.Maskable.Width != 512 {
		t.Fatalf("png: favicon=%+v maskable=%d", src.Favicon.Type, src.Maskable.Width)
	}
	if src.Animated.Width != 200 || src.Animated.Height != 100 {
		t.Fatalf("animated dimensions %dx%d", src.Animated.Width, src.Animated.Height)
	}

	// The loaded Source produces the new manifest fields through the handler.
	m := manifestOf(t, NewHandler(Static(src), WithVersionedURLs()), "/api/brand")
	if m.Assets.LogoDark == nil || m.Assets.Favicon == nil || m.Assets.Maskable == nil || m.Assets.Animated == nil {
		t.Fatalf("assets = %+v", m.Assets)
	}
	if m.Theme == nil || len(m.Theme.Gradient) != 2 || m.Theme.Dark == nil || m.StorageNamespace != "vantora" {
		t.Fatalf("manifest = %+v", m)
	}
	if m.Assistant == nil || m.Assistant.Mascot == nil || m.Assistant.Mascot.ID != "vantora" {
		t.Fatalf("assistant = %+v", m.Assistant)
	}
}

func TestLoadFSRejects(t *testing.T) {
	mutate := func(f func(fstest.MapFS)) fstest.MapFS {
		m := vantoraPack()
		f(m)
		return m
	}
	setPack := func(m fstest.MapFS, old, new string) {
		m["brand.json"] = &fstest.MapFile{Data: []byte(strings.Replace(string(m["brand.json"].Data), old, new, 1))}
	}
	cases := map[string]fstest.MapFS{
		"no brand.json": {},
		"dotdot":        mutate(func(m fstest.MapFS) { setPack(m, `"vantora-isotipo.svg"`, `"../secret.svg"`) }),
		"absolute":      mutate(func(m fstest.MapFS) { setPack(m, `"vantora-isotipo.svg"`, `"/etc/x.svg"`) }),
		"backslash":     mutate(func(m fstest.MapFS) { setPack(m, `"vantora-isotipo.svg"`, `"a\\b.svg"`) }),
		"deep nesting":  mutate(func(m fstest.MapFS) { setPack(m, `"vantora-isotipo.svg"`, `"assets/a/b.svg"`) }),
		"other subdir":  mutate(func(m fstest.MapFS) { setPack(m, `"vantora-isotipo.svg"`, `"img/a.svg"`) }),
		"missing file":  mutate(func(m fstest.MapFS) { delete(m, "vantora-isotipo.svg") }),
		"bad extension": mutate(func(m fstest.MapFS) { setPack(m, `"vantora-isotipo.svg"`, `"x.mp4"`) }),
		"bad color":     mutate(func(m fstest.MapFS) { setPack(m, `"#0033F0"`, `"blue"`) }),
		"unknown field": mutate(func(m fstest.MapFS) { setPack(m, `"key"`, `"colour":"x","key"`) }),
		"bad key":       mutate(func(m fstest.MapFS) { setPack(m, `"key":"vantora"`, `"key":"Vantora!"`) }),
		"gradient of 1": mutate(func(m fstest.MapFS) { setPack(m, `["#00fcff","#0033f0"]`, `["#00fcff"]`) }),
		"script in svg": mutate(func(m fstest.MapFS) {
			m["vantora-isotipo.svg"] = &fstest.MapFile{Data: []byte(`<svg ` + svgNS + `><script>alert(1)</script></svg>`)}
		}),
		"script animated": mutate(func(m fstest.MapFS) {
			m["animated.svg"] = &fstest.MapFile{Data: []byte(`<svg ` + svgNS + `><script>alert(1)</script></svg>`)}
		}),
		"fake png": mutate(func(m fstest.MapFS) { m["favicon.png"] = &fstest.MapFile{Data: []byte("not a png")} }),
		"too big": mutate(func(m fstest.MapFS) {
			m["vantora-logo-fondo-claro.svg"] = &fstest.MapFile{Data: bytes.Repeat([]byte(" "), MaxAssetBytes+1)}
		}),
		"bad mascot": mutate(func(m fstest.MapFS) { setPack(m, `"hex-frame"`, `"star"`) }),
	}
	for name, fsys := range cases {
		if _, err := LoadFS(fsys); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLoadFSInvalidIsErrInvalid(t *testing.T) {
	m := vantoraPack()
	m["brand.json"] = &fstest.MapFile{Data: []byte(strings.Replace(string(m["brand.json"].Data), `"#0033F0"`, `"blue"`, 1))}
	_, err := LoadFS(m)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
}

func TestSVGAnimationOnlyInAnimatedSlots(t *testing.T) {
	m := vantoraPack()
	m["vantora-isotipo.svg"] = m["animated.svg"]
	if _, err := LoadFS(m); err == nil {
		t.Fatal("SMIL accepted as a static icon")
	}
}

// compileSchema returns the brand.json schema compiled with jsonschema.
func compileSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(SchemaJSON))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("brand.schema.json", doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("brand.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPackValidatesAgainstSchema(t *testing.T) {
	s := compileSchema(t)
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(vantoraPack()["brand.json"].Data))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(doc); err != nil {
		t.Fatalf("vantora-like pack against schema: %v", err)
	}
	bad, _ := jsonschema.UnmarshalJSON(strings.NewReader(`{"key":"x","name":"X","assets":{"icon":"../x.svg"}}`))
	if s.Validate(bad) == nil {
		t.Fatal("schema accepted a bad key and a traversal path")
	}
}

// TestSchemaParity keeps brand.schema.json and the Go types (PackFile and the
// structs it reaches) in lockstep: same property names at every level.
func TestSchemaParity(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal(SchemaJSON, &schema); err != nil {
		t.Fatal(err)
	}
	var walk func(path string, node map[string]any, typ reflect.Type)
	walk = func(path string, node map[string]any, typ reflect.Type) {
		for typ.Kind() == reflect.Ptr {
			typ = typ.Elem()
		}
		props, _ := node["properties"].(map[string]any)
		goFields := map[string]reflect.Type{}
		for i := 0; i < typ.NumField(); i++ {
			tag := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
			if tag == "" || tag == "-" {
				continue
			}
			goFields[tag] = typ.Field(i).Type
		}
		for name := range props {
			if _, ok := goFields[name]; !ok {
				t.Errorf("%s: schema property %q has no Go field", path, name)
			}
		}
		for name, ft := range goFields {
			child, ok := props[name].(map[string]any)
			if !ok {
				t.Errorf("%s: Go field %q missing from the schema", path, name)
				continue
			}
			for ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				walk(path+"."+name, child, ft)
			}
		}
	}
	walk("$", schema, reflect.TypeOf(PackFile{}))
}

func TestLoadFSFromDisk(t *testing.T) {
	dir := t.TempDir()
	for name, f := range vantoraPack() {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, f.Data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	src, err := LoadFS(os.DirFS(dir))
	if err != nil {
		t.Fatal(err)
	}
	m := manifestOf(t, NewHandler(Static(src), WithVersionedURLs()), "/api/brand")
	if m.Assets.LogoDark == nil || m.Theme == nil || len(m.Theme.Gradient) != 2 || m.StorageNamespace != "vantora" {
		t.Fatalf("manifest = %+v", m)
	}
}

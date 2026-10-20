package money

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestStdlibOnly fails the build if any file of the package (tests included)
// imports something outside the Go standard library. wasm guests receive a
// verbatim copy of this directory (tools/sync-money.mjs in addons) and can
// only build with the standard library; gorm, uuid or kernel packages are not
// allowed here.
func TestStdlibOnly(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no Go files: %v", err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			first, _, _ := strings.Cut(path, "/")
			// Standard library paths have no dot in the first element.
			if strings.Contains(first, ".") || path == "C" {
				t.Errorf("%s imports %q: package money must only use the standard library", name, path)
			}
		}
		if strings.Contains(string(src), "metacore-"+"kernel") && !strings.HasSuffix(name, "_test.go") {
			t.Errorf("%s mentions the kernel module path; guests reject binaries containing it", name)
		}
	}
}

package v3

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidate_RepoExamples(t *testing.T) {
	cases := []string{
		"../../docs/spec/v3/examples/addon-example.json",
		"../../docs/spec/v3/examples/preset-example.json",
	}
	for _, p := range cases {
		t.Run(filepath.Base(p), func(t *testing.T) {
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("read %s: %v", p, err)
			}
			if err := Validate(b); err != nil {
				t.Fatalf("validate %s: %v", p, err)
			}
		})
	}
}

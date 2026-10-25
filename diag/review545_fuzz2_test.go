package diag_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/diag"
)

// Secreto conocido (antes o despues del Log) en cualquier linea: nunca debe sobrevivir entero ni
// un tramo de 12 bytes cuando la linea cabe en memoria (< 8 KB).
func FuzzLateSecretAnyLine(f *testing.F) {
	f.Add("https://a.io/?q=", "Zq9kLm2Pw7xYv4", " tail", true)
	f.Add("k=", "correct horse battery", "", false)
	f.Fuzz(func(t *testing.T, pre, sec, post string, after bool) {
		if len(sec) < 12 || len(pre)+len(sec)+len(post) > 8000 || strings.TrimSpace(sec) != sec {
			return
		}
		b := diag.NewBag()
		if !after {
			b.AddSecrets(sec)
		}
		b.Log(pre + sec + post)
		if after {
			b.AddSecrets(sec)
		}
		out := strings.Join(persistCrumbs(b), "\n")
		if strings.Contains(out, sec) {
			t.Fatalf("full secret survived: pre=%q sec=%q post=%q out=%q", pre, sec, post, out)
		}
	})
}

func persistCrumbs(b *diag.Bag) []string {
	d := diag.Diagnostic{Org: uuid.New(), AddonKey: "acme", Code: "x"}
	b.Apply(&d)
	return diag.Sanitize(d, b.Scrubber).Breadcrumbs
}

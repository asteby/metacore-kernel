package diag_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/diag"
)

// Un PAN valido (Luhn) con separadores arbitrarios entre digitos no debe sobrevivir.
func FuzzPANSeparators(f *testing.F) {
	f.Add(" ", "-", ".")
	f.Add("  ", "/", "_")
	f.Fuzz(func(t *testing.T, s1, s2, s3 string) {
		if len(s1) > 2 || len(s2) > 2 || len(s3) > 2 {
			return
		}
		for _, s := range []string{s1, s2, s3} {
			for _, r := range s {
				if r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r > 127 || r < 0x20 {
					return
				}
			}
		}
		pan := "4111" + s1 + "1111" + s2 + "1111" + s3 + "1111"
		b := diag.NewBag()
		b.Log("card " + pan + " declined")
		d := diag.Diagnostic{Org: uuid.New(), AddonKey: "acme", Code: "x"}
		b.Apply(&d)
		o := diag.Sanitize(d, b.Scrubber)
		j := strings.Join(o.Breadcrumbs, "|")
		if strings.Count(j, "1111") >= 3 {
			t.Fatalf("PAN survived with seps %q %q %q: %q", s1, s2, s3, j)
		}
	})
}

// Un secreto conocido en cualquier posicion de una linea de log no debe filtrarse ni parcialmente (>=12 chars).
func FuzzKnownSecretOffset(f *testing.F) {
	f.Add(0, "https://a.io/?q=")
	f.Fuzz(func(t *testing.T, pad int, prefix string) {
		if pad < 0 || pad > 2000 || len(prefix) > 50 {
			return
		}
		secret := "QwErTyUiOpAsDfGhJkLzXcVbNm"
		b := diag.NewBag()
		b.AddSecrets(secret)
		b.Log(prefix + strings.Repeat("x", pad) + " " + secret)
		d := diag.Diagnostic{Org: uuid.New(), AddonKey: "acme", Code: "x"}
		b.Apply(&d)
		o := diag.Sanitize(d, b.Scrubber)
		for _, c := range o.Breadcrumbs {
			for i := 0; i+12 <= len(secret); i++ {
				if strings.Contains(c, secret[i:i+12]) {
					t.Fatalf("partial secret leaked pad=%d prefix=%q: %q", pad, prefix, c)
				}
			}
		}
	})
}

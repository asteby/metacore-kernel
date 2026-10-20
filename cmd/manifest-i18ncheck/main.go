// Command manifest-i18ncheck fails when an addon manifest references i18n keys
// (settings, connector credentials and form layout, actions) that are missing
// from any of its declared locale bundles. See manifest/i18ncheck.
//
//	go run github.com/asteby/metacore-kernel/cmd/manifest-i18ncheck [-examples N] <addon-dir>...
//
// Each argument is a directory containing manifest.json. Exit status 1 when
// any addon has missing keys or unreadable bundles.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/asteby/metacore-kernel/manifest/i18ncheck"
)

func main() {
	examples := flag.Int("examples", 3, "missing keys printed per addon (0 = all)")
	literals := flag.Bool("literals", false, "also list addons whose labels are literal text instead of i18n keys (informational)")
	flag.Parse()
	dirs := flag.Args()
	if len(dirs) == 0 {
		fmt.Fprintln(os.Stderr, "usage: manifest-i18ncheck [-examples N] <addon-dir>...")
		os.Exit(2)
	}
	sort.Strings(dirs)
	checked, failing := 0, 0
	for _, dir := range dirs {
		if _, err := os.Stat(dir + "/manifest.json"); err != nil {
			continue
		}
		rep, err := i18ncheck.CheckDir(dir)
		if err != nil {
			fmt.Printf("ERROR %s: %v\n", dir, err)
			failing++
			continue
		}
		checked++
		if *literals && len(rep.Literals) > 0 {
			ex := rep.Literals[0]
			fmt.Printf("i %s: %d texto(s) literal(es) sin i18n, p. ej. %q (%s)\n", rep.Addon, len(rep.Literals), ex.Key, ex.Where)
		}
		if rep.OK() {
			continue
		}
		failing++
		keys := map[string]bool{}
		for _, m := range rep.Missing {
			keys[m.Key] = true
		}
		fmt.Printf("%s: %d clave(s) faltante(s) en %d hueco(s) (locales %v, %d referenciadas)\n",
			rep.Addon, len(keys), len(rep.Missing), rep.Locales, rep.Referenced)
		for _, p := range rep.Problems {
			fmt.Printf("  ! %s\n", p)
		}
		for i, m := range rep.Missing {
			if *examples > 0 && i >= *examples {
				fmt.Printf("  … %d más\n", len(rep.Missing)-i)
				break
			}
			fmt.Printf("  - [%s] %s (%s)\n", m.Locale, m.Key, m.Where)
		}
	}
	fmt.Printf("revisados: %d · con faltantes: %d\n", checked, failing)
	if failing > 0 {
		os.Exit(1)
	}
}

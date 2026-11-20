// Command ed25519gate fails the build when Ed25519 verification is used
// outside the vetted verification packages, so every consumer goes through
// security/signedenvelope (purpose separation, key scoping, typed errors).
//
// It parses Go files (go/ast), so it is not fooled by comments, import aliases,
// dot-imports or function values:
//
//	ed25519.Verify(...)             call
//	ed25519.VerifyWithOptions(...)  call
//	f := ed25519.Verify             function value
//	import e "crypto/ed25519"; e.Verify(...)
//	import . "crypto/ed25519"       (the dot-import itself is a violation)
//
// Counting is per file. Two ledgers (lines "path count", '#' comments):
//
//	-allowed   permanent, explicit files (the verification packages). Counts
//	           must match exactly, so a new call inside an allowed file also
//	           needs a reviewed edit of the ledger.
//	-baseline  transitional debt (ops/hub start with their sites here). Counts
//	           must match exactly (a migrated call forces the line to shrink).
//	-base REF  compares the baseline with its version at REF (a git ref such as
//	           origin/main): it may only lose files or lower counts, never add or
//	           raise, so "shrink only" is enforced and not just promised.
//
// Test files are ignored. Run from the repo root (or pass -root).
package main

import (
	"bufio"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

var edPaths = map[string]bool{
	"crypto/ed25519":                           true,
	"golang.org/x/crypto/ed25519":              true,
	"github.com/cloudflare/circl/sign/ed25519": true,
}

var verifyNames = map[string]bool{"Verify": true, "VerifyWithOptions": true}

func main() {
	root := flag.String("root", ".", "repo root")
	allowedF := flag.String("allowed", "", "ledger of permanent allowed files")
	baselineF := flag.String("baseline", "", "ledger of transitional files")
	baseRef := flag.String("base", "", "git ref to compare the baseline against (shrink-only)")
	flag.Parse()

	allowed := mustLedger(*root, *allowedF)
	baseline := mustLedger(*root, *baselineF)
	hits, err := scan(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ed25519gate:", err)
		os.Exit(2)
	}

	var problems []string
	files := map[string]bool{}
	for f := range hits {
		files[f] = true
	}
	for f := range allowed {
		files[f] = true
	}
	for f := range baseline {
		files[f] = true
	}
	var names []string
	for f := range files {
		names = append(names, f)
	}
	sort.Strings(names)
	for _, f := range names {
		n := hits[f]
		a, inA := allowed[f]
		b, inB := baseline[f]
		switch {
		case inA && inB:
			problems = append(problems, fmt.Sprintf("%s is in both ledgers", f))
		case inA:
			if n != a {
				problems = append(problems, fmt.Sprintf("%s: %d ed25519 verify uses, allowed ledger says %d (edit the ledger in a reviewed change)", f, n, a))
			}
		case inB:
			if n != b {
				problems = append(problems, fmt.Sprintf("%s: %d uses, baseline says %d (baseline must match: lower it when you migrate, never raise it)", f, n, b))
			}
		case n > 0:
			problems = append(problems, fmt.Sprintf("%s: %d ed25519 verify use(s) outside the vetted packages: use security/signedenvelope", f, n))
		}
	}
	if *baseRef != "" && *baselineF != "" {
		old, err := baseLedger(*root, *baseRef, *baselineF)
		if err != nil {
			problems = append(problems, "cannot read baseline at "+*baseRef+": "+err.Error())
		}
		for f, n := range baseline {
			if old == nil {
				break
			}
			if on, ok := old[f]; !ok {
				if err == nil {
					problems = append(problems, fmt.Sprintf("baseline gained a file vs %s: %s", *baseRef, f))
				}
			} else if n > on {
				problems = append(problems, fmt.Sprintf("baseline raised vs %s: %s %d -> %d", *baseRef, f, on, n))
			}
		}
	}
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(os.Stderr, "FAIL:", p)
		}
		os.Exit(1)
	}
	fmt.Println("ed25519 verify gate: OK")
}

func scan(root string) (map[string]int, error) {
	hits := map[string]int{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", "testdata", "_worktrees":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", p, perr)
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		n := count(f)
		if n > 0 {
			hits[rel] = n
		}
		return nil
	})
	return hits, err
}

func count(f *ast.File) int {
	aliases := map[string]bool{}
	n := 0
	for _, im := range f.Imports {
		path, _ := strconv.Unquote(im.Path.Value)
		if !edPaths[path] {
			continue
		}
		name := "ed25519"
		if im.Name != nil {
			name = im.Name.Name
		}
		switch name {
		case "_":
			continue
		case ".":
			n++ // a dot-import hides every use: forbidden on its own
			continue
		}
		aliases[name] = true
	}
	if len(aliases) == 0 {
		return n
	}
	ast.Inspect(f, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok || !verifyNames[sel.Sel.Name] {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && aliases[id.Name] && id.Obj == nil {
			n++
		}
		return true
	})
	return n
}

func mustLedger(root, path string) map[string]int {
	m := map[string]int{}
	if path == "" {
		return m
	}
	fh, err := os.Open(filepath.Join(root, path))
	if err != nil {
		fmt.Fprintln(os.Stderr, "ed25519gate:", err)
		os.Exit(2)
	}
	defer fh.Close()
	parseLedger(fh, m)
	return m
}

func parseLedger(r interface{ Read([]byte) (int, error) }, m map[string]int) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if i := strings.Index(line, "#"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		fs := strings.Fields(line)
		if len(fs) != 2 {
			fmt.Fprintf(os.Stderr, "ed25519gate: bad ledger line %q (want: path count)\n", line)
			os.Exit(2)
		}
		n, err := strconv.Atoi(fs[1])
		if err != nil || n < 1 {
			fmt.Fprintf(os.Stderr, "ed25519gate: bad count in %q\n", line)
			os.Exit(2)
		}
		m[fs[0]] = n
	}
}

func baseLedger(root, ref, path string) (map[string]int, error) {
	out, err := exec.Command("git", "-C", root, "show", ref+":"+path).Output()
	if err != nil {
		// Ledger not at the base yet (bootstrap PR that introduces it): skip.
		return nil, nil
	}
	m := map[string]int{}
	parseLedger(strings.NewReader(string(out)), m)
	return m, nil
}

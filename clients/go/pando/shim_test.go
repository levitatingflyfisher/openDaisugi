package pando

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Main is the old `coppice steward import` command, which the chair keeps
// calling through RunImport. The import tests drive it by its words.
func Main(argv []string, _ string, out, errw io.Writer) int {
	dir, asJSON := "", false
	f := ImportFlags{From: "all"}
	for k := 1; k < len(argv); k++ {
		switch a := argv[k]; a {
		case "--data-dir":
			dir = argv[k+1]
			k++
		case "--from":
			f.From = argv[k+1]
			k++
		case "--since":
			f.Since = argv[k+1]
			k++
		case "--json":
			asJSON = true
		default:
			f.Set(a)
		}
	}
	return RunImport(dir, nil, f, asJSON, out, errw)
}

// runCLI runs daisugi pando over a store dir that a test made. The dir is
// moved in as <data home>/pando/work for the run and moved back after: a
// link would be refused, as a store path may hold none.
func runCLI(t *testing.T, _ string, argv ...string) (int, string, string) {
	t.Helper()
	for k := range argv {
		if argv[k] == "--data-dir" && k+1 < len(argv) {
			data := t.TempDir()
			if err := os.MkdirAll(filepath.Join(data, "pando"), 0o700); err != nil {
				t.Fatal(err)
			}
			orig, in := argv[k+1], filepath.Join(data, "pando", "work")
			if err := os.Rename(orig, in); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := os.Rename(in, orig); err != nil {
					t.Fatal(err)
				}
			}()
			argv[k+1] = data
		}
	}
	var out, errw strings.Builder
	code := Run(argv, CLIEnv{Stdin: strings.NewReader(""), Out: &out, Err: &errw})
	return code, out.String(), errw.String()
}

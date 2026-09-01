// Package boundary_test enforces that only internal/vt touches libghostty.
// It deliberately imports nothing from go.mitchellh.com/libghostty and pulls
// in no cgo, so it compiles and runs on a machine that never ran
// scripts/toolchain.sh: the confinement check must not itself require the
// thing it is checking for.
package boundary_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// libghostty's Go API is explicitly unstable. Confining it to internal/vt is
// what keeps a bump a one-package job. This test is that confinement.
//
// It lives in its own cgo-free package rather than inside internal/vt: a test
// file under internal/vt, even one in package vt_test, still builds package
// vt to link the test binary, which means it builds cgo and the libghostty
// binding. On a machine without the toolchain, that turns "go test ./..."
// into a hard compile failure instead of the graceful skip
// toolchain.SkipReason() is meant to provide, and it means this very test can
// never fire on such a machine to report a clean result either.
func TestOnlyInternalVTImportsLibghostty(t *testing.T) {
	root := filepath.Join("..", "..")
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		slash := filepath.ToSlash(path)
		if strings.Contains(slash, "/internal/vt/") || strings.Contains(slash, "/internal/boundary/") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), "go.mitchellh.com/libghostty") {
			offenders = append(offenders, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("these files import libghostty outside internal/vt: %v", offenders)
	}
}

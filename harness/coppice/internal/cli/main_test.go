package cli

import (
	"os"
	"testing"

	"github.com/opendaisugi/coppice/internal/testhome"
)

// TestMain points HOME and every XDG directory at a scratch directory for
// the whole package (testhome.Isolate). A server a test starts then reads
// no operator config and writes no plugin files under the operator's home.
func TestMain(m *testing.M) {
	dir, err := testhome.Isolate("coppice-cli-test-*")
	if err != nil {
		panic(err)
	}
	// A server a test starts would start the real daisugi voice serve. A
	// daisugi first on PATH that refuses everything keeps voice down.
	bin := dir + "/bin"
	if err := os.MkdirAll(bin, 0o700); err != nil {
		panic(err)
	}
	if err := os.WriteFile(bin+"/daisugi", []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		panic(err)
	}
	os.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

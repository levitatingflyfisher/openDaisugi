// This file is an EXTERNAL test package (adapters_test), not an internal one
// (adapters), and that is load-bearing: pi, opencode and claude each import
// "coppice/internal/adapters" (to call Register from their own init()), so an
// INTERNAL test file here that blank-imports any of them would close the
// cycle back onto the very package under test - `go test` refuses that with
// "import cycle not allowed in test". An external test package compiles as
// its own unit that DEPENDS on adapters, rather than as adapters itself, so
// the same blank imports are fine from out here.
package adapters_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/adapters"
	_ "github.com/opendaisugi/coppice/internal/adapters/claude"
	_ "github.com/opendaisugi/coppice/internal/adapters/codex"
	_ "github.com/opendaisugi/coppice/internal/adapters/opencode"
	_ "github.com/opendaisugi/coppice/internal/adapters/pi"
	_ "github.com/opendaisugi/coppice/internal/adapters/sprig"
	"github.com/opendaisugi/coppice/internal/pane"
)

func TestNamesListsEverySlotIncludingTheUnbuiltOnes(t *testing.T) {
	names := adapters.Names()
	want := []string{"claude", "codex", "opencode", "pi", "sprig"}
	for _, w := range want {
		found := false
		for _, n := range names {
			if n == w {
				found = true
			}
		}
		if !found {
			t.Fatalf("Names() = %v, want it to include %q", names, w)
		}
	}
}

func TestGetOnAnUnknownAdapterFails(t *testing.T) {
	if _, ok := adapters.Get("telepathy"); ok {
		t.Fatal(`Get("telepathy") reported ok`)
	}
}

// A slot that is not built must refuse loudly. A slot that silently produced a
// pane doing nothing would be exactly the dishonest control section 3.5 bans.
func TestUnbuiltAdaptersRefuseToStart(t *testing.T) {
	for _, name := range []string{"opencode"} {
		a, ok := adapters.Get(name)
		if !ok {
			t.Fatalf("Get(%q) failed", name)
		}
		_, err := a.Start(context.Background(), pane.StartOpts{}, nil)
		if !errors.Is(err, adapters.ErrNotBuilt) {
			t.Fatalf("%s.Start() = %v, want ErrNotBuilt", name, err)
		}
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("%s.Start() error %q does not name the adapter", name, err)
		}
	}
}

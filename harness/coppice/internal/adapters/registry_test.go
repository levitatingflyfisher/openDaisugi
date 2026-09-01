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
// The test builds its own slot: starting a registered adapter here would run
// that harness's real binary.
func TestUnbuiltAdaptersRefuseToStart(t *testing.T) {
	a := adapters.NotBuilt{AdapterName: "telepathy", Owner: "spec-99"}
	_, err := a.Start(context.Background(), pane.StartOpts{}, nil)
	if !errors.Is(err, adapters.ErrNotBuilt) {
		t.Fatalf("Start() = %v, want ErrNotBuilt", err)
	}
	if !strings.Contains(err.Error(), "telepathy") || !strings.Contains(err.Error(), "spec-99") {
		t.Fatalf("Start() error %q does not name the adapter and its owner", err)
	}
}

// The opencode slot is built now. It must not read as a design.
func TestTheOpencodeSlotIsBuilt(t *testing.T) {
	a, ok := adapters.Get("opencode")
	if !ok {
		t.Fatal(`Get("opencode") failed`)
	}
	if _, notBuilt := a.(adapters.NotBuilt); notBuilt {
		t.Fatal("the opencode adapter is still a NotBuilt slot")
	}
}

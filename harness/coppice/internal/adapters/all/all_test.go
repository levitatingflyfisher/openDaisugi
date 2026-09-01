package all

import (
	"testing"

	"github.com/opendaisugi/coppice/internal/adapters"
	"github.com/opendaisugi/coppice/internal/pane"
)

// Importing this package must be enough to populate the registry. main.go and
// the CLI's tests both rely on that: neither imports an adapter package by
// hand.
func TestAllRegistersEveryAdapter(t *testing.T) {
	names := adapters.Names()
	for _, want := range []string{"claude", "codex", "opencode", "pi", "sprig"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("adapters.Names() = %v, missing %q", names, want)
		}
	}
}

// Every built headless adapter names the pids of the processes it starts,
// so the server can place a tool call inside its pane. An adapter without
// that would have its own gate hook refused every state report, with no
// word said, so this test fails on it instead.
func TestEveryBuiltAdapterNamesItsPids(t *testing.T) {
	for _, name := range adapters.Names() {
		a, _ := adapters.Get(name)
		if _, notBuilt := a.(adapters.NotBuilt); notBuilt {
			continue
		}
		src, ok := a.(pane.PidSource)
		if !ok {
			t.Errorf("adapter %s is built but is not a pane.PidSource, so its pane cannot be placed", name)
			continue
		}
		if src.PidProc() == nil {
			t.Errorf("adapter %s names no Pider", name)
		}
	}
}

package all

import (
	"testing"

	"github.com/opendaisugi/coppice/internal/adapters"
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

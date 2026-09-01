package tui

import (
	"os"
	"testing"

	"github.com/opendaisugi/coppice/internal/testhome"
)

// The floor's test server and everything it defaults to stay out of the
// operator's real home (see TestMain).
func TestTheFloorTestServerStaysOutOfTheRealHome(t *testing.T) {
	if testhome.RealHome() == "" {
		t.Fatal("TestMain did not isolate the home")
	}
	for _, k := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR"} {
		if testhome.UnderRealHome(os.Getenv(k)) || os.Getenv(k) == "" {
			t.Fatalf("%s is %q, not a scratch directory", k, os.Getenv(k))
		}
	}
	s := newTestServer(t)
	if testhome.UnderRealHome(s.Socket()) {
		t.Fatalf("socket %s is under the real home", s.Socket())
	}
}

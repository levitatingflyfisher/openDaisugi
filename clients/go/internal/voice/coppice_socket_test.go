package voice

import (
	"os"
	"testing"
)

// XDG_DATA_HOME set, XDG_RUNTIME_DIR unset, ~/.opendaisugi present: coppice
// listens under XDG_DATA_HOME, so the voice lookup must dial there.
func TestDefaultCoppiceSocketFollowsCoppiceDataRule(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(home+"/.opendaisugi", 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("OPENDAISUGI_HOME", "")
	t.Setenv("XDG_DATA_HOME", home+"/x")
	want := home + "/x/opendaisugi/coppice/server.sock"
	if got := DefaultCoppiceSocket(home); got != want {
		t.Fatalf("DefaultCoppiceSocket = %q, want %q", got, want)
	}
}

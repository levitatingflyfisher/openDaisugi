package claude

import (
	"testing"

	"github.com/opendaisugi/coppice/internal/pane"
)

// The claude adapter forks by resuming the parent's session with
// --fork-session. Start already builds the binary and --resume from
// StartOpts.Resume, so ForkArgv returns only the extra flag.
func TestForkArgvIsTheForkSessionFlag(t *testing.T) {
	var f pane.Forker = New().(pane.Forker)
	argv, err := f.ForkArgv("sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(argv) != 1 || argv[0] != "--fork-session" {
		t.Fatalf("ForkArgv = %v, want [--fork-session]", argv)
	}
}

func TestForkArgvRefusesAnEmptySessionID(t *testing.T) {
	f := New().(pane.Forker)
	if _, err := f.ForkArgv(""); err == nil {
		t.Fatal("ForkArgv accepted an empty session id")
	}
}

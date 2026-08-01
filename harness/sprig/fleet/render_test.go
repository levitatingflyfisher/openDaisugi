package fleet

import (
	"strings"
	"testing"
)

func TestMinimapAnswersWhoNeedsYouAtAGlance(t *testing.T) {
	jobs := []Job{
		{ID: "auth", Task: "add jwt", State: "running"},
		{ID: "cli", Task: "fix flags", State: "blocked"}, // needs a ruling
		{ID: "docs", Task: "readme", State: "done"},
	}
	out := RenderMinimap(jobs, false)
	// the one question the fleet view exists to answer, in the header
	if !strings.Contains(out, "1 need you") {
		t.Fatalf("header must count who needs you:\n%s", out)
	}
	// every agent is present with its state
	for _, want := range []string{"auth", "running", "cli", "blocked", "docs", "done"} {
		if !strings.Contains(out, want) {
			t.Fatalf("minimap missing %q:\n%s", want, out)
		}
	}
	// the ramp is on screen (doctrine: print the keybindings)
	if !strings.Contains(out, "warp") || !strings.Contains(out, "approve") {
		t.Fatalf("keybinding hints must be on screen:\n%s", out)
	}
}

func TestMinimapColorEncodesState(t *testing.T) {
	jobs := []Job{{ID: "x", Task: "t", State: "failed"}}
	plain := RenderMinimap(jobs, false)
	colored := RenderMinimap(jobs, true)
	if strings.Contains(plain, "\x1b[") {
		t.Fatal("plain must have no ANSI")
	}
	if !strings.Contains(colored, "\x1b[") {
		t.Fatal("colored must encode state with ANSI (color IS the data)")
	}
}

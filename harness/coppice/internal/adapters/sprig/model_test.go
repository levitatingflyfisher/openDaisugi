package sprig

import (
	"os"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/pane"
)

// SetModel names the model for the turns after it. Each turn's argv then
// carries --model NAME after the pane's own args, so it wins over a --model
// the pane started with, and before the task. An empty name takes the swap
// back.
func TestSetModelAddsTheModelFlagToTheNextTurns(t *testing.T) {
	h := start(t, "--model", "first")
	h.setFixture(`{"type":"assistant","id":"a1","ts":1,"model":"m","text":"ok","usage":{},"toolUses":[]}`)
	ms, ok := h.p.(pane.ModelSetter)
	if !ok {
		t.Fatal("the sprig proc is not a ModelSetter")
	}
	ms.SetModel("opus-x")
	if err := h.p.Prompt("one"); err != nil {
		t.Fatal(err)
	}
	waitForIdle(t, h.p)
	b, _ := os.ReadFile(h.argvLog)
	if !strings.Contains(string(b), "--model first --model opus-x -- one") {
		t.Fatalf("argv %q: want the swapped model after the pane's own", b)
	}
	ms.SetModel("")
	if err := h.p.Prompt("two"); err != nil {
		t.Fatal(err)
	}
	waitForIdle(t, h.p)
	b, _ = os.ReadFile(h.argvLog)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	last := lines[len(lines)-1]
	if strings.Contains(last, "opus-x") || !strings.Contains(last, "-- two") {
		t.Fatalf("last argv %q: an empty name should take the swap back", last)
	}
}

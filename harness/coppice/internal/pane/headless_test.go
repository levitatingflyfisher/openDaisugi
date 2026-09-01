package pane

import (
	"errors"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

// pi and OpenCode both hand us a tool name and a summary when they block. A
// floor client cannot render "blocked on what?" without them, so the field
// travels on the event rather than being invented twice in specs 04 and 05.
func TestABlockedEventCarriesItsAsk(t *testing.T) {
	ev := Event{Kind: EvState, State: "blocked",
		Ask: &proto.Ask{ID: "ui_1", Tool: "Write", Summary: "src/main.go", Deadline: 1757300090}}
	st, ok := StateOf(ev)
	if !ok || st != "blocked" {
		t.Fatalf("StateOf = %q %v, want blocked true", st, ok)
	}
	if ev.Ask == nil || ev.Ask.Tool != "Write" {
		t.Fatalf("the ask did not survive: %+v", ev.Ask)
	}
}

func TestWriteTranscriptRendersTextIntoTheGrid(t *testing.T) {
	toolchain.RequireOrSkip(t)
	g, err := NewGrid(60, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	WriteTranscript(g, Event{Kind: EvText, Text: "the model said this"})
	WriteTranscript(g, Event{Kind: EvTool, Tool: "Bash", Detail: "ls -la"})
	WriteTranscript(g, Event{Kind: EvError, Detail: "stream ended early"})
	screen, err := g.Read(ReadVisible)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"the model said this", "Bash", "ls -la", "stream ended early"} {
		if !strings.Contains(screen, want) {
			t.Fatalf("grid = %q, want it to contain %q", screen, want)
		}
	}
}

// A multi-line event must not leave the cursor mid-row: the next event would
// then be appended to the wrong place. Every write ends at column zero.
func TestWriteTranscriptEndsEveryEventOnItsOwnLine(t *testing.T) {
	toolchain.RequireOrSkip(t)
	g, err := NewGrid(60, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	WriteTranscript(g, Event{Kind: EvText, Text: "first\nsecond"})
	WriteTranscript(g, Event{Kind: EvText, Text: "third"})
	screen, _ := g.Read(ReadVisible)
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	if len(lines) < 3 || !strings.HasPrefix(lines[2], "third") {
		t.Fatalf("lines = %q, want third to start its own line", lines)
	}
}

func TestStateOfMapsEventsToStates(t *testing.T) {
	cases := []struct {
		ev    Event
		state string
		ok    bool
	}{
		{Event{Kind: EvText}, StateWorkingStr, true},
		{Event{Kind: EvTool, Tool: "Bash"}, StateWorkingStr, true},
		{Event{Kind: EvState, State: "blocked",
			Ask: &proto.Ask{ID: "a1", Tool: "Write", Summary: "src/main.go"}}, "blocked", true},
		{Event{Kind: EvEnd, State: "done"}, "done", true},
		{Event{Kind: EvEnd}, "done", true},
		// A parse error marks the pane unknown, never idle. Master spec 3.6.
		{Event{Kind: EvError, Detail: "bad json"}, "unknown", true},
	}
	for _, c := range cases {
		got, ok := StateOf(c.ev)
		if ok != c.ok || got != c.state {
			t.Fatalf("StateOf(%+v) = %q %v, want %q %v", c.ev, got, ok, c.state, c.ok)
		}
	}
}

// WriteTranscript must hand its caller the grid's own
// write error instead of swallowing it, so pumpAdapter can tell "the grid is
// closed" apart from "nothing went wrong" and stop trying to render without
// pretending the event was ever drawn.
func TestWriteTranscriptReturnsTheGridsWriteError(t *testing.T) {
	toolchain.RequireOrSkip(t)
	g, err := NewGrid(60, 10)
	if err != nil {
		t.Fatal(err)
	}
	g.Close()
	if err := WriteTranscript(g, Event{Kind: EvText, Text: "too late"}); !errors.Is(err, ErrGridClosed) {
		t.Fatalf("WriteTranscript on a closed grid = %v, want ErrGridClosed", err)
	}
}

func TestAnErrorEventNeverProducesIdle(t *testing.T) {
	for _, detail := range []string{"", "truncated", "unexpected end of JSON input"} {
		got, _ := StateOf(Event{Kind: EvError, Detail: detail})
		if got == "idle" {
			t.Fatalf("an error event with detail %q produced idle", detail)
		}
	}
}

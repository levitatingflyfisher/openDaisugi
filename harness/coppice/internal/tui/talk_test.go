package tui

import (
	"strings"
	"testing"
	"time"
)

// readUntil polls a pane's screen until it holds want, read as one line.
func readUntil(t *testing.T, socket, pane, want string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		res := rawCall(t, socket, "pane.read", map[string]any{"pane": pane})
		text, _ := res["text"].(string)
		text = strings.ReplaceAll(text, "\n", "")
		if strings.Contains(text, want) {
			return text
		}
		if time.Now().After(deadline) {
			t.Fatalf("pane %s never showed %q: %q", pane, want, text)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTalkGoesToTheForeman(t *testing.T) {
	s := newTestServer(t)
	foremanConfig(t)
	res := rawCall(t, s.Socket(), "floor.foreman", map[string]any{"harness": "pi"})
	id, _ := res["pane"].(string)
	var out strings.Builder
	err := Run(Options{Socket: s.Socket(), In: scripted("\x14please tidy the docs folder today\r", "\x03"),
		Out: &out, Size: func() (int, int) { return 80, 24 }, Cwd: t.TempDir(), Default: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stripSGR(lastFrame(out.String())), "Sent to foreman.") {
		t.Fatalf("talk did not say where the words went: %q", lastFrame(out.String()))
	}
	// The page goes first, once the screen is quiet, then the words.
	deadline := time.Now().Add(40 * time.Second)
	for {
		read := rawCall(t, s.Socket(), "pane.read", map[string]any{"pane": id})
		text, _ := read["text"].(string)
		if strings.Contains(strings.ReplaceAll(text, "\n", ""), "please tidy the docs folder today") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the words never reached the foreman: %q", text)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// With no foreman running, the first sentence starts one: the default
// harness, labelled foreman, in the foreman's own directory.
func TestTalkWithNoForemanStartsOne(t *testing.T) {
	s := newTestServer(t)
	foremanConfig(t)
	var out strings.Builder
	err := Run(Options{Socket: s.Socket(), In: scripted("\x14please tidy the docs folder today\r", "\x03"),
		Out: &out, Size: func() (int, int) { return 80, 24 }, Cwd: t.TempDir(), Default: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stripSGR(lastFrame(out.String())), "foreman starts and gets its page") {
		t.Fatalf("no answer: %q", lastFrame(out.String()))
	}
	res := rawCall(t, s.Socket(), "pane.list", nil)
	rows, _ := res["panes"].([]any)
	if len(rows) != 1 {
		t.Fatalf("panes = %v, want one foreman", rows)
	}
	row, _ := rows[0].(map[string]any)
	if row["label"] != "foreman" || row["harness"] != "pi" || !strings.HasSuffix(str(row, "cwd"), "/foreman") {
		t.Fatalf("row = %v, want the pi foreman in its own directory", row)
	}
}

// Notes the server kept draw dim above the prompt, the last three only.
func TestNotesDrawDimAboveThePrompt(t *testing.T) {
	s := newTestServer(t)
	for _, n := range []string{"floor › one", "floor › two", "floor › pane.create  docs  pi", "floor › four"} {
		rawCall(t, s.Socket(), "floor.note", map[string]any{"text": n})
	}
	out, err := runFloor(t, s, scripted("\x03"))
	if err != nil {
		t.Fatal(err)
	}
	first := splitFrames(out)[0]
	if !strings.Contains(first, sgrFaint+"  floor › pane.create  docs  pi") {
		t.Fatalf("the note is not dim in the frame: %q", first)
	}
	if strings.Contains(first, "floor › one") {
		t.Fatalf("more than three notes drawn: %q", first)
	}
}

// A note event folds into the model, and the render draws it dim.
func TestANoteEventRendersDim(t *testing.T) {
	m := &Model{}
	m.AddNote("floor › pane.create  docs  pi")
	lines := Render(m, 80, 10)
	found := false
	for _, l := range lines {
		if strings.HasPrefix(l, sgrFaint) && strings.Contains(l, "  floor › pane.create  docs  pi") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no dim note line: %q", lines)
	}
	if !strings.HasPrefix(stripSGR(lines[len(lines)-1]), promptMark) {
		t.Fatalf("the prompt is not the last line: %q", lines[len(lines)-1])
	}
}

package tui

import (
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/detect"
)

// foremanConfig writes a config whose pi runs cat, so the line the floor
// sends comes back on the pane's screen and no real harness ever runs.
func foremanConfig(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := config.Save(config.Config{Default: "pi", Harness: map[string]config.Harness{
		"pi": {Command: "sh", Args: []string{"-c", "echo ready; exec cat"}},
	}}); err != nil {
		t.Fatal(err)
	}
}

func TestForemanWithAHarnessOpensAFloorPaneAndPromotesIt(t *testing.T) {
	s := newTestServer(t)
	foremanConfig(t)
	// The floor quits at once. promote goes on and sends the line once
	// the pane printed and went quiet.
	if _, err := runFloor(t, s, scripted("\x14foreman pi\r", "\x03")); err != nil {
		t.Fatal(err)
	}
	res := rawCall(t, s.Socket(), "pane.list", nil)
	rows, _ := res["panes"].([]any)
	if len(rows) != 1 {
		t.Fatalf("panes = %v, want one", rows)
	}
	row, _ := rows[0].(map[string]any)
	if row["label"] != "foreman" || row["harness"] != "pi" {
		t.Fatalf("row = %v, want label foreman and harness pi", row)
	}
	id, _ := row["id"].(string)
	deadline := time.Now().Add(15 * time.Second)
	for {
		read := rawCall(t, s.Socket(), "pane.read", map[string]any{"pane": id})
		// The pane wraps at its width, so the screen is read as one line.
		text, _ := read["text"].(string)
		text = strings.ReplaceAll(text, "\n", "")
		if strings.Contains(text, "Say ready.") {
			if !strings.Contains(text, PromotionLine) {
				t.Fatalf("the pane got %q, want the promotion line", text)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the promotion line never reached the pane: %v", read)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cfg, found, err := config.Load()
	if err != nil || !found {
		t.Fatalf("config: %v %v", found, err)
	}
	if _, ok := cfg.Harness["pi"]; !ok {
		t.Fatal("saving the foreman dropped the harness tables")
	}
}

func TestForemanAloneNamesTheForemanOrSaysThereIsNone(t *testing.T) {
	s := newTestServer(t)
	foremanConfig(t)
	out, err := runFloor(t, s, scripted("\x14foreman\r", "\x03"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stripSGR(lastFrame(out)), "no foreman yet. Say a sentence and one starts, or type: foreman claude") {
		t.Fatalf("no answer for a floor with no foreman: %q", lastFrame(out))
	}
	// A pane labelled foreman that the server did not start is not the
	// foreman.
	other := createShellPane(t, s.Socket(), "sh", "-c", "echo hi; sleep 30")
	rawCall(t, s.Socket(), "pane.rename", map[string]any{"pane": other, "label": "foreman"})
	out, err = runFloor(t, s, scripted("\x14foreman\r", "\x03"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stripSGR(lastFrame(out)), "no foreman yet.") {
		t.Fatalf("a pane that is only labelled foreman was named: %q", lastFrame(out))
	}
	res := rawCall(t, s.Socket(), "floor.foreman", map[string]any{"harness": "pi"})
	id, _ := res["pane"].(string)
	out, err = runFloor(t, s, scripted("\x14foreman\r", "\x03"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stripSGR(lastFrame(out)), "foreman: foreman ("+id+")") {
		t.Fatalf("the tracked foreman is not named: %q", lastFrame(out))
	}
}

func TestForemanIsAPlumbingWord(t *testing.T) {
	if Classify("foreman pi", Harnesses) != Plumbing {
		t.Fatal("foreman pi reads as talk")
	}
}

// syncBuf is a writer the floor and the test share.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A harness that never goes idle, a start question or a busy screen, gets
// no page and no Enter. The server's note says so on the floor.
func TestForemanSendsNothingToAPaneThatNeverGoesIdle(t *testing.T) {
	s := newTestServer(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := config.Save(config.Config{Default: "pi", Harness: map[string]config.Harness{
		"pi": {Command: "sh", Args: []string{"-c", "while :; do echo busy; sleep 0.2; done"}},
	}}); err != nil {
		t.Fatal(err)
	}
	inR, inW := io.Pipe()
	out := &syncBuf{}
	done := make(chan error, 1)
	go func() {
		done <- Run(Options{Socket: s.Socket(), In: inR, Out: out,
			Size: func() (int, int) { return 80, 24 }, Cwd: t.TempDir(), Default: "pi"})
	}()
	_, _ = io.WriteString(inW, "\x14foreman pi\r")
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(stripSGR(out.String()), "foreman is not ready yet") {
		if time.Now().After(deadline) {
			t.Fatalf("no timeout message: %q", lastFrame(out.String()))
		}
		time.Sleep(50 * time.Millisecond)
	}
	_, _ = io.WriteString(inW, "\x03")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	res := rawCall(t, s.Socket(), "pane.list", nil)
	rows, _ := res["panes"].([]any)
	row, _ := rows[0].(map[string]any)
	read := rawCall(t, s.Socket(), "pane.read", map[string]any{"pane": row["id"]})
	if text, _ := read["text"].(string); strings.Contains(text, "Say ready") {
		t.Fatalf("the line reached a pane that never went idle: %q", text)
	}
}

// A harness that draws a question and then goes quiet stays blocked, and
// promotion sends it nothing: no line and no Enter to answer the question.
func TestForemanSendsNothingToAQuestionThatWentQuiet(t *testing.T) {
	quietQuestion(t, `printf "Run a dynamic workflow?\n1. Yes\n2. No\nEsc to cancel\n"`)
}

// Claude Code's trust question on a new folder is one such question.
func TestForemanSendsNothingToTheTrustQuestion(t *testing.T) {
	quietQuestion(t, `printf " Accessing workspace:\n\n Quick safety check: Is this a project you created or one you trust?\n\n `+
		`❯ 1. Yes, I trust this folder\n   2. No, exit\n\n Enter to confirm . Esc to exit\n"`)
}

// quietQuestion runs foreman claude against a fake claude that prints
// draw, waits for a key, and goes quiet. The pane must stay blocked and get
// nothing.
func quietQuestion(t *testing.T, draw string) {
	t.Helper()
	s := newTestServer(t)
	set, err := detect.LoadSet(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.srv.StartManifestTick(set)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	question := draw + `; read x; echo "ANSWERED $x"; exec cat`
	if err := config.Save(config.Config{Default: "claude", Harness: map[string]config.Harness{
		"claude": {Command: "sh", Args: []string{"-c", question}},
	}}); err != nil {
		t.Fatal(err)
	}
	inR, inW := io.Pipe()
	out := &syncBuf{}
	done := make(chan error, 1)
	go func() {
		done <- Run(Options{Socket: s.Socket(), In: inR, Out: out,
			Size: func() (int, int) { return 80, 24 }, Cwd: t.TempDir(), Default: "claude"})
	}()
	_, _ = io.WriteString(inW, "\x14foreman claude\r")
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(stripSGR(out.String()), "foreman is not ready yet") {
		if time.Now().After(deadline) {
			t.Fatalf("no timeout message: %q", lastFrame(out.String()))
		}
		time.Sleep(50 * time.Millisecond)
	}
	_, _ = io.WriteString(inW, "\x03")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// The question reads blocked once its screen is quiet. The server
	// waits the whole time, so nothing reaches it.
	var row map[string]any
	deadline = time.Now().Add(15 * time.Second)
	for {
		res := rawCall(t, s.Socket(), "pane.list", nil)
		rows, _ := res["panes"].([]any)
		row, _ = rows[0].(map[string]any)
		if row["state"] == "blocked" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the quiet question reads %v/%v, want blocked", row["state"], row["source"])
		}
		time.Sleep(100 * time.Millisecond)
	}
	read := rawCall(t, s.Socket(), "pane.read", map[string]any{"pane": row["id"]})
	if text, _ := read["text"].(string); strings.Contains(text, "ANSWERED") || strings.Contains(text, "Say ready") {
		t.Fatalf("the question got an answer: %q", text)
	}
}

package sprig

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newCLI(final string) *CLI {
	return &CLI{
		Version:  "9.9.9",
		NewModel: func() (Model, error) { return &scriptModel{turns: []Message{{Role: "assistant", Text: final}}}, nil },
	}
}

func TestCLIVersion(t *testing.T) {
	var out, errb bytes.Buffer
	code := (&CLI{Version: "9.9.9"}).Run([]string{"--version"}, strings.NewReader(""), &out, &errb)
	if code != 0 || !strings.Contains(out.String(), "9.9.9") {
		t.Fatalf("code=%d out=%q", code, out.String())
	}
}

func TestCLIRunsTaskToStdout(t *testing.T) {
	var out, errb bytes.Buffer
	code := newCLI("the answer is 42").Run([]string{"what is 6x7?"}, strings.NewReader(""), &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, errb.String())
	}
	if strings.TrimSpace(out.String()) != "the answer is 42" {
		t.Fatalf("stdout=%q", out.String())
	}
}

func TestCLINoTaskFailsHelpfully(t *testing.T) {
	var out, errb bytes.Buffer
	code := (&CLI{Version: "0"}).Run([]string{}, strings.NewReader(""), &out, &errb)
	if code == 0 {
		t.Fatal("no task must be a non-zero exit")
	}
	if errb.Len() == 0 {
		t.Fatal("no task must explain itself on stderr")
	}
}

func TestCLIReadsTaskFromStdinDash(t *testing.T) {
	var out, errb bytes.Buffer
	code := newCLI("ok").Run([]string{"-"}, strings.NewReader("do the thing"), &out, &errb)
	if code != 0 || strings.TrimSpace(out.String()) != "ok" {
		t.Fatalf("code=%d out=%q", code, out.String())
	}
}

// --- --session-dir / --session / --resume: the CLI writes and resumes a session tree ---

func TestCLIResumeWithoutSessionDirRefusesUpFront(t *testing.T) {
	// --resume without --session-dir was previously silently ignored (the
	// whole session-tree block is gated on *sessionDir != "", so --resume
	// alone just ran a normal fresh task with no hint anything was wrong).
	// A user who explicitly asked to resume a session deserves a clear
	// refusal, not a run that quietly forgot the ask.
	var out, errb bytes.Buffer
	code := newCLI("should never run").Run([]string{"--resume", "s2", "say hi"},
		strings.NewReader(""), &out, &errb)
	if code == 0 {
		t.Fatal("--resume without --session-dir must be a non-zero exit")
	}
	if !strings.Contains(errb.String(), "--resume") || !strings.Contains(errb.String(), "--session-dir") {
		t.Fatalf("stderr must name both flags, got %q", errb.String())
	}
	if out.Len() != 0 {
		t.Fatalf("must not run the task at all, got stdout=%q", out.String())
	}
}

func TestCLIWithSessionDirWritesASessionFile(t *testing.T) {
	dir := t.TempDir()
	var out, errb bytes.Buffer
	code := newCLI("done").Run([]string{"--session-dir", dir, "--session", "s1", "say hi"},
		strings.NewReader(""), &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, errb.String())
	}
	entries, err := ReadEntries(filepath.Join(dir, "s1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, e := range entries {
		types = append(types, e.Type)
	}
	if len(types) < 3 || types[0] != "session" || types[1] != "prompt" || types[2] != "assistant" {
		t.Fatalf("want session/prompt/assistant, got %v", types)
	}
	if !strings.Contains(errb.String(), "session s1") {
		t.Fatalf("want the session id announced on stderr, got %q", errb.String())
	}
}

func TestCLIWithoutSessionDirWritesNoSessionFile(t *testing.T) {
	dir := t.TempDir() // never passed as --session-dir
	var out, errb bytes.Buffer
	code := newCLI("done").Run([]string{"say hi"}, strings.NewReader(""), &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("no --session-dir must write nothing, found %v", entries)
	}
}

func TestCLIResumeContinuesAnExistingSessionAndFeedsItsHistoryToTheModel(t *testing.T) {
	dir := t.TempDir()
	// First run: creates the session and a prior turn.
	first := newCLI("first answer")
	var out1, errb1 bytes.Buffer
	if code := first.Run([]string{"--session-dir", dir, "--session", "s2", "first task"},
		strings.NewReader(""), &out1, &errb1); code != 0 {
		t.Fatalf("first run: exit %d, stderr=%s", code, errb1.String())
	}

	// Second run: --resume must reopen s2 (not create a fresh, colliding
	// file) and feed the model a history that includes the first turn.
	var gotHistory []Message
	cli := &CLI{
		Version: "9.9.9",
		NewModel: func() (Model, error) {
			return &captureHistoryModel{reply: "second answer", captured: &gotHistory}, nil
		},
	}
	var out2, errb2 bytes.Buffer
	code := cli.Run([]string{"--session-dir", dir, "--resume", "s2", "second task"},
		strings.NewReader(""), &out2, &errb2)
	if code != 0 {
		t.Fatalf("resumed run: exit %d, stderr=%s", code, errb2.String())
	}
	if strings.TrimSpace(out2.String()) != "second answer" {
		t.Fatalf("stdout=%q", out2.String())
	}
	if !containsUserMsg(gotHistory, "first task") {
		t.Fatalf("resumed history must carry the earlier prompt, got %+v", gotHistory)
	}
	if !containsUserMsg(gotHistory, "second task") {
		t.Fatalf("resumed history must carry the new task too, got %+v", gotHistory)
	}

	entries, err := ReadEntries(filepath.Join(dir, "s2.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 5 { // session, prompt, assistant, prompt, assistant (appended, not replaced)
		t.Fatalf("resume must APPEND to the same file, got %d entries: %+v", len(entries), entries)
	}
}

// captureHistoryModel records the history it was asked to continue from, so
// a test can prove --resume's rebuilt history actually reached the model.
type captureHistoryModel struct {
	reply    string
	captured *[]Message
}

func (m *captureHistoryModel) Next(history []Message) (Message, error) {
	*m.captured = history
	return Message{Role: "assistant", Text: m.reply}, nil
}

func TestCLIJSONOutputIsMachineReadable(t *testing.T) {
	var out, errb bytes.Buffer
	code := newCLI("hi").Run([]string{"--json", "x"}, strings.NewReader(""), &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("stdout not JSON: %q", out.String())
	}
	if m["answer"] != "hi" {
		t.Fatalf("got %v", m)
	}
}

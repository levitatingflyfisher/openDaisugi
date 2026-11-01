package sprig

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// helpCLI is a CLI whose model replays the scripted turns and records the
// history of each call.
func helpCLI(turns []Message, seen *[][]Message) *CLI {
	return &CLI{Version: "t", NewModel: func(ModelOptions) (Model, error) {
		return &recordingScript{turns: turns, seen: seen}, nil
	}}
}

type recordingScript struct {
	turns []Message
	i     int
	seen  *[][]Message
}

func (m *recordingScript) Next(h []Message) (Message, error) {
	*m.seen = append(*m.seen, append([]Message{}, h...))
	msg := m.turns[m.i]
	m.i++
	return msg, nil
}

func helpCall(kind, text string) Message {
	return Message{Role: "assistant", Calls: []ToolCall{{Name: "help", Input: map[string]any{"kind": kind, "text": text}}}}
}

func TestHelpEndsTheRunWithItsKindAndText(t *testing.T) {
	for _, kind := range HelpKinds {
		var seen [][]Message
		// A second turn the run must never take.
		cli := helpCLI([]Message{helpCall(kind, "which db?"), {Role: "assistant", Text: "never"}}, &seen)
		var out, errb bytes.Buffer
		code := cli.Run([]string{"--json", "--tools", "read,help", "task"}, strings.NewReader(""), &out, &errb)
		if code != 0 {
			t.Fatalf("%s: exit %d stderr=%s", kind, code, errb.String())
		}
		var doc map[string]any
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if doc["status"] != "help" || doc["help_kind"] != kind || doc["help_text"] != "which db?" || doc["answer"] != "which db?" {
			t.Fatalf("%s: %v", kind, doc)
		}
		if len(seen) != 1 {
			t.Fatalf("%s: the model was asked %d times", kind, len(seen))
		}
	}
}

func TestHelpWithABadKindOrNoTextIsAnErrorTheModelSees(t *testing.T) {
	var seen [][]Message
	cli := helpCLI([]Message{helpCall("shout", "x"), helpCall("need_info", "  "), {Role: "assistant", Text: "ok"}}, &seen)
	var out, errb bytes.Buffer
	code := cli.Run([]string{"--json", "--tools", "help", "task"}, strings.NewReader(""), &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d %s", code, errb.String())
	}
	if strings.Contains(out.String(), `"status"`) {
		t.Fatalf("a bad call must not end the run as help: %s", out.String())
	}
	last := seen[2]
	if !strings.Contains(last[2].Text, "kind is one of need_info, escalate, give_up") || !strings.Contains(last[4].Text, "text is empty") {
		t.Fatalf("the errors did not reach the model: %+v", last)
	}
}

func TestHelpIsOnlyOfferedWhenTheWallNamesIt(t *testing.T) {
	var seen [][]Message
	cli := helpCLI([]Message{helpCall("need_info", "q"), {Role: "assistant", Text: "done"}}, &seen)
	var out, errb bytes.Buffer
	code := cli.Run([]string{"--tools", "read", "task"}, strings.NewReader(""), &out, &errb)
	if code != 0 || strings.TrimSpace(out.String()) != "done" {
		t.Fatalf("exit %d out=%q", code, out.String())
	}
	if got := seen[1][2].Text; got != "REFUSED by the warden: unknown tool: help" {
		t.Fatalf("help outside the wall must be an unknown tool, got %q", got)
	}
}

func TestHelpSkipsTheWardenButReachesTheSessionTree(t *testing.T) {
	dir := t.TempDir()
	var seen [][]Message
	cli := helpCLI([]Message{helpCall("escalate", "owner call")}, &seen)
	var out, errb bytes.Buffer
	// --warden with a command that is not there would deny every call.
	code := cli.Run([]string{"--json", "--warden", "--warden-cmd", "/nonexistent/warden", "--tools", "help",
		"--session-dir", dir, "--session", "w1", "task"}, strings.NewReader(""), &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d %s", code, errb.String())
	}
	entries, err := ReadEntries(filepath.Join(dir, "w1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range entries {
		kinds = append(kinds, e.Type)
	}
	if strings.Join(kinds, ",") != "session,prompt,assistant,tool_call,verdict,tool_result" {
		t.Fatalf("the help call must leave a full session record, got %v", kinds)
	}
}

func TestHelpResumeAppendsTheAnswerAfterTheToolResult(t *testing.T) {
	dir := t.TempDir()
	var seen [][]Message
	first := helpCLI([]Message{helpCall("need_info", "which db?")}, &seen)
	var out, errb bytes.Buffer
	if code := first.Run([]string{"--json", "--tools", "help", "--session-dir", dir, "--session", "w1", "task"},
		strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("first: %d %s", code, errb.String())
	}
	var seen2 [][]Message
	second := helpCLI([]Message{{Role: "assistant", Text: "used sqlite"}}, &seen2)
	out.Reset()
	if code := second.Run([]string{"--json", "--tools", "help", "--session-dir", dir, "--resume", "w1", "--session", "w2", "sqlite"},
		strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("second: %d %s", code, errb.String())
	}
	h := seen2[0]
	roles := ""
	for _, m := range h {
		roles += m.Role[:1]
	}
	if roles != "uatu" || h[3].Text != "sqlite" || !strings.Contains(h[2].Text, "Your question was sent") {
		t.Fatalf("resumed history is wrong: %+v", h)
	}
}

func TestHelpDescriptionTellsWhenToUseIt(t *testing.T) {
	for _, want := range []string{"stuck after a few tries", "lacks a fact", "owner's decision", "inside your envelope", "Never guess past a missing fact"} {
		if !strings.Contains(HelpDescription, want) {
			t.Errorf("the description lacks %q", want)
		}
	}
	if len(apiToolsFor([]string{"help"})) != 1 {
		t.Fatal("the API wires offer help")
	}
	if !strings.Contains(formatPromptFor(nil, []string{"read", "help"}), "two tools") {
		t.Fatal("the prompt counts the tools")
	}
}

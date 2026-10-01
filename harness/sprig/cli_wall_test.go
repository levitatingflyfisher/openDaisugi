package sprig

import (
	"bytes"
	"strings"
	"testing"
)

// --- --tools, --model and the usage in --json -------------------------------

// optsCLI records the ModelOptions the CLI hands the backend and answers
// from a script.
func optsCLI(got *ModelOptions, turns ...Message) *CLI {
	return &CLI{
		Version: "9.9.9",
		NewModel: func(o ModelOptions) (Model, error) {
			*got = o
			return &scriptModel{turns: turns}, nil
		},
	}
}

func TestCLIToolsDefaultIsAllFour(t *testing.T) {
	var got ModelOptions
	var out, errb bytes.Buffer
	code := optsCLI(&got, Message{Role: "assistant", Text: "ok"}).Run([]string{"x"}, strings.NewReader(""), &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, errb.String())
	}
	if strings.Join(got.Tools, ",") != "read,write,edit,bash" {
		t.Fatalf("default wall = %v", got.Tools)
	}
	if got.Model != "" {
		t.Fatalf("no --model must leave the backend's default, got %q", got.Model)
	}
}

func TestCLIToolsWallKeepsSprigsOrder(t *testing.T) {
	var got ModelOptions
	var out, errb bytes.Buffer
	code := optsCLI(&got, Message{Role: "assistant", Text: "ok"}).Run([]string{"--tools", "bash,read,bash", "x"},
		strings.NewReader(""), &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, errb.String())
	}
	if strings.Join(got.Tools, ",") != "read,bash" {
		t.Fatalf("wall must keep sprig's order once each, got %v", got.Tools)
	}
}

func TestCLIToolsWallRefusesACallOutsideIt(t *testing.T) {
	var out, errb bytes.Buffer
	var seen []Message
	cli := &CLI{Version: "1", NewModel: func(ModelOptions) (Model, error) {
		return &recordingModel{turns: []Message{
			{Role: "assistant", Calls: []ToolCall{{Name: "bash", Input: map[string]any{"cmd": "echo hi"}}}},
			{Role: "assistant", Text: "could not"},
		}, seen: &seen}, nil
	}}
	code := cli.Run([]string{"--tools", "read", "x"}, strings.NewReader(""), &out, &errb)
	if code != 0 || strings.TrimSpace(out.String()) != "could not" {
		t.Fatalf("exit %d out=%q stderr=%q", code, out.String(), errb.String())
	}
	if len(seen) < 3 || seen[2].Text != "REFUSED by the gate: unknown tool: bash" {
		t.Fatalf("a call outside the wall must be refused as an unknown tool, got %+v", seen)
	}
}

func TestCLIToolsEmptyOrUnknownExit2(t *testing.T) {
	for _, tc := range []struct{ arg, want string }{
		{"", "sprig: --tools is empty; name one or more of read, write, edit, bash\n"},
		{"read,grep", "sprig: --tools: unknown tool \"grep\"; the tools are read, write, edit, bash\n"},
		{"read,", "sprig: --tools: unknown tool \"\"; the tools are read, write, edit, bash\n"},
	} {
		var got ModelOptions
		var out, errb bytes.Buffer
		code := optsCLI(&got, Message{Role: "assistant", Text: "no"}).Run([]string{"--tools", tc.arg, "x"},
			strings.NewReader(""), &out, &errb)
		if code != 2 || errb.String() != tc.want || out.Len() != 0 {
			t.Fatalf("--tools %q: exit %d stderr=%q stdout=%q", tc.arg, code, errb.String(), out.String())
		}
	}
}

func TestCLIModelFlagReachesTheBackend(t *testing.T) {
	var got ModelOptions
	var out, errb bytes.Buffer
	code := optsCLI(&got, Message{Role: "assistant", Text: "ok"}).Run([]string{"--model", "sonnet", "x"},
		strings.NewReader(""), &out, &errb)
	if code != 0 || got.Model != "sonnet" {
		t.Fatalf("exit %d model=%q", code, got.Model)
	}
}

func TestCLIJSONSumsUsageAndNamesTheModel(t *testing.T) {
	var out, errb bytes.Buffer
	cli := &CLI{Version: "1", NewModel: func(o ModelOptions) (Model, error) {
		return &namedModel{scriptModel{turns: []Message{
			{Role: "assistant", Calls: []ToolCall{{Name: "read", Input: map[string]any{"path": "nope"}}},
				Usage: Usage{Fresh: 1, CacheRead: 2, Out: 3}},
			{Role: "assistant", Text: "done", Usage: Usage{Fresh: 10, CacheRead: 20, Out: 30}},
		}}, "m-1"}, nil
	}}
	code := cli.Run([]string{"--json", "x"}, strings.NewReader(""), &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	want := `{"answer":"done","model":"m-1","turns":4,"usage":{"cache_read_input_tokens":22,"input_tokens":11,"output_tokens":33}}` + "\n"
	if out.String() != want {
		t.Fatalf("got  %s\nwant %s", out.String(), want)
	}
}

func TestCLIJSONUsageEmptyWhenNoTurnReportsIt(t *testing.T) {
	var out, errb bytes.Buffer
	code := newCLI("hi").Run([]string{"--json", "x"}, strings.NewReader(""), &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	want := `{"answer":"hi","model":"","turns":2,"usage":{}}` + "\n"
	if out.String() != want {
		t.Fatalf("got  %s\nwant %s", out.String(), want)
	}
}

func TestPromptListsOnlyTheWall(t *testing.T) {
	// The full wall gives the prompt from before --tools, byte for byte.
	const head = "You are sprig, a minimal coding agent. You have four tools:\n" +
		"  read  {\"path\"}          — return a file's contents\n" +
		"  write {\"path\",\"content\"} — write a file\n" +
		"  edit  {\"path\",\"old\",\"new\"} — replace a unique string\n" +
		"  bash  {\"cmd\"}           — run a shell command\n\n" +
		"To CALL a tool, reply with ONLY this and nothing else:\n" +
		"```sprig-tool\n{\"tool\":\"<name>\",\"input\":{...}}\n```\n" +
		"To FINISH, reply with your final answer as plain text (no block).\n\n" +
		"Example — call a tool:\n" +
		"```sprig-tool\n{\"tool\":\"read\",\"input\":{\"path\":\"main.go\"}}\n```\n" +
		"Example — finish (plain text, no block):\n" +
		"main.go defines three functions: New, Run, and Close.\n\n" +
		"Conversation so far:\n[user] t\n\nYour reply:"
	if got := formatPromptFor([]Message{{Role: "user", Text: "t"}}, AllToolNames); got != head {
		t.Fatalf("the full wall must give today's prompt byte for byte:\n%q", got)
	}
	p := formatPromptFor([]Message{{Role: "user", Text: "t"}}, []string{"bash"})
	if !strings.HasPrefix(p, "You are sprig, a minimal coding agent. You have one tool:\n  bash  {\"cmd\"}") {
		t.Fatalf("prompt head = %q", p[:120])
	}
	if strings.Contains(p, "read  {") || !strings.Contains(p, `{"tool":"bash","input":{"cmd":"ls"}}`) {
		t.Fatalf("a tool outside the wall must not be offered:\n%s", p)
	}
}

func TestAPIToolsAndSystemPromptFollowTheWall(t *testing.T) {
	if len(apiToolsFor(AllToolNames)) != 4 || apiSystemPromptFor(AllToolNames) != apiSystemPrompt {
		t.Fatal("the full wall must give today's tools and system prompt")
	}
	tools := apiToolsFor([]string{"read", "edit"})
	if len(tools) != 2 || tools[0]["name"] != "read" || tools[1]["name"] != "edit" {
		t.Fatalf("tools = %v", tools)
	}
	if !strings.Contains(apiSystemPromptFor([]string{"read", "edit"}), "(read, edit)") {
		t.Fatal(apiSystemPromptFor([]string{"read", "edit"}))
	}
}

type namedModel struct {
	scriptModel
	name string
}

func (m *namedModel) ModelName() string { return m.name }

// recordingModel answers from a script and keeps the last history it saw.
type recordingModel struct {
	turns []Message
	i     int
	seen  *[]Message
}

func (m *recordingModel) Next(h []Message) (Message, error) {
	*m.seen = append([]Message{}, h...)
	msg := m.turns[m.i]
	m.i++
	return msg, nil
}

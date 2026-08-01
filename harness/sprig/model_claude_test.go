package sprig

import (
	"strings"
	"testing"
)

func TestParseResponseExtractsAToolCall(t *testing.T) {
	text := "I'll list the files.\n```sprig-tool\n{\"tool\":\"bash\",\"input\":{\"cmd\":\"ls\"}}\n```\n"
	msg := parseResponse(text)
	if len(msg.Calls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(msg.Calls))
	}
	if msg.Calls[0].Name != "bash" || msg.Calls[0].Input["cmd"] != "ls" {
		t.Fatalf("bad call: %+v", msg.Calls[0])
	}
}

func TestParseResponsePlainTextIsFinal(t *testing.T) {
	msg := parseResponse("The file contains three functions.")
	if len(msg.Calls) != 0 {
		t.Fatal("plain text must not be parsed as a tool call")
	}
	if msg.Text != "The file contains three functions." {
		t.Fatalf("got %q", msg.Text)
	}
}

func TestParseResponseMalformedBlockDegradesToText(t *testing.T) {
	// A broken tool block must NOT crash the loop — treat it as a final answer so
	// the run ends cleanly rather than panicking on the model's mistake.
	msg := parseResponse("```sprig-tool\n{not valid json\n```")
	if len(msg.Calls) != 0 {
		t.Fatal("a malformed block must not yield a phantom call")
	}
	if msg.Text == "" {
		t.Fatal("a malformed block should fall back to the raw text")
	}
}

func TestFormatPromptCarriesTaskToolsAndProtocol(t *testing.T) {
	p := formatPrompt([]Message{{Role: "user", Text: "count the TODOs"}})
	for _, want := range []string{"count the TODOs", "read", "write", "edit", "bash", "sprig-tool"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt missing %q", want)
		}
	}
}

func TestClaudeCodeModelNextUsesTheInjectedRunner(t *testing.T) {
	// Prove Next() drives the whole path — format → run → parse — with a FAKE
	// runner, so no live `claude -p` call (and no quota) is needed to test it.
	m := &ClaudeCodeModel{run: func(string) (string, error) {
		return "```sprig-tool\n{\"tool\":\"read\",\"input\":{\"path\":\"x.go\"}}\n```", nil
	}}
	msg, err := m.Next([]Message{{Role: "user", Text: "read x.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.Calls) != 1 || msg.Calls[0].Name != "read" {
		t.Fatalf("Next did not parse the tool call: %+v", msg)
	}
}

func TestClaudeCodeModelNextSetsTheConfiguredModelNameAndLeavesUsageZero(t *testing.T) {
	// `claude -p`'s plain-text output carries no token counts — Usage stays
	// zero HONESTLY (there is nothing to report on this backend), but Model
	// is real: it's the ClaudeCodeModel's own configured value, not a guess.
	m := &ClaudeCodeModel{Model: "haiku", run: func(string) (string, error) {
		return "final answer", nil
	}}
	msg, err := m.Next([]Message{{Role: "user", Text: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Model != "haiku" {
		t.Fatalf("want the configured model name, got %q", msg.Model)
	}
	if msg.Usage != (Usage{}) {
		t.Fatalf("want zero usage (genuinely unavailable on this backend), got %+v", msg.Usage)
	}
}

func TestAgentRunsEndToEndWithClaudeCodeModelFake(t *testing.T) {
	// The FULL harness, end to end, with a fake claude-code backend: it calls a
	// tool (gated) then finishes. Proves sprig is functional; only the live model
	// substitution is quota-gated.
	step := 0
	m := &ClaudeCodeModel{run: func(string) (string, error) {
		step++
		if step == 1 {
			return "```sprig-tool\n{\"tool\":\"bash\",\"input\":{\"cmd\":\"echo hi\"}}\n```", nil
		}
		return "done — it printed hi", nil
	}}
	agent := &Agent{Model: m, Exec: NewExecutor(DefaultTools(), AllowAll{}), MaxTurns: 5}
	out, err := agent.Run("say hi via bash")
	if err != nil {
		t.Fatal(err)
	}
	if out != "done — it printed hi" {
		t.Fatalf("got %q", out)
	}
}

// --- forgiving parser: real models drift off the exact ```sprig-tool fence ---

func TestParseResponseAcceptsAJSONFence(t *testing.T) {
	// Models very often reach for a generic ```json block instead of our tag.
	msg := parseResponse("Sure.\n```json\n{\"tool\":\"read\",\"input\":{\"path\":\"x.go\"}}\n```")
	if len(msg.Calls) != 1 || msg.Calls[0].Name != "read" {
		t.Fatalf("a ```json fenced call must be honored, got %+v", msg)
	}
}

func TestParseResponseAcceptsAToolFence(t *testing.T) {
	msg := parseResponse("```tool\n{\"tool\":\"bash\",\"input\":{\"cmd\":\"ls\"}}\n```")
	if len(msg.Calls) != 1 || msg.Calls[0].Name != "bash" {
		t.Fatalf("a ```tool fenced call must be honored, got %+v", msg)
	}
}

func TestParseResponseAcceptsBareJSONWithKnownTool(t *testing.T) {
	// No fence at all — the whole reply is the call object. Common with terse models.
	msg := parseResponse("{\"tool\":\"edit\",\"input\":{\"path\":\"a\",\"old\":\"x\",\"new\":\"y\"}}")
	if len(msg.Calls) != 1 || msg.Calls[0].Name != "edit" {
		t.Fatalf("a bare known-tool JSON reply must be honored, got %+v", msg)
	}
}

func TestParseResponseBareJSONUnknownToolIsFinalText(t *testing.T) {
	// Guard against the forgiving path over-triggering: a bare object naming a
	// tool sprig does NOT have is NOT a call — it stays a final answer.
	msg := parseResponse("{\"tool\":\"deploy\",\"input\":{}}")
	if len(msg.Calls) != 0 {
		t.Fatalf("an unknown-tool object must not become a phantom call, got %+v", msg.Calls)
	}
}

func TestFormatPromptIncludesAWorkedExample(t *testing.T) {
	// The live failure was the model answering in prose instead of calling a tool.
	// A concrete worked example is the forcing prompt that fixed it — make it permanent.
	p := formatPrompt([]Message{{Role: "user", Text: "count the TODOs"}})
	if !strings.Contains(p, "Example") {
		t.Fatal("prompt must carry a worked example of a call and a finish")
	}
}

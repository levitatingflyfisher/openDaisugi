package sprig

import (
	"encoding/json"
	"strings"
	"testing"
)

// claudeEnvelope builds a fixture matching what a real `claude -p
// --output-format json` run prints on stdout: type "result", the turn's
// text in result, and Claude Code's own usage accounting. Confirmed live
// against Claude Code v2.1.283: the non-streaming json envelope carries a
// top-level "type" field, the same field name the "result" line of
// --output-format stream-json carries.
func claudeEnvelope(t *testing.T, result string, isError bool, usage map[string]int) string {
	t.Helper()
	env := map[string]any{
		"type": "result", "subtype": "success", "is_error": isError,
		"result": result, "session_id": "fake-session",
	}
	if usage != nil {
		u := map[string]any{}
		for k, v := range usage {
			u[k] = v
		}
		env["usage"] = u
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

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
	// Prove Next() drives the whole path (format, run, parse, unwrap the
	// json envelope) with a FAKE runner, so no live `claude -p` call (and no
	// quota) is needed to test it.
	raw := claudeEnvelope(t, "```sprig-tool\n{\"tool\":\"read\",\"input\":{\"path\":\"x.go\"}}\n```", false, nil)
	m := &ClaudeCodeModel{run: func(string) (string, error) { return raw, nil }}
	msg, err := m.Next([]Message{{Role: "user", Text: "read x.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.Calls) != 1 || msg.Calls[0].Name != "read" {
		t.Fatalf("Next did not parse the tool call: %+v", msg)
	}
}

func TestClaudeCodeModelNextParsesUsageFromTheCLIEnvelope(t *testing.T) {
	// This backend used to leave Usage at zero because `claude -p`'s
	// plain-text default carries no token counts. sprig now asks for
	// --output-format json (see claudeArgs), so Usage must come from the
	// envelope's own usage object. Each bucket gets a distinct number, so a
	// swap of cache_read and cache_creation would fail this.
	raw := claudeEnvelope(t, "final answer", false, map[string]int{
		"input_tokens": 11, "cache_read_input_tokens": 22,
		"cache_creation_input_tokens": 33, "output_tokens": 44,
	})
	m := &ClaudeCodeModel{Model: "haiku", run: func(string) (string, error) { return raw, nil }}
	msg, err := m.Next([]Message{{Role: "user", Text: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Model != "haiku" {
		t.Fatalf("want the configured model name, got %q", msg.Model)
	}
	want := Usage{Fresh: 11, CacheRead: 22, CacheWrite: 33, Out: 44}
	if msg.Usage != want {
		t.Fatalf("usage %+v, want %+v", msg.Usage, want)
	}
	if msg.Text != "final answer" {
		t.Fatalf("got text %q", msg.Text)
	}
}

func TestClaudeCodeModelNextFailsOnIsError(t *testing.T) {
	// is_error can be true on an exit-0 reply (max turns, a refused turn in
	// the CLI itself). That must surface as an error, never as a final
	// answer parseResponse would happily accept.
	raw := claudeEnvelope(t, "hit max turns", true, nil)
	m := &ClaudeCodeModel{run: func(string) (string, error) { return raw, nil }}
	if _, err := m.Next([]Message{{Role: "user", Text: "x"}}); err == nil {
		t.Fatal("want an error when the CLI envelope reports is_error")
	}
}

func TestClaudeCodeModelNextFailsOnUnreadableOutput(t *testing.T) {
	// A stripping proxy, an old CLI still on plain-text, or a broken binary
	// could all hand Next() something that is not the json envelope it asked
	// for. That must fail loudly, not silently degrade to a half-parsed
	// answer with lost usage.
	m := &ClaudeCodeModel{run: func(string) (string, error) { return "not json at all", nil }}
	if _, err := m.Next([]Message{{Role: "user", Text: "x"}}); err == nil {
		t.Fatal("want an error on output that is not the json envelope")
	}
}

func TestClaudeCodeModelNextFailsOnValidJSONWithNoTypeField(t *testing.T) {
	// Valid JSON that is still not the envelope: no top-level "type". This is
	// exactly the shape src/opendaisugi/claude_code_llm.py's own test
	// fixtures use, since that Python reader never checks "type" (it only
	// reads is_error/result/usage). sprig's own check is stricter, on
	// purpose: without it, this line would decode to empty text and zero
	// usage with no error at all.
	raw := `{"result":"x","is_error":false,"usage":{"input_tokens":1}}`
	m := &ClaudeCodeModel{run: func(string) (string, error) { return raw, nil }}
	if _, err := m.Next([]Message{{Role: "user", Text: "x"}}); err == nil {
		t.Fatal("want an error on json with no top-level type field")
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
			return claudeEnvelope(t, "```sprig-tool\n{\"tool\":\"bash\",\"input\":{\"cmd\":\"echo hi\"}}\n```", false, nil), nil
		}
		return claudeEnvelope(t, "done — it printed hi", false, nil), nil
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

func TestClaudeArgsRequestsJSONOutputAndBindsModelSafely(t *testing.T) {
	args := claudeArgs("claude", "haiku")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--output-format json") {
		t.Fatalf("want --output-format json in argv, got %v", args)
	}
	if !strings.Contains(joined, "--model=haiku") {
		t.Fatalf("want --model=haiku (bound form, never a separate token), got %v", args)
	}
	for _, a := range args {
		if a == "haiku" {
			t.Fatalf("model must never appear as a bare argv element: %v", args)
		}
	}
}

func TestAgentReportsRealUsageIntoTheSessionTree(t *testing.T) {
	// End to end: a claude-code backend with a fake run, wired to a REAL
	// SessionWriter, must land non-zero usage in the actual tree file, not
	// just in the in-memory Message, under the exact camelCase keys
	// coppice's sprigAssistant reads.
	raw := claudeEnvelope(t, "done", false, map[string]int{
		"input_tokens": 11, "cache_read_input_tokens": 22,
		"cache_creation_input_tokens": 33, "output_tokens": 44,
	})
	m := &ClaudeCodeModel{Model: "haiku", run: func(string) (string, error) { return raw, nil }}
	dir := t.TempDir()
	w, err := NewSessionWriter(dir, "s1", "/w")
	if err != nil {
		t.Fatal(err)
	}
	agent := &Agent{Model: m, Exec: NewExecutor(DefaultTools(), AllowAll{}), MaxTurns: 5, SessionObserver: w}
	if _, err := agent.Run("say hi"); err != nil {
		t.Fatal(err)
	}

	entries, err := ReadEntries(w.Path())
	if err != nil {
		t.Fatal(err)
	}
	var usage map[string]any
	for _, e := range entries {
		if e.Type == "assistant" {
			usage, _ = e.Data["usage"].(map[string]any)
		}
	}
	if usage == nil {
		t.Fatal("no assistant entry with usage in the tree")
	}
	want := map[string]float64{"fresh": 11, "cacheRead": 22, "cacheWrite": 33, "out": 44}
	for k, v := range want {
		if usage[k] != v {
			t.Fatalf("usage[%q] = %v, want %v (full usage: %v)", k, usage[k], v, usage)
		}
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

package sprig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func splitLines(raw []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, b := range raw {
		if b == '\n' {
			if i > start {
				out = append(out, raw[start:i])
			}
			start = i + 1
		}
	}
	if start < len(raw) {
		out = append(out, raw[start:])
	}
	return out
}

func TestSessionWriterWritesTheSpecShape(t *testing.T) {
	dir := t.TempDir()
	w, err := NewSessionWriter(dir, "e1", "/w")
	if err != nil {
		t.Fatal(err)
	}
	w.OnPrompt("list files")
	w.OnAssistant("claude-sonnet-4", "Sure.", Usage{Fresh: 12, CacheRead: 1000, CacheWrite: 50, Out: 30},
		[]ToolUse{{ID: "toolu_1", Name: "bash"}})
	w.OnToolCall("toolu_1", "bash", map[string]any{"command": "ls"})
	w.OnVerdict("toolu_1", false, "permissions: no pipes", 400*time.Microsecond)
	w.OnToolResult("toolu_1", false, "REFUSED by the gate")

	raw, _ := os.ReadFile(filepath.Join(dir, "e1.jsonl"))
	var rows []map[string]any
	for _, line := range splitLines(raw) {
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("bad line %q: %v", line, err)
		}
		rows = append(rows, m)
	}
	if rows[0]["type"] != "session" || rows[0]["harness"] != "sprig" || rows[0]["v"] != float64(1) {
		t.Fatalf("bad header: %v", rows[0])
	}
	want := []string{"session", "prompt", "assistant", "tool_call", "verdict", "tool_result"}
	for i, ty := range want {
		if rows[i]["type"] != ty {
			t.Fatalf("row %d: want %s got %v", i, ty, rows[i]["type"])
		}
	}
	if rows[1]["parentId"] != nil {
		t.Fatalf("prompt must be a root: %v", rows[1])
	}
	for i := 2; i < len(rows); i++ {
		if rows[i]["parentId"] != rows[i-1]["id"] {
			t.Fatalf("row %d not linked to previous: %v", i, rows[i])
		}
		if len(rows[i]["id"].(string)) != 8 {
			t.Fatalf("id must be 8 hex: %v", rows[i]["id"])
		}
	}
	usage := rows[2]["usage"].(map[string]any)
	if usage["cacheRead"] != float64(1000) {
		t.Fatalf("usage not written: %v", usage)
	}
	toolUses := rows[2]["toolUses"].([]any)
	if len(toolUses) != 1 || toolUses[0].(map[string]any)["id"] != "toolu_1" {
		t.Fatalf("toolUses not written: %v", rows[2]["toolUses"])
	}
	if rows[4]["decision"] != "deny" || rows[4]["toolUseId"] != "toolu_1" {
		t.Fatalf("verdict: %v", rows[4])
	}
	if rows[5]["ok"] != false || rows[5]["summary"] != "REFUSED by the gate" {
		t.Fatalf("tool_result: %v", rows[5])
	}
}

func TestOnToolResultTruncatesOnARuneBoundaryNotAByteBoundary(t *testing.T) {
	// A byte slice at exactly the multi-byte-rune boundary would cut a UTF-8
	// sequence in half, and json.Marshal would silently turn the result into
	// invalid UTF-8 (replaced with U+FFFD) rather than erroring — routine
	// with tool output (file contents, error text) that isn't pure ASCII.
	dir := t.TempDir()
	w, _ := NewSessionWriter(dir, "e5", "/w")
	// 399 ASCII bytes then a 3-byte rune (€) straddling the 400-byte cutoff.
	summary := strings.Repeat("a", 399) + "€€€"
	w.OnToolResult("t1", true, summary)

	entries, err := ReadEntries(w.Path())
	if err != nil {
		t.Fatal(err)
	}
	got, _ := entries[len(entries)-1].Data["summary"].(string)
	if !utf8.ValidString(got) {
		t.Fatalf("truncated summary is not valid UTF-8: %q", got)
	}
	if utf8.RuneCountInString(got) != toolResultSummaryLimit {
		t.Fatalf("want %d runes, got %d: %q", toolResultSummaryLimit, utf8.RuneCountInString(got), got)
	}
}

func TestOnVerdictAllowWritesAnAllowDecision(t *testing.T) {
	dir := t.TempDir()
	w, _ := NewSessionWriter(dir, "e0", "/w")
	w.OnPrompt("x")
	w.OnToolCall("t1", "bash", map[string]any{"command": "ls"})
	w.OnVerdict("t1", true, "", 10*time.Microsecond)
	entries, err := ReadEntries(w.Path())
	if err != nil {
		t.Fatal(err)
	}
	last := entries[len(entries)-1]
	if last.Type != "verdict" || last.Data["decision"] != "allow" {
		t.Fatalf("want an allow verdict, got %+v", last.Data)
	}
}

func TestHeadPathSkipsTornLineAndFollowsHead(t *testing.T) {
	dir := t.TempDir()
	w, _ := NewSessionWriter(dir, "e2", "/w")
	w.OnPrompt("one")
	w.OnAssistant("m", "a", Usage{}, nil)
	f, _ := os.OpenFile(w.Path(), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"type":"assistant","id":"abcd`)
	f.Close()
	path, err := HeadPath(w.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(path) != 2 || path[0].Type != "prompt" || path[1].Type != "assistant" {
		t.Fatalf("head path: %+v", path)
	}
}

func TestOpenSessionWriterAppendsToTheSameTree(t *testing.T) {
	dir := t.TempDir()
	w, _ := NewSessionWriter(dir, "e3", "/w")
	w.OnPrompt("one")
	again, err := OpenSessionWriter(dir, "e3")
	if err != nil {
		t.Fatal(err)
	}
	again.OnPrompt("two")
	entries, _ := ReadEntries(again.Path())
	if entries[2].ParentID != entries[1].ID {
		t.Fatalf("second prompt must hang off the head: %+v", entries)
	}
}

func TestNewSessionWriterRefusesAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewSessionWriter(dir, "e4", "/w"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSessionWriter(dir, "e4", "/w"); err == nil {
		t.Fatal("want an error creating over an existing session file")
	}
}

func TestOpenSessionWriterMissingFileErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := OpenSessionWriter(dir, "nope"); err == nil {
		t.Fatal("want an error opening a session that was never created")
	}
}

func TestNoopSessionObserverImplementsTheInterface(t *testing.T) {
	var obs SessionObserver = NoopSessionObserver{}
	obs.OnPrompt("x")
	obs.OnAssistant("m", "t", Usage{}, nil)
	obs.OnToolCall("id", "n", nil)
	obs.OnVerdict("id", true, "", 0)
	obs.OnToolResult("id", true, "s")
}

// The existing fleet-supervision Observer (OnState) must still exist unchanged
// and distinct from SessionObserver — this is Task 10's compile blocker fix,
// pinned as a test so a future redeclaration collides here first.
func TestObserverAndSessionObserverAreDistinctInterfaces(t *testing.T) {
	var _ Observer = (*recordObs)(nil)
	var _ SessionObserver = (*SessionWriter)(nil)
	var _ SessionObserver = NoopSessionObserver{}
}

// --- HistoryFromEntries: --resume rebuilds the model-facing history --------

func TestHistoryFromEntriesRebuildsPromptAssistantAndToolResult(t *testing.T) {
	dir := t.TempDir()
	w, _ := NewSessionWriter(dir, "r1", "/w")
	w.OnPrompt("read x.go")
	w.OnAssistant("haiku", "", Usage{}, []ToolUse{{ID: "t1", Name: "read"}})
	w.OnToolCall("t1", "read", map[string]any{"path": "x.go"})
	w.OnVerdict("t1", true, "", time.Microsecond)
	w.OnToolResult("t1", true, "file contents")
	w.OnAssistant("haiku", "it has three functions", Usage{}, nil)

	entries, err := HeadPath(w.Path())
	if err != nil {
		t.Fatal(err)
	}
	history := HistoryFromEntries(entries)

	want := []struct {
		role, text string
	}{
		{"user", "read x.go"},
		{"assistant", ""},
		{"tool", "file contents"},
		{"assistant", "it has three functions"},
	}
	if len(history) != len(want) {
		t.Fatalf("want %d turns, got %d: %+v", len(want), len(history), history)
	}
	for i, w := range want {
		if history[i].Role != w.role || history[i].Text != w.text {
			t.Fatalf("turn %d: want {%s %q}, got %+v", i, w.role, w.text, history[i])
		}
	}
	// The tool_use's Input must be recovered from the paired tool_call entry
	// — losslessly, with no separate "raw" fallback needed.
	if len(history[1].Calls) != 1 || history[1].Calls[0].ID != "t1" ||
		history[1].Calls[0].Name != "read" || history[1].Calls[0].Input["path"] != "x.go" {
		t.Fatalf("assistant Calls not rebuilt losslessly: %+v", history[1].Calls)
	}
}

func TestHistoryFromEntriesOnAnEmptyPathIsEmpty(t *testing.T) {
	if got := HistoryFromEntries(nil); len(got) != 0 {
		t.Fatalf("want no turns, got %+v", got)
	}
}

// The session tree on path E: sprig owns the loop, so it writes the same
// JSONL shape daisugi's gate writes on path D (docs/plans/2026-08-27-cockpit-spec.md
// §6.1) — one append-only file per session, id/parentId on every entry, the
// head persisted as the last write wins.
//
// This interface is named SessionObserver, not Observer: loop.go already
// declares `type Observer interface { OnState(state string) }` for fleet
// state-fusion (grove's cockpit). A second, unrelated five-method interface
// reusing that name would not just be confusing — it would not compile.
// SessionObserver and Observer are deliberately distinct types on distinct
// fields (Agent.Observer vs Agent.SessionObserver / Executor.SessionObserver).
package sprig

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Usage mirrors daisugi's four token-usage buckets (fresh input, cache read,
// cache write, output) so a session's cost is comparable across path D and E.
type Usage struct{ Fresh, CacheRead, CacheWrite, Out int }

// ToolUse names one tool call an assistant turn requested, echoed onto the
// "assistant" entry so a reader can see intent without joining to tool_call rows.
type ToolUse struct{ ID, Name string }

// SessionObserver receives every event the session tree records, in order.
// Implementations must be cheap and must not block the loop for long — a
// SessionWriter's own writes are the only I/O it does, and a write failure
// there is swallowed (best-effort: a session-tree failure must never abort
// the agent's actual work, the same contract daisugi's gate holds for
// _log_tree on path D).
type SessionObserver interface {
	OnPrompt(text string)
	OnAssistant(model, text string, usage Usage, toolUses []ToolUse)
	OnToolCall(id, name string, input map[string]any)
	OnVerdict(id string, allow bool, reason string, latency time.Duration)
	OnToolResult(id string, ok bool, summary string)
}

// NoopSessionObserver is the default when --session-dir is not set: every
// method is a no-op, so the loop and executor can always call through their
// SessionObserver field without a nil check at every call site.
type NoopSessionObserver struct{}

func (NoopSessionObserver) OnPrompt(string)                               {}
func (NoopSessionObserver) OnAssistant(string, string, Usage, []ToolUse)  {}
func (NoopSessionObserver) OnToolCall(string, string, map[string]any)     {}
func (NoopSessionObserver) OnVerdict(string, bool, string, time.Duration) {}
func (NoopSessionObserver) OnToolResult(string, bool, string)             {}

// Entry is one parsed row: the fields every entry carries (type, id,
// parentId, ts), plus Data holding the row's own fields verbatim — the same
// "meta keys are structural, everything else is a payload" split
// session_tree.py's Entry.from_row uses. ParentID is "" for a root entry
// (never a valid id, which is always 8 hex chars) — a plain string rather
// than a pointer, so callers compare ids without a dereference.
type Entry struct {
	Type     string
	ID       string
	ParentID string
	TS       float64
	Data     map[string]any
}

// SessionWriter appends to one session's JSONL file and tracks the current
// head in memory, so a long-lived writer never re-reads its own file (the
// same head-caching rationale as session_tree.py's SessionTree).
type SessionWriter struct {
	path string
	head string // "" means root (no entry yet)
}

func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b) // crypto/rand.Read never errors on Linux/darwin/windows
	return hex.EncodeToString(b)
}

func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// NewSessionWriter creates a fresh session file and writes its header.
// Refuses to overwrite an existing file — a session id collision must be
// visible, not silently merged into someone else's tree.
func NewSessionWriter(dir, sessionID, cwd string) (*SessionWriter, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	w := &SessionWriter{path: filepath.Join(dir, sessionID+".jsonl")}
	if _, err := os.Stat(w.path); err == nil {
		return nil, errors.New("session file exists: " + w.path)
	}
	header := map[string]any{
		"type": "session", "v": 1, "id": sessionID, "harness": "sprig", "cwd": cwd,
		"harnessSessionId": nil, "transcriptPath": nil, "parentSession": nil,
		"parentEntry": nil, "cacheKey": nil, "ts": now(),
	}
	if err := w.writeRaw(header); err != nil {
		return nil, err
	}
	_ = os.Chmod(w.path, 0o600)
	return w, nil
}

// OpenSessionWriter reopens an existing session file, restoring the head by
// scanning it once (a cold instance, same as session_tree.py's SessionTree.open).
func OpenSessionWriter(dir, sessionID string) (*SessionWriter, error) {
	w := &SessionWriter{path: filepath.Join(dir, sessionID+".jsonl")}
	if _, err := os.Stat(w.path); err != nil {
		return nil, err
	}
	entries, err := ReadEntries(w.path)
	if err != nil {
		return nil, err
	}
	w.head = headOf(entries)
	return w, nil
}

func (w *SessionWriter) Path() string { return w.path }
func (w *SessionWriter) Head() string { return w.head }

func (w *SessionWriter) writeRaw(row map[string]any) error {
	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(row)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// append writes one entry hanging off the current head and, on success,
// advances the head to it. A write failure leaves the head where it was and
// is otherwise swallowed — SessionObserver methods return nothing to fail.
func (w *SessionWriter) append(typ string, data map[string]any) {
	row := map[string]any{"type": typ, "id": newID(), "ts": now()}
	if w.head == "" {
		row["parentId"] = nil
	} else {
		row["parentId"] = w.head
	}
	for k, v := range data {
		row[k] = v
	}
	if err := w.writeRaw(row); err != nil {
		return // best-effort: a write failure never stops the loop
	}
	w.head = row["id"].(string)
}

func (w *SessionWriter) OnPrompt(text string) { w.append("prompt", map[string]any{"text": text}) }

func (w *SessionWriter) OnAssistant(model, text string, u Usage, uses []ToolUse) {
	tu := make([]map[string]any, 0, len(uses))
	for _, x := range uses {
		tu = append(tu, map[string]any{"id": x.ID, "name": x.Name})
	}
	w.append("assistant", map[string]any{
		"model": model, "text": text,
		"usage": map[string]any{
			"fresh": u.Fresh, "cacheRead": u.CacheRead, "cacheWrite": u.CacheWrite, "out": u.Out,
		},
		"toolUses": tu,
	})
}

func (w *SessionWriter) OnToolCall(id, name string, input map[string]any) {
	w.append("tool_call", map[string]any{
		"toolUseId": id, "name": name, "input": input,
		"detail": detailOf(name, input), "agentId": nil, "agentType": nil,
	})
}

func (w *SessionWriter) OnVerdict(id string, allow bool, reason string, latency time.Duration) {
	decision := "deny"
	if allow {
		decision = "allow"
	}
	w.append("verdict", map[string]any{
		"toolUseId": id, "decision": decision, "mode": "enforce",
		"reason": reason, "clause": reason, "counterexample": map[string]any{},
		"envelopeId": nil, "planId": nil, "latencyMs": float64(latency.Microseconds()) / 1000.0,
		"answeredBy": nil,
	})
}

const toolResultSummaryLimit = 400

func (w *SessionWriter) OnToolResult(id string, ok bool, summary string) {
	w.append("tool_result", map[string]any{
		"toolUseId": id, "ok": ok, "summary": truncateRunes(summary, toolResultSummaryLimit),
	})
}

// truncateRunes caps a string at n RUNES, not n bytes. A byte slice
// (s[:n]) can land inside a multi-byte UTF-8 sequence — tool output with
// non-ASCII (file contents, error text) is routine — leaving invalid UTF-8
// that json.Marshal silently replaces with U+FFFD rather than erroring, so
// the corruption would otherwise pass unnoticed into the session tree.
func truncateRunes(s string, n int) string {
	if len(s) <= n { // fast path: ASCII or already short — len(s) counts bytes,
		return s // so this can only UNDER-truncate here, never over
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// detailOf picks the one input field worth showing at a glance — the same
// "command, then path" preference the gate's own detail extraction uses.
func detailOf(name string, input map[string]any) string {
	for _, k := range []string{"command", "cmd", "path", "file_path", "url"} {
		if v, ok := input[k].(string); ok {
			return v
		}
	}
	return name
}

// ReadEntries parses the file, skipping blank and torn lines — the same
// tolerance session_tree.py's entries() has for a writer caught mid-append.
func ReadEntries(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal(line, &raw); err != nil {
			continue // a torn line from a writer mid-append
		}
		ty, _ := raw["type"].(string)
		if ty == "" {
			continue
		}
		id, _ := raw["id"].(string)
		e := Entry{Type: ty, ID: id, TS: num(raw["ts"]), Data: raw}
		if p, ok := raw["parentId"].(string); ok {
			e.ParentID = p
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// headOf finds the current leaf: "last write wins" among a head entry's
// leafId and a non-label/session entry's own id — same rule as
// session_tree.py's SessionTree.head().
func headOf(entries []Entry) string {
	head := ""
	for _, e := range entries {
		switch e.Type {
		case "head":
			head, _ = e.Data["leafId"].(string)
		case "label", "session":
			// neither moves the head
		default:
			if e.ID != "" {
				head = e.ID
			}
		}
	}
	return head
}

// HeadPath returns root→head, the path --resume rebuilds the model-facing
// history from.
func HeadPath(path string) ([]Entry, error) {
	entries, err := ReadEntries(path)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]Entry, len(entries))
	for _, e := range entries {
		if e.ID != "" && e.Type != "session" {
			byID[e.ID] = e
		}
	}
	var out []Entry
	for cur := headOf(entries); cur != ""; {
		e, ok := byID[cur]
		if !ok {
			break
		}
		out = append([]Entry{e}, out...)
		cur = e.ParentID
	}
	return out, nil
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

// HistoryFromEntries rebuilds the model-facing history --resume needs from a
// session tree's head path (HeadPath): a "prompt" entry becomes a user turn,
// "assistant" becomes an assistant turn, "tool_result" becomes a tool turn.
// "tool_call"/"verdict"/anything else carries no turn of its own.
//
// An assistant entry's ToolUses record only {id, name} (OnAssistant never
// receives Input — see the doc on Message in loop.go); this joins each one
// against the following "tool_call" entry sharing its toolUseId, which DOES
// carry the input map, to rebuild ToolCall{ID, Name, Input} losslessly. That
// is why no separate "raw" escape hatch is needed here: prompt/assistant/
// tool_result plus this join reconstruct every field the model backends
// actually read (Role, Text, Calls) — see model_claude.go's formatPrompt and
// model_api.go's buildMessages, neither of which reads anything else off
// Message.
func HistoryFromEntries(entries []Entry) []Message {
	toolInput := make(map[string]map[string]any)
	for _, e := range entries {
		if e.Type != "tool_call" {
			continue
		}
		id, _ := e.Data["toolUseId"].(string)
		if id == "" {
			continue
		}
		input, _ := e.Data["input"].(map[string]any)
		toolInput[id] = input
	}
	var out []Message
	for _, e := range entries {
		switch e.Type {
		case "prompt":
			text, _ := e.Data["text"].(string)
			out = append(out, Message{Role: "user", Text: text})
		case "assistant":
			text, _ := e.Data["text"].(string)
			model, _ := e.Data["model"].(string)
			msg := Message{Role: "assistant", Text: text, Model: model}
			if raw, ok := e.Data["toolUses"].([]any); ok {
				for _, ru := range raw {
					m, ok := ru.(map[string]any)
					if !ok {
						continue
					}
					id, _ := m["id"].(string)
					name, _ := m["name"].(string)
					msg.Calls = append(msg.Calls, ToolCall{ID: id, Name: name, Input: toolInput[id]})
				}
			}
			out = append(out, msg)
		case "tool_result":
			summary, _ := e.Data["summary"].(string)
			out = append(out, Message{Role: "tool", Text: summary})
		}
	}
	return out
}

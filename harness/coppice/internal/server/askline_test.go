package server

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestAskLineOfReadsTheQuestionTheAgentEndedOn(t *testing.T) {
	a := func(text string) message { return message{Role: "agent", Text: text} }
	for name, c := range map[string]struct {
		msgs []message
		want string
	}{
		"a question":           {[]message{{Role: "owner", Text: "go"}, a("Done.\nShould I push this now?")}, "Should I push this now?"},
		"markup after it":      {[]message{a("Which one do you want, **A or B?**")}, "Which one do you want, **A or B?**"},
		"a statement":          {[]message{a("All done.")}, ""},
		"a question mid text":  {[]message{a("Why? Because it was slow.")}, ""},
		"the owner spoke last": {[]message{a("Shall I go on?"), {Role: "owner", Text: "yes"}}, ""},
		"a tool call last":     {[]message{a("Shall I go on?"), {Role: "agent", Tool: "Bash", Text: "ls"}}, ""},
		"a note last":          {[]message{a("Shall I go on?"), {Role: "note", Text: "auto: x"}}, "Shall I go on?"},
		"none":                 {nil, ""},
	} {
		if got := askLineOf(c.msgs); got != c.want {
			t.Errorf("%s: askLineOf = %q, want %q", name, got, c.want)
		}
	}
	long := strings.Repeat("word ", 100) + "?"
	if got := askLineOf([]message{a(long)}); len([]rune(got)) > askRunes+1 {
		t.Errorf("a long ask is %d runes, want at most %d", len([]rune(got)), askRunes)
	}
}

// An idle pane whose last message asks something carries it on its
// pane.list row, and a pane that is working does not.
func TestAnIdlePaneListsItsLastAsk(t *testing.T) {
	s := newFactsServer(t)
	path := chatFixture(t, s, "claude-chat.jsonl")
	line, _ := json.Marshal(map[string]any{
		"type": "assistant", "timestamp": "2099-01-01T00:00:00Z",
		"message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": "Done with the clock.\nWant me to push it?"}}},
	})
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	reportTranscript(t, s, "w1:p1", path)
	listRow := func() map[string]any {
		t.Helper()
		res := result(t, roundTrip(t, s, `{"id":"l","cmd":"pane.list"}`)[0])
		for _, p := range res["panes"].([]any) {
			if row := p.(map[string]any); row["id"] == "w1:p1" {
				return row
			}
		}
		t.Fatal("no row for w1:p1")
		return nil
	}
	setState(t, s, "w1:p1", "idle")
	if got := listRow()["last_ask"]; got != "Want me to push it?" {
		t.Fatalf("last_ask = %v", got)
	}
	setState(t, s, "w1:p1", "working")
	if got, ok := listRow()["last_ask"]; ok {
		t.Fatalf("a working pane lists an ask: %v", got)
	}
}

// A pane connection does not read another agent's last words from the list:
// last_ask is for the operator, as pane.messages is.
func TestAPaneConnectionGetsNoLastAsk(t *testing.T) {
	s := newFactsServer(t)
	path := chatFixture(t, s, "claude-chat.jsonl")
	line, _ := json.Marshal(map[string]any{
		"type": "assistant", "timestamp": "2099-01-01T00:00:00Z",
		"message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": "Want me to push it?"}}},
	})
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()
	reportTranscript(t, s, "w1:p1", path)
	setState(t, s, "w1:p1", "idle")
	send, next, stop := streamFacts(t, s, &peerFacts{checked: true, pane: true, paneID: "w1:p99"})
	defer stop()
	send(`{"id":"1","cmd":"pane.list"}`)
	res, _ := next()["result"].(map[string]any)
	for _, p := range res["panes"].([]any) {
		if _, has := p.(map[string]any)["last_ask"]; has {
			t.Fatalf("a pane connection read last_ask: %v", p)
		}
	}
}

package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/foreman"
)

// chatCall sends floor.chat with the extra fields and returns the result.
func chatCall(t *testing.T, s *Server, extra map[string]any) (msgs []map[string]any, res map[string]any) {
	t.Helper()
	req := map[string]any{"id": "1", "cmd": "floor.chat"}
	for k, v := range extra {
		req[k] = v
	}
	b, _ := json.Marshal(req)
	res = result(t, roundTrip(t, s, string(b))[0])
	raw, _ := json.Marshal(res["messages"])
	_ = json.Unmarshal(raw, &msgs)
	return msgs, res
}

func appendLog(t *testing.T, s *Server, kind, text string) {
	t.Helper()
	fc := s.ensureChat()
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if _, err := fc.chat.Append(ctx0, kind, text); err != nil {
		t.Fatal(err)
	}
}

func newLogServer(t *testing.T) (*Server, *fakeEnv, string) {
	t.Helper()
	e := newFakeEnv(t)
	s, logDir := chatServer(t, e, nil)
	s.StartForeman()
	if fc := s.ensureChat(); fc.chat == nil {
		t.Fatal(fc.problem)
	}
	return s, e, logDir
}

func TestFloorChatReadsTheLogAndMapsEachKind(t *testing.T) {
	s, _, _ := newLogServer(t)
	appendLog(t, s, foreman.KindUser, "hello")
	appendLog(t, s, foreman.KindForeman, "hi there")
	appendLog(t, s, foreman.KindTool, "zoom({\"id\":0,\"n\":1})\nTHE SECRET RESULT")
	appendLog(t, s, foreman.KindWork, "[trellis-1] report\nall done")
	appendLog(t, s, foreman.KindNote, "a note")
	appendLog(t, s, foreman.KindPast, "an old line")
	msgs, res := chatCall(t, s, nil)
	if res["source"] != "log" || res["more"] != false {
		t.Fatalf("result = %v", res)
	}
	type row struct{ id, role, text, tool string }
	var got []row
	for _, m := range msgs {
		got = append(got, row{m["id"].(string), m["role"].(string), fmt.Sprint(m["text"]), fmt.Sprint(m["tool"])})
	}
	want := []row{
		{"log/0", "owner", "hello", "<nil>"},
		{"log/1", "agent", "hi there", "<nil>"},
		{"log/2", "agent", "", "zoom"},
		{"log/3", "work", "[trellis-1] report\nall done", "<nil>"},
		{"log/4", "note", "a note", "<nil>"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("messages =\n%v\nwant\n%v", got, want)
	}
	if b, _ := json.Marshal(msgs); strings.Contains(string(b), "SECRET") {
		t.Fatal("a tool result reached floor.chat")
	}
	// past is shown on request.
	msgs, _ = chatCall(t, s, map[string]any{"past": true})
	if len(msgs) != 6 || msgs[5]["role"] != "past" || msgs[5]["text"] != "an old line" {
		t.Fatalf("with past: %v", msgs)
	}
}

func TestSinceAndLimitCountLogMessages(t *testing.T) {
	s, _, _ := newLogServer(t)
	for i := 0; i < 10; i++ {
		appendLog(t, s, foreman.KindUser, fmt.Sprintf("line %d", i))
	}
	msgs, res := chatCall(t, s, map[string]any{"limit": 3})
	if len(msgs) != 3 || msgs[0]["id"] != "log/7" || msgs[2]["id"] != "log/9" || res["more"] != true {
		t.Fatalf("limit: %v %v", msgs, res)
	}
	msgs, res = chatCall(t, s, map[string]any{"since": "log/6"})
	if len(msgs) != 3 || msgs[0]["id"] != "log/7" || res["more"] != false {
		t.Fatalf("since: %v %v", msgs, res)
	}
	msgs, res = chatCall(t, s, map[string]any{"since": "log/9"})
	if len(msgs) != 0 || res["more"] != false {
		t.Fatalf("since the last: %v %v", msgs, res)
	}
	msgs, res = chatCall(t, s, map[string]any{"since": "log/2", "limit": 2})
	if len(msgs) != 2 || msgs[0]["id"] != "log/8" || res["more"] != true {
		t.Fatalf("since and limit: %v %v", msgs, res)
	}
}

func TestAPastLineDoesNotCostALimitSlot(t *testing.T) {
	s, _, _ := newLogServer(t)
	appendLog(t, s, foreman.KindUser, "keep")
	for i := 0; i < 5; i++ {
		appendLog(t, s, foreman.KindPast, "old")
	}
	msgs, _ := chatCall(t, s, map[string]any{"limit": 2})
	if len(msgs) != 1 || msgs[0]["text"] != "keep" {
		t.Fatalf("messages = %v", msgs)
	}
}

func TestAnOwnerLineIsReadOnceItsTurnStarted(t *testing.T) {
	s, _, _ := newLogServer(t)
	fc := s.ensureChat()
	appendLog(t, s, foreman.KindUser, "first")
	appendLog(t, s, foreman.KindForeman, "reply")
	appendLog(t, s, foreman.KindUser, "second")
	read := func() []any {
		msgs, _ := chatCall(t, s, nil)
		var out []any
		for _, m := range msgs {
			if m["role"] == "owner" {
				out = append(out, m["read"])
			}
		}
		return out
	}
	if got := fmt.Sprint(read()); got != "[false false]" {
		t.Fatalf("before any turn: %s", got)
	}
	if err := foreman.AppendTurn(fc.dir, foreman.TurnRecord{Msg: 0, Started: "2026-10-13T10:00:00Z", Status: foreman.TurnRunning}); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(read()); got != "[true false]" {
		t.Fatalf("turn 0 started: %s", got)
	}
	if err := foreman.AppendTurn(fc.dir, foreman.TurnRecord{Msg: 2, Started: "2026-10-13T10:01:00Z", Status: foreman.TurnRunning}); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(read()); got != "[true true]" {
		t.Fatalf("turn 2 started: %s", got)
	}
}

func TestALineShowsAtOnceThoughItsNodeIsNotBuilt(t *testing.T) {
	s, _, _ := newLogServer(t)
	t.Setenv("FAKE_MODE", "slow")
	long := strings.Repeat("word ", 300)
	appendLog(t, s, foreman.KindUser, long)
	msgs, _ := chatCall(t, s, nil)
	if len(msgs) != 1 || msgs[0]["id"] != "log/0" {
		t.Fatalf("messages = %v", msgs)
	}
	if pendingOf(s.ensureChat()) == 0 {
		t.Fatal("the node was built; the test proves nothing")
	}
}

func TestChatFilesFromBeforeTheLogAreMergedWithoutTranscripts(t *testing.T) {
	s, _, _ := newLogServer(t)
	dir := s.chatDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := `{"kind":"user","date":"2020-01-02T10:00:00.000Z","pane":"p1","text":"an old question"}` + "\n" +
		`{"kind":"note","date":"2020-01-02T10:00:05.000Z","pane":"p1","text":"A new foreman started."}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "2020-01-02.jsonl"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	// A line dated after the log began is not merged.
	later := `{"kind":"user","date":"2099-01-01T10:00:00.000Z","pane":"p1","text":"from the future"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "2099-01-01.jsonl"), []byte(later), 0o600); err != nil {
		t.Fatal(err)
	}
	appendLog(t, s, foreman.KindUser, "new question")
	msgs, res := chatCall(t, s, nil)
	if res["source"] != "log" || len(msgs) != 3 {
		t.Fatalf("messages = %v", msgs)
	}
	if msgs[0]["text"] != "an old question" || msgs[1]["role"] != "note" || msgs[2]["id"] != "log/0" {
		t.Fatalf("messages = %v", msgs)
	}
	if _, has := msgs[0]["read"]; has {
		t.Fatalf("an old line carries a read mark: %v", msgs[0])
	}
	// Reading since an old line works.
	after, _ := chatCall(t, s, map[string]any{"since": msgs[0]["id"]})
	if len(after) != 2 {
		t.Fatalf("after the first old line: %v", after)
	}
}

func TestADamagedLogIsMovedAsideAndFloorChatShowsTheNote(t *testing.T) {
	e := newFakeEnv(t)
	s, logDir := chatServer(t, e, nil)
	if err := os.MkdirAll(filepath.Join(logDir, "main"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "main", "2026-10-08.jsonl"), []byte(`{"i":0,"kind":"user","text":"old","size":3,"date":"2026-10-08T10:00:00Z"}`+"\n"+"garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.StartForeman()
	msgs, res := chatCall(t, s, nil)
	if res["source"] != "log" || len(msgs) != 1 || msgs[0]["role"] != "note" || !strings.Contains(fmt.Sprint(msgs[0]["text"]), "could not be read") {
		t.Fatalf("messages = %v", msgs)
	}
	if moved, _ := filepath.Glob(logDir + ".broken-*"); len(moved) != 1 {
		t.Fatalf("moved = %v", moved)
	}
}

func TestPaneModeFloorChatIsUnchanged(t *testing.T) {
	newFakeEnv(t)
	s := newTestServer(t)
	_, res := chatCall(t, s, nil)
	if res["source"] != "transcript" {
		t.Fatalf("result = %v", res)
	}
}

func TestATalkShowsInFloorChatAndIsReadWhenItsTurnStarts(t *testing.T) {
	ts := newTurnServer(t, nil)
	t.Setenv("FAKE_TURN_REPLY", "the reply")
	ts.say(t, "owner words")
	waitUntil(t, "the turn", 90*time.Second, ts.turnsDone(1))
	msgs, _ := chatCall(t, ts.Server, nil)
	if len(msgs) != 2 || msgs[0]["role"] != "owner" || msgs[0]["read"] != true || msgs[1]["text"] != "the reply" {
		t.Fatalf("messages = %v", msgs)
	}
}

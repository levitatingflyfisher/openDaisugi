package server

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/layout"
	_ "modernc.org/sqlite"
)

// hermesDB makes the state.db of Hermes profile steward under a fake HOME
// and returns the HOME and a function that runs SQL on the file.
func hermesDB(t *testing.T) (string, func(q string, args ...any)) {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".hermes", "profiles", "steward")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	run := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	run(`PRAGMA journal_mode=WAL`)
	run(`CREATE TABLE sessions (id TEXT PRIMARY KEY, source TEXT NOT NULL, parent_session_id TEXT, started_at REAL NOT NULL, archived INTEGER NOT NULL DEFAULT 0)`)
	run(`CREATE TABLE messages (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, role TEXT NOT NULL, content TEXT, tool_calls TEXT, tool_name TEXT, timestamp REAL NOT NULL, _compressed_summary INTEGER NOT NULL DEFAULT 0, active INTEGER NOT NULL DEFAULT 1)`)
	run(`INSERT INTO sessions (id, source, started_at) VALUES ('sess1','cli',1)`)
	return home, run
}

func hermesPane(t *testing.T, s *Server, home string) string {
	t.Helper()
	s.getenv = func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	id := newClaudePane(t, s)
	if err := s.updatePane(id, func(p *layout.Pane) {
		p.Harness = "hermes-steward"
		p.Argv = []string{"/home/user/.local/bin/hermes", "-p", "steward", "-c"}
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAHermesPaneGivesItsSessionAsMessages(t *testing.T) {
	home, run := hermesDB(t)
	run(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('sess1','user','open the garden gate',100)`)
	run(`INSERT INTO messages (session_id, role, content, tool_calls, timestamp) VALUES ('sess1','assistant','Looking.',
		'[{"id":"c1","type":"function","function":{"name":"terminal","arguments":"{\"command\":\"ls -la\"}"}}]',101)`)
	run(`INSERT INTO messages (session_id, role, content, tool_name, timestamp) VALUES ('sess1','tool','FAKE-total 0','terminal',102)`)
	run(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('sess1','system','FAKE-system prompt',103)`)
	run(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('sess1','assistant','Done.',104)`)
	s := newTestServer(t)
	id := hermesPane(t, s, home)
	res := result(t, messagesOf(t, s, `"pane":"`+id+`"`))
	checkMessages(t, res, []wantMsg{
		{"1", "owner", "open the garden gate", ""},
		{"2", "agent", "Looking.", ""},
		{"2.1", "agent", "ls -la", "terminal"},
		{"5", "agent", "Done.", ""},
	})
	// A reply that streams in is a later row; the earlier ids stay.
	run(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('sess1','user','thanks',105)`)
	res = result(t, messagesOf(t, s, `"pane":"`+id+`","since":"5"`))
	checkMessages(t, res, []wantMsg{{"6", "owner", "thanks", ""}})
}

func TestAHermesPaneWithNoDatabaseHasNoTranscript(t *testing.T) {
	s := newTestServer(t)
	id := hermesPane(t, s, t.TempDir())
	r := messagesOf(t, s, `"pane":"`+id+`"`)
	if r.OK {
		t.Fatalf("want a refusal, got %+v", r)
	}
}

func TestTheHermesDatabaseGrowingMovesTheWatchMark(t *testing.T) {
	home, run := hermesDB(t)
	run(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('sess1','user','hi',1)`)
	s := newTestServer(t)
	id := hermesPane(t, s, home)
	marks := map[string]fileMark{}
	s.lookMessages(marks, false)
	before := marks[id]
	if before.path == "" {
		t.Fatal("the watch found no file for the Hermes pane")
	}
	run(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('sess1','assistant','a reply that is long enough to grow the log file a good deal more than a few bytes',2)`)
	s.lookMessages(marks, false)
	if marks[id] == before {
		t.Fatalf("mark did not move: %+v", before)
	}
}

func TestTheChatReadsAHermesStewardsReplies(t *testing.T) {
	home, run := hermesDB(t)
	run(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('sess1','user','hello','1')`)
	run(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('sess1','assistant','Hello, owner.','2')`)
	s := newTestServer(t)
	id := hermesPane(t, s, home)
	src := s.chatSources(id)
	if len(src) != 1 || src[0].kind != kindHermes {
		t.Fatalf("sources = %+v", src)
	}
	msgs, _, err := readMessages(src[0].path, src[0].kind)
	if err != nil || len(msgs) != 2 || msgs[1].Text != "Hello, owner." || msgs[1].Role != "agent" {
		t.Fatalf("%v %+v", err, msgs)
	}
}

// newHermesPane makes one more Hermes pane of the steward profile. argv is
// its command line; started is its start time.
func newHermesPane(t *testing.T, s *Server, argv []string, started int64) string {
	t.Helper()
	id := newClaudePane(t, s)
	if err := s.updatePane(id, func(p *layout.Pane) {
		p.Harness = "hermes-steward"
		p.Argv = argv
		p.StartedAt = started
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func chatTextsOf(t *testing.T, s *Server, id string) []string {
	t.Helper()
	var out []string
	for _, src := range s.chatSources(id) {
		msgs, _, err := readMessages(src.path, src.kind)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range msgs {
			if m.Role == "agent" {
				out = append(out, m.Text)
			}
		}
	}
	return out
}

// A closed pane read the newest session of its profile at read time, so
// every closed steward pane repeated the live pane's reply. A pane now binds
// to the session it showed while it ran.
func TestAClosedHermesPaneKeepsItsOwnSession(t *testing.T) {
	home, run := hermesDB(t)
	run(`DELETE FROM sessions`)
	s := hermesServer(t, home)
	argv := []string{"/bin/hermes", "-p", "steward"}
	a := newHermesPane(t, s, argv, 99)
	promptAt(s, a, "first question", 100)
	hermesTurn(run, "s1", "first question", "first reply", 100.2)
	if got := chatTextsOf(t, s, a); len(got) != 1 || got[0] != "first reply" {
		t.Fatalf("pane A while live: %v", got)
	}
	if err := s.closePane(a, nil); err != nil {
		t.Fatal(err)
	}
	// Pane B starts later and Hermes makes its session at B's prompt.
	b := newHermesPane(t, s, argv, 199)
	promptAt(s, b, "second question", 200)
	hermesTurn(run, "s2", "second question", "second reply", 200.2)
	if got := chatTextsOf(t, s, b); len(got) != 1 || got[0] != "second reply" {
		t.Fatalf("pane B: %v", got)
	}
	if got := chatTextsOf(t, s, a); len(got) != 1 || got[0] != "first reply" {
		t.Fatalf("closed pane A must keep its own session, got %v", got)
	}
}

// hermesServer is a test server whose HOME is home.
func hermesServer(t *testing.T, home string) *Server {
	t.Helper()
	s := newTestServer(t)
	s.getenv = func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	return s
}

// promptAt tells the server that text went into pane id at unix second at.
func promptAt(s *Server, id, text string, at float64) {
	s.noteInput(id, []byte(text+"\r"), time.UnixMilli(int64(at*1000)))
}

// hermesTurn is what current Hermes leaves for a first turn: the session row
// and the messages appear together, stamped with the moment of the turn.
func hermesTurn(run func(string, ...any), session, prompt, reply string, at float64) {
	run(`INSERT INTO sessions (id, source, started_at) VALUES (?,'cli',?)`, session, at)
	run(`INSERT INTO messages (session_id, role, content, timestamp) VALUES (?,'user',?,?)`, session, prompt, at)
	run(`INSERT INTO messages (session_id, role, content, timestamp) VALUES (?,'assistant',?,?)`, session, reply, at+1)
}

// Two panes that continue one session (-c) would show it twice. The newer
// pane owns it.
func TestTwoHermesPanesOnOneSessionShowItOnce(t *testing.T) {
	home, run := hermesDB(t)
	run(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('sess1','assistant','Nothing has changed.',10)`)
	s := newTestServer(t)
	s.getenv = func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	argv := []string{"/bin/hermes", "-p", "steward", "-c"}
	a := newHermesPane(t, s, argv, 50)
	if got := chatTextsOf(t, s, a); len(got) != 1 {
		t.Fatalf("pane A alone: %v", got)
	}
	if err := s.closePane(a, nil); err != nil {
		t.Fatal(err)
	}
	b := newHermesPane(t, s, argv, 60)
	if got := chatTextsOf(t, s, b); len(got) != 1 {
		t.Fatalf("pane B: %v", got)
	}
	if got := chatTextsOf(t, s, a); len(got) != 0 {
		t.Fatalf("the older pane must not repeat the session: %v", got)
	}
}

// A closed pane from before the start time was recorded, and that never
// bound a session, cannot say which session was its own. It shows none.
func TestAClosedHermesPaneThatNeverBoundShowsNothing(t *testing.T) {
	home, run := hermesDB(t)
	run(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('sess1','assistant','live reply',10)`)
	s := newTestServer(t)
	s.getenv = func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	id := newClaudePane(t, s)
	if err := s.updatePane(id, func(p *layout.Pane) {
		p.Harness = "hermes-steward"
		p.Argv = []string{"/bin/hermes", "-p", "steward", "-c"}
		p.Closed = true
		p.EndedAt = 500
	}); err != nil {
		t.Fatal(err)
	}
	if got := chatTextsOf(t, s, id); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

// Hermes keeps a stand-in such as "[terminal]" as the content of an
// assistant row that only calls tools. It is not the agent's words.
func TestAHermesMarkerBesideToolCallsIsNotAnAgentMessage(t *testing.T) {
	home, run := hermesDB(t)
	run(`INSERT INTO messages (session_id, role, content, tool_calls, timestamp) VALUES ('sess1','assistant','[terminal]',
		'[{"id":"c1","type":"function","function":{"name":"terminal","arguments":"{\"command\":\"coppice pane list\"}"}}]',1)`)
	run(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('sess1','assistant','[terminal]',2)`)
	s := newTestServer(t)
	id := hermesPane(t, s, home)
	res := result(t, messagesOf(t, s, `"pane":"`+id+`"`))
	checkMessages(t, res, []wantMsg{
		{"1", "agent", "coppice pane list", "terminal"},
		{"2", "agent", "[terminal]", ""},
	})
}

// After a compression the older pane is bound to the parent session and the
// newer one to the child. Both resolve to the child, so only the newer pane
// shows it.
func TestACompressionDoesNotMakeTwoPanesShowTheSameSession(t *testing.T) {
	home, run := hermesDB(t)
	run(`DELETE FROM sessions`)
	run(`ALTER TABLE sessions ADD COLUMN end_reason TEXT`)
	run(`INSERT INTO sessions (id, source, started_at, end_reason) VALUES ('X','cli',100,'compression')`)
	run(`INSERT INTO sessions (id, source, parent_session_id, started_at) VALUES ('X2','cli','X',200)`)
	run(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('X','assistant','old words',101)`)
	run(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('X2','assistant','steward reply',201)`)
	s := newTestServer(t)
	s.getenv = func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	argv := []string{"/bin/hermes", "-p", "steward", "-c"}
	a := newHermesPane(t, s, argv, 50)
	if err := s.updatePane(a, func(p *layout.Pane) { p.HermesSession = "X" }); err != nil {
		t.Fatal(err)
	}
	b := newHermesPane(t, s, argv, 300)
	if err := s.updatePane(b, func(p *layout.Pane) { p.HermesSession = "X2" }); err != nil {
		t.Fatal(err)
	}
	if got := chatTextsOf(t, s, b); len(got) != 2 || got[1] != "steward reply" {
		t.Fatalf("newer pane: %v", got)
	}
	if got := chatTextsOf(t, s, a); len(got) != 0 {
		t.Fatalf("the older pane must not repeat the child session: %v", got)
	}
}

// Current Hermes writes a session at the first prompt. Two flagless panes in
// one profile: A opened, B opened, A typed, B typed. Each pane shows its own
// session whichever is read first.
func TestTwoFlaglessHermesPanesBindAtTheirFirstPrompt(t *testing.T) {
	for _, bFirst := range []bool{false, true} {
		home, run := hermesDB(t)
		run(`DELETE FROM sessions`)
		s := hermesServer(t, home)
		argv := []string{"/bin/hermes", "-p", "steward"}
		a := newHermesPane(t, s, argv, 100)
		b := newHermesPane(t, s, argv, 200)
		// Nothing is written yet: both panes show nothing.
		if got := chatTextsOf(t, s, a); len(got) != 0 {
			t.Fatalf("idle pane A: %v", got)
		}
		if got := chatTextsOf(t, s, b); len(got) != 0 {
			t.Fatalf("idle pane B: %v", got)
		}
		promptAt(s, a, "what is in the garden", 300)
		hermesTurn(run, "SA", "what is in the garden", "reply of A", 300.3)
		promptAt(s, b, "how tall is the fence", 310)
		hermesTurn(run, "SB", "how tall is the fence", "reply of B", 310.3)
		first, second := a, b
		if bFirst {
			first, second = b, a
		}
		got := map[string][]string{first: chatTextsOf(t, s, first), second: chatTextsOf(t, s, second)}
		if g := got[a]; len(g) != 1 || g[0] != "reply of A" {
			t.Fatalf("bFirst=%v pane A: %v", bFirst, g)
		}
		if g := got[b]; len(g) != 1 || g[0] != "reply of B" {
			t.Fatalf("bFirst=%v pane B: %v", bFirst, g)
		}
	}
}

// A pane that has sent nothing shows nothing, even when another pane's
// session is the newest one of the profile.
func TestAHermesPaneThatHasNotPromptedShowsNothing(t *testing.T) {
	home, run := hermesDB(t)
	run(`DELETE FROM sessions`)
	hermesTurn(run, "SB", "hello there", "reply of B", 250)
	s := hermesServer(t, home)
	a := newHermesPane(t, s, []string{"/bin/hermes", "-p", "steward"}, 100)
	if got := chatTextsOf(t, s, a); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
	// A slash command starts no session, so it is not a prompt.
	promptAt(s, a, "/help", 260)
	if p, _ := s.tree.Pane(a); p.HermesPrompt != "" {
		t.Fatalf("a slash command was taken as a prompt: %q", p.HermesPrompt)
	}
}

// The same words in two panes: each pane gets the session written closest to
// its own prompt, in either read order.
func TestTwoPanesThatSendTheSameWordsEachGetTheirOwnSession(t *testing.T) {
	for _, bFirst := range []bool{false, true} {
		home, run := hermesDB(t)
		run(`DELETE FROM sessions`)
		s := hermesServer(t, home)
		argv := []string{"/bin/hermes", "-p", "steward"}
		a := newHermesPane(t, s, argv, 100)
		b := newHermesPane(t, s, argv, 101)
		promptAt(s, a, "hi", 300)
		promptAt(s, b, "hi", 303)
		hermesTurn(run, "SA", "hi", "reply of A", 300.2)
		hermesTurn(run, "SB", "hi", "reply of B", 303.2)
		order := []string{a, b}
		if bFirst {
			order = []string{b, a}
		}
		for _, id := range order {
			want := "reply of A"
			if id == b {
				want = "reply of B"
			}
			if g := chatTextsOf(t, s, id); len(g) != 1 || g[0] != want {
				t.Fatalf("bFirst=%v pane %s: %v", bFirst, id, g)
			}
		}
	}
}

// A session that existed before the pane's prompt is not the pane's, even
// with the same words.
func TestAnOlderSessionWithTheSameWordsIsNotTheNewPanesSession(t *testing.T) {
	home, run := hermesDB(t)
	run(`DELETE FROM sessions`)
	hermesTurn(run, "OLD", "hi", "old reply", 50)
	s := hermesServer(t, home)
	a := newHermesPane(t, s, []string{"/bin/hermes", "-p", "steward"}, 100)
	promptAt(s, a, "hi", 300)
	if got := chatTextsOf(t, s, a); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

// A closed pane that never bound shows nothing new, prompt or not.
func TestAClosedHermesPaneThatNeverBoundShowsNothingEvenWithAPrompt(t *testing.T) {
	home, run := hermesDB(t)
	run(`DELETE FROM sessions`)
	s := hermesServer(t, home)
	a := newHermesPane(t, s, []string{"/bin/hermes", "-p", "steward"}, 100)
	promptAt(s, a, "hello", 300)
	if err := s.closePane(a, nil); err != nil {
		t.Fatal(err)
	}
	hermesTurn(run, "SA", "hello", "reply", 300.2)
	if got := chatTextsOf(t, s, a); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

// A pane that resumes with -c binds to the newest session as before, and the
// pane that sent the prompt owns the session over it.
func TestAPromptingPaneOwnsItsSessionOverAResumingOne(t *testing.T) {
	home, run := hermesDB(t)
	run(`DELETE FROM sessions`)
	s := hermesServer(t, home)
	a := newHermesPane(t, s, []string{"/bin/hermes", "-p", "steward"}, 100)
	promptAt(s, a, "plan the week", 300)
	hermesTurn(run, "SA", "plan the week", "reply of A", 300.2)
	if got := chatTextsOf(t, s, a); len(got) != 1 {
		t.Fatalf("pane A: %v", got)
	}
	// The steward restarts with -c in a newer pane: it binds to the newest
	// session, which is SA. The older pane sent the prompt and keeps it.
	b := newHermesPane(t, s, []string{"/bin/hermes", "-p", "steward", "-c"}, 400)
	if got := chatTextsOf(t, s, b); len(got) != 0 {
		t.Fatalf("the resuming pane must not repeat the session: %v", got)
	}
	if got := chatTextsOf(t, s, a); len(got) != 1 || got[0] != "reply of A" {
		t.Fatalf("pane A after the restart: %v", got)
	}
}

// A compression ends the bound session and starts a child. The pane follows
// to the child.
func TestAPromptBoundPaneFollowsACompressionToTheTip(t *testing.T) {
	home, run := hermesDB(t)
	run(`DELETE FROM sessions`)
	run(`ALTER TABLE sessions ADD COLUMN end_reason TEXT`)
	s := hermesServer(t, home)
	a := newHermesPane(t, s, []string{"/bin/hermes", "-p", "steward"}, 100)
	promptAt(s, a, "start the long job", 300)
	hermesTurn(run, "X", "start the long job", "early words", 300.2)
	if got := chatTextsOf(t, s, a); len(got) != 1 || got[0] != "early words" {
		t.Fatalf("before: %v", got)
	}
	run(`UPDATE sessions SET end_reason='compression' WHERE id='X'`)
	run(`INSERT INTO sessions (id, source, parent_session_id, started_at) VALUES ('X2','cli','X',400)`)
	run(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('X2','assistant','late words',401)`)
	got := chatTextsOf(t, s, a)
	if len(got) == 0 || got[len(got)-1] != "late words" {
		t.Fatalf("after: %v", got)
	}
}

func TestFeedLineKeepsTheTypedLine(t *testing.T) {
	var l lineIn
	if lines := l.feed([]byte("helo\x7fl")); string(l.buf) != "hell" || len(lines) != 0 {
		t.Fatalf("%q %v", string(l.buf), lines)
	}
	lines := l.feed([]byte("o\x1b[200~ world\x1b[201~\r"))
	if len(l.buf) != 0 || len(lines) != 1 || lines[0] != "hello world" {
		t.Fatalf("%q %v", string(l.buf), lines)
	}
	var m lineIn
	lines = m.feed([]byte("a\x15b\x1b[Ac\r"))
	if len(lines) != 1 || lines[0] != "bc" {
		t.Fatalf("%v", lines)
	}
}

// A line break inside a bracketed paste is part of one message, and Ctrl-J
// is a new line inside the message, not Enter. Only a carriage return
// outside a paste submits.
func TestFeedLineCountsABreakInsideAPasteAsASpace(t *testing.T) {
	var l lineIn
	lines := l.feed([]byte("\x1b[200~first line\nsecond line\r\nthird\x1b[201~"))
	if len(lines) != 0 {
		t.Fatalf("a paste must not end the line: %v", lines)
	}
	lines = l.feed([]byte("\r"))
	if len(lines) != 1 || strings.Join(strings.Fields(lines[0]), " ") != "first line second line third" {
		t.Fatalf("%q", lines)
	}
	var m lineIn
	if lines := m.feed([]byte("one\ntwo\r")); len(lines) != 1 || lines[0] != "one two" {
		t.Fatalf("ctrl-j: %q", lines)
	}
}

// A character cut in two by the write boundary is joined, not turned into
// U+FFFD.
func TestFeedLineJoinsACharacterSplitAcrossWrites(t *testing.T) {
	var l lineIn
	b := []byte("caf\u00e9 \u65e5\u672c\r")
	for i := range b {
		if lines := l.feed(b[i : i+1]); i == len(b)-1 {
			if len(lines) != 1 || lines[0] != "caf\u00e9 \u65e5\u672c" {
				t.Fatalf("%q", lines)
			}
		}
	}
}

// A closed pane that sent the prompt does not keep the session over a live
// pane that resumes it. The live pane shows it; the closed one goes empty.
func TestAClosedPromptingPaneDoesNotTakeTheSessionFromALiveResumingPane(t *testing.T) {
	home, run := hermesDB(t)
	run(`DELETE FROM sessions`)
	s := hermesServer(t, home)
	a := newHermesPane(t, s, []string{"/bin/hermes", "-p", "steward"}, 100)
	promptAt(s, a, "plan the week", 300)
	hermesTurn(run, "SA", "plan the week", "reply of A", 300.2)
	if got := chatTextsOf(t, s, a); len(got) != 1 {
		t.Fatalf("pane A: %v", got)
	}
	if err := s.closePane(a, nil); err != nil {
		t.Fatal(err)
	}
	b := newHermesPane(t, s, []string{"/bin/hermes", "-p", "steward", "-c"}, 400)
	run(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('SA','user','next day',500)`)
	run(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('SA','assistant','live reply in B',501)`)
	if got := chatTextsOf(t, s, b); len(got) == 0 {
		t.Fatalf("the live resuming pane shows nothing")
	}
	if got := chatTextsOf(t, s, a); len(got) != 0 {
		t.Fatalf("the closed pane shows the live chat: %v", got)
	}
}

// With both panes closed the pane that sent the prompt keeps the session.
func TestWithBothPanesClosedThePrompterKeepsTheSession(t *testing.T) {
	home, run := hermesDB(t)
	run(`DELETE FROM sessions`)
	s := hermesServer(t, home)
	a := newHermesPane(t, s, []string{"/bin/hermes", "-p", "steward"}, 100)
	promptAt(s, a, "plan the week", 300)
	hermesTurn(run, "SA", "plan the week", "reply of A", 300.2)
	_ = chatTextsOf(t, s, a) // bind a
	b := newHermesPane(t, s, []string{"/bin/hermes", "-p", "steward", "-c"}, 400)
	if got := chatTextsOf(t, s, b); len(got) != 0 {
		t.Fatalf("live prompter owns it: %v", got)
	}
	if err := s.closePane(a, nil); err != nil {
		t.Fatal(err)
	}
	// b binds now too, while a is closed; then b closes.
	_ = chatTextsOf(t, s, b)
	if err := s.closePane(b, nil); err != nil {
		t.Fatal(err)
	}
	if got := chatTextsOf(t, s, a); len(got) != 1 {
		t.Fatalf("closed prompter: %v", got)
	}
	if got := chatTextsOf(t, s, b); len(got) != 0 {
		t.Fatalf("closed resumer: %v", got)
	}
}

// A prompt whose text Hermes stored differently (a cursor edit changed what
// the terminal showed) binds by time once the window is over.
func TestACursorEditedPromptBindsByTimeAfterTheWindow(t *testing.T) {
	home, run := hermesDB(t)
	run(`DELETE FROM sessions`)
	s := hermesServer(t, home)
	a := newHermesPane(t, s, []string{"/bin/hermes", "-p", "steward"}, 100)
	// The operator typed "helo", Left, "l": Hermes stored "hello".
	s.noteInput(a, []byte("helo\x1b[Dl\r"), time.UnixMilli(300000))
	hermesTurn(run, "SA", "hello", "reply of A", 300.5)
	setClock(s, time.UnixMilli(300000).Add(80*time.Second))
	if got := chatTextsOf(t, s, a); len(got) != 0 {
		t.Fatalf("inside the window the pane waits for a text match: %v", got)
	}
	setClock(s, time.UnixMilli(300000).Add(120*time.Second))
	if got := chatTextsOf(t, s, a); len(got) != 1 || got[0] != "reply of A" {
		t.Fatalf("after the window: %v", got)
	}
}

// The fallback never takes a session another live pane holds, nor one whose
// text another unbound pane typed, and takes the earliest of what is left.
func TestTheTimeFallbackLeavesOtherPanesSessionsAlone(t *testing.T) {
	home, run := hermesDB(t)
	run(`DELETE FROM sessions`)
	s := hermesServer(t, home)
	a := newHermesPane(t, s, []string{"/bin/hermes", "-p", "steward"}, 100)
	b := newHermesPane(t, s, []string{"/bin/hermes", "-p", "steward"}, 110)
	s.noteInput(a, []byte("helo\x1b[Dl\r"), time.UnixMilli(300000))
	s.noteInput(b, []byte("something else\r"), time.UnixMilli(300001))
	hermesTurn(run, "SB", "something else", "reply of B", 300.8)
	hermesTurn(run, "SA", "hello", "reply of A", 300.9)
	setClock(s, time.UnixMilli(300000).Add(120*time.Second))
	// A reads first. SB is earlier but belongs to B's text, so A gets SA.
	if got := chatTextsOf(t, s, a); len(got) != 1 || got[0] != "reply of A" {
		t.Fatalf("A: %v", got)
	}
	if got := chatTextsOf(t, s, b); len(got) != 1 || got[0] != "reply of B" {
		t.Fatalf("B: %v", got)
	}
}

// The fallback takes the session closest to the first prompt, not the
// earliest one in the window.
func TestTheTimeFallbackTakesTheSessionClosestToThePrompt(t *testing.T) {
	home, run := hermesDB(t)
	run(`DELETE FROM sessions`)
	s := hermesServer(t, home)
	a := newHermesPane(t, s, []string{"/bin/hermes", "-p", "steward"}, 100)
	s.noteInput(a, []byte("helo\x1b[Dl\r"), time.UnixMilli(300000))
	// SEARLY began 3 s before the prompt, SNEAR 0.4 s after it.
	hermesTurn(run, "SEARLY", "unrelated early", "reply of early", 297.0)
	hermesTurn(run, "SNEAR", "hello", "reply of near", 300.4)
	setClock(s, time.UnixMilli(300000).Add(120*time.Second))
	if got := chatTextsOf(t, s, a); len(got) != 1 || got[0] != "reply of near" {
		t.Fatalf("closest session: %v", got)
	}
}

// setClock makes the server's clock read t.
func setClock(s *Server, t time.Time) { s.clockSkew.Store(int64(t.Sub(time.Now()))) }

//go:build unix

package server

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/proto"
)

// The fixtures in testdata/transcripts are invented. Each holds the
// traps the reader must skip, and the text FAKE- marks every line that
// must never show as a message.

// fitCwd puts dir in place of the fixtures' padded "/fake" working
// directory, keeping the line's length, so each message keeps its id.
func fitCwd(t *testing.T, b []byte, dir string) []byte {
	t.Helper()
	re := regexp.MustCompile(`"cwd":"/fake" *`)
	return re.ReplaceAllFunc(b, func(m []byte) []byte {
		q, _ := json.Marshal(dir)
		out := append([]byte(`"cwd":`), q...)
		if len(out) > len(m) {
			t.Fatalf("working directory %s is too long for the fixture's padding", dir)
		}
		return append(out, bytes.Repeat([]byte(" "), len(m)-len(out))...)
	})
}

// chatFixture copies a transcript fixture under the Claude projects
// directory the test server's panes see, with pane w1:p1's working
// directory, and returns its path.
func chatFixture(t *testing.T, s *Server, name string) string {
	t.Helper()
	return chatFixtureFor(t, s, name, "w1:p1")
}

// chatFixtureFor is chatFixture with pane id's working directory.
func chatFixtureFor(t *testing.T, s *Server, name, id string) string {
	t.Helper()
	dir := filepath.Join(s.getenv("CLAUDE_CONFIG_DIR"), "projects", "fake-chat")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "transcripts", name))
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := s.tree.Pane(id)
	b = fitCwd(t, b, rec.Cwd)
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// reportTranscript has pane id's own connection report path.
func reportTranscript(t *testing.T, s *Server, id, path string) {
	t.Helper()
	send, next, stop := ownPane(t, s, id)
	defer stop()
	send(gateReportLine("r", id, `"transcript_path":"`+path+`"`))
	mustOK(t, next())
}

// messagesOf runs pane.messages as the operator and returns the reply.
func messagesOf(t *testing.T, s *Server, params string) proto.Response {
	t.Helper()
	return roundTrip(t, s, `{"id":"m","cmd":"pane.messages",`+params+`}`)[0]
}

type wantMsg struct{ id, role, text, tool string }

func checkMessages(t *testing.T, res map[string]any, want []wantMsg) {
	t.Helper()
	got, _ := res["messages"].([]any)
	if len(got) != len(want) {
		b, _ := json.MarshalIndent(got, "", " ")
		t.Fatalf("got %d messages, want %d:\n%s", len(got), len(want), b)
	}
	for i, w := range want {
		m, _ := got[i].(map[string]any)
		tool, _ := m["tool"].(string)
		if m["id"] != w.id || m["role"] != w.role || m["text"] != w.text || tool != w.tool {
			t.Fatalf("message %d = %v, want %+v", i, m, w)
		}
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "FAKE-") {
		t.Fatalf("a message holds text it must never show: %s", b)
	}
}

var claudeChatWant = []wantMsg{
	{"67", "owner", "fix the fake test, it fails on CI", ""},
	{"669", "agent", "The test reads a clock.\nI give it a fake one.[31m", ""},
	{"923", "agent", "Running the suite.", ""},
	{"923.1", "agent", "pytest tests/test_fake.py -q", "Bash"},
	{"2276", "owner", "now push it, please", ""},
	{"2494", "agent", "/fake/src/clock.py", "Read"},
	{"2494.1", "agent", "", "Glob"},
	{"2792", "agent", "A fake reply with no time.", ""},
}

func TestPaneMessagesReadTheTranscriptThePanesOwnHookReported(t *testing.T) {
	s := newFactsServer(t)
	path := chatFixture(t, s, "claude-chat.jsonl")
	reportTranscript(t, s, "w1:p1", path)
	r := messagesOf(t, s, `"pane":"w1:p1"`)
	res := result(t, r)
	checkMessages(t, res, claudeChatWant)
	if res["source"] != "transcript" || res["more"] != false {
		t.Fatalf("source %v more %v", res["source"], res["more"])
	}
	got, _ := res["messages"].([]any)
	first, _ := got[0].(map[string]any)
	if first["at"] != 4070908800.0 {
		t.Fatalf("at %v, want the entry's timestamp", first["at"])
	}
	read, _ := got[5].(map[string]any)
	if read["at"] != 4070905210.0 {
		t.Fatalf("at %v, want the timestamp with its zone", read["at"])
	}
	last, _ := got[7].(map[string]any)
	if last["at"] != 0.0 {
		t.Fatalf("at %v, want 0 for a time the server cannot read", last["at"])
	}
}

func TestPaneMessagesSinceAndLimit(t *testing.T) {
	s := newFactsServer(t)
	reportTranscript(t, s, "w1:p1", chatFixture(t, s, "claude-chat.jsonl"))
	res := result(t, messagesOf(t, s, `"pane":"w1:p1","since":"923.1"`))
	checkMessages(t, res, claudeChatWant[4:])
	if res["more"] != false {
		t.Fatalf("more %v after since, want false", res["more"])
	}
	res = result(t, messagesOf(t, s, `"pane":"w1:p1","limit":2`))
	checkMessages(t, res, claudeChatWant[6:])
	if res["more"] != true {
		t.Fatalf("more %v under a limit, want true", res["more"])
	}
	res = result(t, messagesOf(t, s, `"pane":"w1:p1","since":"669","limit":2`))
	checkMessages(t, res, claudeChatWant[6:])
	if res["more"] != true {
		t.Fatalf("more %v, want true when the limit cut messages after since", res["more"])
	}
	res = result(t, messagesOf(t, s, `"pane":"w1:p1","since":"no-such-id"`))
	checkMessages(t, res, claudeChatWant)
	res = result(t, messagesOf(t, s, `"pane":"w1:p1","since":"2792"`))
	checkMessages(t, res, nil)
}

func TestPaneMessagesRefusesAPaneWithNoTranscript(t *testing.T) {
	s := newFactsServer(t)
	r := messagesOf(t, s, `"pane":"w1:p1"`)
	if r.OK || r.Error.Code != proto.ErrBadRequest ||
		r.Error.Message != "auth fix (w1:p1) has no transcript to read. Its hook reported none." {
		t.Fatalf("pane.messages with no transcript = %+v", r.Error)
	}
	r = messagesOf(t, s, `"pane":"w9:p9"`)
	if r.OK || r.Error.Code != proto.ErrNoSuchPane {
		t.Fatalf("pane.messages of no pane = %+v", r.Error)
	}
}

// A path another pane's hook reported first, or a report from a
// connection that is not the pane's own, is never read for this pane.
func TestPaneMessagesNeverReadAPathNoHookOfThePaneReported(t *testing.T) {
	s := newFactsServer(t)
	got := roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	p2 := result(t, got[0])["pane"].(string)
	if err := s.updatePane(p2, func(p *layout.Pane) { p.Harness = "claude" }); err != nil {
		t.Fatal(err)
	}
	path := chatFixture(t, s, "claude-chat.jsonl")
	reportTranscript(t, s, "w1:p1", path)
	if r := refusedReport(t, s, p2, path); r != transcriptTakenRefusal {
		t.Fatalf("a second pane's report of the same path: %q", r)
	}
	if r := messagesOf(t, s, `"pane":"`+p2+`"`); r.OK {
		t.Fatalf("pane %s read a path pane w1:p1 reported first: %+v", p2, r.Result)
	}
	// The operator's report, and a hello that says it is the pane, name no
	// file: only a process inside the pane does.
	got = roundTrip(t, s, gateReportLine("x", p2, `"transcript_path":"`+path+`"`))
	if !got[0].OK || result(t, got[0])["transcript"] != transcriptNotOwnRefusal {
		t.Fatalf("an operator report of a transcript = %+v", got[0])
	}
	got = roundTripFacts(t, s, &peerFacts{checked: true},
		`{"id":"h","cmd":"hello","role":"pane","pane":"`+p2+`"}`,
		gateReportLine("x", p2, `"transcript_path":"`+path+`"`))
	if !got[1].OK || result(t, got[1])["transcript"] != transcriptNotOwnRefusal {
		t.Fatalf("a hello-placed report of a transcript = %+v", got[1])
	}
	if r := messagesOf(t, s, `"pane":"`+p2+`"`); r.OK {
		t.Fatalf("pane %s read a path the operator reported: %+v", p2, r.Result)
	}
}

// refusedReport sends a report naming path from pane id's own process and
// returns the refusal of its transcript, or "" when the transcript was
// taken. The rest of the report always counts.
func refusedReport(t *testing.T, s *Server, id, path string) string {
	t.Helper()
	send, next, stop := ownPane(t, s, id)
	defer stop()
	send(gateReportLine("r", id, `"transcript_path":"`+path+`"`))
	r := next()
	if r["ok"] != true {
		t.Fatalf("a report was refused whole: %v", r)
	}
	res, _ := r["result"].(map[string]any)
	msg, _ := res["transcript"].(string)
	return msg
}

// writeTranscript writes lines to a new transcript under the projects
// directory the test server's panes see, and returns its path.
func writeTranscript(t *testing.T, s *Server, sub, name, text string) string {
	t.Helper()
	dir := filepath.Join(s.getenv("CLAUDE_CONFIG_DIR"), "projects", sub)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A refused transcript refuses only itself: the state still counts.
func TestARefusedTranscriptStillReportsTheState(t *testing.T) {
	s := newFactsServer(t)
	path := chatFixture(t, s, "claude-chat.jsonl")
	reportTranscript(t, s, "w1:p1", path)
	p2 := newClaudePane(t, s)
	send, next, stop := ownPane(t, s, p2)
	defer stop()
	send(`{"id":"r","cmd":"pane.report_state","pane":"` + p2 + `","event":{"v":1,"ts":1757300000.0,` +
		`"session_id":"s","harness":"claude-code","pane":"` + p2 + `","state":"blocked","source":"gate",` +
		`"ask":{"id":"toolu_x","tool":"Bash","summary":"ls","deadline":9999999999.0},` +
		`"transcript_path":"` + path + `"}}`)
	r := next()
	res, _ := r["result"].(map[string]any)
	if r["ok"] != true || res["state"] != "blocked" || res["transcript"] != transcriptTakenRefusal {
		t.Fatalf("report = %v, want the state taken and the transcript refused", r)
	}
}

// A pane.resume hands the old pane's transcripts to the new pane, so the
// resumed session's hook names its own file.
func TestAResumedPaneKeepsItsTranscript(t *testing.T) {
	s := newFactsServer(t)
	path := chatFixture(t, s, "claude-chat.jsonl")
	reportTranscript(t, s, "w1:p1", path)
	p2 := newClaudePane(t, s)
	s.moveClaims("w1:p1", p2)
	if r := refusedReport(t, s, p2, path); r != "" {
		t.Fatalf("the resumed pane's own transcript was refused: %q", r)
	}
	if r := refusedReport(t, s, "w1:p1", path); r != transcriptTakenRefusal {
		t.Fatalf("the old pane kept the transcript: %q", r)
	}
}

func TestAFirstRecordWithNoTimeOrDirectoryIsRefused(t *testing.T) {
	s := newFactsServer(t)
	rec, _ := s.tree.Pane("w1:p1")
	cwd, _ := json.Marshal(rec.Cwd)
	for name, line := range map[string]string{
		"notime.jsonl": `{"type":"user","cwd":` + string(cwd) + `,"message":{"content":"x"}}`,
		"nocwd.jsonl":  `{"type":"user","timestamp":"2099-01-01T00:00:00Z","message":{"content":"x"}}`,
	} {
		p := writeTranscript(t, s, "bare", name, line+"\n")
		if r := refusedReport(t, s, "w1:p1", p); r != transcriptBareRefusal {
			t.Fatalf("%s: %q", name, r)
		}
	}
	long := `{"type":"user","timestamp":"2099-01-01T00:00:00Z","cwd":` + string(cwd) +
		`,"message":{"content":"` + strings.Repeat("x", firstRecordCap) + `"}}` + "\n"
	p := writeTranscript(t, s, "bare", "long.jsonl", long)
	if r := refusedReport(t, s, "w1:p1", p); r != transcriptLongRefusal {
		t.Fatalf("long first line: %q", r)
	}
}

// A transcript that began in another directory, such as the owner's own
// Claude session in another terminal, is not the pane's.
// A pane that works in / would take every transcript as under its
// directory, so it takes only one that began in / itself.
func TestAPaneInTheRootTakesOnlyATranscriptThatBeganThere(t *testing.T) {
	s := newFactsServer(t)
	got := roundTrip(t, s, strings.Replace(paneCreate, "%s", "/", 1))
	id := result(t, got[0])["pane"].(string)
	if err := s.updatePane(id, func(p *layout.Pane) { p.Harness = "claude" }); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"user","timestamp":"2099-01-01T00:00:00Z","cwd":"/elsewhere","message":{"content":"x"}}`
	p := writeTranscript(t, s, "root", "a.jsonl", line+"\n")
	if r := refusedReport(t, s, id, p); r != transcriptCwdRefusal {
		t.Fatalf("a pane in / took a transcript from /elsewhere: %q", r)
	}
	line = `{"type":"user","timestamp":"2099-01-01T00:00:00Z","cwd":"/","message":{"content":"x"}}`
	p = writeTranscript(t, s, "root", "b.jsonl", line+"\n")
	if r := refusedReport(t, s, id, p); r != "" {
		t.Fatalf("a pane in / refused a transcript that began in /: %q", r)
	}
}

func TestATranscriptFromAnotherDirectoryIsRefused(t *testing.T) {
	s := newFactsServer(t)
	line := `{"type":"user","timestamp":"2099-01-01T00:00:00Z","cwd":"/elsewhere","message":{"content":"x"}}`
	p := writeTranscript(t, s, "other", "other.jsonl", line+"\n")
	if r := refusedReport(t, s, "w1:p1", p); r != transcriptCwdRefusal {
		t.Fatalf("a transcript from another directory: %q", r)
	}
}

// A claim is on the real path, checked again at each read: a directory
// swapped for a symlink after the claim leads nowhere the pane may read.
func TestASymlinkSwapAfterTheClaimIsNotRead(t *testing.T) {
	s := newFactsServer(t)
	path := chatFixture(t, s, "claude-chat.jsonl")
	reportTranscript(t, s, "w1:p1", path)
	dir := filepath.Dir(path)
	other := filepath.Join(filepath.Dir(dir), "victim")
	if err := os.Rename(dir, other); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, dir); err != nil {
		t.Fatal(err)
	}
	if r := messagesOf(t, s, `"pane":"w1:p1"`); r.OK {
		t.Fatalf("read through a swapped path: %+v", r.Result)
	}
}

// newClaudePane adds a live claude pane to s and returns its id.
func newClaudePane(t *testing.T, s *Server) string {
	t.Helper()
	got := roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	id := result(t, got[0])["pane"].(string)
	if err := s.updatePane(id, func(p *layout.Pane) { p.Harness = "claude" }); err != nil {
		t.Fatal(err)
	}
	return id
}

// An ended pane's transcript, the ended foreman's included, stays its own.
func TestAPathAnEndedPaneNamedIsNeverTaken(t *testing.T) {
	s := newFactsServer(t)
	path := chatFixture(t, s, "claude-chat.jsonl")
	reportTranscript(t, s, "w1:p1", path)
	if err := s.updatePane("w1:p1", func(p *layout.Pane) { p.Closed = true }); err != nil {
		t.Fatal(err)
	}
	p2 := newClaudePane(t, s)
	if r := refusedReport(t, s, p2, path); r != transcriptTakenRefusal {
		t.Fatalf("a pane took an ended pane's transcript: %q", r)
	}
}

// A second spelling of a claimed file through a directory symlink is the
// same file.
func TestASymlinkedSpellingOfAClaimedTranscriptIsRefused(t *testing.T) {
	s := newFactsServer(t)
	path := chatFixture(t, s, "claude-chat.jsonl")
	reportTranscript(t, s, "w1:p1", path)
	alias := filepath.Join(filepath.Dir(filepath.Dir(path)), "alias")
	if err := os.Symlink(filepath.Dir(path), alias); err != nil {
		t.Fatal(err)
	}
	p2 := newClaudePane(t, s)
	if r := refusedReport(t, s, p2, filepath.Join(alias, filepath.Base(path))); r != transcriptTakenRefusal {
		t.Fatalf("a symlinked spelling of a claimed transcript: %q", r)
	}
}

func TestATranscriptOlderThanThePaneIsRefused(t *testing.T) {
	s := newFactsServer(t)
	dir := filepath.Join(s.getenv("CLAUDE_CONFIG_DIR"), "projects", "old")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "old.jsonl")
	rec, _ := s.tree.Pane("w1:p1")
	cwd, _ := json.Marshal(rec.Cwd)
	line := `{"type":"user","timestamp":"2020-01-01T00:00:00Z","cwd":` + string(cwd) +
		`,"message":{"content":"FAKE-OLD"}}` + "\n"
	if err := os.WriteFile(old, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := refusedReport(t, s, "w1:p1", old); r != transcriptOldRefusal {
		t.Fatalf("a transcript older than the pane: %q", r)
	}
}

// The claims outlive the server, in a 0600 file in the data dir.
func TestAClaimOutlivesTheServer(t *testing.T) {
	s := newFactsServer(t)
	path := chatFixture(t, s, "claude-chat.jsonl")
	reportTranscript(t, s, "w1:p1", path)
	file := filepath.Join(s.cfg.DataDir, claimsFile)
	fi, err := os.Stat(file)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("claims file: %v %v", fi, err)
	}
	s2, err := New(Config{SocketPath: filepath.Join(t.TempDir(), "s.sock"), DataDir: s.cfg.DataDir})
	if err != nil {
		t.Fatal(err)
	}
	real, _ := realTranscriptPath(path)
	if got := s2.claimedBy("w1:p1"); len(got) != 1 || got[0] != real {
		t.Fatalf("claims after a restart: %v", got)
	}
	if r := s2.claim("w1:p2", real); r != transcriptTakenRefusal {
		t.Fatalf("a claim after a restart: %q", r)
	}
}

func TestASprigSessionTreeGivesMessages(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "transcripts", "sprig-chat.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fake-session.jsonl"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t)
	path, kind := s.transcriptOf(layout.Pane{
		ID: "w9:p1", Kind: layout.KindHeadless, Harness: "sprig",
		Argv: []string{"sprig", "--session-dir", dir}, HarnessSessionID: "fake-session",
	})
	msgs, cut, err := readMessages(path, kind)
	if err != nil || cut {
		t.Fatalf("read: %v cut %v", err, cut)
	}
	res := map[string]any{}
	raw, _ := json.Marshal(msgs)
	var list []any
	_ = json.Unmarshal(raw, &list)
	res["messages"] = list
	checkMessages(t, res, []wantMsg{
		{"0", "owner", "a fake task", ""},
		{"82", "agent", "Looking at it.", ""},
		{"295", "agent", "ls -la", "bash"},
		{"702", "agent", "fetch the fake page", "web"},
		{"957", "agent", "Done.", ""},
	})
	if msgs[0].At != 1.5 {
		t.Fatalf("at %v, want the entry's ts", msgs[0].At)
	}
}

func TestMessageTextIsCleanedAndCapped(t *testing.T) {
	long := strings.Repeat("é", 2100)
	if got := cleanMessage(long); got != strings.Repeat("é", 2000)+"…" {
		t.Fatalf("a long message is %d runes", len([]rune(got)))
	}
	if got := cleanMessage("\x1b]0;x\x07a\tb\r\n\x00c\u0085 "); got != "]0;xa b\nc" {
		t.Fatalf("cleaned %q", got)
	}
	// Bidi marks, zero-width characters and the line and paragraph
	// separators change how a line shows, so they go too.
	if got := cleanMessage("a\u202eb\u200bc\u2028d\u2029e\ufefff"); got != "abcdef" {
		t.Fatalf("cleaned %q", got)
	}
	cmd := strings.Repeat("x", 300)
	if got := toolSummary(map[string]any{"command": cmd}, ""); got != strings.Repeat("x", 200)+"…" {
		t.Fatalf("a long summary is %d runes", len([]rune(got)))
	}
	if got := toolSummary(map[string]any{"command": " ", "path": "a\n\tb"}, "d"); got != "a b" {
		t.Fatalf("summary %q, want the first string that is not empty", got)
	}
	if got := toolSummary(nil, "do\nit"); got != "do it" {
		t.Fatalf("summary %q, want the detail", got)
	}
	// A missing file reads as no messages.
	msgs, cut, err := readMessages(filepath.Join(t.TempDir(), "none.jsonl"), kindClaude)
	if err != nil || cut || len(msgs) != 0 {
		t.Fatalf("missing file: %v %v %v", msgs, cut, err)
	}
}

// The read takes only the last messagesWindow bytes, and says that older
// messages exist.
func TestPaneMessagesReadOnlyTheTail(t *testing.T) {
	old := messagesWindow
	messagesWindow = 300
	defer func() { messagesWindow = old }()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "transcripts", "claude-chat.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	msgs, cut, err := readMessages(p, kindClaude)
	if err != nil || !cut {
		t.Fatalf("cut %v err %v, want a cut read", cut, err)
	}
	if len(msgs) != 1 || msgs[0].ID != "2792" {
		t.Fatalf("got %+v, want only the last message", msgs)
	}
}

func TestOnlyTheOperatorReadsTheChat(t *testing.T) {
	s := newFactsServer(t)
	reportTranscript(t, s, "w1:p1", chatFixture(t, s, "claude-chat.jsonl"))
	got := roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	p2 := result(t, got[0])["pane"].(string)

	chat := `{"id":"1","cmd":"floor.chat"}`
	own := `{"id":"2","cmd":"pane.messages","pane":"w1:p1"}`
	refused := func(r proto.Response, msg string) bool {
		return !r.OK && r.Error != nil && r.Error.Code == proto.ErrUnauthorized && r.Error.Message == msg
	}
	for _, f := range []*peerFacts{
		{checked: true, pane: true, paneID: "w1:p1"},
		{checked: true, unknown: true},
		{checked: true, plugin: "p"},
	} {
		got := roundTripFacts(t, s, f, chat)
		if !refused(got[0], TalkRefusal) {
			t.Fatalf("floor.chat from %+v = %+v, want the refusal", f, got[0])
		}
	}
	// A pane reads its own messages, and no other pane's.
	got = roundTripFacts(t, s, &peerFacts{checked: true, pane: true, paneID: "w1:p1"}, own)
	if !got[0].OK {
		t.Fatalf("a pane reading its own messages = %+v", got[0].Error)
	}
	got = roundTripFacts(t, s, &peerFacts{checked: true, pane: true, paneID: p2}, own)
	if !refused(got[0], MessagesRefusal) {
		t.Fatalf("a pane reading another's messages = %+v", got[0])
	}
	// A peer nobody placed may not name itself the pane.
	got = roundTripFacts(t, s, &peerFacts{checked: true, unknown: true},
		`{"id":"0","cmd":"hello","role":"pane","pane":"w1:p1"}`, own)
	if !refused(got[1], MessagesRefusal) {
		t.Fatalf("an unplaced peer reading a pane's messages = %+v", got[1])
	}
	got = roundTripFacts(t, s, &peerFacts{checked: true, plugin: "p"}, own)
	if !refused(got[0], MessagesRefusal) {
		t.Fatalf("a plugin reading a pane's messages = %+v", got[0])
	}
	// A named web token reads neither. The owner's own token, with no
	// name, reads both.
	got = roundTrip(t, s, `{"id":"h","cmd":"hello","name_from":"token","name":"kid"}`, chat, own)
	if !refused(got[1], ChatRefusal) || !refused(got[2], ChatRefusal) {
		t.Fatalf("a named token = %+v / %+v", got[1], got[2])
	}
	got = roundTrip(t, s, `{"id":"h","cmd":"hello","name_from":"token"}`, chat, own)
	if !got[1].OK || !got[2].OK {
		t.Fatalf("the owner's token = %+v / %+v", got[1].Error, got[2].Error)
	}
	// A socket name is a label, not a token.
	got = roundTrip(t, s, `{"id":"h","cmd":"hello","name":"me"}`, chat)
	if !got[1].OK {
		t.Fatalf("a socket name = %+v", got[1].Error)
	}
}

// chatFile is the path of today's chat file in s's data dir.
func chatFile(s *Server) string {
	return filepath.Join(s.cfg.DataDir, "chat", time.Now().UTC().Format("2006-01-02")+".jsonl")
}

func chatOf(t *testing.T, s *Server, params string) map[string]any {
	t.Helper()
	return result(t, roundTrip(t, s, `{"id":"c","cmd":"floor.chat"`+params+`}`)[0])
}

func TestTalkWritesTheOwnersLineToTheChatFirst(t *testing.T) {
	fakeForeman(t)
	s := newTestServer(t)
	res := talk(t, s, "hello\tforeman\x07")
	id := res["pane"].(string)
	b, err := os.ReadFile(chatFile(s))
	if err != nil {
		t.Fatal(err)
	}
	var line map[string]any
	if err := json.Unmarshal(b, &line); err != nil || !strings.HasSuffix(string(b), "}\n") {
		t.Fatalf("chat file %q: %v", b, err)
	}
	if line["kind"] != "user" || line["text"] != "hello foreman" || line["pane"] != id {
		t.Fatalf("chat line %v", line)
	}
	date, _ := line["date"].(string)
	if when, err := time.Parse("2006-01-02T15:04:05.000Z", date); err != nil || time.Since(when) > time.Minute {
		t.Fatalf("date %q, want now in UTC to the millisecond", date)
	}
	for path, want := range map[string]os.FileMode{chatFile(s): 0o600, filepath.Dir(chatFile(s)): 0o700} {
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm() != want {
			t.Fatalf("%s mode %v, want %v (%v)", path, fi.Mode().Perm(), want, err)
		}
	}
	c := chatOf(t, s, "")
	msgs, _ := c["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("floor.chat = %v", c)
	}
	m := msgs[0].(map[string]any)
	day := time.Now().UTC().Format("2006-01-02")
	if m["role"] != "owner" || m["text"] != "hello foreman" || m["pane"] != id || m["id"] != "chat/"+day+"/0" {
		t.Fatalf("message %v", m)
	}
}

func TestANewForemanAddsANoteToTheChat(t *testing.T) {
	fakeForeman(t)
	s := newTestServer(t)
	first := talk(t, s, "one")["pane"].(string)
	roundTrip(t, s, `{"id":"x","cmd":"pane.close","pane":"`+first+`"}`)
	second := talk(t, s, "two")["pane"].(string)
	if second == first {
		t.Fatal("no new foreman started")
	}
	c := chatOf(t, s, "")
	msgs, _ := c["messages"].([]any)
	var roles, texts, panes []string
	for _, x := range msgs {
		m := x.(map[string]any)
		roles = append(roles, m["role"].(string))
		texts = append(texts, m["text"].(string))
		panes = append(panes, m["pane"].(string))
	}
	if strings.Join(roles, ",") != "owner,note,owner" ||
		strings.Join(texts, "|") != "one|A new foreman started.|two" ||
		strings.Join(panes, ",") != first+","+second+","+second {
		t.Fatalf("chat roles %v texts %v panes %v", roles, texts, panes)
	}
}

// The chat holds the owner's lines from the chat file and only the
// foreman's own messages from its transcript, merged by time.
func TestFloorChatMergesTheForemansTranscript(t *testing.T) {
	fakeForeman(t)
	s := newTestServer(t)
	s.factsEvery = 0
	claudeDir := t.TempDir()
	s.getenv = func(k string) string {
		if k == "CLAUDE_CONFIG_DIR" {
			return claudeDir
		}
		return ""
	}
	id := talk(t, s, "hello foreman")["pane"].(string)
	if err := s.updatePane(id, func(p *layout.Pane) { p.Harness = "claude" }); err != nil {
		t.Fatal(err)
	}
	reportTranscript(t, s, id, chatFixtureFor(t, s, "claude-chat.jsonl", id))
	c := chatOf(t, s, "")
	msgs, _ := c["messages"].([]any)
	var want []wantMsg
	for _, w := range claudeChatWant {
		if w.role == "agent" {
			want = append(want, wantMsg{id + "/0/" + w.id, w.role, w.text, w.tool})
		}
	}
	// By time: the reply with no time, the owner's line, written now, then
	// the transcript, dated 2099: the two tool calls an hour behind by
	// their zone, then the rest.
	day := time.Now().UTC().Format("2006-01-02")
	owner := wantMsg{"chat/" + day + "/0", "owner", "hello foreman", ""}
	want = []wantMsg{want[5], owner, want[3], want[4], want[0], want[1], want[2]}
	checkMessages(t, c, want)
	for _, x := range msgs {
		if x.(map[string]any)["pane"] != id {
			t.Fatalf("message %v names no foreman", x)
		}
	}
	c = chatOf(t, s, `,"since":"`+id+`/0/923"`)
	checkMessages(t, c, want[len(want)-1:])
	// After a restart the claim names the foreman's transcript, and the
	// chat keeps its replies.
	s2, err := New(Config{SocketPath: filepath.Join(t.TempDir(), "s.sock"), DataDir: s.cfg.DataDir})
	if err != nil {
		t.Fatal(err)
	}
	c = result(t, roundTrip(t, s2, `{"id":"c","cmd":"floor.chat"}`)[0])
	checkMessages(t, c, want)
}

func writeChatDay(t *testing.T, s *Server, day, text string) string {
	t.Helper()
	dir := filepath.Join(s.cfg.DataDir, "chat")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, day+".jsonl")
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestADamagedChatFileMovesAsideAndTheChatStartsFresh(t *testing.T) {
	s := newTestServer(t)
	good := writeChatDay(t, s, "2026-01-01", `{"kind":"user","date":"2026-01-01T00:00:00.000Z","pane":"w1:p1","text":"kept"}`+"\n")
	bad := writeChatDay(t, s, "2026-01-02",
		`{"kind":"user","date":"2026-01-02T00:00:00.000Z","pane":"w1:p1","text":"lost"}`+"\nnot a chat line\n")
	c := chatOf(t, s, "")
	msgs, _ := c["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["text"] != "kept" {
		t.Fatalf("chat %v, want only the line of the good file", c)
	}
	if _, err := os.Stat(good); err != nil {
		t.Fatalf("the good file moved: %v", err)
	}
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Fatalf("the damaged file is still in place: %v", err)
	}
	moved, _ := filepath.Glob(bad + ".broken-*")
	if len(moved) != 1 {
		t.Fatalf("moved aside: %v", moved)
	}
	fi, err := os.Stat(moved[0])
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("moved file mode: %v %v", fi, err)
	}
	notes := result(t, roundTrip(t, s, `{"id":"n","cmd":"floor.notes"}`)[0])["notes"].([]any)
	found := false
	for _, n := range notes {
		if txt, _ := n.(map[string]any)["text"].(string); strings.Contains(txt, filepath.Base(moved[0])) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no note names where the file went: %v", notes)
	}
}

func TestAHalfWrittenChatLineIsLeftOutAndCutBeforeTheNextWrite(t *testing.T) {
	fakeForeman(t)
	s := newTestServer(t)
	day := time.Now().UTC().Format("2006-01-02")
	p := writeChatDay(t, s, day, `{"kind":"user","date":"2026-01-01T00:00:01.000Z","pane":"w1:p1","text":"whole"}`+"\n"+`{"kind":"us`)
	c := chatOf(t, s, "")
	if msgs, _ := c["messages"].([]any); len(msgs) != 1 {
		t.Fatalf("chat %v, want the whole line only", c)
	}
	talk(t, s, "next")
	b, _ := os.ReadFile(p)
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], `"text":"next"`) {
		t.Fatalf("chat file %q, want the half line moved out", b)
	}
	// The half line is moved, never cut away.
	moved, _ := filepath.Glob(p + ".broken-*")
	if len(moved) != 1 {
		t.Fatalf("moved aside: %v", moved)
	}
	tail, _ := os.ReadFile(moved[0])
	fi, _ := os.Stat(moved[0])
	if string(tail) != `{"kind":"us` || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the moved tail is %q, mode %v", tail, fi.Mode().Perm())
	}
}

// A damaged chat file that cannot move aside stays where it is, and the
// rest of the chat still reads, with a note.
func TestADamagedChatFileThatCannotMoveLeavesTheRestReadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root renames in a dir of mode 0500")
	}
	s := newTestServer(t)
	writeChatDay(t, s, "2026-01-01", `{"kind":"user","date":"2026-01-01T00:00:00.000Z","pane":"w1:p1","text":"kept"}`+"\n")
	bad := writeChatDay(t, s, "2026-01-02", "not a chat line\n")
	dir := filepath.Dir(bad)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	c := chatOf(t, s, "")
	msgs, _ := c["messages"].([]any)
	if len(msgs) != 1 || c["more"] != true {
		t.Fatalf("floor.chat = %v, want the readable file and more", c)
	}
	if _, err := os.Lstat(bad); err != nil {
		t.Fatalf("the damaged file is gone: %v", err)
	}
}

func TestAChatFileThatCannotBeReadMovesNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file of mode 0")
	}
	s := newTestServer(t)
	p := writeChatDay(t, s, "2026-01-01", `{"kind":"user","date":"2026-01-01T00:00:01.000Z","pane":"w1:p1","text":"x"}`+"\n")
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(p, 0o600)
	writeChatDay(t, s, "2026-01-02", `{"kind":"user","date":"2026-01-02T00:00:00.000Z","pane":"w1:p1","text":"kept"}`+"\n")
	// The rest of the chat still reads, more says a part was left out, and
	// one note names the file, however often the chat is read.
	for i := 0; i < 2; i++ {
		c := chatOf(t, s, "")
		msgs, _ := c["messages"].([]any)
		if len(msgs) != 1 || c["more"] != true {
			t.Fatalf("floor.chat = %v, want the readable file and more", c)
		}
	}
	notes := result(t, roundTrip(t, s, `{"id":"n","cmd":"floor.notes"}`)[0])["notes"].([]any)
	n := 0
	for _, x := range notes {
		if txt, _ := x.(map[string]any)["text"].(string); strings.Contains(txt, "cannot read the chat file") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d notes about the unreadable file, want 1: %v", n, notes)
	}
	if _, err := os.Lstat(p); err != nil {
		t.Fatalf("the file moved: %v", err)
	}
	if moved, _ := filepath.Glob(p + ".broken-*"); len(moved) != 0 {
		t.Fatalf("an I/O error moved the file: %v", moved)
	}
}

// waitEvent reads from next until an event of kind for pane comes, or
// fails after within.
func waitEvent(t *testing.T, next func() map[string]any, kind, pane string, within time.Duration) map[string]any {
	t.Helper()
	got := make(chan map[string]any, 1)
	go func() {
		for {
			m := next()
			if m["event"] == kind && m["pane"] == pane {
				got <- m
				return
			}
		}
	}()
	select {
	case m := <-got:
		return m
	case <-time.After(within):
		t.Fatalf("no %s event for %s", kind, pane)
		return nil
	}
}

func TestAMessagesEventComesWhenTheTranscriptGrows(t *testing.T) {
	s := newFactsServer(t)
	path := chatFixture(t, s, "claude-chat.jsonl")
	reportTranscript(t, s, "w1:p1", path)
	send, next, stop := streamFacts(t, s, nil)
	defer stop()
	send(`{"id":"s","cmd":"events.subscribe","kinds":["messages"],"panes":"*"}`)
	mustOK(t, next())
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"assistant","timestamp":"2026-01-01T00:01:00Z","message":{"content":"more"}}` + "\n")
	_ = f.Close()
	ev := waitEvent(t, next, "messages", "w1:p1", 5*time.Second)
	if _, has := ev["chat"]; has {
		t.Fatalf("event %v, want no chat for a pane that is not the foreman", ev)
	}
}

func TestATalkSendsAChatMessagesEvent(t *testing.T) {
	fakeForeman(t)
	s := newTestServer(t)
	send, next, stop := streamFacts(t, s, nil)
	defer stop()
	send(`{"id":"s","cmd":"events.subscribe","kinds":["messages"],"panes":"*"}`)
	mustOK(t, next())
	send(`{"id":"t","cmd":"floor.talk","text":"hi"}`)
	// The event may come before the reply or after it.
	var id string
	var ev map[string]any
	for id == "" || ev == nil {
		m := next()
		if m["id"] == "t" {
			res, _ := m["result"].(map[string]any)
			id, _ = res["pane"].(string)
		}
		if m["event"] == "messages" {
			ev = m
		}
	}
	if ev["pane"] != id || ev["chat"] != true {
		t.Fatalf("event %v, want chat true for %s", ev, id)
	}
}

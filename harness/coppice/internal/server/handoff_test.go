package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/detect"
	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/proto"
)

// Moving the conversation to another loop (F12b). agent.move reads the
// transcript the way pane.messages does, builds a brief with no tool rows,
// and starts a new agent of the chosen harness beside the old one.

// moveServer is a facts server whose claude pane w1:p1 reported the
// claude-chat transcript, and whose config names a codex harness that
// echoes what it reads.
func moveServer(t *testing.T) (*Server, string) {
	t.Helper()
	s := newFactsServer(t)
	out := filepath.Join(t.TempDir(), "brief.txt")
	if err := config.Save(config.Config{Harness: map[string]config.Harness{
		"codex": {Command: "bash", Args: []string{"-c", "cat > " + out}},
	}}); err != nil {
		t.Fatal(err)
	}
	reportTranscript(t, s, "w1:p1", chatFixture(t, s, "claude-chat.jsonl"))
	return s, out
}

func moveOf(t *testing.T, s *Server, params string) map[string]any {
	t.Helper()
	got := roundTrip(t, s, `{"id":"mv","cmd":"agent.move",`+params+`}`)
	return result(t, got[0])
}

func liveCount(s *Server) int {
	n := 0
	for _, p := range s.tree.Panes() {
		if !p.Closed {
			n++
		}
	}
	return n
}

func TestMovePreviewGivesTheCostAndStartsNothing(t *testing.T) {
	s, _ := moveServer(t)
	before := liveCount(s)
	res := moveOf(t, s, `"pane":"w1:p1","harness":"codex","preview":true`)
	if res["harness"] != "codex" || res["from"] != "claude" {
		t.Fatalf("preview = %v", res)
	}
	tokens, _ := res["tokens"].(float64)
	msgs, _ := res["messages"].(float64)
	if tokens < 20 || msgs != 5 {
		t.Fatalf("tokens %v messages %v, want some tokens and the 5 owner and agent texts", tokens, msgs)
	}
	if res["trimmed"] != false {
		t.Fatalf("trimmed = %v, want false", res["trimmed"])
	}
	if _, has := res["pane"]; has {
		t.Fatalf("a preview names a new pane: %v", res)
	}
	if liveCount(s) != before {
		t.Fatalf("a preview started an agent")
	}
}

func TestMovePreviewReportsTheConversationSizeFromTheTranscriptUsage(t *testing.T) {
	s, _ := moveServer(t)
	// The fixture counts no usage, so the preview names no conversation size.
	res := moveOf(t, s, `"pane":"w1:p1","harness":"codex","preview":true`)
	if _, has := res["conversation"]; has {
		t.Fatalf("conversation = %v for a transcript with no usage", res["conversation"])
	}
	// A transcript with usage: fresh input and cache writes count once, cache
	// reads and output not at all.
	cwd := mustPane(t, s, "w1:p1").Cwd
	dir := filepath.Join(s.getenv("CLAUDE_CONFIG_DIR"), "projects", "fake-usage")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	lines := `{"type":"user","timestamp":"2099-01-01T00:00:00Z","cwd":"` + cwd + `","message":{"content":"hello there"}}` + "\n" +
		`{"type":"assistant","timestamp":"2099-01-01T00:00:01Z","message":{"id":"m1","model":"claude-fake-1","content":[{"type":"text","text":"hi"}],` +
		`"usage":{"input_tokens":100,"cache_creation_input_tokens":50,"cache_read_input_tokens":9000,"output_tokens":20}}}` + "\n"
	path := filepath.Join(dir, "usage.jsonl")
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	reportTranscript(t, s, "w1:p1", path)
	res = moveOf(t, s, `"pane":"w1:p1","harness":"codex","preview":true`)
	if res["conversation"] != float64(150) || res["messages"] != float64(2) {
		t.Fatalf("preview = %v, want conversation 150 over 2 messages", res)
	}
}

func mustPane(t *testing.T, s *Server, id string) (p layout.Pane) {
	t.Helper()
	p, ok := s.tree.Pane(id)
	if !ok {
		t.Fatalf("no pane %s", id)
	}
	return p
}

func TestMoveStartsANewAgentAndKeepsTheOldOne(t *testing.T) {
	s, out := moveServer(t)
	res := moveOf(t, s, `"pane":"w1:p1","harness":"codex"`)
	id, _ := res["pane"].(string)
	if res["when"] != "typed" {
		t.Fatalf("a terminal agent with no ready rule: when = %v, want typed", res["when"])
	}
	if id == "" || id == "w1:p1" || res["from"] != "claude" || res["harness"] != "codex" {
		t.Fatalf("move = %v", res)
	}
	old, ok := s.tree.Pane("w1:p1")
	if !ok || old.Closed {
		t.Fatalf("the old agent did not stay: %+v", old)
	}
	if _, live := s.Live("w1:p1"); !live {
		t.Fatal("the old agent is no longer live")
	}
	rec := mustPane(t, s, id)
	if rec.Harness != "codex" || rec.Cwd != old.Cwd || rec.Workspace != old.Workspace || rec.Tab != old.Tab {
		t.Fatalf("new record = %+v, old = %+v", rec, old)
	}
	// The new agent read the brief: owner and agent text, no tool rows.
	var got string
	for end := time.Now().Add(5 * time.Second); !strings.Contains(got, "A fake reply with no time."); {
		if time.Now().After(end) {
			t.Fatalf("the new agent never read the whole brief:\n%s", got)
		}
		time.Sleep(20 * time.Millisecond)
		b, _ := os.ReadFile(out)
		got = string(b)
	}
	for _, want := range []string{"fix the fake test, it fails on CI", "now push it, please", "(claude)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the brief lacks %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"FAKE-", "pytest", "Bash", "clock.py", "Glob"} {
		if strings.Contains(got, bad) {
			t.Fatalf("the brief carries %q:\n%s", bad, got)
		}
	}
}

func TestMoveRefusals(t *testing.T) {
	s, _ := moveServer(t)
	cases := map[string]string{
		`"pane":"w1:p1","harness":"claude"`:    "auth fix already runs on claude.",
		`"pane":"w1:p1","harness":"nope"`:      `no harness named "nope"`,
		`"pane":"w1:p1","harness":"two words"`: "harness must be a name",
		`"pane":"w1:p1","harness":""`:          "harness must be a name",
		`"pane":"w9:p9","harness":"codex"`:     `no pane "w9:p9"`,
	}
	for params, want := range cases {
		got := roundTrip(t, s, `{"id":"mv","cmd":"agent.move",`+params+`,"preview":true}`)[0]
		if want == "" {
			if !got.OK {
				t.Fatalf("%s: %+v", params, got.Error)
			}
			continue
		}
		if got.OK || got.Error.Code == "" || !strings.Contains(got.Error.Message, want) {
			t.Fatalf("%s: %+v, want %q", params, got, want)
		}
	}
}

func TestMoveRefusesAPaneWithNoTranscriptAndAnEndedPane(t *testing.T) {
	s := newFactsServer(t) // pane w1:p1 reported no transcript
	if err := config.Save(config.Config{Harness: map[string]config.Harness{
		"codex": {Command: "bash", Args: []string{"-c", "cat"}},
	}}); err != nil {
		t.Fatal(err)
	}
	got := roundTrip(t, s, `{"id":"mv","cmd":"agent.move","pane":"w1:p1","harness":"codex"}`)[0]
	if got.OK || !strings.Contains(got.Error.Message, "has no transcript to move") {
		t.Fatalf("no transcript: %+v", got)
	}
	if liveCount(s) != 1 {
		t.Fatal("a refused move started an agent")
	}
	if err := s.closePane("w1:p1", nil); err != nil {
		t.Fatal(err)
	}
	got = roundTrip(t, s, `{"id":"mv","cmd":"agent.move","pane":"w1:p1","harness":"codex"}`)[0]
	if got.OK || got.Error.Code != "pane_closed" {
		t.Fatalf("ended pane: %+v", got)
	}
}

func TestMoveIsRefusedFromAPaneAPluginAnUnplacedPeerAndAViewOnlyAttach(t *testing.T) {
	s, _ := moveServer(t)
	for _, f := range []*peerFacts{
		{checked: true, pane: true, paneID: "w1:p1"},
		{checked: true, plugin: "p"},
		{checked: true, unknown: true},
	} {
		for _, line := range []string{
			`{"id":"1","cmd":"agent.move","pane":"w1:p1","harness":"codex"}`,
			`{"id":"2","cmd":"agent.move","pane":"w1:p1","harness":"codex","preview":true}`,
		} {
			got := roundTripFacts(t, s, f, line)[0]
			if got.OK || got.Error.Message != MoveRefusal {
				t.Fatalf("%+v %s: %+v", f, line, got)
			}
		}
	}
	if liveCount(s) != 1 {
		t.Fatal("a refused move started an agent")
	}
	got := roundTrip(t, s, `{"id":"a","cmd":"pane.attach","pane":"w1:p1","view_only":true}`,
		`{"id":"b","cmd":"agent.move","pane":"w1:p1","harness":"codex"}`)
	if got[1].OK || got[1].Error.Code != "not_attached" {
		t.Fatalf("view-only move: %+v", got[1])
	}
}

func TestTheBriefKeepsTheNewestWithinItsBudgetAndNoToolRows(t *testing.T) {
	var msgs []message
	for i := 0; i < 60; i++ {
		role := "owner"
		if i%2 == 1 {
			role = "agent"
		}
		msgs = append(msgs, message{Role: role, Text: strings.Repeat("w", 400) + " #" + string(rune('A'+i%26))})
		if i%3 == 0 {
			msgs = append(msgs, message{Role: "agent", Text: "TOOL-ROW", Tool: "Bash"})
		}
	}
	text, used, trimmed := buildBrief("claude", "/work/x", msgs)
	if !trimmed || used >= 60 || used < 10 {
		t.Fatalf("used %d trimmed %v", used, trimmed)
	}
	if strings.Contains(text, "TOOL-ROW") {
		t.Fatal("a tool row is in the brief")
	}
	if len([]rune(text)) > briefRunes+1000 {
		t.Fatalf("brief is %d runes", len([]rune(text)))
	}
	// Oldest first, and the newest message is the last one in.
	last := msgs[len(msgs)-1]
	if !strings.HasSuffix(strings.TrimSpace(text), last.Text) && last.Tool == "" {
		t.Fatalf("the brief does not end with the newest text")
	}
	if !strings.Contains(text, "/work/x") || !strings.Contains(text, "claude") {
		t.Fatalf("the brief names neither the directory nor the old harness:\n%.300s", text)
	}
	if _, used, _ := buildBrief("claude", "/w", []message{{Role: "agent", Text: "", Tool: "Bash"}}); used != 0 {
		t.Fatalf("a brief of tool rows alone used %d messages", used)
	}
}

// A terminal agent that has a screen ready rule gets the brief through the
// ready queue: the move answers at once with when "queued", the brief waits
// while the pane shows no title, and it goes once, after the title shows.
func TestMoveToATerminalAgentHoldsTheBriefUntilItReadsReady(t *testing.T) {
	s, out := moveServer(t)
	set, err := detect.LoadSet("")
	if err != nil {
		t.Fatal(err)
	}
	s.StartManifestTick(set)
	t.Cleanup(s.StopManifestTick)
	wait, open := gate(t)
	// codex reads idle when its terminal title is set (the osc_title_idle rule).
	if err := config.Save(config.Config{Harness: map[string]config.Harness{
		"codex": {Command: "bash", Args: []string{"-c", "stty -echo; echo loading; " + wait + "printf '\\033]0;codex\\007'; cat > " + out}},
	}}); err != nil {
		t.Fatal(err)
	}
	res := moveOf(t, s, `"pane":"w1:p1","harness":"codex"`)
	if res["when"] != "queued" || res["queued"] != float64(1) || !strings.HasPrefix(fmt.Sprint(res["note"]), "queued until codex") {
		t.Fatalf("move = %v, want the brief queued until codex is ready", res)
	}
	time.Sleep(3 * readySettle)
	if b, _ := os.ReadFile(out); len(b) != 0 {
		t.Fatalf("the brief reached a pane that is not ready:\n%s", b)
	}
	open()
	var got string
	for end := time.Now().Add(10 * time.Second); !strings.Contains(got, "A fake reply with no time."); {
		if time.Now().After(end) {
			t.Fatalf("the brief never came after the pane read ready:\n%s", got)
		}
		time.Sleep(20 * time.Millisecond)
		b, _ := os.ReadFile(out)
		got = string(b)
	}
	time.Sleep(3 * readySettle)
	b, _ := os.ReadFile(out)
	if n := strings.Count(string(b), "now push it, please"); n != 1 {
		t.Fatalf("the brief was delivered %d times, want once:\n%s", n, b)
	}
}

// A named web token may not read the chat (ChatRefusal), so it may not move
// it either: the brief is the transcript's text, and preview shows its size.
func TestMoveIsRefusedToANamedToken(t *testing.T) {
	s, _ := moveServer(t)
	hello := `{"id":"h","cmd":"hello","name_from":"token","name":"kid"}`
	for _, line := range []string{
		`{"id":"1","cmd":"agent.move","pane":"w1:p1","harness":"codex"}`,
		`{"id":"2","cmd":"agent.move","pane":"w1:p1","harness":"codex","preview":true}`,
	} {
		got := roundTrip(t, s, hello, line)[1]
		if got.OK || got.Error.Code != "unauthorized" || got.Error.Message != ChatRefusal {
			t.Fatalf("%s: %+v", line, got)
		}
	}
	if liveCount(s) != 1 {
		t.Fatal("a refused move started an agent")
	}
}

// When the new agent cannot take the brief, the move starts nothing: the
// new agent goes and the error says no new agent started (F12B-R-6). Here
// the new agent sits on a trust question, which refuses every prompt.
func TestMoveRemovesTheNewAgentWhenTheBriefIsRefused(t *testing.T) {
	s := readyServer(t)
	id := startFake(t, s, "claude", showTrust(t, `stty -echo; read x; echo "ANSWERED $x"; sleep 10`))
	waitManifest(t, s, id, proto.StateBlocked)
	got := s.sendBrief(&Client{}, "mv", id, "codex", "the brief")
	if got.OK || got.Error.Code != proto.ErrAdapter ||
		!strings.HasPrefix(got.Error.Message, "codex could not take the brief, so no new agent started: ") {
		t.Fatalf("sendBrief = %+v %+v", got, got.Error)
	}
	if p, ok := s.tree.Pane(id); ok && !p.Closed {
		t.Fatalf("the new agent stayed: %+v", p)
	}
}

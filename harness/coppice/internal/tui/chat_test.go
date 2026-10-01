package tui

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/textwidth"
	"github.com/opendaisugi/coppice/internal/tiles"
)

// chatModel is a floor with the foreman in window 1 and a working agent
// behind it, and a chat of two lines and a tool use.
func chatModel() *Model {
	m := tiledModel()
	m.Rows = append(m.Rows, Row{ID: "f", Label: "foreman", Harness: "claude", State: "idle"})
	m.ForemanID = "f"
	m.Tiles = tiles.New(tiles.Focus, 1)
	m.Tiles.Fill("f")
	m.Chat = []ChatMsg{
		{ID: "c1", Role: "owner", Text: "start the import fix"},
		{ID: "c2", Role: "agent", Text: "Started glean-import and told claude-trellis to look."},
		{ID: "c3", Role: "agent", Tool: "Bash ls"},
	}
	return m
}

func TestTheForemanWindowDrawsTheChatAndItsOwnLine(t *testing.T) {
	m := chatModel()
	joined := stripSGR(strings.Join(Render(m, 120, 30), "\n"))
	for _, want := range []string{"you start the import fix", "foreman Started glean-import", "· Bash ls", chatPlaceholder, "Screen"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the chat window lacks %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, headlessPlaceholder) {
		t.Fatalf("the chat window says it takes whole messages for an agent:\n%s", joined)
	}
	if !m.takesLine("f") || m.takesLine("a") {
		t.Fatal("only the foreman's window should take whole lines")
	}
}

func TestScreenInTheHeaderShowsTheForemansTerminal(t *testing.T) {
	m := chatModel()
	m.ChatScreen = true
	joined := stripSGR(strings.Join(Render(m, 120, 30), "\n"))
	if strings.Contains(joined, "start the import fix") || strings.Contains(joined, chatPlaceholder) {
		t.Fatalf("the chat stayed over the terminal:\n%s", joined)
	}
	if !strings.Contains(joined, "Chat") || m.takesLine("f") {
		t.Fatalf("Chat did not name the way back, or the terminal takes a line:\n%s", joined)
	}
}

func TestOnlyTheForemanHasAScreenWord(t *testing.T) {
	m := chatModel()
	if m.toggleWord("f") != screenWord || m.toggleWord("a") != "" || m.toggleWord("") != "" {
		t.Fatalf("toggle words: %q %q", m.toggleWord("f"), m.toggleWord("a"))
	}
	m.ChatScreen = true
	if m.toggleWord("f") != chatWord {
		t.Fatalf("the terminal view offers %q, want %q", m.toggleWord("f"), chatWord)
	}
}

func TestAChatLineIsNeverWiderThanTheWindowAndWrapsWords(t *testing.T) {
	msgs := []ChatMsg{{ID: "c1", Role: "agent", Text: strings.Repeat("word ", 40)}, {Role: "note", Text: "A new foreman started."}}
	rows := chatRows(msgs, 30)
	if len(rows) < 6 {
		t.Fatalf("a long reply did not wrap: %d lines", len(rows))
	}
	for _, r := range rows {
		if w := textwidth.Width(stripSGR(r)); w != 30 {
			t.Fatalf("line is %d cells, want 30: %q", w, r)
		}
	}
}

func TestAChatShowsItsNewestLinesWhenTheWindowIsShort(t *testing.T) {
	m := chatModel()
	got := chatLines(m, 40, 2)
	if len(got) != 2 || !strings.Contains(stripSGR(got[1]), "Bash ls") {
		t.Fatalf("a short window should keep the newest lines: %q", got)
	}
	if empty := chatLines(&Model{}, 40, 3); len(empty) != 1 || !strings.Contains(stripSGR(empty[0]), "No messages yet") {
		t.Fatalf("an empty chat says nothing: %q", empty)
	}
}

func TestAReplyNoWindowShowedIsUnseenUntilTheChatIsDrawn(t *testing.T) {
	m := chatModel()
	if lastReply(m.Chat) != "c2" {
		t.Fatalf("lastReply = %q, want c2: a tool use is not a reply", lastReply(m.Chat))
	}
	if !m.chatUnseen() {
		t.Fatal("a reply was seen before any window drew it")
	}
	Render(m, 120, 30)
	if m.chatUnseen() {
		t.Fatal("the chat window drew the reply but it is still unseen")
	}
	m.Chat = append(m.Chat, ChatMsg{ID: "c4", Role: "agent", Text: "Done."})
	if !m.chatUnseen() {
		t.Fatal("a newer reply is not unseen")
	}
}

func TestDecodeChatKeepsRolesAndToolNames(t *testing.T) {
	got := decodeChat(map[string]any{"messages": []any{
		map[string]any{"id": "c1", "role": "owner", "text": "hi"},
		map[string]any{"id": "c2", "role": "agent", "tool": "Bash"},
		"junk",
	}})
	if len(got) != 2 || got[0].Role != "owner" || got[1].Tool != "Bash" {
		t.Fatalf("decodeChat = %+v", got)
	}
}

// startForemanFloor starts a floor at 140 columns through a recording
// proxy with the floor's foreman running. The foreman's window draws the
// chat and has the keys.
func startForemanFloor(t *testing.T) (string, *recordingProxy, *io.PipeWriter, *signalWriter, chan error) {
	t.Helper()
	s := newTestServer(t)
	foremanConfig(t)
	res := rawCall(t, s.Socket(), "floor.foreman", map[string]any{"harness": "pi"})
	id, _ := res["pane"].(string)
	p := newRecordingProxy(t, s.Socket())
	pw, out, done := startFloor(t, p.sock, 140)
	if !waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(stripSGR(lastFrame(out.String())), chatPlaceholder)
	}) {
		t.Fatalf("the foreman window did not draw its chat line: %q", stripSGR(lastFrame(out.String())))
	}
	return id, p, pw, out, done
}

func TestEnterInTheChatWindowSendsFloorTalkAndNoKeys(t *testing.T) {
	id, p, pw, out, done := startForemanFloor(t)
	_, _ = io.WriteString(pw, "check trellis\r")
	if !waitFor(t, 5*time.Second, func() bool { return len(sentReqs(p, "floor.talk")) == 1 }) {
		t.Fatalf("floor.talk was not sent once: %v", sentReqs(p, "floor.talk"))
	}
	if got := sentReqs(p, "floor.talk")[0]["text"]; got != "check trellis" {
		t.Fatalf("floor.talk text = %v", got)
	}
	for _, cmd := range []string{"pane.send_text", "pane.send_keys", "agent.prompt"} {
		for _, req := range sentReqs(p, cmd) {
			if req["pane"] == id {
				t.Fatalf("the foreman got a raw %s: %v", cmd, req)
			}
		}
	}
	if !waitFor(t, 5*time.Second, func() bool {
		return !strings.Contains(stripSGR(lastFrame(out.String())), headlessMark+"check trellis")
	}) {
		t.Fatalf("the line did not clear after Enter: %q", stripSGR(lastFrame(out.String())))
	}
	quit(t, pw, done)
}

func TestTabInTheChatWindowShowsTheTerminalAndKeysGoThere(t *testing.T) {
	id, p, pw, out, done := startForemanFloor(t)
	_, _ = io.WriteString(pw, "\t")
	if !waitFor(t, 5*time.Second, func() bool {
		f := stripSGR(lastFrame(out.String()))
		return !strings.Contains(f, chatPlaceholder) && strings.Contains(f, "Chat")
	}) {
		t.Fatalf("Tab did not show the terminal: %q", stripSGR(lastFrame(out.String())))
	}
	_, _ = io.WriteString(pw, "x")
	if !waitFor(t, 5*time.Second, func() bool {
		for _, req := range sentReqs(p, "pane.send_text") {
			if req["pane"] == id {
				return true
			}
		}
		return false
	}) {
		t.Fatal("a key did not reach the foreman's terminal in the Screen view")
	}
	quit(t, pw, done)
}

func TestTheForemanFillsAfterTheAgentsThatNeedYouAndNeverBefore(t *testing.T) {
	m := tiledModel()
	m.Rows = append(m.Rows, Row{ID: "f", Label: "foreman", Harness: "claude", State: "idle"})
	m.ForemanID = "f"
	m.Chat = []ChatMsg{{ID: "c2", Role: "agent", Text: "Done."}}
	got := strings.Join(m.startOrder(), ",")
	if got != "a,f,b" {
		t.Fatalf("start order with a reply waiting = %s, want a,f,b: needs you, the foreman, the rest", got)
	}
	m.ChatSeen = "c2"
	if got := strings.Join(m.startOrder(), ","); got != "a,b,f" {
		t.Fatalf("start order with no reply waiting = %s, want a,b,f", got)
	}
}

func TestAReplyWithNoFreeWindowMarksTheForemansRailRowAndMovesNothing(t *testing.T) {
	// Focus layout, one window, held by an agent that needs you.
	m := chatModel()
	m.Tiles = tiles.New(tiles.Focus, 1)
	m.Tiles.Fill("a")
	before := strings.Join(m.Tiles.Slots, ",")
	if !m.replyWaiting("f") {
		t.Fatal("the foreman has a reply and no window, but its row is not marked")
	}
	for _, cols := range []int{100, 140} {
		joined := stripSGR(strings.Join(Render(m, cols, 30), "\n"))
		if cols == 140 && !strings.Contains(joined, replyMark) {
			t.Fatalf("no %q on the foreman's row:\n%s", replyMark, joined)
		}
		if cols == 100 && !strings.Contains(joined, "*f  ") {
			t.Fatalf("the slim row shows no reply mark:\n%s", joined)
		}
	}
	if after := strings.Join(m.Tiles.Slots, ","); after != before {
		t.Fatalf("a reply moved the windows: %s to %s", before, after)
	}
	// A locked layout is the same: nothing moves.
	m.Tiles.Locked = true
	Render(m, 140, 30)
	if after := strings.Join(m.Tiles.Slots, ","); after != before {
		t.Fatalf("a locked layout moved: %s to %s", before, after)
	}
	// Once a window shows it, the mark goes.
	m2 := chatModel()
	if m2.replyWaiting("f") {
		t.Fatal("the foreman is in a window but its row says reply")
	}
}

func TestAServerRefusalShowsInTheChatWindowAndNeverSaysNoMessages(t *testing.T) {
	m := chatModel()
	m.Chat = nil
	m.ChatErr = chatRefused
	joined := stripSGR(strings.Join(Render(m, 140, 30), "\n"))
	if !strings.Contains(joined, "outside a coppice pane") || strings.Contains(joined, "No messages yet") {
		t.Fatalf("the refusal does not show:\n%s", joined)
	}
}

func TestTheFooterNamesTheWayBackToTheChat(t *testing.T) {
	m := chatModel()
	m.Typing = "f"
	m.ChatScreen = true
	if !strings.Contains(footerText(m), "click Chat") {
		t.Fatalf("footer = %q", footerText(m))
	}
	m.ChatScreen = false
	if strings.Contains(footerText(m), "click Chat") {
		t.Fatal("the chat view names a way back")
	}
}

func TestAPastedTabBeforeANewlineIsASpaceInTheChatLine(t *testing.T) {
	id, p, pw, _, done := startForemanFloor(t)
	_, _ = io.WriteString(pw, "a\t\r")
	if !waitFor(t, 5*time.Second, func() bool { return len(sentReqs(p, "floor.talk")) == 1 }) {
		t.Fatalf("floor.talk was not sent once: %v", sentReqs(p, "floor.talk"))
	}
	if got := sentReqs(p, "floor.talk")[0]["text"]; got != "a " {
		t.Fatalf("floor.talk text = %q, want %q: the tab must be a space", got, "a ")
	}
	for _, req := range sentReqs(p, "pane.send_text") {
		if req["pane"] == id {
			t.Fatalf("a pasted tab or newline reached the foreman's terminal: %v", req)
		}
	}
	quit(t, pw, done)
}

package tui

import (
	"fmt"
	"strings"

	"github.com/opendaisugi/coppice/internal/textwidth"
)

// ChatMsg is one line of the chat with the foreman, from floor.chat. Role
// is owner, agent, note, work or past. Tool is set on a tool use line.
type ChatMsg struct {
	ID, Role, Text, Tool string
}

// chatLimit is how many messages one floor.chat read asks for.
const chatLimit = 50

// chatPlaceholder is the foot line of the foreman's window while its
// input line is empty. The words go to the foreman, not to its terminal.
const chatPlaceholder = "Tell the foreman. Type and press Enter."

// chatEmpty is what the foreman's window says before any message.
const chatEmpty = "No messages yet. Type a sentence below. It goes to the foreman."

// chatRefused is what the foreman's window says when the server refuses
// floor.chat: it counts a process inside one of its own panes as a pane,
// and a pane may not read the chat.
const chatRefused = "The server will not show this chat here. This floor runs inside a coppice pane, and a pane may not read the chat or talk to the foreman. Run the floor outside a coppice pane."

// replyMark is the state word of the foreman's rail row while it holds a
// reply and no window shows it.
const replyMark = "reply"

// Screen and Chat are the header words that switch the foreman's window
// between its chat and its terminal.
const (
	screenWord = "Screen"
	chatWord   = "Chat"
)

// chatWindow reports whether pane id is the foreman and its window draws
// the chat. Screen turns it to the foreman's terminal.
func (m *Model) chatWindow(id string) bool {
	return id != "" && id == m.ForemanID && !m.ChatScreen && m.has(id)
}

// takesLine reports whether pane id's window takes whole lines in an input
// line of its own: a headless agent, and the foreman's chat.
func (m *Model) takesLine(id string) bool {
	return m.isHeadless(id) || m.chatWindow(id)
}

// toggleWord is the header word of the foreman's window: Screen while it
// draws the chat, Chat while it draws the terminal. Other windows have none.
func (m *Model) toggleWord(id string) string {
	switch {
	case id == "" || id != m.ForemanID || !m.has(id):
		return ""
	case m.ChatScreen:
		return chatWord
	}
	return screenWord
}

// lastReply is the id of the newest message the foreman wrote in words, or
// "". A tool use is not a reply, and neither is the owner's line.
func lastReply(msgs []ChatMsg) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "agent" && msgs[i].Tool == "" && msgs[i].ID != "" {
			return msgs[i].ID
		}
	}
	return ""
}

// chatUnseen reports whether the foreman has a reply no chat window showed.
func (m *Model) chatUnseen() bool {
	last := lastReply(m.Chat)
	return m.ForemanID != "" && last != "" && last != m.ChatSeen
}

// decodeChat reads the messages of a floor.chat reply.
func decodeChat(res map[string]any) []ChatMsg {
	list, _ := res["messages"].([]any)
	out := make([]ChatMsg, 0, len(list))
	for _, v := range list {
		d, ok := v.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, ChatMsg{ID: str(d, "id"), Role: str(d, "role"), Text: str(d, "text"), Tool: str(d, "tool")})
	}
	return out
}

// chatRows draws the chat as w-cell lines, oldest first: one block per
// message, its speaker on the first line. A tool use is one dim line.
func chatRows(msgs []ChatMsg, w int) []string {
	var out []string
	for _, c := range msgs {
		who := ""
		switch c.Role {
		case "owner":
			who = "you "
		case "agent":
			who = "foreman "
		}
		if c.Tool != "" {
			out = append(out, sgrFaint+textwidth.Pad(textwidth.Truncate("  · "+textwidth.Printable(c.Tool, 0), w), w)+sgrReset)
			continue
		}
		text := textwidth.Printable(c.Text, 0)
		if text == "" {
			continue
		}
		pad := strings.Repeat(" ", textwidth.Width(who))
		room := w - textwidth.Width(who)
		if room < 4 {
			room = 4
		}
		for i, l := range wrapWords(text, room, 1<<10) {
			lead := pad
			if i == 0 {
				lead = who
			}
			line := textwidth.Pad(textwidth.Truncate(lead+l, w), w)
			switch {
			case i == 0 && who != "":
				line = styleWho(who, line)
			case c.Role == "note" || c.Role == "past":
				line = sgrFaint + line + sgrReset
			}
			out = append(out, line)
		}
	}
	return out
}

// styleWho colors the speaker word at the head of a chat line.
func styleWho(who, line string) string {
	if who == "" || !strings.HasPrefix(line, who) {
		return line
	}
	return sgrAccent + who + sgrReset + line[len(who):]
}

// chatLines is the body of the foreman's window: the newest messages that
// fit h lines, w cells wide.
func chatLines(m *Model, w, h int) []string {
	if h <= 0 {
		return nil
	}
	rows := chatRows(m.Chat, w)
	if m.ChatErr != "" && len(m.Chat) == 0 {
		rows = nil
		for _, l := range wrapWords(chatRefused, w, 1<<10) {
			rows = append(rows, sgrAmber+textwidth.Pad(textwidth.Truncate(l, w), w)+sgrReset)
		}
	}
	if len(rows) == 0 {
		rows = []string{sgrFaint + textwidth.Pad(textwidth.Truncate(chatEmpty, w), w) + sgrReset}
	}
	if len(rows) > h {
		rows = rows[len(rows)-h:]
	}
	return rows
}

// toggleScreen turns the foreman's window between its chat and its
// terminal. The caller gives the keys to the window after it.
func (f *floor) toggleScreen() {
	f.m.ChatScreen = !f.m.ChatScreen
}

// readChat asks floor.chat for the newest messages. A failed read keeps the
// last chat, so one slow answer never empties the window. With no foreman
// it reads nothing. A reply no window showed then claims a window.
func (f *floor) readChat() {
	m := f.m
	if m.ForemanID == "" {
		m.Chat, m.ImportLine = nil, ""
		return
	}
	f.readImport()
	res, err := call(f.o.Socket, "floor.chat", map[string]any{"limit": chatLimit})
	if err != nil {
		if strings.Contains(err.Error(), "only the operator") {
			m.ChatErr = chatRefused
		}
		return
	}
	m.Chat = decodeChat(res)
	m.ChatErr = ""
}

// importLine is the bar's words for an import that runs or is paused, from
// a floor.import progress reply; "" when there is none.
func importLine(res map[string]any) string {
	state, _ := res["state"].(string)
	done, _ := res["done"].(float64)
	total, _ := res["total"].(float64)
	switch state {
	case "running":
		return fmt.Sprintf("importing %d/%d", int(done), int(total))
	case "paused":
		return fmt.Sprintf("import paused %d/%d", int(done), int(total))
	}
	return ""
}

// readImport asks floor.import for the progress of the import of old chats.
// It starts no scan. A server without the verb, or a refusal, leaves no line.
func (f *floor) readImport() {
	res, err := call(f.o.Socket, "floor.import", map[string]any{"action": "progress"})
	if err != nil {
		f.m.ImportLine = ""
		return
	}
	f.m.ImportLine = importLine(res)
}

// foremanFirst reports whether pane id is the foreman and holds a reply no
// window showed, so it fills right after the agents that need you.
func (m *Model) foremanFirst(id string) bool {
	return id != "" && id == m.ForemanID && m.chatUnseen() && !m.ChatScreen
}

// replyWaiting reports whether pane id is the foreman with an unseen reply
// and no window to show it. Its rail row says so.
func (m *Model) replyWaiting(id string) bool {
	if !m.foremanFirst(id) {
		return false
	}
	if m.Tiles == nil {
		return true
	}
	_, shown := m.Tiles.SlotOf(id)
	return !shown
}

// setForeman names pane id as the floor's foreman, or none for "". The
// chat and the Screen choice start over. A window that has the keys and
// now takes lines, or no longer does, gets its key reader set again.
func (f *floor) setForeman(id string) {
	m := f.m
	old := m.ForemanID
	m.ForemanID, m.ChatScreen, m.Chat, m.ChatErr = id, false, nil, ""
	if m.Typing != "" && (m.Typing == old || m.Typing == id) {
		f.setTyping(m.Typing)
	}
}

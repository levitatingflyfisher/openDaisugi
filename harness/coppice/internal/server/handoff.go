package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/opendaisugi/coppice/internal/adapters"
	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/proto"
)

// Moving a conversation to another loop (F12b). agent.move reads the
// transcript the way pane.messages does, builds a brief from its tail with
// no tool rows, and starts a new agent of the chosen harness beside the
// old one, in the same directory. The old agent stays until the owner
// closes it.

// MoveRefusal is what a pane, a plugin and an unplaced peer get for
// agent.move.
const MoveRefusal = "only the operator moves an agent's conversation."

const (
	// briefRunes bounds the messages a brief carries, and briefMessages
	// their number. The preface is not counted.
	briefRunes    = 12000
	briefMessages = 40
)

// buildBrief is the first prompt of the agent a conversation moves to: a
// preface, then the newest owner and agent texts that fit the budget,
// oldest first. A row with a tool name is left out whole, even its one-line
// input, so no command or path of a tool reaches the new agent. used is how
// many messages went in, and trimmed is true when older ones were left out.
func buildBrief(from, cwd string, msgs []message) (text string, used int, trimmed bool) {
	var keep []message
	for _, m := range msgs {
		if m.Tool == "" && (m.Role == "owner" || m.Role == "agent") && m.Text != "" {
			keep = append(keep, m)
		}
	}
	runes, first := 0, len(keep)
	for first > 0 && len(keep)-first < briefMessages {
		n := utf8.RuneCountInString(keep[first-1].Text)
		if runes+n > briefRunes {
			break
		}
		runes += n
		first--
	}
	trimmed = first > 0
	keep = keep[first:]
	if len(keep) == 0 {
		return "", 0, trimmed
	}
	var b strings.Builder
	b.WriteString("You take over a conversation from another coding agent (" + oneLine(from, 64) + ") in this directory: " +
		oneLine(cwd, 400) + ".\n")
	b.WriteString("Its latest messages follow, oldest first. Tool output is left out, so read the files you need. ")
	b.WriteString("Do not repeat finished work. When you have read this, say in one line where things stand, then wait for the owner.\n")
	for _, m := range keep {
		b.WriteString("\n" + m.Role + ": " + m.Text + "\n")
	}
	return b.String(), len(keep), trimmed
}

// briefTokens is the size of text in tokens, about: four bytes to a token.
func briefTokens(text string) int { return (len(text) + 3) / 4 }

// conversationTokens is the conversation's size in tokens as the transcript
// counts it: the input read fresh and the cache writes, each once. Cache
// reads are left out, since they count the same context again on every
// turn. ok is false while the transcript counted no usage.
func (s *Server) conversationTokens(p layout.Pane) (int64, bool) {
	v := s.viewFacts(p)
	if v.tokens == nil {
		return 0, false
	}
	n := v.tokens.Fresh + v.tokens.CacheWrite
	return n, n > 0
}

// moveKind is the kind of pane a harness starts as: a terminal agent when
// coppice.toml has a table for it, else a headless one when an adapter is
// built. ok is false when neither holds.
func moveKind(harness string) (kind string, ok bool) {
	if _, found := harnessArgv(harness, nil); found {
		return "pty", true
	}
	if a, found := adapters.Get(harness); found {
		if _, notBuilt := a.(adapters.NotBuilt); !notBuilt {
			return "headless", true
		}
	}
	return "", false
}

// request builds an internal request from a verb name and its params.
func request(id, cmd string, params map[string]any) *proto.Request {
	params["id"], params["cmd"] = id, cmd
	b, _ := json.Marshal(params)
	r := &proto.Request{}
	_ = json.Unmarshal(b, r)
	return r
}

// handleMove answers agent.move {pane, harness, preview?}. With preview it
// says what a move would carry and starts nothing. Without, it starts a
// new agent of harness in the pane's directory and sends it the brief. The
// brief waits in the ready queue of a terminal agent, as any prompt does.
func (s *Server) handleMove(c *Client, r *proto.Request) proto.Response {
	// The brief is the chat's text, so a named token gets what pane.messages
	// gives it: a refusal, preview included.
	if c.tokenNamed() {
		return proto.ErrResp(r.ID, proto.ErrUnauthorized, ChatRefusal)
	}
	id := paneOf(r)
	if c.viewOnlyOn(id) {
		return refuseViewOnly(r, id)
	}
	harness, _ := r.Str("harness")
	if !validHarness(harness) {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, fmt.Sprintf(
			"harness must be a name of at most %d letters, digits, '.', '_' or '-', as in coppice.toml.", harnessMax))
	}
	preview, _ := r.Bool("preview")
	if !preview && s.isClosed() {
		return proto.ErrResp(r.ID, proto.ErrServerClosed, "this server has closed. It cannot start a new pane.")
	}
	rec, ok := s.tree.Pane(id)
	if !ok {
		return proto.ErrResp(r.ID, proto.ErrNoSuchPane, fmt.Sprintf("no pane %q. Run: coppice pane list", id))
	}
	label := s.labelOf(id)
	if rec.Closed {
		return proto.ErrResp(r.ID, proto.ErrPaneClosed, fmt.Sprintf("%s has ended.", label))
	}
	if rec.Harness == "" {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, swapRefusal("loop", label, harnessWord("")))
	}
	if rec.Harness == harness {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, fmt.Sprintf("%s already runs on %s.", label, harness))
	}
	kind, ok := moveKind(harness)
	if !ok {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, fmt.Sprintf(
			"no harness named %q in %s. Run coppice once to write it, or add [harness.%s] with command = %q.",
			harness, config.Path(), harness, harness))
	}
	path, tkind := s.transcriptOf(rec)
	if path == "" {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			fmt.Sprintf("%s (%s) has no transcript to move. Its hook reported none.", label, id))
	}
	msgs, _, err := readMessages(path, tkind)
	if err != nil {
		return proto.ErrResp(r.ID, proto.ErrInternal,
			fmt.Sprintf("cannot read the transcript of %s (%s).", label, id))
	}
	brief, used, trimmed := buildBrief(rec.Harness, rec.Cwd, msgs)
	if used == 0 {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			fmt.Sprintf("%s has no conversation to move yet. Wait for its first reply.", label))
	}
	res := map[string]any{
		"from": rec.Harness, "harness": harness, "tokens": briefTokens(brief),
		"messages": used, "trimmed": trimmed,
	}
	if conv, known := s.conversationTokens(rec); known {
		res["conversation"] = conv
	}
	if preview {
		return proto.OKResp(r.ID, res)
	}

	create := map[string]any{
		"cwd": rec.Cwd, "harness": harness, "kind": kind, "workspace": rec.Workspace, "tab": rec.Tab,
		"cols": rec.Cols, "rows": rec.Rows,
	}
	if rec.TaskID != "" {
		create["task"] = rec.TaskID
	}
	made := s.handlePaneCreate(c, request(r.ID, "pane.create", create))
	if !made.OK {
		return made
	}
	m, _ := made.Result.(map[string]any)
	newID, _ := m["pane"].(string)
	sent := s.sendBrief(c, r.ID, newID, harness, brief)
	if !sent.OK {
		return sent
	}
	res["pane"], res["workspace"], res["tab"] = newID, m["workspace"], m["tab"]
	// "now": the agent has the brief. "typed": a terminal agent with no
	// screen rule got it typed at once, and nothing says it landed.
	res["when"] = "now"
	if kind == "pty" && !s.gated(newID) {
		res["when"] = "typed"
	}
	if p, ok := sent.Result.(map[string]any); ok {
		if q, queued := p["queued"]; queued {
			res["when"], res["queued"], res["note"] = "queued", q, p["note"]
		}
	}
	return proto.OKResp(r.ID, res)
}

// sendBrief sends the brief to the new agent newID. When the agent cannot
// take it, the agent goes and the reply says so: a blank start is not what
// the owner asked for (F12B-R-6). It returns the prompt's reply on success.
func (s *Server) sendBrief(c *Client, id, newID, harness, brief string) proto.Response {
	sent := s.handleAgentPrompt(c, request(id, "agent.prompt", map[string]any{"pane": newID, "text": brief}))
	if sent.OK {
		return sent
	}
	if nrec, found := s.tree.Pane(newID); found {
		s.closeRecord(nrec)
	}
	why := ""
	if sent.Error != nil {
		why = sent.Error.Message
	}
	return proto.ErrResp(id, proto.ErrAdapter, fmt.Sprintf(
		"%s could not take the brief, so no new agent started: %s", harness, endSentence(why)))
}

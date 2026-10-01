package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/opendaisugi/coppice/internal/adapters/sprig"
	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/proto"
)

// message is one message of a transcript or of the chat, as
// pane.messages and floor.chat give it. Role is owner, agent or note.
type message struct {
	ID   string  `json:"id"`
	At   float64 `json:"at"`
	Role string  `json:"role"`
	Text string  `json:"text"`
	Tool string  `json:"tool,omitempty"`
	Pane string  `json:"pane,omitempty"`
	// Read is set on floor.chat's owner lines only: true once that
	// foreman's transcript records the line as the owner's.
	Read *bool `json:"read,omitempty"`
}

// messagesWindow is the most bytes one read of a transcript takes, from
// its end, and the most bytes floor.chat reads from the chat files in all.
// A variable so a test can make it small.
var messagesWindow int64 = 4 << 20

const (
	// messagesDefault and messagesMax bound limit.
	messagesDefault = 100
	messagesMax     = 500
	// messageRunes caps a message's text, and toolRunes a tool's summary.
	messageRunes = 2000
	toolRunes    = 200
	// chatForemen is how many foremen's transcripts floor.chat reads.
	chatForemen = 4
	// chatFilesMax is how many chat files floor.chat reads.
	chatFilesMax = 31
	// messagesEvery is how often the watch looks at each transcript.
	messagesEvery = 500 * time.Millisecond
)

// MessagesRefusal is the message a pane, a plugin or an unplaced peer
// gets for pane.messages of a pane that is not its own.
const MessagesRefusal = "a pane may read only its own messages. The operator reads any pane's."

// ChatRefusal is the message a named web token gets for floor.chat and
// pane.messages.
const ChatRefusal = "the chat is for the owner's own sign-in."

// newForemanNote is the chat line the server writes when it starts a
// foreman after an earlier one.
const newForemanNote = "A new foreman started."

// chatFields is what New sets up for the chat.
type chatFields struct {
	// chatMu orders every read, write and move of the chat files. It is
	// taken after talkMu, never before it.
	chatMu sync.Mutex
	// chatChecked holds each chat file this process checked before its
	// first write to it.
	chatChecked map[string]bool

	// watchOnce starts the watch for transcript growth.
	watchOnce sync.Once

	// chatNoted holds each note floor.chat wrote, so a file that stays
	// unreadable makes one note, not one per read. Under chatMu.
	chatNoted map[string]bool
}

// noteOnce writes text as a note the first time this process sees it.
func (s *Server) noteOnce(text string) {
	s.chatMu.Lock()
	if s.chatNoted == nil {
		s.chatNoted = map[string]bool{}
	}
	seen := s.chatNoted[text]
	s.chatNoted[text] = true
	s.chatMu.Unlock()
	if !seen {
		s.Note(text, "")
	}
}

// RegisterMessageCommands wires up pane.messages and floor.chat.
func (s *Server) RegisterMessageCommands() {
	_ = s.Handle("pane.messages", s.handlePaneMessages)
	_ = s.Handle("floor.chat", s.handleFloorChat)
}

// chatGuard refuses floor.chat and pane.messages for a connection that
// may not read them. ok is false with the refusal when it refuses.
func (s *Server) chatGuard(c *Client, ro role, r *proto.Request) (proto.Response, bool) {
	if r.Cmd != "floor.chat" && r.Cmd != "pane.messages" {
		return proto.Response{}, true
	}
	if r.Cmd == "pane.messages" && ro.noAllow {
		id, _ := r.Str("pane")
		own := ro.pane && !ro.unknown && ro.plugin == "" && ro.paneID != "" && ro.paneID == id
		if !own {
			return proto.ErrResp(r.ID, proto.ErrUnauthorized, MessagesRefusal), false
		}
	}
	if c.tokenNamed() {
		return proto.ErrResp(r.ID, proto.ErrUnauthorized, ChatRefusal), false
	}
	return proto.Response{}, true
}

// SprigRefusal is the message a pane, a plugin or an unplaced peer gets
// for resuming or forking a sprig session that belongs to another pane.
const SprigRefusal = "that sprig session belongs to another pane. Only the operator resumes or forks it."

// sprigGuard refuses a pane.create that resumes, or a pane.fork that
// forks, a sprig session another pane's record names, live or ended, for
// a connection that is not the operator: the new pane would read that
// session tree as its own messages.
func (s *Server) sprigGuard(ro role, r *proto.Request) (proto.Response, bool) {
	if !ro.noAllow {
		return proto.Response{}, true
	}
	switch r.Cmd {
	case "pane.create":
		if h, _ := r.Str("harness"); h != "sprig" {
			return proto.Response{}, true
		}
		argv, _ := r.StrSlice("cmd_argv")
		rid, ok := sprig.ResumeID(argv)
		if !ok {
			return proto.Response{}, true
		}
		path, ok := sprig.TreePath(argv, rid)
		if !ok {
			return proto.Response{}, true
		}
		if owner := s.sprigOwner(path); owner != "" && owner != ro.paneID {
			return proto.ErrResp(r.ID, proto.ErrUnauthorized, SprigRefusal), false
		}
	case "pane.fork":
		id, _ := r.Str("pane")
		if p, ok := s.tree.Pane(id); ok && p.Harness == "sprig" && p.Kind == layout.KindHeadless && id != ro.paneID {
			return proto.ErrResp(r.ID, proto.ErrUnauthorized, SprigRefusal), false
		}
	}
	return proto.Response{}, true
}

// sprigOwner is the pane whose record names the sprig session tree path,
// compared by real path, or "".
func (s *Server) sprigOwner(path string) string {
	want, _ := realTranscriptPath(path)
	for _, p := range s.tree.Panes() {
		if p.Harness != "sprig" || p.Kind != layout.KindHeadless {
			continue
		}
		if tp, ok := sprig.TreePath(p.Argv, p.HarnessSessionID); ok {
			if r, _ := realTranscriptPath(tp); r == want || tp == path {
				return p.ID
			}
		}
	}
	return ""
}

// tokenNamed reports whether the connection's hello said its name came
// from a token and gave a name: a named web token, from the phone server
// in this process or from a web server of its own.
func (c *Client) tokenNamed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nameFixed && c.name != ""
}

// transcriptOf is the file the server reads for pane p, and its kind: a
// sprig pane's own session tree, or the transcript a Claude pane's own
// hook reported last. It is "" when the server reads no file for p.
func (s *Server) transcriptOf(p layout.Pane) (string, transcriptKind) {
	if p.Harness == "sprig" && p.Kind == layout.KindHeadless {
		if path, ok := sprig.TreePath(p.Argv, p.HarnessSessionID); ok {
			return path, kindSprig
		}
		return "", kindSprig
	}
	if pf := s.factsFor(p.ID, false); pf != nil {
		pf.mu.Lock()
		path, kind := pf.path, kindClaude
		if t := pf.files[path]; t != nil {
			kind = t.kind
		}
		pf.mu.Unlock()
		if path != "" {
			return path, kind
		}
	}
	// After a restart the claims, kept in the data dir, still name the
	// pane's transcripts. The newest is the one it read last.
	if claimed := s.claimedBy(p.ID); len(claimed) > 0 {
		return claimed[len(claimed)-1], kindClaude
	}
	return "", kindClaude
}

// limitOf reads limit: absent or below 1 is messagesDefault, and above
// messagesMax is messagesMax.
func limitOf(r *proto.Request) int {
	n, ok := r.Int("limit")
	switch {
	case !ok || n < 1:
		return messagesDefault
	case n > messagesMax:
		return messagesMax
	}
	return n
}

// pick keeps the messages after the one whose id is since, or all when
// since is "" or names none, and of those the newest limit. more is true
// when older messages were left out: by limit, or, with no since found,
// because cut says the read did not reach the start of a file.
func pick(msgs []message, since string, limit int, cut bool) ([]message, bool) {
	from, found := 0, false
	if since != "" {
		for i, m := range msgs {
			if m.ID == since {
				from, found = i+1, true
			}
		}
	}
	out := msgs[from:]
	more := cut && !found
	if len(out) > limit {
		out, more = out[len(out)-limit:], true
	}
	if out == nil {
		out = []message{}
	}
	return out, more
}

func (s *Server) handlePaneMessages(c *Client, r *proto.Request) proto.Response {
	id, _ := r.Str("pane")
	p, ok := s.tree.Pane(id)
	if !ok {
		return proto.ErrResp(r.ID, proto.ErrNoSuchPane, fmt.Sprintf("no pane %q. Run: coppice pane list", id))
	}
	path, kind := s.transcriptOf(p)
	if path == "" {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			fmt.Sprintf("%s (%s) has no transcript to read. Its hook reported none.", s.labelOf(id), id))
	}
	// A pane reading its own messages reads only a file that began after
	// it did: a file it named empty and filled later from an older one is
	// not its own.
	if c != nil && c.roleOf().pane && kind == kindClaude {
		if refusal := s.checkFirstRecord(id, path); refusal != "" {
			return proto.ErrResp(r.ID, proto.ErrUnauthorized, refusal)
		}
	}
	msgs, cut, err := readMessages(path, kind)
	if err != nil {
		return proto.ErrResp(r.ID, proto.ErrInternal,
			fmt.Sprintf("cannot read the transcript of %s (%s).", s.labelOf(id), id))
	}
	since, _ := r.Str("since")
	out, more := pick(msgs, since, limitOf(r), cut)
	return proto.OKResp(r.ID, map[string]any{"messages": out, "source": "transcript", "more": more})
}

// readMessages reads the messages of the last messagesWindow bytes of
// path. cut is true when the read did not start at the start of the file.
// A missing file has no messages.
func readMessages(path string, kind transcriptKind) ([]message, bool, error) {
	if kind == kindClaude {
		if err := verifyReal(path); err != nil {
			return nil, false, err
		}
	}
	lines, start, err := tailLines(path, messagesWindow)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var out []message
	for _, l := range lines {
		var got []message
		if kind == kindSprig {
			got = sprigMessages(l.b)
		} else {
			got = claudeMessages(l.b)
		}
		for i := range got {
			got[i].ID = strconv.FormatInt(l.off, 10)
			if i > 0 {
				got[i].ID += "." + strconv.Itoa(i)
			}
		}
		out = append(out, got...)
	}
	return out, start > 0, nil
}

// fileLine is one complete line of a file and the byte offset it starts
// at.
type fileLine struct {
	off int64
	b   []byte
}

// tailLines reads the complete lines in the last window bytes of path,
// never following a symlink and never reading a file that is not regular.
// A line the read starts inside, and a last line with no line feed, are
// left out. start is where the read began.
func tailLines(path string, window int64) ([]fileLine, int64, error) {
	f, _, _, _, err := openRegular(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := fi.Size()
	start := size - window
	if start < 0 {
		start = 0
	}
	buf := make([]byte, size-start)
	n, err := f.ReadAt(buf, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, 0, err
	}
	buf = buf[:n]
	pos := 0
	if start > 0 {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			return nil, start, nil
		}
		pos = i + 1
	}
	var out []fileLine
	for pos < len(buf) {
		i := bytes.IndexByte(buf[pos:], '\n')
		if i < 0 {
			break
		}
		out = append(out, fileLine{off: start + int64(pos), b: buf[pos : pos+i]})
		pos += i + 1
	}
	return out, start, nil
}

func str(m map[string]any, k string) string {
	v, _ := m[k].(string)
	return v
}

func isTrue(m map[string]any, k string) bool {
	v, _ := m[k].(bool)
	return v
}

// commandRecords lead an owner text that records a command and its
// output, not words the owner typed.
var commandRecords = []string{"<local-command-", "<bash-", "<command-"}

// claudeMessages reads one Claude Code transcript line. Its ids are set
// by the caller.
func claudeMessages(b []byte) []message {
	var e map[string]any
	if json.Unmarshal(b, &e) != nil || e == nil {
		return nil
	}
	typ := str(e, "type")
	if typ != "user" && typ != "assistant" {
		return nil
	}
	if isTrue(e, "isMeta") || isTrue(e, "isSidechain") || isTrue(e, "isCompactSummary") {
		return nil
	}
	if _, ok := e["toolUseResult"]; ok {
		return nil
	}
	msg, _ := e["message"].(map[string]any)
	if msg == nil {
		return nil
	}
	at := rfc3339Seconds(str(e, "timestamp"))
	role := "owner"
	if typ == "assistant" {
		role = "agent"
	}
	var out []message
	add := func(text, tool string) {
		text = cleanMessage(text)
		if role == "owner" {
			for _, p := range commandRecords {
				if strings.HasPrefix(text, p) {
					return
				}
			}
		}
		if text == "" && tool == "" {
			return
		}
		out = append(out, message{At: at, Role: role, Text: text, Tool: tool})
	}
	switch c := msg["content"].(type) {
	case string:
		add(c, "")
	case []any:
		for _, x := range c {
			blk, _ := x.(map[string]any)
			if blk == nil {
				continue
			}
			switch str(blk, "type") {
			case "text":
				add(str(blk, "text"), "")
			case "tool_use":
				if role == "agent" {
					add(toolSummary(blk["input"], ""), oneLine(str(blk, "name"), toolRunes))
				}
			}
		}
	}
	return out
}

// sprigMessages reads one sprig session tree line.
func sprigMessages(b []byte) []message {
	var e map[string]any
	if json.Unmarshal(b, &e) != nil || e == nil {
		return nil
	}
	at, _ := e["ts"].(float64)
	var m message
	switch str(e, "type") {
	case "prompt":
		m = message{Role: "owner", Text: cleanMessage(str(e, "text"))}
	case "assistant":
		m = message{Role: "agent", Text: cleanMessage(str(e, "text"))}
	case "tool_call":
		m = message{Role: "agent", Text: toolSummary(e["input"], str(e, "detail")), Tool: oneLine(str(e, "name"), toolRunes)}
	default:
		return nil
	}
	if m.Text == "" && m.Tool == "" {
		return nil
	}
	m.At = at
	return []message{m}
}

// rfc3339Seconds is an RFC 3339 time as unix seconds, or 0 when it does
// not parse.
func rfc3339Seconds(v string) float64 {
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return 0
	}
	return float64(t.UnixNano()) / 1e9
}

// dropControls drops the control characters of text but the line feed,
// and makes a tab a space.
func dropControls(text string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t':
			return ' '
		case r == '\n':
			return r
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			return -1
		case r == 0x2028 || r == 0x2029 || unicode.Is(unicode.Cf, r):
			// A format character (a bidi mark, a zero-width space) or a
			// line or paragraph separator changes how a line shows.
			return -1
		}
		return r
	}, text)
}

// capRunes cuts text at n runes and marks the cut.
func capRunes(text string, n int) string {
	if utf8.RuneCountInString(text) <= n {
		return text
	}
	i := 0
	for k := range text {
		if i == n {
			return text[:k] + "…"
		}
		i++
	}
	return text
}

// cleanMessage is a message's text: no control characters but the line
// feed, a tab as a space, trimmed, and cut at messageRunes.
func cleanMessage(text string) string {
	return capRunes(strings.TrimSpace(dropControls(text)), messageRunes)
}

// oneLine is text as one line: no control characters, each run of white
// space one space, cut at n runes.
func oneLine(text string, n int) string {
	return capRunes(strings.Join(strings.Fields(dropControls(text)), " "), n)
}

// summaryKeys are the input keys a tool summary reads, in order.
var summaryKeys = []string{"command", "file_path", "path", "pattern", "url", "query", "description"}

// toolSummary is a one-line summary of a tool's input: the first summary
// key that holds a string with words, else detail.
func toolSummary(input any, detail string) string {
	if m, ok := input.(map[string]any); ok {
		for _, k := range summaryKeys {
			if v, ok := m[k].(string); ok {
				if l := oneLine(v, toolRunes); l != "" {
					return l
				}
			}
		}
	}
	return oneLine(detail, toolRunes)
}

// The chat.

// chatDayName is the name of one day's chat file.
var chatDayName = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}\.jsonl$`)

// chatRecord is one line of a chat file. Kind is the foreman log's own:
// user for the owner's words, note for a line the server adds. Date is
// UTC to the millisecond, always the same length, so a line's length and
// so each id after it does not hang on the clock.
type chatRecord struct {
	Kind string `json:"kind"`
	Date string `json:"date"`
	Pane string `json:"pane"`
	Text string `json:"text"`
}

// chatDateLayout is how a chat line writes its date.
const chatDateLayout = "2006-01-02T15:04:05.000Z"

func (s *Server) chatDir() string { return filepath.Join(s.cfg.DataDir, "chat") }

// messagesEvent tells subscribers that new messages may exist.
type messagesEvent struct {
	Event string  `json:"event"`
	Pane  string  `json:"pane"`
	Chat  bool    `json:"chat,omitempty"`
	TS    float64 `json:"ts"`
}

func (s *Server) sendMessagesEvent(pane string, chat bool) {
	s.Broadcast("messages", pane, messagesEvent{Event: "messages", Pane: pane, Chat: chat, TS: nowSeconds()})
}

// writeChat appends one line to today's chat file, the day in UTC, then
// sends a messages event. A write that fails leaves a note and nothing
// else. The caller holds talkMu.
func (s *Server) writeChat(kind, pane, text string) {
	s.chatMu.Lock()
	now := s.clock()
	path := filepath.Join(s.chatDir(), now.UTC().Format("2006-01-02")+".jsonl")
	b, _ := json.Marshal(chatRecord{Kind: kind, Date: now.UTC().Format(chatDateLayout), Pane: pane, Text: text})
	var err error
	wrote := s.writeData(func() { err = s.appendChat(path, append(b, '\n')) })
	s.chatMu.Unlock()
	if !wrote {
		return
	}
	if err != nil {
		s.Note("cannot write the chat file "+path+". The words still go to the foreman.", pane)
		return
	}
	s.sendMessagesEvent(pane, true)
}

// appendChat writes line to the end of path, making its directory 0700
// and the file 0600. Before its first write to a file, it checks the file:
// a damaged one moves aside, and a half-written last line is cut off. The
// caller holds chatMu.
func (s *Server) appendChat(path string, line []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if fi.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	}
	if s.chatChecked == nil {
		s.chatChecked = map[string]bool{}
	}
	if !s.chatChecked[path] {
		if err := s.checkChatFile(path); err != nil {
			return err
		}
		s.chatChecked[path] = true
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	} else if st.Mode().Perm() != 0o600 {
		if err := f.Chmod(0o600); err != nil {
			return err
		}
	}
	if _, err := f.Write(line); err != nil {
		delete(s.chatChecked, path)
		return err
	}
	return nil
}

// checkChatFile reads path as floor.chat does. A damaged file moves aside.
// A last line with no line feed is cut off. A missing file is fine. The
// caller holds chatMu.
func (s *Server) checkChatFile(path string) error {
	_, _, end, damaged, err := readChatFile(path, filepath.Base(path), messagesWindow)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if damaged {
		return s.moveChatAside(path)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if end >= 0 && end < fi.Size() {
		return s.moveTailAside(path, end)
	}
	return nil
}

// moveTailAside moves the bytes of path past end, a line a crash left half
// written, to <path>.broken-<UTC time> (0600), then cuts them from path,
// and leaves one note that names where they went. No byte is lost. The
// caller holds chatMu.
func (s *Server) moveTailAside(path string, end int64) error {
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	tail := make([]byte, fi.Size()-end)
	if _, err := f.ReadAt(tail, end); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	base := path + ".broken-" + s.clock().UTC().Format("20060102T150405Z")
	target := base
	var out *os.File
	for n := 2; ; n++ {
		out, err = os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		target = fmt.Sprintf("%s-%d", base, n)
	}
	_, werr := out.Write(tail)
	if werr == nil {
		// The copy is on disk before any byte leaves the chat file.
		werr = out.Sync()
	}
	if cerr := out.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		err = d.Sync()
		_ = d.Close()
		if err != nil {
			return err
		}
	} else {
		return err
	}
	if err := f.Truncate(end); err != nil {
		return err
	}
	s.Note("A chat file ended in a half-written line. The line moved to "+target+".", "")
	return nil
}

// readChatFile reads the complete lines in the last budget bytes of one
// chat file. used is how many bytes it read, and end is the offset just
// past the last complete line, or -1 when the read holds no line feed.
// damaged is true when a complete line is not a JSON object.
func readChatFile(path, name string, budget int64) (msgs []message, used, end int64, damaged bool, err error) {
	lines, start, err := tailLines(path, budget)
	if err != nil {
		return nil, 0, 0, false, err
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, 0, 0, false, err
	}
	used = fi.Size() - start
	end = -1
	if start == 0 {
		end = 0
	}
	day := strings.TrimSuffix(name, ".jsonl")
	for _, l := range lines {
		end = l.off + int64(len(l.b)) + 1
		var rec map[string]any
		if json.Unmarshal(l.b, &rec) != nil || rec == nil {
			return nil, used, end, true, nil
		}
		role := ""
		switch str(rec, "kind") {
		case "user":
			role = "owner"
		case "note":
			role = "note"
		default:
			continue
		}
		at := rfc3339Seconds(str(rec, "date"))
		text := cleanMessage(str(rec, "text"))
		if text == "" {
			continue
		}
		msgs = append(msgs, message{
			ID: "chat/" + day + "/" + strconv.FormatInt(l.off, 10), At: at, Role: role, Text: text, Pane: str(rec, "pane"),
		})
	}
	return msgs, used, end, false, nil
}

// moveChatAside moves a damaged chat file to <name>.broken-<UTC time>,
// mode 0600, and leaves one note that names where it went. The caller
// holds chatMu.
func (s *Server) moveChatAside(path string) error {
	base := path + ".broken-" + s.clock().UTC().Format("20060102T150405Z")
	target := base
	for n := 2; ; n++ {
		if _, err := os.Lstat(target); errors.Is(err, os.ErrNotExist) {
			break
		}
		target = fmt.Sprintf("%s-%d", base, n)
	}
	if err := os.Rename(path, target); err != nil {
		return err
	}
	delete(s.chatChecked, path)
	_ = os.Chmod(target, 0o600)
	s.Note("A chat file could not be read. It moved to "+target+". The chat starts fresh.", "")
	return nil
}

// readChat reads the newest chat files, at most messagesWindow bytes and
// chatFilesMax files in all, oldest message first. cut is true when it
// left a file or a part of one unread.
func (s *Server) readChat() ([]message, bool, error) {
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	ents, err := os.ReadDir(s.chatDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, errChatUnread{s.chatDir()}
	}
	var names []string
	for _, e := range ents {
		if chatDayName.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	budget, cut := messagesWindow, false
	var files [][]message
	var unread []string
	read := 0
	for i := len(names) - 1; i >= 0; i-- {
		if budget <= 0 || read >= chatFilesMax {
			cut = true
			break
		}
		path := filepath.Join(s.chatDir(), names[i])
		msgs, used, _, damaged, err := readChatFile(path, names[i], budget)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			// Moved nowhere: an I/O error is no damage. The chat shows
			// the rest, and the note says what it left out.
			unread = append(unread, path)
			cut = true
			continue
		}
		if damaged {
			if err := s.moveChatAside(path); err != nil {
				// It could not move aside: leave it, and the chat shows
				// the rest with a note.
				unread = append(unread, path)
				cut = true
			}
			continue
		}
		read++
		if fi, err := os.Lstat(path); err == nil && used < fi.Size() {
			cut = true
		}
		budget -= used
		files = append(files, msgs)
	}
	var out []message
	for i := len(files) - 1; i >= 0; i-- {
		out = append(out, files[i]...)
	}
	if len(unread) > 0 {
		return out, cut, errChatUnread{strings.Join(unread, ", ")}
	}
	return out, cut, nil
}

// errChatUnread names what floor.chat could not read.
type errChatUnread struct{ what string }

func (e errChatUnread) Error() string { return "cannot read " + e.what }

func (s *Server) handleFloorChat(_ *Client, r *proto.Request) proto.Response {
	lines, cut, err := s.readChat()
	var unread errChatUnread
	if errors.As(err, &unread) {
		s.noteOnce("cannot read the chat file " + unread.what + ". The chat shows the rest.")
	} else if err != nil {
		return proto.ErrResp(r.ID, proto.ErrInternal, "cannot read the chat in "+s.chatDir()+".")
	}
	// The foremen the lines name, newest first, then put oldest first.
	// The tracked foreman comes first even when no line names it yet, as
	// for one that floor.foreman or the terminal floor started.
	var foremen []string
	seen := map[string]bool{}
	s.talkMu.Lock()
	tracked := s.trackedForeman()
	s.talkMu.Unlock()
	if tracked != "" {
		seen[tracked] = true
		foremen = append(foremen, tracked)
	}
	for i := len(lines) - 1; i >= 0 && len(foremen) < chatForemen; i-- {
		if id := lines[i].Pane; id != "" && !seen[id] {
			seen[id] = true
			foremen = append(foremen, id)
		}
	}
	var replies []message
	heard := map[string][]message{}
	for i := len(foremen) - 1; i >= 0; i-- {
		id := foremen[i]
		for _, src := range s.chatSources(id) {
			msgs, tcut, err := readMessages(src.path, src.kind)
			if err != nil {
				cut = true
				s.noteOnce("cannot read the foreman's transcript " + src.path + ". The chat shows the rest.")
				continue
			}
			cut = cut || tcut
			for _, m := range msgs {
				if m.Role == "owner" && m.Tool == "" {
					heard[id] = append(heard[id], m)
				}
				if m.Role != "agent" {
					continue
				}
				m.ID, m.Pane = id+"/"+src.n+"/"+m.ID, id
				replies = append(replies, m)
			}
		}
	}
	markRead(lines, heard)
	all := append(lines, replies...)
	sort.SliceStable(all, func(i, j int) bool { return all[i].At < all[j].At })
	since, _ := r.Str("since")
	out, more := pick(all, since, limitOf(r), cut)
	return proto.OKResp(r.ID, map[string]any{"messages": out, "source": "transcript", "more": more})
}

// markRead sets Read on each owner line of the chat. A line is read when
// its foreman's transcript records the same words as the owner's, at or
// after the time the line was written. Each recorded message reads one
// line at most, in order, so an older message with the same words never
// reads a new line, and a line typed while a turn holds stays unread until
// its own turn records it.
func markRead(lines []message, heard map[string][]message) {
	for _, h := range heard {
		sort.SliceStable(h, func(i, j int) bool { return h[i].At < h[j].At })
	}
	next := map[string]int{}
	for i := range lines {
		if lines[i].Role != "owner" {
			continue
		}
		read := false
		h, key := heard[lines[i].Pane], talkText(lines[i].Text)
		for j := next[lines[i].Pane]; j < len(h); j++ {
			if h[j].At >= lines[i].At && talkText(h[j].Text) == key {
				read = true
				next[lines[i].Pane] = j + 1
				break
			}
		}
		lines[i].Read = &read
	}
}

// chatSource is one transcript of a foreman, and its number among the
// pane's transcripts, oldest 0, which keeps message ids apart.
type chatSource struct {
	path string
	kind transcriptKind
	n    string
}

// chatSources is the transcripts floor.chat reads for foreman id: a sprig
// pane's session tree, else every transcript the pane claimed, kept in
// the data dir across a restart, the newest chatForemen of them.
func (s *Server) chatSources(id string) []chatSource {
	if p, ok := s.tree.Pane(id); ok && p.Harness == "sprig" && p.Kind == layout.KindHeadless {
		if path, kind := s.transcriptOf(p); path != "" {
			return []chatSource{{path, kind, "0"}}
		}
		return nil
	}
	claimed := s.claimedBy(id)
	var out []chatSource
	for k := len(claimed) - 1; k >= 0 && len(out) < chatForemen; k-- {
		out = append([]chatSource{{claimed[k], kindClaude, strconv.Itoa(k)}}, out...)
	}
	return out
}

// The watch.

// fileMark is what the watch last saw of a pane's transcript.
type fileMark struct {
	path string
	size int64
	mod  int64
}

// watchMessages starts the watch for transcript growth, once. It takes
// the first look before it returns, so a change after a subscribe is a
// change the watch sees.
func (s *Server) watchMessages() {
	s.watchOnce.Do(func() {
		marks := map[string]fileMark{}
		s.lookMessages(marks, false)
		go func() {
			t := time.NewTicker(messagesEvery)
			defer t.Stop()
			for {
				select {
				case <-s.talkStop:
					return
				case <-t.C:
					s.lookMessages(marks, true)
				}
			}
		}()
	})
}

// lookMessages looks at every pane's transcript once and, with send,
// sends a messages event for each pane whose file changed in size or
// time, or that got or changed its file.
func (s *Server) lookMessages(marks map[string]fileMark, send bool) {
	live := map[string]bool{}
	for _, p := range s.tree.Panes() {
		live[p.ID] = true
		path, _ := s.transcriptOf(p)
		m := fileMark{path: path}
		if path != "" {
			if fi, err := os.Lstat(path); err == nil && fi.Mode().IsRegular() {
				m.size, m.mod = fi.Size(), fi.ModTime().UnixNano()
			}
		}
		old, had := marks[p.ID]
		marks[p.ID] = m
		if send && ((had && old != m) || (!had && path != "")) {
			s.sendMessagesEvent(p.ID, s.isTrackedForeman(p.ID))
		}
	}
	for id := range marks {
		if !live[id] {
			delete(marks, id)
		}
	}
}

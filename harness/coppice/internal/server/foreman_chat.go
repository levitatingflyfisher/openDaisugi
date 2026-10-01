package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/foreman"
	"github.com/opendaisugi/coppice/internal/proto"
)

// ForemanConfig is the [foreman] table as the server uses it.
type ForemanConfig struct {
	// Mode is "pane" (the default) or "chat".
	Mode string
	// CompactorProfile and CompactorModel say where the chat's summaries
	// are made. CompactorModel "" is haiku on the default profile and the
	// profile's own model on any other.
	CompactorProfile string
	CompactorModel   string
	// TurnProfile says where each turn runs: "" is Claude Code, any other
	// value is the name of an operator agent profile.
	TurnProfile string
	// TurnModel is the model of each turn; "" is Claude Code's own default,
	// or the profile's own model.
	TurnModel string
	// TurnTimeout is how long one turn may take; zero means
	// config.ForemanTurnTimeout. A test sets it small.
	TurnTimeout time.Duration
	// ViewMax and ViewMin are the bounds of the chat's view: a batch merge
	// starts over ViewMax and runs down to ViewMin. Zero means the defaults,
	// 128 KiB and 64 KiB. A test sets them small.
	ViewMax, ViewMin int
	// LogDir, when set, is the foreman's log dir. Empty means the rule the
	// coppice foreman command reads by: coppice's own data dir, X/foreman,
	// unless it is the default one, which uses the data home's foreman.
	LogDir string
	// Problem is why the [foreman] table did not read, or "". The server
	// then runs in pane mode and says so in a note.
	Problem string
}

// ForemanConfigOf is the server's view of settings read from coppice.toml.
func ForemanConfigOf(s config.ForemanSettings) ForemanConfig {
	return ForemanConfig{
		Mode: s.Mode, CompactorProfile: s.CompactorProfile, CompactorModel: s.CompactorModel,
		TurnProfile: s.TurnProfile, TurnModel: s.TurnModel,
	}
}

func (f *ForemanConfig) fill() {
	if f.Mode == "" {
		f.Mode = config.ForemanPane
	}
	if f.CompactorProfile == "" {
		f.CompactorProfile = config.DefaultCompactorProfile
	}
	if f.TurnTimeout <= 0 {
		f.TurnTimeout = config.ForemanTurnTimeout
	}
}

// chatMode reports whether the foreman runs as a chat.
func (s *Server) chatMode() bool { return s.cfg.Foreman.Mode == config.ForemanChat }

// foremanLogDir is the foreman's log dir, by the rule the CLI reader uses.
func (s *Server) foremanLogDir() string {
	if s.cfg.Foreman.LogDir != "" {
		return s.cfg.Foreman.LogDir
	}
	return foreman.LogDir(s.cfg.DataDir)
}

// The binaries the chat mode starts. The environment names them for the
// tests; a test never lets them fall back to the real ones.
func envBin(env, def string) string {
	if b := os.Getenv(env); b != "" {
		return b
	}
	return def
}

func sprigBin() string   { return envBin("COPPICE_SPRIG_BIN", "sprig") }
func daisugiBin() string { return envBin("COPPICE_DAISUGI_BIN", "daisugi") }
func claudeBin() string  { return envBin("COPPICE_CLAUDE_BIN", "claude") }

// foremanChat is the foreman's chat while the server runs in chat mode.
type foremanChat struct {
	dir string
	// mu is the chat lock: the lock around every call on chat, and the one
	// Drain is given.
	mu   sync.Mutex
	chat *foreman.Chat
	// problem is why the chat is not open, in plain words, or "".
	problem string
	sum     *foreman.ModelSummarizer
	// snap is the frozen view of the claude runner: the lines that make
	// the snapshot in the system prompt. snapSet says one was frozen.
	snap    []foreman.ViewLine
	snapSet bool

	kick   chan struct{}
	cancel context.CancelFunc
	done   chan struct{}
}

// foremanChatFields is what New sets up for the chat mode.
type foremanChatFields struct {
	// fcMu guards fc and fcShut. fc is the chat once opened.
	fcMu   sync.Mutex
	fc     *foremanChat
	fcShut bool
	// compactRetry is how long the compactor waits after a failed drain.
	compactRetry time.Duration
	// compactTimeout caps one compactor call; zero is the default.
	compactTimeout time.Duration
	// turns is the queue of the chat-mode foreman's turns. talkMu guards it.
	turns turnState
}

// StartForeman opens the foreman's chat when the server runs in chat mode.
// It is idempotent, and floor.talk calls it if the caller did not. The
// floor always starts: a log that cannot be opened is a plain error in a
// note and in each floor.talk reply, and nothing else changes.
func (s *Server) StartForeman() {
	if s.chatMode() {
		s.ensureChat()
	}
}

func (s *Server) ensureChat() *foremanChat {
	s.fcMu.Lock()
	defer s.fcMu.Unlock()
	if s.fc == nil {
		if s.fcShut {
			// A closed server opens no chat. This one holds none.
			dead := &foremanChat{problem: "this server has closed", done: make(chan struct{}), cancel: func() {}}
			close(dead.done)
			return dead
		}
		s.fc = s.openChat()
	}
	return s.fc
}

func (s *Server) openChat() *foremanChat {
	fc := &foremanChat{dir: s.foremanLogDir(), kick: make(chan struct{}, 1), done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	fc.cancel = cancel
	fc.sum = &foreman.ModelSummarizer{UsageDir: fc.dir, Now: s.clock, Call: s.compactorCall, Timeout: s.compactTimeout}
	c, err := foreman.Open(ctx, foreman.Config{Dir: fc.dir, Now: s.clock, ViewMax: s.cfg.Foreman.ViewMax, ViewMin: s.cfg.Foreman.ViewMin})
	if err != nil {
		fc.problem = fmt.Sprintf("The foreman's chat in %s could not be opened: %v. Fix that, then start coppice again. The rest of the floor works.", fc.dir, err)
		s.Note(fc.problem, "")
		cancel()
		close(fc.done)
		return fc
	}
	fc.chat = c
	s.sweepTurnDir()
	if n := c.Notice(); n != "" {
		_, _ = c.Append(ctx, foreman.KindNote, n)
		s.Note(n, "")
	}
	go fc.loop(s, ctx)
	fc.wake()
	s.closeDanglingTurns(fc)
	s.loadWorkers(fc)
	return fc
}

// sweepTurnDir removes the files a turn that did not end left in this data
// dir's turn dir: its system prompt, its view and its socket. It runs when
// the chat opens, before any turn of this server can start. A turn removes
// its own files when it ends, so an old file here is from a crash or a
// kill. A file newer than the turn timeout plus a minute stays, and so do
// a directory and a file of another name.
func (s *Server) sweepTurnDir() {
	dir := s.turnDir()
	cutoff := s.clock().Add(-(s.cfg.Foreman.TurnTimeout + time.Minute))
	for _, pat := range []string{"t-*.system", "t-*.view", "t-*.sock"} {
		names, _ := filepath.Glob(filepath.Join(dir, pat))
		for _, name := range names {
			if fi, err := os.Lstat(name); err == nil && !fi.IsDir() && fi.ModTime().Before(cutoff) {
				_ = os.Remove(name)
			}
		}
	}
}

// wake asks the compactor to look for work.
func (fc *foremanChat) wake() {
	select {
	case fc.kick <- struct{}{}:
	default:
	}
}

// loop builds the nodes that need a model, whenever it is woken, until
// ctx ends. A failed drain leaves the nodes pending; it is tried again
// after a wait or at the next wake.
func (fc *foremanChat) loop(s *Server, ctx context.Context) {
	defer close(fc.done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-fc.kick:
		}
		for {
			err := fc.chat.Drain(ctx, fc.sum, &fc.mu)
			if err == nil || ctx.Err() != nil {
				break
			}
			s.noteOnce("The foreman could not summarize its chat: " + err.Error() +
				". Nothing is lost. It tries again.")
			// A failed node waits by the chat's own schedule (30 s, 2 min,
			// 5 min). Without one, the fixed wait.
			wait := s.compactRetry
			if wait <= 0 {
				wait = time.Minute
			}
			fc.mu.Lock()
			if d, ok := fc.chat.RetryIn(); ok {
				wait = max(d, time.Second)
			}
			fc.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-fc.kick:
			case <-time.After(wait):
			}
		}
	}
}

// closeForemanChat stops the compactor and closes the chat. A call to the
// model in flight is cancelled; its node stays pending for the next start.
// It waits for the compactor to stop, so no node is written after it
// returns.
func (s *Server) closeForemanChat() {
	s.fcMu.Lock()
	s.fcShut = true
	fc := s.fc
	s.fcMu.Unlock()
	if fc == nil {
		return
	}
	fc.cancel()
	<-fc.done
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.chat != nil {
		_ = fc.chat.Close()
		fc.chat = nil
	}
}

var errNoChat = errors.New("the foreman's chat is not open")

// chatAppend logs one message under the chat lock, wakes the compactor
// and tells subscribers there are new messages.
func (s *Server) chatAppend(fc *foremanChat, kind, text string) error {
	fc.mu.Lock()
	if fc.chat == nil {
		fc.mu.Unlock()
		return errNoChat
	}
	_, err := fc.chat.Append(context.Background(), kind, text)
	fc.mu.Unlock()
	fc.wake()
	if err == nil {
		s.sendMessagesEvent("", true)
	}
	return err
}

// compactorDir is where the compactor keeps its fixed system texts, in the
// foreman's turn dir.
func (s *Server) compactorDir() string { return filepath.Join(s.foremanDir(), "compactor") }

// systemFile writes the fixed system text a call uses to a file named by
// its hash and returns the path. The dir is 0700, the file 0600.
func (s *Server) systemFile(system string) (string, error) {
	dir := s.compactorDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(system))
	path := filepath.Join(dir, "sys-"+hex.EncodeToString(sum[:6])+".txt")
	if b, err := os.ReadFile(path); err == nil && string(b) == system {
		return path, nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(system), 0o600); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}

// bundleEntry is one profile in `daisugi agent-profiles bundle`.
type bundleEntry struct {
	Wire    string  `json:"wire"`
	BaseURL *string `json:"base_url"`
	Model   *string `json:"model"`
	Key     *string `json:"key"`
}

// operatorBundle reads the operator's profile table. The bytes live in this
// call only.
func (s *Server) operatorBundle(ctx context.Context) (map[string]bundleEntry, error) {
	data, err := s.bundleBytes(ctx)
	if err != nil {
		return nil, err
	}
	defer clear(data)
	var table map[string]bundleEntry
	if err := json.Unmarshal(data, &table); err != nil {
		return nil, errors.New("daisugi agent-profiles bundle wrote something that is not a profile table")
	}
	return table, nil
}

func firstLine(text string, err error) string {
	// The last line of a process's complaint is the one that names the
	// fault: a traceback, a refusal, a missing login.
	text = strings.TrimSpace(text)
	line := text
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		line = text[i+1:]
	}
	if line == "" && err != nil {
		return err.Error()
	}
	if len(line) > 200 {
		line = line[:200]
	}
	return line
}

// compactorCall is the model call of the compactor: one sprig run that
// offers the model no tool. A profile other than default gets its key on
// descriptor 3, from the operator's bundle, for this call only.
func (s *Server) compactorCall(ctx context.Context, system, prompt string) (string, foreman.CallUsage, error) {
	sysPath, err := s.systemFile(system)
	if err != nil {
		return "", foreman.CallUsage{}, fmt.Errorf("cannot write the compactor's system text: %w", err)
	}
	f := s.cfg.Foreman
	wire, base, profileModel := "claude-code", "", ""
	var key *string
	if f.CompactorProfile != config.DefaultCompactorProfile {
		table, err := s.operatorBundle(ctx)
		if err != nil {
			return "", foreman.CallUsage{}, err
		}
		e, ok := table[f.CompactorProfile]
		if !ok {
			return "", foreman.CallUsage{}, fmt.Errorf("the compactor profile %q is not in the operator's agent profiles (a key variable it names may be unset)", f.CompactorProfile)
		}
		wire, key = e.Wire, e.Key
		if e.BaseURL != nil {
			base = *e.BaseURL
		}
		if e.Model != nil {
			profileModel = *e.Model
		}
	}
	model := config.ForemanSettings{CompactorProfile: f.CompactorProfile, CompactorModel: f.CompactorModel}.CompactorModelFor(profileModel)
	args := []string{"--tools", "none", "--system-file", sysPath, "--json", "--max-turns", "1", "--wire", wire, "--model", model}
	if base == "" {
		base = defaultBase(wire)
	}
	if base != "" {
		args = append(args, "--base-url", base)
	}
	hasKey := key != nil && *key != ""
	var keyBytes []byte
	if hasKey {
		keyBytes = []byte(*key)
		args = append(args, "--key-fd", "3")
	}
	args = append(args, "-")
	if err := os.MkdirAll(s.compactorDir(), 0o700); err != nil {
		return "", foreman.CallUsage{}, err
	}
	stdout, stderr, code, err := runCmd(ctx, cmdOpts{Dir: s.compactorDir(), Name: sprigBin(), Args: args,
		Stdin: []byte(prompt), FD3: keyBytes, HasFD3: hasKey, Env: sprigEnv()})
	clear(keyBytes)
	if ctx.Err() != nil {
		return "", foreman.CallUsage{}, ctx.Err()
	}
	if err != nil || code != 0 {
		return "", foreman.CallUsage{}, fmt.Errorf("the compactor's model call failed: %s", firstLine(stderr, err))
	}
	var out struct {
		Answer string         `json:"answer"`
		Usage  map[string]int `json:"usage"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		return "", foreman.CallUsage{}, errors.New("the compactor's model call gave an answer that is not JSON")
	}
	u := foreman.CallUsage{
		Input: out.Usage["input_tokens"], Output: out.Usage["output_tokens"],
		CacheRead: out.Usage["cache_read_input_tokens"], CacheWrite: out.Usage["cache_creation_input_tokens"],
	}
	return out.Answer, u, nil
}

// limitedBuffer keeps the first max bytes written to it and drops the rest.
type limitedBuffer struct {
	b   *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.b.Len(); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		l.b.Write(p[:room])
	}
	return len(p), nil
}

// talkChat takes the owner's words in chat mode: it logs them as a user
// message, queues the turn that answers it, and returns at once.
func (s *Server) talkChat(r *proto.Request, text string) proto.Response {
	fc := s.ensureChat()
	if fc.chat == nil {
		return proto.ErrResp(r.ID, proto.ErrInternal, fc.problem)
	}
	// One lock across the room check, the append and the queue push: two
	// talks cannot swap places, and a line that is refused is not logged.
	s.talkMu.Lock()
	defer s.talkMu.Unlock()
	if err := s.turnRoom(); err != nil {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, err.Error())
	}
	fc.mu.Lock()
	ids, err := fc.chat.Append(context.Background(), foreman.KindUser, text)
	fc.mu.Unlock()
	fc.wake()
	if err != nil && len(ids) == 0 {
		return proto.ErrResp(r.ID, proto.ErrInternal, "the owner's words could not be logged: "+err.Error())
	}
	s.sendMessagesEvent("", true)
	// The turn answers the last part of a long message; the tail holds all.
	last := ids[len(ids)-1]
	queued := s.queueTurnLocked(fc, last)
	return proto.OKResp(r.ID, map[string]any{
		"mode": "chat", "message": fmt.Sprintf("log/%d", last), "queued": queued, "label": DefaultForemanLabel,
	})
}

// closeDanglingTurns settles the turn records a stop or a crash left. A
// record with no end is closed as failed. When the newest owner line is
// past the newest turn that ended on its own, one note says it was not
// answered. Nothing is retried: a turn hours late could start workers.
func (s *Server) closeDanglingTurns(fc *foremanChat) {
	recs, _ := foreman.ReadTurns(fc.dir)
	ended := map[int64]bool{}
	lastDone := int64(-1)
	for _, r := range recs {
		if r.Status != foreman.TurnRunning {
			ended[r.Msg] = true
			if r.Msg > lastDone {
				lastDone = r.Msg
			}
		}
	}
	now := s.clock().UTC().Format(foreman.DateLayout)
	closed := map[int64]bool{}
	for _, r := range recs {
		if r.Status == foreman.TurnRunning && !ended[r.Msg] {
			ended[r.Msg] = true
			closed[r.Msg] = true
			r.Ended, r.Status = now, foreman.TurnFailed
			_ = foreman.AppendTurn(fc.dir, r)
		}
	}
	fc.mu.Lock()
	var newest int64 = -1
	for i := fc.chat.Count() - 1; i >= 0; i-- {
		if m, err := fc.chat.Message(i); err == nil && m.Kind == foreman.KindUser {
			newest = i
			break
		}
	}
	fc.mu.Unlock()
	if newest > lastDone {
		text := fmt.Sprintf("The foreman did not answer log/%d: coppice stopped first. Say it again.", newest)
		_ = s.chatAppend(fc, foreman.KindNote, text)
		s.Note(text, "")
		// The note is the answer. A record says so, so a later start does
		// not note the same line again.
		// A line whose own running record was just closed is already counted.
		if !closed[newest] {
			_ = foreman.AppendTurn(fc.dir, foreman.TurnRecord{Msg: newest, Started: now, Ended: now, Status: foreman.TurnFailed})
		}
	}
}

// logRole maps a log message kind to the role floor.chat gives it.
func logMessage(i int64, m foreman.Message, read bool) (message, bool) {
	out := message{ID: "log/" + strconv.FormatInt(i, 10), At: rfc3339Seconds(m.Date)}
	switch m.Kind {
	case foreman.KindUser:
		out.Role = "owner"
		out.Read = &read
	case foreman.KindForeman:
		out.Role = "agent"
	case foreman.KindWork:
		out.Role = "work"
	case foreman.KindNote:
		out.Role = "note"
	case foreman.KindPast:
		out.Role = "past"
	case foreman.KindTool:
		// A tool use shows its name only. The result stays in the log.
		first, _, _ := strings.Cut(m.Text, "\n")
		name, _, _ := strings.Cut(first, "(")
		out.Role, out.Tool = "agent", oneLine(name, toolRunes)
		return out, out.Tool != ""
	default:
		return message{}, false
	}
	out.Text = cleanMessage(m.Text)
	return out, out.Text != ""
}

// floorChatLog is floor.chat in chat mode: the messages come from the
// foreman's log, read through the server's own writer under the chat lock,
// so a line shows as soon as it is logged and not when its compaction
// ends. Chat files from before the log began are merged in before its
// first message, with no transcripts. past messages are left out unless the
// request sends past: true. An owner line is read once turns.jsonl shows
// its turn started.
func (s *Server) floorChatLog(r *proto.Request) proto.Response {
	fc := s.ensureChat()
	if fc.chat == nil {
		return proto.ErrResp(r.ID, proto.ErrInternal, fc.problem)
	}
	wantPast, _ := r.Bool("past")
	since, _ := r.Str("since")
	limit := limitOf(r)
	recs, _ := foreman.ReadTurns(fc.dir)
	lastStarted := int64(-1)
	for _, t := range recs {
		if t.Msg > lastStarted {
			lastStarted = t.Msg
		}
	}
	lo, sinceLog := int64(0), false
	if rest, ok := strings.CutPrefix(since, "log/"); ok {
		if n, err := strconv.ParseInt(rest, 10, 64); err == nil && n >= 0 {
			lo, sinceLog = n+1, true
		}
	}
	// A since that names an older chat file line needs the whole log.
	needAll := since != "" && !sinceLog
	fc.mu.Lock()
	count := fc.chat.Count()
	var newest []message
	i := count - 1
	var firstAt float64
	if count > 0 {
		if m, err := fc.chat.Message(0); err == nil {
			firstAt = rfc3339Seconds(m.Date)
		}
	}
	for ; i >= lo && (needAll || len(newest) < limit); i-- {
		m, err := fc.chat.Message(i)
		if err != nil {
			continue
		}
		if m.Kind == foreman.KindPast && !wantPast {
			continue
		}
		if row, ok := logMessage(i, m, lastStarted >= i); ok {
			newest = append(newest, row)
		}
	}
	fc.mu.Unlock()
	reachedStart := i < lo
	for a, b := 0, len(newest)-1; a < b; a, b = a+1, b-1 {
		newest[a], newest[b] = newest[b], newest[a]
	}
	if sinceLog || !reachedStart {
		out, _ := pick(newest, "", limit, false)
		more := !reachedStart && len(newest) >= limit
		return proto.OKResp(r.ID, map[string]any{"messages": out, "source": "log", "more": more})
	}
	old, cut, err := s.readChat()
	var unread errChatUnread
	if errors.As(err, &unread) {
		s.noteOnce("cannot read the chat file " + unread.what + ". The chat shows the rest.")
	}
	var merged []message
	for _, m := range old {
		if firstAt == 0 || m.At < firstAt {
			merged = append(merged, m)
		}
	}
	all := append(merged, newest...)
	out, more := pick(all, since, limit, cut)
	return proto.OKResp(r.ID, map[string]any{"messages": out, "source": "log", "more": more})
}

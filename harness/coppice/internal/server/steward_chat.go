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

	"daisugi-verify/pando"
	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/steward"
)

// StewardConfig is the [steward] table as the server uses it.
type StewardConfig struct {
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
	// TurnProfiles is the chain of turn profiles, in order. When it is
	// set it wins over TurnProfile; see turnChain.
	TurnProfiles []string
	// TurnModel is the one model of every account in the chain; "" is
	// Claude Code's own default, or the profile's own model. A chain of
	// several accounts needs it (the config checks).
	TurnModel string
	// TurnTimeout is how long one turn may take; zero means
	// config.StewardTurnTimeout. A test sets it small.
	TurnTimeout time.Duration
	// ViewMax and ViewMin are the bounds of the chat's view: a batch merge
	// starts over ViewMax and runs down to ViewMin. Zero means the defaults,
	// 128 KiB and 64 KiB. A test sets them small.
	ViewMax, ViewMin int
	// LogDir, when set, is the steward's log dir. Empty means the rule the
	// coppice steward command reads by: coppice's own data dir, X/steward,
	// unless it is the default one, which uses the data home's steward.
	LogDir string
	// Store names the Pando R2 store: <data home>/pando/<store>. Empty
	// means work.
	Store string
	// StoreDir, when set, is the store's dir itself (a test sets it). Else
	// a set LogDir holds the store too, as it did before the memory moved
	// into daisugi; else the store is pando/<Store> under the data home.
	StoreDir string
	// ImportHome, when set, is the home dir import reads old Claude Code and
	// Codex chats under. Empty means the user's home. A test sets it.
	ImportHome string
	// Dir is the steward's working dir from [steward] dir; empty means
	// StewardDir().
	Dir string
	// Problem is why the [steward] table did not read, or "". The server
	// then runs in pane mode and says so in a note.
	Problem string
}

// StewardConfigOf is the server's view of settings read from coppice.toml.
func StewardConfigOf(s config.StewardSettings) StewardConfig {
	return StewardConfig{
		Mode: s.Mode, CompactorProfile: s.CompactorProfile, CompactorModel: s.CompactorModel,
		TurnProfiles: s.TurnProfiles, TurnModel: s.TurnModel, Store: s.Store, Dir: s.Dir,
	}
}

func (f *StewardConfig) fill() {
	if f.Mode == "" {
		f.Mode = config.StewardPane
	}
	if f.CompactorProfile == "" {
		f.CompactorProfile = config.DefaultCompactorProfile
	}
	if f.Store == "" {
		f.Store = pando.DefaultStore
	}
	if f.TurnTimeout <= 0 {
		f.TurnTimeout = config.StewardTurnTimeout
	}
}

// chatMode reports whether the steward runs as a chat.
func (s *Server) chatMode() bool { return s.cfg.Steward.Mode == config.StewardChat }

// stewardLogDir is the steward's log dir, by the rule the CLI reader uses.
func (s *Server) stewardLogDir() string {
	if s.cfg.Steward.LogDir != "" {
		return s.cfg.Steward.LogDir
	}
	return steward.LogDir(s.cfg.DataDir)
}

// stewardStoreDir is the dir of the Pando R2 store the chat is kept in
// (ADR-0023). The chair's own files (turns, calls, failover, the queue) stay
// in stewardLogDir.
func (s *Server) stewardStoreDir() string {
	if d := s.cfg.Steward.StoreDir; d != "" {
		return d
	}
	if s.cfg.Steward.LogDir != "" {
		return s.cfg.Steward.LogDir
	}
	d, err := pando.StoreDir(steward.StoreRoot(s.cfg.DataDir), s.cfg.Steward.Store)
	if err != nil {
		return filepath.Join(steward.StoreRoot(s.cfg.DataDir), "pando", pando.DefaultStore)
	}
	return d
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

// stewardChat is the steward's chat while the server runs in chat mode.
type stewardChat struct {
	// dir is the chair's dir (turns, calls, failover); store is the dir of
	// the Pando R2 store the chat lives in.
	dir, store string
	// kids are the child stores written since the compactor last built
	// their nodes. mu guards it.
	kids []string
	// mu is the chat lock: the lock around every call on chat, and the one
	// Drain is given.
	mu   sync.Mutex
	chat *pando.Chat
	// problem is why the chat is not open, in plain words, or "".
	problem string
	sum     *pando.ModelSummarizer
	// snap is the frozen view of the claude runner: the lines that make
	// the snapshot in the system prompt. snapSet says one was frozen.
	snap    []pando.ViewLine
	snapSet bool
	// unknown holds the lines of <state> for tool calls whose outcome is
	// unknown after a stop or a crash (FM-R-66). The next turn shows them
	// once. unkMu guards it.
	unkMu   sync.Mutex
	unknown []string

	// imp feeds the import queue (FM-R-72). est is the last estimate of what
	// could be imported; estMu guards est, estOK, estRun, estGen and
	// impEvent. estRun is open while a scan runs: one scan at a time, and a
	// caller that wants one joins it (FM-R-83). estGen counts the accepts, so
	// a scan that began before one does not set the estimate.
	imp    *pando.Importer
	estMu  sync.Mutex
	est    []pando.ImportSession
	estOK  bool
	estRun chan struct{}
	estGen int
	// estSig and estAt are the transcript signature before, and the time of,
	// the scan that set est: accept reuses est only while both are fresh.
	estSig string
	estAt  time.Time
	// scans counts the scans that ran; scanHold, when set, runs at the start
	// of each (tests hold one open).
	scans    int
	scanHold func()
	impEvent time.Time

	kick   chan struct{}
	cancel context.CancelFunc
	done   chan struct{}
	// ctx ends when the chat closes; bg counts the jobs that run beside it.
	ctx context.Context
	bg  sync.WaitGroup
}

// stewardChatFields is what New sets up for the chat mode.
type stewardChatFields struct {
	// fcMu guards fc and fcShut. fc is the chat once opened.
	fcMu   sync.Mutex
	fc     *stewardChat
	fcShut bool
	// compactRetry is how long the compactor waits after a failed drain.
	compactRetry time.Duration
	// compactTimeout caps one compactor call; zero is the default.
	compactTimeout time.Duration
	// turns is the queue of the chat-mode steward's turns. talkMu guards it.
	turns turnState
	// importPoll is how often the compactor loop looks at import.json for a
	// change the command made; zero means 5 s. A test sets it small.
	importPoll time.Duration
}

// StartSteward opens the steward's chat when the server runs in chat mode.
// It is idempotent, and floor.talk calls it if the caller did not. The
// floor always starts: a log that cannot be opened is a plain error in a
// note and in each floor.talk reply, and nothing else changes.
func (s *Server) StartSteward() {
	if s.chatMode() {
		s.ensureChat()
	}
}

func (s *Server) ensureChat() *stewardChat {
	fc, fresh := s.ensureChatLocked()
	if fresh {
		// The model check runs a subprocess, so it runs after the chat lock
		// is released: a slow daisugi cannot stall floor.talk (FM-R-58).
		go func() {
			defer fc.bg.Done()
			s.checkModelsAtOpen(fc)
			s.offerImport(fc)
		}()
	}
	return fc
}

// ensureChatLocked opens the chat once. fresh is true for the call that
// opened it with a chat that works; it has then counted one background job
// in fc.bg, which the caller starts and which closeStewardChat waits for.
func (s *Server) ensureChatLocked() (fc *stewardChat, fresh bool) {
	s.fcMu.Lock()
	defer s.fcMu.Unlock()
	if s.fc == nil {
		if s.fcShut {
			// A closed server opens no chat. This one holds none.
			dead := &stewardChat{problem: "this server has closed", done: make(chan struct{}), cancel: func() {}}
			close(dead.done)
			return dead, false
		}
		s.fc = s.openChat()
		if s.fc.problem == "" {
			s.fc.bg.Add(1)
			fresh = true
		}
	}
	return s.fc, fresh
}

func (s *Server) openChat() *stewardChat {
	fc := &stewardChat{dir: s.stewardLogDir(), store: s.stewardStoreDir(), kick: make(chan struct{}, 1), done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	fc.cancel = cancel
	fc.ctx = ctx
	fc.sum = &pando.ModelSummarizer{UsageDir: fc.dir, Now: s.clock, Call: s.compactorCall, Timeout: s.compactTimeout}
	if old := s.oldStoreToMigrate(); old != "" {
		fc.problem = fmt.Sprintf("Your conversation memory is in the old place, %s. Run: daisugi pando migrate. The rest of the floor works.", old)
		s.Note(fc.problem, "")
		cancel()
		close(fc.done)
		return fc
	}
	if n := s.adoptChairFiles(); n != "" {
		s.Note(n, "")
	}
	// The chair's own files (turns, calls, failover, the queue) go in this
	// dir. It is the store dir only when a log_dir holds both, so it is made
	// here: without it every turn and call record would fail to write.
	if err := os.MkdirAll(fc.dir, 0o700); err != nil {
		fc.problem = fmt.Sprintf("The steward's chat in %s could not be opened: %v. Fix that, then start coppice again. The rest of the floor works.", fc.dir, err)
		s.Note(fc.problem, "")
		cancel()
		close(fc.done)
		return fc
	}
	c, err := pando.Open(ctx, pando.Config{Dir: fc.store, Now: s.clock, ViewMax: s.cfg.Steward.ViewMax, ViewMin: s.cfg.Steward.ViewMin,
		OnMovedAside: func(broken, fresh string) error {
			// The chair's call log is in the store dir only when one dir
			// holds both (a set log_dir). Then it moved with the history.
			if filepath.Clean(fc.dir) == filepath.Clean(fc.store) {
				return steward.CarryDanglingCalls(broken, fresh)
			}
			return steward.CarryDanglingCalls(fc.dir, fc.dir)
		}})
	if err != nil {
		fc.problem = fmt.Sprintf("The steward's chat in %s could not be opened: %v. Fix that, then start coppice again. The rest of the floor works.", fc.store, err)
		s.Note(fc.problem, "")
		cancel()
		close(fc.done)
		return fc
	}
	fc.chat = c
	fc.imp = &pando.Importer{Dir: fc.store, Skip: s.importSkip()}
	s.sweepTurnDir()
	if n := c.Notice(); n != "" {
		_, _ = c.Append(ctx, pando.KindNote, n)
		s.Note(n, "")
	}
	go fc.loop(s, ctx)
	// The queue has a loop of its own: a compactor that waits to retry a
	// failed summary must not hold back a queued job.
	fc.bg.Add(1)
	go func() {
		defer fc.bg.Done()
		s.poolLoop(fc, ctx)
	}()
	fc.wake()
	unknown, started, unreadable := s.settleDanglingCalls(fc)
	s.closeDanglingTurns(fc, unknown, started, unreadable)
	s.loadWorkers(fc)
	s.failoverChain(fc)
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
	cutoff := s.clock().Add(-(s.cfg.Steward.TurnTimeout + time.Minute))
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
func (fc *stewardChat) wake() {
	select {
	case fc.kick <- struct{}{}:
	default:
	}
}

// hasPending is true when a node waits to be built.
func (fc *stewardChat) hasPending() bool {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.chat != nil && fc.chat.Pending() > 0
}

// pollEvery is how often the compactor loop looks at import.json. A test
// sets importPoll. The environment variable COPPICE_IMPORT_POLL_MS is a
// test knob for the compare cases: read only when COPPICE_TEST_HOOKS=1, a
// number of ms clamped to 10..5000, or "off" for no poll, so a verb is the
// only thing that wakes the loop (FM-R-84). on is false for no poll.
func (s *Server) pollEvery() (every time.Duration, on bool) {
	if s.importPoll > 0 {
		return s.importPoll, true
	}
	every = 5 * time.Second
	if os.Getenv("COPPICE_TEST_HOOKS") == "1" {
		v := os.Getenv("COPPICE_IMPORT_POLL_MS")
		if v == "off" {
			return 0, false
		}
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			every = time.Duration(min(max(ms, 10), 5000)) * time.Millisecond
		}
	}
	return every, true
}

// loop builds the nodes that need a model, whenever it is woken, until
// ctx ends. A failed drain leaves the nodes pending; it is tried again
// after a wait or at the next wake.
func (fc *stewardChat) loop(s *Server, ctx context.Context) {
	defer close(fc.done)
	var tick <-chan time.Time
	if every, on := s.pollEvery(); on {
		poll := time.NewTicker(every)
		defer poll.Stop()
		tick = poll.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-fc.kick:
		case <-tick:
			// The import command or a click may have changed import.json
			// while the loop was idle, or a turn ended with an import node
			// held back.
			if !fc.importWanted() && !fc.hasPending() {
				continue
			}
		}
		for {
			err := fc.chat.DrainHeldBy(ctx, fc.sum, &fc.mu, s.turnsBusy)
			// Live nodes first: only a drained chat feeds the import
			// queue, one message at a time (FM-R-72).
			if err == nil && ctx.Err() == nil && s.importFeed(fc, ctx) {
				continue
			}
			if err == nil && ctx.Err() == nil {
				err = s.tendKids(fc, ctx)
			}
			if err == nil || ctx.Err() != nil {
				break
			}
			s.noteOnce("The steward could not summarize its chat: " + err.Error() +
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

// closeStewardChat stops the compactor and closes the chat. A call to the
// model in flight is cancelled; its node stays pending for the next start.
// It waits for the compactor to stop, so no node is written after it
// returns.
func (s *Server) closeStewardChat() {
	s.fcMu.Lock()
	s.fcShut = true
	fc := s.fc
	s.fcMu.Unlock()
	if fc == nil {
		return
	}
	fc.cancel()
	<-fc.done
	fc.bg.Wait()
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.chat != nil {
		_ = fc.chat.Close()
		fc.chat = nil
	}
}

var errNoChat = errors.New("the steward's chat is not open")

// chatAppend logs one message under the chat lock, wakes the compactor
// and tells subscribers there are new messages.
func (s *Server) chatAppend(fc *stewardChat, kind, text string) error {
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
// steward's turn dir.
func (s *Server) compactorDir() string { return filepath.Join(s.stewardDir(), "compactor") }

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
func (s *Server) compactorCall(ctx context.Context, system, prompt string) (string, pando.CallUsage, error) {
	sysPath, err := s.systemFile(system)
	if err != nil {
		return "", pando.CallUsage{}, fmt.Errorf("cannot write the compactor's system text: %w", err)
	}
	f := s.cfg.Steward
	wire, base, profileModel := "claude-code", "", ""
	var key *string
	if f.CompactorProfile != config.DefaultCompactorProfile {
		table, err := s.operatorBundle(ctx)
		if err != nil {
			return "", pando.CallUsage{}, err
		}
		e, ok := table[f.CompactorProfile]
		if !ok {
			return "", pando.CallUsage{}, fmt.Errorf("the compactor profile %q is not in the operator's agent profiles (a key variable it names may be unset)", f.CompactorProfile)
		}
		wire, key = e.Wire, e.Key
		if e.BaseURL != nil {
			base = *e.BaseURL
		}
		if e.Model != nil {
			profileModel = *e.Model
		}
	}
	model := config.StewardSettings{CompactorProfile: f.CompactorProfile, CompactorModel: f.CompactorModel}.CompactorModelFor(profileModel)
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
		return "", pando.CallUsage{}, err
	}
	wrap, werr := s.fenceCmd("sprig", s.compactorDir(), []string{s.compactorDir()}, hasKey)
	if werr != nil {
		clear(keyBytes)
		return "", pando.CallUsage{}, werr
	}
	stdout, stderr, code, err := runCmd(ctx, cmdOpts{Dir: s.compactorDir(), Name: sprigBin(), Args: args,
		Stdin: []byte(prompt), FD3: keyBytes, HasFD3: hasKey, Env: sprigEnv(), Wrap: wrap})
	clear(keyBytes)
	if ctx.Err() != nil {
		return "", pando.CallUsage{}, ctx.Err()
	}
	if err != nil {
		return "", pando.CallUsage{}, fmt.Errorf("the compactor's model call failed: %s", firstLine(stderr, err))
	}
	if code != 0 {
		// The run started and failed. The bounded text of both streams goes
		// to the classifier only (an outage delays, anything else is clipped
		// after one retry: FM-R-64) and is never shown.
		return "", pando.CallUsage{}, &pando.CallError{
			Msg:    "the compactor's model call failed: " + firstLine(stderr, nil),
			Detail: failDetail(stderr, stdout), Stderr: steward.Tail(stderr, steward.ClassifyBytes), Ran: true,
		}
	}
	var out struct {
		Answer     string         `json:"answer"`
		Usage      map[string]int `json:"usage"`
		Stop       string         `json:"stop"`
		StopReason string         `json:"stop_reason"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		return "", pando.CallUsage{}, &pando.CallError{
			Msg: "the compactor's model call gave an answer that is not JSON", Ran: true,
		}
	}
	u := pando.CallUsage{
		Input: out.Usage["input_tokens"], Output: out.Usage["output_tokens"],
		CacheRead: out.Usage["cache_read_input_tokens"], CacheWrite: out.Usage["cache_creation_input_tokens"],
		Stop: out.Stop,
	}
	if u.Stop == "" {
		u.Stop = out.StopReason
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
	act, _ := actParam(r)
	if act != "" {
		// A repeat of a known act returns the first answer and queues no
		// turn (FM-R-65). The check comes before the room check: a repeat
		// is not a new turn.
		fc.mu.Lock()
		first, known := fc.chat.ActMsg(act)
		same := known && fc.chat.ActMatches(act, text)
		fc.mu.Unlock()
		if known && !same {
			// An act id is the sender's key for one sentence. Other words
			// under a known id would be dropped as a repeat, with no word
			// to the sender (FM-R-65).
			return proto.ErrResp(r.ID, proto.ErrBadRequest, fmt.Sprintf("act %s was used for other words. Send a new id for a new sentence.", act))
		}
		if known {
			queued := 0
			for k, m := range s.turns.queue {
				if m == first {
					queued = k + 1
				}
			}
			return proto.OKResp(r.ID, map[string]any{
				"mode": "chat", "message": fmt.Sprintf("log/%d", first), "queued": queued, "label": DefaultStewardLabel, "already": true,
			})
		}
	}
	if err := s.turnRoom(); err != nil {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, err.Error())
	}
	fc.mu.Lock()
	ids, err := fc.chat.AppendAct(context.Background(), pando.KindUser, text, act)
	fc.mu.Unlock()
	fc.wake()
	if err != nil && len(ids) == 0 {
		return proto.ErrResp(r.ID, proto.ErrInternal, "the owner's words could not be logged: "+err.Error())
	}
	s.wakePause()
	s.sendMessagesEvent("", true)
	// The turn answers the last part of a long message; the tail holds all.
	last := ids[len(ids)-1]
	if len(ids) > 1 {
		if s.turns.parts == nil {
			s.turns.parts = map[int64][]int64{}
		}
		s.turns.parts[last] = ids
	}
	queued := s.queueTurnLocked(fc, last)
	return proto.OKResp(r.ID, map[string]any{
		"mode": "chat", "message": fmt.Sprintf("log/%d", last), "queued": queued, "label": DefaultStewardLabel,
	})
}

// closeDanglingTurns settles the turn records a stop or a crash left. A
// record with no end is closed as failed. When the newest owner line is
// past the newest turn that ended on its own, one note says it was not
// answered. Nothing is retried: a turn hours late could start workers.
func (s *Server) closeDanglingTurns(fc *stewardChat, unknown, started map[int64]bool, unreadable bool) {
	recs, _ := steward.ReadTurns(fc.dir)
	ended := map[int64]bool{}
	lastDone := int64(-1)
	for _, r := range recs {
		if r.Status != steward.TurnRunning {
			ended[r.Msg] = true
			if r.Msg > lastDone {
				lastDone = r.Msg
			}
			// A message the turn heard at a tool call is not unanswered.
			for _, id := range r.Mid {
				if id > lastDone {
					lastDone = id
				}
			}
		}
	}
	now := s.clock().UTC().Format(pando.DateLayout)
	closed := map[int64]bool{}
	for _, r := range recs {
		if r.Status == steward.TurnRunning && !ended[r.Msg] {
			ended[r.Msg] = true
			closed[r.Msg] = true
			r.Ended, r.Status = now, steward.TurnFailed
			_ = steward.AppendTurn(fc.dir, r)
		}
	}
	fc.mu.Lock()
	var newest int64 = -1
	for i := fc.chat.Count() - 1; i >= 0; i-- {
		if m, err := fc.chat.Message(i); err == nil && m.Kind == pando.KindUser {
			newest = i
			break
		}
	}
	fc.mu.Unlock()
	if newest > lastDone {
		text := fmt.Sprintf("The steward did not answer log/%d: coppice stopped first. Say it again.", newest)
		if started[newest] {
			// A tool call of this turn started, and may have ended before the
			// crash: a repeat could run it twice (FM-R-66).
			text = fmt.Sprintf("The steward did not answer log/%d: coppice stopped first. A tool call ran in this turn. Check the workers before you say it again.", newest)
		}
		if unreadable {
			// The call log could not be read: any call may have run.
			text = fmt.Sprintf("The steward did not finish log/%d: coppice stopped first. Whether a tool call ran is unknown, so the line is not run again. Check the workers first.", newest)
		}
		if unknown[newest] {
			// A tool call of this turn has an unknown outcome: no invitation
			// to repeat it (FM-R-66).
			text = fmt.Sprintf("The steward did not finish log/%d: coppice stopped first, while a tool call ran. Its outcome is unknown, so it is not run again. Check the workers first.", newest)
		}
		_ = s.chatAppend(fc, pando.KindNote, text)
		s.Note(text, "")
		// The note is the answer. A record says so, so a later start does
		// not note the same line again.
		// A line whose own running record was just closed is already counted.
		if !closed[newest] {
			_ = steward.AppendTurn(fc.dir, steward.TurnRecord{Msg: newest, Started: now, Ended: now, Status: steward.TurnFailed})
		}
	}
}

// logRole maps a log message kind to the role floor.chat gives it.
func logMessage(i int64, m pando.Message, read bool) (message, bool) {
	out := message{ID: "log/" + strconv.FormatInt(i, 10), At: rfc3339Seconds(m.Date)}
	switch m.Kind {
	case pando.KindUser:
		out.Role = "owner"
		out.Read = &read
	case pando.KindSteward:
		out.Role = "agent"
	case pando.KindWork:
		out.Role = "work"
	case pando.KindNote:
		out.Role = "note"
	case pando.KindPast:
		out.Role = "past"
	case pando.KindTool:
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
// steward's log, read through the server's own writer under the chat lock,
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
	recs, _ := steward.ReadTurns(fc.dir)
	s.talkMu.Lock()
	delivered := map[int64]bool{}
	for id := range s.turns.delivered {
		delivered[id] = true
	}
	// A restart forgets the memory: the end record of a turn keeps the
	// messages it heard.
	for _, t := range recs {
		for _, id := range t.Mid {
			delivered[id] = true
		}
	}
	queued := map[int64]bool{}
	for _, id := range s.turns.queue {
		queued[id] = true
	}
	s.talkMu.Unlock()
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
		if m.Kind == pando.KindPast && !wantPast {
			continue
		}
		if row, ok := logMessage(i, m, lastStarted >= i || delivered[i]); ok {
			if m.Kind == pando.KindUser {
				switch {
				case delivered[i]:
					row.Delivery = "delivered mid-turn"
				case queued[i]:
					row.Delivery = "queued"
				}
			}
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

// oldStoreToMigrate is the old log dir when the chat still lives there and
// `daisugi pando migrate` has not run. The server then opens no chat.
func (s *Server) oldStoreToMigrate() string {
	if s.cfg.Steward.StoreDir != "" || s.cfg.Steward.LogDir != "" {
		return ""
	}
	return steward.OldStore(steward.LogDir(s.cfg.DataDir), s.stewardStoreDir())
}

// adoptChairFiles copies the chair's files out of a dir `daisugi pando
// migrate` moved aside, once.
func (s *Server) adoptChairFiles() string {
	if s.cfg.Steward.StoreDir != "" || s.cfg.Steward.LogDir != "" {
		return ""
	}
	return steward.AdoptChairFiles(s.stewardLogDir())
}

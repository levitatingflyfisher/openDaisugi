// Package server is the coppice daemon: one unix socket, one uid, one operator,
// one instance. It owns the pane tree and the merged state, and it hands both
// to command handlers registered by name.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/pane"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/state"
	"github.com/opendaisugi/coppice/internal/web"
)

// ErrAlreadyRunning is what AcquireStartLock returns when another server holds
// the lock. It is a refusal, never a takeover: the running server owns the
// data directory and the socket.
var ErrAlreadyRunning = errors.New("another coppice server holds the start lock")

// ErrAlreadyListening is what Listen returns when this Server has already
// bound a socket. Binding a second one and overwriting the stored listener
// would leak the first: nothing ever closes it, since Close only ever knows
// about whatever net.Listener is currently stored.
var ErrAlreadyListening = errors.New("this server is already listening")

// writeTeardownDeadline bounds how long a stuck Send can hold a connection's
// teardown open. A peer that has stopped reading must not be able to pin a
// goroutine, and the file descriptor behind it, forever.
//
// It is armed twice, not once. The moment a connection starts tearing down,
// teardown itself arms it on whatever write may already be in flight, which
// is what catches a handler that was already blocked writing to a peer that
// never reads - the case TestATeardownDoesNotWaitForAHandlerStuckWritingToADeadPeer
// builds. From then on, teardownWriter arms it again, fresh, immediately
// before every later Write on that connection - which is what catches a
// handler that has not written anything yet: judging that handler's
// eventual write against a deadline computed once, when teardown began,
// would fail it the moment it finally tries to send, even though nothing
// was ever actually stuck. See handlerTeardownWait and peerCloseHandlerWait
// for the two different bounds that decide how long a handler is given to
// reach that write at all.
const writeTeardownDeadline = time.Second

// handlerTeardownWait bounds how long Close's own teardown of a connection
// waits for in-flight handlers to finish before giving up and proceeding
// anyway. An operator-requested coppice server stop should return quickly;
// see peerCloseHandlerWait for the much longer bound an ordinary connection
// gets when it ends on its own, server still up.
const handlerTeardownWait = 2 * time.Second

// peerCloseHandlerWait bounds how long an ORDINARY connection's teardown -
// the server stays up, only this one connection is ending, a --stdio
// caller's half-close after its one request among the ways that happens -
// waits for its own in-flight handlers to finish.
//
// It is generous because every write those handlers attempt is bounded by
// writeTeardownDeadline anyway, armed fresh at the moment of that attempt: a
// handler stuck on a peer that never reads still fails within about a
// second of actually trying to write, no matter how long this bound is.
// What this bound protects is a handler that has not written anything yet -
// still doing its own work, unaware the connection is going - which must
// not be cut off by a bound sized for how fast an operator-requested server
// stop should return. 130s comfortably outlasts agent.wait's and
// pane.wait_output's 120s documented default timeout, though both already
// return within milliseconds of a connection tearing down, since both watch
// c.Dead().
//
// The cost: a handler blocked on something with no deadline of its own,
// that never watches c.Dead() and never writes at all, pins its own
// goroutine, the connection's dispatch goroutine, its Client pump and the
// connection's file descriptor for up to the full 130s instead of 2s -
// Serve's own defer conn.Close() runs only after ServeConn returns, and
// ServeConn does not return until this wait ends or the handler does.
const peerCloseHandlerWait = 130 * time.Second

// Config is the whole of a server's configuration. There is deliberately no
// "skip the uid check" field: every accepted connection is checked, and no
// future caller can turn that off by setting a struct member.
type Config struct {
	SocketPath string
	DataDir    string
	// ForemanDir is where the floor's foreman works. Empty means
	// ForemanDir(), under XDG_STATE_HOME.
	ForemanDir string
	// ForemanWait is how long the talk queue waits on the foreman before
	// it writes a note. Zero means DefaultForemanWait.
	ForemanWait time.Duration
	// GateRoot is the gate directory agent.allow and agent.deny answer
	// in. Empty means the gate directory beside DataDir.
	GateRoot string
	// HookHome is the home whose harness settings floor.facts reads to
	// tell whether a daisugi gate hook is installed. Empty means the
	// user's home directory.
	HookHome string
	// StartDir is where a pane.create with no cwd of its own falls back to
	// once no project, pinned or recent, can supply one either. Empty
	// means the directory this process itself was started in, read once
	// with os.Getwd in New.
	StartDir string
	// Voice says whether the server runs a voice server of its own. The
	// zero value starts none until voice.start asks.
	Voice VoiceConfig
}

// EventQueue is how many unsent events one client may fall behind by. A PWA on
// a stalled tailnet must not be able to hold up ApplyState for every pane,
// including the pane.report_state response a gate hook is blocking on.
const EventQueue = 256

// Client is one connection. Attached holds the per-client frame baselines, and
// its non-emptiness is what makes a client an operator: only a client watching
// a pane may speak for the human in front of it.
//
// Events go through a bounded queue drained by one goroutine. Responses go
// straight to Enc, so a wedged event stream never delays an answer the caller
// is waiting on.
type Client struct {
	Enc      *proto.Encoder
	mu       sync.Mutex
	Attached map[string]*pane.FrameState
	Subs     map[string]bool // event kinds this client subscribed to
	SubPanes map[string]bool // "*" means every pane

	// flows holds one paneFlow per attached pane, under mu, beside the
	// FrameState in Attached. Frames do not go through the events queue.
	// Each flow has its own bounded buffer and its own drain goroutine, so
	// a client that reads frames slowly loses frames for that pane and
	// hears about it, instead of losing the whole connection. See
	// attach.go.
	flows map[string]*paneFlow

	// autoPanes tracks which SubPanes entries pane.attach added on its own,
	// as opposed to an explicit events.subscribe naming that pane (or "*").
	// pane.detach uses this to undo exactly the subscription attach itself
	// created, and leaves alone whatever a client asked for on its own -
	// see handleDetach and handleSubscribe in attach.go.
	autoPanes map[string]bool

	// viewOnly marks, under mu, every attached pane this connection may
	// only watch. pane.send_text, pane.send_keys, pane.run, pane.resize
	// and agent.prompt refuse a pane marked here. A plain pane.attach, a
	// pane.detach, or the frame pump forgetting the pane clears the mark.
	viewOnly map[string]bool

	// tearing is 0 until this client's connection starts tearing down, then
	// 1 for the rest of its life. teardownWriter reads it before every
	// Write: once nonzero, each write arms its own fresh write deadline
	// immediately before attempting the write, rather than every future
	// write being judged against one deadline computed when teardown began.
	// Accessed only via the atomic package - it is set from ServeConn's
	// teardown goroutine and read from whatever goroutine is calling Send
	// at the time, which is never the same one.
	tearing int32

	// framing is the wire shape this connection speaks, as a proto.Framing.
	// The read loop sets it the moment a JSON-RPC line arrives, and the pump
	// and every reply read it at write time. It only ever moves from Native
	// to JSONRPC.
	framing atomic.Int32

	// facts is what the kernel said about the peer when Serve accepted
	// it. helloRole and helloPane are what the client said in hello. All
	// three are read under mu through roleOf.
	facts     peerFacts
	helloRole string
	helloPane string
	// helloPlugin is the plugin a hello named. It only narrows a
	// connection the kernel did not place as a plugin.
	helloPlugin string
	// name is the name a hello gave, and nameFrom is token or socket.
	// nameFixed is true once the phone server named the connection from
	// its token, named or not. All three sit under mu. See Who.
	name      string
	nameFrom  string
	nameFixed bool

	events chan any
	dead   chan struct{}
	once   sync.Once
}

// Framing reports the wire shape this connection speaks.
func (c *Client) Framing() proto.Framing { return proto.Framing(c.framing.Load()) }

func (c *Client) setFraming(f proto.Framing) { c.framing.Store(int32(f)) }

// send writes one reply in the given framing. Every reply on a connection
// goes through here, so no reply can leave in the wrong shape.
func (c *Client) send(r proto.Response, f proto.Framing) error {
	return c.Enc.SendBytes(proto.EncodeResponse(r, f))
}

// teardownWriter arms a fresh write deadline immediately before every Write,
// once its client has started tearing down. Before that, and for a writer
// that does not support deadlines - an io.Pipe in a test, say - it writes
// straight through with no deadline at all.
type teardownWriter struct {
	w       io.Writer
	tearing *int32
}

func (t *teardownWriter) Write(p []byte) (int, error) {
	if atomic.LoadInt32(t.tearing) != 0 {
		if dl, ok := t.w.(interface{ SetWriteDeadline(time.Time) error }); ok {
			_ = dl.SetWriteDeadline(time.Now().Add(writeTeardownDeadline))
		}
	}
	return t.w.Write(p)
}

func newClient(w io.Writer) *Client {
	c := &Client{
		Attached:  map[string]*pane.FrameState{},
		flows:     map[string]*paneFlow{},
		Subs:      map[string]bool{},
		SubPanes:  map[string]bool{},
		autoPanes: map[string]bool{},
		viewOnly:  map[string]bool{},
		events:    make(chan any, EventQueue),
		dead:      make(chan struct{}),
	}
	c.Enc = proto.NewEncoder(&teardownWriter{w: w, tearing: &c.tearing})
	go c.pump()
	return c
}

// pump is the only goroutine that writes this client's events.
func (c *Client) pump() {
	for {
		select {
		case <-c.dead:
			return
		case v := <-c.events:
			b := proto.EncodeEvent(v, c.Framing())
			if b == nil || c.Enc.SendBytes(b) != nil {
				c.Drop()
				return
			}
		}
	}
}

// Emit queues one event. A client that has fallen EventQueue events behind is
// disconnected rather than allowed to stall the server: dropping one client is
// better than freezing every pane. Frames never come through here. They have
// their own per-pane buffer, see paneFlow in attach.go.
func (c *Client) Emit(v any) {
	select {
	case <-c.dead:
	case c.events <- v:
	default:
		c.Drop()
	}
}

// Drop marks the client gone. It is safe to call more than once.
func (c *Client) Drop() { c.once.Do(func() { close(c.dead) }) }

// Dead reports whether this client has been dropped.
func (c *Client) Dead() bool {
	select {
	case <-c.dead:
		return true
	default:
		return false
	}
}

// viewOnlyOn reports whether this client holds a view-only attach on
// pane id. A nil client holds none.
func (c *Client) viewOnlyOn(id string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.viewOnly[id]
}

// refuseViewOnly is the not_attached reply a write on a view-only pane
// gets. It names the plain attach that lifts the mark.
func refuseViewOnly(r *proto.Request, id string) proto.Response {
	return proto.ErrResp(r.ID, proto.ErrNotAttached,
		fmt.Sprintf("this connection is attached to %s view only. Run: coppice attach %s", id, id))
}

// Operator reports whether this client is attached to the given pane. Master
// spec 3.1 lets only an attached client speak with source operator.
func (c *Client) Operator(paneID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.Attached[paneID]
	return ok
}

type Handler func(c *Client, r *proto.Request) proto.Response

// RestoreReport is what a restart actually achieved. It is
// declared here because handleStatus returns it and Server owns the field.
type RestoreReport struct {
	Panes         int      `json:"panes"`
	AlreadyClosed int      `json:"already_closed"`
	Resumed       int      `json:"resumed"`
	MarkedDone    int      `json:"marked_done"`
	MarkedUnknown int      `json:"marked_unknown"`
	Notes         []string `json:"notes"`
}

// Server declares every field it owns in one place, rather than letting
// the struct grow across files.
type Server struct {
	// inflight counts in-flight handler goroutines, across every connection.
	// It is accessed only via the atomic package (Close's bounded wait polls
	// it from a goroutine that never holds mu, so a mutex would not fit) and
	// is placed first because a 64-bit word must be 64-bit aligned for
	// atomic access on 32-bit platforms, and only a struct's first word is
	// guaranteed that.
	inflight int64

	cfg      Config
	tree     *layout.Tree
	states   *state.Store
	started  time.Time
	peerUID  func(*net.UnixConn) (uint32, error)
	handlers map[string]Handler
	// peerPID and procStat are the kernel lookups Serve places a peer
	// with. They are fields so a test can replace them.
	peerPID  func(*net.UnixConn) (int, error)
	procStat func(pid int) (ppid, sid int, err error)
	// listPIDs lists every live pid. A test replaces it.
	listPIDs func() ([]int, error)

	// asks holds, under asksMu, every ask each pane reported with its
	// deadline and tier, so agent.allow can answer any live one.
	asksMu sync.Mutex
	asks   map[string]map[string]notedAsk

	// holds maps each pane whose ask a foreman hears first to that hold.
	// surfaced holds, per pane, the ask ids that went to the operator, so
	// none is held twice. holdFor is how long a hold lasts; a test
	// shortens it. All three are under holdsMu.
	holdsMu  sync.Mutex
	holds    map[string]*hold
	surfaced map[string][]string
	holdFor  time.Duration
	// nextHold numbers the holds. typed marks each pane someone typed into
	// since it last went idle. Both are under holdsMu.
	nextHold int
	typed    map[string]bool
	// open marks each pane whose last input did not end with Enter.
	open map[string]bool

	// clockSkew is added to the wall clock the process source reads, in
	// nanoseconds. It is zero in a real server. A test moves it forward to
	// pass a quiet window without sleeping through it.
	clockSkew atomic.Int64

	// children holds the subagents of each pane, under childMu.
	childMu  sync.Mutex
	children map[string]map[string]*child

	// notes holds the last notesKept notes, oldest first, under notesMu.
	notesMu sync.Mutex
	notes   []note

	// plugPIDs maps the pid of each policy to its plugin id, while the
	// policy runs and after it exits until no process is left in its
	// session. plugExited holds the pids of policies that exited. plugSpecs
	// maps each enabled plugin id to the verbs and events it holds. All
	// three are under plugMu.
	plugMu     sync.RWMutex
	plugPIDs   map[int]string
	plugExited map[int]bool
	plugSpecs  map[string]PluginSpec

	liveMu sync.RWMutex
	live   map[string]*LivePane

	// resumeMu guards resuming: the set of pane ids a pane.resume call
	// currently has claimed. handlePaneResume claims an id before it
	// reads the tree, and holds the claim for the whole call, so two
	// concurrent pane.resume calls for the same id cannot both spawn a
	// new pane from it.
	resumeMu sync.Mutex
	resuming map[string]bool

	// labelMu is held from the moment a pane.create with no label of its
	// own picks a default one until the new record actually lands in the
	// tree, so two creates racing for the same harness and directory can
	// never both pick the same label.
	labelMu sync.Mutex
	// placeMu makes first creates on a fresh server share one workspace.
	placeMu sync.Mutex

	// recentMu guards recent-dirs.json, the list of directories panes most
	// recently started in. It is a plain mutex, not the tree's own lock:
	// this file has nothing to do with the pane tree and must not wait
	// behind it, or make it wait behind a slow disk.
	recentMu sync.Mutex

	lockMu sync.Mutex
	lock   *startLock

	// factsMu guards facts and claims: what the floor knows about each
	// pane beyond its state, and which pane reported each transcript
	// path. See facts.go. A paneFacts has its own lock, and the two are
	// never held at once.
	factsMu sync.Mutex
	facts   map[string]*paneFacts
	claims  map[string]string
	// factsEvery is the shortest time between two reads of one pane's
	// transcript. A test sets it to zero.
	factsEvery time.Duration
	// pollWait is how long a row waits for a read it just started before it
	// shows the facts it already has. pollDeadline is how long a read may
	// take: a path whose read takes longer is never read again.
	// openTranscript opens a transcript for reading. A test replaces all
	// three.
	pollWait       time.Duration
	pollDeadline   time.Duration
	openTranscript func(path string) (*os.File, uint64, uint64, bool, error)
	// getenv reads the server's own environment, which every pane
	// inherits. A test replaces it.
	getenv func(string) string
	// outside caches the gateway address, its router and whether it
	// answers, under outsideMu. See facts_stack.go.
	outsideMu sync.Mutex
	outside   outsideCache

	mu       sync.Mutex
	ln       net.Listener
	clients  map[*Client]bool
	closed   bool
	serving  bool
	waiters  []*waiter
	tickStop chan struct{}
	// endedSweepStop ends the ended-record sweep goroutine started by
	// StartEndedSweep, the same way tickStop ends the manifest tick. Under
	// the same mu.
	endedSweepStop chan struct{}
	restore        RestoreReport
	detectWarnings []string

	// closeDone is closed once, by the first call to Close, after teardown
	// and the start-lock release have both finished. A second, overlapping
	// call to Close waits on it instead of returning early, so every caller
	// of Close - not just the first - only gets its answer back once this
	// server has actually finished shutting down.
	closeDone chan struct{}

	// voice runs daisugi voice serve for the floor. See voice.go.
	voice *voiceSup

	// talkFields holds the owner's words on their way to the floor's
	// foreman. See foreman.go.
	talkFields

	// promptFields holds the prompts that wait for a pane to be ready.
	// See ready.go.
	promptFields

	// tickScanned and tickSkipped (detect.go) count, since this Server
	// started, how many pane-visits the manifest tick actually read the
	// screen for versus decided not to from the pane's current state alone,
	// before ever touching the screen. They exist for tests: a merged state
	// that never changed proves nothing about which of those two things
	// happened, because state.Merge's own precedence rules can produce that
	// same net effect whether or not scanOnce's own up-front skip fired.
	tickScanned int
	tickSkipped int

	// afterTeardownSnapshot, when set, is called by teardownLivePanes (once
	// per call, so twice across one Close) right after it takes its
	// snapshot of s.live, before it starts stopping anything that snapshot
	// found. It is nil in every real server and in every test but one:
	// TestClosesSecondTeardownPassCatchesAPaneStartedWhileTheFirstPassRan
	// sets it to learn, deterministically, the instant Close's FIRST
	// teardown pass has looked at s.live, rather than guessing at a sleep
	// long enough to cover that moment under -race and under load.
	// teardownLivePanes reads it with no lock. That is only safe because a
	// test sets it before starting the goroutine that calls Close, so the
	// "go" statement's own happens-before covers the read. Set it before
	// the Close that will read it, never after, and never from a second
	// goroutine while a Close is already in flight.
	afterTeardownSnapshot func()

	// afterApplyState, when set, is called synchronously by ApplyState, on
	// the same goroutine and before it returns, with the pane id and the
	// event it just merged. It is nil in every real server and in every
	// test but one: TestHeadlessPaneRecordIsClosedBeforeApplyStatePostsDone
	// uses it to read the pane's own Closed flag from inside pumpAdapter's
	// own call stack, at the instant its done event is applied - not from a
	// second goroutine racing to observe the same fact after the fact,
	// which is exactly how a fixed poll interval or a second round trip
	// missed the ordering bug this same test used to be named for:
	// TestHeadlessPaneRecordIsClosedBeforeWaitOutputSeesDone passed 10/10
	// under -race even with the fix reverted. Set it before
	// the pane.create that will start the goroutine calling ApplyState,
	// never after.
	afterApplyState func(paneID string, ev proto.PaneStateEvent)

	// presenceGap, when set, is called by tellPresence after it works out
	// who looks at a pane and before it sends the event. It is nil outside
	// tests. A test uses it to hold one presence event between the two
	// steps.
	presenceGap func(paneID string)
	// presenceMu orders presence events. See tellPresence.
	presenceMu sync.Mutex
	// beforeWorktreeRemove, when set, is called by task.close with each
	// worktree path just before git is asked to remove it, after the panes
	// closed. Test-only: it is how a test makes a worktree dirty in the
	// window between the dirty check and the removal.
	beforeWorktreeRemove func(path string)
}

// SetDetectionWarnings records what the manifest loader complained about, so
// server.status can show it. Warnings nothing reads are the same as no
// warnings: a broken override file would silently revert to the bundled
// manifest with no signal an operator could see.
func (s *Server) SetDetectionWarnings(w []string) {
	s.mu.Lock()
	s.detectWarnings = append([]string(nil), w...)
	s.mu.Unlock()
}

// A nil slice marshals as null, and server.status's
// detection_warnings did exactly that whenever nothing had warned - a
// caller iterating it would have to special-case the type. []string{}, the
// same treatment RestoreReport.Notes already gets, marshals as [] instead.
func (s *Server) DetectionWarnings() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]string{}, s.detectWarnings...)
	return out
}

func (s *Server) LastRestore() RestoreReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restore
}

func New(cfg Config) (*Server, error) {
	if cfg.SocketPath == "" {
		cfg.SocketPath = SocketPath()
	}
	if cfg.DataDir == "" {
		cfg.DataDir = DataDir()
	}
	// Every pane is told this directory in COPPICE_DATA_DIR, and the gate
	// takes only an absolute path there, so make a relative one absolute
	// once, here.
	if abs, err := filepath.Abs(cfg.DataDir); err == nil {
		cfg.DataDir = abs
	}
	if cfg.ForemanWait <= 0 {
		cfg.ForemanWait = DefaultForemanWait
	}
	if cfg.GateRoot == "" {
		cfg.GateRoot = web.GateRoot(cfg.DataDir)
	}
	if cfg.HookHome == "" {
		cfg.HookHome, _ = os.UserHomeDir()
	}
	if cfg.StartDir == "" {
		if wd, err := os.Getwd(); err == nil {
			cfg.StartDir = wd
		}
	}
	s := &Server{
		cfg: cfg, tree: layout.New(), states: state.NewStore(),
		started: time.Now(), peerUID: peerUID, peerPID: peerPID, procStat: procStat,
		handlers: map[string]Handler{}, clients: map[*Client]bool{},
		live:           map[string]*LivePane{},
		resuming:       map[string]bool{},
		children:       map[string]map[string]*child{},
		asks:           map[string]map[string]notedAsk{},
		holds:          map[string]*hold{},
		surfaced:       map[string][]string{},
		typed:          map[string]bool{},
		open:           map[string]bool{},
		holdFor:        DefaultHoldFor,
		plugPIDs:       map[int]string{},
		plugExited:     map[int]bool{},
		plugSpecs:      map[string]PluginSpec{},
		listPIDs:       listPIDs,
		facts:          map[string]*paneFacts{},
		claims:         map[string]string{},
		factsEvery:     time.Second,
		pollWait:       50 * time.Millisecond,
		pollDeadline:   5 * time.Second,
		openTranscript: openRegular,
		getenv:         os.Getenv,
		closeDone:      make(chan struct{}),
		talkFields:     talkFields{talkStop: make(chan struct{}), foremanWait: cfg.ForemanWait},
		promptFields:   promptFields{prompts: map[string]*promptQueue{}, promptSending: map[string]bool{}, trusted: map[string]trustAnswer{}, trusting: map[string]bool{}},
		// Notes starts as [] rather than nil, so server.status reports
		// "notes": [] for a server that has not restored anything yet -
		// never called Restore, or called it against an empty data
		// directory - instead of null.
		restore: RestoreReport{Notes: []string{}},
		voice:   newVoiceSup(cfg.Voice, cfg.DataDir),
	}
	_ = s.Handle("server.status", s.handleStatus)
	// Every handler must be registered before Serve starts (Handle refuses
	// after that point), so the pane and attach verbs are wired in here
	// rather than left for a caller to remember - a real daemon that skipped
	// this step would serve a socket with no pane verbs, or no pane.attach,
	// at all.
	s.RegisterPaneCommands()
	s.RegisterTaskCommands()
	s.RegisterAttachCommands()
	s.RegisterAgentCommands()
	s.RegisterFloorCommands()
	s.RegisterEndedCommands()
	s.RegisterProjectCommands()
	s.RegisterVoiceCommands()
	s.RegisterForemanCommands()
	return s, nil
}

// AcquireStartLock takes the single-instance lock and holds it for the life of
// this server. Call it FIRST, before Restore and before Listen: a second start
// must fail before it can restore a layout, spawn resumed panes, or touch the
// socket.
func (s *Server) AcquireStartLock() error {
	s.lockMu.Lock()
	defer s.lockMu.Unlock()
	if s.lock != nil {
		return nil
	}
	l, pid, err := acquireStartLock(s.cfg.DataDir)
	if err != nil {
		if errors.Is(err, ErrAlreadyRunning) {
			who := "another process"
			if pid > 0 {
				who = fmt.Sprintf("pid %d", pid)
			}
			return fmt.Errorf("%w: coppice server already running (%s). Run: coppice server status",
				ErrAlreadyRunning, who)
		}
		return err
	}
	s.lock = l
	return nil
}

func (s *Server) HoldsStartLock() bool {
	s.lockMu.Lock()
	defer s.lockMu.Unlock()
	return s.lock != nil
}

func (s *Server) releaseStartLock() {
	s.lockMu.Lock()
	l := s.lock
	s.lock = nil
	s.lockMu.Unlock()
	if l != nil {
		_ = l.release()
	}
}

func (s *Server) Tree() *layout.Tree   { return s.tree }
func (s *Server) States() *state.Store { return s.states }

// Handle registers a command handler under its name. Every handler must be
// registered before Serve starts accepting connections: the read loop looks
// up s.handlers with no lock of its own, on the assumption that registration
// is finished before there is anything to race it. A Handle call after Serve
// has started is refused rather than silently allowed to race that lookup.
func (s *Server) Handle(n string, h Handler) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.serving {
		return fmt.Errorf(
			"cannot register command %q: Serve has already started. Register every handler before Serve", n)
	}
	s.handlers[n] = h
	return nil
}

func (s *Server) Listen() error {
	// The lock is what makes the unlink below safe. Probing the socket and then
	// removing it is check-then-act: between the probe and the unlink another
	// start can bind, and this one would delete a live socket and take its
	// place. Holding the lock means any socket file here is stale by
	// construction, so no probe is needed and none is done.
	if !s.HoldsStartLock() {
		return fmt.Errorf(
			"call AcquireStartLock before Listen. The lock is what makes removing a stale socket safe")
	}
	s.mu.Lock()
	already := s.ln != nil
	s.mu.Unlock()
	if already {
		return ErrAlreadyListening
	}
	dir := filepath.Dir(s.cfg.SocketPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	_ = os.Remove(s.cfg.SocketPath)
	ln, err := net.Listen("unix", s.cfg.SocketPath)
	if err != nil {
		return err
	}
	if err := os.Chmod(s.cfg.SocketPath, 0o600); err != nil {
		_ = ln.Close()
		return err
	}
	s.mu.Lock()
	if s.ln != nil {
		// Lost a race with a concurrent Listen: close what we just bound
		// instead of leaking it, and report the same refusal.
		s.mu.Unlock()
		_ = ln.Close()
		return ErrAlreadyListening
	}
	s.ln = ln
	s.mu.Unlock()
	return nil
}

func (s *Server) Serve() error {
	s.mu.Lock()
	ln := s.ln
	s.serving = true
	s.mu.Unlock()
	if ln == nil {
		return fmt.Errorf("call Listen before Serve")
	}
	me := uint32(os.Getuid())
	for {
		conn, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			return err
		}
		go func(conn net.Conn) {
			defer conn.Close()
			uc, ok := conn.(*net.UnixConn)
			if !ok {
				s.refuse(conn, "this connection is not a unix socket")
				return
			}
			uid, err := s.peerUID(uc)
			if err != nil || uid != me {
				s.refuse(conn, "this socket serves one user. Run coppice as the user that started the server.")
				return
			}
			s.serveConn(conn, conn, s.factsOf(uc))
		}(conn)
	}
}

func (s *Server) refuse(w io.Writer, msg string) {
	_ = proto.NewEncoder(w).Send(proto.ErrResp("", proto.ErrUnauthorized, msg))
}

// ServeConn runs the request loop over any reader and writer. Only the socket
// path reaches it in production; the tests drive it directly. --stdio does NOT
// build a server: it proxies to the running one.
func (s *Server) ServeConn(r io.Reader, w io.Writer) {
	s.serveConn(r, w, nil)
}

// factsOf places the peer of a socket connection. On a system with no
// peer pid it returns nil, and the hello alone decides the role.
func (s *Server) factsOf(uc *net.UnixConn) *peerFacts {
	if !peerPIDSupported {
		return nil
	}
	pid, err := s.peerPID(uc)
	if err != nil {
		return &peerFacts{checked: true, unknown: true}
	}
	// A policy's pid is recorded under plugMu while it starts, so a policy
	// that dials at once waits here until it can be placed.
	s.plugMu.Lock()
	defer s.plugMu.Unlock()
	s.prunePlugins()
	f := classifyPeer(pid, os.Getpid(), leadsSession(), s.panePIDs(), s.plugPIDs, s.procStat)
	return &f
}

// serveConn is ServeConn with the kernel facts about the peer, or nil
// when there are none.
func (s *Server) serveConn(r io.Reader, w io.Writer, facts *peerFacts) {
	c := newClient(w)
	if facts != nil {
		c.facts = *facts
	}
	s.mu.Lock()
	s.clients[c] = true
	s.mu.Unlock()
	// wg tracks this connection's own in-flight handler goroutines. Without
	// it, ServeConn could return - and a caller could tear down w right
	// behind it - while the last request's handler was still running,
	// silently dropping its response.
	var wg sync.WaitGroup
	defer func() {
		// Drop first: raise the cancellation signal a handler (agent.wait
		// among them) can select on, and stop this client from
		// being handed new events, before doing anything below that could
		// itself block. Waiting on wg first, before Drop, made a handler
		// stuck writing to a peer that had stopped reading pin this whole
		// teardown - and the goroutine and file descriptor behind it -
		// forever: nothing but the caller's conn.Close() could free it, and
		// that runs only after ServeConn returns.
		c.Drop()
		s.mu.Lock()
		delete(s.clients, c)
		s.mu.Unlock()
		// The people this client counted as looking stop counting now.
		c.mu.Lock()
		gone := make([]string, 0, len(c.Attached))
		for id := range c.Attached {
			gone = append(gone, id)
		}
		c.mu.Unlock()
		for _, id := range gone {
			s.tellPresence(id)
		}

		// From here on, teardownWriter arms a fresh deadline immediately
		// before every future Write on this connection - see its own doc
		// comment. That alone does not cover a write already in flight
		// RIGHT NOW, blocked before this line ever ran: teardownWriter's
		// check happens once, at the start of a Write call, so a write that
		// started before tearing flips never gets a deadline armed for it
		// this way. Arming one here, directly on w, catches exactly that
		// write - the same one TestATeardownDoesNotWaitForAHandlerStuckWritingToADeadPeer
		// builds, where the handler's Send is already blocked, or about to
		// block, the instant this connection starts tearing down.
		atomic.StoreInt32(&c.tearing, 1)
		if dl, ok := w.(interface{ SetWriteDeadline(time.Time) error }); ok {
			_ = dl.SetWriteDeadline(time.Now().Add(writeTeardownDeadline))
		}

		// Wait for in-flight handlers, but bounded. Server-wide shutdown,
		// through Close, gets the short bound: an operator-requested stop
		// should return quickly, and Close has its own independent bound on
		// waiting for handlers besides. An ordinary connection ending on
		// its own - this client disconnected, or a --stdio caller
		// half-closed after its one request - gets the long bound instead:
		// the server is still up, so there is no reason to rush a handler
		// that is still doing real work. Either way, a write that peer
		// never reads still fails within about writeTeardownDeadline of
		// being attempted, so the long bound here is not "how long a stuck
		// write is tolerated" - it is "how long a handler that has not
		// written anything yet is given to get there."
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		wait := handlerTeardownWait
		if !s.isClosed() {
			wait = peerCloseHandlerWait
		}
		select {
		case <-done:
		case <-time.After(wait):
		}
	}()

	d := proto.NewDecoder(r)
	for {
		line, err := d.Next()
		if err != nil {
			return
		}
		// Close drops every client it knows about, and sets s.closed the
		// moment it starts, before it can necessarily have reached this
		// one (a client that registered after Close took its snapshot,
		// say). Either signal means this server no longer owns what a
		// handler would touch - the tree, the state store, even the data
		// directory, since a new server may already hold the start lock -
		// so a still-open connection must stop being served here, not keep
		// dispatching into a server that already gave up its exclusivity.
		if c.Dead() || s.isClosed() {
			return
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		req, lineFraming, err := proto.DecodeLine(line)
		// A JSON-RPC line switches this connection for good. The framing
		// is read once here and handed to the reply goroutine, so a reply
		// leaves in the shape its own request arrived in.
		if lineFraming == proto.JSONRPC {
			c.setFraming(proto.JSONRPC)
		}
		f := c.Framing()
		if err != nil {
			// Nothing runs without a reply. A native connection gets the
			// bad_request line it always got. A JSON-RPC connection gets the
			// JSON-RPC code DecodeLine chose, with the request's id when it
			// had a usable one and null when it did not.
			code, msg := proto.RPCInvalidRequest, err.Error()
			var de *proto.DecodeError
			if errors.As(err, &de) {
				code, msg = de.Code, de.Message
			}
			resp := proto.ErrResp(req.ID, proto.ErrBadRequest, msg)
			resp.RawID, resp.RPCCode = req.RawID, code
			if len(resp.RawID) == 0 {
				resp.RawID = json.RawMessage("null")
			}
			_ = c.send(resp, f)
			continue
		}
		if f == proto.JSONRPC && lineFraming == proto.Native {
			resp := proto.ErrResp(req.ID, proto.ErrBadRequest,
				"this connection speaks JSON-RPC 2.0. Send jsonrpc 2.0 on every line.")
			resp.RawID, resp.RPCCode = req.RawID, proto.RPCInvalidRequest
			if len(resp.RawID) == 0 {
				resp.RawID = json.RawMessage("null")
			}
			_ = c.send(resp, f)
			continue
		}
		// hello runs here, in the read loop, so the verbs after it on
		// the same connection see the role it sets.
		if req.Cmd == "hello" {
			resp := s.handleHello(c, &req)
			resp.RawID = req.RawID
			_ = c.send(resp, f)
			continue
		}
		h, ok := s.handlers[req.Cmd]
		if !ok {
			resp := proto.ErrResp(req.ID, proto.ErrBadRequest,
				fmt.Sprintf("no command %q. Known commands: %s", req.Cmd, s.commandList()))
			resp.RawID, resp.RPCCode = req.RawID, proto.RPCMethodNotFound
			_ = c.send(resp, f)
			continue
		}
		// Each request runs on its own goroutine. agent.wait blocks for up to
		// two minutes and pane.wait_output for thirty seconds, both inside the
		// handler; the cockpit and the PWA hold one long-lived connection and
		// must keep answering for every other pane meanwhile. proto.Encoder.Send
		// is mutex-guarded, so writes stay one whole line at a time. wg.Add and
		// the inflight increment happen here, synchronously in the read loop,
		// before this goroutine is handed the request, so a later wg.Wait or
		// waitForHandlers can never start too soon.
		wg.Add(1)
		atomic.AddInt64(&s.inflight, 1)
		go func(req proto.Request, f proto.Framing) {
			defer wg.Done()
			defer atomic.AddInt64(&s.inflight, -1)
			resp, allowed := s.guard(c, &req)
			if allowed {
				s.noteVerb(c, &req)
				resp = h(c, &req)
			}
			resp.RawID = req.RawID
			_ = c.send(resp, f)
		}(req, f)
	}
}

func (s *Server) commandList() string {
	names := make([]string, 0, len(s.handlers))
	for n := range s.handlers {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// Broadcast sends one event to every client that asked for its kind and pane.
func (s *Server) Broadcast(kind, paneID string, v any) {
	s.mu.Lock()
	clients := make([]*Client, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()
	for _, c := range clients {
		c.mu.Lock()
		want := c.Subs[kind] && (c.SubPanes["*"] || c.SubPanes[paneID])
		c.mu.Unlock()
		if want {
			c.Emit(v) // queued, never blocking on this client's socket
		}
	}
}

// RestartNote is user-facing copy. It states plainly what a restart brings
// back and what it does not, which is the honesty tag for this whole server.
// Exported so the README can be tested against this exact string: two
// different answers to what survives a restart is worse than one.
const RestartNote = "A restart brings back the layout, the labels and the working directories. " +
	"It does not bring back running processes. Panes come back closed or unknown, never idle. " +
	"A headless pane resumes only when its adapter recorded a harness session id."

func (s *Server) handleStatus(c *Client, r *proto.Request) proto.Response {
	panes := s.tree.Panes()
	live := 0
	for _, p := range panes {
		if !p.Closed {
			live++
		}
	}
	return proto.OKResp(r.ID, map[string]any{
		"protocol":           proto.Version,
		"pid":                os.Getpid(),
		"socket":             s.cfg.SocketPath,
		"data_dir":           s.cfg.DataDir,
		"uptime_s":           int(time.Since(s.started).Seconds()),
		"panes":              len(panes),
		"panes_live":         live,
		"resumable":          s.resumableCount(),
		"restart_note":       RestartNote,
		"restore":            s.LastRestore(),
		"detection_warnings": s.DetectionWarnings(),
	})
}

// isClosed reports whether Close has started. It is the fallback the read
// loop checks alongside c.Dead(): Close sets this before it can necessarily
// have reached every client in its snapshot, so a connection that registered
// right around that moment is still caught.
func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Close stops listening, drops every connected client, kills every live
// pane's process, and waits (bounded) for any in-flight handler to finish,
// releasing the start lock only after all of that. This is the FIRST call's
// job. Close is idempotent: a second, overlapping call does none of this
// itself. It blocks on closeDone instead, so it returns only once the first
// call has actually finished tearing every pane down and released the lock -
// never before, and never by skipping ahead to release the lock on its own.
//
// The order matters: a `coppice server start` racing a genuine shutdown must
// either see the lock still held, or find a server that gave its handlers
// their full handlerTeardownWait to finish and, in the ordinary case, left
// no process running behind it - never one that freed the lock out from
// under work still in flight, or skipped teardown outright. waitForHandlers
// itself can give up with a handler still in flight; it is a bounded wait,
// not a guarantee every handler has actually returned. Two separate bounds
// keep this from hanging forever rather than guaranteeing every process is
// gone: each of teardownLivePanes' two calls has its own closeTeardownDeadline
// for the panes its own snapshot found, and waitForHandlers has its own bound
// for the handlers. A pane whose process ignores SIGKILL long enough to miss
// both of those windows is the one case this order does not cover.
//
// teardownLivePanes runs twice: once here, and once more after
// waitForHandlers, right before the lock is released. See its own doc
// comment for why the second pass exists, why it is nearly free for a pane
// the first pass already tore down, and why its deadline is per-call rather
// than a total across both calls.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		<-s.closeDone
		return nil
	}
	s.closed = true
	ln := s.ln
	tickStop := s.tickStop
	s.tickStop = nil
	endedSweepStop := s.endedSweepStop
	s.endedSweepStop = nil
	clients := make([]*Client, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()
	close(s.talkStop)

	// Stops the manifest tick goroutine (detect.go), if StartManifestTick
	// ever started one. A caller that forgot StopManifestTick must not leak
	// it past Close - taking tickStop under the same lock that sets closed,
	// and nil-ing it here, is also what makes StopManifestTick a safe no-op
	// if it runs after this.
	if tickStop != nil {
		close(tickStop)
	}
	if endedSweepStop != nil {
		close(endedSweepStop)
	}
	s.stopHolds()

	var err error
	if ln != nil {
		err = ln.Close()
	}
	for _, c := range clients {
		c.Drop()
	}

	s.teardownLivePanes()

	s.waitForHandlers()

	// Backstop pass: a handler already dispatched when isClosed() started
	// refusing new work (handlePaneCreate's own guard) has had its full
	// chance to run by now, so any pane it managed to start is caught here
	// too, before the lock goes away.
	s.teardownLivePanes()

	// The voice process goes before the lock, so a server started right
	// after this one never finds the old voice server still running.
	s.voice.stop()

	s.releaseStartLock()
	close(s.closeDone)
	return err
}

// waitForHandlers waits, up to handlerTeardownWait, for every in-flight
// request handler across every connection to finish. It polls an atomic
// counter rather than a sync.WaitGroup: connections other than the one being
// torn down can still be dispatching new requests while Close runs, and
// WaitGroup panics if Add ever raises the counter off zero while a Wait is
// already in flight. A plain counter has no such hazard.
func (s *Server) waitForHandlers() {
	deadline := time.Now().Add(handlerTeardownWait)
	for atomic.LoadInt64(&s.inflight) > 0 {
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

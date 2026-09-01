// Package sprig runs sprig headlessly, one process per prompt - the same
// shape codex.go uses, and for the same underlying reason. harness/sprig/
// cli.go's own Run reads its task from argv or, failing that, blocks
// reading ALL of stdin to EOF, and only creates the session tree file AFTER
// that task is known, then runs it to completion via agent.Run and returns.
// There is no stdin-fed loop to hold open across turns: an earlier version
// of this adapter assumed one, and could never actually drive a real
// sprig - Stop() would cancel the context and close stdin before the first
// task was ever read, and the tree file this adapter exists to tail would
// never even get created.
//
// Each Prompt spawns one process:
// `sprig --session-dir <dir> --session <id> [extra] -- <task>` or
// `sprig --session-dir <dir> --resume <id> [extra] -- <task>` -
// cli.go:59's own refusal ("--resume needs --session-dir too") is why
// --session-dir travels on every turn, fresh or resumed. Which of the two
// is NOT simply "does the session file exist right now", even though a
// live os.Stat at every turn's own start (turnArgv) is most of the answer:
// a caller's own explicit choice for the very first turn is honoured as
// given regardless of what the file says (an explicit --session tries
// fresh even against a file that already exists, so sprig's own refusal
// can surface honestly; an explicit --resume - or StartOpts.Resume - tries
// --resume even against a file that does not exist YET, and never falls
// back to minting one under that id even once it's clear the file will
// never exist - see turnArgv's own comment for both directions and why
// they are not symmetric with a plain file check). Once a turn has
// actually STARTED (p.sessionEstablished, set only after cmd.Start
// succeeds, never before), the file decides on its own for a pane that was
// minted or given an explicit --session; this is a live check at each
// turn's own start, not a count of how many turns have been attempted: a
// turn whose spawn itself fails never creates anything on disk, and a
// counter that already advanced past that failed attempt would wrongly
// send --resume next against a file that was never written, wedging the
// pane for good (see turnArgv's own comment). `extra` is StartOpts.Argv
// with --session-dir/--session/--resume stripped out, so this adapter's
// own choice of flag and id is never shadowed by a stale copy the caller
// happened to pass in. The adapter keeps ONE event channel across turns,
// same as codex, so a client sees one continuous conversation even though
// a new process runs for every prompt; only one turn may run at a time,
// and a second Prompt while one is in flight is refused outright, never
// queued.
//
// This adapter never emits EvEnd. adapter.go's rule (d) treats EvEnd as
// terminal, and the server's pump reacts to it by calling Stop() and
// draining - correct for claude's one long-lived process, wrong here,
// where every turn's process exits on its own and the pane keeps taking
// prompts, exactly the reasoning codex.go's own package doc gives for the
// same choice. Only Stop() ends a sprig pane, and the events channel
// closing (owned solely by relay, never by Stop itself) is how a client
// learns that.
//
// sprig's own state does not come from a screen: it comes from the session
// tree it already writes on path E (harness/sprig/session_tree.go), tailed
// from the file offset the PREVIOUS turn left off at - never replayed from
// the start of the file. Start itself stats the session's path before any
// turn ever runs and seeds the offset from whatever is already there,
// unconditionally: not only an actual --resume, but also a client's own
// explicit --session that happens to name a file already on disk (that
// turn is about to fail regardless - NewSessionWriter's own refusal - and
// its pre-existing history must not replay into the pane on the way there
// either). A "verdict","decision":"deny" row is sprig's own real gate
// refusal, so this adapter, unlike claude's and codex's, DOES report
// blocked from its own stream for it: the clause travels in Detail. Every
// other permission decision still also reaches coppice through the gate
// hook (COPPICE_SOCK/COPPICE_PANE, wired by rule (a) of pane.Adapter) - the
// tree row and the hook are two paths to the same fact, not competing ones.
//
// A turn that exits non-zero - sprig's own session-tree refusals (a
// --resume of a session id nothing ever wrote, or a --session of one that
// already exists), or any other failure cli.go reports on stderr - surfaces
// as an EvError naming both sprig's own captured message (the single last
// non-banner line - lastUsefulLine's own comment says why, and why it is
// capped) and the exit code, always, the same "name it regardless" shape
// codex.go's own turn-ended-without-completion diagnostic uses: a fact
// sprig produced, the same principle the tree's own deny verdicts follow.
//
// Model text (Event.Text) is written into the pane's grid raw, unescaped -
// rule (d) of pane.Adapter requires this file to say which choice it made,
// same as claude's and codex's own adapters: an assistant row whose text
// happened to contain coppice's own "[end]"/"[error]" marker text could
// forge a marker row, so a reader must treat those markers as advisory, not
// authoritative, for a sprig pane too.
package sprig

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/opendaisugi/coppice/internal/adapters"
	"github.com/opendaisugi/coppice/internal/pane"
)

// Binary is the command run for a headless sprig turn. COPPICE_SPRIG_BIN
// overrides it, which is how the tests run without a real sprig on PATH.
const Binary = "sprig"

// TailInterval is how often the session tree file is re-read WHILE a turn's
// process is running. sprig only ever appends, so a poll on the file is
// cheap and needs no inotify.
const TailInterval = 200 * time.Millisecond

// eventsBufferSize is the events channel's buffer capacity. It is a var, not
// a const, only so TestStopWhileTheStreamIsFlowingDoesNotPanic can shrink it
// to force relay's blocked-send branches to actually run under -race,
// instead of them sitting untested because the real 256-slot buffer never
// fills for a small fixture - the same trick claude's and codex's own test
// files use on their equivalent var.
var eventsBufferSize = 256

func binary() string {
	if b := os.Getenv("COPPICE_SPRIG_BIN"); b != "" {
		return b
	}
	return Binary
}

type adapter struct{}

// New returns the sprig adapter. See the package doc for the one-process-
// per-turn shape this is built around.
func New() pane.Adapter { return adapter{} }

func (adapter) Name() string { return "sprig" }

func init() { adapters.Register(New()) }

// --- argv helpers -----------------------------------------------------
//
// All three of these stop at a literal "--": Go's flag package (sprig's
// own, via cli.go's flag.FlagSet) treats "--" as the end of flag parsing,
// so anything at or after it in a pane's StartOpts.Argv is opaque,
// positional content to sprig, not a flag this adapter should read, strip,
// or reinterpret - even if a token there happens to spell "--session".

// beforeDoubleDash returns argv up to (not including) the first literal
// "--", or the whole slice if there is none.
func beforeDoubleDash(argv []string) []string {
	for i, a := range argv {
		if a == "--" {
			return argv[:i]
		}
	}
	return argv
}

// TreePath is the session tree file this adapter tails for a pane with
// argv and harness session id sessionID. ok is false when argv names no
// absolute --session-dir, or sessionID is not a plain file name.
func TreePath(argv []string, sessionID string) (string, bool) {
	dir, ok := flagValue(argv, "session-dir")
	if !ok || !filepath.IsAbs(dir) {
		return "", false
	}
	if sessionID == "" || sessionID == "." || sessionID == ".." ||
		strings.ContainsAny(sessionID, `/\`) {
		return "", false
	}
	return filepath.Join(dir, sessionID+".jsonl"), true
}

// flagValue reads --name VALUE or --name=VALUE out of argv, never looking
// at or past a literal "--".
func flagValue(argv []string, name string) (string, bool) {
	head := beforeDoubleDash(argv)
	for i, a := range head {
		if a == "--"+name && i+1 < len(head) {
			return head[i+1], true
		}
		if len(a) > len(name)+3 && a[:len(name)+3] == "--"+name+"=" {
			return a[len(name)+3:], true
		}
	}
	return "", false
}

// insertAfterSessionDir splices extra flags in right after --session-dir's
// own flag+value pair, rather than at the end of argv, and never past a
// literal "--" even when one is present. Go's flag package stops parsing at
// the first token that is not itself a recognised flag, so appending at the
// very end would silently vanish into a positional task if one preceded it;
// placing the splice before any "--" keeps the injected flags visible to
// sprig's own flag.Parse regardless of what the caller put after it.
func insertAfterSessionDir(argv []string, extra ...string) []string {
	head := beforeDoubleDash(argv)
	tail := argv[len(head):] // "--" and everything after, verbatim (or empty)
	insertPos := len(head)
	for i, a := range head {
		switch {
		case a == "--session-dir" && i+1 < len(head):
			insertPos = i + 2
		case strings.HasPrefix(a, "--session-dir="):
			insertPos = i + 1
		}
	}
	out := make([]string, 0, len(argv)+len(extra))
	out = append(out, head[:insertPos]...)
	out = append(out, extra...)
	out = append(out, head[insertPos:]...)
	out = append(out, tail...)
	return out
}

// sessionFlagNames are the three flags this adapter itself owns on every
// turn's argv: --session-dir always, and exactly one of --session/--resume.
var sessionFlagNames = map[string]bool{"--session-dir": true, "--session": true, "--resume": true}

// stripSessionFlags removes --session-dir/--session/--resume (and their
// values) from argv, so this adapter's own per-turn construction of those
// flags - built fresh every turn from StartOpts.Cwd/Env and the id this
// adapter decided on - is never shadowed by a stale copy the caller
// originally passed in StartOpts.Argv. Like flagValue and
// insertAfterSessionDir, it never touches anything at or after a literal
// "--": a token there that merely looks like one of these names is opaque,
// positional content, left alone.
func stripSessionFlags(argv []string) []string {
	head := beforeDoubleDash(argv)
	tail := argv[len(head):]
	out := make([]string, 0, len(head))
	for i := 0; i < len(head); i++ {
		a := head[i]
		if sessionFlagNames[a] {
			i++ // also skip its value
			continue
		}
		if eq := strings.IndexByte(a, '='); eq > 0 && sessionFlagNames[a[:eq]] {
			continue
		}
		out = append(out, a)
	}
	return append(out, tail...)
}

// newSessionID mints an id sprig will accept as --session: NewSessionWriter
// does not validate the id's shape at all, only that dir/id+".jsonl" does
// not already exist, so any filesystem-safe string works. The "coppice-"
// prefix is not required by sprig; it is here so an `ls` of the session
// directory tells a coppice-minted id apart from one sprig minted itself
// (defaultSessionID in session_tree.go: two raw 8-hex draws, no prefix).
func newSessionID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "coppice-" + hex.EncodeToString(b)
}

// detailLineCap bounds how much of one captured line reaches a client: the
// pump renders one grid row per event, so an unbounded line (a stack trace,
// a huge stderr dump) would blow that row out. 400 runes is generous for a
// one-line diagnostic while staying bounded.
const detailLineCap = 400

// isSessionPathBanner reports whether line is cli.go's own informational
// "sprig: session %s at %s" line (cli.go:121), printed on every successful
// --session-dir setup whether the turn that follows succeeds or fails. It
// says nothing about failure, so it must never be picked as the reason one
// happened. It is NOT the same prefix as sprig's session-tree ERRORS
// (cli.go:109,116: "sprig: session tree: ..."), which share the leading
// "sprig: session " text but are real content, never skipped - the "tree:"
// that immediately follows is what tells the two apart.
func isSessionPathBanner(line string) bool {
	return strings.HasPrefix(line, "sprig: session ") && !strings.HasPrefix(line, "sprig: session tree:")
}

// lastUsefulLine picks the single line of captured stdout+stderr that best
// explains a non-zero exit: the LAST non-empty line that is not cli.go's
// own benign session-path banner. Capped so one pathological line cannot
// blow out the pump's one-row-per-event contract.
func lastUsefulLine(captured string) string {
	lines := strings.Split(strings.TrimRight(captured, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || isSessionPathBanner(line) {
			continue
		}
		if r := []rune(line); len(r) > detailLineCap {
			line = string(r[:detailLineCap])
		}
		return line
	}
	return ""
}

// proc keeps ONE event channel across turns even though a new child process
// runs for every one - the same shape codex.go's proc uses, for the same
// reason. Exactly one goroutine ever closes that channel: relay, below,
// after Stop has been signalled and every turn's own goroutine has
// finished sending. Stop signals and kills; it never closes a channel a
// turn goroutine may still be sending on - a send on a closed channel
// panics, and a panic in an adapter goroutine takes the whole daemon down
// with every other pane's process orphaned alongside it.
type proc struct {
	ctx    context.Context
	cancel context.CancelFunc
	opts   pane.StartOpts

	sessionDir string   // from --session-dir, validated once at Start
	extraArgv  []string // opts.Argv with --session-dir/--session/--resume stripped

	in        chan pane.Event // turn goroutines send here
	events    chan pane.Event // clients read here; closed by relay, by nobody else
	stop      chan struct{}   // closed by Stop, once
	stopOnce  sync.Once
	relayDone chan struct{} // closed by relay, BEFORE events - see relay's own doc comment
	turns     sync.WaitGroup

	mu                 sync.Mutex
	sessionID          string
	firstTurnIsResume  bool // the id came from an explicit --resume/o.Resume, not a mint
	sessionEstablished bool // a turn's cmd.Start has actually succeeded at least once
	tailOffset         int64
	running            *exec.Cmd  // the turn currently in flight, for Stop to kill
	pids               []int      // the root pid of every turn, newest last, up to pane.PidKeep
	turnActive         bool       // claimed the instant beginTurn succeeds, well before running is set
	readEnd            *os.File   // this turn's own end of the output pipe - Stop can force it shut
	closeReadEnd       *sync.Once // shared with runTurn so whichever side closes readEnd first wins
}

func (adapter) Start(ctx context.Context, o pane.StartOpts, _ *pane.Grid) (pane.Proc, error) {
	// Rule (b) of pane.Adapter: the server owns rendering. g is accepted and
	// ignored, same as claude and codex.
	dir, ok := flagValue(o.Argv, "session-dir")
	if !ok || dir == "" {
		return nil, fmt.Errorf(
			"sprig needs --session-dir in the pane's argv. That file is where its state comes from")
	}

	// sprig's flag is --session, not --session-id: harness/sprig/cli.go's
	// own fs.String("session", ...) declaration is what
	// TestTheSessionFlagMatchesSprigsOwnSource checks this against.
	sessionFlag, hasSession := flagValue(o.Argv, "session")
	resumeFlag, hasResumeFlag := flagValue(o.Argv, "resume")

	var sessionID string
	var isResume bool
	switch {
	case hasSession && sessionFlag != "":
		// The caller already named a fresh session. Use it as given.
		sessionID = sessionFlag
	case hasResumeFlag && resumeFlag != "":
		// The caller already asked to resume, directly in argv.
		sessionID = resumeFlag
		isResume = true
	case o.Resume != "":
		// Resume a recorded session via StartOpts.
		sessionID = o.Resume
		isResume = true
	default:
		// sprig's default with no --session is "a fresh one", and it never
		// tells us which id it picked. Mint the id ourselves, or the tail
		// below has no filename to watch.
		sessionID = newSessionID()
	}

	ctx, cancel := context.WithCancel(ctx)
	p := &proc{
		ctx: ctx, cancel: cancel, opts: o,
		sessionDir: dir,
		extraArgv:  stripSessionFlags(o.Argv),
		in:         make(chan pane.Event, 256),
		events:     make(chan pane.Event, eventsBufferSize),
		stop:       make(chan struct{}),
		relayDone:  make(chan struct{}),
		sessionID:  sessionID, firstTurnIsResume: isResume,
	}
	// Never replay a pre-existing session's recorded history into this
	// pane: start tailing from wherever the file already ends, not from
	// byte zero. This is unconditional, not gated on isResume: an explicit
	// --session naming a file that ALREADY exists (SHOULD-FIX 1 - a
	// client's own doing, not this adapter's resume path) is just as much
	// a pre-existing file as an actual --resume is, and its history must
	// not replay either, even though the turn itself is about to fail
	// (NewSessionWriter's own refusal - see the package doc). On the
	// ordinary mint path the file does not exist yet, the stat simply
	// fails, and the offset correctly stays 0.
	if fi, err := os.Stat(filepath.Join(dir, sessionID+".jsonl")); err == nil {
		p.tailOffset = fi.Size()
	}
	go p.relay()
	return p, nil
}

// relay is the single owner of the events channel: the only goroutine that
// ever sends on it or closes it. It forwards whatever turn goroutines hand
// it through p.in until Stop is signalled, waits for every turn goroutine to
// actually finish (so a turn mid-send never races a close), makes a
// best-effort pass to flush anything already queued, and only then closes.
//
// That flush is bounded by TIME, not by a single failed attempt: an earlier
// shape gave up the instant one p.events send would have blocked, which
// could drop a turn's own terminal event (its last, most important one) to
// nothing worse than a momentarily full buffer. Reusing one deadline across
// every item in the drain, rather than resetting a fresh one per item,
// keeps the whole flush bounded to one interval regardless of how many
// items are left to deliver.
//
// It closes relayDone BEFORE events - the opposite of claude's order, and
// deliberately so: sprig has several turn goroutines across the pane's
// life, not claude's one long-lived producer, so a caller that reacts to
// Events() closing must be able to trust that relayDone is already closed
// by that moment, not racing to catch up a few instructions later.
func (p *proc) relay() {
	defer close(p.events)
	defer close(p.relayDone)
	for {
		select {
		case ev := <-p.in:
			select {
			case p.events <- ev:
			case <-p.stop:
				return
			}
		case <-p.stop:
			p.turns.Wait()
			deadline := time.After(time.Second)
			for {
				select {
				case ev := <-p.in:
					select {
					case p.events <- ev:
					case <-deadline:
						return
					}
				default:
					return
				}
			}
		}
	}
}

// beginTurn atomically checks whether this pane can start a new turn and, if
// so, claims one before releasing the lock - the same check codex.go's own
// beginTurn makes, for the same reason: a pane that has been asked to stop
// starts nothing further, and only one turn may run at a time, so
// turnActive can never let a second Prompt clobber the first turn's
// *exec.Cmd.
func (p *proc) beginTurn() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.stop:
		return fmt.Errorf("sprig: this pane has stopped")
	default:
	}
	if p.turnActive {
		return fmt.Errorf("sprig: a turn is already running; wait for idle")
	}
	p.turnActive = true
	p.turns.Add(1)
	return nil
}

func (p *proc) endTurn() {
	p.mu.Lock()
	p.turnActive = false
	p.running = nil
	p.mu.Unlock()
}

func (p *proc) abortTurn() {
	p.endTurn()
	p.turns.Done()
}

// errSessionNotFound is turnArgv's own refusal for a resume-started pane
// whose session file is missing - never a spawn failure, never silently
// papered over by minting a fresh session under the id the caller asked to
// resume. See turnArgv's own comment for why.
type errSessionNotFound struct{ sessionID string }

func (e errSessionNotFound) Error() string {
	return fmt.Sprintf("session %s not found. Create a new pane.", e.sessionID)
}

// turnArgv builds one turn's full argv: --session-dir always, exactly one
// of --session or --resume, then whatever extra flags the pane's own
// StartOpts.Argv carried (--gate, --max-turns, ...), then the task,
// separated by "--" so a task that happens to start with "-" is never
// misread as a flag. It refuses outright, returning errSessionNotFound
// instead of an argv, when a resume-started pane's session file does not
// exist - see below.
//
// The session/resume choice is NOT "the first turn is fresh, every turn
// after resumes" keyed off a counter of how many turns have been
// ATTEMPTED: a turn whose spawn itself fails (os.Pipe, cmd.Start) never
// creates anything on disk, and a counter that already advanced past that
// failed attempt would wrongly send --resume on the next real attempt,
// against a file that was never written - sprig refuses ("no such file"),
// and the pane can never recover. Instead: p.sessionEstablished is a
// one-way latch set only once a turn's cmd.Start has actually SUCCEEDED
// (see Prompt) - so a failed spawn leaves it false and the id's own first
// mode (p.firstTurnIsResume, decided once in Start from the caller's
// explicit --session/--resume/o.Resume, or the mint default) still
// governs. Once a turn HAS actually started, whether to resume from then
// on is decided by the file itself, not by counting turns: an os.Stat of
// the session's own path, checked fresh at every such turn's start,
// because "does sprig's own file exist right now" is the fact that
// actually determines which flag can succeed, and a stat can never drift
// out of sync with reality the way a counter can.
//
// The one case p.firstTurnIsResume is checked even when established is
// false, or the file is missing, is deliberate and asymmetric with the
// --session side on purpose:
//
//   - A pane MINTED (or given an explicit --session) that has never
//     established a file yet tries --session regardless of what the file
//     check says: a caller's own explicit --session naming a file that
//     already exists is a directive to try fresh anyway, so sprig's own
//     refusal can surface honestly - not a case for this adapter to
//     silently paper over by switching to --resume of a session the caller
//     never asked to continue.
//   - A pane STARTED to resume a specific id (StartOpts.Resume, or an
//     explicit --resume in argv) must NEVER mint a fresh session under
//     that id, however the file came to be missing: not there at all, or a
//     first turn that spawned and then exited before ever reaching
//     openOrNewSessionWriter (which is what would have created it - the
//     spawn-succeeds-but-exits-early case belongs to a MINTED pane instead,
//     where firstTurnIsResume is false, and IS meant to retry fresh; see
//     the mutation testing this line survived). Every turn on such a pane,
//     first or hundredth, refuses with errSessionNotFound instead - a
//     resumed session silently becoming a different one under the same id
//     would hide a lost history from a client that has no way to tell
//     (SessionID() reports the same id either way), which is worse than an
//     honest, permanent refusal until the operator starts a new pane.
func (p *proc) turnArgv(task string) ([]string, error) {
	p.mu.Lock()
	sessionID := p.sessionID
	established := p.sessionEstablished
	firstTurnIsResume := p.firstTurnIsResume
	p.mu.Unlock()

	_, statErr := os.Stat(filepath.Join(p.sessionDir, sessionID+".jsonl"))
	exists := statErr == nil

	flag := "--session"
	switch {
	case firstTurnIsResume && !exists:
		return nil, errSessionNotFound{sessionID: sessionID}
	case established && exists:
		flag = "--resume"
	case firstTurnIsResume:
		flag = "--resume"
	}
	base := append([]string{"--session-dir", p.sessionDir}, p.extraArgv...)
	argv := insertAfterSessionDir(base, flag, sessionID)
	return append(argv, "--", task), nil
}

// Prompt starts one turn: a new sprig process for just this task. It
// refuses outright, via beginTurn, if a turn is already running or the pane
// has stopped - never queueing, never overlapping two turns.
func (p *proc) Prompt(text string) error {
	if err := p.beginTurn(); err != nil {
		return err
	}

	argv, err := p.turnArgv(text)
	if err != nil {
		// A resume-started pane whose session file is missing: refuse
		// outright, never spawn, never mint a fresh session under the id
		// the caller asked to resume. abortTurn releases the slot so the
		// NEXT Prompt reaches this exact same check again - not stuck on
		// "a turn is already running", but honestly refusing again, every
		// time, until the operator starts a new pane. Both the plain
		// return AND an EvError: a caller awaiting this call sees it
		// synchronously, and anything only watching Events() sees it too.
		p.abortTurn()
		p.send(pane.Event{Kind: pane.EvError, Detail: err.Error()})
		return fmt.Errorf("sprig: %w", err)
	}

	cmd := exec.CommandContext(p.ctx, binary(), argv...)
	cmd.Dir = p.opts.Cwd
	// Rule (a) of pane.Adapter: the only path by which the gate hook finds
	// COPPICE_SOCK and COPPICE_PANE, for this turn's process same as every
	// other adapter's spawn.
	cmd.Env = pane.BuildEnv(os.Environ(), p.opts.Env, p.opts.Sock, p.opts.PaneID, p.opts.DataDir)
	// Setpgid makes this turn's pid its own pgid too. sprig's own Bash tool
	// runs shell commands as its own children (tools.go's BashTool.Run,
	// exec.Command("sh","-c",cmd).CombinedOutput()), and a command that
	// backgrounds work and exits (`x & true`) leaves that work as a
	// grandchild of THIS turn, holding this pipe's write end open; a
	// whole-group kill (Stop, and runTurn's own matching cleanup) reaches
	// it, where killing just cmd.Process would not - and unwedges sprig's
	// own CombinedOutput() call too, which would otherwise wait on that
	// same held-open pipe forever on sprig's side. WaitDelay is the belt
	// Cmd's own docs name for a child that fails to exit after its Context
	// is cancelled.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second

	// The adapter owns this pipe outright (os.Pipe, not cmd.StdoutPipe):
	// Cmd.Wait's documented contract for StdoutPipe ("it is incorrect to
	// call Wait before all reads from the pipe have completed") cannot be
	// honoured AND have Wait unstick a grandchild-held pipe at the same
	// time. Owning the pipe moves "unstick a wedged read" to Stop (which
	// already knows when it is safe to force one closed), and Wait runs
	// once, after the scan loop, the order StdoutPipe's own doc calls
	// correct. Stdout and stderr share this one pipe: sprig's own output is
	// sequential, never both at once (a banner line or two on stderr, then
	// EITHER the final answer on stdout on success OR one error line on
	// stderr on failure - never a live interleaved stream from a model),
	// so a single combined capture loses nothing that matters and this
	// adapter needs one pipe's worth of shutdown plumbing, not two.
	r, w, perr := os.Pipe()
	if perr != nil {
		p.abortTurn()
		return perr
	}
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Start(); err != nil {
		_ = r.Close()
		_ = w.Close()
		p.abortTurn()
		return fmt.Errorf("cannot start %s: %w", binary(), err)
	}
	// Our own copy of the write end must close now: the child (and any
	// grandchild it backgrounds and forgets, see Setpgid above) holds its
	// OWN copy from the fork, and until every copy closes, a read on r
	// never sees EOF - including ours, which would otherwise keep the pipe
	// open forever even after the child exits normally.
	_ = w.Close()

	once := &sync.Once{}
	p.mu.Lock()
	p.running = cmd
	if cmd.Process != nil {
		p.pids = append(p.pids, cmd.Process.Pid)
		if len(p.pids) > pane.PidKeep {
			p.pids = p.pids[len(p.pids)-pane.PidKeep:]
		}
	}
	p.readEnd = r
	p.closeReadEnd = once
	// The spawn has genuinely succeeded now - see turnArgv's own comment
	// for why this is a one-way latch set here, after success, rather than
	// a counter incremented before it.
	p.sessionEstablished = true
	p.mu.Unlock()

	go p.runTurn(cmd, r, once)
	return nil
}

// runTurn reads one turn's process to completion, tails the session tree
// while it runs, and reports the turn's own terminal state.
//
// cmd.Wait is called exactly once, AFTER the output scan loop returns - the
// documented safe order for a pipe this code manages itself (see Prompt's
// own comment). A grandchild that backgrounds work and holds r's write end
// open would otherwise keep this loop blocked forever with no way for THIS
// goroutine to unstick it: unsticking a wedged read is Stop's job, not
// Wait's - Stop owns r too, through the same closeR passed in here, and
// forces it closed itself once it has waited a bounded amount of time and
// decided nothing else is going to end this turn.
func (p *proc) runTurn(cmd *exec.Cmd, r *os.File, closeR *sync.Once) {
	defer p.turns.Done()

	treePath := filepath.Join(p.sessionDir, p.currentSessionID()+".jsonl")
	tailDone := make(chan struct{})
	tailFinished := make(chan struct{})
	go func() {
		defer close(tailFinished)
		p.tailTurn(treePath, tailDone)
	}()

	var captured strings.Builder
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		captured.Write(line)
		captured.WriteByte('\n')
	}
	scanErr := sc.Err()

	requested := func() bool {
		select {
		case <-p.stop:
			return true
		default:
			return false
		}
	}

	// Close our own reference to r now that reading is done - ordinary
	// hygiene, since a long-lived pane runs many turns and each gets its
	// own pipe - through the SAME Once Stop may also reach for (see
	// Prompt), so whichever side gets there first wins and the other's
	// Close is a harmless no-op.
	closeR.Do(func() { _ = r.Close() })

	stopped := requested()
	if scanErr != nil && !stopped {
		p.send(pane.Event{Kind: pane.EvError, Detail: "stream read failed: " + scanErr.Error()})
	}

	werr := cmd.Wait()

	// Belt alongside WaitDelay: kill whatever remains of this turn's whole
	// process group. Harmless if nothing remains in it (ESRCH, ignored).
	if cmd.Process != nil && cmd.Process.Pid > 1 {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	// The turn's own process has now fully exited. Read the tree ONE more
	// time before reporting anything: sprig can append a row and then exit
	// fast enough that the last TailInterval poll never saw it, and the
	// turn's final rows must never be lost to that race. tailTurn's own
	// last act, once tailDone closes, is exactly that catch-up read.
	close(tailDone)
	<-tailFinished

	stopped = requested() // re-check: Wait can take a moment
	if stopped {
		// Stop killed this turn. That is not this turn's own failure to
		// report - the pane is already ending.
		p.endTurn()
		return
	}

	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	p.endTurn()

	if exitCode != 0 {
		// sprig's own real message - a session-tree refusal, or whatever
		// else cli.go printed to stderr - is what was captured, not a bare
		// exit code alone. Only the single LAST non-banner line, though
		// (lastUsefulLine's own comment says why), and the exit code is
		// always appended too, the same "name it regardless" shape
		// codex.go's own turn-ended-without-completion diagnostic uses:
		// a reader should never have to choose between sprig's own words
		// and the fact that matters most.
		detail := lastUsefulLine(captured.String())
		if detail == "" {
			if werr != nil {
				detail = werr.Error()
			} else {
				detail = "sprig exited"
			}
		}
		p.send(pane.Event{Kind: pane.EvError, Detail: fmt.Sprintf("%s exit=%d", detail, exitCode)})
		return
	}
	p.send(pane.Event{Kind: pane.EvState, State: pane.StateIdleStr, Detail: "turn complete"})
}

// tailTurn polls the session tree while one turn's process is alive, and
// does one final read the instant it is told the turn ended (done closing)
// or the whole pane is stopping, before returning.
func (p *proc) tailTurn(path string, done <-chan struct{}) {
	t := time.NewTicker(TailInterval)
	defer t.Stop()
	for {
		select {
		case <-done:
			p.pollTree(path)
			return
		case <-p.stop:
			p.pollTree(path)
			return
		case <-t.C:
			p.pollTree(path)
		}
	}
}

// pollTree reads whatever is new in the session tree since the last poll
// (any turn's), advancing the offset this adapter carries across turns.
func (p *proc) pollTree(path string) {
	p.mu.Lock()
	offset := p.tailOffset
	p.mu.Unlock()
	newOffset := p.readNewRows(path, offset)
	p.mu.Lock()
	p.tailOffset = newOffset
	p.mu.Unlock()
}

// maxTailReadPerPoll bounds one readNewRows call: a --resume onto a large
// pre-existing session tree must not read the whole file in one poll. The
// tail offset persists across polls (TailInterval), so a file bigger than
// this just takes a few more polls to catch up instead of one unbounded
// read and parse.
const maxTailReadPerPoll = 8 << 20

// readNewRows reads whatever sprig has appended to path since offset and
// returns the new offset. It only advances past a line that actually ends
// in a newline: a row still being written (no trailing \n yet) is left for
// the next poll rather than read half-written, unlike a bufio.Scanner used
// directly on the tail, which would happily hand back a torn final line at
// EOF as though it were complete.
//
// If the file is now SMALLER than offset - truncated or replaced somehow,
// never expected in ordinary operation but not something to wedge on if it
// happens - the offset resets to 0 rather than seeking past EOF forever.
func (p *proc) readNewRows(path string, offset int64) int64 {
	f, err := os.Open(path)
	if err != nil {
		return offset // the file appears once sprig writes its header
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() < offset {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset
	}
	b, err := io.ReadAll(io.LimitReader(f, maxTailReadPerPoll))
	if err != nil {
		return offset
	}
	start := 0
	for i, c := range b {
		if c != '\n' {
			continue
		}
		line := b[start:i]
		start = i + 1
		if len(line) == 0 {
			continue
		}
		for _, ev := range p.parseRow(line) {
			p.send(ev)
		}
	}
	return offset + int64(start)
}

// treeRow is the subset of harness/sprig/session_tree.go's row shapes this
// adapter reads: the "session" header, "prompt", "assistant", "tool_call",
// "verdict" and "tool_result" - the six types SessionWriter ever appends
// (session_tree.go:100-205). Every row also carries id/parentId/ts, which
// this adapter has no use for: a pane's transcript does not need the tree's
// own DAG shape, only what each row says.
type treeRow struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	Text     string `json:"text"`
	Name     string `json:"name"`
	Detail   string `json:"detail"`
	Decision string `json:"decision"`
	Clause   string `json:"clause"`
	Reason   string `json:"reason"`
	Summary  string `json:"summary"`
	OK       *bool  `json:"ok"`
}

// parseRow turns one session-tree line into zero or more events.
func (p *proc) parseRow(line []byte) []pane.Event {
	var r treeRow
	if err := json.Unmarshal(line, &r); err != nil {
		return []pane.Event{{Kind: pane.EvError,
			Detail: "cannot read a sprig session row: " + err.Error()}}
	}
	switch r.Type {
	case "session":
		// The header. Pure handshake, same as claude's system/init line: it
		// names the session but is not transcript content, so it earns no
		// visible grid row of its own. Reconciling sessionID here is still
		// worth doing even though Start (and every turn's own argv) already
		// fixed it: it is what makes SessionID() correct if a real sprig
		// ever picked a different id than the one asked for.
		p.mu.Lock()
		p.sessionID = r.ID
		p.mu.Unlock()
		return nil
	case "prompt":
		return []pane.Event{{Kind: pane.EvState, State: pane.StateWorkingStr, Detail: "prompt"}}
	case "assistant":
		if r.Text == "" {
			// A turn whose only content this adapter does not model (a tool
			// call with no accompanying text, say) must still leave a
			// trace rather than vanish silently.
			return []pane.Event{{Kind: pane.EvState, State: pane.StateWorkingStr}}
		}
		return []pane.Event{{Kind: pane.EvText, Text: r.Text}}
	case "tool_call":
		return []pane.Event{{Kind: pane.EvTool, Tool: r.Name, Detail: r.Detail}}
	case "verdict":
		if r.Decision == "deny" {
			// This IS the real gate verdict the package doc points to:
			// blocked, with the clause attached, because sprig's tree
			// carries a fact it produced, not because this adapter is
			// guessing at a terminal.
			clause := r.Clause
			if clause == "" {
				clause = r.Reason
			}
			return []pane.Event{{Kind: pane.EvState, State: pane.StateBlockedStr,
				Detail: "verdict=deny clause=" + clause}}
		}
		return []pane.Event{{Kind: pane.EvState, State: pane.StateWorkingStr,
			Detail: "verdict=allow"}}
	case "tool_result":
		text := r.Summary
		if r.OK != nil && !*r.OK {
			text = "[tool failed] " + r.Summary
		}
		return []pane.Event{{Kind: pane.EvText, Text: text}}
	default:
		// A row type this adapter does not model yet. It parsed fine as
		// JSON, so this is not the "broken stream" case rule (d) is about -
		// that is EvError above, for a line that could not be read as JSON
		// at all. An unmodelled-but-valid row just means sprig's tree grew
		// a shape this adapter has not caught up to, and it fails LOUDLY,
		// by naming the type visibly, never idle - the same choice
		// claude's and codex's own default cases make.
		detail := r.Type
		if detail == "" {
			detail = "unrecognized session-tree row"
		}
		return []pane.Event{{Kind: pane.EvState, State: pane.StateWorkingStr, Detail: detail}}
	}
}

func (p *proc) currentSessionID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessionID
}

// send hands one event to the relay through p.in. It never touches the
// events channel itself, so it can never race the relay's close of it.
func (p *proc) send(ev pane.Event) {
	select {
	case <-p.stop:
	case p.in <- ev:
	}
}

// stoppedErr reports whether this pane has already ended, either because
// Stop was called (p.stop) or because relay already finished and closed
// events (p.relayDone).
func (p *proc) stoppedErr() error {
	select {
	case <-p.stop:
		return fmt.Errorf("sprig: this pane has stopped")
	case <-p.relayDone:
		return fmt.Errorf("sprig: this pane has stopped")
	default:
		return nil
	}
}

// Steer has no equivalent for a one-process-per-turn harness: there is no
// running process to interrupt between turns, and interrupting the one
// mid-turn is not something sprig exposes headlessly. Saying so honestly is
// better than silently sending a prompt and calling it steering.
func (p *proc) Steer(string) error {
	return fmt.Errorf("sprig: %w. Send the next prompt instead.", pane.ErrUnsupported)
}

// WriteStdin has no meaning here: each turn's process is not a long-lived
// conversation partner the way claude's is, so there is no persistent
// stdin worth writing to - sprig's own task travels as a positional
// argument, not on stdin. A pane that has already stopped gets the same
// honest "this pane has stopped" every other adapter gives, ahead of the
// unsupported answer.
func (p *proc) WriteStdin([]byte) error {
	if err := p.stoppedErr(); err != nil {
		return err
	}
	return fmt.Errorf("sprig: %w. Use agent prompt.", pane.ErrUnsupported)
}

// Events always returns the same channel: pumpAdapter's drain step calls it
// again after Stop, and a different channel there would silently stop
// draining the one it was already reading.
func (p *proc) Events() <-chan pane.Event { return p.events }

// PidProc makes this adapter a pane.PidSource: its proc names its pids.
func (adapter) PidProc() pane.Pider { return &proc{} }

// Pids names the root pid of every turn this pane ran, so the server can
// place a process a turn starts, even one left behind after the turn
// ended, as belonging to this pane.
func (p *proc) Pids() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.pids...)
}

func (p *proc) SessionID() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessionID, p.sessionID != ""
}

// Stop signals and kills. It does NOT close the events channel itself:
// relay owns that, and closing it here would race a turn goroutine
// mid-send into a closed channel. stopOnce makes a second Stop a safe
// no-op past the first.
//
// The kill below reaches whatever IS still tracked in p.running - nothing,
// if this turn's slot already freed before Stop got here, in which case
// p.cancel() and the turn's own exit are what end it instead. Either way,
// this gives the turn goroutine a bounded ~1s to finish on its own - the
// common case, once its process (and any grandchild sharing its process
// group, thanks to Setpgid) is dead. If it has not by then, force it: close
// the read end THIS turn owns, through the SAME sync.Once its own runTurn
// uses, so whichever side gets there first wins. That makes the blocked
// scanner in runTurn return an error instead of waiting on a pipe nothing
// in this process tree is going to close, and runTurn proceeds to Wait
// from there exactly as it would on any other exit.
func (p *proc) Stop() error {
	p.stopOnce.Do(func() {
		p.mu.Lock()
		close(p.stop)
		cmd := p.running
		p.mu.Unlock()

		p.cancel()
		if cmd != nil && cmd.Process != nil && cmd.Process.Pid > 1 {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}

		select {
		case <-p.relayDone:
		case <-time.After(time.Second):
			p.mu.Lock()
			r, once := p.readEnd, p.closeReadEnd
			p.mu.Unlock()
			if r != nil && once != nil {
				once.Do(func() { _ = r.Close() })
			}
		}
	})
	<-p.relayDone
	return nil
}

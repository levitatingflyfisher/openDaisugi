// Package codex runs Codex headlessly and normalises its stream-json into
// coppice events. Unlike Claude, Codex is one process per turn: `codex exec
// --json` runs, streams JSONL, and exits. The adapter keeps one event channel
// across turns and resumes with a recorded session id once it has seen one,
// so a client sees one continuous conversation - the process exiting after
// every turn is not the session ending.
//
// WIRE FORMAT IS UNVERIFIED ON THIS BOX: there is no codex binary here to
// record a real session against, so both the fixtures this package's tests
// replay and the resume argv below are built from published `codex exec
// --json` documentation, not from a captured trace. Two published
// vocabularies exist and disagree with each other (an older session_id /
// session.created / assistant_message shape, and a newer thread_id /
// thread.started / agent_message shape); this adapter accepts BOTH rather
// than betting on one, and every wireLine field and case below says which
// vocabulary it belongs to. A message type this adapter has never seen
// leaves a visible "working" row naming the type (see parse's default case)
// rather than vanishing - if the real wire format turns out to disagree with
// both vocabularies once a codex binary exists to check against, that
// disagreement will show up in a pane's transcript instead of hiding.
// Whether a resumed turn can actually read the rollout file the PREVIOUS
// turn's own tail (still tearing down, see runTurn) has not finished
// flushing is unverified for the same reason: nothing here has a real codex
// to race against.
//
// A turn's own process ending is only sometimes the turn ending cleanly:
// turn.completed and turn.failed both close it out (the latter with its
// reason, if the wire line carries one), but a process that exits - crashes,
// or simply stops sending JSON - without either is still handled: it must
// not leave a client's pane looking like it is still "working" forever, so
// that case gets its own idle event naming the exit code. See runTurn's own
// comment for the full reasoning, including why an EXPLICIT Stop() suppresses
// this rather than adding a redundant diagnostic on top of the pane already
// ending.
//
// Only one turn may run at a time: Prompt refuses outright while another is
// in flight, rather than queueing or overlapping it. See beginTurn.
//
// This adapter never emits state "blocked". Permission decisions do not
// surface on this stream at all - they go through the gate hook
// (COPPICE_SOCK/COPPICE_PANE, wired by rule (a) of pane.Adapter), which is
// what reports blocked, with source gate, when it has something to report.
//
// This adapter also never emits EvEnd. adapter.go's rule (d) treats EvEnd as
// terminal, and the server's pump reacts to it by calling Stop() and
// draining - correct for claude's one long-lived process, wrong here, where
// every turn's process exits on its own and the pane keeps taking prompts.
// Only Stop() ends a codex pane, and the events channel closing (owned
// solely by relay, never by Stop itself) is how a client learns that.
//
// Codex's own hooks are a fail-open class (spec-01 keeps that honesty tag), so
// a coppice pane running Codex is a soft gate. Nothing here changes that.
//
// Model text (Event.Text) is written into the pane's grid raw, unescaped -
// rule (d) of pane.Adapter requires this file to say which choice it made,
// same as claude's own adapter: a model that happened to emit coppice's own
// "[end]"/"[error]" marker text could forge a marker row, so a reader must
// treat those markers as advisory, not authoritative, for a codex pane too.
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/opendaisugi/coppice/internal/adapters"
	"github.com/opendaisugi/coppice/internal/pane"
)

// Binary is the command run for a headless Codex turn. COPPICE_CODEX_BIN
// overrides it, which is how the tests run without the real binary.
const Binary = "codex"

func binary() string {
	if b := os.Getenv("COPPICE_CODEX_BIN"); b != "" {
		return b
	}
	return Binary
}

// resumeArgv builds the argv for resuming an existing codex session, per
// published `codex exec --json` docs: `codex exec resume <id> --json
// <prompt>`. It is a package-level VARIABLE, not a plain function, precisely
// because that shape is unverified (see the package doc) - a later
// correction, once a real binary exists to check against, needs no change
// anywhere else that calls it. The "--" ahead of prompt is not shown in the
// published example; it is added anyway, same as the fresh-run argv below,
// so a prompt that happens to start with "-" is never misread as a flag.
//
// extra carries the pane's own StartOpts.Argv (--model, a sandbox flag,
// ...). This used to be dropped on the resume path -
// only the fresh-run branch spliced it in - and Start seeds p.sessionID from
// o.Resume, so a pane RESTORED with a recorded session id takes this path
// on its very first prompt and silently never got its own cmd_argv. Both
// paths must carry it, same as claude carries o.Argv on both of its paths.
var resumeArgv = func(id string, extra []string, prompt string) []string {
	argv := []string{"exec", "resume", id, "--json"}
	argv = append(argv, extra...)
	return append(argv, "--", prompt)
}

type adapter struct{}

// New returns the codex adapter. See the package doc for the one-process-
// per-turn shape this is built around.
func New() pane.Adapter { return adapter{} }

func (adapter) Name() string { return "codex" }

func init() { adapters.Register(New()) }

// eventsBufferSize is the events channel's buffer capacity. It is a var, not
// a const, only so TestStopWhileTheStreamIsFlowingDoesNotPanic can shrink it
// to force relay's blocked-send branches to actually run under -race,
// instead of them sitting untested because the real 256-slot buffer never
// fills for a small fixture - the same trick claude's own test file uses on
// its equivalent var.
var eventsBufferSize = 256

// inBufferSize is p.in's own buffer capacity, the hand-off channel every
// turn goroutine sends into before relay forwards to events. A var for the
// same reason as eventsBufferSize: a test can shrink it to force send's own
// blocked-send path to actually run, rather than it sitting untested behind
// a 256-slot buffer no small fixture ever fills.
var inBufferSize = 256

// proc keeps ONE event channel across turns even though a new child process
// runs for every one. Exactly one goroutine ever closes that channel: relay,
// below, after Stop has been signalled and every turn's own goroutine has
// finished sending. Stop signals and kills; it never closes a channel a
// turn goroutine may still be sending on - a send on a closed channel
// panics, and a panic in an adapter goroutine takes the whole daemon down
// with every other pane's process orphaned alongside it.
type proc struct {
	ctx    context.Context
	cancel context.CancelFunc
	opts   pane.StartOpts

	in        chan pane.Event // turn goroutines send here
	events    chan pane.Event // clients read here; closed by relay, by nobody else
	stop      chan struct{}   // closed by Stop, once
	stopOnce  sync.Once
	relayDone chan struct{} // closed by relay, BEFORE events - see relay's doc comment

	mu           sync.Mutex
	sessionID    string
	running      *exec.Cmd  // the turn currently in flight, for Stop to kill
	pids         []int      // the root pid of every turn, newest last, up to pane.PidKeep
	turnActive   bool       // claimed the instant beginTurn succeeds, well before running is set
	readEnd      *os.File   // this turn's own end of the stdout pipe (see Prompt) - Stop can force it shut
	closeReadEnd *sync.Once // shared with runTurn so whichever side closes readEnd first wins
	turns        sync.WaitGroup
}

func (adapter) Start(ctx context.Context, o pane.StartOpts, _ *pane.Grid) (pane.Proc, error) {
	// Rule (b) of pane.Adapter: the server owns rendering. g is accepted and
	// ignored, same as claude and sprig.
	ctx, cancel := context.WithCancel(ctx)
	p := &proc{
		ctx: ctx, cancel: cancel, opts: o,
		in:        make(chan pane.Event, inBufferSize),
		events:    make(chan pane.Event, eventsBufferSize),
		stop:      make(chan struct{}),
		relayDone: make(chan struct{}),
	}
	if o.Resume != "" {
		// A resumed pane already knows its session id; codex's own
		// session.created/thread.started line on a resumed run only
		// confirms it.
		p.sessionID = o.Resume
	}
	go p.relay()
	return p, nil
}

// relay is the single owner of the events channel: the only goroutine that
// ever sends on it or closes it. It forwards whatever turn goroutines hand it
// through p.in until Stop is signalled, waits for every turn goroutine to
// actually finish (so a turn mid-send never races a close), makes one
// best-effort pass to flush anything already queued, and only then closes.
//
// It closes relayDone BEFORE events - the opposite of claude's order, and
// deliberately so: codex has several turn goroutines across the pane's life,
// not claude's one long-lived producer, so a caller that reacts to Events()
// closing must be able to trust that relayDone is already closed by that
// moment, not racing to catch up a few instructions later.
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
			// Let the turn already in flight finish sending, then flush
			// whatever it queued, best-effort, and finish.
			p.turns.Wait()
			for {
				select {
				case ev := <-p.in:
					select {
					case p.events <- ev:
					default:
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
// so, claims one before releasing the lock. Two refusals share this one
// check: a pane that has been asked to stop starts nothing further, and only
// one turn may run at a time, so turnActive - claimed here, well before
// running is ever set - can never let a second Prompt clobber the first
// turn's *exec.Cmd. Stop's own close of p.stop happens under this same lock
// (see Stop), so the two can never interleave: either this runs first and
// the turn is counted before Stop closes p.stop, or Stop's close runs first
// and this observes it and claims nothing. Without that shared lock,
// turns.Add racing turns.Wait (relay, reacting to the same close) would be
// exactly the kind of sync.WaitGroup misuse its own docs warn against - a
// call with a positive delta must happen before the Wait it is meant to be
// waited for, not merely be attempted around the same time.
//
// The actual spawn happens AFTER this returns, without holding p.mu:
// cmd.Start can block briefly on the OS, and there is no reason to make Stop
// wait behind it. That stays safe even if Stop races in right there, because
// the child is started via exec.CommandContext(p.ctx, ...): if p.ctx is
// already cancelled by the time Start runs, Start fails outright and nothing
// is spawned; if it is cancelled a moment after Start succeeds, the exec
// package's own watcher goroutine kills the child as soon as it observes
// ctx.Done, whether or not this turn's cmd ever made it into p.running for
// Stop's own kill to find.
func (p *proc) beginTurn() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.stop:
		return fmt.Errorf("codex: this pane has stopped")
	default:
	}
	if p.turnActive {
		return fmt.Errorf("codex: a turn is already running; wait for idle")
	}
	p.turnActive = true
	p.turns.Add(1)
	return nil
}

// endTurn releases the claim beginTurn took, so the next Prompt can succeed.
func (p *proc) endTurn() {
	p.mu.Lock()
	p.turnActive = false
	p.running = nil
	p.mu.Unlock()
}

// abortTurn is endTurn plus turns.Done, for the early-failure paths in
// Prompt where no goroutine ever starts to call turns.Done on its own.
func (p *proc) abortTurn() {
	p.endTurn()
	p.turns.Done()
}

// stoppedErr reports whether this pane has already ended, either because
// Stop was called (p.stop) or because relay already finished and closed
// events (p.relayDone). Guarding on both, not just p.stop, matters because a
// caller can observe Events() closing before Stop was ever called from this
// side (a pump reacting to a closed channel, say) - beginTurn's own stop
// check would still refuse a new turn in that case since relay only reaches
// relayDone after p.stop closes, but Prompt and WriteStdin need the same
// honest answer without paying for a full beginTurn/endTurn pair just to ask.
func (p *proc) stoppedErr() error {
	select {
	case <-p.stop:
		return fmt.Errorf("codex: this pane has stopped")
	case <-p.relayDone:
		return fmt.Errorf("codex: this pane has stopped")
	default:
		return nil
	}
}

// Prompt starts a new codex exec --json process for this one turn. It
// resumes the recorded session once one exists, so a client sees one
// continuous conversation across turns that each get their own process. It
// refuses outright, via beginTurn, if a turn is already running or the pane
// has stopped - never queueing, never overlapping two turns.
func (p *proc) Prompt(text string) error {
	if err := p.beginTurn(); err != nil {
		return err
	}

	p.mu.Lock()
	sessionID := p.sessionID
	p.mu.Unlock()

	var argv []string
	if sessionID != "" {
		argv = resumeArgv(sessionID, p.opts.Argv, text)
	} else {
		argv = append([]string{"exec", "--json"}, p.opts.Argv...)
		argv = append(argv, "--", text)
	}

	cmd := exec.CommandContext(p.ctx, binary(), argv...)
	cmd.Dir = p.opts.Cwd
	// Rule (a) of pane.Adapter: the only path by which the gate hook finds
	// COPPICE_SOCK and COPPICE_PANE, for this turn's process same as every
	// other adapter's spawn.
	cmd.Env = pane.BuildEnv(os.Environ(), p.opts.Env, p.opts.Sock, p.opts.PaneID, p.opts.DataDir)
	// Setpgid makes this turn's pid its own pgid too (no session/controlling
	// terminal needed, unlike the pty side's Setsid - just a distinct group
	// to signal). A harness that backgrounds work and exits (`x & exit 0`)
	// leaves that work as a grandchild holding this turn's stdout pipe open;
	// a whole-group kill (Stop, and runTurn's own matching cleanup) reaches
	// it, where killing just cmd.Process would not. WaitDelay stays set as
	// the belt Cmd's own doc names for "a child process that fails to exit
	// after the associated Context is canceled" - a process that ignores
	// the kill this ctx cancellation triggers. This 2s value is also the
	// bound on the grandchild-holds-stdout wedge itself: a backgrounded job
	// this turn's direct child never waits for cannot hold cmd.Wait open
	// past it, Setpgid's own group kill above notwithstanding.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second

	// The adapter owns this pipe outright - os.Pipe, not cmd.StdoutPipe -
	// because Cmd.Wait's own documented contract for StdoutPipe ("it is
	// incorrect to call Wait before all reads from the pipe have completed")
	// cannot be honoured AND have Wait unstick a grandchild-held pipe at the
	// same time. Calling Wait concurrently, before the scan loop
	// finishes, gets Wait's eager pipe-close to unstick such
	// a read - but breaks the documented order doing it: Wait's close races
	// this turn's own still-in-progress reads and can fire on an entirely
	// ordinary fast exit, no grandchild involved, dropping tail lines and
	// misreporting a broken stream (~1 in 20-40 runs of
	// TestTurnEndingWithoutCompletionNamesTheExitCode in isolation on this
	// box). Owning the pipe moves "unstick a wedged read" to Stop, which
	// already knows when it is safe to force one closed (see Stop's own
	// comment) - Wait goes back to running once, after the scan loop, the
	// order StdoutPipe's own doc calls correct.
	r, w, perr := os.Pipe()
	if perr != nil {
		p.abortTurn()
		return perr
	}
	cmd.Stdout = w
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		_ = r.Close()
		_ = w.Close()
		p.abortTurn()
		return fmt.Errorf("cannot start %s: %w", binary(), err)
	}
	// Our own copy of the write end must close now: the child (and any
	// grandchild it backgrounds and forgets, see Setpgid above) holds its
	// OWN copy from the fork, and until every copy closes, a read on r never
	// sees EOF - including ours, which would otherwise keep the pipe open
	// forever even after the child exits normally.
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
	p.mu.Unlock()

	go p.runTurn(cmd, r, once)
	return nil
}

// runTurn reads one turn's process to completion and reports it. Sending
// events never touches the events channel directly (see send): the relay is
// the only thing allowed to close it, so this goroutine only ever hands
// events to relay through p.in.
//
// cmd.Wait is called exactly once, AFTER the scan loop returns - the
// documented safe order for a pipe this code manages itself (see Prompt's
// own comment on why the adapter owns r/w via os.Pipe rather than
// cmd.StdoutPipe). A grandchild that backgrounds work and holds r's write
// end open (see Prompt's comment on Setpgid) would otherwise keep this loop
// blocked forever with no way for THIS goroutine to unstick it: unsticking
// a wedged read is Stop's job (see Stop's own comment), not Wait's - Stop
// owns r too, through the same closeR passed in here, and forces it closed
// itself once it has waited a bounded amount of time and decided nothing
// else is going to end this turn.
func (p *proc) runTurn(cmd *exec.Cmd, r *os.File, closeR *sync.Once) {
	defer p.turns.Done()

	completed := false
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		evs, done := p.parse(sc.Bytes())
		if done && !completed {
			completed = true
			// Free the turn slot THE INSTANT the turn logically ends - a
			// turn.completed or turn.failed JSON line - not when the
			// underlying process eventually exits. bash keeps this turn's
			// stdout pipe open for a little while after its last line is
			// written (it is still running, checking COPPICE_CODEX_EXIT and
			// tearing down), so waiting for EOF or Wait here would gate a
			// client's next Prompt on that OS-level teardown delay even
			// though the client already saw this turn's terminal event and
			// is entitled to send another. endTurn happens-before the
			// terminal event is sent below, and the events channel is what
			// a caller actually synchronizes on, so a Prompt issued the
			// instant a client observes that event can never race this.
			//
			// The !completed guard makes this fire at most once: a second
			// done line (malformed, but not this adapter's job to police)
			// must not call endTurn again after a NEW turn may already have
			// claimed the slot this one just freed.
			p.endTurn()
		}
		for _, ev := range evs {
			p.send(ev)
		}
	}
	scanErr := sc.Err()
	// Close our own reference to r now that reading is done - ordinary
	// hygiene, since a long-lived pane runs many turns and each gets its own
	// pipe - through the SAME Once Stop may also reach for (see Prompt),
	// so whichever side gets there first wins and the other's Close is a
	// harmless no-op.
	closeR.Do(func() { _ = r.Close() })

	// A read error here is either a genuinely broken stream, or Stop forcing
	// r closed to unstick a read that nothing else was going to end. Stop
	// closing r IS what unblocked this loop in the second case - that is a
	// quiet end, not a fault worth an EvError row.
	stopping := false
	select {
	case <-p.stop:
		stopping = true
	default:
	}
	if scanErr != nil && !stopping {
		p.send(pane.Event{Kind: pane.EvError, Detail: "stream read failed: " + scanErr.Error()})
	}
	if !completed {
		// The stream ended without ever sending a terminal signal at all -
		// free the slot now that no more lines are coming, same reasoning
		// as above, just discovered at EOF (or Stop's forced close) instead
		// of mid-stream.
		p.endTurn()
	}

	// Belt alongside WaitDelay: WaitDelay only bounds a process that fails
	// to exit after ctx cancellation, not a grandchild's own lifetime - a
	// backgrounded job would otherwise keep running behind a pane that looks
	// idle. Setpgid at spawn (see Prompt) makes this turn's pid its own
	// pgid, so a whole-group signal reaches it; the pid>1 guard matches
	// Stop's own (see Stop) - never signal pgid 0, -1, or 1. Harmless if
	// nothing remains in the group: killing an empty/already-dead group is
	// ESRCH, ignored.
	//
	// This runs BEFORE Wait, not after: Wait reaps the process, and once
	// reaped, the kernel is free to hand this same pid to an unrelated new
	// process. A kill issued after that reap could land on that unrelated
	// process's group instead of this turn's own, a real if narrow hazard
	// that no unit test can force deterministically - it depends on the
	// kernel's own pid allocator racing this goroutine. Killing the group
	// first, while the pid is still certainly this turn's own, closes that
	// window by construction.
	if cmd.Process != nil && cmd.Process.Pid > 1 {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	werr := cmd.Wait()

	// Sample the stop signal AFTER Wait, not before: Wait can take a moment,
	// and a Stop that arrives while it is running still means this exit was
	// requested, not a surprise. Checking earlier risked naming a spurious
	// exit=-1 (Go's ExitCode for "killed by signal") on a turn Stop killed a
	// heartbeat after this goroutine had already decided "not requested".
	requested := false
	select {
	case <-p.stop:
		requested = true
	default:
	}

	if requested {
		// Stop killed this turn's process. That is not this turn's own
		// failure to report - the pane is already ending, and the package
		// doc explains why that never becomes an EvEnd either way.
		return
	}
	if !completed {
		// The process ended without ever sending turn.completed or
		// turn.failed - a crash, or a turn that simply stopped talking.
		// Leaving the pane looking like it is still "working" forever is
		// the wedge this exists to prevent: report idle, naming the exit
		// code so a nonzero one is not silently lost.
		detail := "turn ended without completion exit=-1"
		switch {
		case cmd.ProcessState != nil:
			detail = fmt.Sprintf("turn ended without completion exit=%d", cmd.ProcessState.ExitCode())
		case werr != nil:
			detail = "turn ended without completion: " + werr.Error()
		}
		p.send(pane.Event{Kind: pane.EvState, State: pane.StateIdleStr, Detail: detail})
	}
}

// send hands one event to the relay through p.in. It never touches the
// events channel itself, so it can never race the relay's close of it.
func (p *proc) send(ev pane.Event) {
	select {
	case <-p.stop:
	case p.in <- ev:
	}
}

// wireLine is the subset of codex exec --json's line shapes this adapter
// reads, across BOTH published vocabularies (see the package doc):
// session.created/thread.started (an id), item.started/item.updated/
// item.completed (an item's lifecycle), and turn.completed/turn.failed (the
// turn is over, one way or the other).
type wireLine struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	ThreadID  string `json:"thread_id"`
	Item      struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Command  string `json:"command"`
		ExitCode *int   `json:"exit_code"`
	} `json:"item"`
	Error   *wireReason `json:"error"`
	Message string      `json:"message"`
}

// wireReason is turn.failed's nested {"error":{"message":"..."}} shape, one
// of two places a failure reason might travel (see reason).
type wireReason struct {
	Message string `json:"message"`
}

// id is whichever of the two vocabularies' identifiers this line carries.
func (w wireLine) id() string {
	if w.SessionID != "" {
		return w.SessionID
	}
	return w.ThreadID
}

// reason is turn.failed's own explanation, when the wire line carries one
// either nested under error.message or as a bare top-level message.
func (w wireLine) reason() string {
	if w.Error != nil && w.Error.Message != "" {
		return w.Error.Message
	}
	return w.Message
}

// parse turns one codex exec --json line into zero or more events, and
// reports whether this line closed the turn out (turn.completed or
// turn.failed) - runTurn uses that to know whether the process exiting
// afterward is expected or needs its own diagnostic. Codex emits one
// complete item per line rather than per-token deltas for item.completed;
// item.updated is the per-delta shape, and is deliberately ignored (see its
// own case) so this still amounts to one grid row per adapter.go rule (d).
func (p *proc) parse(line []byte) ([]pane.Event, bool) {
	var w wireLine
	if err := json.Unmarshal(line, &w); err != nil {
		return []pane.Event{{Kind: pane.EvError,
			Detail: "cannot read a codex JSON line: " + err.Error()}}, false
	}
	if id := w.id(); id != "" {
		p.mu.Lock()
		p.sessionID = id
		p.mu.Unlock()
	}

	switch w.Type {
	case "session.created", "thread.started":
		return []pane.Event{{Kind: pane.EvState, State: pane.StateWorkingStr,
			Detail: "session " + w.id()}}, false

	case "item.started", "item.updated":
		// No row. item.updated is codex's per-delta shape - a message still
		// being written - and adapter.go rule (d) wants one event per
		// completed message, not one per token; item.started names nothing
		// worth a grid row before there is any content at all.
		return nil, false

	case "item.completed":
		switch w.Item.Type {
		case "assistant_message", "agent_message":
			return []pane.Event{{Kind: pane.EvText, Text: w.Item.Text}}, false
		case "command_execution":
			d := w.Item.Command
			if w.Item.ExitCode != nil {
				d = fmt.Sprintf("%s (exit %d)", d, *w.Item.ExitCode)
			}
			return []pane.Event{{Kind: pane.EvTool, Tool: "shell", Detail: d}}, false
		case "error":
			detail := w.Item.Text
			if detail == "" {
				detail = "item error"
			}
			return []pane.Event{{Kind: pane.EvError, Detail: detail}}, false
		default:
			if w.Item.Text != "" {
				// An item type this adapter does not otherwise recognise,
				// but one that still carries text. Surfacing the words
				// beats losing them to an "unmodelled" row that only names
				// the type.
				return []pane.Event{{Kind: pane.EvText, Text: w.Item.Text}}, false
			}
			// Unmodelled and textless. It parsed fine as JSON, so this is
			// not the "broken stream" case rule (d) is about; it just means
			// codex is still doing something this adapter has no shape for
			// yet, never idle - and it fails LOUDLY, by naming the type
			// visibly, rather than vanishing silently on a wire mismatch.
			return []pane.Event{{Kind: pane.EvState, State: pane.StateWorkingStr,
				Detail: w.Item.Type}}, false
		}

	case "turn.completed":
		// The turn ended cleanly. Codex is waiting on us now, and the
		// process exiting because of that is not the session ending - see
		// the package doc: another prompt starts another process, same pane.
		return []pane.Event{{Kind: pane.EvState, State: pane.StateIdleStr, Detail: "turn complete"}}, true

	case "turn.failed":
		// The turn ended badly, but it still ENDED: the pane must not read
		// as stuck "working" just because it was a failure rather than a
		// success. idle, not an error state, matches turn.completed's own
		// shape - the reason lives in Detail either way.
		detail := "turn failed"
		if r := w.reason(); r != "" {
			detail += ": " + r
		}
		return []pane.Event{{Kind: pane.EvState, State: pane.StateIdleStr, Detail: detail}}, true

	case "error":
		// A stream-level fault, not item.completed's own error shape. The
		// reason travels the same two ways turn.failed's own wireLine.reason
		// already reads: nested under error.message, or a bare top-level
		// message. Falling back to the literal type name only when neither
		// is present keeps this loud rather than silent, never blank.
		detail := w.Type
		if r := w.reason(); r != "" {
			detail = r
		}
		return []pane.Event{{Kind: pane.EvError, Detail: detail}}, false

	default:
		// A message shape this adapter does not model at all yet - neither
		// vocabulary names it. Visible and working, never idle, so a real
		// wire mismatch (this whole package is UNVERIFIED - see the package
		// doc) fails loudly in a pane's transcript instead of hiding.
		return []pane.Event{{Kind: pane.EvState, State: pane.StateWorkingStr, Detail: w.Type}}, false
	}
}

// Steer has no equivalent for a one-process-per-turn harness: there is no
// running process to interrupt between turns, and interrupting the one
// mid-turn is not something codex exec --json exposes headlessly. Saying so
// honestly is better than silently sending a prompt and calling it steering.
func (p *proc) Steer(string) error {
	return fmt.Errorf("codex: %w. Send the next prompt instead.", pane.ErrUnsupported)
}

// WriteStdin has no meaning here even while a turn is running: each turn's
// process is not a long-lived conversation partner the way claude's is, so
// there is no persistent stdin worth writing to. A pane that has already
// stopped gets the same honest "this pane has stopped" every other adapter
// gives, ahead of the unsupported answer, so a caller checking for "has this
// pane ended" gets one consistent error regardless of which write-shaped
// call it made.
func (p *proc) WriteStdin([]byte) error {
	if err := p.stoppedErr(); err != nil {
		return err
	}
	return fmt.Errorf("codex: %w. Use agent prompt.", pane.ErrUnsupported)
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

// Stop signals and kills. It does NOT close the events channel itself: relay
// owns that, and closing it here would race a turn goroutine mid-send into a
// closed channel. Closing p.stop under the same lock beginTurn takes is what
// makes that check race-free (see beginTurn's own doc comment). stopOnce
// makes a second Stop - the pump's own terminal-close path, or a caller
// calling it directly after an adapter-sent error - a safe no-op past the
// first.
//
// The kill below reaches whatever IS still tracked in p.running - nothing,
// if this turn's slot already freed (endTurn already ran; see its own doc
// comment) before Stop got here, in which case p.cancel() and the turn's own
// exit are what end it instead. Either way, this gives the turn goroutine a
// bounded ~1s to finish on its own - the common case, once its process (and
// any grandchild sharing its process group, thanks to Setpgid - see Prompt)
// is dead. If it has not by then - a grandchild NOT in that process group,
// say - force it: close the read end THIS turn owns (see Prompt), through
// the SAME sync.Once its own runTurn uses, so whichever side gets there
// first wins. That makes the blocked scanner in runTurn return an error
// instead of waiting on a pipe nothing in this process tree is going to
// close, and runTurn proceeds to Wait from there exactly as it would on any
// other exit.
//
// The pid>1 guard on the process-group signal matters: -0 would signal THIS
// process's own group (Setpgid gives every turn a distinct one, but a
// zero-value Process could not), -1 would signal every process this caller
// can see, and -1 (pid==1, init) would reach whatever process group init
// itself leads - none of those is ever what "kill this one turn" should
// mean.
//
// Waiting on relayDone, not on events, is deliberate: relay closes relayDone
// first and events second (the opposite of claude's order - see relay's doc
// comment), so by the time Stop returns here, relayDone is guaranteed closed
// but events may still close a moment later. A caller that needs events
// itself closed keeps reading it until it reports ok == false, same as every
// test in this package does.
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

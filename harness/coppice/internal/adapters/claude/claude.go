// Package claude runs Claude Code headlessly and normalises its stream-json
// into coppice events. The gate still decides: an event here says working, and
// a source: gate report saying blocked outranks it.
//
// This adapter never emits state "blocked". In -p mode, Claude's own
// permission decisions do not surface on this stream at all - they go through
// the gate hook (COPPICE_SOCK/COPPICE_PANE, wired by rule (a) below), which is
// what reports blocked, with source gate, when it has something to report. No
// recorded fixture this package's tests replay carries a permission message,
// so no wire shape for one has been invented here; a claude pane's only path
// to blocked is the gate, not this file.
//
// Model text (Event.Text) is written into the pane's grid raw, unescaped -
// rule (d) of pane.Adapter requires this file to say which choice it made.
// Claude's own stream-json content is not escaped against coppice's own
// "[end]"/"[error]" marker lines before it reaches WriteTranscript, so a
// model that happened to emit that literal text could forge a marker row; a
// reader must treat those markers as advisory, not authoritative, for a
// claude pane exactly as adapter.go already warns.
package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/opendaisugi/coppice/internal/adapters"
	"github.com/opendaisugi/coppice/internal/pane"
)

// eventsBufferSize is the events channel's buffer capacity. It is a var, not
// a const, only so TestStopWhileTheStreamIsFlowingDoesNotPanic can shrink it
// to force relay's blocked-send branch (the "case <-p.stop" guarding
// p.events <- ev) to actually run under -race, instead of that branch sitting
// untested because the real 256-slot buffer never fills for a small fixture.
var eventsBufferSize = 256

// Binary is the command run for a headless Claude pane. COPPICE_CLAUDE_BIN
// overrides it, which is how the tests run without a subscription.
//
// The argv this adapter runs is `-p --output-format stream-json
// --input-format stream-json --verbose`. That --verbose is required: without
// it, `claude -p --output-format stream-json --input-format stream-json`
// prints "Error: When using --print, --output-format=stream-json requires
// --verbose" and refuses to run at all (verified against the real binary on
// this box). Do not drop it.
const Binary = "claude"

func binary() string {
	if b := os.Getenv("COPPICE_CLAUDE_BIN"); b != "" {
		return b
	}
	return Binary
}

type adapter struct{}

// New returns the claude adapter. Claude Code runs as one long-lived process
// for the whole pane - unlike Codex, which runs one process per turn - so
// Prompt only ever writes another stdin line to the same child.
func New() pane.Adapter { return adapter{} }

func (adapter) Name() string { return "claude" }

// The claude adapter can fork a session. The server checks this at
// pane.fork.
var _ pane.Forker = adapter{}

// ForkArgv returns the flag that makes a resumed claude process branch into
// a new session instead of continuing the old one. Start already adds
// --resume from StartOpts.Resume, so only --fork-session is extra. An empty
// session id is refused: claude would start a fresh session and nothing
// would be forked.
func (adapter) ForkArgv(sessionID string) ([]string, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("claude cannot fork without a session id")
	}
	return []string{"--fork-session"}, nil
}

func init() { adapters.Register(New()) }

// proc wraps the one long-lived claude process for this pane. Exactly one
// goroutine, relay, ever sends on events or closes it - see relay's own doc
// comment for why that is safe with only one producer, unlike codex/sprig
// which run one process per turn and need an extra hand-off channel to keep
// the same guarantee with several producers.
type proc struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	stdin  io.WriteCloser

	events    chan pane.Event // the relay is the only sender and the only closer
	stop      chan struct{}   // closed by Stop, once
	stopOnce  sync.Once
	relayDone chan struct{} // closed by relay when events is closed

	mu        sync.Mutex
	sessionID string
}

func (adapter) Start(ctx context.Context, o pane.StartOpts, _ *pane.Grid) (pane.Proc, error) {
	// Rule (b): the server owns rendering. g is accepted and ignored.
	ctx, cancel := context.WithCancel(ctx)

	argv := []string{
		"-p",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
	}
	if o.Resume != "" {
		argv = append(argv, "--resume", o.Resume)
	}
	argv = append(argv, o.Argv...)

	cmd := exec.CommandContext(ctx, binary(), argv...)
	cmd.Dir = o.Cwd
	// Rule (a): this is the only path by which the gate hook finds
	// COPPICE_SOCK and COPPICE_PANE.
	cmd.Env = pane.BuildEnv(os.Environ(), o.Env, o.Sock, o.PaneID)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("cannot start %s: %w", binary(), err)
	}

	p := &proc{
		cmd: cmd, cancel: cancel, stdin: stdin,
		events:    make(chan pane.Event, eventsBufferSize),
		stop:      make(chan struct{}),
		relayDone: make(chan struct{}),
	}
	if o.Resume != "" {
		// A resumed pane already knows its session id; the init line the real
		// binary would send again on resume only confirms it.
		p.sessionID = o.Resume
	}
	go p.relay(stdout)
	return p, nil
}

// relay is the single producer that reads claude's stdout, parses it, and is
// the only goroutine that ever sends on p.events or closes it. That one-
// producer property is what makes closing p.events from here race-free: no
// other goroutine ever sends on it, so nothing can race a send against this
// function's own close.
//
// Stop does not close p.events itself, for the same reason adapter.go states
// as a hard rule: a Stop that closed the channel while this loop was still
// mid-send would panic, and a panic in an adapter goroutine takes the whole
// daemon down with every other pane's process orphaned alongside it. Instead
// Stop closes p.stop, cancels the context, and kills the process; this loop
// notices p.stop (either between events or, if the events buffer is ever
// full, via the select guarding the send itself) and unwinds on its own,
// still the only closer.
func (p *proc) relay(stdout io.Reader) {
	// relayDone closes before events, not after: stoppedErr checks
	// relayDone, and a goroutine reacting to Events() closing must already
	// see stoppedErr() as non-nil, never a window where events has closed
	// but relayDone has not caught up yet.
	defer close(p.events)
	defer close(p.relayDone)

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)

readLoop:
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		for _, ev := range p.parse(line) {
			select {
			case p.events <- ev:
			case <-p.stop:
				break readLoop
			}
		}
	}
	if err := sc.Err(); err != nil {
		select {
		case p.events <- pane.Event{Kind: pane.EvError, Detail: "stream read failed: " + err.Error()}:
		case <-p.stop:
		}
	}

	// A Stop the caller asked for is not a surprise; note that BEFORE Wait,
	// since Wait can take a moment and Stop could still arrive concurrently -
	// this way "requested" only ever means "asked before we decided", never
	// a race against the very Stop that is about to kill the process anyway.
	requested := false
	select {
	case <-p.stop:
		requested = true
	default:
	}

	// The stream is over, either because claude exited on its own or because
	// Stop killed it. Either way this pane's process is done; make sure it
	// actually is, and reap it so it does not become a zombie.
	p.cancel()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	// Wait always populates ProcessState once the process was actually
	// started, which Start already guarantees by the time relay runs - so
	// Wait's own returned error, once distinct from a nil ProcessState, is
	// unreachable here and named nothing the ProcessState branch below does
	// not already cover.
	_ = p.cmd.Wait()

	detail := "claude exited"
	if !requested && p.cmd.ProcessState != nil {
		// Nobody asked for this: name why, the same way the pty side's own
		// watchExit does (internal/server/panes.go, "exit=%d"), so a reader
		// can tell a crash from a clean stop instead of seeing identical
		// text for both.
		detail = fmt.Sprintf("claude exited: exit=%d", p.cmd.ProcessState.ExitCode())
	}

	end := pane.Event{Kind: pane.EvEnd, State: pane.StateDoneStr, Detail: detail}
	select {
	case p.events <- end:
	case <-p.stop:
		// Stop already fired and nobody may still be draining; delivering
		// EvEnd here is best-effort, not a promise.
	}
}

// wireLine is the subset of claude's stream-json shapes this adapter reads:
// system/init (session id), assistant (text and tool_use content blocks),
// user (tool_result content blocks), and result (end of turn).
type wireLine struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	Result    string `json:"result"`
	Message   struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// contentBlock is one element of message.content when that field is an array.
type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

// contentBlocks decodes message.content, which claude's wire format sends as
// either an array of typed blocks or, for a plain-text-only message, a bare
// string. The bare string is not a shape this adapter fails to read - it is
// exactly one text block, so it decodes to one. Only a THIRD shape - neither
// an array of blocks nor a string - is an error.
func contentBlocks(raw json.RawMessage) ([]contentBlock, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return blocks, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []contentBlock{{Type: "text", Text: s}}, nil
	}
	return nil, fmt.Errorf("message.content is neither an array of blocks nor a string")
}

// parse turns one stream-json line into zero or more events. Claude's
// stream-json emits one COMPLETE message per line rather than per-token
// deltas, so one line naturally becomes one grid row per rule (d); there is
// no delta buffering to do here.
func (p *proc) parse(line []byte) []pane.Event {
	var w wireLine
	if err := json.Unmarshal(line, &w); err != nil {
		return []pane.Event{{
			Kind:   pane.EvError,
			Detail: "cannot read a stream-json line: " + err.Error(),
		}}
	}

	switch w.Type {
	case "system":
		if w.SessionID != "" {
			p.mu.Lock()
			p.sessionID = w.SessionID
			p.mu.Unlock()
		}
		if w.Subtype == "init" {
			// Pure handshake: it names the session, but it is not transcript
			// content, so it earns no visible grid row - only assistant
			// output and tool activity do.
			return nil
		}
		return []pane.Event{{Kind: pane.EvState, State: pane.StateWorkingStr, Detail: w.Subtype}}

	case "assistant":
		blocks, err := contentBlocks(w.Message.Content)
		if err != nil {
			return []pane.Event{{Kind: pane.EvError, Detail: "cannot read message.content: " + err.Error()}}
		}
		var out []pane.Event
		var unmodelled []string
		for _, c := range blocks {
			switch c.Type {
			case "text":
				out = append(out, pane.Event{Kind: pane.EvText, Text: c.Text})
			case "tool_use":
				// The id travels in Detail (pane.Event has no dedicated field
				// for it) so a reader can match this row to the later
				// tool-result row, which already names the same id via its
				// own tool_use_id.
				detail := string(c.Input)
				if c.ID != "" {
					detail = c.ID + " " + detail
				}
				out = append(out, pane.Event{Kind: pane.EvTool, Tool: c.Name, Detail: detail})
			default:
				unmodelled = append(unmodelled, c.Type)
			}
		}
		if len(out) == 0 {
			// A message whose every block is something this adapter does not
			// model (thinking, say) must still leave a trace rather than
			// vanish silently.
			out = append(out, pane.Event{Kind: pane.EvState, State: pane.StateWorkingStr,
				Detail: strings.Join(unmodelled, ",")})
		}
		return out

	case "user":
		blocks, err := contentBlocks(w.Message.Content)
		if err != nil {
			return []pane.Event{{Kind: pane.EvError, Detail: "cannot read message.content: " + err.Error()}}
		}
		var out []pane.Event
		for _, c := range blocks {
			if c.Type == "tool_result" {
				out = append(out, pane.Event{
					Kind: pane.EvText,
					Text: "tool result " + c.ToolUseID + ": " + string(c.Content),
				})
			}
		}
		return out

	case "result":
		// The turn is over. Claude is waiting on us now, which is idle - but
		// only when the turn actually succeeded. Any other subtype (the
		// fixture only ever carries "success") fails closed to unknown
		// rather than guessing what a subtype it has never seen means.
		if w.Subtype == "success" {
			return []pane.Event{{Kind: pane.EvState, State: pane.StateIdleStr, Detail: w.Result}}
		}
		return []pane.Event{{Kind: pane.EvError, Detail: "result subtype " + w.Subtype}}

	default:
		// A message shape this adapter does not model yet. It parsed fine as
		// JSON, so this is not the "broken stream" case rule (d)'s fail-closed
		// language is about - that is EvError above, for JSON this adapter
		// could not read at all. An unmodelled-but-valid message just means
		// claude is still doing something.
		return []pane.Event{{Kind: pane.EvState, State: pane.StateWorkingStr, Detail: w.Type}}
	}
}

// stoppedErr reports whether this pane has already ended, either because
// Stop was called (p.stop) or because the child exited on its own and relay
// already finished (p.relayDone). Both cases share the same error: a caller
// cannot write to a process that is not there to read it, and only checking
// p.stop would let a write past a natural exit reach a closed stdin pipe and
// surface a raw broken-pipe error instead of this honest one.
func (p *proc) stoppedErr() error {
	select {
	case <-p.stop:
		return fmt.Errorf("claude: this pane has stopped")
	case <-p.relayDone:
		return fmt.Errorf("claude: this pane has stopped")
	default:
		return nil
	}
}

// Prompt writes exactly one JSON user message line. Claude's stream-json
// input format frames on newlines, so an embedded newline would split one
// prompt into two lines and the second half would be read back as a
// malformed line of its own; json.Marshal escapes any newline in text as \n
// rather than emitting one, so that cannot happen here.
func (p *proc) Prompt(text string) error {
	if err := p.stoppedErr(); err != nil {
		return err
	}
	msg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]any{{"type": "text", "text": text}},
		},
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = p.stdin.Write(append(b, '\n'))
	return err
}

// Steer has no equivalent in Claude Code's headless mode: there is no
// side-channel to interrupt a turn already in flight. Saying so honestly is
// better than silently sending a prompt and calling it steering.
func (p *proc) Steer(string) error {
	return fmt.Errorf("claude: %w. Send a prompt instead.", pane.ErrUnsupported)
}

// WriteStdin shares Prompt's stoppedErr guard: without it, a write after the
// pane has ended would still fail (the pipe is closed or has no reader by
// then), but with a raw broken-pipe error instead of the same honest answer
// Prompt gives.
func (p *proc) WriteStdin(b []byte) error {
	if err := p.stoppedErr(); err != nil {
		return err
	}
	_, err := p.stdin.Write(b)
	return err
}

// Events always returns the same channel: pumpAdapter's drain step calls it
// again after Stop, and a different channel there would silently stop
// draining the one it was already reading.
func (p *proc) Events() <-chan pane.Event { return p.events }

func (p *proc) SessionID() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessionID, p.sessionID != ""
}

// Stop signals, cancels and kills, then waits for the relay to actually close
// Events(): closing it here itself, instead of waiting for the relay to do
// it, is exactly the race rule (c) forbids. stopOnce makes a second Stop (the
// pump's own terminal-close path calls Stop again after an adapter-sent
// EvEnd; a caller may also call it directly) a safe no-op past the first.
func (p *proc) Stop() error {
	p.stopOnce.Do(func() {
		close(p.stop)
		p.cancel()
		_ = p.stdin.Close()
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
	})
	<-p.relayDone
	return nil
}

// Package pi drives `pi --mode rpc` as a headless coppice pane. pi's own
// permission dialogs arrive on this stream as extension_ui_request events,
// so unlike claude this adapter does emit blocked, with the ask attached; the
// operator's answer goes back through Prompt as an extension_ui_response.
//
// Facts about pi's wire shapes are recorded in
// src/opendaisugi/harness_pi/extension/PINS.md. Re-read that file before
// changing a shape here:
//   - Framing: LF-delimited JSONL, one command or event per line.
//   - {"type":"prompt","message":str} needs streamingBehavior while the agent
//     streams, or pi returns an error.
//   - {"type":"get_state"} answers data.sessionId and data.sessionFile.
//   - {"type":"switch_session","sessionPath":str} resumes by file path, never
//     by the bare session id.
//   - Dialog requests carry method select, confirm, input or editor and expect
//     an extension_ui_response. select, input and editor carry title; confirm
//     carries title and message. notify, setStatus, setWidget, setTitle and
//     set_editor_text never expect a response.
//
// Model text (Event.Text) is written into the pane's grid raw, unescaped,
// which is the choice rule 4 of pane.Adapter asks each adapter to state.
// pi's text deltas are coalesced into one Event per message here, at
// message_end, so the pump renders one grid row per message. A model that
// emitted the literal "[end]" or "[error]" text could forge a marker row, so
// a reader treats those markers as advisory for a pi pane.
package pi

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
	"time"

	"github.com/opendaisugi/coppice/internal/adapters"
	"github.com/opendaisugi/coppice/internal/pane"
	"github.com/opendaisugi/coppice/internal/proto"
)

// Binary is the command run for a headless pi pane. COPPICE_PI_BIN overrides
// it, and a test points adapter.Bin at a fake script.
const Binary = "pi"

// defaultAskDeadline is how long a blocked ask stays open for a floor client
// when the dialog request carries no timeout of its own.
const defaultAskDeadline = 90 * time.Second

const eventsBufferSize = 256

// adapter drives `pi --mode rpc`. Bin overrides the binary; empty means
// COPPICE_PI_BIN, else "pi" on PATH.
type adapter struct {
	Bin string
}

// New returns the pi adapter.
func New() pane.Adapter { return adapter{} }

func init() { adapters.Register(New()) }

func (adapter) Name() string { return "pi" }

func (a adapter) binary() string {
	if a.Bin != "" {
		return a.Bin
	}
	if b := os.Getenv("COPPICE_PI_BIN"); b != "" {
		return b
	}
	return Binary
}

// Start runs `pi --mode rpc` in o.Cwd. No --session-dir is set: StartOpts
// carries the socket path and no state root, and the two do not share a
// parent, so there is no honest place to derive one from. pi keeps its
// default session directory, and resume works by file path regardless. An
// operator who wants a custom directory passes --session-dir in o.Argv.
func (a adapter) Start(ctx context.Context, o pane.StartOpts, _ *pane.Grid) (pane.Proc, error) {
	// Rule 2: the server owns rendering. g is accepted and ignored.
	ctx, cancel := context.WithCancel(ctx)

	argv := []string{"--mode", "rpc"}
	argv = append(argv, o.Argv...)

	bin := a.binary()
	cmd := exec.CommandContext(ctx, bin, argv...)
	cmd.Dir = o.Cwd
	// Rule 1: the only path by which the gate extension finds COPPICE_SOCK
	// and COPPICE_PANE.
	cmd.Env = pane.BuildEnv(os.Environ(), o.Env, o.Sock, o.PaneID)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("pi adapter: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("pi adapter: stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("cannot start %s: %w", bin, err)
	}

	p := &proc{
		cmd: cmd, cancel: cancel, stdin: stdin,
		events:    make(chan pane.Event, eventsBufferSize),
		stop:      make(chan struct{}),
		relayDone: make(chan struct{}),
	}
	if o.Resume != "" {
		// A resumed pane knows its session file at once. The get_state that
		// follows a successful switch_session only confirms it.
		p.sessionFile = o.Resume
	}
	go p.relay(stdout)

	if o.Resume != "" {
		_ = p.send(map[string]any{"type": "switch_session", "sessionPath": o.Resume})
	} else {
		_ = p.send(map[string]any{"type": "get_state"})
	}
	return p, nil
}

// proc is one running `pi --mode rpc` process. Exactly one goroutine, relay,
// ever sends on events or closes it.
type proc struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	stdin  io.WriteCloser

	events    chan pane.Event // relay is the only sender and the only closer
	stop      chan struct{}   // closed by Stop, once
	stopOnce  sync.Once
	relayDone chan struct{} // closed by relay when events is closed

	mu            sync.Mutex
	writeMu       sync.Mutex
	pendingID     string
	pendingMethod string
	pendingUntil  time.Time // zero when the dialog carries no timeout
	sessionID     string
	sessionFile   string
	streaming     bool
}

// send writes one JSONL command. json.Marshal escapes any newline inside a
// string, so one command is always exactly one line.
func (p *proc) send(v map[string]any) error {
	if err := p.stoppedErr(); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_, err = p.stdin.Write(append(b, '\n'))
	return err
}

func (p *proc) stoppedErr() error {
	select {
	case <-p.stop:
		return fmt.Errorf("pi: this pane has stopped")
	case <-p.relayDone:
		return fmt.Errorf("pi: this pane has stopped")
	default:
		return nil
	}
}

func (p *proc) WriteStdin(b []byte) error {
	if err := p.stoppedErr(); err != nil {
		return err
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_, err := p.stdin.Write(b)
	return err
}

// Prompt answers the pending dialog when one is open, else sends a prompt.
// A dialog whose own timeout has passed is no longer open: pi has already
// resolved it with a default, so the text goes to pi as a prompt, not as a
// stale answer. While the agent streams the prompt carries streamingBehavior
// "steer": pi refuses a bare prompt during streaming. agent_start opens the
// streaming window and agent_settled closes it; agent_end alone may still be
// followed by a retry or a queued continuation.
func (p *proc) Prompt(text string) error {
	p.mu.Lock()
	if !p.pendingUntil.IsZero() && time.Now().After(p.pendingUntil) {
		p.clearPendingLocked()
	}
	pendingID := p.pendingID
	streaming := p.streaming
	p.mu.Unlock()
	if pendingID != "" {
		return p.answerPending(text)
	}
	req := map[string]any{"type": "prompt", "message": text}
	if streaming {
		req["streamingBehavior"] = "steer"
	}
	return p.send(req)
}

func (p *proc) Steer(text string) error {
	return p.send(map[string]any{"type": "steer", "message": text})
}

// Events always returns the same channel: pumpAdapter's drain step calls it
// again after Stop.
func (p *proc) Events() <-chan pane.Event { return p.events }

// SessionID returns the value Start's Resume must be fed for a real resume.
// pi's switch_session takes a sessionPath, the file path from get_state's
// sessionFile, and the bare sessionId makes the resume fail. So this returns
// sessionFile. The short id is kept for nothing else today.
func (p *proc) SessionID() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessionFile, p.sessionFile != ""
}

// Stop signals, cancels, closes stdin and kills, then waits for relay to
// close Events(). Closing the channel here would race the relay's own send,
// which rule 3 forbids. stopOnce makes a second Stop a no-op.
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

// clearPendingLocked forgets the open dialog. The caller holds p.mu.
func (p *proc) clearPendingLocked() {
	p.pendingID, p.pendingMethod, p.pendingUntil = "", "", time.Time{}
}

func (p *proc) answerPending(text string) error {
	p.mu.Lock()
	id, method := p.pendingID, p.pendingMethod
	p.clearPendingLocked()
	p.mu.Unlock()
	resp := map[string]any{"type": "extension_ui_response", "id": id}
	if method == "confirm" {
		lower := strings.ToLower(strings.TrimSpace(text))
		resp["confirmed"] = lower == "y" || lower == "yes"
	} else {
		resp["value"] = text
	}
	return p.send(resp)
}

func isDialogMethod(m string) bool {
	switch m {
	case "select", "confirm", "input", "editor":
		return true
	default:
		return false
	}
}

func strOf(v any) string {
	s, _ := v.(string)
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// relay is the single producer. It reads pi's stdout line by line, turns
// each line into zero or more events, and is the only goroutine that sends
// on p.events or closes it. Stop closes p.stop and kills the process; this
// loop notices and unwinds on its own, still the only closer.
func (p *proc) relay(stdout io.Reader) {
	defer close(p.events)
	defer close(p.relayDone)

	var text strings.Builder // text deltas of the message in flight
	emit := func(ev pane.Event) bool {
		select {
		case p.events <- ev:
			return true
		case <-p.stop:
			return false
		}
	}
	flushText := func() bool {
		if text.Len() == 0 {
			return true
		}
		ev := pane.Event{Kind: pane.EvText, Text: text.String()}
		text.Reset()
		return emit(ev)
	}

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)

readLoop:
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal(line, &raw); err != nil {
			if !emit(pane.Event{Kind: pane.EvError, Detail: "cannot read an rpc line: " + err.Error()}) {
				break readLoop
			}
			continue
		}
		switch strOf(raw["type"]) {
		case "message_update":
			if ame, ok := raw["assistantMessageEvent"].(map[string]any); ok && ame["type"] == "text_delta" {
				text.WriteString(strOf(ame["delta"]))
			}
		case "message_end":
			if !flushText() {
				break readLoop
			}
		default:
			if !flushText() {
				break readLoop
			}
			for _, ev := range p.parse(raw) {
				if !emit(ev) {
					break readLoop
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		emit(pane.Event{Kind: pane.EvError, Detail: "stream read failed: " + err.Error()})
	}
	flushText()

	requested := false
	select {
	case <-p.stop:
		requested = true
	default:
	}

	p.cancel()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	_ = p.cmd.Wait()

	detail := "pi exited"
	if !requested && p.cmd.ProcessState != nil {
		detail = fmt.Sprintf("pi exited: exit=%d", p.cmd.ProcessState.ExitCode())
	}
	emit(pane.Event{Kind: pane.EvEnd, State: pane.StateDoneStr, Detail: detail})
}

// parse turns one non-text rpc event into zero or more pane events, and
// updates the process metadata a later Prompt or SessionID reads.
func (p *proc) parse(raw map[string]any) []pane.Event {
	switch strOf(raw["type"]) {
	case "agent_start":
		p.mu.Lock()
		p.streaming = true
		p.mu.Unlock()
		return []pane.Event{{Kind: pane.EvState, State: pane.StateWorkingStr}}
	case "agent_end":
		return []pane.Event{{Kind: pane.EvState, State: pane.StateIdleStr}}
	case "agent_settled":
		// A dialog blocks the run, so a settled run has no open dialog.
		p.mu.Lock()
		p.streaming = false
		p.clearPendingLocked()
		p.mu.Unlock()
		return []pane.Event{{Kind: pane.EvState, State: pane.StateIdleStr}}
	case "tool_execution_start":
		args, _ := json.Marshal(raw["args"])
		detail := string(args)
		if id := strOf(raw["toolCallId"]); id != "" {
			detail = id + " " + detail
		}
		return []pane.Event{{Kind: pane.EvTool, Tool: strOf(raw["toolName"]), Detail: detail}}
	case "extension_ui_request":
		method := strOf(raw["method"])
		if !isDialogMethod(method) {
			return nil
		}
		id := strOf(raw["id"])
		summary := firstNonEmpty(strOf(raw["title"]), strOf(raw["message"]))
		// timeout is milliseconds. pi resolves the dialog itself when it
		// passes, so the ask deadline and the local expiry both follow it.
		var until time.Time
		deadline := time.Now().Add(defaultAskDeadline)
		if ms, ok := raw["timeout"].(float64); ok && ms > 0 {
			until = time.Now().Add(time.Duration(ms) * time.Millisecond)
			deadline = until
		}
		p.mu.Lock()
		p.pendingID, p.pendingMethod, p.pendingUntil = id, method, until
		p.mu.Unlock()
		return []pane.Event{{
			Kind: pane.EvState, State: pane.StateBlockedStr, Detail: summary,
			Ask: &proto.Ask{
				ID: id, Tool: method, Summary: summary,
				Deadline: float64(deadline.Unix()),
			},
		}}
	case "extension_error":
		detail := strOf(raw["error"])
		if ev := strOf(raw["event"]); ev != "" {
			detail = ev + ": " + detail
		}
		return []pane.Event{{Kind: pane.EvError, Detail: "extension error: " + detail}}
	case "response":
		switch strOf(raw["command"]) {
		case "get_state":
			if data, ok := raw["data"].(map[string]any); ok {
				p.mu.Lock()
				p.sessionID = strOf(data["sessionId"])
				if f := strOf(data["sessionFile"]); f != "" {
					p.sessionFile = f
				}
				if s, ok := data["isStreaming"].(bool); ok {
					p.streaming = s
				}
				p.mu.Unlock()
			}
		case "switch_session":
			// The switch landed. Ask pi for the file it now writes, so the
			// next resume is fed the current path and not a stale one.
			if ok, _ := raw["success"].(bool); ok {
				go func() { _ = p.send(map[string]any{"type": "get_state"}) }()
			}
		}
		return nil
	default:
		return nil
	}
}

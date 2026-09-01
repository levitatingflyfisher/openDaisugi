// Package opencode drives `opencode serve` as a headless coppice pane. The
// adapter starts one server per pane on a loopback port with a per-pane
// basic-auth password, creates or resumes one session on it, sends prompts
// with prompt_async and reads pane state from the /event stream. The gate
// plugin in src/opendaisugi/harness_opencode gates each tool call inside
// that server. This package never decides a tool call.
//
// Facts about OpenCode's endpoints and event shapes are recorded in
// PINS.md next to this file. Re-read it before changing a shape here.
//
// Model text, Event.Text, is written into the pane's grid raw, unescaped,
// which is the choice rule 4 of pane.Adapter asks each adapter to state.
// OpenCode streams text as deltas, and this adapter emits one Event per text
// part, when the part ends. A model that wrote the literal "[end]" or
// "[error]" text could forge a marker row, so a reader treats those markers
// as advisory for an OpenCode pane.
package opencode

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/opendaisugi/coppice/internal/adapters"
	"github.com/opendaisugi/coppice/internal/pane"
)

// Binary is the command run for a headless OpenCode pane. COPPICE_OPENCODE_BIN
// overrides it, and a test points adapter.Bin at a fake.
const Binary = "opencode"

// serverUser is the basic-auth user name the adapter sets and sends.
const serverUser = "opencode"

// readyTimeout bounds the wait for the listen line and for /global/health.
var readyTimeout = 20 * time.Second

// streamGrace is how long the relay waits for the process to exit after the
// event stream ends, before it calls the pane unknown.
const streamGrace = 300 * time.Millisecond

const eventsBufferSize = 256

// askTick is how often the relay looks for an ask past its deadline.
var askTick = time.Second

type adapter struct {
	Bin string
}

// New returns the OpenCode adapter.
func New() pane.Adapter { return adapter{} }

func init() { adapters.Register(New()) }

func (adapter) Name() string { return "opencode" }

// PidProc makes this adapter a pane.PidSource: its proc names its pids.
func (adapter) PidProc() pane.Pider { return &proc{} }

func (a adapter) binary() string {
	if a.Bin != "" {
		return a.Bin
	}
	if b := os.Getenv("COPPICE_OPENCODE_BIN"); b != "" {
		return b
	}
	return Binary
}

// serveArgs binds the server to loopback. With port 0 the server tries port
// 4096 first and a free port when 4096 is taken. The listen line names the
// port it bound. extra is the pane's own argv.
func serveArgs(extra []string) []string {
	return append([]string{"serve", "--hostname", "127.0.0.1", "--port", "0"}, extra...)
}

// pureEnv is the variable that makes OpenCode skip every external plugin,
// the gate plugin too. The --pure flag sets it.
const pureEnv = "OPENCODE_PURE"

// refusePure fails when the pane's argv asks OpenCode to skip its plugins.
// A pane that ran that way would run with no gate.
func refusePure(argv []string) error {
	for _, a := range argv {
		if a == "--pure" || strings.HasPrefix(a, "--pure=") {
			return errors.New("opencode: --pure makes OpenCode skip the gate plugin. Remove --pure from the pane's argv")
		}
	}
	return nil
}

// baseEnv is the inherited environment without OPENCODE_PURE.
func baseEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		if strings.HasPrefix(kv, pureEnv+"=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// opencodeEnv copies the pane's env and sets the server's credentials in the
// copy. The pane env can hold anything else, but never the password or the
// user name, so a pane cannot break the adapter's own auth, and never
// OPENCODE_PURE, so a pane cannot turn the gate plugin off.
func opencodeEnv(optsEnv map[string]string, token string) map[string]string {
	env := make(map[string]string, len(optsEnv)+2)
	for k, v := range optsEnv {
		env[k] = v
	}
	delete(env, pureEnv)
	env["OPENCODE_SERVER_PASSWORD"] = token
	env["OPENCODE_SERVER_USERNAME"] = serverUser
	return env
}

var listenRe = regexp.MustCompile(`listening on (\S+)`)

// parseListenLine reads the base URL out of the server's listen line. It
// accepts only plain http on 127.0.0.1 with a port and no path, so the
// adapter never sends its password anywhere else.
func parseListenLine(line string) (string, error) {
	m := listenRe.FindStringSubmatch(line)
	if m == nil {
		return "", fmt.Errorf("opencode: not a listen line: %q", line)
	}
	u, err := url.Parse(m[1])
	if err != nil {
		return "", fmt.Errorf("opencode: bad listen address %q: %w", m[1], err)
	}
	port, perr := strconv.Atoi(u.Port())
	if u.Scheme != "http" || u.Hostname() != "127.0.0.1" || perr != nil || port <= 0 ||
		(u.Path != "" && u.Path != "/") || u.User != nil || u.RawQuery != "" {
		return "", fmt.Errorf("opencode: the server listens on %s. The adapter talks only to http://127.0.0.1:<port>. Remove --hostname from the pane's argv", m[1])
	}
	return "http://127.0.0.1:" + strconv.Itoa(port), nil
}

// gatePluginPath is where the server will load the gate plugin from, read
// from the environment it starts with: $XDG_CONFIG_HOME/opencode/plugins
// when XDG_CONFIG_HOME is absolute, else $HOME/.config/opencode/plugins.
func gatePluginPath(env []string) (string, error) {
	var home, xdg string
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "HOME":
			home = v
		case "XDG_CONFIG_HOME":
			xdg = v
		}
	}
	var base string
	switch {
	case filepath.IsAbs(xdg):
		base = xdg
	case filepath.IsAbs(home):
		base = filepath.Join(home, ".config")
	default:
		return "", errors.New("opencode: neither XDG_CONFIG_HOME nor HOME names a config directory, so the adapter cannot find the gate plugin")
	}
	return filepath.Join(base, "opencode", "plugins", "daisugi-gate.ts"), nil
}

// gatePluginSHA256 is the SHA-256 of the gate plugin the Python package
// ships, src/opendaisugi/harness_opencode/plugin/daisugi-gate.ts. A test
// fails when the two differ.
const gatePluginSHA256 = "bb30ce696666fa97b983c6032f00d4c04606715463f9fa9b4110986b4b8cd9a2"

// maxPluginBytes bounds the read of the plugin file.
const maxPluginBytes = 1 << 20

// requireGatePlugin fails unless the file where the server will look for
// the gate plugin is the plugin this coppice ships, byte for byte. A
// missing, edited or old copy would leave OpenCode ungated or gated by
// code no one reviewed.
func requireGatePlugin(env []string) error {
	p, err := gatePluginPath(env)
	if err != nil {
		return err
	}
	fix := "Run: daisugi install --harness opencode"
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 || st.Size() > maxPluginBytes {
		return fmt.Errorf("opencode: the gate plugin is not at %s, so OpenCode would run ungated. %s", p, fix)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return fmt.Errorf("opencode: cannot read the gate plugin at %s: %v. %s", p, err, fix)
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != gatePluginSHA256 {
		return fmt.Errorf("opencode: the gate plugin at %s is not the one this coppice ships. %s", p, fix)
	}
	return nil
}

func randomToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("opencode: make a password: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Start runs `opencode serve` in o.Cwd, waits until it answers, and creates
// a session, or resumes o.Resume when the server still has it. Rule 2: the
// server owns rendering, so g is accepted and ignored.
func (a adapter) Start(ctx context.Context, o pane.StartOpts, _ *pane.Grid) (pane.Proc, error) {
	if err := refusePure(o.Argv); err != nil {
		return nil, err
	}
	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)

	bin := a.binary()
	cmd := exec.CommandContext(ctx, bin, serveArgs(o.Argv)...)
	cmd.Dir = o.Cwd
	// Rule 1: the only path by which the gate plugin finds COPPICE_SOCK and
	// COPPICE_PANE.
	cmd.Env = pane.BuildEnv(baseEnv(os.Environ()), opencodeEnv(o.Env, token), o.Sock, o.PaneID, o.DataDir)
	if err := requireGatePlugin(cmd.Env); err != nil {
		cancel()
		return nil, err
	}
	cmd.Stderr = os.Stderr
	// A pipe of our own, not StdoutPipe: Wait must not wait on a reader, and
	// a child of the server that keeps stdout open must not hold Wait.
	outR, outW, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("opencode: stdout pipe: %w", err)
	}
	cmd.Stdout = outW
	if err := cmd.Start(); err != nil {
		cancel()
		outR.Close()
		outW.Close()
		return nil, fmt.Errorf("cannot start %s: %w", bin, err)
	}
	outW.Close()

	p := &proc{
		cmd: cmd, ctx: ctx, cancel: cancel,
		events:    make(chan pane.Event, eventsBufferSize),
		stop:      make(chan struct{}),
		relayDone: make(chan struct{}),
		exited:    make(chan struct{}),
		rejectErr: make(chan string, 16),
	}
	go func() {
		_ = cmd.Wait()
		close(p.exited)
	}()

	fail := func(err error) (pane.Proc, error) {
		cancel()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-p.exited
		outR.Close()
		return nil, err
	}

	base, err := p.waitListen(outR)
	if err != nil {
		return fail(err)
	}
	c := newClient(base, serverUser, token)
	readyCtx, readyCancel := context.WithTimeout(ctx, readyTimeout)
	err = c.waitReady(readyCtx)
	readyCancel()
	if err != nil {
		return fail(err)
	}

	var initial []pane.Event
	sessionID := ""
	if o.Resume != "" && c.sessionExists(ctx, o.Resume) {
		sessionID = o.Resume
	} else {
		if o.Resume != "" {
			initial = append(initial, pane.Event{Kind: pane.EvError,
				Detail: fmt.Sprintf("opencode has no session %s. This pane starts a new one.", o.Resume)})
		}
		if sessionID, err = c.createSession(ctx, "coppice pane"); err != nil {
			return fail(err)
		}
	}
	resp, err := c.openEvents(ctx)
	if err != nil {
		return fail(err)
	}

	p.client = c
	p.sessionID = sessionID
	p.paneID = o.PaneID
	p.tr = newTranslator(sessionID, time.Now)
	// A fresh server runs nothing until the first prompt, so the pane is
	// idle once the session exists and the stream is open.
	initial = append(initial, pane.Event{Kind: pane.EvState, State: pane.StateIdleStr})
	go p.relay(resp.Body, initial)
	return p, nil
}

// waitListen reads stdout until the listen line, then keeps reading it into
// nothing, so the server never blocks on a full pipe. It fails when the
// process exits first or readyTimeout passes.
func (p *proc) waitListen(out io.ReadCloser) (string, error) {
	type result struct {
		base string
		err  error
	}
	found := make(chan result, 1)
	go func() {
		defer out.Close()
		sc := bufio.NewScanner(out)
		sent := false
		for sc.Scan() {
			if sent {
				continue
			}
			if !strings.Contains(sc.Text(), "listening on") {
				continue
			}
			base, err := parseListenLine(sc.Text())
			found <- result{base, err}
			sent = true
		}
		if !sent {
			found <- result{"", errors.New("opencode: the server closed stdout before it listened")}
		}
	}()
	select {
	case r := <-found:
		return r.base, r.err
	case <-p.exited:
		return "", errors.New("opencode: the server exited before it listened")
	case <-time.After(readyTimeout):
		return "", fmt.Errorf("opencode: the server did not listen within %s", readyTimeout)
	}
}

// proc is one running `opencode serve` and the session this pane owns on it.
// Exactly one goroutine, relay, ever sends on events or closes it.
type proc struct {
	cmd    *exec.Cmd
	ctx    context.Context
	cancel context.CancelFunc
	client *client
	tr     *translator

	sessionID string
	paneID    string

	events    chan pane.Event // relay is the only sender and the only closer
	stop      chan struct{}   // closed by Stop, once
	stopOnce  sync.Once
	relayDone chan struct{} // closed by relay when events is closed
	exited    chan struct{} // closed when the server process has exited
	rejectErr chan string   // a failed deadline reject, for relay to report
}

func (p *proc) stoppedErr() error {
	select {
	case <-p.stop:
		return errors.New("opencode: this pane has stopped")
	case <-p.relayDone:
		return errors.New("opencode: this pane has stopped")
	default:
		return nil
	}
}

// Prompt is PromptFrom with no operator behind the text.
func (p *proc) Prompt(text string) error { return p.PromptFrom(text, false) }

// PromptFrom sends text as a prompt. A typed prompt never answers
// OpenCode's permission prompt: while one is open the prompt is refused
// and the error names the agent.allow and agent.deny lines that answer it.
// While a question is open only the operator's text answers it. A pane's
// text is refused.
func (p *proc) PromptFrom(text string, operator bool) error {
	if err := p.stoppedErr(); err != nil {
		return err
	}
	perms, questions := p.tr.open()
	if len(perms) > 0 {
		id, pn := perms[0], p.paneName()
		return fmt.Errorf("opencode waits on its permission prompt %s. A prompt does not answer it. "+
			"Run: coppice agent allow %s %s, or: coppice agent deny %s %s", id, pn, id, pn, id)
	}
	if len(questions) > 0 {
		if !operator {
			return errors.New("opencode waits on a question for the operator. A pane cannot answer it")
		}
		a, ok := p.tr.get(questions[0])
		if !ok {
			return p.client.promptAsync(p.ctx, p.sessionID, text)
		}
		if a.count == 1 {
			if err := p.client.replyQuestion(p.ctx, a.id, [][]string{{text}}); err != nil {
				return err
			}
			p.tr.remove(a.id)
			return nil
		}
		// A question with several parts cannot take one typed line. It is
		// rejected, and the text goes out as the next prompt.
		if err := p.client.rejectQuestion(p.ctx, a.id); err != nil {
			return err
		}
		p.tr.remove(a.id)
	}
	return p.client.promptAsync(p.ctx, p.sessionID, text)
}

func (p *proc) paneName() string {
	if p.paneID != "" {
		return p.paneID
	}
	return "PANE"
}

// OwnsAsk reports whether id is an open ask of this pane's OpenCode.
func (p *proc) OwnsAsk(id string) bool { return p.tr != nil && p.tr.owns(id) }

// Answer answers one open ask. The server calls it for agent.allow and
// agent.deny only, after its role checks. An allow replies once, never
// always: a lasting rule belongs in OpenCode's own config. A question
// cannot be allowed, only denied, since it needs a typed answer. The ask is
// forgotten only after OpenCode takes the answer.
func (p *proc) Answer(id string, allow bool, reason string) error {
	if err := p.stoppedErr(); err != nil {
		return err
	}
	a, ok := p.tr.get(id)
	if !ok {
		return fmt.Errorf("opencode has no open ask %s", id)
	}
	var err error
	switch {
	case a.kind == "question" && allow:
		return fmt.Errorf("ask %s is a question. Type the answer into the pane", id)
	case a.kind == "question":
		err = p.client.rejectQuestion(p.ctx, id)
	case allow:
		err = p.client.replyPermission(p.ctx, id, "once", "")
	default:
		err = p.client.replyPermission(p.ctx, id, "reject", reason)
	}
	if err != nil {
		return err
	}
	p.tr.remove(id)
	return nil
}

// rejectExpired rejects an ask no one answered by its deadline. When the
// reject fails, OpenCode still waits on the ask and nothing here can answer
// it, so relay reports the pane unknown with the reason.
func (p *proc) rejectExpired(a openAsk) {
	var err error
	if a.kind == "question" {
		err = p.client.rejectQuestion(p.ctx, a.id)
	} else {
		err = p.client.replyPermission(p.ctx, a.id, "reject", "no answer from the operator before the deadline")
	}
	if err == nil {
		return
	}
	detail := fmt.Sprintf("ask %s passed its deadline and the reject failed: %v. OpenCode may still wait on it", a.id, err)
	select {
	case p.rejectErr <- detail:
	case <-p.stop:
	}
}

// Steer sends a follow-up message. OpenCode queues it behind the turn that
// runs now.
func (p *proc) Steer(text string) error {
	if err := p.stoppedErr(); err != nil {
		return err
	}
	return p.client.promptAsync(p.ctx, p.sessionID, text)
}

// WriteStdin has no meaning here: the adapter drives OpenCode over HTTP.
func (p *proc) WriteStdin([]byte) error {
	if err := p.stoppedErr(); err != nil {
		return err
	}
	return fmt.Errorf("opencode: %w. Send a prompt instead.", pane.ErrUnsupported)
}

// Events always returns the same channel: pumpAdapter's drain step calls it
// again after Stop.
func (p *proc) Events() <-chan pane.Event { return p.events }

// SessionID returns the OpenCode session id, the value Start's Resume takes.
func (p *proc) SessionID() (string, bool) { return p.sessionID, p.sessionID != "" }

// Pids names the server process, so the server can place a process OpenCode
// starts as a child of this pane. It is empty when there is none.
func (p *proc) Pids() []int {
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	return []int{p.cmd.Process.Pid}
}

// Stop signals, cancels and kills, then waits for relay to close Events().
// Closing the channel here would race the relay's own send, which rule 3
// forbids. stopOnce makes a second Stop a no-op.
func (p *proc) Stop() error {
	p.stopOnce.Do(func() {
		close(p.stop)
		p.cancel()
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
	})
	<-p.relayDone
	return nil
}

// pump sends the initial events, then each translated bus event, until the
// process exits or Stop is called. It returns when the pane is over.
func (p *proc) pump(lines <-chan busEvent, streamErr *error, initial []pane.Event, emit func(pane.Event) bool) {
	for _, ev := range initial {
		if !emit(ev) {
			return
		}
	}
	tick := time.NewTicker(askTick)
	defer tick.Stop()
	for in := lines; ; {
		select {
		case <-tick.C:
			for _, a := range p.tr.expire() {
				go p.rejectExpired(a)
			}
		case detail := <-p.rejectErr:
			if !emit(pane.Event{Kind: pane.EvState, State: pane.StateUnknownStr, Detail: detail}) {
				return
			}
		case ev, ok := <-in:
			if !ok {
				in = nil
				select {
				case <-p.exited:
					return
				case <-p.stop:
					return
				case <-time.After(streamGrace):
				}
				detail := "the event stream ended while opencode still runs"
				if *streamErr != nil {
					detail += ": " + (*streamErr).Error()
				}
				if !emit(pane.Event{Kind: pane.EvState, State: pane.StateUnknownStr, Detail: detail}) {
					return
				}
				continue
			}
			for _, pe := range p.tr.translate(ev) {
				if !emit(pe) {
					return
				}
			}
		case <-p.exited:
			return
		case <-p.stop:
			return
		}
	}
}

// relay is the single producer. It sends the initial events, then each
// translated bus event, until the process exits or Stop is called. When the
// stream ends while the process lives, the pane is unknown, never a stale
// idle. It always ends with one end event and closes Events().
func (p *proc) relay(body io.ReadCloser, initial []pane.Event) {
	defer close(p.events)
	defer close(p.relayDone)

	quit := make(chan struct{})
	defer close(quit)
	defer body.Close()

	lines := make(chan busEvent)
	var streamErr error
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
		for sc.Scan() {
			ev, ok := parseSSELine(sc.Text())
			if !ok {
				continue
			}
			select {
			case lines <- ev:
			case <-quit:
				return
			}
		}
		streamErr = sc.Err()
	}()

	emit := func(ev pane.Event) bool {
		select {
		case p.events <- ev:
			return true
		case <-p.stop:
			return false
		}
	}

	p.pump(lines, &streamErr, initial, emit)

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
	<-p.exited
	detail := "opencode exited"
	if !requested && p.cmd.ProcessState != nil {
		detail = fmt.Sprintf("opencode exited: exit=%d", p.cmd.ProcessState.ExitCode())
	}
	emit(pane.Event{Kind: pane.EvEnd, State: pane.StateDoneStr, Detail: detail})
}

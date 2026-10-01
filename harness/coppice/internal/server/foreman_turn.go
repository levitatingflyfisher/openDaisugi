package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/opendaisugi/coppice/internal/foreman"
	"github.com/opendaisugi/coppice/skills"
)

// A turn is one fresh model run. The owner's message is logged, then one
// run answers it over [tools][system][view][state][message]: the four tools
// (MCP), the fixed system page, the chat's view, a short state block and the
// message. The run keeps no session. Only its result text is logged, as the
// foreman's reply; a thought never is.

// TurnInput is what a turn runner is given. Both runners use one layout:
// the system prompt is the fixed page and the frozen snapshot of the view,
// and the stdin is the view lines added since, the tail, the state and the
// message. The snapshot changes only when the chat freezes a new one, so a
// cache that holds a prefix holds the page and the snapshot from one turn
// to the next.
type TurnInput struct {
	// System is the system prompt: the page, then the frozen view.
	System []byte
	// Stdin is what follows: the added view lines, the tail, the state and
	// the message.
	Stdin []byte
	// Socket is the per-turn tool socket. The runner must give the model a
	// way to reach the foreman's four tools there, and must call Started
	// with the pid of the process it starts, so the socket serves only
	// that process and its children.
	Socket string
	// Dir is the working directory of the run: the foreman's turn dir.
	Dir string
	// N names the turn; the runner may use it to name its own files.
	N int64
	// Started is called once with the pid of the started process.
	Started func(pid int)
}

// TurnUsage is the token counts of a turn.
type TurnUsage = foreman.CallUsage

// TurnResult is what a turn gives back.
type TurnResult struct {
	// Text is the model's final answer. Nothing else of the run is kept.
	Text  string
	Usage TurnUsage
}

// TurnRunner runs one turn. The claude runner is the first. A runner for
// another wire takes the same input: the stdin bytes and the socket.
type TurnRunner interface {
	Run(ctx context.Context, in TurnInput) (TurnResult, error)
}

// foremanBuiltIns are the built-in tools of Claude Code that a turn turns
// off by name. The hook of the turn refuses anything not named mcp__foreman__,
// so a built-in that a later release adds is refused too.
var foremanBuiltIns = []string{
	"Agent", "AskUserQuestion", "Bash", "BashOutput", "Edit", "ExitPlanMode", "Glob", "Grep", "KillShell",
	"LS", "Monitor", "MultiEdit", "NotebookEdit", "NotebookRead", "Read", "Skill", "SlashCommand", "Task",
	"TaskOutput", "TaskStop", "TodoWrite", "ToolSearch", "WebFetch", "WebSearch", "Write",
}

// claudeTurn runs a turn on claude -p.
type claudeTurn struct {
	bin   string
	model string
	// wrap, when set, is the fence prefix for a turn in dir (FN-R-17).
	wrap func(dir string) ([]string, error)
	// self is the command that runs `coppice foreman`: the MCP relay and
	// the hook are its verbs. Empty means this executable.
	self []string
}

func (c *claudeTurn) selfCmd() []string { return selfCmd(c.self) }

// selfCmd is the command that runs `coppice foreman`: self when set, else
// this executable.
func selfCmd(self []string) []string {
	if len(self) > 0 {
		return self
	}
	exe, err := os.Executable()
	// A binary that was replaced while the server runs reads as
	// "/path/coppice (deleted)". The path of the new file is the same
	// without that suffix.
	exe = trimDeleted(exe)
	if err != nil || exe == "" {
		exe = "coppice"
	}
	return []string{exe, "foreman"}
}

// trimDeleted drops the " (deleted)" that Linux adds to the path of an
// executable that was replaced while it runs.
func trimDeleted(exe string) string { return strings.TrimSuffix(exe, " (deleted)") }

// shellQuote quotes one word for sh.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// mcpConfigJSON is the --mcp-config of a turn: one server, the relay.
func mcpConfigJSON(self []string, sock string) string {
	b, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"foreman": map[string]any{
		"command": self[0], "args": append(append([]string{}, self[1:]...), "tools", "--sock", sock),
	}}})
	return string(b)
}

// settingsJSON is the --settings of a turn: the hook that refuses every tool
// but the foreman's four.
func settingsJSON(self []string) string {
	hookCmd := strings.Join(append(quoteAll(self), "hook"), " ")
	b, _ := json.Marshal(map[string]any{"hooks": map[string]any{"PreToolUse": []any{map[string]any{
		"matcher": ".*", "hooks": []any{map[string]any{"type": "command", "command": hookCmd}},
	}}}})
	return string(b)
}

// argv is the claude argv of a turn. The MCP config and the settings go in
// as JSON strings, so no file of the turn can be swapped between its write
// and claude's read. Three walls keep the built-ins out: --tools "" turns
// them all off, --disallowedTools names them, and the hook refuses any tool
// that is not one of the four.
func (c *claudeTurn) argv(systemFile, mcpJSON, settings string) []string {
	var allowed []string
	for _, n := range foreman.ToolNames {
		allowed = append(allowed, "mcp__foreman__"+n)
	}
	args := []string{"-p", "--output-format", "json", "--system-prompt-file", systemFile,
		"--mcp-config", mcpJSON, "--strict-mcp-config", "--tools", "",
		"--allowedTools", strings.Join(allowed, ","),
		"--disallowedTools", strings.Join(foremanBuiltIns, ","),
		"--settings", settings, "--no-session-persistence"}
	if c.model != "" {
		args = append(args, "--model", c.model)
	}
	return args
}

// claudeSystem is the system prompt of a turn: the fixed page, then the
// frozen view. It is the same bytes from one turn to the next until the
// chat freezes a new snapshot, so the prefix cache of the wire holds the
// page, the tools before it and the view.
func claudeSystem(snapshot []byte) string {
	return skills.ForemanTurn + "\n<chat>\n" + string(snapshot) + "</chat>\n"
}

// turnStdin is the stdin of a turn: the view lines added since the
// snapshot, then the tail, the state and the message.
func turnStdin(added, rest []byte) []byte {
	if len(added) == 0 {
		return rest
	}
	return []byte("<newer>\n" + string(added) + "</newer>\n" + string(rest))
}

// writeSystemFile writes the system prompt of a turn to t-<n>.system in the
// turn dir (0700): created new, never through a link, mode 0600. A single
// argument cannot hold more than 131071 bytes on Linux, and a frozen view
// is often longer, so the prompt goes in a file.
func writeSystemFile(dir string, n int64, text string) (string, error) {
	path := filepath.Join(dir, "t-"+strconv.FormatInt(n, 10)+".system")
	_ = os.Remove(path)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

func (c *claudeTurn) Run(ctx context.Context, in TurnInput) (TurnResult, error) {
	self := c.selfCmd()
	sysFile, err := writeSystemFile(in.Dir, in.N, string(in.System))
	if err != nil {
		return TurnResult{}, fmt.Errorf("cannot write the turn's system prompt: %w", err)
	}
	defer os.Remove(sysFile)
	name, args := c.bin, c.argv(sysFile, mcpConfigJSON(self, in.Socket), settingsJSON(self))
	if c.wrap != nil {
		w, werr := c.wrap(in.Dir)
		if werr != nil {
			return TurnResult{}, werr
		}
		if len(w) > 0 {
			name, args = w[0], append(append(append([]string{}, w[1:]...), c.bin), args...)
		}
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = in.Dir
	cmd.Stdin = bytes.NewReader(in.Stdin)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &limitedBuffer{b: &stderr, max: 4096}
	if err := cmd.Start(); err != nil {
		return TurnResult{}, fmt.Errorf("cannot start %s: %w", c.bin, err)
	}
	if in.Started != nil {
		in.Started(cmd.Process.Pid)
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return TurnResult{}, ctx.Err()
		}
		// claude may write its usage-limit result to stdout and leave stderr
		// empty, so the classifier gets both.
		return TurnResult{}, &runnerError{
			lead:   "claude ended with an error",
			msg:    "claude ended with an error: " + firstLine(stderr.String(), err),
			detail: failDetail(stderr.String(), stdout.String()),
		}
	}
	var out struct {
		Type    string         `json:"type"`
		IsError bool           `json:"is_error"`
		Result  string         `json:"result"`
		Usage   map[string]int `json:"usage"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || out.Type != "result" {
		return TurnResult{}, errors.New("claude gave an answer that is not a result")
	}
	if out.IsError {
		return TurnResult{}, &runnerError{lead: "claude reported an error", msg: "claude reported an error: " + firstLine(out.Result, nil), detail: foreman.Tail(out.Result, foreman.ClassifyBytes)}
	}
	return TurnResult{Text: out.Result, Usage: TurnUsage{
		Input: out.Usage["input_tokens"], Output: out.Usage["output_tokens"],
		CacheRead: out.Usage["cache_read_input_tokens"], CacheWrite: out.Usage["cache_creation_input_tokens"],
	}}, nil
}

func quoteAll(ws []string) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = shellQuote(w)
	}
	return out
}

// runnerError is a failed turn: msg is the one line for the chat, detail the
// bounded whole text (stderr and stdout) that the failover classifier reads.
type runnerError struct{ lead, msg, detail string }

func (e *runnerError) Error() string { return e.msg }

// failDetail joins what a runner's process wrote to stderr and stdout, the
// last foreman.ClassifyBytes of each.
func failDetail(stderr, stdout string) string {
	return foreman.Tail(stderr, foreman.ClassifyBytes) + "\n" + foreman.Tail(stdout, foreman.ClassifyBytes)
}

// safeTurnError is the one way a failed model run is worded for the chat,
// the note and the server log: the process's own words never leave the
// runner, only the lead and the fixed words of foreman.SafeReason. An error
// that is not a model run's output (a missing binary, a profile that is not
// in the table) is coppice's own text and passes. FM-R-60.
func safeTurnError(err error) string {
	var re *runnerError
	if !errors.As(err, &re) {
		return err.Error()
	}
	return re.lead + ": " + foreman.SafeReason(failText(err))
}

// failText is the text the classifier reads for a runner's error.
func failText(err error) string {
	var re *runnerError
	if errors.As(err, &re) && re.detail != "" {
		return re.msg + "\n" + re.detail
	}
	return err.Error()
}

// turnTailBytes caps the raw tail of a turn's input.
const turnTailBytes = 32 << 10

// turnState holds the queue of owner messages that wait for a turn.
type turnState struct {
	queue []int64
	// delivered holds the owner messages that reached a running turn at one
	// of its tool calls (FM-R-101). talkMu guards it with queue.
	delivered map[int64]bool
	// parts holds, for the last log id of an owner message that the log split,
	// every id of its parts in order (FM-R-111).
	parts   map[int64][]int64
	running bool
	// runner is the TurnRunner; nil means the claude runner.
	runner TurnRunner
	// self is the foreman verbs command for the claude runner; a test sets it.
	self []string
	// wg tracks the loop that runs the turns, so Close can wait for it.
	wg sync.WaitGroup
	// chain is the turn profiles with their cool-downs (foreman_failover.go),
	// loaded on first use. chainMu guards it.
	chainMu sync.Mutex
	chain   *foreman.Chain
	// pauseCancel ends the wait of a paused turn; nil when none waits.
	pauseCancel context.CancelFunc
	// pauseSet, when set, runs once the wake hook is set and before the pause
	// note is logged; a test sets it.
	pauseSet func()
	// pauseGap, when set, runs before the wake hook is set; a test sets it.
	pauseGap func()
	// runnerFor, when set, makes the runner of a profile; a test sets it.
	runnerFor func(profile string) TurnRunner
	// sleep, when set, replaces the wait of a turn whose profiles all cool.
	sleep func(ctx context.Context, d time.Duration) error
}

// turnTag matches the tags of a turn's input, opening or closing.
var turnTag = regexp.MustCompile(`</?(chat|newer|tail|state|message|message-now)\b`)

// escapeTurnTags puts a backslash after the "<" of each tag of the input,
// so text from the log cannot close the block it sits in or open another.
func escapeTurnTags(s string) string {
	return turnTag.ReplaceAllStringFunc(s, func(m string) string { return "<\\" + m[1:] })
}

// indent puts two spaces before each line of text, so a header of the tail
// can only appear at column 0.
func indent(text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}

// tailText renders the messages the view does not cover, word for word,
// newest kept: at most turnTailBytes, and always the newest whole. Each
// message is a header at column 0 and its text indented by two spaces, with
// the tags of the input escaped. A message the cap drops is in neither the
// view nor the tail: gap is the first and last of those, and hasGap is true.
// The model reads any dropped message with zoom.
func tailText(msgs []foreman.Message) (text string, gap [2]int64, hasGap bool) {
	var parts []string
	size := 0
	dropped := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		p := fmt.Sprintf("[%d %s %s]\n%s\n", m.I, m.Kind, m.Date, indent(escapeTurnTags(m.Text)))
		if size > 0 && size+len(p) > turnTailBytes {
			dropped = i + 1
			break
		}
		size += len(p)
		parts = append([]string{p}, parts...)
	}
	text = "<tail>\n" + strings.Join(parts, "\n") + "</tail>\n"
	if dropped == 0 {
		return text, gap, false
	}
	return text, [2]int64{msgs[0].I, msgs[dropped-1].I}, true
}

// gapLine is the line of <state> that names the messages the model cannot
// see, or "".
func gapLine(gap [2]int64, hasGap bool) string {
	switch {
	case !hasGap:
		return ""
	case gap[0] == gap[1]:
		return fmt.Sprintf("message log/%d is not summarized yet\n", gap[0])
	}
	return fmt.Sprintf("messages log/%d to log/%d are not summarized yet\n", gap[0], gap[1])
}

// turnInput builds the input of the turn that answers message msg: the
// system prompt (the page and the frozen snapshot of the view) and the
// stdin (the lines added to the snapshot, the raw tail, the state and the
// message).
//
// The state lines of unknown tool outcomes stay in the chat until a model
// has seen them: nUnk counts the ones this input shows, and the caller
// drops them once a model run starts.
func (s *Server) turnInput(fc *foremanChat, msg foreman.Message, nonce string) (system, stdin []byte, nUnk int, err error) {
	workers := s.workersState()
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.chat == nil {
		return nil, nil, 0, errNoChat
	}
	tail, err := fc.chat.Tail()
	if err != nil {
		return nil, nil, 0, err
	}
	// The answered message is in <message> alone: not in the tail, and not
	// as a line of the view.
	kept := tail[:0:0]
	for _, m := range tail {
		if m.I != msg.I {
			kept = append(kept, m)
		}
	}
	var b strings.Builder
	tailTxt, gap, hasGap := tailText(kept)
	b.WriteString(tailTxt)
	unk := ""
	lines, nUnk := fc.peekUnknown()
	for _, l := range lines {
		unk += "tool outcome unknown: " + escapeTurnTags(l) + "\n"
	}
	owner := ""
	if nonce != "" {
		owner = "owner block id: " + nonce + "\n"
	}
	b.WriteString("<state>\nnow: " + s.clock().UTC().Format(foreman.DateLayout) + "\n" + owner + gapLine(gap, hasGap) + unk + workers + "</state>\n")
	b.WriteString("<message>\n" + escapeTurnTags(msg.Text) + "\n</message>\n")
	snapshot, added := fc.splitView(fc.chat.ViewText(escapeTurnTags, msg.I))
	return []byte(claudeSystem(snapshot)), turnStdin(added, []byte(b.String())), nUnk, nil
}

// queueTurnLocked adds the turn for owner message msg and starts the loop
// that runs the turns, one at a time, in order. The caller holds talkMu
// and has checked for room (turnRoom).
func (s *Server) queueTurnLocked(fc *foremanChat, msg int64) int {
	t := &s.turns
	t.queue = append(t.queue, msg)
	if !t.running {
		t.running = true
		t.wg.Add(1)
		go s.runTurns(fc)
	}
	return len(t.queue)
}

// turnRoom is the refusal for a talk when maxTalkQueue turns wait, or when
// the server has closed; nil when there is room. The caller holds talkMu.
func (s *Server) turnRoom() error {
	if s.isClosed() {
		return errors.New("this server has closed")
	}
	if n := len(s.turns.queue); n >= maxTalkQueue {
		return fmt.Errorf("the foreman has %d turns waiting already. Wait for it to answer, then talk again", n)
	}
	return nil
}

// runTurns runs the queued turns, oldest first, until the queue is empty or
// the server closes.
func (s *Server) runTurns(fc *foremanChat) {
	defer s.turns.wg.Done()
	for {
		s.talkMu.Lock()
		if len(s.turns.queue) == 0 {
			s.turns.running = false
			s.talkMu.Unlock()
			// An import node held back during the turn may go on.
			fc.wake()
			return
		}
		msg := s.turns.queue[0]
		s.turns.queue = s.turns.queue[1:]
		s.talkMu.Unlock()
		select {
		case <-s.talkStop:
			s.talkMu.Lock()
			s.turns.running = false
			s.talkMu.Unlock()
			return
		default:
		}
		s.runTurn(fc, msg)
	}
}

// runTurn runs the turn for owner message msg and logs what came of it.
// The end record of turns.jsonl is the last thing a turn writes: by then
// the tool socket is closed and the reply or the note is in the log, so a
// reader that trusts the record sees a turn that is over.
func (s *Server) runTurn(fc *foremanChat, msgID int64) {
	rec := foreman.TurnRecord{Msg: msgID, Started: s.clock().UTC().Format(foreman.DateLayout), Status: foreman.TurnRunning}
	_ = foreman.AppendTurn(fc.dir, rec)
	s.sendMessagesEvent("", true)
	var ts *turnSocket
	midNote := ""
	end := func(status, reply, note string, usage *TurnUsage) {
		if ts != nil {
			// Close waits for a call in flight, so the ids are final.
			ts.Close()
			mid := ts.midIDs()
			ts = nil
			rec.Mid = mid
			if status != foreman.TurnOK && len(mid) > 0 {
				// These messages reached the model in a tool result, so they
				// are heard. Running them again could repeat what the model
				// did on them: one line says so and none runs again
				// (FM-R-110).
				midNote = "Your message reached the turn before it failed."
			}
		}
		if reply != "" {
			if err := s.chatAppend(fc, foreman.KindForeman, reply); err != nil {
				s.noteOnce("The foreman's log could not take its reply: " + err.Error())
			}
		}
		if note != "" {
			if err := s.chatAppend(fc, foreman.KindNote, note); err != nil {
				s.noteOnce("The foreman's log could not take a note: " + err.Error())
			}
			s.Note(note, "")
		}
		if midNote != "" {
			if err := s.chatAppend(fc, foreman.KindNote, midNote); err != nil {
				s.noteOnce("The foreman's log could not take a note: " + err.Error())
			}
		}
		rec.Ended, rec.Status, rec.Usage = s.clock().UTC().Format(foreman.DateLayout), status, usage
		_ = foreman.AppendTurn(fc.dir, rec)
		s.sendMessagesEvent("", true)
	}
	// again is the end of a failure note. After a tool call ran in this
	// turn the outcome is known to the log, and a repeat could run it twice.
	again := func() string {
		if ts != nil && ts.seq.Load() > 0 {
			return "A tool call ran in this turn. Check the workers before you say it again."
		}
		return "Say it again to try again."
	}
	fail := func(why string) {
		end(foreman.TurnFailed, "", "The foreman's turn failed: "+why+" "+again(), nil)
	}
	fc.mu.Lock()
	var msg foreman.Message
	var err error
	if fc.chat != nil {
		msg, err = fc.chat.Message(msgID)
	} else {
		err = errNoChat
	}
	fc.mu.Unlock()
	if err != nil {
		fail(err.Error())
		return
	}
	// The socket opens first: its nonce marks the owner's block in the
	// state of this turn's input (FM-R-109).
	ts, err = s.openTurnSocket(fc, msgID)
	if err != nil {
		ts = nil
		fail("the tool socket could not open: " + err.Error())
		return
	}
	system, stdin, nUnk, err := s.turnInput(fc, msg, ts.nonce)
	if err != nil {
		fail(err.Error())
		return
	}
	defer func() {
		if ts != nil {
			ts.Close()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-s.talkStop:
			cancel()
		case <-ctx.Done():
		}
	}()
	var seen sync.Once
	started := func(pid int) {
		ts.Started(pid)
		seen.Do(func() { fc.dropUnknown(nUnk) })
	}
	run, err := s.runChain(ctx, fc, ts, TurnInput{System: system, Stdin: stdin, Socket: ts.Path(), Dir: s.turnDir(), N: msgID, Started: started})
	if err != nil {
		select {
		case <-s.talkStop:
			end(foreman.TurnFailed, "", "The foreman's turn was stopped when coppice stopped. "+again(), nil)
			return
		default:
		}
		if errors.Is(err, errTurnTimeout) {
			end(foreman.TurnTimeout, "", fmt.Sprintf("The foreman's turn timed out: it ran longer than %s and was stopped. %s", s.cfg.Foreman.TurnTimeout, again()), nil)
			return
		}
		fail(err.Error() + ".")
		return
	}
	res := run.res
	u := res.Usage
	end(foreman.TurnOK, strings.TrimSpace(res.Text), run.note, &u)
}

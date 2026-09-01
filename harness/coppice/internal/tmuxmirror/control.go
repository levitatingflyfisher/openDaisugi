package tmuxmirror

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

// EventKind names one thing tmux control mode printed.
type EventKind int

const (
	// EventReply is a %begin block closed by %end or %error.
	EventReply EventKind = iota
	// EventOutput is %output from a tmux pane.
	EventOutput
	// EventWindowClose is %window-close or %unlinked-window-close.
	EventWindowClose
	// EventExit is %exit. tmux ends the control client after it.
	EventExit
	// EventOther is any other notification.
	EventOther
)

// Event is one parsed unit of control-mode output.
type Event struct {
	Kind EventKind
	// Num is the command number of a reply.
	Num int
	// Client is true for a reply to a command this client sent. tmux marks
	// those with flag 1. The block that answers the attach itself has 0.
	Client bool
	// OK is false for a reply that ends in %error.
	OK bool
	// Lines is the text inside a reply block.
	Lines []string
	// Window is the window id of a close, such as @3.
	Window string
	// Pane and Data are the pane id and the text of an output line.
	Pane string
	Data string
	// Raw is the line, for a notification the mirror does not read.
	Raw string
}

// Parser turns control-mode lines into events, one line at a time.
type Parser struct {
	open  bool
	block Event
}

// Line reads one line without its newline. It returns an event when the
// line completes one.
func (p *Parser) Line(line string) (Event, bool) {
	line = strings.TrimSuffix(line, "\r")
	if p.open {
		if num, flags, ok := guard(line, "%end"); ok && num == p.block.Num {
			return p.close(true, flags), true
		}
		if num, flags, ok := guard(line, "%error"); ok && num == p.block.Num {
			return p.close(false, flags), true
		}
		p.block.Lines = append(p.block.Lines, line)
		return Event{}, false
	}
	if num, flags, ok := guard(line, "%begin"); ok {
		p.open = true
		p.block = Event{Kind: EventReply, Num: num, Client: flags&1 == 1}
		return Event{}, false
	}
	word, rest, _ := strings.Cut(line, " ")
	switch word {
	case "%window-close", "%unlinked-window-close":
		id, _, _ := strings.Cut(rest, " ")
		return Event{Kind: EventWindowClose, Window: id, Raw: line}, true
	case "%output":
		pane, data, _ := strings.Cut(rest, " ")
		return Event{Kind: EventOutput, Pane: pane, Data: data, Raw: line}, true
	case "%exit":
		return Event{Kind: EventExit, Raw: line}, true
	}
	return Event{Kind: EventOther, Raw: line}, true
}

func (p *Parser) close(ok bool, flags int) Event {
	ev := p.block
	ev.OK = ok
	ev.Client = flags&1 == 1
	p.open = false
	p.block = Event{}
	return ev
}

// guard reads "%begin TIME NUM FLAGS" and the %end and %error lines of the
// same shape.
func guard(line, word string) (num, flags int, ok bool) {
	f := strings.Fields(line)
	if len(f) != 4 || f[0] != word {
		return 0, 0, false
	}
	n, err1 := strconv.Atoi(f[2])
	fl, err2 := strconv.Atoi(f[3])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return n, fl, true
}

// Parse reads every event from r.
func Parse(r io.Reader) ([]Event, error) {
	var p Parser
	var out []Event
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if ev, ok := p.Line(sc.Text()); ok {
			out = append(out, ev)
		}
	}
	return out, sc.Err()
}

// ErrClosed is the error of a command sent after tmux ended the client.
var ErrClosed = errors.New("tmux ended the control client")

// Client sends commands to tmux -C and reads its replies. One command is in
// flight at a time. Notifications go to Events.
type Client struct {
	w       io.WriteCloser
	mu      sync.Mutex
	replies chan Event
	events  chan Event
	done    chan struct{}
	timeout time.Duration
	// broken is set when a reply did not come in time. A late reply could
	// then answer the next command, so the client sends nothing more.
	broken bool
}

// NewClient reads r in the background. w takes one command per line.
func NewClient(w io.WriteCloser, r io.Reader) *Client {
	c := &Client{
		w:       w,
		replies: make(chan Event, 1),
		events:  make(chan Event, 256),
		done:    make(chan struct{}),
		timeout: 5 * time.Second,
	}
	go c.read(r)
	return c
}

func (c *Client) read(r io.Reader) {
	defer close(c.done)
	var p Parser
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		ev, ok := p.Line(sc.Text())
		if !ok {
			continue
		}
		switch {
		case ev.Kind == EventReply && ev.Client:
			select {
			case c.replies <- ev:
			default:
			}
		case ev.Kind == EventReply, ev.Kind == EventOutput:
		case ev.Kind == EventExit:
			return
		default:
			select {
			case c.events <- ev:
			default:
			}
		}
	}
}

// Events carries the notifications the mirror reads. A full channel drops
// the newest, and the next sync repairs what it missed.
func (c *Client) Events() <-chan Event { return c.events }

// Done closes when tmux ends the control client or its output closes.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err is nil while the client can send. It is ErrClosed once tmux ended
// the client, and an error once a reply came too late.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.broken {
		return errors.New("tmux stopped answering the control client")
	}
	select {
	case <-c.done:
		return ErrClosed
	default:
	}
	return nil
}

// Close ends the client by closing tmux's stdin.
func (c *Client) Close() error { return c.w.Close() }

// Do sends one command and returns the lines of its reply. A reply that
// ends in %error is an error with those lines. A command that holds a
// newline is refused before it is sent, since tmux would read two commands.
func (c *Client) Do(cmd string) ([]string, error) {
	if strings.ContainsAny(cmd, "\r\n") {
		return nil, fmt.Errorf("a tmux command must be one line: %q", cmd)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.broken {
		return nil, errors.New("tmux stopped answering the control client")
	}
	select {
	case <-c.done:
		return nil, ErrClosed
	default:
	}
	if _, err := io.WriteString(c.w, cmd+"\n"); err != nil {
		return nil, err
	}
	select {
	case ev := <-c.replies:
		if !ev.OK {
			return ev.Lines, fmt.Errorf("tmux refused %q: %s", cmd, strings.Join(ev.Lines, " "))
		}
		return ev.Lines, nil
	case <-c.done:
		return nil, ErrClosed
	case <-time.After(c.timeout):
		c.broken = true
		return nil, fmt.Errorf("tmux did not answer %q", cmd)
	}
}

// quote makes s one tmux argument that tmux reads as literal text.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// literal doubles each #, so tmux does not read a format in a name or in
// status-right.
func literal(s string) string {
	return strings.ReplaceAll(s, "#", "##")
}

// ListCmd lists the session's windows with the mirror's mark.
func ListCmd(session string) string {
	return "list-windows -t " + quote(session) + " -F '#{window_id}\t#{window_name}\t#{" + MarkOption + "}'"
}

// CreateCmd opens a window in the background that runs
// exe --socket socket attach pane, and prints its window id. The command is
// given as separate arguments, so tmux runs it with no shell.
func CreateCmd(session, name, exe, socket, pane string) string {
	return "new-window -d -P -F '#{window_id}' -t " + quote(session+":") +
		" -n " + quote(literal(name)) + " " + line([]string{exe, "--socket", socket, "attach", pane})
}

// ctlFlags are the flags ctl sends as they are.
var ctlFlags = map[string]bool{"-q": true, "-v": true, "-t": true, "-u": true, "-w": true}

// ctl joins a tmux command into one control-mode line. The verb and each
// flag in ctlFlags go as they are, and every other argument is quoted, so a
// value such as -x stays a value.
func ctl(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		if i == 0 || ctlFlags[a] {
			q[i] = a
		} else {
			q[i] = quote(a)
		}
	}
	return strings.Join(q, " ")
}

// line joins args into one control-mode command, each one quoted.
func line(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = quote(a)
	}
	return strings.Join(q, " ")
}

// MarkCmd sets the mark that says the mirror owns the window.
func MarkCmd(window, pane string) string {
	return "set-option -w -t " + quote(window) + " " + MarkOption + " " + quote(pane)
}

// RenameCmd renames a window.
func RenameCmd(window, name string) string {
	return "rename-window -t " + quote(window) + " " + quote(literal(name))
}

// KillCmd kills a window.
func KillCmd(window string) string {
	return "kill-window -t " + quote(window)
}

// StatusCmd sets status-right for one session, not for every session.
func StatusCmd(session, line string) string {
	return "set-option -t " + quote(session) + " status-right " + quote(literal(line))
}

// SavedOption is the session option that keeps the session's own
// status-right while a mirror runs: "-" when it had none, "=" and the
// value when it had one. A second mirror on the session reads it, so it
// never keeps the first one's count as the session's own.
const SavedOption = "@coppice_saved_status"

// restoreArgs are the two tmux commands that put back the saved
// status-right and drop the saved option, joined by ";" as tmux reads them.
func restoreArgs(session string, saved *string) [][]string {
	set := []string{"set-option", "-u", "-t", session, "status-right"}
	if saved != nil {
		set = []string{"set-option", "-t", session, "status-right", *saved}
	}
	return [][]string{set, {"set-option", "-u", "-t", session, SavedOption}}
}

// ParseWindows reads the lines ListCmd prints. A line with fewer than three
// fields is skipped.
func ParseWindows(lines []string) []Window {
	var out []Window
	for _, l := range lines {
		f := strings.Split(l, "\t")
		if len(f) < 3 || !strings.HasPrefix(f[0], "@") {
			continue
		}
		out = append(out, Window{
			ID:   f[0],
			Name: strings.Join(f[1:len(f)-1], "\t"),
			Pane: f[len(f)-1],
		})
	}
	return out
}

// Mirror keeps one session's windows in line with the panes.
type Mirror struct {
	Tmux    *Client
	Session string
	// Exe is the coppice program each window runs, and Socket the server
	// socket it attaches through.
	Exe    string
	Socket string
	// Log takes a line for each op tmux refused. Nil drops the lines.
	Log func(string)
	// OneShot runs tmux once with args. Stop uses it when the control
	// client is gone.
	OneShot func(args []string) error

	mu      sync.Mutex
	started bool
	saved   *string
	owned   map[string]string
	hidden  map[string]bool
}

// Start finds the session's own status-right, so Stop can put it back. A
// value a running mirror saved comes first. Otherwise Start reads
// status-right and saves it in SavedOption.
func (m *Mirror) Start() error {
	kept, err := m.Tmux.Do(ctl([]string{"show-options", "-q", "-v", "-t", m.Session, SavedOption}))
	if err != nil {
		return err
	}
	var saved *string
	switch {
	case len(kept) > 0 && kept[0] == "-":
	case len(kept) > 0 && strings.HasPrefix(kept[0], "="):
		v := strings.TrimPrefix(kept[0], "=")
		saved = &v
	default:
		lines, err := m.Tmux.Do(ctl([]string{"show-options", "-v", "-t", m.Session, "status-right"}))
		if err != nil {
			return err
		}
		enc := "-"
		if len(lines) > 0 {
			v := lines[0]
			saved = &v
			enc = "=" + v
		}
		if _, err := m.Tmux.Do(ctl([]string{"set-option", "-t", m.Session, SavedOption, enc})); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started = true
	m.saved = saved
	return nil
}

// Stop puts back the status-right that Start found. The windows stay.
// When the control client is gone, it runs tmux once through OneShot.
func (m *Mirror) Stop() error {
	m.mu.Lock()
	started, saved := m.started, m.saved
	m.mu.Unlock()
	if !started {
		return nil
	}
	cmds := restoreArgs(m.Session, saved)
	if m.Tmux.Err() == nil {
		var err error
		for _, c := range cmds {
			if _, err = m.Tmux.Do(ctl(c)); err != nil {
				break
			}
		}
		if err == nil || m.Tmux.Err() == nil {
			return err
		}
	}
	if m.OneShot == nil {
		return errors.New("the tmux control client is gone and no one-shot run is set")
	}
	args := append(append(append([]string{}, cmds[0]...), ";"), cmds[1]...)
	return m.OneShot(args)
}

// Hidden reports whether the pane's window was closed. The mirror does not
// open a hidden pane again until it starts again.
func (m *Mirror) Hidden(pane string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hidden[pane]
}

// Handle reads one notification. The close of a window the mirror owns
// hides its pane.
func (m *Mirror) Handle(ev Event) {
	if ev.Kind != EventWindowClose {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if pane, ok := m.owned[ev.Window]; ok {
		delete(m.owned, ev.Window)
		if m.hidden == nil {
			m.hidden = map[string]bool{}
		}
		m.hidden[pane] = true
	}
}

// Sync lists the windows, applies the plan for the panes that are not
// hidden, and sets the status line from every pane. A refused list or
// status ends it with an error. A refused op on one window does not.
func (m *Mirror) Sync(panes []Pane) error {
	lines, err := m.Tmux.Do(ListCmd(m.Session))
	if err != nil {
		return err
	}
	windows := ParseWindows(lines)
	m.mu.Lock()
	if m.owned == nil {
		m.owned = map[string]string{}
	}
	for _, w := range windows {
		if w.Pane != "" {
			m.owned[w.ID] = w.Pane
		}
	}
	var shown []Pane
	for _, p := range panes {
		if !m.hidden[p.ID] {
			shown = append(shown, p)
		}
	}
	m.mu.Unlock()
	for _, op := range Plan(shown, windows) {
		if err := m.apply(op); err != nil {
			// A window can go between the list and the op. A refused op
			// is logged, and the next sync plans again. A client that
			// can no longer send ends the sync.
			if cerr := m.Tmux.Err(); cerr != nil {
				return cerr
			}
			if m.Log != nil {
				m.Log(err.Error())
			}
		}
	}
	_, err = m.Tmux.Do(StatusCmd(m.Session, StatusLine(panes)))
	return err
}

func (m *Mirror) apply(op Op) error {
	switch op.Kind {
	case OpRename:
		_, err := m.Tmux.Do(RenameCmd(op.Window, op.Name))
		return err
	case OpKill:
		m.mu.Lock()
		delete(m.owned, op.Window)
		m.mu.Unlock()
		_, err := m.Tmux.Do(KillCmd(op.Window))
		return err
	case OpCreate:
		lines, err := m.Tmux.Do(CreateCmd(m.Session, op.Name, m.Exe, m.Socket, op.Pane))
		if err != nil {
			return err
		}
		if len(lines) != 1 || !strings.HasPrefix(lines[0], "@") {
			return fmt.Errorf("tmux gave no window id for %s: %q", op.Pane, lines)
		}
		id := strings.TrimSpace(lines[0])
		m.mu.Lock()
		m.owned[id] = op.Pane
		m.mu.Unlock()
		if _, err := m.Tmux.Do(MarkCmd(id, op.Pane)); err != nil {
			// A window with no mark looks like the user's own, and the
			// next sync would open another. Kill it and plan again.
			m.mu.Lock()
			delete(m.owned, id)
			m.mu.Unlock()
			_, _ = m.Tmux.Do(KillCmd(id))
			return err
		}
		return nil
	}
	return fmt.Errorf("unknown op %q", op.Kind)
}

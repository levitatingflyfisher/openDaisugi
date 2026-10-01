package server

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/opendaisugi/coppice/internal/foreman"
)

// The foreman's tools are served on a socket that lives one turn. The turn
// runner starts the model with an MCP server (coppice foreman tools --sock
// S) that relays each call to S, and nothing else. The listener lives in
// the foreman's turn dir (0700), takes only the four verbs, and checks
// SO_PEERCRED: the peer must descend from the process the turn runner
// started, so a worker of the same user cannot call them. The public
// socket and its role rule are not involved.
//
// The pane foreman's tools are served the same way on a standing socket
// (foreman_standing.go): it lives as long as the server, and its peer must
// descend from the pane foreman's process.

// turnSocket is the tool socket of one turn.
type turnSocket struct {
	s    *Server
	fc   *foremanChat
	ln   *net.UnixListener
	path string
	// root is the pid the turn runner started, 0 until it has started.
	root atomic.Int64
	// rootFn, when set, names the root pid at each call: the standing
	// socket's root is the pane foreman, which can change.
	rootFn func() int
	// standing is true for the pane foreman's socket. With no chat open,
	// zoom and date answer a plain line, and no call is logged.
	standing bool
	wg       sync.WaitGroup
	// calls counts the tool requests the socket has taken from its turn. A
	// turn that made any is not run again on another profile.
	calls atomic.Int64
	// turn is the owner message the turn answers; -1 for the standing
	// socket. seq counts the write-ahead call lines. tag names the socket
	// in a call id (FM-R-66).
	turn int64
	seq  atomic.Int64
	tag  string
	// midMu guards mid: the owner messages this turn took at its tool calls.
	midMu sync.Mutex
	mid   []int64
	// nonce is made for this turn. The owner's words reach the model in a
	// block that carries it; the model trusts only a block whose nonce is the
	// one in its own state (FM-R-109).
	nonce string
	// callFn, when set, runs a tool in place of call. A test uses it to look
	// at the log at the moment of the effect.
	callFn func(foreman.ToolRequest) (string, error)
}

// openTurnSocket listens at <turn dir>/t-<n>.sock. A stale socket file of
// an earlier run is removed; anything else at that name is an error.
func (s *Server) openTurnSocket(fc *foremanChat, n int64) (*turnSocket, error) {
	dir := s.turnDir()
	path := filepath.Join(dir, fmt.Sprintf("t-%d.sock", n))
	// The standing socket's rule (FR-R-9): a path of 100 bytes or more is
	// refused before anything is made, with words that say why.
	if len(path) >= 100 {
		return nil, fmt.Errorf("the path %s is too long for a socket. Use a shorter data dir", path)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	return s.listenTools(fc, path, false, nil, n, fmt.Sprintf("c%d", n))
}

// newNonce is 96 random bits as 24 hex digits: the mark of the owner's block
// in one turn (FM-R-109).
func newNonce() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// No randomness: a clock-based mark still differs from a forged one
		// that was written before the turn began.
		return fmt.Sprintf("%024x", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", b)
}

// messageNowTag matches a message-now tag, opening or closing.
var messageNowTag = regexp.MustCompile(`</?message-now\b`)

// escapeMessageNow puts a backslash after the "<" of each message-now tag in
// a tool result, so that text from a worker or a file cannot pass for the
// owner's block (FM-R-109).
func escapeMessageNow(s string) string {
	return messageNowTag.ReplaceAllStringFunc(s, func(m string) string { return "<\\" + m[1:] })
}

// standingTag names the standing socket in a call id. The clock alone can
// repeat when two sockets open in one second, so random bytes follow it.
func standingTag() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("s%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("s%d-%x", time.Now().Unix(), b)
}

// listenTools listens at path, 0600, and serves the four verbs. A stale
// socket file of an earlier run is removed; anything else at that name is
// an error. standing, rootFn, turn and tag are set before serving starts.
func (s *Server) listenTools(fc *foremanChat, path string, standing bool, rootFn func() int, turn int64, tag string) (*turnSocket, error) {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s is not a socket. Move it away", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	addr, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", addr)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	ts := &turnSocket{s: s, fc: fc, ln: ln, path: path, standing: standing, rootFn: rootFn,
		turn: turn, tag: tag, nonce: newNonce()}
	ts.wg.Add(1)
	go ts.serve()
	return ts, nil
}

// Path is the socket the model's tool relay dials.
func (ts *turnSocket) Path() string { return ts.path }

// Started tells the socket which process is the turn's. Until it does,
// every peer is refused.
func (ts *turnSocket) Started(pid int) { ts.root.Store(int64(pid)) }

// Close stops serving and removes the socket. A call in flight finishes.
func (ts *turnSocket) Close() {
	_ = ts.ln.Close()
	ts.wg.Wait()
	_ = os.Remove(ts.path)
}

func (ts *turnSocket) serve() {
	defer ts.wg.Done()
	for {
		conn, err := ts.ln.AcceptUnix()
		if err != nil {
			return
		}
		// A call counts from the accept, so one in flight blocks a replay.
		ts.calls.Add(1)
		ts.wg.Add(1)
		go func() {
			defer ts.wg.Done()
			defer conn.Close()
			ts.handle(conn)
		}()
	}
}

// descends reports whether pid is root or lies below it in the process
// tree.
func (s *Server) descends(pid, root int) bool {
	for i := 0; i < 64 && pid > 1; i++ {
		if pid == root {
			return true
		}
		ppid, _, err := s.procStat(pid)
		if err != nil {
			return false
		}
		pid = ppid
	}
	return pid == root && root > 1
}

func (ts *turnSocket) rootPID() int {
	if ts.rootFn != nil {
		return ts.rootFn()
	}
	return int(ts.root.Load())
}

func (ts *turnSocket) allowed(conn *net.UnixConn) bool {
	if !peerPIDSupported {
		return false
	}
	root := ts.rootPID()
	// The runner names its process right after it starts it. A peer that
	// is faster than that waits a moment, then is refused. The standing
	// socket's root is the pane foreman, which is running or is not.
	for wait := 0; root <= 1 && wait < 200 && !ts.standing; wait++ {
		time.Sleep(10 * time.Millisecond)
		root = ts.rootPID()
	}
	if root <= 1 {
		return false
	}
	uid, err := ts.s.peerUID(conn)
	if err != nil || uid != uint32(os.Getuid()) {
		return false
	}
	pid, err := ts.s.peerPID(conn)
	return err == nil && ts.s.descends(pid, root)
}

// writeToolResponse writes one reply. The error says whether the model's side
// got it: a message added to a reply that was not written was not delivered.
func writeToolResponse(conn net.Conn, r foreman.ToolResponse) error {
	b, _ := json.Marshal(r)
	_ = conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, err := conn.Write(append(b, '\n'))
	return err
}

func (ts *turnSocket) handle(conn *net.UnixConn) {
	if !ts.allowed(conn) {
		writeToolResponse(conn, foreman.ToolResponse{Error: "refused: this socket serves the tools of one turn, to that turn's own process only."})
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	line, err := bufio.NewReaderSize(conn, 64*1024).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}
	var req foreman.ToolRequest
	if err := json.Unmarshal(bytes.TrimSpace(line), &req); err != nil {
		writeToolResponse(conn, foreman.ToolResponse{Error: "the request is not JSON"})
		return
	}
	args, _ := json.Marshal(req.Args)
	logging := !ts.standing || ts.fc.hasChat()
	// An acting call writes its intent first, fsynced, and then the effect
	// runs. When the line cannot be written there is no effect (FM-R-66).
	var callID string
	if logging && acting(req.Tool) {
		var err error
		if callID, err = ts.startCall(req.Tool, string(args)); err != nil {
			// The line may be on disk (a failed directory sync). Close it as
			// failed, if that can be written, so the next open does not
			// report a call that never ran. If it cannot be written, the
			// open reports it as unknown: the safe side.
			ts.endCall(callID, err)
			writeToolResponse(conn, foreman.ToolResponse{Error: "the call was not started: the foreman's log could not take its intent line (" + err.Error() + ")"})
			return
		}
	}
	run := ts.call
	if ts.callFn != nil {
		run = ts.callFn
	}
	text, callErr := run(req)
	if callID != "" {
		ts.endCall(callID, callErr)
	}
	logged := text
	if callErr != nil {
		logged = "error: " + callErr.Error()
	}
	if logging {
		ts.s.logToolCall(ts.fc, req.Tool, string(args), logged)
	}
	// An owner message that came while this turn ran reaches it here, at
	// the end of a tool call, as a block after the result (FM-R-101). The
	// log keeps the result as it was.
	extra, taken := "", []int64(nil)
	if !ts.standing {
		extra, taken = ts.s.deliverMid(ts)
	}
	// Text a worker or a file wrote can hold a forged message-now tag; it is
	// neutralised before the real block is added (FM-R-109).
	var werr error
	if callErr != nil {
		werr = writeToolResponse(conn, foreman.ToolResponse{Error: escapeMessageNow(callErr.Error()) + extra})
	} else {
		werr = writeToolResponse(conn, foreman.ToolResponse{OK: true, Result: escapeMessageNow(text) + extra})
	}
	if len(taken) > 0 {
		if werr != nil {
			// The model did not get the block: the messages go next turn
			// (FM-R-111).
			ts.s.requeueDelivered(taken)
			return
		}
		ts.midMu.Lock()
		ts.mid = append(ts.mid, taken...)
		ts.midMu.Unlock()
		ts.s.sendMessagesEvent("", true)
	}
}

// midIDs are the owner messages this turn took, in order.
func (ts *turnSocket) midIDs() []int64 {
	ts.midMu.Lock()
	defer ts.midMu.Unlock()
	return append([]int64{}, ts.mid...)
}

// deliverMid takes the owner messages queued behind the running turn ts and
// returns them as blocks to add to a tool result, with the queue ids it took.
// A message taken here leaves the queue, so no turn answers it again; the act
// ids of FM-R-65 already keep a repeated send from being logged twice. The
// caller writes the reply and then either records the ids as heard
// (ts.mid) or, when the write failed, gives them back with requeueDelivered
// (FM-R-111). A message that the turn does not reach before it ends stays
// queued and is its own turn, as before. A long message that the log split
// in parts delivers every part, in order, in one block each.
func (s *Server) deliverMid(ts *turnSocket) (string, []int64) {
	s.talkMu.Lock()
	var take, keep []int64
	for _, id := range s.turns.queue {
		if id > ts.turn {
			take = append(take, id)
		} else {
			keep = append(keep, id)
		}
	}
	if len(take) == 0 {
		s.talkMu.Unlock()
		return "", nil
	}
	s.turns.queue = keep
	if s.turns.delivered == nil {
		s.turns.delivered = map[int64]bool{}
	}
	parts := map[int64][]int64{}
	for _, id := range take {
		parts[id] = s.partsOf(id)
		for _, p := range parts[id] {
			s.turns.delivered[p] = true
		}
	}
	s.talkMu.Unlock()
	var b strings.Builder
	var got []int64
	for _, id := range take {
		var one strings.Builder
		ok := true
		for _, p := range parts[id] {
			ts.fc.mu.Lock()
			var m foreman.Message
			var err error
			if ts.fc.chat != nil {
				m, err = ts.fc.chat.Message(p)
			} else {
				err = errNoChat
			}
			ts.fc.mu.Unlock()
			if err != nil {
				ok = false
				break
			}
			one.WriteString("\n<message-now id=\"" + ts.nonce + "\">\n" + escapeTurnTags(m.Text) + "\n</message-now>")
		}
		if !ok {
			// Not readable: it goes back to the queue as its own turn.
			s.requeueDelivered([]int64{id})
			continue
		}
		got = append(got, id)
		b.WriteString(one.String())
	}
	return b.String(), got
}

// partsOf is the log ids of the owner message whose last part is id: every
// part, in order. The caller holds talkMu.
func (s *Server) partsOf(id int64) []int64 {
	if p := s.turns.parts[id]; len(p) > 0 {
		return p
	}
	return []int64{id}
}

// requeueDelivered puts delivered messages back at the front of the queue,
// as turns of their own.
func (s *Server) requeueDelivered(ids []int64) {
	s.talkMu.Lock()
	defer s.talkMu.Unlock()
	for _, id := range ids {
		for _, p := range s.partsOf(id) {
			delete(s.turns.delivered, p)
		}
	}
	s.turns.queue = append(append([]int64{}, ids...), s.turns.queue...)
}

// acting reports whether a tool has effects outside the chat: only these
// write an intent line first.
func acting(tool string) bool { return tool == "spawn" }

// startCall writes the start line of an acting call and returns its id.
func (ts *turnSocket) startCall(tool, args string) (string, error) {
	id := fmt.Sprintf("%s.%d", ts.tag, ts.seq.Add(1))
	err := foreman.AppendCall(ts.fc.dir, foreman.CallRecord{
		Call: id, Phase: foreman.CallStart, Turn: ts.turn, Tool: tool, Args: args,
		At: ts.s.clock().UTC().Format(foreman.DateLayout),
	})
	return id, err
}

// endCall writes the end line. A failure to write it leaves the call
// dangling, which the next open reports as unknown: the safe side.
func (ts *turnSocket) endCall(id string, callErr error) {
	outcome := foreman.CallOK
	if callErr != nil {
		outcome = foreman.CallFailed
	}
	_ = foreman.AppendCall(ts.fc.dir, foreman.CallRecord{
		Call: id, Phase: foreman.CallEnd, Outcome: outcome,
		At: ts.s.clock().UTC().Format(foreman.DateLayout),
	})
}

// settleDanglingCalls runs when the chat opens. Each start line with no end
// is a call whose outcome is unknown: it gets one note in the chat, one line
// for the state of the next turn, and last an end line of outcome unknown.
// If the end line cannot be written the call is reported again at the next
// open, which is the safe side. If the call log cannot be read, nothing is
// known about any turn: one note says so and unreadable is true. The result
// names the turns that had such a call, and started names the turns whose
// log holds any start line, ended or not. It is never run again (FM-R-66).
func (s *Server) settleDanglingCalls(fc *foremanChat) (unknown, started map[int64]bool, unreadable bool) {
	unknown, started = map[int64]bool{}, map[int64]bool{}
	// A torn tail is cut back so the next start line is whole (FM-R-67).
	_ = foreman.RepairCalls(fc.dir)
	recs, err := foreman.ReadCalls(fc.dir)
	if err != nil {
		note := "The foreman's call log could not be read (" + err.Error() + "). Whether a tool call ran before coppice stopped is unknown. Check the workers before you ask for anything again."
		_ = s.chatAppend(fc, foreman.KindNote, note)
		s.Note(note, "")
		fc.addUnknown("the call log could not be read, so a tool call may have run")
		return unknown, started, true
	}
	// The one read of the file serves the dangling calls and the turns that
	// started a tool call (closeDanglingTurns).
	for _, r := range recs {
		if r.Phase == foreman.CallStart && r.Turn >= 0 {
			started[r.Turn] = true
		}
	}
	now := s.clock().UTC().Format(foreman.DateLayout)
	for _, r := range foreman.DanglingCalls(recs) {
		line := fmt.Sprintf("%s(%s) started at %s, outcome unknown", r.Tool, clipArgs(r.Args), r.At)
		note := line + ". It is not run again. Check the workers before you ask for it again."
		_ = s.chatAppend(fc, foreman.KindNote, note)
		s.Note(note, "")
		fc.addUnknown(line)
		if r.Turn >= 0 {
			unknown[r.Turn] = true
		}
		_ = foreman.AppendCall(fc.dir, foreman.CallRecord{Call: r.Call, Phase: foreman.CallEnd, Outcome: foreman.CallUnknown, At: now})
	}
	return unknown, started, false
}

func (fc *foremanChat) addUnknown(line string) {
	fc.unkMu.Lock()
	fc.unknown = append(fc.unknown, line)
	fc.unkMu.Unlock()
}

// peekUnknown returns the state lines of unknown tool outcomes, and how
// many there are. They stay until a model has seen them (dropUnknown).
func (fc *foremanChat) peekUnknown() ([]string, int) {
	fc.unkMu.Lock()
	defer fc.unkMu.Unlock()
	return append([]string(nil), fc.unknown...), len(fc.unknown)
}

// dropUnknown removes the first n state lines: a model has seen them.
func (fc *foremanChat) dropUnknown(n int) {
	fc.unkMu.Lock()
	defer fc.unkMu.Unlock()
	if n > len(fc.unknown) {
		n = len(fc.unknown)
	}
	fc.unknown = fc.unknown[n:]
}

// clipArgs cuts the arguments shown in a note at 80 bytes.
func clipArgs(args string) string {
	if len(args) <= 80 {
		return args
	}
	n := 80
	for n > 0 && !utf8.RuneStart(args[n]) {
		n--
	}
	return args[:n] + "..."
}

// intArg reads a whole-number argument.
func intArg(args map[string]any, name string) (int64, error) {
	v, ok := args[name]
	if !ok {
		return 0, fmt.Errorf("%s is missing", name)
	}
	f, ok := v.(float64)
	if !ok || f != math.Trunc(f) || math.Abs(f) > 1<<53 {
		return 0, fmt.Errorf("%s must be a whole number", name)
	}
	return int64(f), nil
}

func strArg(args map[string]any, name string, need bool) (string, error) {
	v, ok := args[name]
	if !ok || v == nil {
		if need {
			return "", fmt.Errorf("%s is missing", name)
		}
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s must be text", name)
	}
	return s, nil
}

// noChat answers zoom and date when no chat is open. A turn's socket has
// one open, so for it this is an error. A foreman with its own memory is
// told so in one plain line.
func (ts *turnSocket) noChat() (string, error) {
	if ts.standing {
		return standingNoMemory, nil
	}
	return "", errNoChat
}

// call runs one of the four verbs. Any other tool is refused.
func (ts *turnSocket) call(req foreman.ToolRequest) (string, error) {
	switch req.Tool {
	case "zoom":
		if _, ok := req.Args["label"]; ok {
			if _, hasID := req.Args["id"]; hasID {
				return "", errors.New("give a label, or an id and an n, not both")
			}
			if _, hasN := req.Args["n"]; hasN {
				return "", errors.New("give a label, or an id and an n, not both")
			}
			if _, ok := req.Args["offset"]; ok {
				return "", errors.New("offset and size go with an id and an n, not a label")
			}
			if _, ok := req.Args["size"]; ok {
				return "", errors.New("offset and size go with an id and an n, not a label")
			}
			label, err := strArg(req.Args, "label", true)
			if err != nil {
				return "", err
			}
			return ts.s.toolZoomWorker(ts.fc, label)
		}
		id, err := intArg(req.Args, "id")
		if err != nil {
			return "", err
		}
		n, err := intArg(req.Args, "n")
		if err != nil {
			return "", err
		}
		var offset, size int64
		if _, ok := req.Args["offset"]; ok {
			if offset, err = intArg(req.Args, "offset"); err != nil {
				return "", err
			}
		}
		if _, ok := req.Args["size"]; ok {
			if size, err = intArg(req.Args, "size"); err != nil {
				return "", err
			}
		}
		ts.fc.mu.Lock()
		defer ts.fc.mu.Unlock()
		if ts.fc.chat == nil {
			return ts.noChat()
		}
		return ts.fc.chat.ZoomPage(id, n, offset, int(min(size, int64(foreman.ZoomPageBytes)+1)))
	case "date":
		id, err := intArg(req.Args, "id")
		if err != nil {
			return "", err
		}
		ts.fc.mu.Lock()
		defer ts.fc.mu.Unlock()
		if ts.fc.chat == nil {
			return ts.noChat()
		}
		return ts.fc.chat.Date(id)
	case "spawn":
		project, err := strArg(req.Args, "project", true)
		if err != nil {
			return "", err
		}
		brief, err := strArg(req.Args, "brief", true)
		if err != nil {
			return "", err
		}
		profile, err := strArg(req.Args, "profile", false)
		if err != nil {
			return "", err
		}
		model, err := strArg(req.Args, "model", false)
		if err != nil {
			return "", err
		}
		background := false
		if v, ok := req.Args["background"]; ok {
			b, isBool := v.(bool)
			if !isBool {
				return "", errors.New("background must be true or false")
			}
			background = b
		}
		if ts.standing {
			return ts.s.toolSpawnOpts(ts.fc, project, brief, profile, model, background)
		}
		return ts.s.toolSpawnBackground(project, brief, profile, model, background)
	case "status":
		if ts.standing {
			ts.s.loadWorkers(ts.fc)
		}
		return ts.s.toolStatus(), nil
	}
	names := append([]string{}, foreman.ToolNames...)
	sort.Strings(names)
	return "", fmt.Errorf("no tool named %q. The tools are %s", req.Tool, strings.Join(names, ", "))
}

// logToolCall logs one call and its result, word for word, as one tool
// message: name(args) on the first line, then the result.
func (s *Server) logToolCall(fc *foremanChat, tool, args, result string) {
	if err := s.chatAppend(fc, foreman.KindTool, tool+"("+args+")\n"+result); err != nil {
		s.noteOnce("The foreman's log could not take a tool call: " + err.Error())
	}
}

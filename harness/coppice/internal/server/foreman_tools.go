package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
	return s.listenTools(fc, path, false, nil)
}

// listenTools listens at path, 0600, and serves the four verbs. A stale
// socket file of an earlier run is removed; anything else at that name is
// an error. standing and rootFn are set before serving starts.
func (s *Server) listenTools(fc *foremanChat, path string, standing bool, rootFn func() int) (*turnSocket, error) {
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
	ts := &turnSocket{s: s, fc: fc, ln: ln, path: path, standing: standing, rootFn: rootFn}
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

func writeToolResponse(conn net.Conn, r foreman.ToolResponse) {
	b, _ := json.Marshal(r)
	_ = conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, _ = conn.Write(append(b, '\n'))
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
	text, callErr := ts.call(req)
	logged := text
	if callErr != nil {
		logged = "error: " + callErr.Error()
	}
	if !ts.standing || ts.fc.hasChat() {
		ts.s.logToolCall(ts.fc, req.Tool, string(args), logged)
	}
	if callErr != nil {
		writeToolResponse(conn, foreman.ToolResponse{Error: callErr.Error()})
		return
	}
	writeToolResponse(conn, foreman.ToolResponse{OK: true, Result: text})
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
		ts.fc.mu.Lock()
		defer ts.fc.mu.Unlock()
		if ts.fc.chat == nil {
			return ts.noChat()
		}
		return ts.fc.chat.Zoom(id, n)
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
		if ts.standing {
			return ts.s.toolSpawnIn(ts.fc, project, brief, profile, model)
		}
		return ts.s.toolSpawn(project, brief, profile, model)
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

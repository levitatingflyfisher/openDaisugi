package sprig

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// The MCP client: sprig starts the stdio servers of --mcp-config, offers
// their tools to the model as mcp__<server>__<tool>, and relays each call.
// A server is a process in a group of its own. The group dies when the run
// ends, on a signal, and with sprig.

// mcpMaxMessage is the most one server message may hold.
const mcpMaxMessage = 32 << 20

// mcpCallTimeout is how long one request waits for its answer.
var mcpCallTimeout = 30 * time.Second

// MCPServerSpec is one stdio server of a config.
type MCPServerSpec struct {
	Name    string
	Command string
	Args    []string
	Env     []string // NAME=value, sorted by name
}

// MCPToolDef is one tool as the model sees it.
type MCPToolDef struct {
	Name        string // mcp__<server>__<tool>
	Description string
	Schema      map[string]any
}

// ParseMCPConfig reads a --mcp-config value: inline JSON when it starts
// with "{", else the path of a file. It returns the servers by name, and
// the JSON text itself. Only stdio servers are taken.
func ParseMCPConfig(value string) ([]MCPServerSpec, string, error) {
	text := strings.TrimSpace(value)
	if !strings.HasPrefix(text, "{") {
		b, err := os.ReadFile(value)
		if err != nil {
			return nil, "", fmt.Errorf("--mcp-config: cannot read %s: %v", value, err)
		}
		text = strings.TrimSpace(string(b))
	}
	var doc struct {
		Servers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(text), &doc); err != nil || doc.Servers == nil {
		return nil, "", errors.New(`--mcp-config: want {"mcpServers": {name: {command, args, env}}}`)
	}
	names := make([]string, 0, len(doc.Servers))
	for n := range doc.Servers {
		names = append(names, n)
	}
	sort.Strings(names)
	var specs []MCPServerSpec
	for _, name := range names {
		var s struct {
			Type    *string           `json:"type"`
			Command *string           `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		}
		if err := json.Unmarshal(doc.Servers[name], &s); err != nil {
			return nil, "", fmt.Errorf("--mcp-config: server %q: want command, args and env", name)
		}
		if (s.Type != nil && *s.Type != "stdio") || s.Command == nil {
			return nil, "", fmt.Errorf("--mcp-config: server %q is not a stdio server; only command servers run", name)
		}
		if *s.Command == "" {
			return nil, "", fmt.Errorf("--mcp-config: server %q has an empty command", name)
		}
		spec := MCPServerSpec{Name: name, Command: *s.Command, Args: s.Args}
		keys := make([]string, 0, len(s.Env))
		for k := range s.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !scrubbed(k) {
				spec.Env = append(spec.Env, k+"="+s.Env[k])
			}
		}
		specs = append(specs, spec)
	}
	return specs, text, nil
}

type mcpLine struct {
	data []byte
	err  error
}

type mcpServer struct {
	name  string
	stdin io.WriteCloser
	lines chan mcpLine
	pgid  int
	// gone is set when the group had no member left and was forgotten: its
	// id may name another process by then, so it is never killed again.
	gone *atomic.Bool

	mu   sync.Mutex
	dead string // why the server is gone; empty while it runs
	next int
}

// MCPSet is the servers of a run and the tools they offer.
type MCPSet struct {
	servers []*mcpServer
	Defs    []MCPToolDef
	Tools   []Tool
}

var mcpToolName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// StartMCP starts every server, runs the handshake and lists the tools.
// A server that fails to start fails the run: a turn must not go on
// with a tool set the operator did not ask for. Notes go to note.
func StartMCP(specs []MCPServerSpec, note io.Writer) (*MCPSet, error) {
	set := &MCPSet{}
	for _, spec := range specs {
		if err := set.add(spec, note); err != nil {
			set.Close()
			return nil, fmt.Errorf("mcp server %s: %v", spec.Name, err)
		}
	}
	return set, nil
}

func (m *MCPSet) add(spec MCPServerSpec, note io.Writer) error {
	s, err := startMCPServer(spec)
	if err != nil {
		return err
	}
	m.servers = append(m.servers, s)
	if _, err := s.request("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "sprig", "version": "0"},
	}); err != nil {
		return err
	}
	_ = s.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	cursor := ""
	for page := 0; page < 10; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := s.request("tools/list", params)
		if err != nil {
			return err
		}
		var res struct {
			Tools []struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
			Next string `json:"nextCursor"`
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&res); err != nil {
			return errors.New("tools/list gave an unreadable answer")
		}
		for _, t := range res.Tools {
			full := "mcp__" + spec.Name + "__" + t.Name
			if t.Name == "" {
				fmt.Fprintf(note, "sprig: mcp server %s: a tool with no name skipped\n", spec.Name)
				continue
			}
			if m.has(full) {
				fmt.Fprintf(note, "sprig: mcp tool %s skipped: the name is already taken\n", full)
				continue
			}
			if !mcpToolName.MatchString(full) {
				fmt.Fprintf(note, "sprig: mcp tool %s skipped: the name is not usable\n", full)
				continue
			}
			schema := t.InputSchema
			if schema == nil {
				schema = map[string]any{"type": "object"}
			}
			def := MCPToolDef{Name: full, Description: t.Description, Schema: schema}
			m.Defs = append(m.Defs, def)
			m.Tools = append(m.Tools, &mcpTool{def: def, server: s, remote: t.Name})
		}
		if res.Next == "" {
			break
		}
		cursor = res.Next
	}
	return nil
}

func (m *MCPSet) has(name string) bool {
	for _, d := range m.Defs {
		if d.Name == name {
			return true
		}
	}
	return false
}

// Close ends the servers: stdin closed, each group killed.
func (m *MCPSet) Close() {
	for _, s := range m.servers {
		s.stdin.Close()
		s.killGroup()
	}
}

// startMCPServer starts the process in a group of its own. The goroutine
// that starts it keeps its OS thread until the process ends, since the
// parent-death signal follows that thread.
func startMCPServer(spec MCPServerSpec) (*mcpServer, error) {
	cmd := exec.Command(spec.Command, spec.Args...)
	cmd.Env = append(ChildEnv(""), spec.Env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	started := make(chan error, 1)
	gone := &atomic.Bool{}
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := cmd.Start(); err != nil {
			started <- err
			return
		}
		started <- nil
		_ = cmd.Wait()
		if forgetToolGroup(cmd.Process.Pid) {
			gone.Store(true)
		}
	}()
	if err := <-started; err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("the command %q was not found", spec.Command)
		}
		return nil, fmt.Errorf("the command %q cannot start", spec.Command)
	}
	s := &mcpServer{name: spec.Name, stdin: in, lines: make(chan mcpLine, 16), pgid: cmd.Process.Pid, gone: gone}
	toolGroups.Lock()
	toolGroups.ids = append(toolGroups.ids, s.pgid)
	toolGroups.Unlock()
	go func() {
		br := bufio.NewReaderSize(out, 64<<10)
		for {
			line, err := readMCPLine(br)
			s.lines <- mcpLine{line, err}
			if err != nil {
				return
			}
		}
	}()
	return s, nil
}

// readMCPLine reads one newline-ended message, at most mcpMaxMessage bytes.
func readMCPLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		part, err := br.ReadSlice('\n')
		buf = append(buf, part...)
		if len(buf) > mcpMaxMessage {
			return nil, errors.New("a message is larger than 32 MiB")
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return nil, io.EOF
		}
		return bytes.TrimSpace(buf), nil
	}
}

func (s *mcpServer) kill(why string) {
	s.mu.Lock()
	if s.dead == "" {
		s.dead = why
	}
	s.mu.Unlock()
	s.stdin.Close()
	s.killGroup()
}

func (s *mcpServer) killGroup() {
	if !s.gone.Load() {
		_ = syscall.Kill(-s.pgid, syscall.SIGKILL)
	}
}

func (s *mcpServer) send(msg map[string]any) error {
	b, _ := json.Marshal(msg)
	done := make(chan error, 1)
	go func() { _, err := s.stdin.Write(append(b, '\n')); done <- err }()
	select {
	case err := <-done:
		return err
	case <-time.After(mcpCallTimeout):
		return errors.New("the server does not read its input")
	}
}

// request sends one request and waits for its answer. A server that
// ends, breaks the format, or stays silent past mcpCallTimeout is dead
// from then on.
func (s *mcpServer) request(method string, params any) (json.RawMessage, error) {
	s.mu.Lock()
	if s.dead != "" {
		why := s.dead
		s.mu.Unlock()
		return nil, errors.New(why)
	}
	s.next++
	id := s.next
	s.mu.Unlock()
	if err := s.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		s.kill("the server is not running")
		return nil, errors.New("the server is not running")
	}
	timer := time.NewTimer(mcpCallTimeout)
	defer timer.Stop()
	for {
		select {
		case l := <-s.lines:
			if l.err != nil {
				why := "the server ended"
				if l.err != io.EOF {
					why = "the server broke the protocol: " + l.err.Error()
				}
				s.kill(why)
				return nil, errors.New(why)
			}
			var m struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(l.data, &m) != nil || len(m.ID) == 0 {
				continue // a notification, or noise
			}
			if m.Method != "" { // a request from the server: sprig answers none
				// The reply goes through the timed send, so a server that
				// reads nothing cannot hold the run past the call timeout.
				if s.send(map[string]any{"jsonrpc": "2.0", "id": m.ID,
					"error": map[string]any{"code": -32601, "message": "method not found"}}) != nil {
					s.kill("the server is not running")
					return nil, errors.New("the server is not running")
				}
				continue
			}
			if string(m.ID) != fmt.Sprint(id) {
				continue
			}
			if m.Error != nil {
				return nil, errors.New(m.Error.Message)
			}
			return m.Result, nil
		case <-timer.C:
			s.kill("the server gave no answer in 30 s")
			return nil, errors.New("the server gave no answer in 30 s")
		}
	}
}

// mcpTool is one remote tool as an executor tool.
type mcpTool struct {
	def    MCPToolDef
	server *mcpServer
	remote string
}

func (t *mcpTool) Name() string { return t.def.Name }

func (t *mcpTool) Run(in map[string]any) (string, error) {
	raw, err := t.server.request("tools/call", map[string]any{"name": t.remote, "arguments": nonNilInput(in)})
	if err != nil {
		return "", err
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if json.Unmarshal(raw, &res) != nil {
		return "", errors.New("the tool gave an unreadable result")
	}
	var parts []string
	for _, c := range res.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		} else {
			parts = append(parts, "["+c.Type+" content left out]")
		}
	}
	text := strings.Join(parts, "\n")
	if res.IsError {
		return "", errors.New(text)
	}
	return text, nil
}

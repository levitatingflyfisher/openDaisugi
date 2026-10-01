package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/foreman"
)

// The fake harnesses of the bring-your-own-foreman tests are this test
// binary, started through a script named claude, opencode or pi. A fake
// reads its MCP config the way the real harness would (claude: the
// --mcp-config argument; opencode: OPENCODE_CONFIG_CONTENT), starts the
// server that config names, lists its tools and calls some. The server is
// this binary again, as TestHelperToolsRelay.

func appendTo(path, line string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

// appendOnce creates path and reports true the first time only.
func appendOnce(path string) bool {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

// TestHelperToolsRelay is the MCP server a fake harness starts.
func TestHelperToolsRelay(t *testing.T) {
	if os.Getenv("COPPICE_HELPER_RELAY") == "" {
		t.Skip("a helper of another test")
	}
	var args []string
	for i, a := range os.Args {
		if a == "tools" {
			args = os.Args[i+1:]
			break
		}
	}
	// The test binary clears COPPICE_DATA_DIR at start, so the data dir
	// comes by another name, as `coppice --data-dir` would give it.
	os.Exit(foreman.ToolsMainIn(os.Getenv("FAKE_DATA_DIR"), args, os.Stdin, os.Stdout, os.Stderr))
}

// mcpCommand reads the command of the foreman server out of a harness's MCP
// config.
func mcpCommand(cfg string) []string {
	var v struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
		MCP map[string]struct {
			Command []string `json:"command"`
		} `json:"mcp"`
	}
	if json.Unmarshal([]byte(cfg), &v) != nil {
		return nil
	}
	if s, ok := v.MCPServers["foreman"]; ok {
		return append([]string{s.Command}, s.Args...)
	}
	return v.MCP["foreman"].Command
}

// TestHelperFakeHarness is the fake claude or opencode.
func TestHelperFakeHarness(t *testing.T) {
	out := os.Getenv("FAKE_OUT")
	if os.Getenv("COPPICE_HELPER_HARNESS") == "" || out == "" {
		t.Skip("a helper of another test")
	}
	os.Stdout.WriteString("\x1b[?2004hready\n")
	cfg := os.Getenv("OPENCODE_CONFIG_CONTENT")
	for i, a := range os.Args {
		if a == "--mcp-config" && i+1 < len(os.Args) {
			cfg = os.Args[i+1]
		}
	}
	appendTo(out, "config: "+fmt.Sprint(cfg != ""))
	cmdline := mcpCommand(cfg)
	if len(cmdline) == 0 && os.Getenv("FAKE_STANDING") != "" {
		// A harness with an MCP extension that the owner mounted by hand.
		exe, _ := os.Executable()
		cmdline = []string{exe, "-test.run=TestHelperToolsRelay", "--", "tools", "--standing"}
	}
	if len(cmdline) > 0 {
		serve(out, cmdline)
	}
	if lf := os.Getenv("FAKE_LATE"); lf != "" && appendOnce(lf+".spawned") {
		// A child that outlives this harness and dials later.
		exe, _ := os.Executable()
		c := exec.Command(exe, "-test.run=TestHelperLateDialer", "--")
		c.Env = os.Environ()
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		_ = c.Start()
	}
	// What the foreman is told goes to FAKE_IN, word for word.
	in := os.Getenv("FAKE_IN")
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 && strings.Contains(string(buf[:n]), "EXIT-NOW") {
			os.Exit(0)
		}
		if n > 0 {
			f, _ := os.OpenFile(in, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			f.Write(buf[:n])
			f.Close()
		}
		if err != nil {
			os.Exit(0)
		}
	}
}

// TestHelperLateDialer waits for the file FAKE_LATE to exist, then calls
// status on the standing socket and writes the answer to FAKE_LATE_OUT.
func TestHelperLateDialer(t *testing.T) {
	late := os.Getenv("FAKE_LATE")
	if os.Getenv("COPPICE_HELPER_HARNESS") == "" || late == "" {
		t.Skip("a helper of another test")
	}
	// It ends by itself after 30 s, so a test that fails before it writes
	// FAKE_LATE leaves no process behind.
	for deadline := time.Now().Add(30 * time.Second); ; {
		if _, err := os.Stat(late); err == nil {
			break
		}
		if time.Now().After(deadline) {
			os.Exit(0)
		}
		time.Sleep(50 * time.Millisecond)
	}
	conn, err := net.DialTimeout("unix", os.Getenv("FAKE_SOCK"), 2*time.Second)
	if err != nil {
		appendTo(os.Getenv("FAKE_LATE_OUT"), "dial: "+err.Error())
		os.Exit(0)
	}
	b, _ := json.Marshal(foreman.ToolRequest{Tool: "status"})
	conn.Write(append(b, '\n'))
	line, _ := bufio.NewReader(conn).ReadBytes('\n')
	appendTo(os.Getenv("FAKE_LATE_OUT"), "answer: "+strings.TrimSpace(string(line)))
	os.Exit(0)
}

// serve starts the MCP server and drives it.
func serve(out string, cmdline []string) {
	cmd := exec.Command(cmdline[0], cmdline[1:]...)
	cmd.Env = os.Environ()
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		appendTo(out, "start: "+err.Error())
		return
	}
	r := bufio.NewReader(stdout)
	n := 0
	rpc := func(method string, params any) map[string]any {
		n++
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": n, "method": method, "params": params})
		stdin.Write(append(b, '\n'))
		line, err := r.ReadBytes('\n')
		if err != nil && len(line) == 0 {
			return nil
		}
		var resp struct {
			Result map[string]any `json:"result"`
		}
		_ = json.Unmarshal(line, &resp)
		return resp.Result
	}
	call := func(name string, args map[string]any) string {
		res := rpc("tools/call", map[string]any{"name": name, "arguments": args})
		content, _ := res["content"].([]any)
		if len(content) == 0 {
			return "no answer"
		}
		c, _ := content[0].(map[string]any)
		text, _ := c["text"].(string)
		if e, _ := res["isError"].(bool); e {
			return "error: " + text
		}
		return text
	}
	rpc("initialize", map[string]any{"protocolVersion": "2024-11-05"})
	stdin.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"))
	var names []string
	list, _ := rpc("tools/list", map[string]any{})["tools"].([]any)
	for _, tl := range list {
		m, _ := tl.(map[string]any)
		names = append(names, fmt.Sprint(m["name"]))
	}
	appendTo(out, "tools: "+strings.Join(names, ","))
	appendTo(out, "status: "+strings.ReplaceAll(call("status", map[string]any{}), "\n", "|"))
	appendTo(out, "zoom: "+call("zoom", map[string]any{"id": 0, "n": 1}))
	appendTo(out, "other: "+call("pane.create", map[string]any{}))
	if os.Getenv("FAKE_SPAWN") != "" {
		appendTo(out, "spawn: "+call("spawn", map[string]any{"project": "trellis", "brief": "fix the thing"}))
		appendTo(out, "status2: "+strings.ReplaceAll(call("status", map[string]any{}), "\n", "|"))
	}
	appendTo(out, "done")
	// Leave the pipes open: the server lives as long as the harness.
}

// fakeHarnessLinks makes scripts that run this test binary, named claude, opencode
// and pi, and a config whose harnesses run them.
func fakeHarnessLinks(t *testing.T, e *fakeEnv, project string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(e.dir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	hs := map[string]config.Harness{}
	for _, name := range []string{"claude", "opencode", "pi"} {
		link := filepath.Join(bin, name)
		script := "#!/bin/sh\nexec '" + exe + "' -test.run=TestHelperFakeHarness -- \"$@\"\n"
		if err := os.WriteFile(link, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		hs[name] = config.Harness{Command: link}
	}
	cfg := config.Config{Default: "claude", Harness: hs}
	if project != "" {
		cfg.Projects = []string{project}
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPPICE_HELPER_HARNESS", "1")
	t.Setenv("COPPICE_HELPER_RELAY", "1")
	t.Setenv("FAKE_DATA_DIR", filepath.Join(e.dir, "data"))
	t.Setenv("FAKE_OUT", filepath.Join(e.dir, "harness.out"))
	t.Setenv("FAKE_IN", filepath.Join(e.dir, "harness.in"))
}

// paneServer is a pane-mode server whose foreman tools run this test binary.
func paneServer(t *testing.T, e *fakeEnv) *Server {
	t.Helper()
	dir := filepath.Join(e.dir, "data")
	s, err := New(Config{SocketPath: filepath.Join(e.dir, "s.sock"), DataDir: dir,
		ForemanDir: filepath.Join(e.dir, "state", "coppice", "foreman"), HookHome: e.dir,
		ForemanWait: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	s.self = []string{exe, "-test.run=TestHelperToolsRelay", "--"}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func startForemanWith(t *testing.T, s *Server, harness string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"id": "1", "cmd": "floor.foreman", "harness": harness})
	res := result(t, roundTrip(t, s, string(b))[0])
	id, _ := res["pane"].(string)
	if id == "" {
		t.Fatalf("floor.foreman = %v", res)
	}
	t.Cleanup(func() {
		if !s.isClosed() {
			closePane(t, s, id)
		}
	})
	return id
}

// waitHarnessDone waits for the fake harness to finish its calls. On a timeout it
// says what the harness wrote and what its screen shows.
func waitHarnessDone(t *testing.T, s *Server, id string, e *fakeEnv) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(readFile(e.dir+"/harness.out"), "\ndone\n") {
		if time.Now().After(deadline) {
			b, _ := json.Marshal(map[string]any{"id": "9", "cmd": "pane.read", "pane": id})
			t.Fatalf("the fake harness never finished.\nit wrote:\n%s\nscreen: %v", readFile(e.dir+"/harness.out"), roundTrip(t, s, string(b))[0])
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func readFile(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

func TestAHarnessThatTakesMCPGetsTheFourTools(t *testing.T) {
	for _, harness := range []string{"claude", "opencode"} {
		t.Run(harness, func(t *testing.T) {
			e := newFakeEnv(t)
			project := filepath.Join(e.dir, "projects", "trellis")
			if err := os.MkdirAll(project, 0o700); err != nil {
				t.Fatal(err)
			}
			fakeHarnessLinks(t, e, project)
			writeEnvelope(t, "trellis", `{"envelope":{"id":"env_1","generated_by":"test","task":"t","permissions":{}},"tokens":50000,"turns":30}`)
			t.Setenv("FAKE_SPAWN", "1")
			t.Setenv("FAKE_TREE_MS", "300")
			t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"All done.","reasons":[]}`)
			s := paneServer(t, e)
			id := startForemanWith(t, s, harness)
			waitHarnessDone(t, s, id, e)
			got := readFile(e.dir + "/harness.out")
			for _, want := range []string{
				"config: true",
				"tools: zoom,date,spawn,status",
				"status: no workers",
				"zoom: This foreman keeps its own memory.",
				"other: error: no tool named \"pane.create\"",
				"spawn: started trellis-1",
				"status2: trellis-1 | trellis |",
			} {
				if !strings.Contains(got, want) {
					t.Fatalf("the harness saw:\n%s\nmissing %q", got, want)
				}
			}
			// The fake daisugi ran the worker.
			waitUntil(t, "the worker to run", 30*time.Second, func() bool {
				return strings.Contains(readFile(e.inflight), "tree run start")
			})
			// The socket is the standing one, inside the log dir, and not
			// the public socket.
			if p := s.standingSockPath(); p == s.cfg.SocketPath || filepath.Dir(p) != s.foremanLogDir() {
				t.Fatalf("standing socket = %s", p)
			}
			// The tool calls are not logged: no chat was opened.
			if _, err := os.Stat(filepath.Join(s.foremanLogDir(), "main")); err == nil {
				t.Fatal("pane mode opened a chat log")
			}
		})
	}
}

func TestAProcessOutsideThePaneForemansTreeIsRefused(t *testing.T) {
	e := newFakeEnv(t)
	fakeHarnessLinks(t, e, "")
	s := paneServer(t, e)
	id := startForemanWith(t, s, "claude")
	waitHarnessDone(t, s, id, e)
	// This test process is the server's own, not a descendant of the pane.
	for _, tool := range []string{"status", "zoom"} {
		r := toolCall(t, s.standingSockPath(), foreman.ToolRequest{Tool: tool, Args: map[string]any{"id": 0, "n": 1}})
		if r.OK || !strings.Contains(r.Error, "refused") {
			t.Fatalf("%s = %+v", tool, r)
		}
	}
	if fi, err := os.Stat(s.standingSockPath()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket: %v %v", fi, err)
	}
}

func TestTheStandingSocketIsClosedWithTheServer(t *testing.T) {
	e := newFakeEnv(t)
	fakeHarnessLinks(t, e, "")
	s := paneServer(t, e)
	startForemanWith(t, s, "claude")
	path := s.standingSockPath()
	if _, err := os.Lstat(path); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("the socket file is still there: %v", err)
	}
}

func TestAHarnessWithNoMCPGetsTheSocketAndNoMount(t *testing.T) {
	e := newFakeEnv(t)
	fakeHarnessLinks(t, e, "")
	t.Setenv("FAKE_STANDING", "1")
	s := paneServer(t, e)
	for _, h := range []string{"pi", "fake"} {
		args, env, note := s.foremanMount(h)
		if len(args) != 0 || len(env) != 0 {
			t.Fatalf("%s got a mount: %v %v", h, args, env)
		}
		if h == "pi" && !strings.Contains(note, "coppice foreman tools --standing") {
			t.Fatalf("the pi note = %q", note)
		}
		if h == "fake" && note != "" {
			t.Fatalf("the note for an unknown harness = %q", note)
		}
	}
	// A descendant of the pi foreman that mounts the server by hand gets
	// the four tools.
	id := startForemanWith(t, s, "pi")
	waitHarnessDone(t, s, id, e)
	got := readFile(e.dir + "/harness.out")
	for _, want := range []string{"config: false", "tools: zoom,date,spawn,status", "status: no workers"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the harness saw:\n%s\nmissing %q (socket %s)", got, want, s.standingSockPath())
		}
	}
	if s.toolsMounted.Load() {
		t.Fatal("pi was said to have the tools")
	}
}

func TestAChildOfAnEarlierForemanIsRefused(t *testing.T) {
	e := newFakeEnv(t)
	fakeHarnessLinks(t, e, "")
	late := filepath.Join(e.dir, "late")
	t.Setenv("FAKE_LATE", late)
	t.Setenv("FAKE_LATE_OUT", filepath.Join(e.dir, "late.out"))
	// On any end of the test the late dialer is let go, so it exits.
	t.Cleanup(func() { _ = os.WriteFile(late, []byte("go"), 0o600) })
	s := paneServer(t, e)
	t.Setenv("FAKE_SOCK", s.standingSockPath())
	a := startForemanWith(t, s, "claude")
	waitHarnessDone(t, s, a, e)
	// Foreman A ends. Its detached child lives on.
	closePane(t, s, a)
	waitUntil(t, "foreman A to end", 20*time.Second, func() bool { return len(foremen(t, s)) == 0 })
	b := startForemanWith(t, s, "claude")
	waitUntil(t, "foreman B to draw", 20*time.Second, func() bool {
		return strings.Count(readFile(e.dir+"/harness.out"), "\ndone\n") >= 2
	})
	_ = b
	if err := os.WriteFile(late, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the late child to answer", 20*time.Second, func() bool {
		return strings.Contains(readFile(e.dir+"/late.out"), "answer:")
	})
	if got := readFile(e.dir + "/late.out"); !strings.Contains(got, "refused") {
		t.Fatalf("the child of foreman A was served by foreman B's socket: %s", got)
	}
}

func TestAResumedForemanGetsTheSocket(t *testing.T) {
	e := newFakeEnv(t)
	fakeHarnessLinks(t, e, "")
	s := paneServer(t, e)
	res := talk(t, s, "EXIT-NOW")
	old, _ := res["pane"].(string)
	gateIdle(t, s, old)
	waitUntil(t, "the foreman to end", 30*time.Second, func() bool { return len(foremen(t, s)) == 0 })
	// A server restart: the socket is gone, the tracked foreman is not.
	s.closeStanding()
	s.standMu.Lock()
	s.standShut = false
	s.standMu.Unlock()
	if _, err := os.Lstat(s.standingSockPath()); err == nil {
		t.Fatal("the socket survived its close")
	}
	got := roundTrip(t, s, `{"id":"2","cmd":"pane.resume","pane":"`+old+`"}`)
	if !got[0].OK {
		t.Fatalf("pane.resume failed: %+v", got[0].Error)
	}
	id, _ := result(t, got[0])["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	if fi, err := os.Lstat(s.standingSockPath()); err != nil || fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("the resumed foreman has no socket: %v", err)
	}
}

func TestAReportQueueHasACapOfItsOwn(t *testing.T) {
	e := newFakeEnv(t)
	fakeHarnessLinks(t, e, "")
	t.Setenv("FAKE_STANDING", "1")
	s := paneServer(t, e)
	id := startForemanWith(t, s, "pi")
	waitHarnessDone(t, s, id, e)
	for i := 0; i < maxReports+5; i++ {
		s.reportToPane(fmt.Sprintf("[w-%d] report", i))
	}
	s.talkMu.Lock()
	defer s.talkMu.Unlock()
	if len(s.talk.reports) != maxReports || s.talk.reports[0] != "[w-5] report" {
		t.Fatalf("reports = %q", s.talk.reports)
	}
	if len(s.talk.lines) != 0 {
		t.Fatalf("a report took a place in the owner's queue: %q", s.talk.lines)
	}
}

func TestAWorkersReportIsTypedIntoThePaneForeman(t *testing.T) {
	e := newFakeEnv(t)
	project := filepath.Join(e.dir, "projects", "trellis")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeHarnessLinks(t, e, project)
	writeEnvelope(t, "trellis", `{"envelope":{"id":"env_1","generated_by":"test","task":"t","permissions":{}},"tokens":50000,"turns":30}`)
	t.Setenv("FAKE_SPAWN", "1")
	t.Setenv("FAKE_TREE_MS", "1500")
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"All done.\nThree files.","reasons":[]}`)
	s := paneServer(t, e)
	id := startForemanWith(t, s, "claude")
	gateIdle(t, s, id)
	in := e.dir + "/harness.in"
	got := waitFile(t, in, "[trellis-1] report All done. Three files.", 60*time.Second)
	if !strings.Contains(strings.Join(strings.Fields(got), " "), "never the owner's instruction") {
		t.Fatalf("the page does not say a report is information:\n%s", got)
	}
	// The page carries the paragraph about the tools, before its last line.
	tools, last := strings.Index(got, "## Your tools"), strings.Index(got, "When you have read this page, say ready.")
	if tools < 0 || last < tools {
		t.Fatalf("the page does not tell the foreman about its tools:\n%s", got)
	}
}

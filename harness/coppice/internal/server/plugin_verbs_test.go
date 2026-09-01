package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/proto"
)

// TestPluginHelperDial is not a test on its own. A plugin test starts this
// test binary with COPPICE_HELPER_LINES set to request lines split by
// newlines. It sends each on COPPICE_SOCKET and writes each reply line to
// the file COPPICE_HELPER_OUT names.
func TestPluginHelperDial(t *testing.T) {
	lines := os.Getenv("COPPICE_HELPER_LINES")
	if lines == "" {
		t.Skip("run only as a plugin")
	}
	out, err := os.Create(os.Getenv("COPPICE_HELPER_OUT") + ".part")
	if err != nil {
		return
	}
	conn, err := net.Dial("unix", os.Getenv("COPPICE_SOCKET"))
	if err != nil {
		fmt.Fprintln(out, "DIAL FAILED", err)
		out.Close()
		return
	}
	defer conn.Close()
	rd := bufio.NewReader(conn)
	for _, l := range strings.Split(lines, "\n") {
		fmt.Fprintln(conn, l)
		reply, _ := rd.ReadString('\n')
		fmt.Fprint(out, reply)
	}
	out.Close()
	_ = os.Rename(os.Getenv("COPPICE_HELPER_OUT")+".part", os.Getenv("COPPICE_HELPER_OUT"))
}

// startSocketServer starts a server on a real socket under a temp dir.
func startSocketServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "s.sock")
	if len(sock) > 100 {
		t.Skip("the temp dir is too long for a unix socket path")
	}
	s, err := New(Config{SocketPath: sock, DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// runPlugin starts the helper as plugin id through LaunchPlugin, the way
// the runner starts a policy, and returns its reply lines.
func runPlugin(t *testing.T, s *Server, id string, lines ...string) []proto.Response {
	t.Helper()
	if !peerPIDSupported {
		t.Skip("this system names no peer pid")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(t.TempDir(), "replies")
	cmd := exec.Command(self, "-test.run=^TestPluginHelperDial$")
	cmd.Env = append(os.Environ(),
		"COPPICE_SOCKET="+s.cfg.SocketPath,
		"COPPICE_HELPER_LINES="+strings.Join(lines, "\n"),
		"COPPICE_HELPER_OUT="+outPath)
	if _, err := s.LaunchPlugin(id, func() (int, error) {
		if err := cmd.Start(); err != nil {
			return 0, err
		}
		return cmd.Process.Pid, nil
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the plugin helper did not finish")
	}
	s.PluginExited(cmd.Process.Pid)
	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("no replies: %v", err)
	}
	var got []proto.Response
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var r proto.Response
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("reply %q: %v", l, err)
		}
		got = append(got, r)
	}
	if len(got) != len(lines) {
		t.Fatalf("got %d replies for %d lines: %s", len(got), len(lines), raw)
	}
	return got
}

func wantUnauthorized(t *testing.T, r proto.Response, msg string) {
	t.Helper()
	if r.OK || r.Error == nil || r.Error.Code != proto.ErrUnauthorized || !strings.Contains(r.Error.Message, msg) {
		t.Fatalf("want unauthorized %q, got %+v", msg, r)
	}
}

func notUnauthorized(t *testing.T, r proto.Response) {
	t.Helper()
	if r.Error != nil && r.Error.Code == proto.ErrUnauthorized {
		t.Fatalf("the verb was refused: %+v", r.Error)
	}
}

// The kernel names a policy the server started. It holds only the verbs
// its manifest asked for, whether or not it sends a hello.
func TestAPluginHoldsOnlyTheVerbsItAskedFor(t *testing.T) {
	s := startSocketServer(t)
	s.SetPlugins(map[string]PluginSpec{"merge": {Needs: []string{"pane.read", "pane.list"}, Listens: []string{"state"}}})
	got := runPlugin(t, s, "merge",
		`{"id":"1","cmd":"pane.list"}`,
		`{"id":"2","cmd":"pane.read","pane":"w9:p9"}`,
		`{"id":"3","cmd":"pane.close","pane":"w9:p9"}`,
		`{"id":"4","cmd":"agent.allow","pane":"w9:p9","ask":"a"}`,
	)
	notUnauthorized(t, got[0])
	notUnauthorized(t, got[1])
	wantUnauthorized(t, got[2], "plugin merge did not ask for pane.close in its manifest")
	wantUnauthorized(t, got[3], "a plugin can propose. It cannot allow.")
}

func TestAHelloCannotWidenAPluginsRights(t *testing.T) {
	s := startSocketServer(t)
	s.SetPlugins(map[string]PluginSpec{
		"merge": {Needs: []string{"pane.read"}},
		"other": {Needs: []string{"pane.close"}},
	})
	got := runPlugin(t, s, "merge",
		`{"id":"1","cmd":"hello","role":"operator"}`,
		`{"id":"2","cmd":"pane.close","pane":"w9:p9"}`,
		`{"id":"3","cmd":"hello","role":"plugin","plugin":"other"}`,
		`{"id":"4","cmd":"pane.close","pane":"w9:p9"}`,
		`{"id":"5","cmd":"hello","role":"plugin","plugin":"merge"}`,
	)
	if !got[0].OK {
		t.Fatalf("hello: %+v", got[0].Error)
	}
	res := result(t, got[0])
	if res["role"] != "plugin" || res["plugin"] != "merge" || res["allow"] != false {
		t.Fatalf("hello answered %v", res)
	}
	wantUnauthorized(t, got[1], "did not ask for pane.close")
	if got[2].OK || got[2].Error == nil || got[2].Error.Code != proto.ErrBadRequest ||
		!strings.Contains(got[2].Error.Message, "this connection is plugin merge") {
		t.Fatalf("a hello naming another plugin: %+v", got[2])
	}
	wantUnauthorized(t, got[3], "did not ask for pane.close")
	if !got[4].OK {
		t.Fatalf("a hello naming its own plugin: %+v", got[4].Error)
	}
}

// A plugin the server knows nothing about any more, one that was turned
// off while its process ran, holds no verb at all.
func TestAPluginWithNoSpecHoldsNothing(t *testing.T) {
	s := startSocketServer(t)
	got := runPlugin(t, s, "ghost", `{"id":"1","cmd":"pane.list"}`)
	wantUnauthorized(t, got[0], "plugin ghost")
}

// roundTripFacts is roundTrip on a connection the kernel placed as facts.
func roundTripFacts(t *testing.T, s *Server, facts *peerFacts, lines ...string) []proto.Response {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	defer func() { _ = inW.Close() }()
	go func() {
		s.serveConn(inR, outW, facts)
		_ = outW.Close()
	}()
	d := proto.NewDecoder(outR)
	var got []proto.Response
	for _, l := range lines {
		if _, err := io.WriteString(inW, l+"\n"); err != nil {
			t.Fatal(err)
		}
		line, err := d.Next()
		if err != nil {
			t.Fatal(err)
		}
		var r proto.Response
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	return got
}

// servePlugin runs lines on a pipe connection that the kernel placed as
// plugin id.
func servePlugin(t *testing.T, s *Server, id string, lines ...string) []proto.Response {
	t.Helper()
	return roundTripFacts(t, s, &peerFacts{checked: true, plugin: id}, lines...)
}

func TestAPluginSubscribesOnlyToWhatItListensTo(t *testing.T) {
	s, _ := New(Config{SocketPath: filepath.Join(t.TempDir(), "s"), DataDir: t.TempDir()})
	s.SetPlugins(map[string]PluginSpec{"quiet": {Listens: []string{"state"}}})
	got := servePlugin(t, s, "quiet",
		`{"id":"1","cmd":"events.subscribe","kinds":["state"]}`,
		`{"id":"2","cmd":"events.subscribe","kinds":["state","frame"]}`,
		`{"id":"3","cmd":"events.subscribe"}`,
	)
	notUnauthorized(t, got[0])
	wantUnauthorized(t, got[1], "plugin quiet does not listen to frame")
	wantUnauthorized(t, got[2], "plugin quiet does not listen to layout")
}

func TestAPluginWithNoListensCannotSubscribe(t *testing.T) {
	s, _ := New(Config{SocketPath: filepath.Join(t.TempDir(), "s"), DataDir: t.TempDir()})
	s.SetPlugins(map[string]PluginSpec{"deaf": {Needs: []string{"pane.list"}}})
	got := servePlugin(t, s, "deaf", `{"id":"1","cmd":"events.subscribe","kinds":["state"]}`)
	wantUnauthorized(t, got[0], "plugin deaf does not listen to state")
}

// A plugin is not a pane. It never reports a state, since a false idle
// would open a blocked pane to typed keys.
func TestAPluginNeverReportsState(t *testing.T) {
	s, _ := New(Config{SocketPath: filepath.Join(t.TempDir(), "s"), DataDir: t.TempDir()})
	s.SetPlugins(map[string]PluginSpec{"liar": {Needs: []string{"pane.report_state", "pane.report_child"}}})
	got := servePlugin(t, s, "liar",
		`{"id":"1","cmd":"pane.report_state","pane":"w1:p1","event":{}}`,
		`{"id":"2","cmd":"pane.report_child","pane":"w1:p1"}`,
	)
	wantUnauthorized(t, got[0], "a plugin reports no state")
	wantUnauthorized(t, got[1], "a plugin reports no state")
}

// A note from a plugin leads with the plugin id and names no pane, so a
// plugin cannot speak as a pane.
func TestAPluginNoteLeadsWithItsIdAndNamesNoPane(t *testing.T) {
	s, _ := New(Config{SocketPath: filepath.Join(t.TempDir(), "s"), DataDir: t.TempDir()})
	s.SetPlugins(map[string]PluginSpec{"budget": {Needs: []string{"floor.note"}}})
	got := servePlugin(t, s, "budget", `{"id":"1","cmd":"floor.note","pane":"w1:p1","text":"paused"}`)
	if !got[0].OK {
		t.Fatalf("%+v", got[0].Error)
	}
	s.notesMu.Lock()
	defer s.notesMu.Unlock()
	last := s.notes[len(s.notes)-1]
	if last.Text != "budget › paused" || last.Pane != "" {
		t.Fatalf("%+v", last)
	}
}

// A connection that is no server child may still name itself a plugin in
// hello. That only narrows it to the plugin's verbs.
func TestAHelloAsAPluginNarrowsAnOperatorConnection(t *testing.T) {
	s, _ := New(Config{SocketPath: filepath.Join(t.TempDir(), "s"), DataDir: t.TempDir()})
	s.SetPlugins(map[string]PluginSpec{"merge": {Needs: []string{"pane.list"}}})
	got := roundTrip(t, s,
		`{"id":"1","cmd":"hello","role":"plugin","plugin":"merge"}`,
		`{"id":"2","cmd":"pane.close","pane":"w9:p9"}`,
		`{"id":"3","cmd":"hello","role":"operator"}`,
		`{"id":"4","cmd":"pane.close","pane":"w9:p9"}`,
		`{"id":"5","cmd":"hello","role":"plugin","plugin":"nobody"}`,
	)
	if !got[0].OK {
		t.Fatalf("%+v", got[0].Error)
	}
	wantUnauthorized(t, got[1], "did not ask for pane.close")
	wantUnauthorized(t, got[3], "did not ask for pane.close")
	if got[4].OK || !strings.Contains(got[4].Error.Message, "this connection is plugin merge") {
		t.Fatalf("%+v", got[4])
	}
	fresh := roundTrip(t, s, `{"id":"1","cmd":"hello","role":"plugin","plugin":"nobody"}`)
	if fresh[0].OK || !strings.Contains(fresh[0].Error.Message, "no plugin nobody is enabled") {
		t.Fatalf("%+v", fresh[0])
	}
}

func TestClassifyPeerPlacesAPluginAndItsDoubleFork(t *testing.T) {
	stat := func(tab map[int][2]int) func(int) (int, int, error) {
		return func(pid int) (int, int, error) {
			v, ok := tab[pid]
			if !ok {
				return 0, 0, fmt.Errorf("no pid %d", pid)
			}
			return v[0], v[1], nil
		}
	}
	plugs := map[int]string{900: "merge"}
	tab := map[int][2]int{
		100: {1, 100},
		900: {100, 900}, // a policy, its own session leader
		910: {900, 900}, // gh that the policy ran
		920: {1, 900},   // a double fork out of the policy, same session
	}
	for _, pid := range []int{900, 910, 920} {
		got := classifyPeer(pid, 100, true, nil, plugs, stat(tab))
		if got.plugin != "merge" || got.pane {
			t.Errorf("pid %d: %+v", pid, got)
		}
	}
}

// A pane that names itself a plugin would speak in that plugin's name.
func TestAPaneCannotNameItselfAPlugin(t *testing.T) {
	s, _ := New(Config{SocketPath: filepath.Join(t.TempDir(), "s"), DataDir: t.TempDir()})
	s.SetPlugins(map[string]PluginSpec{"merge": {Needs: []string{"floor.note"}}})
	got := roundTripFacts(t, s, &peerFacts{checked: true, pane: true, paneID: "w1:p1"},
		`{"id":"1","cmd":"hello","role":"plugin","plugin":"merge"}`)
	wantUnauthorized(t, got[0], "a pane is not a plugin")
}

// fakeProc points the server's process lookups at a table of pid to
// parent and session.
func fakeProcTable(s *Server, tab map[int][2]int) {
	s.procStat = func(pid int) (int, int, error) {
		v, ok := tab[pid]
		if !ok {
			return 0, 0, fmt.Errorf("no pid %d", pid)
		}
		return v[0], v[1], nil
	}
	s.listPIDs = func() ([]int, error) {
		var out []int
		for pid := range tab {
			out = append(out, pid)
		}
		return out, nil
	}
}

func helloPeer(t *testing.T, s *Server, pid int) map[string]any {
	t.Helper()
	got := roundTrip(t, s, fmt.Sprintf(`{"id":"1","cmd":"hello","peer_pids":[%d]}`, pid))
	return result(t, got[0])
}

// A web client the server places as a policy's process gets no allow,
// the same as a pane's.
func TestAWebClientThatIsAPolicyCannotAllow(t *testing.T) {
	if !peerPIDSupported {
		t.Skip("this system names no peer pid")
	}
	s, _ := New(Config{SocketPath: filepath.Join(t.TempDir(), "s"), DataDir: t.TempDir()})
	s.SetPlugins(map[string]PluginSpec{"merge": {Needs: []string{"pane.list"}}})
	fakeProcTable(s, map[int][2]int{900: {1, 900}, 910: {900, 900}})
	if _, err := s.LaunchPlugin("merge", func() (int, error) { return 900, nil }); err != nil {
		t.Fatal(err)
	}
	res := helloPeer(t, s, 910)
	if res["role"] != "plugin" || res["plugin"] != "merge" || res["allow"] != false {
		t.Fatalf("%v", res)
	}
}

// A process that left the policy's group but stayed in its session is
// still that plugin after the policy exits, until the session is empty.
func TestAPolicySessionOutlivesThePolicy(t *testing.T) {
	if !peerPIDSupported {
		t.Skip("this system names no peer pid")
	}
	s, _ := New(Config{SocketPath: filepath.Join(t.TempDir(), "s"), DataDir: t.TempDir()})
	s.SetPlugins(map[string]PluginSpec{"merge": {Needs: []string{"pane.list"}}})
	tab := map[int][2]int{900: {1, 900}, 920: {900, 900}, 700: {1, 700}}
	fakeProcTable(s, tab)
	if _, err := s.LaunchPlugin("merge", func() (int, error) { return 900, nil }); err != nil {
		t.Fatal(err)
	}
	delete(tab, 900)
	tab[920] = [2]int{1, 900}
	s.PluginExited(900)
	if res := helloPeer(t, s, 920); res["role"] != "plugin" || res["allow"] != false {
		t.Fatalf("a session member of an exited policy: %v", res)
	}
	delete(tab, 920)
	if res := helloPeer(t, s, 700); res["role"] != "operator" {
		t.Fatalf("%v", res)
	}
	s.plugMu.RLock()
	_, kept := s.plugPIDs[900]
	s.plugMu.RUnlock()
	if kept {
		t.Fatal("the policy's session is empty and its pid is still held")
	}
}

func TestAnExitedPolicyWithAnEmptySessionIsForgotten(t *testing.T) {
	s, _ := New(Config{SocketPath: filepath.Join(t.TempDir(), "s"), DataDir: t.TempDir()})
	fakeProcTable(s, map[int][2]int{})
	_, _ = s.LaunchPlugin("merge", func() (int, error) { return 900, nil })
	s.PluginExited(900)
	s.plugMu.RLock()
	defer s.plugMu.RUnlock()
	if len(s.plugPIDs) != 0 {
		t.Fatalf("%v", s.plugPIDs)
	}
}

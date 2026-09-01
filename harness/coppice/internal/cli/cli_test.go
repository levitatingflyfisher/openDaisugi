package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/server"
	"github.com/opendaisugi/coppice/internal/toolchain"

	_ "github.com/opendaisugi/coppice/internal/adapters/all"
)

func runCLI(t *testing.T, sock, dataDir string, argv ...string) (int, string, string) {
	t.Helper()
	// Dial autostarts a server when nothing is listening. Tests that want a
	// failing dial to stay a failing dial turn that off.
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	var out, errb bytes.Buffer
	c := &CLI{Version: "test", Socket: sock, DataDir: dataDir, In: strings.NewReader(""), Out: &out, Err: &errb}
	code := c.Run(argv)
	return code, out.String(), errb.String()
}

// liveServer registers everything New does not, the same set registerAll
// wires in the real binary, so a test here can drive server.stop too.
func liveServer(t *testing.T) (sock, dataDir string) {
	t.Helper()
	toolchain.RequireOrSkip(t)
	dir := t.TempDir()
	sock = filepath.Join(dir, "server.sock")
	s, err := server.New(server.Config{SocketPath: sock, DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterStopCommand(func() { go func() { _ = s.Close() }() }); err != nil {
		t.Fatal(err)
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()
	t.Cleanup(func() { _ = s.Close() })
	return sock, dir
}

func TestNoArgumentsPrintsUsageAndExitsOne(t *testing.T) {
	code, out, errb := runCLI(t, "/nonexistent.sock", t.TempDir())
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	text := out + errb
	for _, want := range []string{"coppice", "server", "pane", "agent", "attach"} {
		if !strings.Contains(text, want) {
			t.Fatalf("usage %q does not mention %q", text, want)
		}
	}
}

// Exit code 3 is "unreachable", per the global exit-code rule.
func TestAnUnreachableServerExitsThreeWithATeachingMessage(t *testing.T) {
	dir := t.TempDir()
	code, _, errb := runCLI(t, filepath.Join(dir, "nope.sock"), dir, "pane", "list")
	if code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
	if !strings.Contains(errb, "coppice server start") {
		t.Fatalf("error %q does not name the command that fixes it", errb)
	}
}

func TestAnUnknownVerbExitsOneAndListsTheVerbs(t *testing.T) {
	code, out, errb := runCLI(t, "/nonexistent.sock", t.TempDir(), "teleport")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(out+errb, "pane") {
		t.Fatalf("the error does not list the verbs: %q", out+errb)
	}
}

func TestPaneCreateAndListOverTheSocket(t *testing.T) {
	sock, dir := liveServer(t)
	cwd := t.TempDir()
	code, out, errb := runCLI(t, sock, dir, "pane", "create", "--cwd", cwd, "--label", "one",
		"--", "sh", "-c", "sleep 300")
	if code != 0 {
		t.Fatalf("pane create exit %d: %s %s", code, out, errb)
	}
	if !strings.Contains(out, "w1:p1") {
		t.Fatalf("pane create printed %q, want the new pane id", out)
	}
	code, out, _ = runCLI(t, sock, dir, "pane", "list", "--json")
	if code != 0 {
		t.Fatalf("pane list --json exit %d", code)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("--json did not print one JSON object: %q", out)
	}
	if _, ok := res["panes"]; !ok {
		t.Fatalf("--json result has no panes key: %q", out)
	}
}

// The human table must not be the JSON. Two audiences, two outputs.
func TestPaneListWithoutJSONPrintsATable(t *testing.T) {
	sock, dir := liveServer(t)
	cwd := t.TempDir()
	runCLI(t, sock, dir, "pane", "create", "--cwd", cwd, "--", "sh", "-c", "sleep 300")
	code, out, _ := runCLI(t, sock, dir, "pane", "list")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Fatalf("pane list printed JSON without --json: %q", out)
	}
	for _, want := range []string{"w1:p1", "pty"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the table %q is missing %q", out, want)
		}
	}
}

// The table printer had no sign a pane's own row was
// closed - a closed pane and a live one printed identically. The state
// column is not a substitute: a pane that never started at all also shows
// closed with no state to say so.
func TestPaneListTableMarksAClosedRow(t *testing.T) {
	sock, dir := liveServer(t)
	cwd := t.TempDir()
	runCLI(t, sock, dir, "pane", "create", "--cwd", cwd, "--", "sh", "-c", "sleep 300")
	runCLI(t, sock, dir, "pane", "create", "--cwd", cwd, "--", "sh", "-c", "sleep 300")
	code, out, errb := runCLI(t, sock, dir, "pane", "close", "w1:p1")
	if code != 0 {
		t.Fatalf("pane close exit %d: %s %s", code, out, errb)
	}
	code, out, errb = runCLI(t, sock, dir, "pane", "list")
	if code != 0 {
		t.Fatalf("pane list exit %d: %s %s", code, out, errb)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	var closedLine, openLine string
	for _, l := range lines {
		if strings.HasPrefix(l, "w1:p1 ") {
			closedLine = l
		}
		if strings.HasPrefix(l, "w1:p2 ") {
			openLine = l
		}
	}
	if !strings.Contains(closedLine, "closed") {
		t.Fatalf("closed pane's row %q does not mark it closed", closedLine)
	}
	if strings.Contains(openLine, "closed") {
		t.Fatalf("open pane's row %q wrongly says closed", openLine)
	}
}

// A gate deny is exit 2, distinct from a user error.
func TestAServerErrorMapsToExitOneAndPrintsTheCode(t *testing.T) {
	sock, dir := liveServer(t)
	code, _, errb := runCLI(t, sock, dir, "pane", "read", "w9:p9")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 for a user error", code)
	}
	if !strings.Contains(errb, "no_such_pane") {
		t.Fatalf("error %q does not carry the code", errb)
	}
}

// --stdio must be a PROXY to the running server, not a second server. A second
// server would take DataDir from the default, write its own empty tree over
// the running server's layout.json, and make `coppice --remote ssh://host
// pane list` report zero panes because it was talking to a fresh in-process
// server instead of the host's.
func TestStdioProxiesToTheRunningServerAndNeverWritesTheLayout(t *testing.T) {
	sock, dir := liveServer(t)

	// One real pane, so the layout on disk is not empty. sleep 300, not a
	// short sleep: watchExit rewrites layout.json the moment the process
	// exits, and this test compares the file byte for byte.
	cwd := t.TempDir()
	runCLI(t, sock, dir, "pane", "create", "--cwd", cwd, "--label", "real", "--", "sh", "-c", "sleep 300")

	layoutPath := filepath.Join(dir, "layout.json")
	before, err := os.ReadFile(layoutPath)
	if err != nil {
		t.Fatal(err)
	}

	// Drive --stdio and ask for the pane list. It must see the running
	// server's pane.
	var out bytes.Buffer
	in := strings.NewReader(`{"id":"1","cmd":"pane.list"}` + "\n")
	c := &CLI{Version: "test", Socket: sock, DataDir: t.TempDir(), In: in, Out: &out, Err: &bytes.Buffer{}}
	if code := c.Run([]string{"--stdio"}); code != 0 {
		t.Fatalf("--stdio exit %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "w1:p1") {
		t.Fatalf("--stdio returned %q, want the running server's pane w1:p1", out.String())
	}
	if !strings.Contains(out.String(), "real") {
		t.Fatalf("--stdio returned %q, want the running server's label", out.String())
	}

	after, err := os.ReadFile(layoutPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("--stdio rewrote layout.json.\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// The other half of the same blocker: a --stdio session must not create a
// server of its own even when none is reachable and autostart is off. It must
// watch the DIRECTORY A RUNAWAY SERVER WOULD ACTUALLY WRITE: --data-dir, not
// some unrelated temp directory the failure would never touch.
func TestStdioWithNoServerFailsRatherThanBuildingOne(t *testing.T) {
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	dir := t.TempDir()
	var out, errb bytes.Buffer
	c := &CLI{Version: "test", Socket: filepath.Join(dir, "absent.sock"), DataDir: dir,
		In: strings.NewReader(`{"id":"1","cmd":"pane.list"}` + "\n"), Out: &out, Err: &errb}
	if code := c.Run([]string{"--stdio"}); code != 3 {
		t.Fatalf("--stdio with no server exited %d, want 3", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "layout.json")); err == nil {
		t.Fatal("--stdio created a layout.json, so it built a server")
	}
}

func TestRemoteTargetBuildsTheSSHCommand(t *testing.T) {
	argv, err := remoteArgv("ssh://build-box")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ssh", "build-box", "coppice", "--stdio"}
	if strings.Join(argv, " ") != strings.Join(want, " ") {
		t.Fatalf("remoteArgv = %v, want %v", argv, want)
	}
}

func TestRemoteTargetRejectsANonSSHScheme(t *testing.T) {
	if _, err := remoteArgv("http://example.com"); err == nil {
		t.Fatal("remoteArgv accepted an http target, want an error naming ssh://")
	}
}

func TestVersionPrintsTheVersionAndExitsZero(t *testing.T) {
	code, out, _ := runCLI(t, "/nonexistent.sock", t.TempDir(), "--version")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out, "test") {
		t.Fatalf("--version printed %q, want the version", out)
	}
}

// server start on a server that is already up is idempotent: exit 0, and say
// so, rather than spawning a redundant child that will just lose the lock.
func TestServerStartWhenAlreadyRunningIsIdempotent(t *testing.T) {
	sock, dir := liveServer(t)
	code, out, errb := runCLI(t, sock, dir, "server", "start")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errb)
	}
	if !strings.Contains(out, "already running") {
		t.Fatalf("output %q does not say already running", out)
	}
}

func TestServerStopStopsTheServer(t *testing.T) {
	sock, dir := liveServer(t)
	code, out, errb := runCLI(t, sock, dir, "server", "stop")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errb)
	}
	if !strings.Contains(out, "stop") {
		t.Fatalf("output %q does not confirm the stop", out)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := net.Dial("unix", sock); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the socket still accepts connections after server stop")
}

// server stop's purpose is "make sure none is running",
// the same reasoning that makes server start idempotent - so it must not
// start one just to stop it. COPPICE_NO_AUTOSTART is deliberately left
// unset, the same discriminating shape TestAttachWithNoPaneArgumentDoesNotAutostart
// uses: a regression to the autostarting dial leaves server.log in DataDir
// before the doomed spawn (this test binary, not a real coppice) even runs.
func TestServerStopWithNothingRunningDoesNotAutostart(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "server.sock")
	var out, errb bytes.Buffer
	c := &CLI{Version: "test", Socket: sock, DataDir: dir, In: strings.NewReader(""), Out: &out, Err: &errb}
	code := c.Run([]string{"server", "stop"})
	if code != 0 {
		t.Fatalf("exit %d, want 0: %s%s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "no server is running") {
		t.Fatalf("output %q does not say no server is running", out.String())
	}
	assertNoAutostartArtifacts(t, dir)
}

func TestServerStopWithNothingRunningJSONShape(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "server.sock")
	code, out, errb := runCLI(t, sock, dir, "server", "stop", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errb)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("server stop --json did not print one JSON object: %q: %v", out, err)
	}
	if res["stopped"] != false {
		t.Fatalf("server stop --json = %v, want stopped false", res)
	}
}

func TestServerStatusPrintsCountsAndTheRestartNote(t *testing.T) {
	sock, dir := liveServer(t)
	code, out, errb := runCLI(t, sock, dir, "server", "status")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errb)
	}
	for _, want := range []string{"pid", "A restart brings back"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status output %q missing %q", out, want)
		}
	}
}

func TestServerTokenNamesTheSocketAsTheCredential(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "server.sock")
	code, out, errb := runCLI(t, sock, dir, "server", "token")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errb)
	}
	if !strings.Contains(out, sock) || !strings.Contains(out, "0600") {
		t.Fatalf("token output %q does not name the socket as the credential", out)
	}
}

func TestServerStartWithRemoteIsRefused(t *testing.T) {
	code, _, errb := runCLI(t, "/nonexistent.sock", t.TempDir(), "--remote", "ssh://host", "server", "start")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errb, "remote host itself") {
		t.Fatalf("error %q does not explain why", errb)
	}
}

// Every other verb refuses an unknown flag by name.
// attach took argv[0] verbatim, so "coppice attach --bogus" answered
// no_such_pane for a pane literally named "--bogus" instead of naming the
// mistake.
func TestAttachRefusesAnArgumentStartingWithDashDash(t *testing.T) {
	code, _, errb := runCLI(t, "/nonexistent.sock", t.TempDir(), "attach", "--bogus")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, errb)
	}
	if !strings.Contains(errb, "--bogus") {
		t.Fatalf("error %q does not name the unknown flag", errb)
	}
	if strings.Contains(errb, "no_such_pane") {
		t.Fatalf("error %q treated --bogus as a pane id", errb)
	}
}

func TestAttachWithRemoteIsRefused(t *testing.T) {
	code, _, errb := runCLI(t, "/nonexistent.sock", t.TempDir(), "--remote", "ssh://host", "attach")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if errb == "" {
		t.Fatal("attach --remote produced no explanation")
	}
}

// coppice attach with no pane id used to autostart
// through the pane-picker's own dial, even though the README says attach
// never does. COPPICE_NO_AUTOSTART is deliberately left unset here, the
// same discriminating shape attach_wire_test.go's watchResize test uses: if
// this form ever regresses to the autostarting dial, server.log appears in
// DataDir before the spawn itself even runs, and the spawn fails regardless
// since os.Executable() here is this test binary, not a real coppice.
func TestAttachWithNoPaneArgumentDoesNotAutostart(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "server.sock")
	var out, errb bytes.Buffer
	c := onATerminal(&CLI{Version: "test", Socket: sock, DataDir: dir, In: strings.NewReader(""), Out: &out, Err: &errb})
	code := c.Run([]string{"attach"})
	if code != 3 {
		t.Fatalf("exit %d, want 3: %s%s", code, out.String(), errb.String())
	}
	assertNoAutostartArtifacts(t, dir)
}

func TestUnknownServerFlagIsRefusedWithTheKnownFlags(t *testing.T) {
	sock, dir := liveServer(t)
	code, _, errb := runCLI(t, sock, dir, "pane", "list", "--bogus", "x")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errb, "--bogus") {
		t.Fatalf("error %q does not name the unknown flag", errb)
	}
}

func TestPaneCloseConfirmsTheCloseWithoutAListKey(t *testing.T) {
	sock, dir := liveServer(t)
	cwd := t.TempDir()
	runCLI(t, sock, dir, "pane", "create", "--cwd", cwd, "--", "sh", "-c", "sleep 300")
	code, out, errb := runCLI(t, sock, dir, "pane", "close", "w1:p1")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errb)
	}
	if !strings.Contains(out, "closed") {
		t.Fatalf("pane close printed %q, want a sign the close happened", out)
	}
}

// attach.Run reserves the last row for its own status line: Options.Rows
// must be the terminal's FULL height, unchanged, or Run reserves a status
// row from an already-shrunk height and the terminal's real last row is
// never written.
func TestResolveAttachSizePassesTheFullTerminalHeight(t *testing.T) {
	cols, rows := resolveAttachSize(80, 40, true)
	if cols != 80 || rows != 40 {
		t.Fatalf("resolveAttachSize(80, 40, true) = (%d, %d), want (80, 40)", cols, rows)
	}
	cols, rows = resolveAttachSize(0, 0, false)
	if cols != 120 || rows != 40 {
		t.Fatalf("resolveAttachSize with ok=false = (%d, %d), want the (120, 40) default", cols, rows)
	}
}

// attach.ErrServerGone means the connection dropped or was never
// acknowledged: exit 3, unreachable, not exit 1, a user error.
func TestAttachExitsThreeWhenTheServerGoesAway(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "server.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf) // read the pane.attach request
		_ = conn.Close()      // then vanish without ever replying
	}()

	// In must not EOF on its own: an empty reader makes attach.Run's own
	// "stdin closed" path fire first, self-detaching before the vanished
	// server is ever noticed. A pipe with nothing written stays open for the
	// life of the call, the way a real terminal's stdin would.
	inR, inW := io.Pipe()
	defer inW.Close()

	var out, errb bytes.Buffer
	c := onATerminal(&CLI{Version: "test", Socket: sock, DataDir: dir, In: inR, Out: &out, Err: &errb})
	code := c.Run([]string{"attach", "w1:p1"})
	if code != 3 {
		t.Fatalf("attach against a vanished server exited %d, want 3: %s", code, errb.String())
	}
}

// --json must be universal, per the usage text's own promise: "Add --json
// to any command". server status ignored it entirely before this test.
func TestServerStatusWithJSONPrintsOneJSONObject(t *testing.T) {
	sock, dir := liveServer(t)
	code, out, errb := runCLI(t, sock, dir, "server", "status", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errb)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("server status --json did not print one JSON object: %q: %v", out, err)
	}
	if _, ok := res["pid"]; !ok {
		t.Fatalf("--json result has no pid key: %q", out)
	}
}

// An unknown flag on a server subcommand must be refused by name, the same
// as every other group: a typo like --forground must not silently start a
// detached server instead of a foreground one.
func TestServerStartUnknownFlagIsRefused(t *testing.T) {
	code, _, errb := runCLI(t, "/nonexistent.sock", t.TempDir(), "server", "start", "--forground")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errb, "--forground") {
		t.Fatalf("error %q does not name the unknown flag", errb)
	}
}

func TestServerTokenUnknownFlagIsRefused(t *testing.T) {
	code, _, errb := runCLI(t, "/nonexistent.sock", t.TempDir(), "server", "token", "--bogus")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errb, "--bogus") {
		t.Fatalf("error %q does not name the unknown flag", errb)
	}
}

func TestServerTokenWithJSONPrintsOneJSONObject(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "server.sock")
	code, out, errb := runCLI(t, sock, dir, "server", "token", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errb)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("server token --json did not print one JSON object: %q: %v", out, err)
	}
	if _, ok := res["note"]; !ok {
		t.Fatalf("--json result has no note key: %q", out)
	}
}

func TestServerStopWithJSONPrintsStoppedTrue(t *testing.T) {
	sock, dir := liveServer(t)
	code, out, errb := runCLI(t, sock, dir, "server", "stop", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errb)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("server stop --json did not print one JSON object: %q: %v", out, err)
	}
	if res["stopped"] != true {
		t.Fatalf("server stop --json = %v, want stopped true", res)
	}
}

// server start now accepts --json, matching the usage text's own promise
// that --json works on any command: it used to be refused outright.
//
// The idempotent path is tested end to end, against a real listener: it
// needs no spawn. The freshly-started shape is NOT tested end to end here -
// server start's detach path re-execs os.Executable(), which inside a Go
// test binary is the test binary itself, not a real coppice, so no test in
// this process can make that spawn actually come up. That shape is instead
// pinned by testing the exact JSON-building helper serverStart calls, and
// verified live with a built binary - see the task report's hand
// verification section.
func TestServerStartWithJSONReportsAlreadyRunning(t *testing.T) {
	sock, dir := liveServer(t)
	code, out, errb := runCLI(t, sock, dir, "server", "start", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errb)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("server start --json did not print one JSON object: %q: %v", out, err)
	}
	if res["already_running"] != true || res["socket"] != sock {
		t.Fatalf("result = %v, want already_running true and socket %q", res, sock)
	}
}

func TestServerStartedJSONShape(t *testing.T) {
	got := startedJSON("/tmp/x/server.sock", "/tmp/x/server.log")
	var res map[string]any
	if err := json.Unmarshal([]byte(got), &res); err != nil {
		t.Fatalf("startedJSON did not produce one JSON object: %q: %v", got, err)
	}
	if res["socket"] != "/tmp/x/server.sock" || res["log"] != "/tmp/x/server.log" {
		t.Fatalf("startedJSON = %v, want socket and log", res)
	}
}

// server start --foreground --json used to parse clean but print
// the human text anyway - the foreground branch never looked at asJSON at
// all. startedLine is the one function both the detached and the
// foreground branches call for their "now listening" line, so the two
// shapes for the same event can never drift apart again.
func TestStartedLineIsJSONWhenAsJSONIsTrue(t *testing.T) {
	got := startedLine(true, "/tmp/x/server.sock", "/tmp/x/server.log")
	if want := startedJSON("/tmp/x/server.sock", "/tmp/x/server.log"); got != want {
		t.Fatalf("startedLine(true, ...) = %q, want exactly startedJSON's shape %q", got, want)
	}
}

func TestStartedLineIsHumanTextWhenAsJSONIsFalse(t *testing.T) {
	got := startedLine(false, "/tmp/x/server.sock", "/tmp/x/server.log")
	if !strings.Contains(got, "/tmp/x/server.sock") || !strings.Contains(got, "/tmp/x/server.log") {
		t.Fatalf("startedLine(false, ...) = %q, missing the socket or log path", got)
	}
	var probe map[string]any
	if err := json.Unmarshal([]byte(got), &probe); err == nil {
		t.Fatalf("startedLine(false, ...) = %q, parsed as JSON, want plain text", got)
	}
}

func TestServerAlreadyRunningJSONShape(t *testing.T) {
	got := alreadyRunningJSON("/tmp/x/server.sock")
	var res map[string]any
	if err := json.Unmarshal([]byte(got), &res); err != nil {
		t.Fatalf("alreadyRunningJSON did not produce one JSON object: %q: %v", got, err)
	}
	if res["already_running"] != true || res["socket"] != "/tmp/x/server.sock" {
		t.Fatalf("alreadyRunningJSON = %v, want already_running true and socket", res)
	}
}

// TestStartedLineIsJSONWhenAsJSONIsTrue only exercises
// startedLine as a pure function; it does not prove the foreground branch of
// serverStart actually passes asJSON through to it, so a hard-coded human
// line in that branch would leave the whole package green. This runs the
// real CLI entry point, server start
// --foreground --json, against a scratch socket, the same command a person
// would type.
func TestServerStartForegroundJSONPrintsJSONOnStdout(t *testing.T) {
	toolchain.RequireOrSkip(t)
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	dir := t.TempDir()
	sock := filepath.Join(dir, "server.sock")
	var out bytes.Buffer
	c := &CLI{Version: "test", Socket: sock, DataDir: dir, In: strings.NewReader(""), Out: &out, Err: io.Discard}

	done := make(chan int, 1)
	go func() { done <- c.Run([]string{"server", "start", "--foreground", "--json"}) }()
	// Best effort: if an assertion below fails first, this still asks the
	// server to stop, so its goroutine does not outlive the test.
	t.Cleanup(func() {
		var discard bytes.Buffer
		stop := &CLI{Version: "test", Socket: sock, DataDir: dir, In: strings.NewReader(""), Out: &discard, Err: &discard}
		stop.Run([]string{"server", "stop"})
	})

	if _, err := waitForSocket(sock, 5*time.Second); err != nil {
		t.Fatalf("server never started listening: %v", err)
	}
	if code, _, errb := runCLI(t, sock, dir, "server", "stop"); code != 0 {
		t.Fatalf("server stop exit %d: %s", code, errb)
	}

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("foreground server exited %d, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the foreground server did not exit within 5s of server stop")
	}

	line, _, _ := strings.Cut(out.String(), "\n")
	var res map[string]any
	if err := json.Unmarshal([]byte(line), &res); err != nil {
		t.Fatalf("first stdout line %q did not parse as JSON: %v", line, err)
	}
	if _, ok := res["socket"]; !ok {
		t.Fatalf("JSON line %v has no socket key", res)
	}
}

// STE100: no em-dashes in the usage text. This test stays in package cli,
// not internal/boundary with the README's own docs tests: usage is an
// unexported const, and only a test built into this package can read it.
func TestUsageTextHasNoEmDashes(t *testing.T) {
	if strings.Contains(usage, "—") {
		t.Fatal("the usage text contains an em-dash")
	}
}

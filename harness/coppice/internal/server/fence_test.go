package server

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

// fakeFence is a stand-in for `daisugi`: it logs its arguments, then runs
// what follows the first "--".
const fakeFence = `#!/bin/sh
echo "$*" >> "$FAKE_FENCE_LOG"
while [ "$1" != "--" ]; do shift; done
shift
exec "$@"
`

// kernelFences swaps the Landlock probe for the test: a kernel that can
// fence answers ABI 9, one that cannot answers 0.
func kernelFences(t *testing.T, ok bool) {
	t.Helper()
	abi := 0
	if ok {
		abi = fenceMinABI
	}
	kernelABI(t, abi)
}

// kernelABI swaps the Landlock ABI the probe reports.
func kernelABI(t *testing.T, abi int) {
	t.Helper()
	old := landlockABI
	landlockABI = func() int { return abi }
	t.Cleanup(func() { landlockABI = old })
}

func fakeFenceBin(t *testing.T) (log string) {
	t.Helper()
	kernelFences(t, true)
	dir := t.TempDir()
	bin := filepath.Join(dir, "daisugi")
	if err := os.WriteFile(bin, []byte(fakeFence), 0o755); err != nil {
		t.Fatal(err)
	}
	log = filepath.Join(dir, "log")
	t.Setenv("COPPICE_DAISUGI_BIN", bin)
	t.Setenv("FAKE_FENCE_LOG", log)
	return log
}

func armWarden(t *testing.T, s *Server) {
	t.Helper()
	kernelFences(t, true)
	if err := os.MkdirAll(filepath.Join(s.cfg.WardenRoot, "envelopes"), 0o700); err != nil {
		t.Fatal(err)
	}
}

// A pane that runs Hermes is fenced with Hermes's own table, like the
// other harnesses the fence knows.
func TestAHermesPaneGetsTheHermesFenceTable(t *testing.T) {
	fakeFenceBin(t)
	s := newTestServer(t)
	armWarden(t, s)
	w, err := s.fenceWrap(layout.Pane{Harness: "hermes", Cwd: "/work"}, s.fenceTmp("w1:p1"))
	if err != nil || !hasPair(w, "--harness", "hermes") {
		t.Fatalf("wrap = %v, err %v", w, err)
	}
}

// A pane opened in the home folder is refused by the fence. The refusal
// says what to do, in one line, instead of a bare spawn failure.
func TestAFencedPaneInTheHomeFolderIsToldWhatToDo(t *testing.T) {
	fakeFenceBin(t)
	s := newTestServer(t)
	armWarden(t, s)
	home, _ := os.UserHomeDir()
	_, err := s.fenceWrap(layout.Pane{Harness: "claude", Cwd: home}, s.fenceTmp("w1:p1"))
	if err == nil || !strings.Contains(err.Error(), "project folder") || !strings.Contains(err.Error(), `fence = "off"`) || strings.Contains(err.Error(), "\n") {
		t.Errorf("cwd %s: %v", home, err)
	}
	if _, err := s.fenceWrap(layout.Pane{Harness: "claude", Cwd: filepath.Join(home, "code", "x")}, s.fenceTmp("w1:p1")); err != nil {
		t.Errorf("a project folder was refused: %v", err)
	}
}

// A fenced steward reaches its tools on the standing socket in the steward
// log dir; the fence plan must grant it. Other panes do not get it.
func TestAFencedStewardIsGrantedItsToolsSocket(t *testing.T) {
	fakeFenceBin(t)
	s := newTestServer(t)
	armWarden(t, s)
	want := s.standingSockPath()
	w, err := s.fenceWrap(layout.Pane{Harness: "claude", Cwd: "/work", Label: "steward"}, s.fenceTmp("w1:p1"))
	if err != nil || !hasPair(w, "--coppice-socket", want) || !hasPair(w, "--coppice-socket", s.cfg.SocketPath) {
		t.Fatalf("steward wrap = %v, err %v (want socket %s)", w, err, want)
	}
	w, _ = s.fenceWrap(layout.Pane{Harness: "claude", Cwd: "/work", Label: "worker"}, s.fenceTmp("w1:p2"))
	if hasPair(w, "--coppice-socket", want) {
		t.Errorf("a worker got the steward's tools socket: %v", w)
	}
	c, err := s.fenceCmdSockets("claude", "/work", nil, false, []string{want})
	if err != nil || !hasPair(c, "--coppice-socket", want) {
		t.Errorf("a chat-mode steward turn lacks the socket: %v %v", c, err)
	}
}

func TestFenceFollowsTheWardenUnderAuto(t *testing.T) {
	fakeFenceBin(t)
	s := newTestServer(t)
	rec := layout.Pane{Harness: "claude", Cwd: "/work"}
	if w, err := s.fenceWrap(rec, s.fenceTmp("w1:p1")); err != nil || w != nil {
		t.Fatalf("no warden: wrap %v err %v", w, err)
	}
	armWarden(t, s)
	w, err := s.fenceWrap(rec, s.fenceTmp("w1:p1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(w) < 4 || w[1] != "fence" || w[len(w)-1] != "--" {
		t.Fatalf("wrap = %v", w)
	}
	for _, want := range [][]string{{"--harness", "claude"}, {"--workspace", "/work"}, {"--warden-root", s.cfg.WardenRoot}} {
		found := false
		for i := 0; i+1 < len(w); i++ {
			if w[i] == want[0] && w[i+1] == want[1] {
				found = true
			}
		}
		if !found {
			t.Errorf("wrap lacks %v: %v", want, w)
		}
	}
	// Disarmed: the warden is off, so no fence.
	if err := os.WriteFile(filepath.Join(s.cfg.WardenRoot, "DISARMED"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if w, _ := s.fenceWrap(rec, s.fenceTmp("w1:p1")); w != nil {
		t.Fatalf("disarmed warden still fenced: %v", w)
	}
}

func TestFenceSettingsAndTheShellPane(t *testing.T) {
	fakeFenceBin(t)
	s := newTestServer(t)
	armWarden(t, s)
	cases := []struct {
		cfg, pane, harness string
		want               bool
	}{
		{"auto", "", "claude", true},
		{"", "", "claude", true},
		{"off", "", "claude", false},
		{"auto", "off", "claude", false},
		{"off", "on", "claude", true},
		{"on", "", "claude", true},
		{"auto", "", "", false},
		{"on", "", "shell", false},
		{"auto", "on", "shell", true},
		{"off", "on", "", true},
	}
	for _, c := range cases {
		s.cfg.Fence = c.cfg
		w, err := s.fenceWrap(layout.Pane{Harness: c.harness, Fence: c.pane, Cwd: "/w"}, s.fenceTmp("w1:p1"))
		if err != nil || (w != nil) != c.want {
			t.Errorf("%+v: wrap %v err %v", c, w, err)
		}
	}
	// With the warden off, "on" still fences and auto does not.
	if err := os.RemoveAll(filepath.Join(s.cfg.WardenRoot, "envelopes")); err != nil {
		t.Fatal(err)
	}
	s.cfg.Fence = "auto"
	if w, _ := s.fenceWrap(layout.Pane{Harness: "claude"}, s.fenceTmp("w1:p1")); w != nil {
		t.Errorf("warden off, auto: %v", w)
	}
	if w, _ := s.fenceWrap(layout.Pane{Harness: "claude", Fence: "on"}, s.fenceTmp("w1:p1")); w == nil {
		t.Errorf("warden off, pane on: no wrap")
	}
}

func TestFenceWithoutDaisugiRefusesInsteadOfRunningBare(t *testing.T) {
	t.Setenv("COPPICE_DAISUGI_BIN", filepath.Join(t.TempDir(), "no-such-daisugi"))
	s := newTestServer(t)
	w, err := s.fenceWrap(layout.Pane{Harness: "claude", Fence: "on"}, s.fenceTmp("w1:p1"))
	if err == nil || w != nil || !strings.Contains(err.Error(), `fence = "off"`) {
		t.Fatalf("wrap %v err %v", w, err)
	}
}

func TestFenceKeepsSprigSessionDirReachable(t *testing.T) {
	fakeFenceBin(t)
	s := newTestServer(t)
	w, err := s.fenceWrap(layout.Pane{Harness: "sprig", Fence: "on", Argv: []string{"--session-dir", "/data/sess"}}, s.fenceTmp("w1:p1"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(w[len(w)-3:], []string{"--workdir", "/data/sess", "--"}) {
		t.Fatalf("wrap = %v", w)
	}
}

func TestAPaneStartsUnderTheFenceWithoutChangingItsSavedCommand(t *testing.T) {
	toolchain.RequireOrSkip(t)
	log := fakeFenceBin(t)
	saveFakeHarness(t)
	s := newTestServer(t)
	armWarden(t, s)
	res := createByHarness(t, s, "")
	id, _ := res["pane"].(string)
	if id == "" {
		t.Fatalf("no pane: %v", res)
	}
	t.Cleanup(func() { closePane(t, s, id) })
	var raw []byte
	for i := 0; i < 100 && len(raw) == 0; i++ {
		raw, _ = os.ReadFile(log)
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.HasPrefix(string(raw), "fence ") || !strings.Contains(string(raw), "-- sh -c sleep 30") {
		t.Fatalf("the fake fence saw %q", raw)
	}
	row := rowFor(t, listPanes(t, s), id)
	if !reflect.DeepEqual(row["cmd"], []any{"sh", "-c", "sleep 30"}) {
		t.Fatalf("the saved command changed: %v", row["cmd"])
	}
	// A pane that says fence off starts bare.
	if err := os.Remove(log); err != nil {
		t.Fatal(err)
	}
	res2 := createByHarness(t, s, `,"fence":"off"`)
	id2, _ := res2["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id2) })
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(log); err == nil {
		t.Fatal("a pane with fence off still ran the fence")
	}
}

func TestPaneCreateRefusesAnUnknownFenceWord(t *testing.T) {
	saveFakeHarness(t)
	s := newTestServer(t)
	res := createByHarness(t, s, `,"fence":"maybe"`)
	if res["error"] == nil {
		t.Fatalf("accepted: %v", res)
	}
}

// hasPair says whether w holds a then b next to each other.
func hasPair(w []string, a, b string) bool {
	for i := 0; i+1 < len(w); i++ {
		if w[i] == a && w[i+1] == b {
			return true
		}
	}
	return false
}

// FN-V-2, FN-V-9: a fenced pane connects to this floor's socket and has a
// private temp dir under the data dir, which goes when the pane ends.
func TestFN_V2_V9_TheWrapNamesTheSocketAndAPrivateTmp(t *testing.T) {
	fakeFenceBin(t)
	s := newTestServer(t)
	w, err := s.fenceWrap(layout.Pane{ID: "w1:p1", Harness: "claude", Fence: "on", Cwd: "/w"}, s.fenceTmp("w1:p1"))
	if err != nil {
		t.Fatal(err)
	}
	if !hasPair(w, "--coppice-socket", s.cfg.SocketPath) {
		t.Errorf("no socket grant: %v", w)
	}
	tmp := ""
	for i, a := range w {
		if a == "--tmpdir" && i+1 < len(w) {
			tmp = w[i+1]
		}
	}
	if !strings.HasPrefix(tmp, filepath.Join(s.cfg.DataDir, "fence-tmp", "w1_p1-")) {
		t.Errorf("no private tmp: %v", w)
	}
	// N-5: each run gets a fresh leaf.
	if s.fenceTmp("w1:p1") == s.fenceTmp("w1:p1") {
		t.Error("two runs share a temp leaf")
	}
}

// FN-V-5: only a missing envelopes dir turns the warden off; a root made
// unreadable still counts as on. The steward's workers and turns run
// under the same fence.
func TestFN_V5_AnUnreadableWardenRootStillFencesAndWorkersAreWrapped(t *testing.T) {
	fakeFenceBin(t)
	s := newTestServer(t)
	armWarden(t, s)
	if err := os.Chmod(s.cfg.WardenRoot, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(s.cfg.WardenRoot, 0o700) })
	if !s.wardenOn() {
		t.Errorf("a chmod 000 warden root reads as off")
	}
	if err := os.Chmod(s.cfg.WardenRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	w, err := s.fenceCmd("", "/repo/wt", []string{"/data/work"}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range [][2]string{{"--workspace", "/repo/wt"}, {"--allow", "/data/work:rw"}, {"--keep-fd", "3"},
		{"--coppice-socket", s.cfg.SocketPath}} {
		if !hasPair(w, p[0], p[1]) {
			t.Errorf("worker wrap lacks %v: %v", p, w)
		}
	}
	if err := os.RemoveAll(filepath.Join(s.cfg.WardenRoot, "envelopes")); err != nil {
		t.Fatal(err)
	}
	if w, _ := s.fenceCmd("", "/repo/wt", nil, false); w != nil {
		t.Errorf("warden off, worker wrapped: %v", w)
	}
}

// FN-V-6: a fenced claude pane starts with the pinned --settings document.
func TestFN_V6_AFencedClaudePaneGetsThePinnedSettings(t *testing.T) {
	s := newTestServer(t)
	armWarden(t, s)
	wrap := []string{"/opt/bin/daisugi", "fence", "--"}
	got := s.fencedClaudeArgv(layout.Pane{Harness: "claude"}, wrap, []string{"claude", "--model", "x"}, true, "")
	want := `{"disableAllHooks":false,"hooks":{"ConfigChange":[{"hooks":[{"command":"exit 2","type":"command"}]}],` +
		`"PreToolUse":[{"hooks":[{"command":"DAISUGI_WARDEN_HOOK=opendaisugi.warden '/opt/bin/daisugi' warden check --root '` +
		s.cfg.WardenRoot + `' --format claude --verify-timeout 10 --ask --ask-timeout 90 || exit 2",` +
		`"onFailure":"block","timeout":105,"type":"command"}],"matcher":"*"}]}}`
	if !reflect.DeepEqual(got, []string{"claude", "--settings", want, "--model", "x"}) {
		t.Fatalf("argv:\n got %q\nwant %q", got, []string{"claude", "--settings", want, "--model", "x"})
	}
	if got := s.fencedClaudeArgv(layout.Pane{Harness: "claude"}, wrap, []string{"-p"}, false, ""); got[0] != "--settings" || got[2] != "-p" {
		t.Errorf("adapter argv: %q", got)
	}
	if got := s.fencedClaudeArgv(layout.Pane{Harness: "claude"}, nil, []string{"claude"}, true, ""); len(got) != 1 {
		t.Errorf("an unfenced pane got settings: %q", got)
	}
}

// FN-V-1: a fenced pane cannot get an unfenced process out of coppice: no
// fence off, what it starts is fenced (a shell too), its env cannot move
// HOME, and it types only into fenced panes.
func TestFN_V1_AFencedPaneGetsOnlyFencedPanes(t *testing.T) {
	toolchain.RequireOrSkip(t)
	log := fakeFenceBin(t)
	saveFakeHarness(t)
	s := newTestServer(t)
	armWarden(t, s)
	parent, _ := createByHarness(t, s, "")["pane"].(string)
	bare, _ := createByHarness(t, s, `,"fence":"off"`)["pane"].(string)
	if parent == "" || bare == "" {
		t.Fatal("no panes")
	}
	t.Cleanup(func() { closePane(t, s, parent); closePane(t, s, bare) })
	if !s.paneFenced(parent) || s.paneFenced(bare) {
		t.Fatalf("fenced: parent %v bare %v", s.paneFenced(parent), s.paneFenced(bare))
	}
	send, next, stop := stream(t, s)
	defer stop()
	send(`{"id":"0","cmd":"hello","role":"pane","pane":"` + parent + `"}`)
	if r := next(); r["ok"] != true {
		t.Fatalf("hello: %v", r)
	}
	send(`{"id":"1","cmd":"pane.create","harness":"fakeh","cwd":"/","fence":"off"}`)
	if r := next(); r["ok"] != false || !strings.Contains(fmt.Sprint(r["error"]), FenceRefusal) {
		t.Errorf("fence off from a pane: %v", r)
	}
	_ = os.Remove(log)
	send(`{"id":"2","cmd":"pane.create","cmd_argv":["sh","-c","sleep 30"],"cwd":"/","env":{"HOME":"/elsewhere","DAISUGI_FENCE":"off"}}`)
	r := next()
	child, _ := r["result"].(map[string]any)["pane"].(string)
	if child == "" {
		t.Fatalf("shell from a pane: %v", r)
	}
	t.Cleanup(func() { closePane(t, s, child) })
	if !s.paneFenced(child) {
		t.Errorf("a fenced pane's shell child runs bare")
	}
	if rec, _ := s.tree.Pane(child); rec.Fence != "on" {
		t.Errorf("child record fence %q", rec.Fence)
	}
	send(`{"id":"3","cmd":"pane.send_text","pane":"` + bare + `","text":"x"}`)
	if r := next(); r["ok"] != false || !strings.Contains(fmt.Sprint(r["error"]), TypeFenceRefusal) {
		t.Errorf("typing into a bare pane: %v", r)
	}
	send(`{"id":"4","cmd":"pane.send_text","pane":"` + child + `","text":"x"}`)
	if r := next(); r["ok"] != true {
		t.Errorf("typing into a fenced pane: %v", r)
	}
	env := pinFencedEnv(map[string]string{"HOME": "/elsewhere", "DAISUGI_FENCE": "off", "XDG_DATA_HOME": "/x", "K": "v"})
	if !reflect.DeepEqual(env, map[string]string{"K": "v"}) {
		t.Errorf("pinned env: %v", env)
	}
}

// FN-V-6: a change to a git hook during a fenced run shows on the ended
// pane's row, and the private temp dir goes with the pane.
func TestFN_V6_AWorkspaceHookChangeShowsOnTheRow(t *testing.T) {
	toolchain.RequireOrSkip(t)
	fakeFenceBin(t)
	saveFakeHarness(t)
	s := newTestServer(t)
	armWarden(t, s)
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".git", "hooks"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A pane that ends on its own keeps its record in Recent.
	// The pane ends only once the test has changed the hook, so the end's
	// hash always sees the change.
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.create","cmd_argv":["sh","-c","while [ ! -e go ]; do sleep 0.05; done"],"fence":"on","cwd":"`+ws+`"}`)
	id, _ := result(t, got[0])["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	s.liveMu.RLock()
	tmp := s.live[id].fenceTmp
	s.liveMu.RUnlock()
	if tmp == "" {
		t.Fatal("the fenced pane has no temp leaf")
	}
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".git", "hooks", "pre-commit"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	for i := 0; i < 200; i++ {
		for _, r := range listEndedPanes(t, s) {
			if r["id"] == id && r["workspace_changed"] != nil {
				row = r
			}
		}
		if row != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if row == nil || !reflect.DeepEqual(row["workspace_changed"], []any{".git/hooks/pre-commit"}) {
		t.Fatalf("ended row: %v", row)
	}
	if _, err := os.Stat(tmp); err == nil {
		t.Errorf("the private temp dir stayed")
	}
	// N-3: the next start in that workspace waits for the operator's ack.
	got = roundTrip(t, s, `{"id":"2","cmd":"pane.create","cmd_argv":["sh","-c","sleep 1"],"cwd":"`+ws+`"}`)
	if got[0].OK || !strings.Contains(fmt.Sprint(got[0].Error), "workspace config changed") {
		t.Fatalf("a start in a changed workspace: %v", got[0])
	}
	// A pane cannot ack it.
	got = roundTrip(t, s, `{"id":"0","cmd":"hello","role":"pane","pane":"w1:p1"}`,
		`{"id":"3","cmd":"pane.ack_changed","cwd":"`+ws+`"}`)
	if got[1].OK || !strings.Contains(fmt.Sprint(got[1].Error), AckRefusal) {
		t.Fatalf("a pane acked: %v", got[1])
	}
	// The hold outlives the server: it is on disk.
	if len(s.workspaceHold(ws)) == 0 {
		t.Fatal("no hold on disk")
	}
	got = roundTrip(t, s, `{"id":"4","cmd":"pane.ack_changed","cwd":"`+ws+`"}`)
	if !got[0].OK || !reflect.DeepEqual(result(t, got[0])["acked"], []any{".git/hooks/pre-commit"}) {
		t.Fatalf("ack: %v", got[0])
	}
	got = roundTrip(t, s, `{"id":"5","cmd":"pane.create","cmd_argv":["sh","-c","sleep 1"],"cwd":"`+ws+`"}`)
	if !got[0].OK {
		t.Fatalf("a start after the ack: %v", got[0])
	}
	id2, _ := result(t, got[0])["pane"].(string)
	closePane(t, s, id2)
}

// N-3: the watch covers the files that run code later, and follows a
// gitfile to its git dir and that dir's common dir.
func TestN3_TheWorkspaceWatchCoversWhatRunsLater(t *testing.T) {
	d := t.TempDir()
	ws, common := filepath.Join(d, "ws"), filepath.Join(d, "main.git")
	wt := filepath.Join(common, "worktrees", "ws")
	for _, p := range []string{ws, filepath.Join(wt, "hooks"), filepath.Join(common, "hooks"),
		filepath.Join(ws, ".claude", "commands"), filepath.Join(ws, ".codex"), filepath.Join(ws, ".opencode", "plugin")} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(ws, ".git"):                        "gitdir: " + wt + "\n",
		filepath.Join(wt, "commondir"):                   "../..\n",
		filepath.Join(ws, ".mcp.json"):                   "{}",
		filepath.Join(ws, ".envrc"):                      "x",
		filepath.Join(ws, ".gitmodules"):                 "x",
		filepath.Join(ws, "opencode.json"):               "{}",
		filepath.Join(ws, ".claude", "commands", "c.md"): "x",
		filepath.Join(ws, ".codex", "config.toml"):       "x",
		filepath.Join(ws, ".opencode", "plugin", "p.ts"): "x",
		filepath.Join(wt, "config"):                      "x",
		filepath.Join(common, "config"):                  "x",
		filepath.Join(common, "hooks", "pre-commit"):     "x",
	}
	for p, b := range files {
		if err := os.WriteFile(p, []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before := workspaceWatched(ws)
	for _, k := range []string{".git", ".mcp.json", ".envrc", ".gitmodules", "opencode.json", ".claude/commands/c.md",
		".codex/config.toml", ".opencode/plugin/p.ts", filepath.Join(wt, "config"), filepath.Join(common, "config"),
		filepath.Join(common, "hooks", "pre-commit")} {
		if before[k] == "" {
			t.Errorf("%s is not watched: %v", k, before)
		}
	}
	// A hook added in the common dir is a change.
	if err := os.WriteFile(filepath.Join(common, "hooks", "post-checkout"), []byte("x"), 0o700); err != nil {
		t.Fatal(err)
	}
	ch := workspaceChanges(before, workspaceWatched(ws))
	if !reflect.DeepEqual(ch, []string{filepath.Join(common, "hooks", "post-checkout")}) {
		t.Errorf("changes: %v", ch)
	}
}

// A home folder holds Claude Code's global state dir. What the harness
// writes while it runs (projects, todos, tool results) is no config change;
// settings, hooks, commands and the like still are.
func TestTheWatchSkipsHarnessStateButKeepsHarnessConfig(t *testing.T) {
	ws := t.TempDir()
	for _, p := range []string{".claude/projects/x", ".claude/todos", ".claude/hooks", ".codex/sessions", ".codex/rules", ".opencode/plugin"} {
		if err := os.MkdirAll(filepath.Join(ws, p), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	w := func(p, b string) {
		if err := os.WriteFile(filepath.Join(ws, p), []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w(".claude/settings.json", "{}")
	w(".claude/hooks/h.sh", "a")
	w(".claude/projects/x/y.txt", "a")
	w(".claude/todos/t.json", "a")
	w(".codex/sessions/s.jsonl", "a")
	w(".codex/rules/r.rules", "a")
	w(".opencode/package.json", "{}")
	before := workspaceWatched(ws)
	w(".claude/projects/x/y.txt", "changed")
	w(".claude/todos/new.json", "new")
	w(".codex/sessions/s.jsonl", "changed")
	if ch := workspaceChanges(before, workspaceWatched(ws)); len(ch) != 0 {
		t.Errorf("state files counted as config changes: %v", ch)
	}
	w(".claude/settings.json", `{"hooks":{}}`)
	w(".claude/hooks/h.sh", "b")
	w(".codex/rules/r.rules", "b")
	w(".opencode/package.json", `{"dependencies":{}}`)
	if ch := workspaceChanges(before, workspaceWatched(ws)); !reflect.DeepEqual(ch, []string{".claude/hooks/h.sh", ".claude/settings.json", ".codex/rules/r.rules", ".opencode/package.json"}) {
		t.Errorf("config changes: %v", ch)
	}
}

// N-4: a worker's sprig starts under a fence whose warden root is the
// worker's own (so the fence keeps it read only and starts its resident
// warden) and that grants no write over it; the tree run itself runs bare.
func TestN4_AWorkersSprigFenceGrantsNoWriteOverItsWardenRoot(t *testing.T) {
	fakeFenceBin(t)
	s := newTestServer(t)
	armWarden(t, s)
	root := "/data/steward/work/warden"
	w, err := s.sprigFence("/repo", root)
	if err != nil || w == nil {
		t.Fatalf("%v %v", w, err)
	}
	if w[len(w)-1] == "--" {
		t.Errorf("the prefix ends in --: %v", w)
	}
	roots := 0
	for i, a := range w {
		if a == "--warden-root" {
			roots++
			if w[i+1] != root {
				t.Errorf("warden root %s", w[i+1])
			}
		}
		if a == "--allow" || a == "--workdir" {
			t.Errorf("a write grant in the worker's fence: %v", w)
		}
	}
	if roots != 1 || !hasPair(w, "--workspace", "/repo") {
		t.Errorf("prefix %v", w)
	}
	if err := os.RemoveAll(filepath.Join(s.cfg.WardenRoot, "envelopes")); err != nil {
		t.Fatal(err)
	}
	if w, _ := s.sprigFence("/repo", root); w != nil {
		t.Errorf("warden off, sprig fenced: %v", w)
	}
}

// R2-4: a hold covers the workspace and every dir under it, it is set as
// soon as a start finds the change (the fenced pane still runs), and the
// refusal names the held workspace, which is what the operator acks.
func TestR2_4_AHoldCoversSubdirsAndIsSetWhileTheFencedPaneRuns(t *testing.T) {
	toolchain.RequireOrSkip(t)
	fakeFenceBin(t)
	saveFakeHarness(t)
	s := newTestServer(t)
	armWarden(t, s)
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	for _, d := range []string{filepath.Join(ws, ".git", "hooks"), filepath.Join(ws, "sub"), ws + "x"} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	key := wsKey(ws)
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.create","cmd_argv":["sh","-c","while [ ! -e go ]; do sleep 0.05; done"],"fence":"on","cwd":"`+ws+`"}`)
	id, _ := result(t, got[0])["pane"].(string)
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(ws, "go"), nil, 0o600)
		closePane(t, s, id)
	})
	if err := os.WriteFile(filepath.Join(ws, ".git", "hooks", "pre-commit"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// The fenced pane still runs: an unfenced start in a subdir waits.
	got = roundTrip(t, s, `{"id":"2","cmd":"pane.create","cmd_argv":["sh","-c","sleep 1"],"fence":"off","cwd":"`+filepath.Join(ws, "sub")+`"}`)
	if got[0].OK || !strings.Contains(fmt.Sprint(got[0].Error), "workspace config changed") ||
		!strings.Contains(fmt.Sprint(got[0].Error), "ack-changed "+key) {
		t.Fatalf("a start under a changed workspace: %v", got[0])
	}
	// A sibling whose name only starts the same is not under it.
	got = roundTrip(t, s, `{"id":"3","cmd":"pane.create","cmd_argv":["sh","-c","sleep 1"],"fence":"off","cwd":"`+ws+`x"}`)
	if !got[0].OK {
		t.Fatalf("a start in a sibling: %v", got[0])
	}
	sib, _ := result(t, got[0])["pane"].(string)
	closePane(t, s, sib)
	got = roundTrip(t, s, `{"id":"4","cmd":"pane.ack_changed","cwd":"`+ws+`"}`)
	if !got[0].OK {
		t.Fatalf("ack: %v", got[0])
	}
	got = roundTrip(t, s, `{"id":"5","cmd":"pane.create","cmd_argv":["sh","-c","sleep 1"],"fence":"off","cwd":"`+filepath.Join(ws, "sub")+`"}`)
	if !got[0].OK {
		t.Fatalf("a start after the ack: %v", got[0])
	}
	id2, _ := result(t, got[0])["pane"].(string)
	closePane(t, s, id2)
}

// ADR-0028: under auto a kernel without Landlock runs agent work bare, and
// the floor says files are not confined. "on" stays fail closed.
func TestAutoRunsBareOnAKernelWithoutLandlock(t *testing.T) {
	fakeFenceBin(t)
	s := newTestServer(t)
	armWarden(t, s)
	kernelFences(t, false)
	for _, mode := range []string{"", "auto"} {
		s.cfg.Fence = mode
		if w, err := s.fenceWrap(layout.Pane{Harness: "claude", Cwd: "/w"}, s.fenceTmp("w1:p1")); err != nil || w != nil {
			t.Errorf("mode %q: wrap %v err %v, want bare", mode, w, err)
		}
		if w, err := s.fenceCmd("", "/w", nil, false); err != nil || w != nil {
			t.Errorf("mode %q: worker fence %v err %v, want bare", mode, w, err)
		}
		if !s.Unconfined() {
			t.Errorf("mode %q: bare run not reported", mode)
		}
	}
	s.cfg.Fence = "on"
	if w, _ := s.fenceWrap(layout.Pane{Harness: "claude", Cwd: "/w"}, s.fenceTmp("w1:p1")); w == nil {
		t.Error("on: no wrap")
	}
	if s.Unconfined() {
		t.Error("on is never unconfined")
	}
}

// The notice shows only when the warden is on and the config turns the fence off.
func TestUnconfinedNamesTheOffConfigOnlyWithTheWardenOn(t *testing.T) {
	fakeFenceBin(t)
	s := newTestServer(t)
	s.cfg.Fence = "off"
	if s.Unconfined() {
		t.Error("warden off: nothing is checked, nothing to say")
	}
	armWarden(t, s)
	if !s.Unconfined() {
		t.Error("warden on, fence off: not reported")
	}
	s.cfg.Fence = "auto"
	if s.Unconfined() {
		t.Error("auto with Landlock: reported")
	}
}

// Plan task 4: the floor's pane.list carries the one-line notice while the
// warden is on and the config turns the fence off, and not otherwise.
func TestPaneListCarriesTheUnconfinedLine(t *testing.T) {
	fakeFenceBin(t)
	s := newAgentServer(t)
	armWarden(t, s)
	s.cfg.Fence = "off"
	got := roundTrip(t, s, `{"id":"3","cmd":"pane.list"}`)
	if m := result(t, got[0]); m["unconfined"] != UnconfinedLine {
		t.Fatalf("unconfined = %v", m["unconfined"])
	}
	s.cfg.Fence = "auto"
	got = roundTrip(t, s, `{"id":"3","cmd":"pane.list"}`)
	if m := result(t, got[0]); m["unconfined"] != nil {
		t.Fatalf("auto with Landlock still says %v", m["unconfined"])
	}
}

// Review F2: `daisugi fence` needs Landlock ABI 9 (it refuses a lower one
// unless asked to allow weak IPC, which coppice never does). Under auto a
// kernel below 9 runs agent work bare and the floor says why. "on" stays
// fail closed.
func TestAutoFencesOnlyAtLandlockABI9(t *testing.T) {
	fakeFenceBin(t)
	for _, tc := range []struct {
		abi   int
		fence bool
	}{{0, false}, {3, false}, {8, false}, {9, true}, {10, true}} {
		s := newTestServer(t)
		armWarden(t, s)
		kernelABI(t, tc.abi) // after armWarden, which sets a kernel that fences
		s.cfg.Fence = "auto"
		w, err := s.fenceWrap(layout.Pane{Harness: "claude", Cwd: "/w"}, s.fenceTmp("w1:p1"))
		if err != nil || (w != nil) != tc.fence {
			t.Errorf("ABI %d: wrap %v err %v, want fenced=%v", tc.abi, w, err, tc.fence)
		}
		if s.Unconfined() == tc.fence {
			t.Errorf("ABI %d: Unconfined() = %v", tc.abi, s.Unconfined())
		}
		line := s.UnconfinedNotice()
		if tc.fence && line != "" {
			t.Errorf("ABI %d: notice %q", tc.abi, line)
		}
		if !tc.fence {
			if !strings.Contains(line, "cannot fence") || !strings.Contains(line, "needs") ||
				strings.Contains(line, "Turn the fence on") {
				t.Errorf("ABI %d: notice %q", tc.abi, line)
			}
		}
		s.cfg.Fence = "on"
		if w, _ := s.fenceWrap(layout.Pane{Harness: "claude", Cwd: "/w"}, s.fenceTmp("w1:p1")); w == nil {
			t.Errorf("ABI %d: on must fence (fail closed)", tc.abi)
		}
	}
}

// The notice for "off" tells the operator to turn the fence on; the notice
// for "auto on a kernel that cannot fence" names the kernel and does not.
func TestTheNoFenceKernelNoticeNamesTheABI(t *testing.T) {
	fakeFenceBin(t)
	s := newTestServer(t)
	armWarden(t, s)
	kernelABI(t, 3)
	s.cfg.Fence = "auto"
	want := "Files are not confined: the warden checks commands, but only a fence stops a file access. " +
		"This kernel cannot fence (Landlock ABI 3, the fence needs 9)."
	if got := s.UnconfinedNotice(); got != want {
		t.Fatalf("notice %q", got)
	}
	kernelABI(t, 0)
	want = "Files are not confined: the warden checks commands, but only a fence stops a file access. " +
		"This kernel cannot fence (no Landlock, the fence needs ABI 9)."
	if got := s.UnconfinedNotice(); got != want {
		t.Fatalf("notice %q", got)
	}
	s.cfg.Fence = "off"
	if got := s.UnconfinedNotice(); got != UnconfinedLine {
		t.Fatalf("off notice %q", got)
	}
}

// Coppice never adds --allow-weak-ipc: weak IPC would let a child reach
// D-Bus or the user systemd outside the fence.
func TestCoppiceNeverAllowsWeakIPC(t *testing.T) {
	log := fakeFenceBin(t)
	s := newTestServer(t)
	armWarden(t, s)
	s.cfg.Fence = "auto"
	w, err := s.fenceCmd("", "/w", nil, false)
	if err != nil || w == nil {
		t.Fatalf("wrap %v err %v", w, err)
	}
	for _, a := range w {
		if strings.Contains(a, "weak-ipc") {
			t.Fatalf("argv has %q", a)
		}
	}
	_ = log
}

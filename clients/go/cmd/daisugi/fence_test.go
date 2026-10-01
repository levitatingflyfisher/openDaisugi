package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var (
	fenceBinOnce sync.Once
	fenceBinPath string
	fenceBinErr  string
)

func fenceBin(t *testing.T) string {
	t.Helper()
	fenceBinOnce.Do(func() {
		// FENCE_TEST_BIN runs these tests against another build (the
		// red half of red then green).
		if b := os.Getenv("FENCE_TEST_BIN"); b != "" {
			fenceBinPath = b
			return
		}
		dir, err := os.MkdirTemp("", "fence-bin-")
		if err != nil {
			fenceBinErr = err.Error()
			return
		}
		fenceBinPath = filepath.Join(dir, "daisugi")
		if out, err := exec.Command("go", "build", "-tags", "netgo", "-o", fenceBinPath, ".").CombinedOutput(); err != nil {
			fenceBinErr = string(out)
		}
	})
	if fenceBinErr != "" {
		t.Fatalf("build: %s", fenceBinErr)
	}
	return fenceBinPath
}

// world is a fake HOME with a key, a gate root, a workspace, a temp dir and
// a directory outside, side by side: none is inside another.
type world struct {
	base, home, ws, tmp, outside string
}

func newWorld(t *testing.T) world {
	t.Helper()
	// A short base: <root>/gate.sock must fit a unix socket path (108
	// bytes), and t.TempDir's name carries the test's.
	tmp, err := os.MkdirTemp("", "fw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	base, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatal(err)
	}
	w := world{base, filepath.Join(base, "home"), filepath.Join(base, "ws"), filepath.Join(base, "tmp"), filepath.Join(base, "outside")}
	for _, d := range []string{
		w.home + "/.ssh", w.home + "/.opendaisugi/gate/envelopes", w.home + "/.claude", w.home + "/.local/bin",
		w.home + "/other", w.ws, w.tmp, w.outside,
	} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for p, body := range map[string]string{
		w.home + "/.ssh/key":                                 "SECRET",
		w.home + "/other/file":                               "OTHER",
		w.home + "/.opendaisugi/gate/envelopes/default.json": `{"permissions":{"file_read":[],"file_write":[]}}`,
		w.outside + "/victim":                                "VICTIM",
		w.ws + "/ok":                                         "OK",
	} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return w
}

func (w world) env(extra ...string) []string {
	return append([]string{"HOME=" + w.home, "TMPDIR=" + w.tmp, "PATH=/usr/bin:/bin", "LANG=C"}, extra...)
}

var (
	startedGate     = regexp.MustCompile(`started the resident gate on \S+ \(pid (\d+)\)`)
	startedGateLine = regexp.MustCompile(`daisugi fence: started the resident gate on \S+ \(pid \d+\)\n`)
)

// killStarted ends, by pid, each resident gate the fence said it started.
func killStarted(t *testing.T, stderr string) {
	for _, m := range startedGate.FindAllStringSubmatch(stderr, -1) {
		if pid, err := strconv.Atoi(m[1]); err == nil && pid > 1 {
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGTERM) })
		}
	}
}

func runFence(t *testing.T, w world, env []string, args ...string) (string, string, int) {
	t.Helper()
	return runFenceCmd(t, w, env, nil, args...)
}

func runFenceCmd(t *testing.T, w world, env []string, extra []*os.File, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(fenceBin(t), append([]string{"fence"}, args...)...)
	cmd.Dir = w.ws
	cmd.Env = env
	cmd.ExtraFiles = extra
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	code := 0
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		code = ee.ExitCode()
	}
	killStarted(t, se.String())
	return so.String(), se.String(), code
}

// probes is the script run inside the fence. Each line is "name ok" for a
// call that worked and "name denied" for one that failed. The nested sh
// is a child of the fenced shell: it must be fenced too.
func probes(w world) string {
	h, o := w.home, w.outside
	return strings.Join([]string{
		`t() { n=$1; shift; if "$@" >/dev/null 2>&1; then echo "$n ok"; else echo "$n denied"; fi; }`,
		`t read_ws cat ` + w.ws + `/ok`,
		`t write_ws sh -c 'echo x > ` + w.ws + `/new'`,
		`t mkdir_ws mkdir ` + w.ws + `/sub`,
		`t write_tmp sh -c 'echo x > "$TMPDIR/f"'`,
		`t write_shared_tmp sh -c 'echo x > ` + w.tmp + `/f'`,
		`t read_slash_tmp ls /tmp`,
		`t write_devnull sh -c 'echo x > /dev/null'`,
		`t read_system cat /etc/hostname`,
		`t exec_system /usr/bin/true`,
		`t read_ssh cat ` + h + `/.ssh/key`,
		`t list_ssh ls ` + h + `/.ssh`,
		`t read_home_other cat ` + h + `/other/file`,
		`t write_home sh -c 'echo x > ` + h + `/newfile'`,
		`t write_outside sh -c 'echo x > ` + o + `/new'`,
		`t clobber_outside sh -c 'echo x > ` + o + `/victim'`,
		`t truncate_outside truncate -s 0 ` + o + `/victim`,
		`t remove_outside rm -rf ` + o,
		`t read_outside cat ` + o + `/victim`,
		`t write_gate_disarmed sh -c 'echo x > ` + h + `/.opendaisugi/gate/DISARMED'`,
		`t write_gate_envelope sh -c 'echo x > ` + h + `/.opendaisugi/gate/envelopes/default.json'`,
		`t read_gate_envelope cat ` + h + `/.opendaisugi/gate/envelopes/default.json`,
		`t mkdir_gate mkdir ` + h + `/.opendaisugi/gate/envelopes/x`,
		`t rename_in_gate mv ` + h + `/.opendaisugi/gate/envelopes/default.json ` + h + `/.opendaisugi/gate/envelopes/d2.json`,
		`t hardlink_ssh ln ` + h + `/.ssh/key ` + w.ws + `/k`,
		`t symlink_ssh_read sh -c 'ln -s ` + h + `/.ssh/key ` + w.ws + `/lnk && cat ` + w.ws + `/lnk'`,
		`t move_ssh_out mv ` + h + `/.ssh/key ` + w.ws + `/k2`,
		`t move_ws_to_outside mv ` + w.ws + `/ok ` + o + `/ok`,
		`t child_reads_ssh sh -c 'sh -c "cat ` + h + `/.ssh/key"'`,
		`t python_rmtree python3 -c "import shutil; shutil.rmtree('` + o + `')"`,
		`t python_disarm python3 -c "open('` + h + `/.opendaisugi/gate/DISARMED','w')"`,
	}, "\n")
}

func parseProbes(out string) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if f := strings.Fields(l); len(f) == 2 {
			m[f[0]] = f[1]
		}
	}
	return m
}

func TestFenceHoldsTheLine(t *testing.T) {
	w := newWorld(t)
	so, se, code := runFence(t, w, w.env(), "--workspace", w.ws, "--", "/bin/sh", "-c", probes(w))
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, so, se)
	}
	got := parseProbes(so)
	want := map[string]string{
		"read_ws": "ok", "write_ws": "ok", "mkdir_ws": "ok", "write_tmp": "ok", "write_devnull": "ok",
		"write_shared_tmp": "denied", "read_slash_tmp": "denied",
		"read_system": "ok", "exec_system": "ok", "read_gate_envelope": "ok",
		"read_ssh": "denied", "list_ssh": "denied", "read_home_other": "denied", "write_home": "denied",
		"write_outside": "denied", "clobber_outside": "denied", "truncate_outside": "denied",
		"remove_outside": "denied", "read_outside": "denied", "write_gate_disarmed": "denied",
		"write_gate_envelope": "denied", "mkdir_gate": "denied", "rename_in_gate": "denied",
		"hardlink_ssh": "denied", "symlink_ssh_read": "denied", "move_ssh_out": "denied",
		"move_ws_to_outside": "denied", "child_reads_ssh": "denied", "python_rmtree": "denied",
		"python_disarm": "denied",
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s: got %q, want %q", name, got[name], w)
		}
	}
	for _, p := range []string{w.home + "/.opendaisugi/gate/DISARMED", w.outside + "/new", w.home + "/newfile", w.ws + "/k"} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s exists after the run", p)
		}
	}
	if b, _ := os.ReadFile(w.outside + "/victim"); string(b) != "VICTIM" {
		t.Errorf("victim changed: %q", b)
	}
	if b, _ := os.ReadFile(w.ws + "/new"); string(b) != "x\n" {
		t.Errorf("the workspace write did not land: %q", b)
	}
}

func TestNoFenceRunsTheHarnessUnfenced(t *testing.T) {
	w := newWorld(t)
	so, _, code := runFence(t, w, w.env(), "--no-fence", "--", "/bin/cat", w.home+"/.ssh/key")
	if code != 0 || so != "SECRET" {
		t.Fatalf("exit %d, out %q", code, so)
	}
}

// FN-V-1: a process inside a fenced pane sets its children's environment,
// so no variable may turn the fence off or move the secrets.
func TestFN_V1_NoVariableTurnsTheFenceOff(t *testing.T) {
	w := newWorld(t)
	so, _, _ := runFence(t, w, w.env("DAISUGI_FENCE=off"), "--", "/bin/cat", w.home+"/.ssh/key")
	if so == "SECRET" {
		t.Errorf("DAISUGI_FENCE=off ran the harness unfenced")
	}
	// HOME moved: the password entry's home keeps its secrets shut. The
	// fake HOME is not the password home here, so read the moved-from one
	// through a HOME that points elsewhere.
	other := w.base + "/elsewhere"
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	so, se, code := runFence(t, w, []string{"HOME=" + other, "PATH=/usr/bin:/bin"}, "--print", "--json", "--workspace", w.ws)
	if code != 0 {
		t.Fatalf("exit %d %s", code, se)
	}
	var p struct{ Home string }
	_ = json.Unmarshal([]byte(so), &p)
	if p.Home != other {
		t.Errorf("home %q, want %q", p.Home, other)
	}
}

func TestFenceRefusesWhenLandlockIsMissing(t *testing.T) {
	for _, probe := range []string{"enosys", "eopnotsupp", "abi:2"} {
		w := newWorld(t)
		marker := w.ws + "/ran"
		so, se, code := runFence(t, w, w.env("DAISUGI_FENCE_PROBE="+probe), "--", "/bin/sh", "-c", "touch "+marker)
		if code != 1 {
			t.Errorf("%s: exit %d, want 1", probe, code)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Errorf("%s: the harness ran", probe)
		}
		if !strings.Contains(se, "--no-fence") || !strings.Contains(se, `fence = "off"`) {
			t.Errorf("%s: no way out in %q (stdout %q)", probe, se, so)
		}
	}
}

func TestFenceRefusesAWorkspaceThatWouldOpenASecret(t *testing.T) {
	w := newWorld(t)
	_, se, code := runFence(t, w, w.env(), "--workspace", w.home, "--", "/bin/true")
	if code != 1 || !strings.Contains(se, "would reach the secret") || !strings.Contains(se, "--no-fence") {
		t.Fatalf("exit %d, stderr %q", code, se)
	}
}

// FN-V-3: a secret is never "named" by being the workspace, a link to it,
// or the temp dir.
func TestFN_V3_ASecretAsWorkspaceOrTempIsRefused(t *testing.T) {
	w := newWorld(t)
	if err := os.Symlink(w.home+"/.ssh", w.ws+"/x"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--workspace", w.home + "/.ssh"},
		{"--workspace", w.ws + "/x"},
		{"--tmpdir", w.home + "/.ssh"},
		{"--workdir", w.ws + "/x"},
	} {
		so, se, code := runFence(t, w, w.env(), append(args, "--", "/bin/cat", w.home+"/.ssh/key")...)
		if code != 1 || so == "SECRET" || !strings.Contains(se, "secret") {
			t.Errorf("%v: exit %d out %q err %q", args, code, so, se)
		}
	}
}

// FN-V-8: a rule root the plan did not resolve (config.yaml here) that is
// a link to a secret is caught when the fence opens it.
func TestFN_V8_ARuleRootThatLinksToASecretIsRefused(t *testing.T) {
	w := newWorld(t)
	if err := os.Symlink(w.home+"/.ssh/key", w.home+"/.opendaisugi/config.yaml"); err != nil {
		t.Fatal(err)
	}
	so, se, code := runFence(t, w, w.env(), "--", "/bin/cat", w.home+"/.opendaisugi/config.yaml")
	if code != 1 || so == "SECRET" || !strings.Contains(se, "resolves to") {
		t.Fatalf("exit %d out %q err %q", code, so, se)
	}
	w2 := newWorld(t)
	if err := os.MkdirAll(w2.home+"/.config", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(w2.home+"/.ssh", w2.home+"/.config/claude"); err != nil {
		t.Fatal(err)
	}
	so, se, code = runFence(t, w2, w2.env(), "--harness", "claude", "--", "/bin/cat", w2.home+"/.config/claude/key")
	if code != 1 || so == "SECRET" {
		t.Fatalf("harness link: exit %d out %q err %q", code, so, se)
	}
}

// FN-V-2: unix sockets outside the granted dirs, abstract sockets and
// signals to outside processes are all stopped; the coppice socket and the
// gate socket still connect.
func TestFN_V2_SocketsAndSignalsStayInside(t *testing.T) {
	w := newWorld(t)
	listen := func(addr string) {
		ln, err := net.Listen("unix", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
	}
	cop := w.base + "/run/coppice"
	if err := os.MkdirAll(cop, 0o700); err != nil {
		t.Fatal(err)
	}
	listen(w.outside + "/agent.sock")
	listen(cop + "/server.sock")
	abstract := "@daisugi-fence-test-" + strconv.Itoa(os.Getpid())
	listen(abstract)
	victim := exec.Command("/bin/sleep", "30")
	if err := victim.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = victim.Process.Kill(); _ = victim.Wait() })
	py := func(addr string) string {
		if strings.HasPrefix(addr, "@") {
			addr = `"\0` + addr[1:] + `"`
		} else {
			addr = `"` + addr + `"`
		}
		return `python3 -c 'import socket; s=socket.socket(socket.AF_UNIX); s.connect(` + addr + `)'`
	}
	script := strings.Join([]string{
		`t() { n=$1; shift; if "$@" >/dev/null 2>&1; then echo "$n ok"; else echo "$n denied"; fi; }`,
		`t outside_sock sh -c '` + strings.ReplaceAll(py(w.outside+"/agent.sock"), "'", `'"'"'`) + `'`,
		`t coppice_sock sh -c '` + strings.ReplaceAll(py(cop+"/server.sock"), "'", `'"'"'`) + `'`,
		`t abstract_sock sh -c '` + strings.ReplaceAll(py(abstract), "'", `'"'"'`) + `'`,
		`t kill_outside kill -TERM ` + strconv.Itoa(victim.Process.Pid),
		`t ssh_env sh -c 'test -z "$SSH_AUTH_SOCK$DBUS_SESSION_BUS_ADDRESS$DISPLAY"'`,
	}, "\n")
	env := w.env("SSH_AUTH_SOCK="+w.outside+"/agent.sock", "DBUS_SESSION_BUS_ADDRESS=unix:path=/x", "DISPLAY=:0")
	so, se, code := runFence(t, w, env, "--coppice-socket", cop+"/server.sock", "--", "/bin/sh", "-c", script)
	if code != 0 {
		t.Fatalf("exit %d %s %s", code, so, se)
	}
	want := map[string]string{"outside_sock": "denied", "coppice_sock": "ok", "abstract_sock": "denied",
		"kill_outside": "denied", "ssh_env": "ok"}
	got := parseProbes(so)
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %q, want %q", k, got[k], v)
		}
	}
	if victim.ProcessState != nil {
		t.Errorf("the outside process was killed")
	}
	// Below ABI 9 the fence refuses, unless weak IPC is allowed.
	_, se, code = runFence(t, w, w.env("DAISUGI_FENCE_PROBE=abi:5"), "--", "/bin/true")
	if code != 1 || !strings.Contains(se, "--allow-weak-ipc") {
		t.Errorf("abi 5: exit %d %q", code, se)
	}
	_, se, code = runFence(t, w, w.env("DAISUGI_FENCE_PROBE=abi:5"), "--allow-weak-ipc", "--", "/bin/true")
	if code != 0 {
		t.Errorf("abi 5 weak ipc: exit %d %q", code, se)
	}
}

// FN-V-10: a descriptor the caller left open does not reach the harness,
// unless --keep-fd names it.
func TestFN_V10_InheritedDescriptorsAreClosed(t *testing.T) {
	w := newWorld(t)
	f, err := os.Open(w.outside + "/victim")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	so, _, _ := runFenceCmd(t, w, w.env(), []*os.File{f}, "--", "/bin/sh", "-c", "cat <&3")
	if so == "VICTIM" {
		t.Errorf("fd 3 reached the harness")
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	so, se, code := runFenceCmd(t, w, w.env(), []*os.File{f}, "--keep-fd", "3", "--", "/bin/sh", "-c", "cat <&3")
	if so != "VICTIM" || code != 0 {
		t.Errorf("--keep-fd 3: exit %d out %q err %q", code, so, se)
	}
}

// FN-V-9: the temp dir is private, on disk, and not /tmp.
func TestFN_V9_ThePrivateTempDir(t *testing.T) {
	// --allow /tmp:rw below must not reach the fake home's secrets, so the
	// world lives off /tmp whatever TMPDIR the run has: TMPDIR is set here
	// to a short dir under /var/tmp, which is not /tmp.
	cache := "/var/tmp"
	if st, err := os.Stat(cache); err != nil || !st.IsDir() {
		t.Skip("no /var/tmp")
	}
	off, err := os.MkdirTemp(cache, "fv9")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(off) })
	t.Setenv("TMPDIR", off)
	w := newWorld(t)
	so, se, code := runFence(t, w, w.env(), "--", "/bin/sh", "-c", `echo "$TMPDIR"; stat -c %a "$TMPDIR"`)
	if code != 0 {
		t.Fatalf("exit %d %s", code, se)
	}
	lines := strings.Fields(so)
	if len(lines) != 2 || !strings.HasPrefix(lines[0], w.home+"/.local/share/opendaisugi/fence-tmp/") || lines[1] != "700" {
		t.Errorf("tmpdir: %q", so)
	}
	// A dead run's dir is swept at the next start.
	dead := w.home + "/.local/share/opendaisugi/fence-tmp/999999999"
	if err := os.MkdirAll(dead, 0o700); err != nil {
		t.Fatal(err)
	}
	runFence(t, w, w.env(), "--", "/bin/true")
	if _, err := os.Stat(dead); err == nil {
		t.Errorf("a dead run's temp dir stayed")
	}
	so, _, code = runFence(t, w, w.env(), "--allow", "/tmp:rw", "--", "/bin/sh", "-c", `echo "$TMPDIR"`)
	if code != 0 || strings.TrimSpace(so) != w.tmp {
		t.Errorf("--allow /tmp:rw: %q", so)
	}
}

func TestFencePrintJSON(t *testing.T) {
	w := newWorld(t)
	so, se, code := runFence(t, w, w.env(), "--print", "--json", "--harness", "claude", "--workspace", w.ws)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, se)
	}
	var p struct {
		Rules []struct{ Path, Mode string }
	}
	if err := json.Unmarshal([]byte(so), &p); err != nil {
		t.Fatal(err)
	}
	modes := map[string]string{}
	for _, r := range p.Rules {
		modes[r.Path] = r.Mode
	}
	if modes[w.ws] != "rw" || modes[w.home+"/.claude"] != "ro" || modes[claudeProject(w)] != "rw" {
		t.Errorf("missing grants: %v", modes)
	}
	if _, ok := modes[w.home+"/.ssh"]; ok {
		t.Errorf("ssh is granted: %v", modes)
	}
	if _, ok := modes[w.tmp]; ok {
		t.Errorf("the shared temp dir is granted: %v", modes)
	}
	if modes[w.home+"/.opendaisugi/gate"] != "sock" {
		t.Errorf("gate root is not read only with its socket: %v", modes)
	}
}

// claudeProject is this world's workspace's dir under ~/.claude/projects.
func claudeProject(w world) string {
	return w.home + "/.claude/projects/" + regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(w.ws, "-")
}

func TestFenceHarnessDirIsUsable(t *testing.T) {
	w := newWorld(t)
	p := claudeProject(w)
	so, _, code := runFence(t, w, w.env(), "--harness", "claude", "--", "/bin/sh", "-c",
		"echo x > "+p+"/s.jsonl && cat "+p+"/s.jsonl")
	if code != 0 || so != "x\n" {
		t.Fatalf("exit %d, out %q", code, so)
	}
}

// FN-V-6: the harness's config, hooks and settings stay read only.
func TestFN_V6_HarnessConfigIsReadOnly(t *testing.T) {
	w := newWorld(t)
	if err := os.WriteFile(w.home+"/.claude/settings.json", []byte(`{"hooks":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// FN-R-31: only this workspace's project dir is writable; another
	// project's memory is not. Landlock cannot cut memory/ out of the own one.
	if err := os.MkdirAll(w.home+"/.claude/projects/-elsewhere/memory", 0o700); err != nil {
		t.Fatal(err)
	}
	script := `t() { n=$1; shift; if "$@" >/dev/null 2>&1; then echo "$n ok"; else echo "$n denied"; fi; }
t read_settings cat ` + w.home + `/.claude/settings.json
t write_settings sh -c 'echo "{}" > ` + w.home + `/.claude/settings.json'
t plant_hook sh -c 'mkdir -p ` + w.home + `/.claude/hooks'
t plant_skill sh -c 'echo x > ` + w.home + `/.claude/CLAUDE.md'
t write_projects sh -c 'echo x > ` + claudeProject(w) + `/s.jsonl'
t write_own_memory sh -c 'mkdir -p ` + claudeProject(w) + `/memory && echo x > ` + claudeProject(w) + `/memory/m.md'
t write_other_project sh -c 'echo x > ` + w.home + `/.claude/projects/p'
t write_other_memory sh -c 'echo x > ` + w.home + `/.claude/projects/-elsewhere/memory/MEMORY.md'`
	so, se, code := runFence(t, w, w.env(), "--harness", "claude", "--", "/bin/sh", "-c", script)
	if code != 0 {
		t.Fatalf("exit %d %s", code, se)
	}
	want := map[string]string{"read_settings": "ok", "write_settings": "denied", "plant_hook": "denied",
		"plant_skill": "denied", "write_projects": "ok", "write_own_memory": "ok", "write_other_project": "denied",
		"write_other_memory": "denied"}
	got := parseProbes(so)
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %q, want %q", k, got[k], v)
		}
	}
}

func TestFenceAllowOpensOnlyWhatItNames(t *testing.T) {
	w := newWorld(t)
	script := "cat " + w.outside + "/victim; echo y > " + w.outside + "/w"
	_, _, code := runFence(t, w, w.env(), "--allow", w.outside, "--", "/bin/sh", "-c", script)
	if _, err := os.Stat(w.outside + "/w"); err == nil || code == 0 {
		t.Errorf("read-only allow let a write through (exit %d)", code)
	}
	_, _, code = runFence(t, w, w.env(), "--allow", w.outside+":rw", "--", "/bin/sh", "-c", script)
	if _, err := os.Stat(w.outside + "/w"); err != nil || code != 0 {
		t.Errorf("rw allow did not work (exit %d): %v", code, err)
	}
}

// The gate hook is a child of the harness, so it runs fenced. It must still
// decide the same, and still log, while the agent cannot touch the marker,
// the envelope or the answers.
func TestGateCheckDecidesTheSameInsideTheFence(t *testing.T) {
	w := newWorld(t)
	root := w.home + "/.opendaisugi/gate"
	env := `{"generated_by":"t","task":"t","permissions":{"file_read":["/**"],"file_write":[]}}`
	if err := os.WriteFile(root+"/envelopes/default.json", []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	payloads := map[string]string{
		"allow": `{"session_id":"s1","tool_name":"Read","tool_input":{"file_path":"/work/a"},"cwd":"/work"}`,
		"deny":  `{"session_id":"s2","tool_name":"Write","tool_input":{"file_path":"/work/a","content":"x"},"cwd":"/work"}`,
	}
	bin := fenceBin(t)
	if err := os.MkdirAll(root+"/answers", 0o700); err != nil {
		t.Fatal(err)
	}
	for name, payload := range payloads {
		pf := w.ws + "/" + name + ".json"
		if err := os.WriteFile(pf, []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
		check := bin + " gate check --mode enforce --root " + root + " --format claude < " + pf
		plainSO, plainSE, plainCode := runFence(t, w, w.env(), "--no-fence", "--", "/bin/sh", "-c", check)
		fenceSO, fenceSE, fenceCode := runFence(t, w, w.env(), "--workspace", w.ws, "--", "/bin/sh", "-c", check)
		fenceSE = startedGateLine.ReplaceAllString(fenceSE, "")
		if plainSO != fenceSO || plainSE != fenceSE || plainCode != fenceCode {
			t.Errorf("%s: plain (%d %q %q) differs from fenced (%d %q %q)", name, plainCode, plainSO, plainSE, fenceCode, fenceSO, fenceSE)
		}
		want := 0
		if name == "deny" {
			want = 2
		}
		if fenceCode != want {
			t.Errorf("%s: fenced exit %d, want %d (%q)", name, fenceCode, want, fenceSE)
		}
	}
	if _, err := os.Stat(root + "/audit/s1.jsonl"); err != nil {
		t.Errorf("the fenced hook wrote no audit log: %v", err)
	}
	if _, err := os.Stat(w.home + "/.opendaisugi/sessions/s1.jsonl"); err != nil {
		t.Errorf("the fenced hook wrote no session tree: %v", err)
	}
	// What the agent still cannot do under the gate. answers/ exists, so a
	// refused write is the fence's doing, not a missing directory.
	script := `t() { n=$1; shift; if "$@" >/dev/null 2>&1; then echo "$n ok"; else echo "$n denied"; fi; }
t disarm sh -c 'echo x > ` + root + `/DISARMED'
t answer sh -c 'echo x > ` + root + `/answers/a1.json'
t envelope sh -c 'echo x > ` + root + `/envelopes/default.json'
t erase_audit rm ` + root + `/audit/s1.jsonl
t truncate_audit truncate -s 0 ` + root + `/audit/s1.jsonl
t rename_audit mv ` + root + `/audit/s1.jsonl ` + root + `/audit/gone
t append_audit sh -c 'echo forged >> ` + root + `/audit/s1.jsonl'
t write_ask sh -c 'mkdir -p ` + root + `/asks; echo x > ` + root + `/asks/a.json'
t write_session sh -c 'echo x >> ` + w.home + `/.opendaisugi/sessions/s1.jsonl'`
	so, se, code := runFence(t, w, w.env(), "--workspace", w.ws, "--", "/bin/sh", "-c", script)
	if code != 0 {
		t.Fatalf("exit %d %s", code, se)
	}
	for k, v := range parseProbes(so) {
		if v != "denied" {
			t.Errorf("%s: %s, want denied", k, v)
		}
	}
	if len(parseProbes(so)) != 9 {
		t.Errorf("probes: %q", so)
	}
}

// FN-V-4: inside the fence the hook is a thin client of the resident gate.
// With the server down it denies; an operator's answer to an ask posted
// from inside the fence is honoured, and the answer is consumed.
func TestFN_V4_FencedHookAsksTheResidentGate(t *testing.T) {
	w := newWorld(t)
	root := w.home + "/.opendaisugi/gate"
	env := `{"generated_by":"t","task":"t","permissions":{"file_read":["/**"],"file_write":[]}}`
	if err := os.WriteFile(root+"/envelopes/default.json", []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := fenceBin(t)
	payload := `{"session_id":"s9","tool_name":"Write","tool_use_id":"tu1","tool_input":{"file_path":"/work/a","content":"x"},"cwd":"/work"}`
	pf := w.ws + "/ask.json"
	if err := os.WriteFile(pf, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	// No server, gate on: a fenced hook denies.
	cmd := exec.Command(bin, "gate", "check", "--mode", "enforce", "--root", root, "--format", "claude")
	cmd.Env = append(w.env(), "DAISUGI_FENCED=landlock")
	cmd.Stdin = strings.NewReader(`{"session_id":"s0","tool_name":"Read","tool_input":{"file_path":"/work/a"},"cwd":"/work"}`)
	var se bytes.Buffer
	cmd.Stderr = &se
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 || !strings.Contains(se.String(), "resident gate") {
		t.Fatalf("server down: %v %q", err, se.String())
	}
	// The operator: a heartbeat, then an allow for the ask once it shows.
	for _, d := range []string{root + "/asks", root + "/answers"} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	beat := func() {
		_ = os.WriteFile(root+"/operator.json", []byte(`{"pid":`+strconv.Itoa(os.Getpid())+`}`), 0o600)
	}
	beat()
	done := make(chan struct{})
	answered := make(chan bool, 1)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(100 * time.Millisecond):
			}
			beat()
			raw, err := os.ReadFile(root + "/asks/tu1.json")
			if err != nil {
				continue
			}
			var ask struct{ Nonce string }
			if json.Unmarshal(raw, &ask) != nil || ask.Nonce == "" {
				continue
			}
			ans := `{"decision":"allow","nonce":"` + ask.Nonce + `","reason":"ok by test"}`
			_ = os.WriteFile(root+"/answers/tu1.json", []byte(ans), 0o600)
			answered <- true
			return
		}
	}()
	defer close(done)
	check := bin + " gate check --mode enforce --root " + root + " --format claude --ask --ask-timeout 20 < " + pf
	so, se2, code := runFence(t, w, w.env(), "--workspace", w.ws, "--", "/bin/sh", "-c", check)
	audit, _ := os.ReadFile(root + "/audit/s9.jsonl")
	if code != 0 || !strings.Contains(string(audit), "allowed by operator: ok by test") {
		t.Errorf("ask under the fence: exit %d out %q err %q audit %q", code, so, se2, audit)
	}
	select {
	case <-answered:
	default:
		t.Errorf("no ask was posted")
	}
	if _, err := os.Stat(root + "/answers/tu1.json"); err == nil {
		t.Errorf("the answer was not consumed")
	}
}

// FN-V-7: Landlock cannot stop chmod, so an agent can make its audit log
// read only. An enforce-mode allow whose audit line cannot land is denied.
func TestFN_V7_AReadOnlyAuditLogDenies(t *testing.T) {
	w := newWorld(t)
	root := w.home + "/.opendaisugi/gate"
	env := `{"generated_by":"t","task":"t","permissions":{"file_read":["/**"],"file_write":[]}}`
	if err := os.WriteFile(root+"/envelopes/default.json", []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(mode string) (int, string) {
		cmd := exec.Command(fenceBin(t), "gate", "check", "--mode", mode, "--root", root, "--format", "claude")
		cmd.Env = w.env()
		cmd.Stdin = strings.NewReader(`{"session_id":"s1","tool_name":"Read","tool_input":{"file_path":"/w/a"},"cwd":"/w"}`)
		var se bytes.Buffer
		cmd.Stderr = &se
		err := cmd.Run()
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), se.String()
		}
		return 0, se.String()
	}
	if code, se := run("enforce"); code != 0 {
		t.Fatalf("first call: %d %q", code, se)
	}
	if err := os.Chmod(root+"/audit/s1.jsonl", 0o444); err != nil {
		t.Fatal(err)
	}
	if code, se := run("enforce"); code != 2 || !strings.Contains(se, "cannot write its audit log") {
		t.Errorf("read-only audit, enforce: %d %q", code, se)
	}
	if code, se := run("audit"); code != 0 {
		t.Errorf("read-only audit, audit mode: %d %q", code, se)
	}
}

// FN-R-21: the fence, still outside its ruleset, records its run in the
// gate root, so the resident gate binds the workspace for checkpoints. The
// record is the harness's own pid, and nothing inside the fence can write
// it.
func TestFN_R21_TheFenceRecordsItsRun(t *testing.T) {
	w := newWorld(t)
	root := w.home + "/.opendaisugi/gate"
	script := `cat ` + root + `/fence-runs/$$.json; echo
if sh -c 'echo x > ` + root + `/fence-runs/$$.json' 2>/dev/null; then echo wrote; else echo denied; fi
if sh -c 'echo x > ` + root + `/fence-runs/1.json' 2>/dev/null; then echo wrote; else echo denied; fi`
	so, se, code := runFence(t, w, w.env(), "--workspace", w.ws, "--", "/bin/sh", "-c", script)
	if code != 0 {
		t.Fatalf("exit %d %s", code, se)
	}
	lines := strings.Split(strings.TrimSpace(so), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], `"workspace":"`+w.ws+`"`) || lines[1] != "denied" || lines[2] != "denied" {
		t.Fatalf("got %q (%s)", so, se)
	}
}

// FN-R-25 (review N-4): a foreman worker's tree run stays outside the
// fence and only sprig runs under it. sprig cannot write the worker's gate
// root, writes its own session dir, and its gate calls reach the resident
// gate the fence starts on that root. The worker's rule list has no write
// grant over its gate root.
func TestFN_R25_AWorkersSprigRunsFencedAndCannotWriteItsGateRoot(t *testing.T) {
	w := newWorld(t)
	bin := fenceBin(t)
	work := w.base + "/work"
	root := work + "/gate"
	envFile := w.base + "/env.json"
	env := `{"id":"env_w","generated_by":"t","task":"t","permissions":{"file_read":["` + w.ws + `/**"],` +
		`"file_write":["` + w.ws + `/**"],"network":false,"shell":true,"shell_allowlist":["cat","ls"],` +
		`"max_execution_time_s":60,"max_output_size_mb":20}}`
	if err := os.WriteFile(envFile, []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(stdin string, extra []string, args ...string) (string, string, int) {
		cmd := exec.Command(bin, args...)
		cmd.Dir = w.ws
		cmd.Env = append(w.env(), extra...)
		cmd.Stdin = strings.NewReader(stdin)
		var so, se bytes.Buffer
		cmd.Stdout, cmd.Stderr = &so, &se
		code := 0
		if err := cmd.Run(); err != nil {
			ee, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatal(err)
			}
			code = ee.ExitCode()
		}
		killStarted(t, se.String())
		return so.String(), se.String(), code
	}
	if so, se, code := run("", nil, "tree", "root", envFile, "--session", "project-x", "--data-dir", work, "--root", root); code != 0 {
		t.Fatalf("tree root: %d %s %s", code, so, se)
	}
	// The fake sprig, in the workspace (the fence lets it run there).
	sprig := w.ws + "/.bin/sprig"
	script := `#!/bin/sh
gate= sess=
while [ $# -gt 0 ]; do
  case "$1" in --gate-cmd) gate=$2; shift;; --session-dir) sess=$2; shift;; esac; shift
done
r() { if "$@" >/dev/null 2>&1; then printf ok; else printf denied; fi; }
envw=$(r sh -c "echo x > ` + root + `/envelopes/x.json")
disarm=$(r sh -c "echo x > ` + root + `/DISARMED")
sessw=$(r sh -c "echo x > $sess/probe")
printf '{"session_id":"s","tool_name":"Read","tool_input":{"file_path":"` + w.ws + `/ok"},"cwd":"` + w.ws + `"}' > ` + w.ws + `/p.json
if sh -c "$gate --format claude < ` + w.ws + `/p.json" >/dev/null 2>&1; then gatev=allow; else gatev=deny; fi
printf '{"answer": "env=%s disarm=%s sess=%s gate=%s", "turns": 1}' "$envw" "$disarm" "$sessw" "$gatev"
`
	if err := os.MkdirAll(filepath.Dir(sprig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sprig, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	fence := `["` + bin + `","fence","--workspace","` + w.ws + `","--gate-root","` + root + `"]`
	req := `{"task":"t","envelope":` + env + `,"tools":["Read","Bash"]}`
	so, se, code := run(req, []string{"DAISUGI_SPRIG=" + sprig, "DAISUGI_SPRIG_FENCE=" + fence},
		"tree", "run", "--parent", "project-x", "--profile=default", "--data-dir", work, "--root", root, "-")
	if code != 0 || !strings.Contains(so, `"answer": "env=denied disarm=denied sess=ok gate=allow"`) {
		t.Fatalf("tree run: %d\n%s\n%s", code, so, se)
	}
	// The rule list the worker's sprig gets: no write over the gate root,
	// and an --allow that would open it is refused.
	so, se, code = run("", nil, "fence", "--print", "--json", "--workspace", w.ws, "--gate-root", root,
		"--allow", work+"/sprig-sessions:rw", "--", "/bin/true")
	if code != 0 {
		t.Fatalf("print: %d %s", code, se)
	}
	var p struct {
		Rules []struct{ Path, Mode string }
	}
	if err := json.Unmarshal([]byte(so), &p); err != nil {
		t.Fatal(err)
	}
	for _, r := range p.Rules {
		if strings.Contains(r.Mode, "rw") && (r.Path == root || strings.HasPrefix(root, r.Path+"/") || strings.HasPrefix(r.Path, root+"/")) {
			t.Errorf("a write grant over the gate root: %+v", r)
		}
	}
	if _, _, code := run("", nil, "fence", "--workspace", w.ws, "--gate-root", root, "--allow", work+":rw", "--", "/bin/true"); code == 0 {
		t.Error("--allow over the worker's gate root was not refused")
	}
}

// FN-R-32: the fence starts the resident gate with its captures root fixed
// at <root>/captures, so a fenced run's allowed calls land there, written
// by the server outside the fence.
func TestFN_R32_AFencedRunsCapturesLand(t *testing.T) {
	w := newWorld(t)
	root := w.home + "/.opendaisugi/gate"
	env := `{"generated_by":"t","task":"t","permissions":{"file_read":["/**"],"file_write":[]}}`
	if err := os.WriteFile(root+"/envelopes/default.json", []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	pf := w.ws + "/call.json"
	payload := `{"session_id":"s7","tool_name":"Read","tool_use_id":"tu7","tool_input":{"file_path":"/work/a"},"cwd":"/work"}`
	if err := os.WriteFile(pf, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	check := fenceBin(t) + " gate check --mode enforce --root " + root + " --format claude < " + pf
	so, se, code := runFence(t, w, w.env(), "--workspace", w.ws, "--", "/bin/sh", "-c", check)
	if code != 0 {
		t.Fatalf("exit %d out %q err %q", code, so, se)
	}
	ents, err := os.ReadDir(root + "/captures")
	if err != nil || len(ents) == 0 {
		t.Fatalf("no capture under %s/captures: %v", root, err)
	}
}

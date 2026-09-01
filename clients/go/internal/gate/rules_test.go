package gate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// allowAllEnvelope permits every path and shell command, so a deny in
// these tests can only come from the pane rule, never from the envelope
// underneath it.
const allowAllEnvelope = `{"generated_by":"t","task":"t","permissions":{
 "file_read":["/**"],"file_write":["/**"],"shell":true,
 "shell_allowlist":["cat","cp","python"],"shell_allow_decomposition":true}}`

func setupAllowAll(t *testing.T) (string, []string) {
	t.Helper()
	root, env := setup(t)
	if err := os.WriteFile(filepath.Join(root, "envelopes", "default.json"), []byte(allowAllEnvelope), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, env
}

// coppiceSecrets is pane_rule._SECRET_FILE's guarded set, each spelled
// relative to a coppice data directory.
var coppiceSecrets = []string{
	"web/token",
	"web/ca/ca.key",
	"web/ca/leaf.key",
	"web/tls/tailscale.key",
	"voice/token",
}

// coppicePublicFiles sit beside the secrets above: a cert nobody needs to
// keep private, and web.json, which holds a path to a token, not a token.
var coppicePublicFiles = []string{
	"web/ca/ca.crt",
	"web/ca/leaf.crt",
	"web/ca/meta.json",
	"web/tls/tailscale.crt",
	"web/web.json",
}

const testDataDir = "/home/user/.opendaisugi/coppice"

func TestWebDoorCoversEveryCoppiceSecret(t *testing.T) {
	for _, s := range coppiceSecrets {
		if !webDoor(testDataDir + "/" + s) {
			t.Errorf("webDoor missed %s", s)
		}
	}
	for _, p := range coppicePublicFiles {
		if webDoor(testDataDir + "/" + p) {
			t.Errorf("webDoor wrongly caught %s", p)
		}
	}
}

func TestReadOfEveryCoppiceSecretIsDenied(t *testing.T) {
	root, env := setupAllowAll(t)
	for _, s := range coppiceSecrets {
		path := testDataDir + "/" + s
		p, _ := json.Marshal(map[string]any{"session_id": "s1", "tool_name": "Read", "cwd": "/work",
			"tool_input": map[string]string{"file_path": path}})
		res := run(t, root, env, "enforce", string(p))
		if res.Exit != 2 || !strings.Contains(res.Stderr, paneRefusal) {
			t.Errorf("%s: got %+v", s, res)
		}
	}
}

func TestShellCatOfEveryCoppiceSecretIsDenied(t *testing.T) {
	root, env := setupAllowAll(t)
	for _, s := range coppiceSecrets {
		path := testDataDir + "/" + s
		p, _ := json.Marshal(map[string]any{"session_id": "s1", "tool_name": "Bash", "cwd": "/work",
			"tool_input": map[string]string{"command": "cat " + path}})
		res := run(t, root, env, "enforce", string(p))
		if res.Exit != 2 || !strings.Contains(res.Stderr, paneRefusal) {
			t.Errorf("%s: got %+v", s, res)
		}
	}
}

func TestShellRedirectReadOfEveryCoppiceSecretIsDenied(t *testing.T) {
	root, env := setupAllowAll(t)
	for _, s := range coppiceSecrets {
		path := testDataDir + "/" + s
		p, _ := json.Marshal(map[string]any{"session_id": "s1", "tool_name": "Bash", "cwd": "/work",
			"tool_input": map[string]string{"command": "python steal.py < " + path}})
		res := run(t, root, env, "enforce", string(p))
		if res.Exit != 2 || !strings.Contains(res.Stderr, paneRefusal) {
			t.Errorf("%s: got %+v", s, res)
		}
	}
}

func TestACpOfEveryCoppiceSecretAsItsSourceIsDenied(t *testing.T) {
	root, env := setupAllowAll(t)
	for _, s := range coppiceSecrets {
		path := testDataDir + "/" + s
		p, _ := json.Marshal(map[string]any{"session_id": "s1", "tool_name": "Bash", "cwd": "/work",
			"tool_input": map[string]string{"command": "cp " + path + " /work/out"}})
		res := run(t, root, env, "enforce", string(p))
		if res.Exit != 2 || !strings.Contains(res.Stderr, paneRefusal) {
			t.Errorf("%s: got %+v", s, res)
		}
	}
}

func TestEveryCoppiceSecretIsDeniedFromInsideTheDataDir(t *testing.T) {
	root, env := setupAllowAll(t)
	for _, s := range coppiceSecrets {
		// The absolute spelling, read with cwd set to the data directory.
		p, _ := json.Marshal(map[string]any{"session_id": "s1", "tool_name": "Read", "cwd": testDataDir,
			"tool_input": map[string]string{"file_path": testDataDir + "/" + s}})
		res := run(t, root, env, "enforce", string(p))
		if res.Exit != 2 || !strings.Contains(res.Stderr, paneRefusal) {
			t.Errorf("read %s: got %+v", s, res)
		}
		// The relative spelling, typed from cwd set to the data directory.
		p2, _ := json.Marshal(map[string]any{"session_id": "s1", "tool_name": "Bash", "cwd": testDataDir,
			"tool_input": map[string]string{"command": "cat " + s}})
		res2 := run(t, root, env, "enforce", string(p2))
		if res2.Exit != 2 || !strings.Contains(res2.Stderr, paneRefusal) {
			t.Errorf("cat %s: got %+v", s, res2)
		}
	}
}

func TestAnMCPToolThatNamesACoppiceSecretIsDenied(t *testing.T) {
	root, env := setupAllowAll(t)
	for _, s := range coppiceSecrets {
		path := testDataDir + "/" + s
		p, _ := json.Marshal(map[string]any{"session_id": "s1", "tool_name": "mcp__filesystem__read_file",
			"cwd": "/work", "tool_input": map[string]string{"path": path}})
		res := run(t, root, env, "enforce", string(p))
		if res.Exit != 2 || !strings.Contains(res.Stderr, paneRefusal) {
			t.Errorf("%s: got %+v", s, res)
		}
	}
}

func TestAPublicOrConfigFileIsLeftToTheEnvelope(t *testing.T) {
	root, env := setupAllowAll(t)
	for _, pth := range coppicePublicFiles {
		path := testDataDir + "/" + pth
		// The Read tool: nothing else guards this path, so an actual
		// allow proves the pane rule left it alone, not merely that it
		// did not happen to name it.
		p, _ := json.Marshal(map[string]any{"session_id": "s1", "tool_name": "Read", "cwd": "/work",
			"tool_input": map[string]string{"file_path": path}})
		res := run(t, root, env, "enforce", string(p))
		if !res.Native || res.Exit != 0 {
			t.Errorf("%s: got %+v", pth, res)
		}
		// A shell cat of anything under the coppice data directory is
		// still denied, by the floor rule (this directory is the
		// floor's own), not by the pane rule. Check the reason names
		// the right rule.
		p2, _ := json.Marshal(map[string]any{"session_id": "s1", "tool_name": "Bash", "cwd": "/work",
			"tool_input": map[string]string{"command": "cat " + path}})
		res2 := run(t, root, env, "enforce", string(p2))
		if strings.Contains(res2.Stderr, paneRefusal) {
			t.Errorf("%s: wrongly caught by the pane rule: %+v", pth, res2)
		}
	}
}

// secretRespellings is the Go twin of the Python test's _respellings: a
// few spellings of one secret's relative path that name the same file, a
// doubled slash, a bare `.` segment, and a bogus segment a `..` undoes,
// each right before the file name.
func secretRespellings(secret string) []string {
	i := strings.LastIndex(secret, "/")
	head, name := secret[:i], secret[i+1:]
	return []string{
		head + "//" + name,
		head + "/./" + name,
		head + "/bogus/../" + name,
	}
}

func TestARespelledReadOfEveryCoppiceSecretIsStillDenied(t *testing.T) {
	root, env := setupAllowAll(t)
	for _, s := range coppiceSecrets {
		for _, spelling := range secretRespellings(s) {
			path := testDataDir + "/" + spelling
			p, _ := json.Marshal(map[string]any{"session_id": "s1", "tool_name": "Read", "cwd": "/work",
				"tool_input": map[string]string{"file_path": path}})
			res := run(t, root, env, "enforce", string(p))
			if res.Exit != 2 || !strings.Contains(res.Stderr, paneRefusal) {
				t.Errorf("%s (%s): got %+v", s, spelling, res)
			}
		}
	}
}

func TestARespelledMCPArgumentNamingACoppiceSecretIsDenied(t *testing.T) {
	root, env := setupAllowAll(t)
	for _, s := range coppiceSecrets {
		for _, spelling := range secretRespellings(s) {
			path := testDataDir + "/" + spelling
			p, _ := json.Marshal(map[string]any{"session_id": "s1", "tool_name": "mcp__filesystem__read_file",
				"cwd": "/work", "tool_input": map[string]string{"path": path}})
			res := run(t, root, env, "enforce", string(p))
			if res.Exit != 2 || !strings.Contains(res.Stderr, paneRefusal) {
				t.Errorf("%s (%s): got %+v", s, spelling, res)
			}
		}
	}
}

// The typed path names no secret by its own text (it goes through
// "link", not "web/ca"). Only the resolved spelling does, so this is the
// one case that needs pathNamesSecret's resolve() candidate, not just
// its normpath() one.
func TestASymlinkedPathToASecretIsStillDenied(t *testing.T) {
	root, env := setupAllowAll(t)
	dir := t.TempDir()
	realDir := filepath.Join(dir, "data", "web", "ca")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "ca.key"), []byte("shh"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	p, _ := json.Marshal(map[string]any{"session_id": "s1", "tool_name": "Read", "cwd": "/work",
		"tool_input": map[string]string{"file_path": filepath.Join(link, "ca.key")}})
	res := run(t, root, env, "enforce", string(p))
	if res.Exit != 2 || !strings.Contains(res.Stderr, paneRefusal) {
		t.Errorf("got %+v", res)
	}
}

// A deny is not an allow: the pane rule leaves every spelling of a deny
// to the envelope and to coppice-server, which lets a pane deny only an
// ask it holds. Every spelling of an allow is still the pane refusal.
func TestAPaneMayDenyButNeverAllow(t *testing.T) {
	root, env := setupAllowAll(t)
	shell := func(cmd string) Result {
		p, _ := json.Marshal(map[string]any{"session_id": "s1", "tool_name": "Bash", "cwd": "/work",
			"tool_input": map[string]string{"command": cmd}})
		return run(t, root, env, "enforce", string(p))
	}
	for _, cmd := range []string{
		"coppice agent deny w1:p1 ask-3",
		"x && coppice agent deny w1:p1 h1 --reason 'not this one'",
		`echo '{"cmd": "agent.deny"}' | nc -U s`,
	} {
		if res := shell(cmd); strings.Contains(res.Stderr, paneRefusal) {
			t.Errorf("%s: the pane rule refused a deny: %+v", cmd, res)
		}
	}
	for _, cmd := range []string{
		"coppice agent allow w1:p1 ask-3",
		"coppice --socket /x agent allow 42",
		`echo '{"cmd": "agent.allow"}' | nc -U s`,
	} {
		if res := shell(cmd); res.Exit != 2 || !strings.Contains(res.Stderr, paneRefusal) {
			t.Errorf("%s: want the pane refusal, got %+v", cmd, res)
		}
	}
}

func shellPayload(cmd string) string {
	p, _ := json.Marshal(map[string]any{"session_id": "s1", "tool_name": "Bash", "cwd": "/work",
		"tool_input": map[string]string{"command": cmd}})
	return string(p)
}

// The server knows a pane only by the caller's process tree and session.
// A deny sent through another server or socket, or under a wrapper that
// starts it outside both, reads as the operator's, so the pane rule
// refuses it.
func TestADenyFromOutsideThePaneIsDenied(t *testing.T) {
	root, env := setupAllowAll(t)
	for _, cmd := range []string{
		"coppice --remote ssh://box agent deny w1:p1 ask-3",
		"coppice --remote=ssh://box agent deny w1:p1 ask-3",
		"x && coppice --socket /run/s.sock agent deny w1:p1 h1",
		"XDG_RUNTIME_DIR=/x coppice agent deny w1:p1 h1",
		"ssh box coppice agent deny w1:p1 h1",
		"ssh box 'coppice agent deny w1:p1 h1'",
		"setsid -f coppice agent deny w1:p1 h1",
		"/usr/bin/setsid coppice agent deny w1:p1 h1",
		"systemd-run --user coppice agent deny w1:p1 h1",
		"nohup coppice agent deny w1:p1 h1",
		"echo 'coppice agent deny w1:p1 h1' | at now",
		"echo 'coppice agent deny w1:p1 h1' | batch",
		"tmux send-keys -t other 'coppice agent deny w1:p1 h1' Enter",
		"screen -dm coppice agent deny w1:p1 h1",
		"script -qc 'coppice agent deny w1:p1 h1' /dev/null",
		"sudo -u me coppice agent deny w1:p1 h1",
		"env -i A=1 setsid coppice agent deny w1:p1 h1",
		`echo '{"cmd": "agent.deny"}' | ssh box nc -U s`,
	} {
		if res := run(t, root, env, "enforce", shellPayload(cmd)); res.Exit != 2 || !strings.Contains(res.Stderr, paneRefusal) {
			t.Errorf("%s: want the pane refusal, got %+v", cmd, res)
		}
	}
	for _, cmd := range []string{"ssh box ls", "nohup make build", "coppice --remote ssh://box agent list"} {
		if res := run(t, root, env, "enforce", shellPayload(cmd)); strings.Contains(res.Stderr, paneRefusal) {
			t.Errorf("%s: the pane rule refused a line with no deny: %+v", cmd, res)
		}
	}
}

func searchPayload(tool, path, cwd string) string {
	in := map[string]string{"pattern": "."}
	if path != "" {
		in["path"] = path
	}
	p, _ := json.Marshal(map[string]any{"session_id": "s1", "tool_name": tool, "cwd": cwd, "tool_input": in})
	return string(p)
}

// secretDirs are the directories, under a coppice data directory, that
// hold a secret: the data directory itself and each one on the way down.
var secretDirs = []string{"", "/web", "/web/ca", "/web/tls", "/voice"}

// A search tool reads every file under its path, or under its cwd when it
// names no path, so a search of a directory that holds a secret reads it.
func TestAGrepOfASecretDirectoryIsDenied(t *testing.T) {
	root, env := setupAllowAll(t)
	for _, sub := range secretDirs {
		for _, p := range []string{
			searchPayload("Grep", testDataDir+sub, "/work"),
			searchPayload("Grep", "", testDataDir+sub),
		} {
			if res := run(t, root, env, "enforce", p); res.Exit != 2 || !strings.Contains(res.Stderr, paneRefusal) {
				t.Errorf("%s: want the pane refusal, got %+v", p, res)
			}
		}
	}
	for _, spelling := range []string{"/web/", "/web//", "/web/./", "/web/bogus/.."} {
		p := searchPayload("Grep", testDataDir+spelling, "/work")
		if res := run(t, root, env, "enforce", p); res.Exit != 2 || !strings.Contains(res.Stderr, paneRefusal) {
			t.Errorf("%s: want the pane refusal, got %+v", spelling, res)
		}
	}
}

// Glob prints names only, and Read opens the one file its path names, so
// neither is refused for its cwd or for listing a secret's directory. An
// unrelated web directory is left alone too.
func TestNamesOnlyAndUnrelatedReadsAreLeftToTheEnvelope(t *testing.T) {
	root, env := setupAllowAll(t)
	read, _ := json.Marshal(map[string]any{"session_id": "s1", "tool_name": "Read", "cwd": testDataDir + "/web",
		"tool_input": map[string]string{"file_path": testDataDir + "/web/web.json"}})
	for _, p := range []string{
		searchPayload("Glob", testDataDir+"/web", "/work"),
		searchPayload("Glob", "", testDataDir+"/web"),
		searchPayload("Grep", "/work/src/web", "/work"),
		searchPayload("Grep", "", "/work/web"),
		string(read),
	} {
		if res := run(t, root, env, "enforce", p); strings.Contains(res.Stderr, paneRefusal) {
			t.Errorf("%s: the pane rule refused it: %+v", p, res)
		}
	}
}

// COPPICE_DATA_DIR names a coppice server's own --data-dir. Its secrets
// and the directories that hold them are guarded like the default ones,
// and the floor rule's cd tracking covers a shell respelling there.
func TestACustomDataDirIsGuardedWhenCoppiceNamesIt(t *testing.T) {
	root, env := setupAllowAll(t)
	const custom = "/srv/elsewhere/cdata"
	named := append(append([]string(nil), env...), "COPPICE_DATA_DIR="+custom)
	p := searchPayload("Grep", custom+"/web", "/work")
	if res := run(t, root, named, "enforce", p); res.Exit != 2 || !strings.Contains(res.Stderr, paneRefusal) {
		t.Errorf("grep: want the pane refusal, got %+v", res)
	}
	if res := run(t, root, env, "enforce", p); strings.Contains(res.Stderr, paneRefusal) {
		t.Errorf("grep with no COPPICE_DATA_DIR: got %+v", res)
	}
	sh := shellPayload("cd " + custom + "/web && cat ./token")
	if res := run(t, root, named, "enforce", sh); res.Exit != 2 {
		t.Errorf("shell respelling: want a deny, got %+v", res)
	}
	if res := run(t, root, env, "enforce", sh); res.Exit == 2 && strings.Contains(res.Stderr, floorRefusal) {
		t.Errorf("shell respelling with no COPPICE_DATA_DIR: got %+v", res)
	}
}

// A relative COPPICE_DATA_DIR names no data directory, as in the oracle.
func TestARelativeCoppiceDataDirNamesNothing(t *testing.T) {
	root, env := setupAllowAll(t)
	named := append(append([]string(nil), env...), "COPPICE_DATA_DIR=cdata")
	if res := run(t, root, named, "enforce", searchPayload("Grep", "/work", "/work")); !res.Native || res.Exit != 0 {
		t.Errorf("want a native allow, got %+v", res)
	}
}

// A recursive search rooted above a data directory reads every secret
// under it, so it is refused with the search refusal. Glob, a Read, and a
// search with a narrow path are left alone.
func TestASearchAboveTheDataDirIsDenied(t *testing.T) {
	root, env := setupAllowAll(t)
	for _, p := range []string{
		searchPayload("Grep", "/home/user", "/work"),
		searchPayload("Grep", "~", "/work"),
		searchPayload("Grep", "/", "/work"),
		searchPayload("Grep", "//", "/work"),
		searchPayload("Grep", "/home/user/.opendaisugi", "/work"),
		searchPayload("Grep", "", "/home/user"),
		shellPayload("rg token ~"),
		shellPayload("grep -r token ~"),
		shellPayload("cd ~ && rg token"),
		shellPayload("find ~ -type f -exec cat"),
		shellPayload("FOO=1 rg token ~/.opendaisugi"),
	} {
		if res := run(t, root, env, "enforce", p); res.Exit != 2 || !strings.Contains(res.Stderr, searchRefusal) {
			t.Errorf("%s: want the search refusal, got %+v", p, res)
		}
	}
	for _, p := range []string{
		searchPayload("Grep", "/work/src", "/home/user"),
		searchPayload("Glob", "/home/user", "/work"),
		searchPayload("Glob", "", "/"),
		shellPayload("find ~ -name x"),
		shellPayload("grep token ~/notes.txt"),
	} {
		if res := run(t, root, env, "enforce", p); strings.Contains(res.Stderr, searchRefusal) {
			t.Errorf("%s: the search refusal fired: %+v", p, res)
		}
	}
	named := append(append([]string(nil), env...), "COPPICE_DATA_DIR=/srv/elsewhere/cdata")
	if res := run(t, root, named, "enforce", searchPayload("Grep", "/srv/elsewhere", "/work")); !strings.Contains(res.Stderr, searchRefusal) {
		t.Errorf("custom data dir parent: got %+v", res)
	}
}

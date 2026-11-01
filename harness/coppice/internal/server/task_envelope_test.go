package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/toolchain"
)

// fakeTreeDaisugi is a stand-in for daisugi that serves the fence (it runs
// what follows "--"), tree check (it refuses a child envelope that names rm
// with daisugi's own wording), and tree root and tree spawn (they log their
// argv). Every call is logged.
const fakeTreeDaisugi = `#!/bin/sh
echo "$*" >> "$FAKE_TREE_LOG"
case "$1 $2" in
"tree check")
  if grep -q '"rm"' "$4"; then echo "child allows shell head rm, which the parent does not"; exit 1; fi
  exit 0 ;;
"tree root"|"tree spawn") exit 0 ;;
esac
if [ "$1" = "fence" ]; then
  while [ "$1" != "--" ]; do shift; done
  shift
  exec "$@"
fi
exit 0
`

// fakeClaude records the arguments it was started with and stays alive.
const fakeClaude = `#!/bin/sh
echo "$@" > "$FAKE_CLAUDE_OUT.$$"
sleep 600
`

type pinFixture struct {
	s      *Server
	log    string
	claude string
	outs   string
}

func newPinFixture(t *testing.T) *pinFixture {
	t.Helper()
	toolchain.RequireOrSkip(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "daisugi")
	if err := os.WriteFile(bin, []byte(fakeTreeDaisugi), 0o755); err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(dir, "claude-fake.sh")
	if err := os.WriteFile(claude, []byte(fakeClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &pinFixture{log: filepath.Join(dir, "log"), claude: claude, outs: filepath.Join(dir, "claude-out")}
	t.Setenv("COPPICE_DAISUGI_BIN", bin)
	t.Setenv("COPPICE_DAISUGI_DATA_DIR", filepath.Join(dir, "dd"))
	t.Setenv("FAKE_TREE_LOG", f.log)
	t.Setenv("FAKE_CLAUDE_OUT", f.outs)
	f.s = newTestServer(t)
	f.s.RegisterPaneCommands()
	f.s.RegisterTaskCommands()
	armWarden(t, f.s)
	return f
}

func (f *pinFixture) calls(t *testing.T) []string {
	t.Helper()
	b, _ := os.ReadFile(f.log)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// claudeArgs waits for the fake claude of the n-th pane and returns what it was started with.
func (f *pinFixture) claudeArgs(t *testing.T, n int) []string {
	t.Helper()
	var out []string
	for i := 0; i < 150; i++ {
		m, _ := filepath.Glob(f.outs + ".*")
		out = out[:0]
		for _, p := range m {
			b, _ := os.ReadFile(p)
			out = append(out, string(b))
		}
		if len(out) >= n {
			return out
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the fake claude never started")
	return nil
}

func (f *pinFixture) newPane(t *testing.T, task string) string {
	t.Helper()
	got := roundTrip(t, f.s, `{"id":"p","cmd":"pane.create","cwd":"/","harness":"claude","fence":"on","kind":"pty","cmd_argv":["`+f.claude+`"],"task":"`+task+`"}`)
	m := result(t, got[0])
	id, _ := m["pane"].(string)
	t.Cleanup(func() { closePane(t, f.s, id) })
	return id
}

const envRoot = `{"id":"env_root","generated_by":"t","task":"t","permissions":{"shell_allowlist":["git","ls"]}}`
const envWide = `{"id":"env_wide","generated_by":"t","task":"t","permissions":{"shell_allowlist":["git","ls","rm"]}}`
const envNarrow = `{"id":"env_narrow","generated_by":"t","task":"t","permissions":{"shell_allowlist":["git"]}}`

func TestTaskCreateProvesTheEdgeAgainstTheParent(t *testing.T) {
	f := newPinFixture(t)
	root, _ := createTask(t, f.s, `"label":"root","envelope":`+envRoot)
	got := roundTrip(t, f.s, `{"id":"1","cmd":"task.create","label":"child","parent":"`+root+`","envelope":`+envWide+`}`)
	if got[0].OK || !strings.Contains(got[0].Error.Message, "shell head rm") ||
		!strings.Contains(got[0].Error.Message, "does not fit inside its parent's") {
		t.Fatalf("a wider child: %+v", got[0])
	}
	if got[0].Error.Code != "bad_request" {
		t.Errorf("code = %q, want bad_request", got[0].Error.Code)
	}
	child, _ := createTask(t, f.s, `"label":"child","parent":"`+root+`","envelope":`+envNarrow)
	tk, _ := f.s.tree.Task(child)
	if !strings.Contains(string(tk.Envelope), "env_narrow") {
		t.Errorf("the task does not carry its envelope: %q", tk.Envelope)
	}
	if listTasks(t, f.s)[child]["envelope"] != true {
		t.Errorf("task.list does not say the task has an envelope")
	}
	// A task with no envelope under one that has it adds no proof.
	before := len(f.calls(t))
	createTask(t, f.s, `"label":"plain","parent":"`+root+`"`)
	if len(f.calls(t)) != before {
		t.Errorf("a child with no envelope ran daisugi")
	}
	// Both an envelope and a project is a mistake.
	got = roundTrip(t, f.s, `{"id":"1","cmd":"task.create","label":"x","envelope":`+envRoot+`,"project":"p"}`)
	if got[0].OK || got[0].Error.Code != "bad_request" {
		t.Errorf("envelope and project: %+v", got[0])
	}
	got = roundTrip(t, f.s, `{"id":"1","cmd":"task.create","label":"x","envelope":[1]}`)
	if got[0].OK || got[0].Error.Code != "bad_request" {
		t.Errorf("an envelope that is not an object: %+v", got[0])
	}
}

func TestOnlyTheOperatorMakesARootTaskWithAnEnvelope(t *testing.T) {
	f := newPinFixture(t)
	send, next, stop := stream(t, f.s)
	defer stop()
	send(`{"id":"0","cmd":"hello","role":"pane","pane":"w1:p1"}`)
	if r := next(); r["ok"] != true {
		t.Fatalf("hello: %v", r)
	}
	send(`{"id":"1","cmd":"task.create","label":"sneak","envelope":` + envWide + `}`)
	r := next()
	e, _ := r["error"].(map[string]any)
	if r["ok"] != false || e["code"] != "unauthorized" || e["message"] != TaskRefusal {
		t.Fatalf("a pane made a root task with an envelope: %v", r)
	}
	// A pane may make a task with no envelope, as before.
	send(`{"id":"2","cmd":"task.create","label":"plain"}`)
	r = next()
	if r["ok"] != true {
		t.Fatalf("a plain task: %v", r)
	}
	// Under a plain task nothing is above it: the envelope would be a root
	// too, and a pane may not make one there either.
	plain, _ := r["result"].(map[string]any)["task"].(string)
	send(`{"id":"3","cmd":"task.create","label":"sneak2","parent":"` + plain + `","envelope":` + envWide + `}`)
	r = next()
	e, _ = r["error"].(map[string]any)
	if r["ok"] != false || e["code"] != "unauthorized" || e["message"] != TaskRefusal {
		t.Fatalf("a pane gave a task under a plain task an envelope: %v", r)
	}
	for _, tk := range f.s.tree.Tasks() {
		if tk.Label == "sneak2" {
			t.Fatalf("the refused task exists: %+v", tk)
		}
	}
}

func TestAPinnedPaneRunsUnderItsTasksMintedSession(t *testing.T) {
	f := newPinFixture(t)
	root, _ := createTask(t, f.s, `"label":"root","envelope":`+envRoot)
	child, _ := createTask(t, f.s, `"label":"child","parent":"`+root+`","envelope":`+envNarrow)
	f.newPane(t, child)
	args := f.claudeArgs(t, 1)
	if len(args) != 1 || !strings.Contains(args[0], "--session '"+child+"-") {
		t.Fatalf("the pane's hook is not pinned to the child's session: %q", args)
	}
	// The root was registered first (tree root), then the child under it
	// (tree spawn --parent), each with a minted t<id>-<16 hex> session.
	var rootCall, spawnCall string
	for _, c := range f.calls(t) {
		if strings.HasPrefix(c, "tree root ") {
			rootCall = c
		}
		if strings.HasPrefix(c, "tree spawn ") {
			spawnCall = c
		}
	}
	tkRoot, _ := f.s.tree.Task(root)
	tkChild, _ := f.s.tree.Task(child)
	if len(tkChild.Session) != len(child)+1+16 || !strings.HasPrefix(tkChild.Session, child+"-") {
		t.Errorf("the session is %q, want %s-<16 hex>", tkChild.Session, child)
	}
	if !strings.Contains(rootCall, "--session "+tkRoot.Session) || !strings.Contains(rootCall, "--root "+f.s.cfg.WardenRoot) {
		t.Errorf("tree root call = %q", rootCall)
	}
	if !strings.Contains(spawnCall, "--parent "+tkRoot.Session) || !strings.Contains(spawnCall, "--session "+tkChild.Session) {
		t.Errorf("tree spawn call = %q", spawnCall)
	}
	// A second pane in the same task shares the session: no new call.
	f.newPane(t, child)
	f.claudeArgs(t, 2)
	trees := 0
	for _, c := range f.calls(t) {
		if strings.HasPrefix(c, "tree root ") || strings.HasPrefix(c, "tree spawn ") {
			trees++
		}
	}
	if trees != 2 {
		t.Errorf("a second pane registered again: %d tree calls, want 2", trees)
	}
	// A task with no envelope of its own takes its parent's session.
	plain, _ := createTask(t, f.s, `"label":"plain","parent":"`+root+`"`)
	f.newPane(t, plain)
	if got := f.claudeArgs(t, 3); !strings.Contains(strings.Join(got, "\n"), "--session '"+tkRoot.Session+"'") {
		t.Errorf("an inheriting pane is not pinned to the root's session: %q", got)
	}
}

func TestAPaneThatCannotBePinnedDoesNotStart(t *testing.T) {
	f := newPinFixture(t)
	root, _ := createTask(t, f.s, `"label":"root","envelope":`+envRoot)
	got := roundTrip(t, f.s, `{"id":"p","cmd":"pane.create","cwd":"/","kind":"pty","cmd_argv":["sh","-c","sleep 30"],"task":"`+root+`"}`)
	if got[0].OK || !strings.Contains(got[0].Error.Message, "only a fenced claude pane can be pinned") {
		t.Fatalf("a shell pane in a task with an envelope: %+v", got[0])
	}
	if len(f.s.tree.Panes()) != 0 {
		t.Errorf("a refused pane left a record")
	}
}

func TestAPaneStartsPanesOnlyUnderItsOwnTask(t *testing.T) {
	f := newPinFixture(t)
	t1, _ := createTask(t, f.s, `"label":"lead","envelope":`+envRoot)
	t2, _ := createTask(t, f.s, `"label":"a","parent":"`+t1+`","envelope":`+envNarrow)
	t3, _ := createTask(t, f.s, `"label":"b","parent":"`+t1+`","envelope":`+envNarrow)
	t4, _ := createTask(t, f.s, `"label":"a1","parent":"`+t2+`","envelope":`+envNarrow)
	p := f.newPane(t, t2)
	send, next, stop := stream(t, f.s)
	defer stop()
	send(`{"id":"0","cmd":"hello","role":"pane","pane":"` + p + `"}`)
	if r := next(); r["ok"] != true {
		t.Fatalf("hello: %v", r)
	}
	create := func(task string) map[string]any {
		send(`{"id":"c","cmd":"pane.create","cwd":"/","harness":"claude","fence":"on","kind":"pty","cmd_argv":["` + f.claude + `"],"task":"` + task + `"}`)
		return next()
	}
	r := create(t3)
	e, _ := r["error"].(map[string]any)
	if r["ok"] != false || e["code"] != "unauthorized" || e["message"] != OwnTaskRefusal {
		t.Fatalf("a sibling task: %v", r)
	}
	if r := create(""); r["ok"] != false {
		t.Errorf("no task at all: %v", r)
	}
	for _, task := range []string{t2, t4} {
		r := create(task)
		if r["ok"] != true {
			t.Fatalf("task %s: %v", task, r)
		}
		res, _ := r["result"].(map[string]any)
		id, _ := res["pane"].(string)
		t.Cleanup(func() { closePane(t, f.s, id) })
	}
	// split goes through the same door.
	send(`{"id":"s","cmd":"pane.split","cwd":"/","harness":"claude","fence":"on","kind":"pty","cmd_argv":["` + f.claude + `"],"task":"` + t3 + `"}`)
	if r := next(); r["ok"] != false {
		t.Errorf("split into a sibling: %v", r)
	}
}

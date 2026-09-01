package server

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/proto"
)

// gitRepo makes a repo on branch main with one commit, inside t.TempDir().
// The test skips when git is not on PATH.
func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "test@example.invalid")
	run("config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(repo, "README"), []byte("hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "README")
	run("commit", "-q", "-m", "first")
	return repo
}

// createTask sends task.create and returns the task id and worktree path.
func createTask(t *testing.T, s *Server, fields string) (string, string) {
	t.Helper()
	got := roundTrip(t, s, `{"id":"1","cmd":"task.create",`+fields+`}`)
	m := result(t, got[0])
	id, _ := m["task"].(string)
	wt, _ := m["worktree"].(string)
	if id == "" {
		t.Fatalf("task.create result = %v, want a task id", m)
	}
	return id, wt
}

// listTasks returns the task.list rows keyed by id.
func listTasks(t *testing.T, s *Server) map[string]map[string]any {
	t.Helper()
	got := roundTrip(t, s, `{"id":"1","cmd":"task.list"}`)
	m := result(t, got[0])
	rows, _ := m["tasks"].([]any)
	out := map[string]map[string]any{}
	for _, r := range rows {
		row, _ := r.(map[string]any)
		id, _ := row["id"].(string)
		out[id] = row
	}
	return out
}

func TestTaskCreateWithWorktreeAnswersThePath(t *testing.T) {
	s := newPaneServer(t)
	repo := gitRepo(t)
	sub := filepath.Join(repo, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	id, wt := createTask(t, s, `"label":"gate-refactor","cwd":"`+sub+`","worktree":true`)
	if id != "t1" {
		t.Fatalf("task id = %q, want t1", id)
	}
	if want := filepath.Join(repo+"-worktrees", "gate-refactor"); wt != want {
		t.Fatalf("worktree = %q, want %q", wt, want)
	}
	if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
		t.Fatal("no worktree on disk")
	}
	rows := listTasks(t, s)
	if rows[id]["worktree"] != wt || rows[id]["label"] != "gate-refactor" || rows[id]["state"] != "" {
		t.Fatalf("task.list row = %v", rows[id])
	}
	if _, ok := rows[id]["ahead"]; !ok {
		t.Fatalf("task.list row has no ahead count: %v", rows[id])
	}
}

func TestTaskCreateWithoutWorktreeKeepsNoPath(t *testing.T) {
	s := newPaneServer(t)
	id, wt := createTask(t, s, `"label":"plain","cwd":"/","model":"opus"`)
	if wt != "" {
		t.Fatalf("worktree = %q, want none", wt)
	}
	child, _ := createTask(t, s, `"label":"child","parent":"`+id+`"`)
	rows := listTasks(t, s)
	if rows[id]["cwd"] != "/" || rows[id]["model"] != "opus" {
		t.Fatalf("task row = %v", rows[id])
	}
	if _, ok := rows[id]["ahead"]; ok {
		t.Fatalf("a task with no worktree reports ahead: %v", rows[id])
	}
	if rows[child]["parent"] != id {
		t.Fatalf("child row = %v", rows[child])
	}
}

func TestTaskCreateRefusesOutsideARepoAndBadLabels(t *testing.T) {
	s := newPaneServer(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	plain := t.TempDir()
	got := roundTrip(t, s, `{"id":"1","cmd":"task.create","label":"x","cwd":"`+plain+`","worktree":true}`)
	if got[0].OK || got[0].Error.Code != proto.ErrBadRequest {
		t.Fatalf("got %+v, want bad_request", got[0])
	}
	if want := "worktree needs a git repo. " + plain + " is not inside one."; got[0].Error.Message != want {
		t.Fatalf("message = %q, want %q", got[0].Error.Message, want)
	}
	repo := gitRepo(t)
	got = roundTrip(t, s, `{"id":"2","cmd":"task.create","label":"Bad Name","cwd":"`+repo+`","worktree":true}`)
	if got[0].OK || got[0].Error.Code != proto.ErrBadRequest || !strings.Contains(got[0].Error.Message, "lowercase") {
		t.Fatalf("got %+v, want the teaching error", got[0])
	}
	got = roundTrip(t, s, `{"id":"3","cmd":"task.create","label":"x","worktree":true}`)
	if got[0].OK || got[0].Error.Code != proto.ErrBadRequest || !strings.Contains(got[0].Error.Message, "cwd") {
		t.Fatalf("got %+v, want bad_request naming cwd", got[0])
	}
	got = roundTrip(t, s, `{"id":"4","cmd":"task.create","cwd":"/"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrBadRequest || !strings.Contains(got[0].Error.Message, "label") {
		t.Fatalf("got %+v, want bad_request naming label", got[0])
	}
	got = roundTrip(t, s, `{"id":"5","cmd":"task.create","label":"x","parent":"t9"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrBadRequest || !strings.Contains(got[0].Error.Message, "t9") {
		t.Fatalf("got %+v, want bad_request naming the parent", got[0])
	}
	if len(listTasks(t, s)) != 0 {
		t.Fatal("a refused task was stored")
	}
}

func TestAPaneBornIntoATaskRunsInItsWorktree(t *testing.T) {
	s := newPaneServer(t)
	repo := gitRepo(t)
	id, wt := createTask(t, s, `"label":"x","cwd":"`+repo+`","worktree":true`)
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","task":"`+id+`","cmd_argv":["sh","-c","sleep 30"],"kind":"pty"}`)
	m := result(t, got[0])
	paneID, _ := m["pane"].(string)
	row := rowFor(t, listPanes(t, s), paneID)
	if row["cwd"] != wt || row["task"] != id {
		t.Fatalf("pane row = %v, want cwd %s and task %s", row, wt, id)
	}
	rows := listTasks(t, s)
	panes, _ := rows[id]["panes"].([]any)
	if len(panes) != 1 || panes[0] != paneID {
		t.Fatalf("task row panes = %v, want [%s]", panes, paneID)
	}
	got = roundTrip(t, s,
		`{"id":"2","cmd":"pane.create","task":"`+id+`","cwd":"/","cmd_argv":["sh"],"kind":"pty"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrBadRequest {
		t.Fatalf("got %+v, want bad_request", got[0])
	}
	if want := "a pane in task " + id + " runs in its worktree " + wt + ". Drop cwd."; got[0].Error.Message != want {
		t.Fatalf("message = %q, want %q", got[0].Error.Message, want)
	}
	got = roundTrip(t, s,
		`{"id":"3","cmd":"pane.create","task":"`+id+`","cwd":"`+wt+`","cmd_argv":["sh","-c","sleep 30"],"kind":"pty"}`)
	if !got[0].OK {
		t.Fatalf("the same cwd as the worktree was refused: %+v", got[0].Error)
	}
}

func TestAPaneInATaskWithoutAWorktreeNeedsACwd(t *testing.T) {
	s := newPaneServer(t)
	cwd := t.TempDir()
	id, _ := createTask(t, s, `"label":"x","cwd":"`+cwd+`"`)
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","task":"`+id+`","cmd_argv":["sh","-c","sleep 30"],"kind":"pty"}`)
	m := result(t, got[0])
	paneID, _ := m["pane"].(string)
	if row := rowFor(t, listPanes(t, s), paneID); row["cwd"] != cwd {
		t.Fatalf("pane row = %v, want the task's cwd %s", row, cwd)
	}
	bare, _ := createTask(t, s, `"label":"bare"`)
	got = roundTrip(t, s,
		`{"id":"2","cmd":"pane.create","task":"`+bare+`","cmd_argv":["sh"],"kind":"pty"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrBadRequest || !strings.Contains(got[0].Error.Message, "cwd") {
		t.Fatalf("got %+v, want bad_request naming cwd", got[0])
	}
	got = roundTrip(t, s,
		`{"id":"3","cmd":"pane.create","task":"t9","cwd":"/","cmd_argv":["sh"],"kind":"pty"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrBadRequest {
		t.Fatalf("got %+v, want bad_request", got[0])
	}
	if want := "no task t9. Run: coppice task list"; got[0].Error.Message != want {
		t.Fatalf("message = %q, want %q", got[0].Error.Message, want)
	}
	if row := rowFor(t, listPanes(t, s), paneID); row["task"] != id {
		t.Fatalf("pane row lost its task: %v", row)
	}
}

func TestTaskStateBubblesFromItsPanes(t *testing.T) {
	s := newPaneServer(t)
	cwd := t.TempDir()
	team, _ := createTask(t, s, `"label":"team"`)
	a, _ := createTask(t, s, `"label":"a","parent":"`+team+`","cwd":"`+cwd+`"`)
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","task":"`+a+`","cmd_argv":["sh","-c","while true; do echo tick; sleep 1; done"],"kind":"pty"}`)
	paneID, _ := result(t, got[0])["pane"].(string)
	waitState(t, s, paneID, "working", 5*time.Second)
	rows := listTasks(t, s)
	if rows[a]["state"] != "working" || rows[team]["state"] != "working" {
		t.Fatalf("before the block: a=%v team=%v", rows[a]["state"], rows[team]["state"])
	}
	deadline := nowSeconds() + 60
	got = roundTrip(t, s, `{"id":"2","cmd":"pane.report_state","pane":"`+paneID+`","event":{`+
		`"state":"blocked","source":"gate","harness":"shell","session_id":"`+paneID+`",`+
		`"ts":1,"v":1,"detail":"","ask":{"id":"a1","tool":"Bash","summary":"rm","deadline":`+
		fmt.Sprintf("%f", deadline)+`}}}`)
	if !got[0].OK {
		t.Fatalf("report_state failed: %+v", got[0].Error)
	}
	rows = listTasks(t, s)
	if rows[a]["state"] != "blocked" || rows[team]["state"] != "blocked" {
		t.Fatalf("after the block: a=%v team=%v", rows[a]["state"], rows[team]["state"])
	}
	if row := rowFor(t, listPanes(t, s), paneID); row["state"] != "blocked" {
		t.Fatalf("pane.list and task.list disagree: pane row = %v", row)
	}
}

func TestTaskCloseWithKeepLeavesTheWorktree(t *testing.T) {
	s := newPaneServer(t)
	repo := gitRepo(t)
	id, wt := createTask(t, s, `"label":"x","cwd":"`+repo+`","worktree":true`)
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","task":"`+id+`","cmd_argv":["sh","-c","sleep 30"],"kind":"pty"}`)
	paneID, _ := result(t, got[0])["pane"].(string)
	got = roundTrip(t, s, `{"id":"2","cmd":"task.close","task":"`+id+`","keep_worktree":true}`)
	m := result(t, got[0])
	if m["task"] != id || m["closed"] != true {
		t.Fatalf("task.close result = %v", m)
	}
	// task.close closes its panes the way an operator pane.close does:
	// the record is removed at once, not marked closed and kept.
	for _, row := range listPanes(t, s) {
		if row["id"] == paneID {
			t.Fatalf("pane row still listed after task.close: %v", row)
		}
	}
	for _, row := range listEndedPanes(t, s) {
		if row["id"] == paneID {
			t.Fatalf("task.close sent its pane to Recent: %v, want it removed outright", row)
		}
	}
	if _, ok := listTasks(t, s)[id]; ok {
		t.Fatal("the task is still listed")
	}
	if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
		t.Fatal("keep_worktree removed the worktree")
	}
	got = roundTrip(t, s, `{"id":"3","cmd":"task.close","task":"`+id+`"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrBadRequest {
		t.Fatalf("a second close = %+v, want bad_request", got[0])
	}
}

func TestTaskCloseRemovesACleanWorktree(t *testing.T) {
	s := newPaneServer(t)
	repo := gitRepo(t)
	team, _ := createTask(t, s, `"label":"team"`)
	id, wt := createTask(t, s, `"label":"x","parent":"`+team+`","cwd":"`+repo+`","worktree":true`)
	got := roundTrip(t, s, `{"id":"1","cmd":"task.close","task":"`+team+`"}`)
	result(t, got[0])
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("the child's worktree is still on disk: %v", err)
	}
	rows := listTasks(t, s)
	if _, ok := rows[id]; ok {
		t.Fatal("the child task is still listed")
	}
	if _, ok := rows[team]; ok {
		t.Fatal("the team is still listed")
	}
}

func TestTaskCloseRefusesADirtyWorktreeAndClosesNothing(t *testing.T) {
	s := newPaneServer(t)
	repo := gitRepo(t)
	team, _ := createTask(t, s, `"label":"team"`)
	id, wt := createTask(t, s, `"label":"x","parent":"`+team+`","cwd":"`+repo+`","worktree":true`)
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","task":"`+id+`","cmd_argv":["sh","-c","sleep 30"],"kind":"pty"}`)
	paneID, _ := result(t, got[0])["pane"].(string)
	if err := os.WriteFile(filepath.Join(wt, "dirty.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got = roundTrip(t, s, `{"id":"2","cmd":"task.close","task":"`+team+`"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrBadRequest {
		t.Fatalf("got %+v, want bad_request", got[0])
	}
	want := "task " + team + " has uncommitted changes in " + wt + ". Commit them, or pass keep_worktree: true."
	if got[0].Error.Message != want {
		t.Fatalf("message = %q, want %q", got[0].Error.Message, want)
	}
	if row := rowFor(t, listPanes(t, s), paneID); row["closed"] != false || row["task"] != id {
		t.Fatalf("a refused close touched the pane: %v", row)
	}
	rows := listTasks(t, s)
	if _, ok := rows[id]; !ok {
		t.Fatal("a refused close removed the task")
	}
	if _, err := os.Stat(filepath.Join(wt, "dirty.txt")); err != nil {
		t.Fatal("a refused close touched the worktree")
	}
}

func TestTaskMoveRefusesACycle(t *testing.T) {
	s := newPaneServer(t)
	a, _ := createTask(t, s, `"label":"a"`)
	b, _ := createTask(t, s, `"label":"b","parent":"`+a+`"`)
	got := roundTrip(t, s, `{"id":"1","cmd":"task.move","task":"`+a+`","parent":"`+b+`"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrBadRequest {
		t.Fatalf("got %+v, want bad_request", got[0])
	}
	got = roundTrip(t, s, `{"id":"2","cmd":"task.move","task":"`+b+`","parent":""}`)
	m := result(t, got[0])
	if m["task"] != b || m["parent"] != "" {
		t.Fatalf("task.move result = %v", m)
	}
	rows := listTasks(t, s)
	if rows[b]["parent"] != "" {
		t.Fatalf("b is still under a: %v", rows[b])
	}
	got = roundTrip(t, s, `{"id":"3","cmd":"task.move","task":"t9","parent":"`+a+`"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrBadRequest {
		t.Fatalf("got %+v, want bad_request", got[0])
	}
}

func TestTasksSurviveASave(t *testing.T) {
	s := newPaneServer(t)
	id, _ := createTask(t, s, `"label":"x","cwd":"/"`)
	raw, err := os.ReadFile(layoutPath(s.cfg.DataDir))
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Tasks map[string]json.RawMessage `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if _, ok := saved.Tasks[id]; !ok {
		t.Fatalf("layout.json has no task %s:\n%s", id, raw)
	}
}

// A worktree that turns dirty after the check and before the removal
// leaves the panes closed and the records in place. The error names the
// way out, and a second close with keep_worktree finishes it.
func TestTaskCloseNamesTheWayOutWhenRemovalFailsAfterPanesClosed(t *testing.T) {
	s := newPaneServer(t)
	repo := gitRepo(t)
	id, wt := createTask(t, s, `"label":"x","cwd":"`+repo+`","worktree":true`)
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","task":"`+id+`","cmd_argv":["sh","-c","sleep 30"],"kind":"pty"}`)
	paneID, _ := result(t, got[0])["pane"].(string)
	s.beforeWorktreeRemove = func(path string) {
		if err := os.WriteFile(filepath.Join(path, "late.txt"), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got = roundTrip(t, s, `{"id":"2","cmd":"task.close","task":"`+id+`"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrInternal {
		t.Fatalf("got %+v, want internal", got[0])
	}
	if !strings.HasSuffix(got[0].Error.Message, "Commit them, or pass keep_worktree: true.") ||
		!strings.Contains(got[0].Error.Message, wt) {
		t.Fatalf("message %q does not name the path and the way out", got[0].Error.Message)
	}
	// The panes already closed (removed, the way an operator close
	// works) before the worktree removal failed; the task record is what
	// stays, so a second close can finish the job.
	for _, row := range listPanes(t, s) {
		if row["id"] == paneID {
			t.Fatalf("pane row still listed after task.close closed its panes: %v", row)
		}
	}
	if _, ok := listTasks(t, s)[id]; !ok {
		t.Fatal("the task record is gone after a failed removal")
	}
	s.beforeWorktreeRemove = nil
	got = roundTrip(t, s, `{"id":"3","cmd":"task.close","task":"`+id+`","keep_worktree":true}`)
	result(t, got[0])
	if _, ok := listTasks(t, s)[id]; ok {
		t.Fatal("keep_worktree did not finish the close")
	}
}

// A task label is cleaned like a pane label: every screen and the tree
// draw it.
func TestTaskCreateCleansItsLabel(t *testing.T) {
	s := newPaneServer(t)
	id, _ := createTask(t, s, `"label":"re\u001b]0;x\u0007view​","cwd":"/"`)
	if got := listTasks(t, s)[id]["label"]; got != "re]0;xview" {
		t.Fatalf("task label = %q, want the control and format characters dropped", got)
	}
	got := roundTrip(t, s, `{"id":"2","cmd":"task.create","label":"\u001b\u0007","cwd":"/"}`)
	if got[0].OK {
		t.Fatalf("a label of nothing but control characters made a task: %+v", got[0])
	}
}

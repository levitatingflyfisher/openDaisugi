package server

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/adapters"
	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/pane"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

// fakeForker is a fakeAdapter that can fork. It starts a fresh fakeProc on
// every Start, because a parent and its fork each need their own event
// stream, and it records every StartOpts it was handed so a test can read
// what the server asked it to resume and with which extra argv.
type fakeForker struct {
	name string

	mu     sync.Mutex
	procs  []*fakeProc
	starts []pane.StartOpts
}

func (a *fakeForker) Name() string { return a.name }

func (a *fakeForker) Start(_ context.Context, o pane.StartOpts, _ *pane.Grid) (pane.Proc, error) {
	p := newFakeProc()
	a.mu.Lock()
	a.procs = append(a.procs, p)
	a.starts = append(a.starts, o)
	a.mu.Unlock()
	return p, nil
}

func (a *fakeForker) ForkArgv(sessionID string) ([]string, error) {
	return []string{"--fork", sessionID}, nil
}

func (a *fakeForker) start(i int) pane.StartOpts {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.starts[i]
}

func (a *fakeForker) proc(i int) *fakeProc {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.procs[i]
}

// endAll ends every stream this adapter started and waits for each pane's
// record to close, so the pump's terminal save runs before the test's data
// directory goes away.
func (a *fakeForker) endAll(t *testing.T, s *Server) {
	t.Helper()
	a.mu.Lock()
	procs := append([]*fakeProc(nil), a.procs...)
	starts := append([]pane.StartOpts(nil), a.starts...)
	a.mu.Unlock()
	for _, p := range procs {
		p.closeIn()
	}
	for _, o := range starts {
		id := o.PaneID
		waitFor(t, 2*time.Second, func() bool {
			info, ok := s.paneInfo(id)
			return ok && info.Closed
		})
	}
}

// createHeadlessPane starts one headless pane on the named harness, with
// the extra argv given.
func createHeadlessPane(t *testing.T, s *Server, harness, label string, argv ...string) string {
	t.Helper()
	quoted := make([]string, 0, len(argv))
	for _, a := range argv {
		quoted = append(quoted, fmt.Sprintf("%q", a))
	}
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","kind":"headless","harness":"`+
			harness+`","label":"`+label+`","env":{"K":"v"},"cmd_argv":[`+strings.Join(quoted, ",")+`]}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}
	id, _ := result(t, got[0])["pane"].(string)
	return id
}

func fork(t *testing.T, s *Server, line string) proto.Response {
	t.Helper()
	return roundTrip(t, s, line)[0]
}

func TestForkOfAShellPaneIsRefusedWithATeachingError(t *testing.T) {
	toolchain.RequireOrSkip(t)
	s := newTestServer(t)
	id := createShellPane(t, s, "sh", "-c", "sleep 30")
	resp := fork(t, s, `{"id":"f","cmd":"pane.fork","pane":"`+id+`"}`)
	if resp.OK || resp.Error.Code != proto.ErrAdapter {
		t.Fatalf("got %+v, want adapter_error", resp)
	}
	if resp.Error.Message != "a pty pane cannot fork a session" {
		t.Fatalf("message = %q", resp.Error.Message)
	}
}

func TestForkWithoutAForkerIsRefusedByHarnessName(t *testing.T) {
	toolchain.RequireOrSkip(t)
	s := newPaneServer(t)
	proc := newFakeProc()
	proc.setSessionID("sess-1")
	adapters.Register(&fakeAdapter{name: "fake-nofork", proc: proc})
	id := createHeadlessPane(t, s, "fake-nofork", "plain")
	t.Cleanup(func() {
		proc.closeIn()
		waitFor(t, 2*time.Second, func() bool {
			info, ok := s.paneInfo(id)
			return ok && info.Closed
		})
	})
	resp := fork(t, s, `{"id":"f","cmd":"pane.fork","pane":"`+id+`"}`)
	if resp.OK || resp.Error.Code != proto.ErrAdapter {
		t.Fatalf("got %+v, want adapter_error", resp)
	}
	if resp.Error.Message != "fake-nofork cannot fork a session yet" {
		t.Fatalf("message = %q", resp.Error.Message)
	}
}

func TestForkBeforeTheAdapterReportsASessionIDIsRefused(t *testing.T) {
	toolchain.RequireOrSkip(t)
	s := newPaneServer(t)
	a := &fakeForker{name: "fake-fork-nosid"}
	adapters.Register(a)
	id := createHeadlessPane(t, s, "fake-fork-nosid", "young")
	t.Cleanup(func() { a.endAll(t, s) })
	resp := fork(t, s, `{"id":"f","cmd":"pane.fork","pane":"`+id+`"}`)
	if resp.OK || resp.Error.Code != proto.ErrAdapter {
		t.Fatalf("got %+v, want adapter_error", resp)
	}
	if resp.Error.Message != "fake-fork-nosid has not reported a session id yet, wait for the first turn" {
		t.Fatalf("message = %q", resp.Error.Message)
	}
	if rows := listPanes(t, s); len(rows) != 1 {
		t.Fatalf("a refused fork left %d panes, want 1", len(rows))
	}
}

func TestForkUsesTheAdaptersResumeCommand(t *testing.T) {
	toolchain.RequireOrSkip(t)
	s := newPaneServer(t)
	a := &fakeForker{name: "fake-fork"}
	adapters.Register(a)
	id := createHeadlessPane(t, s, "fake-fork", "parent", "--model", "x")
	t.Cleanup(func() { a.endAll(t, s) })
	a.proc(0).setSessionID("sess-parent")

	resp := fork(t, s, `{"id":"f","cmd":"pane.fork","pane":"`+id+`"}`)
	if !resp.OK {
		t.Fatalf("pane.fork failed: %+v", resp.Error)
	}
	res := result(t, resp)
	newID, _ := res["pane"].(string)
	if newID == "" || newID == id {
		t.Fatalf("pane.fork returned pane %q", newID)
	}
	if res["parent_pane"] != id {
		t.Fatalf("reply parent_pane = %v, want %s", res["parent_pane"], id)
	}

	row := rowFor(t, listPanes(t, s), newID)
	if row["parent_pane"] != id {
		t.Fatalf("parent not recorded: %v", row)
	}
	if row["label"] != "parent fork" || row["harness"] != "fake-fork" || row["kind"] != "headless" {
		t.Fatalf("the fork did not copy the parent's shape: %v", row)
	}
	parentRow := rowFor(t, listPanes(t, s), id)
	if _, has := parentRow["parent_pane"]; has {
		t.Fatalf("the parent row carries parent_pane: %v", parentRow)
	}
	if row["cwd"] != parentRow["cwd"] || row["workspace"] != parentRow["workspace"] || row["tab"] != parentRow["tab"] {
		t.Fatalf("the fork is not beside its parent: %v vs %v", row, parentRow)
	}

	// The adapter was asked to resume the parent's session with the extra
	// argv it named, and only for this first start.
	child := a.start(1)
	if child.Resume != "sess-parent" {
		t.Fatalf("child Resume = %q, want sess-parent", child.Resume)
	}
	// The parent's own argv first, then the fork argv.
	if strings.Join(child.Argv, " ") != "--model x --fork sess-parent" {
		t.Fatalf("child Argv = %v, want the parent's argv then the fork argv", child.Argv)
	}
	if child.PaneID != newID {
		t.Fatalf("child PaneID = %q, want %s", child.PaneID, newID)
	}
	info, ok := s.paneInfo(newID)
	if !ok {
		t.Fatal("the fork is not live")
	}
	if info.ParentPane != id || info.HarnessSessionID != "sess-parent" || !info.ForkPending {
		t.Fatalf("child record = %+v", info)
	}
	if strings.Join(info.Argv, " ") != "--model x" {
		t.Fatalf("the saved child argv = %v, want the parent's argv only", info.Argv)
	}
	parent, _ := s.paneInfo(id)
	if parent.ForkPending {
		t.Fatal("the parent gained a pending flag")
	}
	// The copies are copies. Changing the child's env and argv in the tree
	// leaves the parent's alone.
	_ = s.updatePane(newID, func(x *layout.Pane) {
		x.Env["FORKED"] = "1"
		x.Argv[0] = "changed"
	})
	parent, _ = s.paneInfo(id)
	if parent.Env["FORKED"] != "" || parent.Env["K"] != "v" || parent.Argv[0] != "--model" {
		t.Fatalf("the parent shares memory with the child: %+v", parent)
	}
}

// waitPane polls paneInfo until cond holds.
func waitPane(t *testing.T, s *Server, id string, cond func(layout.Pane) bool) {
	t.Helper()
	waitFor(t, 2*time.Second, func() bool {
		info, ok := s.paneInfo(id)
		return ok && cond(info)
	})
}

// A fork carries the parent's session id and a pending flag until its
// adapter reports an id of its own. The borrowed id reported back does not
// clear it, because the claude adapter seeds its id from the resume target.
func TestAForkStaysPendingUntilItsAdapterReportsANewSessionID(t *testing.T) {
	toolchain.RequireOrSkip(t)
	s := newPaneServer(t)
	a := &fakeForker{name: "fake-fork-pending"}
	adapters.Register(a)
	id := createHeadlessPane(t, s, "fake-fork-pending", "parent")
	t.Cleanup(func() { a.endAll(t, s) })
	a.proc(0).setSessionID("sess-parent")
	resp := fork(t, s, `{"id":"f","cmd":"pane.fork","pane":"`+id+`"}`)
	if !resp.OK {
		t.Fatalf("pane.fork failed: %+v", resp.Error)
	}
	newID, _ := result(t, resp)["pane"].(string)

	a.proc(1).setSessionID("sess-parent")
	a.proc(1).send(pane.Event{Kind: pane.EvText, Text: "still the parent's id"})
	waitPane(t, s, newID, func(p layout.Pane) bool {
		ev, ok := s.States().Current(newID)
		return ok && ev.Source == proto.SrcHeadless && ev.State == proto.StateWorking
	})
	if info, _ := s.paneInfo(newID); !info.ForkPending || info.HarnessSessionID != "sess-parent" {
		t.Fatalf("the borrowed id cleared the flag: %+v", info)
	}

	a.proc(1).setSessionID("sess-child")
	a.proc(1).send(pane.Event{Kind: pane.EvText, Text: "now my own"})
	waitPane(t, s, newID, func(p layout.Pane) bool {
		return !p.ForkPending && p.HarnessSessionID == "sess-child"
	})
	// The saved layout agrees.
	tree, err := layout.Load(layoutPath(s.cfg.DataDir))
	if err != nil {
		t.Fatal(err)
	}
	if saved, _ := tree.Pane(newID); saved.ForkPending || saved.HarnessSessionID != "sess-child" {
		t.Fatalf("saved child = %+v", saved)
	}
}

func TestAForkOfAPendingForkIsRefused(t *testing.T) {
	toolchain.RequireOrSkip(t)
	s := newPaneServer(t)
	a := &fakeForker{name: "fake-fork-twice"}
	adapters.Register(a)
	id := createHeadlessPane(t, s, "fake-fork-twice", "parent")
	t.Cleanup(func() { a.endAll(t, s) })
	a.proc(0).setSessionID("sess-parent")
	resp := fork(t, s, `{"id":"f","cmd":"pane.fork","pane":"`+id+`"}`)
	if !resp.OK {
		t.Fatalf("pane.fork failed: %+v", resp.Error)
	}
	newID, _ := result(t, resp)["pane"].(string)
	a.proc(1).setSessionID("sess-parent")
	again := fork(t, s, `{"id":"g","cmd":"pane.fork","pane":"`+newID+`"}`)
	if again.OK || again.Error.Code != proto.ErrAdapter {
		t.Fatalf("got %+v, want adapter_error", again)
	}
	if again.Error.Message != "fake-fork-twice has not reported a session id yet, wait for the first turn" {
		t.Fatalf("message = %q", again.Error.Message)
	}
}

// forkThenClose runs one server in dir, records the parent's session id,
// forks it, and closes the server before the fork reports its own id. It
// returns the parent and child ids.
func forkThenClose(t *testing.T, dir, name string) (parent, child string) {
	t.Helper()
	s1, err := New(Config{SocketPath: filepath.Join(dir, "a.sock"), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	a := &fakeForker{name: name}
	adapters.Register(a)
	parent = createHeadlessPane(t, s1, name, "parent", "--model", "x")
	a.proc(0).setSessionID("sess-parent")
	a.proc(0).send(pane.Event{Kind: pane.EvText, Text: "hello"})
	waitPane(t, s1, parent, func(p layout.Pane) bool { return p.HarnessSessionID == "sess-parent" })
	resp := fork(t, s1, `{"id":"f","cmd":"pane.fork","pane":"`+parent+`"}`)
	if !resp.OK {
		t.Fatalf("pane.fork failed: %+v", resp.Error)
	}
	child, _ = result(t, resp)["pane"].(string)
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	return parent, child
}

// A restart before the fork reported its own id repeats the fork: the child
// starts with the parent's id and the fork argv again. The parent resumes
// its own session with no fork argv.
func TestARestoredPendingForkForksAgain(t *testing.T) {
	toolchain.RequireOrSkip(t)
	dir := t.TempDir()
	parent, child := forkThenClose(t, dir, "fake-fork-restore")

	s2, err := New(Config{SocketPath: filepath.Join(dir, "b.sock"), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if err := s2.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	a2 := &fakeForker{name: "fake-fork-restore"}
	adapters.Register(a2)
	if err := s2.Restore(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a2.endAll(t, s2) })
	rep := s2.LastRestore()
	if rep.Resumed != 2 || rep.MarkedUnknown != 0 {
		t.Fatalf("restore report = %+v, want both panes resumed", rep)
	}
	starts := map[string]pane.StartOpts{}
	a2.mu.Lock()
	for _, o := range a2.starts {
		starts[o.PaneID] = o
	}
	a2.mu.Unlock()
	if got := starts[parent]; got.Resume != "sess-parent" || strings.Join(got.Argv, " ") != "--model x" {
		t.Fatalf("parent start = %+v, want a plain resume", got)
	}
	if got := starts[child]; got.Resume != "sess-parent" || strings.Join(got.Argv, " ") != "--model x --fork sess-parent" {
		t.Fatalf("child start = %+v, want the fork repeated", got)
	}
	if info, _ := s2.paneInfo(child); !info.ForkPending {
		t.Fatalf("the restored child lost its pending flag: %+v", info)
	}
}

// When the adapter that served a pending fork cannot fork any more, the
// restore leaves the child closed and says why. It never resumes the
// parent's session in the child's place.
func TestARestoredPendingForkStaysClosedWithoutAForker(t *testing.T) {
	toolchain.RequireOrSkip(t)
	dir := t.TempDir()
	parent, child := forkThenClose(t, dir, "fake-fork-lost")

	s2, err := New(Config{SocketPath: filepath.Join(dir, "b.sock"), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if err := s2.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	proc := newFakeProc()
	adapters.Register(&fakeAdapter{name: "fake-fork-lost", proc: proc})
	if err := s2.Restore(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		proc.closeIn()
		waitPane(t, s2, parent, func(p layout.Pane) bool { return p.Closed })
	})
	rep := s2.LastRestore()
	if rep.Resumed != 1 || rep.MarkedUnknown != 1 {
		t.Fatalf("restore report = %+v, want the parent resumed and the child unknown", rep)
	}
	info, _ := s2.paneInfo(child)
	if _, live := s2.Live(child); live {
		t.Fatalf("the child is live after a restore without a Forker: %+v", info)
	}
	rec, _ := s2.Tree().Pane(child)
	if !rec.Closed {
		t.Fatalf("the child record is open: %+v", rec)
	}
	ev, ok := s2.States().Current(child)
	if !ok || ev.State != proto.StateUnknown || !strings.Contains(ev.Detail, "cannot fork") {
		t.Fatalf("child state = %+v, want unknown with a fork detail", ev)
	}
}

func TestForkTakesTheLabelItIsGiven(t *testing.T) {
	toolchain.RequireOrSkip(t)
	s := newPaneServer(t)
	a := &fakeForker{name: "fake-fork-label"}
	adapters.Register(a)
	id := createHeadlessPane(t, s, "fake-fork-label", "parent")
	t.Cleanup(func() { a.endAll(t, s) })
	a.proc(0).setSessionID("sess-parent")
	resp := fork(t, s, `{"id":"f","cmd":"pane.fork","pane":"`+id+`","label":"branch"}`)
	if !resp.OK {
		t.Fatalf("pane.fork failed: %+v", resp.Error)
	}
	newID, _ := result(t, resp)["pane"].(string)
	if row := rowFor(t, listPanes(t, s), newID); row["label"] != "branch" {
		t.Fatalf("label = %v, want branch", row["label"])
	}
}

func TestForkOfAClosedOrUnknownPaneIsRefusedByCode(t *testing.T) {
	toolchain.RequireOrSkip(t)
	s := newTestServer(t)
	id := createShellPane(t, s, "sh", "-c", "sleep 30")
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.close","pane":"`+id+`"}`,
		`{"id":"2","cmd":"pane.fork","pane":"`+id+`"}`,
		`{"id":"3","cmd":"pane.fork","pane":"w9:p9"}`,
		`{"id":"4","cmd":"pane.fork"}`)
	if got[1].OK || got[1].Error.Code != proto.ErrPaneClosed {
		t.Fatalf("got %+v, want pane_closed", got[1])
	}
	if got[2].OK || got[2].Error.Code != proto.ErrNoSuchPane {
		t.Fatalf("got %+v, want no_such_pane", got[2])
	}
	if got[3].OK || got[3].Error.Code != proto.ErrBadRequest {
		t.Fatalf("got %+v, want bad_request", got[3])
	}
}

func TestNewRegistersPaneFork(t *testing.T) {
	s := newTestServer(t)
	if _, ok := s.handlers["pane.fork"]; !ok {
		t.Fatal("New did not register pane.fork")
	}
}

package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/opendaisugi/coppice/internal/adapters"
	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/pane"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/state"
)

// LivePane joins a snapshot of the record in the tree to the things that only
// exist while the server runs. Info is a COPY: the tree owns the record, and
// changing one goes through updatePane so the tree and this copy never disagree.
type LivePane struct {
	Info    layout.Pane
	Grid    *pane.Grid
	PTY     *pane.PTY
	Adapter pane.Proc

	// started is when startPane built this entry. pane.list measures
	// quiet_for from here until the grid has seen its first byte.
	started time.Time

	// harness is the record's harness at spawn, or "shell" when it had
	// none. The process tick names it on every event it applies. It is set
	// once before putLive and never written again, so the tick goroutine
	// reads it without a lock.
	harness string

	// tickStop ends the process tick goroutine of a pty pane. removeLive
	// closes it through tickOnce, so a second close is a no-op.
	tickStop chan struct{}
	tickOnce sync.Once

	// procState is the last state the process tick applied. Only the tick
	// goroutine, and the one synchronous call startPane makes before that
	// goroutine exists, ever touch it.
	procState string
}

// stopTick ends this pane's process tick goroutine, if it has one.
func (lp *LivePane) stopTick() {
	if lp.tickStop == nil {
		return
	}
	lp.tickOnce.Do(func() { close(lp.tickStop) })
}

// liveMu guards s.live. It is a field, not a package variable: two Server
// values in one test binary must not contend on one global lock.

func (s *Server) Live(id string) (*LivePane, bool) {
	s.liveMu.RLock()
	defer s.liveMu.RUnlock()
	lp, ok := s.live[id]
	return lp, ok
}

func (s *Server) putLive(id string, lp *LivePane) {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	s.live[id] = lp
}

// removeLive drops id's LivePane entirely. Only an operator pane.close does
// this: a pane whose process merely exited keeps its
// entry, and its grid, so its final screen stays readable.
func (s *Server) removeLive(id string) {
	s.liveMu.Lock()
	lp, ok := s.live[id]
	delete(s.live, id)
	s.liveMu.Unlock()
	if ok {
		lp.stopTick()
	}
}

// closeTeardownDeadline bounds how long ONE call to teardownLivePanes waits
// for every live pane its own snapshot found to stop. It is not a total
// budget across a whole Close: Close calls teardownLivePanes twice (see
// below), and each call arms its own closeTeardownDeadline, so a shutdown
// where every pane is unusually slow to die can take up to two of these, not
// one. A stopping server kills what it spawned rather than orphaning it,
// since a restarted daemon cannot resume a pty and must not leave one
// running unmanaged. One shared deadline for every pane a single call is
// tearing down, not one per pane, is what keeps a handful of slow panes
// within one pass from adding up to a shutdown that never finishes.
const closeTeardownDeadline = 2 * time.Second

// teardownLivePanes stops every live pane its own snapshot finds. Close calls
// this twice: once after every client is dropped and once more after
// waitForHandlers, right before the start lock is released. The first pass
// is the ordinary case. The second is a backstop for a handler that was
// already running when isClosed() started refusing new work -
// handlePaneCreate refuses once isClosed() is true, but a pane.create
// dispatched a moment earlier can still reach putLive between the first
// pass's snapshot and its own return. Calling this again after that handler
// has finished (waitForHandlers already waited for it) catches that pane
// too. A pane the first pass already tore down costs the second pass almost
// nothing: PTY.Close and Adapter.Stop are each idempotent, so re-calling
// them on an already-dead pane returns at once rather than waiting out
// another closeTeardownDeadline.
//
// It does not touch layout.json. watchExit and pumpAdapter both check
// isClosed() before they write, so PTY.Close/Adapter.Stop here trigger no
// write of their own: the tree on disk stays exactly as it was at the last
// save. Restore is what marks these records closed on the next start, from
// the process's absence, not from anything this method writes now.
//
// Every pane is torn down concurrently, under one shared deadline, so one
// slow process cannot delay every other pane's shutdown behind it. A pty
// pane's teardown waits on PTY.Done(), not just PTY.Close(): Close only waits
// for the reader goroutine, not for the child to actually be reaped, so
// without this a pane could count as "torn down" while its process is still
// a zombie for a moment longer.
//
// The outer wait below is bounded by closeTeardownDeadline regardless, but
// a single pane's own <-lp.PTY.Done() is not: a child that ignores SIGKILL
// parks that one pane's goroutine on Done() past this whole call's own
// return, until the child finally dies or the server process itself exits.
func (s *Server) teardownLivePanes() {
	s.liveMu.RLock()
	lps := make([]*LivePane, 0, len(s.live))
	for _, lp := range s.live {
		lps = append(lps, lp)
	}
	s.liveMu.RUnlock()
	// Test-only: see afterTeardownSnapshot's own doc comment (server.go).
	// Nil for every real server.
	if hook := s.afterTeardownSnapshot; hook != nil {
		hook()
	}
	if len(lps) == 0 {
		return
	}

	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for _, lp := range lps {
			wg.Add(1)
			go func(lp *LivePane) {
				defer wg.Done()
				if lp.PTY != nil {
					_ = lp.PTY.Close()
					<-lp.PTY.Done()
				}
				if lp.Adapter != nil {
					_ = lp.Adapter.Stop()
				}
			}(lp)
		}
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(closeTeardownDeadline):
	}
}

// refreshLiveInfo re-reads the tree's record for id and, if a LivePane still
// exists for it, replaces its cached Info with the fresh copy, all under one
// hold of liveMu. updatePane and closePane both call this right after they
// change the tree, which is what keeps LivePane.Info from disagreeing with
// it.
//
// The tree read happens WHILE liveMu is held, not before it: two calls racing
// against each other (an updatePane and a closePane, say) each do their own
// tree write and then their own refreshLiveInfo, and it is the LAST of the
// two to acquire liveMu that must win, with a read that reflects both writes.
// Reading the tree before acquiring liveMu lets that ordering invert - the
// last one to acquire the lock can still publish a snapshot it captured
// before the other one's write, leaving Info stale (e.g. Closed still false
// after the tree already says true) until the next refresh happens to catch
// it up, which is this package's only lock nesting: liveMu first, then the
// tree's own lock, via s.tree.Pane below.
func (s *Server) refreshLiveInfo(id string) {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	rec, ok := s.tree.Pane(id)
	if !ok {
		return
	}
	if lp, ok := s.live[id]; ok {
		lp.Info = rec
	}
}

// paneInfo reads a live pane's cached Info under liveMu. LivePane.Info stays
// an exported field, since a struct literal (LivePane{Info: rec, ...})
// builds it directly, so a method literally named Info() on
// *LivePane is impossible (Go forbids a field and method sharing a name);
// this lives on *Server instead, for any caller in this package that wants a
// race-free read instead of touching lp.Info directly - detect.go's
// pane.explain among them.
func (s *Server) paneInfo(id string) (layout.Pane, bool) {
	s.liveMu.RLock()
	defer s.liveMu.RUnlock()
	lp, ok := s.live[id]
	if !ok {
		return layout.Pane{}, false
	}
	return lp.Info, true
}

// updatePane changes a pane's record via a field-level edit (resize and the
// like), refreshes the live snapshot, and persists - all in one place.
// closePane, just below, is its sibling for closing: layout.Tree.ClosePane
// carries its own tested exit-code-copy semantics, so closePane calls that
// directly instead of reimplementing it as a callback, but does the exact
// same refresh-then-persist afterward. Together they are the only two
// callers of Tree.UpdatePane/Tree.ClosePane for a pane that has a LivePane -
// that is what keeps LivePane.Info from ever going stale against the tree.
func (s *Server) updatePane(id string, f func(*layout.Pane)) error {
	if err := s.tree.UpdatePane(id, f); err != nil {
		return err
	}
	s.refreshLiveInfo(id)
	s.saveLayout()
	return nil
}

// closePane is updatePane's counterpart for closing a pane. See updatePane's
// doc comment above for why the two together are the only path to
// Tree.UpdatePane/Tree.ClosePane for a live pane.
func (s *Server) closePane(id string, exit *int) error {
	if err := s.tree.ClosePane(id, exit); err != nil {
		return err
	}
	s.refreshLiveInfo(id)
	s.saveLayout()
	return nil
}

// namedKeys is the closed set pane.send_keys accepts. An unknown name is a
// bad_request that lists what works, rather than a silent no-op.
//
// f1-f12 use the
// ordinary xterm sequences a shell running under a real terminal expects -
// f1-f4 the SS3 form, f5-f12 the CSI tilde form. testdata/keys.json pins
// the full name list; TestNamedKeysVocabularyMatchesTestdataKeysJSON fails
// if this map's keys ever drift from that file.
var namedKeys = map[string]string{
	"enter": "\r", "return": "\r", "tab": "\t", "esc": "\x1b", "escape": "\x1b",
	"space": " ", "backspace": "\x7f", "up": "\x1b[A", "down": "\x1b[B",
	"right": "\x1b[C", "left": "\x1b[D",
	"ctrl+a": "\x01", "ctrl+b": "\x02", "ctrl+c": "\x03", "ctrl+d": "\x04",
	"ctrl+e": "\x05", "ctrl+l": "\x0c", "ctrl+o": "\x0f", "ctrl+r": "\x12",
	"ctrl+u": "\x15", "ctrl+w": "\x17",
	"f1": "\x1bOP", "f2": "\x1bOQ", "f3": "\x1bOR", "f4": "\x1bOS",
	"f5": "\x1b[15~", "f6": "\x1b[17~", "f7": "\x1b[18~", "f8": "\x1b[19~",
	"f9": "\x1b[20~", "f10": "\x1b[21~", "f11": "\x1b[23~", "f12": "\x1b[24~",
}

func keyNames() string {
	out := make([]string, 0, len(namedKeys))
	for k := range namedKeys {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// RegisterPaneCommands wires up every pane, tab, workspace and session verb.
// New calls this itself, before Serve can start, so a caller cannot forget it
// and ship a daemon with no pane verbs; a test calling it again is a harmless
// re-registration (Handle just overwrites the same names), since that also
// always happens before Serve.
func (s *Server) RegisterPaneCommands() {
	_ = s.Handle("workspace.create", s.handleWorkspaceCreate)
	_ = s.Handle("workspace.list", s.handleWorkspaceList)
	_ = s.Handle("tab.create", s.handleTabCreate)
	_ = s.Handle("tab.list", s.handleTabList)
	_ = s.Handle("pane.create", s.handlePaneCreate)
	_ = s.Handle("pane.split", s.handlePaneCreate)
	_ = s.Handle("pane.list", s.handlePaneList)
	_ = s.Handle("pane.send_text", s.handleSendText)
	_ = s.Handle("pane.send_keys", s.handleSendKeys)
	_ = s.Handle("pane.run", s.handleRun)
	_ = s.Handle("pane.read", s.handleRead)
	_ = s.Handle("pane.resize", s.handleResize)
	_ = s.Handle("pane.close", s.handleClose)
	_ = s.Handle("pane.wait_output", s.handleWaitOutput)
	_ = s.Handle("pane.fork", s.handlePaneFork)
	_ = s.Handle("session.list", s.handlePaneList)
	_ = s.Handle("session.stop", s.handleClose)
	_ = s.Handle("events.pause", s.handlePause)
	_ = s.Handle("events.resume", s.handleResume)
}

func (s *Server) handleWorkspaceCreate(_ *Client, r *proto.Request) proto.Response {
	cwd, _ := r.Str("cwd")
	label, _ := r.Str("label")
	ws := s.tree.CreateWorkspace(label, cwd)
	tab, err := s.tree.CreateTab(ws.ID, "main")
	if err != nil {
		return proto.ErrResp(r.ID, proto.ErrInternal, err.Error())
	}
	s.saveLayout()
	return proto.OKResp(r.ID, map[string]any{"workspace": ws.ID, "tab": tab.ID})
}

func (s *Server) handleWorkspaceList(_ *Client, r *proto.Request) proto.Response {
	out := []map[string]any{}
	for _, w := range s.tree.Workspaces() {
		out = append(out, map[string]any{
			"id": w.ID, "label": w.Label, "cwd": w.Cwd, "tabs": len(w.TabIDs),
		})
	}
	return proto.OKResp(r.ID, map[string]any{"workspaces": out})
}

func (s *Server) handleTabCreate(_ *Client, r *proto.Request) proto.Response {
	wsID, ok := r.Str("workspace")
	if !ok {
		wsID, _ = s.tree.Current()
	}
	if wsID == "" {
		return proto.ErrResp(r.ID, proto.ErrNoSuchWorkspace,
			"there is no workspace yet. Run: coppice workspace create --cwd .")
	}
	label, _ := r.Str("label")
	tab, err := s.tree.CreateTab(wsID, label)
	if err != nil {
		return proto.ErrResp(r.ID, proto.ErrNoSuchWorkspace, err.Error())
	}
	s.saveLayout()
	return proto.OKResp(r.ID, map[string]any{"tab": tab.ID, "workspace": wsID})
}

func (s *Server) handleTabList(_ *Client, r *proto.Request) proto.Response {
	wsID, ok := r.Str("workspace")
	if !ok {
		wsID, _ = s.tree.Current()
	}
	tabs, err := s.tree.Tabs(wsID)
	if err != nil {
		return proto.ErrResp(r.ID, proto.ErrNoSuchWorkspace, err.Error())
	}
	out := []map[string]any{}
	for _, tb := range tabs {
		out = append(out, map[string]any{"id": tb.ID, "label": tb.Label, "panes": len(tb.PaneIDs)})
	}
	return proto.OKResp(r.ID, map[string]any{"tabs": out})
}

func (s *Server) handlePaneCreate(_ *Client, r *proto.Request) proto.Response {
	// Close's teardown snapshots s.live once, then again after
	// waitForHandlers as a backstop (teardownLivePanes' own doc comment).
	// Refusing here closes the gap between those two passes at its source:
	// a pane.create dispatched after Close has started must never reach
	// putLive and become a pane the second pass has to catch instead of one
	// that was never let in.
	if s.isClosed() {
		// server_closed, not pane_closed -
		// this refusal is about the server, and pane.create has no pane of
		// its own yet for pane_closed to name.
		return proto.ErrResp(r.ID, proto.ErrServerClosed,
			"this server has closed. It cannot start a new pane.")
	}
	cwd, ok := r.Str("cwd")
	if !ok || cwd == "" {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			"pane.create needs cwd. Pass the directory the pane should start in.")
	}
	kind := layout.KindPTY
	if k, ok := r.Str("kind"); ok {
		switch k {
		case "pty":
			kind = layout.KindPTY
		case "headless":
			kind = layout.KindHeadless
		default:
			return proto.ErrResp(r.ID, proto.ErrBadRequest,
				fmt.Sprintf("kind %q is not pty or headless", k))
		}
	}
	argv, _ := r.StrSlice("cmd_argv")
	harness, _ := r.Str("harness")
	if kind == layout.KindHeadless && harness == "" {
		// "coppice agent list" lists panes, not
		// adapters. adapters.Names() is the real list, the same one
		// startPane itself falls back to naming below.
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			fmt.Sprintf("a headless pane needs harness. Known adapters: %s",
				strings.Join(adapters.Names(), ", ")))
	}
	// A headless pane no longer
	// refuses outright. startPane resolves rec.Harness against the adapters
	// registry instead, and its error - unknown harness, or one registered
	// only as a NotBuilt slot - is mapped to bad_request below, once the
	// pane has a real record to fail against. The harness check above still
	// runs first, so a caller missing harness sees that message instead.
	if kind == layout.KindPTY && len(argv) == 0 {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			"a pty pane needs cmd_argv. Put the command after -- on the command line.")
	}

	wsID, tabID := s.tree.Current()
	if id, ok := r.Str("workspace"); ok {
		wsID = id
	}
	if id, ok := r.Str("tab"); ok {
		tabID = id
	}
	if wsID == "" {
		ws := s.tree.CreateWorkspace("", cwd)
		tab, err := s.tree.CreateTab(ws.ID, "main")
		if err != nil {
			return proto.ErrResp(r.ID, proto.ErrInternal, err.Error())
		}
		wsID, tabID = ws.ID, tab.ID
	}

	cols, ok := r.Int("cols")
	if !ok {
		cols = 120
	}
	rows, ok := r.Int("rows")
	if !ok {
		rows = 40
	}
	env, _ := r.StrMap("env")
	label, _ := r.Str("label")

	rec, err := s.tree.CreatePane(wsID, tabID, layout.Pane{
		Label: label, Cwd: cwd, Argv: argv, Env: env, Kind: kind,
		Harness: harness, Cols: cols, Rows: rows,
	})
	if err != nil {
		// errors.Is, not a string search: layout.CreatePane wraps its own
		// sentinels with %w specifically so a caller can tell them apart
		// this way. A tab id that happens to contain the substring
		// "workspace" must not get misreported as no_such_workspace.
		code := proto.ErrNoSuchTab
		if errors.Is(err, layout.ErrNoWorkspace) {
			code = proto.ErrNoSuchWorkspace
		}
		return proto.ErrResp(r.ID, code, err.Error())
	}

	if err := s.startPane(rec); err != nil {
		// Creation is one step from the caller's side. Nothing ran, so
		// nothing stays: the record leaves the tree and the saved layout,
		// and no live entry or stored state remains to list as a ghost. The
		// pane number is spent regardless, so a later pane never gets an id
		// a caller already saw fail.
		_ = s.tree.RemovePane(rec.ID)
		s.removeLive(rec.ID)
		s.states.Forget(rec.ID)
		s.saveLayout()
		// An unknown harness, or one registered only as a NotBuilt slot, is a
		// client mistake naming something that does not exist or does not
		// work yet - bad_request, not spawn_failed. Every other startPane
		// failure (a pty binary that would not exec, say) stays spawn_failed.
		code := proto.ErrSpawnFailed
		if errors.Is(err, errUnknownHarness) || errors.Is(err, adapters.ErrNotBuilt) ||
			errors.Is(err, errUnknownPaneKind) {
			code = proto.ErrBadRequest
		}
		return proto.ErrResp(r.ID, code, err.Error())
	}
	s.saveLayout()
	return proto.OKResp(r.ID, map[string]any{
		"pane": rec.ID, "workspace": wsID, "tab": tabID,
	})
}

// handlePaneFork starts a new headless pane that continues the session of a
// live one. The parent's adapter must implement pane.Forker and must have
// reported a session id of its own: a parent that is itself a pending fork
// is refused. The child copies the parent's harness, cwd, kind, env, argv,
// size, workspace and tab, records the parent id, borrows the parent's
// session id as its resume target, and carries ForkPending. startPane reads
// that flag and adds the adapter's fork argv to the start, never to the
// saved argv. pumpAdapter clears the flag when the adapter reports a
// session id that differs from the borrowed one. Until then a restart
// repeats the fork, and never resumes the parent's session in this pane.
func (s *Server) handlePaneFork(_ *Client, r *proto.Request) proto.Response {
	if s.isClosed() {
		return proto.ErrResp(r.ID, proto.ErrServerClosed,
			"this server has closed. It cannot start a new pane.")
	}
	lp, bad := s.livePane(r)
	if bad != nil {
		return *bad
	}
	id, _ := r.Str("pane")
	info, ok := s.paneInfo(id)
	if !ok {
		return proto.ErrResp(r.ID, proto.ErrPaneClosed,
			fmt.Sprintf("pane %s has closed. Create a new one with: coppice pane create", id))
	}
	if info.Kind != layout.KindHeadless || lp.Adapter == nil {
		return proto.ErrResp(r.ID, proto.ErrAdapter, "a pty pane cannot fork a session")
	}
	a, ok := adapters.Get(info.Harness)
	if !ok {
		return proto.ErrResp(r.ID, proto.ErrAdapter,
			fmt.Sprintf("%s cannot fork a session yet", info.Harness))
	}
	forker, ok := a.(pane.Forker)
	if !ok {
		return proto.ErrResp(r.ID, proto.ErrAdapter,
			fmt.Sprintf("%s cannot fork a session yet", info.Harness))
	}
	sid, ok := lp.Adapter.SessionID()
	if !ok || sid == "" {
		sid = info.HarnessSessionID
	}
	// A pending fork's adapter may answer with the borrowed id, so the
	// flag is the check, not the id.
	if sid == "" || info.ForkPending {
		return proto.ErrResp(r.ID, proto.ErrAdapter,
			fmt.Sprintf("%s has not reported a session id yet, wait for the first turn", info.Harness))
	}
	if _, err := forker.ForkArgv(sid); err != nil {
		return proto.ErrResp(r.ID, proto.ErrAdapter, err.Error())
	}

	label, ok := r.Str("label")
	if !ok || label == "" {
		label = info.Label + " fork"
	}
	rec, err := s.tree.CreatePane(info.Workspace, info.Tab, layout.Pane{
		Label: label, Cwd: info.Cwd, Env: maps.Clone(info.Env),
		Argv: append([]string(nil), info.Argv...), Kind: info.Kind,
		Harness: info.Harness, Cols: info.Cols, Rows: info.Rows,
		HarnessSessionID: sid, ParentPane: id, ForkPending: true,
	})
	if err != nil {
		code := proto.ErrNoSuchTab
		if errors.Is(err, layout.ErrNoWorkspace) {
			code = proto.ErrNoSuchWorkspace
		}
		return proto.ErrResp(r.ID, code, err.Error())
	}
	if err := s.startPane(rec); err != nil {
		// Same cleanup as pane.create: nothing ran, so nothing stays.
		_ = s.tree.RemovePane(rec.ID)
		s.removeLive(rec.ID)
		s.states.Forget(rec.ID)
		s.saveLayout()
		return proto.ErrResp(r.ID, proto.ErrSpawnFailed, err.Error())
	}
	s.saveLayout()
	return proto.OKResp(r.ID, map[string]any{
		"pane": rec.ID, "workspace": rec.Workspace, "tab": rec.Tab, "parent_pane": id,
	})
}

// errUnknownHarness is startPane's signal, for a headless pane, that no
// adapter is registered under this name at all - as opposed to
// adapters.ErrNotBuilt, a name that IS registered but is only a designed
// slot. handlePaneCreate maps both to bad_request: naming what a client got
// wrong is never a spawn failure.
var errUnknownHarness = errors.New("no adapter registered for this harness")

// errUnknownPaneKind is startPane's refusal: a rec.Kind
// its switch has no case for gets an error naming it, never a live entry
// with nothing behind it. handlePaneCreate's own "kind" validation already
// refuses anything but "pty"/"headless" at the wire before a record is ever
// built, so this is defense in depth for any other caller of startPane
// (restart-restore among them) rather than a path reachable
// through pane.create today.
var errUnknownPaneKind = errors.New("no such pane kind")

// startPane builds the grid and, for a pty pane, the process; for a headless
// pane, the adapter. Both kinds end up with the same thing behind them from a
// client's point of view: a *pane.Grid a client reads through pane.read, and
// a live entry pane.send_text/pane.close can reach.
//
// A headless record with ForkPending starts as a fork of the session in
// HarnessSessionID: the adapter must implement pane.Forker, and its fork
// argv is appended to rec.Argv for this start only, never saved. An adapter
// that cannot fork fails the start, so a restore leaves such a pane closed
// instead of resuming the parent's session in it.
func (s *Server) startPane(rec layout.Pane) error {
	g, err := pane.NewGrid(rec.Cols, rec.Rows)
	if err != nil {
		return err
	}
	lp := &LivePane{Info: rec, Grid: g, started: time.Now(), harness: rec.Harness}
	if lp.harness == "" {
		lp.harness = "shell"
	}
	switch rec.Kind {
	case layout.KindPTY:
		// A bare name resolves on PATH here, before the spawn, so a miss
		// is named as such. exec's own wording buries the cause in a quoted
		// Go error; this one says what to do next.
		if len(rec.Argv) > 0 && !strings.Contains(rec.Argv[0], "/") {
			if _, err := exec.LookPath(rec.Argv[0]); err != nil {
				g.Close()
				return fmt.Errorf("%s is not on PATH. Install it or give a full path.", rec.Argv[0])
			}
		}
		p, err := pane.StartPTY(pane.SpawnOpts{
			Cwd: rec.Cwd, Argv: rec.Argv, Env: rec.Env,
			Cols: rec.Cols, Rows: rec.Rows,
			Sock: s.cfg.SocketPath, PaneID: rec.ID,
		}, g)
		if err != nil {
			g.Close()
			return err
		}
		lp.PTY = p
		lp.tickStop = make(chan struct{})
		go s.watchExit(rec.ID, p)
	case layout.KindHeadless:
		a, ok := adapters.Get(rec.Harness)
		if !ok {
			g.Close()
			return fmt.Errorf("%w %q. Known adapters: %s",
				errUnknownHarness, rec.Harness, strings.Join(adapters.Names(), ", "))
		}
		argv := rec.Argv
		if rec.ForkPending {
			forker, ok := a.(pane.Forker)
			if !ok {
				g.Close()
				return fmt.Errorf("%s cannot fork a session, and this pane is a fork of %s that never reported its own session id",
					rec.Harness, rec.ParentPane)
			}
			extra, err := forker.ForkArgv(rec.HarnessSessionID)
			if err != nil {
				g.Close()
				return err
			}
			argv = append(append([]string(nil), rec.Argv...), extra...)
		}
		proc, err := a.Start(context.Background(), pane.StartOpts{
			Cwd: rec.Cwd, Env: rec.Env, Argv: argv,
			Resume: rec.HarnessSessionID, Sock: s.cfg.SocketPath, PaneID: rec.ID,
		}, g)
		if err != nil {
			g.Close()
			return err
		}
		lp.Adapter = proc
		go s.pumpAdapter(rec.ID, rec.Harness, proc, g)
	default:
		g.Close()
		return fmt.Errorf("%w %q", errUnknownPaneKind, rec.Kind)
	}
	s.putLive(rec.ID, lp)
	if lp.PTY != nil {
		// The first process fact lands before pane.create answers, so a
		// pane.list that follows at once already sees it. Only after that
		// does the goroutine take over.
		s.processTick(rec.ID, lp)
		go s.runProcessTicks(rec.ID, lp)
	}
	return nil
}

// processQuiet is how long a pty pane may go without output and still count
// as working. Past it the process source reports idle.
const processQuiet = 5 * time.Second

// processTickEvery is how often runProcessTicks looks at a pty pane's grid.
const processTickEvery = time.Second

// processTick applies one process fact for a pty pane: working when the
// grid took bytes within processQuiet, else idle. It applies an event only
// when that fact changed since the last one it sent. A fact that repeats
// every second would override a manifest read of the same screen each time,
// and the manifest is what tells a quiet prompt from a quiet wait. Merge
// keeps a gate, headless or operator hold on top of this source, so a
// blocked pane stays blocked, and a done stays done.
func (s *Server) processTick(paneID string, lp *LivePane) {
	st, detail := proto.StateIdle, "no output yet"
	if last, ok := lp.Grid.LastWrite(); ok {
		if time.Since(last) < processQuiet {
			st, detail = proto.StateWorking, "output in the last 5 s"
		} else {
			detail = "no output for 5 s"
		}
	}
	if st == lp.procState {
		return
	}
	lp.procState = st
	id := paneID
	s.ApplyState(paneID, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: paneID, Harness: lp.harness, Pane: &id,
		State: st, Source: proto.SrcProcess, Detail: detail,
	})
}

// runProcessTicks calls processTick once per processTickEvery until the
// child exits, an operator pane.close removes the entry, or the server
// closes. A tick that lands after watchExit posted done changes nothing:
// done is terminal in Merge.
func (s *Server) runProcessTicks(paneID string, lp *LivePane) {
	t := time.NewTicker(processTickEvery)
	defer t.Stop()
	for {
		select {
		case <-lp.PTY.Done():
			return
		case <-lp.tickStop:
			return
		case <-t.C:
			if s.isClosed() {
				return
			}
			s.processTick(paneID, lp)
		}
	}
}

// stopDrainWait bounds how long pumpAdapter waits, after EvEnd, for an
// adapter to actually close its Events() channel once told to Stop().
// EvEnd is terminal for the pump - an adapter that
// keeps producing (or simply keeps the channel open) after its own
// end-of-turn signal must not be able to hold the pane's record open behind
// a done nobody can see yet.
const stopDrainWait = time.Second

// pumpAdapter turns one adapter's events into transcript rows (rule (b): the
// server renders, so a headless pane is also just a grid to pane.read and
// attach) and PaneStateEvents with source headless. It is the only place a
// headless pane's state is produced, so the fail-closed rule lives in one
// spot: ApplyState validates every event before it is merged or broadcast, so
// an adapter that invented a state string (Event{State: "busy"}, say) cannot
// reach a client as anything but unknown with the rejection reason in detail
// - see TestAnAdapterCannotInventAState for that path exercised
// directly against ApplyState.
//
// The record
// closes BEFORE done is posted, never after. watchExit does closePane THEN
// ApplyState(done); the original version of this function did the reverse,
// which let a pane.wait_output --state done waiter observe an OPEN record
// for a headless pane in a way it never could for a pty one. See
// TestHeadlessPaneRecordIsClosedBeforeWaitOutputSeesDone.
//
// EvEnd is terminal. The loop below breaks the
// instant it sees one, rather than folding it into the generic per-event
// path (which would post done from inside the loop, before the terminal
// close below ever runs - exactly the ordering bug described above).
// pumpAdapter then calls
// Stop() itself and drains whatever the adapter still sends, bounded by
// ONE stopDrainWait deadline for the whole drain (the first
// version created a fresh timer inside the select on every iteration, which
// an adapter sending faster than once per stopDrainWait could hold off
// forever), before closing the record: an adapter is not trusted to close
// its own channel promptly just because it said EvEnd.
//
// WriteTranscript now reports the grid's own write
// error. Once it does, this stops trying to render (logging the transition
// once, not on every subsequent event) but keeps applying state regardless -
// a closed grid is not the same as a dead pane, and state must keep flowing
// to it either way.
//
// This goroutine can still be blocked in Events(), or in the drain
// step above, well after Server.Close() has already released the start lock
// - a headless adapter with a long-lived process, or simply one that has not
// sent anything since shutdown. Once the server has closed, it no longer
// owns the data directory - a new server may already hold it - so nothing
// here may write layout.json past that point: neither the in-loop
// HarnessSessionID record nor the terminal close. Unlike watchExit, the
// in-loop ApplyState calls stay unguarded even so, on the same reasoning
// scanOnce's own doc comment gives for finishing a scan already in flight
// when Close runs: ApplyState touches only this Server's in-memory tree and
// state store, neither of which Close frees.
func (s *Server) pumpAdapter(paneID, harness string, p pane.Proc, g *pane.Grid) {
	gridClosed := false
	write := func(ev pane.Event) {
		if gridClosed {
			return
		}
		if err := pane.WriteTranscript(g, ev); err != nil {
			gridClosed = true
			log.Printf("coppice: pane %s: its grid is closed, no longer rendering its headless "+
				"transcript (state keeps flowing): %v", paneID, err)
		}
	}

	sawEnd := false
	endDetail := ""
	for ev := range p.Events() {
		write(ev)
		if ev.Kind == pane.EvEnd {
			sawEnd = true
			endDetail = truncate(ev.Detail, proto.DetailMax)
			break
		}
		st, ok := pane.StateOf(ev)
		if !ok {
			continue
		}
		id := paneID
		e := proto.PaneStateEvent{
			V: 1, TS: nowSeconds(), SessionID: paneID, Harness: harness, Pane: &id,
			State: st, Source: proto.SrcHeadless, Detail: truncate(ev.Detail, proto.DetailMax),
		}
		// An ask only means anything on a blocked event, and Validate rejects
		// it anywhere else. pi (spec-04) and OpenCode (spec-05) fill it.
		if st == proto.StateBlocked {
			e.Ask = ev.Ask
		}
		if sid, ok := p.SessionID(); ok {
			e.HarnessSessionID = &sid
			if !s.isClosed() {
				// A fork borrows its parent's id until the adapter names a
				// different one. That first different id is the fork's own,
				// and the pending flag ends with it.
				if rec, ok := s.tree.Pane(paneID); ok && rec.HarnessSessionID != sid {
					_ = s.updatePane(paneID, func(x *layout.Pane) {
						x.HarnessSessionID = sid
						x.ForkPending = false
					})
				}
			}
		}
		s.ApplyState(paneID, e)
	}

	detail := "adapter stream ended"
	if sawEnd {
		// The adapter declared its own end; hold it to that. Stop() signals
		// (rule (c)) - the adapter's own producer goroutine is still
		// the one that closes Events() - so this drains rather than reading
		// once, bounded so a laggard, or an adapter that ignores Stop()
		// outright, cannot wedge the terminal close below.
		_ = p.Stop()
		// This timer is created ONCE, outside the loop, and the
		// same channel is selected on every iteration below. A bare
		// time.After(stopDrainWait) called fresh INSIDE the select (the
		// pre-fix version) arms a new stopDrainWait-long timer every time an
		// event arrives - an adapter that ignores Stop() and sends faster
		// than once per stopDrainWait then never lets that branch fire at
		// all, holding the drain, the terminal close, and this goroutine
		// open forever. Hoisting it here is what makes stopDrainWait a
		// ceiling on the WHOLE wait, not a quiet period between messages.
		// See TestPumpAdapterHasOneTotalDrainDeadlineNotOnePerEvent.
		deadline := time.After(stopDrainWait)
	drain:
		for {
			select {
			case ev, ok := <-p.Events():
				if !ok {
					break drain
				}
				write(ev)
			case <-deadline:
				break drain
			}
		}
		detail = endDetail
	}

	if s.isClosed() {
		return
	}
	// closePane BEFORE ApplyState(done), the record-closing order described
	// above. closePane, not a raw
	// s.tree.ClosePane: it also refreshes LivePane.Info and saves the
	// layout, which is what keeps the live snapshot from going stale against
	// the tree - see closePane's own doc comment, and watchExit just below,
	// which closes a pty pane the same way and in the same order.
	_ = s.closePane(paneID, nil)

	id := paneID
	s.ApplyState(paneID, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: paneID, Harness: harness, Pane: &id,
		State: proto.StateDone, Source: proto.SrcHeadless, Detail: detail,
	})
}

// drainWait bounds how long watchExit waits, once the child has already
// exited, for the reader goroutine to finish draining the last bytes into the
// grid. It is a courtesy, not a guarantee: a grandchild can still hold the pty
// slave open well past the direct child's own exit, in which case the reader
// stays blocked on Read() long after there is anything left worth reading.
// watchExit proceeds once this bound passes rather than let a lingering
// grandchild hold up the done report forever.
const drainWait = 500 * time.Millisecond

// watchExit turns a process exit into the one PaneStateEvent only the server
// can produce. done comes from process or headless and from nowhere else, and
// this fires exactly once and never retries, which is why state.Merge must let
// it through every hold.
//
// PTY.Done() means the child exited, not
// that the grid has seen everything the child wrote - a dedicated goroutine
// owns cmd.Wait() and can return before the separate reader goroutine has
// drained the last bytes off the master. Waiting on Drained() too, bounded by
// drainWait, is what makes sure the child's last output has already landed in
// the grid by the time a client sees this pane report done.
//
// Closing the tree record here does
// NOT remove the LivePane or free its grid - a pane whose process exited
// keeps both, so its final screen stays readable; only an operator
// pane.close does that.
//
// This goroutine can still be waiting on Done()/Drained()
// well after Server.Close() has already released the start lock (a child
// that outlives the server, or simply hasn't exited yet at shutdown). Once
// the server has closed, it no longer owns the tree or the data directory -
// a new server may already hold the lock - so nothing past this point may
// write to either.
func (s *Server) watchExit(paneID string, p *pane.PTY) {
	<-p.Done()
	select {
	case <-p.Drained():
	case <-time.After(drainWait):
	}
	if s.isClosed() {
		return
	}
	code, _ := p.ExitCode()
	_ = s.closePane(paneID, &code)
	harness := "shell"
	if rec, ok := s.tree.Pane(paneID); ok && rec.Harness != "" {
		harness = rec.Harness
	}
	s.ApplyState(paneID, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: paneID, Harness: harness,
		Pane: &paneID, State: proto.StateDone, Source: proto.SrcProcess,
		Detail: fmt.Sprintf("exit=%d", code),
	})
}

func nowSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// ApplyState, and the stateEvent wire shape it broadcasts, live in agents.go:
// pane.report_state, the PTY watcher and every future adapter all funnel
// through that one entry point.

// handlePaneList lists every record with its effective state. A live pty
// pane always has a stored event, because startPane applies the first
// process fact before pane.create answers, so the unknown default below is
// reached only by a record with no live entry and no stored event: a closed
// record from an older layout that Restore did not mark. quiet_for is
// seconds since the grid last took a byte, or since the entry started when
// it never has, and rides on every row that still has a grid.
func (s *Server) handlePaneList(_ *Client, r *proto.Request) proto.Response {
	out := []map[string]any{}
	for _, p := range s.tree.Panes() {
		row := map[string]any{
			"id": p.ID, "label": p.Label, "cwd": p.Cwd, "cmd": p.Argv,
			"kind": string(p.Kind), "harness": p.Harness,
			"workspace": p.Workspace, "tab": p.Tab, "closed": p.Closed,
			"cols": p.Cols, "rows": p.Rows,
			"state": proto.StateUnknown, "source": nil, "detail": "",
		}
		if p.ExitCode != nil {
			row["exit_code"] = *p.ExitCode
		}
		if p.ParentPane != "" {
			row["parent_pane"] = p.ParentPane
		}
		if lp, ok := s.Live(p.ID); ok {
			last := lp.started
			if lw, ok := lp.Grid.LastWrite(); ok {
				last = lw
			}
			row["quiet_for"] = time.Since(last).Seconds()
		}
		if ev, ok := s.states.Current(p.ID); ok {
			// EffectiveState is the read-time half of
			// Merge's own expiry rule. Current alone hands back a blocked
			// event forever, even long after its ask's deadline passed -
			// EffectiveState is what turns that into working for a reader
			// with no fresh event to merge against.
			eff := state.EffectiveState(&ev, nowSeconds())
			row["state"] = eff.State
			row["source"] = eff.Source
			row["detail"] = eff.Detail
			// Copied from the stored merged
			// event, never a receive-time stamp - EffectiveState never
			// touches either field, so eff.TS and eff.SessionID are always
			// the ones the event itself carried.
			row["ts"] = eff.TS
			row["session_id"] = eff.SessionID
			if eff.Ask != nil {
				row["ask"] = eff.Ask
			}
		}
		out = append(out, row)
	}
	return proto.OKResp(r.ID, map[string]any{"panes": out})
}

// livePane resolves a pane id to something writable, mapping every failure to
// the right closed-enum code.
func (s *Server) livePane(r *proto.Request) (*LivePane, *proto.Response) {
	id, ok := r.Str("pane")
	if !ok || id == "" {
		resp := proto.ErrResp(r.ID, proto.ErrBadRequest, "this command needs pane. Run: coppice pane list")
		return nil, &resp
	}
	rec, ok := s.tree.Pane(id)
	if !ok {
		resp := proto.ErrResp(r.ID, proto.ErrNoSuchPane,
			fmt.Sprintf("no pane %q. Run: coppice pane list", id))
		return nil, &resp
	}
	if rec.Closed {
		resp := proto.ErrResp(r.ID, proto.ErrPaneClosed,
			fmt.Sprintf("pane %s has closed. Create a new one with: coppice pane create", id))
		return nil, &resp
	}
	lp, ok := s.Live(id)
	if !ok {
		// restart.go's Restore closes every non-resumed record before Serve
		// ever starts, so the rec.Closed branch above already catches every
		// ordinary case where a pane's process did not survive a restart.
		// This is defense in depth for a live map that somehow disagrees
		// with an open tree record: the same closed answer, not a claim
		// about a restart that may not have happened.
		resp := proto.ErrResp(r.ID, proto.ErrPaneClosed,
			fmt.Sprintf("pane %s has closed. Create a new one with: coppice pane create", id))
		return nil, &resp
	}
	return lp, nil
}

func (lp *LivePane) write(b []byte) error {
	switch {
	case lp.PTY != nil:
		_, err := lp.PTY.Write(b)
		return err
	case lp.Adapter != nil:
		return lp.Adapter.WriteStdin(b)
	default:
		return fmt.Errorf("this pane has nothing to write to")
	}
}

func (s *Server) handleSendText(c *Client, r *proto.Request) proto.Response {
	// Absent text is a mistake worth naming, not a
	// silent bare Enter. A caller that really wants a bare Enter passes "".
	text, ok := r.Str("text")
	if !ok {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			`pane.send_text needs text. Pass "" explicitly to send a bare Enter.`)
	}
	lp, bad := s.livePane(r)
	if bad != nil {
		return *bad
	}
	if id, _ := r.Str("pane"); c.viewOnlyOn(id) {
		return refuseViewOnly(r, id)
	}
	enter, ok := r.Bool("enter")
	if !ok {
		enter = true
	}
	if enter {
		text += "\r"
	}
	if err := lp.write([]byte(text)); err != nil {
		return proto.ErrResp(r.ID, proto.ErrInternal, err.Error())
	}
	return proto.OKResp(r.ID, map[string]any{"sent": len(text)})
}

func (s *Server) handleSendKeys(c *Client, r *proto.Request) proto.Response {
	lp, bad := s.livePane(r)
	if bad != nil {
		return *bad
	}
	if id, _ := r.Str("pane"); c.viewOnlyOn(id) {
		return refuseViewOnly(r, id)
	}
	keys, ok := r.StrSlice("keys")
	if !ok || len(keys) == 0 {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			"pane.send_keys needs keys. Known keys: "+keyNames())
	}
	var out strings.Builder
	for _, k := range keys {
		if seq, ok := namedKeys[strings.ToLower(k)]; ok {
			out.WriteString(seq)
			continue
		}
		if len([]rune(k)) == 1 {
			out.WriteString(k)
			continue
		}
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			fmt.Sprintf("key %q is not known. Known keys: %s", k, keyNames()))
	}
	if err := lp.write([]byte(out.String())); err != nil {
		return proto.ErrResp(r.ID, proto.ErrInternal, err.Error())
	}
	return proto.OKResp(r.ID, map[string]any{"sent": len(keys)})
}

// handleRun reads its command line from "line", not "cmd": proto.Request
// already reserves the top-level "cmd" key for the verb itself
// ("pane.run"), and a wire object cannot carry two values under the same
// JSON key - encoding/json resolves a duplicate key to whichever occurrence
// comes last, silently replacing the verb.
func (s *Server) handleRun(c *Client, r *proto.Request) proto.Response {
	line, ok := r.Str("line")
	if !ok {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, "pane.run needs line. Pass the line to type.")
	}
	lp, bad := s.livePane(r)
	if bad != nil {
		return *bad
	}
	if id, _ := r.Str("pane"); c.viewOnlyOn(id) {
		return refuseViewOnly(r, id)
	}
	if err := lp.write([]byte(line + "\r")); err != nil {
		return proto.ErrResp(r.ID, proto.ErrInternal, err.Error())
	}
	return proto.OKResp(r.ID, map[string]any{"sent": len(line) + 1})
}

// handleRead deliberately does not go through livePane: a pane whose
// process exited keeps its grid, so
// its final screen stays readable, and only an operator pane.close frees it
// and removes the live entry. So this checks the tree only for existence
// (no_such_pane), and the live map only for whether the grid is still there
// at all (pane_closed) - never the tree's own Closed flag, which a
// process-exited pane also carries but must not block a read.
func (s *Server) handleRead(_ *Client, r *proto.Request) proto.Response {
	id, ok := r.Str("pane")
	if !ok || id == "" {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, "this command needs pane. Run: coppice pane list")
	}
	if _, ok := s.tree.Pane(id); !ok {
		return proto.ErrResp(r.ID, proto.ErrNoSuchPane,
			fmt.Sprintf("no pane %q. Run: coppice pane list", id))
	}
	lp, ok := s.Live(id)
	if !ok {
		return proto.ErrResp(r.ID, proto.ErrPaneClosed,
			fmt.Sprintf("pane %s has closed. Create a new one with: coppice pane create", id))
	}
	src := pane.ReadVisible
	if v, ok := r.Str("source"); ok {
		src = pane.ReadSource(v)
	}
	text, err := lp.Grid.Read(src)
	if err != nil {
		// The grid can close out from under this call if pane.close races
		// it on another connection: Live() above found the LivePane, but
		// handleClose can free its Grid between that lookup and this Read.
		if errors.Is(err, pane.ErrGridClosed) {
			return proto.ErrResp(r.ID, proto.ErrPaneClosed,
				fmt.Sprintf("pane %s has closed. Create a new one with: coppice pane create", id))
		}
		return proto.ErrResp(r.ID, proto.ErrBadRequest, err.Error())
	}
	return proto.OKResp(r.ID, map[string]any{"text": text, "source": string(src)})
}

func (s *Server) handleResize(c *Client, r *proto.Request) proto.Response {
	lp, bad := s.livePane(r)
	if bad != nil {
		return *bad
	}
	if id, _ := r.Str("pane"); c.viewOnlyOn(id) {
		return refuseViewOnly(r, id)
	}
	cols, okc := r.Int("cols")
	rows, okr := r.Int("rows")
	if !okc || !okr || cols <= 0 || rows <= 0 {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			"pane.resize needs positive cols and rows.")
	}
	if lp.PTY != nil {
		if err := lp.PTY.Resize(cols, rows); err != nil {
			return proto.ErrResp(r.ID, proto.ErrInternal, err.Error())
		}
	} else if err := lp.Grid.Resize(cols, rows); err != nil {
		return proto.ErrResp(r.ID, proto.ErrInternal, err.Error())
	}
	// paneInfo, not lp.Info directly: refreshLiveInfo (called by updatePane
	// and closePane) writes lp.Info under liveMu from another goroutine, so
	// reading it here without that lock is a data race.
	id, _ := r.Str("pane")
	if info, ok := s.paneInfo(id); ok {
		_ = s.updatePane(info.ID, func(x *layout.Pane) { x.Cols, x.Rows = cols, rows })
	}
	return proto.OKResp(r.ID, map[string]any{"cols": cols, "rows": rows})
}

// handleClose is the ONLY path that frees a pane's grid and removes its
// LivePane: a process exit alone
// (watchExit) keeps both, so the pane's final screen stays readable. Order
// matters here: closePane (which refreshes LivePane.Info under liveMu) runs
// BEFORE removeLive, or the refresh would find nothing left in s.live to
// write into and silently do nothing.
func (s *Server) handleClose(_ *Client, r *proto.Request) proto.Response {
	id, _ := r.Str("pane")
	if id == "" {
		id, _ = r.Str("session")
	}
	rec, ok := s.tree.Pane(id)
	if !ok {
		return proto.ErrResp(r.ID, proto.ErrNoSuchPane,
			fmt.Sprintf("no pane %q. Run: coppice pane list", id))
	}
	if lp, ok := s.Live(id); ok {
		if lp.PTY != nil {
			_ = lp.PTY.Close()
		}
		if lp.Adapter != nil {
			_ = lp.Adapter.Stop()
		}
		lp.Grid.Close()
	}
	_ = s.closePane(rec.ID, rec.ExitCode)
	s.removeLive(rec.ID)
	return proto.OKResp(r.ID, map[string]any{"pane": rec.ID, "closed": true})
}

// handleWaitOutput waits for text on the screen, or for a merged state, or
// times out. It polls at 50 ms: a pane's grid changes on the PTY reader's
// goroutine, and a poll keeps the wait out of that hot path.
func (s *Server) handleWaitOutput(c *Client, r *proto.Request) proto.Response {
	id, _ := r.Str("pane")
	rec, ok := s.tree.Pane(id)
	if !ok {
		return proto.ErrResp(r.ID, proto.ErrNoSuchPane,
			fmt.Sprintf("no pane %q. Run: coppice pane list", id))
	}
	contains, wantText := r.Str("contains")
	wantState, wantsState := r.Str("state")
	if !wantText && !wantsState {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			"pane.wait_output needs contains or state.")
	}
	ms, ok := r.Int("timeout_ms")
	if !ok || ms <= 0 {
		ms = 30000
	}
	deadline := time.Now().Add(time.Duration(ms) * time.Millisecond)
	for time.Now().Before(deadline) {
		// A client that has already disconnected must
		// not keep this handler polling all the way to timeout_ms.
		if c.Dead() {
			return proto.ErrResp(r.ID, proto.ErrTimeout, "client disconnected while waiting")
		}
		// The live entry is gone
		// only once an operator has run pane.close - a process exit alone
		// leaves it in place, so this is the "at once, no polling to
		// timeout" signal that pane.close already happened.
		lp, live := s.Live(rec.ID)
		if !live {
			return proto.ErrResp(r.ID, proto.ErrPaneClosed,
				fmt.Sprintf("pane %s has closed. Create a new one with: coppice pane create", rec.ID))
		}
		if wantsState {
			// EffectiveState is what turns a
			// long-expired blocked/ask into working for a reader with no
			// fresh event to merge against - Current alone never expires it.
			if ev, ok := s.states.Current(rec.ID); ok {
				eff := state.EffectiveState(&ev, nowSeconds())
				if eff.State == wantState {
					// closed rides in the same reply as the matched state,
					// read here rather than left for a caller's own
					// separate pane.list: closePane and ApplyState(done)
					// can run in either order, and reading both here, in
					// one round trip, keeps the reply accurate regardless
					// of which one went first.
					closed := false
					if info, ok := s.paneInfo(rec.ID); ok {
						closed = info.Closed
					}
					return proto.OKResp(r.ID, map[string]any{
						"matched": "state", "state": eff.State, "closed": closed,
					})
				}
			}
		}
		if wantText {
			if text, err := lp.Grid.Read(pane.ReadRecent); err == nil &&
				strings.Contains(text, contains) {
				return proto.OKResp(r.ID, map[string]any{"matched": "text"})
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return proto.ErrResp(r.ID, proto.ErrTimeout,
		fmt.Sprintf("pane %s did not match within %d ms. Run: coppice pane read %s", rec.ID, ms, rec.ID))
}

func (s *Server) saveLayout() {
	// Best effort. A layout write failure must never fail a command that
	// already succeeded in the world.
	_ = s.tree.Save(layoutPath(s.cfg.DataDir))
}

func layoutPath(dataDir string) string { return filepath.Join(dataDir, "layout.json") }

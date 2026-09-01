package server

import (
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/opendaisugi/coppice/internal/adapters"
	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/proto"
)

// EndedTTL is how long an ended record sits in Recent before the sweep
// removes it.
const EndedTTL = 7 * 24 * time.Hour

// EndedSweepEvery is how often the background sweep runs while the server
// is up. The first sweep runs once, synchronously, at the end of Restore -
// see Restore's own doc comment - so a server that ran for months before
// this build existed does not wait a full hour for its first pass.
const EndedSweepEvery = time.Hour

// RegisterEndedCommands wires up pane.forget and pane.resume. New calls this
// beside RegisterPaneCommands.
func (s *Server) RegisterEndedCommands() {
	_ = s.Handle("pane.forget", s.handlePaneForget)
	_ = s.Handle("pane.resume", s.handlePaneResume)
}

// sortEndedNewestFirst orders ended records by EndedAt, newest first, with
// id as a stable tiebreak for two records stamped in the same second.
// pane.list and agent.list both call this for `ended: true`, so the two
// never disagree on order.
func sortEndedNewestFirst(panes []layout.Pane) {
	sort.Slice(panes, func(i, j int) bool {
		if panes[i].EndedAt != panes[j].EndedAt {
			return panes[i].EndedAt > panes[j].EndedAt
		}
		return panes[i].ID > panes[j].ID
	})
}

// SweepEnded removes every ended record whose EndedAt is older than
// EndedTTL, measured against the server clock (clockSkew-aware, so a test
// can move it forward instead of sleeping seven days). It returns how many
// it removed. A record with no EndedAt yet - unreachable in practice once
// Restore's own migration has run, since every path that marks a record
// ended stamps one - is left alone rather than treated as infinitely old.
func (s *Server) SweepEnded() int {
	cutoff := s.clock().Add(-EndedTTL).Unix()
	n := 0
	for _, p := range s.tree.Panes() {
		if p.Closed && p.EndedAt != 0 && p.EndedAt < cutoff {
			s.dropRecord(p.ID)
			n++
		}
	}
	return n
}

// StartEndedSweep runs SweepEnded once every EndedSweepEvery until
// StopEndedSweep or Close. Calling it twice without an intervening
// StopEndedSweep is a no-op, and calling it after Close is also a no-op -
// the same two guards StartManifestTick uses, for the same reason: a
// caller racing Close with a start must not spin up a goroutine nothing
// will ever stop.
func (s *Server) StartEndedSweep() {
	s.mu.Lock()
	if s.closed || s.endedSweepStop != nil {
		s.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	s.endedSweepStop = stop
	s.mu.Unlock()

	go func() {
		t := time.NewTicker(EndedSweepEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				// Checked here, not just at the top of Close's own
				// teardown: once the server has closed it may no longer
				// own the data directory (a new server may already hold
				// the start lock), and SweepEnded's dropRecord writes
				// layout.json on every removal.
				if s.isClosed() {
					return
				}
				s.SweepEnded()
			}
		}
	}()
}

// StopEndedSweep stops the sweep goroutine started by StartEndedSweep, if
// one is running. Idempotent, the same as StopManifestTick. Server.Close
// also stops it on its own, so a caller that forgets this does not leak the
// goroutine past Close.
func (s *Server) StopEndedSweep() {
	s.mu.Lock()
	stop := s.endedSweepStop
	s.endedSweepStop = nil
	s.mu.Unlock()
	if stop != nil {
		close(stop)
	}
}

// endedFallbackState is the state a closed pane's own record can honestly
// report when nothing was ever stored for it in this server's state
// store - the shape a pane that already ended before a restart is in,
// since the state store holds nothing across a restart (only layout.json
// does) and Restore's AlreadyClosed branch stamps EndedAt without
// inventing a receive-time event for a fact this server never itself
// observed. done is always the right word here: Closed only ever becomes
// true once a pane's process is genuinely gone, so this reports exactly
// what a live-observed done event would have said, using the record's
// own EndedAt as the timestamp - the pane's real end time, not the
// moment this row happens to be read.
func endedFallbackState(p layout.Pane) (state, source, detail string, ts float64) {
	detail = "ended before this server started"
	if p.ExitCode != nil {
		detail = fmt.Sprintf("exit=%d", *p.ExitCode)
	}
	return proto.StateDone, proto.SrcProcess, detail, float64(p.EndedAt)
}

// resumableCount is how many ended records server.status can honestly say
// pane.resume would actually resume, rather than start fresh: a headless
// record with a harness session id, or a pty one with a harness session id
// whose harness table has resume_args. It reads the config file once per
// call, the same way harnessArgv does, so an edit to resume_args takes
// effect on the next status call with no restart.
func (s *Server) resumableCount() int {
	cfg, found, err := config.Load()
	n := 0
	for _, p := range s.tree.Panes() {
		if !p.Closed || p.HarnessSessionID == "" {
			continue
		}
		switch p.Kind {
		case layout.KindHeadless:
			n++
		case layout.KindPTY:
			if err == nil && found {
				if h, ok := cfg.Harness[p.Harness]; ok && len(h.ResumeArgs) > 0 {
					n++
				}
			}
		}
	}
	return n
}

// handlePaneForget removes an ended record: `pane` for one, or `ended: true`
// for every ended record at once. A live pane is refused, never silently
// left alone - the caller asked to forget something that has not ended.
// The two params together are refused too: answering them as "every ended
// record" would wipe out every other one the caller never named, and
// answering as "just this one" would silently ignore ended: true - neither
// guess is safe to make on the caller's behalf.
func (s *Server) handlePaneForget(_ *Client, r *proto.Request) proto.Response {
	all, _ := r.Bool("ended")
	id, hasPane := r.Str("pane")
	hasPane = hasPane && id != ""
	if all && hasPane {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			"pane.forget takes pane, or ended: true, not both.")
	}
	if all {
		n := 0
		for _, p := range s.tree.Panes() {
			if p.Closed {
				s.dropRecord(p.ID)
				n++
			}
		}
		return proto.OKResp(r.ID, map[string]any{"forgot": n})
	}
	if !hasPane {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			"pane.forget needs pane, or ended: true to forget every ended record.")
	}
	rec, ok := s.tree.Pane(id)
	if !ok {
		return proto.ErrResp(r.ID, proto.ErrNoSuchPane,
			fmt.Sprintf("no pane %q. Run: coppice pane list --ended", id))
	}
	if !rec.Closed {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			fmt.Sprintf("pane %s is still live. Close it first: coppice pane close %s", id, id))
	}
	s.dropRecord(id)
	return proto.OKResp(r.ID, map[string]any{"pane": id, "forgot": true})
}

// resumeArgvFor builds a pty pane's resume command line: the harness
// table's command and ordinary args, then resume_args with every
// "{session}" replaced by sid, then the pane's own extraArgs - whatever it
// was given past the harness's fixed args at pane.create time (a task
// prompt, say). Without that last part a resumed pane would run the
// harness's bare resume invocation and lose whatever made this pane's own
// launch different from any other pane of the same harness.
//
// ok is false when the config file is missing, does not parse, has no
// table for name, or that table has no resume_args - the caller's signal
// to start fresh instead.
func resumeArgvFor(name, sid string, extraArgs []string) ([]string, bool) {
	cfg, found, err := config.Load()
	if err != nil || !found {
		return nil, false
	}
	h, ok := cfg.Harness[name]
	if !ok || len(h.ResumeArgs) == 0 {
		return nil, false
	}
	resumeArgs := make([]string, len(h.ResumeArgs))
	for i, a := range h.ResumeArgs {
		resumeArgs[i] = strings.ReplaceAll(a, "{session}", sid)
	}
	extra := append(resumeArgs, extraArgs...)
	return harnessArgv(name, extra)
}

// claimResume marks id as having a pane.resume in flight for it, so a
// second, concurrent pane.resume for the same id is refused outright
// instead of racing the first one to a spawn: both could otherwise read
// the ended record before either has removed it, and each start its own
// live pane from it. ok is false when id is already claimed.
func (s *Server) claimResume(id string) (ok bool) {
	s.resumeMu.Lock()
	defer s.resumeMu.Unlock()
	if s.resuming[id] {
		return false
	}
	s.resuming[id] = true
	return true
}

// releaseResume clears a claim claimResume made. handlePaneResume defers
// this right after a successful claim, so every return path releases it -
// success, a refusal, or a failed spawn - and a caller can always retry.
func (s *Server) releaseResume(id string) {
	s.resumeMu.Lock()
	delete(s.resuming, id)
	s.resumeMu.Unlock()
}

// handlePaneResume starts a new live pane with an ended record's label,
// cwd, harness and task, and removes the ended record once that new pane
// has actually started. The three-way resume rule:
//
//   - headless with a recorded harness session id: resumes that session,
//     the same Resume field Restore sends on an ordinary restart. A pane
//     that was itself a pending fork carries ForkPending and ParentPane
//     forward, so startPane repeats the fork instead of resuming the
//     borrowed session directly - the same rule a restart follows for a
//     fork that never reported its own session id.
//   - pty with a recorded harness session id AND a `resume_args` entry for
//     its harness: builds the resume command line from that.
//   - anything else: starts fresh from the record's own argv. The reply
//     says resumed: false, never a guess dressed up as a fact.
//
// The ended record is left in place if the new pane fails to start, so a
// caller can fix whatever was wrong (a stale binary, a bad resume_args
// entry) and try again without losing the record.
//
// The id is claimed (claimResume) before the tree is even read, and the
// tree read happens only once the claim is held - never before. A second
// call for the same id, whether truly concurrent or arriving just after
// the first one finished, always finds the claim already held and refuses
// at once, or claims it itself once the first call's own release runs -
// which happens two ways: right after a successful dropRecord, so the
// second call's own fresh tree read finds the record gone and answers
// no_such_pane; or, if CreatePane or startPane failed, with no dropRecord
// at all, so the second call's fresh read finds the same ended record the
// first call left untouched and can retry the resume itself.
func (s *Server) handlePaneResume(_ *Client, r *proto.Request) proto.Response {
	if s.isClosed() {
		return proto.ErrResp(r.ID, proto.ErrServerClosed,
			"this server has closed. It cannot start a new pane.")
	}
	id, ok := r.Str("pane")
	if !ok || id == "" {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			"pane.resume needs pane. Run: coppice pane list --ended")
	}
	// An optional size lets a client start the new pane at the size of the
	// window it will show in. Either one given must be positive.
	cols, hasCols := r.Int("cols")
	rows, hasRows := r.Int("rows")
	if (hasCols && cols <= 0) || (hasRows && rows <= 0) {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			"pane.resume takes positive cols and rows, or neither.")
	}
	if !s.claimResume(id) {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			fmt.Sprintf("pane %s is already resuming.", id))
	}
	defer s.releaseResume(id)

	rec, ok := s.tree.Pane(id)
	if !ok {
		return proto.ErrResp(r.ID, proto.ErrNoSuchPane,
			fmt.Sprintf("no pane %q. Run: coppice pane list --ended", id))
	}
	if !rec.Closed {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			fmt.Sprintf("pane %s is still live. Run: coppice pane list", id))
	}

	argv := append([]string(nil), rec.Argv...)
	resumed := false
	sessionID := ""
	forkPending := false
	parentPane := ""

	switch {
	case rec.Kind == layout.KindHeadless && rec.HarnessSessionID != "":
		sessionID = rec.HarnessSessionID
		forkPending = rec.ForkPending
		parentPane = rec.ParentPane
		resumed = true
	case rec.Kind == layout.KindPTY && rec.HarnessSessionID != "":
		if resolved, ok := resumeArgvFor(rec.Harness, rec.HarnessSessionID, rec.ExtraArgs); ok {
			argv = resolved
			sessionID = rec.HarnessSessionID
			resumed = true
		}
	}

	if !hasCols {
		cols = rec.Cols
	}
	if !hasRows {
		rows = rec.Rows
	}
	// A record labelled foreman is the floor's foreman again only when it
	// is the one the server tracks. Any other keeps no foreman label, so
	// two panes never read as the foreman. talkMu is held from here to
	// the end, so a talk cannot start a foreman in between.
	label := cleanLabel(rec.Label)
	talkHeld := isForemanLabel(label) || s.isTrackedForeman(id)
	if talkHeld {
		s.talkMu.Lock()
		defer s.talkMu.Unlock()
		if s.trackedForeman() != id && isForemanLabel(label) {
			label = DefaultForemanLabel + "-old"
		}
	}
	newRec, err := s.tree.CreatePane(rec.Workspace, rec.Tab, layout.Pane{
		Label: label, Cwd: rec.Cwd, Argv: argv, Env: maps.Clone(rec.Env), Kind: rec.Kind,
		Harness: rec.Harness, Cols: cols, Rows: rows, TaskID: rec.TaskID,
		HarnessSessionID: sessionID, ForkPending: forkPending, ParentPane: parentPane,
		ExtraArgs: rec.ExtraArgs,
	})
	if err != nil {
		code := proto.ErrNoSuchTab
		if errors.Is(err, layout.ErrNoWorkspace) {
			code = proto.ErrNoSuchWorkspace
		}
		return proto.ErrResp(r.ID, code, err.Error())
	}
	if err := s.startPane(newRec); err != nil {
		// Same cleanup as pane.create and pane.fork: nothing ran, so
		// nothing stays of the NEW record. The ended one this resume was
		// trying to replace is left exactly as it was.
		_ = s.tree.RemovePane(newRec.ID)
		s.removeLive(newRec.ID)
		s.states.Forget(newRec.ID)
		s.saveLayout()
		code := proto.ErrSpawnFailed
		if errors.Is(err, errUnknownHarness) || errors.Is(err, adapters.ErrNotBuilt) ||
			errors.Is(err, errUnknownPaneKind) {
			code = proto.ErrBadRequest
		}
		return proto.ErrResp(r.ID, code, err.Error())
	}
	s.dropRecord(id)
	if talkHeld {
		s.followForemanLocked(id, newRec.ID, resumed)
	}
	return proto.OKResp(r.ID, map[string]any{
		"pane": newRec.ID, "workspace": newRec.Workspace, "tab": newRec.Tab, "resumed": resumed,
	})
}

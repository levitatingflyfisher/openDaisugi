package server

import (
	"fmt"
	"os"

	"github.com/opendaisugi/coppice/internal/adapters"
	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/proto"
)

// RestoreReport and LastRestore live in server.go, because Server owns the
// field and handleStatus returns it. This file only fills it.
//
// Restore reads the layout from disk. It refuses without the start lock: a
// second `coppice server start` must fail before it can read this file, spawn
// resumed panes, and write its own tree back over the running server's. It
// refuses once Serve has started, using the same serving flag Handle already
// refuses against, since s.tree is read with no lock of its own by every
// handler and replacing it out from under one would be a data race as well as
// a stale read. It also refuses while any pane is live. Restore replaces
// s.tree wholesale, and a live pane whose record only exists in the old tree
// would be orphaned the moment that happens. In practice Restore always runs
// before Serve starts any pane, so neither of these two should ever fire;
// they exist so a caller that got the order wrong fails loudly instead of
// losing a pane's record, or racing a handler, silently.
//
// It restores the tree, the labels, the working directories and the id
// counters. It resurrects nothing: a pty pane's process is gone, so the pane
// comes back closed and done. A headless pane whose adapter recorded a
// harness session id is resumed; one without an id comes back closed and
// unknown, because we cannot say whether that agent finished. A pane already
// closed before the restart is left alone and counted separately, so the
// report never implies a restart raised more panes than there were.
func (s *Server) Restore() error {
	if !s.HoldsStartLock() {
		return fmt.Errorf(
			"call AcquireStartLock before Restore. Two starts restoring the same layout would each spawn resumed panes.")
	}
	s.mu.Lock()
	serving := s.serving
	s.mu.Unlock()
	if serving {
		return fmt.Errorf(
			"cannot restore after Serve has started. Restore runs before the server accepts connections.")
	}
	s.liveMu.RLock()
	live := len(s.live)
	s.liveMu.RUnlock()
	if live > 0 {
		return fmt.Errorf(
			"cannot restore while %d panes are live. Restore must run before any pane starts.", live)
	}
	path := layoutPath(s.cfg.DataDir)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	tree, err := layout.Load(path)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w. "+
			"Move it aside and start again to get a fresh workshop.", path, err)
	}
	s.tree = tree

	// Notes starts as an empty slice, not nil: server.status marshals this
	// report as JSON, and a nil slice serializes as null, which a caller
	// iterating "notes" in JavaScript would have to special-case where an
	// empty array would have been a silent no-op.
	rep := RestoreReport{Notes: []string{}}
	for _, rec := range tree.Panes() {
		rep.Panes++
		if rec.Closed {
			rep.AlreadyClosed++
			// A layout saved before EndedAt existed has Closed true with
			// no age of its own. Stamping it now, rather than leaving it
			// at zero, is what lets it land in Recent under `ended: true`
			// and eventually age out through the seven-day sweep, instead
			// of sitting invisible forever - exactly the zombie this
			// whole feature exists to clear.
			if rec.EndedAt == 0 {
				_ = tree.ClosePane(rec.ID, rec.ExitCode, s.clock().Unix())
			}
			continue
		}
		switch {
		case rec.Kind == layout.KindHeadless && rec.HarnessSessionID != "":
			if _, ok := adapters.Get(rec.Harness); !ok {
				s.markRestored(rec, proto.StateUnknown, "no adapter named "+rec.Harness)
				rep.MarkedUnknown++
				rep.Notes = append(rep.Notes, fmt.Sprintf(
					"%s used harness %q, which this build has no adapter for.",
					rec.ID, rec.Harness))
				continue
			}
			if err := s.startPane(rec); err != nil {
				s.markRestored(rec, proto.StateUnknown, "resume failed: "+err.Error())
				rep.MarkedUnknown++
				rep.Notes = append(rep.Notes, fmt.Sprintf(
					"%s did not resume: %v. Create a new pane in %s.", rec.ID, err, rec.Cwd))
				continue
			}
			rep.Resumed++
			// startPane returning nil means the adapter
			// was asked to resume and accepted the spawn, not that it
			// actually resumed - an adapter that rejects the session id can
			// still exit into done within the first second. The note says
			// what happened at this point, not a verdict on what comes
			// after it.
			if rec.ForkPending {
				rep.Notes = append(rep.Notes, fmt.Sprintf(
					"%s asked its adapter to fork harness session %s again, because the fork had not reported its own session id.",
					rec.ID, rec.HarnessSessionID))
			} else {
				rep.Notes = append(rep.Notes, fmt.Sprintf(
					"%s asked its adapter to resume harness session %s.", rec.ID, rec.HarnessSessionID))
			}
		case rec.Kind == layout.KindPTY:
			s.markRestored(rec, proto.StateDone, "the process did not survive the restart")
			rep.MarkedDone++
			rep.Notes = append(rep.Notes, fmt.Sprintf(
				"%s ran %v. Its process is gone. Create a new pane in %s.",
				rec.ID, rec.Argv, rec.Cwd))
		default:
			s.markRestored(rec, proto.StateUnknown, "no harness session id was recorded")
			rep.MarkedUnknown++
			rep.Notes = append(rep.Notes, fmt.Sprintf(
				"%s had no recorded harness session, so its outcome is unknown.", rec.ID))
		}
	}
	s.mu.Lock()
	s.restore = rep
	s.mu.Unlock()
	s.saveLayout()
	// The at-start half of the seven-day rule: whatever this restart just
	// marked ended, plus whatever was already ended and old enough, is
	// swept once here. StartEndedSweep (called by the CLI once Restore
	// returns) is the hourly half.
	s.SweepEnded()
	return nil
}

// markRestored closes the record and records the restored state through
// ApplyState, the same single entry point the PTY watcher, the manifest tick
// and every adapter use, so master spec 3.1 is applied once regardless of
// where an event comes from. Source is always process: this is the restart
// itself observing that no process is behind the record any more, never the
// process or the headless adapter reporting anything of its own.
// proto.Validate accepts both done and unknown from source process; st is
// always one of those two, so no branch is needed to pick between sources,
// and ApplyState never has cause to reject either event. Restore runs before
// Serve, so ApplyState's broadcast and waiter notification reach nobody yet
// - there is no connected client and no waiter to notify - but the state
// still lands in s.states exactly as it would from any other caller.
//
// It calls s.tree.ClosePane directly rather than the server's own closePane.
// closePane also refreshes a LivePane's cached Info, which is the only path
// to Tree.ClosePane everywhere else in this package, but Restore runs before
// Serve, before any of these records has a LivePane yet, so that refresh
// would have nothing to update. Restore's own EndedAt migration, just above,
// is the only other caller in this file that goes straight to
// Tree.ClosePane, for the same reason.
func (s *Server) markRestored(rec layout.Pane, st, detail string) {
	_ = s.tree.ClosePane(rec.ID, rec.ExitCode, s.clock().Unix())
	s.tree.DropForeman(rec.ID)
	id := rec.ID
	harness := rec.Harness
	if harness == "" {
		harness = "shell"
	}
	s.ApplyState(id, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: id, Harness: harness, Pane: &id,
		State: st, Source: proto.SrcProcess, Detail: truncate(detail, proto.DetailMax),
	})
}

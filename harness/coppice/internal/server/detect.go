package server

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/opendaisugi/coppice/internal/detect"
	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/pane"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/state"
)

// ManifestTick is how often a pty pane with no better source is scanned.
const ManifestTick = 500 * time.Millisecond

// GateQuiet is the hold state.Merge's rule 6 gives an incoming manifest
// event against a fresher, higher-rank current state (internal/state/state.go).
// It is NOT the only hold a gate can place, and the two are not symmetric: a
// gate blocked report carrying an unexpired ask is Merge's rule 4, and that
// hold lasts until the gate itself clears it or the ask's own deadline
// passes - which can be far longer than two seconds. scanOnce checks for
// that hold separately (tickSkipReason), rather than assuming GateQuiet
// covers every case a gate can hold a pane back.
//
// For an ordinary (non-ask) higher-rank source, the hold does release after
// GateQuiet - scanOnce's own skip and Merge's rule 6 both measure the
// identical receive-clock window - which is what makes a harness that
// stopped calling the gate recoverable instead of frozen on its last gate
// report for the rest of the session.
const GateQuiet = 2 * time.Second

// detectionInput reads a pane's detection window plus its OSC state, and
// hands back the raw text too: scanOnce needs only the detect.Input, but
// pane.explain's detection_text and pane.read --source detection must show
// the identical string (TestReadDetectionReturnsTheSameTextExplainUsed), so
// this is the one place that reads the window at all.
//
// The window is the whole unwrapped screen, not a fixed bottom slice - each
// rule's own region does the narrowing, the way Herdr's rules expect. That
// bounds this call's cost at rows times cols per pane per tick, never more:
// PlainScreenUnwrapped reads only the live viewport, not the scrollback
// behind it.
func detectionInput(lp *LivePane) (detect.Input, string, error) {
	text, err := lp.Grid.Read(pane.ReadDetection)
	if err != nil {
		return detect.Input{}, "", err
	}
	return detect.Input{
		Screen:      text,
		OSCTitle:    lp.Grid.Title(),
		OSCProgress: lp.Grid.Progress(),
	}, text, nil
}

// StartManifestTick scans pty panes that have no better source, once every
// ManifestTick. It is the fallback of last resort: a pane held back by
// tickSkipReason (done, a gate ask hold, or an ordinary source that spoke
// within GateQuiet) is skipped entirely. set is loaded once by the caller
// (registerAll in cmd/coppice) and handed in here - this never loads its
// own, so a broken override file fails once, at startup, with one named
// warning, not silently every 500 ms forever.
//
// Calling this twice without an intervening StopManifestTick is a no-op:
// only one tick goroutine ever runs per Server. Calling it after Close is
// also a no-op (checked under the same lock as the closed flag itself) - a
// caller racing Close with a start must not spin up a goroutine nothing will
// ever stop, since a second Close takes its early-return branch without
// touching tickStop again.
func (s *Server) StartManifestTick(set *detect.Set) {
	s.mu.Lock()
	if s.closed || s.tickStop != nil {
		s.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	s.tickStop = stop
	s.mu.Unlock()
	s.setDetectSet(set)

	go func() {
		t := time.NewTicker(ManifestTick)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				s.scanOnce(set)
			}
		}
	}()
}

// StopManifestTick stops the tick goroutine started by StartManifestTick, if
// one is running. It is idempotent: calling it with no tick running, or
// twice in a row, does nothing. Server.Close also stops the tick on its own
// (see Close in server.go), so a caller that forgets StopManifestTick does
// not leak the goroutine past Close.
func (s *Server) StopManifestTick() {
	s.mu.Lock()
	stop := s.tickStop
	s.tickStop = nil
	s.mu.Unlock()
	s.setDetectSet(nil)
	if stop != nil {
		close(stop)
	}
}

// ManifestTickRunning reports whether a tick goroutine started by
// StartManifestTick is currently running. A caller that only checks Close
// does not hang proves nothing about whether a tick ever started - Close
// stops one if it exists and is a harmless no-op if it does not, so both
// look identical from the outside. This is the honest signal.
func (s *Server) ManifestTickRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tickStop != nil
}

// tickSkipReason reports why scanOnce would skip this pane's screen right
// now, without ever reading it - a fact about its CURRENT state alone that
// makes the read pointless, because state.Merge (internal/state/state.go)
// would discard whatever a fresh manifest event said anyway. It returns ""
// when nothing holds the pane back and scanning should proceed.
//
// Both scanOnce and pane.explain's tick field call this: what the tick would
// do and what explain reports are the same decision, made once, so the two
// can never drift apart the way a hand-written second copy eventually would.
func (s *Server) tickSkipReason(paneID string, now float64) string {
	// Mirrors scanOnce's own rec.Kind != KindPTY filter (below): a headless
	// pane's state comes from its adapter's events (pumpAdapter, source
	// headless), never from reading its grid against a manifest. scanOnce's
	// loop already skips a headless record before it would ever call this,
	// so this check only ever fires for a caller that does not filter by
	// kind first - pane.explain, which shows what the tick would decide for
	// ANY pane, headless included.
	if rec, ok := s.tree.Pane(paneID); ok && rec.Kind != layout.KindPTY {
		return "skipped: headless pane, not scanned"
	}
	ev, ok := s.states.Current(paneID)
	if !ok {
		return ""
	}
	// Merge rule 2/3: done is terminal. Nothing observed on a screen
	// afterwards is about a running agent.
	if ev.State == proto.StateDone {
		return "skipped: done"
	}
	// Merge rule 4: a gate blocked hold carrying an unexpired ask outranks
	// every other source, INCLUDING a manifest event that has already
	// cleared GateQuiet - the ask's answer arrives through the gate, not
	// through the screen. This hold is not bounded by GateQuiet at all; it
	// lasts until the gate clears it or the ask's own deadline passes, which
	// can be far longer than two seconds. See GateQuiet's own doc comment.
	if ev.State == proto.StateBlocked && ev.Source == proto.SrcGate &&
		ev.Ask != nil && now < ev.Ask.Deadline {
		return fmt.Sprintf("skipped: gate blocked on an ask until %s",
			time.Unix(int64(ev.Ask.Deadline), 0).UTC().Format(time.RFC3339))
	}
	// Merge rule 6: an ordinary (non-ask) hold on a higher-rank source
	// releases after GateQuiet, measured on the store's own receive clock
	// (s.states.Received), never on ev.TS, which is whatever the reporter's
	// own clock said. A hook with a clock a year ahead must not be able to
	// keep the scanner away forever.
	if ev.Source != proto.SrcManifest {
		if received, ok := s.states.Received(paneID); ok && now-received < GateQuiet.Seconds() {
			return fmt.Sprintf("skipped: %s spoke %ds ago", ev.Source, int(now-received))
		}
	}
	return ""
}

// recordTickScanned and recordTickSkipped tally, under s.mu, how many
// pane-visits the manifest tick actually read the screen for versus decided
// not to from tickSkipReason alone. tickCounts is their unexported reader.
// They exist for tests: a merged state that never changed proves nothing
// about which of the two happened, because state.Merge's own precedence
// rules can produce that identical net effect whether or not scanOnce's own
// up-front skip ever fired - see TestAPaneWithAFreshGateSourceIsNotScanned's
// doc comment for the concrete case this covers.
func (s *Server) recordTickScanned() {
	s.mu.Lock()
	s.tickScanned++
	s.mu.Unlock()
}

func (s *Server) recordTickSkipped() {
	s.mu.Lock()
	s.tickSkipped++
	s.mu.Unlock()
}

func (s *Server) tickCounts() (scanned, skipped int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tickScanned, s.tickSkipped
}

// scanOnce is one pass of the manifest tick over every pane in the tree.
//
// Unlike a request handler - which increments s.inflight for the length of
// its call, so Close's waitForHandlers waits for it to finish - this
// goroutine's own scan is never counted in s.inflight at all. Close only
// stops the NEXT tick from firing (by closing tickStop) and this isClosed
// check below skips a scan not yet started; a scan already past this check
// when Close runs can still be mid-loop and call ApplyState after the start
// lock has been released, with nothing in Close waiting for it. That is
// acceptable today only because ApplyState touches nothing but this
// Server's own in-memory tree and state store, neither of which Close frees
// - any future write added to this path that touches something Close DOES
// free (the data directory, a file handle) needs its own guard, not a free
// ride on this one check.
func (s *Server) scanOnce(set *detect.Set) {
	if s.isClosed() {
		return
	}
	now := nowSeconds()
	for _, rec := range s.tree.Panes() {
		if rec.Closed || rec.Kind != layout.KindPTY || rec.Harness == "" {
			continue
		}
		c, ok := set.For(rec.Harness)
		if !ok {
			continue
		}
		if reason := s.tickSkipReason(rec.ID, now); reason != "" {
			s.recordTickSkipped()
			continue
		}
		lp, ok := s.Live(rec.ID)
		if !ok {
			continue
		}
		in, _, err := detectionInput(lp)
		if err != nil {
			// A closed grid (an operator pane.close racing this scan, or the
			// grid closed directly with the tree record left open) or any
			// other read failure just means nothing to report this tick -
			// never a reason to panic or to stop scanning the rest of the
			// tree. Recorded as neither scanned nor skipped: tickSkipReason
			// found no reason to hold this pane back, but the screen still
			// was not actually read, so counting it "scanned" would be a lie
			// the closed-grid test would otherwise let through unnoticed.
			continue
		}
		// Only past every reason not to, and only once the screen has
		// actually been read, does this count as a scan - see the doc
		// comment on recordTickScanned/recordTickSkipped for why that
		// distinction is the whole point of the counters.
		s.recordTickScanned()
		r := c.Evaluate(in)
		// No match means no event, and a skip_state_update rule means no
		// event either: the pane is in a viewer or a menu, and overwriting
		// its state every 500 ms would erase what a real source told us.
		if !r.Matched || r.Skip {
			continue
		}
		id := rec.ID
		s.ApplyState(id, proto.PaneStateEvent{
			V: 1, TS: now, SessionID: id, Harness: rec.Harness, Pane: &id,
			State: string(r.State), Source: proto.SrcManifest,
			Detail: manifestDetail(r),
		})
	}
}

// RegisterExplainCommand wires pane.explain, a debugging window onto BOTH
// halves of a pane's state: the merged fact a reader actually sees
// (effective_state/effective_source, EffectiveState of the current event)
// and the manifest's own opinion of the screen alone (manifest_state), which
// are very often different - a gate hold or an unmatched screen is exactly
// when an operator reaches for this command. It runs the identical
// evaluation scanOnce does, against the identical detection text
// pane.read --source detection returns, shows every rule the evaluator
// visited - id, matched, the region it read - not only the one that won,
// and reports tick: the identical tickSkipReason decision scanOnce itself
// would make for this pane right now. set is loaded once by the caller, the
// same set StartManifestTick is handed; neither loads its own.
func (s *Server) RegisterExplainCommand(set *detect.Set) {
	_ = s.Handle("pane.explain", func(_ *Client, r *proto.Request) proto.Response {
		lp, bad := s.livePane(r)
		if bad != nil {
			return *bad
		}
		id, _ := r.Str("pane")

		// pane_closed, not silently "no harness": s.Live(id) can go missing
		// between livePane's own check above and here, if pane.close races
		// this call on another connection. Reporting "no harness" for a pane
		// that in fact had one, only because it closed a moment ago, would
		// be the wrong verdict - say plainly that it closed instead.
		if _, ok := s.Live(id); !ok {
			return proto.ErrResp(r.ID, proto.ErrPaneClosed,
				fmt.Sprintf("pane %s has closed. Create a new one with: coppice pane create", id))
		}

		in, text, err := detectionInput(lp)
		if err != nil {
			// Mirrors handleRead's own handling: the grid can close out from
			// under this call if pane.close races it on another connection -
			// livePane above found the LivePane, but handleClose can free
			// its Grid between that lookup and this Read.
			if errors.Is(err, pane.ErrGridClosed) {
				return proto.ErrResp(r.ID, proto.ErrPaneClosed,
					fmt.Sprintf("pane %s has closed. Create a new one with: coppice pane create", id))
			}
			return proto.ErrResp(r.ID, proto.ErrInternal, err.Error())
		}

		// The tree record, not lp.Info and not s.paneInfo: the SAME source
		// scanOnce itself reads (rec.Harness from s.tree.Panes()), so
		// explain's verdict about "no manifest for this harness" - and the
		// tick field below, which reuses this exact rec.ID - can never
		// disagree with what the scanner actually used.
		//
		// The s.Live(id) check above does not make this unreachable: the
		// tree and the live map are two different structures under two
		// different locks, and Restore or a forgotten record could in
		// principle leave one without the other. A vanished tree record here
		// gets the same pane_closed answer as a vanished live entry, never
		// the zero-value Pane's empty harness masquerading as "no harness".
		rec, ok := s.tree.Pane(id)
		if !ok {
			return proto.ErrResp(r.ID, proto.ErrPaneClosed,
				fmt.Sprintf("pane %s has closed. Create a new one with: coppice pane create", id))
		}
		harness := rec.Harness

		now := nowSeconds()
		out := map[string]any{
			"pane":               id,
			"harness":            harness,
			"detection_text":     text,
			"osc_title":          in.OSCTitle,
			"osc_progress":       in.OSCProgress,
			"detection_warnings": s.DetectionWarnings(),
		}
		// effective_state/effective_source is the merged fact a reader
		// actually sees - pane.list, agent.get, a cockpit roster - via
		// state.EffectiveState of the current event. manifest_state, set
		// further down, is the manifest's own opinion of the screen ALONE:
		// the two can legitimately disagree (a gate hold, or a screen mid-
		// transition), and that disagreement is exactly what this command
		// exists to show side by side rather than collapse into one verdict.
		if cur, ok := s.states.Current(id); ok {
			eff := state.EffectiveState(&cur, now)
			out["effective_state"] = eff.State
			out["effective_source"] = eff.Source
		}
		if received, ok := s.states.Received(id); ok {
			// One decimal, not the raw float: the tick field a few lines
			// down already truncates its own "<n>s ago" to whole seconds,
			// and a payload showing e.g. 1.9999999713897705 next to "2s ago"
			// would read as two answers disagreeing rather than one number
			// shown two ways.
			out["received_age_s"] = math.Round((now-received)*10) / 10
		}

		if harness == "" {
			// Set before this early return, not only
			// past it - scanOnce's own loop skips a no-harness pane before
			// it ever reaches tickSkipReason, so "skipped: no harness" is
			// that same decision, named here rather than left silent.
			out["tick"] = "skipped: no harness"
			out["note"] = "this pane has no harness, so no manifest applies. " +
				"Create it with --harness NAME to enable screen detection."
			return proto.OKResp(r.ID, out)
		}
		c, ok := set.For(harness)
		if !ok {
			out["note"] = fmt.Sprintf("no manifest for harness %q. Run: coppice agent list", harness)
			return proto.OKResp(r.ID, out)
		}

		res := c.Evaluate(in)
		out["agent"] = res.Agent
		out["source_file"] = set.Source(res.Agent)
		out["matched"] = res.Matched
		out["rule_id"] = res.RuleID
		out["priority"] = res.Priority
		out["region"] = res.Region
		out["manifest_state"] = string(res.State)
		out["skip_state_update"] = res.Skip
		out["evaluated"] = res.Evaluated
		if !res.Matched {
			out["note"] = "no rule matched, so no state is claimed. " +
				"The floor shows unknown rather than guessing idle."
		}

		// tick: what scanOnce would do for this pane right now, from the
		// identical decision scanOnce itself makes - tickSkipReason first,
		// then the same matched/skip_state_update check - never a
		// re-derivation that could drift from the real scanner.
		if reason := s.tickSkipReason(rec.ID, now); reason != "" {
			out["tick"] = reason
		} else if !res.Matched {
			out["tick"] = "skipped: no rule matched"
		} else if res.Skip {
			out["tick"] = "skipped: rule sets skip_state_update"
		} else {
			out["tick"] = "applied"
		}

		return proto.OKResp(r.ID, out)
	})
}

package server

import (
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/pane"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/state"
)

// RegisterAgentCommands wires up pane.report_state and every agent.* verb.
// New calls this itself, before Serve can start, the same way it calls
// RegisterPaneCommands and RegisterAttachCommands - see
// TestNewRegistersTheAgentVerbs.
func (s *Server) RegisterAgentCommands() {
	_ = s.Handle("pane.report_state", s.handleReportState)
	_ = s.Handle("agent.list", s.handleAgentList)
	_ = s.Handle("agent.get", s.handleAgentGet)
	_ = s.Handle("agent.prompt", s.handleAgentPrompt)
	_ = s.Handle("agent.wait", s.handleAgentWait)
	_ = s.Handle("agent.read", s.handleAgentRead)
}

// truncate cuts s to at most n runes. ApplyState uses it to build a
// rejection detail from an arbitrary Validate error, which must itself never
// be able to fail proto's own DetailMax and turn one bad event into two.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// ApplyState merges one event into the pane's state, records it, broadcasts
// it when it is news, and returns the event that was actually stored. This is
// the single entry point: the PTY watcher, the manifest tick, the headless
// adapters and pane.report_state all arrive here, so master spec 3.1 is
// applied once.
//
// It validates first. Only pane.report_state passes through ParseStateEvent
// (which already validates on the way in), so without this an adapter
// emitting Event{Kind: EvState, State: "busy"} directly would have that
// string merged, stored and broadcast, breaking the closed state enum for
// every client. An event that fails validation becomes unknown with the
// reason in detail, which is the fail-closed direction: never idle, never a
// state nobody defined. The substitute keeps the incoming event's own Source
// rather than promoting it to process: the source did observe something, and
// pretending it was the process itself would misattribute a rejection nobody
// but that source produced.
//
// The return value matters as much as the
// broadcast. A caller that built ev from a request and echoes ev back to
// its own caller - handleReportState, before this fix - can otherwise answer
// "ok" with the very state that got rejected, or with a fact Merge's own
// precedence held back (a gate blocked hold outranking an operator report),
// while something else entirely landed in the store. Every caller must reply
// with what THIS function returns, never with the ev it was given.
func (s *Server) ApplyState(paneID string, ev proto.PaneStateEvent) proto.PaneStateEvent {
	if err := ev.Validate(); err != nil {
		harness := ev.Harness
		if harness == "" {
			harness = "unknown"
		}
		id := paneID
		ev = proto.PaneStateEvent{
			V: 1, TS: nowSeconds(), SessionID: paneID, Harness: harness, Pane: &id,
			State: proto.StateUnknown, Source: ev.Source,
			Detail: truncate("rejected state event: "+err.Error(), proto.DetailMax),
		}
		log.Printf("coppice: pane %s sent an invalid state event: %v", paneID, err)
	}
	// The merge's hold window reads the server clock, which a test can
	// move past the window.
	now := float64(s.clock().UnixNano()) / 1e9
	if ev.State == proto.StateBlocked {
		s.noteAsk(paneID, ev.Ask)
	}
	prev, hadPrev := s.states.Current(paneID)
	merged, changed := s.states.Apply(paneID, ev, now)
	if hadPrev && prev.State == proto.StateWorking && turnEnded(merged) {
		s.wentIdle(paneID)
	}
	// The hold is decided before the event goes out, so the first event a
	// floor sees for a held ask already says who holds it.
	esc := s.escalate(paneID, merged)
	s.notifyWaiters(paneID, merged, now)
	if changed {
		s.Broadcast("state", paneID, stateEvent{Event: "state", PaneStateEvent: merged, Held: esc.held})
	}
	if esc.note != "" {
		s.NoteTo(esc.note, paneID, esc.foreman)
	}
	for _, h := range esc.release {
		s.surface(h.pane, h.ask)
	}
	if esc.held != nil {
		s.tellHolds(esc.held.By)
	}
	if merged.State == proto.StateIdle {
		s.tellHolds(paneID)
	}
	// Test-only: see afterApplyState's own doc comment (server.go).
	if hook := s.afterApplyState; hook != nil {
		hook(paneID, merged)
	}
	return merged
}

// stateEvent is the state event on the wire. Held is set only by the
// server, when a foreman hears the ask first. No pane report can set it.
type stateEvent struct {
	Event string `json:"event"`
	proto.PaneStateEvent
	Held *heldWire `json:"held,omitempty"`
}

// handleReportState is the push authority from spec-01. The server downgrades
// an operator claim from a client that is not attached: only a client watching
// the pane can be speaking for the human in front of it.
//
// The limit of that check, stated plainly: a caller can simply send
// source: "gate" and be believed. The socket's uid check is the only
// boundary, and it is the right one for a one-user machine. If coppice ever
// serves more than one uid, this is the function that needs a real
// credential.
func (s *Server) handleReportState(c *Client, r *proto.Request) proto.Response {
	id, ok := r.Str("pane")
	if !ok || id == "" {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			"pane.report_state needs pane. Run: coppice pane list")
	}
	if _, ok := s.tree.Pane(id); !ok {
		return proto.ErrResp(r.ID, proto.ErrNoSuchPane,
			fmt.Sprintf("no pane %q. Run: coppice pane list", id))
	}
	raw, ok := r.Raw("event")
	if !ok {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			"pane.report_state needs event, one PaneStateEvent object.")
	}
	ev, err := proto.ParseStateEvent(raw)
	if err != nil {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, err.Error())
	}
	extras, err := proto.ParseReportExtras(raw)
	if err != nil {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, err.Error())
	}
	ev.Pane = &id
	// Only the server marks an ask as held by a harness: when the pane's
	// live harness owns the ask the report names, as it does when the gate
	// plugin reports the same ask the adapter already did.
	if ev.Ask != nil {
		ev.Ask.Holder = ""
		if s.harnessAsker(id, ev.Ask.ID) != nil {
			ev.Ask.Holder = proto.HolderHarness
		}
	}
	// Only the server names the person behind an event. A name the event
	// carries counts for nothing.
	ev.Who = nil
	downgraded := false
	if ev.Source == proto.SrcOperator && !c.Operator(id) {
		ev.Source = proto.SrcGate
		downgraded = true
		// A blocked claim with no ask is valid as
		// operator (a human needs no ask to say "I'm blocked"), but that
		// same claim is invalid as gate (a gate's blocked claim always
		// carries the ask it is holding). ParseStateEvent already validated
		// the event as it arrived, against its ORIGINAL source, so this is
		// the one mutation in this handler that can turn a valid report into
		// an invalid one - and it must be caught here, by name, rather than
		// handed to ApplyState to quietly reject and store as unknown while
		// this handler still answered ok.
		if err := ev.Validate(); err != nil {
			return proto.ErrResp(r.ID, proto.ErrBadRequest,
				fmt.Sprintf("a gate blocked report needs an ask: %v. "+
					"Attach to this pane to report as operator instead.", err))
		}
	}
	if ev.Source == proto.SrcOperator {
		if name, from := c.Who(); name != "" && from != "pane" && from != "plugin" {
			ev.Who = &name
		}
	}
	// Only the pane's own connection may name a file for the server to read
	// or a session for pane.resume to start: a peer placed in this pane,
	// not a plugin, not a peer nobody could place.
	ro := c.roleOf()
	own := ro.pane && !ro.unknown && ro.plugin == "" && ro.paneID == id
	s.noteReport(id, ev.Harness, extras, own)
	if own && ev.HarnessSessionID != nil {
		s.noteHarnessSession(id, ev.Harness, *ev.HarnessSessionID)
	}
	merged := s.ApplyState(id, ev)
	res := map[string]any{
		"pane": id, "source": merged.Source, "state": merged.State, "detail": merged.Detail,
	}
	if merged.Ask != nil {
		res["ask"] = merged.Ask
	}
	if downgraded {
		res["note"] = "source became gate. Attach to this pane to speak as the operator."
	}
	return proto.OKResp(r.ID, res)
}

// sessionIDOK is the shape a harness session id must have before
// pane.resume may put it on a command line: letters, digits, dot,
// underscore, colon and dash, not led by a dash, at most 128 bytes.
func sessionIDOK(sid string) bool {
	if sid == "" || len(sid) > 128 || sid[0] == '-' {
		return false
	}
	for _, r := range sid {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == ':' || r == '-':
		default:
			return false
		}
	}
	return true
}

// noteHarnessSession stores the harness session id a live pty pane's own
// hook reported, so pane.resume can resume that session once the pane
// ends. A headless pane's adapter already names its session, and its fork
// bookkeeping must not change under it, so only pty panes take this. The
// layout is written only when the id changes, since a gate hook reports on
// every tool call. A report whose harness is not the pane's own names
// some other session, so it sets nothing: pane.resume would run the
// pane's harness on it.
func (s *Server) noteHarnessSession(id, harness, sid string) {
	if !sessionIDOK(sid) {
		return
	}
	rec, ok := s.tree.Pane(id)
	if !ok || rec.Kind != layout.KindPTY || rec.Closed || rec.HarnessSessionID == sid ||
		!sameHarness(harness, rec.Harness) {
		return
	}
	_ = s.updatePane(id, func(p *layout.Pane) {
		if p.Kind == layout.KindPTY && !p.Closed {
			p.HarnessSessionID = sid
		}
	})
}

// waiter lets agent.wait and agent.prompt --wait resolve the moment a state
// arrives, instead of polling. A waiter that nobody satisfies is cleaned up by
// its own timeout or, if the client goes away first, by its cancel channel.
type waiter struct {
	pane  string
	until string
	ch    chan proto.PaneStateEvent
}

// notifyWaiters matches against state.EffectiveState of the merged event,
// not the raw event, so its match rule is exactly WaitState's up-front
// check. Without this, an event that arrives already
// past its own ask's deadline (a delayed or replayed report, or a directly
// constructed test event) reads as blocked here but as working to every
// reader that calls effectiveCurrent - a waiter registered for "working"
// would miss it and sleep to its own timeout instead of resolving at once.
//
// now is the caller's own clock read, not a fresh one taken in here: Apply
// and notifyWaiters judge the same ask deadline for the same event, and two
// independent nowSeconds() calls a heartbeat apart could straddle that
// deadline and disagree about whether it has passed.
func (s *Server) notifyWaiters(paneID string, ev proto.PaneStateEvent, now float64) {
	eff := *state.EffectiveState(&ev, now)
	s.mu.Lock()
	keep := s.waiters[:0]
	for _, w := range s.waiters {
		if w.pane == paneID && (w.until == "" || w.until == eff.State) {
			select {
			case w.ch <- eff:
			default:
			}
			continue
		}
		keep = append(keep, w)
	}
	s.waiters = keep
	s.mu.Unlock()
}

// effectiveCurrent reads a pane's current state through state.EffectiveState,
// so a blocked hold whose ask deadline has already passed reads as working
// here exactly as it does at every other read site (pane.list,
// pane.wait_output). ok is false only when the pane has no state at all yet.
func (s *Server) effectiveCurrent(paneID string) (proto.PaneStateEvent, bool) {
	ev, ok := s.states.Current(paneID)
	if !ok {
		return ev, false
	}
	return *state.EffectiveState(&ev, nowSeconds()), true
}

// WaitState blocks until the pane's merged state equals until, the timeout
// passes, or cancel closes.
//
// The waiter is registered BEFORE the current state is read. Reading first and
// registering after loses any state that arrives between the two, and the
// caller then waits the whole timeout: `agent.wait --until done` on a pane
// that exits a millisecond later would time out spuriously.
//
// cancel is the calling client's c.dead: agent.wait can be asked to hold for
// up to two minutes, and a connection that drops mid-wait must not pin this
// goroutine - and the bounded teardown wait behind it - for anywhere near
// that long. Every return path below deregisters the waiter and drains its
// channel, the cancel path included.
func (s *Server) WaitState(paneID, until string, timeout time.Duration, cancel <-chan struct{}) (proto.PaneStateEvent, bool) {
	w := &waiter{pane: paneID, until: until, ch: make(chan proto.PaneStateEvent, 1)}
	s.mu.Lock()
	s.waiters = append(s.waiters, w)
	s.mu.Unlock()

	drop := func() {
		s.mu.Lock()
		keep := s.waiters[:0]
		for _, x := range s.waiters {
			if x != w {
				keep = append(keep, x)
			}
		}
		s.waiters = keep
		s.mu.Unlock()
		// Drain, so a notify that raced with the deregistration is not left
		// sitting in a channel nobody reads.
		select {
		case <-w.ch:
		default:
		}
	}

	if ev, ok := s.effectiveCurrent(paneID); ok && (until == "" || ev.State == until) {
		drop()
		return ev, true
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case ev := <-w.ch:
		drop()
		return ev, true
	case <-timer.C:
		drop()
		ev, _ := s.effectiveCurrent(paneID)
		return ev, false
	case <-cancel:
		drop()
		ev, _ := s.effectiveCurrent(paneID)
		return ev, false
	}
}

// agentRow is the one place agent.get, agent.list and agent.read each build
// their view of a pane, so all three agree on shape and all three read
// through EffectiveState the same way pane.list does.
func (s *Server) agentRow(paneID string) (map[string]any, bool) {
	rec, ok := s.tree.Pane(paneID)
	if !ok {
		return nil, false
	}
	row := map[string]any{
		"pane": rec.ID, "label": rec.Label, "cwd": rec.Cwd, "kind": string(rec.Kind),
		"harness": rec.Harness, "closed": rec.Closed,
		"state": proto.StateUnknown, "source": nil, "detail": "",
	}
	if rec.Closed {
		row["ended_at"] = rec.EndedAt
		if rec.ExitCode != nil {
			row["exit_code"] = *rec.ExitCode
		} else {
			row["exit_code"] = nil
		}
	}
	if rec.ParentPane != "" {
		row["parent_pane"] = rec.ParentPane
	}
	if eff, ok := s.effectiveCurrent(rec.ID); ok {
		row["state"] = eff.State
		row["source"] = eff.Source
		row["detail"] = eff.Detail
		// Copied from the stored merged
		// event, never a receive-time stamp - EffectiveState never touches
		// either field, so eff.TS and eff.SessionID are always the ones the
		// event itself carried.
		row["ts"] = eff.TS
		row["session_id"] = eff.SessionID
		if eff.Ask != nil {
			row["ask"] = eff.Ask
		}
		if eff.HarnessSessionID != nil {
			row["harness_session_id"] = *eff.HarnessSessionID
		} else if rec.HarnessSessionID != "" {
			// The state event does not always carry one
			// - markRestored's does not, and neither does a resumed
			// adapter's own first event before it speaks - but the tree
			// record, which Restore itself read it from, always does.
			row["harness_session_id"] = rec.HarnessSessionID
		}
	} else if rec.Closed {
		// No stored event at all, yet the record itself says closed: the
		// same restart case paneListRow's own fallback handles. See
		// endedFallbackState.
		state, source, detail, ts := endedFallbackState(rec)
		row["state"] = state
		row["source"] = source
		row["detail"] = detail
		row["ts"] = ts
		row["session_id"] = rec.ID
		if rec.HarnessSessionID != "" {
			row["harness_session_id"] = rec.HarnessSessionID
		}
	}
	return row, true
}

// handleAgentList follows pane.list's own rule: live records only, or
// with `ended: true`, only ended ones, newest first. agent.get and
// agent.read do not filter this way - an ended pane still answers either,
// since its grid stays readable until it is forgotten or resumed.
func (s *Server) handleAgentList(_ *Client, r *proto.Request) proto.Response {
	wantEnded, _ := r.Bool("ended")
	matched := make([]layout.Pane, 0)
	for _, p := range s.tree.Panes() {
		if p.Closed != wantEnded {
			continue
		}
		matched = append(matched, p)
	}
	if wantEnded {
		sortEndedNewestFirst(matched)
	}
	out := []map[string]any{}
	for _, p := range matched {
		if row, ok := s.agentRow(p.ID); ok {
			out = append(out, row)
		}
	}
	return proto.OKResp(r.ID, map[string]any{"agents": out})
}

func (s *Server) handleAgentGet(_ *Client, r *proto.Request) proto.Response {
	id, _ := r.Str("pane")
	row, ok := s.agentRow(id)
	if !ok {
		return proto.ErrResp(r.ID, proto.ErrNoSuchPane,
			fmt.Sprintf("no pane %q. Run: coppice agent list", id))
	}
	return proto.OKResp(r.ID, row)
}

func (s *Server) handleAgentPrompt(c *Client, r *proto.Request) proto.Response {
	text, ok := r.Str("text")
	if !ok {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, "agent.prompt needs text.")
	}
	lp, bad := s.livePane(r)
	if bad != nil {
		return *bad
	}
	// id comes from the request, not lp.Info.ID: refreshLiveInfo (called by
	// updatePane, among others - see pane.resize) writes lp.Info under
	// liveMu from whatever goroutine is resizing this pane on a different
	// connection, so reading the field straight off lp without that lock is
	// exactly the race paneInfo/attach.go's own id-from-request pattern
	// exists to avoid. lp.Adapter, lp.PTY and lp.Grid are untouched by this -
	// startPane sets them once and refreshLiveInfo only ever rewrites Info.
	id, _ := r.Str("pane")
	if c.viewOnlyOn(id) {
		return refuseViewOnly(r, id)
	}
	if lp.Adapter != nil {
		// A harness that holds asks of its own learns who typed, so a
		// pane's text can never answer one.
		var err error
		if pp, ok := lp.Adapter.(pane.Prompter); ok {
			err = pp.PromptFrom(text, !c.roleOf().noAllow)
		} else {
			err = lp.Adapter.Prompt(text)
		}
		if err != nil {
			return proto.ErrResp(r.ID, proto.ErrAdapter, err.Error())
		}
	} else if s.gated(id) {
		// A sender that still waits when a drop comes hears of it as its
		// error, and a drop after it gave up tells it as for any sender.
		// See queuedPrompt.wait.
		out, err := s.submitPrompt(id, text, senderPane(c), c)
		if err != nil {
			return promptErr(r, err)
		}
		if !out.sent {
			return s.queuedPromptReply(c, r, id, text, out)
		}
	} else if err := lp.write([]byte(text + "\r")); err != nil {
		return proto.ErrResp(r.ID, proto.ErrInternal, err.Error())
	}
	s.markInput(id, true)
	return s.promptWait(c, r, id, len(text), nil)
}

// promptWait answers agent.prompt once its text went to pane id. With
// wait, it waits for the state the request names. extra goes in the
// reply.
func (s *Server) promptWait(c *Client, r *proto.Request, id string, sent int, extra map[string]any) proto.Response {
	wait, _ := r.Bool("wait")
	if !wait {
		out := map[string]any{"pane": id, "sent": sent}
		for k, v := range extra {
			out[k] = v
		}
		return proto.OKResp(r.ID, out)
	}
	until, ok := r.Str("until")
	if !ok {
		until = proto.StateIdle
	}
	ms, ok := r.Int("timeout_ms")
	if !ok || ms <= 0 {
		ms = 120000
	}
	ev, done := s.WaitState(id, until, time.Duration(ms)*time.Millisecond, c.dead)
	if !done {
		if c.Dead() {
			return proto.ErrResp(r.ID, proto.ErrTimeout, "client disconnected while waiting")
		}
		return proto.ErrResp(r.ID, proto.ErrTimeout,
			fmt.Sprintf("pane %s is %s after %d ms, not %s. Run: coppice agent get %s",
				id, orUnknown(ev.State), ms, until, id))
	}
	return proto.OKResp(r.ID, waitReply(id, ev))
}

func orUnknown(s string) string {
	if s == "" {
		return proto.StateUnknown
	}
	return s
}

// waitReply is the {pane, state, source, ask} shape agent.wait and
// agent.prompt --wait both answer with on success. ask is omitted when
// nil, the same treatment agentRow already gives it - the two handlers
// used to always include it, so a pane with no open ask printed the
// literal Go string "<nil>" through printHuman.
func waitReply(pane string, ev proto.PaneStateEvent) map[string]any {
	out := map[string]any{"pane": pane, "state": ev.State, "source": ev.Source}
	if ev.Ask != nil {
		out["ask"] = ev.Ask
	}
	return out
}

func (s *Server) handleAgentWait(c *Client, r *proto.Request) proto.Response {
	id, _ := r.Str("pane")
	if _, ok := s.tree.Pane(id); !ok {
		return proto.ErrResp(r.ID, proto.ErrNoSuchPane,
			fmt.Sprintf("no pane %q. Run: coppice agent list", id))
	}
	until, ok := r.Str("until")
	if !ok {
		until = proto.StateIdle
	}
	ms, ok := r.Int("timeout_ms")
	if !ok || ms <= 0 {
		ms = 120000
	}
	ev, done := s.WaitState(id, until, time.Duration(ms)*time.Millisecond, c.dead)
	if !done {
		if c.Dead() {
			return proto.ErrResp(r.ID, proto.ErrTimeout, "client disconnected while waiting")
		}
		return proto.ErrResp(r.ID, proto.ErrTimeout,
			fmt.Sprintf("pane %s is %s after %d ms, not %s. Run: coppice agent get %s",
				id, orUnknown(ev.State), ms, until, id))
	}
	return proto.OKResp(r.ID, waitReply(id, ev))
}

// handleAgentRead deliberately does not go through livePane: a pane
// whose process exited keeps its grid, so its final screen stays readable,
// and only an operator pane.close frees it and removes the live entry. This
// mirrors handleRead in panes.go exactly - tree existence for no_such_pane,
// s.Live for pane_closed, then Grid.Read, where ErrGridClosed (a pane.close
// racing this call between the Live lookup and the read) also maps to
// pane_closed and any other read error is bad_request - rather than
// livePane's stricter rec.Closed check, which would refuse an exited-but-
// still-readable agent's final screen with pane_closed before ever reading
// it.
//
// Within the agent verbs, source means the pane's STATE source
// everywhere (agent.get, agent.list, and agentRow's own row) - so agent.read
// answers the grid window it just read under a key of its own, region, and
// never touches row["source"]. The request still accepts source too (region
// takes precedence when both are given), matching pane.read's own request
// shape for a caller migrating between the two verbs; only the RESPONSE key
// changed. pane.read (handleRead, panes.go) is untouched: it keeps "source"
// on both its request and its response, since it has no state fields to
// collide with.
func (s *Server) handleAgentRead(_ *Client, r *proto.Request) proto.Response {
	id, ok := r.Str("pane")
	if !ok || id == "" {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, "this command needs pane. Run: coppice agent list")
	}
	if _, ok := s.tree.Pane(id); !ok {
		return proto.ErrResp(r.ID, proto.ErrNoSuchPane,
			fmt.Sprintf("no pane %q. Run: coppice agent list", id))
	}
	lp, ok := s.Live(id)
	if !ok {
		return proto.ErrResp(r.ID, proto.ErrPaneClosed,
			fmt.Sprintf("pane %s has closed. Create a new one with: coppice pane create", id))
	}
	region := pane.ReadRecent
	if v, ok := r.Str("region"); ok {
		region = pane.ReadSource(v)
	} else if v, ok := r.Str("source"); ok {
		region = pane.ReadSource(v)
	}
	text, err := lp.Grid.Read(region)
	if err != nil {
		if errors.Is(err, pane.ErrGridClosed) {
			return proto.ErrResp(r.ID, proto.ErrPaneClosed,
				fmt.Sprintf("pane %s has closed. Create a new one with: coppice pane create", id))
		}
		return proto.ErrResp(r.ID, proto.ErrBadRequest, err.Error())
	}
	// agentRow's own ok is no longer discarded.
	// The tree check above already found this pane, so this should always
	// succeed - nothing in this package ever deletes a tree record - but a
	// caller that skipped straight to a nil-map write on a future ok=false
	// would panic instead of answering no_such_pane.
	row, ok := s.agentRow(id)
	if !ok {
		return proto.ErrResp(r.ID, proto.ErrNoSuchPane,
			fmt.Sprintf("no pane %q. Run: coppice agent list", id))
	}
	row["text"] = text
	// row["source"] is left exactly as agentRow set it - the pane's STATE
	// source, same key/meaning as agent.get and agent.list. The grid region
	// just read gets its own key, so a client switching on "source" across
	// all three agent verbs sees one meaning everywhere.
	row["region"] = string(region)
	return proto.OKResp(r.ID, row)
}

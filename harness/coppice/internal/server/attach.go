// Package server (this file): pane.attach, pane.detach and events.subscribe.
// Client, its bounded event queue, Emit, Drop, Dead, Operator and Broadcast
// already exist in server.go - this file only adds what watches a
// pane and turns its grid into frames on the wire.
package server

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/pane"
	"github.com/opendaisugi/coppice/internal/proto"
)

// FrameInterval is the 60 Hz ceiling from spec-02. A harness redrawing a
// spinner must not turn into a frame per redraw on the wire.
const FrameInterval = 16 * time.Millisecond

// FrameBuffer is how many frames one attached pane may hold for one client
// before the pump drops new frames instead of waiting. It is a variable so
// a test can lower it and overflow it with a short burst.
var FrameBuffer = 64

// paneFlow is the frame path for one attached pane on one client. The pump
// is the only sender on frames and the only goroutine that closes it. The
// drain is the only reader. paused, wantFull and awaitFull sit under mu and
// are read in short sections only, so the pump never waits on a write.
// sendMu covers the drain's check of paused together with its write, so
// once events.pause has answered, no frame for the pane follows.
type paneFlow struct {
	frames chan queuedFrame

	mu        sync.Mutex
	paused    bool
	wantFull  bool // the pump builds a full frame next
	awaitFull bool // the drain discards deltas until a full frame arrives
	pending   int  // drops the drain discarded with a frame, owed to the client

	sendMu sync.Mutex
}

// queuedFrame is one frame on its way to the drain. dropped is how many
// frames the pump dropped before this one, so the drain can say so first.
type queuedFrame struct {
	frame   proto.Frame
	dropped int
}

func newPaneFlow() *paneFlow {
	return &paneFlow{frames: make(chan queuedFrame, FrameBuffer)}
}

func (s *Server) RegisterAttachCommands() {
	_ = s.Handle("pane.attach", s.handleAttach)
	_ = s.Handle("pane.detach", s.handleDetach)
	_ = s.Handle("events.subscribe", s.handleSubscribe)
}

func (s *Server) handleAttach(c *Client, r *proto.Request) proto.Response {
	lp, bad := s.livePane(r)
	if bad != nil {
		return *bad
	}
	// id comes from the request, not lp.Info.ID: refreshLiveInfo (called by
	// updatePane below, among others) writes lp.Info under liveMu from
	// whatever goroutine is resizing a pane, so reading the field straight
	// off lp without that lock is exactly the race paneInfo exists to avoid.
	id, _ := r.Str("pane")
	// A view-only attach watches the pane at the size it has. It never
	// resizes, and the connection may not write to the pane afterwards.
	viewOnly, _ := r.Bool("view_only")
	fit, hasFit, badFit := fitOf(r)
	if badFit != nil {
		return *badFit
	}
	// fitMu covers the whole attach, so two clients that attach at once
	// cannot both find the pane free.
	// The record's new size goes to disk after the fit lock is let go
	// (CP-R-53): a save waits on a slow disk, and the lock held through it
	// made every other attach wait too, past its 5 s ack. The full attach
	// saves before it tells presence; this defer, which runs after
	// unlockFit's, saves for the attach that was already in place.
	needSave := false
	defer func() {
		if needSave {
			s.saveLayout()
		}
	}()
	s.fitMu.Lock()
	fitUnlocked := false
	unlockFit := func() {
		if !fitUnlocked {
			fitUnlocked = true
			s.fitMu.Unlock()
		}
	}
	defer unlockFit()
	// Another client's fit ends the moment this client attaches.
	s.restoreFitLocked(id, c)
	fitted := false
	if cols, okc := r.Int("cols"); okc && !viewOnly {
		if rows, okr := r.Int("rows"); okr && cols > 0 && rows > 0 {
			s.resizeLocked(id, lp, cols, rows)
			needSave = true
		}
	}
	// A client whose fit a resize ended is not fitted again until it
	// detaches and attaches fresh.
	c.mu.Lock()
	refused := c.fitEnded[id]
	c.mu.Unlock()
	if hasFit && !viewOnly && !refused {
		if held, mine := s.fits[id]; mine && held.client == c {
			resizeLive(lp, fit[0], fit[1])
			fitted = true
		} else if !s.othersAttached(id, c) {
			if info, ok := s.paneInfo(id); ok {
				if s.fits == nil {
					s.fits = map[string]fitHold{}
				}
				s.fits[id] = fitHold{client: c, cols: info.Cols, rows: info.Rows}
				resizeLive(lp, fit[0], fit[1])
				fitted = true
			}
		}
	}
	reply := func() proto.Response {
		res := map[string]any{"pane": id, "attached": true}
		if hasFit {
			res["fitted"] = fitted
		}
		return proto.OKResp(r.ID, res)
	}

	c.mu.Lock()
	if viewOnly {
		c.viewOnly[id] = true
	} else {
		delete(c.viewOnly, id)
	}
	if _, already := c.Attached[id]; already {
		c.mu.Unlock()
		return reply()
	}
	fs := pane.NewFrameState()
	c.Attached[id] = fs
	flow := newPaneFlow()
	c.flows[id] = flow
	// Attaching also subscribes this client to the pane's state events, so a
	// renderer does not need a second call to show the status line.
	c.Subs["state"] = true
	c.Subs["frame"] = true
	// This pane's SubPanes entry is recorded as attach-added only if nothing
	// already covers it: a prior explicit events.subscribe naming this pane,
	// or "*", must survive a later pane.detach undisturbed - only the
	// subscription attach itself is creating here should be attach's own to
	// undo.
	if !c.SubPanes["*"] && !c.SubPanes[id] {
		c.autoPanes[id] = true
	}
	c.SubPanes[id] = true
	c.mu.Unlock()

	go s.framePump(c, id, fs, flow)
	unlockFit()
	// The save comes before the presence event, as in the Rust server.
	if needSave {
		needSave = false
		s.saveLayout()
	}
	s.tellPresence(id)
	return reply()
}

// fitHold is the size a pane had before a client fitted it, and the client
// that holds the fit.
type fitHold struct {
	client     *Client
	cols, rows int
}

// FitMaxCols and FitMaxRows cap a fit. A phone never needs more.
const (
	FitMaxCols = 1000
	FitMaxRows = 500
)

// fitOf reads the fit of a pane.attach. ok is false when the request has
// none. A fit that is not an object with whole cols from 1 to FitMaxCols
// and rows from 1 to FitMaxRows is a bad_request.
func fitOf(r *proto.Request) (size [2]int, ok bool, bad *proto.Response) {
	raw, has := r.Raw("fit")
	if !has {
		return size, false, nil
	}
	var f struct {
		Cols *int `json:"cols"`
		Rows *int `json:"rows"`
	}
	if err := json.Unmarshal(raw, &f); err != nil || f.Cols == nil || f.Rows == nil ||
		*f.Cols <= 0 || *f.Rows <= 0 || *f.Cols > FitMaxCols || *f.Rows > FitMaxRows {
		resp := proto.ErrResp(r.ID, proto.ErrBadRequest, "fit needs cols from 1 to 1000 and rows from 1 to 500.")
		return size, false, &resp
	}
	return [2]int{*f.Cols, *f.Rows}, true, nil
}

// resizeLive sets the size of a pane's terminal and grid and nothing else.
// A fit and its restore use it, so the layout record keeps the owner's size
// and a crash or restart never saves the phone's.
func resizeLive(lp *LivePane, cols, rows int) {
	if lp.PTY != nil {
		_ = lp.PTY.Resize(cols, rows)
	} else {
		_ = lp.Grid.Resize(cols, rows)
	}
}

// resizeLocked sets a pane's size and its record in memory. The caller
// holds fitMu, and saves the layout once it let go of it.
func (s *Server) resizeLocked(id string, lp *LivePane, cols, rows int) {
	resizeLive(lp, cols, rows)
	_ = s.setRecord(id, func(x *layout.Pane) { x.Cols, x.Rows = cols, rows })
}

// othersAttached is true when a client other than c is attached to pane id,
// a view included.
func (s *Server) othersAttached(id string, c *Client) bool {
	s.mu.Lock()
	clients := make([]*Client, 0, len(s.clients))
	for o := range s.clients {
		clients = append(clients, o)
	}
	s.mu.Unlock()
	for _, o := range clients {
		if o == c || o.Dead() {
			continue
		}
		o.mu.Lock()
		_, on := o.Attached[id]
		o.mu.Unlock()
		if on {
			return true
		}
	}
	return false
}

// restoreFitLocked puts the old size back when a client other than c holds
// the fit of pane id. With c nil it puts it back whoever holds it. The
// caller holds fitMu.
func (s *Server) restoreFitLocked(id string, c *Client) {
	held, ok := s.fits[id]
	if !ok || (c != nil && held.client == c) {
		return
	}
	delete(s.fits, id)
	if lp, live := s.Live(id); live {
		resizeLive(lp, held.cols, held.rows)
	}
}

// releaseFit puts the old size back when c holds the fit of pane id, as the
// client detaches or ends. It also clears the mark a resize left, so the
// client's next attach is fresh and may fit again. Both happen under fitMu,
// so a resize cannot mark the client after the mark is cleared.
func (s *Server) releaseFit(id string, c *Client) {
	s.fitMu.Lock()
	defer s.fitMu.Unlock()
	c.mu.Lock()
	delete(c.fitEnded, id)
	c.mu.Unlock()
	if held, ok := s.fits[id]; ok && held.client == c {
		s.restoreFitLocked(id, nil)
	}
}

// looking is the sorted names of the people attached to pane id now: each
// live operator connection with a name, once per name. A connection with
// no name, a pane and a plugin are not people, so none of them shows. A
// view-only attach counts, so a view on the floor page counts for the
// page's own name.
func (s *Server) looking(id string) []string {
	s.mu.Lock()
	clients := make([]*Client, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()
	seen := map[string]bool{}
	out := []string{}
	for _, c := range clients {
		if c.Dead() {
			continue
		}
		c.mu.Lock()
		_, on := c.Attached[id]
		c.mu.Unlock()
		if !on {
			continue
		}
		name, from := c.Who()
		if name == "" || from == "pane" || from == "plugin" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// presenceEvent says who looks at a pane now.
type presenceEvent struct {
	Event   string   `json:"event"`
	Pane    string   `json:"pane"`
	Looking []string `json:"looking"`
}

// tellPresence sends who looks at pane id to every client that subscribed
// to presence. It works out the list and queues the event under one lock,
// so a list worked out earlier never reaches a client after a later one.
func (s *Server) tellPresence(id string) {
	s.presenceMu.Lock()
	defer s.presenceMu.Unlock()
	ev := presenceEvent{Event: "presence", Pane: id, Looking: s.looking(id)}
	if gap := s.presenceGap; gap != nil {
		gap(id)
	}
	s.Broadcast("presence", id, ev)
}

// framePump is one goroutine per attached client per pane. It wakes on a
// fixed tick rather than on every byte, which is what makes the 60 Hz
// ceiling real instead of aspirational.
//
// c.Dead() is checked first on every tick, not only after a changed frame is
// found: a pane whose screen has gone quiet (a child asleep, a prompt with
// nothing left to say) would otherwise never reach that check again, and this
// goroutine would tick forever after its client disconnected. Finding the
// client dead here, and on every other pane-gone branch below (a missing
// live entry, an ErrGridClosed read), is also this pump's only chance to
// clear its own entry out of c.Attached - nothing else ever will for a
// client that vanished without calling pane.detach, or for a pane an
// operator pane.close already closed and freed out from under it - which is
// what keeps Client.Operator from reporting true for a pane no one is
// watching, or that no longer exists, anymore.
//
// The c.Attached[paneID] == fs identity guard buys exactly one thing: a
// stale pump - one whose client detached and reattached to the same pane
// before this tick - cannot clobber the new attach's c.Attached entry, since
// its own fs no longer matches what is stored there, so its forget() below is
// a no-op and its return leaves the new pump's entry alone. It does NOT mean
// a stale pump never runs again once it goes stale: the tick that notices
// !still can be preceded by one that does not, in which case this pump
// already computed and emitted one last frame against fs's OLD baseline
// before the next tick catches up - a stray diff a client may see once, not
// a correctness bug in what c.Attached ends up holding.
//
// Frames go to the drain through flow.frames, never through c.Emit. When
// the buffer is full the pump drops the frame, counts it, and forces the
// next frame to be full. The frame that then fits carries the count, and
// the drain sends a frame_gap event before it. While the client has paused
// this pane the pump builds nothing. On resume it builds one full frame.
//
// This pump is the only sender on flow.frames, so it closes the channel
// when it returns and the drain ends with it.
func (s *Server) framePump(c *Client, paneID string, fs *pane.FrameState, flow *paneFlow) {
	t := time.NewTicker(FrameInterval)
	defer t.Stop()
	defer close(flow.frames)
	go c.drainFrames(flow)
	forget := func() {
		c.mu.Lock()
		was := c.Attached[paneID] == fs
		if was {
			delete(c.Attached, paneID)
			delete(c.flows, paneID)
			delete(c.viewOnly, paneID)
		}
		c.mu.Unlock()
		if was {
			s.releaseFit(paneID, c)
			s.tellPresence(paneID)
		}
	}
	dropped := 0
	for range t.C {
		if c.Dead() {
			forget()
			return
		}
		c.mu.Lock()
		still := c.Attached[paneID] == fs
		c.mu.Unlock()
		if !still {
			return
		}
		lp, ok := s.Live(paneID)
		if !ok {
			forget()
			return
		}
		flow.mu.Lock()
		paused, wantFull := flow.paused, flow.wantFull
		if !paused {
			flow.wantFull = false
		}
		flow.mu.Unlock()
		if paused {
			continue
		}
		if wantFull {
			fs.ForceFull()
		}
		frame, changed, err := fs.Next(paneID, lp.Grid)
		if err != nil {
			// pane.ErrGridClosed (an operator pane.close raced this pump) or
			// any other read failure: the pane is gone as far as this pump
			// is concerned, so stop rather than spin on a broken grid.
			forget()
			return
		}
		if !changed {
			continue
		}
		select {
		case flow.frames <- queuedFrame{frame: frame, dropped: dropped}:
			dropped = 0
		default:
			// The client has not read FrameBuffer frames. This one is lost.
			// The next frame that fits must be full, because the client
			// cannot apply a delta on top of frames it never saw.
			dropped++
			flow.mu.Lock()
			flow.wantFull = true
			flow.mu.Unlock()
		}
	}
}

// drainFrames writes one pane's frames to the wire, in the client's framing,
// until the pump closes the channel. A frame that arrives while the pane is
// paused is discarded. After a resume, deltas are discarded until the full
// frame the pump was told to build arrives. A frame that follows drops is
// announced by a frame_gap event first. A discarded frame's drop count is
// kept and added to the next frame that is sent, so dropped counts every
// frame the pump dropped. A frame discarded here under a pause or before the
// full frame is not counted. A write failure drops the client, as the events
// pump does.
func (c *Client) drainFrames(flow *paneFlow) {
	for q := range flow.frames {
		flow.sendMu.Lock()
		flow.mu.Lock()
		discard := flow.paused || (flow.awaitFull && !q.frame.Full)
		if discard {
			flow.pending += q.dropped
		} else {
			flow.awaitFull = false
			q.dropped += flow.pending
			flow.pending = 0
		}
		flow.mu.Unlock()
		if discard {
			flow.sendMu.Unlock()
			continue
		}
		ok := true
		if q.dropped > 0 {
			ok = c.sendEvent(proto.FrameGap{Event: "frame_gap", Pane: q.frame.Pane, Dropped: q.dropped})
		}
		if ok {
			ok = c.sendEvent(q.frame)
		}
		flow.sendMu.Unlock()
		if !ok {
			c.Drop()
			return
		}
	}
}

// sendEvent writes one event line in this connection's framing. It reports
// false when the event could not be encoded or written.
func (c *Client) sendEvent(ev any) bool {
	b := proto.EncodeEvent(ev, c.Framing())
	return b != nil && c.Enc.SendBytes(b) == nil
}

// flowFor returns the frame path this client holds for a pane, or a
// not_attached reply. events.pause and events.resume act on this
// connection only, so a pane the connection has not attached is refused
// with the same teaching text pane.detach gives.
func (c *Client) flowFor(r *proto.Request) (*paneFlow, string, *proto.Response) {
	id, ok := r.Str("pane")
	if !ok || id == "" {
		resp := proto.ErrResp(r.ID, proto.ErrBadRequest, "this command needs pane. Run: coppice pane list")
		return nil, "", &resp
	}
	c.mu.Lock()
	flow := c.flows[id]
	c.mu.Unlock()
	if flow == nil {
		resp := proto.ErrResp(r.ID, proto.ErrNotAttached,
			fmt.Sprintf("this client is not attached to %s. Run: coppice attach %s", id, id))
		return nil, id, &resp
	}
	return flow, id, nil
}

// handlePause stops frames for one pane on this connection. It takes
// sendMu so a frame the drain is writing at this moment lands before the
// reply, and no frame follows the reply until events.resume.
func (s *Server) handlePause(c *Client, r *proto.Request) proto.Response {
	flow, id, bad := c.flowFor(r)
	if bad != nil {
		return *bad
	}
	flow.sendMu.Lock()
	flow.mu.Lock()
	flow.paused = true
	flow.mu.Unlock()
	flow.sendMu.Unlock()
	return proto.OKResp(r.ID, map[string]any{"pane": id, "paused": true})
}

// handleResume lets frames flow again. The next frame is a full one, and
// the drain discards any delta that was still queued from before the pause.
func (s *Server) handleResume(c *Client, r *proto.Request) proto.Response {
	flow, id, bad := c.flowFor(r)
	if bad != nil {
		return *bad
	}
	flow.mu.Lock()
	flow.paused = false
	flow.wantFull = true
	flow.awaitFull = true
	flow.mu.Unlock()
	return proto.OKResp(r.ID, map[string]any{"pane": id, "paused": false})
}

func (s *Server) handleDetach(c *Client, r *proto.Request) proto.Response {
	id, _ := r.Str("pane")
	c.mu.Lock()
	_, was := c.Attached[id]
	delete(c.Attached, id)
	delete(c.flows, id)
	delete(c.viewOnly, id)
	// Narrow the subscription only when this call actually detached
	// something. A pane the frame pump had already forgotten on its own (a
	// pane-gone branch: an operator pane.close on another connection, say)
	// leaves was false here - this pane.detach did not do the detaching,
	// framePump did, moments or seconds earlier - and the auto subscription
	// it set up must survive exactly as it would for any other client that
	// never called pane.detach at all: until this client disconnects, or
	// explicitly narrows it with events.subscribe. Doing this unconditionally
	// used to mean a detach racing watchExit's own pane-gone cleanup came
	// back not_attached AND silently dropped the subscription anyway.
	if was && c.autoPanes[id] {
		delete(c.SubPanes, id)
		delete(c.autoPanes, id)
	}
	c.mu.Unlock()
	if !was {
		return proto.ErrResp(r.ID, proto.ErrNotAttached,
			fmt.Sprintf("this client is not attached to %s. Run: coppice attach %s", id, id))
	}
	s.releaseFit(id, c)
	s.tellPresence(id)
	return proto.OKResp(r.ID, map[string]any{"pane": id, "attached": false})
}

// handleSubscribe lets a client ask for event kinds beyond what pane.attach
// already gave it: state and layout for any set of panes, or every pane via
// "*". frame is accepted as a kind here too, but only pane.attach ever
// starts a frame pump - subscribing to "frame" for a pane this client has
// not attached is inert until an attach happens, since nothing in this
// server sends a frame event for a pane with no running pump behind it.
func (s *Server) handleSubscribe(c *Client, r *proto.Request) proto.Response {
	const panesHint = `panes must be "*" or a non-empty list of pane ids`
	kinds, ok := r.StrSlice("kinds")
	if !ok || len(kinds) == 0 {
		kinds = []string{"state", "layout"}
	}
	for _, k := range kinds {
		switch k {
		case "state", "layout", "frame", "note", "child", "presence", "messages":
		default:
			return proto.ErrResp(r.ID, proto.ErrBadRequest,
				fmt.Sprintf("kind %q is not state, layout, frame, note, child, presence or messages", k))
		}
	}
	var panes []string
	if raw, ok := r.Raw("panes"); ok {
		var one string
		if err := json.Unmarshal(raw, &one); err == nil {
			panes = []string{one}
		} else if err := json.Unmarshal(raw, &panes); err != nil {
			return proto.ErrResp(r.ID, proto.ErrBadRequest, panesHint)
		}
	} else {
		panes = []string{"*"}
	}
	// Degenerate values - an empty list, an empty string, a JSON null (which
	// unmarshals into the "one string" branch above as "", same as an
	// explicit "") - must be refused rather than silently subscribing to
	// nothing: a caller who typo'd an empty panes value almost certainly
	// wanted something, not a subscription that will never deliver.
	if len(panes) == 0 {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, panesHint)
	}
	for _, p := range panes {
		if p == "" {
			return proto.ErrResp(r.ID, proto.ErrBadRequest, panesHint)
		}
	}
	c.mu.Lock()
	for _, k := range kinds {
		c.Subs[k] = true
	}
	for _, p := range panes {
		c.SubPanes[p] = true
		// This subscription is now explicit for p, so pane.detach must not
		// treat it as attach's to undo any more - see handleDetach. "*"
		// explicitly covers every pane, including ones only ever marked
		// auto by a pane.attach that ran before this call.
		if p == "*" {
			for id := range c.autoPanes {
				delete(c.autoPanes, id)
			}
		} else {
			delete(c.autoPanes, p)
		}
	}
	c.mu.Unlock()
	for _, k := range kinds {
		if k == "messages" {
			s.watchMessages()
		}
	}
	return proto.OKResp(r.ID, map[string]any{"kinds": kinds, "panes": panes})
}

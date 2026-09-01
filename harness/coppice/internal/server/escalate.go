package server

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/opendaisugi/coppice/internal/proto"
)

// DefaultHoldFor is how long a foreman holds a child's undoable ask before
// it goes to the operator.
const DefaultHoldFor = 120 * time.Second

// holdMargin is how long before the gate gives up on an ask a hold must
// end, so the operator still has time to answer it.
const holdMargin = 30 * time.Second

// surfacedKept is how many surfaced ask ids the server keeps per pane.
const surfacedKept = 64

// hold is one ask a foreman hears before the operator does.
type hold struct {
	pane, ask     string
	foreman, task string
	since, until  float64
	timer         *time.Timer
	// id is the short name the server gives the hold. The typed line and
	// agent.deny use it, so no text the child chose has to reach the
	// foreman.
	id string
	// told is true once the foreman's one typed line was written. sending
	// is true while a write of it runs.
	told, sending bool
}

// heldWire is the held field on a state event and a pane.list row: who
// holds the ask, for which task, and until when.
type heldWire struct {
	Hold         string  `json:"hold"`
	By           string  `json:"by"`
	ForemanLabel string  `json:"foreman_label"`
	Task         string  `json:"task"`
	TaskLabel    string  `json:"task_label"`
	Ask          string  `json:"ask"`
	Since        float64 `json:"since"`
	Until        float64 `json:"until"`
}

// heldOf is the held field for h.
func (s *Server) heldOf(h *hold) *heldWire {
	w := &heldWire{
		Hold: h.id, By: h.foreman, ForemanLabel: s.labelOf(h.foreman), Task: h.task, TaskLabel: h.task,
		Ask: h.ask, Since: h.since, Until: h.until,
	}
	if tk, ok := s.tree.Task(h.task); ok && tk.Label != "" {
		w.TaskLabel = tk.Label
	}
	return w
}

// foremanOf finds the nearest foreman for pane id: the Foreman of the
// pane's task, or of the nearest ancestor that has one. It returns the
// foreman pane and the task that named it, or two empty strings.
func (s *Server) foremanOf(id string) (foreman, task string) {
	p, ok := s.tree.Pane(id)
	if !ok {
		return "", ""
	}
	seen := map[string]bool{}
	for cur := p.TaskID; cur != "" && !seen[cur]; {
		seen[cur] = true
		tk, ok := s.tree.Task(cur)
		if !ok {
			return "", ""
		}
		if tk.Foreman != "" {
			return tk.Foreman, tk.ID
		}
		cur = tk.Parent
	}
	return "", ""
}

// canAnswer reports whether foreman can take an ask from pane asker: it
// is a pane the tree holds, it is open, it is not the asker, and it does
// not wait on an ask of its own.
func (s *Server) canAnswer(foreman, asker string) bool {
	if foreman == asker {
		return false
	}
	p, ok := s.tree.Pane(foreman)
	if !ok || p.Closed {
		return false
	}
	if eff, ok := s.effectiveState(foreman); ok && eff.State == proto.StateBlocked {
		return false
	}
	return true
}

// escalation is what escalate decided for one event: the held field the
// broadcast carries, and the note for the foreman, sent after the lock is
// released.
type escalation struct {
	held    *heldWire
	note    string
	foreman string
	// release lists the asks pane id held as a foreman, when id is now
	// blocked itself. They go to the operator once the lock is released.
	release []heldAsk
}

// heldAsk names one held ask: the asking pane and the ask id.
type heldAsk struct{ pane, ask string }

// heldBy lists the asks foreman holds. The caller holds holdsMu.
func (s *Server) heldBy(foreman string) []heldAsk {
	var out []heldAsk
	for _, h := range s.holds {
		if h.foreman == foreman {
			out = append(out, heldAsk{h.pane, h.ask})
		}
	}
	return out
}

// escalate decides, before the event goes out, whether pane id's merged
// state is an ask its foreman hears first. An undoable ask on a pane under
// a foreman that can answer is held until the hold ends. A permanent ask is
// never held, but the foreman still hears it. A state that is not that ask
// any more ends the hold. An ask that already surfaced is never held
// again. A foreman that is blocked itself cannot answer, so every ask it
// holds is released to the operator.
func (s *Server) escalate(id string, ev proto.PaneStateEvent) escalation {
	s.holdsMu.Lock()
	defer s.holdsMu.Unlock()
	if ev.State == proto.StateBlocked {
		if rel := s.heldBy(id); len(rel) > 0 {
			out := s.escalateAsk(id, ev)
			out.release = rel
			return out
		}
	}
	return s.escalateAsk(id, ev)
}

// escalateAsk is escalate for pane id's own ask. The caller holds
// holdsMu.
func (s *Server) escalateAsk(id string, ev proto.PaneStateEvent) escalation {
	cur := s.holds[id]
	if ev.State != proto.StateBlocked || ev.Ask == nil || ev.Ask.ID == "" {
		s.dropHold(id)
		return escalation{}
	}
	if cur != nil && cur.ask == ev.Ask.ID {
		return escalation{held: s.heldOf(cur)}
	}
	s.dropHold(id)
	if s.wasSurfaced(id, ev.Ask.ID) {
		return escalation{}
	}
	s.markSurfaced(id, ev.Ask.ID)
	foreman, task := s.foremanOf(id)
	if foreman == "" || !s.canAnswer(foreman, id) {
		return escalation{}
	}
	out := escalation{foreman: foreman}
	label := s.labelOf(id)
	summary := ev.Ask.Summary
	if ev.Ask.Tool != "" {
		summary = ev.Ask.Tool + ": " + summary
	}
	if proto.NormalTier(ev.Ask.Tier) != proto.TierUndoable {
		out.note = fmt.Sprintf("%s asks %s: %s. It cannot be undone, so it goes to the operator now.",
			label, ev.Ask.ID, summary)
		return out
	}
	now := nowSeconds()
	until := now + s.holdFor.Seconds()
	if ev.Ask.Deadline > 0 {
		if last := ev.Ask.Deadline - holdMargin.Seconds(); last < until {
			until = last
		}
	}
	if until <= now {
		out.note = fmt.Sprintf("%s asks %s: %s. The gate waits too little for a hold, so it goes to the operator now.",
			label, ev.Ask.ID, summary)
		return out
	}
	s.unmarkSurfaced(id, ev.Ask.ID)
	s.nextHold++
	h := &hold{pane: id, ask: ev.Ask.ID, foreman: foreman, task: task, since: now, until: until,
		id: fmt.Sprintf("h%d", s.nextHold)}
	askID := ev.Ask.ID
	h.timer = time.AfterFunc(time.Duration((until-now)*float64(time.Second)), func() { s.surface(id, askID) })
	s.holds[id] = h
	out.held = s.heldOf(h)
	out.note = fmt.Sprintf("%s asks %s: %s. You hear it first. To refuse it, run: coppice agent deny %s %s. "+
		"It goes to the operator in %d s.", label, ev.Ask.ID, summary, id, ev.Ask.ID, int(until-now))
	return out
}

// dropHold ends the hold on pane id, if any. The caller holds holdsMu.
func (s *Server) dropHold(id string) {
	if h := s.holds[id]; h != nil {
		h.timer.Stop()
		delete(s.holds, id)
	}
}

// wasSurfaced reports whether ask went to the operator already. The
// caller holds holdsMu.
func (s *Server) wasSurfaced(id, ask string) bool {
	for _, a := range s.surfaced[id] {
		if a == ask {
			return true
		}
	}
	return false
}

// markSurfaced records that ask went to the operator, and keeps only the
// last surfacedKept per pane. The caller holds holdsMu.
func (s *Server) markSurfaced(id, ask string) {
	list := append(s.surfaced[id], ask)
	if len(list) > surfacedKept {
		list = append([]string(nil), list[len(list)-surfacedKept:]...)
	}
	s.surfaced[id] = list
}

// unmarkSurfaced forgets ask, for an ask that is held after all. The
// caller holds holdsMu.
func (s *Server) unmarkSurfaced(id, ask string) {
	list := s.surfaced[id]
	for i, a := range list {
		if a == ask {
			s.surfaced[id] = append(list[:i:i], list[i+1:]...)
			return
		}
	}
}

// heldFor is the held field for pane id, or nil when no hold is on it.
func (s *Server) heldFor(id string) *heldWire {
	s.holdsMu.Lock()
	defer s.holdsMu.Unlock()
	if h := s.holds[id]; h != nil {
		return s.heldOf(h)
	}
	return nil
}

// holder is the foreman that holds ask on pane id, or "".
func (s *Server) holder(id, ask string) string {
	s.holdsMu.Lock()
	defer s.holdsMu.Unlock()
	if h := s.holds[id]; h != nil && h.ask == ask {
		return h.foreman
	}
	return ""
}

// surface ends the hold on ask of pane id when it is still the one held,
// and sends the pane's state again with no hold, so every floor shows the
// ask to the operator.
//
// The state is read and sent under holdsMu. An ApplyState for the same
// pane decides its own hold under that lock before it sends, so a newer
// event always goes out after this one, never before it.
func (s *Server) surface(id, ask string) {
	s.holdsMu.Lock()
	h := s.holds[id]
	if h == nil || h.ask != ask {
		s.holdsMu.Unlock()
		return
	}
	delete(s.holds, id)
	s.markSurfaced(id, ask)
	foreman := h.foreman
	ev, ok := s.states.Current(id)
	if ok && ev.State == proto.StateBlocked && ev.Ask != nil && ev.Ask.ID == ask {
		s.Broadcast("state", id, stateEvent{Event: "state", PaneStateEvent: ev})
	}
	s.holdsMu.Unlock()
	s.NoteTo(fmt.Sprintf("%s's ask %s goes to the operator now.", s.labelOf(id), ask), id, foreman)
}

// recheckHolds sends to the operator every held ask whose pane's nearest
// foreman is no longer the one that holds it. It runs after a task's
// foreman changes and after a task moves.
func (s *Server) recheckHolds() {
	s.holdsMu.Lock()
	var gone []heldAsk
	for _, h := range s.holds {
		if f, _ := s.foremanOf(h.pane); f != h.foreman {
			gone = append(gone, heldAsk{h.pane, h.ask})
		}
	}
	s.holdsMu.Unlock()
	for _, g := range gone {
		s.surface(g.pane, g.ask)
	}
}

// plainAsk matches an ask id the server may name in the line it types
// into a foreman. The asking pane wrote the id, so any other shape is left
// out.
var plainAsk = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// foremanLine is the one line typed into a foreman for a hold. It names
// the hold by the server's own id, the pane, the ask id when it is plain,
// the seconds left, and two commands. It never holds the ask's summary:
// the asking pane wrote that, and the foreman reads it through agent get,
// as data.
func foremanLine(h *hold, now float64) string {
	ask := ""
	if plainAsk.MatchString(h.ask) {
		ask = " " + h.ask
	}
	left := int(h.until - now)
	if left < 0 {
		left = 0
	}
	return fmt.Sprintf("coppice: hold %s: pane %s waits on ask%s, and you hear it first for %d s. "+
		"Read it: coppice agent get %s. Refuse it: coppice agent deny %s %s",
		h.id, h.pane, ask, left, h.pane, h.pane, h.id)
}

// resolveHold is the ask id that ask names on pane id: the held ask when
// ask is that hold's short id, and ask itself otherwise.
func (s *Server) resolveHold(id, ask string) string {
	s.holdsMu.Lock()
	defer s.holdsMu.Unlock()
	if h := s.holds[id]; h != nil && h.id == ask {
		return h.ask
	}
	return ask
}

// endsLine reports whether input ends with Enter: a carriage return or a
// line feed.
func endsLine(input string) bool {
	return strings.HasSuffix(input, "\r") || strings.HasSuffix(input, "\n")
}

// markInput records that someone typed into pane id, and whether that
// input ended with Enter. The server types no hold line into the pane
// while the mark stands, so a half-typed line never gets a line stuck to
// it.
func (s *Server) markInput(id string, entered bool) {
	s.holdsMu.Lock()
	defer s.holdsMu.Unlock()
	s.typed[id] = true
	s.open[id] = !entered
}

// wentIdle forgets the input mark of pane id once a turn ended, but only
// when the last input to it ended with Enter. A half line typed at any
// time keeps the mark, since it still sits in the input line.
func (s *Server) wentIdle(id string) {
	s.holdsMu.Lock()
	defer s.holdsMu.Unlock()
	if s.open[id] {
		return
	}
	delete(s.typed, id)
	delete(s.open, id)
}

// turnSources are the sources that know a turn ended: the gate and its
// hooks, and a headless harness's own stream. The process source only
// knows the screen went quiet, and a manifest read only knows the prompt
// box shows, which it also does with a half-typed line in it.
var turnSources = map[string]bool{
	proto.SrcGate: true, proto.SrcHeadless: true,
}

// turnEnded reports whether ev is an idle that ends a turn.
func turnEnded(ev proto.PaneStateEvent) bool {
	return ev.State == proto.StateIdle && turnSources[ev.Source]
}

// idle reports whether pane id's effective state is an idle that ends a
// turn. A foreman whose idle comes only from the process source is never
// idle here, so it gets the note and never typed bytes.
func (s *Server) idle(id string) bool {
	eff, ok := s.effectiveState(id)
	return ok && turnEnded(*eff)
}

// tellHolds types one line into foreman for each ask it holds and has not
// been told of, when foreman is idle and nobody typed into it since it
// last went idle. A working or blocked foreman gets no bytes: a line would
// mix into its turn, and an Enter would answer its own question. The
// server types on its own authority, so the guard's typing rule does not
// apply. Each write runs on a goroutine of its own, so a full input
// buffer never stops the caller.
func (s *Server) tellHolds(foreman string) {
	if !s.idle(foreman) {
		return
	}
	s.holdsMu.Lock()
	if s.typed[foreman] {
		s.holdsMu.Unlock()
		return
	}
	var tell []*hold
	for _, h := range s.holds {
		if h.foreman == foreman && !h.told && !h.sending {
			h.sending = true
			tell = append(tell, h)
		}
	}
	s.holdsMu.Unlock()
	for _, h := range tell {
		go s.tellOne(foreman, h)
	}
}

// tellOne writes one hold's line into foreman. The state and the input
// mark are read again just before the write. The hold is marked told
// only when the write succeeds, so a failed or skipped one is tried again
// at the next idle.
func (s *Server) tellOne(foreman string, h *hold) {
	err := errNotTold
	if s.idle(foreman) {
		s.holdsMu.Lock()
		ok := !s.typed[foreman] && s.holds[h.pane] == h
		line := foremanLine(h, nowSeconds())
		if ok {
			// The server's own line is input too. The next line waits for
			// the turn it starts to end, so a foreman gets one per turn.
			s.typed[foreman] = true
			s.open[foreman] = false
		}
		s.holdsMu.Unlock()
		if ok {
			err = s.typeInto(foreman, line)
		}
	}
	s.holdsMu.Lock()
	h.sending = false
	h.told = err == nil
	s.holdsMu.Unlock()
}

// errNotTold is the reason a hold line was not written: the foreman was
// not idle, someone typed into it, or the hold ended.
var errNotTold = errors.New("the foreman was not told")

// typeInto sends one line with Enter to pane id, as agent.prompt does.
func (s *Server) typeInto(id, line string) error {
	lp, ok := s.Live(id)
	if !ok {
		return errNotTold
	}
	if lp.Adapter != nil {
		return lp.Adapter.Prompt(line)
	}
	// The Enter goes on its own after pasteGap, so a harness that reads a
	// fast burst as a paste still submits the line.
	if err := lp.write([]byte(line)); err != nil {
		return err
	}
	time.Sleep(pasteGap)
	return lp.write([]byte("\r"))
}

// endHolds is for a pane that closed: its own hold ends, and every ask it
// held as a foreman goes to the operator at once.
func (s *Server) endHolds(id string) {
	s.holdsMu.Lock()
	s.dropHold(id)
	delete(s.surfaced, id)
	gone := s.heldBy(id)
	s.holdsMu.Unlock()
	for _, g := range gone {
		s.surface(g.pane, g.ask)
	}
}

// stopHolds stops every hold timer, so none fires after Close.
func (s *Server) stopHolds() {
	s.holdsMu.Lock()
	defer s.holdsMu.Unlock()
	for id := range s.holds {
		s.dropHold(id)
	}
}

// endHold ends the hold on ask of pane id once someone answered it.
func (s *Server) endHold(id, ask string) {
	s.holdsMu.Lock()
	defer s.holdsMu.Unlock()
	if h := s.holds[id]; h != nil && h.ask == ask {
		s.dropHold(id)
		s.markSurfaced(id, ask)
	}
}

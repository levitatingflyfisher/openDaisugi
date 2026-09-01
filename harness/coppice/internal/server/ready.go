package server

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/opendaisugi/coppice/internal/detect"
	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/pane"
	"github.com/opendaisugi/coppice/internal/proto"
)

// A text prompt reaches an agent only when its input box is ready. A
// prompt is pane.send_text with text and Enter, or agent.prompt to a pty
// pane. Raw keys and text with no Enter go through at once: they are the
// owner's own keys.
//
// This applies only to a pty pane whose harness has a manifest with an
// idle rule, and only once the manifest tick runs. Any other pane gets the text at once, as
// before.

// TrustRule is the claude.toml rule that sees Claude Code's folder trust
// screen.
const TrustRule = proto.TrustRule

// ruleWords are the words a blocked row shows for a rule, in place of the
// bare rule id.
var ruleWords = map[string]string{
	TrustRule: "asks to trust this folder",
}

// manifestDetail is the detail of a state event the manifest tick sends:
// the rule's words when it has some, then the rule and the region.
func manifestDetail(r detect.Result) string {
	ids := fmt.Sprintf("rule=%s region=%s", r.RuleID, r.Region)
	if w, ok := ruleWords[r.RuleID]; ok {
		return w + " (" + ids + ")"
	}
	return ids
}

// IsTrustDetail reports whether a state detail came from the trust rule.
func IsTrustDetail(detail string) bool { return proto.IsTrustDetail(detail) }

// pasteGap, in foreman.go, is also the pause between a prompt's text and
// its Enter. Claude Code reads a burst that ends in CR as a paste and
// keeps the CR as a new line in the input box. A separate Enter after a
// short pause submits it.

// resubmitWait is how long after the Enter the server looks at the input
// box again. When the box still holds the start of the text, the Enter was
// read as part of a paste, and the server sends one more Enter. Never more
// than one.
var resubmitWait = 800 * time.Millisecond

// readySettle is how long a pane must read ready before a queued prompt
// goes. A harness can draw its input box a moment before it reads keys.
var readySettle = 300 * time.Millisecond

// promptPoll is how often a queue looks at its pane's screen.
var promptPoll = 100 * time.Millisecond

// promptQueueWait is the longest a queue waits on a pane that stays in one
// state and is not working. Past it the queue drops what it holds and
// says so, so a pane that never reads ready cannot keep prompts for ever.
// The wait starts again each time the pane's state changes, and time the
// pane spends working does not count: a long turn is not a stuck pane.
var promptQueueWait = 10 * time.Minute

// maxPromptQueue and maxPromptBytes cap one pane's queue. Past either a
// prompt is refused.
const (
	maxPromptQueue = 16
	maxPromptBytes = 64 << 10
)

// ScreenQuestionRefusal is the refusal for a prompt to a pane that waits
// on a question on its screen. %s is the pane's label.
const ScreenQuestionRefusal = "%s is waiting on a question on its screen. Answer it first: open it."

// TrustQuestionRefusal is the same refusal for a pane on Claude's trust
// screen, which names the command that answers it. The first %s is the
// pane's label, the second its id.
const TrustQuestionRefusal = "%s is waiting on a question on its screen. Answer it first: coppice pane trust %s, or open it."

// questionRefusal is the refusal for a prompt to pane id, which waits on
// the question v shows.
func questionRefusal(label, id string, v detect.Result) string {
	if v.RuleID == TrustRule {
		return fmt.Sprintf(TrustQuestionRefusal, label, id)
	}
	return fmt.Sprintf(ScreenQuestionRefusal, label)
}

// QueuedNote is what a queued prompt's reply says. %s is the pane's label.
const QueuedNote = "queued until %s is ready"

// queuedPrompt is one prompt that waits. done gets nil once it was sent,
// or the reason it was not. It has room for one. from is the pane that
// sent it, or "", and client the connection that sent it, or nil: a drop
// is told to both.
type queuedPrompt struct {
	text   string
	done   chan error
	from   string
	client *Client
	// wait is shared with a sender that waits on the reply. See the
	// wait states below.
	wait *atomic.Int32
}

// The states of queuedPrompt.wait. A sender that waits on the reply sets
// waitOn. A drop that finds waitOn takes it to waitTold: the waiter gets
// the drop as its error, so the drop tells the sender nothing more. A
// waiter that gives up takes waitOn back to waitOff, and a later drop
// then tells the sender. The two moves are compare-and-swap, so exactly
// one of them wins.
const (
	waitOff int32 = iota
	waitOn
	waitTold
)

// promptQueue is one pane's prompts that wait, oldest first. label is the
// pane's label while it lives, so a note after the pane closed still
// names it.
type promptQueue struct {
	items   []queuedPrompt
	bytes   int
	running bool
	label   string
}

// promptFields is what New sets up for the prompt queues.
type promptFields struct {
	promptMu  sync.Mutex
	prompts   map[string]*promptQueue
	detectSet *detect.Set
	// promptSending is true while a pane gets a prompt, from its text to
	// the check for a second Enter. A second prompt waits for it.
	promptSending map[string]bool
	// trusted is the operator's last answer to each pane's trust screen.
	// See trustGrace.
	trusted map[string]trustAnswer
	// trusting is true while a pane.trust call for a pane runs.
	trusting map[string]bool
}

// trustAnswer is one answer to a trust screen and when it went.
type trustAnswer struct {
	at  time.Time
	yes bool
}

// pruneTrustLocked forgets the trust answers older than trustGrace. The
// caller holds promptMu.
func (s *Server) pruneTrustLocked() {
	for id, a := range s.trusted {
		if time.Since(a.at) >= trustGrace {
			delete(s.trusted, id)
		}
	}
}

// trustGrace is how long after an answer to the trust screen that screen
// may still show while Claude draws its next one. A prompt sent then,
// after "Trust this folder", waits in the queue, since the question is
// already answered. A second answer in that time is refused, so a double
// click cannot send two.
var trustGrace = 5 * time.Second

// answeredTrust reports whether the operator trusted pane id's folder
// within trustGrace.
func (s *Server) answeredTrust(id string) bool {
	s.promptMu.Lock()
	defer s.promptMu.Unlock()
	s.pruneTrustLocked()
	a, ok := s.trusted[id]
	return ok && a.yes
}

// setDetectSet records the manifests the tick uses, so a prompt can read
// its pane's screen the same way.
func (s *Server) setDetectSet(set *detect.Set) {
	s.promptMu.Lock()
	s.detectSet = set
	s.promptMu.Unlock()
}

// screenVerdict reads pane id's screen against its harness's manifest. ok
// is false when the pane is no pty pane, has no harness or no manifest, or
// no manifest tick runs. Then the prompt gate does not apply.
func (s *Server) screenVerdict(id string) (detect.Result, bool) {
	s.promptMu.Lock()
	set := s.detectSet
	s.promptMu.Unlock()
	if set == nil {
		return detect.Result{}, false
	}
	rec, ok := s.tree.Pane(id)
	if !ok || rec.Kind != layout.KindPTY || rec.Harness == "" {
		return detect.Result{}, false
	}
	c, ok := set.For(rec.Harness)
	if !ok || !hasIdleRule(c) {
		return detect.Result{}, false
	}
	lp, ok := s.Live(id)
	if !ok || lp.PTY == nil || lp.Grid == nil {
		return detect.Result{}, false
	}
	in, _, err := detectionInput(lp)
	if err != nil {
		return detect.Result{State: detect.StateUnknown}, true
	}
	return c.Evaluate(in), true
}

// hasIdleRule reports whether a manifest can ever read a screen as idle.
// A manifest with no idle rule could never say the input box is ready,
// so a prompt to its harness would wait for nothing. Such a harness gets
// prompts at once, as a harness with no manifest does.
func hasIdleRule(c *detect.Compiled) bool {
	for _, r := range c.Manifest.Rules {
		if r.EffectiveState() == detect.StateIdle {
			return true
		}
	}
	return false
}

// gated reports whether prompts to pane id go through the prompt gate.
func (s *Server) gated(id string) bool {
	_, ok := s.screenVerdict(id)
	return ok
}

// waitsOnQuestion reports whether pane id waits on a question: its screen
// shows one, or another source says it is blocked, as a gate ask does.
func (s *Server) waitsOnQuestion(id string, v detect.Result) bool {
	if v.Matched && v.State == detect.StateBlocked {
		return true
	}
	if ev, ok := s.effectiveCurrent(id); ok && ev.State == proto.StateBlocked && ev.Source != proto.SrcManifest {
		return true
	}
	return false
}

// ready reports whether v shows the pane idle at its input box.
func ready(v detect.Result) bool {
	return v.Matched && v.State == detect.StateIdle
}

// errPromptRefused carries a refusal with its message.
type errPromptRefused struct{ msg string }

func (e errPromptRefused) Error() string { return e.msg }

// promptOutcome is what submitPrompt did.
type promptOutcome struct {
	// sent is true when the text went to the pane now.
	sent bool
	// queued is the prompt's place in the queue, from 1, when it waits.
	queued int
	// done gets the queued prompt's result.
	done chan error
	// wait is the prompt's wait state. See queuedPrompt.wait.
	wait *atomic.Int32
}

// submitPrompt sends a prompt to pane id now when the pane is ready,
// queues it when the pane is starting or working, and refuses it when the
// pane waits on a question. The caller has checked that the gate applies.
// from and c name the sender, which hears of a drop.
func (s *Server) submitPrompt(id, text, from string, c *Client) (promptOutcome, error) {
	label := s.labelOf(id)
	if len(text) > maxPromptBytes {
		return promptOutcome{}, errPromptRefused{fmt.Sprintf(
			"the prompt is %d bytes. A prompt may be %d bytes at most.", len(text), maxPromptBytes)}
	}
	v, _ := s.screenVerdict(id)
	answered := v.RuleID == TrustRule && s.answeredTrust(id)
	if s.waitsOnQuestion(id, v) && !answered {
		return promptOutcome{}, errPromptRefused{questionRefusal(label, id, v)}
	}
	// A pane that reads ready now must stay ready for readySettle, the
	// same wait a queued prompt gets, before the prompt goes at once.
	if ready(v) && s.queueIdle(id) && s.staysReady(id) {
		s.promptMu.Lock()
		if s.queueIdleLocked(id) {
			delete(s.prompts, id)
			s.promptSending[id] = true
			s.promptMu.Unlock()
			err := s.deliverPrompt(id, text)
			return promptOutcome{sent: err == nil}, err
		}
		s.promptMu.Unlock()
	}
	s.promptMu.Lock()
	q := s.prompts[id]
	if q == nil {
		q = &promptQueue{label: label}
		s.prompts[id] = q
	}
	if len(q.items) >= maxPromptQueue || q.bytes+len(text) > maxPromptBytes {
		n, b := len(q.items), q.bytes
		s.promptMu.Unlock()
		return promptOutcome{}, errPromptRefused{fmt.Sprintf(
			"%s has %d prompts, %d bytes, waiting already. A queue holds %d prompts and %d bytes. Wait until it is ready, then send again.",
			label, n, b, maxPromptQueue, maxPromptBytes)}
	}
	done := make(chan error, 1)
	wait := new(atomic.Int32)
	q.items = append(q.items, queuedPrompt{text: text, done: done, from: from, client: c, wait: wait})
	q.bytes += len(text)
	out := promptOutcome{queued: len(q.items), done: done, wait: wait}
	if !q.running {
		q.running = true
		go s.runPrompts(id)
	}
	s.promptMu.Unlock()
	return out, nil
}

// queueIdle reports whether pane id has no prompt waiting or going.
func (s *Server) queueIdle(id string) bool {
	s.promptMu.Lock()
	defer s.promptMu.Unlock()
	return s.queueIdleLocked(id)
}

// queueIdleLocked is queueIdle for a caller that holds promptMu.
func (s *Server) queueIdleLocked(id string) bool {
	q := s.prompts[id]
	return (q == nil || (len(q.items) == 0 && !q.running)) && !s.promptSending[id]
}

// staysReady reports whether pane id still reads ready, with no question,
// at each poll for readySettle.
func (s *Server) staysReady(id string) bool {
	end := time.Now().Add(readySettle)
	for time.Now().Before(end) {
		select {
		case <-s.talkStop:
			return false
		case <-time.After(promptPoll):
		}
		v, ok := s.screenVerdict(id)
		if !ok || !ready(v) || s.waitsOnQuestion(id, v) {
			return false
		}
	}
	return true
}

// deliverPrompt writes text, waits pasteGap, then writes the Enter on its
// own. The caller set promptSending for id. The check for a second Enter
// runs after this returns, and clears promptSending when it is done.
func (s *Server) deliverPrompt(id, text string) error {
	lp, ok := s.Live(id)
	if !ok {
		s.clearSending(id)
		return errForemanGone
	}
	s.markInput(id, true)
	if err := lp.write([]byte(text)); err != nil {
		s.clearSending(id)
		return err
	}
	time.Sleep(pasteGap)
	if err := lp.write([]byte("\r")); err != nil {
		s.clearSending(id)
		return err
	}
	go s.resubmit(id, text)
	return nil
}

func (s *Server) clearSending(id string) {
	s.promptMu.Lock()
	delete(s.promptSending, id)
	s.promptMu.Unlock()
}

// resubmit waits resubmitWait, then sends one more Enter when the input
// box still holds the start of text.
func (s *Server) resubmit(id, text string) {
	defer s.clearSending(id)
	select {
	case <-s.talkStop:
		return
	case <-time.After(resubmitWait):
	}
	if s.stillInBox(id, text) {
		if lp, ok := s.Live(id); ok {
			_ = lp.write([]byte("\r"))
		}
	}
}

// stillInBox reports whether pane id reads idle with the start of text,
// or a pasted-text mark, still in its input box.
func (s *Server) stillInBox(id, text string) bool {
	v, ok := s.screenVerdict(id)
	if !ok || !ready(v) {
		return false
	}
	lp, ok := s.Live(id)
	if !ok {
		return false
	}
	in, _, err := detectionInput(lp)
	if err != nil {
		return false
	}
	return boxHolds(detect.Region(in, "prompt_box_body"), text)
}

// boxHolds reports whether an input box's text holds the start of text,
// or the mark a harness shows for pasted text.
func boxHolds(box, text string) bool {
	if strings.Contains(box, "[Pasted text") {
		return true
	}
	head := strings.TrimSpace(strings.SplitN(text, "\n", 2)[0])
	if head == "" {
		return false
	}
	// A long line wraps in the box, so only its start is looked for.
	for utf8.RuneCountInString(head) > 24 {
		_, size := utf8.DecodeLastRuneInString(head)
		head = head[:len(head)-size]
	}
	return strings.Contains(strings.Join(strings.Fields(box), " "), strings.Join(strings.Fields(head), " "))
}

// errPromptTimeout is why a queue dropped its prompts: the pane did not
// read ready in time.
var errPromptTimeout = errors.New("the pane was not ready in time")

// runPrompts sends pane id's queued prompts in order, each once the pane
// reads ready for readySettle. It stops when the queue is empty, and drops
// the rest, and says so, when the pane ends or waits too long.
func (s *Server) runPrompts(id string) {
	var readySince time.Time
	// waitStart is when the pane entered the state it is in, or last took
	// a prompt. lastKey names that state. See promptQueueWait.
	waitStart, lastKey := time.Now(), ""
	for {
		s.promptMu.Lock()
		q := s.prompts[id]
		if q == nil || len(q.items) == 0 {
			// An empty queue leaves the map, so a closed pane leaves
			// nothing behind.
			delete(s.prompts, id)
			s.promptMu.Unlock()
			return
		}
		sending := s.promptSending[id]
		s.promptMu.Unlock()

		rec, live := s.tree.Pane(id)
		if !live || rec.Closed {
			s.dropPrompts(id, errForemanGone)
			return
		}
		s.promptMu.Lock()
		if q.label = rec.Label; q.label == "" {
			q.label = id
		}
		s.promptMu.Unlock()
		v, ok := s.screenVerdict(id)
		key := "unread"
		if ok {
			key = string(v.State)
			if s.waitsOnQuestion(id, v) {
				key = string(detect.StateBlocked)
			}
		}
		if key != lastKey || key == string(detect.StateWorking) {
			waitStart, lastKey = time.Now(), key
		}
		if time.Since(waitStart) > promptQueueWait {
			s.dropPrompts(id, errPromptTimeout)
			return
		}
		switch {
		case !ok:
			// Nothing can tell when the pane is ready: the manifest tick
			// stopped, or the pane lost its screen. The prompts hold, and
			// the wait above drops them in time.
			readySince = time.Time{}
		case sending || !ready(v) || s.waitsOnQuestion(id, v):
			readySince = time.Time{}
		case readySince.IsZero():
			readySince = time.Now()
		case time.Since(readySince) >= readySettle:
			readySince = time.Time{}
			s.sendHead(id)
			// The next prompt waits for the pane to take this one.
			s.awaitTurn(id)
			waitStart, lastKey = time.Now(), ""
			continue
		}
		select {
		case <-s.talkStop:
			s.dropPrompts(id, errTalkStopped)
			return
		case <-time.After(promptPoll):
		}
	}
}

// sendHead takes the oldest prompt of pane id off its queue and sends it.
func (s *Server) sendHead(id string) {
	s.promptMu.Lock()
	q := s.prompts[id]
	if q == nil || len(q.items) == 0 {
		s.promptMu.Unlock()
		return
	}
	head := q.items[0]
	q.items = q.items[1:]
	q.bytes -= len(head.text)
	s.promptSending[id] = true
	s.promptMu.Unlock()
	head.done <- s.deliverPrompt(id, head.text)
}

// awaitTurn waits, a short time at most, for pane id to leave ready after
// a prompt, so the next prompt does not land before the harness took this
// one. A prompt that starts no turn, such as a slash command, ends the
// wait by its time limit.
func (s *Server) awaitTurn(id string) {
	end := time.Now().Add(3 * time.Second)
	for time.Now().Before(end) {
		select {
		case <-s.talkStop:
			return
		case <-time.After(promptPoll):
		}
		if v, ok := s.screenVerdict(id); !ok || !ready(v) {
			return
		}
	}
}

// dropPrompts ends pane id's queue: every prompt in it gets why, the
// floor gets one note, and each sender hears of it. A sender that waits
// on the reply gets why as its error. A pane that sent a prompt gets a
// note addressed to it and one line typed into its own prompt, through
// its own queue. A client connection still open gets the note as an
// event.
func (s *Server) dropPrompts(id string, why error) {
	s.promptMu.Lock()
	q := s.prompts[id]
	var lost []queuedPrompt
	label := id
	if q != nil {
		lost = q.items
		if q.label != "" {
			label = q.label
		}
		q.items, q.bytes, q.running = nil, 0, false
	}
	delete(s.prompts, id)
	// Only a pane that ended forgets its trust answer. A drop for any
	// other reason leaves the grace to pruning, so a second pane.trust
	// right after the first is still refused.
	if errors.Is(why, errForemanGone) {
		delete(s.trusted, id)
	}
	s.promptMu.Unlock()
	for _, p := range lost {
		p.done <- why
	}
	if len(lost) == 0 || errors.Is(why, errTalkStopped) {
		return
	}
	n := "1 prompt was"
	if len(lost) > 1 {
		n = fmt.Sprintf("%d prompts were", len(lost))
	}
	text := fmt.Sprintf("%s ended, so %s not sent.", label, n)
	if errors.Is(why, errPromptTimeout) {
		text = fmt.Sprintf("%s was not ready in %d min, so %s not sent.", label, int(promptQueueWait/time.Minute), n)
	}
	s.Note(text, id)
	told := map[string]bool{}
	clients := map[*Client]bool{}
	for _, p := range lost {
		if p.wait != nil && p.wait.CompareAndSwap(waitOn, waitTold) {
			// Its waiter reports the drop as its error.
			continue
		}
		if p.from != "" && p.from != id && !told[p.from] {
			told[p.from] = true
			s.NoteTo(text, id, p.from)
			if s.gated(p.from) {
				// The sender's own queue keeps this line out of a
				// question on its screen. A refusal is fine: the note
				// stands.
				_, _ = s.submitPrompt(p.from, "coppice: "+text, "", nil)
			}
		}
		if p.client != nil && !p.client.Dead() && !clients[p.client] {
			clients[p.client] = true
			p.client.Emit(note{Event: "note", Text: text, Pane: id, To: p.from, TS: nowSeconds()})
		}
	}
}

// isPromptRequest reports whether r is a text prompt: agent.prompt,
// pane.run, or pane.send_text with text and Enter. Raw text and a bare
// Enter are not. pane.run always ends in Enter, so on a gated pane it is
// a prompt even with an empty line: that Enter would answer a question.
func isPromptRequest(r *proto.Request) bool {
	switch r.Cmd {
	case "agent.prompt", "pane.run":
		return true
	case "pane.send_text":
		text, _ := r.Str("text")
		enter, ok := r.Bool("enter")
		return text != "" && (!ok || enter)
	}
	return false
}

// TrustRefusal is what a pane or a plugin gets for pane.trust.
const TrustRefusal = "only the operator trusts a folder."

// trustOption matches one option line of the trust screen. Group 1 is the
// cursor mark, group 2 the option's words.
var trustOption = regexp.MustCompile(`(?i)^[\s│]*(❯)?\s*(?:\d\.\s*)?(yes, i trust this folder|no, exit)`)

// trustMoves is how many lines the cursor must go down, or up when it is
// less than zero, to reach "Yes, I trust this folder". ok is false when
// the screen shows no yes option or no cursor mark.
func trustMoves(screen string) (int, bool) {
	yes, sel, n := -1, -1, 0
	for _, line := range strings.Split(screen, "\n") {
		m := trustOption.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if strings.HasPrefix(strings.ToLower(m[2]), "yes") {
			yes = n
		}
		if m[1] != "" {
			sel = n
		}
		n++
	}
	// No cursor mark means the screen is not the one this reads. Moving
	// from a guessed line could land on "No, exit".
	if yes < 0 || sel < 0 {
		return 0, false
	}
	return yes - sel, true
}

// handleTrust answers Claude Code's folder trust screen for the operator.
// trust true, the default, moves the cursor to "Yes, I trust this folder"
// and presses Enter. trust false presses Esc, which ends Claude. It
// refuses when the pane does not show the trust screen. The server never
// writes Claude's own config to trust a folder.
func (s *Server) handleTrust(c *Client, r *proto.Request) proto.Response {
	lp, bad := s.livePane(r)
	if bad != nil {
		return *bad
	}
	id := paneOf(r)
	if c.viewOnlyOn(id) {
		return refuseViewOnly(r, id)
	}
	v, ok := s.screenVerdict(id)
	if !ok || !v.Matched || v.RuleID != TrustRule {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			fmt.Sprintf("%s does not ask to trust a folder now.", s.labelOf(id)))
	}
	trust, ok := r.Bool("trust")
	if !ok {
		trust = true
	}
	// One answer at a time, and none again inside trustGrace: the screen
	// can still show a moment after the first answer, and a second one
	// would land on whatever Claude draws next.
	s.promptMu.Lock()
	s.pruneTrustLocked()
	_, recent := s.trusted[id]
	if s.trusting[id] || recent {
		s.promptMu.Unlock()
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			fmt.Sprintf("%s has an answer to its trust screen already. Wait a moment and look at it again.", s.labelOf(id)))
	}
	s.trusting[id] = true
	s.promptMu.Unlock()
	defer func() {
		s.promptMu.Lock()
		delete(s.trusting, id)
		s.promptMu.Unlock()
	}()
	s.markInput(id, true)
	if !trust {
		if err := lp.write([]byte("\x1b")); err != nil {
			return proto.ErrResp(r.ID, proto.ErrInternal, err.Error())
		}
		s.promptMu.Lock()
		s.trusted[id] = trustAnswer{at: time.Now(), yes: false}
		s.promptMu.Unlock()
		return proto.OKResp(r.ID, map[string]any{"pane": id, "trusted": false})
	}
	screen, err := lp.Grid.Read(pane.ReadDetection)
	if err != nil {
		return proto.ErrResp(r.ID, proto.ErrPaneClosed,
			fmt.Sprintf("pane %s has closed. Create a new one with: coppice pane create", id))
	}
	moves, ok := trustMoves(screen)
	if !ok {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			fmt.Sprintf("%s shows no option to trust the folder.", s.labelOf(id)))
	}
	key := "\x1b[B"
	if moves < 0 {
		key, moves = "\x1b[A", -moves
	}
	// Each key is its own write, pasteGap apart, so the harness reads
	// keys and not a paste.
	for i := 0; i < moves; i++ {
		if err := lp.write([]byte(key)); err != nil {
			return proto.ErrResp(r.ID, proto.ErrInternal, err.Error())
		}
		time.Sleep(pasteGap)
	}
	if err := lp.write([]byte("\r")); err != nil {
		return proto.ErrResp(r.ID, proto.ErrInternal, err.Error())
	}
	s.promptMu.Lock()
	s.trusted[id] = trustAnswer{at: time.Now(), yes: true}
	s.promptMu.Unlock()
	return proto.OKResp(r.ID, map[string]any{"pane": id, "trusted": true})
}

// senderPane is the pane a connection belongs to, or "" for the operator
// or a peer the server cannot place.
func senderPane(c *Client) string {
	if c == nil {
		return ""
	}
	return c.roleOf().paneID
}

// paneOf is the pane a request names, or "".
func paneOf(r *proto.Request) string {
	id, _ := r.Str("pane")
	return id
}

// promptErr turns a submitPrompt error into the reply.
func promptErr(r *proto.Request, err error) proto.Response {
	var refused errPromptRefused
	if errors.As(err, &refused) {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, refused.msg)
	}
	if errors.Is(err, errForemanGone) {
		id, _ := r.Str("pane")
		return proto.ErrResp(r.ID, proto.ErrPaneClosed,
			fmt.Sprintf("pane %s has closed. Create a new one with: coppice pane create", id))
	}
	return proto.ErrResp(r.ID, proto.ErrInternal, err.Error())
}

// promptReply answers pane.send_text for a prompt to a gated pane: sent
// now, queued, or refused.
func (s *Server) promptReply(c *Client, r *proto.Request, id, text string) proto.Response {
	out, err := s.submitPrompt(id, text, senderPane(c), c)
	if err != nil {
		return promptErr(r, err)
	}
	if out.sent {
		return proto.OKResp(r.ID, map[string]any{"sent": len(text) + 1})
	}
	return proto.OKResp(r.ID, map[string]any{
		"sent": 0, "queued": out.queued, "note": fmt.Sprintf(QueuedNote, s.labelOf(id)),
	})
}

// queuedPromptReply answers agent.prompt for a prompt that waits. Without
// wait it answers at once. With wait it first waits for the prompt to go,
// within the request's timeout, and then for the state the request names.
func (s *Server) queuedPromptReply(c *Client, r *proto.Request, id, text string, out promptOutcome) proto.Response {
	note := fmt.Sprintf(QueuedNote, s.labelOf(id))
	if wait, _ := r.Bool("wait"); !wait {
		return proto.OKResp(r.ID, map[string]any{"pane": id, "sent": 0, "queued": out.queued, "note": note})
	}
	ms, ok := r.Int("timeout_ms")
	if !ok || ms <= 0 {
		ms = 120000
	}
	out.wait.Store(waitOn)
	// giveUp ends the wait. When a drop already claimed this waiter, its
	// error is on done and is reported instead.
	giveUp := func(resp proto.Response) proto.Response {
		if out.wait.CompareAndSwap(waitOn, waitOff) {
			return resp
		}
		if err := <-out.done; err != nil {
			return promptErr(r, err)
		}
		return resp
	}
	select {
	case err := <-out.done:
		if err != nil {
			return promptErr(r, err)
		}
	case <-time.After(time.Duration(ms) * time.Millisecond):
		return giveUp(proto.ErrResp(r.ID, proto.ErrTimeout, fmt.Sprintf(
			"the prompt is still %s after %d ms. It goes when %s is ready.", note, ms, s.labelOf(id))))
	case <-c.dead:
		return giveUp(proto.ErrResp(r.ID, proto.ErrTimeout, "client disconnected while waiting"))
	}
	return s.promptWait(c, r, id, len(text), nil)
}

package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/skills"
)

// DefaultStewardLabel is the label of the floor's steward when the config
// file names none.
const DefaultStewardLabel = "steward"

// DefaultStewardWait is how long the talk queue waits on the steward
// before it posts a note, and how long it waits for the page's turn to
// start.
const DefaultStewardWait = 30 * time.Second

// talkPoll is how often the talk queue looks at the steward's state.
const talkPoll = 100 * time.Millisecond

// pasteGap is the pause between a paste and the Enter that sends it. A
// harness that reads fast input as more of the paste would swallow an
// Enter that came with it.
var pasteGap = 150 * time.Millisecond

// talkQueue is the owner's words on their way to the floor's steward. One
// goroutine at a time types them, in order. talkMu guards it.
type talkQueue struct {
	// pane is the steward the words go to, or "".
	pane string
	// paged is true once pane has its page, or when pane was started
	// somewhere else and so got its page there.
	paged bool
	// lines waits to be typed, oldest first.
	lines []string
	// reports are workers' lines for the pane steward. They wait behind
	// the owner's words and have a small queue of their own.
	reports []string
	// dropNoted is true after the note for a run of dropped reports, until
	// the reports are all typed or cleared.
	dropNoted bool
	// running is true while the goroutine that types runs.
	running bool
	// steward is the id of the pane the server started as the floor's
	// steward, and loaded is true once it was read from the data dir.
	steward string
	loaded  bool
}

// errStewardGone is why the queue stopped: the steward's process ended or
// its record went away.
var errStewardGone = errors.New("the steward ended")

// errTalkStopped is why the queue stopped: the server closes.
var errTalkStopped = errors.New("the server closes")

// RegisterStewardCommands wires up floor.talk and floor.steward.
func (s *Server) RegisterStewardCommands() {
	_ = s.Handle("floor.talk", s.handleFloorTalk)
	_ = s.Handle("floor.steward", s.handleFloorSteward)
	_ = s.Handle("floor.import", s.handleFloorImport)
}

// maxTalkQueue is how many sentences may wait for the steward. Past it a
// talk is refused, so a stuck steward cannot pile up words without end.
const maxTalkQueue = 32

// stewardFile is the file in the data dir that names the pane the server
// started as the floor's steward.
const stewardFile = "steward.json"

// StewardDir is the steward's own working directory:
// $XDG_STATE_HOME/coppice-steward, else ~/.local/state/coppice-steward.
// It is scratch space, not a project. It sits beside the coppice state dir,
// not in it: the warden and `daisugi fence` both treat the coppice config,
// state and data dirs as secrets, so a steward inside one cannot start
// under the fence. [steward] dir in coppice.toml sets another place.
func StewardDir() string {
	if st := os.Getenv("XDG_STATE_HOME"); st != "" {
		return filepath.Join(st, "coppice-steward")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".local", "state", "coppice-steward")
	}
	return filepath.Join(home, ".local", "state", "coppice-steward")
}

// stewardDir is this server's steward directory: Config.StewardDir, else
// StewardDir.
// turnDir is where chat-mode turns work: a dir of their own under the log
// dir, so each data dir has its own and a second server never touches the
// files of a turn that runs in another. The pane steward keeps stewardDir.
func (s *Server) turnDir() string { return filepath.Join(s.stewardLogDir(), "turns") }

func (s *Server) stewardDir() string {
	if s.cfg.StewardDir != "" {
		return s.cfg.StewardDir
	}
	if s.cfg.Steward.Dir != "" {
		return s.cfg.Steward.Dir
	}
	return StewardDir()
}

// isStewardDir reports whether dir is the steward's own directory, read
// through symlinks.
func (s *Server) isStewardDir(dir string) bool {
	if dir == "" {
		return false
	}
	own := s.stewardDir()
	if r, err := filepath.EvalSymlinks(own); err == nil {
		own = r
	}
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	return filepath.Clean(dir) == filepath.Clean(own)
}

// trackedSteward is the id of the pane the server started as the floor's
// steward, read once from the data dir, or "". The caller holds talkMu.
// The label is only for display: any pane may carry a label, so the
// server never finds the steward by it.
func (s *Server) trackedSteward() string {
	if !s.talk.loaded {
		s.talk.loaded = true
		var v struct {
			Pane string `json:"pane"`
		}
		if b, err := os.ReadFile(filepath.Join(s.cfg.DataDir, stewardFile)); err == nil && json.Unmarshal(b, &v) == nil {
			s.talk.steward = v.Pane
		}
	}
	return s.talk.steward
}

// setSteward records id as the floor's steward, in memory and in the data
// dir. The caller holds talkMu.
func (s *Server) setSteward(id string) {
	s.talk.steward, s.talk.loaded = id, true
	b, _ := json.Marshal(map[string]string{"pane": id})
	path := filepath.Join(s.cfg.DataDir, stewardFile)
	tmp := path + ".tmp"
	var err error
	if !s.writeData(func() {
		if err = os.WriteFile(tmp, b, 0o600); err == nil {
			err = os.Rename(tmp, path)
		}
	}) || err == nil {
		return
	}
	s.Note("cannot write "+path+". The steward is known until the server stops.", id)
}

// isTrackedSteward reports whether id is the pane the server tracks as
// the floor's steward.
func (s *Server) isTrackedSteward(id string) bool {
	s.talkMu.Lock()
	defer s.talkMu.Unlock()
	return id != "" && s.trackedSteward() == id
}

// lostReportsNote is the note for worker reports that were cleared because
// the steward ended before it took them.
func lostReportsNote(label string, n int) string {
	plural, were := "", "was"
	if n > 1 {
		plural, were = "s", "were"
	}
	return fmt.Sprintf("%s ended, so %d worker report%s %s not typed to it. The workers' logs are still there: status and zoom show them.", label, n, plural, were)
}

// followStewardLocked makes newID the floor's steward when it resumes the
// tracked steward's ended record old, so the next talk goes to it and no
// second steward starts. A resumed session had its page already. A fresh
// start gets it now. The caller holds talkMu.
func (s *Server) followStewardLocked(old, newID string, resumed bool) {
	if old == "" || !s.talk.loaded || s.talk.steward != old {
		return
	}
	s.setSteward(newID)
	s.talk.pane, s.talk.paged = newID, resumed
	s.openStandingNote(newID)
	if !resumed {
		s.runTalkLocked()
	}
}

// stewardState reports whether pane id is a live top pane, and whether it
// is an ended one.
func (s *Server) stewardState(id string) (live, ended bool) {
	if id == "" {
		return false, false
	}
	p, ok := s.tree.Pane(id)
	if !ok {
		return false, false
	}
	return !p.Closed, p.Closed
}

// talkText is text as one line of printable words: a line break would
// submit part of the words and type the rest into whatever the first part
// started, and a control character could drive the steward's terminal.
func talkText(text string) string {
	text = strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f':
			return ' '
		case r < 0x20 || r == 0x7f:
			return -1
		}
		return r
	}, text)
	return strings.Join(strings.Fields(text), " ")
}

// validAct reports whether a client act id is empty (none) or 1 to 64 of
// letters, digits and . : - _ .
func validAct(act string) bool {
	if len(act) > 64 {
		return false
	}
	for i := 0; i < len(act); i++ {
		c := act[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == ':', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// actParam reads the optional act of floor.talk. ok is false when act is
// present and is neither a string nor null: that is an error to the sender,
// not "no act" (FM-R-65).
func actParam(r *proto.Request) (act string, ok bool) {
	raw, has := r.Params["act"]
	if !has || string(bytes.TrimSpace(raw)) == "null" {
		return "", true
	}
	return r.Str("act")
}

// handleFloorTalk sends one sentence of the owner's to the floor's
// steward. With no steward running it starts one first: the default
// harness, in the steward's own directory, labelled steward. The words
// wait in a queue and go in order, after the page. The reply comes at
// once and says where the words go.
func (s *Server) handleFloorTalk(_ *Client, r *proto.Request) proto.Response {
	raw, _ := r.Str("text")
	text := talkText(raw)
	if text == "" {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, "floor.talk needs text: the words for the steward.")
	}
	if s.isClosed() {
		return proto.ErrResp(r.ID, proto.ErrServerClosed, "this server has closed. It cannot take words for the steward.")
	}
	if act, ok := actParam(r); !ok || !validAct(act) {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, "act must be 1 to 64 letters, digits, dots, colons, dashes or underscores: an id the sender makes for this sentence.")
	}
	if s.chatMode() {
		return s.talkChat(r, strings.TrimSpace(raw))
	}
	s.talkMu.Lock()
	defer s.talkMu.Unlock()
	if len(s.talk.lines) >= maxTalkQueue {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, fmt.Sprintf(
			"the steward has %d sentences waiting already. Look at its window, then talk again.", len(s.talk.lines)))
	}
	id := s.trackedSteward()
	live, ended := s.stewardState(id)
	started, note := false, ""
	if !live {
		var bad *proto.Response
		id, bad = s.startSteward(r.ID, "")
		if bad != nil {
			return *bad
		}
		started = true
		if ended {
			note = "The steward had ended. A new one starts, and your words go to it."
		}
	} else {
		if id != s.talk.pane {
			// The server started this steward before a restart, so it
			// got its page then.
			s.talk.pane, s.talk.paged = id, true
		}
		s.openStandingNote(id)
		if ev, ok := s.effectiveCurrent(id); ok && ev.State == proto.StateBlocked {
			note = "The steward waits on a question. Answer it in its window, and your words go after."
		}
	}
	// The owner's words go to the chat file before they are typed, so they
	// show while the steward starts and outlive a steward that ends.
	s.writeChat("user", id, text)
	s.talk.lines = append(s.talk.lines, text)
	queued := len(s.talk.lines)
	s.runTalkLocked()
	if note != "" {
		s.Note(note, id)
	}
	out := map[string]any{"pane": id, "label": s.labelOf(id), "started": started, "queued": queued}
	if note != "" {
		out["note"] = note
	}
	return proto.OKResp(r.ID, out)
}

// handleFloorSteward starts the floor's steward with the harness the
// request names, or the default harness, and gives it its page. It
// refuses while the steward runs, so talk never has two places to go.
func (s *Server) handleFloorSteward(_ *Client, r *proto.Request) proto.Response {
	if s.isClosed() {
		return proto.ErrResp(r.ID, proto.ErrServerClosed, "this server has closed. It cannot start a steward.")
	}
	if s.chatMode() {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			"the steward runs as a chat in this mode. It has no pane to start. Talk to it with floor.talk.")
	}
	harness, _ := r.Str("harness")
	s.talkMu.Lock()
	defer s.talkMu.Unlock()
	if id := s.trackedSteward(); id != "" {
		if live, _ := s.stewardState(id); live {
			return proto.ErrResp(r.ID, proto.ErrBadRequest, fmt.Sprintf(
				"the steward %s (%s) runs already. Close it first, or talk to it.", s.labelOf(id), id))
		}
	}
	id, bad := s.startSteward(r.ID, harness)
	if bad != nil {
		return *bad
	}
	s.runTalkLocked()
	return proto.OKResp(r.ID, map[string]any{"pane": id, "label": s.labelOf(id), "started": true})
}

// runTalkLocked starts the goroutine that types into the steward, unless
// it runs. The caller holds talkMu.
func (s *Server) runTalkLocked() {
	if !s.talk.running {
		s.talk.running = true
		go s.runTalk()
	}
}

// startSteward starts the floor's steward with harness, or the default
// harness when it is "", records its id, and points the queue at it. It
// answers the new pane's id, or the refusal. The caller holds talkMu.
func (s *Server) startSteward(reqID, harness string) (string, *proto.Response) {
	if harness == "" {
		name, ok := defaultHarnessName()
		if !ok {
			resp := proto.ErrResp(reqID, proto.ErrBadRequest,
				fmt.Sprintf("no default harness in %s, so no steward can start. Run coppice open HARNESS once to set one.", config.Path()))
			return "", &resp
		}
		harness = name
	}
	dir := s.stewardDir()
	var err error
	if !s.writeData(func() { err = os.MkdirAll(dir, 0o700) }) {
		resp := proto.ErrResp(reqID, proto.ErrServerClosed, "this server has closed. It cannot start a steward.")
		return "", &resp
	}
	if err != nil {
		resp := proto.ErrResp(reqID, proto.ErrInternal, "cannot make the steward's directory "+dir+": "+err.Error())
		return "", &resp
	}
	mountArgs, mountEnv, mountNote := s.stewardMount(harness)
	s.toolsMounted.Store(len(mountArgs) > 0 || len(mountEnv) > 0)
	create := map[string]any{
		"id": reqID, "cmd": "pane.create", "kind": "pty", "harness": harness, "cwd": dir, "label": DefaultStewardLabel,
	}
	if len(mountArgs) > 0 {
		create["args"] = mountArgs
	}
	if len(mountEnv) > 0 {
		create["env"] = mountEnv
	}
	b, _ := json.Marshal(create)
	var req proto.Request
	if err := json.Unmarshal(b, &req); err != nil {
		resp := proto.ErrResp(reqID, proto.ErrInternal, err.Error())
		return "", &resp
	}
	resp := s.handlePaneCreate(nil, &req)
	if !resp.OK {
		resp.ID = reqID
		return "", &resp
	}
	res, _ := resp.Result.(map[string]any)
	id, _ := res["pane"].(string)
	if s.trackedSteward() != "" {
		s.writeChat("note", id, newStewardNote)
	}
	s.setSteward(id)
	s.talk.pane, s.talk.paged = id, false
	if mountNote != "" {
		s.Note(mountNote, id)
	}
	return id, nil
}

// runTalk types the queued words into the steward, one at a time, until
// the queue is empty. A new steward first gets its page. Words wait while
// the steward waits on a question, since an Enter there would answer it.
func (s *Server) runTalk() {
	for {
		s.talkMu.Lock()
		if len(s.talk.lines) == 0 && len(s.talk.reports) == 0 && (s.talk.paged || s.talk.pane == "") {
			s.talk.running = false
			s.talkMu.Unlock()
			return
		}
		id, paged := s.talk.pane, s.talk.paged
		s.talkMu.Unlock()

		var err error
		if !paged {
			if err = s.pageSteward(id); err == nil {
				s.talkMu.Lock()
				if s.talk.pane == id {
					s.talk.paged = true
				}
				s.talkMu.Unlock()
				continue
			}
		} else {
			err = s.settle(id, func(ev proto.PaneStateEvent) bool { return ev.State != proto.StateBlocked }, false,
				s.labelOf(id)+" waits on a question. Answer it in its window, and your words go after.")
			if err == nil {
				s.talkMu.Lock()
				if s.talk.pane != id || len(s.talk.lines)+len(s.talk.reports) == 0 {
					s.talkMu.Unlock()
					continue
				}
				var line string
				if len(s.talk.lines) > 0 {
					line = s.talk.lines[0]
					s.talk.lines = s.talk.lines[1:]
				} else {
					line = s.talk.reports[0]
					s.talk.reports = s.talk.reports[1:]
					if len(s.talk.reports) == 0 {
						s.talk.dropNoted = false
					}
				}
				s.talkMu.Unlock()
				if err = s.typeInto(id, line); err == nil {
					s.markInput(id, true)
					continue
				}
				err = errStewardGone
			}
		}
		s.talkMu.Lock()
		if errors.Is(err, errTalkStopped) {
			s.talk.running = false
			s.talkMu.Unlock()
			return
		}
		if s.talk.pane != id {
			// A new steward took the queue while this one ended.
			s.talkMu.Unlock()
			continue
		}
		lost := len(s.talk.lines)
		lostReports := len(s.talk.reports)
		s.talk.pane, s.talk.paged, s.talk.lines, s.talk.reports, s.talk.running = "", false, nil, nil, false
		s.talk.dropNoted = false
		s.talkMu.Unlock()
		label := s.labelOf(id)
		if lostReports > 0 {
			// The reports are cleared with the steward; its workers' logs
			// are not, so say so.
			s.Note(lostReportsNote(label, lostReports), id)
		}
		if !paged {
			s.Note(label+" ended before it was ready, so your words were not sent. Its last screen is in Recent. Talk again to start a new one.", id)
		} else if lost > 0 {
			s.Note(label+" ended, so your last words were not sent. Talk again to start a new one.", id)
		}
		return
	}
}

// pageSteward gives a new steward its page once it drew its screen and
// went idle, then waits for the turn the page starts to end.
func (s *Server) pageSteward(id string) error {
	notReady := s.labelOf(id) + " is not ready yet. If it asks something, answer it in its window. Your words wait."
	idle := func(ev proto.PaneStateEvent) bool { return ev.State == proto.StateIdle }
	if err := s.settle(id, idle, true, notReady); err != nil {
		return err
	}
	// The page starts a turn. A turn can start and end between two polls
	// (a hook says working, then idle, a millisecond apart), so a waiter
	// that sees every state from before the page went catches its start.
	// The start can still come from no source at all, so the wait for it
	// is bounded (CP-R-51).
	started, drop := s.watchState(id, proto.StateWorking)
	defer drop()
	if err := s.sendPage(id); err != nil {
		return errStewardGone
	}
	end := time.Now().Add(s.stewardWait)
	for time.Now().Before(end) {
		if ev, ok := s.effectiveCurrent(id); ok && ev.State != proto.StateIdle {
			break
		}
		select {
		case <-started:
			return s.settle(id, idle, false, notReady)
		default:
		}
		if err := s.talkSleep(id); err != nil {
			return err
		}
	}
	return s.settle(id, idle, false, notReady)
}

// stewardPage is the text pasted into a new steward: the header line,
// then the skill page.
func stewardPage() string {
	return skills.FloorHeader + "\n\n" + skills.Steward
}

// sendPage types the page into pane id. A pane whose program has
// bracketed paste on gets the whole page as one paste and then Enter. A
// pane without it gets the one line that tells it to print the page for
// itself, since a line break would send part of the page.
func (s *Server) sendPage(id string) error {
	lp, ok := s.Live(id)
	if !ok {
		return errStewardGone
	}
	s.markInput(id, true)
	if lp.Adapter != nil {
		return lp.Adapter.Prompt(s.stewardPageText())
	}
	if lp.Grid == nil || !lp.Grid.PasteMode() {
		return lp.write([]byte(skills.FloorLine + "\r"))
	}
	if err := lp.write([]byte("\x1b[200~" + s.stewardPageText() + "\x1b[201~")); err != nil {
		return err
	}
	time.Sleep(pasteGap)
	return lp.write([]byte("\r"))
}

// settle waits until pane id's state passes ok. With fresh, the pane
// must also have drawn its screen, and the state must be newer than the
// first thing it drew: a new pane reads idle before its first byte, and
// that idle says nothing about whether the harness is ready. A harness
// that keeps drawing while it is idle still passes. It posts note once
// when the wait passes stewardWait.
func (s *Server) settle(id string, ok func(proto.PaneStateEvent) bool, fresh bool, note string) error {
	start := time.Now()
	noted := false
	for {
		lp, live := s.Live(id)
		if rec, ok := s.tree.Pane(id); !live || !ok || rec.Closed {
			return errStewardGone
		}
		ev, has := s.effectiveCurrent(id)
		newer := !fresh
		wardenNewer := !fresh
		if fresh && lp.Grid != nil {
			if first, drawn := lp.Grid.FirstWrite(); drawn {
				newer = ev.TS*1e9 >= float64(first.UnixNano())
				// A fact older than the first draw says nothing, but a
				// pane that drew and then kept quiet for processQuiet is
				// ready all the same. The process source would say so
				// with an idle fact; a hook idle older than the first
				// draw keeps that fact from being sent (CP-R-56 holds
				// the burst), so the queue reads the quiet itself.
				if last, _ := lp.Grid.LastWrite(); !newer && s.clock().Sub(last) >= processQuiet {
					newer = true
				}
				// The hook idle must also postdate the first draw, as
				// any state must for a fresh pane.
				wardenNewer = lp.wardenIdleSince.Load() >= first.UnixNano()
			}
		}
		if has && newer && ok(ev) {
			return nil
		}
		if has && wardenNewer && s.wardenIdleStands(lp, ev) && ok(withState(ev, proto.StateIdle)) {
			return nil
		}
		if !noted && time.Since(start) > s.stewardWait {
			noted = true
			s.Note(note, id)
		}
		if err := s.talkSleep(id); err != nil {
			return err
		}
	}
}

// wardenIdleSettle is how long a warden idle must stand before the talk queue
// takes it over a process working fact (CP-R-57).
const wardenIdleSettle = 3 * time.Second

// wardenIdleStands reports whether the pane's last warden fact is an idle that
// is wardenIdleSettle old and the merged state is only the process source
// saying working (CP-R-57). The hooks are the harness's own word that it
// waits at its prompt; a screen that redraws does not outrank them.
func (s *Server) wardenIdleStands(lp *LivePane, ev proto.PaneStateEvent) bool {
	since := lp.wardenIdleSince.Load()
	return since != 0 && ev.State == proto.StateWorking && ev.Source == proto.SrcProcess &&
		s.clock().Sub(time.Unix(0, since)) >= wardenIdleSettle
}

// withState is ev with its state replaced.
func withState(ev proto.PaneStateEvent, st string) proto.PaneStateEvent {
	ev.State = st
	return ev
}

// talkSleep waits one poll. It fails when the server closes or when the
// steward's process ended.
func (s *Server) talkSleep(id string) error {
	select {
	case <-s.talkStop:
		return errTalkStopped
	case <-time.After(talkPoll):
	}
	if rec, ok := s.tree.Pane(id); !ok || rec.Closed {
		return errStewardGone
	}
	return nil
}

// talkFields is what New sets up for the talk queue.
type talkFields struct {
	talkMu      sync.Mutex
	talk        talkQueue
	talkStop    chan struct{}
	stewardWait time.Duration
}

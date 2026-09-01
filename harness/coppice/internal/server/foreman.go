package server

import (
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

// DefaultForemanLabel is the label of the floor's foreman when the config
// file names none.
const DefaultForemanLabel = "foreman"

// DefaultForemanWait is how long the talk queue waits on the foreman
// before it posts a note, and how long it waits for the page's turn to
// start.
const DefaultForemanWait = 30 * time.Second

// talkPoll is how often the talk queue looks at the foreman's state.
const talkPoll = 100 * time.Millisecond

// pasteGap is the pause between a paste and the Enter that sends it. A
// harness that reads fast input as more of the paste would swallow an
// Enter that came with it.
var pasteGap = 150 * time.Millisecond

// talkQueue is the owner's words on their way to the floor's foreman. One
// goroutine at a time types them, in order. talkMu guards it.
type talkQueue struct {
	// pane is the foreman the words go to, or "".
	pane string
	// paged is true once pane has its page, or when pane was started
	// somewhere else and so got its page there.
	paged bool
	// lines waits to be typed, oldest first.
	lines []string
	// running is true while the goroutine that types runs.
	running bool
	// foreman is the id of the pane the server started as the floor's
	// foreman, and loaded is true once it was read from the data dir.
	foreman string
	loaded  bool
}

// errForemanGone is why the queue stopped: the foreman's process ended or
// its record went away.
var errForemanGone = errors.New("the foreman ended")

// errTalkStopped is why the queue stopped: the server closes.
var errTalkStopped = errors.New("the server closes")

// RegisterForemanCommands wires up floor.talk and floor.foreman.
func (s *Server) RegisterForemanCommands() {
	_ = s.Handle("floor.talk", s.handleFloorTalk)
	_ = s.Handle("floor.foreman", s.handleFloorForeman)
}

// maxTalkQueue is how many sentences may wait for the foreman. Past it a
// talk is refused, so a stuck foreman cannot pile up words without end.
const maxTalkQueue = 32

// foremanFile is the file in the data dir that names the pane the server
// started as the floor's foreman.
const foremanFile = "foreman.json"

// ForemanDir is the foreman's own working directory:
// $XDG_STATE_HOME/coppice/foreman, else ~/.local/state/coppice/foreman.
// It is scratch space, not a project. The gate refuses every call made
// from inside a directory it guards, and it guards the coppice config and
// data directories, so this one is never inside them.
func ForemanDir() string {
	if st := os.Getenv("XDG_STATE_HOME"); st != "" {
		return filepath.Join(st, "coppice", "foreman")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".local", "state", "coppice", "foreman")
	}
	return filepath.Join(home, ".local", "state", "coppice", "foreman")
}

// foremanDir is this server's foreman directory: Config.ForemanDir, else
// ForemanDir.
func (s *Server) foremanDir() string {
	if s.cfg.ForemanDir != "" {
		return s.cfg.ForemanDir
	}
	return ForemanDir()
}

// isForemanDir reports whether dir is the foreman's own directory, read
// through symlinks.
func (s *Server) isForemanDir(dir string) bool {
	if dir == "" {
		return false
	}
	own := s.foremanDir()
	if r, err := filepath.EvalSymlinks(own); err == nil {
		own = r
	}
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	return filepath.Clean(dir) == filepath.Clean(own)
}

// trackedForeman is the id of the pane the server started as the floor's
// foreman, read once from the data dir, or "". The caller holds talkMu.
// The label is only for display: any pane may carry a label, so the
// server never finds the foreman by it.
func (s *Server) trackedForeman() string {
	if !s.talk.loaded {
		s.talk.loaded = true
		var v struct {
			Pane string `json:"pane"`
		}
		if b, err := os.ReadFile(filepath.Join(s.cfg.DataDir, foremanFile)); err == nil && json.Unmarshal(b, &v) == nil {
			s.talk.foreman = v.Pane
		}
	}
	return s.talk.foreman
}

// setForeman records id as the floor's foreman, in memory and in the data
// dir. The caller holds talkMu.
func (s *Server) setForeman(id string) {
	s.talk.foreman, s.talk.loaded = id, true
	b, _ := json.Marshal(map[string]string{"pane": id})
	path := filepath.Join(s.cfg.DataDir, foremanFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		err = os.Rename(tmp, path)
		if err == nil {
			return
		}
	}
	s.Note("cannot write "+path+". The foreman is known until the server stops.", id)
}

// isTrackedForeman reports whether id is the pane the server tracks as
// the floor's foreman.
func (s *Server) isTrackedForeman(id string) bool {
	s.talkMu.Lock()
	defer s.talkMu.Unlock()
	return id != "" && s.trackedForeman() == id
}

// followForemanLocked makes newID the floor's foreman when it resumes the
// tracked foreman's ended record old, so the next talk goes to it and no
// second foreman starts. A resumed session had its page already. A fresh
// start gets it now. The caller holds talkMu.
func (s *Server) followForemanLocked(old, newID string, resumed bool) {
	if old == "" || !s.talk.loaded || s.talk.foreman != old {
		return
	}
	s.setForeman(newID)
	s.talk.pane, s.talk.paged = newID, resumed
	if !resumed {
		s.runTalkLocked()
	}
}

// foremanState reports whether pane id is a live top pane, and whether it
// is an ended one.
func (s *Server) foremanState(id string) (live, ended bool) {
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
// started, and a control character could drive the foreman's terminal.
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

// handleFloorTalk sends one sentence of the owner's to the floor's
// foreman. With no foreman running it starts one first: the default
// harness, in the foreman's own directory, labelled foreman. The words
// wait in a queue and go in order, after the page. The reply comes at
// once and says where the words go.
func (s *Server) handleFloorTalk(_ *Client, r *proto.Request) proto.Response {
	raw, _ := r.Str("text")
	text := talkText(raw)
	if text == "" {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, "floor.talk needs text: the words for the foreman.")
	}
	if s.isClosed() {
		return proto.ErrResp(r.ID, proto.ErrServerClosed, "this server has closed. It cannot take words for the foreman.")
	}
	s.talkMu.Lock()
	defer s.talkMu.Unlock()
	if len(s.talk.lines) >= maxTalkQueue {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, fmt.Sprintf(
			"the foreman has %d sentences waiting already. Look at its window, then talk again.", len(s.talk.lines)))
	}
	id := s.trackedForeman()
	live, ended := s.foremanState(id)
	started, note := false, ""
	if !live {
		var bad *proto.Response
		id, bad = s.startForeman(r.ID, "")
		if bad != nil {
			return *bad
		}
		started = true
		if ended {
			note = "The foreman had ended. A new one starts, and your words go to it."
		}
	} else {
		if id != s.talk.pane {
			// The server started this foreman before a restart, so it
			// got its page then.
			s.talk.pane, s.talk.paged = id, true
		}
		if ev, ok := s.effectiveCurrent(id); ok && ev.State == proto.StateBlocked {
			note = "The foreman waits on a question. Answer it in its window, and your words go after."
		}
	}
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

// handleFloorForeman starts the floor's foreman with the harness the
// request names, or the default harness, and gives it its page. It
// refuses while the foreman runs, so talk never has two places to go.
func (s *Server) handleFloorForeman(_ *Client, r *proto.Request) proto.Response {
	if s.isClosed() {
		return proto.ErrResp(r.ID, proto.ErrServerClosed, "this server has closed. It cannot start a foreman.")
	}
	harness, _ := r.Str("harness")
	s.talkMu.Lock()
	defer s.talkMu.Unlock()
	if id := s.trackedForeman(); id != "" {
		if live, _ := s.foremanState(id); live {
			return proto.ErrResp(r.ID, proto.ErrBadRequest, fmt.Sprintf(
				"the foreman %s (%s) runs already. Close it first, or talk to it.", s.labelOf(id), id))
		}
	}
	id, bad := s.startForeman(r.ID, harness)
	if bad != nil {
		return *bad
	}
	s.runTalkLocked()
	return proto.OKResp(r.ID, map[string]any{"pane": id, "label": s.labelOf(id), "started": true})
}

// runTalkLocked starts the goroutine that types into the foreman, unless
// it runs. The caller holds talkMu.
func (s *Server) runTalkLocked() {
	if !s.talk.running {
		s.talk.running = true
		go s.runTalk()
	}
}

// startForeman starts the floor's foreman with harness, or the default
// harness when it is "", records its id, and points the queue at it. It
// answers the new pane's id, or the refusal. The caller holds talkMu.
func (s *Server) startForeman(reqID, harness string) (string, *proto.Response) {
	if harness == "" {
		name, ok := defaultHarnessName()
		if !ok {
			resp := proto.ErrResp(reqID, proto.ErrBadRequest,
				fmt.Sprintf("no default harness in %s, so no foreman can start. Run coppice open HARNESS once to set one.", config.Path()))
			return "", &resp
		}
		harness = name
	}
	dir := s.foremanDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		resp := proto.ErrResp(reqID, proto.ErrInternal, "cannot make the foreman's directory "+dir+": "+err.Error())
		return "", &resp
	}
	b, _ := json.Marshal(map[string]any{
		"id": reqID, "cmd": "pane.create", "kind": "pty", "harness": harness, "cwd": dir, "label": DefaultForemanLabel,
	})
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
	s.setForeman(id)
	s.talk.pane, s.talk.paged = id, false
	return id, nil
}

// runTalk types the queued words into the foreman, one at a time, until
// the queue is empty. A new foreman first gets its page. Words wait while
// the foreman waits on a question, since an Enter there would answer it.
func (s *Server) runTalk() {
	for {
		s.talkMu.Lock()
		if len(s.talk.lines) == 0 && (s.talk.paged || s.talk.pane == "") {
			s.talk.running = false
			s.talkMu.Unlock()
			return
		}
		id, paged := s.talk.pane, s.talk.paged
		s.talkMu.Unlock()

		var err error
		if !paged {
			if err = s.pageForeman(id); err == nil {
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
				if s.talk.pane != id || len(s.talk.lines) == 0 {
					s.talkMu.Unlock()
					continue
				}
				line := s.talk.lines[0]
				s.talk.lines = s.talk.lines[1:]
				s.talkMu.Unlock()
				if err = s.typeInto(id, line); err == nil {
					s.markInput(id, true)
					continue
				}
				err = errForemanGone
			}
		}
		s.talkMu.Lock()
		if errors.Is(err, errTalkStopped) {
			s.talk.running = false
			s.talkMu.Unlock()
			return
		}
		if s.talk.pane != id {
			// A new foreman took the queue while this one ended.
			s.talkMu.Unlock()
			continue
		}
		lost := len(s.talk.lines)
		s.talk.pane, s.talk.paged, s.talk.lines, s.talk.running = "", false, nil, false
		s.talkMu.Unlock()
		label := s.labelOf(id)
		if !paged {
			s.Note(label+" ended before it was ready, so your words were not sent. Its last screen is in Recent. Talk again to start a new one.", id)
		} else if lost > 0 {
			s.Note(label+" ended, so your last words were not sent. Talk again to start a new one.", id)
		}
		return
	}
}

// pageForeman gives a new foreman its page once it drew its screen and
// went idle, then waits for the turn the page starts to end.
func (s *Server) pageForeman(id string) error {
	notReady := s.labelOf(id) + " is not ready yet. If it asks something, answer it in its window. Your words wait."
	idle := func(ev proto.PaneStateEvent) bool { return ev.State == proto.StateIdle }
	if err := s.settle(id, idle, true, notReady); err != nil {
		return err
	}
	if err := s.sendPage(id); err != nil {
		return errForemanGone
	}
	// The page starts a turn. Its start can be missed, or come from no
	// source at all, so the wait for it is short.
	end := time.Now().Add(s.foremanWait)
	for time.Now().Before(end) {
		if ev, ok := s.effectiveCurrent(id); ok && ev.State != proto.StateIdle {
			break
		}
		if err := s.talkSleep(id); err != nil {
			return err
		}
	}
	return s.settle(id, idle, false, notReady)
}

// foremanPage is the text pasted into a new foreman: the header line,
// then the skill page.
func foremanPage() string {
	return skills.FloorHeader + "\n\n" + skills.Foreman
}

// sendPage types the page into pane id. A pane whose program has
// bracketed paste on gets the whole page as one paste and then Enter. A
// pane without it gets the one line that tells it to print the page for
// itself, since a line break would send part of the page.
func (s *Server) sendPage(id string) error {
	lp, ok := s.Live(id)
	if !ok {
		return errForemanGone
	}
	s.markInput(id, true)
	if lp.Adapter != nil {
		return lp.Adapter.Prompt(foremanPage())
	}
	if lp.Grid == nil || !lp.Grid.PasteMode() {
		return lp.write([]byte(skills.FloorLine + "\r"))
	}
	if err := lp.write([]byte("\x1b[200~" + foremanPage() + "\x1b[201~")); err != nil {
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
// when the wait passes foremanWait.
func (s *Server) settle(id string, ok func(proto.PaneStateEvent) bool, fresh bool, note string) error {
	start := time.Now()
	noted := false
	for {
		lp, live := s.Live(id)
		if rec, ok := s.tree.Pane(id); !live || !ok || rec.Closed {
			return errForemanGone
		}
		ev, has := s.effectiveCurrent(id)
		newer := !fresh
		if fresh && lp.Grid != nil {
			if first, drawn := lp.Grid.FirstWrite(); drawn {
				newer = ev.TS*1e9 >= float64(first.UnixNano())
			}
		}
		if has && newer && ok(ev) {
			return nil
		}
		if !noted && time.Since(start) > s.foremanWait {
			noted = true
			s.Note(note, id)
		}
		if err := s.talkSleep(id); err != nil {
			return err
		}
	}
}

// talkSleep waits one poll. It fails when the server closes or when the
// foreman's process ended.
func (s *Server) talkSleep(id string) error {
	select {
	case <-s.talkStop:
		return errTalkStopped
	case <-time.After(talkPoll):
	}
	if rec, ok := s.tree.Pane(id); !ok || rec.Closed {
		return errForemanGone
	}
	return nil
}

// talkFields is what New sets up for the talk queue.
type talkFields struct {
	talkMu      sync.Mutex
	talk        talkQueue
	talkStop    chan struct{}
	foremanWait time.Duration
}

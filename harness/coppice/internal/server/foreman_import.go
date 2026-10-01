package server

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/opendaisugi/coppice/internal/foreman"
	"github.com/opendaisugi/coppice/internal/proto"
)

// Import of old Claude Code and Codex chats (FM-R-71 to FM-R-77). The owner
// chooses it, with a click or with coppice foreman import; the server never
// starts it. The compactor loop feeds the queue one message at a time,
// after the live nodes and never while a turn runs.

// ImportRefusal is the message a pane or a plugin gets for floor.import. A
// pane must never trigger an import of the owner's chat history.
const ImportRefusal = "only the operator imports old chats into the foreman."

// importHomeDir is the home dir whose transcripts import reads.
func (s *Server) importHomeDir() string {
	if h := s.cfg.Foreman.ImportHome; h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}

// turnsIdle is true when no turn runs and none waits for its turn.
func (s *Server) turnsIdle() bool {
	s.talkMu.Lock()
	defer s.talkMu.Unlock()
	return !s.turns.running && len(s.turns.queue) == 0
}

// turnsBusy is true when a turn runs or waits: no import call starts then.
func (s *Server) turnsBusy() bool { return !s.turnsIdle() }

// importSkip are the dirs whose sessions import never takes: coppice's own
// (FM-R-78).
func (s *Server) importSkip() []string {
	return []string{s.foremanDir(), s.foremanLogDir(), s.cfg.DataDir}
}

// importOpts are the scan options the state holds, with the skip dirs.
func (s *Server) importOpts(st foreman.ImportState) foreman.ImportOpts {
	o := st.Opts()
	o.Skip = s.importSkip()
	return o
}

// importWanted is true when the owner chose to import and the queue holds
// work that is not paused.
func (fc *foremanChat) importWanted() bool {
	st, err := foreman.ReadImportState(fc.dir)
	return err == nil && st.Decision == "accepted" && !st.Paused && len(st.Queue) > 0
}

// importFeed logs one queued message when the chat is drained and no turn
// runs. It reports whether it logged one, so the loop drains again.
func (s *Server) importFeed(fc *foremanChat, ctx context.Context) bool {
	if fc.imp == nil || !fc.importWanted() {
		return false
	}
	fed, err := fc.imp.Feed(ctx, fc.chat, &fc.mu, s.turnsIdle)
	if err != nil {
		if ctx.Err() == nil {
			s.noteOnce("The foreman could not import a past message: " + err.Error() + ". Nothing is lost. It tries again.")
		}
		return false
	}
	if fed {
		// The floor reads progress when the chat changes. A message every
		// 2 s is enough; past messages are not in the floor's chat.
		fc.estMu.Lock()
		due := time.Since(fc.impEvent) > 2*time.Second
		if due {
			fc.impEvent = time.Now()
		}
		fc.estMu.Unlock()
		if due {
			s.sendMessagesEvent("", true)
		}
	}
	return fed
}

// beginEstimate says whether the caller runs the scan (started, and done
// to close when it ends) or must wait for the one that runs (FM-R-83).
func (fc *foremanChat) beginEstimate() (run chan struct{}, gen int, started bool) {
	fc.estMu.Lock()
	defer fc.estMu.Unlock()
	if fc.estRun != nil {
		return fc.estRun, 0, false
	}
	fc.estRun = make(chan struct{})
	return fc.estRun, fc.estGen, true
}

// scan runs the scan a started caller owns and closes run. A scan that began
// before an accept does not set the estimate.
func (s *Server) scan(fc *foremanChat, ctx context.Context, run chan struct{}, gen int) []foreman.ImportSession {
	// The flight is freed even when the scan panics (F-2).
	defer fc.endFlight(run)
	fc.estMu.Lock()
	fc.scans++
	hold := fc.scanHold
	fc.estMu.Unlock()
	if hold != nil {
		hold()
	}
	st, _ := foreman.ReadImportState(fc.dir)
	home := s.importHomeDir()
	sig := foreman.TranscriptSig(home)
	ss := foreman.ScanContext(ctx, home, s.importOpts(st), st, nil)
	fc.estMu.Lock()
	if ctx.Err() == nil && gen == fc.estGen {
		fc.est, fc.estOK, fc.estSig, fc.estAt = ss, true, sig, time.Now()
	}
	fc.estMu.Unlock()
	if ctx.Err() != nil {
		return nil
	}
	return ss
}

// endFlight ends the flight run stands for and wakes its waiters. It is safe
// to call from a defer.
func (fc *foremanChat) endFlight(run chan struct{}) {
	fc.estMu.Lock()
	fc.estRun = nil
	fc.estMu.Unlock()
	close(run)
}

// estimateFresh is true when the cached estimate may stand for an accept:
// it exists, is under a minute old, and the transcript files are as they
// were when it was scanned (F-1).
func (s *Server) estimateFresh(fc *foremanChat) bool {
	fc.estMu.Lock()
	ok, sig, at := fc.estOK, fc.estSig, fc.estAt
	fc.estMu.Unlock()
	return ok && time.Since(at) < estimateMaxAge && foreman.TranscriptSig(s.importHomeDir()) == sig
}

// estimateMaxAge is how long a cached estimate may serve an accept.
const estimateMaxAge = 60 * time.Second

// estimate is what could be imported now, scanned once and kept. The scan
// reads every transcript, so it runs beside the chat and never under its
// lock, and only one runs at a time: a caller that finds one running waits
// for it and shares what it found.
func (s *Server) estimate(fc *foremanChat, ctx context.Context, force bool) []foreman.ImportSession {
	for {
		fc.estMu.Lock()
		if fc.estOK && !force {
			ss := fc.est
			fc.estMu.Unlock()
			return ss
		}
		fc.estMu.Unlock()
		run, gen, started := fc.beginEstimate()
		if started {
			return s.scan(fc, ctx, run, gen)
		}
		select {
		case <-run:
		case <-ctx.Done():
			return nil
		}
		fc.estMu.Lock()
		ok, ss := fc.estOK, fc.est
		fc.estMu.Unlock()
		if ok {
			return ss
		}
		// The scan we joined began before an accept and set no estimate:
		// what it found is old. Ask again.
	}
}

// scanWith scans with options of the caller's own, in the one flight: it
// waits for a running scan, then holds the flight, so two full parses never
// run at once. It sets no estimate (the options are not the state's).
func (s *Server) scanWith(fc *foremanChat, ctx context.Context, o foreman.ImportOpts) []foreman.ImportSession {
	for {
		run, _, started := fc.beginEstimate()
		if started {
			return s.scanOwn(fc, ctx, run, o)
		}
		select {
		case <-run:
		case <-ctx.Done():
			return nil
		}
	}
}

// scanOwn is the scan a started scanWith owns. The flight is freed even on a
// panic (F-2); a cancelled scan gives nothing (F-4).
func (s *Server) scanOwn(fc *foremanChat, ctx context.Context, run chan struct{}, o foreman.ImportOpts) []foreman.ImportSession {
	defer fc.endFlight(run)
	fc.estMu.Lock()
	hold := fc.scanHold
	fc.estMu.Unlock()
	if hold != nil {
		hold()
	}
	st, _ := foreman.ReadImportState(fc.dir)
	ss := foreman.ScanContext(ctx, s.importHomeDir(), o, st, nil)
	if ctx.Err() != nil {
		return nil
	}
	return ss
}

// sameScan is true when o scans what the shared estimate scans.
func sameScan(o, base foreman.ImportOpts) bool {
	norm := func(f string) string {
		if f == "" {
			return "all"
		}
		return f
	}
	return norm(o.From) == norm(base.From) && o.Tools == base.Tools && o.Since.Equal(base.Since)
}

// offerImport puts the offer in the chat once, when old chats exist and the
// owner has not chosen yet. It runs after the chat opens, off the chat
// lock.
func (s *Server) offerImport(fc *foremanChat) {
	st, err := foreman.ReadImportState(fc.dir)
	if err != nil || st.Offered || st.Decision != "" {
		return
	}
	ss := s.estimate(fc, fc.ctx, true)
	all, _ := foreman.Totals(ss)
	if all.Sessions == 0 {
		return
	}
	first := false
	if err := foreman.UpdateImportState(fc.dir, func(st *foreman.ImportState) error {
		first = !st.Offered && st.Decision == ""
		st.Offered = true
		return nil
	}); err != nil || !first {
		return
	}
	text := fmt.Sprintf("I can bring in %s (about %s) from Claude Code and Codex. Press Import on the floor, or run: coppice foreman import. Nothing is read into the chat until you do. Imported messages are summarized by the foreman's model like any chat message.",
		importPlural(all.Sessions, "past chat"), importPlural(all.Messages, "message"))
	_ = s.chatAppend(fc, foreman.KindNote, text)
}

func importPlural(n int, one string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %ss", n, one)
}

// importReply is floor.import's answer.
type importReply struct {
	foreman.ImportStatus
	// Sessions and Messages are what could be imported now.
	Sessions   int  `json:"sessions"`
	Messages   int  `json:"messages"`
	Estimating bool `json:"estimating"`
	// Note is set when an accept found nothing to queue.
	Note string `json:"note,omitempty"`
}

func (s *Server) importStatus(fc *foremanChat) importReply {
	st, _ := foreman.ReadImportState(fc.dir)
	rep := importReply{ImportStatus: foreman.StatusOf(st)}
	if st.Decision == "declined" {
		// The owner said no: no transcript is read for the floor's sake.
		return rep
	}
	fc.estMu.Lock()
	ok, ss := fc.estOK, fc.est
	fc.estMu.Unlock()
	if !ok {
		rep.Estimating = true
		// Reading every transcript takes a while: start it, answer now. One
		// scan runs at a time; a status call that finds one running only
		// says estimating.
		if run, gen, started := fc.beginEstimate(); started {
			fc.bg.Add(1)
			go func() {
				defer fc.bg.Done()
				s.scan(fc, fc.ctx, run, gen)
				s.sendMessagesEvent("", true)
			}()
		}
		return rep
	}
	all, _ := foreman.Totals(ss)
	rep.Sessions, rep.Messages = all.Sessions, all.Messages
	return rep
}

// handleFloorImport is floor.import: status, accept, decline, pause or
// resume. accept takes optional from, since and tools like the command.
func (s *Server) handleFloorImport(_ *Client, r *proto.Request) proto.Response {
	if !s.chatMode() {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, "import needs the foreman in chat mode: set mode = \"chat\" in [foreman].")
	}
	fc := s.ensureChat()
	if fc.chat == nil {
		return proto.ErrResp(r.ID, proto.ErrInternal, fc.problem)
	}
	action, _ := r.Str("action")
	var err error
	nothing := false
	switch action {
	case "", "status":
	case "progress":
		// The state alone, with no scan: the TUI's bar asks this often.
		st, _ := foreman.ReadImportState(fc.dir)
		return proto.OKResp(r.ID, importReply{ImportStatus: foreman.StatusOf(st)})
	case "accept":
		from, _ := r.Str("from")
		if from == "" {
			from = "all"
		}
		if from != "claude" && from != "codex" && from != "all" {
			return proto.ErrResp(r.ID, proto.ErrBadRequest, `floor.import from must be claude, codex or all.`)
		}
		o := foreman.ImportOpts{From: from, Skip: s.importSkip()}
		o.Tools, _ = r.Bool("tools")
		if since, _ := r.Str("since"); since != "" {
			t, perr := time.Parse("2006-01-02", since)
			if perr != nil {
				return proto.ErrResp(r.ID, proto.ErrBadRequest, "floor.import since must be a date like 2026-03-01.")
			}
			o.Since = t.UTC()
		}
		st, _ := foreman.ReadImportState(fc.dir)
		// The Import button shares the single-flight scan (IM-R-12); only
		// options that differ from the estimate's scan on their own, still
		// one at a time.
		var ss []foreman.ImportSession
		if sameScan(o, s.importOpts(st)) {
			// The cache serves only while fresh; else this joins or starts a
			// scan (F-1).
			ss = s.estimate(fc, fc.ctx, !s.estimateFresh(fc))
		} else {
			ss = s.scanWith(fc, fc.ctx, o)
		}
		if fc.ctx.Err() != nil {
			return proto.ErrResp(r.ID, proto.ErrInternal, "the import was cancelled: coppice is stopping. Nothing was queued.")
		}
		if len(ss) > 0 {
			err = foreman.Accept(fc.dir, o, ss)
		} else {
			nothing = true
		}
		fc.estMu.Lock()
		fc.estOK = false
		fc.estGen++
		fc.estMu.Unlock()
	case "decline":
		err = foreman.Decline(fc.dir)
	case "pause":
		err = foreman.SetPaused(fc.dir, true)
	case "resume":
		err = foreman.SetPaused(fc.dir, false)
	default:
		return proto.ErrResp(r.ID, proto.ErrBadRequest, "floor.import action must be status, progress, accept, decline, pause or resume.")
	}
	if err != nil {
		return proto.ErrResp(r.ID, proto.ErrInternal, "cannot write the import state in "+fc.dir+": "+err.Error())
	}
	if action == "accept" || action == "resume" {
		fc.wake()
	}
	rep := s.importStatus(fc)
	if nothing {
		rep.Note = "nothing new to import"
	}
	return proto.OKResp(r.ID, rep)
}

package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/opendaisugi/coppice/internal/foreman"
)

// Failover of the chat-mode foreman (FM-R-45 to FM-R-54). [foreman]
// turn_profiles is an ordered chain. A turn runs on the first profile that is
// not cooling down. When that profile is out (a quota, an overload, not
// reachable) it cools down and the same turn input goes to the next one. When
// the cool-down ends, the next turn tries the first profile again. The judge
// of a failure and the cool-down arithmetic are in internal/foreman
// (failover.go); this file runs the turns over them.

// turnChain is the chain of turn profiles: turn_profiles when it is set,
// else turn_profile as a chain of one.
func (f ForemanConfig) turnChain() []string {
	if len(f.TurnProfiles) > 0 {
		return f.TurnProfiles
	}
	return []string{f.TurnProfile}
}

// isClaudeProfile reports whether a profile name means Claude Code.
func isClaudeProfile(p string) bool { return p == "" || p == "default" }

// runnerFor is the runner of account i of the chain. Every account serves
// the one model, turn_model.
func (s *Server) runnerFor(i int, profile string) TurnRunner {
	if s.turns.runnerFor != nil {
		return s.turns.runnerFor(profile)
	}
	if s.turns.runner != nil {
		return s.turns.runner
	}
	model := s.cfg.Foreman.TurnModel
	if isClaudeProfile(profile) {
		return &claudeTurn{bin: claudeBin(), model: model, self: s.turns.self,
			wrap: func(dir string) ([]string, error) { return s.fenceCmd("claude", dir, []string{dir}, false) }}
	}
	return &sprigTurn{s: s, profile: profile, model: model, self: s.turns.self}
}

// failoverChain is the chain of this server, loaded from the state file on
// first use and written back at each change.
func (s *Server) failoverChain(fc *foremanChat) *foreman.Chain {
	s.turns.chainMu.Lock()
	if s.turns.chain != nil {
		defer s.turns.chainMu.Unlock()
		return s.turns.chain
	}
	c, _ := foreman.LoadChain(fc.dir, s.cfg.Foreman.turnChain(), s.clock())
	s.turns.chain = c
	_ = c.Save(fc.dir, s.clock())
	s.turns.chainMu.Unlock()
	if n := c.LoadNote(); n != "" {
		s.chainNote(fc, n)
	}
	return c
}

// checkModelsAtOpen runs after the chat opens and its lock is released: it
// checks that the accounts of the turn chain serve one model, once, in the
// chat (FM-R-54, FM-R-58). It stops with the chat.
func (s *Server) checkModelsAtOpen(fc *foremanChat) {
	ctx, cancel := context.WithTimeout(fc.ctx, 10*time.Second)
	defer cancel()
	if bad, _ := s.chainModelsProblem(ctx); bad != "" && fc.ctx.Err() == nil {
		s.chainNote(fc, bad+" Fix [foreman] in the config, then start coppice again.")
	}
}

// wakePause ends the wait of a paused turn, if one waits: an owner message
// does this (FM-R-55).
func (s *Server) wakePause() {
	s.turns.chainMu.Lock()
	cancel := s.turns.pauseCancel
	s.turns.chainMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// ownerWaiting reports whether an owner message waits behind this turn.
func (s *Server) ownerWaiting() bool {
	s.talkMu.Lock()
	defer s.talkMu.Unlock()
	return len(s.turns.queue) > 0
}

func (s *Server) setPause(cancel context.CancelFunc) {
	s.turns.chainMu.Lock()
	s.turns.pauseCancel = cancel
	s.turns.chainMu.Unlock()
}

func (s *Server) saveChain(fc *foremanChat, c *foreman.Chain) {
	if err := c.Save(fc.dir, s.clock()); err != nil {
		s.noteOnce("The foreman's failover state could not be saved: " + err.Error())
	}
}

// failoverSleep waits d, or until ctx ends. A test replaces it.
func (s *Server) failoverSleep(ctx context.Context, d time.Duration) error {
	if s.turns.sleep != nil {
		return s.turns.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// clockText is a time of day for a note, in UTC like the log.
func clockText(t time.Time) string { return t.UTC().Format("15:04") + " UTC" }

// whenText is clockText, with the date when t is not on the day of now.
func whenText(t, now time.Time) string {
	if t.UTC().Format("2006-01-02") == now.UTC().Format("2006-01-02") {
		return clockText(t)
	}
	return t.UTC().Format("Jan 2 15:04") + " UTC"
}

// outList names each account that cools and until when, earliest end last
// in no particular order: "account Claude until Oct 24 09:00 UTC and account
// desk until 14:05 UTC".
func (s *Server) outList(ch *foreman.Chain) string {
	now := s.clock()
	var outs []string
	for _, r := range ch.Status(now) {
		if r.State == "cooling" {
			until, _ := time.Parse(foreman.DateLayout, r.Until)
			outs = append(outs, fmt.Sprintf("account %s until %s", r.Name, whenText(until, now)))
		}
	}
	return strings.Join(outs, " and ")
}

// errTurnTimeout ends a chain run when one attempt took longer than the turn
// may. It does not move on: a slow model is not an outage.
var errTurnTimeout = errors.New("the turn timed out")

// loggedStart reports whether the write-ahead log holds a start line for
// this turn. It reads the file, not the memory of the process, so a turn
// with a tool start is not replayed even when the count was lost (FM-R-50,
// FM-R-66).
func (ts *turnSocket) loggedStart() bool {
	if ts.turn < 0 || ts.fc == nil {
		return false
	}
	recs, _ := foreman.ReadCalls(ts.fc.dir)
	return foreman.TurnHasToolStart(recs, ts.turn)
}

// chainRun is what runChain gives back.
type chainRun struct {
	res TurnResult
	// idx is the profile the turn ran on.
	idx int
	// note is the one note to log after the reply: a move, or a move back.
	note string
}

// runChain runs one turn over the chain. The same input goes to each profile
// in turn, never twice to one profile in a pass. A turn that ran a tool on a
// profile before it failed is not replayed on another: its effects would
// repeat. When every profile is cooling, the turn waits for the earliest end.
func (s *Server) runChain(ctx context.Context, fc *foremanChat, ts *turnSocket, in TurnInput) (chainRun, error) {
	ch := s.failoverChain(fc)
	if err := s.checkChainModels(ctx); err != nil {
		return chainRun{}, err
	}
	passes := 0
	i := ch.First(s.clock())
	for {
		if i < 0 {
			until := ch.Earliest(s.clock())
			if until.IsZero() {
				i = ch.First(s.clock())
				if i < 0 {
					return chainRun{}, errors.New("no turn profile can run")
				}
				continue
			}
			passes++
			now := s.clock()
			// The wake hook is set before the note, so an owner message in
			// between still ends the pause (FM-R-55).
			if s.turns.pauseGap != nil {
				s.turns.pauseGap()
			}
			pctx, pcancel := context.WithCancel(ctx)
			s.setPause(pcancel)
			// A message that came before the hook was set is already queued:
			// it ends the pause too (FM-R-55).
			if s.ownerWaiting() {
				pcancel()
			}
			if s.turns.pauseSet != nil {
				s.turns.pauseSet()
			}
			if passes == 1 {
				s.chainNote(fc, fmt.Sprintf("The foreman is paused: every account is out (%s). It tries again at %s, or when you talk.", s.outList(ch), whenText(until, now)))
			} else {
				s.chainNote(fc, fmt.Sprintf("The foreman is paused again (wait %d of this turn): every account is out again (%s). Each repeat waits longer, up to 2 hours. It tries again at %s, or when you talk.", passes, s.outList(ch), whenText(until, now)))
			}
			serr := s.failoverSleep(pctx, until.Sub(now))
			s.setPause(nil)
			pcancel()
			if ctx.Err() != nil {
				return chainRun{}, ctx.Err()
			}
			i = ch.First(s.clock())
			if serr != nil && i < 0 {
				// The owner spoke and the accounts are still out: say so.
				return chainRun{}, fmt.Errorf("every account is still out (%s)", s.outList(ch))
			}
			if i >= 0 {
				why := "the pause ended"
				if serr != nil {
					why = "you spoke, so the pause ended"
				}
				s.chainNote(fc, fmt.Sprintf("The foreman is no longer paused (%s). It tries account %s now.", why, foreman.ProfileName(ch.Profile(i))))
			}
			continue
		}
		profile := ch.Profile(i)
		before := ts.calls.Load()
		actx, cancel := context.WithTimeout(ctx, s.cfg.Foreman.TurnTimeout)
		res, err := s.runnerFor(i, profile).Run(actx, in)
		timedOut := errors.Is(actx.Err(), context.DeadlineExceeded)
		cancel()
		if err == nil {
			prev := ch.LastRan()
			if prev < 0 {
				prev = 0
			}
			ch.MarkOK(i)
			ch.MarkRan(i)
			s.saveChain(fc, ch)
			return chainRun{res: res, idx: i, note: s.moveNote(ch, prev, i)}, nil
		}
		if ctx.Err() != nil {
			return chainRun{}, ctx.Err()
		}
		if timedOut {
			return chainRun{}, errTurnTimeout
		}
		now := s.clock()
		name := foreman.ProfileName(profile)
		v := foreman.Classify(failText(err), now)
		switch v.Outcome {
		case foreman.KeyProblem:
			ch.MarkFailing(i, v.Reason)
			s.saveChain(fc, ch)
			return chainRun{}, fmt.Errorf("%s on %s, so the foreman did not move to the next profile", v.Reason, name)
		case foreman.Out:
			until := ch.MarkOut(i, v, now)
			s.saveChain(fc, ch)
			if ts.calls.Load() > before || ts.loggedStart() {
				return chainRun{}, fmt.Errorf("%s ran out (%s) after it had used tools in this turn, so the turn was not run again on another profile: its effects could repeat. Until %s", name, v.Reason, whenText(until, now))
			}
			i = ch.Next(i, s.clock())
		default:
			ch.MarkFailing(i, foreman.SafeReason(failText(err)))
			s.saveChain(fc, ch)
			return chainRun{}, errors.New(safeTurnError(err))
		}
	}
}

// moveNote is the note for a turn that ran on profile idx when the last turn
// ran on prev: that the earlier profiles are out and until when, or that the
// foreman is back on a profile it prefers. Empty when nothing moved.
func (s *Server) moveNote(ch *foreman.Chain, prev, idx int) string {
	if idx == prev {
		return ""
	}
	name := foreman.ProfileName(ch.Profile(idx))
	if idx < prev {
		return fmt.Sprintf("account %s is back; this turn ran on account %s.", name, name)
	}
	var outs []string
	for j, r := range ch.Status(s.clock()) {
		if j < idx && r.State == "cooling" {
			until, _ := time.Parse(foreman.DateLayout, r.Until)
			outs = append(outs, fmt.Sprintf("account %s is out until %s", r.Name, whenText(until, s.clock())))
		}
	}
	if len(outs) == 0 {
		return ""
	}
	return fmt.Sprintf("%s; this turn ran on account %s.", strings.Join(outs, " and "), name)
}

// lastTurnProfile is the name of the profile the last turn ran on, for the
// stack bar; false before any turn has run.
func (s *Server) lastTurnProfile() (string, bool) {
	s.turns.chainMu.Lock()
	defer s.turns.chainMu.Unlock()
	if s.turns.chain == nil {
		return "", false
	}
	i := s.turns.chain.LastRan()
	if i < 0 {
		return "", false
	}
	return foreman.ProfileName(s.turns.chain.Profile(i)), true
}

// modelMismatch finds two models in a chain that must serve one: the
// turn model and the model a profile names for itself. It returns both
// models and false when they agree. models maps a profile name to its own
// model ("" for none).
func modelMismatch(chain []string, turnModel string, models map[string]string) (a, b string, bad bool) {
	for _, p := range chain {
		if isClaudeProfile(p) {
			continue
		}
		if m := models[p]; m != "" && m != turnModel {
			return turnModel, m, true
		}
	}
	return "", "", false
}

// chainModelsProblem finds a turn chain whose accounts serve two models. It
// reads the operator's profile table, so it needs the daisugi bundle, and it
// looks only at a turn chain of more than one account. The text is "" when
// all is well. FM-R-54. Delegation to other models is not in its reach.
func (s *Server) chainModelsProblem(ctx context.Context) (string, error) {
	chain := s.cfg.Foreman.turnChain()
	if len(chain) < 2 || s.turns.runnerFor != nil || s.turns.runner != nil {
		return "", nil
	}
	table, err := s.operatorBundle(ctx)
	if err != nil {
		return "", err
	}
	models := map[string]string{}
	for p, e := range table {
		if e.Model != nil {
			models[p] = *e.Model
		}
	}
	if a, b, bad := modelMismatch(chain, s.cfg.Foreman.TurnModel, models); bad {
		return fmt.Sprintf("The foreman's turn chain serves two models: turn_model is %q and an account serves %q. The foreman never moves to another model; a turn chain holds only accounts of one model.", a, b), nil
	}
	return "", nil
}

// checkChainModels is the backstop at each turn: it refuses a turn chain of
// two models before any account runs.
func (s *Server) checkChainModels(ctx context.Context) error {
	bad, err := s.chainModelsProblem(ctx)
	if err != nil {
		return err
	}
	if bad != "" {
		return errors.New(strings.TrimSuffix(strings.Replace(bad, "The foreman's turn chain", "the turn chain", 1), "."))
	}
	return nil
}

// chainNote logs a note in the middle of a turn.
func (s *Server) chainNote(fc *foremanChat, text string) {
	if err := s.chatAppend(fc, foreman.KindNote, text); err != nil {
		s.noteOnce("The foreman's log could not take a note: " + err.Error())
		return
	}
	s.Note(text, "")
}

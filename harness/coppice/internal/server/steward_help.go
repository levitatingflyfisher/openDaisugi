package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"daisugi-verify/pando"
	"github.com/opendaisugi/coppice/internal/proto"
)

// A worker can ask for help (the help tool of sprig). need_info and
// escalate end its run with a question: the worker then holds no process
// and no model call. The question is one work message. The steward answers
// with its reply tool, and the worker goes on as a new segment of the same
// session, with the answer as its next message. A question not answered
// within the project's help_wait_s ends the worker as given up, with the
// question in its report. give_up ends it at once. Every call is journaled
// with the worker label and the kind.

// helpToolName is the tool of a worker that can ask.
const helpToolName = "help"

// DefaultHelpWaitS is how long a question waits for its answer when the
// project's envelope file names no help_wait_s.
const DefaultHelpWaitS = 3600

// helpAsk is the question a waiting worker put. Deadline is on the
// server's clock.
type helpAsk struct {
	Kind     string `json:"kind"`
	Text     string `json:"text"`
	Deadline string `json:"deadline"`
}

// helpWait is how long a question of this project waits.
func (s *Server) helpWait(pe *projectEnvelope) time.Duration {
	s.workMu.Lock()
	over := s.helpWaitFor
	s.workMu.Unlock()
	if over > 0 {
		return over
	}
	if pe != nil && pe.HelpWaitS > 0 {
		return time.Duration(pe.HelpWaitS) * time.Second
	}
	return DefaultHelpWaitS * time.Second
}

// workerHelp handles a segment that ended by help.
func (s *Server) workerHelp(ctx context.Context, fc *stewardChat, w *workerRec, pe *projectEnvelope, session, kind, text string) {
	if ctx.Err() != nil {
		return
	}
	label := "[" + w.Label + "]"
	text = strings.TrimSpace(text)
	switch kind {
	case "need_info", "escalate", "give_up":
	default:
		s.finishWorker(ctx, fc, w, workFailed, session, label+" failed: the worker's help had the kind "+oneLine(kind, 40))
		return
	}
	s.noteVerdict(w.Label, &proto.Verdict{Decision: "help", Tool: "help", Clause: kind}, "")
	if kind == "give_up" {
		report := label + " gave up\n" + quoted(text) + "\n" + s.changesLine(fc, w, session)
		s.finishWorker(ctx, fc, w, workGaveUp, session, report)
		return
	}
	wait := s.helpWait(pe)
	deadline := s.clock().Add(wait).UTC().Format(pando.DateLayout)
	s.workMu.Lock()
	w.State, w.Session = workWaiting, session
	w.SessionFile = s.workerSessionPath(fc, session, w.Fenced)
	w.Ask = &helpAsk{Kind: kind, Text: text, Deadline: deadline}
	s.armHelpLocked(fc, w, wait)
	s.saveWorkersLocked(fc)
	s.workMu.Unlock()
	if kind == "escalate" {
		s.logWork(fc, label+" asks the owner\n"+quoted(text)+"\n(This is for the owner. Tell the owner, then pass the answer on with reply.)")
		s.Note(w.Label+" asks the owner: "+oneLine(text, 200)+". Answer it in the steward's chat.", "")
		return
	}
	s.logWork(fc, label+" asks a question\n"+quoted(text)+"\n(Answer it with reply.)")
}

// armHelpLocked starts the timer that ends w's wait. The caller holds
// workMu.
func (s *Server) armHelpLocked(fc *stewardChat, w *workerRec, d time.Duration) {
	if s.helpTimers == nil {
		s.helpTimers = map[string]*time.Timer{}
	}
	if old := s.helpTimers[w.Label]; old != nil {
		old.Stop()
	}
	s.helpTimers[w.Label] = time.AfterFunc(d, func() { s.expireHelp(fc, w) })
}

// rearmHelpLocked arms the timer of a waiting worker the table held at
// start. It returns a line to log when the time has already passed: the
// worker is then given up. The caller holds workMu.
func (s *Server) rearmHelpLocked(fc *stewardChat, w *workerRec) string {
	if w.Ask != nil {
		if at, err := time.Parse(pando.DateLayout, w.Ask.Deadline); err == nil {
			if rem := at.Sub(s.clock()); rem > 0 {
				s.armHelpLocked(fc, w, rem)
				return ""
			}
		}
	}
	ask := w.Ask
	w.State, w.Ask = workGaveUp, nil
	if ask == nil {
		return "[" + w.Label + "] gave up: its question was lost when coppice stopped"
	}
	return "[" + w.Label + "] gave up: no answer came in time\nquestion (" + ask.Kind + "):\n" + quoted(ask.Text)
}

// expireHelp ends a waiting worker whose question went unanswered.
func (s *Server) expireHelp(fc *stewardChat, w *workerRec) {
	s.workMu.Lock()
	if w.State != workWaiting || w.Ask == nil {
		s.workMu.Unlock()
		return
	}
	ask, session := *w.Ask, w.Session
	w.State, w.Ask = workGaveUp, nil
	delete(s.helpTimers, w.Label)
	ctx := s.workCtx
	s.workMu.Unlock()
	if ctx == nil {
		return
	}
	text := "[" + w.Label + "] gave up: no answer came in time\nquestion (" + ask.Kind + "):\n" + quoted(ask.Text) +
		"\n" + s.changesLine(fc, w, session)
	s.finishWorker(ctx, fc, w, workGaveUp, session, text)
}

// toolReply is the steward's reply tool: it answers the question of a
// waiting worker, and the worker goes on with the answer as its next
// message.
func (s *Server) toolReply(fc *stewardChat, label, answer string) (string, error) {
	if strings.TrimSpace(answer) == "" {
		return "", errors.New("the answer is empty")
	}
	s.loadWorkers(fc)
	s.workMu.Lock()
	var w *workerRec
	for _, x := range s.work {
		if x.Label == label {
			w = x
		}
	}
	if w == nil {
		s.workMu.Unlock()
		return "", fmt.Errorf("no worker named %q. status lists them", label)
	}
	if w.State != workWaiting {
		state := w.State
		s.workMu.Unlock()
		return "", fmt.Errorf("%s is not waiting for an answer (it is %s)", label, state)
	}
	project, dir := w.Project, w.Dir
	s.workMu.Unlock()
	// The same check as a spawn: a default grant is not resumed bare.
	pe, err := s.projectEnvelopeFor(project, dir)
	if err != nil {
		return "", err
	}
	s.workMu.Lock()
	if s.isClosed() {
		s.workMu.Unlock()
		return "", errors.New("the server is closing")
	}
	if w.State != workWaiting {
		state := w.State
		s.workMu.Unlock()
		return "", fmt.Errorf("%s is not waiting for an answer (it is %s)", label, state)
	}
	w.State, w.Ask = workRunning, nil
	if t := s.helpTimers[label]; t != nil {
		t.Stop()
		delete(s.helpTimers, label)
	}
	session := w.Session
	s.saveWorkersLocked(fc)
	s.workWG.Add(1)
	ctx := s.workCtx
	s.workMu.Unlock()
	go s.runWorkerStep(ctx, fc, w, pe, answer, "", session)
	return "answered " + label + "; it goes on", nil
}

// changesLine is the line a given-up report ends with: the files the worker
// wrote or edited, from its session file, sorted.
func (s *Server) changesLine(fc *stewardChat, w *workerRec, session string) string {
	if session == "" {
		return "changed: nothing"
	}
	return changedFiles(s.workerSessionPath(fc, session, w.Fenced))
}

// changedFiles reads a sprig session file and lists the paths of its write
// and edit calls.
func changedFiles(sessionFile string) string {
	f, err := os.Open(sessionFile)
	if err != nil {
		return "changed: unknown (no session file)"
	}
	defer f.Close()
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for sc.Scan() {
		var e struct {
			Type  string         `json:"type"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Type != "tool_call" || (e.Name != "write" && e.Name != "edit") {
			continue
		}
		if p, ok := e.Input["path"].(string); ok && p != "" {
			seen[p] = true
		}
	}
	if len(seen) == 0 {
		return "changed: nothing"
	}
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return "changed: " + strings.Join(paths, ", ")
}

// quoted is a worker's own text with "> " before each line, so no line of
// it can pass for a line coppice writes, such as another worker's
// "[label] asks a question".
func quoted(text string) string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return strings.Join(lines, "\n")
}

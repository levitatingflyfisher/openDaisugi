package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/opendaisugi/coppice/internal/proto"
)

// journalKept is how many gate verdicts the journal keeps and the most one
// floor.journal reply holds.
const journalKept = 500

// journalPaneKept is the most entries one pane holds in the journal, so a
// pane that reports a verdict on every tool call cannot push the others
// out. When a pane passes it, its own oldest entry goes.
const journalPaneKept = 100

// journalPush adds e at the end of ring. If its pane now holds more than
// journalPaneKept, that pane's oldest entry goes, never another pane's.
// Then the ring is cut to its last journalKept.
func journalPush(ring []journalEntry, e journalEntry) []journalEntry {
	ring = append(ring, e)
	n := 0
	for _, x := range ring {
		if x.Pane == e.Pane {
			n++
		}
	}
	if n > journalPaneKept {
		for i, x := range ring {
			if x.Pane == e.Pane {
				ring = append(ring[:i], ring[i+1:]...)
				break
			}
		}
	}
	if len(ring) > journalKept {
		ring = ring[len(ring)-journalKept:]
	}
	return ring
}

// journalFile is where the journal lives, under the data dir.
var journalFile = filepath.Join("journal", "verdicts.jsonl")

// journalEntry is one verdict a gate told the server about. At is unix
// seconds on the server's clock.
type journalEntry struct {
	At       float64 `json:"at"`
	Pane     string  `json:"pane"`
	Decision string  `json:"decision"`
	Tool     string  `json:"tool"`
	Clause   string  `json:"clause"`
	Mode     string  `json:"mode,omitempty"`
}

// parseJournalLine reads one line of the file. The rule is the same in the
// Go and the Rust server: the line is one JSON object, at is a number,
// pane and decision are strings that are not empty, tool, clause and mode
// are strings when they are there (null is not there), and a key is
// matched by its exact spelling. Anything else is not a verdict.
func parseJournalLine(line []byte) (journalEntry, bool) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(line, &raw) != nil || raw == nil {
		return journalEntry{}, false
	}
	str := func(k string, dst *string) bool {
		v, ok := raw[k]
		if !ok || string(v) == "null" {
			return true
		}
		return json.Unmarshal(v, dst) == nil
	}
	var e journalEntry
	at, ok := raw["at"]
	if !ok || string(at) == "null" || json.Unmarshal(at, &e.At) != nil {
		return journalEntry{}, false
	}
	if !str("pane", &e.Pane) || !str("decision", &e.Decision) || !str("tool", &e.Tool) ||
		!str("clause", &e.Clause) || !str("mode", &e.Mode) {
		return journalEntry{}, false
	}
	return e, e.Pane != "" && e.Decision != ""
}

// verdictJournal is the last journalKept verdicts the server was told,
// oldest first, and the file that keeps them across a restart.
type verdictJournal struct {
	mu     sync.Mutex
	loaded bool
	ring   []journalEntry
	// lines is how many lines the file holds. The file is cut back to the
	// ring when it holds twice the ring.
	lines int
}

// loadJournalLocked reads the file once. It replays the lines through the
// quotas an append meets, cuts the file back to the ring and makes it 0600.
// A torn last line (a crash in the middle of an append) is dropped alone. A
// file with a bad line before the last moves aside to <name>.broken-<UTC time> with a note, and the journal
// starts fresh. The caller holds j.mu.
func (s *Server) loadJournalLocked() {
	j := &s.journal
	if j.loaded {
		return
	}
	j.loaded = true
	path := filepath.Join(s.cfg.DataDir, journalFile)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		go s.Note("cannot read "+path+". The verdict journal starts empty and is not saved.", "")
		return
	}
	var lines [][]byte
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) != 0 {
			lines = append(lines, line)
		}
	}
	var ring []journalEntry
	for i, line := range lines {
		e, ok := parseJournalLine(line)
		if !ok {
			// A crash in the middle of an append leaves a torn last
			// line. Only that line goes. A bad line before it means
			// the file is not ours to trust.
			if i == len(lines)-1 {
				s.saveDroppedJournalLine(path, line)
				break
			}
			moved := moveAsideFile(path, s.clock())
			go s.Note("The verdict journal could not be read. It moved to "+moved+". The journal starts fresh.", "")
			return
		}
		ring = journalPush(ring, e)
	}
	j.ring, j.lines = ring, len(ring)
	s.rewriteJournalLocked()
}

// saveDroppedJournalLine appends the torn last line of the journal to
// <journal>.dropped (0600) and says so in one note. The line is never
// deleted.
func (s *Server) saveDroppedJournalLine(path string, line []byte) {
	dropped := path + ".dropped"
	var err error
	if !s.writeData(func() {
		var f *os.File
		if f, err = os.OpenFile(dropped, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o600); err != nil {
			return
		}
		_, err = f.Write(append(append([]byte{}, line...), '\n'))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}) || err != nil {
		go s.Note("The last line of the verdict journal could not be read. It is dropped and could not be saved.", "")
		return
	}
	go s.Note("The last line of the verdict journal could not be read. It is dropped and saved to "+dropped+".", "")
}

// rewriteJournalLocked writes the ring over the file, 0600, through a
// temporary file. The caller holds j.mu.
func (s *Server) rewriteJournalLocked() {
	j := &s.journal
	var buf bytes.Buffer
	for _, e := range j.ring {
		line, _ := json.Marshal(e)
		buf.Write(line)
		buf.WriteByte('\n')
	}
	path := filepath.Join(s.cfg.DataDir, journalFile)
	tmp := path + ".tmp"
	var err error
	if !s.writeData(func() {
		if err = os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
			if err = os.WriteFile(tmp, buf.Bytes(), 0o600); err == nil {
				_ = os.Chmod(tmp, 0o600)
				err = os.Rename(tmp, path)
			}
		}
	}) || err != nil {
		return
	}
	j.lines = len(j.ring)
}

// noteVerdict adds one verdict a gate reported to the journal and its file.
func (s *Server) noteVerdict(pane string, v *proto.Verdict, mode string) {
	if v == nil || pane == "" || v.Decision == "" {
		return
	}
	j := &s.journal
	j.mu.Lock()
	defer j.mu.Unlock()
	s.appendJournalLocked(journalEntry{At: unixSeconds(s.clock()), Pane: pane, Decision: v.Decision, Tool: v.Tool, Clause: v.Clause, Mode: mode})
}

// noteTreeVerdict adds a verdict read from a sprig session tree, which no
// gate reports. It carries the tree's own time. A verdict with no usable
// time (none, or one after the server's clock) is skipped, since the server
// cannot place it. It is also skipped when the journal already holds the
// same verdict: the same pane, time, decision, tool and clause. A server
// that restarts and reads the tree again so adds nothing twice. Nor does it
// add a verdict the quota pruned: when the pane holds its quota, a verdict
// older than the pane's oldest entry is skipped.
func (s *Server) noteTreeVerdict(pane string, v gateFact, mode string) {
	if pane == "" || v.Decision == "" {
		return
	}
	if now := unixSeconds(s.clock()); v.At <= 0 || v.At > now {
		return
	}
	e := journalEntry{At: v.At, Pane: pane, Decision: v.Decision, Tool: v.Tool, Clause: v.Clause, Mode: mode}
	j := &s.journal
	j.mu.Lock()
	defer j.mu.Unlock()
	s.loadJournalLocked()
	held, oldest := 0, e.At
	for _, x := range j.ring {
		if x.Pane != e.Pane {
			continue
		}
		if x.At == e.At && x.Decision == e.Decision && x.Tool == e.Tool && x.Clause == e.Clause {
			return
		}
		if held == 0 || x.At < oldest {
			oldest = x.At
		}
		held++
	}
	// A pane at its quota that reads a verdict older than its oldest entry
	// is reading one the quota already pruned. Adding it would push out a
	// newer one.
	if held >= journalPaneKept && e.At < oldest {
		return
	}
	s.appendJournalLocked(e)
}

// appendJournalLocked adds e to the ring and the file. The caller holds
// j.mu.
func (s *Server) appendJournalLocked(e journalEntry) {
	j := &s.journal
	s.loadJournalLocked()
	j.ring = journalPush(j.ring, e)
	if j.lines+1 > 2*journalKept {
		s.rewriteJournalLocked()
		return
	}
	line, _ := json.Marshal(e)
	line = append(line, '\n')
	path := filepath.Join(s.cfg.DataDir, journalFile)
	var err error
	if !s.writeData(func() {
		if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return
		}
		var f *os.File
		if f, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o600); err != nil {
			return
		}
		_, err = f.Write(line)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}) || err != nil {
		return
	}
	j.lines++
}

// handleFloorJournal answers floor.journal: the last verdicts, newest
// first. pane keeps one agent's, since keeps those made after that unix
// time, and limit (absent or below 1 is journalKept, above it too) keeps
// the newest few. A pane gets only its own entries, whatever pane it asks
// for: a pane that asks for another's gets none. A plugin and a peer the
// server cannot place get none, even if their hello names a pane. An
// operator or a view gets all.
func (s *Server) handleFloorJournal(c *Client, r *proto.Request) proto.Response {
	pane, _ := r.Str("pane")
	var since float64
	if raw, ok := r.Raw("since"); ok {
		if json.Unmarshal(raw, &since) != nil {
			return proto.ErrResp(r.ID, proto.ErrBadRequest, "floor.journal since is a unix time in seconds.")
		}
	}
	limit, _ := r.Int("limit")
	if limit < 1 || limit > journalKept {
		limit = journalKept
	}
	if ro := c.roleOf(); ro.noAllow {
		// Only a real pane sees its own lines, by the rule of chatGuard. A
		// plugin or a peer the server cannot place may say in its hello
		// that it is a pane, and still gets nothing.
		real := ro.pane && !ro.unknown && ro.plugin == "" && ro.paneID != ""
		if !real || (pane != "" && pane != ro.paneID) {
			return proto.OKResp(r.ID, map[string]any{"entries": []journalEntry{}})
		}
		pane = ro.paneID
	}
	j := &s.journal
	j.mu.Lock()
	s.loadJournalLocked()
	out := []journalEntry{}
	for i := len(j.ring) - 1; i >= 0 && len(out) < limit; i-- {
		e := j.ring[i]
		if (pane != "" && e.Pane != pane) || (since > 0 && e.At <= since) {
			continue
		}
		out = append(out, e)
	}
	j.mu.Unlock()
	return proto.OKResp(r.ID, map[string]any{"entries": out})
}

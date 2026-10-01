package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// savedTailCap is the most bytes of the gateway's turn journal one read
// takes, from its end. A day of turns fits well inside it.
const savedTailCap = 8 << 20

// savedCache holds what the floor last read from the gateway's turn
// journal, under mu: the frontier tokens saved today for each pane the
// records name. The file is read again when its size, its time or the
// local day changed.
type savedCache struct {
	mu   sync.Mutex
	size int64
	mod  time.Time
	day  string
	by   map[string]int64
}

// gatewayTurnsPath is where the gateway writes one record per routed
// turn: <daisugi data dir>/gateway/turns.jsonl. The data dir is the parent
// of the gate root.
func (s *Server) gatewayTurnsPath() string {
	return filepath.Join(filepath.Dir(s.cfg.GateRoot), "gateway", "turns.jsonl")
}

// savedToday is the frontier tokens the gateway says it saved today, local
// time, on turns that name a pane of this floor. ok is false when the
// gateway has no such figure: no journal, one that cannot be read, or no
// turn today that names a pane here. A turn that names no pane counts for
// no pane. The floor never estimates a saving of its own.
func (s *Server) savedToday(now time.Time) (tokens int64, ok bool) {
	path := s.gatewayTurnsPath()
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return 0, false
	}
	day := now.Format("2006-01-02")
	c := &s.saved
	c.mu.Lock()
	if c.by == nil || c.size != fi.Size() || !c.mod.Equal(fi.ModTime()) || c.day != day {
		by, err := readSavedToday(path, now, s.floorKey())
		if err != nil {
			c.size, c.mod, c.day, c.by = 0, time.Time{}, "", nil
			c.mu.Unlock()
			return 0, false
		}
		c.size, c.mod, c.day, c.by = fi.Size(), fi.ModTime(), day, by
	}
	by := c.by
	c.mu.Unlock()
	found := false
	for _, p := range s.tree.Panes() {
		if v, here := by[p.ID]; here {
			tokens += v
			found = true
		}
	}
	return tokens, found
}

// readSavedToday sums frontier_tokens_saved for each pane over the records
// of the last savedTailCap bytes of path whose created_at falls on now's
// local day. A line that is not a record, or names no pane, is skipped. A
// record that names a floor counts only when it is this one: pane ids
// start at w1:p1 on every floor, and floors share a gateway journal.
func readSavedToday(path string, now time.Time, floor string) (map[string]int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	by := map[string]int64{}
	var r io.Reader = f
	cut := false
	if fi.Size() > savedTailCap {
		if _, err := f.Seek(fi.Size()-savedTailCap, io.SeekStart); err != nil {
			return nil, err
		}
		cut = true
	}
	br := bufio.NewReaderSize(r, 1<<16)
	if cut {
		// The first line is likely half a line.
		if _, err := br.ReadBytes('\n'); err != nil {
			return by, nil
		}
	}
	ty, tm, td := now.Date()
	for {
		line, rerr := br.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			var rec struct {
				CreatedAt string `json:"created_at"`
				Pane      string `json:"pane"`
				Floor     string `json:"floor"`
				Saved     *int64 `json:"frontier_tokens_saved"`
			}
			if json.Unmarshal(line, &rec) == nil && rec.Saved != nil && rec.Pane != "" &&
				(rec.Floor == "" || rec.Floor == floor) {
				if at, perr := time.Parse(time.RFC3339Nano, rec.CreatedAt); perr == nil {
					if y, m, d := at.In(now.Location()).Date(); y == ty && m == tm && d == td {
						by[rec.Pane] += *rec.Saved
					}
				}
			}
		}
		if rerr != nil {
			return by, nil
		}
	}
}

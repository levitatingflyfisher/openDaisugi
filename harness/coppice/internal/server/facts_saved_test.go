//go:build unix

package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// turn is one record of the gateway's turn journal: when, how many
// frontier tokens it saved, and the pane it names, or "".
type turn struct {
	at    string
	saved int
	pane  string
}

// writeTurns writes the gateway's turn journal beside the gate root.
func writeTurns(t *testing.T, s *Server, rows ...turn) {
	t.Helper()
	dir := filepath.Join(filepath.Dir(s.cfg.GateRoot), "gateway")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, r := range rows {
		if r.pane == "" {
			fmt.Fprintf(&b, `{"created_at":%q,"frontier_tokens_saved":%d,"model":"m"}`+"\n", r.at, r.saved)
			continue
		}
		fmt.Fprintf(&b, `{"created_at":%q,"frontier_tokens_saved":%d,"model":"m","pane":%q}`+"\n", r.at, r.saved, r.pane)
	}
	if err := os.WriteFile(filepath.Join(dir, "turns.jsonl"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func savedToday(t *testing.T, s *Server) (any, bool) {
	t.Helper()
	m := result(t, roundTrip(t, s, `{"id":"f","cmd":"floor.facts"}`)[0])
	v, ok := m["saved_today"]
	return v, ok
}

// No gateway figure, no number. The gateway names no pane on a turn today,
// so a turn file with no pane, an empty one, an old one, or one that names
// a pane this floor does not have, gives floor.facts no saving.
func TestFloorFactsNamesNoSavingWithoutAGatewayFigureForAPaneHere(t *testing.T) {
	s := newFactsServer(t)
	s.cfg.GateRoot = filepath.Join(t.TempDir(), "gate")
	if v, ok := savedToday(t, s); ok {
		t.Fatalf("saved_today %v with no turn file", v)
	}
	writeTurns(t, s)
	if v, ok := savedToday(t, s); ok {
		t.Fatalf("saved_today %v with an empty turn file", v)
	}
	now := s.clock().UTC().Format("2006-01-02T15:04:05.000000+00:00")
	old := s.clock().AddDate(0, 0, -3).UTC().Format(time.RFC3339Nano)
	writeTurns(t, s, turn{old, 900, "w1:p1"})
	if v, ok := savedToday(t, s); ok {
		t.Fatalf("saved_today %v with only an old turn", v)
	}
	writeTurns(t, s, turn{now, 900, ""})
	if v, ok := savedToday(t, s); ok {
		t.Fatalf("saved_today %v from a turn that names no pane", v)
	}
	writeTurns(t, s, turn{now, 900, "w7:p7"})
	if v, ok := savedToday(t, s); ok {
		t.Fatalf("saved_today %v from a pane this floor does not have", v)
	}
}

// The saving is the sum of the gateway's own frontier tokens saved, over
// the turns of today, local time, that name a pane of this floor, and no
// more.
func TestFloorFactsSumsTheGatewayTokensSavedTodayForThePanesHere(t *testing.T) {
	s := newFactsServer(t)
	s.cfg.GateRoot = filepath.Join(t.TempDir(), "gate")
	now := s.clock().UTC().Format("2006-01-02T15:04:05.000000+00:00")
	old := s.clock().AddDate(0, 0, -3).UTC().Format(time.RFC3339Nano)
	writeTurns(t, s, turn{old, 900, "w1:p1"}, turn{now, 120, "w1:p1"}, turn{now, 30, "w1:p1"}, turn{now, 500, "w7:p7"}, turn{now, 70, ""})
	v, ok := savedToday(t, s)
	m, _ := v.(map[string]any)
	if !ok || m["tokens"] != float64(150) {
		t.Fatalf("saved_today %v, want tokens 150", v)
	}
	// A new turn shows at the next read.
	writeTurns(t, s, turn{now, 120, "w1:p1"}, turn{now, 30, "w1:p1"}, turn{now, 50, "w1:p1"})
	v, _ = savedToday(t, s)
	m, _ = v.(map[string]any)
	if m["tokens"] != float64(200) {
		t.Fatalf("saved_today %v after a new turn, want 200", v)
	}
}

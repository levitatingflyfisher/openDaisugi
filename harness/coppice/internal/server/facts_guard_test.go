//go:build unix

package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/layout"
)

// hangingOpen is a transcript opener that waits until release is closed,
// the way a read on a stale network mount can wait forever.
func hangingOpen(release <-chan struct{}) func(string) (*os.File, uint64, uint64, bool, error) {
	return func(path string) (*os.File, uint64, uint64, bool, error) {
		<-release
		return openRegular(path)
	}
}

func reportOwn(t *testing.T, s *Server, pane, extra string) {
	t.Helper()
	send, next, stop := ownPane(t, s, pane)
	defer stop()
	send(gateReportLine("r", pane, extra))
	mustOK(t, next())
}

func TestAHangingTranscriptNeverHoldsUpPaneListOrFloorFacts(t *testing.T) {
	s := newFactsServer(t)
	s.pollWait = 20 * time.Millisecond
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	s.openTranscript = hangingOpen(release)
	p := projFixture(t, s, "claude-transcript.jsonl")
	reportOwn(t, s, "w1:p1", `"transcript_path":"`+p+`"`)

	start := time.Now()
	for i := 0; i < 3; i++ {
		if _, ok := listRow(t, s, "w1:p1")["tokens"]; ok {
			t.Fatal("tokens from a read that has not returned")
		}
		if got := roundTrip(t, s, `{"id":"f","cmd":"floor.facts"}`); !got[0].OK {
			t.Fatalf("floor.facts: %+v", got[0].Error)
		}
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("three pane.list and floor.facts calls took %v behind a hanging read", d)
	}
	once.Do(func() { close(release) })
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tok, ok := listRow(t, s, "w1:p1")["tokens"].(map[string]any); ok && tok["out"] == 12.0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the read that came back never showed its tokens")
}

func TestAReadPastItsDeadlineIsNeverTriedAgain(t *testing.T) {
	s := newFactsServer(t)
	s.pollWait = 10 * time.Millisecond
	s.pollDeadline = 20 * time.Millisecond
	release := make(chan struct{})
	s.openTranscript = hangingOpen(release)
	p := projFixture(t, s, "claude-transcript.jsonl")
	reportOwn(t, s, "w1:p1", `"transcript_path":"`+p+`"`)
	listRow(t, s, "w1:p1")
	time.Sleep(60 * time.Millisecond)
	close(release)
	time.Sleep(100 * time.Millisecond)
	for i := 0; i < 3; i++ {
		if _, ok := listRow(t, s, "w1:p1")["tokens"]; ok {
			t.Fatal("tokens from a path whose read hung past its deadline")
		}
	}
}

func TestAPaneHoldsABoundedNumberOfClaims(t *testing.T) {
	s := newFactsServer(t)
	dir := filepath.Join(s.getenv("CLAUDE_CONFIG_DIR"), "projects", "fake-project")
	send, next, stop := ownPane(t, s, "w1:p1")
	defer stop()
	for i := 0; i < maxFilesPerPane+retiredKeep+10; i++ {
		p := filepath.Join(dir, fmt.Sprintf("fake-%03d.jsonl", i))
		send(gateReportLine("r", "w1:p1", `"transcript_path":"`+p+`"`))
		mustOK(t, next())
	}
	s.factsMu.Lock()
	n := 0
	for _, id := range s.claims {
		if id == "w1:p1" {
			n++
		}
	}
	s.factsMu.Unlock()
	if n > maxFilesPerPane+retiredKeep {
		t.Fatalf("the pane holds %d claims, want at most %d", n, maxFilesPerPane+retiredKeep)
	}
}

func TestAModelNameIsCut(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	line := `{"type":"assistant","message":{"id":"m","model":"` + strings.Repeat("m", 500) + `"}}` + "\n"
	if err := os.WriteFile(p, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	tl := newTailer(p, kindClaude)
	if err := tl.poll(); err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(tl.model)); n != 200 {
		t.Fatalf("model is %d runes, want 200", n)
	}
}

func TestOnlyAJSONLFileUnderClaudesProjectsIsRead(t *testing.T) {
	s := newFactsServer(t)
	outside := copyFixture(t, "claude-transcript.jsonl")
	inside := projFixture(t, s, "claude-transcript.jsonl")
	notJSONL := inside + ".txt"
	if err := os.Rename(inside, notJSONL); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{outside, notJSONL} {
		reportOwn(t, s, "w1:p1", `"transcript_path":"`+p+`"`)
		if _, ok := listRow(t, s, "w1:p1")["tokens"]; ok {
			t.Fatalf("%s was read", p)
		}
	}
}

func TestASprigVerdictFromTheFutureIsDatedNow(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s := newTestServer(t)
	s.factsEvery = 0
	s.pollWait = 5 * time.Second
	s.getenv = func(string) string { return "" }
	dir := t.TempDir()
	tree := `{"type":"verdict","id":"v","ts":99999999999.0,"toolUseId":"x","decision":"allow","mode":"enforce","clause":""}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "fake-session.jsonl"), []byte(tree), 0o600); err != nil {
		t.Fatal(err)
	}
	row := s.paneListRow(layout.Pane{
		ID: "w9:p1", Kind: layout.KindHeadless, Harness: "sprig",
		Argv: []string{"sprig", "--session-dir", dir}, HarnessSessionID: "fake-session",
	})
	g, _ := row["gate"].(*gateFact)
	if g == nil || g.At > unixSeconds(time.Now())+1 {
		t.Fatalf("gate %+v, want at no later than now", g)
	}
}

// A transcript an ended pane let go of, then another pane took over,
// counts once in tokens_today.
func TestATranscriptThatMovesPanesCountsOnceToday(t *testing.T) {
	s := newFactsServer(t)
	roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	if err := s.updatePane("w1:p2", func(p *layout.Pane) { p.Harness = "claude" }); err != nil {
		t.Fatal(err)
	}
	first := projFixture(t, s, "claude-transcript.jsonl")
	reportOwn(t, s, "w1:p1", `"transcript_path":"`+first+`"`)
	listRow(t, s, "w1:p1")
	for i := 0; i < maxFilesPerPane; i++ {
		reportOwn(t, s, "w1:p1", `"transcript_path":"`+projFixture(t, s, "claude-transcript-more.jsonl")+`"`)
		listRow(t, s, "w1:p1")
	}
	if err := s.updatePane("w1:p1", func(p *layout.Pane) { p.Closed = true }); err != nil {
		t.Fatal(err)
	}
	reportOwn(t, s, "w1:p2", `"transcript_path":"`+first+`"`)
	listRow(t, s, "w1:p2")
	m := result(t, roundTrip(t, s, `{"id":"f","cmd":"floor.facts"}`)[0])
	tok, _ := m["tokens_today"].(map[string]any)
	want := 12.0 + 3*float64(maxFilesPerPane)
	if tok["out"] != want {
		t.Fatalf("tokens_today out %v, want %v", tok["out"], want)
	}
}

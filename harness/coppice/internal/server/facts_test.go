//go:build unix

package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/layout"
)

// gateReportLine is one pane.report_state a gate hook sends, with extra
// JSON fields spliced into the event.
func gateReportLine(id, pane, extra string) string {
	if extra != "" {
		extra = "," + extra
	}
	return `{"id":"` + id + `","cmd":"pane.report_state","pane":"` + pane + `","event":` +
		`{"v":1,"ts":1757300000.0,"session_id":"fake-sid","harness_session_id":"fake-claude-session",` +
		`"harness":"claude-code","pane":"` + pane + `","state":"working","source":"gate",` +
		`"detail":"verdict=allow"` + extra + `}}`
}

// newFactsServer is a server with one live pty pane, w1:p1, running sh
// and named as a claude pane, and no process environment of its own.
func newFactsServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s := newAgentServer(t)
	s.factsEvery = 0
	s.pollWait = 5 * time.Second
	claudeDir := t.TempDir()
	s.getenv = func(k string) string {
		if k == "CLAUDE_CONFIG_DIR" {
			return claudeDir
		}
		return ""
	}
	got := roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	if !got[0].OK {
		t.Fatalf("pane.create: %+v", got[0].Error)
	}
	if err := s.updatePane("w1:p1", func(p *layout.Pane) { p.Harness = "claude" }); err != nil {
		t.Fatal(err)
	}
	return s
}

// projFixture copies a fake transcript to a new file under the Claude
// projects directory the test server's panes see, where Claude Code keeps
// its transcripts, and returns its path.
func projFixture(t *testing.T, s *Server, name string) string {
	t.Helper()
	dir := filepath.Join(s.getenv("CLAUDE_CONFIG_DIR"), "projects", "fake-project")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "facts", name))
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(dir, "*-"+name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

// ownPane is a connection the kernel placed in pane id, the way a gate
// hook running inside that pane connects.
func ownPane(t *testing.T, s *Server, id string) (send func(string), next func() map[string]any, stop func()) {
	t.Helper()
	return streamFacts(t, s, &peerFacts{checked: true, pane: true, paneID: id})
}

func listRow(t *testing.T, s *Server, id string) map[string]any {
	t.Helper()
	got := roundTrip(t, s, `{"id":"l","cmd":"pane.list"}`)
	m := result(t, got[0])
	panes, _ := m["panes"].([]any)
	for _, p := range panes {
		row, _ := p.(map[string]any)
		if row["id"] == id {
			return row
		}
	}
	t.Fatalf("no row for %s in %v", id, panes)
	return nil
}

func mustOK(t *testing.T, r map[string]any) {
	t.Helper()
	if r["ok"] != true {
		t.Fatalf("reply %v, want ok", r)
	}
}

func TestPaneListShowsTheLastVerdictAndModeAGateReported(t *testing.T) {
	s := newFactsServer(t)
	send, next, stop := ownPane(t, s, "w1:p1")
	defer stop()
	send(gateReportLine("1", "w1:p1", `"mode":"watching","verdict":{"decision":"allow","tool":"Read","clause":""}`))
	mustOK(t, next())
	send(gateReportLine("2", "w1:p1", `"mode":"enforcing","verdict":{"decision":"deny","tool":"Bash","clause":"shell: curl"}`))
	mustOK(t, next())
	row := listRow(t, s, "w1:p1")
	g, _ := row["gate"].(map[string]any)
	if g == nil || g["decision"] != "deny" || g["tool"] != "Bash" || g["clause"] != "shell: curl" {
		t.Fatalf("gate %v, want the last verdict", row["gate"])
	}
	if at, _ := g["at"].(float64); at <= 0 {
		t.Fatalf("gate at %v, want a time", g["at"])
	}
	st, _ := row["stack"].(map[string]any)
	d, _ := st["daisugi"].(map[string]any)
	if st["loop"] != "claude" || d["mode"] != "enforcing" || d["armed"] != true {
		t.Fatalf("stack %v, want loop claude, daisugi enforcing and armed", st)
	}
}

func TestAPaneWithNoHarnessAndNoReportsHasNoFacts(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s := newAgentServer(t)
	s.getenv = func(string) string { return "" }
	roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	row := listRow(t, s, "w1:p1")
	for _, k := range []string{"stack", "gate", "tokens"} {
		if _, ok := row[k]; ok {
			t.Errorf("row has %s = %v, want it absent", k, row[k])
		}
	}
}

func TestAHarnessWithNoGateReportsIsOff(t *testing.T) {
	s := newFactsServer(t)
	row := listRow(t, s, "w1:p1")
	st, _ := row["stack"].(map[string]any)
	d, _ := st["daisugi"].(map[string]any)
	if d["mode"] != "off" {
		t.Fatalf("daisugi %v, want off", st["daisugi"])
	}
	for _, k := range []string{"gate", "tokens"} {
		if _, ok := row[k]; ok {
			t.Errorf("row has %s, want it absent", k)
		}
	}
	if _, ok := st["model"]; ok {
		t.Errorf("stack has a model %v nobody reported", st["model"])
	}
}

func TestTheDisarmMarkerShowsTheGateUnarmed(t *testing.T) {
	s := newFactsServer(t)
	if err := os.MkdirAll(s.cfg.GateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.cfg.GateRoot, "DISARMED"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	row := listRow(t, s, "w1:p1")
	st, _ := row["stack"].(map[string]any)
	d, _ := st["daisugi"].(map[string]any)
	if d["armed"] != false {
		t.Fatalf("daisugi %v, want armed false", d)
	}
}

func TestATranscriptThePanesOwnHookReportedGivesModelAndTokens(t *testing.T) {
	s := newFactsServer(t)
	p := projFixture(t, s, "claude-transcript.jsonl")
	send, next, stop := ownPane(t, s, "w1:p1")
	defer stop()
	send(gateReportLine("1", "w1:p1", `"transcript_path":"`+p+`"`))
	mustOK(t, next())
	row := listRow(t, s, "w1:p1")
	st, _ := row["stack"].(map[string]any)
	if st["model"] != "claude-fake-2" {
		t.Fatalf("model %v, want claude-fake-2", st["model"])
	}
	tok, _ := row["tokens"].(map[string]any)
	if tok["fresh"] != 13.0 || tok["cache_read"] != 300.0 || tok["cache_write"] != 20.0 || tok["out"] != 12.0 {
		t.Fatalf("tokens %v", tok)
	}
	appendFixture(t, p, "claude-transcript-more.jsonl")
	row = listRow(t, s, "w1:p1")
	st, _ = row["stack"].(map[string]any)
	tok, _ = row["tokens"].(map[string]any)
	if st["model"] != "claude-fake-3" || tok["out"] != 15.0 {
		t.Fatalf("after more turns: model %v tokens %v", st["model"], tok)
	}
	b, _ := json.Marshal(row)
	if strings.Contains(string(b), p) {
		t.Fatalf("the row names the transcript path: %s", b)
	}
}

func TestATranscriptPathFromAnotherConnectionIsNeverRead(t *testing.T) {
	s := newFactsServer(t)
	p := projFixture(t, s, "claude-transcript.jsonl")
	got := roundTrip(t, s, gateReportLine("1", "w1:p1", `"transcript_path":"`+p+`"`))
	if !got[0].OK {
		t.Fatalf("report: %+v", got[0].Error)
	}
	row := listRow(t, s, "w1:p1")
	if _, ok := row["tokens"]; ok {
		t.Fatalf("tokens %v read from a path an operator connection named", row["tokens"])
	}
	st, _ := row["stack"].(map[string]any)
	if _, ok := st["model"]; ok {
		t.Fatalf("model %v read from a path an operator connection named", st["model"])
	}
}

func TestAPathAnotherLivePaneReportedIsNotRead(t *testing.T) {
	s := newFactsServer(t)
	roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	p := projFixture(t, s, "claude-transcript.jsonl")
	send1, next1, stop1 := ownPane(t, s, "w1:p1")
	defer stop1()
	send1(gateReportLine("1", "w1:p1", `"transcript_path":"`+p+`"`))
	mustOK(t, next1())
	send2, next2, stop2 := ownPane(t, s, "w1:p2")
	defer stop2()
	send2(gateReportLine("2", "w1:p2", `"transcript_path":"`+p+`"`))
	mustOK(t, next2())
	if _, ok := listRow(t, s, "w1:p2")["tokens"]; ok {
		t.Fatal("a second live pane read the first pane's transcript")
	}
	if _, ok := listRow(t, s, "w1:p1")["tokens"]; !ok {
		t.Fatal("the first pane lost its transcript")
	}
}

func TestAPtyPanesOwnHookSetsItsHarnessSessionID(t *testing.T) {
	s := newFactsServer(t)
	got := roundTrip(t, s, gateReportLine("1", "w1:p1", ""))
	if !got[0].OK {
		t.Fatalf("report: %+v", got[0].Error)
	}
	if rec, _ := s.tree.Pane("w1:p1"); rec.HarnessSessionID != "" {
		t.Fatalf("an operator connection set the session id to %q", rec.HarnessSessionID)
	}
	send, next, stop := ownPane(t, s, "w1:p1")
	defer stop()
	send(gateReportLine("2", "w1:p1", ""))
	mustOK(t, next())
	if rec, _ := s.tree.Pane("w1:p1"); rec.HarnessSessionID != "fake-claude-session" {
		t.Fatalf("session id %q, want the one the pane's own hook reported", rec.HarnessSessionID)
	}
}

func TestABadVerdictRefusesTheReport(t *testing.T) {
	s := newFactsServer(t)
	got := roundTrip(t, s, gateReportLine("1", "w1:p1", `"verdict":{"decision":"perhaps","tool":"Bash"}`))
	if got[0].OK {
		t.Fatal("a report with an unknown decision was taken")
	}
}

func TestASprigPaneReadsItsOwnSessionTree(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s := newTestServer(t)
	s.factsEvery = 0
	s.getenv = func(string) string { return "" }
	tree := copyFixture(t, "sprig-tree.jsonl")
	dir := filepath.Dir(tree)
	if err := os.Rename(tree, filepath.Join(dir, "fake-session.jsonl")); err != nil {
		t.Fatal(err)
	}
	row := s.paneListRow(layout.Pane{
		ID: "w9:p1", Kind: layout.KindHeadless, Harness: "sprig",
		Argv: []string{"sprig", "--session-dir", dir}, HarnessSessionID: "fake-session",
	})
	st, _ := row["stack"].(map[string]any)
	tok, _ := row["tokens"].(Tokens)
	if st["model"] != "sprig-fake-2" || tok.Out != 5 || tok.Fresh != 5 {
		t.Fatalf("stack %v tokens %+v", st, row["tokens"])
	}
	g, _ := row["gate"].(*gateFact)
	if g == nil || g.Decision != "deny" || g.Tool != "bash" {
		t.Fatalf("gate %+v", row["gate"])
	}
	d, _ := st["daisugi"].(map[string]any)
	if d["mode"] != "enforcing" {
		t.Fatalf("daisugi %v, want enforcing from the tree's verdicts", d)
	}
}

// A pane that reports more transcripts than it keeps, then comes back to
// the first one, reads only what was added to it since: every line counts
// once.
func TestAnEvictedTranscriptResumesWhereItStopped(t *testing.T) {
	s := newFactsServer(t)
	send, next, stop := ownPane(t, s, "w1:p1")
	defer stop()
	report := func(p string) {
		send(gateReportLine("r", "w1:p1", `"transcript_path":"`+p+`"`))
		mustOK(t, next())
		listRow(t, s, "w1:p1")
	}
	first := projFixture(t, s, "claude-transcript.jsonl")
	report(first)
	for i := 0; i < maxFilesPerPane; i++ {
		report(projFixture(t, s, "claude-transcript-more.jsonl"))
	}
	report(first)
	appendFixture(t, first, "claude-transcript-more.jsonl")
	tok, _ := listRow(t, s, "w1:p1")["tokens"].(map[string]any)
	// The first file once (13 fresh, 12 out), each other file once
	// (1 fresh, 3 out), and the one appended block (1 fresh, 3 out).
	wantFresh := 13.0 + float64(maxFilesPerPane) + 1
	wantOut := 12.0 + 3*float64(maxFilesPerPane) + 3
	if tok["fresh"] != wantFresh || tok["out"] != wantOut {
		t.Fatalf("tokens %v, want fresh %v out %v", tok, wantFresh, wantOut)
	}
}

// Using a file again keeps it: the file used longest ago is the one let go.
func TestTheLeastRecentlyUsedTranscriptIsTheOneLetGo(t *testing.T) {
	pf := &paneFacts{}
	pf.use("/fake/main.jsonl", kindClaude, nil)
	for i := 0; i < maxFilesPerPane-1; i++ {
		pf.use(filepath.Join("/fake", string(rune('a'+i))+".jsonl"), kindClaude, nil)
		pf.use("/fake/main.jsonl", kindClaude, nil)
	}
	pf.use("/fake/new.jsonl", kindClaude, nil)
	if _, ok := pf.retired["/fake/main.jsonl"]; ok {
		t.Fatal("the file in use was let go")
	}
	if _, ok := pf.files["/fake/main.jsonl"]; !ok {
		t.Fatal("the file in use is gone")
	}
}

// A report whose harness is not the pane's own names nothing the pane
// keeps: not its session, not its transcript.
func TestAReportFromAnotherHarnessSetsNoSessionAndNoTranscript(t *testing.T) {
	s := newFactsServer(t)
	p := projFixture(t, s, "claude-transcript.jsonl")
	send, next, stop := ownPane(t, s, "w1:p1")
	defer stop()
	line := strings.Replace(gateReportLine("1", "w1:p1", `"transcript_path":"`+p+`"`),
		`"harness":"claude-code"`, `"harness":"codex"`, 1)
	send(line)
	mustOK(t, next())
	if rec, _ := s.tree.Pane("w1:p1"); rec.HarnessSessionID != "" {
		t.Fatalf("a codex report set a claude pane's session id to %q", rec.HarnessSessionID)
	}
	if err := s.updatePane("w1:p1", func(x *layout.Pane) { x.Harness = "codex" }); err != nil {
		t.Fatal(err)
	}
	send(gateReportLine("2", "w1:p1", `"transcript_path":"`+p+`"`))
	mustOK(t, next())
	if rec, _ := s.tree.Pane("w1:p1"); rec.HarnessSessionID != "" {
		t.Fatalf("a claude report set a codex pane's session id to %q", rec.HarnessSessionID)
	}
	if _, ok := listRow(t, s, "w1:p1")["tokens"]; ok {
		t.Fatal("a codex pane read a transcript a claude report named")
	}
}

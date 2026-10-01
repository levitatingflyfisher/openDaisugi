//go:build unix

package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/proto"
)

// verdictLine is one gate report that carries a verdict.
func verdictLine(n int, decision, tool string) string {
	v := fmt.Sprintf(`"mode":"enforcing","verdict":{"decision":%q,"tool":%q,"clause":"c%d"}`, decision, tool, n)
	return gateReportLine(fmt.Sprintf("v%d", n), "w1:p1", v)
}

func journalOf(t *testing.T, s *Server, params string) []any {
	t.Helper()
	m := result(t, roundTrip(t, s, `{"id":"j","cmd":"floor.journal"`+params+`}`)[0])
	list, _ := m["entries"].([]any)
	return list
}

// floor.journal lists the verdicts the gate told the server about, newest
// first, and keeps them in a 0600 file.
func TestFloorJournalListsVerdictsNewestFirstInA0600File(t *testing.T) {
	s := newFactsServer(t)
	roundTrip(t, s, verdictLine(1, "allow", "Bash"), verdictLine(2, "deny", "Write"))
	list := journalOf(t, s, "")
	if len(list) != 2 {
		t.Fatalf("entries %v, want 2", list)
	}
	first := list[0].(map[string]any)
	if first["decision"] != "deny" || first["tool"] != "Write" || first["clause"] != "c2" || first["pane"] != "w1:p1" || first["mode"] != "enforcing" {
		t.Fatalf("newest entry %v", first)
	}
	if _, ok := first["at"].(float64); !ok {
		t.Fatalf("entry has no at: %v", first)
	}
	fi, err := os.Stat(filepath.Join(s.cfg.DataDir, "journal", "verdicts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("journal file mode %v, want 0600", fi.Mode().Perm())
	}
}

// A report with no verdict adds nothing.
func TestFloorJournalSkipsReportsWithNoVerdict(t *testing.T) {
	s := newFactsServer(t)
	roundTrip(t, s, gateReportLine("r", "w1:p1", ""))
	if list := journalOf(t, s, ""); len(list) != 0 {
		t.Fatalf("entries %v, want none", list)
	}
}

// pane, since and limit narrow the list.
func TestFloorJournalFiltersByPaneSinceAndLimit(t *testing.T) {
	s := newFactsServer(t)
	roundTrip(t, s, verdictLine(1, "allow", "Bash"))
	mid := journalOf(t, s, "")[0].(map[string]any)["at"].(float64) + 5
	s.clockSkew.Store(int64(10 * time.Second))
	roundTrip(t, s, verdictLine(2, "deny", "Bash"), verdictLine(3, "allow", "Read"))
	if n := len(journalOf(t, s, `,"pane":"w9:p9"`)); n != 0 {
		t.Fatalf("other pane: %d entries", n)
	}
	if n := len(journalOf(t, s, `,"pane":"w1:p1"`)); n != 3 {
		t.Fatalf("this pane: %d entries", n)
	}
	if n := len(journalOf(t, s, fmt.Sprintf(`,"since":%f`, mid))); n != 2 {
		t.Fatalf("since: %d entries, want 2", n)
	}
	list := journalOf(t, s, `,"limit":1`)
	if len(list) != 1 || list[0].(map[string]any)["clause"] != "c3" {
		t.Fatalf("limit 1: %v", list)
	}
	got := roundTrip(t, s, `{"id":"b","cmd":"floor.journal","since":"x"}`)
	if got[0].OK {
		t.Fatalf("a text since was accepted")
	}
}

// The journal keeps 500 and survives a restart.
func TestFloorJournalKeepsFiveHundredAcrossARestart(t *testing.T) {
	s := newFactsServer(t)
	// Six panes, so no pane passes its quota of 100.
	for i := 1; i <= 520; i++ {
		s.noteVerdict(fmt.Sprintf("w1:p%d", i%6+1), &proto.Verdict{Decision: "allow", Tool: "Bash", Clause: fmt.Sprintf("c%d", i)}, "")
	}
	list := journalOf(t, s, "")
	if len(list) != 500 || list[0].(map[string]any)["clause"] != "c520" || list[499].(map[string]any)["clause"] != "c21" {
		t.Fatalf("%d entries, first %v last %v", len(list), list[0], list[len(list)-1])
	}
	dir := s.cfg.DataDir
	_ = s.Close()
	b, err := os.ReadFile(filepath.Join(dir, "journal", "verdicts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "\n"); n > 1000 {
		t.Fatalf("file has %d lines", n)
	}
	s2, err := New(Config{SocketPath: filepath.Join(dir, "server.sock"), DataDir: dir,
		ForemanDir: filepath.Join(dir, "state", "foreman"), HookHome: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	list = journalOf(t, s2, "")
	if len(list) != 500 || list[0].(map[string]any)["clause"] != "c520" {
		t.Fatalf("after restart: %d entries, first %v", len(list), list[0])
	}
	if lines := strings.Count(readJournalFile(t, filepath.Join(dir, "journal", "verdicts.jsonl")), "\n"); lines != 500 {
		t.Fatalf("file has %d lines after the start, want 500", lines)
	}
}

func readJournalFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// goodJournalLine is one well-formed line of the journal file.
const goodJournalLine = `{"at":1.5,"pane":"w1:p1","decision":"allow","tool":"Bash","clause":"c0"}`

// A file with a bad line before the last moves aside and the journal starts fresh.
func TestFloorJournalMovesABrokenFileAside(t *testing.T) {
	s := newFactsServer(t)
	path := filepath.Join(s.cfg.DataDir, "journal", "verdicts.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not json\n"+goodJournalLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if list := journalOf(t, s, ""); len(list) != 0 {
		t.Fatalf("entries %v, want none", list)
	}
	moved, _ := filepath.Glob(path + ".broken-*")
	if len(moved) != 1 {
		t.Fatalf("moved files %v, want one", moved)
	}
	roundTrip(t, s, verdictLine(1, "allow", "Bash"))
	if list := journalOf(t, s, ""); len(list) != 1 {
		t.Fatalf("entries %v, want 1", list)
	}
}

// A sprig pane's verdicts are in its session tree and are reported by no
// gate. The tailer keeps each one it reads, and the journal takes them
// once, even after a restart reads the same tree again.
func TestSprigTreeVerdictsReachTheJournalOnceAcrossARestart(t *testing.T) {
	tl := newTailer(copyFixture(t, "sprig-tree.jsonl"), kindSprig)
	if err := tl.poll(); err != nil {
		t.Fatal(err)
	}
	if len(tl.verdicts) != 1 || tl.verdicts[0].Decision != "deny" || tl.verdicts[0].Tool != "bash" {
		t.Fatalf("tailer verdicts %+v", tl.verdicts)
	}
	s := newFactsServer(t)
	s.noteTreeVerdict("w1:p1", tl.verdicts[0], "enforcing")
	s.noteTreeVerdict("w1:p1", tl.verdicts[0], "enforcing")
	list := journalOf(t, s, "")
	if len(list) != 1 {
		t.Fatalf("entries %v, want 1", list)
	}
	e := list[0].(map[string]any)
	if e["decision"] != "deny" || e["tool"] != "bash" || e["clause"] != "fake clause" || e["at"] != 4.0 || e["mode"] != "enforcing" {
		t.Fatalf("entry %v", e)
	}
	dir := s.cfg.DataDir
	_ = s.Close()
	s2, err := New(Config{SocketPath: filepath.Join(dir, "server.sock"), DataDir: dir,
		ForemanDir: filepath.Join(dir, "state", "foreman"), HookHome: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	s2.noteTreeVerdict("w1:p1", tl.verdicts[0], "enforcing")
	if n := len(journalOf(t, s2, "")); n != 1 {
		t.Fatalf("after a restart %d entries, want 1", n)
	}
	later := tl.verdicts[0]
	later.At = 6
	s2.noteTreeVerdict("w1:p1", later, "enforcing")
	if n := len(journalOf(t, s2, "")); n != 2 {
		t.Fatalf("a newer verdict gave %d entries, want 2", n)
	}
}

// A poll of a sprig pane's tree puts its verdicts in the journal, and a
// second poll with nothing new adds none.
func TestAPollOfASprigTreeJournalsItsVerdicts(t *testing.T) {
	s := newFactsServer(t)
	path := copyFixture(t, "sprig-tree.jsonl")
	pf := s.factsFor("w1:p1", true)
	pf.mu.Lock()
	if err := pf.use(path, kindSprig, s.openTranscript); err != nil {
		pf.mu.Unlock()
		t.Fatal(err)
	}
	tl := pf.files[pf.path]
	pf.mu.Unlock()
	for i := 0; i < 2; i++ {
		done := make(chan struct{})
		s.runPoll("w1:p1", pf, tl, done)
		<-done
	}
	list := journalOf(t, s, "")
	if len(list) != 1 || list[0].(map[string]any)["decision"] != "deny" {
		t.Fatalf("entries %v, want the one deny", list)
	}
}

// A crash in the middle of an append leaves a torn last line. Only that line
// goes: the file is not moved aside, the earlier verdicts stay, and the next
// verdict starts on a line of its own.
func TestFloorJournalDropsOnlyATornLastLine(t *testing.T) {
	s := newFactsServer(t)
	path := filepath.Join(s.cfg.DataDir, "journal", "verdicts.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(goodJournalLine+"\n"+`{"at":2.5,"pane":"w1:p1","deci`), 0o600); err != nil {
		t.Fatal(err)
	}
	list := journalOf(t, s, "")
	if len(list) != 1 || list[0].(map[string]any)["clause"] != "c0" {
		t.Fatalf("entries %v, want the one good line", list)
	}
	if moved, _ := filepath.Glob(path + ".broken-*"); len(moved) != 0 {
		t.Fatalf("a torn last line moved the file aside: %v", moved)
	}
	torn := `{"at":2.5,"pane":"w1:p1","deci` + "\n"
	if got := readJournalFile(t, path+".dropped"); got != torn {
		t.Fatalf("dropped file %q, want %q", got, torn)
	}
	if fi, err := os.Stat(path + ".dropped"); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("dropped file mode: %v %v", fi, err)
	}
	var said bool
	for i := 0; i < 100 && !said; i++ {
		notes, _ := result(t, roundTrip(t, s, `{"id":"n","cmd":"floor.notes"}`)[0])["notes"].([]any)
		for _, n := range notes {
			if text, _ := n.(map[string]any)["text"].(string); strings.Contains(text, path+".dropped") {
				said = true
			}
		}
		if !said {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !said {
		t.Fatalf("no note names the dropped file")
	}
	roundTrip(t, s, verdictLine(1, "deny", "Bash"))
	body := readJournalFile(t, path)
	if strings.Count(body, "\n") != 2 || !strings.HasSuffix(body, "\n") {
		t.Fatalf("file %q, want two whole lines", body)
	}
	if n := len(journalOf(t, s, "")); n != 2 {
		t.Fatalf("%d entries, want 2", n)
	}
}

// A line is a verdict by one rule, the same in the Rust server: one JSON
// object, at a number, pane and decision not-empty strings, tool, clause and
// mode strings when they are there, keys by their exact spelling.
func TestParseJournalLineIsStrict(t *testing.T) {
	good := []string{
		goodJournalLine,
		`{"at":1,"pane":"p","decision":"allow"}`,
		`{"at":1,"pane":"p","decision":"allow","tool":null,"clause":null,"mode":null}`,
		`{"at":1e2,"pane":"p","decision":"ask","extra":[1,2]}`,
	}
	for _, l := range good {
		if _, ok := parseJournalLine([]byte(l)); !ok {
			t.Errorf("rejected %s", l)
		}
	}
	bad := []string{
		``, `null`, `5`, `[]`, `"x"`, `{`,
		`{"pane":"p","decision":"allow"}`,
		`{"at":null,"pane":"p","decision":"allow"}`,
		`{"at":"1","pane":"p","decision":"allow"}`,
		`{"AT":1,"pane":"p","decision":"allow"}`,
		`{"at":1,"pane":5,"decision":"allow"}`,
		`{"at":1,"pane":"","decision":"allow"}`,
		`{"at":1,"pane":"p","decision":""}`,
		`{"at":1,"pane":"p"}`,
		`{"at":1,"pane":"p","decision":"allow","tool":7}`,
		`{"at":1,"pane":"p","decision":"allow","clause":false}`,
		`{"at":1,"pane":"p","decision":"allow","mode":{}}`,
		`{"at":1,"pane":"p","decision":"allow"} x`,
	}
	for _, l := range bad {
		if _, ok := parseJournalLine([]byte(l)); ok {
			t.Errorf("accepted %s", l)
		}
	}
}

// No pane holds more than 100 of the 500 entries. When a pane passes its
// quota its own oldest entry goes, never another pane's, and a restart reads
// the file to the same ring.
func TestFloorJournalKeepsAPaneToItsQuota(t *testing.T) {
	s := newFactsServer(t)
	note := func(pane, clause string) {
		s.noteVerdict(pane, &proto.Verdict{Decision: "allow", Tool: "Bash", Clause: clause}, "")
	}
	for i := 1; i <= 130; i++ {
		note("w1:p1", fmt.Sprintf("a%d", i))
		if i%4 == 0 {
			note("w1:p2", fmt.Sprintf("b%d", i/4))
		}
	}
	check := func(s *Server, when string) {
		t.Helper()
		a := journalOf(t, s, `,"pane":"w1:p1"`)
		b := journalOf(t, s, `,"pane":"w1:p2"`)
		if len(a) != 100 || a[0].(map[string]any)["clause"] != "a130" || a[99].(map[string]any)["clause"] != "a31" {
			t.Fatalf("%s: pane 1 holds %d, first %v last %v", when, len(a), a[0], a[len(a)-1])
		}
		if len(b) != 32 || b[31].(map[string]any)["clause"] != "b1" {
			t.Fatalf("%s: pane 2 holds %d, want all 32", when, len(b))
		}
	}
	check(s, "live")
	dir := s.cfg.DataDir
	_ = s.Close()
	s2, err := New(Config{SocketPath: filepath.Join(dir, "server.sock"), DataDir: dir,
		ForemanDir: filepath.Join(dir, "state", "foreman"), HookHome: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	check(s2, "after a restart")
}

// A tree verdict is the same as one in the journal when its pane, time,
// decision, tool and clause are all the same. Another verdict at the same
// time is a second one. A verdict with no usable time is skipped, not given
// the server's time.
func TestTreeVerdictsAreKeyedOnTheWholeTupleAndNeedATime(t *testing.T) {
	s := newFactsServer(t)
	v := gateFact{Decision: "deny", Tool: "bash", Clause: "no-rm", At: 4}
	s.noteTreeVerdict("w1:p1", v, "")
	s.noteTreeVerdict("w1:p1", v, "enforcing")
	if n := len(journalOf(t, s, "")); n != 1 {
		t.Fatalf("the same verdict twice gave %d entries, want 1", n)
	}
	other := v
	other.Clause = "no-curl"
	s.noteTreeVerdict("w1:p1", other, "")
	other = v
	other.Decision = "ask"
	s.noteTreeVerdict("w1:p1", other, "")
	other = v
	other.Tool = "write"
	s.noteTreeVerdict("w1:p1", other, "")
	s.noteTreeVerdict("w1:p2", v, "")
	if n := len(journalOf(t, s, "")); n != 5 {
		t.Fatalf("five different verdicts at one time gave %d entries, want 5", n)
	}
	for _, at := range []float64{0, -3, 1e12} {
		bad := v
		bad.At = at
		bad.Clause = "no time"
		s.noteTreeVerdict("w1:p1", bad, "")
	}
	if n := len(journalOf(t, s, "")); n != 5 {
		t.Fatalf("a verdict with no usable time was added: %d entries, want 5", n)
	}
}

// A pane reading floor.journal gets only its own pane's entries, whichever
// pane it asks for. So does a plugin, which has no pane of its own and gets
// none. An operator gets all.
func TestFloorJournalGivesAPaneAndAPluginOnlyItsOwnEntries(t *testing.T) {
	s := newFactsServer(t)
	s.SetPlugins(map[string]PluginSpec{"merge": {Needs: []string{"floor.journal"}}})
	for i, pane := range []string{"w1:p1", "w1:p2", "w1:p1"} {
		s.noteVerdict(pane, &proto.Verdict{Decision: "allow", Tool: "Bash", Clause: fmt.Sprintf("c%d", i+1)}, "")
	}
	entries := func(line ...string) []any {
		t.Helper()
		got := roundTrip(t, s, line...)
		m := result(t, got[len(got)-1])
		list, _ := m["entries"].([]any)
		return list
	}
	if n := len(entries(`{"id":"j","cmd":"floor.journal"}`)); n != 3 {
		t.Fatalf("an operator got %d entries, want 3", n)
	}
	hello := `{"id":"h","cmd":"hello","role":"pane","pane":"w1:p2"}`
	list := entries(hello, `{"id":"j","cmd":"floor.journal"}`)
	if len(list) != 1 || list[0].(map[string]any)["clause"] != "c2" {
		t.Fatalf("pane w1:p2 got %v, want only c2", list)
	}
	if n := len(entries(hello, `{"id":"j","cmd":"floor.journal","pane":"w1:p2"}`)); n != 1 {
		t.Fatalf("a pane asking for itself got %d entries, want 1", n)
	}
	if n := len(entries(hello, `{"id":"j","cmd":"floor.journal","pane":"w1:p1"}`)); n != 0 {
		t.Fatalf("a pane asking for another's got %d entries, want none", n)
	}
	got := roundTrip(t, s, `{"id":"h","cmd":"hello","role":"plugin","plugin":"merge"}`, `{"id":"j","cmd":"floor.journal"}`)
	if !got[1].OK {
		t.Fatalf("a plugin was refused: %+v", got[1].Error)
	}
	if list, _ := result(t, got[1])["entries"].([]any); len(list) != 0 {
		t.Fatalf("a plugin got %v, want none", list)
	}
}

// A plugin, or a peer the server cannot place, may say in its hello that it
// is a pane. It still reads nothing, not even the entries of the pane it
// names.
func TestFloorJournalGivesNothingToAPluginOrAnUnknownPeerThatClaimsAPane(t *testing.T) {
	s := newFactsServer(t)
	s.SetPlugins(map[string]PluginSpec{"merge": {Needs: []string{"floor.journal"}}})
	s.noteVerdict("w1:p2", &proto.Verdict{Decision: "deny", Tool: "Bash", Clause: "victim"}, "")
	for name, facts := range map[string]*peerFacts{
		"plugin":  {checked: true, plugin: "merge"},
		"unknown": {checked: true, unknown: true},
	} {
		got := roundTripFacts(t, s, facts,
			`{"id":"h","cmd":"hello","role":"pane","pane":"w1:p2"}`,
			`{"id":"j","cmd":"floor.journal"}`,
			`{"id":"k","cmd":"floor.journal","pane":"w1:p2"}`)
		for _, i := range []int{1, 2} {
			if !got[i].OK {
				t.Fatalf("%s: refused: %+v", name, got[i].Error)
			}
			if list, _ := result(t, got[i])["entries"].([]any); len(list) != 0 {
				t.Fatalf("%s read %v, want none", name, list)
			}
		}
	}
	// A real pane still reads its own.
	got := roundTripFacts(t, s, &peerFacts{checked: true, pane: true, paneID: "w1:p2"},
		`{"id":"j","cmd":"floor.journal"}`)
	if list, _ := result(t, got[0])["entries"].([]any); len(list) != 1 {
		t.Fatalf("a pane got %v, want its one entry", list)
	}
}

// A restart reads the whole tree again. A verdict the pane's quota pruned
// is older than every entry the pane holds, and it stays out; a newer one
// still comes in.
func TestTreeVerdictsThePaneQuotaPrunedStayOutAfterARestart(t *testing.T) {
	s := newFactsServer(t)
	tree := func(i int) gateFact {
		return gateFact{Decision: "deny", Tool: "bash", Clause: fmt.Sprintf("t%d", i), At: float64(1000 + i)}
	}
	for i := 1; i <= 110; i++ {
		s.noteTreeVerdict("w1:p1", tree(i), "")
	}
	if n := len(journalOf(t, s, "")); n != 100 {
		t.Fatalf("%d entries, want the quota of 100", n)
	}
	// The restart reads t1 to t110 again.
	for i := 1; i <= 110; i++ {
		s.noteTreeVerdict("w1:p1", tree(i), "")
	}
	list := journalOf(t, s, "")
	if len(list) != 100 || list[0].(map[string]any)["clause"] != "t110" || list[99].(map[string]any)["clause"] != "t11" {
		t.Fatalf("after the re-read %d entries, first %v last %v", len(list), list[0], list[len(list)-1])
	}
	s.noteTreeVerdict("w1:p1", tree(111), "")
	if list := journalOf(t, s, ""); list[0].(map[string]any)["clause"] != "t111" || len(list) != 100 {
		t.Fatalf("a newer verdict: %d entries, first %v", len(list), list[0])
	}
	// Another pane with room still takes an old verdict.
	s.noteTreeVerdict("w1:p2", tree(1), "")
	if n := len(journalOf(t, s, `,"pane":"w1:p2"`)); n != 1 {
		t.Fatalf("a pane with room has %d entries, want 1", n)
	}
}

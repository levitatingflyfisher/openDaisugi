package foreman

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnActIsKnownAfterAReopen(t *testing.T) {
	dir := t.TempDir()
	clk := newClock()
	c := openT(t, dir, &fakeSummarizer{}, clk, nil)
	ids, err := c.AppendAct(ctx, KindUser, "build the thing", "act-1")
	if err != nil || len(ids) != 1 {
		t.Fatalf("AppendAct = %v, %v", ids, err)
	}
	if i, ok := c.ActMsg("act-1"); !ok || i != ids[0] {
		t.Fatalf("ActMsg = %d, %v", i, ok)
	}
	if _, ok := c.ActMsg("act-2"); ok {
		t.Fatal("an unknown act is known")
	}
	c.Close()
	c2, err := Open(ctx, Config{Dir: dir, Now: clk.now})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if i, ok := c2.ActMsg("act-1"); !ok || i != ids[0] {
		t.Fatalf("after reopen ActMsg = %d, %v", i, ok)
	}
	m, _ := c2.Message(ids[0])
	if m.Act != "act-1" || m.Kind != KindUser {
		t.Fatalf("message = %+v", m)
	}
}

func TestTheActRidesOnEveryPartOfASplitText(t *testing.T) {
	clk := newClock()
	dir := t.TempDir()
	c := openT(t, dir, &fakeSummarizer{}, clk, func(cfg *Config) { cfg.SplitBytes = 100 })
	text := strings.Repeat("w", 250)
	ids, err := c.AppendAct(ctx, KindUser, text, "act-long")
	if err != nil || len(ids) != 3 {
		t.Fatalf("AppendAct = %v, %v", ids, err)
	}
	if i, _ := c.ActMsg("act-long"); i != ids[2] {
		t.Fatalf("ActMsg = %d, want the last part, %d", i, ids[2])
	}
	for _, id := range ids {
		if m, _ := c.Message(id); m.Act != "act-long" {
			t.Fatalf("message %d carries act %q", id, m.Act)
		}
	}
	if !c.ActMatches("act-long", text) || c.ActMatches("act-long", text+"x") || c.ActMatches("act-long", "other") {
		t.Fatal("ActMatches does not tell the same words from other words")
	}
	c.Close()
	c = openT(t, dir, &fakeSummarizer{}, clk, func(cfg *Config) { cfg.SplitBytes = 100 })
	if i, ok := c.ActMsg("act-long"); !ok || i != ids[2] || !c.ActMatches("act-long", text) {
		t.Fatalf("after a reopen: %d, %v", i, ok)
	}
}

func TestAnActIsOnlyForTheWordsItFirstCarried(t *testing.T) {
	c := openT(t, t.TempDir(), &fakeSummarizer{}, newClock(), nil)
	if _, err := c.AppendAct(ctx, KindUser, "first sentence", "a1"); err != nil {
		t.Fatal(err)
	}
	if !c.ActMatches("a1", "first sentence") || c.ActMatches("a1", "second sentence") || c.ActMatches("nope", "first sentence") {
		t.Fatal("ActMatches is wrong")
	}
}

func TestAMessageWithNoActWritesNoActKey(t *testing.T) {
	dir := t.TempDir()
	c := openT(t, dir, &fakeSummarizer{}, newClock(), nil)
	if _, err := c.Append(ctx, KindUser, "plain"); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "main", "*.jsonl"))
	b, _ := os.ReadFile(files[0])
	if strings.Contains(string(b), "act") {
		t.Fatalf("the line has an act key: %s", b)
	}
}

func TestADanglingCallIsAStartWithNoEnd(t *testing.T) {
	dir := t.TempDir()
	mustCall := func(r CallRecord) {
		t.Helper()
		if err := AppendCall(dir, r); err != nil {
			t.Fatal(err)
		}
	}
	mustCall(CallRecord{Call: "c4.1", Phase: CallStart, Turn: 4, Tool: "spawn", Args: `{"project":"a"}`, At: "2026-10-22T10:00:00Z"})
	mustCall(CallRecord{Call: "c4.1", Phase: CallEnd, Outcome: "ok", At: "2026-10-22T10:00:01Z"})
	mustCall(CallRecord{Call: "c5.1", Phase: CallStart, Turn: 5, Tool: "spawn", Args: `{"project":"b"}`, At: "2026-10-22T10:05:00Z"})
	recs, err := ReadCalls(dir)
	if err != nil || len(recs) != 3 {
		t.Fatalf("ReadCalls = %v, %v", recs, err)
	}
	d := DanglingCalls(recs)
	if len(d) != 1 || d[0].Call != "c5.1" || d[0].Turn != 5 {
		t.Fatalf("DanglingCalls = %+v", d)
	}
	if !TurnHasToolStart(recs, 4) || !TurnHasToolStart(recs, 5) || TurnHasToolStart(recs, 6) {
		t.Fatal("TurnHasToolStart is wrong")
	}
	if fi, _ := os.Stat(filepath.Join(dir, CallsFile)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", fi.Mode().Perm())
	}
}

func TestAnUnknownEndSettlesACallOnce(t *testing.T) {
	dir := t.TempDir()
	_ = AppendCall(dir, CallRecord{Call: "c1.1", Phase: CallStart, Turn: 1, Tool: "spawn", At: "2026-10-22T10:00:00Z"})
	recs, _ := ReadCalls(dir)
	if len(DanglingCalls(recs)) != 1 {
		t.Fatal("not dangling")
	}
	_ = AppendCall(dir, CallRecord{Call: "c1.1", Phase: CallEnd, Outcome: CallUnknown, At: "2026-10-22T11:00:00Z"})
	recs, _ = ReadCalls(dir)
	if len(DanglingCalls(recs)) != 0 {
		t.Fatal("still dangling after the unknown end")
	}
}

func TestACallLineCutByACrashIsSkipped(t *testing.T) {
	dir := t.TempDir()
	_ = AppendCall(dir, CallRecord{Call: "c1.1", Phase: CallStart, Turn: 1, Tool: "spawn", At: "x"})
	f, _ := os.OpenFile(filepath.Join(dir, CallsFile), os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"call":"c2.1","phase":"sta`)
	f.Close()
	recs, err := ReadCalls(dir)
	if err != nil || len(recs) != 1 {
		t.Fatalf("ReadCalls = %v, %v", recs, err)
	}
}

func tear(t *testing.T, dir, text string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, CallsFile), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(text)
	f.Close()
}

func TestAStartAfterATornTailIsWhole(t *testing.T) {
	dir := t.TempDir()
	_ = AppendCall(dir, CallRecord{Call: "c1.1", Phase: CallStart, Turn: 1, Tool: "spawn", At: "x"})
	tear(t, dir, `{"call":"c2.1","phase":"sta`)
	if err := AppendCall(dir, CallRecord{Call: "c3.1", Phase: CallStart, Turn: 3, Tool: "spawn", At: "y"}); err != nil {
		t.Fatal(err)
	}
	recs, err := ReadCalls(dir)
	if err != nil || len(recs) != 2 || recs[1].Call != "c3.1" {
		t.Fatalf("ReadCalls = %v, %v", recs, err)
	}
	if got := DanglingCalls(recs); len(got) != 2 {
		t.Fatalf("dangling = %v", got)
	}
}

func TestAFileThatIsAllTornIsCutToNothing(t *testing.T) {
	dir := t.TempDir()
	tear(t, dir, strings.Repeat("x", 9000))
	if err := RepairCalls(dir); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, CallsFile)); fi.Size() != 0 {
		t.Fatalf("size %d", fi.Size())
	}
	if err := RepairCalls(t.TempDir()); err != nil {
		t.Fatalf("a missing file: %v", err)
	}
}

func TestACarriedCallNeverSharesAnIdWithTheNewChat(t *testing.T) {
	old, dir := t.TempDir(), t.TempDir()
	_ = AppendCall(old, CallRecord{Call: "c5.1", Phase: CallStart, Turn: 5, Tool: "spawn", At: "t"})
	if err := CarryDanglingCalls(old, dir); err != nil {
		t.Fatal(err)
	}
	carried := DanglingCalls(mustRead(t, dir))
	if len(carried) != 1 || carried[0].Call == "c5.1" {
		t.Fatalf("carried = %+v", carried)
	}
	_ = AppendCall(dir, CallRecord{Call: carried[0].Call, Phase: CallEnd, Outcome: CallUnknown, At: "t"})
	// The new chat's turn for message 5 makes the same id the old one had.
	_ = AppendCall(dir, CallRecord{Call: "c5.1", Phase: CallStart, Turn: 5, Tool: "spawn", At: "t2"})
	if got := DanglingCalls(mustRead(t, dir)); len(got) != 1 || got[0].Call != "c5.1" {
		t.Fatalf("dangling = %+v", got)
	}
}

func mustRead(t *testing.T, dir string) []CallRecord {
	t.Helper()
	recs, err := ReadCalls(dir)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

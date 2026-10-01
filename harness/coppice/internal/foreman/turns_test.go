package foreman

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTurnRecordsRoundTripAndStatsPrintTheCacheShare(t *testing.T) {
	dir := t.TempDir()
	for _, r := range []TurnRecord{
		{Msg: 0, Started: "2026-10-13T10:00:00Z", Status: TurnRunning},
		{Msg: 0, Started: "2026-10-13T10:00:00Z", Ended: "2026-10-13T10:00:05Z", Status: TurnOK,
			Usage: &CallUsage{Input: 100, Output: 50, CacheRead: 900, CacheWrite: 0}},
		{Msg: 1, Started: "2026-10-13T10:01:00Z", Status: TurnRunning},
		{Msg: 1, Started: "2026-10-13T10:01:00Z", Ended: "2026-10-13T10:01:01Z", Status: TurnFailed},
		{Msg: 2, Started: "2026-10-13T10:02:00Z", Ended: "2026-10-13T10:07:00Z", Status: TurnTimeout},
	} {
		if err := AppendTurn(dir, r); err != nil {
			t.Fatal(err)
		}
	}
	if fi, _ := os.Stat(filepath.Join(dir, TurnsFile)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", fi.Mode().Perm())
	}
	recs, err := ReadTurns(dir)
	if err != nil || len(recs) != 5 {
		t.Fatalf("ReadTurns = %d, %v", len(recs), err)
	}
	if !TurnStarted(recs, 1) || TurnStarted(recs, 7) {
		t.Fatal("TurnStarted is wrong")
	}
	got := Stats(recs)
	want := "turns: 3 (ok 1, failed 1, timeout 1)\n" +
		"cache read share: 90.0% of 1000 input tokens (fresh 100, cache read 900, cache write 0); output 50\n"
	if got != want {
		t.Fatalf("Stats =\n%s\nwant\n%s", got, want)
	}
	if !strings.Contains(Stats(nil), "none yet") {
		t.Fatalf("Stats(nil) = %q", Stats(nil))
	}
}

func TestStatsThroughTheCommand(t *testing.T) {
	dir := t.TempDir()
	if err := AppendTurn(dir, TurnRecord{Msg: 0, Status: TurnOK, Usage: &CallUsage{Input: 1, CacheRead: 3}}); err != nil {
		t.Fatal(err)
	}
	var out, errb strings.Builder
	if code := Main([]string{"stats", "--data-dir", dir}, "", &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "cache read share: 75.0%") {
		t.Fatalf("out = %q", out.String())
	}
}

func TestTheHookRefusesEveryToolButTheForemansFour(t *testing.T) {
	for tool, want := range map[string]int{
		"mcp__foreman__zoom": 0, "mcp__foreman__date": 0, "mcp__foreman__spawn": 0, "mcp__foreman__status": 0,
		"Bash": 2, "Read": 2, "WebFetch": 2, "SomeNewBuiltIn": 2, "mcp__foreman__bash": 2, "mcp__other__zoom": 2, "": 2,
	} {
		var errb strings.Builder
		in := strings.NewReader(`{"tool_name":"` + tool + `","tool_input":{}}`)
		if got := HookMain(in, &errb); got != want {
			t.Errorf("%q: exit %d, want %d", tool, got, want)
		}
	}
	var errb strings.Builder
	if HookMain(strings.NewReader("not json"), &errb) != 2 {
		t.Error("a hook payload that is not JSON was allowed")
	}
}

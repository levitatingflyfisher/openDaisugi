package foreman

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

const poolText = `
idle_after = "90s"
[pool.desk]
model = "m1"
[[pool.desk.entries]]
base_url = "http://desk.tail1.ts.net:11434/v1"
slots = 2
[[pool.desk.entries]]
name = "lap"
base_url = "http://127.0.0.1:8080/v1"
`

func TestParsePoolsFillsTheDefaults(t *testing.T) {
	set, err := ParsePools(poolText, "p.toml")
	if err != nil {
		t.Fatal(err)
	}
	p := set.Pools["desk"]
	if set.IdleAfter != 90*time.Second || p.Model != "m1" || len(p.Entries) != 2 {
		t.Fatalf("set = %+v pool = %+v", set, p)
	}
	if p.Entries[0].Name != "desk.tail1.ts.net" || p.Entries[0].Slots != 2 || p.Entries[1].Slots != 1 {
		t.Fatalf("entries = %+v", p.Entries)
	}
	empty, err := ParsePools("", "p.toml")
	if err != nil || empty.IdleAfter != DefaultIdleAfter || len(empty.Pools) != 0 {
		t.Fatalf("empty = %+v, %v", empty, err)
	}
}

func TestParsePoolsRefusesWhatCouldHideAModelSwitch(t *testing.T) {
	for name, text := range map[string]string{
		"no model":   "[pool.a]\n[[pool.a.entries]]\nbase_url = \"http://h:1/v1\"\n",
		"no entries": "[pool.a]\nmodel = \"m\"\n",
		"the claude": "[pool.default]\nmodel = \"m\"\n[[pool.default.entries]]\nbase_url = \"http://h:1/v1\"\n",
		"bad url":    "[pool.a]\nmodel = \"m\"\n[[pool.a.entries]]\nbase_url = \"ftp://h\"\n",
		"same name":  "[pool.a]\nmodel = \"m\"\n[[pool.a.entries]]\nname = \"x\"\nbase_url = \"http://h:1/v1\"\n[[pool.a.entries]]\nname = \"x\"\nbase_url = \"http://g:1/v1\"\n",
		"bad idle":   "idle_after = \"soon\"\n",
		"not toml":   "[[[",
	} {
		if _, err := ParsePools(text, "p.toml"); err == nil || !strings.Contains(err.Error(), "p.toml") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestPickEntryTakesTheFirstFreeSlotThatIsNotCooling(t *testing.T) {
	set, _ := ParsePools(poolText, "p.toml")
	p := set.Pools["desk"]
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	a, b := p.Entries[0].Name, p.Entries[1].Name
	if got, ok := PickEntry(p, nil, now, false, time.Minute); !ok || got != a {
		t.Fatalf("empty pick = %q %v", got, ok)
	}
	loads := map[string]Load{a: {Running: 2, Slots: 2}}
	if got, _ := PickEntry(p, loads, now, false, time.Minute); got != b {
		t.Fatalf("a is full: %q", got)
	}
	loads = map[string]Load{a: {Until: now.Add(time.Minute)}}
	if got, _ := PickEntry(p, loads, now, false, time.Minute); got != b {
		t.Fatalf("a cools: %q", got)
	}
	loads = map[string]Load{a: {Until: now.Add(time.Minute)}, b: {Running: 1, Slots: 1}}
	if _, ok := PickEntry(p, loads, now, false, time.Minute); ok {
		t.Fatal("every entry was taken")
	}
	// The cool-down ended: the entry is tried again.
	if got, _ := PickEntry(p, map[string]Load{a: {Until: now.Add(-time.Second)}}, now, false, time.Minute); got != a {
		t.Fatalf("a is back: %q", got)
	}
}

func TestPickEntryHoldsBackgroundJobsForAnIdleEntry(t *testing.T) {
	set, _ := ParsePools(poolText, "p.toml")
	p := set.Pools["desk"]
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	a, b := p.Entries[0].Name, p.Entries[1].Name
	loads := map[string]Load{
		a: {LastFg: now.Add(-10 * time.Second)},
		b: {FgRunning: 1, Running: 1, Slots: 4},
	}
	if _, ok := PickEntry(p, loads, now, true, 2*time.Minute); ok {
		t.Fatal("a background job took an entry with recent foreground work")
	}
	if got, _ := PickEntry(p, loads, now, false, 2*time.Minute); got != a {
		t.Fatalf("a foreground job: %q", got)
	}
	loads[a] = Load{LastFg: now.Add(-3 * time.Minute)}
	if got, ok := PickEntry(p, loads, now, true, 2*time.Minute); !ok || got != a {
		t.Fatalf("idle entry: %q %v", got, ok)
	}
}

func TestHealthBacksOffAndStartsOverOnSuccess(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	var h Health
	conn := Verdict{Outcome: Out, Reason: "cannot connect"}
	h.Fail(now, conn)
	first := h.Until.Sub(now)
	h.Fail(now, conn)
	second := h.Until.Sub(now)
	if first != ConnCoolStart || second != 2*ConnCoolStart {
		t.Fatalf("back-off %s then %s", first, second)
	}
	for i := 0; i < 20; i++ {
		h.Fail(now, conn)
	}
	if h.Until.Sub(now) != ConnCoolMax {
		t.Fatalf("the back-off passed its cap: %s", h.Until.Sub(now))
	}
	h.Ok()
	h.Fail(now, conn)
	if h.Until.Sub(now) != ConnCoolStart {
		t.Fatalf("success did not start it over")
	}
	// A quota with a named reset waits for it; one without uses the failover bounds.
	reset := now.Add(7 * time.Minute)
	h.Ok()
	h.Fail(now, Verdict{Outcome: Out, Reason: "rate_limit_error", Reset: reset})
	if !h.Until.Equal(reset) {
		t.Fatalf("until = %s", h.Until)
	}
	h.Ok()
	h.Fail(now, Verdict{Outcome: Out, Reason: "rate_limit_error"})
	if h.Until.Sub(now) != CoolStart {
		t.Fatalf("until = %s", h.Until.Sub(now))
	}
}

func TestTheQueueFileAndTheRequestsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	q, err := ReadQueue(dir)
	if err != nil || len(q.Jobs) != 0 {
		t.Fatalf("empty = %+v, %v", q, err)
	}
	want := QueueState{
		Jobs:    []QueueJob{{Label: "p-1", Project: "p", Pool: "desk", State: "waiting", Added: "x"}, {Label: "p-2", Project: "p", Pool: "desk", Background: true, State: "running", Entry: "a", Added: "y"}},
		Entries: []QueueEntry{{Pool: "desk", Name: "a", Slots: 1, Running: 1, State: "busy"}, {Pool: "desk", Name: "b", Slots: 1, State: "cooling", Until: "2026-10-24T12:00:00Z", Reason: "cannot connect"}},
	}
	if err := WriteQueue(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadQueue(dir)
	if err != nil || len(got.Jobs) != 2 || got.Entries[1].Reason != "cannot connect" {
		t.Fatalf("got = %+v, %v", got, err)
	}
	var out bytes.Buffer
	FormatQueue(&out, got)
	for _, s := range []string{"p-1 | p | desk | foreground | waiting | -", "p-2 | p | desk | background | running | a", "desk/a | 1 of 1 slots | busy", "desk/b | 0 of 1 slots | cooling until 2026-10-24T12:00:00Z (cannot connect)"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("the queue lacks %q:\n%s", s, out.String())
		}
	}
	now := time.Now()
	for i, r := range []QueueRequest{{Project: "p", Brief: "one", Profile: "desk"}, {Project: "p", Brief: "two", Profile: "desk"}} {
		if _, err := WriteQueueRequest(dir, r, now.Add(time.Duration(i))); err != nil {
			t.Fatal(err)
		}
	}
	reqs, bad := TakeQueueRequests(dir)
	if len(reqs) != 2 || bad != 0 || reqs[0].Brief != "one" || reqs[1].Brief != "two" {
		t.Fatalf("reqs = %+v bad = %d", reqs, bad)
	}
	if again, _ := TakeQueueRequests(dir); len(again) != 0 {
		t.Fatalf("a request was taken twice: %+v", again)
	}
}

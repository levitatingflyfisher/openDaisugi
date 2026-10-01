package foreman

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

func TestClassifyCodes(t *testing.T) {
	for _, tc := range []struct {
		text string
		want Outcome
	}{
		{"sprig ended with an error: anthropic api 429: slow down", Out},
		{"sprig ended with an error: anthropic api 529: overloaded", Out},
		{"sprig ended with an error: openai api 503: unavailable", Out},
		{"sprig ended with an error: openai api 400: bad request", NotOut},
		{"sprig ended with an error: anthropic api 401: invalid x-api-key", KeyProblem},
		{"sprig ended with an error: openai api 403: forbidden", KeyProblem},
		{"sprig ended with an error: openai api 500: boom", NotOut},
		{"the model said no", NotOut},
		{"a tool failed: exit 2", NotOut},
		{"claude gave an answer that is not a result", NotOut},
		{"my id is 14290 and 5030 items", NotOut}, // numbers inside numbers are not codes
	} {
		if got := Classify(tc.text, t0).Outcome; got != tc.want {
			t.Errorf("%q: outcome %v, want %v", tc.text, got, tc.want)
		}
	}
}

// Each known text of a quota or usage limit is its own case, so a cut text
// shows up by name.
func TestClassifyEachQuotaText(t *testing.T) {
	for _, text := range OutTexts {
		in := "sprig ended with an error: openai api 400: " + text
		if v := Classify(in, t0); v.Outcome != Out {
			t.Errorf("text %q: outcome %v, want Out", text, v.Outcome)
		}
		// The text is matched in any case.
		if v := Classify(strings.ToUpper(in), t0); v.Outcome != Out {
			t.Errorf("text %q upper: outcome %v, want Out", text, v.Outcome)
		}
	}
	if len(OutTexts) < 8 {
		t.Fatalf("only %d texts", len(OutTexts))
	}
}

func TestClassifyConnectionFaults(t *testing.T) {
	for _, text := range []string{
		"sprig ended with an error: Post \"http://100.64.0.2:8080/v1/chat\": dial tcp 100.64.0.2:8080: connect: connection refused",
		"sprig ended with an error: dial tcp: lookup desk.tail.ts.net: no such host",
		"sprig ended with an error: dial tcp 100.64.0.2:8080: i/o timeout",
		"sprig ended with an error: dial tcp 100.64.0.2:8080: connect: no route to host",
		"sprig ended with an error: dial tcp 100.64.0.2:8080: connect: network is unreachable",
		"openai api: error sending request for url (http://desk/v1): tcp connect error: Connection refused (os error 111)",
	} {
		if v := Classify(text, t0); v.Outcome != Out {
			t.Errorf("%q: outcome %v", text, v.Outcome)
		}
	}
	// A timeout of the whole turn is not a connect timeout.
	if v := Classify("the foreman's turn timed out", t0); v.Outcome != NotOut {
		t.Errorf("a turn timeout moved on")
	}
}

func TestClassifyAKeyProblemIsNotHiddenBehindAQuotaText(t *testing.T) {
	v := Classify("anthropic api 401: invalid key. usage limit reached", t0)
	if v.Outcome != KeyProblem || !strings.Contains(v.Reason, "401") {
		t.Fatalf("verdict %+v", v)
	}
}

func TestClassifyReadsTheResetTime(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       time.Time
	}{
		{"retry-after seconds", "anthropic api 429: x\nretry-after: 90", t0.Add(90 * time.Second)},
		{"retry-after date", "api 429: x\nRetry-After: Wed, 21 Oct 2026 12:30:00 GMT", t0.Add(30 * time.Minute)},
		{"anthropic reset", "api 429: x\nanthropic-ratelimit-requests-reset: 2026-10-21T14:05:00Z", time.Date(2026, 10, 21, 14, 5, 0, 0, time.UTC)},
		{"openai reset duration", "api 429: x\nx-ratelimit-reset-requests: 6m0s", t0.Add(6 * time.Minute)},
		{"openai reset seconds", "api 429: x\nx-ratelimit-reset-tokens: 20", t0.Add(20 * time.Second)},
		{"claude epoch", "Claude AI usage limit reached|1792590000", time.Unix(1792590000, 0).UTC()},
		{"retry-after wins", "api 429\nretry-after: 10\nanthropic-ratelimit-tokens-reset: 2026-10-21T20:00:00Z", t0.Add(10 * time.Second)},
		{"earliest of the resets", "api 429\nanthropic-ratelimit-tokens-reset: 2026-10-21T20:00:00Z\nx-ratelimit-reset-requests: 1m", t0.Add(time.Minute)},
		{"none", "api 429: slow down", time.Time{}},
		{"past reset is ignored", "api 429\nanthropic-ratelimit-tokens-reset: 2026-10-21T11:00:00Z", time.Time{}},
		{"capped at 7 days", "api 429\nretry-after: 99999999", t0.Add(7 * 24 * time.Hour)},
	} {
		v := Classify(tc.text, t0)
		if v.Outcome != Out {
			t.Errorf("%s: outcome %v", tc.name, v.Outcome)
			continue
		}
		if !v.Reset.Equal(tc.want) {
			t.Errorf("%s: reset %v, want %v", tc.name, v.Reset, tc.want)
		}
	}
}

func chain3() *Chain { return NewChain([]string{"", "desk", "grok"}) }

func TestCoolDownDefaultsToFifteenMinutesAndDoubles(t *testing.T) {
	c := chain3()
	want := []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 2 * time.Hour}
	now := t0
	for i, d := range want {
		until := c.MarkOut(0, Verdict{Outcome: Out, Reason: "HTTP 429"}, now)
		if got := until.Sub(now); got != d {
			t.Fatalf("repeat %d: cool-down %v, want %v", i+1, got, d)
		}
		now = until
	}
}

func TestCoolDownEndsAtTheNamedResetAndSuccessResetsTheDoubling(t *testing.T) {
	c := chain3()
	reset := t0.Add(7 * time.Minute)
	if until := c.MarkOut(0, Verdict{Outcome: Out, Reset: reset}, t0); !until.Equal(reset) {
		t.Fatalf("until %v, want %v", until, reset)
	}
	c.MarkOut(0, Verdict{Outcome: Out}, reset) // second repeat: 30 min
	c.MarkOK(0)
	if until := c.MarkOut(0, Verdict{Outcome: Out}, reset); until.Sub(reset) != 15*time.Minute {
		t.Fatalf("after a success the cool-down is %v", until.Sub(reset))
	}
}

func TestChainOrderSkipsCoolingProfilesAndReturnsToTheFirst(t *testing.T) {
	c := chain3()
	if got := c.First(t0); got != 0 {
		t.Fatalf("first = %d", got)
	}
	until := c.MarkOut(0, Verdict{Outcome: Out}, t0)
	if got := c.First(t0.Add(time.Minute)); got != 1 {
		t.Fatalf("first while 0 cools = %d", got)
	}
	c.MarkOut(1, Verdict{Outcome: Out}, t0)
	if got := c.First(t0.Add(time.Minute)); got != 2 {
		t.Fatalf("first while 0 and 1 cool = %d", got)
	}
	// Profile 0's cool-down ends: preference order wins again.
	if got := c.First(until); got != 0 {
		t.Fatalf("first after the cool-down = %d", got)
	}
}

func TestWhenEveryProfileCoolsTheEarliestEndIsKnown(t *testing.T) {
	c := chain3()
	c.MarkOut(0, Verdict{Outcome: Out, Reset: t0.Add(40 * time.Minute)}, t0)
	c.MarkOut(1, Verdict{Outcome: Out, Reset: t0.Add(10 * time.Minute)}, t0)
	c.MarkOut(2, Verdict{Outcome: Out, Reset: t0.Add(20 * time.Minute)}, t0)
	if got := c.First(t0); got != -1 {
		t.Fatalf("first = %d, want -1", got)
	}
	if got := c.Earliest(t0); !got.Equal(t0.Add(10 * time.Minute)) {
		t.Fatalf("earliest = %v", got)
	}
}

func TestStatusStatesAndTheFile(t *testing.T) {
	dir := t.TempDir()
	c := chain3()
	c.MarkOut(0, Verdict{Outcome: Out, Reason: "HTTP 429"}, t0)
	c.MarkFailing(1, "HTTP 401: a key problem")
	c.MarkRan(2)
	if err := c.Save(dir, t0); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dir, FailoverFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file: %v %v", fi, err)
	}
	got, err := LoadChain(dir, []string{"", "desk", "grok"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	rows := got.Status(t0.Add(time.Minute))
	if rows[0].State != "cooling" || rows[0].Until != "2026-10-21T12:15:00Z" || rows[0].Name != "Claude" || rows[0].Reason != "HTTP 429" {
		t.Fatalf("row 0 = %+v", rows[0])
	}
	if rows[1].State != "failing" || rows[1].Reason != "HTTP 401" {
		t.Fatalf("row 1 = %+v", rows[1])
	}
	if rows[2].State != "ready" {
		t.Fatalf("row 2 = %+v", rows[2])
	}
	if got.LastRan() != 2 {
		t.Fatalf("last ran = %d", got.LastRan())
	}
	// After the cool-down ends the profile reads ready.
	if r := got.Status(t0.Add(16 * time.Minute)); r[0].State != "ready" {
		t.Fatalf("row 0 later = %+v", r[0])
	}
	// A chain that changed in the config starts clean.
	other, err := LoadChain(dir, []string{"", "other"}, t0)
	if err != nil || other.Status(t0)[0].State != "ready" || other.LastRan() != -1 {
		t.Fatalf("changed chain: %+v %v", other.Status(t0), err)
	}
}

func TestStatusJSONHasAFixedShape(t *testing.T) {
	c := NewChain([]string{"", "desk"})
	c.MarkOut(0, Verdict{Outcome: Out, Reason: "HTTP 429"}, t0)
	c.MarkRan(1)
	got := string(c.StatusJSON(t0))
	want := `{"chain":[{"profile":"","name":"Claude","state":"cooling","until":"2026-10-21T12:15:00Z","reason":"HTTP 429"},{"profile":"desk","name":"desk","state":"ready"}],"last_ran":"desk"}` + "\n"
	if got != want {
		t.Fatalf("json = %s\nwant   %s", got, want)
	}
}

// writeState writes a failover.json for the chain "", "desk" with the given
// end times, as a hand edit or a wrong clock would leave it.
func writeState(t *testing.T, dir, until0, until1 string) {
	t.Helper()
	doc := `{"chain":[{"profile":"","name":"Claude","state":"cooling","until":"` + until0 + `","reason":"HTTP 429","repeats":9},` +
		`{"profile":"desk","name":"desk","state":"cooling","until":"` + until1 + `","reason":"HTTP 429"}]}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, FailoverFile), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDropsAnEndTimeMoreThanSevenDaysAheadAndSaysSoOnce(t *testing.T) {
	dir := t.TempDir()
	writeState(t, dir, "2031-01-01T00:00:00Z", "2026-10-21T12:20:00Z")
	c, err := LoadChain(dir, []string{"", "desk"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	rows := c.Status(t0)
	if rows[0].State != "ready" || rows[1].State != "cooling" {
		t.Fatalf("rows = %+v", rows)
	}
	note := c.LoadNote()
	if !strings.Contains(note, "Claude") || strings.Contains(note, "desk") || !strings.Contains(note, "7 days") {
		t.Fatalf("note = %q", note)
	}
	// The doubling does not carry on from a time that was dropped.
	if d := c.MarkOut(0, Verdict{Outcome: Out, Reason: "x"}, t0).Sub(t0); d != CoolStart {
		t.Fatalf("cool-down after a dropped time = %v", d)
	}
}

func TestLoadDropsAnEndTimeThatDoesNotParseAndKeepsAPastOneAsReady(t *testing.T) {
	dir := t.TempDir()
	writeState(t, dir, "next tuesday", "2020-01-01T00:00:00Z")
	c, _ := LoadChain(dir, []string{"", "desk"}, t0)
	for _, r := range c.Status(t0) {
		if r.State != "ready" {
			t.Fatalf("rows = %+v", c.Status(t0))
		}
	}
	// One note for the bad value; a past time is simply ready, no note.
	if n := c.LoadNote(); !strings.Contains(n, "Claude") || strings.Contains(n, "desk") {
		t.Fatalf("note = %q", n)
	}
}

func TestLoadKeepsAnEndTimeInsideSevenDays(t *testing.T) {
	dir := t.TempDir()
	writeState(t, dir, t0.Add(7*24*time.Hour).Format(DateLayout), "2026-10-21T12:20:00Z")
	c, _ := LoadChain(dir, []string{"", "desk"}, t0)
	if c.LoadNote() != "" || c.Status(t0)[0].State != "cooling" {
		t.Fatalf("rows = %+v note %q", c.Status(t0), c.LoadNote())
	}
}

func TestSafeReasonKeepsOnlyFixedWordsWhateverTheKeyLooksLike(t *testing.T) {
	hostile := []string{
		"openai api 400: bad key sk-ant-api03-AbCdEf0123456789xyzXYZ",
		"b64 QUJD/REVG+RzEy/MzQ1+Njc4/OWFi+Y2Rl/ZmdoaWprbG1ub3BxcnN0dXZ3eHl6MDEyMzQ1Njc4OQ==",
		`openai api 400: {"key":"sk-ab\/cd\u0045fGh1234567890"}`,
		"openai api 400: bad key sk-abcdefgh\nijklmnop12345678",
		`openai api 400: {"api_key": "shortkey1"} x-api-key: abc123 authorization: Basic Zm9v token=tok123`,
		"Bearer abcDEF123.tok-en_value",
	}
	for _, in := range hostile {
		got := SafeReason(in)
		if got != "HTTP 400" && got != "the turn failed" {
			t.Fatalf("%q -> %q", in, got)
		}
		if strings.ContainsAny(got, "/+\\") || len(got) > 20 {
			t.Fatalf("%q -> %q", in, got)
		}
	}
	for _, in := range []string{"HTTP 429", "cannot connect", "usage limit reached", "HTTP 401: the key was refused. That is a key problem, not a quota"} {
		if SafeReason(in) != in {
			t.Fatalf("%q -> %q", in, SafeReason(in))
		}
	}
}

func TestAFailingReasonIsMaskedAndShortInStateAndStatus(t *testing.T) {
	c := NewChain([]string{"", "desk"})
	c.MarkFailing(0, SafeReason("openai api 400: bad key sk-proj-0123456789abcdefABCDEF "+strings.Repeat("x ", 200)))
	row := c.Status(t0)[0]
	if strings.Contains(row.Reason, "sk-proj") || len(row.Reason) > 130 || row.State != "failing" {
		t.Fatalf("row = %+v", row)
	}
	dir := t.TempDir()
	if err := c.Save(dir, t0); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, FailoverFile))
	if strings.Contains(string(data), "sk-proj") {
		t.Fatalf("file = %s", data)
	}
}

func TestClassifyReadsTheWholeTextNotTheLastLine(t *testing.T) {
	body := "openai api 429: {\n  \"error\": {\n    \"type\": \"insufficient_quota\"\n  }\n}"
	if v := Classify(body, t0); v.Outcome != Out {
		t.Fatalf("verdict = %+v", v)
	}
	// Only the last 4 KiB count: a code far up in a long text is not read.
	long := "anthropic api 429: x\n" + strings.Repeat("filler line\n", 600)
	if v := Classify(long, t0); v.Outcome != NotOut {
		t.Fatalf("long text verdict = %+v", v)
	}
	// A header on its own line reaches the reset reader.
	if v := Classify("anthropic api 429: {\n}\nretry-after: 1200\n}", t0); v.Reset.Sub(t0) != 20*time.Minute {
		t.Fatalf("reset = %v", v.Reset)
	}
}

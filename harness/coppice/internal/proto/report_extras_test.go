package proto

import (
	"strings"
	"testing"
)

func TestReportExtrasAreAbsentWhenNotSent(t *testing.T) {
	x, err := ParseReportExtras([]byte(`{"v":1,"ts":1,"session_id":"s","harness":"claude","state":"idle","source":"gate"}`))
	if err != nil {
		t.Fatal(err)
	}
	if x.TranscriptPath != "" || x.Verdict != nil || x.Mode != "" {
		t.Fatalf("extras %+v, want none", x)
	}
}

func TestReportExtrasParseEveryField(t *testing.T) {
	x, err := ParseReportExtras([]byte(`{"transcript_path":"/home/a/t.jsonl","mode":"watching",` +
		`"verdict":{"decision":"deny","tool":"Bash","clause":"shell: curl is not allowed"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if x.TranscriptPath != "/home/a/t.jsonl" || x.Mode != ModeWatching {
		t.Fatalf("extras %+v", x)
	}
	if x.Verdict == nil || x.Verdict.Decision != "deny" || x.Verdict.Tool != "Bash" ||
		x.Verdict.Clause != "shell: curl is not allowed" {
		t.Fatalf("verdict %+v", x.Verdict)
	}
}

func TestReportExtrasNullMeansNotSent(t *testing.T) {
	x, err := ParseReportExtras([]byte(`{"transcript_path":null,"mode":null,"verdict":null}`))
	if err != nil {
		t.Fatal(err)
	}
	if x.TranscriptPath != "" || x.Verdict != nil || x.Mode != "" {
		t.Fatalf("extras %+v, want none", x)
	}
}

func TestReportExtrasRefuseBadValues(t *testing.T) {
	for _, line := range []string{
		`{"transcript_path":7}`,
		`{"transcript_path":"relative/t.jsonl"}`,
		`{"mode":"shadow"}`,
		`{"mode":"enforce"}`,
		`{"verdict":"allow"}`,
		`{"verdict":{"decision":"maybe","tool":"Bash"}}`,
		`{"verdict":{"tool":"Bash"}}`,
		`{"verdict":{"decision":"allow","tool":5}}`,
		`{"verdict":{"decision":"allow","clause":[]}}`,
	} {
		if _, err := ParseReportExtras([]byte(line)); err == nil {
			t.Errorf("%s: want an error", line)
		}
	}
}

func TestReportExtrasCutALongClauseAndTool(t *testing.T) {
	long := strings.Repeat("x", 500)
	x, err := ParseReportExtras([]byte(`{"verdict":{"decision":"allow","tool":"` + long + `","clause":"` + long + `"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len([]rune(x.Verdict.Clause)) != DetailMax || len([]rune(x.Verdict.Tool)) != DetailMax {
		t.Fatalf("clause %d tool %d runes, want %d", len([]rune(x.Verdict.Clause)),
			len([]rune(x.Verdict.Tool)), DetailMax)
	}
}

// ParseStateEvent keeps ignoring the extras, so the event every client sees
// never carries a transcript path.
func TestAStateEventWithExtrasStillParses(t *testing.T) {
	ev, err := ParseStateEvent([]byte(`{"v":1,"ts":1,"session_id":"s","harness":"claude",` +
		`"state":"working","source":"gate","transcript_path":"/t.jsonl","mode":"enforcing",` +
		`"verdict":{"decision":"allow","tool":"Bash","clause":""}}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.State != StateWorking {
		t.Fatalf("event %+v", ev)
	}
}

package proto

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRequestDecodesCommandAndParams(t *testing.T) {
	d := NewDecoder(strings.NewReader(
		`{"id":"1","cmd":"pane.create","cwd":"/repo","cmd_argv":["claude"],"cols":120}` + "\n"))
	line, err := d.Next()
	if err != nil {
		t.Fatal(err)
	}
	var r Request
	if err := json.Unmarshal(line, &r); err != nil {
		t.Fatal(err)
	}
	if r.ID != "1" || r.Cmd != "pane.create" {
		t.Fatalf("got id=%q cmd=%q, want 1 / pane.create", r.ID, r.Cmd)
	}
	if cwd, ok := r.Str("cwd"); !ok || cwd != "/repo" {
		t.Fatalf("Str(cwd) = %q %v, want /repo true", cwd, ok)
	}
	if argv, ok := r.StrSlice("cmd_argv"); !ok || len(argv) != 1 || argv[0] != "claude" {
		t.Fatalf("StrSlice(cmd_argv) = %v %v, want [claude] true", argv, ok)
	}
	if cols, ok := r.Int("cols"); !ok || cols != 120 {
		t.Fatalf("Int(cols) = %d %v, want 120 true", cols, ok)
	}
	if _, ok := r.Str("nope"); ok {
		t.Fatal("Str(nope) reported ok for an absent key")
	}
}

func TestGoldenOKResponse(t *testing.T) {
	b, err := json.Marshal(OKResp("1", map[string]string{"pane": "w1:p1"}))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"1","ok":true,"result":{"pane":"w1:p1"}}`
	if string(b) != want {
		t.Fatalf("OKResp marshalled to %s, want %s", b, want)
	}
}

func TestGoldenErrorResponse(t *testing.T) {
	b, err := json.Marshal(ErrResp("7", ErrNoSuchPane, "pane w1:p9 does not exist"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"7","ok":false,"error":{"code":"no_such_pane","message":"pane w1:p9 does not exist"}}`
	if string(b) != want {
		t.Fatalf("ErrResp marshalled to %s, want %s", b, want)
	}
}

// The error codes are a closed enum. A code the spec does not list must not
// reach a client, because clients switch on it.
func TestValidCodeRejectsAnUnlistedCode(t *testing.T) {
	for _, c := range []ErrCode{ErrBadRequest, ErrNoSuchPane, ErrNoSuchWorkspace, ErrNoSuchTab,
		ErrPaneClosed, ErrServerClosed, ErrNotAttached, ErrAdapter, ErrSpawnFailed, ErrTimeout,
		ErrUnauthorized, ErrInternal} {
		if !ValidCode(c) {
			t.Fatalf("ValidCode(%q) = false, want true", c)
		}
	}
	if ValidCode("kaboom") {
		t.Fatal(`ValidCode("kaboom") = true, want false`)
	}
}

func TestGoldenFrameEvent(t *testing.T) {
	f := Frame{
		Event: "frame", Pane: "w1:p1", Seq: 1, Cols: 2, Rows: 1, Cursor: [2]int{0, 0},
		RowsChanged: map[int][]Cell{0: {{Text: "h", FG: "#ffffff"}, {Text: "i"}}},
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"event":"frame","pane":"w1:p1","seq":1,"cols":2,"rows":1,"cursor":[0,0],` +
		`"rows_changed":{"0":[["h","#ffffff","",0],["i","","",0]]}}`
	if string(b) != want {
		t.Fatalf("Frame marshalled to %s, want %s", b, want)
	}
}

func TestEncoderWritesOneLinePerValue(t *testing.T) {
	var buf bytes.Buffer
	e := NewEncoder(&buf)
	if err := e.Send(OKResp("1", nil)); err != nil {
		t.Fatal(err)
	}
	if err := e.Send(OKResp("2", nil)); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("Encoder wrote %d lines, want 2: %q", len(lines), buf.String())
	}
}

func TestDecoderRejectsAnOverlongLine(t *testing.T) {
	huge := `{"id":"1","cmd":"x","pad":"` + strings.Repeat("a", 2<<20) + `"}` + "\n"
	d := NewDecoder(strings.NewReader(huge))
	if _, err := d.Next(); err == nil {
		t.Fatal("Decoder accepted a 2 MB line, want an error so one client cannot exhaust memory")
	}
}

func TestParseStateEventRoundTrip(t *testing.T) {
	line := []byte(`{"v":1,"ts":1757300000.123,"session_id":"d41c","harness_session_id":null,` +
		`"harness":"claude-code","pane":"w1:p3","state":"blocked","source":"gate",` +
		`"ask":{"id":"toolu_1","tool":"Bash","summary":"rm -rf build/","deadline":1757300090},` +
		`"detail":"verdict=deny clause=shell.deny[2]"}`)
	ev, err := ParseStateEvent(line)
	if err != nil {
		t.Fatal(err)
	}
	if ev.State != StateBlocked || ev.Source != SrcGate || ev.Ask == nil || ev.Ask.Tool != "Bash" {
		t.Fatalf("ParseStateEvent = %+v, want a gate blocked event with a Bash ask", ev)
	}
	back, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var a, b map[string]any
	if err := json.Unmarshal(line, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(back, &b); err != nil {
		t.Fatal(err)
	}
	for k, av := range a {
		if bv, ok := b[k]; !ok || !jsonEqual(av, bv) {
			t.Fatalf("round trip lost or changed %q: %v -> %v", k, av, bv)
		}
	}
}

func jsonEqual(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

func TestValidateRejectsAnUnknownState(t *testing.T) {
	_, err := ParseStateEvent([]byte(`{"v":1,"ts":1,"session_id":"s","harness":"x",` +
		`"state":"busy","source":"gate"}`))
	if err == nil {
		t.Fatal(`ParseStateEvent accepted state "busy", want an error`)
	}
}

func TestValidateRejectsAnUnknownSource(t *testing.T) {
	_, err := ParseStateEvent([]byte(`{"v":1,"ts":1,"session_id":"s","harness":"x",` +
		`"state":"idle","source":"vibes"}`))
	if err == nil {
		t.Fatal(`ParseStateEvent accepted source "vibes", want an error`)
	}
}

// A headless adapter may block too, and its ask must survive the round trip.
// pi and OpenCode both produce this shape.
func TestAHeadlessBlockedEventKeepsItsAsk(t *testing.T) {
	ev, err := ParseStateEvent([]byte(`{"v":1,"ts":1757300075.0,"session_id":"d41c",` +
		`"harness_session_id":"pi-77","harness":"pi","pane":"w1:p6","state":"blocked",` +
		`"source":"headless","ask":{"id":"ui_1","tool":"Write","summary":"src/main.go",` +
		`"deadline":1757300165.0},"detail":"extension_ui_request"}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Ask == nil || ev.Ask.Tool != "Write" || ev.Ask.Summary != "src/main.go" {
		t.Fatalf("ask = %+v, want the Write ask", ev.Ask)
	}
}

func TestValidateRequiresAnAskOnBlockedFromTheGate(t *testing.T) {
	_, err := ParseStateEvent([]byte(`{"v":1,"ts":1,"session_id":"s","harness":"x",` +
		`"state":"blocked","source":"gate"}`))
	if err == nil {
		t.Fatal("ParseStateEvent accepted a gate blocked event with no ask, want an error")
	}
}

func TestValidateRejectsADetailOverTwoHundredChars(t *testing.T) {
	long := strings.Repeat("x", 201)
	_, err := ParseStateEvent([]byte(`{"v":1,"ts":1,"session_id":"s","harness":"x",` +
		`"state":"idle","source":"gate","detail":"` + long + `"}`))
	if err == nil {
		t.Fatal("ParseStateEvent accepted a 201 char detail, want an error")
	}
}

func TestSourceRankFollowsThePrecedenceOrder(t *testing.T) {
	if !(SourceRank(SrcOperator) > SourceRank(SrcGate) &&
		SourceRank(SrcGate) > SourceRank(SrcHeadless) &&
		SourceRank(SrcHeadless) > SourceRank(SrcProcess) &&
		SourceRank(SrcProcess) > SourceRank(SrcManifest) &&
		SourceRank(SrcManifest) > SourceRank("nonsense")) {
		t.Fatal("SourceRank does not follow operator > gate > headless > process > manifest")
	}
}

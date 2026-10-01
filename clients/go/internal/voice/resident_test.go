package voice

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fake engine is this test binary run again with
// DAISUGI_FAKE_RESIDENT set to a spec: the test passes the variable to
// the child through ResidentConfig.Env, never to this process.
type fakeSpec struct {
	Starts  []map[string]any `json:"starts"`
	Replies []map[string]any `json:"replies"`
	Log     string           `json:"log"`
}

func TestMain(m *testing.M) {
	if raw := os.Getenv("DAISUGI_FAKE_RESIDENT"); raw != "" {
		os.Exit(fakeResident(raw))
	}
	os.Exit(m.Run())
}

func countLog(path, key string) int {
	b, _ := os.ReadFile(path)
	return strings.Count(string(b), `"`+key+`"`)
}

func appendLog(path string, v map[string]any) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(v)
	f.Write(append(b, '\n'))
}

func pick(items []map[string]any, i int, def map[string]any) map[string]any {
	if len(items) == 0 {
		return def
	}
	if i >= len(items) {
		i = len(items) - 1
	}
	return items[i]
}

func fakeResident(raw string) int {
	var spec fakeSpec
	if json.Unmarshal([]byte(raw), &spec) != nil {
		return 9
	}
	start := pick(spec.Starts, countLog(spec.Log, "start"), map[string]any{})
	appendLog(spec.Log, map[string]any{"start": true})
	if d, ok := start["delay"].(float64); ok {
		time.Sleep(time.Duration(d * float64(time.Second)))
	}
	if e, ok := start["error"].(string); ok {
		b, _ := json.Marshal(map[string]string{"error": e})
		fmt.Printf("%s\n", b)
		return 3
	}
	if c, ok := start["exit"].(float64); ok {
		fmt.Fprint(os.Stderr, start["stderr"])
		return int(c)
	}
	fmt.Println(`{"ready": "daisugi-voice-1", "load_ms": 0}`)
	if d, ok := start["stall"].(float64); ok {
		time.Sleep(time.Duration(d * float64(time.Second)))
	}
	for {
		head := make([]byte, 4)
		if _, err := io.ReadFull(os.Stdin, head); err != nil {
			if errors.Is(err, io.EOF) {
				appendLog(spec.Log, map[string]any{"eof": true})
				return 0
			}
			return 2
		}
		body := make([]byte, binary.BigEndian.Uint32(head))
		if _, err := io.ReadFull(os.Stdin, body); err != nil {
			return 2
		}
		k := countLog(spec.Log, "clip")
		appendLog(spec.Log, map[string]any{"clip": len(body)})
		reply := pick(spec.Replies, k, map[string]any{"text": "hello world"})
		if d, ok := reply["delay"].(float64); ok {
			time.Sleep(time.Duration(d * float64(time.Second)))
		}
		if c, ok := reply["crash"].(float64); ok {
			fmt.Fprint(os.Stderr, reply["stderr"])
			return int(c)
		}
		if l, ok := reply["line"].(string); ok {
			fmt.Println(l)
		} else if e, ok := reply["error"].(string); ok {
			b, _ := json.Marshal(map[string]string{"error": e})
			fmt.Printf("%s\n", b)
		} else {
			b, _ := json.Marshal(map[string]any{"text": reply["text"]})
			fmt.Printf("%s\n", b)
		}
	}
}

func fakeEngine(t *testing.T, starts, replies []map[string]any, load, clip float64) (*Resident, string) {
	t.Helper()
	log := t.TempDir() + "/log.jsonl"
	raw, _ := json.Marshal(fakeSpec{Starts: starts, Replies: replies, Log: log})
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r := NewResident(ResidentConfig{Name: "fake", Binary: "fake-cli", Argv: []string{self, "-test.run=^$"},
		Env: []string{"DAISUGI_FAKE_RESIDENT=" + string(raw)}, LoadTimeoutS: load, ClipTimeoutS: clip})
	t.Cleanup(r.Stop)
	return r, log
}

var clip16k = func() []byte {
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+16000))
	b.WriteString("WAVEfmt ")
	binary.Write(&b, binary.LittleEndian, []any{uint32(16), uint16(1), uint16(1), uint32(16000), uint32(32000), uint16(2), uint16(16)})
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(16000))
	b.Write(make([]byte, 16000))
	return b.Bytes()
}()

func engineErr(t *testing.T, err error, kind, prefix string) {
	t.Helper()
	var ee *EngineError
	if !errors.As(err, &ee) || ee.Kind != kind || !strings.Contains(ee.Msg, prefix) {
		t.Fatalf("want %s error with %q, got %v", kind, prefix, err)
	}
}

func TestFrameIsTheBigEndianLengthThenTheClip(t *testing.T) {
	if got := Frame([]byte("abc")); !bytes.Equal(got, []byte("\x00\x00\x00\x03abc")) {
		t.Fatalf("%q", got)
	}
}

func TestReplyLines(t *testing.T) {
	cases := map[string]string{`{"text": "hi", "decode_ms": 3}` + "\n": "text", `{"error": "bad"}`: "error",
		`{"text": 3}`: "", "[1]": "", "\xff": "", `{"text": "a", "error": "b"}`: "text"}
	for line, want := range cases {
		if got := ReplyKind(ParseResidentLine([]byte(line))); got != want {
			t.Errorf("%q: %q, want %q", line, got, want)
		}
	}
	if !IsReadyLine(ParseResidentLine([]byte(`{"ready": "daisugi-voice-1"}`))) ||
		IsReadyLine(ParseResidentLine([]byte(`{"ready": "daisugi-voice-2"}`))) {
		t.Fatal("ready line")
	}
}

func TestExitReason(t *testing.T) {
	if got := ExitReason(4, []string{"a", "boom "}); got != "exited 4: boom" {
		t.Fatal(got)
	}
	if got := ExitReason(-9, nil); got != "killed by signal 9" {
		t.Fatal(got)
	}
}

func TestOneLoadManyClips(t *testing.T) {
	r, log := fakeEngine(t, nil, []map[string]any{{"text": "one"}, {"text": "two"}}, 30, 30)
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"one", "two", "two"} {
		got, err := r.TranscribeText(clip16k)
		if err != nil || got != want {
			t.Fatalf("%q %v, want %q", got, err, want)
		}
	}
	if countLog(log, "start") != 1 || countLog(log, "clip") != 3 {
		t.Fatal("one start, three clips")
	}
}

func TestLoadFailures(t *testing.T) {
	r, _ := fakeEngine(t, []map[string]any{{"error": "the model /m did not load"}}, nil, 30, 30)
	if err := r.Start(); err == nil || err.Msg != "fake-cli did not start: the model /m did not load" {
		t.Fatal(err)
	}
	r, _ = fakeEngine(t, []map[string]any{{"exit": 4.0, "stderr": "one\nboom\n"}}, nil, 30, 30)
	if err := r.Start(); err == nil || err.Msg != "fake-cli did not start: exited 4: boom" {
		t.Fatal(err)
	}
	r, _ = fakeEngine(t, []map[string]any{{"delay": 5.0}}, nil, 0.5, 30)
	if err := r.Start(); err == nil || err.Msg != "fake-cli did not start: it did not load its model in 0.5 seconds" {
		t.Fatal(err)
	}
}

func TestCrashRestartAndLoading(t *testing.T) {
	r, log := fakeEngine(t, []map[string]any{{}, {"delay": 1.5}},
		[]map[string]any{{"crash": 7.0, "stderr": "died\n"}, {"text": "back"}}, 30, 30)
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	_, err := r.TranscribeText(clip16k)
	engineErr(t, err, "unavailable", "stopped during a clip (exited 7: died). It is starting again.")
	_, err = r.TranscribeText(clip16k)
	engineErr(t, err, "loading", "The speech engine is loading its model.")
	if s := r.WaitReady(10); s != "ready" {
		t.Fatal(s)
	}
	if got, err := r.TranscribeText(clip16k); err != nil || got != "back" {
		t.Fatal(got, err)
	}
	if countLog(log, "start") != 2 {
		t.Fatal("two starts")
	}
}

func TestAChildThatStopsReadingCannotHoldAClip(t *testing.T) {
	// 160 KB is bigger than a pipe's buffer, so the write itself blocks.
	big := append(append([]byte(nil), clip16k[:44]...), make([]byte, 160000)...)
	r, _ := fakeEngine(t, []map[string]any{{"stall": 30.0}, {}}, []map[string]any{{"text": "x"}}, 30, 0.5)
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	_, err := r.TranscribeText(big)
	engineErr(t, err, "unavailable", "did not answer in 0.5 seconds")
	if time.Since(t0) > 5*time.Second {
		t.Fatal("the write was not under the clip timeout")
	}
	r.WaitReady(10)
	if got, err := r.TranscribeText(big); err != nil || got != "x" {
		t.Fatal(got, err)
	}
}

func TestMalformedReplyAndErrorReply(t *testing.T) {
	r, _ := fakeEngine(t, nil, []map[string]any{{"error": "bad clip"}, {"line": "not json"}, {"text": "fine"}}, 30, 30)
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.TranscribeText(clip16k); err == nil || err.Error() != "fake-cli: bad clip" {
		t.Fatal(err)
	}
	_, err := r.TranscribeText(clip16k)
	engineErr(t, err, "unavailable", "gave a reply that is not one JSON object with text")
	r.WaitReady(10)
	if got, err := r.TranscribeText(clip16k); err != nil || got != "fine" {
		t.Fatal(got, err)
	}
}

func TestFailedRestartWaitsForTheNextClip(t *testing.T) {
	r, log := fakeEngine(t, []map[string]any{{}, {"error": "no model"}, {}},
		[]map[string]any{{"crash": 1.0}, {"text": "third"}}, 30, 30)
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	r.TranscribeText(clip16k)
	if s := r.WaitReady(10); s != "failed" {
		t.Fatal(s)
	}
	time.Sleep(300 * time.Millisecond)
	if countLog(log, "start") != 2 {
		t.Fatal("no loop")
	}
	_, err := r.TranscribeText(clip16k)
	engineErr(t, err, "unavailable", "did not start again: no model. The next clip tries again.")
	r.WaitReady(10)
	if got, err := r.TranscribeText(clip16k); err != nil || got != "third" {
		t.Fatal(got, err)
	}
}

func TestClipsTogetherAndStop(t *testing.T) {
	r, log := fakeEngine(t, nil, []map[string]any{{"delay": 0.2, "text": "a"}}, 30, 30)
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, err := r.TranscribeText(clip16k); err != nil || got != "a" {
				t.Error(got, err)
			}
		}()
	}
	wg.Wait()
	r.Stop()
	if countLog(log, "eof") != 1 {
		t.Fatal("the child ends through its stdin")
	}
	_, err := r.TranscribeText(clip16k)
	engineErr(t, err, "unavailable", "has stopped")
}

func TestChooseEngine(t *testing.T) {
	ram := func(f float64) *float64 { return &f }
	desk := VoiceHardware{RAMGB: ram(15.5), CPUs: 4}
	all := map[string]bool{"parakeet": true, "moonshine": true, "faster-whisper": true}
	e, m, line := ChooseEngine(desk, all, true)
	if e != "parakeet" || m != "v2" || line != "No voice engine is set, so voice uses Parakeet v2: this box has 15.5 GB of RAM and 4 cores." {
		t.Fatal(e, m, line)
	}
	_, _, line = ChooseEngine(desk, map[string]bool{"moonshine": true}, false)
	if !strings.HasSuffix(line, "Parakeet v2 would come first, but parakeet-cli is not on PATH.") {
		t.Fatal(line)
	}
	e, _, line = ChooseEngine(VoiceHardware{CPUs: 1}, nil, false)
	if e != "moonshine" || !strings.Contains(line, "an unknown amount of RAM and 1 core.") {
		t.Fatal(e, line)
	}
	if got := HardwareOrder(VoiceHardware{RAMGB: ram(7.9), CPUs: 8}); strings.Join(got, ",") != "moonshine,tiny" {
		t.Fatal(got)
	}
}

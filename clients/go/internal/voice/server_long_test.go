package voice

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// toneWav is seconds of a 16 kHz mono tone, with silence of l samples
// starting at each index in quiet.
func toneWav(seconds int, quiet map[int]int) []byte {
	n := seconds * 16000
	data := make([]byte, 2*n)
	for i := 0; i < n; i++ {
		v := int16(8000 * math.Sin(float64(i)/7))
		for at, l := range quiet {
			if i >= at && i < at+l {
				v = 0
			}
		}
		binary.LittleEndian.PutUint16(data[2*i:], uint16(v))
	}
	w, _ := writeWavPCM(data, 16000)
	return w
}

// fakeWhisper writes a whisper-cli stand-in. For each -f file it counts a
// call in a file and writes "window N" to <file>.txt when -otxt is given,
// or prints it. A file named in the fail file's list (one number per line)
// is skipped, as whisper-cli skips a file it cannot read.
func fakeWhisper(t *testing.T, dir string) string {
	t.Helper()
	counter := filepath.Join(dir, "count")
	script := filepath.Join(dir, "whisper-cli")
	body := fmt.Sprintf(`#!/bin/sh
files=""; txt=0; prev=""
for a in "$@"; do
  if [ "$prev" = "-f" ]; then files="$files $a"; fi
  if [ "$a" = "-otxt" ]; then txt=1; fi
  prev="$a"
done
runs=$(cat %[1]s.runs 2>/dev/null || echo 0); echo $((runs+1)) > %[1]s.runs
for f in $files; do
  n=$(cat %[1]s 2>/dev/null || echo 0)
  n=$((n+1))
  echo $n > %[1]s
  if grep -qx $n %[1]s.fail 2>/dev/null; then continue; fi
  if [ $txt = 1 ]; then echo "window $n" > "$f.txt"; else echo "window $n"; fi
done
`, counter)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func TestSplitWavShortClipIsOneWindowUnchanged(t *testing.T) {
	wav := toneWav(30, nil)
	got, err := SplitWav(wav)
	if err != nil || len(got) != 1 || !bytes.Equal(got[0], wav) {
		t.Fatalf("a 30 s clip must stay whole: %d windows, %v", len(got), err)
	}
}

func TestSplitWavCutsInTheQuietestPlaceNearTheBoundary(t *testing.T) {
	// A pause 28 s into the clip, inside the last 3 s of the first window.
	wav := toneWav(65, map[int]int{28 * 16000: 3200})
	got, err := SplitWav(wav)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("65 s wants 3 windows, got %d", len(got))
	}
	total := 0.0
	for i, g := range got {
		d, _ := WavDurationS(g)
		if d > WindowSeconds+1e-9 {
			t.Errorf("window %d is %.2f s", i, d)
		}
		total += d
	}
	if math.Abs(total-65) > 1e-6 {
		t.Errorf("the windows hold %.3f s, want 65", total)
	}
	d0, _ := WavDurationS(got[0])
	if d0 < 28 || d0 > 28.2 {
		t.Errorf("the first cut is at %.3f s, want inside the pause at 28.0 to 28.2", d0)
	}
}

func longServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	os.WriteFile(tokenFile, []byte("tok\n"), 0o600)
	model := filepath.Join(dir, "m.bin")
	os.WriteFile(model, []byte("x"), 0o644)
	eng := &Engine{Name: "whisper.cpp", binary: fakeWhisper(t, dir), model: model}
	srv, err := Listen("127.0.0.1", 0, ServerConfig{DataDir: dir, Home: dir, TokenFile: tokenFile, Engine: eng}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Close() })
	return srv, "http://" + srv.ln.Addr().String(), dir
}

func postClip(t *testing.T, base string, wav []byte) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", base+"/transcribe", bytes.NewReader(wav))
	req.Header.Set("Content-Type", "audio/wav")
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func TestThreeMinuteClipGivesTheTextOfAllWindowsInOrder(t *testing.T) {
	_, base, _ := longServer(t)
	status, body := postClip(t, base, toneWav(180, nil))
	if status != 200 {
		t.Fatalf("status %d: %v", status, body)
	}
	text, _ := body["text"].(string)
	words := strings.Fields(text)
	if len(words) < 12 || len(words)%2 != 0 {
		t.Fatalf("text %q", text)
	}
	for i := 0; i < len(words)/2; i++ {
		if words[2*i] != "window" || words[2*i+1] != fmt.Sprint(i+1) {
			t.Fatalf("windows out of order: %q", text)
		}
	}
	if d := body["duration_s"].(float64); math.Abs(d-180) > 1e-6 {
		t.Errorf("duration_s %v", d)
	}
}

func TestTheOldSixtySecondCapIsGone(t *testing.T) {
	_, base, _ := longServer(t)
	if status, body := postClip(t, base, toneWav(61, nil)); status != 200 {
		t.Fatalf("a 61 s clip got %d: %v", status, body)
	}
}

// A clip a little over the cap is read up to the cap and the reply says how
// much was cut. It is never refused: the page records right up to the cap.
func TestAClipOverTheCapIsReadToTheCapAndTheReplySaysWhatWasCut(t *testing.T) {
	_, base, _ := longServer(t)
	status, body := postClip(t, base, extendWav(t, toneWav(int(MaxAudioSeconds), nil), 8000))
	if status != 200 {
		t.Fatalf("got %d %v", status, body)
	}
	if body["cut_s"] != float64(1) {
		t.Errorf("cut_s %v, want 1 (half a second, rounded up)", body["cut_s"])
	}
	if d := body["duration_s"].(float64); d != MaxAudioSeconds {
		t.Errorf("duration_s %v, want the cap", d)
	}
}

func TestAClipAtTheCapSaysNothingWasCut(t *testing.T) {
	_, base, _ := longServer(t)
	status, body := postClip(t, base, toneWav(int(MaxAudioSeconds), nil))
	if status != 200 {
		t.Fatalf("got %d %v", status, body)
	}
	if _, has := body["cut_s"]; has {
		t.Errorf("cut_s on a clip that fit: %v", body)
	}
}

func TestHealthNamesTheLongestClip(t *testing.T) {
	_, base, _ := longServer(t)
	resp, err := http.Get(base + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var o map[string]any
	json.Unmarshal(raw, &o)
	if o["ok"] != true || o["max_seconds"] != MaxAudioSeconds {
		t.Fatalf("%s", raw)
	}
}

// A second clip while one runs is refused by the server itself, and the
// lock is let go when the first ends.
func TestASecondClipWhileOneRunsIsRefusedWith409(t *testing.T) {
	_, base, dir := longServer(t)
	hold := filepath.Join(dir, "hold")
	os.WriteFile(hold, nil, 0o644)
	script := filepath.Join(dir, "whisper-cli")
	os.WriteFile(script, []byte(fmt.Sprintf(`#!/bin/sh
touch %[1]s/started
while [ -e %[1]s/hold ]; do sleep 0.05; done
for a in "$@"; do if [ "$prev" = "-f" ]; then echo "ok" > "$a.txt"; fi; prev="$a"; done
`, dir)), 0o755)
	done := make(chan int, 1)
	go func() {
		status, _ := postClip(t, base, toneWav(1, nil))
		done <- status
	}()
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(filepath.Join(dir, "started")); err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	status, body := postClip(t, base, toneWav(1, nil))
	if status != 409 || body["error"] != "busy" {
		t.Fatalf("got %d %v", status, body)
	}
	os.Remove(hold)
	if st := <-done; st != 200 {
		t.Fatalf("the first clip got %d", st)
	}
	if status, body := postClip(t, base, toneWav(1, nil)); status != 200 {
		t.Fatalf("the lock was not let go: %d %v", status, body)
	}
}

// extendWav appends extra silent samples to a 16 kHz mono wav.
func extendWav(t *testing.T, wav []byte, extra int) []byte {
	t.Helper()
	w, err := openWave(wav)
	if err != nil {
		t.Fatal(err)
	}
	frames, _ := w.readFrames(w.nframes)
	out, perr := writeWavPCM(append(append([]byte(nil), frames...), make([]byte, 2*extra)...), w.rate)
	if perr != nil {
		t.Fatal(perr)
	}
	return out
}

// The model loads once for all windows: one whisper-cli run for a long clip.
func TestALongClipRunsTheEngineOnce(t *testing.T) {
	_, base, dir := longServer(t)
	if status, body := postClip(t, base, toneWav(180, nil)); status != 200 {
		t.Fatalf("status %d: %v", status, body)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "count.runs"))
	if strings.TrimSpace(string(raw)) != "1" {
		t.Fatalf("whisper-cli ran %q times, want 1", raw)
	}
}

// A window that cannot be read costs only itself: the others keep their
// text and the gap is marked.
func TestAFailedWindowKeepsTheTextOfTheOthersAndMarksTheGap(t *testing.T) {
	_, base, dir := longServer(t)
	os.WriteFile(filepath.Join(dir, "count.fail"), []byte("2\n"), 0o644)
	status, body := postClip(t, base, toneWav(100, nil))
	if status != 200 {
		t.Fatalf("status %d: %v", status, body)
	}
	text, _ := body["text"].(string)
	if text != "window 1 "+GapMarker+" window 3 window 4" {
		t.Fatalf("text %q", text)
	}
	if body["gaps"] != float64(1) {
		t.Fatalf("gaps %v", body["gaps"])
	}
}

// cutLLM answers a cleanup call with the first half of the text it is given,
// as a model with a short context would.
type cutLLM struct{ calls []string }

func (c *cutLLM) Complete(model, system, user string) (string, *big.Int, *big.Int, error) {
	c.calls = append(c.calls, user)
	return user[:len(user)/2], big.NewInt(1), big.NewInt(1), nil
}

type tidyLLM struct{ calls []string }

func (c *tidyLLM) Complete(model, system, user string) (string, *big.Int, *big.Int, error) {
	c.calls = append(c.calls, user)
	return strings.ToUpper(user[:1]) + user[1:] + ".", big.NewInt(1), big.NewInt(1), nil
}

func cleanupCfg(dir string) CleanupConfig {
	m := "test-model"
	return CleanupConfig{On: true, Model: &m, DataDir: dir}
}

func TestACutCleanupKeepsTheRawTextOfThatWindow(t *testing.T) {
	tr := &Transcript{Text: "window one window two", Parts: []string{"window one", "window two"}}
	res, err := CleanParts(tr, cleanupCfg(t.TempDir()), &cutLLM{}, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "window one window two" || res.Cleaned || res.Reason == nil {
		t.Fatalf("a cut reply must fall back to the raw text: %+v", res)
	}
}

func TestCleanupRunsPerWindowAndJoinsInOrder(t *testing.T) {
	tr := &Transcript{Text: "alpha beta " + GapMarker, Parts: []string{"alpha", "beta", GapMarker}}
	llm := &tidyLLM{}
	res, err := CleanParts(tr, cleanupCfg(t.TempDir()), llm, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Alpha. Beta. "+GapMarker || !res.Cleaned {
		t.Fatalf("%+v", res)
	}
	if len(llm.calls) != 2 {
		t.Fatalf("the gap marker must not reach the model: %v", llm.calls)
	}
}

// failLLM never answers, as a model that is down.
type failLLM struct{ calls int }

func (c *failLLM) Complete(model, system, user string) (string, *big.Int, *big.Int, error) {
	c.calls++
	return "", nil, nil, fmt.Errorf("no route")
}

func TestCleanupStopsCallingAModelThatDidNotAnswer(t *testing.T) {
	parts := []string{"w0", "w1", "w2", "w3"}
	tr := &Transcript{Text: strings.Join(parts, " "), Parts: parts}
	llm := &failLLM{}
	res, err := CleanParts(tr, cleanupCfg(t.TempDir()), llm, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if llm.calls != 1 || res.Text != "w0 w1 w2 w3" || res.Cleaned || res.Reason == nil {
		t.Fatalf("calls %d: %+v", llm.calls, res)
	}
}

type slowLLM struct {
	now   *time.Time
	calls int
}

func (c *slowLLM) Complete(model, system, user string) (string, *big.Int, *big.Int, error) {
	c.calls++
	*c.now = c.now.Add(50 * time.Second)
	return strings.ToUpper(user[:1]) + user[1:] + ".", big.NewInt(1), big.NewInt(1), nil
}

func TestCleanupPastItsTimeBudgetKeepsTheRawTextOfTheRest(t *testing.T) {
	clock := time.Unix(1000, 0)
	llm := &slowLLM{now: &clock}
	parts := []string{"w0", "w1", "w2", "w3", "w4", "w5"}
	tr := &Transcript{Text: strings.Join(parts, " "), Parts: parts}
	res, err := CleanPartsBudget(tr, cleanupCfg(t.TempDir()), llm, time.Unix(0, 0),
		func() time.Time { return clock }, 120*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if llm.calls != 3 || res.Text != "W0. W1. W2. w3 w4 w5" || !res.Cleaned ||
		res.Reason == nil || !strings.Contains(*res.Reason, "took too long") {
		t.Fatalf("calls %d: %+v", llm.calls, res)
	}
}

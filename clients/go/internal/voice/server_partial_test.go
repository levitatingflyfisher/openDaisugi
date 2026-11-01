package voice

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func postTo(t *testing.T, url, token string, body []byte) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "audio/wav")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
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

func engineRuns(t *testing.T, dir string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "count"))
	if err != nil {
		return 0
	}
	var n int
	fmt.Sscan(strings.TrimSpace(string(raw)), &n)
	return n
}

// holdingWhisper makes whisper-cli wait while dir/hold exists, then print
// "ok". It touches dir/started when it begins.
func holdingWhisper(t *testing.T, dir string) {
	t.Helper()
	os.WriteFile(filepath.Join(dir, "hold"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, "whisper-cli"), []byte(fmt.Sprintf(`#!/bin/sh
touch %[1]s/started
while [ -e %[1]s/hold ]; do sleep 0.05; done
echo ok
for a in "$@"; do if [ "$prev" = "-f" ]; then echo "ok" > "$a.txt"; fi; prev="$a"; done
`, dir)), 0o755)
}

func waitStarted(t *testing.T, dir string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(filepath.Join(dir, "started")); err == nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the engine never started")
}

// A partial is the text so far, with the sequence number the caller sent.
func TestPartialAnswersTheTextSoFarAndEchoesTheSequence(t *testing.T) {
	_, base, _ := longServer(t)
	status, body := postTo(t, base+"/partial?seq=7", "tok", toneWav(2, nil))
	if status != 200 {
		t.Fatalf("got %d %v", status, body)
	}
	if body["text"] != "window 1" || body["seq"] != float64(7) || body["engine"] != "whisper.cpp" {
		t.Fatalf("%v", body)
	}
	for _, k := range []string{"cleaned", "delivered", "raw_text"} {
		if _, has := body[k]; has {
			t.Errorf("a partial carries %s: %v", k, body)
		}
	}
}

// With no sequence from the caller, the server counts.
func TestPartialWithNoSequenceCountsUp(t *testing.T) {
	_, base, _ := longServer(t)
	_, a := postTo(t, base+"/partial", "tok", toneWav(1, nil))
	_, b := postTo(t, base+"/partial?seq=x", "tok", toneWav(1, nil))
	if a["seq"] != float64(1) || b["seq"] != float64(2) {
		t.Fatalf("%v %v", a, b)
	}
}

func TestPartialNeedsTheTokenAndDecodableAudio(t *testing.T) {
	_, base, _ := longServer(t)
	if status, body := postTo(t, base+"/partial", "", toneWav(1, nil)); status != 401 {
		t.Fatalf("no token: %d %v", status, body)
	}
	if status, body := postTo(t, base+"/partial", "tok", []byte("RIFFnot a wav")); status != 400 || body["error"] != "bad_audio" {
		t.Fatalf("bad audio: %d %v", status, body)
	}
}

// A partial that comes while a clip is being read is dropped at once, never
// queued.
func TestPartialWhileAClipRunsIsDropped(t *testing.T) {
	_, base, dir := longServer(t)
	holdingWhisper(t, dir)
	done := make(chan int, 1)
	go func() {
		status, _ := postClip(t, base, toneWav(1, nil))
		done <- status
	}()
	waitStarted(t, dir)
	start := time.Now()
	status, body := postTo(t, base+"/partial?seq=3", "tok", toneWav(1, nil))
	if status != 409 || body["error"] != "busy" {
		t.Fatalf("got %d %v", status, body)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("the partial waited %v; it must be dropped at once", time.Since(start))
	}
	os.Remove(filepath.Join(dir, "hold"))
	if st := <-done; st != 200 {
		t.Fatalf("the clip got %d", st)
	}
}

// The owner taps Stop while a partial runs: the final clip waits for the
// partial and is read, never refused as busy.
func TestTheFinalClipWaitsForARunningPartial(t *testing.T) {
	_, base, dir := longServer(t)
	holdingWhisper(t, dir)
	done := make(chan int, 1)
	go func() {
		status, _ := postTo(t, base+"/partial?seq=1", "tok", toneWav(1, nil))
		done <- status
	}()
	waitStarted(t, dir)
	go func() {
		time.Sleep(300 * time.Millisecond)
		os.Remove(filepath.Join(dir, "hold"))
	}()
	status, body := postClip(t, base, toneWav(1, nil))
	if status != 200 || body["text"] != "ok" {
		t.Fatalf("the final clip got %d %v", status, body)
	}
	if st := <-done; st != 200 {
		t.Fatalf("the partial got %d", st)
	}
}

// A growing clip reads each finished window once. Only the window still
// growing is read again, so a long dictation costs one window a partial.
func TestPartialReadsOnlyTheWindowStillGrowing(t *testing.T) {
	_, base, dir := longServer(t)
	clip := toneWav(65, nil)
	status, body := postTo(t, base+"/partial", "tok", clip)
	if status != 200 || engineRuns(t, dir) != 3 {
		t.Fatalf("first: %d %v, %d runs", status, body, engineRuns(t, dir))
	}
	status, body = postTo(t, base+"/partial", "tok", extendWav(t, clip, 5*16000))
	if status != 200 {
		t.Fatalf("second: %d %v", status, body)
	}
	if n := engineRuns(t, dir); n != 4 {
		t.Fatalf("the second partial ran the engine on %d windows, want 1", n-3)
	}
	if body["text"] != "window 1 window 2 window 4" {
		t.Fatalf("text %q", body["text"])
	}
	// The final clip reads every window itself.
	postClip(t, base, extendWav(t, clip, 5*16000))
	if n := engineRuns(t, dir); n != 7 {
		t.Fatalf("the final clip read %d windows, want 3", n-4)
	}
}

// /status says partials are on and which engine reads them. Like /health
// it needs no token: a probe must never count toward the ban that would
// turn the final clip's 401, which names the fix, into a 429.
func TestStatusSaysPartialsAreOn(t *testing.T) {
	_, base, _ := longServer(t)
	get := func(token string) (int, map[string]any) {
		req, _ := http.NewRequest("GET", base+"/status", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var o map[string]any
		json.Unmarshal(raw, &o)
		return resp.StatusCode, o
	}
	for i := 0; i < 4; i++ {
		if status, _ := get("wrong"); status != 200 {
			t.Fatalf("a wrong token on /status: %d", status)
		}
	}
	if status, body := postTo(t, base+"/partial", "wrong", toneWav(1, nil)); status != 401 {
		t.Fatalf("after four /status probes a wrong token must still meet 401, not a ban: %d %v", status, body)
	}
	status, o := get("")
	if status != 200 || o["partial"] != true || o["partial_engine"] != "whisper.cpp" ||
		o["engine"] != "whisper.cpp" || o["max_seconds"] != MaxAudioSeconds {
		t.Fatalf("%d %v", status, o)
	}
}

// A full cache drops its oldest windows, never the clip being dictated:
// clearing it mid-partial made the next partial read every window again,
// long enough at 20 minutes to hold the engine past the final clip's wait.
func TestAFullWindowCacheDropsTheOldestNotTheCurrentClip(t *testing.T) {
	old := maxWindowCache
	maxWindowCache = 6
	t.Cleanup(func() { maxWindowCache = old })
	_, base, dir := longServer(t)
	// An earlier dictation fills most of the cache.
	earlier := toneWav(125, map[int]int{10 * 16000: 8000, 70 * 16000: 8000})
	if status, body := postTo(t, base+"/partial", "tok", earlier); status != 200 {
		t.Fatalf("earlier: %d %v", status, body)
	}
	clip := toneWav(95, nil)
	for _, extra := range []int{0, 30, 35} {
		before := engineRuns(t, dir)
		status, body := postTo(t, base+"/partial", "tok", extendWav(t, clip, extra*16000))
		if status != 200 {
			t.Fatalf("+%d s: %d %v", extra, status, body)
		}
		if extra == 35 {
			if n := engineRuns(t, dir) - before; n != 1 {
				t.Fatalf("after the cache filled, a partial read %d windows; want only the last", n)
			}
		}
	}
}

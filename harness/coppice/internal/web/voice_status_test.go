package web

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// voiceReplies answers voice.status and voice.start with status, and every
// other verb as echoOK does.
func voiceReplies(status map[string]any) func(map[string]any) []map[string]any {
	return func(req map[string]any) []map[string]any {
		if req["cmd"] == "voice.status" || req["cmd"] == "voice.start" {
			return []map[string]any{{"id": req["id"], "ok": true, "result": status}}
		}
		return echoOK(req)
	}
}

// postClip sends one clip to /api/voice/transcribe and decodes the reply.
func postClip(t *testing.T, base, tok string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", base+"/api/voice/transcribe", strings.NewReader("clip"))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "audio/webm")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

// The failure this names: with no --voice-url the mic was dead even when
// the coppice server runs a voice server. The web server asks the coppice
// server where voice is, and sends the token that voice server checks.
func TestVoiceTranscribeUsesTheVoiceServerTheCoppiceServerRuns(t *testing.T) {
	upstream, upstreamURL := newVoiceUpstream(t, 200, `{"text":"hello"}`)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("voice-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, d := newFakeServer(t, voiceReplies(map[string]any{
		"state": "ready", "ready": true, "url": upstreamURL, "token_file": tokenFile,
	}))
	ts, _, tok := newTestServer(t, Options{Dial: d})
	code, body := postClip(t, ts.URL, tok)
	if code != 200 || body["text"] != "hello" {
		t.Fatalf("%d %v", code, body)
	}
	if got := upstream.last().Authorization; got != "Bearer voice-token" {
		t.Fatalf("Authorization = %q, want the voice token", got)
	}
}

// The failure this names: a mic that cannot work answered with a sentence
// and no way to act on it. The reply says why, what fixes it, and names
// the action that tries again.
func TestVoiceTranscribeSaysWhyVoiceIsDownAndOffersRetry(t *testing.T) {
	_, d := newFakeServer(t, voiceReplies(map[string]any{
		"state": "down", "ready": false, "url": "",
		"reason": "Voice needs daisugi's voice extra.",
		"fix":    "Install it with pip install 'opendaisugi[voice]'",
	}))
	ts, _, tok := newTestServer(t, Options{Dial: d})
	code, body := postClip(t, ts.URL, tok)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", code)
	}
	want := "Voice needs daisugi's voice extra. Install it with pip install 'opendaisugi[voice]', then press Retry."
	if body["error"] != want || body["action"] != "retry" || body["state"] != "down" {
		t.Fatalf("body = %v", body)
	}
}

func TestVoiceStatusWithNoFixStillEndsWithRetry(t *testing.T) {
	_, d := newFakeServer(t, voiceReplies(map[string]any{
		"state": "idle", "ready": false, "reason": "Voice is not started.", "fix": "",
	}))
	ts, _, tok := newTestServer(t, Options{Dial: d})
	req, _ := http.NewRequest("GET", ts.URL+"/api/voice/status", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != 200 || body["ready"] != false || body["message"] != "Voice is not started. Press Retry." ||
		body["action"] != "retry" {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
}

// The failure this names: Retry must do something. It asks the coppice
// server to start voice, and answers what voice is now.
func TestVoiceRetryAsksTheCoppiceServerToStartVoice(t *testing.T) {
	f, d := newFakeServer(t, voiceReplies(map[string]any{
		"state": "starting", "ready": false, "reason": "Voice is starting. The first start can take a minute.",
		"fix": "Wait a moment",
	}))
	ts, _, tok := newTestServer(t, Options{Dial: d})
	req, _ := http.NewRequest("POST", ts.URL+"/api/voice/retry", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != 200 || body["state"] != "starting" ||
		body["message"] != "Voice is starting. The first start can take a minute. Wait a moment, then press Retry." {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	sawStart := false
	for _, line := range f.Received() {
		if strings.Contains(string(line), `"voice.start"`) {
			sawStart = true
		}
	}
	if !sawStart {
		t.Fatal("Retry never sent voice.start")
	}
}

func TestVoiceStatusWhenReadySaysNothing(t *testing.T) {
	_, d := newFakeServer(t, voiceReplies(map[string]any{"state": "ready", "ready": true, "url": "http://127.0.0.1:1"}))
	ts, _, tok := newTestServer(t, Options{Dial: d})
	req, _ := http.NewRequest("GET", ts.URL+"/api/voice/status", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if body["ready"] != true || body["message"] != "" {
		t.Fatalf("%v", body)
	}
	if _, has := body["url"]; has {
		t.Fatalf("the page does not need the voice server's address: %v", body)
	}
}

// The failure this names: a coppice server built before voice.status
// refuses the verb. The mic says so in words, with Retry, instead of
// failing on an unknown verb.
func TestVoiceTranscribeWithAnOldCoppiceServerSaysSo(t *testing.T) {
	_, d := newFakeServer(t, func(req map[string]any) []map[string]any {
		return []map[string]any{{"id": req["id"], "ok": false,
			"error": map[string]any{"code": "bad_request", "message": "unknown command voice.status"}}}
	})
	ts, _, tok := newTestServer(t, Options{Dial: d})
	code, body := postClip(t, ts.URL, tok)
	msg, _ := body["error"].(string)
	if code != http.StatusServiceUnavailable || !strings.Contains(msg, "too old to start voice") || body["action"] != "retry" {
		t.Fatalf("%d %v", code, body)
	}
}

// The failure this names: --voice-url still wins, and still sends the web
// token, as daisugi voice serve reads that file by default.
func TestVoiceURLFlagOverridesTheCoppiceServer(t *testing.T) {
	upstream, upstreamURL := newVoiceUpstream(t, 200, `{"text":"flag"}`)
	f, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d, VoiceURL: upstreamURL})
	code, body := postClip(t, ts.URL, tok)
	if code != 200 || body["text"] != "flag" || upstream.last().Authorization != "Bearer "+tok {
		t.Fatalf("%d %v", code, body)
	}
	for _, line := range f.Received() {
		if strings.Contains(string(line), "voice.status") {
			t.Fatal("asked the coppice server although --voice-url was given")
		}
	}
}

// The failure this names: --voice-url on another machine was sent the web
// sign-in token. Without a token file of its own, voice is off, and the
// page says why and that the fix is on the box.
func TestAVoiceURLOffThisMachineNeedsItsOwnTokenFile(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d, VoiceURL: "http://10.9.9.9:7477"})
	code, body := postClip(t, ts.URL, tok)
	msg, _ := body["error"].(string)
	if code != http.StatusServiceUnavailable || body["state"] != "off" || body["away"] != true ||
		!strings.Contains(msg, "another machine") || !strings.Contains(msg, "--voice-token-file") {
		t.Fatalf("%d %v", code, body)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/api/voice/status", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st map[string]any
	json.NewDecoder(resp.Body).Decode(&st)
	if st["ready"] != false || st["state"] != "off" {
		t.Fatalf("status %v", st)
	}
}

func TestAVoiceTokenFileGoesWithTheVoiceURL(t *testing.T) {
	upstream, upstreamURL := newVoiceUpstream(t, 200, `{"text":"own token"}`)
	tokenFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenFile, []byte("remote-token\n"), 0o600)
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d, VoiceURL: upstreamURL, VoiceTokenFile: tokenFile})
	code, body := postClip(t, ts.URL, tok)
	if code != 200 || upstream.last().Authorization != "Bearer remote-token" {
		t.Fatalf("%d %v %q", code, body, upstream.last().Authorization)
	}
}

// The failure this names: the mic could not offer the fix for a missing
// voice extra. The reply carries the line to type, and voice off by config
// says the fix is on the box, with no Retry.
func TestVoiceDownCarriesTheCommandAndOffIsAway(t *testing.T) {
	_, d := newFakeServer(t, voiceReplies(map[string]any{
		"state": "down", "ready": false, "reason": "Voice needs daisugi's voice extra.",
		"fix": "Install it with pip install 'opendaisugi[voice]'", "command": "pip install 'opendaisugi[voice]'",
	}))
	ts, _, tok := newTestServer(t, Options{Dial: d})
	code, body := postClip(t, ts.URL, tok)
	if code != 503 || body["command"] != "pip install 'opendaisugi[voice]'" || body["action"] != "retry" {
		t.Fatalf("%d %v", code, body)
	}
	_, d2 := newFakeServer(t, voiceReplies(map[string]any{
		"state": "off", "ready": false, "reason": "Voice is off in coppice.toml.",
		"fix": "Remove [voice] enabled = false there and start the coppice server again",
	}))
	ts2, _, tok2 := newTestServer(t, Options{Dial: d2})
	_, body = postClip(t, ts2.URL, tok2)
	if body["away"] != true || body["action"] != nil ||
		body["error"] != "Voice is off in coppice.toml. Remove [voice] enabled = false there and start the coppice server again." {
		t.Fatalf("%v", body)
	}
}

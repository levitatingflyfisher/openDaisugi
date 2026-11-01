package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func postPartial(t *testing.T, base, tok, query string, body []byte) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", base+"/api/voice/partial"+query, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "audio/webm;codecs=opus")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// The recording so far goes to the voice server's /partial with its
// sequence number, and the text comes back as it was.
func TestVoicePartialForwardsTheClipSoFarWithItsSequence(t *testing.T) {
	upstream, upstreamURL := newVoiceUpstream(t, 200, `{"text":"run the","seq":4,"duration_s":2.0,"engine":"parakeet","ms":80}`)
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d, VoiceURL: upstreamURL})
	status, body := postPartial(t, ts.URL, tok, "?seq=4", []byte("clip-so-far"))
	if status != 200 || body["text"] != "run the" || body["seq"] != float64(4) {
		t.Fatalf("%d %v", status, body)
	}
	got := upstream.last()
	if got.Path != "/partial" || got.Query != "seq=4" || string(got.Body) != "clip-so-far" ||
		got.ContentType != "audio/webm;codecs=opus" || got.Authorization != "Bearer "+tok {
		t.Fatalf("the voice server got %+v", got)
	}
}

// An older voice server has no /partial. Its 404 reaches the page as a
// 404, so the page can say live text needs a newer voice server.
func TestVoicePartialRelaysAnOldServersNotFound(t *testing.T) {
	_, upstreamURL := newVoiceUpstream(t, 404, `{"error":"not_found","message":"This server answers /health, /transcribe, and /deliver."}`)
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d, VoiceURL: upstreamURL})
	status, body := postPartial(t, ts.URL, tok, "?seq=1", []byte("x"))
	if status != 404 || body["code"] != "not_found" {
		t.Fatalf("%d %v", status, body)
	}
}

// A partial is only worth its text while the owner still talks. A voice
// server that does not answer soon gets a short wait, not the clip's
// 90 minutes.
func TestVoicePartialWaitsOnlyAShortTime(t *testing.T) {
	old := voicePartialClient.Timeout
	voicePartialClient.Timeout = 200 * time.Millisecond
	t.Cleanup(func() { voicePartialClient.Timeout = old })
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); up.Close() })
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d, VoiceURL: up.URL})
	start := time.Now()
	status, _ := postPartial(t, ts.URL, tok, "", []byte("x"))
	if status != http.StatusGatewayTimeout || time.Since(start) > 3*time.Second {
		t.Fatalf("got %d after %v", status, time.Since(start))
	}
}

func getVoiceStatus(t *testing.T, base, tok string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest("GET", base+"/api/voice/status", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	return body
}

// The status the page reads says whether live text is on and which engine
// reads it, from the voice server's own /status. The probe sends no token,
// so it never counts toward the voice server's ban.
func TestVoiceStatusSaysWhetherLiveTextIsOn(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status" && r.Header.Get("Authorization") == "" {
			w.Write([]byte(`{"ok":true,"max_seconds":900,"engine":"parakeet","partial":true,"partial_engine":"parakeet"}`))
			return
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(up.Close)
	_, d := newFakeServer(t, voiceReplies(map[string]any{"state": "ready", "ready": true, "url": up.URL}))
	ts, _, tok := newTestServer(t, Options{Dial: d})
	body := getVoiceStatus(t, ts.URL, tok)
	if body["partial"] != true || body["partial_known"] != true || body["partial_engine"] != "parakeet" || body["max_seconds"] != float64(900) {
		t.Fatalf("%v", body)
	}
}

// An older voice server has no /status: live text is off, and the cap
// still comes from /health.
func TestVoiceStatusWithAnOlderVoiceServerSaysLiveTextIsOff(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Write([]byte(`{"ok":true,"max_seconds":600}`))
			return
		}
		w.WriteHeader(404)
		w.Write([]byte(`{"error":"not_found"}`))
	}))
	t.Cleanup(up.Close)
	_, d := newFakeServer(t, voiceReplies(map[string]any{"state": "ready", "ready": true, "url": up.URL}))
	ts, _, tok := newTestServer(t, Options{Dial: d})
	body := getVoiceStatus(t, ts.URL, tok)
	if body["partial"] != false || body["partial_known"] != true || body["max_seconds"] != float64(600) {
		t.Fatalf("%v", body)
	}
	if _, has := body["partial_engine"]; has {
		t.Fatalf("an older server names no partial engine: %v", body)
	}
}

// A voice server that did not answer the probe says nothing about live
// text: the page must not read that as an older server.
func TestVoiceStatusWithNoAnswerDoesNotKnowAboutLiveText(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	t.Cleanup(up.Close)
	_, d := newFakeServer(t, voiceReplies(map[string]any{"state": "ready", "ready": true, "url": up.URL}))
	ts, _, tok := newTestServer(t, Options{Dial: d})
	body := getVoiceStatus(t, ts.URL, tok)
	if _, has := body["partial_known"]; has || body["partial"] != false {
		t.Fatalf("%v", body)
	}
}

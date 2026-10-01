//go:build unix

package server

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSpeech is a speech server on loopback. It never runs a model: it
// answers a few bytes of audio, or the failure the test asks for.
type fakeSpeech struct {
	*httptest.Server
	mu   sync.Mutex
	got  []map[string]any
	auth []string
	path []string
	mode string
}

func newFakeSpeech(t *testing.T, mode string) *fakeSpeech {
	t.Helper()
	f := &fakeSpeech{mode: mode}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.got = append(f.got, body)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.path = append(f.path, r.URL.Path)
		mode := f.mode
		f.mu.Unlock()
		switch mode {
		case "error":
			http.Error(w, "no such voice", http.StatusNotFound)
		case "slow":
			time.Sleep(2 * time.Second)
			_, _ = w.Write([]byte("late"))
		case "huge":
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = w.Write(make([]byte, speechMaxAudio+10))
		case "empty":
			w.Header().Set("Content-Type", "audio/mpeg")
		case "redirect":
			http.Redirect(w, r, "http://127.0.0.1:1/elsewhere", http.StatusFound)
		default:
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = w.Write([]byte("ID3 fake audio"))
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func say(t *testing.T, s *Server, text string) (map[string]any, string) {
	t.Helper()
	line, _ := json.Marshal(map[string]any{"id": "1", "cmd": "speech.say", "text": text})
	r := roundTrip(t, s, string(line))[0]
	if r.OK {
		return result(t, r), ""
	}
	return nil, r.Error.Message
}

func TestSpeechStatusBrowserWhenNoURL(t *testing.T) {
	s := newTestServer(t)
	m := result(t, roundTrip(t, s, `{"id":"1","cmd":"speech.status"}`)[0])
	if m["mode"] != "browser" || len(m) != 1 {
		t.Fatalf("status = %v", m)
	}
}

func TestSpeechStatusServer(t *testing.T) {
	s := newTestServer(t)
	s.cfg.Voice.SpeakURL = "http://gpu:8880/v1/"
	s.cfg.Voice.SpeakProfile = "tts"
	m := result(t, roundTrip(t, s, `{"id":"1","cmd":"speech.status"}`)[0])
	if m["mode"] != "server" || m["url"] != "http://gpu:8880/v1/audio/speech" || m["model"] != "kokoro" ||
		m["voice"] != "af_heart" || m["profile"] != "tts" {
		t.Fatalf("status = %v", m)
	}
}

func TestSpeechStatusBadURLIsBrowserWithALine(t *testing.T) {
	for _, bad := range []string{"gpu:8880", "http://u:pw@gpu:8880", "http://gpu:8880/?key=SECRET", "ftp://gpu"} {
		s := newTestServer(t)
		s.cfg.Voice.SpeakURL = bad
		m := result(t, roundTrip(t, s, `{"id":"1","cmd":"speech.status"}`)[0])
		b, _ := json.Marshal(m)
		if m["mode"] != "browser" || !strings.Contains(m["line"].(string), "speak_url") || strings.Contains(string(b), "SECRET") || strings.Contains(string(b), "pw") {
			t.Fatalf("%s: status = %v", bad, m)
		}
	}
}

func TestSpeechSayAudioAndRequestShape(t *testing.T) {
	f := newFakeSpeech(t, "ok")
	s := newTestServer(t)
	s.cfg.Voice.SpeakURL = f.URL
	s.cfg.Voice.SpeakModel, s.cfg.Voice.SpeakVoice = "m1", "v1"
	m, why := say(t, s, "  Hello there.  ")
	if why != "" {
		t.Fatal(why)
	}
	raw, _ := base64.StdEncoding.DecodeString(m["audio"].(string))
	if string(raw) != "ID3 fake audio" || m["type"] != "audio/mpeg" || m["bytes"].(float64) != 14 {
		t.Fatalf("reply = %v", m)
	}
	if f.path[0] != "/v1/audio/speech" || f.got[0]["input"] != "Hello there." || f.got[0]["model"] != "m1" ||
		f.got[0]["voice"] != "v1" || f.got[0]["response_format"] != "mp3" || f.auth[0] != "" {
		t.Fatalf("request = %v %v %q", f.path, f.got, f.auth)
	}
}

func TestSpeechSayRefusals(t *testing.T) {
	s := newTestServer(t)
	if _, why := say(t, s, "hi"); !strings.Contains(why, "speak_url") {
		t.Fatalf("no server: %q", why)
	}
	f := newFakeSpeech(t, "ok")
	s.cfg.Voice.SpeakURL = f.URL
	if _, why := say(t, s, "  "); !strings.Contains(why, "needs the text") {
		t.Fatalf("empty: %q", why)
	}
	if _, why := say(t, s, strings.Repeat("a", speechMaxText+1)); !strings.Contains(why, "one sentence") {
		t.Fatalf("long: %q", why)
	}
}

func TestSpeechSayFailuresSayWhatToDo(t *testing.T) {
	old := speechLimit
	speechLimit = 300 * time.Millisecond
	t.Cleanup(func() { speechLimit = old })
	for mode, want := range map[string]string{
		"error": "answered 404", "slow": "took longer than 300 ms", "huge": "more than 2 MiB",
		"empty": "sent no audio", "redirect": "answered 302",
	} {
		f := newFakeSpeech(t, mode)
		s := newTestServer(t)
		s.cfg.Voice.SpeakURL = f.URL
		_, why := say(t, s, "Hello.")
		if !strings.Contains(why, want) || !strings.Contains(why, "speech server at 127.0.0.1:") {
			t.Fatalf("%s: %q", mode, why)
		}
	}
	// Nothing listens.
	s := newTestServer(t)
	s.cfg.Voice.SpeakURL = "http://127.0.0.1:" + closedPort(t)
	if _, why := say(t, s, "Hello."); !strings.Contains(why, "did not answer") {
		t.Fatalf("closed: %q", why)
	}
}

// The speech request uses no proxy from the environment, as the Rust server
// uses none, and follows no redirect.
func TestSpeechClientUsesNoProxy(t *testing.T) {
	tr, ok := speechClient().Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Fatalf("transport = %#v", speechClient().Transport)
	}
}

// A key goes only over https, or over http to this box or the tailnet.
func TestSpeechKeyOnlyGoesWhereItIsSafe(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://api.example.com": true, "http://127.0.0.1:8880": true, "http://localhost:8880": true,
		"http://[::1]:8880": true, "http://100.64.0.1:8880": true, "http://100.127.255.254": true,
		"http://[fd7a:115c:a1e0::1]:8880": true, "http://gpu.tail1.ts.net:8880": true,
		"http://100.128.0.1": false, "http://100.63.255.255": false, "http://10.0.0.5:8880": false,
		"http://gpu:8880": false, "http://example.com": false, "http://[fd7a:115c:a1e1::1]": false,
		"http://notts.net.example.com": false,
	} {
		_, u, ok := speechEndpoint(raw)
		if !ok {
			t.Fatalf("%s is not an address", raw)
		}
		if got := keyMayGo(u); got != want {
			t.Errorf("keyMayGo(%s) = %v, want %v", raw, got, want)
		}
	}
}

// A host is letters, digits, dots and hyphens, or an IP with no zone.
func TestSpeechEndpointReadsOnlyPlainHosts(t *testing.T) {
	for raw, want := range map[string]bool{
		"http://gpu-box.local:1": true, "http://127.0.0.1": true, "http://[::1]:1": true, "HTTP://GPU:1": true,
		"http://gpu_box:1": false, "http://[::1%25lo]:1": false, "http://gpu!:1": false, "http://[gpu]:1": false, "http://[fe80::1%25eth0]": false,
	} {
		if _, _, ok := speechEndpoint(raw); ok != want {
			t.Errorf("speechEndpoint(%s) ok = %v, want %v", raw, ok, want)
		}
	}
	// And a key never goes to a zone address, whatever reads it.
	u, err := url.Parse("http://[::1%25lo]:1")
	if err != nil || keyMayGo(u) {
		t.Errorf("a zone address may take a key: %v %v", u, err)
	}
}

// A key is never sent over plain http to a host that is not this box or on
// the tailnet: the request is refused before anything leaves, with the fix.
func TestSpeechRefusesAKeyOverPlainHTTP(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "daisugi")
	script := "#!/bin/sh\nprintf '%s' '{\"tts\":{\"wire\":\"openai\",\"base_url\":null,\"model\":null,\"key\":\"sk-test-KEY\"}}' >&3\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPPICE_DAISUGI_BIN", bin)
	s := newTestServer(t)
	s.cfg.Voice.SpeakURL = "http://speech.example.invalid:9"
	s.cfg.Voice.SpeakProfile = "tts"
	_, why := say(t, s, "Hello.")
	if !strings.Contains(why, "plain http") || !strings.Contains(why, "https") || strings.Contains(why, "KEY") {
		t.Fatalf("why = %q", why)
	}
}

// A pane and a plugin may not read aloud.
func TestSpeechVerbsRefuseAPaneAndAPlugin(t *testing.T) {
	s := newTestServer(t)
	for _, f := range []*peerFacts{{checked: true, pane: true, paneID: "w1:p1"}, {checked: true, plugin: "p"}} {
		got := roundTripFacts(t, s, f, `{"id":"1","cmd":"speech.status"}`, `{"id":"2","cmd":"speech.say","text":"hi"}`)
		for _, r := range got {
			if r.OK || r.Error.Message != SpeechRefusal {
				t.Fatalf("%+v = %+v", f, r)
			}
		}
	}
}

// A named web token (a kid's) may not read aloud, as it may not read the
// chat. The owner's own token may.
func TestSpeechVerbsRefuseANamedToken(t *testing.T) {
	s := newTestServer(t)
	got := roundTrip(t, s, `{"id":"h","cmd":"hello","name_from":"token","name":"kid"}`,
		`{"id":"1","cmd":"speech.status"}`, `{"id":"2","cmd":"speech.say","text":"hi"}`)
	for _, r := range got[1:] {
		if r.OK || r.Error.Message != SpeechRefusal {
			t.Fatalf("a named token = %+v", r)
		}
	}
	got = roundTrip(t, s, `{"id":"h","cmd":"hello","name_from":"token"}`, `{"id":"1","cmd":"speech.status"}`)
	if !got[1].OK {
		t.Fatalf("the owner's token = %+v", got[1].Error)
	}
}

// The key comes from the named profile's bundle entry, goes only as the
// bearer, and never shows in the reply or in an error.
func TestSpeechSayKeyFromProfileBundle(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "daisugi")
	// agent-profiles bundle --fd 3 writes the table to descriptor 3.
	script := "#!/bin/sh\nprintf '%s' '{\"tts\":{\"wire\":\"openai\",\"base_url\":null,\"model\":null,\"key\":\"sk-test-KEY\"}}' >&3\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPPICE_DAISUGI_BIN", bin)
	f := newFakeSpeech(t, "ok")
	s := newTestServer(t)
	s.cfg.Voice.SpeakURL = f.URL + "/v1/audio/speech"
	s.cfg.Voice.SpeakProfile = "tts"
	m, why := say(t, s, "Hello.")
	if why != "" {
		t.Fatal(why)
	}
	b, _ := json.Marshal(m)
	if f.auth[0] != "Bearer sk-test-KEY" || strings.Contains(string(b), "KEY") || f.path[0] != "/v1/audio/speech" {
		t.Fatalf("auth %q path %q reply %s", f.auth[0], f.path[0], b)
	}
	s.cfg.Voice.SpeakProfile = "missing"
	if _, why := say(t, s, "Hello."); !strings.Contains(why, `"missing" is not in the operator's agent profiles`) || strings.Contains(why, "KEY") {
		t.Fatalf("missing: %q", why)
	}
}

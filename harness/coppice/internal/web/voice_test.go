package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// capturedVoiceRequest is one request the fake voice server saw.
type capturedVoiceRequest struct {
	Method, Path, Query, ContentType, Authorization string
	Body                                            []byte
}

// fakeVoiceServer is a stand-in for daisugi voice serve. It records every
// request it answers and replies with whatever reply says next, so a test
// can drive a sequence of forwards without restarting the server.
type fakeVoiceServer struct {
	mu       sync.Mutex
	received []capturedVoiceRequest
	status   int
	body     string
}

func newVoiceUpstream(t *testing.T, status int, body string) (*fakeVoiceServer, string) {
	t.Helper()
	f := &fakeVoiceServer{status: status, body: body}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.received = append(f.received, capturedVoiceRequest{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
			ContentType: r.Header.Get("Content-Type"), Authorization: r.Header.Get("Authorization"),
			Body: raw,
		})
		status, body := f.status, f.body
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return f, ts.URL
}

func (f *fakeVoiceServer) last() capturedVoiceRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.received[len(f.received)-1]
}

func (f *fakeVoiceServer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.received)
}

// The failure this names: the clip, its Content-Type, and the query string
// the phone sent must all reach the voice server unchanged, and the bearer
// forwarded must be the same token this server itself trusts, not something
// the phone supplied.
func TestVoiceTranscribeForwardsTheClipAndQueryAndBearer(t *testing.T) {
	upstream, upstreamURL := newVoiceUpstream(t, 200, `{"text":"hello","raw_text":"hello","cleaned":false}`)
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d, VoiceURL: upstreamURL})

	req, _ := http.NewRequest("POST", ts.URL+"/api/voice/transcribe?cleanup=1", bytes.NewReader([]byte("clip-bytes")))
	// bearerFrom trims the header before comparing it against the stored
	// token, so a doubled space still passes the guard. A proxy that just
	// copied this header onward would forward it doubled too; one that
	// reads its own token store instead always sends exactly one space.
	req.Header.Set("Authorization", "Bearer  "+tok)
	req.Header.Set("Content-Type", "audio/webm;codecs=opus")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["text"] != "hello" {
		t.Fatalf("body was %v", body)
	}

	if upstream.count() != 1 {
		t.Fatalf("the voice server saw %d requests, want 1", upstream.count())
	}
	got := upstream.last()
	if got.Method != "POST" || got.Path != "/transcribe" {
		t.Fatalf("the voice server was asked %s %s", got.Method, got.Path)
	}
	if got.Query != "cleanup=1" {
		t.Fatalf("the query string was %q, want cleanup=1", got.Query)
	}
	if got.ContentType != "audio/webm;codecs=opus" {
		t.Fatalf("Content-Type was %q", got.ContentType)
	}
	if string(got.Body) != "clip-bytes" {
		t.Fatalf("the voice server received %q", got.Body)
	}
	// The bearer the proxy sends is read from its own token store, the same
	// token this test authenticated the phone request with, never a bearer
	// the phone request happened to carry itself.
	if got.Authorization != "Bearer "+tok {
		t.Fatalf("Authorization was %q, want Bearer %s", got.Authorization, tok)
	}
}

// The failure this names: the voice server's own error shape puts a short
// code under "error" and the sentence a person should read under
// "message". Relaying that shape unchanged would show the operator a bare
// code like bad_audio, which names nothing to do next.
func TestVoiceTranscribeRewritesTheVoiceServersMessageIntoError(t *testing.T) {
	_, upstreamURL := newVoiceUpstream(t, 400,
		`{"error":"bad_audio","message":"This audio could not be decoded. Send WAV, WebM, Ogg, MP3, or M4A."}`)
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d, VoiceURL: upstreamURL})

	req, _ := http.NewRequest("POST", ts.URL+"/api/voice/transcribe", strings.NewReader("clip"))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if body["error"] != "This audio could not be decoded. Send WAV, WebM, Ogg, MP3, or M4A." {
		t.Fatalf("error was not rewritten to the sentence: %v", body)
	}
	if body["code"] != "bad_audio" {
		t.Fatalf("the original code was not kept under code: %v", body)
	}
}

// The failure this names: an unconditional remarshal would round-trip
// duration_s and rtf through float64 and could reorder keys, changing bytes
// a person never asked this proxy to touch, for a reply that carried
// nothing to rewrite in the first place.
func TestVoiceTranscribeRelaysASuccessfulReplyByteForByte(t *testing.T) {
	const upstreamBody = `{"text":"hello","raw_text":"hello","duration_s":1.5,"rtf":0.32,"engine":"faster-whisper","cleaned":false}`
	_, upstreamURL := newVoiceUpstream(t, 200, upstreamBody)
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d, VoiceURL: upstreamURL})

	req, _ := http.NewRequest("POST", ts.URL+"/api/voice/transcribe", strings.NewReader("clip"))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != upstreamBody {
		t.Fatalf("a reply with nothing to rewrite was not relayed byte for byte:\ngot  %s\nwant %s", raw, upstreamBody)
	}
}

// The failure this names: something between the phone and the voice server,
// or the voice server itself under a fault it cannot turn into JSON, sends
// back an HTML error page or plain text. Forcing application/json onto that
// body would have app.js's resp.json().catch swallow it and show the phone
// a bare "HTTP 502" instead of whatever page a person could actually read.
func TestVoiceTranscribeRelaysTheUpstreamsContentTypeWhenTheBodyIsNotJSON(t *testing.T) {
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("upstream proxy exploded"))
	}))
	t.Cleanup(ts2.Close)

	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d, VoiceURL: ts2.URL})

	req, _ := http.NewRequest("POST", ts.URL+"/api/voice/transcribe", strings.NewReader("clip"))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain" {
		t.Fatalf("Content-Type was %q, want text/plain", ct)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "upstream proxy exploded" {
		t.Fatalf("body was %q", raw)
	}
}

// The failure this names: no daisugi voice serve running, or the wrong
// --voice-url, must read as a clear next step, not a bare router error or
// a hang until the client times out.
func TestVoiceTranscribeAnswers502OnATransportFailure(t *testing.T) {
	// A real listener, closed before the request, so the dial itself fails
	// fast instead of this test waiting out a real network timeout.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadURL := "http://" + ln.Addr().String()
	ln.Close()

	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d, VoiceURL: deadURL})

	req, _ := http.NewRequest("POST", ts.URL+"/api/voice/transcribe", strings.NewReader("clip"))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "daisugi voice serve") {
		t.Fatalf("the 502 does not name daisugi voice serve: %q", msg)
	}
	if !strings.Contains(msg, deadURL) {
		t.Fatalf("the 502 does not name the voice server's address: %q", msg)
	}
}

// The failure this names: a malformed voice_url written by hand into
// web.json, or by an older build with no flag validation, must still name
// itself when the request to it cannot even be built, not answer a bare
// sentence with no address in it.
func TestVoiceTranscribeNamesTheAddressWhenTheRequestCannotBeBuilt(t *testing.T) {
	const badURL = "http://%zz"
	_, d := newFakeServer(t, echoOK)
	// A token file of its own, since an address that does not parse is
	// never taken for this machine.
	tokenFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenFile, []byte("t"), 0o600)
	ts, _, tok := newTestServer(t, Options{Dial: d, VoiceURL: badURL, VoiceTokenFile: tokenFile})

	req, _ := http.NewRequest("POST", ts.URL+"/api/voice/transcribe", strings.NewReader("clip"))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", resp.StatusCode)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, badURL) {
		t.Fatalf("the 500 does not name the voice server's address: %q", msg)
	}
}

// The failure this names: a runaway upload must never reach the voice
// server at all, and the phone must hear a sentence naming the limit, not
// a truncated clip silently sent on and rejected downstream as bad_audio.
func TestVoiceTranscribeRefusesAClipOverTheSizeCap(t *testing.T) {
	upstream, upstreamURL := newVoiceUpstream(t, 200, `{"text":"unreachable"}`)
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d, VoiceURL: upstreamURL})

	oversize := bytes.Repeat([]byte("a"), MaxVoiceClipBytes+1)
	req, _ := http.NewRequest("POST", ts.URL+"/api/voice/transcribe", bytes.NewReader(oversize))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.ContentLength = int64(len(oversize))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", resp.StatusCode)
	}
	if upstream.count() != 0 {
		t.Fatalf("an oversize clip still reached the voice server")
	}
}

// A two-minute clip is a few hundred KB as webm and about 4 MB as wav; a
// 20 minute dictation as wav is about 38 MB. The proxy must pass all of it on
// to the voice server, which owns the real limit and says it.
func TestVoiceTranscribeForwardsALongClipWhole(t *testing.T) {
	upstream, upstreamURL := newVoiceUpstream(t, 200, `{"text":"long"}`)
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d, VoiceURL: upstreamURL})

	clip := bytes.Repeat([]byte("a"), 38<<20)
	req, _ := http.NewRequest("POST", ts.URL+"/api/voice/transcribe", bytes.NewReader(clip))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.ContentLength = int64(len(clip))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if got := len(upstream.last().Body); got != len(clip) {
		t.Fatalf("the voice server got %d bytes, want %d", got, len(clip))
	}
}

// A wait that runs out is not a dead server: the answer says it timed out,
// so the owner does not read "voice is down" and start the job again.
func TestVoiceTranscribeSaysWhenTheWaitRanOut(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
	}))
	t.Cleanup(slow.Close)
	old := voiceClient.Timeout
	voiceClient.Timeout = 100 * time.Millisecond
	t.Cleanup(func() { voiceClient.Timeout = old })

	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d, VoiceURL: slow.URL})
	req, _ := http.NewRequest("POST", ts.URL+"/api/voice/transcribe", strings.NewReader("clip"))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	msg, _ := body["error"].(string)
	if resp.StatusCode != http.StatusGatewayTimeout || !strings.Contains(msg, "took longer than") ||
		!strings.Contains(msg, "may still be working") {
		t.Fatalf("got %d %q", resp.StatusCode, msg)
	}
}

// Two clips never run at once, and the voice server is the one that says so:
// it answers 409 while it works, and the proxy passes that answer on.
func TestVoiceTranscribeRelaysTheVoiceServersBusyAnswer(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	var busy atomic.Bool
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !busy.CompareAndSwap(false, true) {
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"error":"busy","message":"The voice server is still turning an earlier clip into text."}`))
			return
		}
		defer busy.Store(false)
		entered <- struct{}{}
		<-release
		w.Write([]byte(`{"text":"done"}`))
	}))
	t.Cleanup(slow.Close)
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d, VoiceURL: slow.URL})
	post := func() (int, string) {
		req, _ := http.NewRequest("POST", ts.URL+"/api/voice/transcribe", strings.NewReader("clip"))
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return 0, ""
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	first := make(chan int, 1)
	go func() { c, _ := post(); first <- c }()
	<-entered
	code, body := post()
	if code != http.StatusConflict || !strings.Contains(body, "still turning an earlier clip") {
		t.Fatalf("second clip got %d %s", code, body)
	}
	close(release)
	if c := <-first; c != 200 {
		t.Fatalf("first clip got %d", c)
	}
	if c, _ := post(); c != 200 {
		t.Fatalf("a clip after the first finished got %d", c)
	}
}

// A page that drops its request mid-send does not cancel the job upstream:
// the proxy keeps waiting for the voice server, as the Rust server does, and
// a second clip meets the voice server's own 409.
func TestVoiceTranscribeKeepsWaitingWhenThePageGoesAway(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	cancelled := make(chan struct{}, 1)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-r.Context().Done():
			cancelled <- struct{}{}
		case <-release:
		}
		w.Write([]byte(`{"text":"done"}`))
	}))
	t.Cleanup(slow.Close)
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d, VoiceURL: slow.URL})
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", ts.URL+"/api/voice/transcribe", strings.NewReader("clip"))
	req.Header.Set("Authorization", "Bearer "+tok)
	done := make(chan struct{})
	go func() { http.DefaultClient.Do(req); close(done) }()
	<-entered
	cancel()
	<-done
	select {
	case <-cancelled:
		t.Fatal("the page going away cancelled the job at the voice server")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
}

// The proxy waits as long as the voice server can run on a 20 minute clip
// (40 windows of 120 s), so it never throws a finished text away.
func TestTheVoiceWaitCoversTheLongestJobTheServerCanRun(t *testing.T) {
	if voiceWait < 85*time.Minute {
		t.Fatalf("the proxy gives up after %v, before the server's 80 minute worst case", voiceWait)
	}
}

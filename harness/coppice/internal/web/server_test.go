package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func unusedDialer(t *testing.T) UnixDialer {
	t.Helper()
	return UnixDialer{Path: filepath.Join(t.TempDir(), "unused.sock")}
}

func TestNewRefusesANilDialer(t *testing.T) {
	store := TokenStore{Path: TokenPath(t.TempDir())}
	if _, err := store.Mint(); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{Tokens: store}); err == nil {
		t.Fatal("New accepted a nil dialer")
	}
}

func TestNewRefusesAnEmptyTokenPath(t *testing.T) {
	if _, err := New(Options{Dial: UnixDialer{Path: "/nonexistent"}}); err == nil {
		t.Fatal("New accepted an empty token path")
	}
}

func TestClientAddrStripsThePort(t *testing.T) {
	r := &http.Request{RemoteAddr: "203.0.113.5:54321"}
	if got := clientAddr(r); got != "203.0.113.5" {
		t.Fatalf("clientAddr = %q, want 203.0.113.5", got)
	}
}

// clientAddr falls back to the raw RemoteAddr when it carries no port, so a
// malformed value never panics the guard.
func TestClientAddrFallsBackToTheRawValueWithNoPort(t *testing.T) {
	r := &http.Request{RemoteAddr: "no-port-here"}
	if got := clientAddr(r); got != "no-port-here" {
		t.Fatalf("clientAddr = %q, want the raw value unchanged", got)
	}
}

// The failure this names: a ban that never lifts would lock the operator's
// own phone out for good after three mistyped tokens, and a ban a good
// token can bypass is not a ban at all.
func TestGuardBanLiftsOnceTheWindowPasses(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	_, s, tok := newTestServer(t, Options{Dial: unusedDialer(t), Now: clock})

	reached := 0
	guarded := s.guard(func(w http.ResponseWriter, r *http.Request) {
		reached++
		w.WriteHeader(http.StatusOK)
	})

	// A wrong token is a strike. A missing one is not.
	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.RemoteAddr = "198.51.100.9:1"
	req.Header.Set("Authorization", "Bearer wrong")

	for i := 0; i < BanFailures; i++ {
		rec := httptest.NewRecorder()
		guarded(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d, want 401", i, rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	guarded(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429 once banned", rec.Code)
	}

	goodReq := httptest.NewRequest(http.MethodGet, "/ws", nil)
	goodReq.RemoteAddr = "198.51.100.9:1"
	goodReq.Header.Set("Authorization", "Bearer "+tok)
	rec = httptest.NewRecorder()
	guarded(rec, goodReq)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("a good token got past a live ban: status %d, want 429", rec.Code)
	}

	now = now.Add(BanDuration + time.Second)
	rec = httptest.NewRecorder()
	guarded(rec, req)
	if rec.Code == http.StatusTooManyRequests {
		t.Fatal("the ban did not lift once its window passed")
	}
	if reached != 0 {
		t.Fatal("the handler ran on a request that still carried no token")
	}
}

// The failure this names: every other refusal in this package answers
// {"error": "..."}, the shape the client's api() reads. A guard refusal
// that answers plain text instead reads on the phone as the literal string
// "HTTP 401", throwing away the sentence the server wrote. A 401 on an API
// path must carry the same JSON shape as a 429 from a live ban.
func TestGuardRefusalsCarryTheJSONErrorShape(t *testing.T) {
	ts, _, _ := newTestServer(t, Options{Dial: unusedDialer(t), Gate: AskChannel{Root: t.TempDir()}})

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/panes", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type %q, want application/json", ct)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("the 401 body did not parse as JSON: %v", err)
	}
	msg, _ := body["error"].(string)
	if msg != "Bad token. Run coppice web token to see the current one." {
		t.Fatalf("error was %q, want the server's own sentence", msg)
	}
}

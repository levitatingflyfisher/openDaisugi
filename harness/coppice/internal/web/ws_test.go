package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// newTestServer mints a token, builds a server from opts with that token
// store, and serves it over httptest. Tokens is always the freshly minted
// store; every other field comes from opts as the caller set it. Every test
// in this package that needs a served, token-guarded server uses this one
// helper rather than minting its own.
func newTestServer(t *testing.T, opts Options) (*httptest.Server, *Server, string) {
	t.Helper()
	store := TokenStore{Path: TokenPath(t.TempDir())}
	tok, err := store.Mint()
	if err != nil {
		t.Fatal(err)
	}
	opts.Tokens = store
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, s, tok
}

func wsAddr(base string) string { return "ws" + strings.TrimPrefix(base, "http") + "/ws" }

// countingDialer wraps a Dialer and counts how many times the connection it
// hands back is closed. It proves the upstream socket a session opened is
// the same one that gets closed when the browser goes away.
type countingDialer struct {
	Dialer
	closed atomic.Int64
}

func (d *countingDialer) Dial(ctx context.Context) (io.ReadWriteCloser, error) {
	conn, err := d.Dialer.Dial(ctx)
	if err != nil {
		return nil, err
	}
	return &countingCloser{ReadWriteCloser: conn, closed: &d.closed}, nil
}

type countingCloser struct {
	io.ReadWriteCloser
	closed *atomic.Int64
}

func (c *countingCloser) Close() error {
	c.closed.Add(1)
	return c.ReadWriteCloser.Close()
}

// pipeDialer hands the session one end of an in-memory pipe and delivers the
// other end to the test over ready, so a test can end the upstream
// connection at a moment of its own choosing.
type pipeDialer struct {
	ready chan net.Conn
}

func newPipeDialer() *pipeDialer { return &pipeDialer{ready: make(chan net.Conn, 1)} }

func (d *pipeDialer) Dial(ctx context.Context) (io.ReadWriteCloser, error) {
	client, server := net.Pipe()
	d.ready <- server
	return client, nil
}

// waitForLiveWS blocks until s reports no running /ws handler, or fails the
// test once budget has passed.
func waitForLiveWS(t *testing.T, s *Server, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for s.liveWS() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the handler goroutine never returned, liveWS is %d", s.liveWS())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The failure this names: an upgrade with no token must not become a socket.
func TestWSUpgradeWithoutATokenIsRefused(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ts, _, _ := newTestServer(t, Options{Dial: d})
	_, resp, err := websocket.Dial(context.Background(), wsAddr(ts.URL), nil)
	if err == nil {
		t.Fatal("an unauthenticated upgrade succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status was %v, want 401", resp)
	}
}

func TestWSUpgradeWithABearerHeaderSucceeds(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d})
	h := http.Header{}
	h.Set("Authorization", "Bearer "+tok)
	c, _, err := websocket.Dial(context.Background(), wsAddr(ts.URL), &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatalf("upgrade with a good token: %v", err)
	}
	c.Close(websocket.StatusNormalClosure, "")
}

// A browser cannot set Authorization on a WebSocket, so the token rides a
// subprotocol offer. The negotiated protocol must be the safe name, never
// the one carrying the secret.
func TestWSUpgradeWithTheSubprotocolTokenDoesNotEchoTheToken(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d})
	c, _, err := websocket.Dial(context.Background(), wsAddr(ts.URL), &websocket.DialOptions{
		Subprotocols: []string{WSSubprotocol, WSBearerPrefix + tok},
	})
	if err != nil {
		t.Fatalf("upgrade with a subprotocol token: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	if got := c.Subprotocol(); got != WSSubprotocol {
		t.Fatalf("negotiated %q, want %q", got, WSSubprotocol)
	}
}

// The failure this names: after three guesses the address must be silenced,
// not merely told no a fourth time.
func TestWSUpgradeFromABannedAddressIsRefusedWith429(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ts, _, _ := newTestServer(t, Options{Dial: d})
	for i := 0; i < BanFailures; i++ {
		websocket.Dial(context.Background(), wsAddr(ts.URL), &websocket.DialOptions{
			Subprotocols: []string{WSSubprotocol, WSBearerPrefix + "wrong"},
		})
	}
	_, resp, err := websocket.Dial(context.Background(), wsAddr(ts.URL), &websocket.DialOptions{
		Subprotocols: []string{WSSubprotocol, WSBearerPrefix + "wrong"},
	})
	if err == nil {
		t.Fatal("a banned address still upgraded")
	}
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status was %v, want 429", resp)
	}
}

// The parity claim, at the byte level: what the browser sends is what the
// socket receives, and what the socket answers is what the browser reads.
func TestWSIsByteIdenticalToTheUnixSocketInBothDirections(t *testing.T) {
	f, d := newFakeServer(t, func(req map[string]any) []map[string]any {
		return []map[string]any{{"id": req["id"], "ok": true, "result": map[string]any{"pane": "w1:p1"}}}
	})
	ts, _, tok := newTestServer(t, Options{Dial: d})
	c, _, err := websocket.Dial(context.Background(), wsAddr(ts.URL), &websocket.DialOptions{
		Subprotocols: []string{WSSubprotocol, WSBearerPrefix + tok},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	sent := `{"id":"7","cmd":"pane.create","cwd":"/repo","kind":"pty"}`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, []byte(sent)); err != nil {
		t.Fatal(err)
	}
	typ, got, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("message type was %v, want text", typ)
	}
	var msg map[string]any
	if err := json.Unmarshal(got, &msg); err != nil {
		t.Fatalf("reply was not one JSON object: %v", err)
	}
	if msg["id"] != "7" {
		t.Fatalf("reply id was %v, want 7", msg["id"])
	}
	// The web hello comes first. Every line after it is the browser's own.
	recv := f.Received()
	if len(recv) != 2 || !isWebHelloReply(recv[0]) || string(recv[1]) != sent {
		t.Fatalf("the socket saw %q, want the web hello and then %q", recv, sent)
	}
}

// The failure this names: another page must not be able to open the
// operator's panes through the browser's own credentials.
func TestWSRejectsAnUpgradeFromAnotherOrigin(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d})
	h := http.Header{}
	h.Set("Authorization", "Bearer "+tok)
	h.Set("Origin", "https://evil.example")
	_, resp, err := websocket.Dial(context.Background(), wsAddr(ts.URL), &websocket.DialOptions{HTTPHeader: h})
	if err == nil {
		t.Fatal("a cross-origin upgrade succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status was %v, want 403", resp)
	}
}

// The failure this names: the handler must actually stop, not merely let a
// second connection through while the first one's goroutines linger.
func TestWSClosingTheBrowserClosesTheSocket(t *testing.T) {
	_, fd := newFakeServer(t, echoOK)
	cd := &countingDialer{Dialer: fd}
	ts, s, tok := newTestServer(t, Options{Dial: cd})
	c, _, err := websocket.Dial(context.Background(), wsAddr(ts.URL), &websocket.DialOptions{
		Subprotocols: []string{WSSubprotocol, WSBearerPrefix + tok},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.Close(websocket.StatusNormalClosure, "")

	waitForLiveWS(t, s, 2*time.Second)
	if got := cd.closed.Load(); got != 1 {
		t.Fatalf("the upstream socket was closed %d times, want 1", got)
	}

	c2, _, err := websocket.Dial(context.Background(), wsAddr(ts.URL), &websocket.DialOptions{
		Subprotocols: []string{WSSubprotocol, WSBearerPrefix + tok},
	})
	if err != nil {
		t.Fatalf("a second connection after a close: %v", err)
	}
	c2.Close(websocket.StatusNormalClosure, "")
}

// The failure this names: coppice-server goes away and the browser is told
// nothing. The tab sits on a socket that looks alive while every keystroke
// goes nowhere, and nothing can ever tell it to reconnect.
func TestWSUpstreamClosingEndsTheHandlerAndTellsTheBrowser(t *testing.T) {
	d := newPipeDialer()
	ts, s, tok := newTestServer(t, Options{Dial: d})
	c, _, err := websocket.Dial(context.Background(), wsAddr(ts.URL), &websocket.DialOptions{
		Subprotocols: []string{WSSubprotocol, WSBearerPrefix + tok},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	upstream := <-d.ready
	// The web hello comes first on every upstream. It is read here so the
	// close below lands on a session that is running.
	if _, err := bufio.NewReader(upstream).ReadBytes('\n'); err != nil {
		t.Fatal(err)
	}
	upstream.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err = c.Read(ctx)
	if err == nil {
		t.Fatal("the browser read a message after the upstream had gone away")
	}
	if websocket.CloseStatus(err) != websocket.StatusInternalError {
		t.Fatalf("close status was %v, want StatusInternalError", websocket.CloseStatus(err))
	}
	if !strings.Contains(err.Error(), "coppice-server closed the connection") {
		t.Fatalf("close reason was %q, want it to name the upstream", err)
	}

	waitForLiveWS(t, s, 2*time.Second)
}

// The failure this names: coppice-server was never reachable in the first
// place, and the browser is left waiting on a socket nobody is going to
// answer on.
func TestWSUnreachableUpstreamClosesWithAReasonNamingIt(t *testing.T) {
	d := UnixDialer{Path: filepath.Join(t.TempDir(), "nothing-listens-here.sock")}
	ts, s, tok := newTestServer(t, Options{Dial: d})
	c, _, err := websocket.Dial(context.Background(), wsAddr(ts.URL), &websocket.DialOptions{
		Subprotocols: []string{WSSubprotocol, WSBearerPrefix + tok},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err = c.Read(ctx)
	if err == nil {
		t.Fatal("a connection to a missing upstream still delivered a message")
	}
	if websocket.CloseStatus(err) != websocket.StatusInternalError {
		t.Fatalf("close status was %v, want StatusInternalError", websocket.CloseStatus(err))
	}
	if !strings.Contains(err.Error(), "not reachable") {
		t.Fatalf("close reason was %q, want it to name the unreachable upstream", err)
	}

	waitForLiveWS(t, s, 2*time.Second)
}

// The failure this names: the websocket accepted a frame the upstream would
// never take, so the whole connection died with no reason instead of the
// one oversize line being refused.
func TestWSRefusesAFrameOverTheUpstreamsOwnLimitAndSaysWhy(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d})
	c, _, err := websocket.Dial(context.Background(), wsAddr(ts.URL), &websocket.DialOptions{
		Subprotocols: []string{WSSubprotocol, WSBearerPrefix + tok},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	oversize := bytes.Repeat([]byte("a"), MaxRequestBytes+1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The write itself may succeed or fail depending on how quickly the
	// server reacts; the close status read back is what this test checks.
	c.Write(ctx, websocket.MessageText, oversize)
	_, _, err = c.Read(ctx)
	if err == nil {
		t.Fatal("an oversize frame was accepted with no close")
	}
	if websocket.CloseStatus(err) != websocket.StatusMessageTooBig {
		t.Fatalf("close status was %v, want StatusMessageTooBig", websocket.CloseStatus(err))
	}
}

// The failure this names: a line the websocket read limit lets through but
// the upstream's own limit refuses must still be logged. Silence here is
// indistinguishable from a phone that simply stopped typing.
func TestWSSendFailureIsLoggedAndEndsTheHandler(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
	ts, s, tok := newTestServer(t, Options{Dial: d, Log: log})
	c, _, err := websocket.Dial(context.Background(), wsAddr(ts.URL), &websocket.DialOptions{
		Subprotocols: []string{WSSubprotocol, WSBearerPrefix + tok},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	// This length passes the websocket's own read limit, MaxRequestBytes-1,
	// but Send refuses it once the newline it appends reaches
	// MaxRequestBytes. That is the one-byte band between the two limits.
	line := bytes.Repeat([]byte("a"), MaxRequestBytes-1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, line); err != nil {
		t.Fatal(err)
	}

	// A real page reads its socket. Doing the same here lets the server's
	// own close handshake finish immediately instead of waiting out its
	// unanswered-peer timeout, which is the accepted cost of a peer that
	// never reads at all.
	_, _, _ = c.Read(ctx)

	waitForLiveWS(t, s, 2*time.Second)
	if !strings.Contains(logBuf.String(), "could not forward a line") {
		t.Fatalf("log did not record the forwarding failure: %q", logBuf.String())
	}
}

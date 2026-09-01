package web

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func stateLine(pane string, ts float64) []byte {
	return []byte(fmt.Sprintf(`{"event":"state","pane":%q,"state":"working","ts":%v}`, pane, ts))
}

// liveRing is a ring whose stream counts as up, holding lines.
func liveRing(now time.Time, lines ...[]byte) *EventRing {
	r := NewEventRing(func() time.Time { return now })
	r.subscribed()
	for _, l := range lines {
		r.Add(l)
	}
	return r
}

func eventsOf(t *testing.T, body string) []map[string]any {
	t.Helper()
	var got struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return got.Events
}

func TestTheRingKeepsTwoHoursAndNoMoreThanItsMax(t *testing.T) {
	now := time.Unix(100000, 0)
	r := liveRing(now, stateLine("old", 100000-3*3600), stateLine("new", 100000-60))
	got := r.Since(0)
	if len(got) != 1 || !strings.Contains(string(got[0]), `"new"`) {
		t.Fatalf("%s", got)
	}
	for i := 0; i < RingMax+5; i++ {
		r.Add(stateLine("p", float64(100000-3000)+float64(i)/10))
	}
	got = r.Since(0)
	if len(got) != RingMax {
		t.Fatalf("the ring holds %d events, want %d", len(got), RingMax)
	}
	var last struct{ TS float64 }
	json.Unmarshal(got[len(got)-1], &last)
	if last.TS != float64(100000-3000)+float64(RingMax+4)/10 {
		t.Fatalf("the newest event is gone: %v", last.TS)
	}
}

func TestTheRingKeepsOnlyStateEventsWithATime(t *testing.T) {
	r := liveRing(time.Unix(1000, 0),
		[]byte(`{"event":"state","pane":"a"}`),
		[]byte(`{"event":"frame","pane":"a","ts":999}`),
		[]byte(`not json`),
		stateLine("a", 999),
	)
	if got := r.Since(0); len(got) != 1 {
		t.Fatalf("%s", got)
	}
}

func TestAPIEventsReturnsOnlyEventsAfterSince(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ring := liveRing(time.Unix(2000, 0), stateLine("a", 1000), stateLine("b", 1500), stateLine("c", 1900))
	ts, _, tok := newTestServer(t, Options{Dial: d, Events: ring})
	resp, body := getWith(t, ts.URL+"/api/events?since=1500", tok)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	evs := eventsOf(t, body)
	if len(evs) != 1 || evs[0]["pane"] != "c" {
		t.Fatalf("%s", body)
	}
	_, body = getWith(t, ts.URL+"/api/events", tok)
	if len(eventsOf(t, body)) != 3 {
		t.Fatalf("with no since: %s", body)
	}
}

func TestAPIEventsRefusesASinceThatIsNotATime(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d, Events: liveRing(time.Unix(2000, 0))})
	for _, q := range []string{"abc", "NaN", "Inf", "-1"} {
		if resp, body := getWith(t, ts.URL+"/api/events?since="+q, tok); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("since=%s: %d %s", q, resp.StatusCode, body)
		}
	}
}

// No ring, or a ring whose stream is down, is an error and never an empty
// list: an empty hour and a missed hour are different facts.
func TestAPIEventsFailsClosedWithNoLiveRing(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	down := NewEventRing(time.Now)
	down.Add(stateLine("a", float64(time.Now().Unix())))
	for name, r := range map[string]*EventRing{"none": nil, "down": down} {
		ts, _, tok := newTestServer(t, Options{Dial: d, Events: r})
		resp, body := getWith(t, ts.URL+"/api/events", tok)
		if resp.StatusCode != http.StatusServiceUnavailable || strings.Contains(body, `"events"`) {
			t.Errorf("%s: %d %s", name, resp.StatusCode, body)
		}
	}
}

func TestAPIEventsIsBehindTheToken(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ts, _, _ := newTestServer(t, Options{Dial: d, Events: liveRing(time.Unix(2000, 0), stateLine("a", 1999))})
	if resp, body := getWith(t, ts.URL+"/api/events", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d %s", resp.StatusCode, body)
	}
}

// dropOnce is a coppice-server whose first connection answers the
// subscribe, sends one event and hangs up, and whose later connections
// answer and stay open.
func dropOnce(t *testing.T) (Dialer, *atomic.Int32) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var n atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			k := n.Add(1)
			go func(conn net.Conn, k int32) {
				defer conn.Close()
				sc := bufio.NewScanner(conn)
				for sc.Scan() {
					var req map[string]any
					if json.Unmarshal(sc.Bytes(), &req) != nil || req["cmd"] != "events.subscribe" {
						continue
					}
					ack, _ := json.Marshal(map[string]any{"id": req["id"], "ok": true, "result": map[string]any{}})
					conn.Write(append(ack, '\n'))
					conn.Write(append(stateLine(fmt.Sprintf("p%d", k), float64(time.Now().Unix())), '\n'))
					if k == 1 {
						return
					}
				}
			}(conn, k)
		}
	}()
	return UnixDialer{Path: path}, &n
}

func TestTheRingSubscribesAndComesBackAfterADrop(t *testing.T) {
	d, conns := dropOnce(t)
	r := NewEventRing(time.Now)
	r.Retry = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		r.Run(ctx, d, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(r.Since(0)) == 2 && r.Live() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := r.Since(0); len(got) != 2 || !r.Live() || conns.Load() < 2 {
		t.Fatalf("after a drop the ring holds %s, live %v, %d connections", got, r.Live(), conns.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not end with its context")
	}
	if r.Live() {
		t.Fatal("a stopped ring still counts as live")
	}
}

// The ring says since when it holds every event. A new subscription starts
// that time again, so a hole while it was down never reads as quiet.
func TestTheRingSaysSinceWhenItHoldsEveryEvent(t *testing.T) {
	now := time.Unix(100000, 0)
	r := NewEventRing(func() time.Time { return now })
	r.subscribed()
	if got := r.From(); got != 100000 {
		t.Fatalf("from after a subscribe: %v", got)
	}
	r.Add(stateLine("a", 100010))
	now = now.Add(3 * time.Hour)
	if got := r.From(); got != 100000+3600 {
		t.Fatalf("from after three hours: %v, want the start of the two hour span", got)
	}
	r.setLive(false)
	now = now.Add(time.Minute)
	r.subscribed()
	if got := r.From(); got != float64(now.Unix()) {
		t.Fatalf("from after a new subscribe: %v, want %v", got, now.Unix())
	}
}

func TestTheRingFromMovesWhenTheMaxDropsEvents(t *testing.T) {
	now := time.Unix(98900, 0)
	r := NewEventRing(func() time.Time { return now })
	r.subscribed()
	now = time.Unix(100000, 0)
	for i := 0; i < RingMax+3; i++ {
		r.Add(stateLine("p", 99000+float64(i)/100))
	}
	oldest := 99000 + float64(3)/100
	if got := r.From(); got != oldest {
		t.Fatalf("from %v, want the oldest kept event %v", got, oldest)
	}
}

func TestAPIEventsSaysSinceWhenTheRingHoldsEveryEvent(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ring := liveRing(time.Unix(2000, 0), stateLine("a", 1990))
	ts, _, tok := newTestServer(t, Options{Dial: d, Events: ring})
	_, body := getWith(t, ts.URL+"/api/events", tok)
	var got struct {
		From   *float64         `json:"from"`
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil || got.From == nil || *got.From != 2000 {
		t.Fatalf("%s", body)
	}
}

// A ring whose Retry is zero after it is made still waits between tries.
func TestAZeroRetryNeverSpins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var n atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			go func(conn net.Conn) {
				defer conn.Close()
				sc := bufio.NewScanner(conn)
				if sc.Scan() {
					var req map[string]any
					json.Unmarshal(sc.Bytes(), &req)
					ack, _ := json.Marshal(map[string]any{"id": req["id"], "ok": true, "result": map[string]any{}})
					conn.Write(append(ack, '\n'))
				}
			}(conn)
		}
	}()
	r := NewEventRing(time.Now)
	r.Retry = 0
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	r.Run(ctx, UnixDialer{Path: path}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got := n.Load(); got > 3 {
		t.Fatalf("%d connections in 300 ms with Retry 0", got)
	}
}

package web

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	// RingMax is the most state events the ring holds.
	RingMax = 10000
	// RingSpan is how far back the ring holds state events.
	RingSpan = 2 * time.Hour
	// ringRetryMax is the longest wait between two tries to subscribe.
	ringRetryMax = 30 * time.Second
)

// ringEvent is one state event line and its time.
type ringEvent struct {
	ts  float64
	raw json.RawMessage
}

// EventRing holds the state events of the last RingSpan, at most RingMax
// of them, oldest first. The web server feeds it from its own
// subscription, so the floor page can read the last two hours after a
// reload. Live is false while that subscription is down, and the API then
// refuses to answer from the ring. From is the time since which the ring
// holds every state event with no break. A new subscription starts it
// again, so a hole while the ring was down never reads as a quiet span.
type EventRing struct {
	// Retry is the first wait after a lost subscription. It doubles up to
	// thirty seconds.
	Retry time.Duration

	mu     sync.Mutex
	events []ringEvent
	live   bool
	// from is the time the current subscription was answered, or the
	// time of the oldest kept event once the max dropped older ones.
	from float64
	now  func() time.Time
}

// NewEventRing makes an empty ring that is not live. now is the clock.
func NewEventRing(now func() time.Time) *EventRing {
	if now == nil {
		now = time.Now
	}
	return &EventRing{Retry: time.Second, now: now}
}

// Add keeps line when it is a state event with a time. Any other line is
// dropped.
func (r *EventRing) Add(line []byte) {
	var ev struct {
		Event string   `json:"event"`
		TS    *float64 `json:"ts"`
	}
	if json.Unmarshal(line, &ev) != nil || ev.Event != "state" || ev.TS == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ringEvent{ts: *ev.TS, raw: append(json.RawMessage(nil), line...)})
	r.prune()
}

// prune drops events older than RingSpan and the oldest past RingMax. The
// caller holds mu.
func (r *EventRing) prune() {
	cut := seconds(r.now().Add(-RingSpan))
	drop := 0
	for drop < len(r.events) && r.events[drop].ts < cut {
		drop++
	}
	capped := false
	if over := len(r.events) - drop - RingMax; over > 0 {
		drop += over
		capped = true
	}
	if drop > 0 {
		r.events = append([]ringEvent(nil), r.events[drop:]...)
	}
	if capped && len(r.events) > 0 && r.events[0].ts > r.from {
		r.from = r.events[0].ts
	}
}

// seconds is t as seconds since the epoch.
func seconds(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

// From is the time since which the ring holds every state event: the
// latest of the subscription's start, the start of the RingSpan, and the
// oldest event kept once the max dropped older ones.
func (r *EventRing) From() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prune()
	return max(r.from, seconds(r.now().Add(-RingSpan)))
}

// subscribed marks the ring live from now on.
func (r *EventRing) subscribed() {
	r.mu.Lock()
	r.live = true
	r.from = seconds(r.now())
	r.mu.Unlock()
}

// Since is the held events whose time is after since, oldest first.
func (r *EventRing) Since(since float64) []json.RawMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prune()
	out := []json.RawMessage{}
	for _, ev := range r.events {
		if ev.ts > since {
			out = append(out, ev.raw)
		}
	}
	return out
}

// Live reports whether the subscription that feeds the ring is up.
func (r *EventRing) Live() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.live
}

func (r *EventRing) setLive(v bool) {
	r.mu.Lock()
	r.live = v
	r.mu.Unlock()
}

// Run feeds the ring until ctx ends. A lost subscription is tried again,
// first after Retry and then after twice the last wait, up to thirty
// seconds.
func (r *EventRing) Run(ctx context.Context, d Dialer, log *slog.Logger) {
	first := r.Retry
	if first <= 0 {
		first = time.Second
	}
	wait := first
	for {
		err := r.watch(ctx, d, func() { wait = first })
		r.setLive(false)
		if ctx.Err() != nil {
			return
		}
		log.Warn("web: the event ring lost its subscription", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, ringRetryMax)
	}
}

// watch subscribes to state events and adds each to the ring. up runs once
// the subscribe is answered.
func (r *EventRing) watch(ctx context.Context, d Dialer, up func()) error {
	sess, err := Open(ctx, d)
	if err != nil {
		return err
	}
	defer sess.Close()
	const subID = "ring-sub"
	sub, err := json.Marshal(map[string]any{
		"id": subID, "cmd": "events.subscribe", "panes": "*", "kinds": []string{"state"},
	})
	if err != nil {
		return err
	}
	if err := sess.Send(sub); err != nil {
		return err
	}
	ack := time.NewTimer(5 * time.Second)
	defer ack.Stop()
	subscribed := false
	for {
		var timeout <-chan time.Time
		if !subscribed {
			timeout = ack.C
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return errors.New("coppice-server did not answer events.subscribe in 5 s")
		case line, ok := <-sess.Lines():
			if !ok {
				return errors.New("coppice-server closed the event stream")
			}
			if !subscribed {
				var reply struct {
					ID string `json:"id"`
					OK bool   `json:"ok"`
				}
				if json.Unmarshal(line, &reply) == nil && reply.ID == subID {
					if !reply.OK {
						var msg map[string]any
						json.Unmarshal(line, &msg)
						return refusal(msg)
					}
					subscribed = true
					r.subscribed()
					up()
					continue
				}
			}
			r.Add(line)
		}
	}
}

// handleEvents answers GET /api/events?since=<ts> with the held state
// events after since, and from, the time since which the ring holds every
// event. With no ring, or while its subscription is down, it refuses: a
// gap is not an empty hour.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	since := 0.0
	if raw := r.URL.Query().Get("since"); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			writeErr(w, http.StatusBadRequest, "since must be a time in seconds, 0 or more.")
			return
		}
		since = v
	}
	ring := s.opts.Events
	if ring == nil || !ring.Live() {
		writeErr(w, http.StatusServiceUnavailable, "The event ring is not running. Run coppice server status.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": ring.Since(since), "from": ring.From()})
}

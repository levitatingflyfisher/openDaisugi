package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
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

type capturedPush struct {
	mu   sync.Mutex
	reqs []struct {
		Path, Title, Click, Auth, Body, Actions string
	}
}

func (c *capturedPush) add(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, struct{ Path, Title, Click, Auth, Body, Actions string }{
		r.URL.Path, r.Header.Get("Title"), r.Header.Get("Click"), r.Header.Get("Authorization"), string(body),
		r.Header.Get("Actions"),
	})
}

func (c *capturedPush) len() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.reqs) }

func newNtfy(t *testing.T) (*capturedPush, string) {
	t.Helper()
	cap := &capturedPush{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.add(r)
		w.WriteHeader(200)
	}))
	t.Cleanup(ts.Close)
	return cap, ts.URL
}

func newPublisher(t *testing.T, base string, now *time.Time, tokenEnv string) *Publisher {
	t.Helper()
	p, err := NewPublisher(PushConfig{
		BaseURL:   base,
		Topic:     "coppice",
		TokenEnv:  tokenEnv,
		ClickBase: "https://box.tail1234.ts.net:8443",
	}, http.DefaultClient, func() time.Time { return *now }, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func blockedEvent(pane string) StateEvent {
	return StateEvent{Pane: pane, Harness: "claude-code", State: "blocked", Source: "gate",
		Ask: &Ask{ID: "toolu_1", Tool: "Bash", Summary: "rm -rf build/"}}
}

func TestPublishesOnceWhenAPaneBecomesBlocked(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	p.OnState(StateEvent{Pane: "w1:p1", State: "working"}, "auth fix")
	p.OnState(blockedEvent("w1:p1"), "auth fix")
	if cap.len() != 1 {
		t.Fatalf("%d publishes, want 1", cap.len())
	}
	got := cap.reqs[0]
	if got.Path != "/coppice" {
		t.Errorf("posted to %q, want /coppice", got.Path)
	}
	if got.Title != "auth fix needs you." {
		t.Errorf("title was %q", got.Title)
	}
	if got.Body != "Wants rm -rf build/. Gate says no." {
		t.Errorf("body was %q", got.Body)
	}
	if got.Click != "https://box.tail1234.ts.net:8443/#/pane/w1:p1?ask=toolu_1" {
		t.Errorf("click was %q", got.Click)
	}
	if got.Auth != "" {
		t.Errorf("an unauthenticated ntfy got an Authorization header: %q", got.Auth)
	}
}

func TestDoesNotPublishWhileThePaneStaysBlocked(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	p.OnState(blockedEvent("w1:p1"), "auth fix")
	now = now.Add(time.Hour)
	p.OnState(blockedEvent("w1:p1"), "auth fix")
	p.OnState(blockedEvent("w1:p1"), "auth fix")
	if cap.len() != 1 {
		t.Fatalf("%d publishes, want 1: repeat blocked events are not new news", cap.len())
	}
}

func TestPublishesAgainAfterBlockedClearsAndReturns(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	p.OnState(blockedEvent("w1:p1"), "auth fix")
	p.OnState(StateEvent{Pane: "w1:p1", State: "working"}, "auth fix")
	now = now.Add(30 * time.Second)
	p.OnState(blockedEvent("w1:p1"), "auth fix")
	if cap.len() != 2 {
		t.Fatalf("%d publishes, want 2", cap.len())
	}
}

// The failure this names: a pane that flaps blocked, working, blocked inside
// five seconds must not buzz the phone twice.
func TestDebouncesARepeatBlockWithinFiveSeconds(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	p.OnState(blockedEvent("w1:p1"), "auth fix")
	p.OnState(StateEvent{Pane: "w1:p1", State: "working"}, "auth fix")
	now = now.Add(2 * time.Second)
	p.OnState(blockedEvent("w1:p1"), "auth fix")
	if cap.len() != 1 {
		t.Fatalf("%d publishes, want 1 inside the %v debounce", cap.len(), PushDebounce)
	}
}

func TestSendsTheBearerTokenFromTheNamedEnvironmentVariable(t *testing.T) {
	cap, base := newNtfy(t)
	t.Setenv("COPPICE_NTFY_TOKEN", "tk_secret")
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "COPPICE_NTFY_TOKEN")
	p.OnState(blockedEvent("w1:p1"), "auth fix")
	if cap.len() != 1 {
		t.Fatal("no publish")
	}
	if cap.reqs[0].Auth != "Bearer tk_secret" {
		t.Fatalf("Authorization was %q", cap.reqs[0].Auth)
	}
}

// The failure this names: a variable that is named but left empty is not a
// token. Sending it anyway would hand ntfy, or a proxy in front of it, an
// empty bearer credential instead of no credential at all.
func TestSendsNoBearerTokenWhenTheNamedVariableIsSetEmpty(t *testing.T) {
	cap, base := newNtfy(t)
	t.Setenv("COPPICE_NTFY_TOKEN", "")
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "COPPICE_NTFY_TOKEN")
	p.OnState(blockedEvent("w1:p1"), "auth fix")
	if cap.len() != 1 {
		t.Fatal("no publish")
	}
	if cap.reqs[0].Auth != "" {
		t.Fatalf("Authorization was %q, want none for an empty variable", cap.reqs[0].Auth)
	}
}

func TestNewPublisherRefusesAnEmptyBaseURLOrTopic(t *testing.T) {
	cases := []PushConfig{
		{Topic: "coppice"},
		{BaseURL: "https://ntfy.example"},
	}
	for _, cfg := range cases {
		_, err := NewPublisher(cfg, nil, nil, nil)
		if err == nil {
			t.Fatalf("%+v: NewPublisher accepted a config with no BaseURL or no Topic", cfg)
		}
		if !strings.Contains(err.Error(), "--ntfy") {
			t.Fatalf("%+v: error %q does not teach the flag", cfg, err)
		}
	}
}

func TestNonAsciiTitleAndBodyAreSanitized(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	ev := blockedEvent("w1:p1")
	ev.Ask.Summary = "rm -rf — build/\nsecond line"
	p.OnState(ev, "café fix")
	got := cap.reqs[0]
	for _, s := range []string{got.Title, got.Body} {
		for _, r := range s {
			if r < 0x20 || r > 0x7e {
				t.Fatalf("%q still carries a non-ASCII or control rune %q", s, r)
			}
		}
	}
}

func TestLabelFallsBackToThePaneId(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	p.OnState(blockedEvent("w1:p1"), "")
	if cap.reqs[0].Title != "w1:p1 needs you." {
		t.Fatalf("title was %q", cap.reqs[0].Title)
	}
}

func TestBodyCarriesDetailWhenThereIsNoAskSummary(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	ev := StateEvent{Pane: "w1:p1", State: "blocked", Detail: "waiting on the human"}
	p.OnState(ev, "auth fix")
	if cap.reqs[0].Body != "waiting on the human" {
		t.Fatalf("body was %q", cap.reqs[0].Body)
	}
}

func TestBodyFallsBackToThePlaceholderWithNeitherAskNorDetail(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	ev := StateEvent{Pane: "w1:p1", State: "blocked", Source: "gate"}
	p.OnState(ev, "auth fix")
	if cap.reqs[0].Body != BlockedByGate {
		t.Fatalf("body was %q", cap.reqs[0].Body)
	}
}

// A block the screen manifest found says the agent asks, never that the
// gate gave no detail, and never the name of the rule that matched.
func TestAScreenBlockPushSaysTheAgentAsks(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	p.OnState(StateEvent{Pane: "w1:p1", State: "blocked", Source: "manifest", Detail: "live_prompt_box"}, "auth fix")
	if cap.reqs[0].Body != BlockedOwnQuestion {
		t.Fatalf("body was %q", cap.reqs[0].Body)
	}
}

// The failure this names: Title and the body both go through asciiHeader
// before they become an HTTP header or a request body. Click went out raw.
// Go's transport refuses a header value carrying a control character
// rather than splitting the request, and the pane id is server-generated,
// so nothing observed has forged one yet, but Click is the one asymmetry
// in an otherwise scrubbed boundary. A realistic click target must still
// arrive byte for byte: the bound has to be large enough that no real URL
// ever gets the three-dot truncation asciiHeader gives an over-length
// Title or body.
func TestClickHeaderSurvivesARealisticURLByteForByte(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	longHost := "operator-" + strings.Repeat("subdomain.", 20) + "tail1234.ts.net"
	clickBase := "https://" + longHost + ":8443"
	p, err := NewPublisher(PushConfig{
		BaseURL: base, Topic: "coppice", ClickBase: clickBase,
	}, http.DefaultClient, func() time.Time { return now }, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.OnState(blockedEvent("w1:p1"), "auth fix")
	want := clickBase + "/#/pane/w1:p1?ask=toolu_1"
	if cap.reqs[0].Click != want {
		t.Fatalf("click was %q, want %q unchanged", cap.reqs[0].Click, want)
	}
}

// A click target built from an operator-set --external-url could in
// principle carry a control character. asciiHeader must scrub it before it
// ever reaches Go's own header validation, the same way Title and the body
// already do, rather than let a malformed Click fail the whole publish.
func TestClickHeaderScrubsAControlCharacter(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p, err := NewPublisher(PushConfig{
		BaseURL: base, Topic: "coppice", ClickBase: "https://box.example\r\nX-Injected: yes",
	}, http.DefaultClient, func() time.Time { return now }, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.OnState(blockedEvent("w1:p1"), "auth fix")
	if cap.len() != 1 {
		t.Fatalf("the publish with a control character in Click never reached ntfy")
	}
	for _, r := range cap.reqs[0].Click {
		if r < 0x20 || r > 0x7e {
			t.Fatalf("Click %q still carries a control rune %q", cap.reqs[0].Click, r)
		}
	}
}

func TestAsciiHeaderTruncatesLongInputWithThreeDots(t *testing.T) {
	long := strings.Repeat("a", 200)
	got := asciiHeader(long, 100)
	if len(got) > 100 {
		t.Fatalf("length was %d, want at most 100", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("got %q, want it to end in three dots", got)
	}
}

// Push is a courtesy, not a permission. An ntfy that is down or refusing
// must never take the floor down with it.
func TestAPushFailureNeverStopsTheServer(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer ts.Close()
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, ts.URL, &now, "")
	p.OnState(blockedEvent("w1:p1"), "auth fix") // must not panic
	p.OnState(StateEvent{Pane: "w1:p1", State: "working"}, "auth fix")
	now = now.Add(30 * time.Second)
	p.OnState(blockedEvent("w1:p1"), "auth fix") // still tries
	if err := p.Test(context.Background()); err == nil {
		t.Fatal("Test reported success against a 500")
	}
}

// The failure this names: an unauthenticated ntfy has no credential but the
// topic name, so a topic that reaches the log is a credential leaking into
// a file every operator assumes is safe to share.
func TestNtfyPublishFailureLogDoesNotCarryTheTopic(t *testing.T) {
	var logBuf strings.Builder
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
	now := time.Unix(1_757_300_000, 0)
	p, err := NewPublisher(PushConfig{
		BaseURL: "http://127.0.0.1:1",
		Topic:   "coppice-3f9a2b-private",
	}, &http.Client{Timeout: time.Second}, func() time.Time { return now }, log)
	if err != nil {
		t.Fatal(err)
	}
	p.OnState(blockedEvent("w1:p1"), "auth fix")
	if strings.Contains(logBuf.String(), "coppice-3f9a2b-private") {
		t.Fatalf("the log carried the topic: %s", logBuf.String())
	}
}

func TestWatchTurnsSocketStateEventsIntoPushes(t *testing.T) {
	cap, base := newNtfy(t)
	_, d := newFakeServer(t, func(req map[string]any) []map[string]any {
		switch req["cmd"] {
		case "pane.list":
			return []map[string]any{{"id": req["id"], "ok": true, "result": map[string]any{
				"panes": []any{map[string]any{"id": "w1:p1", "label": "auth fix"}}}}}
		case "events.subscribe":
			return []map[string]any{
				{"id": req["id"], "ok": true},
				{"event": "state", "pane": "w1:p1", "state": "blocked", "source": "gate", "harness": "claude-code",
					"ask": map[string]any{"id": "toolu_1", "tool": "Bash", "summary": "rm -rf build/"}},
			}
		}
		return []map[string]any{{"id": req["id"], "ok": true}}
	})
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.Watch(ctx, d) }()
	deadline := time.Now().Add(3 * time.Second)
	for cap.len() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if cap.len() != 1 {
		t.Fatalf("%d publishes from one socket state event", cap.len())
	}
	if !strings.Contains(cap.reqs[0].Title, "auth fix") {
		t.Errorf("Watch did not look the label up: title %q", cap.reqs[0].Title)
	}

	// The failure this names: a Watch that ignores cancellation keeps its
	// socket open and keeps publishing from a Publisher its caller believes
	// it has already retired.
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Watch returned %v after its context was cancelled, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not return after its context was cancelled")
	}
}

// The failure this names: Watch reads events.subscribe's reply on the same
// stream as every event. Treating that reply as unconditionally good leaves
// Watch waiting on events a refused subscription will never send.
func TestWatchReturnsARefusalWhenTheSubscriptionIsRefused(t *testing.T) {
	_, d := newFakeServer(t, func(req map[string]any) []map[string]any {
		if req["cmd"] == "events.subscribe" {
			return []map[string]any{{"id": req["id"], "ok": false, "error": map[string]any{
				"code": "bad_request", "message": "kinds is not a known filter",
			}}}
		}
		return []map[string]any{{"id": req["id"], "ok": true, "result": map[string]any{"panes": []any{}}}}
	})
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, "http://127.0.0.1:1", &now, "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := p.Watch(ctx, d)
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("Watch returned %v, want a RefusedError", err)
	}
	if refused.Code != "bad_request" {
		t.Fatalf("code was %q, want bad_request", refused.Code)
	}
}

// The failure this names: a pane the initial pane.list did not know, seen
// again and again, must cost one lookup, not one lookup per event.
func TestUnlistedPaneCostsOneLookupNotOnePerEvent(t *testing.T) {
	var paneListCalls atomic.Int64
	_, d := newFakeServer(t, func(req map[string]any) []map[string]any {
		switch req["cmd"] {
		case "pane.list":
			paneListCalls.Add(1)
			return []map[string]any{{"id": req["id"], "ok": true, "result": map[string]any{"panes": []any{}}}}
		case "events.subscribe":
			replies := []map[string]any{{"id": req["id"], "ok": true}}
			for i := 0; i < 10; i++ {
				replies = append(replies, map[string]any{
					"event": "state", "pane": "w9:p9", "state": "working",
					"source": "gate", "harness": "claude-code",
				})
			}
			return replies
		}
		return []map[string]any{{"id": req["id"], "ok": true}}
	})
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, "http://127.0.0.1:1", &now, "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.Watch(ctx, d) }()

	deadline := time.Now().Add(2 * time.Second)
	for paneListCalls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// A window past the second call, wide enough for a bug that calls
	// pane.list once per event to have made a third or fourth call by now.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if got := paneListCalls.Load(); got != 2 {
		t.Fatalf("pane.list was called %d times for ten events about one unlisted pane, want 2", got)
	}
}

// actionsOf splits an ntfy Actions header into its actions, each a list
// of its comma parts with the quotes the header uses left on.
func actionsOf(h string) [][]string {
	if h == "" {
		return nil
	}
	var out [][]string
	for _, a := range strings.Split(h, "; ") {
		out = append(out, strings.Split(a, ", "))
	}
	return out
}

func tieredEvent(pane, tier string) StateEvent {
	ev := blockedEvent(pane)
	ev.Ask.Tier = tier
	ev.Ask.Deadline = 1_757_300_090
	return ev
}

func TestAPermanentAskCardHasDenyThenLook(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	p.OnState(tieredEvent("w1:p1", "permanent"), "auth fix")
	acts := actionsOf(cap.reqs[0].Actions)
	if len(acts) != 2 {
		t.Fatalf("actions %q, want exactly two", cap.reqs[0].Actions)
	}
	if acts[0][0] != "http" || acts[0][1] != "Deny" || acts[1][0] != "view" || acts[1][1] != "Look" {
		t.Fatalf("actions %q, want Deny then Look", cap.reqs[0].Actions)
	}
}

func TestAnAskWithNoTierCardIsPermanent(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	p.OnState(tieredEvent("w1:p1", ""), "auth fix")
	if n := len(actionsOf(cap.reqs[0].Actions)); n != 2 {
		t.Fatalf("actions %q, want two", cap.reqs[0].Actions)
	}
}

func TestAnUndoableAskCardHasAllowLast(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	p.OnState(tieredEvent("w1:p1", "undoable"), "auth fix")
	acts := actionsOf(cap.reqs[0].Actions)
	if len(acts) != 3 || acts[0][1] != "Deny" || acts[1][1] != "Look" || acts[2][1] != "Allow" {
		t.Fatalf("actions %q, want Deny, Look, Allow", cap.reqs[0].Actions)
	}
	// Allow opens the pane screen. The card carries no credential that
	// can allow.
	if acts[2][0] != "view" || acts[2][2] != "https://box.tail1234.ts.net:8443/#/pane/w1:p1?ask=toolu_1" {
		t.Fatalf("Allow is %q, want a view of the pane screen", acts[2])
	}
}

func TestTheBodySaysTheRuleWhenTheGateGivesOne(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	ev := tieredEvent("w1:p1", "permanent")
	rule := 3
	ev.Ask.Gate = &AskGate{Verdict: "deny", Rule: &rule}
	p.OnState(ev, "auth fix")
	if cap.reqs[0].Body != "Wants rm -rf build/. Gate says no, rule 3." {
		t.Fatalf("body %q", cap.reqs[0].Body)
	}
}

// The Deny action carries a deny token, never the bearer: ntfy stores the
// action's headers, and a leaked card can at worst deny.
func TestTheDenyActionCarriesADenyTokenNotTheBearer(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	p.OnState(tieredEvent("w1:p1", "permanent"), "auth fix")
	deny := cap.reqs[0].Actions
	tok := denyTokenIn(t, deny)
	if !strings.Contains(deny, "https://box.tail1234.ts.net:8443/api/ask/answer") {
		t.Fatalf("deny action %q does not post to the answer route", deny)
	}
	if !strings.Contains(deny, `"decision":"deny"`) || !strings.Contains(deny, `"tool_use_id":"toolu_1"`) {
		t.Fatalf("deny action %q has the wrong body", deny)
	}
	if got, ok := p.DenyGrant(tok); !ok || got != "toolu_1" {
		t.Fatalf("DenyGrant(%q) = %q %v, want toolu_1", tok, got, ok)
	}
	if _, ok := p.DenyGrant("not-a-token"); ok {
		t.Fatal("a made-up token was granted")
	}
}

func denyTokenIn(t *testing.T, actions string) string {
	t.Helper()
	const mark = "headers.Authorization=Bearer "
	i := strings.Index(actions, mark)
	if i < 0 {
		t.Fatalf("no bearer in %q", actions)
	}
	rest := actions[i+len(mark):]
	if j := strings.IndexAny(rest, ", ;"); j >= 0 {
		rest = rest[:j]
	}
	if len(rest) < 32 {
		t.Fatalf("token %q is too short", rest)
	}
	return rest
}

func TestADenyTokenIsGoneOnceTheAskIsGone(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	p.OnState(tieredEvent("w1:p1", "permanent"), "auth fix")
	tok := denyTokenIn(t, cap.reqs[0].Actions)
	p.OnState(StateEvent{Pane: "w1:p1", State: "working"}, "auth fix")
	if _, ok := p.DenyGrant(tok); ok {
		t.Fatal("a token outlived its ask")
	}
	if !p.DenyRetired(tok) {
		t.Fatal("a retired token is not known as retired")
	}
}

func TestADenyTokenIsGoneAfterTheDeadline(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	p.OnState(tieredEvent("w1:p1", "permanent"), "auth fix")
	tok := denyTokenIn(t, cap.reqs[0].Actions)
	now = now.Add(91 * time.Second)
	if _, ok := p.DenyGrant(tok); ok {
		t.Fatal("a token outlived its ask's deadline")
	}
	if !p.DenyRetired(tok) {
		t.Fatal("a retired token is not known as retired")
	}
}

func TestRetireDenyDropsTheToken(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	p.OnState(tieredEvent("w1:p1", "permanent"), "auth fix")
	tok := denyTokenIn(t, cap.reqs[0].Actions)
	p.RetireDeny("toolu_1")
	if _, ok := p.DenyGrant(tok); ok {
		t.Fatal("a retired token still granted")
	}
	if !p.DenyRetired(tok) {
		t.Fatal("a retired token is not known as retired")
	}
}

func TestACardWithNoClickBaseHasNoActions(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p, err := NewPublisher(PushConfig{BaseURL: base, Topic: "coppice"}, http.DefaultClient,
		func() time.Time { return now }, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.OnState(tieredEvent("w1:p1", "undoable"), "auth fix")
	if cap.reqs[0].Actions != "" {
		t.Fatalf("actions %q with no external URL", cap.reqs[0].Actions)
	}
}

func TestAMadeUpTokenIsNotRetired(t *testing.T) {
	_, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	if p.DenyRetired("made-up") {
		t.Fatal("a token nobody minted read as retired")
	}
}

func TestARetiredTokenIsForgottenAfterADay(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	p.OnState(tieredEvent("w1:p1", "permanent"), "auth fix")
	tok := denyTokenIn(t, cap.reqs[0].Actions)
	p.RetireDeny("toolu_1")
	now = now.Add(25 * time.Hour)
	if p.DenyRetired(tok) {
		t.Fatal("a retired token was kept past a day")
	}
}

// An ask with no deadline still gets a token that dies by time.
func TestADenyTokenForAnAskWithNoDeadlineStillExpires(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	ev := tieredEvent("w1:p1", "permanent")
	ev.Ask.Deadline = 0
	p.OnState(ev, "auth fix")
	tok := denyTokenIn(t, cap.reqs[0].Actions)
	if _, ok := p.DenyGrant(tok); !ok {
		t.Fatal("the token was not granted at once")
	}
	now = now.Add(11 * time.Minute)
	if _, ok := p.DenyGrant(tok); ok {
		t.Fatal("a token for an ask with no deadline never expired")
	}
}

// The card lands on the ask: the click, Look, and Allow each name the pane
// and the ask, and carry no credential.
func TestTheCardClickAndLookNameThePaneAndTheAsk(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	p.OnState(tieredEvent("w1:p1", "undoable"), "auth fix")
	want := "https://box.tail1234.ts.net:8443/#/pane/w1:p1?ask=toolu_1"
	if cap.reqs[0].Click != want {
		t.Fatalf("click was %q, want %q", cap.reqs[0].Click, want)
	}
	acts := actionsOf(cap.reqs[0].Actions)
	if acts[1][1] != "Look" || acts[1][2] != want {
		t.Fatalf("Look is %q, want a view of %q", acts[1], want)
	}
	if acts[2][1] != "Allow" || acts[2][2] != want {
		t.Fatalf("Allow is %q, want a view of %q", acts[2], want)
	}
	for _, a := range acts[1:] {
		if strings.Contains(strings.Join(a, ", "), "Bearer") || strings.Contains(strings.Join(a, ", "), "token") {
			t.Fatalf("a view action carries a credential: %q", a)
		}
	}
}

// An ask id that is not safe to carry never rides in the click URL. The
// card still opens the pane.
func TestAnUnsafeAskIDStaysOutOfTheClick(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	ev := blockedEvent("w1:p1")
	ev.Ask.ID = "toolu_1&x=<y>"
	p.OnState(ev, "auth fix")
	if cap.reqs[0].Click != "https://box.tail1234.ts.net:8443/#/pane/w1:p1" {
		t.Fatalf("click was %q", cap.reqs[0].Click)
	}
}

// A pane id with a character the Actions header or the route treats as
// syntax is escaped, so it neither splits the action list nor breaks the
// page's route.
func TestAPaneIDIsEscapedInTheClick(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	p.OnState(tieredEvent("w1:p1;a,b?c#d%e", "undoable"), "odd")
	want := "https://box.tail1234.ts.net:8443/#/pane/w1:p1%3Ba%2Cb%3Fc%23d%25e?ask=toolu_1"
	if cap.reqs[0].Click != want {
		t.Fatalf("click was %q, want %q", cap.reqs[0].Click, want)
	}
	if acts := actionsOf(cap.reqs[0].Actions); len(acts) != 3 || acts[1][2] != want {
		t.Fatalf("actions %q, want three with Look at %q", cap.reqs[0].Actions, want)
	}
}

// A held ask waits on a foreman, so the phone stays quiet. When the hold
// ends and the same ask goes out with no hold, that is the edge the
// phone hears.
func TestAHeldAskPublishesOnlyWhenTheHoldEnds(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	held := blockedEvent("w1:p1")
	held.Held = &Held{By: "w1:p9", Task: "t1"}
	p.OnState(held, "auth fix")
	p.OnState(held, "auth fix")
	if cap.len() != 0 {
		t.Fatalf("%d publishes for a held ask, want 0", cap.len())
	}
	now = now.Add(2 * time.Minute)
	p.OnState(blockedEvent("w1:p1"), "auth fix")
	if cap.len() != 1 {
		t.Fatalf("%d publishes after the hold ended, want 1", cap.len())
	}
}

// A push says a block with no ask in the floor's own words.
func TestPushBlockWordsMatchTheFloor(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "words.json"))
	if err != nil {
		t.Fatal(err)
	}
	var w map[string]any
	if err := json.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	if w["blocked_by_gate"] != BlockedByGate || w["blocked_own_question"] != BlockedOwnQuestion {
		t.Fatalf("push words %q / %q differ from words.json", BlockedByGate, BlockedOwnQuestion)
	}
}

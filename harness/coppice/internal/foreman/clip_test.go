package foreman

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// step is one scripted model call: an answer, a stop reason and an error.
type step struct {
	text string
	stop string
	err  error
}

type stepCall struct {
	mu    sync.Mutex
	steps []step
	n     int
}

func (s *stepCall) call(_ context.Context, _, _ string) (string, CallUsage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.n
	s.n++
	if i >= len(s.steps) {
		i = len(s.steps) - 1
	}
	st := s.steps[i]
	return st.text, CallUsage{Stop: st.stop}, st.err
}

// ranFail is a model run that failed with detail on stderr.
func ranFail(detail string) error {
	return &CallError{Msg: "the compactor's model call failed: x", Detail: detail, Stderr: detail, Ran: true}
}

func TestAnEmptyAnswerClipsTheSourceAndWritesNoEmptyNode(t *testing.T) {
	sc := &stepCall{steps: []step{{text: " \n "}}}
	m := &ModelSummarizer{Call: sc.call}
	got, err := m.Compress(ctx, Message{Text: "the start\nof a long message " + strings.Repeat("z", 200)}, 64)
	if err != nil || !strings.HasPrefix(got, ClipMarker+"the start of a long message ") || len(got) != 64 {
		t.Fatalf("Compress = %q (%d), %v", got, len(got), err)
	}
	if sc.n != 1 {
		t.Fatalf("%d calls", sc.n)
	}
}

func TestARefusalStopClipsTheSourceEvenWithText(t *testing.T) {
	sc := &stepCall{steps: []step{{text: "I cannot help with that.", stop: "refusal"}}}
	m := &ModelSummarizer{Call: sc.call}
	got, err := m.Merge(ctx, NodeID{L: 1, I: 0}, "left side", "right side", 64)
	if err != nil || got != ClipMarker+"left side right side" {
		t.Fatalf("Merge = %q, %v", got, err)
	}
}

func TestARefusalThatExitsNonZeroDelaysThenClipsAtTheNextAsk(t *testing.T) {
	sc := &stepCall{steps: []step{{err: ranFail("I can't summarize this")}}}
	m := &ModelSummarizer{Call: sc.call}
	msg := Message{I: 4, Text: "source text"}
	// The first failure delays the node: there is no retry inside the ask.
	if got, err := m.Compress(ctx, msg, 64); err == nil || got != "" || sc.n != 1 {
		t.Fatalf("first failure: %q, %v after %d calls", got, err, sc.n)
	}
	// The chat asks again after its back-off, and the repeat clips.
	got, err := m.Compress(ctx, msg, 64)
	if err != nil || got != ClipMarker+"source text" || sc.n != 2 {
		t.Fatalf("second ask: %q, %v after %d calls", got, err, sc.n)
	}
}

func TestATransientFailureNeverClipsHoweverOftenItRepeats(t *testing.T) {
	for _, detail := range []string{
		"sprig: anthropic api 500: internal error",
		"sprig: anthropic api 502: bad gateway",
		"read tcp: connection reset by peer",
		"unexpected EOF",
		"net/http: TLS handshake timeout",
		"Client.Timeout exceeded while awaiting headers",
	} {
		sc := &stepCall{steps: []step{{err: ranFail(detail)}}}
		m := &ModelSummarizer{Call: sc.call}
		for i := 0; i < 4; i++ {
			if got, err := m.Compress(ctx, Message{I: 1, Text: "s"}, 64); err == nil || got != "" {
				t.Fatalf("%q round %d: Compress = %q, %v", detail, i, got, err)
			}
		}
	}
}

func TestA500ThenA502AtTwoAsksGivesTwoErrorsAndNoClip(t *testing.T) {
	sc := &stepCall{steps: []step{{err: ranFail("anthropic api 500: x")}, {err: ranFail("anthropic api 502: y")}}}
	m := &ModelSummarizer{Call: sc.call}
	for i := 0; i < 2; i++ {
		if got, err := m.Compress(ctx, Message{Text: "s"}, 64); err == nil || got != "" {
			t.Fatalf("ask %d: Compress = %q, %v", i, got, err)
		}
	}
	if sc.n != 2 {
		t.Fatalf("%d calls", sc.n)
	}
}

func TestAModelsOwnWordsNeverMakeAFailureTransient(t *testing.T) {
	// Phrases that are on the transient list sit in stdout (Detail) only,
	// in the model's text; stderr has the runner's own plain complaint. If
	// the compactor read Detail again, the node would never clip.
	words := "Geoff said: connection timed out, internal server error, connection reset by peer, unexpected EOF"
	sc := &stepCall{steps: []step{{err: &CallError{Msg: "the compactor's model call failed: I can't summarize this", Detail: "I can't summarize this\n" + words, Stderr: "I can't summarize this", Ran: true}}}}
	m := &ModelSummarizer{Call: sc.call}
	msg := Message{I: 7, Text: "source text"}
	if got, err := m.Compress(ctx, msg, 64); err == nil || got != "" {
		t.Fatalf("first ask: %q, %v", got, err)
	}
	if got, err := m.Compress(ctx, msg, 64); err != nil || got != ClipMarker+"source text" {
		t.Fatalf("second ask: %q, %v", got, err)
	}
}

func TestIsTransientMatchesWholeWordsOfTheRunnersOwnErrors(t *testing.T) {
	for _, no := range []string{"Geoff", "thereof", "the build timed out", "a server error in the code", "try again later"} {
		if IsTransient(no) {
			t.Errorf("%q is transient", no)
		}
	}
	for _, yes := range []string{"read tcp: unexpected EOF", "Get x: EOF", "anthropic api 503: x", "connection reset by peer", "net/http: TLS handshake timeout",
		"read tcp 1.2.3.4:5->6.7.8.9:443: read: connection timed out", "dial tcp: i/o timeout", "read tcp: read: connection reset by peer",
		"http2: server sent GOAWAY and closed the connection", "stream error: stream ID 3; INTERNAL_ERROR", "context deadline exceeded"} {
		if !IsTransient(yes) {
			t.Errorf("%q is not transient", yes)
		}
	}
}

func TestAStrikeIsClearedByALine(t *testing.T) {
	sc := &stepCall{steps: []step{{err: ranFail("odd")}, {text: "a good line"}, {err: ranFail("odd")}}}
	m := &ModelSummarizer{Call: sc.call}
	msg := Message{I: 2, Text: "source text"}
	_, _ = m.Compress(ctx, msg, 64)
	if got, err := m.Compress(ctx, msg, 64); err != nil || got != "a good line" {
		t.Fatalf("Compress = %q, %v", got, err)
	}
	if got, err := m.Compress(ctx, msg, 64); err == nil {
		t.Fatalf("a new failure after a line clipped at once: %q", got)
	}
}

func TestAnOutageOnlyDelaysAndNeverClips(t *testing.T) {
	for _, detail := range []string{"Claude AI usage limit reached|1791500000", "dial tcp: connection refused", "anthropic api 429: rate_limit_error"} {
		sc := &stepCall{steps: []step{{err: ranFail(detail)}}}
		m := &ModelSummarizer{Call: sc.call}
		got, err := m.Compress(ctx, Message{Text: "source text"}, 64)
		if err == nil || got != "" || sc.n != 1 {
			t.Fatalf("%q: Compress = %q, %v after %d calls", detail, got, err, sc.n)
		}
	}
}

func TestAnErrorThatIsNotAModelRunNeverClips(t *testing.T) {
	sc := &stepCall{steps: []step{{err: errFake}}}
	m := &ModelSummarizer{Call: sc.call}
	if got, err := m.Compress(ctx, Message{Text: "s"}, 64); err == nil || sc.n != 1 {
		t.Fatalf("Compress = %q, %v after %d calls", got, err, sc.n)
	}
}

func TestAnEmptyRetryAfterATooLongTryKeepsTheLongTry(t *testing.T) {
	sc := &stepCall{steps: []step{{text: strings.Repeat("q", 90)}, {text: ""}}}
	m := &ModelSummarizer{Call: sc.call}
	got, err := m.Compress(ctx, Message{Text: strings.Repeat("m", 200)}, 64)
	if err != nil || got != strings.Repeat("q", 64) {
		t.Fatalf("Compress = %q, %v", got, err)
	}
}

func TestAClippedNodeDoesNotBlockTheNodesAfterIt(t *testing.T) {
	clk := newClock()
	c := openT(t, t.TempDir(), nil, clk, nil)
	logLongs(t, c, 4)
	n := 0
	m := &ModelSummarizer{Call: func(_ context.Context, _, _ string) (string, CallUsage, error) {
		n++
		if n == 1 { // the first message's node
			return "", CallUsage{Stop: "refusal"}, nil
		}
		return "fine", CallUsage{}, nil
	}}
	if err := c.Drain(ctx, m, nil); err != nil {
		t.Fatal(err)
	}
	if c.view.Count != 4 {
		t.Fatalf("the view covers %d of 4 messages", c.view.Count)
	}
	first := c.nodes[NodeID{L: 0, I: 0}].Text
	if !strings.HasPrefix(first, ClipMarker) || len(first) > c.cfg.NodeBytes {
		t.Fatalf("node 0 = %q", first)
	}
	for id, nd := range c.nodes {
		if nd.Text == "" {
			t.Fatalf("node %s is empty", id.Name())
		}
	}
}

package voice

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"daisugi-verify/internal/pyjson"
)

// liveStream is a recording that can show its audio so far.
type liveStream struct {
	mu  sync.Mutex
	pcm []byte
}

func (s *liveStream) Start() error { return nil }
func (s *liveStream) Stop() error  { return nil }
func (s *liveStream) add(b []byte) {
	s.mu.Lock()
	s.pcm = append(s.pcm, b...)
	s.mu.Unlock()
}
func (s *liveStream) Snapshot() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.pcm...)
}
func (s *liveStream) Read(int) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.pcm
	s.pcm = nil
	return c, false, nil
}

// liveClient answers partials from a list of (text, seq), or with err.
type liveClient struct {
	mu       sync.Mutex
	partials []struct {
		text string
		seq  int
	}
	err      error
	asked    [][]byte
	seqs     []int
	finals   [][]byte
	finalTxt string
}

func (c *liveClient) Partial(wav []byte, seq int) (string, int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, wav)
	c.seqs = append(c.seqs, seq)
	if c.err != nil {
		return "", 0, c.err
	}
	p := c.partials[0]
	c.partials = c.partials[1:]
	return p.text, p.seq, nil
}

func (c *liveClient) Transcribe(wav []byte) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.finals = append(c.finals, wav)
	o := pyjson.NewObject()
	o.Set("text", c.finalTxt)
	return o, nil
}

func (c *liveClient) Deliver(string, string, string) (any, error) {
	o := pyjson.NewObject()
	o.Set("delivered", "preview")
	return o, nil
}

// liveRun drives one recording: start, then during() with the tick
// channel and the shown lines, then stop and quit. It answers every
// event in order: "show:" lines and "print:" lines.
func liveRun(t *testing.T, c Client, s *liveStream, during func(tick chan time.Time, shown <-chan string)) []string {
	t.Helper()
	var mu sync.Mutex
	var events []string
	shown := make(chan string, 16)
	tick := make(chan time.Time)
	keys := []string{" ", " ", "q"}
	n := 0
	live := Live{
		Show: func(line string) {
			mu.Lock()
			events = append(events, "show:"+line)
			mu.Unlock()
			shown <- line
		},
		Ticks: func() (<-chan time.Time, func()) { return tick, func() {} },
	}
	_, err := RunPTTLive("w1:p1", c,
		func() (string, bool) {
			if n >= len(keys) {
				return "", false
			}
			k := keys[n]
			n++
			if n == 2 {
				<-shown // Listening
				during(tick, shown)
			}
			return k, true
		},
		func() (Stream, error) { return s, nil },
		1600, func(line string) {
			mu.Lock()
			events = append(events, "print:"+line)
			mu.Unlock()
		}, live)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), events...)
}

func TestPTTShowsTheTextSoFarAndThenTheFinal(t *testing.T) {
	c := &liveClient{finalTxt: "run the tests please"}
	c.partials = append(c.partials, struct {
		text string
		seq  int
	}{"run the", 1}, struct {
		text string
		seq  int
	}{"run the tests", 2})
	s := &liveStream{}
	events := liveRun(t, c, s, func(tick chan time.Time, shown <-chan string) {
		s.add(bytes.Repeat([]byte{1, 0}, 800))
		tick <- time.Now()
		<-shown
		s.add(bytes.Repeat([]byte{2, 0}, 800))
		tick <- time.Now()
		<-shown
	})
	want := []string{"show:" + LiveListening, "show:run the", "show:run the tests", "show:", "print:run the tests please", "print:preview"}
	if strings.Join(events, "|") != strings.Join(want, "|") {
		t.Fatalf("events:\n%q\nwant:\n%q", events, want)
	}
	if len(c.asked) != 2 || len(c.asked[1]) <= len(c.asked[0]) {
		t.Fatalf("each partial must carry the whole recording so far")
	}
	if c.seqs[0] != 1 || c.seqs[1] != 2 {
		t.Fatalf("seqs %v", c.seqs)
	}
	if len(c.finals) != 1 || len(c.finals[0]) != len(c.asked[1]) {
		t.Fatalf("the final clip must be the whole recording")
	}
}

func TestPTTDropsAPartialOlderThanTheOneShown(t *testing.T) {
	c := &liveClient{finalTxt: "x"}
	c.partials = append(c.partials, struct {
		text string
		seq  int
	}{"newer", 5}, struct {
		text string
		seq  int
	}{"older", 3}, struct {
		text string
		seq  int
	}{"newest", 6})
	s := &liveStream{}
	events := liveRun(t, c, s, func(tick chan time.Time, shown <-chan string) {
		s.add([]byte{1, 0})
		tick <- time.Now()
		<-shown
		tick <- time.Now()
		tick <- time.Now()
		<-shown
	})
	for _, e := range events {
		if e == "show:older" {
			t.Fatalf("an older partial was shown: %q", events)
		}
	}
}

// An older voice server has no /partial. The line says so once, no more
// partials go out, and the clip works as before.
func TestPTTWithAnOlderServerSaysSoOnceAndGoesOn(t *testing.T) {
	c := &liveClient{finalTxt: "as before", err: &ServerError{Msg: "This server answers /health, /transcribe, and /deliver.", Status: 404}}
	s := &liveStream{}
	events := liveRun(t, c, s, func(tick chan time.Time, shown <-chan string) {
		s.add([]byte{1, 0})
		tick <- time.Now()
		if got := <-shown; got != LiveTextOld {
			t.Errorf("shown %q", got)
		}
		select {
		case tick <- time.Now():
			t.Error("a tick was taken after the 404")
		case <-time.After(100 * time.Millisecond):
		}
	})
	if len(c.asked) != 1 {
		t.Fatalf("partials after a 404: %d", len(c.asked))
	}
	if events[len(events)-2] != "print:as before" {
		t.Fatalf("%q", events)
	}
}

// Any other failure says nothing: the next partial is on its way.
func TestPTTPartialFailureIsQuiet(t *testing.T) {
	c := &liveClient{finalTxt: "fine", err: &ServerError{Msg: "busy", Busy: true, Status: 409}}
	s := &liveStream{}
	events := liveRun(t, c, s, func(tick chan time.Time, shown <-chan string) {
		s.add([]byte{1, 0})
		tick <- time.Now()
		tick <- time.Now()
		// Stop reads the stream first, which empties it. Wait for the
		// second partial to go out before Stop, or it finds no audio.
		for deadline := time.Now().Add(5 * time.Second); ; {
			c.mu.Lock()
			n := len(c.asked)
			c.mu.Unlock()
			if n >= 2 || time.Now().After(deadline) {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
	for _, e := range events {
		if strings.Contains(e, "busy") {
			t.Fatalf("a failed partial was shown: %q", events)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.asked) != 2 {
		t.Fatalf("asked %d", len(c.asked))
	}
}

// No partial goes out before there is any audio.
func TestPTTAsksNothingBeforeThereIsAudio(t *testing.T) {
	c := &liveClient{finalTxt: "x"}
	s := &liveStream{}
	liveRun(t, c, s, func(tick chan time.Time, shown <-chan string) {
		tick <- time.Now()
		tick <- time.Now()
	})
	if len(c.asked) != 0 {
		t.Fatalf("asked %d", len(c.asked))
	}
}

func TestTailLineKeepsTheEndThatFits(t *testing.T) {
	if got := TailLine("short", 20); got != "short" {
		t.Fatal(got)
	}
	got := TailLine("one two three four five six seven eight nine ten", 20)
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "nine ten") || len([]rune(got)) > 19 {
		t.Fatalf("%q", got)
	}
	if got := TailLine("a\nb\tc", 20); got != "a b c" {
		t.Fatalf("control characters must not reach the terminal: %q", got)
	}
}

func TestHTTPClientPartialReadsTextAndSeq(t *testing.T) {
	_, base, _ := longServer(t)
	data := t.TempDir()
	os.MkdirAll(filepath.Join(data, "coppice", "web"), 0o700)
	os.WriteFile(TokenFile(data), []byte("tok\n"), 0o600)
	c, err := NewHTTPClient(base, data, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	text, seq, err := c.Partial(toneWav(1, nil), 3)
	if err != nil || text != "window 1" || seq != 3 {
		t.Fatalf("%q %d %v", text, seq, err)
	}
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"error": "not_found", "message": "This server answers /health, /transcribe, and /deliver."}`))
	}))
	defer old.Close()
	c, _ = NewHTTPClient(old.URL, data, 5*time.Second)
	_, _, err = c.Partial(toneWav(1, nil), 1)
	var se *ServerError
	if !errors.As(err, &se) || se.Status != 404 {
		t.Fatalf("%v", err)
	}
}

// A refused token must not be spent on partials: three failures ban the
// address, and the final clip would meet "wait 60 seconds" instead of the
// sentence that names the fix. A refusal ends the partials quietly.
func TestPTTStopsPartialsWhenTheTokenIsRefused(t *testing.T) {
	for _, status := range []int{401, 403, 429} {
		c := &liveClient{finalTxt: "x", err: &ServerError{Msg: "refused", Status: status}}
		s := &liveStream{}
		events := liveRun(t, c, s, func(tick chan time.Time, shown <-chan string) {
			s.add([]byte{1, 0})
			tick <- time.Now()
			// A line that says Listening and never changes would look live.
			if got := <-shown; got != "" {
				t.Errorf("%d: the line must clear on a refusal, got %q", status, got)
			}
			select {
			case tick <- time.Now():
				t.Errorf("%d: a tick was taken after the refusal", status)
			case <-time.After(100 * time.Millisecond):
			}
		})
		if len(c.asked) != 1 {
			t.Fatalf("%d: partials after a refusal: %d", status, len(c.asked))
		}
		for _, e := range events {
			if strings.Contains(e, "refused") {
				t.Fatalf("%d: the refusal was shown: %q", status, events)
			}
		}
	}
}

// slowPartialClient holds its partial until release closes, and notes
// whether the final clip came while the partial was still out.
type slowPartialClient struct {
	liveClient
	inFlight chan struct{}
	release  chan struct{}
	early    bool
	lastSeen []byte
}

func (c *slowPartialClient) Partial(wav []byte, seq int) (string, int, error) {
	close(c.inFlight)
	<-c.release
	return "late words", seq, nil
}

func (c *slowPartialClient) Transcribe(wav []byte) (any, error) {
	early := true
	select {
	case <-c.release:
		early = false
	default:
	}
	c.mu.Lock()
	c.early, c.lastSeen = early, wav
	c.mu.Unlock()
	return c.liveClient.Transcribe(wav)
}

// Stop sends the clip at once: it never waits for a partial still out,
// and a late partial never draws over the final.
func TestPTTStopSendsTheClipWithoutWaitingForAPartial(t *testing.T) {
	c := &slowPartialClient{inFlight: make(chan struct{}), release: make(chan struct{})}
	c.finalTxt = "the final"
	s := &liveStream{}
	var events []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		events = liveRun(t, c, s, func(tick chan time.Time, shown <-chan string) {
			s.add(bytes.Repeat([]byte{1, 0}, 800))
			tick <- time.Now()
			<-c.inFlight
		})
	}()
	deadline := time.After(5 * time.Second)
	for {
		c.mu.Lock()
		sent := len(c.finals) > 0
		c.mu.Unlock()
		if sent {
			break
		}
		select {
		case <-deadline:
			close(c.release)
			<-done
			t.Fatal("the final clip waited for the partial still out")
		case <-time.After(10 * time.Millisecond):
		}
	}
	close(c.release)
	<-done
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.early {
		t.Fatal("the final clip was sent only after the partial came back")
	}
	if len(c.lastSeen) != 44+1600 {
		t.Fatalf("the final clip holds %d bytes, want the recording as it stopped", len(c.lastSeen))
	}
	for _, e := range events {
		if e == "show:late words" {
			t.Fatalf("a late partial drew over the final: %q", events)
		}
	}
}

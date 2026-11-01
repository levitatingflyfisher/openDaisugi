package voice

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"daisugi-verify/internal/pyjson"
)

type fakeStream struct{ chunk []byte }

func (s *fakeStream) Start() error { return nil }
func (s *fakeStream) Stop() error  { return nil }
func (s *fakeStream) Read(int) ([]byte, bool, error) {
	c := s.chunk
	s.chunk = nil
	return c, false, nil
}

// busyClient refuses its first Transcribe as busy, or with a plain error.
type busyClient struct {
	busy  bool
	calls [][]byte
}

func (c *busyClient) Transcribe(wav []byte) (any, error) {
	c.calls = append(c.calls, wav)
	if len(c.calls) == 1 {
		return nil, &ServerError{Msg: "still working", Busy: c.busy}
	}
	return pyjson.NewObject(), nil
}
func (c *busyClient) Deliver(string, string, string) (any, error) { return nil, nil }

func runTwoClips(t *testing.T, c Client) []string {
	t.Helper()
	streams := []Stream{&fakeStream{chunk: bytes.Repeat([]byte{1, 0}, 400)}, &fakeStream{chunk: bytes.Repeat([]byte{2, 0}, 400)}}
	keys := []string{" ", " ", " ", " ", "q"}
	var said []string
	_, err := RunPTT("w1:p1", c,
		func() (string, bool) {
			if len(keys) == 0 {
				return "", false
			}
			k := keys[0]
			keys = keys[1:]
			return k, true
		},
		func() (Stream, error) { s := streams[0]; streams = streams[1:]; return s, nil },
		1600, func(s string) { said = append(said, s) })
	if err != nil {
		t.Fatal(err)
	}
	return said
}

// A clip the busy server refused is kept and goes out with the next one.
func TestPTTKeepsAClipTheBusyServerRefused(t *testing.T) {
	c := &busyClient{busy: true}
	said := runTwoClips(t, c)
	if len(c.calls) != 2 {
		t.Fatalf("calls: %d", len(c.calls))
	}
	if len(c.calls[1]) <= len(c.calls[0]) || !bytes.Contains(c.calls[1], c.calls[0][44:]) {
		t.Fatalf("the second post must hold the first clip's audio and the new audio")
	}
	found := false
	for _, s := range said {
		found = found || s == "Your recording is kept and goes out with the next one."
	}
	if !found {
		t.Fatalf("no kept line: %v", said)
	}
}

// Any other error keeps the clip too.
func TestPTTKeepsAClipAfterAnErrorThatIsNotBusy(t *testing.T) {
	c := &busyClient{}
	said := runTwoClips(t, c)
	if len(c.calls) != 2 || len(c.calls[1]) <= len(c.calls[0]) {
		t.Fatalf("second post must hold the first clip too: %d %d", len(c.calls[0]), len(c.calls[1]))
	}
	found := false
	for _, s := range said {
		found = found || s == "Your recording is kept and goes out with the next one."
	}
	if !found {
		t.Fatalf("no kept line: %v", said)
	}
}

// The HTTP client marks a 409 busy reply, and only that one.
func TestTheHTTPClientMarksABusyReply(t *testing.T) {
	status, body := 409, `{"error":"busy","message":"still working"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	dir := t.TempDir()
	tf := TokenFile(dir)
	if err := os.MkdirAll(filepath.Dir(tf), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tf, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewHTTPClient(srv.URL, dir, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var se *ServerError
	_, err = c.Transcribe([]byte("x"))
	if !errors.As(err, &se) || !se.Busy || se.Msg != "still working" {
		t.Fatalf("409 busy: %#v", err)
	}
	status, body = 500, `{"error":"engine","message":"broke"}`
	_, err = c.Transcribe([]byte("x"))
	if !errors.As(err, &se) || se.Busy {
		t.Fatalf("500: %#v", err)
	}
}

// busyThenFails refuses the first post as busy and every later one with a
// plain error.
type busyThenFails struct{ calls [][]byte }

func (c *busyThenFails) Transcribe(wav []byte) (any, error) {
	c.calls = append(c.calls, wav)
	return nil, &ServerError{Msg: "x", Busy: len(c.calls) == 1}
}
func (c *busyThenFails) Deliver(string, string, string) (any, error) { return nil, nil }

// A kept clip that gets a plain error stays kept, and one still unsent at
// quit is named.
func TestPTTKeepsAJoinedClipAfterAPlainErrorAndNamesItOnQuit(t *testing.T) {
	c := &busyThenFails{}
	said := runTwoClips(t, c)
	kept, quit := 0, false
	for _, s := range said {
		if s == "Your recording is kept and goes out with the next one." {
			kept++
		}
		quit = quit || s == "A kept recording was not sent."
	}
	if kept != 2 || !quit {
		t.Fatalf("kept %d quit %v: %v", kept, quit, said)
	}
}

// A timeout and a reply that is not JSON are server errors too, so a clip that
// meets them is kept.
func TestTheHTTPClientTurnsATimeoutAndBadJSONIntoServerErrors(t *testing.T) {
	body := "<html>oops"
	slow := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if slow {
			time.Sleep(300 * time.Millisecond)
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	dir := t.TempDir()
	tf := TokenFile(dir)
	if err := os.MkdirAll(filepath.Dir(tf), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tf, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewHTTPClient(srv.URL, dir, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	var se *ServerError
	_, err = c.Transcribe([]byte("x"))
	if !errors.As(err, &se) || se.Busy || se.Msg != "The voice server sent an answer that is not JSON." {
		t.Fatalf("bad json: %#v", err)
	}
	_, err = c.Deliver("w1:p1", "hi", "preview")
	if !errors.As(err, &se) || se.Msg != "The voice server sent an answer that is not JSON." {
		t.Fatalf("bad json on deliver: %#v", err)
	}
	slow = true
	_, err = c.Transcribe([]byte("x"))
	if !errors.As(err, &se) || se.Busy || se.Msg != "The voice server did not answer in time." {
		t.Fatalf("timeout: %#v", err)
	}
}

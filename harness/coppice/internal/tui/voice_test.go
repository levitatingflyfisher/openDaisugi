package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/server"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

// fakeVoice stands in for daisugi voice serve on loopback. It answers
// /health, and /transcribe with text when the bearer is right.
type fakeVoice struct {
	mu    sync.Mutex
	posts []*http.Request
	text  string
}

func newFakeVoice(t *testing.T, token, text string) (*fakeVoice, string) {
	t.Helper()
	f := &fakeVoice{text: text}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Write([]byte(`{"ok":true}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		f.mu.Lock()
		f.posts = append(f.posts, r)
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"bad_token","message":"This request needs a bearer token."}`))
			return
		}
		b, _ := json.Marshal(map[string]string{"text": f.text})
		w.Write(b)
	}))
	t.Cleanup(ts.Close)
	return f, ts.URL
}

func (f *fakeVoice) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts)
}

// newVoiceTestServer is a coppice server whose voice is the one at url.
// It never starts daisugi.
func newVoiceTestServer(t *testing.T, url, tokenFile string) *testServer {
	t.Helper()
	toolchain.RequireOrSkip(t)
	dir := t.TempDir()
	sock := filepath.Join(dir, "server.sock")
	s, err := server.New(server.Config{SocketPath: sock, DataDir: dir,
		Voice: server.VoiceConfig{URL: url, TokenFile: tokenFile}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()
	t.Cleanup(func() { _ = s.Close() })
	return &testServer{sock: sock, srv: s}
}

// fakeRecorder makes lookPath find a pw-record that writes a clip of
// silence to its last argument and waits for an interrupt. It returns the
// file the fake writes its arguments to, and one it touches when it is
// interrupted.
func fakeRecorder(t *testing.T) (argsFile, stopped string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	stopped = filepath.Join(dir, "stopped")
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" > %q
for a; do last=$a; done
head -c 20000 /dev/zero > "$last"
trap 'touch %q; exit 0' INT
while :; do sleep 0.05; done
`, argsFile, stopped)
	bin := filepath.Join(dir, "pw-record")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	was := lookPath
	lookPath = func(name string) (string, error) {
		if name == "pw-record" {
			return bin, nil
		}
		return "", errors.New("not found")
	}
	t.Cleanup(func() { lookPath = was })
	return argsFile, stopped
}

// flat is a frame's text with its escapes gone and its runs of spaces
// and line ends made one space, so a line the floor wrapped reads whole.
func flat(frame string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(stripSGR(frame), "\x1b[K", "")), " ")
}

// text is what the floor wrote so far.
func (w *signalWriter) text() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// voiceFloor runs the floor at cols columns with keys from a pipe, a data
// directory, and the default talk key.
func voiceFloor(t *testing.T, s *testServer, cols int) (*io.PipeWriter, *signalWriter, string, chan error) {
	t.Helper()
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	out := newSignalWriter()
	data := t.TempDir()
	done := make(chan error, 1)
	go func() {
		done <- Run(Options{Socket: s.Socket(), In: pr, Out: out, DataDir: data,
			Size: func() (int, int) { return cols, 24 }, Cwd: t.TempDir(), Default: "claude"})
	}()
	<-out.first
	return pw, out, data, done
}

func tokenFile(t *testing.T, tok string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The failure this names: the TUI had no voice. A press starts a clip, a
// second press stops it, and the text lands in the selected agent's input
// line with no Enter, so the owner reads it first.
func TestTheTalkKeyTypesWhatWasSaidIntoTheSelectedAgent(t *testing.T) {
	argsFile, stopped := fakeRecorder(t)
	voice, url := newFakeVoice(t, "vtok", "tidy the docs folder")
	s := newVoiceTestServer(t, url, tokenFile(t, "vtok"))
	id := createShellPane(t, s.Socket(), "sh", "-c", "echo ready; cat")
	pw, out, data, done := voiceFloor(t, s, 80)

	io.WriteString(pw, "\x1c")
	if !waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(stripSGR(lastFrame(out.text())), "recording… press ctrl-\\ to stop")
	}) {
		t.Fatalf("no recording line: %q", stripSGR(lastFrame(out.text())))
	}
	if !strings.Contains(out.text(), kittyQuery) {
		t.Fatal("the first clip did not ask the terminal about key release")
	}
	time.Sleep(minToggle + 100*time.Millisecond)
	io.WriteString(pw, "\x1c")
	text := readUntil(t, s.Socket(), id, "tidy the docs folder")
	if strings.Count(text, "tidy the docs folder") != 1 {
		t.Fatalf("the text was sent with Enter: %q", text)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(stripSGR(lastFrame(out.text())), "Typed into "+id+". Read it, then press Enter there.")
	}) {
		t.Fatalf("no typed line: %q", stripSGR(lastFrame(out.text())))
	}
	if voice.count() != 1 {
		t.Fatalf("%d posts, want 1", voice.count())
	}
	voice.mu.Lock()
	post := voice.posts[0]
	voice.mu.Unlock()
	if post.Header.Get("Content-Type") != "audio/wav" {
		t.Fatalf("Content-Type %q", post.Header.Get("Content-Type"))
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "--rate 16000 --channels 1") ||
		!strings.HasPrefix(strings.Fields(string(args))[len(strings.Fields(string(args)))-1], filepath.Join(data, "voice")) {
		t.Fatalf("recorder args %q", args)
	}
	if _, err := os.Stat(stopped); err != nil {
		t.Fatal("the recorder was not interrupted, so its WAV is not finished")
	}
	left, _ := filepath.Glob(filepath.Join(data, "voice", "clip-*.wav"))
	if len(left) != 0 {
		t.Fatalf("clips left behind: %v", left)
	}
	pw.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// The failure this names: where the terminal reports key release, the
// clip records while the key is held, and the flags pushed for that come
// off on the release.
func TestHoldingTheTalkKeyInAWindowRecordsUntilItIsLetGo(t *testing.T) {
	fakeRecorder(t)
	_, url := newFakeVoice(t, "vtok", "run the tests")
	s := newVoiceTestServer(t, url, tokenFile(t, "vtok"))
	id := createShellPane(t, s.Socket(), "sh", "-c", "echo ready; cat")
	pw, out, _, done := voiceFloor(t, s, 140)
	if !waitFor(t, 5*time.Second, func() bool { return strings.Contains(lastFrame(out.text()), "typing here") }) {
		t.Fatal("the window never took the keys")
	}

	io.WriteString(pw, "\x1c")
	if !waitFor(t, 5*time.Second, func() bool { return strings.Contains(out.text(), kittyQuery) }) {
		t.Fatal("no query")
	}
	io.WriteString(pw, "\x1b[?0u")
	if !waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(flat(lastFrame(out.text())), "recording… let go of ctrl-\\, or press it again, to stop")
	}) {
		t.Fatalf("the answer did not turn on hold: %q", stripSGR(lastFrame(out.text())))
	}
	if !strings.Contains(out.text(), kittyPush) {
		t.Fatal("the flags were not pushed")
	}
	// Repeats while held, then the release.
	io.WriteString(pw, "\x1b[92;5:2u\x1b[92;5:2u")
	time.Sleep(100 * time.Millisecond)
	io.WriteString(pw, "\x1b[92;1:3u")
	text := readUntil(t, s.Socket(), id, "run the tests")
	if strings.Contains(text, "92;") || strings.Contains(text, "[?0u") {
		t.Fatalf("a key report reached the agent: %q", text)
	}
	if !strings.Contains(out.text(), kittyPop+deviceQuery) {
		t.Fatal("the flags were not popped, with a device query after the pop")
	}
	// A report the terminal sent before it saw the pop, and a key typed
	// then, are held back until the terminal answers the device query.
	io.WriteString(pw, "\x1b[57442;5:3uzz")
	time.Sleep(100 * time.Millisecond)
	io.WriteString(pw, "\x1b[?62;22c")
	io.WriteString(pw, "hello")
	// After the answer, a stray report on the typing path is dropped too.
	io.WriteString(pw, "\x1b[57442;5:3u!")
	text = readUntil(t, s.Socket(), id, "run the testshello!")
	for _, bad := range []string{"57442", "zz", "62;22", "[?"} {
		if strings.Contains(text, bad) {
			t.Fatalf("%q reached the agent: %q", bad, text)
		}
	}
	pw.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestEscThrowsAClipAway(t *testing.T) {
	_, stopped := fakeRecorder(t)
	voice, url := newFakeVoice(t, "vtok", "never")
	s := newVoiceTestServer(t, url, tokenFile(t, "vtok"))
	createShellPane(t, s.Socket(), "sh", "-c", "echo ready; cat")
	pw, out, data, done := voiceFloor(t, s, 80)
	io.WriteString(pw, "\x1c")
	if !waitFor(t, 5*time.Second, func() bool { return strings.Contains(lastFrame(out.text()), "recording") }) {
		t.Fatal("no recording")
	}
	io.WriteString(pw, "\x1b")
	if !waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(stripSGR(lastFrame(out.text())), "The clip was thrown away.")
	}) {
		t.Fatalf("no thrown away line: %q", stripSGR(lastFrame(out.text())))
	}
	// The recorder stops on the side, so the floor never waits for it.
	if !waitFor(t, 5*time.Second, func() bool { _, err := os.Stat(stopped); return err == nil }) {
		t.Fatal("the recorder still runs")
	}
	var left []string
	waitFor(t, 5*time.Second, func() bool {
		left, _ = filepath.Glob(filepath.Join(data, "voice", "clip-*.wav"))
		return len(left) == 0
	})
	if len(left) != 0 || voice.count() != 0 {
		t.Fatalf("clips %v, posts %d", left, voice.count())
	}
	pw.Close()
	<-done
}

// The failure this names: with no recorder or no voice server the talk key
// did nothing a person could see. One line says why and what to do.
func TestTheTalkKeySaysWhyWhenItCannotRecord(t *testing.T) {
	_, url := newFakeVoice(t, "vtok", "never")
	s := newVoiceTestServer(t, url, tokenFile(t, "vtok"))
	createShellPane(t, s.Socket(), "sh", "-c", "echo ready; cat")
	was := lookPath
	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	t.Cleanup(func() { lookPath = was })
	pw, out, _, done := voiceFloor(t, s, 80)
	io.WriteString(pw, "\x1c")
	want := "Voice needs a recorder: pw-record, parecord or arecord."
	if !waitFor(t, 5*time.Second, func() bool { return strings.Contains(flat(lastFrame(out.text())), want) }) {
		t.Fatalf("no recorder line: %q", flat(lastFrame(out.text())))
	}
	if strings.Contains(out.text(), kittyQuery) {
		t.Fatal("asked the terminal for a clip that never started")
	}
	pw.Close()
	<-done

	dead := newVoiceTestServer(t, "http://127.0.0.1:1", tokenFile(t, "vtok"))
	createShellPane(t, dead.Socket(), "sh", "-c", "echo ready; cat")
	fakeRecorder(t)
	pw, out, _, done = voiceFloor(t, dead, 80)
	io.WriteString(pw, "\x1c")
	want = "The voice server at http://127.0.0.1:1 does not answer. Start it on that machine, then press ctrl-\\ again."
	if !waitFor(t, 5*time.Second, func() bool { return strings.Contains(flat(lastFrame(out.text())), want) }) {
		t.Fatalf("no voice-down line: %q", flat(lastFrame(out.text())))
	}
	pw.Close()
	<-done
}

// The failure this names: a floor that closed while a clip was being heard
// left the clip on disk. The next clip deletes old leftovers, never a
// clip still in use.
func TestANewClipDeletesOldLeftoverClips(t *testing.T) {
	fakeRecorder(t)
	dir := t.TempDir()
	voiceDir := filepath.Join(dir, "voice")
	os.MkdirAll(voiceDir, 0o700)
	old := filepath.Join(voiceDir, "clip-old.wav")
	fresh := filepath.Join(voiceDir, "clip-fresh.wav")
	os.WriteFile(old, []byte("x"), 0o600)
	os.WriteFile(fresh, []byte("x"), 0o600)
	past := time.Now().Add(-staleClip - time.Minute)
	os.Chtimes(old, past, past)
	c, err := startClip(dir)
	if err != nil {
		t.Fatal(err)
	}
	c.discard()
	if _, err := os.Stat(old); err == nil {
		t.Fatal("an old leftover clip was kept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("a recent clip was deleted")
	}
}

func TestCleanTranscriptKeepsOnlyPrintableText(t *testing.T) {
	cases := map[string]string{
		"a\r\nb\x1b[31mc":          "a b[31mc",
		"one\ttwo":                 "one two",
		"  hi\x00\x07\x7f there  ": "hi there",
		"café \u009bx\u0085y":      "café xy",
		"\n\n":                     "",
		"bad \xff byte":            "bad byte",
	}
	for in, want := range cases {
		if got := cleanTranscript(in); got != want {
			t.Fatalf("cleanTranscript(%q) = %q, want %q", in, got, want)
		}
	}
}

// The failure this names: a transcript is typed into a pty. A newline in
// it is Enter, and an escape drives the agent's screen, so neither may
// reach the agent. The pane prints each byte it gets in hex.
func TestATranscriptWithControlBytesReachesTheAgentAsPlainText(t *testing.T) {
	fakeRecorder(t)
	_, url := newFakeVoice(t, "vtok", "a\r\nb\x1b[31mc")
	s := newVoiceTestServer(t, url, tokenFile(t, "vtok"))
	id := createShellPane(t, s.Socket(), "sh", "-c",
		"stty raw -echo; echo ready; while :; do dd bs=1 count=1 2>/dev/null | od -An -tx1; done")
	pw, _, _, done := voiceFloor(t, s, 80)
	io.WriteString(pw, "\x1c")
	time.Sleep(minToggle + 100*time.Millisecond)
	io.WriteString(pw, "\x1c")
	readUntil(t, s.Socket(), id, "63") // c, the last byte
	res := rawCall(t, s.Socket(), "pane.read", map[string]any{"pane": id})
	text, _ := res["text"].(string)
	for _, f := range strings.Fields(text) {
		switch f {
		case "0d", "0a", "1b", "09":
			t.Fatalf("the agent got byte %s: %q", f, text)
		}
	}
	pw.Close()
	<-done
}

// The failure this names: asking the server whether voice can run took up
// to ten seconds on the floor's own loop, and the floor froze. It asks on
// the side now, and the floor keeps drawing and taking keys meanwhile.
func TestTheFloorKeepsWorkingWhileItAsksAboutVoice(t *testing.T) {
	fakeRecorder(t)
	_, url := newFakeVoice(t, "vtok", "later")
	s := newVoiceTestServer(t, url, tokenFile(t, "vtok"))
	createShellPane(t, s.Socket(), "sh", "-c", "echo ready; cat")
	release := make(chan struct{})
	was := askVoice
	askVoice = func(socket string) (*voiceTarget, string) {
		<-release
		return was(socket)
	}
	t.Cleanup(func() { askVoice = was })
	pw, out, _, done := voiceFloor(t, s, 80)
	io.WriteString(pw, "\x1c")
	if !waitFor(t, 5*time.Second, func() bool { return strings.Contains(flat(lastFrame(out.text())), "Checking voice") }) {
		t.Fatalf("no checking line: %q", flat(lastFrame(out.text())))
	}
	io.WriteString(pw, "\x14still here")
	if !waitFor(t, 5*time.Second, func() bool { return strings.Contains(flat(lastFrame(out.text())), "still here") }) {
		close(release)
		t.Fatalf("the floor froze while it asked: %q", flat(lastFrame(out.text())))
	}
	close(release)
	if !waitFor(t, 5*time.Second, func() bool { return strings.Contains(flat(lastFrame(out.text())), "recording") }) {
		t.Fatalf("no recording once the answer came: %q", flat(lastFrame(out.text())))
	}
	pw.Close()
	<-done
}

func TestTheFooterNamesTheTalkKeyWhenThereIsRoom(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", State: "working"}}}
	if got := strings.Join(footerLines(m, 200), ""); !strings.Contains(got, "ctrl-\\ speak") {
		t.Fatalf("200 columns: %q", got)
	}
	if n := len(footerLines(m, 140)); n != 1 {
		t.Fatalf("the talk key cost a footer line at 140 columns: %d lines", n)
	}
	m.Talk = 0x07
	if got := strings.Join(footerLines(m, 200), ""); !strings.Contains(got, "ctrl-g speak") {
		t.Fatalf("a set talk key is not named: %q", got)
	}
	m.Typing = "a"
	if got := footerText(m); !strings.HasSuffix(got, "ctrl-g speak") {
		t.Fatalf("typing footer %q", got)
	}
}

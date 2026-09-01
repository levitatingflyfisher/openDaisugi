package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestFakeDaisugiProcess is not a test. It is the daisugi a fake script on
// PATH runs, in this test binary, when COPPICE_FAKE_DAISUGI names a mode.
// Tests never start the real voice server.
func TestFakeDaisugiProcess(t *testing.T) {
	mode := os.Getenv("COPPICE_FAKE_DAISUGI")
	if mode == "" {
		t.Skip("only runs as the fake daisugi")
	}
	var args []string
	for i, a := range os.Args {
		if a == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	os.Exit(fakeDaisugi(mode, args))
}

// fakeDaisugi acts out daisugi voice serve in one of three modes: ok
// serves /health and /transcribe, nohelp has no voice command, and
// noextra fails the way a daisugi without its voice extra does.
func fakeDaisugi(mode string, args []string) int {
	if p := os.Getenv("COPPICE_FAKE_DAISUGI_ARGS"); p != "" {
		f, _ := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		fmt.Fprintln(f, strings.Join(args, " "))
		f.Close()
	}
	if len(args) < 2 || args[0] != "voice" || args[1] != "serve" {
		return 2
	}
	for _, a := range args {
		if a == "--help" {
			if mode == "nohelp" {
				return 2
			}
			return 0
		}
	}
	if mode == "noextra" {
		fmt.Fprintln(os.Stderr, "faster-whisper is not installed. Install it with: pip install 'opendaisugi[voice]'")
		return 3
	}
	flag := func(name string) string {
		for i, a := range args {
			if a == name && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	ln, err := net.Listen("tcp", flag("--host")+":"+flag("--port"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	tokenFile := flag("--token-file")
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/transcribe", func(w http.ResponseWriter, r *http.Request) {
		tok, _ := os.ReadFile(tokenFile)
		if r.Header.Get("Authorization") != "Bearer "+strings.TrimSpace(string(tok)) {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"bad_token","message":"Bad token."}`))
			return
		}
		w.Write([]byte(`{"text":"hello from the fake"}`))
	})
	_ = http.Serve(ln, mux)
	return 0
}

// fakeDaisugiOnPath puts a daisugi script first on PATH that runs this
// test binary as the fake daisugi in mode. It returns the file the fake
// writes each command line to.
func fakeDaisugiOnPath(t *testing.T, mode string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run='^TestFakeDaisugiProcess$' -- \"$@\"\n", exe)
	if err := os.WriteFile(filepath.Join(dir, "daisugi"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("COPPICE_FAKE_DAISUGI", mode)
	argsFile := filepath.Join(dir, "args")
	t.Setenv("COPPICE_FAKE_DAISUGI_ARGS", argsFile)
	return argsFile
}

// newVoiceServer is a test server whose voice checks for health often.
func newVoiceServer(t *testing.T, vc VoiceConfig) *Server {
	t.Helper()
	dir := t.TempDir()
	s, err := New(Config{SocketPath: filepath.Join(dir, "server.sock"), DataDir: dir, Voice: vc})
	if err != nil {
		t.Fatal(err)
	}
	s.voice.every = 20 * time.Millisecond
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// waitVoice polls voice.status until its state is want.
func waitVoice(t *testing.T, s *Server, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		st := s.voice.status()
		if st["state"] == want {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("voice never reached %s: %v", want, st)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestANewServerStartsNoVoiceUntilAsked(t *testing.T) {
	fakeDaisugiOnPath(t, "ok")
	s := newTestServer(t)
	time.Sleep(100 * time.Millisecond)
	st := s.voice.status()
	if st["state"] != "idle" || st["ready"] != false {
		t.Fatalf("status = %v, want idle", st)
	}
	if st["reason"] == "" {
		t.Fatalf("an idle voice must say why: %v", st)
	}
}

func TestTheServerStartsTheVoiceServerOnLoopbackAndStopsItWithItself(t *testing.T) {
	argsFile := fakeDaisugiOnPath(t, "ok")
	s := newVoiceServer(t, VoiceConfig{Args: []string{"--data-dir", "/somewhere"}})
	s.StartVoice()
	st := waitVoice(t, s, "ready")
	url, _ := st["url"].(string)
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Fatalf("url = %q, want loopback", url)
	}
	if !answers(url, time.Second) {
		t.Fatal("/health does not answer")
	}
	tokenFile := filepath.Join(s.cfg.DataDir, "voice", "token")
	if st["token_file"] != tokenFile {
		t.Fatalf("token_file = %v, want %s", st["token_file"], tokenFile)
	}
	fi, err := os.Stat(tokenFile)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file %v %v, want mode 0600", fi, err)
	}
	b, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(b), "voice serve --host 127.0.0.1 --port ") ||
		!strings.Contains(string(b), "--token-file "+tokenFile+" --data-dir /somewhere") {
		t.Fatalf("daisugi ran with %q", b)
	}
	pid, _ := st["pid"].(int)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if answers(url, 200*time.Millisecond) {
		t.Fatal("the voice server still answers after the server closed")
	}
	if pid <= 0 || syscall.Kill(pid, 0) == nil {
		t.Fatalf("voice process %d is still there", pid)
	}
}

func TestAVoiceServerThatDiesStartsOnceMoreThenIsDown(t *testing.T) {
	fakeDaisugiOnPath(t, "ok")
	s := newVoiceServer(t, VoiceConfig{})
	s.StartVoice()
	first := waitVoice(t, s, "ready")
	pid1, _ := first["pid"].(int)
	_ = syscall.Kill(pid1, syscall.SIGKILL)
	deadline := time.Now().Add(20 * time.Second)
	var again map[string]any
	for {
		again = s.voice.status()
		if p, _ := again["pid"].(int); again["state"] == "ready" && p != pid1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("voice did not start again: %v", again)
		}
		time.Sleep(20 * time.Millisecond)
	}
	pid2, _ := again["pid"].(int)
	_ = syscall.Kill(pid2, syscall.SIGKILL)
	down := waitVoice(t, s, "down")
	if !strings.Contains(down["reason"].(string), "stopped twice") || down["ready"] != false {
		t.Fatalf("status = %v", down)
	}
	// voice.start begins again, with a fresh restart.
	s.voice.ensure()
	waitVoice(t, s, "ready")
}

func TestVoiceWithoutTheVoiceExtraSaysSo(t *testing.T) {
	fakeDaisugiOnPath(t, "noextra")
	s := newVoiceServer(t, VoiceConfig{})
	s.StartVoice()
	st := waitVoice(t, s, "down")
	if st["reason"] != "Voice needs daisugi's voice extra." ||
		!strings.Contains(st["fix"].(string), "opendaisugi[voice]") ||
		st["command"] != "pip install 'opendaisugi[voice]'" {
		t.Fatalf("status = %v", st)
	}
	// A client types command in a shell. Any other state names none.
	s.voice.lookPath = func(string) (string, error) { return "", errors.New("gone") }
	s.voice.ensure()
	st = waitVoice(t, s, "down")
	if st["command"] != "pip install 'opendaisugi[voice]'" {
		t.Fatalf("not on PATH: %v", st)
	}
	if idle := newVoiceServer(t, VoiceConfig{}).voice.status(); idle["command"] != "" {
		t.Fatalf("idle names a command: %v", idle)
	}
}

func TestVoiceWithADaisugiThatCannotServeVoiceSaysSo(t *testing.T) {
	fakeDaisugiOnPath(t, "nohelp")
	s := newVoiceServer(t, VoiceConfig{})
	s.StartVoice()
	st := waitVoice(t, s, "down")
	if st["reason"] != "This daisugi cannot serve voice." || st["command"] != "pip install --upgrade 'opendaisugi[voice]'" {
		t.Fatalf("status = %v", st)
	}
}

func TestVoiceWithNoDaisugiOnPathSaysSo(t *testing.T) {
	s := newVoiceServer(t, VoiceConfig{})
	s.voice.lookPath = func(string) (string, error) { return "", errors.New("not found") }
	s.StartVoice()
	st := waitVoice(t, s, "down")
	if !strings.Contains(st["reason"].(string), "not on PATH") || st["fix"] == "" {
		t.Fatalf("status = %v", st)
	}
}

func TestVoiceTurnedOffStartsNothing(t *testing.T) {
	argsFile := fakeDaisugiOnPath(t, "ok")
	s := newVoiceServer(t, VoiceConfig{Off: true})
	s.StartVoice()
	s.voice.ensure()
	time.Sleep(100 * time.Millisecond)
	st := s.voice.status()
	if st["state"] != "off" || !strings.Contains(st["reason"].(string), "off in coppice.toml") {
		t.Fatalf("status = %v", st)
	}
	if _, err := os.Stat(argsFile); err == nil {
		t.Fatal("daisugi ran with voice off")
	}
}

func TestAVoiceURLIsUsedAsItIsAndAskedForHealth(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	s := newVoiceServer(t, VoiceConfig{URL: up.URL + "/"})
	s.StartVoice()
	st := s.voice.status()
	if st["ready"] != true || st["url"] != up.URL || st["managed"] != false {
		t.Fatalf("status = %v", st)
	}
	if st["token_file"] != filepath.Join(s.cfg.DataDir, "web", "token") {
		t.Fatalf("token_file = %v, want the web token file", st["token_file"])
	}
	up.Close()
	st = s.voice.status()
	if st["ready"] != false || !strings.Contains(st["reason"].(string), "does not answer") {
		t.Fatalf("status = %v", st)
	}
	s2 := newVoiceServer(t, VoiceConfig{URL: up.URL, TokenFile: "/elsewhere/token"})
	if got := s2.voice.status()["token_file"]; got != "/elsewhere/token" {
		t.Fatalf("token_file = %v", got)
	}
}

func TestVoiceVerbsAnswerOnTheSocket(t *testing.T) {
	s := newTestServer(t)
	s.voice.lookPath = func(string) (string, error) { return "", errors.New("not found") }
	got := roundTrip(t, s, `{"id":"1","cmd":"voice.status"}`, `{"id":"2","cmd":"voice.start"}`)
	for _, r := range got {
		if !r.OK {
			t.Fatalf("reply %+v", r)
		}
	}
	b, _ := json.Marshal(got[0].Result)
	if !strings.Contains(string(b), `"state":"idle"`) {
		t.Fatalf("voice.status = %s", b)
	}
	waitVoice(t, s, "down")
}

// The failure this names: a coppice.toml that does not read turned voice
// on. Voice off carries the reason the config gives.
func TestVoiceOffCarriesItsReason(t *testing.T) {
	s := newVoiceServer(t, VoiceConfig{Off: true, OffReason: "coppice.toml does not read.", OffFix: "Fix it"})
	st := s.voice.status()
	if st["state"] != "off" || st["reason"] != "coppice.toml does not read." || st["fix"] != "Fix it" {
		t.Fatalf("status = %v", st)
	}
}

// The failure this names: a voice url on another machine got the web
// sign-in token. Without a token_file of its own it is refused.
func TestAVoiceURLOffThisMachineNeedsItsOwnTokenFile(t *testing.T) {
	for _, url := range []string{"http://10.0.0.5:7477", "https://voice.example:7477", "http://[fe80::1]:7477"} {
		s := newVoiceServer(t, VoiceConfig{URL: url})
		st := s.voice.status()
		if st["state"] != "off" || !strings.Contains(st["reason"].(string), "another machine") ||
			!strings.Contains(st["fix"].(string), "token_file") {
			t.Fatalf("%s: status = %v", url, st)
		}
	}
	for _, url := range []string{"http://127.0.0.1:1", "http://localhost:1", "http://[::1]:1"} {
		s := newVoiceServer(t, VoiceConfig{URL: url})
		if st := s.voice.status(); st["state"] == "off" {
			t.Fatalf("%s is this machine: %v", url, st)
		}
	}
	s := newVoiceServer(t, VoiceConfig{URL: "http://10.0.0.5:7477", TokenFile: "/its/token"})
	if st := s.voice.status(); st["state"] == "off" || st["token_file"] != "/its/token" {
		t.Fatalf("with a token_file: %v", s.voice.status())
	}
}

// The failure this names: args could move the voice server off loopback
// or away from its token. The flags coppice sets are refused in args.
func TestVoiceArgsCannotSetTheFlagsCoppiceSets(t *testing.T) {
	argsFile := fakeDaisugiOnPath(t, "ok")
	for _, args := range [][]string{{"--host", "0.0.0.0"}, {"--listen=0.0.0.0:1"}, {"--port", "1"}, {"--token-file", "/x"}} {
		s := newVoiceServer(t, VoiceConfig{Args: args})
		s.StartVoice()
		st := s.voice.status()
		if st["state"] != "off" || !strings.Contains(st["reason"].(string), args[0][:strings.IndexAny(args[0]+"=", "=")]) {
			t.Fatalf("%v: status = %v", args, st)
		}
	}
	if _, err := os.Stat(argsFile); err == nil {
		t.Fatal("daisugi ran with a refused flag")
	}
}

// The failure this names: the log is kept across runs, so a run that
// ended without a word showed the reason an earlier run gave.
func TestLastLineReadsOnlyTheLatestRun(t *testing.T) {
	p := filepath.Join(t.TempDir(), "voice.log")
	os.WriteFile(p, []byte("coppice: starting daisugi voice serve\nfaster-whisper is not installed.\n"+
		"coppice: starting daisugi voice serve\n"), 0o600)
	if got := lastLine(p); got != "" {
		t.Fatalf("lastLine = %q, want nothing from the latest run", got)
	}
	os.WriteFile(p, []byte("old\ncoppice: starting daisugi voice serve\nnew line\n\n"), 0o600)
	if got := lastLine(p); got != "new line" {
		t.Fatalf("lastLine = %q", got)
	}
}

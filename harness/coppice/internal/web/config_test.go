package web

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigTreatsAMissingFileAsOff(t *testing.T) {
	c, err := LoadConfig(filepath.Join(t.TempDir(), "web.json"))
	if err != nil {
		t.Fatalf("a missing config is not an error: %v", err)
	}
	if c.Enabled {
		t.Fatal("a missing config enabled the phone server")
	}
}

// The failure this names: a config file that half parses must never come
// back enabled. json.Unmarshal fills in every field it reaches before a
// type error, so a caller that swallowed the error instead of returning it
// would hand back an enabled Config with a blank Listen.
func TestLoadConfigRefusesAMalformedFileRatherThanEnablingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.json")
	if err := os.WriteFile(path, []byte(`{"enabled": true, "listen": 8443}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err == nil {
		t.Fatal("a malformed web.json loaded without an error")
	}
	if c.Enabled {
		t.Fatal("a malformed web.json came back enabled")
	}
}

func TestSaveConfigWritesAPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "coppice", "web.json")
	if err := SaveConfig(path, Config{Enabled: true, Listen: ":8443", TLS: "localca"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("web.json mode is %o, want 600", fi.Mode().Perm())
	}
	back, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Enabled || back.Listen != ":8443" || back.TLS != "localca" {
		t.Fatalf("round trip gave %+v", back)
	}
}

// The failure this names: an operator who never asked for a phone server
// must not get a listener when the coppice server starts. Checking only
// that AutoStart returned nil would pass on exactly the bug this test
// exists to catch, so it names a port and proves nothing came up on it.
func TestAutoStartDoesNothingWhenTheConfigIsAbsentOrDisabled(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "web.json")
	_, d := newFakeServer(t, echoOK)
	store := TokenStore{Path: filepath.Join(dir, "web-token")}
	if _, err := store.Mint(); err != nil {
		t.Fatal(err)
	}
	addr := freeAddr(t)

	// No config file at all.
	stop, err := AutoStart(context.Background(), configPath, Options{Dial: d, Tokens: store})
	if err != nil {
		t.Fatal(err)
	}
	assertNothingListening(t, addr, "with no web.json")
	stop()

	// A config that names a port but is switched off.
	if err := SaveConfig(configPath, Config{Enabled: false, Listen: addr, TLS: "off"}); err != nil {
		t.Fatal(err)
	}
	stop, err = AutoStart(context.Background(), configPath, Options{Dial: d, Tokens: store})
	if err != nil {
		t.Fatal(err)
	}
	assertNothingListening(t, addr, "with enabled false")
	stop()

	// Switched on, the same port answers. Without this the two checks above
	// would pass on a port nothing could ever bind.
	if err := SaveConfig(configPath, Config{Enabled: true, Listen: addr, TLS: "off"}); err != nil {
		t.Fatal(err)
	}
	stop, err = AutoStart(context.Background(), configPath, Options{Dial: d, Tokens: store})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	waitForPort(t, addr)
}

func TestServeOverPlainLoopbackAnswersTheTokenCheck(t *testing.T) {
	home := t.TempDir()
	_, d := newFakeServer(t, echoOK)
	store := TokenStore{Path: filepath.Join(home, "web-token")}
	tok, err := store.Mint()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeAddr(t)
	cfg := Config{Enabled: true, Listen: addr, TLS: "off"}
	go Serve(ctx, cfg, Options{Dial: d, Tokens: store, Gate: AskChannel{Root: home}})
	waitForPort(t, addr)

	if code := getStatus(t, "http://"+addr+"/api/token/check", tok); code != 200 {
		t.Fatalf("token check returned %d", code)
	}
	if code := getStatus(t, "http://"+addr+"/api/token/check", "wrong"); code != 401 {
		t.Fatalf("a bad token returned %d, want 401", code)
	}
}

// The failure this names: Serve starts the CA hand-off before it tries to
// listen for the phone itself, so a phone port that is already taken must
// not leave that hand-off answering with nothing behind it. AutoStart's own
// context has to come down on a failed Serve, not only when a caller asks
// it to stop.
func TestAutoStartStopsTheCAHandoffWhenServeFailsToListen(t *testing.T) {
	dataDir := t.TempDir()
	configPath := filepath.Join(dataDir, "web.json")
	_, d := newFakeServer(t, echoOK)
	store := TokenStore{Path: filepath.Join(dataDir, "web-token")}
	if _, err := store.Mint(); err != nil {
		t.Fatal(err)
	}

	caDir := filepath.Join(dataDir, "ca")
	if _, err := (CADir{Path: caDir}).Init([]string{"localhost"}, nil, time.Now()); err != nil {
		t.Fatal(err)
	}

	// Held before AutoStart runs, so Serve's own listen fails immediately.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	phoneAddr := held.Addr().String()
	caListen := freeAddr(t)

	if err := SaveConfig(configPath, Config{
		Enabled: true, Listen: phoneAddr, TLS: "localca", CADir: caDir, CAListen: caListen,
	}); err != nil {
		t.Fatal(err)
	}

	stop, err := AutoStart(context.Background(), configPath, Options{Dial: d, Tokens: store})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// Serve's own listen on phoneAddr fails, since held already owns it.
	// Whether the CA hand-off manages to open its own listener before that
	// failure cancels its context, or never gets the chance to at all, this
	// window has to end with nothing answering on caListen: a build without
	// the fix leaves it answering once it does open, since nothing ever
	// cancels its context.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.DialTimeout("tcp", caListen, 100*time.Millisecond); err == nil {
			conn.Close()
			t.Fatalf("the CA hand-off on %s is still answering after the phone failed to listen", caListen)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The failure this names: a coppice server restarted by AutoStart, rather
// than started by hand from the CLI, builds Options from the saved Config
// alone. A voice_url written once by --voice-url --persist and never read
// back by Serve would leave every restart after the first answering 409,
// with the phone's record button pointed at nothing, and nothing red to
// say so. gate_root is pinned in the same test since Serve's copy of it
// works the same way and costs nothing extra to check here.
func TestServeReadsVoiceURLAndGateRootFromTheSavedConfig(t *testing.T) {
	dataDir := t.TempDir()
	upstream, upstreamURL := newVoiceUpstream(t, 200, `{"text":"hello"}`)
	store := TokenStore{Path: TokenPath(dataDir)}
	tok, err := store.Mint()
	if err != nil {
		t.Fatal(err)
	}
	addr := freeAddr(t)
	gateRoot := filepath.Join(dataDir, "gate")

	configPath := filepath.Join(dataDir, "web.json")
	cfg := Config{Enabled: true, Listen: addr, TLS: "off", VoiceURL: upstreamURL, GateRoot: gateRoot}
	if err := SaveConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, d := newFakeServer(t, echoOK)
	// Options here carries no VoiceURL and no Gate, the same as AutoStart's
	// own call: only Serve's own read of the loaded Config can supply them.
	go Serve(ctx, loaded, Options{Dial: d, Tokens: store})
	waitForPort(t, addr)

	req, err := http.NewRequest("POST", "http://"+addr+"/api/voice/transcribe", strings.NewReader("clip"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d, want 200: Serve did not read voice_url out of the saved config", resp.StatusCode)
	}
	if upstream.count() != 1 {
		t.Fatalf("the voice server saw %d requests, want 1", upstream.count())
	}

	// gate_root: a made-up tool_use_id must read as ErrNoAsk, status 409,
	// the answer for a real, empty gate directory. A Root Serve never set
	// answers a different, generic error at status 500 instead. A deny goes
	// straight to the gate directory, so it tests the root and nothing else.
	answerReq, err := http.NewRequest("POST", "http://"+addr+"/api/ask/answer",
		strings.NewReader(`{"tool_use_id":"toolu_nonexistent","decision":"deny"}`))
	if err != nil {
		t.Fatal(err)
	}
	answerReq.Header.Set("Authorization", "Bearer "+tok)
	answerResp, err := http.DefaultClient.Do(answerReq)
	if err != nil {
		t.Fatal(err)
	}
	defer answerResp.Body.Close()
	if answerResp.StatusCode != http.StatusConflict {
		var body map[string]any
		json.NewDecoder(answerResp.Body).Decode(&body)
		t.Fatalf("status %d, want 409: Serve did not read gate_root out of the saved config: %v", answerResp.StatusCode, body)
	}
}

// The failure this names: plain HTTP on anything but loopback would put
// every pane on the box on the wire in the clear.
func TestServeRefusesPlainHTTPOffLoopback(t *testing.T) {
	home := t.TempDir()
	_, d := newFakeServer(t, echoOK)
	store := TokenStore{Path: filepath.Join(home, "web-token")}
	store.Mint()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// 0.0.0.0:0 is never bound: RequireLoopback refuses the host before
	// Serve tries to listen on anything.
	err := Serve(ctx, Config{Enabled: true, Listen: "0.0.0.0:0", TLS: "off"},
		Options{Dial: d, Tokens: store})
	if err == nil {
		t.Fatal("Serve accepted plain HTTP on 0.0.0.0")
	}
}

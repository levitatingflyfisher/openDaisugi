package web

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf is a log buffer the server's goroutines and the test share.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// tailnetServe starts Serve under --tls tailscale with a blank-host listen
// on a free port, and returns the port, the log, and what Listening heard.
func tailnetServe(t *testing.T, dataDir string, wait bool) (string, *syncBuf, chan []string, chan error) {
	t.Helper()
	_, d := newFakeServer(t, echoOK)
	_, port, _ := net.SplitHostPort(freeAddr(t))
	certFile, keyFile := TailscalePaths(dataDir)
	logs := &syncBuf{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	heard := make(chan []string, 1)
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, Config{Enabled: true, Listen: ":" + port, TLS: "tailscale", CertFile: certFile, KeyFile: keyFile},
			Options{Dial: d, Tokens: TokenStore{Path: filepath.Join(dataDir, "t")},
				Log:            slog.New(slog.NewTextHandler(logs, nil)),
				Listening:      func(a []string) { heard <- a },
				WaitForTailnet: wait, TailnetRetry: 50 * time.Millisecond,
				ServingFile: ServingPath(dataDir)})
	}()
	return port, logs, heard, done
}

// A tailnet address the box will not bind is skipped with one log line,
// and the server serves on the addresses it can bind.
func TestServeTailscaleSkipsATailnetAddressItCannotBind(t *testing.T) {
	useFakeTailscale(t)
	t.Setenv("FAKE_TS_IPS", "127.0.0.2,fd7a:115c:a1e0::1")
	port, logs, heard, done := tailnetServe(t, t.TempDir(), false)
	select {
	case addrs := <-heard:
		if strings.Join(addrs, " ") != "127.0.0.2:"+port+" 127.0.0.1:"+port {
			t.Fatalf("listening on %v", addrs)
		}
	case err := <-done:
		t.Fatalf("Serve returned %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("Serve never said it listens")
	}
	if n := strings.Count(logs.String(), "web: skipped a tailnet address it cannot bind"); n != 1 {
		t.Fatalf("%d skip lines, want 1:\n%s", n, logs)
	}
	if !strings.Contains(logs.String(), "addr=[fd7a:115c:a1e0::1]:"+port) {
		t.Fatalf("the skip line does not name the address:\n%s", logs)
	}
}

// With no tailnet address it can bind, a foreground serve fails, in one
// line that names the fix.
func TestServeTailscaleFailsWhenNoTailnetAddressBinds(t *testing.T) {
	useFakeTailscale(t)
	t.Setenv("FAKE_TS_IPS", "fd7a:115c:a1e0::1")
	_, _, _, done := tailnetServe(t, t.TempDir(), false)
	select {
	case err := <-done:
		if err == nil || !strings.HasPrefix(err.Error(), "No tailnet address could be bound: listen tcp [fd7a:115c:a1e0::1]:") ||
			!strings.HasSuffix(err.Error(), "Check that tailscale is up, then run coppice web serve --tls tailscale again.") {
			t.Fatalf("got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not fail")
	}
}

// At boot, a saved phone server whose tailnet address is not up yet
// serves loopback at once and binds the tailnet when it comes, with one
// line when it first waits and one when it binds.
func TestServeTailscaleWaitsForTheTailnetAtBoot(t *testing.T) {
	useFakeTailscale(t)
	dataDir := t.TempDir()
	up := filepath.Join(dataDir, "up")
	certFile, keyFile := TailscalePaths(dataDir)
	if _, err := EnsureCert("box.tail1.ts.net", certFile, keyFile, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(up, []byte("down"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_TS_UP_FILE", up)
	port, logs, _, done := tailnetServe(t, dataDir, true)
	waitForPort(t, "127.0.0.1:"+port)
	assertNothingListening(t, "127.0.0.2:"+port, "before the tailnet is up")
	select {
	case err := <-done:
		t.Fatalf("Serve returned %v", err)
	default:
	}
	if err := os.WriteFile(up, []byte("up"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForPort(t, "127.0.0.2:"+port)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(logs.String(), "web: listening on the tailnet") {
		time.Sleep(20 * time.Millisecond)
	}
	out := logs.String()
	if n := strings.Count(out, "web: waiting for the tailnet address"); n != 1 {
		t.Fatalf("%d waiting lines, want 1:\n%s", n, out)
	}
	if n := strings.Count(out, `msg="web: listening on the tailnet" addrs=127.0.0.2:`+port); n != 1 {
		t.Fatalf("%d listening lines, want 1:\n%s", n, out)
	}
}

// The backoff runs from the first delay, doubling, to 60 s.
func TestTailnetBackoffDoublesToSixtySeconds(t *testing.T) {
	d := 2 * time.Second
	var seen []time.Duration
	for i := 0; i < 7; i++ {
		seen = append(seen, d)
		d = NextTailnetRetry(d)
	}
	want := []time.Duration{2, 4, 8, 16, 32, 60, 60}
	for i := range want {
		if seen[i] != want[i]*time.Second {
			t.Fatalf("backoff %v, want %v seconds", seen, want)
		}
	}
}

// While a tailscale serve runs, it tells the coppice server which
// certificate it serves, so floor.facts can warn about any path.
func TestServeTailscaleWritesTheServingFileWhileItRuns(t *testing.T) {
	useFakeTailscale(t)
	dataDir := t.TempDir()
	_, _, heard, _ := tailnetServe(t, dataDir, false)
	select {
	case <-heard:
	case <-time.After(10 * time.Second):
		t.Fatal("Serve never said it listens")
	}
	raw, err := os.ReadFile(ServingPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	var s Serving
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	certFile, _ := TailscalePaths(dataDir)
	if s.PID != os.Getpid() || s.CertFile != certFile {
		t.Fatalf("serving file %s", raw)
	}
	cert, ok := ServingCert(ServingPath(dataDir))
	if !ok || cert != certFile {
		t.Fatalf("ServingCert %q %v", cert, ok)
	}
}

// A serving file left by a process that is gone names nothing.
func TestServingCertIgnoresADeadServe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serving.json")
	if err := os.WriteFile(path, []byte(`{"pid": 999999999, "cert_file": "/x.crt"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if cert, ok := ServingCert(path); ok {
		t.Fatalf("a dead serve named %q", cert)
	}
	if _, ok := ServingCert(filepath.Join(t.TempDir(), "none.json")); ok {
		t.Fatal("a missing file named a certificate")
	}
}

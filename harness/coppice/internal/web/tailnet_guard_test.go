package web

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Only an address inside Tailscale's ranges is a tailnet address. A
// loopback address is kept too: it reaches only this box, and it is what
// the fake tailscale reports so the tests can bind it.
func TestTailnetAddrKeepsOnlyTailscaleRanges(t *testing.T) {
	for ip, want := range map[string]bool{
		"100.64.0.1": true, "100.127.255.255": true, "100.128.0.1": false, "100.63.255.255": false,
		"fd7a:115c:a1e0::1": true, "fd7a:115c:a1e0:ab12::1": true, "fd7a:115c:a1e1::1": false,
		"127.0.0.2": true, "::1": true,
		"": false, "0.0.0.0": false, "::": false, "192.168.1.2": false, "10.0.0.1": false, "nonsense": false,
	} {
		if got := TailnetAddr(ip); got != want {
			t.Errorf("TailnetAddr(%q) = %v, want %v", ip, got, want)
		}
	}
}

// An address tailscale reports that is not a tailnet address, an empty or
// unspecified one above all, is skipped with one log line and never bound.
func TestServeTailscaleSkipsAnAddressOutsideTheTailnet(t *testing.T) {
	useFakeTailscale(t)
	t.Setenv("FAKE_TS_IPS", ",0.0.0.0,127.0.0.2")
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
	if n := strings.Count(logs.String(), "web: skipped an address that is not a tailnet address"); n != 2 {
		t.Fatalf("%d skip lines, want 2:\n%s", n, logs)
	}
	assertNothingListening(t, "127.0.0.3:"+port, "on an address that is not the tailnet")
}

// A --listen with a host is used exactly as given, also when tailscale
// comes up late at boot: the tailnet is never bound behind it.
func TestServeTailscaleWithAHostNeverBindsTheTailnetLater(t *testing.T) {
	useFakeTailscale(t)
	dataDir := t.TempDir()
	up := filepath.Join(dataDir, "up")
	certFile, keyFile := TailscalePaths(dataDir)
	if _, err := EnsureCert("box.tail1.ts.net", certFile, keyFile, time.Now()); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(up, []byte("down"), 0o600)
	t.Setenv("FAKE_TS_UP_FILE", up)
	_, d := newFakeServer(t, echoOK)
	_, port, _ := net.SplitHostPort(freeAddr(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logs := &syncBuf{}
	go Serve(ctx, Config{Enabled: true, Listen: "127.0.0.1:" + port, TLS: "tailscale", CertFile: certFile, KeyFile: keyFile},
		Options{Dial: d, Tokens: TokenStore{Path: filepath.Join(dataDir, "t")}, Log: slog.New(slog.NewTextHandler(logs, nil)),
			WaitForTailnet: true, TailnetRetry: 20 * time.Millisecond})
	waitForPort(t, "127.0.0.1:"+port)
	os.WriteFile(up, []byte("up"), 0o600)
	time.Sleep(400 * time.Millisecond)
	assertNothingListening(t, "127.0.0.2:"+port, "behind a --listen with a host")
	if strings.Contains(logs.String(), "listening on the tailnet") {
		t.Fatalf("the tailnet was bound:\n%s", logs)
	}
}

// At boot, a valid pair is served on the tailnet even when tailscale cert
// fails. The failure is logged once; the daily check tries again.
func TestServeTailscaleServesAValidPairWhenRenewalFails(t *testing.T) {
	useFakeTailscale(t)
	dataDir := t.TempDir()
	certFile, keyFile := TailscalePaths(dataDir)
	t.Setenv("FAKE_TS_DAYS", "10")
	if _, err := EnsureCert("box.tail1.ts.net", certFile, keyFile, time.Now()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_TS_CERT_FAIL", "1")
	for _, wait := range []bool{true, false} {
		port, logs, heard, done := tailnetServe(t, dataDir, wait)
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
		if n := strings.Count(logs.String(), "web: the certificate renewal failed"); n != 1 {
			t.Fatalf("%d renewal lines, want 1:\n%s", n, logs)
		}
	}
}

// A missing or expired pair cannot serve, so a renewal that fails stops a
// foreground serve.
func TestServeTailscaleWithNoPairAndAFailingCertStops(t *testing.T) {
	useFakeTailscale(t)
	t.Setenv("FAKE_TS_CERT_FAIL", "1")
	_, _, _, done := tailnetServe(t, t.TempDir(), false)
	select {
	case err := <-done:
		if err == nil || !strings.HasPrefix(err.Error(), "tailscale cert failed: fake tailscale: the certificate service did not answer.") {
			t.Fatalf("got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not stop")
	}
}

// EnsureCert never overwrites a file that is not a PEM certificate, or a
// key file that is not a PEM private key.
func TestEnsureCertRefusesToOverwriteAFileThatIsNotACertificate(t *testing.T) {
	useFakeTailscale(t)
	dir := t.TempDir()
	notes := filepath.Join(dir, "notes.txt")
	os.WriteFile(notes, []byte("my notes\n"), 0o644)
	_, err := EnsureCert("box.tail1.ts.net", notes, filepath.Join(dir, "k.key"), time.Now())
	if err == nil || err.Error() != notes+" is not a PEM certificate, so tailscale cert will not write over it. Move it away or name another --cert, then run coppice web serve --tls tailscale again." {
		t.Fatalf("got %v", err)
	}
	if raw, _ := os.ReadFile(notes); string(raw) != "my notes\n" {
		t.Fatalf("the file was overwritten: %q", raw)
	}
	key := filepath.Join(dir, "k2.key")
	os.WriteFile(key, []byte("not a key\n"), 0o600)
	_, err = EnsureCert("box.tail1.ts.net", filepath.Join(dir, "c2.crt"), key, time.Now())
	if err == nil || err.Error() != key+" is not a PEM private key, so tailscale cert will not write over it. Move it away or name another --key, then run coppice web serve --tls tailscale again." {
		t.Fatalf("got %v", err)
	}
	if raw, _ := os.ReadFile(key); string(raw) != "not a key\n" {
		t.Fatalf("the key file was overwritten: %q", raw)
	}
}

// EnsureCert sets 0700 only on a directory it made.
func TestEnsureCertLeavesTheModeOfADirectoryItDidNotMake(t *testing.T) {
	useFakeTailscale(t)
	dir := filepath.Join(t.TempDir(), "shared")
	os.Mkdir(dir, 0o755)
	os.Chmod(dir, 0o755)
	if _, err := EnsureCert("box.tail1.ts.net", filepath.Join(dir, "c.crt"), filepath.Join(dir, "c.key"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v, want 0755 kept", info.Mode().Perm())
	}
}

// The serving file names the key as well, so Renew can renew the pair.
func TestServingNamesTheCertAndTheKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web", "serving.json")
	if err := writeServing(path, "/a/c.crt", "/a/c.key"); err != nil {
		t.Fatal(err)
	}
	c, k, ok := ServingPair(path)
	if !ok || c != "/a/c.crt" || k != "/a/c.key" {
		t.Fatalf("ServingPair %q %q %v", c, k, ok)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
}

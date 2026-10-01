package web

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeTailscaleDir is the fake tailscale the web cases use too. Each test
// that reaches tailscale puts it first on PATH, so the real tailscale on
// this box, if any, is never run.
func fakeTailscaleDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs("../../../coppice-rs/cases/web/fixtures/bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tailscale")); err != nil {
		t.Fatalf("no fake tailscale: %v", err)
	}
	return dir
}

// useFakeTailscale puts the fake first on PATH, with /usr/bin and /bin
// after it for the openssl the fake runs.
func useFakeTailscale(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", fakeTailscaleDir(t)+":/usr/bin:/bin")
	for _, k := range []string{"FAKE_TS_NAME", "FAKE_TS_IPS", "FAKE_TS_STATE", "FAKE_TS_HTTPS", "FAKE_TS_DAYS", "FAKE_TS_LOG", "FAKE_TS_UP_FILE", "FAKE_TS_CERT_FAIL"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func TestReadTailscaleGivesTheNameAndTheTailnetAddresses(t *testing.T) {
	useFakeTailscale(t)
	t.Setenv("FAKE_TS_IPS", "127.0.0.2,fd7a:115c::1")
	st, err := ReadTailscale()
	if err != nil {
		t.Fatal(err)
	}
	if st.Name != "box.tail1.ts.net" {
		t.Fatalf("name %q, want box.tail1.ts.net with no trailing dot", st.Name)
	}
	if strings.Join(st.IPs, ",") != "127.0.0.2,fd7a:115c::1" {
		t.Fatalf("addresses %v", st.IPs)
	}
}

func TestReadTailscaleWithNoTailscaleSaysSoInOneLine(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := ReadTailscale()
	if err == nil || err.Error() != NoTailscale {
		t.Fatalf("got %v, want %q", err, NoTailscale)
	}
	if strings.Contains(NoTailscale, "\n") || !strings.Contains(NoTailscale, "coppice web serve") {
		t.Fatalf("the line must be one line and name coppice web serve: %q", NoTailscale)
	}
}

func TestReadTailscaleRefusesAStoppedTailscale(t *testing.T) {
	useFakeTailscale(t)
	t.Setenv("FAKE_TS_STATE", "NeedsLogin")
	_, err := ReadTailscale()
	if err == nil || !strings.Contains(err.Error(), "NeedsLogin") || !strings.Contains(err.Error(), "tailscale up") {
		t.Fatalf("got %v", err)
	}
}

func TestReadTailscaleRefusesATailnetWithHTTPSOff(t *testing.T) {
	useFakeTailscale(t)
	t.Setenv("FAKE_TS_HTTPS", "off")
	_, err := ReadTailscale()
	if err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("got %v", err)
	}
}

func TestEnsureCertGetsTheFirstCertificate(t *testing.T) {
	useFakeTailscale(t)
	dir := filepath.Join(t.TempDir(), "tls")
	certFile, keyFile := filepath.Join(dir, "t.crt"), filepath.Join(dir, "t.key")
	res, err := EnsureCert("box.tail1.ts.net", certFile, keyFile, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !res.First || !res.Got || res.Days != 90 {
		t.Fatalf("got %+v, want first, got, 90 days", res)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("the tls dir is not 0700: %v %v", info, err)
	}
}

func TestEnsureCertKeepsACertificateWithThirtyDaysOrMore(t *testing.T) {
	useFakeTailscale(t)
	log := filepath.Join(t.TempDir(), "ts.log")
	t.Setenv("FAKE_TS_LOG", log)
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "t.crt"), filepath.Join(dir, "t.key")
	if _, err := EnsureCert("box.tail1.ts.net", certFile, keyFile, time.Now()); err != nil {
		t.Fatal(err)
	}
	res, err := EnsureCert("box.tail1.ts.net", certFile, keyFile, time.Now().Add(59*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.Got || res.First || res.Days != 31 {
		t.Fatalf("got %+v, want the kept certificate with 31 days", res)
	}
	raw, _ := os.ReadFile(log)
	if n := strings.Count(string(raw), "cert "); n != 1 {
		t.Fatalf("tailscale cert ran %d times, want 1:\n%s", n, raw)
	}
}

func TestEnsureCertRenewsUnderThirtyDays(t *testing.T) {
	useFakeTailscale(t)
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "t.crt"), filepath.Join(dir, "t.key")
	t.Setenv("FAKE_TS_DAYS", "20")
	if _, err := EnsureCert("box.tail1.ts.net", certFile, keyFile, time.Now()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_TS_DAYS", "90")
	res, err := EnsureCert("box.tail1.ts.net", certFile, keyFile, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Got || res.First || res.Days != 90 {
		t.Fatalf("got %+v, want a renewal with 90 days", res)
	}
}

func TestEnsureCertNamesTheFixWhenTailscaleCertFails(t *testing.T) {
	useFakeTailscale(t)
	dir := t.TempDir()
	_, err := EnsureCert("fail.tail1.ts.net", filepath.Join(dir, "t.crt"), filepath.Join(dir, "t.key"), time.Now())
	if err == nil || !strings.Contains(err.Error(), "HTTPS is off") || !strings.Contains(err.Error(), "coppice web serve --tls tailscale") {
		t.Fatalf("got %v", err)
	}
}

func TestListenAddrsKeepsTheTailnetAndLoopbackForABlankHost(t *testing.T) {
	got, err := ListenAddrs(":8443", []string{"100.64.0.1", "fd7a:115c::1"})
	if err != nil {
		t.Fatal(err)
	}
	want := "100.64.0.1:8443 [fd7a:115c::1]:8443 127.0.0.1:8443"
	if strings.Join(got, " ") != want {
		t.Fatalf("got %v, want %s", got, want)
	}
	got, err = ListenAddrs("0.0.0.0:9000", []string{"100.64.0.1"})
	if err != nil || strings.Join(got, " ") != "0.0.0.0:9000" {
		t.Fatalf("a named host is used as given: %v %v", got, err)
	}
	if _, err := ListenAddrs("nohost", nil); err == nil {
		t.Fatal("a listen with no port was accepted")
	}
}

func TestCertDaysIsWholeDaysLeft(t *testing.T) {
	now := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		left time.Duration
		want int
	}{
		{3*24*time.Hour - time.Second, 2},
		{3 * 24 * time.Hour, 3},
		{time.Hour, 0},
		{-time.Hour, -1},
	}
	for _, c := range cases {
		if got := CertDays(now.Add(c.left), now); got != c.want {
			t.Errorf("CertDays(%v left) = %d, want %d", c.left, got, c.want)
		}
	}
}

// A renewed pair on disk is served from the next handshake on, with no
// restart.
func TestCertReloaderServesTheNewPairAfterARenewal(t *testing.T) {
	useFakeTailscale(t)
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "t.crt"), filepath.Join(dir, "t.key")
	if _, err := EnsureCert("box.tail1.ts.net", certFile, keyFile, time.Now()); err != nil {
		t.Fatal(err)
	}
	r, err := NewCertReloader(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	first, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	// A new mtime marks the new pair, even within the same second.
	t.Setenv("FAKE_TS_DAYS", "40")
	os.Remove(certFile)
	if _, err := EnsureCert("box.tail1.ts.net", certFile, keyFile, time.Now()); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(certFile, time.Now().Add(time.Minute), time.Now().Add(time.Minute))
	second, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(first.Certificate[0]) == string(second.Certificate[0]) {
		t.Fatal("the reloader kept the old certificate after the pair changed")
	}
}

// Serve under --tls tailscale with a listen address that has no host gets
// the certificate, listens on the tailnet address and loopback, says so
// once it listens, and listens nowhere else.
func TestServeTailscaleListensOnTheTailnetAndLoopbackOnly(t *testing.T) {
	useFakeTailscale(t)
	dataDir := t.TempDir()
	_, d := newFakeServer(t, echoOK)
	store := TokenStore{Path: filepath.Join(dataDir, "web-token")}
	tok, err := store.Mint()
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(freeAddr(t))
	certFile, keyFile := TailscalePaths(dataDir)
	cfg := Config{Enabled: true, Listen: ":" + port, TLS: "tailscale", CertFile: certFile, KeyFile: keyFile}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	heard := make(chan []string, 1)
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, cfg, Options{Dial: d, Tokens: store, Gate: AskChannel{Root: dataDir},
			Listening: func(addrs []string) { heard <- addrs }})
	}()
	select {
	case addrs := <-heard:
		want := "127.0.0.2:" + port + " 127.0.0.1:" + port
		if strings.Join(addrs, " ") != want {
			t.Fatalf("listening on %v, want %s", addrs, want)
		}
	case err := <-done:
		t.Fatalf("Serve returned %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("Serve never said it listens")
	}
	pool := x509.NewCertPool()
	pem, _ := os.ReadFile(certFile)
	pool.AppendCertsFromPEM(pem)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "box.tail1.ts.net"}}}
	for _, host := range []string{"127.0.0.2", "127.0.0.1"} {
		req, _ := http.NewRequest("GET", "https://"+net.JoinHostPort(host, port)+"/api/token/check", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET on %s: %v", host, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("token check on %s returned %d", host, resp.StatusCode)
		}
	}
	assertNothingListening(t, net.JoinHostPort("127.0.0.3", port), "on an address that is not the tailnet or loopback")
}

// A saved tailscale config on a box with no tailscale does not serve, and
// says why in the one line.
func TestServeTailscaleWithNoTailscaleRefuses(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dataDir := t.TempDir()
	_, d := newFakeServer(t, echoOK)
	certFile, keyFile := TailscalePaths(dataDir)
	err := Serve(context.Background(), Config{Enabled: true, Listen: ":0", TLS: "tailscale", CertFile: certFile, KeyFile: keyFile},
		Options{Dial: d, Tokens: TokenStore{Path: filepath.Join(dataDir, "t")}})
	if err == nil || err.Error() != NoTailscale {
		t.Fatalf("got %v", err)
	}
}

// The daily check renews a certificate that has fallen under the window
// while the server runs.
func TestServeTailscaleChecksTheCertificateAgainWhileItRuns(t *testing.T) {
	useFakeTailscale(t)
	log := filepath.Join(t.TempDir(), "ts.log")
	t.Setenv("FAKE_TS_LOG", log)
	t.Setenv("FAKE_TS_DAYS", "20")
	dataDir := t.TempDir()
	_, d := newFakeServer(t, echoOK)
	_, port, _ := net.SplitHostPort(freeAddr(t))
	certFile, keyFile := TailscalePaths(dataDir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Serve(ctx, Config{Enabled: true, Listen: "127.0.0.1:" + port, TLS: "tailscale", CertFile: certFile, KeyFile: keyFile},
		Options{Dial: d, Tokens: TokenStore{Path: filepath.Join(dataDir, "t")}, CertCheckEvery: 50 * time.Millisecond})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		raw, _ := os.ReadFile(log)
		if strings.Count(string(raw), "cert ") >= 3 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	raw, _ := os.ReadFile(log)
	t.Fatalf("the certificate was not checked again:\n%s", raw)
}

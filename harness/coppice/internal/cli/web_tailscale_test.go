package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/web"
)

// useFakeTailscale puts the web cases' fake tailscale first on PATH, so
// the real tailscale on this box, if any, is never run.
func useFakeTailscale(t *testing.T) {
	t.Helper()
	dir, err := filepath.Abs("../../../coppice-rs/cases/web/fixtures/bin")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	for _, k := range []string{"FAKE_TS_NAME", "FAKE_TS_IPS", "FAKE_TS_STATE", "FAKE_TS_HTTPS", "FAKE_TS_DAYS", "FAKE_TS_LOG", "FAKE_TS_UP_FILE", "FAKE_TS_CERT_FAIL"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

// listeningServe stands in for web.Serve: it says it listens on the
// addresses the real one would, and returns.
func listeningServe(addrs ...string) func(context.Context, web.Config, web.Options) error {
	return func(_ context.Context, _ web.Config, o web.Options) error {
		if o.Listening != nil {
			o.Listening(addrs)
		}
		return nil
	}
}

// One command does the whole job: it gets the certificate, names the
// public logs the first time, saves the settings, prints the sign-in QR
// for the MagicDNS name, and says in one line where it listens.
func TestServeTailscaleDoesTheWholeJob(t *testing.T) {
	useFakeTailscale(t)
	dataDir := t.TempDir()
	out, code := runWithStubServe(t, dataDir, listeningServe("127.0.0.2:8443", "127.0.0.1:8443"),
		"web", "serve", "--tls", "tailscale", "--persist")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{
		"Got a certificate for box.tail1.ts.net. It runs out in 90 days.\n",
		"This name is now in public certificate logs: box.tail1.ts.net.\n",
		"sign in   https://box.tail1.ts.net:8443/#t=",
		"Scan this on the phone.",
		"Serving https://box.tail1.ts.net:8443 on 127.0.0.2:8443 and 127.0.0.1:8443. Only the tailnet and this box can reach it.\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the output has no %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "█") && !strings.Contains(out, "▀") && !strings.Contains(out, "▄") {
		t.Errorf("no QR in the output:\n%s", out)
	}
	cfg, err := web.LoadConfig(web.ConfigPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	certFile, _ := web.TailscalePaths(dataDir)
	if !cfg.Enabled || cfg.TLS != "tailscale" || cfg.CertFile != certFile || cfg.ExternalURL != "https://box.tail1.ts.net:8443" {
		t.Fatalf("saved %+v", cfg)
	}

	// The second start keeps the certificate, and the name is not news.
	out, code = runWithStubServe(t, dataDir, listeningServe("127.0.0.1:8443"), "web", "serve", "--tls", "tailscale", "--qr=false")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if strings.Contains(out, "certificate") {
		t.Fatalf("a kept certificate is not news:\n%s", out)
	}
}

func TestServeTailscaleWithNoTailscaleSaysSoAndLeavesNothing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dataDir := t.TempDir()
	out, code := runWithStubServe(t, dataDir, noopServe, "web", "serve", "--tls", "tailscale", "--persist")
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	if out != web.NoTailscale+"\n" {
		t.Fatalf("got %q, want the one line", out)
	}
	if _, err := os.Stat(web.ConfigPath(dataDir)); err == nil {
		t.Fatal("a run that cannot serve saved web.json")
	}
	if _, err := os.Stat(web.TokenPath(dataDir)); err == nil {
		t.Fatal("a run that cannot serve minted a token")
	}
}

// A certificate with under 30 days left is renewed at start, with no
// public-logs line: the name was already there.
func TestServeTailscaleRenewsACertificateNearItsEnd(t *testing.T) {
	useFakeTailscale(t)
	dataDir := t.TempDir()
	t.Setenv("FAKE_TS_DAYS", "10")
	if code, out, errb := runCLI(t, "/nonexistent.sock", dataDir, "web", "cert", "tailscale"); code != 0 {
		t.Fatalf("exit %d:\n%s%s", code, out, errb)
	}
	t.Setenv("FAKE_TS_DAYS", "90")
	out, code := runWithStubServe(t, dataDir, noopServe, "web", "serve", "--tls", "tailscale", "--qr=false")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "Renewed the certificate for box.tail1.ts.net. It runs out in 90 days.") || strings.Contains(out, "public certificate logs") {
		t.Fatalf("got:\n%s", out)
	}
}

// --listen with a host is used as given, and the line says so.
func TestServeTailscaleListenWithAHostOverrides(t *testing.T) {
	useFakeTailscale(t)
	dataDir := t.TempDir()
	var got web.Config
	stub := func(_ context.Context, c web.Config, _ web.Options) error { got = c; return nil }
	out, code := runWithStubServe(t, dataDir, stub, "web", "serve", "--tls", "tailscale", "--listen", "0.0.0.0:9443", "--qr=false")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if got.Listen != "0.0.0.0:9443" || got.ExternalURL != "https://box.tail1.ts.net:9443" {
		t.Fatalf("serve got %+v", got)
	}
}

// A renewal that fails while the pair on disk is still good serves that
// pair, and says so in one line.
func TestServeTailscaleKeepsAValidPairWhenRenewalFails(t *testing.T) {
	useFakeTailscale(t)
	dataDir := t.TempDir()
	t.Setenv("FAKE_TS_DAYS", "10")
	if code, out, errb := runCLI(t, "/nonexistent.sock", dataDir, "web", "cert", "tailscale"); code != 0 {
		t.Fatalf("exit %d:\n%s%s", code, out, errb)
	}
	t.Setenv("FAKE_TS_CERT_FAIL", "1")
	out, code := runWithStubServe(t, dataDir, listeningServe("127.0.0.2:8443", "127.0.0.1:8443"), "web", "serve", "--tls", "tailscale", "--qr=false")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	want := "The certificate renewal failed, so the certificate on disk serves. It runs out in 10 days. " +
		"tailscale cert failed: fake tailscale: the certificate service did not answer."
	if !strings.Contains(out, want) {
		t.Fatalf("no %q in:\n%s", want, out)
	}
}

// A --cert that is not a certificate is refused and left as it was.
func TestServeTailscaleRefusesAFileThatIsNotACertificate(t *testing.T) {
	useFakeTailscale(t)
	dataDir := t.TempDir()
	notes := filepath.Join(dataDir, "notes.txt")
	os.WriteFile(notes, []byte("my notes\n"), 0o644)
	out, code := runWithStubServe(t, dataDir, noopServe, "web", "serve", "--tls", "tailscale", "--qr=false",
		"--cert", notes, "--key", filepath.Join(dataDir, "k.key"))
	if code != 1 || !strings.Contains(out, "is not a PEM certificate") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if raw, _ := os.ReadFile(notes); string(raw) != "my notes\n" {
		t.Fatalf("overwritten: %q", raw)
	}
	// web cert tailscale refuses the same way.
	dir := filepath.Join(dataDir, "d")
	os.Mkdir(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "tailscale.crt"), []byte("my notes\n"), 0o644)
	code, _, e := runCLI(t, "/nonexistent.sock", dataDir, "web", "cert", "tailscale", "--dir", dir)
	if code != 1 || !strings.Contains(e, "is not a PEM certificate") {
		t.Fatalf("exit %d: %s", code, e)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "tailscale.crt")); string(raw) != "my notes\n" {
		t.Fatalf("overwritten: %q", raw)
	}
}

// Renew types coppice web cert tailscale. With no --dir it renews the
// pair the running serve uses, else the pair web.json saves, so the warning
// clears wherever the pair lives.
func TestWebCertTailscaleRenewsThePairTheServeUses(t *testing.T) {
	useFakeTailscale(t)
	dataDir := t.TempDir()
	own, ownKey := filepath.Join(dataDir, "own", "c.crt"), filepath.Join(dataDir, "own", "c.key")
	os.MkdirAll(filepath.Dir(web.ServingPath(dataDir)), 0o700)
	body := fmt.Sprintf(`{"pid": %d, "cert_file": %q, "key_file": %q}`, os.Getpid(), own, ownKey)
	os.WriteFile(web.ServingPath(dataDir), []byte(body), 0o600)
	if code, out, errb := runCLI(t, "/nonexistent.sock", dataDir, "web", "cert", "tailscale"); code != 0 {
		t.Fatalf("exit %d:\n%s%s", code, out, errb)
	}
	if _, err := web.LoadLeaf(own); err != nil {
		t.Fatalf("the served pair was not renewed: %v", err)
	}
	def, _ := web.TailscalePaths(dataDir)
	if _, err := os.Stat(def); err == nil {
		t.Fatal("the default pair was written instead")
	}

	// No serve runs: the pair web.json saves.
	os.Remove(web.ServingPath(dataDir))
	saved, savedKey := filepath.Join(dataDir, "saved", "c.crt"), filepath.Join(dataDir, "saved", "c.key")
	if err := web.SaveConfig(web.ConfigPath(dataDir), web.Config{Enabled: true, Listen: ":8443", TLS: "tailscale", CertFile: saved, KeyFile: savedKey}); err != nil {
		t.Fatal(err)
	}
	if code, out, errb := runCLI(t, "/nonexistent.sock", dataDir, "web", "cert", "tailscale"); code != 0 {
		t.Fatalf("exit %d:\n%s%s", code, out, errb)
	}
	if _, err := web.LoadLeaf(saved); err != nil {
		t.Fatalf("the saved pair was not renewed: %v", err)
	}
}

// coppice web cert tailscale with no name reads it from tailscale, so the
// Renew button can type one fixed line.
func TestWebCertTailscaleWithNoNameReadsTheName(t *testing.T) {
	useFakeTailscale(t)
	dataDir := t.TempDir()
	code, out, errb := runCLI(t, "/nonexistent.sock", dataDir, "web", "cert", "tailscale")
	if code != 0 {
		t.Fatalf("exit %d:\n%s%s", code, out, errb)
	}
	certFile, _ := web.TailscalePaths(dataDir)
	leaf, err := web.LoadLeaf(certFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "box.tail1.ts.net" {
		t.Fatalf("the certificate is for %v", leaf.DNSNames)
	}
	if strings.Contains(out, "Renewal is yours") {
		t.Fatalf("renewal is no longer the owner's:\n%s", out)
	}
}

package web

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The CLI resolves the data directory once and passes it in. This package
// never reads the home directory on its own, so the default path is a
// function of that data directory, not a lookup.
func TestTLSDirIsWebTLSUnderTheDataDir(t *testing.T) {
	got := TLSDir("/srv/coppice-data")
	want := filepath.Join("/srv/coppice-data", "web", "tls")
	if got != want {
		t.Fatalf("TLSDir returned %q, want %q", got, want)
	}
}

// TailscalePaths is the one source both this task and the CLI that consumes
// it can read, so a certificate coppice web cert tailscale writes is the
// same pair coppice web serve looks for when no --cert or --key is given.
func TestTailscalePathsAreUnderTLSDir(t *testing.T) {
	certFile, keyFile := TailscalePaths("/srv/coppice-data")
	wantDir := TLSDir("/srv/coppice-data")
	if filepath.Dir(certFile) != wantDir || filepath.Dir(keyFile) != wantDir {
		t.Fatalf("TailscalePaths returned %q %q, want files under %q", certFile, keyFile, wantDir)
	}
	if filepath.Base(certFile) != "tailscale.crt" || filepath.Base(keyFile) != "tailscale.key" {
		t.Fatalf("TailscalePaths returned %q %q", certFile, keyFile)
	}
}

func TestResolveLocalCANeedsACADir(t *testing.T) {
	_, _, err := TLSOptions{Source: TLSLocalCA}.Resolve()
	if err == nil {
		t.Fatal("--tls localca accepted an empty --ca-dir")
	}
	if !strings.Contains(err.Error(), "--ca-dir") {
		t.Fatalf("the error does not name the missing flag: %v", err)
	}
}

func TestResolveFilesNeedsBothPaths(t *testing.T) {
	_, _, err := TLSOptions{Source: TLSFiles, CertFile: "only.crt"}.Resolve()
	if err == nil {
		t.Fatal("--tls files accepted a cert with no key")
	}
	if !strings.Contains(err.Error(), "--key") {
		t.Fatalf("the error does not name the missing flag: %v", err)
	}
}

func TestResolveTailscaleTeachesTheCertCommandWhenTheFilesAreMissing(t *testing.T) {
	dir := t.TempDir()
	_, _, err := TLSOptions{Source: TLSTailscale, CertFile: filepath.Join(dir, "a.crt"), KeyFile: filepath.Join(dir, "a.key")}.Resolve()
	if err == nil {
		t.Fatal("--tls tailscale accepted missing files")
	}
	if !strings.Contains(err.Error(), "tailscale cert --cert-file") {
		t.Fatalf("the error does not teach the command: %v", err)
	}
}

// The failure this names: when only the key is missing, the error must not
// blame the cert. The operator checks the path it names, finds the file
// there, and has no next move.
func TestResolveTailscaleNamesTheKeyWhenOnlyTheKeyIsMissing(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "a.crt")
	if err := os.WriteFile(certFile, []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(dir, "a.key")
	_, _, err := TLSOptions{Source: TLSTailscale, CertFile: certFile, KeyFile: keyFile}.Resolve()
	if err == nil {
		t.Fatal("--tls tailscale accepted a missing key")
	}
	if strings.Contains(err.Error(), "no tailscale certificate at") {
		t.Fatalf("the error blames the certificate when only the key is missing: %v", err)
	}
	if !strings.Contains(err.Error(), keyFile) {
		t.Fatalf("the error does not name the missing key: %v", err)
	}
}

func TestResolveFilesNamesTheMissingCertNotTheKey(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "a.key")
	if err := os.WriteFile(keyFile, []byte("stub"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := TLSOptions{Source: TLSFiles, CertFile: filepath.Join(dir, "a.crt"), KeyFile: keyFile}.Resolve()
	if err == nil {
		t.Fatal("--tls files accepted a missing cert")
	}
	if !strings.Contains(err.Error(), "a.crt") {
		t.Fatalf("the error does not name the missing cert: %v", err)
	}
}

func TestResolveLocalCATeachesCertInitWhenThereIsNoCA(t *testing.T) {
	_, _, err := TLSOptions{Source: TLSLocalCA, CADir: filepath.Join(t.TempDir(), "ca")}.Resolve()
	if err == nil {
		t.Fatal("--tls localca accepted a missing CA")
	}
	if !strings.Contains(err.Error(), "coppice web cert init") {
		t.Fatalf("the error does not teach the command: %v", err)
	}
}

// The failure this names: an operator handed only a directory has to guess
// which of the two files is missing. Naming the leaf file itself, the way
// the files and tailscale branches already do, removes the guess.
func TestResolveLocalCANamesTheMissingLeafNotTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	_, _, err := TLSOptions{Source: TLSLocalCA, CADir: dir}.Resolve()
	if err == nil {
		t.Fatal("--tls localca accepted a missing CA directory")
	}
	wantCert, _ := CADir{Path: dir}.LeafPaths()
	// The wrapped stat error already spells the file path, so a Contains
	// check would pass even if the message itself went back to naming the
	// directory. Pin the message's own words, not just the path riding
	// along inside the wrapped error.
	if !strings.HasPrefix(err.Error(), "no local certificate at "+wantCert) {
		t.Fatalf("the error does not name the missing leaf file %q: %v", wantCert, err)
	}
}

func TestResolveLocalCAFindsTheLeafOnceInitHasRun(t *testing.T) {
	d, _ := initCA(t)
	certFile, keyFile := TLSOptions{Source: TLSLocalCA, CADir: d.Path}.mustResolve(t)
	wantCert, wantKey := d.LeafPaths()
	if certFile != wantCert || keyFile != wantKey {
		t.Fatalf("resolved %q %q, want %q %q", certFile, keyFile, wantCert, wantKey)
	}
}

func TestExpiryWarningFiresInsideFourteenDaysAndIsSilentOutside(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	near := &x509.Certificate{NotAfter: now.Add(10 * 24 * time.Hour)}
	far := &x509.Certificate{NotAfter: now.Add(40 * 24 * time.Hour)}
	if msg := ExpiryWarning(near, now); msg == "" || !strings.Contains(msg, "10 days") {
		t.Fatalf("warning for a 10-day certificate was %q", msg)
	}
	if msg := ExpiryWarning(far, now); msg != "" {
		t.Fatalf("warning for a 40-day certificate was %q", msg)
	}
}

func TestExpiryWarningFiresExactlyAtTheWindowBoundary(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	c := &x509.Certificate{NotAfter: now.Add(ExpiryWarnWindow)}
	if msg := ExpiryWarning(c, now); msg == "" {
		t.Fatal("a certificate at exactly the warning window was silent")
	}
}

// Under a day left, the warning reads in hours. "0 days" on the last
// afternoon reads as a bug, not a deadline.
func TestExpiryWarningRendersUnderADayInHours(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	c := &x509.Certificate{NotAfter: now.Add(time.Hour)}
	msg := ExpiryWarning(c, now)
	if !strings.Contains(msg, "1 hour") {
		t.Fatalf("warning for a 1-hour certificate was %q", msg)
	}
}

func TestExpiryWarningUsesSingularDayAtExactlyOneDay(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	c := &x509.Certificate{NotAfter: now.Add(24 * time.Hour)}
	msg := ExpiryWarning(c, now)
	if !strings.Contains(msg, "1 day.") {
		t.Fatalf("warning for a 1-day certificate was %q", msg)
	}
}

func TestExpiryWarningReportsAnExpiredCertificate(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	c := &x509.Certificate{NotAfter: now.Add(-time.Hour)}
	msg := ExpiryWarning(c, now)
	if !strings.Contains(msg, "expired") {
		t.Fatalf("warning for an expired certificate was %q", msg)
	}
}

// The failure this names: nothing ever drives RequireLoopback through
// Resolve itself, so a refactor of the off case could drop the guard and
// every existing test would still pass.
func TestResolveOffRefusesANonLoopbackListenAndAllowsLoopback(t *testing.T) {
	if _, _, err := (TLSOptions{Source: TLSOff, Listen: "100.64.1.2:8443"}).Resolve(); err == nil {
		t.Fatal("--tls off accepted a non-loopback listen address")
	}
	if _, _, err := (TLSOptions{Source: TLSOff, Listen: "127.0.0.1:8443"}).Resolve(); err != nil {
		t.Fatalf("--tls off refused a loopback listen address: %v", err)
	}
}

// The failure this names: plain HTTP on a tailnet address would put every
// pane on the box on the wire in the clear. --tls off exists for loopback
// and must refuse anything else.
func TestPlainHTTPIsRefusedOnAnyAddressThatIsNotLoopback(t *testing.T) {
	for _, listen := range []string{":8443", "0.0.0.0:8443", "100.64.1.2:8443", "[::]:8443"} {
		if err := RequireLoopback(listen); err == nil {
			t.Errorf("--tls off accepted %q", listen)
		}
	}
	for _, listen := range []string{"127.0.0.1:8443", "localhost:8443", "[::1]:8443"} {
		if err := RequireLoopback(listen); err != nil {
			t.Errorf("--tls off refused loopback %q: %v", listen, err)
		}
	}
}

func (o TLSOptions) mustResolve(t *testing.T) (string, string) {
	t.Helper()
	certFile, keyFile, err := o.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return certFile, keyFile
}

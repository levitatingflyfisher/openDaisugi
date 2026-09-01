package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/web"
)

func TestWebCertInitCreatesTheCAAndPrintsAQR(t *testing.T) {
	dataDir := t.TempDir()
	code, out, errb := runCLI(t, "/nonexistent.sock", dataDir,
		"web", "cert", "init", "--name", "localhost", "--ip", "127.0.0.1")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out+errb)
	}
	for _, name := range []string{"ca.crt", "ca.key", "leaf.crt", "leaf.key", "meta.json"} {
		if _, err := os.Stat(filepath.Join(web.LocalCADir(dataDir), name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if !strings.ContainsAny(out, "█▀▄") {
		t.Errorf("cert init printed no QR:\n%s", out)
	}
	if !strings.Contains(out, "/ca.crt") {
		t.Errorf("cert init did not print the CA download URL:\n%s", out)
	}
}

// The failure this names: the default: branch of the --qr switch drew the
// URL QR for any value it did not recognize, so a typo like --qr banana was
// silently accepted as if it were --qr url. A refusal here must land before
// the CA is written, since a run that cannot proceed should leave nothing
// behind for an operator to find later.
func TestWebCertInitRefusesAnUnrecognisedQRValue(t *testing.T) {
	dataDir := t.TempDir()
	code, out, errb := runCLI(t, "/nonexistent.sock", dataDir,
		"web", "cert", "init", "--name", "localhost", "--ip", "127.0.0.1", "--qr", "banana")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, out+errb)
	}
	for _, want := range []string{"url", "pem", "off"} {
		if !strings.Contains(out+errb, want) {
			t.Errorf("the refusal does not name %q:\n%s", want, out+errb)
		}
	}
	if _, err := os.Stat(filepath.Join(web.LocalCADir(dataDir), "ca.crt")); err == nil {
		t.Fatal("a refused --qr value still left a CA on disk")
	}
}

// The failure this names: an operator who has not run init must be told the
// command, not shown a stack trace.
func TestWebCertShowTeachesInitWhenThereIsNoCA(t *testing.T) {
	code, out, errb := runCLI(t, "/nonexistent.sock", t.TempDir(), "web", "cert", "show")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, out+errb)
	}
	if !strings.Contains(out+errb, "coppice web cert init") {
		t.Errorf("the message does not teach the command:\n%s", out+errb)
	}
}

func TestWebCertShowListsWhatTheLeafCovers(t *testing.T) {
	dataDir := t.TempDir()
	runCLI(t, "/nonexistent.sock", dataDir, "web", "cert", "init", "--name", "localhost", "--ip", "127.0.0.1")
	code, out, errb := runCLI(t, "/nonexistent.sock", dataDir, "web", "cert", "show")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out+errb)
	}
	for _, want := range []string{"localhost", "127.0.0.1", "expires"} {
		if !strings.Contains(out, want) {
			t.Errorf("cert show did not mention %q:\n%s", want, out)
		}
	}
}

// The flags are ours, not tailscale's own defaults: what tailscale names a
// file when nothing tells it was never checked, so this never leans on it.
// PATH is set to the fake alone, so the real tailscale on this box, if any,
// is unreachable from this test.
func TestWebCertTailscalePassesExplicitCertAndKeyPaths(t *testing.T) {
	dataDir := t.TempDir()
	fakeDir := t.TempDir()
	argvFile := filepath.Join(fakeDir, "argv.txt")
	script := "#!/bin/sh\necho \"$@\" > \"" + argvFile + "\"\n" +
		"for a in \"$@\"; do case \"$a\" in --cert-file=*) echo cert > \"${a#--cert-file=}\";; --key-file=*) echo key > \"${a#--key-file=}\";; esac; done\n"
	if err := os.WriteFile(filepath.Join(fakeDir, "tailscale"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeDir)

	code, out, errb := runCLI(t, "/nonexistent.sock", dataDir, "web", "cert", "tailscale", "box.tail1234.ts.net")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out+errb)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--cert-file=", "--key-file=", "box.tail1234.ts.net"} {
		if !strings.Contains(string(argv), want) {
			t.Errorf("tailscale was called as %q, missing %q", argv, want)
		}
	}
}

// coppice web cert tailscale writes to the exact pair TailscalePaths names,
// so coppice web serve --tls tailscale finds them with no extra flags.
func TestWebCertTailscaleWritesTheSharedPairByDefault(t *testing.T) {
	dataDir := t.TempDir()
	fakeDir := t.TempDir()
	script := "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in " +
		"--cert-file=*) echo cert > \"${a#--cert-file=}\";; --key-file=*) echo key > \"${a#--key-file=}\";; esac; done\n"
	if err := os.WriteFile(filepath.Join(fakeDir, "tailscale"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeDir)

	code, out, errb := runCLI(t, "/nonexistent.sock", dataDir, "web", "cert", "tailscale", "box.tail1234.ts.net")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out+errb)
	}
	wantCert, wantKey := web.TailscalePaths(dataDir)
	for _, p := range []string{wantCert, wantKey} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func TestWebWithRemoteIsRefused(t *testing.T) {
	code, _, errb := runCLI(t, "/nonexistent.sock", t.TempDir(), "--remote", "ssh://host", "web", "cert", "show")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if strings.Contains(errb, "unknown command") {
		t.Fatalf("web fell through to the unknown-command path instead of refusing --remote: %q", errb)
	}
	if !strings.Contains(errb, "does not take --remote") {
		t.Errorf("the refusal does not name --remote: %q", errb)
	}
}

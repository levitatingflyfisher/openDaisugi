package cli

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/web"
)

// run drives one CLI invocation with a fresh *CLI over dataDir, the same
// data directory every call in a test shares, so settings a first call
// persists are what a later call reads back. COPPICE_NO_AUTOSTART keeps
// runCLI from spawning a real coppice-server: nothing web serve or web
// token does here needs one.
func run(t *testing.T, dataDir string, argv ...string) (string, int) {
	t.Helper()
	code, out, errb := runCLI(t, filepath.Join(dataDir, "server.sock"), dataDir, argv...)
	return out + errb, code
}

// runWithStubServe drives one CLI invocation the same way run does, except
// webServeCommand's own call into a real Serve is replaced by stub. Use it
// for any web serve invocation whose configuration is expected to resolve
// cleanly, since without a stub that run would go on to bind a real
// listener and block until this test's own timeout.
func runWithStubServe(t *testing.T, dataDir string, stub func(context.Context, web.Config, web.Options) error, argv ...string) (string, int) {
	t.Helper()
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	var out, errb bytes.Buffer
	c := &CLI{
		Version: "test", Socket: filepath.Join(dataDir, "server.sock"), DataDir: dataDir,
		In: strings.NewReader(""), Out: &out, Err: &errb,
		serveWeb: stub,
	}
	code := c.Run(argv)
	return out.String() + errb.String(), code
}

// noopServe stands in for web.Serve in a test whose whole point is what
// happens before Serve is ever called: it never binds anything, so a
// success it should not reach cannot turn into a hang.
func noopServe(context.Context, web.Config, web.Options) error { return nil }

// A control that does nothing is a lie. Web Push is not built, so the flag
// says so and names the path that is. --listen names a loopback address
// purely as a well-formed value; --web-push refuses before anything is ever
// bound, so this needs no stub.
func TestWebPushFlagRefusesAndNamesNtfy(t *testing.T) {
	out, code := run(t, t.TempDir(), "web", "serve", "--web-push", "--tls", "off", "--listen", "127.0.0.1:0")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, out)
	}
	if !strings.Contains(out, "ntfy") {
		t.Errorf("the message does not name ntfy:\n%s", out)
	}
	if !strings.Contains(out, "not built") {
		t.Errorf("the message does not say the feature is not built:\n%s", out)
	}
}

// 0.0.0.0:0 is never bound: RequireLoopback refuses the host before Serve
// is even reached, so this needs no stub. The failure this second half
// names: a refused configuration must never print a token or a QR, since
// neither can mean anything for a phone server that is not going to run.
func TestWebServeRefusesPlainHTTPOffLoopbackFromTheCLI(t *testing.T) {
	out, code := run(t, t.TempDir(), "web", "serve", "--tls", "off", "--listen", "0.0.0.0:0")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, out)
	}
	if !strings.Contains(out, "127.0.0.1") && !strings.Contains(out, "localca") {
		t.Errorf("the message teaches neither loopback nor localca:\n%s", out)
	}
	if strings.ContainsAny(out, "█▀▄") {
		t.Errorf("a refused configuration still drew a QR:\n%s", out)
	}
	if strings.Contains(out, "sign in") || strings.Contains(out, "Minted") {
		t.Errorf("a refused configuration still printed a token or sign-in line:\n%s", out)
	}
}

// The failure this names: a refusal that arrives after --persist has
// already written the file leaves a coppice server retrying a
// configuration that never worked. 0.0.0.0:0 is never bound here either.
func TestARefusedConfigurationIsNeverPersisted(t *testing.T) {
	dataDir := t.TempDir()
	out, code := run(t, dataDir, "web", "serve", "--tls", "off", "--listen", "0.0.0.0:0", "--persist")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, out)
	}
	if _, err := os.Stat(web.ConfigPath(dataDir)); err == nil {
		t.Fatal("a refused configuration was still persisted")
	}
}

func TestWebTokenMintsOnFirstRunAndPrintsTheURL(t *testing.T) {
	dataDir := t.TempDir()
	out, code := run(t, dataDir, "web", "token", "--url", "https://box.tail1234.ts.net:8443")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if _, err := os.Stat(web.TokenPath(dataDir)); err != nil {
		t.Fatalf("no token file: %v", err)
	}
	if !strings.Contains(out, "https://box.tail1234.ts.net:8443/#t=") {
		t.Errorf("token did not print a scannable URL:\n%s", out)
	}
	if !strings.ContainsAny(out, "█▀▄") {
		t.Errorf("token printed no QR:\n%s", out)
	}
}

// The failure this names: a web.json that fails to parse must not stop
// coppice web token from printing a usable sign-in URL. It warns and
// carries on with a guessed address instead of exiting.
func TestWebTokenWarnsOnAMalformedConfigAndCarriesOn(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(web.ConfigPath(dataDir)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(web.ConfigPath(dataDir), []byte(`{"enabled": true, "listen": 8443}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code := run(t, dataDir, "web", "token", "--qr=false")
	if code != 0 {
		t.Fatalf("exit %d, want 0: %s", code, out)
	}
	if !strings.Contains(out, "could not read the saved settings") {
		t.Errorf("the output does not warn about the malformed config:\n%s", out)
	}
	if !strings.Contains(out, ":8443/#t=") {
		t.Errorf("the sign-in URL did not fall back to :8443:\n%s", out)
	}
}

// The failure this names: an https URL printed for a plain-HTTP listener is
// a QR that cannot connect, and the operator has no way to see why. --tls
// off only needs a loopback listen to resolve; --tls localca needs a real
// local CA, issued below, to resolve. Both calls use the stub, since both
// configurations now resolve cleanly and would otherwise reach a real
// Serve and block.
func TestTheSignInURLFollowsTheTLSSource(t *testing.T) {
	dataDir := t.TempDir()

	runWithStubServe(t, dataDir, noopServe, "web", "serve", "--tls", "off", "--listen", "127.0.0.1:0", "--persist")
	out, _ := run(t, dataDir, "web", "token", "--qr=false")
	if !strings.Contains(out, "http://") || strings.Contains(out, "https://") {
		t.Fatalf("with --tls off saved, the sign-in URL was not plain http:\n%s", out)
	}

	run(t, dataDir, "web", "cert", "init", "--name", "localhost", "--ip", "127.0.0.1")
	runWithStubServe(t, dataDir, noopServe, "web", "serve", "--tls", "localca", "--listen", "127.0.0.1:0", "--persist")
	out, _ = run(t, dataDir, "web", "token", "--qr=false")
	if !strings.Contains(out, "https://") {
		t.Fatalf("with --tls localca saved, the sign-in URL was not https:\n%s", out)
	}
}

// The failure this names: an operator who set up push and then turned
// autostart off would find their ntfy settings gone, with nothing having
// said so. --tls off with a loopback listen resolves cleanly with no CA
// needed, so the first call uses the stub; --forget itself never reaches
// Serve at all, so it needs none.
func TestForgetKeepsEverySettingExceptEnabled(t *testing.T) {
	dataDir := t.TempDir()
	runWithStubServe(t, dataDir, noopServe, "web", "serve", "--tls", "off", "--listen", "127.0.0.1:0", "--persist",
		"--ntfy", "https://ntfy.box.local", "--ntfy-topic", "coppice",
		"--ntfy-token-env", "COPPICE_NTFY_TOKEN")

	saved := web.ConfigPath(dataDir)
	body, err := os.ReadFile(saved)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "ntfy.box.local") {
		t.Fatalf("--persist did not save the ntfy URL:\n%s", body)
	}

	run(t, dataDir, "web", "serve", "--forget")
	body, err = os.ReadFile(saved)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "ntfy.box.local") {
		t.Fatalf("--forget threw away the ntfy URL:\n%s", body)
	}
	if !strings.Contains(string(body), web.GateRoot(dataDir)) {
		t.Fatalf("--forget threw away the gate root:\n%s", body)
	}
	if !strings.Contains(string(body), `"enabled": false`) {
		t.Fatalf("--forget did not turn autostart off:\n%s", body)
	}
}

// --tls off with a loopback listen resolves cleanly with no CA needed, so
// this uses the stub rather than reaching a real Serve.
func TestGateRootIsConfigurableAndSaved(t *testing.T) {
	dataDir := t.TempDir()
	root := filepath.Join(dataDir, "elsewhere", "gate")
	runWithStubServe(t, dataDir, noopServe, "web", "serve", "--tls", "off", "--listen", "127.0.0.1:0",
		"--persist", "--gate-root", root)
	body, err := os.ReadFile(web.ConfigPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "elsewhere") {
		t.Fatalf("--gate-root was not saved:\n%s", body)
	}
}

// The failure this names: a phone with no --voice-url ever reaching the
// server means the record button gets no address to send a clip to, and a
// coppice server restarted by AutoStart instead of by hand has only web.json
// to read that address back from.
func TestVoiceURLIsConfigurableSavedAndPassedToOptions(t *testing.T) {
	dataDir := t.TempDir()
	var got web.Options
	stub := func(_ context.Context, _ web.Config, opts web.Options) error {
		got = opts
		return nil
	}
	runWithStubServe(t, dataDir, stub, "web", "serve", "--tls", "off", "--listen", "127.0.0.1:0",
		"--persist", "--voice-url", "http://127.0.0.1:7477")
	if got.VoiceURL != "http://127.0.0.1:7477" {
		t.Fatalf("Options.VoiceURL was %q, want http://127.0.0.1:7477", got.VoiceURL)
	}
	body, err := os.ReadFile(web.ConfigPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "http://127.0.0.1:7477") {
		t.Fatalf("--voice-url was not saved:\n%s", body)
	}
}

// The failure this names: the address is written everywhere else in this
// command, and in this very how-to, as a bare host:port. An operator who
// writes --voice-url the same way as --listen or --ca-listen gets a value
// that saves cleanly and then answers every tap of Record with a sentence
// naming no command and no address. Refusing it here, before web.json is
// ever touched, catches the typo at the terminal instead of on the phone.
func TestVoiceURLRefusesAnythingThatIsNotAnAbsoluteHTTPURL(t *testing.T) {
	for _, bad := range []string{
		"127.0.0.1:7477", "htp://127.0.0.1:7477", "localhost:7477", "not a url at all",
	} {
		dataDir := t.TempDir()
		out, code := run(t, dataDir, "web", "serve", "--tls", "off", "--listen", "127.0.0.1:0",
			"--persist", "--voice-url", bad)
		if code != 1 {
			t.Fatalf("--voice-url %q: exit %d, want 1: %s", bad, code, out)
		}
		if !strings.Contains(out, "--voice-url needs an http:// or https:// address") {
			t.Fatalf("--voice-url %q: the message does not name the fix:\n%s", bad, out)
		}
		if _, err := os.Stat(web.ConfigPath(dataDir)); err == nil {
			t.Fatalf("--voice-url %q: a refused value was still persisted", bad)
		}
	}
}

func TestWebTokenShowsTheSameTokenUntilRotated(t *testing.T) {
	dataDir := t.TempDir()
	first, _ := run(t, dataDir, "web", "token", "--qr=false")
	second, _ := run(t, dataDir, "web", "token", "--qr=false")
	if first != second {
		t.Fatal("a second coppice web token minted a new token instead of showing the current one")
	}
	third, _ := run(t, dataDir, "web", "token", "--qr=false", "--rotate")
	if third == second {
		t.Fatal("--rotate did not change the token")
	}
}

// The failure this names: an operator who saved a non-default listen and
// external URL runs web token later, from a fresh process with no flags,
// and must see that saved address rather than the flag's own unrelated
// default.
func TestWebTokenFallsBackToTheSavedListenAndExternalURL(t *testing.T) {
	dataDir := t.TempDir()

	addr := freeAddrForTest(t)
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	runWithStubServe(t, dataDir, noopServe, "web", "serve", "--tls", "off", "--listen", addr, "--persist")
	out, code := run(t, dataDir, "web", "token", "--qr=false")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.Contains(out, ":"+port+"/#t=") {
		t.Fatalf("web token did not fall back to the saved listen port %s:\n%s", port, out)
	}

	addr2 := freeAddrForTest(t)
	runWithStubServe(t, dataDir, noopServe, "web", "serve", "--tls", "off", "--listen", addr2,
		"--external-url", "https://box.tail1234.ts.net:9443", "--persist")
	out, code = run(t, dataDir, "web", "token", "--qr=false")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.Contains(out, "https://box.tail1234.ts.net:9443/#t=") {
		t.Fatalf("web token did not use the saved external url:\n%s", out)
	}
}

// coppice web cert tailscale always writes to the same pair TailscalePaths
// names. --tls tailscale with no explicit --cert or --key must read that
// same pair back, so an operator who ran cert tailscale first does not also
// have to type the paths back in by hand. The files here only have to
// exist for Resolve to accept them; the stub means Serve, which would parse
// them as a certificate, is never reached.
func TestServeDefaultsTailscaleCertAndKeyToTheSharedPair(t *testing.T) {
	dataDir := t.TempDir()
	wantCert, wantKey := web.TailscalePaths(dataDir)
	if err := os.MkdirAll(filepath.Dir(wantCert), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wantCert, []byte("stub"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wantKey, []byte("stub"), 0o600); err != nil {
		t.Fatal(err)
	}
	runWithStubServe(t, dataDir, noopServe, "web", "serve", "--tls", "tailscale", "--listen", "127.0.0.1:0", "--persist")
	body, err := os.ReadFile(web.ConfigPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), wantCert) || !strings.Contains(string(body), wantKey) {
		t.Fatalf("--tls tailscale with no --cert or --key did not default to %s / %s:\n%s",
			wantCert, wantKey, body)
	}
}

// The failure this names: a config with only one of --cert or --key
// persisted would fail later, from AutoStart, with a message naming flags
// rather than the file the operator is looking at in web.json. Refusing up
// front, before persisting, means that file is never written half set. This
// is refused before Resolve is even reached, so it needs no stub.
func TestServeTailscaleRefusesOneOfCertOrKeyAlone(t *testing.T) {
	dataDir := t.TempDir()
	out, code := run(t, dataDir, "web", "serve", "--tls", "tailscale", "--listen", "127.0.0.1:0",
		"--cert", filepath.Join(dataDir, "only.crt"), "--persist")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, out)
	}
	if !strings.Contains(out, "--cert") || !strings.Contains(out, "--key") {
		t.Fatalf("the message does not name both flags:\n%s", out)
	}
	if _, err := os.Stat(web.ConfigPath(dataDir)); err == nil {
		t.Fatal("a tailscale config with only one of --cert or --key was still persisted")
	}
}

// The other half of the same refusal: --key alone, with no --cert, must be
// refused the same way --cert alone is above. Only one direction of the XOR
// had a test.
func TestServeTailscaleRefusesKeyAloneToo(t *testing.T) {
	dataDir := t.TempDir()
	out, code := run(t, dataDir, "web", "serve", "--tls", "tailscale", "--listen", "127.0.0.1:0",
		"--key", filepath.Join(dataDir, "only.key"), "--persist")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, out)
	}
	if !strings.Contains(out, "--cert") || !strings.Contains(out, "--key") {
		t.Fatalf("the message does not name both flags:\n%s", out)
	}
	if _, err := os.Stat(web.ConfigPath(dataDir)); err == nil {
		t.Fatal("a tailscale config with only one of --cert or --key was still persisted")
	}
}

// --tls files must keep its own refusal naming --cert and --key. A tailscale
// default filling in for a blank --cert here would replace that message with
// a confusing "no such file" pointing at a tailscale path nobody asked for.
// This is refused by Resolve before Serve is reached, so it needs no stub.
func TestServeFilesStillNeedsExplicitCertAndKey(t *testing.T) {
	out, code := run(t, t.TempDir(), "web", "serve", "--tls", "files", "--listen", "127.0.0.1:0")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, out)
	}
	if !strings.Contains(out, "--cert") || !strings.Contains(out, "--key") {
		t.Fatalf("the message does not name --cert and --key:\n%s", out)
	}
}

// The wiring assertion: AutoStart is only useful if the coppice server
// actually calls it, and no unit test in package web can see that. This
// reads cli.go by name rather than globbing the package directory, since a
// glob over "*.go" also matches this very test file, which necessarily
// holds the literal string this test is looking for.
func TestTheCoppiceServerCallsWebAutoStart(t *testing.T) {
	body, err := os.ReadFile("cli.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "web.AutoStart(") {
		t.Fatal("cli.go does not call web.AutoStart, so web.enabled would never start anything")
	}
}

// freeAddrForTest asks the operating system for a loopback port nothing is
// using, then gives it back. Used here only to pick a distinguishable,
// never-colliding value for an address that a stub means nothing ever
// binds, so a literal number is not the risk it would be for a real listen.
func freeAddrForTest(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

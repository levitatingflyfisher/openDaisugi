package server

import (
	"os"
	"testing"

	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/testhome"
)

// A test server resolves no path under the operator's real home: not its
// socket, data, warden root, hook home or steward directory, nor the
// defaults it falls back to. TestMain moves HOME and the XDG directories
// into scratch; this fails if that ever stops holding.
func TestATestServerResolvesNoPathUnderTheRealHome(t *testing.T) {
	if testhome.RealHome() == "" {
		t.Fatal("TestMain did not isolate the home")
	}
	if h, _ := os.UserHomeDir(); testhome.UnderRealHome(h) {
		t.Fatalf("HOME is still the real home: %s", h)
	}
	if v := os.Getenv("OPENDAISUGI_HOME"); v == "" || v != testhome.Canary() {
		t.Fatalf("OPENDAISUGI_HOME is %q, want the canary %q", v, testhome.Canary())
	}
	for _, k := range []string{"COPPICE_DATA_DIR", "CLAUDE_CONFIG_DIR"} {
		if v, set := os.LookupEnv(k); set {
			t.Fatalf("%s is still set: %q", k, v)
		}
	}
	s := newTestServer(t)
	paths := map[string]string{
		"socket": s.cfg.SocketPath, "data": s.cfg.DataDir, "warden root": s.cfg.WardenRoot,
		"hook home": s.cfg.HookHome, "steward": s.cfg.StewardDir,
		"default socket": SocketPath(), "default data": DataDir(), "default steward": StewardDir(),
		"coppice.toml": config.Path(),
	}
	for name, p := range paths {
		if testhome.UnderRealHome(p) {
			t.Errorf("%s %s is under the real home", name, p)
		}
	}
	// A server made with only a socket and a data dir, as many tests make
	// one, falls back to the scratch home too.
	dir := t.TempDir()
	bare, err := New(Config{SocketPath: dir + "/s.sock", DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bare.Close() }()
	if testhome.UnderRealHome(bare.cfg.HookHome) || testhome.UnderRealHome(bare.cfg.StewardDir) {
		t.Fatalf("a bare test server reads the real home: hook home %s, steward %s", bare.cfg.HookHome, bare.cfg.StewardDir)
	}
}

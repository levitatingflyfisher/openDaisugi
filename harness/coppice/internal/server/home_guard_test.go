package server

import (
	"os"
	"testing"

	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/testhome"
)

// A test server resolves no path under the operator's real home: not its
// socket, data, gate root, hook home or foreman directory, nor the
// defaults it falls back to. TestMain moves HOME and the XDG directories
// into scratch; this fails if that ever stops holding.
func TestATestServerResolvesNoPathUnderTheRealHome(t *testing.T) {
	if testhome.RealHome() == "" {
		t.Fatal("TestMain did not isolate the home")
	}
	if h, _ := os.UserHomeDir(); testhome.UnderRealHome(h) {
		t.Fatalf("HOME is still the real home: %s", h)
	}
	s := newTestServer(t)
	paths := map[string]string{
		"socket": s.cfg.SocketPath, "data": s.cfg.DataDir, "gate root": s.cfg.GateRoot,
		"hook home": s.cfg.HookHome, "foreman": s.cfg.ForemanDir,
		"default socket": SocketPath(), "default data": DataDir(), "default foreman": ForemanDir(),
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
	if testhome.UnderRealHome(bare.cfg.HookHome) || testhome.UnderRealHome(bare.cfg.ForemanDir) {
		t.Fatalf("a bare test server reads the real home: hook home %s, foreman %s", bare.cfg.HookHome, bare.cfg.ForemanDir)
	}
}

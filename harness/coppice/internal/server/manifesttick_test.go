package server

import (
	"path/filepath"
	"testing"

	"github.com/opendaisugi/coppice/internal/detect"
)

// ManifestTickRunning is the honest way to prove a tick is actually running,
// rather than trusting that Close does not hang either way.
func TestManifestTickRunningReflectsStartAndClose(t *testing.T) {
	dir := t.TempDir()
	s, err := New(Config{SocketPath: filepath.Join(dir, "server.sock"), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if s.ManifestTickRunning() {
		t.Fatal("ManifestTickRunning is true before StartManifestTick")
	}

	set, err := detect.LoadSet("")
	if err != nil {
		t.Fatal(err)
	}
	s.StartManifestTick(set)

	if !s.ManifestTickRunning() {
		t.Fatal("ManifestTickRunning is false after StartManifestTick")
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s.ManifestTickRunning() {
		t.Fatal("ManifestTickRunning is true after Close")
	}
}

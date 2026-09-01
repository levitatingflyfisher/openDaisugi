package cli

import (
	"testing"

	"github.com/opendaisugi/coppice/internal/server"
)

// server stop must not declare victory the instant the socket refuses
// connections: Close releases the start lock only after it has torn every
// live pane down, well after it stops listening. A server start that races
// in right after server stop returns must not lose to a lock the old process
// has not actually released yet.
func TestServerStopWaitsForTheLockNotJustTheSocket(t *testing.T) {
	sock, dir := liveServer(t)
	cwd := t.TempDir()
	// A live pty pane gives Close's teardown real, nonzero work: without one,
	// Close can finish so fast that a socket-only poll never has a chance to
	// observe the gap between "socket closed" and "lock released".
	runCLI(t, sock, dir, "pane", "create", "--cwd", cwd, "--", "sh", "-c", "sleep 300")

	code, out, errb := runCLI(t, sock, dir, "server", "stop")
	if code != 0 {
		t.Fatalf("server stop exit %d: %s %s", code, out, errb)
	}

	probe, err := server.New(server.Config{SocketPath: sock, DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := probe.AcquireStartLock(); err != nil {
		t.Fatalf("server stop returned but the start lock is still held: %v", err)
	}
	_ = probe.Close()
}

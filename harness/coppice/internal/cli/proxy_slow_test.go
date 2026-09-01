package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/server"
)

// A --stdio caller sends one request, then half-closes: that is the whole
// point of Proxy's CloseWrite. This proves a handler that takes real,
// nonzero time still gets its reply through to a caller that already
// half-closed, with no pty and no big grid involved - just a handler that
// sleeps briefly before answering.
//
// A handler slower than this, and one that does not watch c.Dead(), used to
// be able to lose its reply the same way once past the server's old 2s
// handler-teardown bound. internal/server's own
// TestASlowHandlerRepliesAfterAPeerHalfCloses now covers that directly, at
// 4s, against the fixed teardown; this test stays a fast smoke check of the
// same path through the actual CLI --stdio command.
func TestProxyDeliversASlightlySlowHandlersReplyAfterCloseWrite(t *testing.T) {
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	dir := t.TempDir()
	sock := filepath.Join(dir, "server.sock")
	s, err := server.New(server.Config{SocketPath: sock, DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	if err := s.Handle("test.slow", func(_ *server.Client, r *proto.Request) proto.Response {
		time.Sleep(300 * time.Millisecond)
		return proto.OKResp(r.ID, map[string]any{"slow": true})
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()
	t.Cleanup(func() { _ = s.Close() })

	var out bytes.Buffer
	in := strings.NewReader(`{"id":"1","cmd":"test.slow"}` + "\n")
	c := &CLI{Version: "test", Socket: sock, DataDir: dir, In: in, Out: &out, Err: &bytes.Buffer{}}
	start := time.Now()
	if code := c.Run([]string{"--stdio"}); code != 0 {
		t.Fatalf("--stdio exit %d after %s: %s", code, time.Since(start), out.String())
	}
	if !strings.Contains(out.String(), `"slow":true`) {
		t.Fatalf("--stdio reply after %s = %q, want the slow handler's own reply", time.Since(start), out.String())
	}
}

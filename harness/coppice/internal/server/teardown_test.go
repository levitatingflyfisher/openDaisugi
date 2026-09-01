package server

import (
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/proto"
)

// A --stdio caller sends one request, then half-closes, then waits: that is
// the whole point of Proxy's CloseWrite. A handler that takes real, nonzero
// time to answer - and does not watch c.Dead() - must still get its reply
// through, even though the peer already signalled it has nothing more to
// send. This is the report's own open concern, reproduced directly against
// ServeConn rather than through a pty or a big grid: a bare handler that
// sleeps 4s, well past the old 2s handlerTeardownWait, then answers.
func TestASlowHandlerRepliesAfterAPeerHalfCloses(t *testing.T) {
	s := newTestServer(t)
	_ = s.Handle("test.slow", func(_ *Client, r *proto.Request) proto.Response {
		time.Sleep(4 * time.Second)
		return proto.OKResp(r.ID, map[string]any{"slow": true})
	})
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()

	conn, err := net.Dial("unix", s.cfg.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, `{"id":"1","cmd":"test.slow"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		t.Fatal("expected a unix connection")
	}
	if err := uc.CloseWrite(); err != nil {
		t.Fatal(err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(6 * time.Second))
	line, err := proto.NewDecoder(conn).Next()
	if err != nil {
		t.Fatalf("did not receive the slow handler's reply: %v", err)
	}
	if !strings.Contains(string(line), `"slow":true`) {
		t.Fatalf("reply = %q, want the slow handler's own reply", line)
	}
}

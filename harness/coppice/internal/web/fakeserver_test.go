package web

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"sync"
	"testing"
)

// fakeServer is a coppice-server that only speaks JSONL. Every test in this
// package runs against it, so the web package never needs the real server's
// internals to be finished, and the parity claim, the browser's bytes are
// the socket's bytes, is checkable at the byte level.
type fakeServer struct {
	mu       sync.Mutex
	received [][]byte // exactly the bytes each request line arrived as
	path     string
}

// newFakeServer listens on a unix socket and answers each request line with
// whatever reply returns. A nil reply answers nothing, which is what an
// event only subscription looks like.
func newFakeServer(t *testing.T, reply func(req map[string]any) []map[string]any) (*fakeServer, Dialer) {
	t.Helper()
	f := &fakeServer{path: filepath.Join(t.TempDir(), "server.sock")}
	ln, err := net.Listen("unix", f.path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn, reply)
		}
	}()
	return f, UnixDialer{Path: f.path}
}

func (f *fakeServer) serve(conn net.Conn, reply func(map[string]any) []map[string]any) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), MaxLineBytes)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		f.mu.Lock()
		f.received = append(f.received, line)
		f.mu.Unlock()
		var req map[string]any
		if json.Unmarshal(line, &req) != nil || reply == nil {
			continue
		}
		for _, msg := range reply(req) {
			body, _ := json.Marshal(msg)
			conn.Write(append(body, '\n'))
		}
	}
}

// Received returns a copy of every request line seen so far, in order.
func (f *fakeServer) Received() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.received))
	copy(out, f.received)
	return out
}

// echoOK answers any request with {"id":…,"ok":true,"result":{"cmd":<cmd>}}.
func echoOK(req map[string]any) []map[string]any {
	return []map[string]any{{"id": req["id"], "ok": true, "result": map[string]any{"cmd": req["cmd"]}}}
}

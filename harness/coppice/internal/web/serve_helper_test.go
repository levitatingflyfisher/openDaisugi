package web

import (
	"net"
	"net/http"
	"testing"
	"time"
)

// freeAddr asks the operating system for a loopback port nothing is using,
// then gives it back. A test that needs a real address to bind, rather than
// one that must be refused before any bind is attempted, uses this instead
// of a literal port number, so two test runs on the same box never collide.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func waitForPort(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("nothing listening on %s after 5 s", addr)
}

// assertNothingListening gives a listener that should not exist half a
// second to appear, then fails if it did.
func assertNothingListening(t *testing.T, addr, when string) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			t.Fatalf("something is listening on %s %s", addr, when)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func getStatus(t *testing.T, url, token string) int {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

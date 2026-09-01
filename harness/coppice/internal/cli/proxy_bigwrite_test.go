package cli

import (
	"bufio"
	"bytes"
	"net"
	"path/filepath"
	"strings"
	"testing"
)

// The teardown write deadline must not truncate a large reply to a
// half-closed reader. A fake unix server - no server.Server, no toolchain
// skip - reads one line, waits for the client's own EOF, then writes 1 MiB
// after that half-close. The client must still receive every byte.
func TestProxyReceivesALateOneMiBWriteAfterTheClientHalfCloses(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "server.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	const size = 1 << 20
	payload := bytes.Repeat([]byte("x"), size)

	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		if _, err := br.ReadString('\n'); err != nil {
			return
		}
		// Wait for the client's own half-close before writing anything back.
		buf := make([]byte, 1)
		for {
			if _, err := br.Read(buf); err != nil {
				break
			}
		}
		_, _ = conn.Write(payload)
	}()

	in := strings.NewReader("hello\n")
	var out bytes.Buffer
	if err := Proxy(sock, dir, in, &out); err != nil {
		t.Fatalf("Proxy error: %v", err)
	}
	<-accepted
	if out.Len() != size {
		t.Fatalf("received %d bytes, want %d", out.Len(), size)
	}
	if !bytes.Equal(out.Bytes(), payload) {
		t.Fatal("received bytes do not match the payload written")
	}
}

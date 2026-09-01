//go:build linux

package web

import (
	"net"
	"os"
	"testing"
)

// A loopback connection from this process is found in /proc and names this
// process.
func TestALoopbackPeerIsFoundByItsSocket(t *testing.T) {
	for _, network := range []string{"tcp4", "tcp6"} {
		addr := "127.0.0.1:0"
		if network == "tcp6" {
			addr = "[::1]:0"
		}
		ln, err := net.Listen(network, addr)
		if err != nil {
			t.Logf("%s: %v", network, err)
			continue
		}
		defer ln.Close()
		accepted := make(chan net.Conn, 1)
		go func() {
			c, err := ln.Accept()
			if err == nil {
				accepted <- c
			}
		}()
		client, err := net.Dial(network, ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		srv := <-accepted
		defer srv.Close()
		pids, local, err := localPeerPIDs(srv.RemoteAddr(), srv.LocalAddr())
		if err != nil || !local {
			t.Fatalf("%s: pids %v local %v err %v", network, pids, local, err)
		}
		found := false
		for _, p := range pids {
			found = found || p == os.Getpid()
		}
		if !found {
			t.Fatalf("%s: pids %v do not hold this process %d", network, pids, os.Getpid())
		}
	}
}

func TestAPeerOnAnotherHostIsNotLocal(t *testing.T) {
	remote := &net.TCPAddr{IP: net.ParseIP("203.0.113.5"), Port: 40000}
	lcl := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8443}
	pids, local, err := localPeerPIDs(remote, lcl)
	if local || err != nil || len(pids) != 0 {
		t.Fatalf("pids %v local %v err %v", pids, local, err)
	}
}

// A loopback address with no socket behind it is local and cannot be
// placed, which is an error, so the caller fails closed.
func TestALocalPeerWithNoSocketIsAnError(t *testing.T) {
	remote := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1}
	lcl := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 2}
	_, local, err := localPeerPIDs(remote, lcl)
	if !local || err == nil {
		t.Fatalf("local %v err %v, want local and an error", local, err)
	}
}

func TestParseProcNetAddr(t *testing.T) {
	ip, port, err := parseProcAddr("0100007F:1F90")
	if err != nil || !ip.Equal(net.ParseIP("127.0.0.1")) || port != 8080 {
		t.Fatalf("%v %d %v", ip, port, err)
	}
	ip, port, err = parseProcAddr("00000000000000000000000001000000:0050")
	if err != nil || !ip.Equal(net.ParseIP("::1")) || port != 80 {
		t.Fatalf("%v %d %v", ip, port, err)
	}
}

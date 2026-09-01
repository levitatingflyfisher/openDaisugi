package cli

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// fakeSocket accepts one connection, reads two lines, answers the second
// with ok, and hands the two lines back.
func fakeSocket(t *testing.T) (string, <-chan []string) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "f.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	got := make(chan []string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		r := bufio.NewReader(conn)
		var lines []string
		for len(lines) < 2 {
			l, err := r.ReadString('\n')
			if err != nil {
				break
			}
			lines = append(lines, l)
			var req map[string]any
			if json.Unmarshal([]byte(l), &req) == nil && req["cmd"] == "pane.list" {
				id, _ := req["id"].(string)
				_, _ = conn.Write([]byte(`{"id":"` + id + `","ok":true,"result":{"panes":[]}}` + "\n"))
				break
			}
		}
		got <- lines
	}()
	return sock, got
}

func TestTheCLIInsideAPaneSaysHelloFirst(t *testing.T) {
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	t.Setenv("COPPICE_PANE", "w1:p1")
	sock, got := fakeSocket(t)
	c, err := Dial(sock, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Do("pane.list", nil); err != nil {
		t.Fatal(err)
	}
	lines := <-got
	if len(lines) != 2 {
		t.Fatalf("lines = %q, want a hello and the request", lines)
	}
	var hello map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &hello); err != nil {
		t.Fatal(err)
	}
	if hello["id"] != "0" || hello["cmd"] != "hello" || hello["role"] != "pane" || hello["pane"] != "w1:p1" {
		t.Fatalf("first line = %v", hello)
	}
}

func TestTheCLIOutsideAPaneSaysNoHello(t *testing.T) {
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	t.Setenv("COPPICE_PANE", "")
	sock, got := fakeSocket(t)
	c, err := Dial(sock, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Do("pane.list", nil); err != nil {
		t.Fatal(err)
	}
	lines := <-got
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if first["cmd"] != "pane.list" {
		t.Fatalf("first line = %v, want the request itself", first)
	}
}

// A pane that reaches another host says it is a pane there, but names no
// pane: its id means nothing on that host.
func TestTheHelloToARemoteHostNamesNoPane(t *testing.T) {
	t.Setenv("COPPICE_PANE", "w1:p1")
	h := helloParams(true)
	if h["role"] != "pane" {
		t.Fatalf("hello = %v", h)
	}
	if _, ok := h["pane"]; ok {
		t.Fatalf("the remote hello names a pane: %v", h)
	}
	if local := helloParams(false); local["pane"] != "w1:p1" {
		t.Fatalf("the local hello = %v", local)
	}
	t.Setenv("COPPICE_PANE", "")
	if helloParams(false) != nil || helloParams(true) != nil {
		t.Fatal("a hello outside a pane")
	}
}

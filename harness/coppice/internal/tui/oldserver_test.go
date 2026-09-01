package tui

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// oldServer answers like a server built before notes and children: it
// refuses a subscribe that names note or child, and it has no floor.notes.
func oldServer(t *testing.T) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "old.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					var req map[string]any
					if json.Unmarshal([]byte(line), &req) != nil {
						continue
					}
					id, _ := req["id"].(string)
					ok := func(res string) string { return `{"id":"` + id + `","ok":true,"result":` + res + "}\n" }
					bad := func(msg string) string {
						return `{"id":"` + id + `","ok":false,"error":{"code":"bad_request","message":"` + msg + `"}}` + "\n"
					}
					var out string
					switch req["cmd"] {
					case "events.subscribe":
						kinds, _ := json.Marshal(req["kinds"])
						if strings.Contains(string(kinds), "note") || strings.Contains(string(kinds), "child") {
							out = bad("kind is not state, layout or frame")
						} else {
							out = ok(`{"kinds":["state"],"panes":["*"]}`)
						}
					case "pane.list":
						out = ok(`{"panes":[{"id":"w1:p1","label":"old","state":"idle","closed":false}]}`)
					case "task.list":
						out = ok(`{"tasks":[]}`)
					default:
						out = bad("no command")
					}
					_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
					if _, err := c.Write([]byte(out)); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return sock
}

// A floor built with notes and children still draws against an older
// server that knows neither.
func TestTheFloorDrawsAgainstAServerWithNoNotes(t *testing.T) {
	sock := oldServer(t)
	var out strings.Builder
	err := Run(Options{Socket: sock, In: scripted("\x03"), Out: &out,
		Size: func() (int, int) { return 80, 24 }, Cwd: t.TempDir(), Default: "claude"})
	if err != nil {
		t.Fatalf("the floor did not open: %v", err)
	}
	if !strings.Contains(stripSGR(out.String()), "old") {
		t.Fatalf("the roster did not draw: %q", out.String())
	}
}

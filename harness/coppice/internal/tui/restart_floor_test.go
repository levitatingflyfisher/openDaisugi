package tui

import (
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/opendaisugi/coppice/internal/attach"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/tiles"
)

// fakeStatusServer answers server.status with restore, pane.list with no
// panes, and anything else with an empty result. It records every cmd.
func fakeStatusServer(t *testing.T, restore map[string]any) (socket string, cmds func() []string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "cf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket = filepath.Join(dir, "s")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var mu sync.Mutex
	var seen []string
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				line, err := proto.NewDecoder(conn).Next()
				if err != nil {
					return
				}
				var req proto.Request
				if err := json.Unmarshal(line, &req); err != nil {
					return
				}
				mu.Lock()
				seen = append(seen, req.Cmd)
				mu.Unlock()
				var result any = map[string]any{}
				switch req.Cmd {
				case "server.status":
					// A count outside restore must not matter.
					result = map[string]any{"resumable": 9, "restore": restore}
				case "pane.list":
					result = map[string]any{"panes": []any{}}
				case "pane.resume":
					result = map[string]any{"pane": "n1", "resumed": true}
				}
				_ = proto.NewEncoder(conn).Send(proto.OKResp(req.ID, result))
			}()
		}
	}()
	return socket, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func restartFloor(t *testing.T, restore map[string]any) (*floor, func() []string) {
	t.Helper()
	socket, cmds := fakeStatusServer(t, restore)
	m := &Model{Ended: []Row{{ID: "a", State: "done"}, {ID: "b", State: "done"}},
		Tiles: tiles.New(tiles.Focus, 1), Screens: map[string]*attach.Screen{}}
	return &floor{o: Options{Socket: socket, Size: func() (int, int) { return 100, 30 }}, m: m,
		fw: &frameWriter{out: io.Discard}, detaching: map[string]bool{}, sizes: map[string]Size{}, sizing: map[string]bool{}}, cmds
}

// The restart line reads restore.resumable only, and says nothing when no
// record the restart stopped can come back.
func TestSayRestartReadsTheRestartOnlyCount(t *testing.T) {
	f, _ := restartFloor(t, map[string]any{"marked_done": 2, "resumable": 2})
	f.sayRestart()
	if f.m.Restarted != 2 || f.m.Message != RestartLine(2) {
		t.Fatalf("Restarted = %d, Message = %q", f.m.Restarted, f.m.Message)
	}
	f, _ = restartFloor(t, map[string]any{"marked_done": 2, "resumable": 0})
	f.sayRestart()
	if f.m.Restarted != 0 || f.m.Message != "" {
		t.Fatalf("a restart with nothing to resume said %q", f.m.Message)
	}
}

// It never takes the place of a line already on the status line.
func TestSayRestartKeepsALineAlreadyThere(t *testing.T) {
	f, _ := restartFloor(t, map[string]any{"resumable": 2})
	f.m.Message = "cannot reach the server"
	f.sayRestart()
	if f.m.Message != "cannot reach the server" || f.m.Restarted != 0 {
		t.Fatalf("Message = %q, Restarted = %d", f.m.Message, f.m.Restarted)
	}
}

// R resumes through pane.resume and ends the line for the session.
func TestRKeyResumesAndEndsTheRestartLine(t *testing.T) {
	f, cmds := restartFloor(t, map[string]any{"resumable": 2})
	f.sayRestart()
	if _, err := f.key('R'); err != nil {
		t.Fatal(err)
	}
	resumes := 0
	for _, c := range cmds() {
		if c == "pane.resume" {
			resumes++
		}
	}
	if resumes != 2 {
		t.Fatalf("pane.resume ran %d times, want 2 (%v)", resumes, cmds())
	}
	if f.m.Restarted != 0 || strings.Contains(footerText(f.m), "R resume all") {
		t.Fatalf("Restarted = %d, footer = %q", f.m.Restarted, footerText(f.m))
	}
}

// Any other key clears the line and resumes nothing.
func TestAnyOtherKeyClearsTheRestartLine(t *testing.T) {
	f, cmds := restartFloor(t, map[string]any{"resumable": 2})
	f.sayRestart()
	if _, err := f.key('j'); err != nil {
		t.Fatal(err)
	}
	if f.m.Restarted != 0 || f.m.Message == RestartLine(2) {
		t.Fatalf("Restarted = %d, Message = %q", f.m.Restarted, f.m.Message)
	}
	for _, c := range cmds() {
		if c == "pane.resume" {
			t.Fatal("a key other than R resumed an agent")
		}
	}
}

// R is never offered while keys go to a window, and never acts there.
func TestRIsNotOfferedWhileTyping(t *testing.T) {
	m := &Model{Restarted: 2, Typing: "a", Leave: 0x1d}
	if strings.Contains(footerText(m), "R resume all") {
		t.Fatalf("footer = %q, offers R while typing", footerText(m))
	}
}

package tmuxmirror

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func transcript(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "tmux", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseAWindowCloseNamesTheWindow(t *testing.T) {
	for _, line := range []string{"%window-close @3", "%unlinked-window-close @3"} {
		evs, err := Parse(strings.NewReader(line + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		if len(evs) != 1 || evs[0].Kind != EventWindowClose || evs[0].Window != "@3" {
			t.Fatalf("%q parsed as %+v", line, evs)
		}
	}
}

func TestParseTheRecordedCloseTranscript(t *testing.T) {
	evs, err := Parse(strings.NewReader(transcript(t, "window-close.txt")))
	if err != nil {
		t.Fatal(err)
	}
	var closes, outputs, replies []Event
	var exit bool
	for _, e := range evs {
		switch e.Kind {
		case EventWindowClose:
			closes = append(closes, e)
		case EventOutput:
			outputs = append(outputs, e)
		case EventReply:
			replies = append(replies, e)
		case EventExit:
			exit = true
		}
	}
	if len(closes) != 2 || closes[0].Window != "@2" || closes[1].Window != "@3" {
		t.Fatalf("closes %+v", closes)
	}
	if len(outputs) != 1 || outputs[0].Pane != "%3" || outputs[0].Data != "hi" {
		t.Fatalf("outputs %+v", outputs)
	}
	// The first block answers the attach itself. Its flags are 0, so it is
	// not a reply to a command this client sent.
	if len(replies) != 3 || replies[0].Client || !replies[1].Client {
		t.Fatalf("replies %+v", replies)
	}
	if len(replies[1].Lines) != 1 || replies[1].Lines[0] != "@2" {
		t.Fatalf("new-window reply %+v", replies[1])
	}
	if !exit {
		t.Fatal("no exit event")
	}
}

func TestParseTheRecordedLifecycleTranscript(t *testing.T) {
	evs, err := Parse(strings.NewReader(transcript(t, "lifecycle.txt")))
	if err != nil {
		t.Fatal(err)
	}
	var client []Event
	for _, e := range evs {
		if e.Kind == EventReply && e.Client {
			client = append(client, e)
		}
	}
	if len(client) != 9 {
		t.Fatalf("%d client replies, want 9: %+v", len(client), client)
	}
	ws := ParseWindows(client[5].Lines)
	if len(ws) != 2 || ws[1] != (Window{ID: "@1", Name: "b'x;y#z", Pane: "w1:p1"}) {
		t.Fatalf("windows %+v", ws)
	}
	last := client[8]
	if last.OK || !strings.Contains(strings.Join(last.Lines, "\n"), "unknown command") {
		t.Fatalf("the unknown command reply %+v", last)
	}
}

// fakeTmux answers each command line with a reply block from a table, the
// way tmux -C does, and records the lines it got.
type fakeTmux struct {
	in     *io.PipeReader
	out    *io.PipeWriter
	got    []string
	answer func(cmd string) ([]string, bool)
}

func (f *fakeTmux) serve() {
	_, _ = io.WriteString(f.out, "%begin 1 1 0\n%end 1 1 0\n%session-changed $0 main\n")
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 1024)
	n := 2
	for {
		k, err := f.in.Read(chunk)
		if err != nil {
			_ = f.out.Close()
			return
		}
		buf = append(buf, chunk[:k]...)
		for {
			i := strings.IndexByte(string(buf), '\n')
			if i < 0 {
				break
			}
			line := string(buf[:i])
			buf = buf[i+1:]
			f.got = append(f.got, line)
			lines, ok := f.answer(line)
			var b strings.Builder
			b.WriteString("%begin 1 " + strconv.Itoa(n) + " 1\n")
			for _, l := range lines {
				b.WriteString(l + "\n")
			}
			end := "%end"
			if !ok {
				end = "%error"
			}
			b.WriteString(end + " 1 " + strconv.Itoa(n) + " 1\n")
			n++
			_, _ = io.WriteString(f.out, b.String())
		}
	}
}

func newFake(answer func(string) ([]string, bool)) (*Client, *fakeTmux) {
	cmdR, cmdW := io.Pipe()
	outR, outW := io.Pipe()
	f := &fakeTmux{in: cmdR, out: outW, answer: answer}
	go f.serve()
	return NewClient(cmdW, outR), f
}

func TestClientDoReturnsTheReplyToItsOwnCommand(t *testing.T) {
	c, _ := newFake(func(cmd string) ([]string, bool) {
		if strings.HasPrefix(cmd, "bad") {
			return []string{"parse error: unknown command: bad"}, false
		}
		return []string{"@7"}, true
	})
	defer c.Close()
	lines, err := c.Do("new-window -d")
	if err != nil || len(lines) != 1 || lines[0] != "@7" {
		t.Fatalf("lines %v err %v", lines, err)
	}
	if _, err := c.Do("bad"); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("err %v", err)
	}
	if _, err := c.Do("a\nkill-server"); err == nil {
		t.Fatal("a command of two lines went out")
	}
}

func TestMirrorSyncCreatesMarksAndSetsTheStatus(t *testing.T) {
	c, f := newFake(func(cmd string) ([]string, bool) {
		switch {
		case strings.HasPrefix(cmd, "list-windows"):
			return []string{"@0\tzsh\t"}, true
		case strings.HasPrefix(cmd, "new-window"):
			return []string{"@9"}, true
		}
		return nil, true
	})
	defer c.Close()
	m := &Mirror{Tmux: c, Session: "main", Exe: "/bin/coppice", Socket: "/run/c.sock"}
	panes := []Pane{{ID: "w1:p1", Label: "docs", State: "blocked"}}
	if err := m.Sync(panes); err != nil {
		t.Fatal(err)
	}
	want := []string{
		ListCmd("main"),
		CreateCmd("main", "docs", "/bin/coppice", "/run/c.sock", "w1:p1"),
		MarkCmd("@9", "w1:p1"),
		StatusCmd("main", "1 need you"),
	}
	if strings.Join(f.got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("sent\n%s\nwant\n%s", strings.Join(f.got, "\n"), strings.Join(want, "\n"))
	}
}

// Closing a mirrored window, or leaving the attach inside it, hides that
// pane until the mirror starts again. The mirror does not open it back.
func TestAClosedWindowHidesItsPane(t *testing.T) {
	c, f := newFake(func(cmd string) ([]string, bool) {
		if strings.HasPrefix(cmd, "new-window") {
			return []string{"@9"}, true
		}
		return nil, true
	})
	defer c.Close()
	m := &Mirror{Tmux: c, Session: "main", Exe: "/bin/coppice", Socket: "/run/c.sock"}
	panes := []Pane{{ID: "w1:p1", Label: "docs"}}
	if err := m.Sync(panes); err != nil {
		t.Fatal(err)
	}
	m.Handle(Event{Kind: EventWindowClose, Window: "@9"})
	f.got = nil
	if err := m.Sync(panes); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range f.got {
		if strings.HasPrefix(cmd, "new-window") {
			t.Fatalf("the mirror opened a hidden pane again: %v", f.got)
		}
	}
	if !m.Hidden("w1:p1") {
		t.Fatal("the pane is not hidden")
	}
}

func TestClientEndsWhenTmuxExits(t *testing.T) {
	cmdR, cmdW := io.Pipe()
	go func() { _, _ = io.Copy(io.Discard, cmdR) }()
	c := NewClient(cmdW, strings.NewReader("%begin 1 1 0\n%end 1 1 0\n%exit\n"))
	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the client did not end")
	}
	if _, err := c.Do("list-windows"); err == nil {
		t.Fatal("a command after exit got no error")
	}
}

// A window can go between list-windows and the op on it. tmux then refuses
// the op. The mirror logs it, goes on, and the next sync plans again.
func TestAFailedOpIsLoggedAndTheSyncGoesOn(t *testing.T) {
	c, f := newFake(func(cmd string) ([]string, bool) {
		switch {
		case strings.HasPrefix(cmd, "list-windows"):
			return []string{"@9\tgone\tw1:p9"}, true
		case strings.HasPrefix(cmd, "kill-window"):
			return []string{"can't find window: @9"}, false
		}
		return nil, true
	})
	defer c.Close()
	var logged []string
	m := &Mirror{Tmux: c, Session: "main", Exe: "/bin/coppice", Socket: "/run/c.sock",
		Log: func(s string) { logged = append(logged, s) }}
	if err := m.Sync(nil); err != nil {
		t.Fatalf("a refused kill ended the sync: %v", err)
	}
	if last := f.got[len(f.got)-1]; last != StatusCmd("main", "quiet") {
		t.Fatalf("the status was not set after the refused kill: %v", f.got)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "can't find window") {
		t.Fatalf("logged %v", logged)
	}
}

func TestAFailedListEndsTheSync(t *testing.T) {
	c, _ := newFake(func(cmd string) ([]string, bool) {
		return []string{"can't find session: main"}, false
	})
	defer c.Close()
	m := &Mirror{Tmux: c, Session: "main"}
	if err := m.Sync(nil); err == nil {
		t.Fatal("a refused list-windows did not end the sync")
	}
}

// The mirror keeps the session's own status-right in a session option
// when it starts, and puts it back when it stops, so no count stays on
// screen that no pane backs.
func TestStopPutsTheStatusBack(t *testing.T) {
	for _, tc := range []struct {
		name     string
		kept     []string
		status   []string
		sent     []string
		stopSent []string
	}{
		{
			name:   "no status of its own",
			status: nil,
			sent: []string{
				"show-options -q -v -t 'main' '@coppice_saved_status'",
				"show-options -v -t 'main' 'status-right'",
				"set-option -t 'main' '@coppice_saved_status' '-'",
			},
			stopSent: []string{
				"set-option -u -t 'main' 'status-right'",
				"set-option -u -t 'main' '@coppice_saved_status'",
			},
		},
		{
			name:   "a status of its own",
			status: []string{"mine #{session_name}"},
			sent: []string{
				"show-options -q -v -t 'main' '@coppice_saved_status'",
				"show-options -v -t 'main' 'status-right'",
				"set-option -t 'main' '@coppice_saved_status' '=mine #{session_name}'",
			},
			stopSent: []string{
				"set-option -t 'main' 'status-right' 'mine #{session_name}'",
				"set-option -u -t 'main' '@coppice_saved_status'",
			},
		},
		{
			// A second mirror on the session finds the first one's saved
			// value, and never saves the first one's count as the session's.
			name: "a mirror already runs",
			kept: []string{"=orig"},
			sent: []string{
				"show-options -q -v -t 'main' '@coppice_saved_status'",
			},
			stopSent: []string{
				"set-option -t 'main' 'status-right' 'orig'",
				"set-option -u -t 'main' '@coppice_saved_status'",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, f := newFake(func(cmd string) ([]string, bool) {
				switch {
				case strings.Contains(cmd, "show-options -q"):
					return tc.kept, true
				case strings.HasPrefix(cmd, "show-options"):
					return tc.status, true
				}
				return nil, true
			})
			defer c.Close()
			m := &Mirror{Tmux: c, Session: "main"}
			if err := m.Start(); err != nil {
				t.Fatal(err)
			}
			if strings.Join(f.got, "\n") != strings.Join(tc.sent, "\n") {
				t.Fatalf("start sent\n%s\nwant\n%s", strings.Join(f.got, "\n"), strings.Join(tc.sent, "\n"))
			}
			f.got = nil
			if err := m.Stop(); err != nil {
				t.Fatal(err)
			}
			if strings.Join(f.got, "\n") != strings.Join(tc.stopSent, "\n") {
				t.Fatalf("stop sent\n%s\nwant\n%s", strings.Join(f.got, "\n"), strings.Join(tc.stopSent, "\n"))
			}
		})
	}
}

// When the control client is gone before Stop, as after a hang-up that
// reached tmux too, Stop puts the status back through one tmux run.
func TestStopWithTheClientGoneRunsTmuxOnce(t *testing.T) {
	c, _ := newFake(func(cmd string) ([]string, bool) {
		if strings.HasPrefix(cmd, "show-options -v") {
			return []string{"orig"}, true
		}
		return nil, true
	})
	var ran [][]string
	m := &Mirror{Tmux: c, Session: "main", OneShot: func(args []string) error {
		ran = append(ran, args)
		return nil
	}}
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	<-c.Done()
	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}
	want := []string{"set-option", "-t", "main", "status-right", "orig", ";",
		"set-option", "-u", "-t", "main", "@coppice_saved_status"}
	if len(ran) != 1 || strings.Join(ran[0], "|") != strings.Join(want, "|") {
		t.Fatalf("ran %q, want one run of %q", ran, want)
	}
}

// A window whose mark tmux refuses would look like the user's own, and the
// next sync would open another. The mirror kills it.
func TestAWindowWithoutItsMarkIsKilled(t *testing.T) {
	c, f := newFake(func(cmd string) ([]string, bool) {
		switch {
		case strings.HasPrefix(cmd, "new-window"):
			return []string{"@9"}, true
		case strings.HasPrefix(cmd, "set-option -w"):
			return []string{"can't find window: @9"}, false
		}
		return nil, true
	})
	defer c.Close()
	m := &Mirror{Tmux: c, Session: "main", Exe: "/bin/coppice", Socket: "/run/c.sock", Log: func(string) {}}
	if err := m.Sync([]Pane{{ID: "w1:p1", Label: "docs"}}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, cmd := range f.got {
		if cmd == KillCmd("@9") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the unmarked window stayed: %v", f.got)
	}
}

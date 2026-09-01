package tmuxmirror

import (
	"strings"
	"testing"
)

func TestPlanCreatesAWindowPerPaneAndRenamesAStaleOne(t *testing.T) {
	panes := []Pane{
		{ID: "w1:p1", Label: "docs", State: "idle"},
		{ID: "w1:p2", Label: "build", State: "blocked"},
		{ID: "w1:p3", Label: "", State: "working"},
	}
	windows := []Window{{ID: "@1", Name: "old", Pane: "w1:p1"}}
	ops := Plan(panes, windows)
	want := []Op{
		{Kind: OpRename, Window: "@1", Pane: "w1:p1", Name: "docs"},
		{Kind: OpCreate, Pane: "w1:p2", Name: "build"},
		{Kind: OpCreate, Pane: "w1:p3", Name: "w1:p3"},
	}
	if len(ops) != len(want) {
		t.Fatalf("ops %+v, want %+v", ops, want)
	}
	for i := range want {
		if ops[i] != want[i] {
			t.Errorf("op %d: %+v, want %+v", i, ops[i], want[i])
		}
	}
}

func TestPlanKillsTheWindowOfAClosedOrGonePaneAndASecondWindowForOnePane(t *testing.T) {
	panes := []Pane{
		{ID: "w1:p1", Label: "docs"},
		{ID: "w1:p2", Label: "build", Closed: true},
	}
	windows := []Window{
		{ID: "@1", Name: "docs", Pane: "w1:p1"},
		{ID: "@2", Name: "build", Pane: "w1:p2"},
		{ID: "@3", Name: "gone", Pane: "w1:p9"},
		{ID: "@4", Name: "docs", Pane: "w1:p1"},
	}
	ops := Plan(panes, windows)
	want := []Op{
		{Kind: OpKill, Window: "@2", Pane: "w1:p2"},
		{Kind: OpKill, Window: "@3", Pane: "w1:p9"},
		{Kind: OpKill, Window: "@4", Pane: "w1:p1"},
	}
	if len(ops) != len(want) {
		t.Fatalf("ops %+v, want %+v", ops, want)
	}
	for i := range want {
		if ops[i] != want[i] {
			t.Errorf("op %d: %+v, want %+v", i, ops[i], want[i])
		}
	}
}

// A window with no pane mark is the user's own. The mirror never renames it
// and never kills it, even when its name is the name of a pane.
func TestPlanNeverTouchesAWindowTheMirrorDidNotCreate(t *testing.T) {
	panes := []Pane{{ID: "w1:p1", Label: "docs"}}
	windows := []Window{
		{ID: "@0", Name: "docs"},
		{ID: "@5", Name: "zsh"},
	}
	for _, op := range Plan(panes, windows) {
		if op.Window == "@0" || op.Window == "@5" {
			t.Fatalf("the plan touches a window it does not own: %+v", op)
		}
	}
	if ops := Plan(nil, windows); len(ops) != 0 {
		t.Fatalf("with no panes the plan is %+v, want nothing", ops)
	}
}

func TestStatusLineCountsWhatNeedsYouAndWhatWorks(t *testing.T) {
	panes := []Pane{
		{ID: "a", State: "blocked"},
		{ID: "b", State: "blocked"},
		{ID: "c", State: "working"},
		{ID: "d", State: "working"},
		{ID: "e", State: "working"},
		{ID: "f", State: "idle"},
		{ID: "g", State: "blocked", Closed: true},
	}
	if got := StatusLine(panes); got != "2 need you · 3 working" {
		t.Fatalf("status %q", got)
	}
	if got := StatusLine(panes[2:3]); got != "1 working" {
		t.Fatalf("status %q", got)
	}
	if got := StatusLine(panes[5:]); got != "quiet" {
		t.Fatalf("status %q", got)
	}
}

func TestWindowNameDropsControlRunesAndStaysShort(t *testing.T) {
	got := WindowName(Pane{ID: "w1:p1", Label: "a\nb\x1b[31mc\td"})
	if strings.ContainsAny(got, "\n\r\x1b\t") {
		t.Fatalf("name %q holds a control rune", got)
	}
	long := WindowName(Pane{ID: "w1:p1", Label: strings.Repeat("x", 80)})
	if n := len([]rune(long)); n > MaxName {
		t.Fatalf("name is %d runes", n)
	}
	if got := WindowName(Pane{ID: "w1:p1", Label: "\n\t"}); got != "w1:p1" {
		t.Fatalf("an empty label names the window %q, want the id", got)
	}
}

// A pane label is text the agent side can set. Each command the mirror
// sends is one line, and the label reaches tmux as literal text: quoted, and
// with # doubled so tmux does not read a format.
func TestCommandsKeepAHostileLabelLiteral(t *testing.T) {
	hostile := Pane{ID: "w1:p1", Label: "x'; kill-server; #{s}\nk"}
	name := WindowName(hostile)
	cmds := []string{
		CreateCmd("main", name, "/bin/coppice", "/run/c.sock", hostile.ID),
		RenameCmd("@1", name),
		MarkCmd("@1", hostile.ID),
		StatusCmd("main", "1 need you"),
		ListCmd("main"),
		KillCmd("@1"),
	}
	for _, c := range cmds {
		if strings.ContainsAny(c, "\n\r") {
			t.Fatalf("command %q is more than one line", c)
		}
	}
	want := `rename-window -t '@1' 'x'\''; kill-server; ##{s}k'`
	if cmds[1] != want {
		t.Fatalf("rename\n got %s\nwant %s", cmds[1], want)
	}
}

// tmux runs a command given as several arguments with no shell, so the
// path and the socket reach coppice as they are.
func TestTheWindowRunsAttachWithAnExplicitSocket(t *testing.T) {
	got := CreateCmd("main", "docs", "/opt/my bin/coppice", "/run/c.sock", "w1:p1")
	want := `new-window -d -P -F '#{window_id}' -t 'main:' -n 'docs' ` +
		`'/opt/my bin/coppice' '--socket' '/run/c.sock' 'attach' 'w1:p1'`
	if got != want {
		t.Fatalf("create\n got %s\nwant %s", got, want)
	}
}

func TestParseWindowsReadsTheMark(t *testing.T) {
	ws := ParseWindows([]string{"@0\tzsh\t", "@1\tb'x;y#z\tw1:p1", "junk"})
	if len(ws) != 2 {
		t.Fatalf("windows %+v", ws)
	}
	if ws[0] != (Window{ID: "@0", Name: "zsh"}) {
		t.Errorf("window 0 %+v", ws[0])
	}
	if ws[1] != (Window{ID: "@1", Name: "b'x;y#z", Pane: "w1:p1"}) {
		t.Errorf("window 1 %+v", ws[1])
	}
}

func TestPanesFromListReadsPaneListRows(t *testing.T) {
	res := map[string]any{"panes": []any{
		map[string]any{"id": "w1:p1", "label": "docs", "state": "idle", "closed": false},
		map[string]any{"id": "w1:p2", "state": "blocked", "closed": true},
		"junk",
		map[string]any{"label": "no id"},
	}}
	got := PanesFromList(res)
	want := []Pane{
		{ID: "w1:p1", Label: "docs", State: "idle"},
		{ID: "w1:p2", State: "blocked", Closed: true},
	}
	if len(got) != len(want) {
		t.Fatalf("panes %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pane %d %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestCtlQuotesAValueThatLooksLikeAFlag(t *testing.T) {
	got := ctl([]string{"set-option", "-t", "main", "status-right", "-x"})
	if got != "set-option -t 'main' 'status-right' '-x'" {
		t.Fatalf("ctl %q", got)
	}
}

// A blocked pane whose ask a foreman holds counts as working, not as
// needing you, until the hold ends.
func TestStatusLineCountsAHeldAskAsWorking(t *testing.T) {
	panes := PanesFromList(map[string]any{"panes": []any{
		map[string]any{"id": "a", "state": "blocked", "held": map[string]any{"by": "f"}},
		map[string]any{"id": "b", "state": "blocked"},
	}})
	if !panes[0].Held || panes[1].Held {
		t.Fatalf("panes = %+v", panes)
	}
	if got := StatusLine(panes); got != "1 need you · 1 working" {
		t.Fatalf("status %q", got)
	}
}

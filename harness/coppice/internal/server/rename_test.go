package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/proto"
)

func TestPaneRenameUpdatesALivePanesLabel(t *testing.T) {
	s := newPaneServer(t)
	id := createShellPane(t, s, "sh", "-c", "sleep 30")
	t.Cleanup(func() { closePane(t, s, id) })

	got := roundTrip(t, s, `{"id":"1","cmd":"pane.rename","pane":"`+id+`","label":"new name"}`)
	m := result(t, got[0])
	if m["pane"] != id || m["label"] != "new name" {
		t.Fatalf("pane.rename result = %v", m)
	}
	row := rowFor(t, listPanes(t, s), id)
	if row["label"] != "new name" {
		t.Fatalf("label after rename = %v, want %q", row["label"], "new name")
	}
}

func TestPaneRenameUpdatesAnEndedRecord(t *testing.T) {
	s := newEndedServer(t)
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","cmd_argv":["sh","-c","exit 0"],"kind":"pty"}`,
	)
	id, _ := result(t, got[0])["pane"].(string)
	waitDone(t, s, id)

	renamed := roundTrip(t, s, `{"id":"2","cmd":"pane.rename","pane":"`+id+`","label":"archived run"}`)
	if !renamed[0].OK {
		t.Fatalf("pane.rename on an ended record failed: %+v", renamed[0].Error)
	}
	for _, row := range listEndedPanes(t, s) {
		if row["id"] == id {
			if row["label"] != "archived run" {
				t.Fatalf("ended row label = %v, want %q", row["label"], "archived run")
			}
			return
		}
	}
	t.Fatalf("no ended row for %s after rename", id)
}

func TestPaneRenameWithAnEmptyLabelIsBadRequest(t *testing.T) {
	s := newPaneServer(t)
	id := createShellPane(t, s, "sh", "-c", "sleep 30")
	t.Cleanup(func() { closePane(t, s, id) })

	got := roundTrip(t, s, `{"id":"1","cmd":"pane.rename","pane":"`+id+`","label":""}`)
	if got[0].OK || got[0].Error == nil || got[0].Error.Code != "bad_request" {
		t.Fatalf("got %+v, want bad_request", got[0])
	}
}

// A label of nothing but whitespace is the same mistake as an empty one:
// trimmed first, so it cannot slip past the empty check as one space.
func TestPaneRenameWithAWhitespaceOnlyLabelIsBadRequest(t *testing.T) {
	s := newPaneServer(t)
	id := createShellPane(t, s, "sh", "-c", "sleep 30")
	t.Cleanup(func() { closePane(t, s, id) })

	got := roundTrip(t, s, `{"id":"1","cmd":"pane.rename","pane":"`+id+`","label":"   "}`)
	if got[0].OK || got[0].Error == nil || got[0].Error.Code != "bad_request" {
		t.Fatalf("got %+v, want bad_request", got[0])
	}
}

// Leading and trailing whitespace around an otherwise real label is
// trimmed before it is stored, not kept verbatim.
func TestPaneRenameTrimsLeadingAndTrailingWhitespace(t *testing.T) {
	s := newPaneServer(t)
	id := createShellPane(t, s, "sh", "-c", "sleep 30")
	t.Cleanup(func() { closePane(t, s, id) })

	got := roundTrip(t, s, `{"id":"1","cmd":"pane.rename","pane":"`+id+`","label":"  padded name  "}`)
	m := result(t, got[0])
	if m["label"] != "padded name" {
		t.Fatalf("label = %v, want the trimmed %q", m["label"], "padded name")
	}
	row := rowFor(t, listPanes(t, s), id)
	if row["label"] != "padded name" {
		t.Fatalf("stored label = %v, want the trimmed %q", row["label"], "padded name")
	}
}

func TestPaneRenameWithNoLabelAtAllIsBadRequest(t *testing.T) {
	s := newPaneServer(t)
	id := createShellPane(t, s, "sh", "-c", "sleep 30")
	t.Cleanup(func() { closePane(t, s, id) })

	got := roundTrip(t, s, `{"id":"1","cmd":"pane.rename","pane":"`+id+`"}`)
	if got[0].OK || got[0].Error == nil || got[0].Error.Code != "bad_request" {
		t.Fatalf("got %+v, want bad_request", got[0])
	}
}

func TestPaneRenameOnAnUnknownPaneIsNoSuchPane(t *testing.T) {
	s := newPaneServer(t)
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.rename","pane":"nope","label":"x"}`)
	if got[0].OK || got[0].Error == nil || got[0].Error.Code != "no_such_pane" {
		t.Fatalf("got %+v, want no_such_pane", got[0])
	}
}

// A label loses its control characters and is cut to labelMax runes on
// every verb that sets one, so no agent can write escape codes into a
// label the owner's screens draw.
func TestLabelsAreCleanedAndCappedOnTheServer(t *testing.T) {
	s := newEndedServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 30"],"kind":"pty","label":"ev\u001b[2Jil\u0007"}`)
	id, _ := result(t, got[0])["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	if row := rowFor(t, listPanes(t, s), id); row["label"] != "ev[2Jil" {
		t.Fatalf("created label = %q, want the control characters dropped", row["label"])
	}
	long := strings.Repeat("x", 100)
	got = roundTrip(t, s, `{"id":"2","cmd":"pane.rename","pane":"`+id+`","label":"\u001b]0;`+long+`"}`)
	m := result(t, got[0])
	label, _ := m["label"].(string)
	if strings.ContainsRune(label, 0x1b) || len([]rune(label)) != labelMax {
		t.Fatalf("renamed label = %q (%d runes), want no ESC and %d runes", label, len([]rune(label)), labelMax)
	}
}

// A pane may rename itself, not another pane. The operator renames any.
func TestAPaneRenamesOnlyItself(t *testing.T) {
	s := newPaneServer(t)
	a := createShellPane(t, s, "sh", "-c", "sleep 30")
	b := createShellPane(t, s, "sh", "-c", "sleep 30")
	t.Cleanup(func() { closePane(t, s, a); closePane(t, s, b) })
	pane := &peerFacts{checked: true, pane: true, paneID: a}
	got := roundTripFacts(t, s, pane, `{"id":"1","cmd":"pane.rename","pane":"`+b+`","label":"swapped"}`)
	if got[0].OK || got[0].Error == nil || got[0].Error.Code != "unauthorized" {
		t.Fatalf("a pane renaming another pane got %+v, want unauthorized", got[0])
	}
	if row := rowFor(t, listPanes(t, s), b); row["label"] == "swapped" {
		t.Fatal("the other pane's label changed")
	}
	got = roundTripFacts(t, s, pane, `{"id":"2","cmd":"pane.rename","pane":"`+a+`","label":"me"}`)
	if !got[0].OK {
		t.Fatalf("a pane renaming itself failed: %+v", got[0].Error)
	}
}

// A label made from the directory's name is cleaned the same way.
func TestADefaultLabelIsCleaned(t *testing.T) {
	s := newPaneServer(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "bad\x1b]0;x\x07dir")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"id": "1", "cmd": "pane.create", "cwd": dir, "harness": "fakeh",
		"cmd_argv": []string{"sh", "-c", "echo hi; sleep 30"}, "kind": "pty"})
	got := roundTrip(t, s, string(b))
	id, _ := result(t, got[0])["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	label, _ := rowFor(t, listPanes(t, s), id)["label"].(string)
	if strings.ContainsAny(label, "\x1b\x07") || label != "fakeh-bad]0;xdir" {
		t.Fatalf("default label = %q, want the control characters dropped", label)
	}
}

// A harness name is a short word. One with spaces or control characters
// is refused, so it can neither spell a label by its cut nor reach a
// screen raw.
func TestAHarnessNameIsAShortWord(t *testing.T) {
	s := newPaneServer(t)
	for _, h := range []string{"foreman" + strings.Repeat(" ", 53), "a b", "x\x1b[2J", strings.Repeat("h", 40)} {
		b, _ := json.Marshal(map[string]any{"id": "1", "cmd": "pane.create", "cwd": t.TempDir(), "harness": h,
			"cmd_argv": []string{"sh", "-c", "sleep 30"}, "kind": "pty"})
		got := roundTrip(t, s, string(b))
		if got[0].OK || got[0].Error == nil || got[0].Error.Code != proto.ErrBadRequest {
			t.Fatalf("harness %q got %+v, want bad_request", h, got[0])
		}
	}
}

// A pane never ends up labelled foreman, whatever made the label: its own
// words, a harness-made default, or characters the cleaning drops.
func TestAPaneNeverEndsUpLabelledForeman(t *testing.T) {
	s := newPaneServer(t)
	pane := &peerFacts{checked: true, pane: true, paneID: "w1:p9"}
	for _, label := range []string{"fore​man", "‮foreman", "fore\x01man"} {
		b, _ := json.Marshal(map[string]any{"id": "1", "cmd": "pane.create", "cwd": t.TempDir(), "label": label,
			"cmd_argv": []string{"sh", "-c", "sleep 30"}, "kind": "pty"})
		got := roundTripFacts(t, s, pane, string(b))
		if got[0].OK || got[0].Error == nil || got[0].Error.Message != ForemanLabelRefusal {
			t.Fatalf("label %q from a pane got %+v, want the refusal", label, got[0])
		}
	}
	if l := cleanLabel("a‍b⁦c"); l != "abc" {
		t.Fatalf("cleanLabel kept format characters: %q", l)
	}
}

// A pane may not take the exact label of another live pane: people find
// their agents by label. The operator may.
func TestAPaneCannotCopyAnotherLivePanesLabel(t *testing.T) {
	s := newPaneServer(t)
	a := createShellPane(t, s, "sh", "-c", "echo a; sleep 30")
	b := createShellPane(t, s, "sh", "-c", "echo b; sleep 30")
	t.Cleanup(func() { closePane(t, s, a); closePane(t, s, b) })
	roundTrip(t, s, `{"id":"1","cmd":"pane.rename","pane":"`+a+`","label":"reviewer"}`)
	pane := &peerFacts{checked: true, pane: true, paneID: b}
	got := roundTripFacts(t, s, pane, `{"id":"2","cmd":"pane.rename","pane":"`+b+`","label":"reviewer"}`)
	if got[0].OK || got[0].Error == nil || got[0].Error.Code != proto.ErrUnauthorized {
		t.Fatalf("a pane took another pane's label: %+v", got[0])
	}
	b2, _ := json.Marshal(map[string]any{"id": "3", "cmd": "pane.create", "cwd": t.TempDir(), "label": "reviewer",
		"cmd_argv": []string{"sh", "-c", "sleep 30"}, "kind": "pty"})
	got = roundTripFacts(t, s, pane, string(b2))
	if got[0].OK || got[0].Error == nil || got[0].Error.Code != proto.ErrUnauthorized {
		t.Fatalf("a pane made a pane with another pane's label: %+v", got[0])
	}
	got = roundTripFacts(t, s, pane, `{"id":"4","cmd":"pane.rename","pane":"`+b+`","label":"b-own"}`)
	if !got[0].OK {
		t.Fatalf("a pane could not rename itself: %+v", got[0].Error)
	}
	got = roundTrip(t, s, `{"id":"5","cmd":"pane.rename","pane":"`+b+`","label":"reviewer"}`)
	if !got[0].OK {
		t.Fatalf("the operator was refused a shared label: %+v", got[0].Error)
	}
}

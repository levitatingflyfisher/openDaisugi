package skills

import (
	"strings"
	"testing"
)

// The turn page is the head of every turn's system prompt, so a cache holds
// it only if its bytes never change from one turn to the next. These tests
// pin what it must say.

func TestTheTurnPageSaysToZoomBeforeActingOnAnOldLine(t *testing.T) {
	for _, want := range []string{
		"Anything older than the tail is a short line.",
		"`zoom` it before you act on it or tell the operator what it says",
		"Say in your reply what you learned from a `zoom` or from a worker's report",
		"`zoom(label)` opens a worker by its label",
	} {
		if !strings.Contains(ForemanTurn, want) {
			t.Errorf("the turn page lacks %q", want)
		}
	}
}

func TestTheTurnPageSaysTheNewMessageIsOnlyInTheMessageBlock(t *testing.T) {
	if strings.Contains(ForemanTurn, "It holds the operator's new message") {
		t.Error("the page says the tail holds the new message")
	}
	if !strings.Contains(ForemanTurn, "but not that message: it comes in `<message>`, and only there.") {
		t.Error("the page does not say the new message is only in <message>")
	}
}

func TestTheTurnPageHasNoSlotForPerTurnText(t *testing.T) {
	for _, bad := range []string{"{{", "}}", "%s", "%d", "$("} {
		if strings.Contains(ForemanTurn, bad) {
			t.Errorf("the turn page holds %q: it must be fixed bytes", bad)
		}
	}
}

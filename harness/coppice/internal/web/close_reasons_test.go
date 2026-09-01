package web

import (
	"io/fs"
	"os"
	"strings"
	"testing"
)

// The two reasons the server closes an already open socket for. Both are
// StatusInternalError, so the reason text is the only thing that tells them
// apart, and that text is copied into app.js by hand rather than shared
// through any wire format. This is the one place both copies are checked
// against each other and against PINS.md, in the same shape pins_test.go
// checks other cross file facts.
const (
	closeReasonUnreachable    = "coppice-server is not reachable"
	closeReasonUpstreamClosed = "coppice-server closed the connection"
)

func TestTheWebsocketCloseReasonsAgreeAcrossGoJSAndPins(t *testing.T) {
	ws, err := os.ReadFile("ws.go")
	if err != nil {
		t.Fatal(err)
	}
	appJS, err := fs.ReadFile(StaticFiles(), "app.js")
	if err != nil {
		t.Fatal(err)
	}
	pins, err := os.ReadFile("../../PINS.md")
	if err != nil {
		t.Fatal(err)
	}
	section, ok := webPinsSection(string(pins))
	if !ok {
		t.Fatalf("PINS.md has no %q section", webPinsHeading)
	}

	for _, reason := range []string{closeReasonUnreachable, closeReasonUpstreamClosed} {
		if !strings.Contains(string(ws), `"`+reason+`"`) {
			t.Errorf("ws.go no longer sends the close reason %q", reason)
		}
		if !strings.Contains(string(appJS), "'"+reason+"'") {
			t.Errorf("app.js no longer carries the close reason %q", reason)
		}
		if !strings.Contains(section, reason) {
			t.Errorf("PINS.md does not record the close reason %q", reason)
		}
	}
}

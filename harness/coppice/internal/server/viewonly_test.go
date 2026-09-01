package server

import (
	"testing"

	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

// sendTextReply sends a bare Enter to pane id on fc and returns the reply.
func sendTextReply(fc *flowClient, reqID, pane string) map[string]any {
	fc.t.Helper()
	fc.send(`{"id":"` + reqID + `","cmd":"pane.send_text","pane":"` + pane + `","text":""}`)
	m, _ := fc.replyFor(reqID)
	return m
}

// expectNotAttached fails unless m is a not_attached refusal that names the
// view-only attach and teaches the plain attach command.
func expectNotAttached(t *testing.T, m map[string]any, pane string) {
	t.Helper()
	if m["ok"] != false {
		t.Fatalf("got %v, want a refusal", m)
	}
	errObj, _ := m["error"].(map[string]any)
	if errObj == nil || errObj["code"] != string(proto.ErrNotAttached) {
		t.Fatalf("error = %v, want not_attached", m["error"])
	}
	msg, _ := errObj["message"].(string)
	want := "this connection is attached to " + pane + " view only. Run: coppice attach " + pane
	if msg != want {
		t.Fatalf("message = %q, want %q", msg, want)
	}
}

func TestAViewOnlyAttachRefusesWritesOnThatConnection(t *testing.T) {
	toolchain.RequireOrSkip(t)
	s := newTestServer(t)
	id := createShellPane(t, s, "sh", "-c", "sleep 30")
	fc := newFlowClient(t, s)
	fc.send(`{"id":"a","cmd":"pane.attach","pane":"` + id + `","view_only":true,"cols":5,"rows":2}`)
	fc.expectOK("a")
	expectNotAttached(t, sendTextReply(fc, "t1", id), id)

	fc.send(`{"id":"k","cmd":"pane.send_keys","pane":"` + id + `","keys":["enter"]}`)
	m, _ := fc.replyFor("k")
	expectNotAttached(t, m, id)
	fc.send(`{"id":"r","cmd":"pane.run","pane":"` + id + `","line":"true"}`)
	m, _ = fc.replyFor("r")
	expectNotAttached(t, m, id)
	fc.send(`{"id":"p","cmd":"agent.prompt","pane":"` + id + `","text":"hi"}`)
	m, _ = fc.replyFor("p")
	expectNotAttached(t, m, id)
	fc.send(`{"id":"z","cmd":"pane.resize","pane":"` + id + `","cols":5,"rows":2}`)
	m, _ = fc.replyFor("z")
	expectNotAttached(t, m, id)

	// A view-only attach never resizes the pane, so the 5 by 2 above left
	// the pane at the size pane.create gave it.
	got := roundTrip(t, s, `{"id":"l","cmd":"pane.list"}`)
	for _, raw := range result(t, got[0])["panes"].([]any) {
		p := raw.(map[string]any)
		if p["id"] != id {
			continue
		}
		if cols, _ := p["cols"].(float64); cols == 5 {
			t.Fatalf("a view-only attach or resize changed the pane's size: %v", p)
		}
	}
}

func TestAPlainAttachOnTheSamePaneClearsViewOnly(t *testing.T) {
	toolchain.RequireOrSkip(t)
	s := newTestServer(t)
	id := createShellPane(t, s, "sh", "-c", "sleep 30")
	fc := newFlowClient(t, s)
	fc.send(`{"id":"a","cmd":"pane.attach","pane":"` + id + `","view_only":true}`)
	fc.expectOK("a")
	expectNotAttached(t, sendTextReply(fc, "t1", id), id)
	fc.send(`{"id":"b","cmd":"pane.attach","pane":"` + id + `"}`)
	fc.expectOK("b")
	if m := sendTextReply(fc, "t2", id); m["ok"] != true {
		t.Fatalf("send_text after a plain attach = %v, want ok", m)
	}
}

func TestADetachClearsViewOnly(t *testing.T) {
	toolchain.RequireOrSkip(t)
	s := newTestServer(t)
	id := createShellPane(t, s, "sh", "-c", "sleep 30")
	fc := newFlowClient(t, s)
	fc.send(`{"id":"a","cmd":"pane.attach","pane":"` + id + `","view_only":true}`)
	fc.expectOK("a")
	expectNotAttached(t, sendTextReply(fc, "t1", id), id)
	fc.send(`{"id":"d","cmd":"pane.detach","pane":"` + id + `"}`)
	fc.expectOK("d")
	if m := sendTextReply(fc, "t2", id); m["ok"] != true {
		t.Fatalf("send_text after a detach = %v, want ok", m)
	}
}

func TestAViewOnlyAttachStillSendsFrames(t *testing.T) {
	toolchain.RequireOrSkip(t)
	s := newTestServer(t)
	id := createShellPane(t, s, "sh", "-c", "echo hello; sleep 30")
	fc := newFlowClient(t, s)
	fc.send(`{"id":"a","cmd":"pane.attach","pane":"` + id + `","view_only":true}`)
	_, events := fc.replyFor("a")
	for _, ev := range events {
		if ev["event"] == "frame" {
			return
		}
	}
	fr := fc.expectEvent("frame")
	if fr["pane"] != id {
		t.Fatalf("frame names pane %v, want %s", fr["pane"], id)
	}
}

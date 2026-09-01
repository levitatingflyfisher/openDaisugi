package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/proto"
)

// paddedJSONLine returns a valid single line JSON request,
// {"id":<id>,"text":<padding>}, whose marshaled bytes total exactly want.
func paddedJSONLine(id string, want int) []byte {
	prefix := []byte(`{"id":"` + id + `","text":"`)
	suffix := []byte(`"}`)
	pad := want - len(prefix) - len(suffix)
	line := make([]byte, 0, want)
	line = append(line, prefix...)
	line = append(line, bytes.Repeat([]byte("x"), pad)...)
	line = append(line, suffix...)
	return line
}

func TestSessionSendsOneLinePerMessage(t *testing.T) {
	f, d := newFakeServer(t, echoOK)
	s, err := Open(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Send([]byte(`{"id":"1","cmd":"server.status"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case line := <-s.Lines():
		if !bytes.Contains(line, []byte(`"ok":true`)) {
			t.Fatalf("reply was %s", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no reply within 2 s")
	}
	got := f.Received()
	if len(got) != 1 || string(got[0]) != `{"id":"1","cmd":"server.status"}` {
		t.Fatalf("the server saw %q", got)
	}
}

// The failure this names: a full 120x40 frame is roughly 150 KB of JSON, and
// bufio.Scanner's default 64 KB buffer would cut it in half and hand the
// browser a broken line.
func TestSessionDoesNotTruncateAOneHundredKilobyteLine(t *testing.T) {
	big := strings.Repeat("x", 100*1024)
	_, d := newFakeServer(t, func(req map[string]any) []map[string]any {
		return []map[string]any{{"id": req["id"], "ok": true, "result": big}}
	})
	s, err := Open(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Send([]byte(`{"id":"1","cmd":"pane.read"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case line := <-s.Lines():
		var msg map[string]any
		if err := json.Unmarshal(line, &msg); err != nil {
			t.Fatalf("the reply line did not survive whole: %v", err)
		}
		if msg["result"] != big {
			t.Fatal("the 100 KB payload came back changed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no reply within 5 s")
	}
}

func TestCallMatchesTheReplyByIdAndIgnoresEventsInBetween(t *testing.T) {
	// The row below is one row exactly as handlePaneList builds it: flat, with
	// state, source and detail merged in from the current event, plus ts and
	// session_id.
	row := map[string]any{
		"id": "w1:p1", "label": "shell", "cwd": "/repo", "cmd": []any{"bash"},
		"kind": "pty", "harness": "", "workspace": "w1", "tab": "t1", "closed": false,
		"cols": 120, "rows": 40,
		"state": proto.StateIdle, "source": proto.SrcManifest, "detail": "",
		"ts": 1000.0, "session_id": "s1",
	}
	_, d := newFakeServer(t, func(req map[string]any) []map[string]any {
		return []map[string]any{
			{"event": "state", "pane": "w1:p1", "state": proto.StateWorking},
			{"id": "not-yours", "ok": true},
			{"id": req["id"], "ok": true, "result": map[string]any{"panes": []any{row}}},
		}
	})
	msg, err := Call(context.Background(), d, map[string]any{"cmd": "pane.list"})
	if err != nil {
		t.Fatal(err)
	}
	if msg["ok"] != true {
		t.Fatalf("Call returned %v", msg)
	}
}

// The failure this names: a refusal returned as a success becomes an empty
// roster painted on top of a real error, which is exactly the guessing this
// package refuses to do. The code has to reach the caller intact.
func TestCallReturnsAnErrorWhenTheServerRefuses(t *testing.T) {
	_, d := newFakeServer(t, func(req map[string]any) []map[string]any {
		return []map[string]any{{"id": req["id"], "ok": false, "error": map[string]any{
			"code": proto.ErrNoSuchWorkspace, "message": `no workspace "w9". Run: coppice workspace list`,
		}}}
	})
	msg, err := Call(context.Background(), d, map[string]any{"cmd": "pane.list"})
	if err == nil {
		t.Fatal("Call reported a refusal as success")
	}
	if msg != nil {
		t.Fatalf("Call returned a body alongside the error: %v", msg)
	}
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("Call returned %T, want *RefusedError", err)
	}
	if refused.Code != string(proto.ErrNoSuchWorkspace) {
		t.Errorf("code is %q", refused.Code)
	}
	if !strings.Contains(refused.Message, "coppice workspace list") {
		t.Errorf("the server's teaching message was dropped: %q", refused.Message)
	}
}

// A refusal with no error body must still be an error, not a success with
// nothing in it.
func TestCallTreatsAnEmptyRefusalAsAnError(t *testing.T) {
	_, d := newFakeServer(t, func(req map[string]any) []map[string]any {
		return []map[string]any{{"id": req["id"], "ok": false}}
	})
	if _, err := Call(context.Background(), d, map[string]any{"cmd": "pane.list"}); err == nil {
		t.Fatal("an ok:false with no error body was reported as success")
	}
}

// The failure this names: with no server there is nothing to report, and a
// caller must get an error rather than an invented empty success.
func TestCallFailsWhenTheSocketIsNotThere(t *testing.T) {
	d := UnixDialer{Path: filepath.Join(t.TempDir(), "nothing.sock")}
	if _, err := Call(context.Background(), d, map[string]any{"cmd": "pane.list"}); err == nil {
		t.Fatal("Call succeeded with no server listening")
	}
}

func TestSessionCloseStopsTheReader(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	s, err := Open(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	select {
	case _, ok := <-s.Lines():
		if ok {
			t.Fatal("Lines yielded after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Lines did not close within 2 s of Close")
	}
	// Close unblocks the reader by closing the connection out from under it,
	// so Err reports that closed connection rather than staying nil.
	if s.Err() == nil {
		t.Fatal("Err was nil after Close stopped the reader")
	}
}

// The failure this names: a reply over MaxLineBytes must reach the caller as
// the real read error, not a generic closed connection message that hides
// why the request actually failed.
func TestCallReportsTheScannerErrorWhenAReplyExceedsMaxLineBytes(t *testing.T) {
	tooBig := strings.Repeat("y", MaxLineBytes+1)
	_, d := newFakeServer(t, func(req map[string]any) []map[string]any {
		return []map[string]any{{"id": req["id"], "ok": true, "result": tooBig}}
	})
	_, err := Call(context.Background(), d, map[string]any{"cmd": "pane.read"})
	if err == nil {
		t.Fatal("Call succeeded with a reply over MaxLineBytes")
	}
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("Call returned %v, want an error wrapping bufio.ErrTooLong", err)
	}
}

// The failure this names: a request the server's own decoder would refuse
// must fail on this side of the wire with a clear reason, instead of being
// written and then dropped by a server that never replies.
func TestSendRefusesALineAtOrOverMaxRequestBytes(t *testing.T) {
	f, d := newFakeServer(t, echoOK)
	s, err := Open(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// The line's length plus the appended newline lands exactly at MaxRequestBytes.
	line := paddedJSONLine("1", MaxRequestBytes-1)
	if err := s.Send(line); err == nil {
		t.Fatal("Send accepted a line at the server's inbound limit")
	}
	if len(f.Received()) != 0 {
		t.Fatal("Send wrote a refused line to the wire")
	}
}

// A line one byte under the refusal boundary must still go through whole.
func TestSendAcceptsALineJustUnderMaxRequestBytes(t *testing.T) {
	f, d := newFakeServer(t, echoOK)
	s, err := Open(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// The line's length plus the appended newline lands one byte under MaxRequestBytes.
	line := paddedJSONLine("1", MaxRequestBytes-2)
	if err := s.Send(line); err != nil {
		t.Fatalf("Send refused a line under the server's limit: %v", err)
	}
	select {
	case reply := <-s.Lines():
		if !bytes.Contains(reply, []byte(`"ok":true`)) {
			t.Fatalf("reply was %s", reply)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no reply within 5 s")
	}
	got := f.Received()
	if len(got) != 1 || len(got[0]) != len(line) {
		t.Fatalf("the server saw %d lines, want one of length %d", len(got), len(line))
	}
}

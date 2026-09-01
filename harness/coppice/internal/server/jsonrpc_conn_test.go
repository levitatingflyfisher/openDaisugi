package server

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/proto"
)

// wireConn drives ServeConn over pipes and reads raw lines back, so a test
// can see the exact framing the server chose.
type wireConn struct {
	t  *testing.T
	in *io.PipeWriter
	r  *bufio.Reader
}

func openWire(t *testing.T, s *Server) *wireConn {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() {
		s.ServeConn(inR, outW)
		_ = outW.Close()
	}()
	t.Cleanup(func() { _ = inW.Close() })
	return &wireConn{t: t, in: inW, r: bufio.NewReader(outR)}
}

func (w *wireConn) write(line string) {
	w.t.Helper()
	if _, err := io.WriteString(w.in, line+"\n"); err != nil {
		w.t.Fatal(err)
	}
}

// line reads one raw line as a map, failing after a bounded wait.
func (w *wireConn) line() map[string]any {
	w.t.Helper()
	type res struct {
		b   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() {
		b, err := w.r.ReadBytes('\n')
		ch <- res{b, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			w.t.Fatalf("reading a line: %v", r.err)
		}
		var m map[string]any
		if err := json.Unmarshal(r.b, &m); err != nil {
			w.t.Fatalf("server sent a line that is not a JSON object: %q", r.b)
		}
		return m
	case <-time.After(5 * time.Second):
		w.t.Fatal("no line within 5 s")
		return nil
	}
}

// reply reads lines until one carries an id key.
func (w *wireConn) reply() map[string]any {
	w.t.Helper()
	for {
		m := w.line()
		if _, has := m["id"]; has {
			return m
		}
	}
}

// call sends one line and returns its reply.
func (w *wireConn) call(line string) map[string]any {
	w.t.Helper()
	w.write(line)
	return w.reply()
}

func rpcError(t *testing.T, m map[string]any) (code float64, message string, data map[string]any) {
	t.Helper()
	e, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("want an error reply, got %v", m)
	}
	code, _ = e["code"].(float64)
	message, _ = e["message"].(string)
	data, _ = e["data"].(map[string]any)
	return code, message, data
}

func TestJSONRPCRequestGetsAJSONRPCReplyWithItsNumericID(t *testing.T) {
	s := newTestServer(t)
	w := openWire(t, s)
	m := w.call(`{"jsonrpc":"2.0","id":7,"method":"server.status"}`)
	if m["jsonrpc"] != "2.0" || m["id"] != float64(7) {
		t.Fatalf("got %v", m)
	}
	if _, has := m["ok"]; has {
		t.Fatalf("a JSON-RPC reply must not carry ok: %v", m)
	}
	res, _ := m["result"].(map[string]any)
	if res["protocol"] != float64(proto.Version) {
		t.Fatalf("result.protocol = %v, want %d", res["protocol"], proto.Version)
	}
}

func TestJSONRPCParamsReachTheHandler(t *testing.T) {
	s := newTestServer(t)
	var seen atomic.Value
	_ = s.Handle("test.echo", func(_ *Client, r *proto.Request) proto.Response {
		v, _ := r.Str("word")
		seen.Store(v)
		return proto.OKResp(r.ID, map[string]any{"word": v})
	})
	w := openWire(t, s)
	m := w.call(`{"jsonrpc":"2.0","id":"x","method":"test.echo","params":{"word":"hello"}}`)
	if m["id"] != "x" || m["result"].(map[string]any)["word"] != "hello" {
		t.Fatalf("got %v", m)
	}
	if seen.Load() != "hello" {
		t.Fatal("params did not reach the handler")
	}
}

func TestJSONRPCNativeErrorMapsToServerErrorWithTheNativeCode(t *testing.T) {
	s := newTestServer(t)
	w := openWire(t, s)
	m := w.call(`{"jsonrpc":"2.0","id":2,"method":"pane.read","params":{"pane":"w9:p9"}}`)
	code, msg, data := rpcError(t, m)
	if code != -32000 || data["code"] != "no_such_pane" || !strings.Contains(msg, "w9:p9") {
		t.Fatalf("got %v", m)
	}
}

func TestJSONRPCUnknownMethodIsMethodNotFound(t *testing.T) {
	s := newTestServer(t)
	w := openWire(t, s)
	m := w.call(`{"jsonrpc":"2.0","id":3,"method":"pane.fly"}`)
	code, msg, data := rpcError(t, m)
	if code != -32601 || data["code"] != "bad_request" || !strings.Contains(msg, "Known commands:") {
		t.Fatalf("got %v", m)
	}
	if m["id"] != float64(3) {
		t.Fatalf("id = %v, want 3", m["id"])
	}
}

func TestJSONRPCNotificationIsRefusedAndNeverRuns(t *testing.T) {
	s := newTestServer(t)
	var calls int32
	_ = s.Handle("test.count", func(_ *Client, r *proto.Request) proto.Response {
		atomic.AddInt32(&calls, 1)
		return proto.OKResp(r.ID, nil)
	})
	w := openWire(t, s)
	m := w.call(`{"jsonrpc":"2.0","method":"test.count"}`)
	code, msg, _ := rpcError(t, m)
	if code != -32600 || msg != "coppice does not accept notifications, send an id" || m["id"] != nil {
		t.Fatalf("got %v", m)
	}
	if _, has := m["id"]; !has {
		t.Fatal("the refusal must carry id null, not omit it")
	}
	// A later request on the same connection proves the earlier one never ran.
	w.call(`{"jsonrpc":"2.0","id":1,"method":"server.status"}`)
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatal("a notification ran a handler")
	}
}

func TestJSONRPCBatchIsRefused(t *testing.T) {
	s := newTestServer(t)
	w := openWire(t, s)
	m := w.call(`[{"jsonrpc":"2.0","id":1,"method":"server.status"}]`)
	code, _, _ := rpcError(t, m)
	if code != -32600 || m["id"] != nil {
		t.Fatalf("got %v", m)
	}
}

func TestInvalidJSONAnswersNativeBeforeTheSwitchAndParseErrorAfter(t *testing.T) {
	s := newTestServer(t)
	w := openWire(t, s)
	m := w.call(`{not json`)
	if m["ok"] != false || m["error"].(map[string]any)["code"] != "bad_request" {
		t.Fatalf("before the switch, want the native bad_request line, got %v", m)
	}
	w.call(`{"jsonrpc":"2.0","id":1,"method":"server.status"}`)
	m = w.call(`{not json`)
	code, _, _ := rpcError(t, m)
	if code != -32700 || m["id"] != nil || m["jsonrpc"] != "2.0" {
		t.Fatalf("after the switch, want -32700 with id null, got %v", m)
	}
}

func TestANativeLineAfterTheSwitchIsRefused(t *testing.T) {
	s := newTestServer(t)
	var calls int32
	_ = s.Handle("test.count", func(_ *Client, r *proto.Request) proto.Response {
		atomic.AddInt32(&calls, 1)
		return proto.OKResp(r.ID, nil)
	})
	w := openWire(t, s)
	w.call(`{"jsonrpc":"2.0","id":1,"method":"server.status"}`)
	m := w.call(`{"id":"2","cmd":"test.count"}`)
	code, _, _ := rpcError(t, m)
	if code != -32600 || m["id"] != "2" {
		t.Fatalf("got %v", m)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatal("a native line ran a handler on a JSON-RPC connection")
	}
}

func TestNativeConnectionStillAnswersNatively(t *testing.T) {
	s := newTestServer(t)
	w := openWire(t, s)
	m := w.call(`{"id":"9","cmd":"server.status"}`)
	if m["id"] != "9" || m["ok"] != true {
		t.Fatalf("got %v", m)
	}
	if _, has := m["jsonrpc"]; has {
		t.Fatalf("a native reply must not carry jsonrpc: %v", m)
	}
}

func TestJSONRPCEventsArriveAsNotifications(t *testing.T) {
	s := newPaneServer(t)
	w := openWire(t, s)
	m := w.call(`{"jsonrpc":"2.0","id":1,"method":"pane.create","params":{"cwd":"` + t.TempDir() +
		`","cmd_argv":["sh","-c","sleep 30"],"kind":"pty","label":"n"}}`)
	res, _ := m["result"].(map[string]any)
	id, _ := res["pane"].(string)
	if id == "" {
		t.Fatalf("pane.create over JSON-RPC failed: %v", m)
	}
	w.call(`{"jsonrpc":"2.0","id":2,"method":"events.subscribe","params":{"kinds":["state"],"panes":"*"}}`)
	w.write(`{"jsonrpc":"2.0","id":3,"method":"pane.report_state","params":{"pane":"` + id + `","event":{` +
		`"v":1,"ts":1,"session_id":"` + id + `","harness":"shell","state":"blocked","source":"gate",` +
		`"detail":"","ask":{"id":"a1","tool":"Bash","summary":"rm","deadline":9999999999}}}}`)
	var ev map[string]any
	for ev == nil {
		l := w.line()
		if _, has := l["id"]; has {
			continue
		}
		ev = l
	}
	if ev["jsonrpc"] != "2.0" || ev["method"] != "state" {
		t.Fatalf("want a state notification, got %v", ev)
	}
	if _, has := ev["event"]; has {
		t.Fatalf("event must move into method: %v", ev)
	}
	params, _ := ev["params"].(map[string]any)
	if params["pane"] != id || params["state"] != "blocked" {
		t.Fatalf("params = %v", params)
	}
	if _, has := params["event"]; has {
		t.Fatalf("params must not repeat the event name: %v", params)
	}
}

// A native line with no id at all, sent after the switch, is refused with
// id null. The reply cannot name an id the line never carried, and an empty
// string would claim one.
func TestANativeLineWithoutAnIDAfterTheSwitchAnswersNull(t *testing.T) {
	s := newTestServer(t)
	w := openWire(t, s)
	w.call(`{"jsonrpc":"2.0","id":1,"method":"server.status"}`)
	m := w.call(`{"cmd":"server.status"}`)
	code, _, _ := rpcError(t, m)
	if code != -32600 {
		t.Fatalf("got %v", m)
	}
	if id, has := m["id"]; !has || id != nil {
		t.Fatalf("id = %v, want null", id)
	}
}

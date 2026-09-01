package proto

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestDecodeJSONRPCRequestMapsMethodToCmd(t *testing.T) {
	r, f, err := DecodeLine([]byte(`{"jsonrpc":"2.0","id":7,"method":"pane.read","params":{"pane":"w1:p1","lines":3}}`))
	if err != nil || f != JSONRPC || r.Cmd != "pane.read" || r.ID != "7" {
		t.Fatalf("got %+v %v %v", r, f, err)
	}
	if n, _ := r.Int("lines"); n != 3 {
		t.Fatal("params not flattened")
	}
	if string(r.RawID) != "7" {
		t.Fatalf("RawID = %q, want the numeric token 7", r.RawID)
	}
}

func TestDecodeNativeLineKeepsTheRawID(t *testing.T) {
	r, f, err := DecodeLine([]byte(`{"id":"1","cmd":"pane.list"}`))
	if err != nil || f != Native || r.Cmd != "pane.list" || r.ID != "1" {
		t.Fatalf("got %+v %v %v", r, f, err)
	}
	if string(r.RawID) != `"1"` {
		t.Fatalf("RawID = %q, want the raw token \"1\"", r.RawID)
	}
}

func TestDecodeJSONRPCStringIDStaysAString(t *testing.T) {
	r, _, err := DecodeLine([]byte(`{"jsonrpc":"2.0","id":"a1","method":"server.status"}`))
	if err != nil || r.ID != "a1" || string(r.RawID) != `"a1"` {
		t.Fatalf("got %+v %v", r, err)
	}
	if r.Params == nil {
		t.Fatal("a request with no params must still have an empty Params map")
	}
}

func decodeErr(t *testing.T, line string) (Request, Framing, *DecodeError) {
	t.Helper()
	r, f, err := DecodeLine([]byte(line))
	var de *DecodeError
	if !errors.As(err, &de) {
		t.Fatalf("%s: want a DecodeError, got %v", line, err)
	}
	return r, f, de
}

func TestDecodeRefusesANotification(t *testing.T) {
	r, f, de := decodeErr(t, `{"jsonrpc":"2.0","method":"server.status"}`)
	if f != JSONRPC || de.Code != RPCInvalidRequest {
		t.Fatalf("got framing %v code %d", f, de.Code)
	}
	if de.Message != "coppice does not accept notifications, send an id" {
		t.Fatalf("message = %q", de.Message)
	}
	if string(r.RawID) != "null" {
		t.Fatalf("RawID = %q, want null", r.RawID)
	}
	_, _, de = decodeErr(t, `{"jsonrpc":"2.0","id":null,"method":"server.status"}`)
	if de.Code != RPCInvalidRequest {
		t.Fatalf("a null id is a notification too, got code %d", de.Code)
	}
}

func TestDecodeRefusesABatch(t *testing.T) {
	_, f, de := decodeErr(t, `[{"jsonrpc":"2.0","id":1,"method":"server.status"}]`)
	if f != JSONRPC || de.Code != RPCInvalidRequest {
		t.Fatalf("got framing %v code %d", f, de.Code)
	}
	// An array with no JSON-RPC member does not switch the framing.
	_, f, de = decodeErr(t, `[1,2]`)
	if f != Native || de.Code != RPCInvalidRequest {
		t.Fatalf("got framing %v code %d", f, de.Code)
	}
}

func TestDecodeRefusesParamsThatAreNotAnObject(t *testing.T) {
	r, _, de := decodeErr(t, `{"jsonrpc":"2.0","id":3,"method":"pane.read","params":["w1:p1"]}`)
	if de.Code != RPCInvalidRequest || string(r.RawID) != "3" {
		t.Fatalf("got code %d RawID %q", de.Code, r.RawID)
	}
}

func TestDecodeRefusesABadIDType(t *testing.T) {
	r, _, de := decodeErr(t, `{"jsonrpc":"2.0","id":{"x":1},"method":"pane.read"}`)
	if de.Code != RPCInvalidRequest || string(r.RawID) != "null" {
		t.Fatalf("got code %d RawID %q", de.Code, r.RawID)
	}
}

func TestDecodeRefusesAMissingMethod(t *testing.T) {
	r, _, de := decodeErr(t, `{"jsonrpc":"2.0","id":4}`)
	if de.Code != RPCInvalidRequest || string(r.RawID) != "4" {
		t.Fatalf("got code %d RawID %q", de.Code, r.RawID)
	}
}

func TestDecodeInvalidJSONIsAParseError(t *testing.T) {
	_, f, de := decodeErr(t, `{not json`)
	if f != Native || de.Code != RPCParseError {
		t.Fatalf("got framing %v code %d", f, de.Code)
	}
}

func TestEncodeErrorInJSONRPCForm(t *testing.T) {
	b := EncodeResponse(ErrResp("7", ErrNoSuchPane, "no pane w9"), JSONRPC)
	if !strings.Contains(string(b), `"code":-32000`) || !strings.Contains(string(b), `"no_such_pane"`) {
		t.Fatalf("bad encoding: %s", b)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["jsonrpc"] != "2.0" || m["id"] != "7" {
		t.Fatalf("got %s", b)
	}
	if _, has := m["ok"]; has {
		t.Fatalf("a JSON-RPC reply must not carry the native ok key: %s", b)
	}
	e := m["error"].(map[string]any)
	if e["message"] != "no pane w9" || e["data"].(map[string]any)["code"] != "no_such_pane" {
		t.Fatalf("got %s", b)
	}
}

func TestEncodeRestoresANumericID(t *testing.T) {
	r := OKResp("7", map[string]int{"n": 1})
	r.RawID = json.RawMessage("7")
	want := `{"jsonrpc":"2.0","id":7,"result":{"n":1}}`
	if got := string(EncodeResponse(r, JSONRPC)); got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	// Native encoding ignores RawID and emits the string id.
	wantNative := `{"id":"7","ok":true,"result":{"n":1}}`
	if got := string(EncodeResponse(r, Native)); got != wantNative {
		t.Fatalf("got %s, want %s", got, wantNative)
	}
}

func TestEncodeJSONRPCSuccessAlwaysCarriesResult(t *testing.T) {
	want := `{"jsonrpc":"2.0","id":"1","result":null}`
	if got := string(EncodeResponse(OKResp("1", nil), JSONRPC)); got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestEncodeRPCCodeOverridesTheServerErrorCode(t *testing.T) {
	r := ErrResp("1", ErrBadRequest, "no command")
	r.RPCCode = RPCMethodNotFound
	b := EncodeResponse(r, JSONRPC)
	if !strings.Contains(string(b), `"code":-32601`) || !strings.Contains(string(b), `"bad_request"`) {
		t.Fatalf("got %s", b)
	}
	r.RawID = json.RawMessage("null")
	if !strings.Contains(string(EncodeResponse(r, JSONRPC)), `"id":null`) {
		t.Fatal("a null RawID must come back as null")
	}
}

func TestEncodeAnUnencodableResultFailsClosed(t *testing.T) {
	bad := OKResp("1", map[string]any{"f": func() {}})
	for _, f := range []Framing{Native, JSONRPC} {
		b := EncodeResponse(bad, f)
		if len(b) == 0 || !strings.Contains(string(b), `"internal"`) {
			t.Fatalf("framing %v: want an internal error line, got %s", f, b)
		}
	}
}

func TestEncodeEventNativeIsPlainJSON(t *testing.T) {
	f := Frame{Event: "frame", Pane: "w1:p1", Seq: 1, Cols: 1, Rows: 1,
		RowsChanged: map[int][]Cell{0: {{Text: "h"}}}}
	want, _ := json.Marshal(f)
	if got := EncodeEvent(f, Native); !bytes.Equal(got, want) {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestEncodeEventJSONRPCLiftsEventIntoMethod(t *testing.T) {
	f := Frame{Event: "frame", Pane: "w1:p1", Seq: 18446744073709551615, Cols: 1, Rows: 1,
		RowsChanged: map[int][]Cell{0: {{Text: "h"}}}}
	b := EncodeEvent(f, JSONRPC)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	if string(m["jsonrpc"]) != `"2.0"` || string(m["method"]) != `"frame"` {
		t.Fatalf("got %s", b)
	}
	if _, has := m["event"]; has {
		t.Fatalf("event must move into method: %s", b)
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(m["params"], &params); err != nil {
		t.Fatal(err)
	}
	if _, has := params["event"]; has {
		t.Fatalf("params must not repeat the event name: %s", b)
	}
	if string(params["pane"]) != `"w1:p1"` || string(params["seq"]) != "18446744073709551615" {
		t.Fatalf("params lost a field or a digit: %s", b)
	}
}

func TestEncodeEventWithoutAnEventNameIsNil(t *testing.T) {
	if b := EncodeEvent(map[string]any{"pane": "x"}, JSONRPC); b != nil {
		t.Fatalf("want nil for an event with no name, got %s", b)
	}
	if b := EncodeEvent(func() {}, Native); b != nil {
		t.Fatalf("want nil for an unencodable event, got %s", b)
	}
}

func TestSendBytesWritesOneLine(t *testing.T) {
	var buf bytes.Buffer
	e := NewEncoder(&buf)
	if err := e.SendBytes([]byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := e.Send(OKResp("2", nil)); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "{\"a\":1}\n{\"id\":\"2\",\"ok\":true}\n" {
		t.Fatalf("got %q", got)
	}
}

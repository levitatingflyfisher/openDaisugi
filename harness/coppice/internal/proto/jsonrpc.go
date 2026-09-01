package proto

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Framing is the shape of the lines on one connection. A connection starts
// Native. The first line that carries "jsonrpc":"2.0" switches it to JSONRPC
// for the rest of its life. PROTOCOL.md states both shapes.
type Framing int

const (
	Native Framing = iota
	JSONRPC
)

func (f Framing) String() string {
	if f == JSONRPC {
		return "jsonrpc"
	}
	return "native"
}

// JSON-RPC 2.0 error codes the server uses. Every native error code maps to
// RPCServerError with the native code under data.code. The other three name
// a line the server could not turn into a request at all.
const (
	RPCParseError     = -32700
	RPCInvalidRequest = -32600
	RPCMethodNotFound = -32601
	RPCServerError    = -32000
)

// DecodeError is why DecodeLine refused a line. Code is the JSON-RPC error
// code a JSON-RPC connection answers with. A native connection answers
// bad_request with Message.
type DecodeError struct {
	Code    int
	Message string
}

func (e *DecodeError) Error() string { return e.Message }

const notJSONMessage = "this line is not JSON. Send one JSON object per line."

var jsonNull = json.RawMessage("null")

// DecodeLine turns one wire line into a Request and reports which framing
// the line used. A native line is an object without "jsonrpc":"2.0". A
// JSON-RPC line maps method to Cmd and flattens params into Params. The id
// token is kept raw so a reply can echo its type.
//
// A refused line returns a *DecodeError. When the refusal is JSON-RPC, the
// returned Request carries the RawID a reply must use: the request's own id
// when it had a usable one, and null when it did not. A JSON array reports
// JSONRPC framing only when one of its members is a JSON-RPC request, so a
// stray array on a native connection does not switch it.
func DecodeLine(b []byte) (Request, Framing, error) {
	line := bytes.TrimSpace(b)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(line, &m); err != nil {
		var arr []json.RawMessage
		if json.Unmarshal(line, &arr) != nil {
			return Request{}, Native, &DecodeError{RPCParseError, notJSONMessage}
		}
		f := Native
		for _, el := range arr {
			var em map[string]json.RawMessage
			if json.Unmarshal(el, &em) == nil && isJSONRPC(em) {
				f = JSONRPC
				break
			}
		}
		return Request{RawID: jsonNull}, f, &DecodeError{RPCInvalidRequest,
			"coppice does not accept batches. Send one request per line."}
	}
	if !isJSONRPC(m) {
		var r Request
		r.fromMap(m)
		return r, Native, nil
	}

	r := Request{Params: map[string]json.RawMessage{}, RawID: jsonNull}
	idRaw, ok := m["id"]
	if !ok || isJSONNull(idRaw) {
		return r, JSONRPC, &DecodeError{RPCInvalidRequest, "coppice does not accept notifications, send an id"}
	}
	var s string
	var n json.Number
	switch {
	case json.Unmarshal(idRaw, &s) == nil:
		r.ID = s
	case json.Unmarshal(idRaw, &n) == nil:
		r.ID = n.String()
	default:
		return r, JSONRPC, &DecodeError{RPCInvalidRequest, "id must be a string or a number"}
	}
	r.RawID = bytes.TrimSpace(idRaw)

	methodRaw, ok := m["method"]
	if !ok || json.Unmarshal(methodRaw, &r.Cmd) != nil || r.Cmd == "" {
		return r, JSONRPC, &DecodeError{RPCInvalidRequest, "method must be a non-empty string"}
	}
	if paramsRaw, ok := m["params"]; ok && !isJSONNull(paramsRaw) {
		if err := json.Unmarshal(paramsRaw, &r.Params); err != nil || r.Params == nil {
			r.Params = map[string]json.RawMessage{}
			return r, JSONRPC, &DecodeError{RPCInvalidRequest, "params must be a JSON object"}
		}
	}
	return r, JSONRPC, nil
}

func isJSONRPC(m map[string]json.RawMessage) bool {
	raw, ok := m["jsonrpc"]
	return ok && string(bytes.TrimSpace(raw)) == `"2.0"`
}

type rpcOK struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result"`
}

type rpcErr struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   rpcErrorBody    `json:"error"`
}

type rpcErrorBody struct {
	Code    int          `json:"code"`
	Message string       `json:"message"`
	Data    rpcErrorData `json:"data"`
}

type rpcErrorData struct {
	Code ErrCode `json:"code"`
}

// EncodeResponse renders one reply in the given framing, without the
// trailing newline. Native ignores RawID and RPCCode and emits the string
// id. JSON-RPC emits RawID verbatim when set, so a numeric id comes
// back numeric, and falls back to the string id otherwise. A reply that
// cannot be marshalled becomes an internal error reply with the same id, so
// a caller never waits on a reply that was silently dropped.
func EncodeResponse(r Response, f Framing) []byte {
	b, err := encodeResponse(r, f)
	if err == nil {
		return b
	}
	fallback := ErrResp(r.ID, ErrInternal, fmt.Sprintf("this reply could not be encoded: %v", err))
	fallback.RawID = r.RawID
	b, err = encodeResponse(fallback, f)
	if err != nil {
		// ErrResp holds only strings, so this cannot fail. A plain line in
		// the connection's own framing is still better than nothing if it
		// ever does.
		if f == JSONRPC {
			return []byte(lastResortJSONRPC)
		}
		return []byte(lastResortNative)
	}
	return b
}

// The two last-resort reply lines, one per framing, for a reply that could
// not be encoded even as an error.
const (
	lastResortNative  = `{"id":"","ok":false,"error":{"code":"internal","message":"this reply could not be encoded"}}`
	lastResortJSONRPC = `{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"this reply could not be encoded","data":{"code":"internal"}}}`
)

func encodeResponse(r Response, f Framing) ([]byte, error) {
	if f != JSONRPC {
		return json.Marshal(r)
	}
	id := r.RawID
	if len(id) == 0 {
		var err error
		if id, err = json.Marshal(r.ID); err != nil {
			return nil, err
		}
	}
	if r.OK {
		return json.Marshal(rpcOK{JSONRPC: "2.0", ID: id, Result: r.Result})
	}
	code := r.RPCCode
	if code == 0 {
		code = RPCServerError
	}
	body := rpcErrorBody{Code: code}
	if r.Error != nil {
		body.Message = r.Error.Message
		body.Data.Code = r.Error.Code
	} else {
		body.Message = "the server refused this request"
		body.Data.Code = ErrInternal
	}
	return json.Marshal(rpcErr{JSONRPC: "2.0", ID: id, Error: body})
}

type rpcNotification struct {
	JSONRPC string                     `json:"jsonrpc"`
	Method  string                     `json:"method"`
	Params  map[string]json.RawMessage `json:"params"`
}

// EncodeEvent renders one event in the given framing, without the trailing
// newline. Native marshals ev as it is. JSON-RPC lifts the event field into
// method and puts every other field under params, keeping each value's
// original token so no number loses a digit. It returns nil when ev cannot
// be encoded or names no event, and the caller drops the client rather than
// send it a line it cannot read.
func EncodeEvent(ev any, f Framing) []byte {
	b, err := json.Marshal(ev)
	if err != nil {
		return nil
	}
	if f != JSONRPC {
		return b
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return nil
	}
	var method string
	if raw, ok := fields["event"]; !ok || json.Unmarshal(raw, &method) != nil || method == "" {
		return nil
	}
	delete(fields, "event")
	out, err := json.Marshal(rpcNotification{JSONRPC: "2.0", Method: method, Params: fields})
	if err != nil {
		return nil
	}
	return out
}

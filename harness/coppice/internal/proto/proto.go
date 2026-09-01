// Package proto is the coppice wire format: one JSON object per line, both
// ways. Requests carry an id, responses echo it, events carry none. See master
// spec section 3.3.
package proto

import "encoding/json"

// Version is the protocol version this package speaks. PROTOCOL.md carries
// the same number, and server.status reports it under "protocol". A client
// checks it once, before it relies on any verb or event shape.
const Version = 1

// ErrCode is the closed enum from spec-02. A client switches on these, so a
// code outside the set is a server bug, not a new case.
type ErrCode string

const (
	ErrBadRequest      ErrCode = "bad_request"
	ErrNoSuchPane      ErrCode = "no_such_pane"
	ErrNoSuchWorkspace ErrCode = "no_such_workspace"
	ErrNoSuchTab       ErrCode = "no_such_tab"
	ErrPaneClosed      ErrCode = "pane_closed"
	// ErrServerClosed is what pane.create
	// answers once Close has started. Distinct from ErrPaneClosed, which
	// always names one particular pane: this is server-scoped, not
	// pane-scoped, and pane.create has no pane of its own to name yet.
	ErrServerClosed ErrCode = "server_closed"
	ErrNotAttached  ErrCode = "not_attached"
	ErrAdapter      ErrCode = "adapter_error"
	ErrSpawnFailed  ErrCode = "spawn_failed"
	ErrTimeout      ErrCode = "timeout"
	ErrUnauthorized ErrCode = "unauthorized"
	ErrInternal     ErrCode = "internal"
)

var validCodes = map[ErrCode]bool{
	ErrBadRequest: true, ErrNoSuchPane: true, ErrNoSuchWorkspace: true, ErrNoSuchTab: true,
	ErrPaneClosed: true, ErrServerClosed: true, ErrNotAttached: true, ErrAdapter: true,
	ErrSpawnFailed: true, ErrTimeout: true, ErrUnauthorized: true, ErrInternal: true,
}

func ValidCode(c ErrCode) bool { return validCodes[c] }

// Request is one line from a client. Command parameters sit beside id and cmd
// at the top level, so they are captured raw and read with the typed helpers.
// On a JSON-RPC line, DecodeLine fills Cmd from method and Params from the
// params object, so a handler reads both framings the same way.
//
// RawID is the id token exactly as the client sent it. A reply copies it, so
// a JSON-RPC client that sent a number gets a number back.
type Request struct {
	ID     string                     `json:"id"`
	Cmd    string                     `json:"cmd"`
	Params map[string]json.RawMessage `json:"-"`
	RawID  json.RawMessage            `json:"-"`
}

func (r *Request) UnmarshalJSON(b []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	r.fromMap(m)
	return nil
}

// fromMap fills a native request from its decoded object. An id or cmd that
// is not a string is left empty.
func (r *Request) fromMap(m map[string]json.RawMessage) {
	r.Params = m
	if raw, ok := m["id"]; ok {
		_ = json.Unmarshal(raw, &r.ID)
		r.RawID = raw
	}
	if raw, ok := m["cmd"]; ok {
		_ = json.Unmarshal(raw, &r.Cmd)
	}
}

func (r *Request) Str(k string) (string, bool) {
	raw, ok := r.Params[k]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

func (r *Request) Int(k string) (int, bool) {
	raw, ok := r.Params[k]
	if !ok {
		return 0, false
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	return n, true
}

func (r *Request) Bool(k string) (bool, bool) {
	raw, ok := r.Params[k]
	if !ok {
		return false, false
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return false, false
	}
	return v, true
}

func (r *Request) StrSlice(k string) ([]string, bool) {
	raw, ok := r.Params[k]
	if !ok {
		return nil, false
	}
	var v []string
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false
	}
	return v, true
}

func (r *Request) StrMap(k string) (map[string]string, bool) {
	raw, ok := r.Params[k]
	if !ok {
		return nil, false
	}
	var v map[string]string
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false
	}
	return v, true
}

// Raw hands back an untouched parameter for a caller that needs its own shape.
func (r *Request) Raw(k string) (json.RawMessage, bool) {
	raw, ok := r.Params[k]
	return raw, ok
}

type Error struct {
	Code    ErrCode `json:"code"`
	Message string  `json:"message"`
}

// Response is one reply. RawID and RPCCode never reach a native line. The
// server copies the request's RawID in before encoding, so a JSON-RPC reply
// echoes the id with its original type. RPCCode, when set, replaces the
// -32000 a native error would map to on a JSON-RPC line; the read loop sets
// it for a method the server does not know and for a line it could not
// decode.
type Response struct {
	ID      string          `json:"id"`
	OK      bool            `json:"ok"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
	RawID   json.RawMessage `json:"-"`
	RPCCode int             `json:"-"`
}

func OKResp(id string, result any) Response { return Response{ID: id, OK: true, Result: result} }

// ErrResp refuses to emit a code outside the enum. A caller that invents one
// gets "internal" and the invented code in the message, so the bug is visible
// without a client having to handle an unknown value.
func ErrResp(id string, code ErrCode, msg string) Response {
	if !ValidCode(code) {
		msg = string(code) + ": " + msg
		code = ErrInternal
	}
	return Response{ID: id, OK: false, Error: &Error{Code: code, Message: msg}}
}

// Cell is one grid cell on the wire: [text, fg, bg, attrs]. An empty colour
// string means the terminal default.
type Cell struct {
	Text  string
	FG    string
	BG    string
	Attrs uint16
}

func (c Cell) MarshalJSON() ([]byte, error) {
	return json.Marshal([]any{c.Text, c.FG, c.BG, c.Attrs})
}

func (c *Cell) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if len(raw) != 4 {
		return json.Unmarshal(b, &struct{}{})
	}
	if err := json.Unmarshal(raw[0], &c.Text); err != nil {
		return err
	}
	if err := json.Unmarshal(raw[1], &c.FG); err != nil {
		return err
	}
	if err := json.Unmarshal(raw[2], &c.BG); err != nil {
		return err
	}
	return json.Unmarshal(raw[3], &c.Attrs)
}

// Frame is one render update for one pane. Seq counts per client, because two
// clients attach at different moments and each needs its own baseline.
type Frame struct {
	Event       string         `json:"event"`
	Pane        string         `json:"pane"`
	Seq         uint64         `json:"seq"`
	Cols        int            `json:"cols"`
	Rows        int            `json:"rows"`
	Cursor      [2]int         `json:"cursor"`
	RowsChanged map[int][]Cell `json:"rows_changed"`
	// Full is true when RowsChanged carries every row. A client applies a
	// full frame to an empty grid. A delta frame leaves the key out, so its
	// bytes are the same as before the key existed.
	Full bool `json:"full,omitempty"`
}

// FrameGap tells a client that the server dropped Dropped frames for Pane on
// this connection because the client did not read them in time. The frame
// that follows a gap is always full.
type FrameGap struct {
	Event   string `json:"event"`
	Pane    string `json:"pane"`
	Dropped int    `json:"dropped"`
}

// Package mcp exposes sprig's tools to a driver (like `claude`) over the Model
// Context Protocol — JSON-RPC 2.0 on stdio. This is Design C, the pi-claude-bridge
// pattern: the DRIVER does the reasoning and emits native tool calls; sprig
// executes them. The whole point is the routing — every tools/call runs through
// the SAME gated Executor, so Claude drives but sprig's boundary still rules.
package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"

	sprig "github.com/opendaisugi/sprig"
)

// protocolVersion is the MCP revision sprig's server speaks in its handshake.
const protocolVersion = "2024-11-05"

// Server answers MCP requests by running tool calls through a gated Executor.
type Server struct {
	exec *sprig.Executor
	name string
}

// NewServer wraps a gated Executor as an MCP tool server.
func NewServer(exec *sprig.Executor) *Server {
	return &Server{exec: exec, name: "sprig"}
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // absent => a notification
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Handle dispatches one JSON-RPC message and returns the response bytes. The
// bool is false when the message is a notification that needs no reply.
func (s *Server) Handle(raw []byte) ([]byte, bool) {
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return s.rpcError(nil, -32700, "parse error"), true
	}
	notification := len(req.ID) == 0
	switch req.Method {
	case "notifications/initialized":
		return nil, false
	case "initialize":
		return s.reply(req.ID, s.initializeResult())
	case "tools/list":
		return s.reply(req.ID, s.toolsList())
	case "tools/call":
		return s.reply(req.ID, s.toolsCall(req.Params))
	default:
		if notification {
			return nil, false
		}
		return s.rpcError(req.ID, -32601, "method not found: "+req.Method), true
	}
}

// Serve runs the MCP stdio transport: newline-delimited JSON-RPC in, one
// response line per request out (notifications produce none). It reads with an
// unbounded reader, not a line scanner, because a write tool call can carry a
// large file payload. Returns nil at EOF.
func (s *Server) Serve(r io.Reader, w io.Writer) error {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if msg := bytes.TrimSpace(line); len(msg) > 0 {
			if resp, ok := s.Handle(msg); ok {
				if _, werr := w.Write(append(resp, '\n')); werr != nil {
					return werr
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func (s *Server) reply(id json.RawMessage, result any) ([]byte, bool) {
	if id == nil {
		id = json.RawMessage("null")
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	return b, true
}

func (s *Server) rpcError(id json.RawMessage, code int, msg string) []byte {
	if id == nil {
		id = json.RawMessage("null")
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
	return b
}

// --- result builders ---

func (s *Server) initializeResult() any {
	return map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": s.name, "version": "0.1.0"},
	}
}

// toolSchemas are the JSON input schemas sprig advertises for its fixed tool
// vocabulary. The driver reads these to emit well-formed native tool calls —
// this is what makes MCP calls structured where the text protocol only hoped.
var toolSchemas = map[string]any{
	"read":  objectSchema([]string{"path"}, "path"),
	"write": objectSchema([]string{"path", "content"}, "path", "content"),
	"edit":  objectSchema([]string{"path", "old", "new"}, "path", "old", "new"),
	"bash":  objectSchema([]string{"cmd"}, "cmd"),
}

var toolDescriptions = map[string]string{
	"read":  "Return a file's contents.",
	"write": "Write content to a file (creates or overwrites).",
	"edit":  "Replace a unique occurrence of old with new in a file.",
	"bash":  "Run a shell command and return its output.",
}

// objectSchema builds a minimal JSON Schema: every named field is a string, and
// the `required` list is passed first.
func objectSchema(required []string, fields ...string) map[string]any {
	props := map[string]any{}
	for _, f := range fields {
		props[f] = map[string]any{"type": "string"}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func (s *Server) toolsList() any {
	tools := []any{}
	for _, name := range s.exec.ToolNames() {
		schema, ok := toolSchemas[name]
		if !ok {
			schema = map[string]any{"type": "object"} // a custom tool still gets a valid schema
		}
		tools = append(tools, map[string]any{
			"name":        name,
			"description": toolDescriptions[name],
			"inputSchema": schema,
		})
	}
	return map[string]any{"tools": tools}
}

func (s *Server) toolsCall(params json.RawMessage) any {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	_ = json.Unmarshal(params, &p)
	if p.Arguments == nil {
		p.Arguments = map[string]any{}
	}
	res := s.exec.Execute(sprig.ToolCall{Name: p.Name, Input: p.Arguments})

	var text string
	isErr := false
	switch {
	case !res.Allowed:
		text, isErr = "REFUSED by the gate: "+res.Reason, true
	case res.Err != nil:
		text, isErr = "error: "+res.Err.Error(), true
		if res.Output != "" {
			text += "\n" + res.Output
		}
	default:
		text = res.Output
	}
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": isErr}
}

package pando

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
)

// MaxToolLine is the longest request line the MCP server reads.
const MaxToolLine = 8 << 20

const mcpProtocol = "2024-11-05"

// IntArg reads a whole-number argument of a tool call.
func IntArg(args map[string]any, name string) (int64, error) {
	v, ok := args[name]
	if !ok {
		return 0, fmt.Errorf("%s is missing", name)
	}
	f, ok := v.(float64)
	if !ok || f != math.Trunc(f) || math.Abs(f) > 1<<53 {
		return 0, fmt.Errorf("%s must be a whole number", name)
	}
	return int64(f), nil
}

// StrArg reads a text argument. A missing one is "" unless need is set.
func StrArg(args map[string]any, name string, need bool) (string, error) {
	v, ok := args[name]
	if !ok || v == nil {
		if need {
			return "", fmt.Errorf("%s is missing", name)
		}
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s must be text", name)
	}
	return s, nil
}

// ZoomDescription is the text the model reads for the zoom tool.
const ZoomDescription = "Open a node of the chat's memory: give id and n. id is the first message, n the count: a power of two. n = 1 gives the message word for word; a larger node gives its two halves as lines. A long node comes in pages: give offset (a byte) and size to read the next page. A bad id answers with the root ids."

// ZoomChildText is what the zoom tool says about child stores.
const ZoomChildText = " A worker's report names its child store and the root ids of its transcript: give child, the worker's label, with an id and an n from those roots to read inside that transcript. Without child the id is in the chat."

// DateDescription is the text the model reads for the date tool.
const DateDescription = "When message id was logged, in UTC."

// RecallDescription is the text the model reads for the recall tool.
const RecallDescription = "Find where a word or a topic was said: give a query. You get node ids with a score and one line each, best first. Then zoom into an id to read it. k is how many (default 8, at most 32)."

// RecallBranchText is what the recall tool says about branches.
const RecallBranchText = " A hit in a branch below has an id like trellis:12+4: zoom it with branch trellis, id 12 and n 4."

// ZoomBranchText is what the zoom and date tools say about branches.
const ZoomBranchText = " Give branch, a path of labels such as trellis or trellis/ui, to read inside a branch below; the id is then in that branch."

var (
	intProp = map[string]any{"type": "integer"}
	strProp = map[string]any{"type": "string"}
)

// ObjectSchema is a JSON schema for a tool's object argument.
func ObjectSchema(required []string, props map[string]any) map[string]any {
	if required == nil {
		required = []string{} // never null: MCP wants an array
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

// ToolZoom answers the zoom tool over one chat.
func ToolZoom(c *Chat, args map[string]any) (string, error) {
	id, err := IntArg(args, "id")
	if err != nil {
		return "", err
	}
	n, err := IntArg(args, "n")
	if err != nil {
		return "", err
	}
	var offset, size int64
	if _, ok := args["offset"]; ok {
		if offset, err = IntArg(args, "offset"); err != nil {
			return "", err
		}
	}
	if _, ok := args["size"]; ok {
		if size, err = IntArg(args, "size"); err != nil {
			return "", err
		}
	}
	return c.ZoomPage(id, n, offset, int(min(size, int64(ZoomPageBytes)+1)))
}

// ToolZoomChild answers the zoom tool of the pando server over c's store:
// see ToolZoomScope.
func ToolZoomChild(c *Chat, args map[string]any) (string, error) {
	return ToolZoomScope(Scope{Store: c.cfg.Dir, Dir: c.cfg.Dir}, args)
}

// ToolZoomScope answers the zoom tool inside a scope. branch (a path of
// labels from the scope) picks a branch below; child picks a worker's
// child store of that branch (or of the scope). Neither ever leaves the
// scope: BranchDir and ChildDir refuse "..", absolute paths and links. With
// neither it is ToolZoom on the scope.
func ToolZoomScope(sc Scope, args map[string]any) (string, error) {
	c, err := scopedChat(sc, args, true)
	if err != nil {
		return "", err
	}
	return ToolZoom(c, args)
}

// scopedChat opens, read only, the store a tool call names inside sc.
func scopedChat(sc Scope, args map[string]any, child bool) (*Chat, error) {
	if sc.Closed {
		return nil, errors.New(sc.Why)
	}
	br, err := StrArg(args, "branch", false)
	if err != nil {
		return nil, err
	}
	dir, err := sc.Below(br)
	if err != nil {
		return nil, err
	}
	if child {
		label, err := StrArg(args, "child", false)
		if err != nil {
			return nil, err
		}
		if label != "" {
			d, err := ChildDir(dir, label)
			if err != nil {
				return nil, err
			}
			if st, serr := os.Lstat(d); serr != nil || !st.IsDir() {
				return nil, fmt.Errorf("no worker transcript is named %q", label)
			}
			dir = d
		}
	}
	if !hasMain(dir) {
		if _, err := os.Stat(dir); err != nil {
			return nil, err
		}
		return &Chat{cfg: Config{Dir: dir}, readOnly: true}, nil
	}
	return OpenReadOnly(dir)
}

// ToolDateScope answers the date tool inside a scope, with an optional branch.
func ToolDateScope(sc Scope, args map[string]any) (string, error) {
	c, err := scopedChat(sc, args, false)
	if err != nil {
		return "", err
	}
	return ToolDate(c, args)
}

// ToolRecallScope answers the recall tool over the scope and every branch
// below it.
func ToolRecallScope(sc Scope, args map[string]any) (string, error) {
	if sc.Closed {
		return "", errors.New(sc.Why)
	}
	q, err := StrArg(args, "query", true)
	if err != nil {
		return "", err
	}
	k := int64(8)
	if _, ok := args["k"]; ok {
		if k, err = IntArg(args, "k"); err != nil {
			return "", err
		}
	}
	hits, err := RecallTree(sc.Dir, q, int(min(max(k, 1), RecallMax)))
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return "no match", nil
	}
	lines := make([]string, len(hits))
	for i, h := range hits {
		lines[i] = FormatHit(h)
	}
	return strings.Join(lines, "\n"), nil
}

// ToolViewScope answers the view tool: the scope's own view, with a line
// for each live branch right below it.
func ToolViewScope(sc Scope, _ map[string]any) (string, error) {
	if sc.Closed {
		return "", errors.New(sc.Why)
	}
	t := ""
	if hasMain(sc.Dir) {
		var err error
		if t, err = ViewContextOf(sc.Dir); err != nil {
			return "", err
		}
	}
	if t == "" {
		if t = BranchLines(sc.Dir); t != "" {
			return t, nil
		}
		return "The memory holds no message yet.", nil
	}
	return t, nil
}

// ToolDate answers the date tool.
func ToolDate(c *Chat, args map[string]any) (string, error) {
	id, err := IntArg(args, "id")
	if err != nil {
		return "", err
	}
	return c.Date(id)
}

// ToolRecall answers the recall tool: lines "id<TAB>score<TAB>line".
func ToolRecall(c *Chat, args map[string]any) (string, error) {
	q, err := StrArg(args, "query", true)
	if err != nil {
		return "", err
	}
	k := int64(8)
	if _, ok := args["k"]; ok {
		if k, err = IntArg(args, "k"); err != nil {
			return "", err
		}
	}
	hits := c.Recall(q, int(min(max(k, 1), RecallMax)))
	if len(hits) == 0 {
		return "no match", nil
	}
	lines := make([]string, len(hits))
	for i, h := range hits {
		lines[i] = FormatHit(h)
	}
	return strings.Join(lines, "\n"), nil
}

// ViewDescription is the text of the view tool.
const ViewDescription = "Show the view again: the summary lines of every chat, fine near now and coarse in the past, then the newest messages that have no summary yet. No arguments. Use recall to search and zoom to read."

// ToolView answers the view tool.
func ToolView(c *Chat, _ map[string]any) (string, error) {
	if t := c.ViewContext(); t != "" {
		return t, nil
	}
	return "The memory holds no message yet.", nil
}

// ReadTools is the tools/list result of the pando server.
func ReadTools() []any { return ScopeTools(false) }

// ScopeTools is the tools/list result of a pando server. A worker of a
// branch gets no view tool: it searches and zooms, and no view is pushed.
func ScopeTools(worker bool) []any {
	tools := []any{
		map[string]any{"name": "zoom", "description": ZoomDescription + ZoomChildText + ZoomBranchText,
			"inputSchema": ObjectSchema([]string{"id", "n"}, map[string]any{"id": intProp, "n": intProp, "offset": intProp, "size": intProp, "child": strProp, "branch": strProp})},
		map[string]any{"name": "date", "description": DateDescription + ZoomBranchText,
			"inputSchema": ObjectSchema([]string{"id"}, map[string]any{"id": intProp, "branch": strProp})},
		map[string]any{"name": "recall", "description": RecallDescription + RecallBranchText,
			"inputSchema": ObjectSchema([]string{"query"}, map[string]any{"query": strProp, "k": intProp})},
	}
	if !worker {
		tools = append(tools, map[string]any{"name": "view", "description": ViewDescription,
			"inputSchema": ObjectSchema(nil, map[string]any{})})
	}
	return tools
}

// RPCRequest is one JSON-RPC line.
type RPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// RPCReply makes a JSON-RPC result line.
func RPCReply(id json.RawMessage, result any) []byte {
	if id == nil {
		id = json.RawMessage("null")
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	return b
}

// RPCError makes a JSON-RPC error line.
func RPCError(id json.RawMessage, code int, msg string) []byte {
	if id == nil {
		id = json.RawMessage("null")
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
	return b
}

// Version is the version the server reports.
const Version = "1"

// ServeMCP runs the stdio server named pando over one chat: zoom, date,
// recall and view. It has no write tool. It returns when in ends. c is read only;
// the caller opens it with OpenReadOnly.
func ServeMCP(ctx context.Context, c *Chat, in io.Reader, out io.Writer) error {
	return ServeScope(ctx, Scope{Store: c.cfg.Dir, Dir: c.cfg.Dir}, in, out)
}

// ServeScope runs the pando server over a scope: the store, or a branch and
// everything below it. A worker's scope has no view tool. A closed scope
// lists no tool.
func ServeScope(ctx context.Context, sc Scope, in io.Reader, out io.Writer) error {
	return serveMCP(ctx, &sc, in, out)
}

// ClosedText is what a closed pando server answers to a call.
const ClosedText = "The memory is closed to this process: it is not a chat of the owner's."

// ServeMCPClosed is the pando server of a process that is not the owner's
// chat (a steward worker, a model call of daisugi's own). The MCP entry sits
// in the user's Claude Code config, so every Claude Code under that home
// starts it. It lists no tool and answers every call with ClosedText, so the
// store stays out of that process.
func ServeMCPClosed(ctx context.Context, in io.Reader, out io.Writer) error {
	return serveMCP(ctx, &Scope{Closed: true, Why: ClosedText}, in, out)
}

func serveMCP(ctx context.Context, sc *Scope, in io.Reader, out io.Writer) error {
	br := bufio.NewReaderSize(in, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if msg := bytes.TrimSpace(line); len(msg) > 0 {
			if resp, ok := mcpHandle(sc, msg); ok {
				if _, werr := out.Write(append(resp, '\n')); werr != nil {
					return werr
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
	}
}

func mcpHandle(sc *Scope, raw []byte) ([]byte, bool) {
	var req RPCRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return RPCError(nil, -32700, "parse error"), true
	}
	switch req.Method {
	case "notifications/initialized":
		return nil, false
	case "initialize":
		return RPCReply(req.ID, map[string]any{
			"protocolVersion": mcpProtocol,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "pando", "version": Version},
		}), true
	case "tools/list":
		if sc.Closed {
			return RPCReply(req.ID, map[string]any{"tools": []any{}}), true // closed: see ServeMCPClosed
		}
		return RPCReply(req.ID, map[string]any{"tools": ScopeTools(sc.Worker)}), true
	case "tools/call":
		if sc.Closed {
			why := sc.Why
			if why == "" {
				why = ClosedText
			}
			return RPCReply(req.ID, map[string]any{
				"content": []any{map[string]any{"type": "text", "text": why}}, "isError": true,
			}), true
		}
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if p.Arguments == nil {
			p.Arguments = map[string]any{}
		}
		// Each call reads the store as it is now: reads take no lock, and a
		// server that runs for hours must see what was logged since it
		// started.
		var text string
		var err error
		switch {
		case p.Name == "zoom":
			text, err = ToolZoomScope(*sc, p.Arguments)
		case p.Name == "date":
			text, err = ToolDateScope(*sc, p.Arguments)
		case p.Name == "recall":
			text, err = ToolRecallScope(*sc, p.Arguments)
		case p.Name == "view" && !sc.Worker:
			text, err = ToolViewScope(*sc, p.Arguments)
		default:
			names := "zoom, date, recall, view"
			if sc.Worker {
				names = "zoom, date, recall"
			}
			return RPCError(req.ID, -32601, fmt.Sprintf("no tool named %q. The tools are %s.", p.Name, names)), true
		}
		isErr := err != nil
		if isErr {
			text = err.Error()
		}
		return RPCReply(req.ID, map[string]any{
			"content": []any{map[string]any{"type": "text", "text": text}}, "isError": isErr,
		}), true
	default:
		if len(req.ID) == 0 {
			return nil, false
		}
		return RPCError(req.ID, -32601, "method not found: "+req.Method), true
	}
}

package steward

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"daisugi-verify/pando"
	"github.com/opendaisugi/coppice/internal/datahome"
)

// The steward's tools reach the model through a small MCP server that
// only relays. `coppice steward tools --sock S` is that server. It speaks
// MCP on stdio to claude, and for each tool call it opens the per-turn
// socket S, sends one request line and reads one response line. It holds
// no state, never dials the public socket, and runs no tool itself: the
// coppice server serves the tools, on a socket that lives one turn.

// ToolNames are the five tools, in the order the model sees them.
var ToolNames = []string{"zoom", "date", "recall", "view", "spawn", "status", "reply"}

// ToolRequest is one request line on the turn socket.
type ToolRequest struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

// ToolResponse is the one response line.
type ToolResponse struct {
	OK     bool   `json:"ok"`
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// MaxToolLine is the longest request or response line on the turn socket.
const MaxToolLine = 8 << 20

const mcpProtocol = "2024-11-05"

// objectSchema builds an input schema. required is never null: the MCP
// schema says an array, and Claude Code rejects the whole tools/list on a
// null.
func objectSchema(required []string, props map[string]any) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

var (
	intProp  = map[string]any{"type": "integer"}
	strProp  = map[string]any{"type": "string"}
	boolProp = map[string]any{"type": "boolean"}
)

// toolList is the tools/list result.
func toolList() map[string]any {
	tools := []any{
		map[string]any{"name": "zoom", "description": "Open a node of the chat's memory: give id and n. id is the first message, n the count: a power of two. n = 1 gives the message word for word; a larger node gives its two halves as lines. A long node comes in pages: give offset (a byte) and size to read the next page. A bad id answers with the root ids. A worker's report names its child store and the root ids of its transcript: give child, the worker's label, with an id and an n from those roots to read inside that transcript. Without child the id is in the chat." + pando.ZoomBranchText,
			"inputSchema": objectSchema(nil, map[string]any{"id": intProp, "n": intProp, "offset": intProp, "size": intProp, "child": strProp, "branch": strProp})},
		map[string]any{"name": "date", "description": "When message id was logged, in UTC." + pando.ZoomBranchText,
			"inputSchema": objectSchema([]string{"id"}, map[string]any{"id": intProp, "branch": strProp})},
		map[string]any{"name": "recall", "description": pando.RecallDescription + pando.RecallBranchText,
			"inputSchema": objectSchema([]string{"query"}, map[string]any{"query": strProp, "k": intProp})},
		map[string]any{"name": "view", "description": pando.ViewDescription,
			"inputSchema": objectSchema(nil, map[string]any{})},
		map[string]any{"name": "spawn", "description": "Start a worker on a project with a brief. It returns at once with a label. The worker's report comes back later as a message that starts with the label in brackets. A profile that names a pool puts the worker on a machine with a free slot, or in the queue (the answer says which); background: true makes it a job for an idle machine, only on a pool. attempts: 2 to 8 runs that many attempts in worktrees of their own, keeps the best by ranking and opens a review card for the operator; it needs why: failed_verify, failed_tests, low_confidence or operator. The report names the card. A worker may ask a question while it works: it comes back as a message that starts with the label and says it asks, and you answer it with reply. branch names the project's branch of the memory (trellis, or trellis/ui below it): a worker with a branch can search and zoom that branch, and its transcript is kept there. long_running: true starts a long-running agent instead, in a pane of its own: its chat is that branch (the project name when branch is left out), you see it in the view and in recall, and it never sees the memory above its branch. Use long_running: true for a task with more than a few steps, or with any build or test loop: such a task is a lead with a branch, not a worker. A worker suits a short task: each of its shell steps is killed past the envelope's step cap (30 s by default).",
			"inputSchema": objectSchema([]string{"project", "brief"}, map[string]any{"project": strProp, "brief": strProp, "profile": strProp, "model": strProp, "attempts": intProp, "why": strProp, "branch": strProp, "long_running": boolProp})},
		map[string]any{"name": "status", "description": "The workers: label, project, state, and when each started. The last line counts the open review cards. A worker that waits for an answer shows its question.",
			"inputSchema": objectSchema(nil, map[string]any{})},
		map[string]any{"name": "reply", "description": "Answer a worker that asked a question: give its label and the answer. The worker goes on with the answer as its next message. A question that says it is for the owner is the owner's to decide: reply only with the owner's answer.",
			"inputSchema": objectSchema([]string{"label", "text"}, map[string]any{"label": strProp, "text": strProp})},
	}
	return map[string]any{"tools": tools}
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func rpcReply(id json.RawMessage, result any) []byte {
	if id == nil {
		id = json.RawMessage("null")
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	return b
}

func rpcError(id json.RawMessage, code int, msg string) []byte {
	if id == nil {
		id = json.RawMessage("null")
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
	return b
}

// relay sends one request to the turn socket and returns the response.
func relay(sock string, req ToolRequest) ToolResponse {
	conn, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return ToolResponse{Error: "the steward's tools are closed: this turn is over"}
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))
	b, _ := json.Marshal(req)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return ToolResponse{Error: "the steward's tools did not take the call: " + err.Error()}
	}
	line, err := bufio.NewReaderSize(conn, 64*1024).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return ToolResponse{Error: "the steward's tools gave no answer"}
	}
	if len(line) > MaxToolLine {
		return ToolResponse{Error: "the answer is too long"}
	}
	var resp ToolResponse
	if err := json.Unmarshal(bytes.TrimSpace(line), &resp); err != nil {
		return ToolResponse{Error: "the steward's tools gave an answer that is not JSON"}
	}
	return resp
}

// handle answers one MCP message. The bool is false for a notification.
func handle(sock string, raw []byte) ([]byte, bool) {
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return rpcError(nil, -32700, "parse error"), true
	}
	switch req.Method {
	case "notifications/initialized":
		return nil, false
	case "initialize":
		return rpcReply(req.ID, map[string]any{
			"protocolVersion": mcpProtocol,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "steward", "version": "1"},
		}), true
	case "tools/list":
		return rpcReply(req.ID, toolList()), true
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if p.Arguments == nil {
			p.Arguments = map[string]any{}
		}
		known := false
		for _, n := range ToolNames {
			known = known || n == p.Name
		}
		var text string
		isErr := false
		if !known {
			text, isErr = fmt.Sprintf("no tool named %q. The tools are %s.", p.Name, strings.Join(ToolNames, ", ")), true
		} else {
			resp := relay(sock, ToolRequest{Tool: p.Name, Args: p.Arguments})
			text, isErr = resp.Result, !resp.OK
			if !resp.OK {
				text = resp.Error
			}
		}
		return rpcReply(req.ID, map[string]any{
			"content": []any{map[string]any{"type": "text", "text": text}}, "isError": isErr,
		}), true
	default:
		if len(req.ID) == 0 {
			return nil, false
		}
		return rpcError(req.ID, -32601, "method not found: "+req.Method), true
	}
}

// ToolsMain runs `coppice steward tools --sock S`: the MCP stdio relay.
func ToolsMain(argv []string, in io.Reader, out, errw io.Writer) int {
	return ToolsMainIn("", argv, in, out, errw)
}

// StandingSock is the pane steward's tool socket for a coppice data dir:
// tools.sock in the log dir. The server listens there once it mounts the
// tools in a pane steward.
func StandingSock(coppiceDataDir string) string {
	return filepath.Join(LogDir(coppiceDataDir), "tools.sock")
}

const toolsUsage = "usage: coppice steward tools [--standing | --sock SOCKET]"

// ToolsMainIn is ToolsMain with the data dir that `coppice --data-dir`
// named, "" for none. With no --sock, or with --standing, it dials the
// standing socket of that data dir, else of $COPPICE_DATA_DIR (every pane
// has it), else of the default data dir.
func ToolsMainIn(dataDir string, argv []string, in io.Reader, out, errw io.Writer) int {
	sock, standing := "", false
	for k := 0; k < len(argv); k++ {
		switch {
		case argv[k] == "--sock" && k+1 < len(argv):
			sock = argv[k+1]
			k++
		case argv[k] == "--standing":
			standing = true
		default:
			fmt.Fprintln(errw, toolsUsage)
			return 1
		}
	}
	if sock != "" && standing {
		fmt.Fprintln(errw, toolsUsage)
		return 1
	}
	if sock == "" {
		if dataDir == "" {
			dataDir = os.Getenv("COPPICE_DATA_DIR")
		}
		if dataDir == "" {
			dataDir = filepath.Join(datahome.Default(), "coppice")
		}
		sock = StandingSock(dataDir)
	}
	br := bufio.NewReaderSize(in, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if msg := bytes.TrimSpace(line); len(msg) > 0 {
			if resp, ok := handle(sock, msg); ok {
				if _, werr := out.Write(append(resp, '\n')); werr != nil {
					return 1
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return 0
			}
			return 1
		}
	}
}

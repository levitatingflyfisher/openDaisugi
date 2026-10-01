package foreman

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

	"github.com/opendaisugi/coppice/internal/datahome"
)

// The foreman's four tools reach the model through a small MCP server that
// only relays. `coppice foreman tools --sock S` is that server. It speaks
// MCP on stdio to claude, and for each tool call it opens the per-turn
// socket S, sends one request line and reads one response line. It holds
// no state, never dials the public socket, and runs no tool itself: the
// coppice server serves the tools, on a socket that lives one turn.

// ToolNames are the four tools, in the order the model sees them.
var ToolNames = []string{"zoom", "date", "spawn", "status"}

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

func objectSchema(required []string, props map[string]any) map[string]any {
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
		map[string]any{"name": "zoom", "description": "Open a node of the chat's memory: give id and n. id is the first message, n the count: a power of two. n = 1 gives the message word for word; a larger node gives its two halves as lines. A long node comes in pages: give offset (a byte) and size to read the next page. A bad id answers with the root ids. Or give label, a worker's label: you get the newest 32 KiB of that worker's own log, once the worker has ended.",
			"inputSchema": objectSchema(nil, map[string]any{"id": intProp, "n": intProp, "offset": intProp, "size": intProp, "label": strProp})},
		map[string]any{"name": "date", "description": "When message id was logged, in UTC.",
			"inputSchema": objectSchema([]string{"id"}, map[string]any{"id": intProp})},
		map[string]any{"name": "spawn", "description": "Start a worker on a project with a brief. It returns at once with a label. The worker's report comes back later as a message that starts with the label in brackets. A profile that names a pool puts the worker on a machine with a free slot, or in the queue (the answer says which); background: true makes it a job for an idle machine, only on a pool.",
			"inputSchema": objectSchema([]string{"project", "brief"}, map[string]any{"project": strProp, "brief": strProp, "profile": strProp, "model": strProp})},
		map[string]any{"name": "status", "description": "The workers: label, project, state, and when each started.",
			"inputSchema": objectSchema(nil, map[string]any{})},
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
		return ToolResponse{Error: "the foreman's tools are closed: this turn is over"}
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))
	b, _ := json.Marshal(req)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return ToolResponse{Error: "the foreman's tools did not take the call: " + err.Error()}
	}
	line, err := bufio.NewReaderSize(conn, 64*1024).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return ToolResponse{Error: "the foreman's tools gave no answer"}
	}
	if len(line) > MaxToolLine {
		return ToolResponse{Error: "the answer is too long"}
	}
	var resp ToolResponse
	if err := json.Unmarshal(bytes.TrimSpace(line), &resp); err != nil {
		return ToolResponse{Error: "the foreman's tools gave an answer that is not JSON"}
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
			"serverInfo":      map[string]any{"name": "foreman", "version": "1"},
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

// ToolsMain runs `coppice foreman tools --sock S`: the MCP stdio relay.
func ToolsMain(argv []string, in io.Reader, out, errw io.Writer) int {
	return ToolsMainIn("", argv, in, out, errw)
}

// StandingSock is the pane foreman's tool socket for a coppice data dir:
// tools.sock in the log dir. The server listens there once it mounts the
// tools in a pane foreman.
func StandingSock(coppiceDataDir string) string {
	return filepath.Join(LogDir(coppiceDataDir), "tools.sock")
}

const toolsUsage = "usage: coppice foreman tools [--standing | --sock SOCKET]"

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

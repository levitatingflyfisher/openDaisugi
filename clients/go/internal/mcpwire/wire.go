// Package mcpwire is the MCP protocol as the oracle's server speaks it
// over stdio: the lines it reads, the messages it takes, and the JSON it
// writes. The oracle's server is the mcp SDK's FastMCP; this package
// copies what that SDK answers, byte for byte, without taking the SDK.
//
// A line is read with universal newlines (\n, \r\n and a lone \r end
// it) and decoded as UTF-8 with each bad byte run replaced, as Python's
// TextIOWrapper(errors="replace") reads stdin. A line that is not a
// JSON-RPC request or notification gets the SDK's error notification.
// A request with an id and a method the client may send is checked the
// way the SDK checks it; a request it rejects gets -32602.
package mcpwire

import (
	"bufio"
	"io"
	"math"
	"strings"

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// LineReader reads stdin as the oracle's stdio transport does.
type LineReader struct {
	r *bufio.Reader
}

// NewLineReader wraps r.
func NewLineReader(r io.Reader) *LineReader { return &LineReader{r: bufio.NewReaderSize(r, 1<<16)} }

// Next is the next line, its end translated to "\n", or false at the end
// of input. A last line with no end is returned as it is.
func (l *LineReader) Next() (string, bool) {
	var buf []byte
	for {
		c, err := l.r.ReadByte()
		if err != nil {
			if len(buf) == 0 {
				return "", false
			}
			return pystr.DecodeReplace(buf), true
		}
		switch c {
		case '\n':
			return pystr.DecodeReplace(buf) + "\n", true
		case '\r':
			// A \r\n is one end; the byte after a \r is read before the
			// line is given, as Python's newline decoder waits for it.
			if next, err := l.r.Peek(1); err == nil && next[0] == '\n' {
				_, _ = l.r.ReadByte()
			}
			return pystr.DecodeReplace(buf) + "\n", true
		}
		buf = append(buf, c)
	}
}

// Kind is what a line holds for the server.
type Kind int

const (
	// Invalid is a line the SDK cannot take as a request or notification:
	// not JSON, not an object, or a response. It gets ErrorNotification.
	Invalid Kind = iota
	// Request has an id: it gets one reply.
	Request
	// Notification has no id (or one the SDK does not read as an id): it
	// gets nothing.
	Notification
)

// Message is one line, classified.
type Message struct {
	Kind   Kind
	ID     any // pyjson.Int or string
	Method string
	// Params is the params object as the SDK passes it on: NaN and the
	// infinities made None (model_dump(mode="json")). Nil when absent or
	// null.
	Params *pyjson.Object
}

// Classify reads a line as JSONRPCMessage.model_validate_json does and
// says what the SDK does with it.
func Classify(line string) Message {
	v, jerr := pmodel.ParseJSON(line)
	if jerr != nil {
		return Message{Kind: Invalid}
	}
	o, ok := v.(*pyjson.Object)
	if !ok {
		return Message{Kind: Invalid}
	}
	if rpc, has := o.Get("jsonrpc"); !has || rpc != "2.0" {
		return Message{Kind: Invalid}
	}
	method, isStr := o.Value("method").(string)
	params, hasParams := o.Get("params")
	var po *pyjson.Object
	paramsOK := true
	if hasParams && params != nil {
		po, paramsOK = params.(*pyjson.Object)
	}
	if !isStr || !paramsOK {
		// Not a request or notification. A response, an error reply or
		// anything else leads to the same error notification.
		return Message{Kind: Invalid}
	}
	if po != nil {
		po = JSONMode(po).(*pyjson.Object)
	}
	id, hasID := o.Get("id")
	switch x := id.(type) {
	case pyjson.Int:
		if hasID {
			return Message{Kind: Request, ID: x, Method: method, Params: po}
		}
	case string:
		return Message{Kind: Request, ID: x, Method: method, Params: po}
	}
	return Message{Kind: Notification, Method: method, Params: po}
}

// JSONMode is a value as model_dump(mode="json") gives it: NaN and the
// infinities become None.
func JSONMode(v any) any {
	switch x := v.(type) {
	case pyjson.Float:
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return nil
		}
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = JSONMode(e)
		}
		return out
	case *pyjson.Object:
		out := pyjson.NewObjectCap(x.Len())
		for _, k := range x.Keys() {
			out.Set(k, JSONMode(x.Value(k)))
		}
		return out
	}
	return v
}

// The methods of the SDK's ClientRequest.
var clientMethods = map[string]bool{
	"ping": true, "initialize": true, "completion/complete": true, "logging/setLevel": true,
	"prompts/get": true, "prompts/list": true, "resources/list": true, "resources/templates/list": true,
	"resources/read": true, "resources/subscribe": true, "resources/unsubscribe": true, "tools/call": true,
	"tools/list": true, "tasks/get": true, "tasks/result": true, "tasks/list": true, "tasks/cancel": true,
}

// Carried is the set of client methods this binary checks and answers.
// A request for another client method is refused.
var Carried = map[string]bool{
	"ping": true, "initialize": true, "logging/setLevel": true, "prompts/get": true, "prompts/list": true,
	"resources/list": true, "resources/templates/list": true, "tools/call": true, "tools/list": true,
	"resources/read": true, "resources/subscribe": true, "resources/unsubscribe": true,
	"completion/complete": true, "tasks/get": true, "tasks/result": true, "tasks/list": true, "tasks/cancel": true,
}

// Check is ClientRequest.model_validate for a request: "" when the SDK
// takes it, "invalid" when it answers -32602, and "unported" for a client
// method this binary does not check.
func Check(m Message) string {
	if !clientMethods[m.Method] {
		return "invalid"
	}
	if !Carried[m.Method] {
		return "unported"
	}
	p := m.Params
	required := map[string]bool{"initialize": true, "logging/setLevel": true, "prompts/get": true, "tools/call": true,
		"resources/read": true, "resources/subscribe": true, "resources/unsubscribe": true,
		"completion/complete": true, "tasks/get": true, "tasks/result": true, "tasks/cancel": true}
	if p == nil {
		if required[m.Method] {
			return "invalid"
		}
		return ""
	}
	// RequestParams: an optional task and _meta with an optional
	// progressToken. A tool call's task (TaskMetadata: an optional int
	// ttl) is read and then not used, as the oracle's server does.
	if t, has := p.Get("task"); has && t != nil {
		if m.Method != "tools/call" {
			return "unported"
		}
		to, ok := t.(*pyjson.Object)
		if !ok {
			return "invalid"
		}
		if ttl, has := to.Get("ttl"); has && ttl != nil {
			if _, isInt := ttl.(pyjson.Int); !isInt {
				// pydantic's lax int takes some floats and strings.
				return "unported"
			}
		}
	}
	if meta, has := p.Get("_meta"); has && meta != nil {
		mo, ok := meta.(*pyjson.Object)
		if !ok {
			return "invalid"
		}
		if tok, has := mo.Get("progressToken"); has && tok != nil && !strOrInt(tok) {
			return "invalid"
		}
	}
	switch m.Method {
	case "tools/list", "resources/list", "resources/templates/list", "prompts/list":
		if !optStr(p, "cursor") {
			return "invalid"
		}
	case "resources/read", "resources/subscribe", "resources/unsubscribe":
		u, ok := p.Value("uri").(string)
		if !ok {
			return "invalid"
		}
		if _, why := NormURI(u); why != "" {
			return why
		}
	case "tasks/get", "tasks/result", "tasks/cancel":
		if _, ok := p.Value("taskId").(string); !ok {
			return "invalid"
		}
	case "tasks/list":
		if !optStr(p, "cursor") {
			return "invalid"
		}
	case "completion/complete":
		ref, ok := p.Value("ref").(*pyjson.Object)
		if !ok {
			return "invalid"
		}
		switch ref.Value("type") {
		case "ref/prompt":
			if _, ok := ref.Value("name").(string); !ok {
				return "invalid"
			}
			if !optStr(ref, "title") {
				return "invalid"
			}
		case "ref/resource":
			if _, ok := ref.Value("uri").(string); !ok {
				return "invalid"
			}
		default:
			return "invalid"
		}
		arg, ok := p.Value("argument").(*pyjson.Object)
		if !ok {
			return "invalid"
		}
		if _, ok := arg.Value("name").(string); !ok {
			return "invalid"
		}
		if _, ok := arg.Value("value").(string); !ok {
			return "invalid"
		}
		if c, has := p.Get("context"); has && c != nil {
			co, ok := c.(*pyjson.Object)
			if !ok {
				return "invalid"
			}
			if a, has := co.Get("arguments"); has && a != nil {
				ao, ok := a.(*pyjson.Object)
				if !ok {
					return "invalid"
				}
				for _, k := range ao.Keys() {
					if _, ok := ao.Value(k).(string); !ok {
						return "invalid"
					}
				}
			}
		}
	case "tools/call":
		if _, ok := p.Value("name").(string); !ok {
			return "invalid"
		}
		if a, has := p.Get("arguments"); has && a != nil {
			if _, ok := a.(*pyjson.Object); !ok {
				return "invalid"
			}
		}
	case "prompts/get":
		if _, ok := p.Value("name").(string); !ok {
			return "invalid"
		}
		if a, has := p.Get("arguments"); has && a != nil {
			ao, ok := a.(*pyjson.Object)
			if !ok {
				return "invalid"
			}
			for _, k := range ao.Keys() {
				if _, ok := ao.Value(k).(string); !ok {
					return "invalid"
				}
			}
		}
	case "logging/setLevel":
		switch p.Value("level") {
		case "debug", "info", "notice", "warning", "error", "critical", "alert", "emergency":
		default:
			return "invalid"
		}
	case "initialize":
		switch p.Value("protocolVersion").(type) {
		case string, pyjson.Int, bool:
		default:
			return "invalid"
		}
		caps, ok := p.Value("capabilities").(*pyjson.Object)
		if !ok {
			return "invalid"
		}
		for _, k := range []string{"experimental", "sampling", "elicitation", "roots", "tasks"} {
			if v, has := caps.Get(k); has && v != nil {
				if _, ok := v.(*pyjson.Object); !ok {
					return "invalid"
				}
			}
		}
		info, ok := p.Value("clientInfo").(*pyjson.Object)
		if !ok {
			return "invalid"
		}
		if _, ok := info.Value("name").(string); !ok {
			return "invalid"
		}
		if _, ok := info.Value("version").(string); !ok {
			return "invalid"
		}
		if !optStr(info, "title") || !optStr(info, "websiteUrl") {
			return "invalid"
		}
	}
	return ""
}

func strOrInt(v any) bool {
	switch v.(type) {
	case string, pyjson.Int, bool:
		return true
	}
	return false
}

// optStr: the key is absent, null or a str.
func optStr(o *pyjson.Object, k string) bool {
	v, has := o.Get(k)
	if !has || v == nil {
		return true
	}
	_, ok := v.(string)
	return ok
}

// Protocol is the version the server answers initialize with.
func Protocol(requested any) string {
	if s, ok := requested.(string); ok {
		for _, v := range SupportedProtocols {
			if s == v {
				return s
			}
		}
	}
	return LatestProtocol
}

// ErrorNotification is the line the SDK writes for a line it cannot take.
const ErrorNotification = `{"method":"notifications/message","params":{"level":"error",` +
	`"logger":"mcp.server.exception_handler","data":"Internal Server Error"},"jsonrpc":"2.0"}`

// Reply is a result line.
func Reply(id any, result string) string {
	return `{"jsonrpc":"2.0","id":` + Compact(id) + `,"result":` + result + `}`
}

// ErrorReply is an error line. data "" is written; a nil data is left out.
func ErrorReply(id any, code int, message string, data *string) string {
	e := pyjson.NewObject().Set("code", pyjson.Int{Text: itoa(code)}).Set("message", message)
	if data != nil {
		e.Set("data", *data)
	}
	return `{"jsonrpc":"2.0","id":` + Compact(id) + `,"error":` + Compact(e) + `}`
}

// InvalidParams is the SDK's reply to a request it does not take.
func InvalidParams(id any) string {
	empty := ""
	return ErrorReply(id, -32602, "Invalid request parameters", &empty)
}

// InitializeResult is the initialize reply's result.
func InitializeResult(protocol string) string {
	return `{"protocolVersion":` + Compact(protocol) + `,"capabilities":` + Capabilities +
		`,"serverInfo":{"name":` + Compact(ServerName) + `,"version":` + Compact(ServerVersion) +
		`},"instructions":` + Compact(Instructions) + `}`
}

// ToolResult is a CallToolResult: the text blocks, the structured content
// (nil: left out) and whether it is an error.
func ToolResult(texts []string, structured any, isError bool) string {
	var b strings.Builder
	b.WriteString(`{"content":[`)
	for i, t := range texts {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"type":"text","text":` + Compact(t) + `}`)
	}
	b.WriteString("]")
	if structured != nil {
		b.WriteString(`,"structuredContent":` + Compact(structured))
	}
	if isError {
		b.WriteString(`,"isError":true}`)
	} else {
		b.WriteString(`,"isError":false}`)
	}
	return b.String()
}

func itoa(n int) string {
	neg := n < 0
	if neg {
		n = -n
	}
	var d []byte
	for {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
		if n == 0 {
			break
		}
	}
	if neg {
		return "-" + string(d)
	}
	return string(d)
}

// specialSchemes are the WHATWG special schemes with their default ports.
var specialSchemes = map[string]string{"http": "80", "https": "443", "ws": "80", "wss": "443", "ftp": "21"}

// NormURI is pydantic's AnyUrl on a URI: the URL as it serializes, or why
// it is not one ("invalid": the SDK answers -32602; "unported": a form this
// binary does not normalize the way the URL parser does). The forms read:
// a scheme, then for http, https, ws, wss and ftp a host (lowercased, the
// default port dropped, an empty path made "/"), for file an empty host,
// and any other scheme kept as written; printable ASCII only, with no %,
// backslash, brackets or user info.
func NormURI(u string) (string, string) {
	i := strings.IndexByte(u, ':')
	if i <= 0 || !isAlpha(u[0]) {
		return "", "invalid"
	}
	for j := 1; j < i; j++ {
		c := u[j]
		if !(isAlpha(c) || (c >= '0' && c <= '9') || c == '+' || c == '-' || c == '.') {
			return "", "invalid"
		}
	}
	for j := 0; j < len(u); j++ {
		c := u[j]
		if c <= 0x20 || c >= 0x7f || c == '%' || c == '\\' || c == '[' || c == ']' {
			return "", "unported"
		}
	}
	scheme, rest := strings.ToLower(u[:i]), u[i+1:]
	port, special := specialSchemes[scheme]
	switch {
	case special:
		if !strings.HasPrefix(rest, "//") {
			return "", "unported"
		}
		rest = rest[2:]
		end := strings.IndexAny(rest, "/?#")
		if end < 0 {
			end = len(rest)
		}
		hostport, tail := rest[:end], rest[end:]
		if strings.Contains(hostport, "@") {
			return "", "unported"
		}
		host, p, hasPort := strings.Cut(hostport, ":")
		if host == "" {
			return "", "invalid"
		}
		if c := host[0]; c >= '0' && c <= '9' {
			return "", "unported"
		}
		host = strings.ToLower(host)
		if hasPort {
			if p == "" || strings.TrimLeft(p, "0123456789") != "" || len(p) > 5 {
				return "", "unported"
			}
			if p != port {
				host += ":" + p
			}
		}
		path := tail
		if k := strings.IndexAny(path, "?#"); k >= 0 {
			path = path[:k]
		}
		for _, seg := range strings.Split(path, "/") {
			if seg == "." || seg == ".." {
				return "", "unported" // the URL parser resolves dot segments
			}
		}
		if tail == "" || tail[0] != '/' {
			tail = "/" + tail
		}
		return scheme + "://" + host + tail, ""
	case scheme == "file":
		if !strings.HasPrefix(rest, "///") {
			return "", "unported"
		}
		return scheme + ":" + rest, ""
	}
	return scheme + ":" + rest, ""
}

func isAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

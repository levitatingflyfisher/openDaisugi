package gate

import (
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"daisugi-verify/internal/lazyre"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/tsbash"
)

// This file is the gate's side of the resident server (gate_server.py):
// one request line read off gate.sock, the call decided as the caller's,
// and the reply line. The server itself is in internal/cli.

// Caller is the pane identity a resident request names, as
// gate.resident_call is entered with it. Sock, Pane, HerdrPane and
// DataDir are set only when the request carried a string for them
// (gate_server._str_or_none). PeerPid is never from the request: the
// server reads it off the connection with SO_PEERCRED.
type Caller struct {
	Sock, Pane, HerdrPane, DataDir *string
	PeerPid                        *int
}

// MaxRequestBytes is gate_server._MAX_REQUEST: a request line is read up
// to its newline or this many bytes, whichever comes first.
const MaxRequestBytes = 4 * 1024 * 1024

// requestDepth is the nesting the oracle's JSON reader accepts on the
// server's request thread before it raises RecursionError: 9,992
// containers in all (found by bisection against the Python server).
const requestDepth = 9992

// PaneEnvKeys are the variables serve() drops from the server's own
// environment: it serves every session, so it is no one pane.
var PaneEnvKeys = []string{"COPPICE_SOCK", "COPPICE_PANE", "HERDR_PANE_ID", "HERDR_PANE"}

// PaneFreeEnviron is environ without PaneEnvKeys (gate._pane_free_env).
func PaneFreeEnviron(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		drop := false
		for _, p := range PaneEnvKeys {
			if k == p {
				drop = true
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

// Request is one resident request, decoded.
type Request struct {
	Argv   []string
	Stdin  []byte
	Caller Caller
}

// IsHookReport is argv[:2] == ["hook", "report"]: the request goes to
// `daisugi hook report`, not to the gate.
func (q *Request) IsHookReport() bool {
	return len(q.Argv) >= 2 && q.Argv[0] == "hook" && q.Argv[1] == "report"
}

// ParseRequest is the try block of gate_server._Handler.handle: the line
// read as json.loads reads bytes, argv as [str(a) for a in req["argv"]],
// stdin as base64.b64decode(req.get("stdin_b64", "")). false is the
// oracle's except: a bad request.
func ParseRequest(line []byte) (*Request, bool) {
	text, ok := requestText(line)
	if !ok {
		return nil, false
	}
	v, derr := pyjson.LoadsPy(text, requestDepth)
	if derr != nil {
		return nil, false
	}
	obj, isObj := v.(*pyjson.Object)
	if !isObj {
		return nil, false
	}
	argvV, has := obj.Get("argv")
	if !has {
		return nil, false
	}
	var argv []string
	switch x := argvV.(type) {
	case []any:
		argv = make([]string, len(x))
		for i, a := range x {
			argv[i] = pyStrOf(a)
		}
	case *pyjson.Object:
		argv = x.Keys()
	case string:
		for _, r := range pystr.Runes(x) {
			argv = append(argv, pystr.FromRunes([]rune{r}))
		}
	default:
		return nil, false
	}
	stdin := []byte{}
	if b, has := obj.Get("stdin_b64"); has {
		s, isStr := b.(string)
		if !isStr {
			return nil, false
		}
		for i := 0; i < len(s); i++ {
			if s[i] >= 0x80 {
				// "string argument should contain only ASCII characters"
				return nil, false
			}
		}
		raw, ok := b64decodeLenient(s)
		if !ok {
			return nil, false
		}
		stdin = raw
	}
	q := &Request{Argv: argv, Stdin: stdin}
	q.Caller.Sock = strField(obj, "coppice_sock")
	q.Caller.Pane = strField(obj, "coppice_pane")
	q.Caller.HerdrPane = strField(obj, "herdr_pane")
	q.Caller.DataDir = strField(obj, "coppice_data_dir")
	return q, true
}

func strField(o *pyjson.Object, k string) *string {
	if s, ok := o.Value(k).(string); ok {
		return &s
	}
	return nil
}

// requestText is the decode json.loads does on bytes: UTF-8 with
// surrogatepass (an encoded lone surrogate is kept), a UTF-8 BOM dropped.
// Python also reads UTF-16 and UTF-32 by their BOM or zero bytes; this
// reads UTF-8 only, so such a request is a bad request (ruling G2-2).
func requestText(b []byte) (string, bool) {
	switch {
	case hasPrefix(b, "\x00\x00\xfe\xff"), hasPrefix(b, "\xff\xfe\x00\x00"),
		hasPrefix(b, "\xfe\xff"), hasPrefix(b, "\xff\xfe"):
		return "", false
	case hasPrefix(b, "\xef\xbb\xbf"):
		b = b[3:]
	case len(b) >= 4 && (b[0] == 0 || b[1] == 0):
		return "", false
	case len(b) == 2 && (b[0] == 0 || b[1] == 0):
		return "", false
	}
	for i := 0; i < len(b); {
		r, n := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && n <= 1 {
			// An encoded surrogate (ED A0..BF 80..BF) is what
			// surrogatepass lets through; Go strings here hold it as is.
			if i+2 < len(b) && b[i] == 0xED && b[i+1] >= 0xA0 && b[i+1] <= 0xBF &&
				b[i+2] >= 0x80 && b[i+2] <= 0xBF {
				i += 3
				continue
			}
			return "", false
		}
		i += n
	}
	return string(b), true
}

func hasPrefix(b []byte, p string) bool { return len(b) >= len(p) && string(b[:len(p)]) == p }

// b64decodeLenient is base64.b64decode(s) with validate=False (CPython
// 3.12's binascii.a2b_base64 in non-strict mode): bytes outside the
// alphabet are skipped, a pad after two or three data characters ends the
// data, and data left one or two characters into a quad is an error.
func b64decodeLenient(s string) ([]byte, bool) {
	out := make([]byte, 0, len(s)*3/4)
	quad, pads := 0, 0
	var left byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '=' {
			if quad >= 2 {
				pads++
				if quad+pads >= 4 {
					return out, true
				}
			}
			continue
		}
		v := b64val(c)
		if v < 0 {
			continue
		}
		pads = 0
		x := byte(v)
		switch quad {
		case 0:
			quad, left = 1, x
		case 1:
			quad = 2
			out = append(out, left<<2|x>>4)
			left = x & 0x0f
		case 2:
			quad = 3
			out = append(out, left<<4|x>>2)
			left = x & 0x03
		case 3:
			quad = 0
			out = append(out, left<<6|x)
			left = 0
		}
	}
	return out, quad == 0
}

func b64val(c byte) int {
	switch {
	case c >= 'A' && c <= 'Z':
		return int(c - 'A')
	case c >= 'a' && c <= 'z':
		return int(c-'a') + 26
	case c >= '0' && c <= '9':
		return int(c-'0') + 52
	case c == '+':
		return 62
	case c == '/':
		return 63
	}
	return -1
}

// ReplyLine is the handler's reply: json.dumps of {v, stdout, stderr,
// exit_code}, and a newline.
func ReplyLine(stdout, stderr string, exit int) []byte {
	o := pyjson.NewObject().Set("v", 1).Set("stdout", stdout).Set("stderr", stderr).Set("exit_code", exit)
	return []byte(pyjson.Dumps(o, true) + "\n")
}

// BadRequest is the reply to a request the handler cannot read.
var BadRequest = ReplyLine("", "openDaisugi gate: DENIED — bad request", 2)

// ResidentEnv marks a child a resident server started: its stdin is the
// request (encodeRequest), not a hook payload. A parent `gate check`
// removes it from what its own child gets (ChildEnviron), so no host
// setting it can make a hook read its payload as a request.
const ResidentEnv = "DAISUGI_GATE_RESIDENT"

// encodeRequest is the child's stdin for a resident call. argv words go
// as JSON strings, which carry a NUL or a lone surrogate that an exec
// argv cannot.
func encodeRequest(q *Request) []byte {
	argv := make([]any, len(q.Argv))
	for i, a := range q.Argv {
		argv[i] = a
	}
	opt := func(p *string) any {
		if p == nil {
			return nil
		}
		return *p
	}
	var pid any
	if q.Caller.PeerPid != nil {
		pid = *q.Caller.PeerPid
	}
	o := pyjson.NewObject().
		Set("argv", argv).
		Set("stdin", base64.StdEncoding.EncodeToString(q.Stdin)).
		Set("sock", opt(q.Caller.Sock)).
		Set("pane", opt(q.Caller.Pane)).
		Set("herdr_pane", opt(q.Caller.HerdrPane)).
		Set("data_dir", opt(q.Caller.DataDir)).
		Set("peer_pid", pid)
	return []byte(pyjson.Dumps(o, true))
}

// decodeRequest reads encodeRequest's bytes back.
func decodeRequest(b []byte) (*Request, error) {
	v, err := pyjson.Loads(string(b))
	if err != nil {
		return nil, err
	}
	o, ok := v.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("not an object")
	}
	q := &Request{}
	words, ok := o.Value("argv").([]any)
	if !ok {
		return nil, fmt.Errorf("no argv")
	}
	for _, w := range words {
		s, ok := w.(string)
		if !ok {
			return nil, fmt.Errorf("argv word not a string")
		}
		q.Argv = append(q.Argv, s)
	}
	enc, _ := o.Value("stdin").(string)
	if q.Stdin, err = base64.StdEncoding.DecodeString(enc); err != nil {
		return nil, err
	}
	q.Caller.Sock = strField(o, "sock")
	q.Caller.Pane = strField(o, "pane")
	q.Caller.HerdrPane = strField(o, "herdr_pane")
	q.Caller.DataDir = strField(o, "data_dir")
	if n, ok := o.Value("peer_pid").(pyjson.Int); ok {
		var pid int
		if _, err := fmt.Sscan(n.Text, &pid); err == nil {
			q.Caller.PeerPid = &pid
		}
	}
	return q, nil
}

// RunResident decides a resident call in this process: run_argv inside
// resident_call, with environ as the server's own environment (pane-free).
func RunResident(q *Request, environ []string) Result {
	env := map[string]string{}
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	if c := tsbash.PythonLocale(); c != "" {
		env["LC_CTYPE"] = c
	}
	res, why := runNative(q.Argv, q.Stdin, env, &q.Caller)
	return finish(denyFormatText(q.Argv, q.Argv), res, why)
}

// RunResidentGuarded decides a resident call in a child process of exe,
// started with sub (`gate check` for the daisugi CLI), as RunGuardedAs
// decides a hook call: any abnormal end of the child is the format's deny.
func RunResidentGuarded(exe string, sub []string, q *Request, environ []string) Result {
	env := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		if !strings.HasPrefix(kv, ResidentEnv+"=") && !strings.HasPrefix(kv, ChildEnv+"=") {
			env = append(env, kv)
		}
	}
	env = append(env, ResidentEnv+"=1")
	return runGuarded(exe, sub, denyFormatText(q.Argv, q.Argv), encodeRequest(q), env,
		childDeadlineText(q.Argv))
}

// ---------------------------------------------------------------------------
// A resident call's reports (gate._report_and_append_state's resident
// branch and _state_report.report_state's explicit mode)
// ---------------------------------------------------------------------------

var coppicePaneID = lazyre.New(`^w[0-9]{1,9}:p[0-9]{1,9}$`)

// validCoppicePane is _state_report._valid_coppice_pane_id.
func validCoppicePane(p *string) bool {
	return p != nil && coppicePaneID().MatchString(*p)
}

// coppiceSockTrustworthy is _state_report._coppice_sock_trustworthy: an
// absolute path to a socket this uid owns, never followed through a
// symlink.
func coppiceSockTrustworthy(p *string) bool {
	if p == nil || *p == "" || !isabs(*p) || strings.ContainsRune(*p, 0) {
		return false
	}
	enc, e := pystr.FSEncode(*p)
	if e != nil {
		return false
	}
	var st syscall.Stat_t
	if syscall.Lstat(string(enc), &st) != nil {
		return false
	}
	return st.Mode&syscall.S_IFMT == syscall.S_IFSOCK && int(st.Uid) == os.Getuid()
}

// residentPane is the pane a resident call's event names: the caller's
// claim when it is shaped like a coppice pane id, else none.
func (r *runner) residentPane() any {
	if validCoppicePane(r.caller.Pane) {
		return *r.caller.Pane
	}
	return nil
}

// reportExplicit is report_state with the caller's sock, pane and herdr
// pane: coppice when the socket and the pane id check out, else herdr
// when a herdr pane is named, else nothing. Never the environment.
func (r *runner) reportExplicit(ev *pyjson.Object, deadline time.Time) {
	c := r.caller
	if coppiceSockTrustworthy(c.Sock) && validCoppicePane(c.Pane) {
		reportAsPane(*c.Sock, *c.Pane, c.PeerPid, ev, deadline)
		return
	}
	if c.HerdrPane != nil && *c.HerdrPane != "" {
		r.sendHerdrReport(*c.HerdrPane, ev, deadline)
	}
}

// reportAsPane is _state_report._report_as_pane_via_coppice: hello as
// pane (with the caller's real pid in peer_pids), and the report only
// once coppice's reply places the connection as that pane.
func reportAsPane(sock, pane string, peerPid *int, ev *pyjson.Object, deadline time.Time) bool {
	enc, e := pystr.FSEncode(sock)
	if e != nil {
		return false
	}
	conn, err := net.DialTimeout("unix", string(enc), time.Until(deadline))
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return false
	}
	cred, ok := peerCred(uc)
	if !ok || int(cred.Uid) != os.Getuid() {
		return false
	}
	hello := pyjson.NewObject().Set("id", "h").Set("cmd", "hello").Set("role", "pane").Set("pane", pane)
	if peerPid != nil {
		hello.Set("peer_pids", []any{*peerPid})
	}
	if _, err := conn.Write([]byte(pyjson.Dumps(hello, true) + "\n")); err != nil {
		return false
	}
	reply := readOneReply(conn)
	if reply == nil || reply.Value("ok") != true {
		return false
	}
	res, ok := reply.Value("result").(*pyjson.Object)
	if !ok || res.Value("role") != "pane" || res.Value("pane") != pane {
		return false
	}
	req := pyjson.NewObject().Set("id", "r").Set("cmd", "pane.report_state").Set("pane", pane).Set("event", ev)
	if _, err := conn.Write([]byte(pyjson.Dumps(req, true) + "\n")); err != nil {
		return false
	}
	rep := readOneReply(conn)
	return rep != nil && rep.Value("ok") == true
}

// readOneReply is _state_report._read_one_reply: one line, a JSON object,
// at most 64 KiB; nil for anything else.
func readOneReply(conn net.Conn) *pyjson.Object {
	var buf []byte
	chunk := make([]byte, 65536)
	for len(buf) == 0 || buf[len(buf)-1] != '\n' {
		n, err := conn.Read(chunk)
		if n == 0 {
			return nil
		}
		buf = append(buf, chunk[:n]...)
		if len(buf) > 65536 {
			return nil
		}
		if err != nil && buf[len(buf)-1] != '\n' {
			return nil
		}
	}
	text, ok := requestText(buf)
	if !ok {
		return nil
	}
	v, err := pyjson.Loads(text)
	if err != nil {
		return nil
	}
	o, _ := v.(*pyjson.Object)
	return o
}

// peerCred reads SO_PEERCRED off a connected unix socket.
func peerCred(c *net.UnixConn) (*syscall.Ucred, bool) {
	raw, err := c.SyscallConn()
	if err != nil {
		return nil, false
	}
	var cred *syscall.Ucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		cred, serr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil || serr != nil {
		return nil, false
	}
	return cred, true
}

// PeerPid is the pid SO_PEERCRED names for a connection a server
// accepted, or nil (gate_server._peer_pid).
func PeerPid(c *net.UnixConn) *int {
	cred, ok := peerCred(c)
	if !ok {
		return nil
	}
	pid := int(cred.Pid)
	return &pid
}

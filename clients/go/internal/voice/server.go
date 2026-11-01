package voice

import (
	"bufio"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"html"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"daisugi-verify/internal/llm"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// The server's ceilings, as voice/server.py sets them.
const (
	MaxAudioSeconds      = 1200.0
	MaxContentLength     = 64_000_000
	MaxDeliverBytes      = 65536
	MaxDeliverTextChars  = 20000
	banWindow            = 60.0
	banThreshold         = 3
	readTimeout          = 30 * time.Second
	notFoundMessage      = "This server answers /health, /transcribe, and /deliver."
	errorContentType     = "text/html;charset=utf-8"
	defaultErrorTemplate = `<!DOCTYPE HTML>
<html lang="en">
    <head>
        <meta charset="utf-8">
        <title>Error response</title>
    </head>
    <body>
        <h1>Error response</h1>
        <p>Error code: %d</p>
        <p>Message: %s.</p>
        <p>Error code explanation: %d - %s.</p>
    </body>
</html>
`
)

// ServerConfig is what one voice server runs with.
type ServerConfig struct {
	DataDir   string
	Home      string
	TokenFile string
	ArmedDir  string
	Engine    *Engine
	Cleanup   CleanupConfig
	Floor     FloorConfig
	LLM       *llm.Client
	// Now is the clock; nil is the wall clock.
	Now func() time.Time
}

// Server is VoiceServer: the listener, the config and the ban ledger.
type Server struct {
	cfg      ServerConfig
	ln       net.Listener
	mu       sync.Mutex
	failures map[string][]float64
	// job is the kind of work that holds the engine: "" when it is free,
	// "clip" for a clip turned into text, "partial" for the text so far.
	// Only this server can see a job that outlives the request that began
	// it.
	jobMu sync.Mutex
	job   string
	// seq counts partials for a caller that sends no sequence number.
	seq int64
	// windows is the text of each finished window a partial has read, by
	// the hash of its audio.
	windows map[[32]byte]string
	// order is the windows' keys, oldest first, for dropping the oldest
	// when the cache is full.
	order [][32]byte
}

// RefuseNoToken is build_server's refusal for an address with no token.
func RefuseNoToken(address, tokenFile string) error {
	return &startError{3, fmt.Sprintf("refusing to listen on %s without a token. %s is missing "+
		"or unreadable. Run coppice web token first, or pass --token-file.", address, tokenFile)}
}

// startError is a failure before the server serves, with the exit code
// the command line gives it.
type startError struct {
	code int
	msg  string
}

func (e *startError) Error() string { return e.msg }

// ExitCode is the exit code of a start failure: 3 for a RuntimeError in
// the oracle, 1 for an OSError or ValueError.
func ExitCode(err error) int {
	var se *startError
	if errors.As(err, &se) {
		return se.code
	}
	return 1
}

// Listen is build_server: the token checked, the socket bound, the TLS
// pair checked, the token checked again. certFile and keyFile are "" when
// not given.
func Listen(host string, port int, cfg ServerConfig, certFile, keyFile *string) (*Server, error) {
	if _, ok, err := ReadToken(cfg.TokenFile); err != nil {
		return nil, &startError{1, err.Error()}
	} else if !ok {
		return nil, RefuseNoToken(host, cfg.TokenFile)
	}
	ln, err := net.Listen("tcp4", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, &startError{1, oserrorText(err)}
	}
	if certFile != nil || keyFile != nil {
		if certFile == nil || keyFile == nil {
			ln.Close()
			return nil, &startError{1, "pass both --tls-cert and --tls-key, or pass neither"}
		}
		pair, err := tls.LoadX509KeyPair(*certFile, *keyFile)
		if err != nil {
			ln.Close()
			return nil, &startError{1, err.Error()}
		}
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{pair}})
	}
	bound := ln.Addr().(*net.TCPAddr).IP.String()
	if _, ok, err := ReadToken(cfg.TokenFile); err != nil || !ok {
		ln.Close()
		if err != nil {
			return nil, &startError{1, err.Error()}
		}
		return nil, RefuseNoToken(bound, cfg.TokenFile)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Server{cfg: cfg, ln: ln, failures: map[string][]float64{}, windows: map[[32]byte]string{}}, nil
}

// oserrorText is str(OSError) for a failed bind: "[Errno N] text".
func oserrorText(err error) string {
	var se *net.OpError
	if errors.As(err, &se) {
		var errno interface{ Error() string } = se.Err
		if sc, ok := se.Err.(interface{ Unwrap() error }); ok && sc.Unwrap() != nil {
			errno = sc.Unwrap()
		}
		switch errno.Error() {
		case "address already in use":
			return "[Errno 98] Address already in use"
		case "permission denied":
			return "[Errno 13] Permission denied"
		case "cannot assign requested address":
			return "[Errno 99] Cannot assign requested address"
		}
	}
	return err.Error()
}

// Serve answers connections until the listener closes.
func (s *Server) Serve() error {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			continue
		}
		go s.handle(conn)
	}
}

// Close stops the listener.
func (s *Server) Close() error { return s.ln.Close() }

func (s *Server) now() float64 { return float64(s.cfg.Now().UnixNano()) / 1e9 }

func (s *Server) prune(now float64) {
	for addr, hits := range s.failures {
		var keep []float64
		for _, t := range hits {
			if now-t < banWindow {
				keep = append(keep, t)
			}
		}
		if len(keep) > 0 {
			s.failures[addr] = keep
		} else {
			delete(s.failures, addr)
		}
	}
}

func (s *Server) recordFailure(addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.prune(now)
	s.failures[addr] = append(s.failures[addr], now)
}

func (s *Server) banned(addr string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(s.now())
	return len(s.failures[addr]) >= banThreshold
}

// deadlineConn resets the read deadline before each read, as a socket
// timeout does.
type deadlineConn struct{ net.Conn }

func (c deadlineConn) Read(p []byte) (int, error) {
	c.Conn.SetReadDeadline(time.Now().Add(readTimeout))
	return c.Conn.Read(p)
}

// request is one parsed request, as BaseHTTPRequestHandler holds it.
type request struct {
	method  string
	path    string
	names   []string
	values  []string
	body    *bufio.Reader
	address string
}

// header is self.headers.get(name): the first value, case-insensitive.
func (r *request) header(name string) (string, bool) {
	ln := strings.ToLower(name)
	for i, n := range r.names {
		if strings.ToLower(n) == ln {
			return r.values[i], true
		}
	}
	return "", false
}

type responder struct {
	w      io.Writer
	method string
}

func (rs *responder) send(status int, ctype string, body []byte) {
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP/1.0 %d %s\r\n", status, http.StatusText(status))
	b.WriteString("Server: opendaisugi-voice/1\r\n")
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().UTC().Format(http.TimeFormat))
	if ctype != "" {
		fmt.Fprintf(&b, "Content-Type: %s\r\n", ctype)
		fmt.Fprintf(&b, "Content-Length: %d\r\n", len(body))
	}
	b.WriteString("\r\n")
	io.WriteString(rs.w, b.String())
	if rs.method != "HEAD" {
		rs.w.Write(body)
	}
}

func (rs *responder) json(status int, payload *pyjson.Object) {
	rs.send(status, "application/json", []byte(pyjson.Dumps(payload, true)))
}

var explains = map[int]string{
	400: "Bad request syntax or unsupported method",
	414: "Requested URI is too long",
	431: "The server refused this request because the request header fields are too large",
	501: "Server does not support this operation",
	505: "Cannot fulfill request",
}

func (rs *responder) sendError(status int, message string) {
	body := fmt.Sprintf(defaultErrorTemplate, status, html.EscapeString(message), status, explains[status])
	body = strings.ReplaceAll(body, "&#39;", "'")
	body = strings.ReplaceAll(body, "&#34;", `"`)
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP/1.0 %d %s\r\n", status, message)
	b.WriteString("Server: opendaisugi-voice/1\r\n")
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().UTC().Format(http.TimeFormat))
	b.WriteString("Connection: close\r\n")
	fmt.Fprintf(&b, "Content-Type: %s\r\n", errorContentType)
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n", len(body))
	io.WriteString(rs.w, b.String())
	if rs.method != "HEAD" {
		io.WriteString(rs.w, body)
	}
}

func obj(kv ...any) *pyjson.Object {
	o := pyjson.NewObject()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

func errBody(code, message string) *pyjson.Object { return obj("error", code, "message", message) }

// readLine is rfile.readline(limit): up to and including a newline, or
// limit bytes.
func readLine(r *bufio.Reader, limit int) ([]byte, error) {
	var out []byte
	for len(out) < limit {
		c, err := r.ReadByte()
		if err != nil {
			return out, err
		}
		out = append(out, c)
		if c == '\n' {
			break
		}
	}
	return out, nil
}

// errDrop is a request the oracle's handler raised on before it answered:
// the connection closes with no reply.
var errDrop = errors.New("dropped")

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	r := bufio.NewReaderSize(deadlineConn{c}, 65536)
	rs := &responder{w: c}
	raw, _ := readLine(r, 65537)
	if len(raw) > 65536 {
		rs.sendError(414, "Request-URI Too Long")
		return
	}
	if len(raw) == 0 {
		return
	}
	line := strings.TrimRight(Latin1(raw), "\r\n")
	words := pystr.Split(line)
	if len(words) == 0 {
		return
	}
	if len(words) >= 3 {
		version := words[len(words)-1]
		major, minor, ok := httpVersion(version)
		if !ok {
			rs.sendError(400, "Bad request version ("+pystr.Repr(version)+")")
			return
		}
		if major >= 2 {
			rs.sendError(505, "Invalid HTTP version ("+strings.SplitN(version, "/", 2)[1]+")")
			return
		}
		_ = minor
	}
	if len(words) < 2 || len(words) > 3 {
		rs.sendError(400, "Bad request syntax ("+pystr.Repr(line)+")")
		return
	}
	method, path := words[0], words[1]
	rs.method = method
	if len(words) == 2 && method != "GET" {
		rs.sendError(400, "Bad HTTP/0.9 request type ("+pystr.Repr(method)+")")
		return
	}
	if strings.HasPrefix(path, "//") {
		path = "/" + strings.TrimLeft(path, "/")
	}
	req := &request{method: method, path: path, body: r}
	if host, _, err := net.SplitHostPort(c.RemoteAddr().String()); err == nil {
		req.address = host
	}
	if code, msg := readHeaders(r, req); code != 0 {
		rs.sendError(code, msg)
		return
	}
	switch method {
	case "GET":
		s.doGet(req, rs)
	case "POST":
		s.doPost(req, rs)
	default:
		rs.sendError(501, "Unsupported method ("+pystr.Repr(method)+")")
	}
}

func httpVersion(v string) (int, int, bool) {
	if !strings.HasPrefix(v, "HTTP/") {
		return 0, 0, false
	}
	parts := strings.Split(v[5:], ".")
	if len(parts) != 2 {
		return 0, 0, false
	}
	var nums [2]int
	for i, p := range parts {
		if p == "" || len(pystr.Runes(p)) > 10 {
			return 0, 0, false
		}
		n := PyInt(p)
		if n == nil || strings.ContainsAny(p, " +-_\t") {
			return 0, 0, false
		}
		nums[i] = int(n.Int64())
	}
	return nums[0], nums[1], true
}

// readHeaders is http.client.parse_headers then the email parser: at most
// 100 lines of at most 65536 bytes, a header ends at the first line that
// is not one, and a line that starts with a space or tab continues the
// last header.
func readHeaders(r *bufio.Reader, req *request) (int, string) {
	var lines []string
	for {
		raw, _ := readLine(r, 65537)
		if len(raw) > 65536 {
			return 431, "Line too long"
		}
		lines = append(lines, Latin1(raw))
		if len(lines) > 100 {
			return 431, "Too many headers"
		}
		if string(raw) == "\r\n" || string(raw) == "\n" || len(raw) == 0 {
			break
		}
	}
	for _, ln := range lines {
		if ln == "\r\n" || ln == "\n" || ln == "" {
			break
		}
		if (ln[0] == ' ' || ln[0] == '\t') && len(req.names) > 0 {
			req.values[len(req.values)-1] += strings.TrimRight(ln, "\r\n")
			continue
		}
		i := strings.IndexByte(ln, ':')
		if i <= 0 || !headerName(ln[:i]) {
			break
		}
		req.names = append(req.names, ln[:i])
		req.values = append(req.values, strings.TrimRight(strings.TrimLeft(ln[i+1:], " \t"), "\r\n"))
	}
	return 0, ""
}

func headerName(n string) bool {
	for i := 0; i < len(n); i++ {
		if n[i] < 0x21 || n[i] > 0x7e || n[i] == ':' {
			return false
		}
	}
	return true
}

func urlPath(raw string) (path, query string) {
	// urlsplit: the fragment goes first, then the query.
	if i := strings.IndexByte(raw, '#'); i >= 0 {
		raw = raw[:i]
	}
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		raw, query = raw[:i], raw[i+1:]
	}
	if strings.HasPrefix(raw, "//") {
		// A netloc: the path starts at the next slash.
		rest := raw[2:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			return rest[j:], query
		}
		return "", query
	}
	if i := strings.IndexByte(raw, ':'); i > 0 && schemeLike(raw[:i]) {
		raw = raw[i+1:]
		if strings.HasPrefix(raw, "//") {
			rest := raw[2:]
			if j := strings.IndexByte(rest, '/'); j >= 0 {
				return rest[j:], query
			}
			return "", query
		}
	}
	return raw, query
}

func schemeLike(s string) bool {
	if s == "" || !(s[0] >= 'a' && s[0] <= 'z' || s[0] >= 'A' && s[0] <= 'Z') {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

// firstQueryValue is parse_qs(query).get(name, [""])[0]: the pairs split
// on "&", blank values dropped, "+" a space, and percent escapes decoded
// as UTF-8 with replacement.
func firstQueryValue(query, name string) (string, bool) {
	for _, part := range strings.Split(query, "&") {
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok || v == "" {
			continue
		}
		if unquotePlus(k) == name {
			return unquotePlus(v), true
		}
	}
	return "", false
}

func unquotePlus(s string) string {
	s = strings.ReplaceAll(s, "+", " ")
	var out []byte
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			n, _ := strconv.ParseUint(s[i+1:i+3], 16, 8)
			out = append(out, byte(n))
			i += 2
			continue
		}
		out = append(out, s[i])
	}
	return pystr.DecodeReplace(out)
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func (s *Server) doGet(req *request, rs *responder) {
	p, _ := urlPath(req.path)
	if p == "/health" {
		rs.json(200, obj("ok", true, "max_seconds", MaxAudioSeconds))
		return
	}
	if p == "/status" {
		s.status(req, rs)
		return
	}
	rs.json(404, errBody("not_found", notFoundMessage))
}

// status is GET /status: what this server does. Like /health it needs no
// token: it names only the engine and whether /partial is there, and a
// probe must never count toward the ban that guards the token. partial is
// true because this server answers /partial, and partial_engine names the
// engine that reads it.
func (s *Server) status(req *request, rs *responder) {
	name := s.cfg.Engine.Name
	rs.json(200, obj("ok", true, "max_seconds", MaxAudioSeconds, "engine", name,
		"partial", true, "partial_engine", name))
}

// authorized is _authorized. errDrop when the oracle's compare_digest
// raises: a supplied or stored token that is not ASCII, or a token file
// that is not UTF-8.
func (s *Server) authorized(req *request) (bool, error) {
	token, ok, err := ReadToken(s.cfg.TokenFile)
	if err != nil {
		return false, errDrop
	}
	h, _ := req.header("Authorization")
	supplied := ""
	if strings.HasPrefix(h, "Bearer ") {
		supplied = pystr.Strip(h[len("Bearer "):])
	}
	if ok {
		if !isASCII(supplied) || !isASCII(token) {
			return false, errDrop
		}
		if subtle.ConstantTimeCompare([]byte(supplied), []byte(token)) == 1 {
			return true, nil
		}
	}
	s.recordFailure(req.address)
	return false, nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func (s *Server) doPost(req *request, rs *responder) {
	p, query := urlPath(req.path)
	if _, te := req.header("Transfer-Encoding"); te {
		if _, cl := req.header("Content-Length"); !cl {
			rs.json(411, errBody("chunked_not_supported", "Send a Content-Length header. Chunked transfer encoding is not read."))
			return
		}
	}
	if s.banned(req.address) {
		rs.json(429, errBody("too_many_failed_tokens", "Too many failed tokens from this address. Wait 60 seconds and try again."))
		return
	}
	ok, err := s.authorized(req)
	if err != nil {
		return
	}
	if !ok {
		rs.json(401, errBody("unauthorized", "This request needs a bearer token. Run coppice web token on the box that runs this server."))
		return
	}
	if p != "/transcribe" && p != "/deliver" && p != "/partial" {
		rs.json(404, errBody("not_found", notFoundMessage))
		return
	}
	if _, has := req.header("Origin"); has {
		rs.json(403, errBody("cross_origin_refused", "This server does not accept requests carrying an Origin header. Call it directly, not from a browser page."))
		return
	}
	if site, has := req.header("Sec-Fetch-Site"); has && site != "same-origin" && site != "none" {
		rs.json(403, errBody("cross_origin_refused", "This server does not accept cross-site requests. Call it directly, not from a browser page."))
		return
	}
	var herr error
	if p == "/transcribe" {
		v, _ := firstQueryValue(query, "cleanup")
		herr = s.transcribe(req, rs, v == "1")
	} else if p == "/partial" {
		herr = s.partial(req, rs, query)
	} else {
		herr = s.deliver(req, rs)
	}
	if herr != nil {
		rs.json(500, errBody("internal_error", "Something went wrong handling this request. Check the server log, then try again."))
	}
}

// contentLength is _content_length: nil when it is not a number or is
// negative; a missing header is 0.
func contentLength(req *request) *big.Int {
	raw, ok := req.header("Content-Length")
	if !ok {
		raw = "0"
	}
	n := PyInt(raw)
	if n == nil || n.Sign() < 0 {
		return nil
	}
	return n
}

func readBody(req *request, n int64) []byte {
	buf := make([]byte, n)
	got, _ := io.ReadFull(req.body, buf)
	return buf[:got]
}

// clipWav reads a request's clip as a 16 kHz mono WAV, cut to the cap. ok
// is false when a reply was already sent.
func clipWav(req *request, rs *responder) (wav []byte, cutS int64, ok bool, err error) {
	n := contentLength(req)
	if n == nil {
		rs.json(400, errBody("bad_content_length", "Send a valid Content-Length header with the request."))
		return nil, 0, false, nil
	}
	if n.Cmp(big.NewInt(MaxContentLength)) > 0 {
		rs.json(413, errBody("payload_too_large", fmt.Sprintf("The request body must be %d bytes or smaller.", MaxContentLength)))
		return nil, 0, false, nil
	}
	body := readBody(req, n.Int64())
	ctype, has := req.header("Content-Type")
	if !has {
		ctype = "application/octet-stream"
	}
	audio := body
	if strings.HasPrefix(ctype, "multipart/form-data") {
		a, _, err := ExtractMultipartAudio(body, ctype)
		if err != nil {
			rs.json(400, errBody("bad_audio", "This audio could not be decoded. Send WAV, WebM, Ogg, MP3, or M4A."))
			return nil, 0, false, nil
		}
		audio = a
	}
	wav, aerr := ToWav16kMono(audio)
	if aerr != nil {
		if aerr.BadAudio() {
			rs.json(400, errBody("bad_audio", "This audio could not be decoded. Send WAV, WebM, Ogg, MP3, or M4A."))
			return nil, 0, false, nil
		}
		return nil, 0, false, aerr
	}
	// A clip a little over the cap is read up to the cap, and the reply says
	// how much was left out. The page stops a few seconds early, but a clip
	// it recorded must never be refused for a few seconds.
	wav, cutS, terr := TruncateWav(wav, MaxAudioSeconds)
	if terr != nil {
		return nil, 0, false, terr
	}
	return wav, cutS, true, nil
}

// busyMessage is the 409 a clip or a partial meets while another clip is
// turned into text.
const busyMessage = "The voice server is still turning an earlier clip into text. Wait for it to finish, then send this one again."

// partialWait bounds how long a clip waits for a partial that holds the
// engine. A partial reads at most one new window, so it ends well inside.
var partialWait = 30 * time.Second

// take claims the engine for kind. A clip that finds a partial running
// waits for it, up to partialWait: the owner taps Stop while a partial
// runs, and the clip must never be refused for that. A partial never
// waits; it is dropped, and the caller sends a newer one.
func (s *Server) take(kind string) bool {
	deadline := time.Now().Add(partialWait)
	for {
		s.jobMu.Lock()
		cur := s.job
		if cur == "" {
			s.job = kind
		}
		s.jobMu.Unlock()
		if cur == "" {
			return true
		}
		if kind != "clip" || cur != "partial" || time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *Server) release() {
	s.jobMu.Lock()
	s.job = ""
	s.jobMu.Unlock()
}

// engineReply sends the 503 for an engine that is loading or missing, and
// answers false for any other error.
func engineReply(rs *responder, err error) bool {
	var ee *EngineError
	if !errors.As(err, &ee) {
		return false
	}
	code := "engine_unavailable"
	if ee.Kind == "loading" {
		code = "engine_loading"
	}
	rs.json(503, errBody(code, ee.Msg))
	return true
}

func (s *Server) transcribe(req *request, rs *responder, wantCleanup bool) error {
	wav, cutS, ok, err := clipWav(req, rs)
	if !ok {
		return err
	}
	if !s.take("clip") {
		rs.json(409, errBody("busy", busyMessage))
		return nil
	}
	defer s.release()
	t, err := s.cfg.Engine.TranscribeLong(wav, nil)
	if err != nil {
		if engineReply(rs, err) {
			return nil
		}
		return err
	}
	text := t.Text
	cleaned := false
	var reason *string
	if wantCleanup {
		tr := HTTPTransport{Client: s.cfg.LLM, BaseURL: s.cfg.Cleanup.BaseURL, Timeout: 30.0}
		res, err := CleanParts(t, s.cfg.Cleanup, tr, s.cfg.Now())
		if err != nil {
			return err
		}
		text, cleaned, reason = res.Text, res.Cleaned, res.Reason
	}
	o := obj("text", text, "raw_text", t.Text, "duration_s", t.DurationS, "rtf", t.RTF,
		"engine", s.cfg.Engine.Name, "cleaned", cleaned)
	if reason != nil {
		o.Set("reason", *reason)
	}
	if t.Gaps > 0 {
		o.Set("gaps", t.Gaps)
	}
	if cutS > 0 {
		o.Set("cut_s", int(cutS))
	}
	rs.json(200, o)
	return nil
}

// maxWindowCache bounds the window texts a server keeps for partials. A
// 20 minute clip has 40 windows, so this holds several dictations. When
// it is full the oldest window goes.
var maxWindowCache = 256

// partial is POST /partial: the text of the clip so far, for the page to
// show while the owner still talks. The reply carries the caller's seq
// (a query value), or the server's own count when there is none, so the
// caller can drop a reply that comes after a newer one. A partial never
// cleans up, never delivers and never arms. It never waits for the
// engine: while another job runs it is dropped with 409, and the caller
// sends a newer one.
func (s *Server) partial(req *request, rs *responder, query string) error {
	wav, _, ok, err := clipWav(req, rs)
	if !ok {
		return err
	}
	seq := int64(0)
	if v, has := firstQueryValue(query, "seq"); has {
		if n, perr := strconv.ParseInt(v, 10, 64); perr == nil && n >= 0 {
			seq = n
		}
	}
	s.jobMu.Lock()
	s.seq++
	if seq == 0 {
		seq = s.seq
	}
	s.jobMu.Unlock()
	if !s.take("partial") {
		rs.json(409, errBody("busy", busyMessage))
		return nil
	}
	defer s.release()
	start := time.Now()
	text, dur, err := s.partialText(wav)
	if err != nil {
		if engineReply(rs, err) {
			return nil
		}
		return err
	}
	rs.json(200, obj("text", text, "seq", int(seq), "duration_s", dur, "engine", s.cfg.Engine.Name,
		"ms", int(time.Since(start).Milliseconds())))
	return nil
}

// partialText reads a clip window by window, as TranscribeLong cuts it.
// A window that is not the last one is finished: its cut depends only on
// the audio up to its own end, so the same audio comes again in the next
// partial. Its text is kept by the hash of its audio and not read again.
// Only the window still growing is read every time.
func (s *Server) partialText(wav []byte) (string, float64, error) {
	dur, aerr := WavDurationS(wav)
	if aerr != nil {
		return "", 0, aerr
	}
	pieces, aerr := SplitWav(wav)
	if aerr != nil {
		return "", 0, aerr
	}
	parts := make([]string, len(pieces))
	for i, p := range pieces {
		last := i == len(pieces)-1
		key := sha256.Sum256(p)
		if !last {
			s.jobMu.Lock()
			t, hit := s.windows[key]
			s.jobMu.Unlock()
			if hit {
				parts[i] = t
				continue
			}
		}
		t, err := s.cfg.Engine.Transcribe(p, nil)
		if err != nil {
			var ee *EngineError
			if errors.As(err, &ee) || len(pieces) == 1 {
				return "", 0, err
			}
			parts[i] = GapMarker
			continue
		}
		parts[i] = t.Text
		if !last {
			s.jobMu.Lock()
			if _, had := s.windows[key]; !had {
				// Full: drop the oldest window. The clip being dictated
				// holds the newest ones, so its windows stay.
				for len(s.order) >= maxWindowCache {
					delete(s.windows, s.order[0])
					s.order = s.order[1:]
				}
				s.order = append(s.order, key)
			}
			s.windows[key] = t.Text
			s.jobMu.Unlock()
		}
	}
	return JoinTexts(parts), dur, nil
}

func (s *Server) deliver(req *request, rs *responder) error {
	ctype, _ := req.header("Content-Type")
	if !strings.HasPrefix(ctype, "application/json") {
		rs.json(400, errBody("bad_content_type", "Send Content-Type: application/json."))
		return nil
	}
	n := contentLength(req)
	if n == nil {
		rs.json(400, errBody("bad_content_length", "Send a valid Content-Length header with the request."))
		return nil
	}
	if n.Cmp(big.NewInt(MaxDeliverBytes)) > 0 {
		rs.json(413, errBody("payload_too_large", fmt.Sprintf("The request body must be %d bytes or smaller.", MaxDeliverBytes)))
		return nil
	}
	body := readBody(req, n.Int64())
	if len(body) == 0 {
		body = []byte("{}")
	}
	payload, perr := loadsRequest(body)
	if perr == errBadJSON {
		rs.json(400, errBody("bad_json", "Send a JSON object with pane, text, and mode."))
		return nil
	}
	if perr != nil {
		return perr
	}
	po, ok := payload.(*pyjson.Object)
	if !ok {
		return errors.New("the payload has no get")
	}
	pane, _ := po.Get("pane")
	text, has := po.Get("text")
	if !has {
		text = ""
	}
	mode, has := po.Get("mode")
	if !has {
		mode = "preview"
	}
	backend, _ := po.Get("backend")
	paneID, ok := pane.(string)
	if !ok || paneID == "" {
		rs.json(400, errBody("bad_request", "pane must be a non-empty string."))
		return nil
	}
	textS, ok := text.(string)
	if !ok || textS == "" {
		rs.json(400, errBody("bad_request", "text must be a non-empty string."))
		return nil
	}
	if pystr.Len(textS) > MaxDeliverTextChars {
		rs.json(413, errBody("text_too_long", fmt.Sprintf("text must be %d characters or fewer.", MaxDeliverTextChars)))
		return nil
	}
	if backend != nil {
		if _, ok := backend.(string); !ok {
			rs.json(400, errBody("bad_request", "backend must be a string."))
			return nil
		}
	}
	modeS, ok := mode.(string)
	if !ok || (modeS != "preview" && modeS != "send") {
		rs.json(400, errBody("bad_request", "mode must be preview or send."))
		return nil
	}
	res, err := Deliver(paneID, textS, modeS, s.cfg.ArmedDir, nil, s.now(), func(text string) error {
		c, err := pickCoppice(s.cfg.Floor, s.cfg.Home)
		if err != nil {
			return err
		}
		_, err = c.promptPane(paneID, text)
		return err
	})
	var fna *FloorNotAvailable
	if errors.As(err, &fna) {
		rs.json(503, errBody("no_pane_backend", fna.Msg))
		return nil
	}
	if err != nil {
		return err
	}
	status := 200
	if res.Delivered == "refused" {
		status = 403
	}
	var reason any
	if res.Reason != nil {
		reason = *res.Reason
	}
	rs.json(status, obj("delivered", res.Delivered, "reason", reason))
	return nil
}

var errBadJSON = errors.New("bad json")

// loadsRequest is json.loads(bytes): a body that does not decode is a
// 500 in the oracle (UnicodeDecodeError), a body that does not parse is a
// 400.
func loadsRequest(raw []byte) (any, error) {
	text, err := decodeJSONBytes(raw)
	if err != nil {
		return nil, err
	}
	v, derr := pyjson.LoadsPy(text, 900)
	if derr != nil {
		return nil, errBadJSON
	}
	return v, nil
}

// decodeJSONBytes is json.loads's detect_encoding then decode, for UTF-8
// and UTF-8 with a BOM. A UTF-16 or UTF-32 body is an error here (a 500),
// where the oracle would read it (ruling VO-6).
func decodeJSONBytes(raw []byte) (string, error) {
	enc := "utf-8"
	switch {
	case len(raw) >= 4 && (string(raw[:4]) == "\x00\x00\xfe\xff" || string(raw[:4]) == "\xff\xfe\x00\x00"):
		enc = "utf-32"
	case len(raw) >= 2 && (string(raw[:2]) == "\xfe\xff" || string(raw[:2]) == "\xff\xfe"):
		enc = "utf-16"
	case len(raw) >= 3 && string(raw[:3]) == "\xef\xbb\xbf":
		raw = raw[3:]
	case len(raw) >= 4 && (raw[0] == 0 || raw[1] == 0):
		enc = "utf-16 or utf-32"
	case len(raw) == 2 && (raw[0] == 0 || raw[1] == 0):
		enc = "utf-16"
	}
	if enc != "utf-8" {
		return "", errors.New("a JSON body in " + enc + " is not read by this binary")
	}
	text, xerr := pystr.DecodeStrict(raw)
	if xerr != nil {
		return "", errors.New(xerr.Msg)
	}
	return text, nil
}

// ParseListen is the --listen check: host:port with digits after the
// first colon.
func ParseListen(listen string) (string, int, bool) {
	host, port, sep := strings.Cut(listen, ":")
	if !sep || port == "" {
		return "", 0, false
	}
	for _, r := range port {
		if !unicode.IsDigit(r) {
			return "", 0, false
		}
	}
	n := PyInt(port)
	if n == nil || !n.IsInt64() {
		return "", 0, false
	}
	return host, int(n.Int64()), true
}

// CheckServerURL is voice ptt's --server check: urlsplit gives the
// scheme http or https and a hostname.
func CheckServerURL(raw string) bool {
	raw = strings.TrimLeft(raw, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\x0b\x0c\r\x0e\x0f"+
		"\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f ")
	raw = strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(raw)
	scheme := ""
	if i := strings.IndexByte(raw, ':'); i > 0 && schemeLike(raw[:i]) {
		scheme, raw = strings.ToLower(raw[:i]), raw[i+1:]
	}
	if scheme != "http" && scheme != "https" {
		return false
	}
	if !strings.HasPrefix(raw, "//") {
		return false
	}
	netloc := raw[2:]
	if j := strings.IndexAny(netloc, "/?#"); j >= 0 {
		netloc = netloc[:j]
	}
	if strings.Contains(netloc, "[") != strings.Contains(netloc, "]") {
		return false
	}
	if k := strings.LastIndexByte(netloc, '@'); k >= 0 {
		netloc = netloc[k+1:]
	}
	host := netloc
	if strings.HasPrefix(netloc, "[") {
		if k := strings.IndexByte(netloc, ']'); k >= 0 {
			host = netloc[1:k]
		}
	} else if k := strings.IndexByte(netloc, ':'); k >= 0 {
		host = netloc[:k]
	}
	return host != ""
}

// IsLoopback is server._is_loopback: the name localhost, or an address
// ipaddress reads as loopback (an IPv4-mapped one included).
func IsLoopback(address string) bool {
	if address == "localhost" {
		return true
	}
	a, err := netip.ParseAddr(address)
	if err != nil {
		return false
	}
	return a.Unmap().IsLoopback()
}

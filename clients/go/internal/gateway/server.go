package gateway

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"daisugi-verify/internal/netproxy"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// This file is gateway_asgi.make_gateway_app: the proxy itself.

// SelectedModelHeader is the header Switchyard names the served target in.
const SelectedModelHeader = "x-model-router-selected-model"

const countTokensPath = "/v1/messages/count_tokens"

// Hop-by-hop and framing headers, never forwarded.
var dropRequest = map[string]bool{"host": true, "content-length": true, "connection": true, "keep-alive": true,
	"transfer-encoding": true, "upgrade": true, "te": true, "trailers": true, "proxy-connection": true}

var dropResponse = map[string]bool{"content-length": true, "connection": true, "keep-alive": true,
	"transfer-encoding": true, "upgrade": true, "te": true, "trailers": true, "proxy-connection": true,
	"content-encoding": true}

// Server is the gateway app.
type Server struct {
	// Anthropic serves the Anthropic wire when the reloader holds
	// nothing; Reloader, when set, is asked first on every such request.
	Anthropic *Gateway
	Reloader  *Reloader
	// OpenAI routes the OpenAI wire; nil passes that wire through
	// unrouted and unmetered.
	OpenAI       *Gateway
	UpstreamBase string
	OpenAIBase   string
	UpstreamKind string
	Client       *http.Client
	Now          func() time.Time

	mu sync.Mutex // prepare and finish run one at a time, as on the oracle's event loop
}

// NewClient is the upstream client: no timeout (agent turns run long),
// no redirects followed, no compression added or removed behind the
// proxy's back, proxies from environ as httpx reads them.
func NewClient(environ []string) *http.Client {
	return NewClientWith(netproxy.HttpxFromVars(netproxy.FromEnviron(environ), true))
}

// NewClientWith is NewClient with the proxy rules already read.
func NewClientWith(rules *netproxy.Httpx) *http.Client {
	tr := netproxy.Transport(rules, &http.Transport{
		DisableCompression: true,
		DialContext:        (&net.Dialer{}).DialContext,
		MaxIdleConns:       64,
		IdleConnTimeout:    90 * time.Second,
	})
	return &http.Client{Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// IsOpenAIWire is is_openai_wire.
func IsOpenAIWire(path string) bool {
	return strings.HasSuffix(strings.TrimRight(path, "/"), "/chat/completions")
}

func sendJSON(w http.ResponseWriter, status int, payload *pyjson.Object) {
	w.Header()["Content-Type"] = []string{"application/json"}
	w.WriteHeader(status)
	io.WriteString(w, pyjson.Dumps(payload, true))
}

func errObj(msg string) *pyjson.Object { return pyjson.NewObject().Set("error", msg) }

// internalError is uvicorn's answer when the app raised before the
// response started.
func internalError(w http.ResponseWriter) {
	h := w.Header()
	for k := range h {
		delete(h, k)
	}
	h["Content-Type"] = []string{"text/plain; charset=utf-8"}
	h["Connection"] = []string{"close"}
	w.WriteHeader(http.StatusInternalServerError)
	io.WriteString(w, "Internal Server Error")
}

func isLoopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// ServeHTTP is the ASGI app.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/_reload" {
		switch {
		case s.Reloader == nil:
			sendJSON(w, 404, errObj("no reloader configured"))
		case strings.ToUpper(r.Method) != "POST":
			sendJSON(w, 405, errObj("reload accepts POST"))
		case !isLoopback(r.RemoteAddr):
			sendJSON(w, 403, errObj("reload is loopback only"))
		default:
			sendJSON(w, 200, pyjson.NewObject().Set("reloaded", s.Reloader.Reload()))
		}
		return
	}
	openai := IsOpenAIWire(path)
	var gw *Gateway
	if openai {
		gw = s.OpenAI
	} else {
		gw = s.Anthropic
		if s.Reloader != nil {
			if cur := s.Reloader.Current(); cur != nil {
				gw = cur
			}
		}
	}
	body, _ := io.ReadAll(r.Body)
	countTokens := strings.TrimRight(path, "/") == countTokensPath
	if !openai && s.UpstreamKind != "anthropic" && countTokens {
		s.countTokensLocally(w, body)
		return
	}
	outbound, prepared, stream := body, (*Prepared)(nil), false
	if gw != nil {
		s.mu.Lock()
		outbound, prepared, stream = PrepareBytes(gw, body)
		s.mu.Unlock()
	} else if v, err := LoadsBytes(body); err == nil {
		if st, err := get(v, "stream", nil); err == nil {
			stream = st == true
		}
	}
	record := prepared
	if gw != nil && gw.RouterMode != ModeRules && countTokens {
		record = nil
	}
	base := s.UpstreamBase
	if openai {
		base = s.OpenAIBase
	}
	url := strings.TrimRight(base, "/") + path
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}
	type attempt struct {
		body     []byte
		original bool
	}
	attempts := []attempt{{outbound, false}}
	if prepared != nil && prepared.Decision.Downgraded {
		attempts = append(attempts, attempt{body, true})
	}
	headers := forwardHeaders(r)
	for i, a := range attempts {
		last := i == len(attempts)-1
		resp, err := s.post(url, headers, a.body)
		if err != nil {
			internalError(w)
			return
		}
		if resp.StatusCode >= 400 && !last {
			resp.Body.Close()
			continue
		}
		if stream {
			s.relayStream(w, resp, gw, record, a.original, openai)
		} else {
			s.relayBuffered(w, resp, gw, record, a.original, openai)
		}
		return
	}
}

// PrepareBytes is _prepare: the bytes to send, the prepared turn, and
// whether the client asked for a stream. Any raise forwards the original
// bytes untouched.
func PrepareBytes(gw *Gateway, body []byte) ([]byte, *Prepared, bool) {
	v, err := LoadsBytes(body)
	if err != nil {
		return body, nil, false
	}
	p, err := gw.Prepare(v)
	if err != nil {
		return body, nil, false
	}
	stream := p.Outbound != nil && getOr(v.(*pyjson.Object), "stream", nil) == true
	return []byte(pyjson.Dumps(p.Outbound, true)), p, stream
}

func forwardHeaders(r *http.Request) http.Header {
	h := http.Header{}
	for k, vs := range r.Header {
		if dropRequest[strings.ToLower(k)] {
			continue
		}
		h[k] = append([]string(nil), vs...)
	}
	// httpx's own defaults, where the client sent none.
	if _, has := h["Accept"]; !has {
		h["Accept"] = []string{"*/*"}
	}
	if _, has := h["Accept-Encoding"]; !has {
		h["Accept-Encoding"] = []string{"gzip, deflate"}
	}
	return h
}

// post sends one attempt upstream. The request is never tied to the
// client's connection: a client that goes away does not stop the turn,
// which is still read and journaled.
func (s *Server) post(url string, headers http.Header, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = headers.Clone()
	if _, has := req.Header["User-Agent"]; !has {
		req.Header["User-Agent"] = []string{"opendaisugi-gateway"}
	}
	req.ContentLength = int64(len(body))
	return s.Client.Do(req)
}

// responseHeaders is the upstream's headers less the hop-by-hop ones,
// each name once with its values joined by ", ", as httpx lists them.
func responseHeaders(w http.ResponseWriter, resp *http.Response) {
	h := w.Header()
	for k := range h {
		delete(h, k)
	}
	for k, vs := range resp.Header {
		lk := strings.ToLower(k)
		if dropResponse[lk] {
			continue
		}
		h[http.CanonicalHeaderKey(lk)] = []string{strings.Join(vs, ", ")}
	}
	if _, has := h["Content-Type"]; !has {
		// No sniffed type: the client gets the upstream's headers only.
		h["Content-Type"] = nil
	}
}

// decodedBody wraps the upstream body in the decoders httpx applies:
// gzip and deflate, in the reverse of their listed order; any other
// encoding is passed through as it came.
func decodedBody(resp *http.Response) io.Reader {
	var encs []string
	for _, v := range resp.Header.Values("Content-Encoding") {
		for _, e := range strings.Split(v, ",") {
			e = strings.ToLower(strings.TrimSpace(e))
			if e == "gzip" || e == "deflate" {
				encs = append(encs, e)
			}
		}
	}
	var rd io.Reader = resp.Body
	for i := len(encs) - 1; i >= 0; i-- {
		if encs[i] == "gzip" {
			rd = &lazyGzip{src: rd}
		} else {
			rd = &lazyDeflate{src: rd}
		}
	}
	return rd
}

type lazyGzip struct {
	src io.Reader
	zr  *gzip.Reader
}

func (g *lazyGzip) Read(p []byte) (int, error) {
	if g.zr == nil {
		zr, err := gzip.NewReader(g.src)
		if err != nil {
			if err == io.EOF {
				return 0, io.EOF
			}
			return 0, err
		}
		zr.Multistream(false)
		g.zr = zr
	}
	return g.zr.Read(p)
}

// lazyDeflate is httpx's DeflateDecoder: zlib-wrapped data, else raw
// deflate.
type lazyDeflate struct {
	src io.Reader
	r   io.Reader
}

func (d *lazyDeflate) Read(p []byte) (int, error) {
	if d.r == nil {
		var head [2]byte
		n, err := io.ReadFull(d.src, head[:])
		if n == 0 {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return 0, io.EOF
			}
			return 0, err
		}
		src := io.MultiReader(bytes.NewReader(head[:n]), d.src)
		if n == 2 && (uint16(head[0])<<8|uint16(head[1]))%31 == 0 && head[0]&0x0f == 8 {
			zr, err := zlib.NewReader(src)
			if err != nil {
				return 0, err
			}
			d.r = zr
		} else {
			d.r = flate.NewReader(src)
		}
	}
	return d.r.Read(p)
}

func (s *Server) relayBuffered(w http.ResponseWriter, resp *http.Response, gw *Gateway, p *Prepared, original, openai bool) {
	content, err := io.ReadAll(decodedBody(resp))
	resp.Body.Close()
	if err != nil {
		internalError(w)
		return
	}
	responseHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)
	w.Write(content)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	var usage any = pyjson.NewObject()
	answer := ""
	var bodyModel *string
	data, derr := LoadsBytes(content)
	if derr == errTooDeep {
		// resp.json() raised RecursionError after the response was sent:
		// the turn is not journaled.
		return
	}
	if o, ok := data.(*pyjson.Object); ok && derr == nil {
		if openai {
			usage = NormalizeOpenAIUsage(getOr(o, "usage", nil))
			answer = openAIText(o)
		} else {
			u := getOr(o, "usage", pyjson.NewObject())
			if !pyjson.Truthy(u) {
				u = pyjson.NewObject()
			}
			usage = u
			answer = bufferedText(o)
			if m, ok := getOr(o, "model", nil).(string); ok {
				bodyModel = &m
			}
		}
	}
	s.Record(gw, p, usage, original, answer, servedTarget(resp.Header.Values(SelectedModelHeader), bodyModel))
}

func (s *Server) relayStream(w http.ResponseWriter, resp *http.Response, gw *Gateway, p *Prepared, original, openai bool) {
	defer resp.Body.Close()
	sn := NewSniffer(openai)
	responseHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	rd := decodedBody(resp)
	buf := make([]byte, 64*1024)
	for {
		n, err := rd.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			sn.Feed(chunk)
			if sn.Raised {
				panic(http.ErrAbortHandler)
			}
			// A client that went away gets nothing more; the turn is
			// still read to its end and journaled.
			w.Write(chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			// The upstream cut the stream after the response started:
			// the oracle's app raises and the connection is dropped.
			panic(http.ErrAbortHandler)
		}
	}
	var model *string
	if !openai {
		model = sn.Model
	}
	s.Record(gw, p, sn.Usage.Value(), original, sn.Text.String(), servedTarget(resp.Header.Values(SelectedModelHeader), model))
}

// Record is _record: measure and journal the turn, best effort.
func (s *Server) Record(gw *Gateway, p *Prepared, usage any, original bool, answer string, served *string) {
	if p == nil || gw == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	eff := *p
	if gw.RouterMode == ModeExternal {
		d, err := gw.ExternalDecision(p, served)
		if err != nil {
			return
		}
		eff.Decision = d
	} else if original {
		eff.Decision.Model = eff.Decision.RequestedModel
		eff.Decision.Tier = "tier2-frontier"
		eff.Decision.Downgraded = false
		eff.Decision.Reason = "downgrade rejected by upstream 4xx; served the original model"
	}
	gw.Finish(&eff, usage, answer, s.now())
}

// servedTarget is _served_target: the header (its values joined) and the
// body's model must both be present and agree.
func servedTarget(header []string, bodyModel *string) *string {
	if len(header) == 0 || bodyModel == nil {
		return nil
	}
	h := latin1(strings.Join(header, ", "))
	if h == "" || *bodyModel == "" {
		return nil
	}
	h = pystr.Strip(h)
	if h != pystr.Strip(*bodyModel) {
		return nil
	}
	return &h
}

// bufferedText is _extract_buffered_text.
func bufferedText(o *pyjson.Object) string {
	content, ok := getOr(o, "content", []any{}).([]any)
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, block := range content {
		bo, ok := block.(*pyjson.Object)
		if !ok || !eqStr(getOr(bo, "type", nil), "text") {
			continue
		}
		t, ok := getOr(bo, "text", "").(string)
		if !ok {
			return ""
		}
		b.WriteString(t)
	}
	return b.String()
}

// openAIText is extract_openai_text.
func openAIText(o *pyjson.Object) string {
	choices := getOr(o, "choices", nil)
	if !pyjson.Truthy(choices) {
		return ""
	}
	var first any
	switch c := choices.(type) {
	case []any:
		first = c[0]
	case string:
		return ""
	default:
		return ""
	}
	fo, ok := first.(*pyjson.Object)
	if !ok {
		return ""
	}
	msg := getOr(fo, "message", nil)
	if !pyjson.Truthy(msg) {
		msg = pyjson.NewObject()
	}
	mo, ok := msg.(*pyjson.Object)
	if !ok {
		return ""
	}
	t, _ := getOr(mo, "content", nil).(string)
	return t
}

func (s *Server) countTokensLocally(w http.ResponseWriter, body []byte) {
	v, err := LoadsBytes(body)
	if err == errTooDeep {
		internalError(w)
		return
	}
	o, ok := v.(*pyjson.Object)
	if err != nil || !ok {
		sendJSON(w, 400, anthropicError("count_tokens needs a JSON object body"))
		return
	}
	n, err := EstimatePrefixTokens(o)
	if err == errTooDeep {
		internalError(w)
		return
	}
	if err != nil {
		sendJSON(w, 400, anthropicError("count_tokens body has a malformed system or messages field"))
		return
	}
	sendJSON(w, 200, pyjson.NewObject().Set("input_tokens", n))
}

func anthropicError(msg string) *pyjson.Object {
	return pyjson.NewObject().Set("type", "error").Set("error",
		pyjson.NewObject().Set("type", "invalid_request_error").Set("message", msg))
}

// Reloader is ConfigReloader: the Anthropic wire's gateway, rebuilt from
// config.yaml when the file changes (checked at most every two seconds,
// on a request), on SIGHUP and on POST /_reload.
type Reloader struct {
	Path        string
	Rebuild     func() (*Gateway, error)
	MinInterval time.Duration

	mu          sync.Mutex
	built       *Gateway
	mtime       *int64
	failedMtime *int64
	checkedAt   time.Time
	checked     bool
}

func (rl *Reloader) statMtime() int64 {
	st, err := os.Stat(rl.Path)
	if err != nil {
		return 0
	}
	return st.ModTime().UnixNano()
}

func (rl *Reloader) load() bool {
	m := rl.statMtime()
	if rl.built != nil && ((rl.mtime != nil && *rl.mtime == m) || (rl.failedMtime != nil && *rl.failedMtime == m)) {
		return false
	}
	g, err := rl.Rebuild()
	if err != nil || g == nil {
		rl.failedMtime = &m
		return false
	}
	rl.built, rl.mtime, rl.failedMtime = g, &m, nil
	return true
}

// Current is ConfigReloader.current().
func (rl *Reloader) Current() *Gateway {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	if rl.built == nil || !rl.checked || now.Sub(rl.checkedAt) >= rl.MinInterval {
		rl.checkedAt, rl.checked = now, true
		rl.load()
	}
	return rl.built
}

// Reload is ConfigReloader.reload(): true when it rebuilt.
func (rl *Reloader) Reload() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.checkedAt, rl.checked = time.Now(), true
	rl.mtime, rl.failedMtime = nil, nil
	return rl.load()
}

package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/netproxy"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// This file is `daisugi tiers setup --remote`: model_host.probe, record
// and describe_host. One round trip per candidate wire, the first that
// answers wins, and nothing is recorded for a host no wire identified.

const ollamaDefaultPort = 11434

// contextFloor is model_host.CONTEXT_FLOOR.
const contextFloor = 32768

var hostKinds = []string{"ollama", "openai", "anthropic"}

var kindBackend = map[string]string{"ollama": "ollama", "openai": "openai-compatible", "anthropic": "anthropic-compatible"}

// openaiContextKeys is model_host._OPENAI_CONTEXT_KEYS.
var openaiContextKeys = []string{"context_length", "context_window", "max_model_len", "n_ctx_train"}

var numCtx = regexp.MustCompile(`\bnum_ctx[ \t\n\r\x0b\x0c]+([0-9]+)`)

const probeModel = "daisugi-probe"

// errHostUnread is a host answer this binary does not read the way the
// oracle does (a value whose type Python takes another way).
var errHostUnread = errors.New("the host answered with a value this binary does not read")

// hostInfo is model_host.HostInfo.
type hostInfo struct {
	baseURL, kind string
	models        []string
	chosen        *string
	context       *int64
	warnings      []string
	reachable     bool
}

// probed is model_host._Probed.
type probed struct {
	models  []string
	chosen  *string
	context *int64
}

// contextFloorWarning is model_host.context_floor_warning.
func contextFloorWarning(ctx *int64) string {
	if ctx == nil {
		return "context window unknown; set it with `--context 32768` if you know it"
	}
	if *ctx < contextFloor {
		return fmt.Sprintf("%d tokens is below the 32K floor agentic coding needs; expect truncation", *ctx)
	}
	return ""
}

// parseRemote is model_host.parse_remote.
func parseRemote(spec string) (string, *int64, error) {
	if strings.Contains(spec, "://") {
		return "", nil, fmt.Errorf("give host[:port] without a scheme, for example box:11434; got %s", pystr.Repr(spec))
	}
	if i := strings.LastIndex(spec, ":"); i >= 0 {
		host, portS := spec[:i], spec[i+1:]
		for _, r := range portS {
			if r >= 0x80 {
				// str.isdigit counts other scripts' digits, and int()
				// reads some of them: not read here.
				return "", nil, errHostUnread
			}
		}
		if portS != "" && pyIsDigit(portS) {
			n, ok := new(big.Int).SetString(portS, 10)
			if !ok || !n.IsInt64() {
				return "", nil, errHostUnread
			}
			v := n.Int64()
			return host, &v, nil
		}
	}
	return spec, nil, nil
}

// pyIsDigit is str.isdigit on ASCII text.
func pyIsDigit(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func hostBaseURL(host string, port *int64) string {
	if port != nil {
		return fmt.Sprintf("http://%s:%d", host, *port)
	}
	if strings.Contains(host, ":") {
		return "http://" + host
	}
	return fmt.Sprintf("http://%s:%d", host, ollamaDefaultPort)
}

// hostClient sends the probe's requests as httpx does, through the
// proxies httpx would use.
type hostClient struct {
	c        *http.Client
	answered bool
}

func (e *Env) newHostClient() *hostClient {
	rules := netproxy.HttpxFromVars(netproxy.FromEnviron(e.Environ), true)
	base := &http.Transport{DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext}
	return &hostClient{c: &http.Client{Transport: netproxy.Transport(rules, base), Timeout: 3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// send is model_host._send: the status and the body, or ok false on any
// transport error. Any response marks the host answered.
func (h *hostClient) send(method, url string, body []byte) (status int, text []byte, ok bool) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return 0, nil, false
	}
	req.Header.Set("Accept", "*/*")
	// The client library's own words; the compare holds them as {CLIENT}
	// (SR-1).
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", "opendaisugi")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.c.Do(req)
	if err != nil {
		return 0, nil, false
	}
	defer resp.Body.Close()
	h.answered = true
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, true
	}
	return resp.StatusCode, b, true
}

// jsonBody is resp.json(): the decoded value, or ok false where it raises.
func jsonBody(b []byte) (any, bool, error) {
	text, derr := pystr.DecodeStrict(b)
	if derr != nil {
		return nil, false, nil
	}
	v, err := pyjson.Loads(text)
	if err != nil {
		if errors.Is(err, pyjson.ErrUnsupported) {
			return nil, false, errHostUnread
		}
		return nil, false, nil
	}
	return v, true, nil
}

// pyIntOf is isinstance(v, int) and its value: a bool is refused, since
// Python counts it an int.
func pyIntOf(v any) (*int64, error) {
	switch x := v.(type) {
	case bool:
		return nil, errHostUnread
	case pyjson.Int:
		n, ok := new(big.Int).SetString(x.Text, 10)
		if !ok || !n.IsInt64() {
			return nil, errHostUnread
		}
		i := n.Int64()
		return &i, nil
	case int:
		i := int64(x)
		return &i, nil
	case int64:
		return &x, nil
	}
	return nil, nil
}

// namesOf is [m[key] for m in xs if isinstance(m, dict) and m.get(key)].
func namesOf(xs []any, key string) ([]string, error) {
	var out []string
	for _, x := range xs {
		o, ok := x.(*pyjson.Object)
		if !ok {
			continue
		}
		v := o.Value(key)
		if !pyjson.Truthy(v) {
			continue
		}
		s, isStr := v.(string)
		if !isStr {
			return nil, errHostUnread
		}
		out = append(out, s)
	}
	return out, nil
}

func (h *hostClient) probeOllama(base string) (*probed, error) {
	st, b, ok := h.send("GET", base+"/api/tags", nil)
	if !ok || st != 200 {
		return nil, nil
	}
	v, ok, err := jsonBody(b)
	if err != nil || !ok {
		return nil, err
	}
	data, isObj := v.(*pyjson.Object)
	if !isObj {
		return nil, nil
	}
	list, isList := data.Value("models").([]any)
	if !isList {
		return nil, nil
	}
	models, err := namesOf(list, "name")
	if err != nil {
		return nil, err
	}
	p := &probed{models: models}
	if len(models) == 0 {
		return p, nil
	}
	p.chosen = &models[0]
	body := []byte(`{"model":` + pyjsonStr(models[0]) + `}`)
	st, b, ok = h.send("POST", base+"/api/show", body)
	if !ok || st != 200 {
		return p, nil
	}
	sv, ok, err := jsonBody(b)
	if err != nil {
		return nil, err
	}
	show, isObj := sv.(*pyjson.Object)
	if !ok || !isObj {
		return p, nil
	}
	// _ollama_num_ctx(show) or _ollama_context_length(show)
	for _, f := range []string{"parameters", "modelfile"} {
		if s, isStr := show.Value(f).(string); isStr {
			if m := numCtx.FindStringSubmatch(s); m != nil {
				n, ok := new(big.Int).SetString(m[1], 10)
				if !ok || !n.IsInt64() {
					return nil, errHostUnread
				}
				if n.Sign() != 0 {
					i := n.Int64()
					p.context = &i
					return p, nil
				}
				break
			}
		}
	}
	if info, isObj := show.Value("model_info").(*pyjson.Object); isObj {
		for _, k := range info.Keys() {
			if !strings.HasSuffix(k, ".context_length") {
				continue
			}
			n, err := pyIntOf(info.Value(k))
			if err != nil {
				return nil, err
			}
			if n != nil {
				p.context = n
				break
			}
		}
	}
	return p, nil
}

func (h *hostClient) probeOpenAI(base string) (*probed, error) {
	st, b, ok := h.send("GET", base+"/v1/models", nil)
	if !ok || st != 200 {
		return nil, nil
	}
	v, ok, err := jsonBody(b)
	if err != nil || !ok {
		return nil, err
	}
	data, isObj := v.(*pyjson.Object)
	if !isObj {
		return nil, nil
	}
	entries, isList := data.Value("data").([]any)
	if !isList {
		return nil, nil
	}
	models, err := namesOf(entries, "id")
	if err != nil {
		return nil, err
	}
	p := &probed{models: models}
	if len(models) > 0 {
		p.chosen = &models[0]
	}
	for _, x := range entries {
		o, isObj := x.(*pyjson.Object)
		if !isObj {
			continue
		}
		id := o.Value("id")
		// e.get("id") == chosen: chosen None matches an entry with no id.
		match := (p.chosen == nil && id == nil) || (p.chosen != nil && id == *p.chosen)
		if !match {
			continue
		}
		for _, k := range openaiContextKeys {
			n, err := pyIntOf(o.Value(k))
			if err != nil {
				return nil, err
			}
			if n != nil {
				p.context = n
				break
			}
		}
		break
	}
	return p, nil
}

func (h *hostClient) probeAnthropic(base string) (*probed, error) {
	body := []byte(`{"model":"` + probeModel + `","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
	st, b, ok := h.send("POST", base+"/v1/messages", body)
	if !ok || st != 200 {
		return nil, nil
	}
	v, ok, err := jsonBody(b)
	if err != nil || !ok {
		return nil, err
	}
	data, isObj := v.(*pyjson.Object)
	if !isObj || data.Value("type") != "message" {
		return nil, nil
	}
	name, isStr := data.Value("model").(string)
	if !isStr || name == "" || name == probeModel {
		return &probed{}, nil
	}
	return &probed{models: []string{name}, chosen: &name}, nil
}

// probeHost is model_host.probe.
func (e *Env) probeHost(host string, port *int64, kind string) (*hostInfo, error) {
	if kind != "auto" && kindBackend[kind] == "" {
		return nil, fmt.Errorf("unknown host kind %s; choose one of: auto, %s", pystr.Repr(kind), strings.Join(hostKinds, ", "))
	}
	base := hostBaseURL(host, port)
	order := hostKinds
	if kind != "auto" {
		order = []string{kind}
	}
	h := e.newHostClient()
	for _, k := range order {
		var p *probed
		var err error
		switch k {
		case "ollama":
			p, err = h.probeOllama(base)
		case "openai":
			p, err = h.probeOpenAI(base)
		case "anthropic":
			p, err = h.probeAnthropic(base)
		}
		if err != nil {
			return nil, err
		}
		if p == nil {
			continue
		}
		info := &hostInfo{baseURL: base, kind: k, models: p.models, chosen: p.chosen, context: p.context, reachable: true}
		if w := contextFloorWarning(p.context); w != "" {
			info.warnings = []string{w}
		}
		return info, nil
	}
	w := "could not reach " + base + "; no request got a response. " +
		"Check that the server runs and that this machine can reach it."
	if h.answered {
		w = "could not identify a model server at " + base + "; it answered on none of: " +
			strings.Join(order, ", ") + ". Check the port, or pass --kind for the wire it speaks."
	}
	return &hostInfo{baseURL: base, kind: "unknown", warnings: []string{w}, reachable: h.answered}, nil
}

// setupRemote is the --remote branch of tiers setup.
func (e *Env) setupRemote(cmd, dataDir, remote, kind string, model *string, context *int64) error {
	host, port, err := parseRemote(remote)
	var info *hostInfo
	if err == nil {
		info, err = e.probeHost(host, port, kind)
	}
	if errors.Is(err, errHostUnread) {
		return e.refuse(cmd, err)
	}
	if err != nil {
		return e.fail3(err.Error(),
			"--remote takes a bare HOST[:PORT], and --kind takes one of the listed wires.",
			"run: daisugi tiers setup --remote HOST[:PORT] --kind auto", 1)
	}
	if kindBackend[info.kind] == "" {
		reason := "unknown host kind " + pystr.Repr(info.kind)
		if len(info.warnings) > 0 {
			reason = info.warnings[len(info.warnings)-1]
		}
		code := 1
		if !info.reachable {
			code = 3
		}
		return e.fail3("cannot record "+info.baseURL+": "+reason,
			"nothing was written. A recorded guess would poison the config.",
			"check that the host is reachable and serves Ollama, an OpenAI-compatible /v1, or an "+
				"Anthropic-compatible /v1/messages. Or pass --kind explicitly.", code)
	}
	path := gateroot.Join(dataDir, "config.yaml")
	if _, err := config.Load(path); err != nil {
		return e.refuse(cmd, fmt.Errorf("%s is not one this binary rewrites: %w", path, err))
	}
	hostModel := info.chosen
	if model != nil && *model != "" {
		hostModel = model
	}
	ctx := info.context
	if context != nil {
		ctx = context
	}
	upd := map[string]any{
		"llm_base_url":  info.baseURL,
		"llm_host_kind": info.kind,
		"llm_backend":   kindBackend[info.kind],
	}
	upd["llm_host_model"] = nil
	if hostModel != nil {
		upd["llm_host_model"] = *hostModel
	}
	upd["llm_context_window"] = nil
	if ctx != nil {
		upd["llm_context_window"] = pyjson.Int{Text: strconv.FormatInt(*ctx, 10)}
	}
	if err := config.Save(path, e.home, upd); err != nil {
		return e.refuse(cmd, fmt.Errorf("%s is not one this binary rewrites: %w", path, err))
	}
	// describe_host
	shown := "unset"
	if hostModel != nil && *hostModel != "" {
		shown = *hostModel
	}
	e.echo("host: %s (%s)\n", info.baseURL, info.kind)
	e.echo("model: %s\n", shown)
	if ctx != nil && *ctx != 0 {
		e.echo("context window: %d tokens\n", *ctx)
	} else {
		e.echo("context window: unknown\n")
	}
	var warnCtx *int64
	if ctx != nil {
		warnCtx = ctx
	}
	if w := contextFloorWarning(warnCtx); w != "" {
		e.echo("warning: %s\n", w)
	}
	var env [][2]string
	switch info.kind {
	case "ollama":
		env = [][2]string{{"ANTHROPIC_BASE_URL", info.baseURL}, {"ANTHROPIC_AUTH_TOKEN", "ollama"}}
	case "anthropic":
		env = [][2]string{{"ANTHROPIC_BASE_URL", info.baseURL}}
	}
	if len(env) > 0 {
		e.echo("point your harness at it:\n")
		for _, kv := range env {
			e.echo("  %s=%s\n", kv[0], kv[1])
		}
		e.echo("or run: daisugi gateway --upstream %s and point the harness at the gateway. "+
			"The gateway answers count_tokens itself. Most self-hosted servers do not.\n", info.baseURL)
	} else {
		e.echo("this host speaks the OpenAI wire only. Point an OpenAI wire harness at it directly, " +
			"or route it through `daisugi gateway --openai-upstream`.\n")
	}
	return nil
}

// pyjsonStr is a JSON string as json.dumps(s, ensure_ascii=False) writes it.
func pyjsonStr(s string) string { return pyjson.Dumps(s, false) }

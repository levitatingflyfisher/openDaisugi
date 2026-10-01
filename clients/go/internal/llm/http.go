package llm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"daisugi-verify/internal/netproxy"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// This file is opendaisugi/llm_client.py: the HTTP backend's wires, the
// request bytes, the reply, the error texts (rulings LLM-2 to LLM-9,
// LLM-14), and the structured call's re-ask loop.

const (
	defaultMaxTokens = 8192
	defaultTimeout   = 600.0
	cutText          = "The output is incomplete due to a max_tokens length limit."
	noKeyText        = "no ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN is set"
	reaskText        = "Correct your JSON ONLY RESPONSE, based on the following errors:\n"
)

var (
	keyRE      = regexp.MustCompile(`(sk-[a-zA-Z0-9_-]{4})[a-zA-Z0-9_-]{8,}([a-zA-Z0-9_-]{4})`)
	userinfoRE = regexp.MustCompile(`://[^/@]*@`)
)

// Clean is llm_client.clean: key-shaped tokens shortened, URL credentials
// dropped.
func Clean(s string) string {
	return keyRE.ReplaceAllString(userinfoRE.ReplaceAllString(s, "://"), "${1}...${2}")
}

func fail(s string) *Error { return &Error{Clean(s)} }

// Wire is where one call goes: Kind is "messages" or "chat".
type Wire struct {
	Kind    string
	Model   string
	URL     string
	Headers [][2]string
}

// Message is one chat message with string content.
type Message struct{ Role, Content string }

func first(getenv func(string) (string, bool), names ...string) string {
	for _, n := range names {
		if v, _ := getenv(n); v != "" {
			return v
		}
	}
	return ""
}

func join(base, suffix string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, suffix) {
		return base
	}
	return base + suffix
}

func ollamaURL(base string) string {
	base = strings.TrimRight(base, "/")
	switch {
	case strings.HasSuffix(base, "/chat/completions"):
		return base
	case strings.HasSuffix(base, "/v1"):
		return base + "/chat/completions"
	}
	return base + "/v1/chat/completions"
}

// ResolveWire is llm_client.resolve_wire.
func ResolveWire(model, baseURL, apiKey string, getenv func(string) (string, bool)) (*Wire, *Error) {
	h := [][2]string{{"accept", "application/json"}, {"accept-encoding", "identity"},
		{"content-type", "application/json"}, {"user-agent", "opendaisugi"}}
	or := func(a, b string) string {
		if a != "" {
			return a
		}
		return b
	}
	if strings.HasPrefix(model, "anthropic/") || strings.HasPrefix(model, "claude") {
		base := or(baseURL, first(getenv, "ANTHROPIC_API_BASE", "ANTHROPIC_BASE_URL"))
		url := join(or(base, "https://api.anthropic.com"), "/v1/messages")
		h = append(h, [2]string{"anthropic-version", "2023-06-01"})
		if key := or(apiKey, first(getenv, "ANTHROPIC_API_KEY")); key != "" {
			h = append(h, [2]string{"x-api-key", key})
		} else if tok := first(getenv, "ANTHROPIC_AUTH_TOKEN"); tok != "" {
			h = append(h, [2]string{"authorization", "Bearer " + tok})
		} else {
			return nil, fail(noKeyText)
		}
		return &Wire{"messages", strings.TrimPrefix(model, "anthropic/"), url, h}, nil
	}
	if strings.HasPrefix(model, "openai/") {
		base := or(baseURL, first(getenv, "OPENAI_API_BASE", "OPENAI_BASE_URL"))
		url := join(or(base, "https://api.openai.com/v1"), "/chat/completions")
		if key := or(apiKey, first(getenv, "OPENAI_API_KEY")); key != "" {
			h = append(h, [2]string{"authorization", "Bearer " + key})
		}
		return &Wire{"chat", strings.TrimPrefix(model, "openai/"), url, h}, nil
	}
	for _, prefix := range []string{"ollama/", "ollama_chat/"} {
		if strings.HasPrefix(model, prefix) {
			base := or(baseURL, first(getenv, "OLLAMA_API_BASE"))
			if apiKey != "" {
				h = append(h, [2]string{"authorization", "Bearer " + apiKey})
			}
			return &Wire{"chat", strings.TrimPrefix(model, prefix), ollamaURL(or(base, "http://localhost:11434")), h}, nil
		}
	}
	return nil, fail("no model wire for " + pystr.Repr(model) +
		": name it anthropic/<model>, openai/<model> or ollama/<model>")
}

// Timeout is llm_client.resolve_timeout with no timeout from the call.
func Timeout(getenv func(string) (string, bool)) float64 {
	if raw, ok := getenv("OPENDAISUGI_LLM_TIMEOUT"); ok {
		if t, ok := pmodel.FloatFromString(raw); ok && !math.IsInf(t, 0) && !math.IsNaN(t) && t > 0 {
			return t
		}
	}
	return defaultTimeout
}

// BodyOpts are the optional keys of a request. MaxTokens < 0 and a nil
// Temperature leave them out. ThinkingBudget > 0 is the Messages wire's
// {"type": "enabled", "budget_tokens": N}, which also raises the default
// max_tokens by N; ReasoningEffort is the chat wire's reasoning_effort.
type BodyOpts struct {
	MaxTokens       int
	Temperature     *int
	JSONObject      bool
	ThinkingBudget  int
	ReasoningEffort string
}

func str(s string) string { return pyjson.Dumps(s, true) }

// Body is llm_client.request_body: compact JSON, every non-ASCII character
// escaped.
func Body(w *Wire, msgs []Message, o BodyOpts) []byte {
	var b strings.Builder
	list := func(ms []Message) {
		b.WriteByte('[')
		for i, m := range ms {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"role":` + str(m.Role) + `,"content":` + str(m.Content) + `}`)
		}
		b.WriteByte(']')
	}
	b.WriteString(`{"model":` + str(w.Model))
	if w.Kind == "messages" {
		var system []string
		var rest []Message
		for _, m := range msgs {
			if m.Role == "system" {
				system = append(system, m.Content)
			} else {
				rest = append(rest, m)
			}
		}
		mt := o.MaxTokens
		if mt < 0 {
			mt = defaultMaxTokens + max(o.ThinkingBudget, 0)
		}
		b.WriteString(`,"max_tokens":` + strconv.Itoa(mt))
		if len(system) > 0 {
			b.WriteString(`,"system":` + str(strings.Join(system, "\n\n")))
		}
		b.WriteString(`,"messages":`)
		list(rest)
		if o.Temperature != nil {
			b.WriteString(`,"temperature":` + strconv.Itoa(*o.Temperature))
		}
		if o.ThinkingBudget > 0 {
			b.WriteString(`,"thinking":{"type":"enabled","budget_tokens":` + strconv.Itoa(o.ThinkingBudget) + `}`)
		}
	} else {
		b.WriteString(`,"messages":`)
		list(msgs)
		if o.MaxTokens >= 0 {
			b.WriteString(`,"max_tokens":` + strconv.Itoa(o.MaxTokens))
		}
		if o.Temperature != nil {
			b.WriteString(`,"temperature":` + strconv.Itoa(*o.Temperature))
		}
		if o.JSONObject {
			b.WriteString(`,"response_format":{"type":"json_object"}`)
		}
		if o.ReasoningEffort != "" {
			b.WriteString(`,"reasoning_effort":` + str(o.ReasoningEffort))
		}
	}
	b.WriteByte('}')
	return []byte(b.String())
}

// Reply is llm_client.Reply.
type Reply struct {
	Text string
	Cut  bool
	// Tokens is the tokens the server reports: input plus output on the
	// Messages wire, total_tokens on the chat wire; nil when it reports
	// none.
	Tokens *int64
	// InputTokens and OutputTokens are Reply.input_tokens and
	// output_tokens: input_tokens and output_tokens on the Messages wire,
	// prompt_tokens and completion_tokens on the chat wire.
	InputTokens, OutputTokens *int64
}

// usageInt is llm_client._int: an int that is not a bool, else nil.
func usageInt(usage *pyjson.Object, key string) *int64 {
	if usage == nil {
		return nil
	}
	n, ok := usage.Value(key).(pyjson.Int)
	if !ok {
		return nil
	}
	v, err := strconv.ParseInt(n.Text, 10, 64)
	if err != nil {
		return nil
	}
	return &v
}

func unreadable(text string) *Error {
	return fail("the model reply could not be read: " + pystr.Slice(text, 0, 200))
}

// ReadReply is llm_client.read_reply.
func ReadReply(w *Wire, status int, text string) (*Reply, *Error) {
	if status < 200 || status > 299 {
		return nil, fail(fmt.Sprintf("the model server answered HTTP %d: %s", status, pystr.Slice(text, 0, 500)))
	}
	v, derr := pyjson.LoadsPy(text, 900)
	o, ok := v.(*pyjson.Object)
	if derr != nil || !ok {
		return nil, unreadable(text)
	}
	if w.Kind == "messages" {
		blocks, isList := o.Value("content").([]any)
		if !isList {
			return nil, unreadable(text)
		}
		var parts []string
		for _, bl := range blocks {
			if bo, isObj := bl.(*pyjson.Object); isObj && bo.Value("type") == "text" {
				if t, isStr := bo.Value("text").(string); isStr {
					parts = append(parts, t)
				}
			}
		}
		usage, _ := o.Value("usage").(*pyjson.Object)
		var tokens *int64
		if i, out := usageInt(usage, "input_tokens"), usageInt(usage, "output_tokens"); i != nil && out != nil {
			t := *i + *out
			tokens = &t
		}
		return &Reply{Text: strings.Join(parts, ""), Cut: o.Value("stop_reason") == "max_tokens", Tokens: tokens,
			InputTokens: usageInt(usage, "input_tokens"), OutputTokens: usageInt(usage, "output_tokens")}, nil
	}
	choices, isList := o.Value("choices").([]any)
	if !isList || len(choices) == 0 {
		return nil, unreadable(text)
	}
	ch, isObj := choices[0].(*pyjson.Object)
	if !isObj {
		return nil, unreadable(text)
	}
	msg, isObj := ch.Value("message").(*pyjson.Object)
	if !isObj {
		return nil, unreadable(text)
	}
	content := ""
	if c, has := msg.Get("content"); has && c != nil {
		s, isStr := c.(string)
		if !isStr {
			return nil, unreadable(text)
		}
		content = s
	}
	usage, _ := o.Value("usage").(*pyjson.Object)
	return &Reply{Text: content, Cut: ch.Value("finish_reason") == "length", Tokens: usageInt(usage, "total_tokens"),
		InputTokens: usageInt(usage, "prompt_tokens"), OutputTokens: usageInt(usage, "completion_tokens")}, nil
}

// Post sends one request as httpx does: the proxies the environment
// names, no redirect followed. It returns the status and the body decoded
// as UTF-8 with replacement, or an *Error with the oracle's text, or an
// error wrapping ErrUnsupported.
func Post(w *Wire, body []byte, timeout float64, proxies *netproxy.Httpx) (int, string, error) {
	d := time.Duration(math.Min(timeout, 1e9) * float64(time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", w.URL, bytes.NewReader(body))
	if err != nil {
		return 0, "", fail("could not reach the model server at " + w.URL)
	}
	for _, kv := range w.Headers {
		req.Header.Set(kv[0], kv[1])
	}
	client := &http.Client{
		Transport:     netproxy.Transport(proxies, &http.Transport{}),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	var raw []byte
	if err == nil {
		raw, err = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	if err != nil {
		var refusal *netproxy.Refusal
		var status *netproxy.StatusError
		switch {
		case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
			return 0, "", fail("the model call to " + w.URL + " timed out")
		case errors.As(err, &refusal):
			if refusal.Fails != "" {
				return 0, "", fail("the model call could not start: " + refusal.Fails)
			}
			if refusal.Invalid != "" {
				// httpx.InvalidURL is not an error post() catches: it
				// reaches the caller as it is.
				return 0, "", fail(refusal.Invalid)
			}
			return 0, "", fmt.Errorf("%w, under a proxy setting it reads as httpx does: %s", ErrUnsupported, refusal.Unported)
		case errors.As(err, &status):
			return 0, "", fail(fmt.Sprintf("the proxy refused the model call: %d %s", status.Code, status.Reason))
		}
		return 0, "", fail("could not reach the model server at " + w.URL)
	}
	if ce := resp.Header.Get("content-encoding"); ce != "" && !strings.EqualFold(ce, "identity") {
		return 0, "", fmt.Errorf("%w: a reply with content-encoding %s", ErrUnsupported, ce)
	}
	return resp.StatusCode, pystr.DecodeReplace(raw), nil
}

// Complete is llm_client.complete: one plain call, its reply's text not
// validated. An error is an *Error with the oracle's text, or wraps
// ErrUnsupported.
func (c *Client) Complete(model string, msgs []Message, o BodyOpts) (*Reply, error) {
	return c.CompleteTimeout(model, msgs, o, Timeout(c.env.Getenv))
}

// CompleteTimeout is complete with an explicit timeout in seconds.
func (c *Client) CompleteTimeout(model string, msgs []Message, o BodyOpts, timeout float64) (*Reply, error) {
	return c.CompleteAt(model, "", msgs, o, timeout)
}

// CompleteAt is complete with a base_url ("" for none) and a timeout.
func (c *Client) CompleteAt(model, baseURL string, msgs []Message, o BodyOpts, timeout float64) (*Reply, error) {
	w, e := ResolveWire(model, baseURL, "", c.env.Getenv)
	if e != nil {
		return nil, e
	}
	status, text, err := Post(w, Body(w, msgs, o), timeout, c.proxies)
	if err != nil {
		return nil, err
	}
	reply, e := ReadReply(w, status, text)
	if e != nil {
		return nil, e
	}
	return reply, nil
}

// withSchema is llm_client.with_schema for a system and a user message.
func withSchema(system, user, suffix string) []Message {
	return []Message{{"system", system + "\n\n" + suffix}, {"user", user}}
}

// errDeadline is a call that ran past its Call.Deadline: asyncio.wait_for's
// TimeoutError around the whole call.
var errDeadline = &Error{"TimeoutError"}

// api is the structured call on the HTTP backend.
func (c *Client) api(call Call) (*pyjson.Object, error) {
	w, e := ResolveWire(call.Model, call.BaseURL, call.APIKey, c.env.Getenv)
	if e != nil {
		return nil, e
	}
	timeout := Timeout(c.env.Getenv)
	msgs := withSchema(call.System, call.User, jsonModeSystem[call.Response.Name])
	retries := call.MaxRetries
	if call.DefaultRetries {
		retries = 3
	}
	attempts := max(retries, 0) + 1
	var firstErr *pmodel.ValidationError
	opts := BodyOpts{MaxTokens: -1, JSONObject: true, ThinkingBudget: call.ThinkingBudget, ReasoningEffort: call.ReasoningEffort}
	for n := 1; n <= attempts; n++ {
		limit := timeout
		if !call.Deadline.IsZero() {
			left := time.Until(call.Deadline).Seconds()
			if left <= 0 {
				return nil, errDeadline
			}
			limit = math.Min(limit, left)
		}
		status, text, err := Post(w, Body(w, msgs, opts), limit, c.proxies)
		if err != nil && !call.Deadline.IsZero() && !time.Now().Before(call.Deadline) {
			return nil, errDeadline
		}
		if err != nil {
			var le *Error
			if errors.As(err, &le) {
				return nil, le
			}
			// A call this binary does not make the oracle's way fails as a
			// model call, with its own text (PX-5).
			return nil, &Error{err.Error()}
		}
		reply, e := ReadReply(w, status, text)
		if e != nil {
			return nil, e
		}
		if reply.Cut {
			return nil, fail(cutText)
		}
		out, verr := pmodel.ValidateJSON(call.Response.Name, call.Response.Model, reply.Text)
		if verr == nil {
			verr = nonFinite(call.Response.Name, out)
		}
		if verr == nil {
			return out.(*pyjson.Object), nil
		}
		if firstErr == nil {
			firstErr = verr
		}
		if n == attempts {
			return nil, fail(flatten(firstErr, n))
		}
		msgs = append(msgs, Message{"assistant", reply.Text}, Message{"user", reaskText + verr.String()})
	}
	return nil, errors.New("unreachable")
}

// flatten is llm_client.flatten: the first attempt's error and the count.
func flatten(first *pmodel.ValidationError, n int) string {
	line := ""
	for _, l := range strings.Split(strings.TrimSpace(first.String()), "\n") {
		if strings.TrimSpace(l) != "" {
			line = strings.TrimSpace(l)
			break
		}
	}
	return fmt.Sprintf("ValidationError: %s (%d attempts)", line, n)
}

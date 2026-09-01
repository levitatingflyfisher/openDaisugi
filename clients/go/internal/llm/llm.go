// Package llm is the oracle's model call for structured output
// (opendaisugi/llm.py with instructor, and claude_code_llm.py): one call
// that asks a model for a reply of a given schema and validates it, with
// the oracle's two backends.
//
//   - claude-code: the local `claude -p --output-format json`, the prompt
//     on stdin, in a neutral working directory, re-asked with the error on
//     a reply that does not parse or validate.
//   - litellm: the Anthropic Messages API over HTTPS, the request bytes
//     litellm sends for an anthropic/ model under instructor's JSON mode,
//     re-asked as instructor re-asks.
//
// Every failure comes back as an *Error whose text is what
// llm.translate_llm_error makes of the oracle's exception. What the oracle
// prints on the way (instructor's log lines on stderr, litellm's banner on
// stdout) is printed the same way. The API key is never printed.
package llm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/verify"
)

// Error is a failed call, worded as translate_llm_error words it.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// ErrUnsupported is a call this binary does not make: a litellm model
// that is not anthropic/<name>. A command checks with Check before it
// writes anything.
var ErrUnsupported = errors.New("only the claude-code backend and anthropic/ models on the litellm backend are in this binary")

// Env is the world a call runs in.
type Env struct {
	Getenv   func(string) (string, bool)
	Home     string
	Stdout   io.Writer
	Stderr   io.Writer
	LookPath func(string) (string, error)
}

// Client makes calls. It keeps the neutral working directory the oracle
// makes once per process.
type Client struct {
	env        Env
	neutralCwd string
	// ClaudeTimeout is ClaudeCodeInstructorClient's timeout_s.
	ClaudeTimeout time.Duration
}

// New is a client over env.
func New(env Env) *Client {
	if env.LookPath == nil {
		env.LookPath = exec.LookPath
	}
	return &Client{env: env, ClaudeTimeout: 120 * time.Second}
}

func (c *Client) getenv(k string) string {
	v, _ := c.env.Getenv(k)
	return v
}

// Backend is resolve_backend(None): OPENDAISUGI_LLM_BACKEND, then the
// llm_backend of ~/.opendaisugi/config.yaml, then auto-detection.
func (c *Client) Backend() string {
	if v := c.getenv("OPENDAISUGI_LLM_BACKEND"); v != "" {
		return v
	}
	cfg, err := config.Load(filepath.Join(c.env.Home, ".opendaisugi", "config.yaml"))
	if err == nil && cfg.LLMBackend != nil {
		if s := pystr.Strip(*cfg.LLMBackend); s != "" {
			return s
		}
	}
	if c.getenv("ANTHROPIC_API_KEY") != "" || c.getenv("ANTHROPIC_AUTH_TOKEN") != "" {
		return "litellm"
	}
	if _, err := c.env.LookPath("claude"); err == nil {
		return "claude-code"
	}
	return "litellm"
}

// Check refuses, before anything is written, a call this binary does not
// make: the litellm backend (or any name but claude-code, which the oracle
// sends to litellm) with a model that is not anthropic/<name>.
func (c *Client) Check(model string) error {
	if c.Backend() == "claude-code" {
		return nil
	}
	if !strings.HasPrefix(model, "anthropic/") {
		return fmt.Errorf("%w: model %s", ErrUnsupported, pystr.Repr(model))
	}
	return nil
}

// preflight is llm.preflight: a missing key or binary is one plain error.
func (c *Client) preflight(backend, model string) *Error {
	switch backend {
	case "litellm":
		anthropic := strings.HasPrefix(model, "anthropic/") || strings.HasPrefix(model, "claude")
		if anthropic && c.getenv("ANTHROPIC_API_KEY") == "" && c.getenv("ANTHROPIC_AUTH_TOKEN") == "" {
			return &Error{"Tried to call the Anthropic API through litellm.\n" +
				"No ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN is set.\n" +
				"Set a key, or run with --llm claude-code to use the local claude CLI."}
		}
	case "claude-code":
		if _, err := c.env.LookPath("claude"); err != nil {
			return &Error{"Tried to use the claude-code backend.\n" +
				"No `claude` command is on PATH.\n" +
				"Install Claude Code, or set ANTHROPIC_API_KEY and run with --llm litellm."}
		}
	}
	return nil
}

// Schema is a response model: its name, as instructor and the schema texts
// name it, and the pydantic model a reply is validated against.
type Schema struct {
	Name  string
	Model *pmodel.Model
}

// Call is one chat.completions.create(model, response_model, messages,
// max_retries).
type Call struct {
	Model      string
	System     string
	User       string
	Response   Schema
	MaxRetries int
}

// Structured makes the call and returns the validated reply as a
// model_dump() value.
func (c *Client) Structured(call Call) (*pyjson.Object, error) {
	backend := c.Backend()
	if e := c.preflight(backend, call.Model); e != nil {
		return nil, e
	}
	if backend == "claude-code" {
		return c.claude(call)
	}
	return c.litellm(call)
}

// ---------------------------------------------------------------------------
// claude-code
// ---------------------------------------------------------------------------

const schemaPreamble = "Respond with ONLY a JSON object that validates against the following schema.\n" +
	"No prose, no code fences, no explanation.\n\n"

// maxReasks is claude_code_llm._MAX_REASKS.
const maxReasks = 3

func (c *Client) claudeArgs() []string {
	args := []string{"-p", "--model=haiku"}
	if raw := strings.TrimSpace(c.getenv("DAISUGI_CLAUDE_ARGS")); raw != "" {
		// A text shlex cannot split is ignored; the oracle logs that on
		// the opendaisugi logger, which prints nothing by default.
		if extra, err := verify.ShlexSplit(raw); err == nil {
			args = append(args, extra...)
		}
	}
	return append(args, "--output-format", "json")
}

func (c *Client) cwd() (string, error) {
	if c.neutralCwd == "" {
		d, err := os.MkdirTemp(os.Getenv("TMPDIR"), "opendaisugi-claude-")
		if err != nil {
			return "", err
		}
		c.neutralCwd = d
	}
	return c.neutralCwd, nil
}

// runClaude is call_claude_p_async: stdout decoded and stripped, or the
// oracle's error for a failed run. A timeout is asyncio's TimeoutError.
func (c *Client) runClaude(prompt string) (string, error) {
	bin, err := c.env.LookPath("claude")
	if err != nil {
		return "", &Error{"claude binary not found: 'claude'"}
	}
	dir, err := c.cwd()
	if err != nil {
		return "", err
	}
	cmd := exec.Command(bin, c.claudeArgs()...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(prompt)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Start(); err != nil {
		return "", &Error{"claude binary not found: 'claude'"}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(c.ClaudeTimeout):
		// _terminate_and_reap: SIGTERM, two seconds, then SIGKILL.
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		return "", &Error{"TimeoutError"}
	}
	code := cmd.ProcessState.ExitCode()
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		code = -int(ws.Signal())
	}
	if code != 0 {
		e := errb.Bytes()
		if len(e) > 500 {
			e = e[:500]
		}
		return "", &Error{fmt.Sprintf("claude -p exited %d: %s", code, pystr.Repr(pystr.DecodeReplace(e)))}
	}
	return pystr.Strip(pystr.DecodeReplace(out.Bytes())), nil
}

// resultText is claude_code_llm._result_text.
func resultText(stdout string) (string, error) {
	notEnvelope := &Error{"claude -p stdout was not its JSON envelope: " + pystr.Repr(pystr.Slice(stdout, 0, 200))}
	v, derr := pyjson.LoadsPy(stdout, 900)
	if derr != nil {
		return "", notEnvelope
	}
	o, ok := v.(*pyjson.Object)
	if !ok {
		return "", notEnvelope
	}
	if t, _ := o.Get("type"); t != "result" {
		return "", notEnvelope
	}
	if e, _ := o.Get("is_error"); pyjson.Truthy(e) {
		r, _ := o.Get("result")
		return "", &Error{"claude -p reported is_error: " + pystr.Repr(pystr.Slice(pyStr(r), 0, 200))}
	}
	r, _ := o.Get("result")
	text, ok := r.(string)
	if !ok {
		return "", notEnvelope
	}
	return text, nil
}

// pyStr is str() of a JSON value.
func pyStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pmodel.Repr(v)
}

// parseStructured is _parse_structured: the first "{" to the last "}",
// read as JSON or as a Python dict literal, validated in Python mode.
func parseStructured(text string, s Schema) (*pyjson.Object, error) {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start == -1 || end <= start {
		return nil, &Error{"no JSON object in claude -p stdout: " + pystr.Repr(pystr.Slice(text, 0, 200))}
	}
	body := text[start : end+1]
	payload, derr := pyjson.LoadsPy(body, 900)
	if derr != nil {
		if d, ok := pmodel.DecodeDictText(body); ok {
			payload = d
		} else {
			return nil, &Error{"claude -p stdout was not valid JSON: " + derr.Error()}
		}
	}
	out, verr := pmodel.Validate(s.Name, s.Model, payload, pmodel.Python)
	if verr == nil {
		verr = nonFinite(s.Name, out)
	}
	if verr != nil {
		return nil, &Error{"claude -p output failed " + s.Name + " validation: " + verr.String()}
	}
	return out.(*pyjson.Object), nil
}

// nonFinite is models.non_finite_error: a reply that holds NaN, Infinity
// or -Infinity is schema-invalid. It gives one finite_number error per
// such number, at its place in the validated reply, in document order,
// and nil when every number is finite.
func nonFinite(title string, reply any) *pmodel.ValidationError {
	errs := pmodel.NonFinite(reply, nil)
	if len(errs) == 0 {
		return nil
	}
	return &pmodel.ValidationError{Title: title, Errs: errs}
}

func (c *Client) claude(call Call) (*pyjson.Object, error) {
	prompt := "[system]\n" + call.System + "\n\n[user]\n" + call.User
	augmented := schemaPreamble + "<json_schema>\n" + claudeSchema[call.Response.Name] + "\n</json_schema>\n\n" + prompt
	reasks := max(0, min(call.MaxRetries, maxReasks))
	attempt := augmented
	for i := 0; i <= reasks; i++ {
		stdout, err := c.runClaude(attempt)
		if err != nil {
			return nil, err
		}
		text, err := resultText(stdout)
		if err != nil {
			return nil, err
		}
		out, err := parseStructured(text, call.Response)
		if err == nil {
			return out, nil
		}
		if i == reasks {
			return nil, err
		}
		attempt = augmented + "\n\nYour last reply could not be used: " + pystr.Slice(err.Error(), 0, 1000) +
			"\nReply again with ONLY one JSON object that validates against the schema."
	}
	return nil, errors.New("unreachable")
}

// ---------------------------------------------------------------------------
// litellm
// ---------------------------------------------------------------------------

// banner is what litellm prints to stdout when a call raises.
const banner = "\n\x1b[1;31mGive Feedback / Get Help: https://github.com/BerriAI/litellm/issues/new\x1b[0m\n" +
	"LiteLLM.Info: If you need to debug this error, use `litellm._turn_on_debug()'.\n\n"

type message struct{ role, text string }

// body is the request litellm sends: compact JSON, non-ASCII kept.
func body(model string, system string, msgs []message, maxTokens int) []byte {
	var b strings.Builder
	b.WriteString(`{"model":`)
	jsonString(&b, model)
	b.WriteString(`,"messages":[`)
	for i, m := range msgs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"role":`)
		jsonString(&b, m.role)
		b.WriteString(`,"content":[{"type":"text","text":`)
		jsonString(&b, m.text)
		b.WriteString(`}]}`)
	}
	b.WriteString(`],"system":[{"type":"text","text":`)
	jsonString(&b, system)
	b.WriteString(`}],"max_tokens":`)
	b.WriteString(strconv.Itoa(maxTokens))
	b.WriteByte('}')
	return []byte(b.String())
}

// jsonString is json.dumps(s, ensure_ascii=False) of a str.
func jsonString(b *strings.Builder, s string) {
	b.WriteString(pyjson.Dumps(s, false))
}

func (c *Client) maxTokens(name string) int {
	if n, ok := anthropicMaxTokens[name]; ok {
		return n
	}
	if v, set := c.env.Getenv("DEFAULT_ANTHROPIC_CHAT_MAX_TOKENS"); set {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return defaultMaxTokens
}

func (c *Client) timeout() float64 {
	if v, set := c.env.Getenv("REQUEST_TIMEOUT"); set {
		if f, ok := pmodel.FloatFromString(v); ok {
			return f
		}
	}
	return 6000
}

// apiError is the exception litellm raises for an HTTP status, worded
// as its str().
func apiError(status int, text string) string {
	name, suffix := "BadRequestError", ""
	switch {
	case status == 401:
		name = "AuthenticationError"
	case status == 403:
		name = "PermissionDeniedError"
	case status == 404:
		name = "NotFoundError"
	case status == 408:
		name = "Timeout"
	case status == 429:
		name = "RateLimitError"
	case status == 502:
		return "litellm.BadGatewayError: AnthropicException BadGatewayError - " + text
	case status == 503:
		name, suffix = "ServiceUnavailableError", ". Handle with `litellm.ServiceUnavailableError`."
	case status >= 500:
		name, suffix = "InternalServerError", ". Handle with `litellm.InternalServerError`."
	}
	return "litellm." + name + ": AnthropicException - " + text + suffix
}

// post sends one request. It returns the reply's text and whether the
// output was cut at max_tokens, or the litellm error text.
func (c *Client) post(payload []byte) (text string, cut bool, errText string) {
	base := c.getenv("ANTHROPIC_API_BASE")
	if base == "" {
		base = c.getenv("ANTHROPIC_BASE_URL")
	}
	if base == "" {
		base = "https://api.anthropic.com"
	}
	url := base
	if !strings.HasSuffix(url, "/v1/messages") {
		url += "/v1/messages"
	}
	timeout := c.timeout()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout*float64(time.Second)))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
	if err != nil {
		return "", false, "litellm.APIConnectionError: AnthropicException - " + err.Error()
	}
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("accept", "application/json")
	req.Header.Set("content-type", "application/json")
	if k := c.getenv("ANTHROPIC_API_KEY"); k != "" {
		req.Header.Set("x-api-key", k)
	} else if t := c.getenv("ANTHROPIC_AUTH_TOKEN"); t != "" {
		req.Header.Set("authorization", "Bearer "+t)
	}
	started := time.Now()
	resp, err := http.DefaultClient.Do(req)
	var raw []byte
	if err == nil {
		raw, err = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			took := time.Since(started).Seconds()
			return "", false, fmt.Sprintf("litellm.Timeout: AnthropicException - litellm.Timeout: Connection timed out. "+
				"Timeout passed=%s, time taken=%s seconds", pyjson.FloatRepr(timeout), pyjson.FloatRepr(pyjson.Round(took, 3)))
		}
		// The key never reaches this text: the error names the URL only.
		return "", false, "litellm.APIConnectionError: AnthropicException - " + err.Error()
	}
	if resp.StatusCode != 200 {
		return "", false, apiError(resp.StatusCode, string(raw))
	}
	v, derr := pyjson.LoadsPy(string(raw), 900)
	o, ok := v.(*pyjson.Object)
	if derr != nil || !ok {
		return "", false, "litellm.APIError: AnthropicException - " + string(raw)
	}
	var parts []string
	if blocks, isList := o.Value("content").([]any); isList {
		for _, bl := range blocks {
			if bo, isObj := bl.(*pyjson.Object); isObj && bo.Value("type") == "text" {
				if t, isStr := bo.Value("text").(string); isStr {
					parts = append(parts, t)
				}
			}
		}
	}
	return strings.Join(parts, ""), o.Value("stop_reason") == "max_tokens", ""
}

func (c *Client) litellm(call Call) (*pyjson.Object, error) {
	name := strings.TrimPrefix(call.Model, "anthropic/")
	system := call.System + "\n\n" + instructorSystem[call.Response.Name]
	msgs := []message{{"user", call.User}}
	attempts := max(call.MaxRetries, 0) + 1
	var firstErr *pmodel.ValidationError
	failed := 0
	for n := 1; n <= attempts; n++ {
		text, cut, errText := c.post(body(name, system, msgs, c.maxTokens(name)))
		if errText != "" {
			fmt.Fprint(c.env.Stdout, banner)
			fmt.Fprintf(c.env.Stderr, "API call failed on attempt %d: %s\n", n, errText)
			fmt.Fprintf(c.env.Stderr, "Max retries exceeded. Total attempts: %d, Last error: %s\n", n, errText)
			if firstErr != nil {
				return nil, &Error{flatten(firstErr, failed)}
			}
			return nil, &Error{errText}
		}
		if cut {
			return nil, &Error{"The output is incomplete due to a max_tokens length limit."}
		}
		out, verr := pmodel.ValidateJSON(call.Response.Name, call.Response.Model, text)
		if verr == nil {
			verr = nonFinite(call.Response.Name, out)
		}
		if verr == nil {
			return out.(*pyjson.Object), nil
		}
		failed++
		if firstErr == nil {
			firstErr = verr
		}
		if n == attempts {
			fmt.Fprintf(c.env.Stderr, "Max retries exceeded. Total attempts: %d, Last error: %s\n", n, verr.String())
			return nil, &Error{flatten(firstErr, failed)}
		}
		msgs = append(msgs, message{"assistant", text},
			message{"user", "Correct your JSON ONLY RESPONSE, based on the following errors:\n" + verr.String()})
	}
	return nil, errors.New("unreachable")
}

// flatten is llm._flatten_retry: the first attempt's cause and the count.
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

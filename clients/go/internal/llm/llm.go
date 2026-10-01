// Package llm is the oracle's model call for structured output
// (opendaisugi/llm_client.py, and claude_code_llm.py): one call that asks a
// model for a reply of a given schema and validates it, with the oracle's
// backends.
//
//   - claude-code: the local `claude -p --output-format json`, the prompt
//     on stdin, in a neutral working directory, re-asked with the error on
//     a reply that does not parse or validate.
//   - the HTTP backend (named api): the Anthropic Messages API or
//     OpenAI-compatible chat completions, chosen by the model name, the
//     request bytes llm_client.py sends, re-asked as it re-asks.
//
// Every failure comes back as an *Error whose text is the oracle's. Nothing
// is printed. The API key is never in an error.
package llm

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/datahome"
	"daisugi-verify/internal/netproxy"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/verify"
)

// Error is a failed call, worded as translate_llm_error words it.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// ErrUnsupported is a call this binary does not make the oracle's way: a
// proxy setting it does not read as httpx does (PX-5), or a reply it does
// not read as httpx does. A command checks with Check before it writes
// anything.
var ErrUnsupported = errors.New("this model call is not in this binary")

// Env is the world a call runs in.
type Env struct {
	Getenv func(string) (string, bool)
	// Environ is the environment in its order, for the proxies httpx
	// reads (LLM-11).
	Environ  []string
	Home     string
	Stdout   io.Writer
	Stderr   io.Writer
	LookPath func(string) (string, error)
}

// Client makes calls. It keeps the neutral working directory the oracle
// makes once per process.
type Client struct {
	env        Env
	proxies    *netproxy.Httpx
	neutralCwd string
	// ClaudeTimeout is ClaudeCodeInstructorClient's timeout_s.
	ClaudeTimeout time.Duration
	// TempDir is where the neutral working directory is made; "" is
	// $TMPDIR, as mkdtemp reads it. A test sets its own.
	TempDir string
}

// New is a client over env.
func New(env Env) *Client {
	if env.LookPath == nil {
		env.LookPath = exec.LookPath
	}
	return &Client{env: env, proxies: netproxy.HttpxFromVars(netproxy.FromEnviron(env.Environ), true),
		ClaudeTimeout: 120 * time.Second}
}

func (c *Client) getenv(k string) string {
	v, _ := c.env.Getenv(k)
	return v
}

// RenamedText is llm.RENAMED_BACKEND_TEXT: the one line that refuses the
// old name of the api backend.
const RenamedText = "The LLM backend 'litellm' is now named 'api'. Use 'api'."

// Renamed reports the old name of the api backend, as resolve_backend
// tests it: with surrounding whitespace stripped.
func Renamed(backend string) bool { return pystr.Strip(backend) == "litellm" }

// Backend is resolve_backend(None): OPENDAISUGI_LLM_BACKEND, then the
// llm_backend of <data home>/config.yaml, then auto-detection. Where
// resolve_backend raises on the old name, this returns it; the caller
// checks Renamed.
func (c *Client) Backend() string {
	if v := c.getenv("OPENDAISUGI_LLM_BACKEND"); v != "" {
		return v
	}
	cfg, err := config.Load(filepath.Join(datahome.Dir(c.getenv, c.env.Home, datahome.Exists), "config.yaml"))
	if err == nil && cfg.LLMBackend != nil {
		if s := pystr.Strip(*cfg.LLMBackend); s != "" {
			return s
		}
	}
	if c.getenv("ANTHROPIC_API_KEY") != "" || c.getenv("ANTHROPIC_AUTH_TOKEN") != "" {
		return "api"
	}
	if _, err := c.env.LookPath("claude"); err == nil {
		return "claude-code"
	}
	return "api"
}

// Check refuses, before anything is written, a call this binary does not
// make the oracle's way: on the HTTP backend, a proxy setting it does not
// read as httpx does.
func (c *Client) Check(model string) error {
	// The old backend name fails in preflight, as the oracle fails.
	if b := c.Backend(); b == "claude-code" || Renamed(b) {
		return nil
	}
	if r := c.proxies.Refusal(); r != nil && r.Unported != "" {
		return fmt.Errorf("%w, under a proxy setting it reads as httpx does: %s", ErrUnsupported, r.Unported)
	}
	return nil
}

// preflight is llm.preflight: the old backend name, or a missing key or
// binary, is one plain error.
func (c *Client) preflight(backend, model string) *Error {
	if Renamed(backend) {
		return &Error{RenamedText}
	}
	switch backend {
	case "api":
		anthropic := strings.HasPrefix(model, "anthropic/") || strings.HasPrefix(model, "claude")
		if anthropic && c.getenv("ANTHROPIC_API_KEY") == "" && c.getenv("ANTHROPIC_AUTH_TOKEN") == "" {
			return &Error{"Tried to call the Anthropic API.\n" +
				"No ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN is set.\n" +
				"Set a key, or run with --llm claude-code to use the local claude CLI."}
		}
	case "claude-code":
		if _, err := c.env.LookPath("claude"); err != nil {
			return &Error{"Tried to use the claude-code backend.\n" +
				"No `claude` command is on PATH.\n" +
				"Install Claude Code, or set ANTHROPIC_API_KEY and run with --llm api."}
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
// max_retries). ThinkingBudget and ReasoningEffort are the thinking
// kwargs, BaseURL and APIKey the base_url and api_key arguments; the
// claude-code backend ignores all four, as the oracle's does. A Deadline
// that is not zero bounds the whole call, re-asks included, as
// asyncio.wait_for does around it: past it the call fails with
// "TimeoutError".
type Call struct {
	Model      string
	System     string
	User       string
	Response   Schema
	MaxRetries int
	// DefaultRetries is a call that passes no max_retries: the HTTP
	// client's default is 3, the claude-code client's 0.
	DefaultRetries  bool
	ThinkingBudget  int
	ReasoningEffort string
	BaseURL         string
	APIKey          string
	Deadline        time.Time
}

// Preflight is llm.preflight(None, model=model) for the backend in effect:
// the old backend name, or a missing key or binary, as one error. The
// oracle raises it as LLMNotConfigured, which is not a failed model call.
func (c *Client) Preflight(model string) *Error {
	return c.preflight(c.Backend(), model)
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
	return c.api(call)
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
		base := c.TempDir
		if base == "" {
			base = os.Getenv("TMPDIR")
		}
		d, err := os.MkdirTemp(base, "opendaisugi-claude-")
		if err != nil {
			return "", err
		}
		c.neutralCwd = d
	}
	return c.neutralCwd, nil
}

// runClaude is call_claude_p_async: stdout decoded and stripped, or the
// oracle's error for a failed run. A timeout is asyncio's TimeoutError.
func (c *Client) runClaude(prompt string, limit time.Duration) (string, error) {
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
	case <-time.After(limit):
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
	tuple := false
	if derr != nil {
		d := pmodel.DecodeDictText(body)
		if d.Refuse != "" {
			return nil, fmt.Errorf("%w: a reply that holds %s", ErrUnsupported, d.Refuse)
		}
		if d.Dict == nil {
			return nil, &Error{"claude -p stdout was not valid JSON: " + derr.Error()}
		}
		payload, tuple = d.Dict, d.Tuple
	}
	out, verr := pmodel.Validate(s.Name, s.Model, payload, pmodel.Python)
	if verr != nil {
		if why := verr.Unreadable(); why != "" {
			return nil, fmt.Errorf("%w: a reply that holds %s", ErrUnsupported, why)
		}
		if tuple {
			return nil, fmt.Errorf("%w: a reply that holds a tuple, and its validation fails", ErrUnsupported)
		}
	}
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
	retries := call.MaxRetries
	if call.DefaultRetries {
		retries = 0
	}
	reasks := max(0, min(retries, maxReasks))
	attempt := augmented
	for i := 0; i <= reasks; i++ {
		limit := c.ClaudeTimeout
		if !call.Deadline.IsZero() {
			left := time.Until(call.Deadline)
			if left <= 0 {
				return nil, errDeadline
			}
			limit = min(limit, left)
		}
		stdout, err := c.runClaude(attempt, limit)
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
		if i == reasks || errors.Is(err, ErrUnsupported) {
			return nil, err
		}
		attempt = augmented + "\n\nYour last reply could not be used: " + pystr.Slice(err.Error(), 0, 1000) +
			"\nReply again with ONLY one JSON object that validates against the schema."
	}
	return nil, errors.New("unreachable")
}

// Sync is call_claude_p_sync: `claude -p --model=MODEL`, then
// DAISUGI_CLAUDE_ARGS, then extra, with the prompt on stdin, run as
// subprocess.run runs it in the neutral working directory, and stdout
// decoded and stripped. A failed run is the oracle's
// EnvelopeGenerationError text.
func (c *Client) Sync(prompt, model string, timeoutS float64, extra ...string) (string, error) {
	return c.SyncIn("", prompt, model, timeoutS, extra...)
}

// SyncIn is Sync run in dir, or in the neutral working directory when
// dir is "".
func (c *Client) SyncIn(dir, prompt, model string, timeoutS float64, extra ...string) (string, error) {
	bin, lerr := c.env.LookPath("claude")
	if lerr != nil {
		return "", &Error{"claude binary not found: 'claude'"}
	}
	if dir == "" {
		var err error
		if dir, err = c.cwd(); err != nil {
			return "", err
		}
	}
	args := []string{"-p", "--model=" + model}
	if raw := strings.TrimSpace(c.getenv("DAISUGI_CLAUDE_ARGS")); raw != "" {
		if more, err := verify.ShlexSplit(raw); err == nil {
			args = append(args, more...)
		}
	}
	args = append(args, extra...)
	cmd := exec.Command(bin, args...)
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
	case <-time.After(time.Duration(timeoutS * float64(time.Second))):
		_ = cmd.Process.Kill()
		<-done
		return "", &Error{"claude -p timed out after " + pyjson.FloatRepr(timeoutS) + "s"}
	}
	code := cmd.ProcessState.ExitCode()
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		code = -int(ws.Signal())
	}
	if code != 0 {
		return "", &Error{fmt.Sprintf("claude -p exited %d: %s", code,
			pystr.Repr(pystr.Slice(pystr.DecodeReplace(errb.Bytes()), 0, 500)))}
	}
	return pystr.Strip(pystr.DecodeReplace(out.Bytes())), nil
}

// FirstJSONObject is claude_code_llm._extract_first_json_object: the
// first "{" to the last "}" of text, read as JSON or as a Python dict
// literal.
func FirstJSONObject(text string) (*pyjson.Object, error) {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start == -1 || end <= start {
		return nil, &Error{"no JSON object in claude -p stdout: " + pystr.Repr(pystr.Slice(text, 0, 200))}
	}
	body := text[start : end+1]
	v, derr := pyjson.LoadsPy(body, 900)
	if derr != nil {
		if derr.TooDeep {
			return nil, fmt.Errorf("%w: a reply nested past what json.loads reads", ErrUnsupported)
		}
		d := pmodel.DecodeDictText(body)
		if d.Refuse != "" {
			return nil, fmt.Errorf("%w: a reply that holds %s", ErrUnsupported, d.Refuse)
		}
		if d.Dict != nil {
			return d.Dict, nil
		}
		return nil, &Error{"claude -p stdout was not valid JSON: " + derr.Error()}
	}
	o, ok := v.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("%w: a reply json.loads reads as no dict", ErrUnsupported)
	}
	return o, nil
}

// Metered is call_claude_p_metered: `claude -p --model=MODEL
// --output-format json` with the prompt on stdin, run as subprocess.run
// runs it, and the reply's text with Claude Code's own token count and
// cost (nil when the reply does not say). A stdout that is not JSON is
// the text itself, with no meter.
func (c *Client) Metered(prompt, model string, timeoutS float64) (text string, tokens *int64, cost *float64, err error) {
	raw, err := c.Sync(prompt, model, timeoutS, "--output-format", "json")
	if err != nil {
		return "", nil, nil, err
	}
	v, derr := pyjson.LoadsPy(raw, 900)
	if derr != nil {
		return raw, nil, nil, nil
	}
	o, ok := v.(*pyjson.Object)
	if !ok {
		return "", nil, nil, &Error{fmt.Sprintf("'%s' object has no attribute 'get'", pmodel.TypeName(v))}
	}
	if e, _ := o.Get("is_error"); pyjson.Truthy(e) {
		r, _ := o.Get("result")
		return "", nil, nil, &Error{"claude -p reported is_error: " + pystr.Repr(pystr.Slice(pyStr(r), 0, 200))}
	}
	if r, _ := o.Get("result"); pyjson.Truthy(r) {
		text = pyStr(r)
	}
	if usage, ok := o.Value("usage").(*pyjson.Object); ok {
		var sum int64
		have := false
		for _, f := range []string{"input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"} {
			switch n := usage.Value(f).(type) {
			case pyjson.Int:
				if x, err := strconv.ParseInt(n.Text, 10, 64); err == nil {
					sum += x
					have = true
				}
			case bool:
				if n {
					sum++
				}
				have = true
			case float64:
				sum += int64(n)
				have = true
			case pyjson.Float:
				sum += int64(n)
				have = true
			}
		}
		if have {
			tokens = &sum
		}
	}
	switch x := o.Value("total_cost_usd").(type) {
	case float64:
		cost = &x
	case pyjson.Float:
		f := float64(x)
		cost = &f
	case pyjson.Int:
		if f, err := strconv.ParseFloat(x.Text, 64); err == nil {
			cost = &f
		}
	}
	return text, tokens, cost, nil
}

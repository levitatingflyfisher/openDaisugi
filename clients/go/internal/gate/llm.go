package gate

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pyre"
	"daisugi-verify/internal/pystr"
)

// This file is llm_check.run_llm_check on its litellm backend, the model
// call an llm_check predicate makes: one POST to Anthropic's messages API,
// the verdict read from the reply, and every failure worded as litellm
// words it, since the failure text becomes the deny reason. A call through
// the claude-code backend (`claude -p`), to a model litellm would route
// elsewhere, or under settings that change litellm's own behavior is
// denied undecided.

const (
	llmDefaultModel = "anthropic/claude-haiku-4-5-20251001"
	llmSystem       = `You are a strict verifier. Answer in strict JSON: {"satisfied": true|false, "rationale": "short reason"}. No prose outside the JSON.`
	llmMissingKey   = "litellm.AuthenticationError: Missing Anthropic API Key - A call is being made to anthropic but no key is set either in the environment variables or via params. Please set `ANTHROPIC_API_KEY` or `ANTHROPIC_AUTH_TOKEN` in your environment vars"
)

// llmResult is llm_check.LLMCheckResult.
type llmResult struct {
	satisfied bool
	reason    string
	errored   bool
}

// llmFailure is an exception _invoke_model raises: its str().
type llmFailure struct{ text string }

func llmFail(text string) { panic(llmFailure{text}) }

// runLLMCheck is llm_check.run_llm_check(rule, payload).
func (r *runner) runLLMCheck(rule string, payload *pyjson.Object) (res llmResult) {
	defer func() {
		if p := recover(); p != nil {
			if f, ok := p.(llmFailure); ok {
				res = llmResult{reason: "error: llm_check call failed: " + f.text, errored: true}
				return
			}
			panic(p)
		}
	}()
	sat, why := r.invokeModel(rule, payload)
	return llmResult{satisfied: sat, reason: why}
}

func (r *runner) envGet(k string) (string, bool) {
	v, ok := r.env[k]
	return v, ok
}

// llmBackend is llm.resolve_backend().
func (r *runner) llmBackend() string {
	if v := r.env["OPENDAISUGI_LLM_BACKEND"]; v != "" {
		return v
	}
	var fromFile string
	if exc := catch(func() { fromFile = r.configuredBackend() }); exc == nil && fromFile != "" {
		return fromFile
	}
	if r.env["ANTHROPIC_API_KEY"] != "" || r.env["ANTHROPIC_AUTH_TOKEN"] != "" {
		return "litellm"
	}
	if r.which("claude") != "" {
		return "claude-code"
	}
	return "litellm"
}

// configuredBackend is config.configured_backend(DEFAULT_DATA_DIR /
// "config.yaml"): the stripped llm_backend, or "".
func (r *runner) configuredBackend() string {
	c := loadConfig(pathJoin(pathJoin(r.pathHome(), ".opendaisugi"), "config.yaml"))
	return pyStrip(c.llmBackend)
}

// llmUnportedEnv names what makes litellm or its HTTP client behave in a
// way the port does not model.
func (r *runner) llmUnportedEnv() string {
	for k, v := range r.env {
		u := strings.ToUpper(k)
		switch {
		case strings.HasPrefix(k, "LITELLM_"), k == "MINIMUM_CUSTOM_KEY_LENGTH":
			return k
		case v != "" && (u == "HTTP_PROXY" || u == "HTTPS_PROXY" || u == "ALL_PROXY" || u == "NO_PROXY"):
			return k
		case v != "" && (k == "SSL_CERT_FILE" || k == "SSL_CERT_DIR" || k == "REQUESTS_CA_BUNDLE" ||
			k == "CURL_CA_BUNDLE" || k == "SSL_VERIFY"):
			return k
		}
	}
	return ""
}

// invokeModel is llm_check._invoke_model on the litellm backend.
func (r *runner) invokeModel(rule string, payload *pyjson.Object) (bool, string) {
	model, ok := r.envGet("OPENDAISUGI_LLM_CHECK_MODEL")
	if !ok {
		model = llmDefaultModel
	}
	if r.llmBackend() == "claude-code" {
		unported("an llm_check through the claude-code backend, which runs claude -p")
	}
	if !strings.HasPrefix(model, "anthropic/") || len(model) == len("anthropic/") {
		unported("an llm_check model litellm does not send to Anthropic")
	}
	if k := r.llmUnportedEnv(); k != "" {
		unported("an llm_check under " + k + ", which changes how litellm calls the model")
	}
	pj := pystr.Slice(pyjson.Dumps(payload, true), 0, 4000)
	user := "Rule:\n" + rule + "\n\nPlan payload (JSON):\n" + pj + "\n\nDoes the plan payload satisfy the rule?"

	// litellm's key: ANTHROPIC_API_KEY when set at all (an empty one sends
	// no header), else ANTHROPIC_AUTH_TOKEN as a bearer token.
	header := http.Header{}
	key, hasKey := r.envGet("ANTHROPIC_API_KEY")
	token, hasToken := r.envGet("ANTHROPIC_AUTH_TOKEN")
	switch {
	case hasKey:
		if key != "" {
			header.Set("x-api-key", key)
		}
	case hasToken:
		if token != "" {
			header.Set("authorization", "Bearer "+token)
		}
	default:
		llmFail(llmMissingKey)
	}
	base := r.env["ANTHROPIC_API_BASE"]
	if base == "" {
		base = r.env["ANTHROPIC_BASE_URL"]
	}
	if base == "" {
		base = "https://api.anthropic.com/v1/messages"
	}
	if !strings.HasSuffix(base, "/v1/messages") {
		base += "/v1/messages"
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		unported("an llm_check URL the port does not send to")
	}
	body := pyjson.NewObject().
		Set("model", strings.TrimPrefix(model, "anthropic/")).
		Set("messages", []any{pyjson.NewObject().Set("role", "user").
			Set("content", []any{pyjson.NewObject().Set("type", "text").Set("text", user)})}).
		Set("temperature", 0).
		Set("max_tokens", 200).
		Set("system", []any{pyjson.NewObject().Set("type", "text").Set("text", llmSystem)})
	header.Set("user-agent", "litellm/"+litellmVersion)
	header.Set("anthropic-version", "2023-06-01")
	header.Set("accept", "application/json")
	header.Set("content-type", "application/json")
	status, text := r.llmPost(base, header, []byte(pyjson.Dumps(body, true)))
	content := llmContent(status, text)
	parsed, derr := pyjson.LoadsPy(content, llmJSONDepth)
	if derr != nil {
		if derr.TooDeep {
			unported("an llm_check reply nested past what json.loads reads")
		}
		llmFail(derr.Error())
	}
	o, isObj := parsed.(*pyjson.Object)
	if !isObj {
		llmFail("'" + pyTypeName(parsed) + "' object has no attribute 'get'")
	}
	sat, has := o.Get("satisfied")
	if !has {
		sat = false
	}
	why, has := o.Get("rationale")
	if !has {
		why = ""
	}
	return pyjson.Truthy(sat), pyStrOf(why)
}

// llmJSONDepth is well under the nesting where json.loads, in the
// verifier's thread, would raise RecursionError; deeper is undecided.
const llmJSONDepth = 900

// llmPost sends the request as litellm's HTTP client does: no redirects
// followed, litellm's own long timeout (the gate's --verify-timeout ends
// the wait first).
func (r *runner) llmPost(target string, header http.Header, body []byte) (int, string) {
	req, err := http.NewRequest("POST", target, bytes.NewReader(body))
	if err != nil {
		unported("an llm_check request the port cannot build")
	}
	req.Header = header
	client := &http.Client{
		Timeout:       600 * time.Second,
		Transport:     &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			llmFail("litellm.InternalServerError: AnthropicException - [Errno 111] Connection refused. " +
				"Handle with `litellm.InternalServerError`.")
		}
		unported("an llm_check call that failed at the network level (" + err.Error() + ")")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		unported("an llm_check reply that could not be read")
	}
	// httpx decodes the reply as UTF-8 unless its content type names
	// another charset, and json.loads of the bytes guesses UTF-16 or 32
	// from NULs and strips a BOM: all of that is left undecided.
	ct := strings.ToLower(resp.Header.Get("content-type"))
	if (strings.Contains(ct, "charset=") && !strings.Contains(ct, "charset=utf-8")) ||
		!utf8.Valid(raw) || bytes.HasPrefix(raw, []byte("\xef\xbb\xbf")) || bytes.IndexByte(raw, 0) >= 0 ||
		resp.Header.Get("content-encoding") != "" {
		unported("an llm_check reply in an encoding the port does not read")
	}
	return resp.StatusCode, string(raw)
}

var (
	llmTimeoutPhrases = []string{"Request Timeout Error", "Request timed out", "Timed out generating response",
		"The read operation timed out"}
	llmContextPhrases = []string{"exceed context limit", "this model's maximum context length is",
		"string too long. expected a string with maximum length", "model's maximum context limit",
		"is longer than the model's context length", "input tokens exceed the configured limit",
		"`inputs` tokens + `max_new_tokens` must be", "exceeds the available context size",
		"exceeds the maximum number of tokens allowed"}
)

// redactSecrets is litellm's redact_string.
func redactSecrets(s string) string {
	re, err := pyre.Compile(litellmSecretPattern, litellmSecretFlags)
	if err != nil {
		panic("gate: litellm secret pattern: " + err.Error())
	}
	return pyreSub(re, s, "REDACTED")
}

// contextWindowExceeded is ExceptionCheckers.is_error_str_context_window_exceeded.
func contextWindowExceeded(s string) bool {
	low := pystr.Lower(s)
	if strings.Contains(low, "string_above_max_length") {
		return false
	}
	if strings.Contains(low, "invalid 'user'") && strings.Contains(low, "string too long") {
		return false
	}
	for _, p := range llmContextPhrases {
		if strings.Contains(low, p) {
			return true
		}
	}
	if strings.Contains(low, "current length is") && strings.Contains(low, "while limit is") {
		return true
	}
	return strings.Contains(low, "maximum input length is") && strings.Contains(low, "tokens")
}

// llmMapped is exception_type's anthropic branch for an error text: the
// str() of the exception litellm raises, or "" when no rule matched.
func llmMapped(errStr string, status int) string {
	for _, p := range llmTimeoutPhrases {
		if strings.Contains(errStr, p) {
			return "litellm.Timeout: APITimeoutError - Request timed out. Error_str: " + errStr
		}
	}
	switch {
	case strings.Contains(errStr, "prompt is too long") || strings.Contains(errStr, "prompt: length") ||
		contextWindowExceeded(errStr):
		return "litellm.ContextWindowExceededError: litellm.BadRequestError: AnthropicError - " + errStr
	case strings.Contains(errStr, "overloaded_error") || strings.Contains(errStr, "Overloaded"):
		return "litellm.InternalServerError: AnthropicError - " + errStr
	case strings.Contains(errStr, "Invalid API Key"):
		return "litellm.AuthenticationError: AnthropicError - " + errStr
	case strings.Contains(errStr, "content filtering policy"):
		return "litellm.BadRequestError: litellm.ContentPolicyViolationError: AnthropicError - " + errStr
	case strings.Contains(errStr, "Client error '400 Bad Request'"):
		return "litellm.BadRequestError: AnthropicError - " + errStr
	}
	e := "AnthropicException - " + errStr
	switch status {
	case 0:
		return ""
	case 401:
		return "litellm.AuthenticationError: " + e
	case 403:
		return "litellm.PermissionDeniedError: " + e
	case 400, 413:
		return "litellm.BadRequestError: " + e
	case 404:
		return "litellm.NotFoundError: " + e
	case 408:
		return "litellm.Timeout: " + e
	case 429:
		return "litellm.RateLimitError: " + e
	case 500, 529:
		return "litellm.InternalServerError: " + e + ". Handle with `litellm.InternalServerError`."
	case 502:
		return "litellm.BadGatewayError: AnthropicException BadGatewayError - " + errStr
	case 503:
		return "litellm.ServiceUnavailableError: " + e + ". Handle with `litellm.ServiceUnavailableError`."
	case 504:
		return "litellm.Timeout: AnthropicException Timeout - " + errStr
	}
	switch {
	case status >= 400 && status < 500:
		return "litellm.BadRequestError: " + e
	case status >= 500 && status < 600:
		return "litellm.APIError: " + e
	case status >= 300 && status < 400:
		return "litellm.APIConnectionError: " + e
	}
	return ""
}

// llmContent reads the reply as litellm does: an error status raises, a
// 200 is a message whose text blocks, joined, are the content.
func llmContent(status int, text string) string {
	if status != 200 {
		msg := llmMapped(redactSecrets(text), status)
		if msg == "" {
			unported("an llm_check reply with status " + strconv.Itoa(status))
		}
		llmFail(msg)
	}
	v, derr := pyjson.LoadsPy(text, llmJSONDepth)
	if derr != nil {
		if derr.TooDeep || derr.NotJSON {
			unported("an llm_check reply litellm reads another way")
		}
		errStr := redactSecrets("Unable to get json response - " + derr.Error() + ", Original Response: " + text)
		if llmMapped(errStr, 0) != "" {
			unported("an llm_check reply whose error text litellm maps another way")
		}
		llmFail("litellm.APIConnectionError: AnthropicException - " + errStr)
	}
	o, ok := v.(*pyjson.Object)
	if !ok {
		unported("an llm_check reply that is not a message")
	}
	if _, isErr := o.Get("error"); isErr {
		unported("an llm_check reply that carries an error")
	}
	for _, k := range []string{"model", "stop_reason", "usage"} {
		if _, has := o.Get(k); !has {
			unported("an llm_check reply with no " + k)
		}
	}
	usage, ok := o.Value("usage").(*pyjson.Object)
	if !ok {
		unported("an llm_check reply whose usage is not an object")
	}
	for _, k := range []string{"input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"} {
		if x, has := usage.Get(k); has {
			if _, isInt := x.(pyjson.Int); !isInt {
				unported("an llm_check reply with a usage count that is not an int")
			}
		}
	}
	blocks, ok := o.Value("content").([]any)
	if !ok {
		unported("an llm_check reply whose content is not a list")
	}
	var b strings.Builder
	for _, x := range blocks {
		blk, ok := x.(*pyjson.Object)
		if !ok {
			unported("an llm_check reply block that is not an object")
		}
		switch blk.Value("type") {
		case "text":
			t, ok := blk.Value("text").(string)
			if !ok {
				unported("an llm_check text block with no text")
			}
			b.WriteString(t)
		case "thinking", "redacted_thinking":
		default:
			unported("an llm_check reply block of a kind the port does not read")
		}
	}
	return b.String()
}

// pyreSub is re.sub(pattern, repl, s) with a plain replacement.
func pyreSub(re *pyre.Regexp, s, repl string) string {
	rs := pystr.Runes(s)
	var b strings.Builder
	last, pos := 0, 0
	mustAdvance := false
	for pos <= len(rs) {
		start, end, _, ok := re.SearchGroups(rs, pos, mustAdvance)
		if !ok {
			break
		}
		b.WriteString(pystr.FromRunes(rs[last:start]))
		b.WriteString(repl)
		last = end
		mustAdvance = end == start
		pos = end
	}
	b.WriteString(pystr.FromRunes(rs[last:]))
	return b.String()
}

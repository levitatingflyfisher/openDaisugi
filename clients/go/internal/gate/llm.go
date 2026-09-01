package gate

import (
	"errors"

	"daisugi-verify/internal/llm"
	"daisugi-verify/internal/netproxy"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// This file is llm_check.run_llm_check on the HTTP backend, the model
// call an llm_check predicate makes: one plain call through the model
// client (internal/llm, the oracle's llm_client.py), the verdict read from
// the reply, and every failure worded as the oracle words it, since the
// failure text becomes the deny reason (LLM-12). A call through the
// claude-code backend (`claude -p`), or under settings the port does not
// read the oracle's way, is denied undecided.

const (
	llmDefaultModel = "anthropic/claude-haiku-4-5-20251001"
	llmSystem       = `You are a strict verifier. Answer in strict JSON: {"satisfied": true|false, "rationale": "short reason"}. No prose outside the JSON.`
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
		return "api"
	}
	if r.which("claude") != "" {
		return "claude-code"
	}
	return "api"
}

// configuredBackend is config.configured_backend(DEFAULT_DATA_DIR /
// "config.yaml"): the stripped llm_backend, or "".
func (r *runner) configuredBackend() string {
	c := loadConfig(pathJoin(pathJoin(r.pathHome(), ".opendaisugi"), "config.yaml"))
	return pyStrip(c.llmBackend)
}

// llmUnportedEnv names a setting that makes httpx behave in a way the
// port does not model.
func (r *runner) llmUnportedEnv() string {
	for _, k := range []string{"SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if r.env[k] != "" {
			return k
		}
	}
	return ""
}

// invokeModel is llm_check._invoke_model on the HTTP backend.
func (r *runner) invokeModel(rule string, payload *pyjson.Object) (bool, string) {
	model, ok := r.envGet("OPENDAISUGI_LLM_CHECK_MODEL")
	if !ok {
		model = llmDefaultModel
	}
	backend := r.llmBackend()
	if llm.Renamed(backend) {
		// resolve_backend raises on the old name: the check fails closed.
		llmFail(llm.RenamedText)
	}
	if backend == "claude-code" {
		unported("an llm_check through the claude-code backend, which runs claude -p")
	}
	if k := r.llmUnportedEnv(); k != "" {
		unported("an llm_check under " + k + ", which changes how httpx calls the model")
	}
	pj := pystr.Slice(pyjson.Dumps(payload, true), 0, 4000)
	user := "Rule:\n" + rule + "\n\nPlan payload (JSON):\n" + pj + "\n\nDoes the plan payload satisfy the rule?"
	w, e := llm.ResolveWire(model, "", "", r.envGet)
	if e != nil {
		llmFail(e.Msg)
	}
	zero := 0
	body := llm.Body(w, []llm.Message{{Role: "system", Content: llmSystem}, {Role: "user", Content: user}},
		llm.BodyOpts{MaxTokens: 200, Temperature: &zero})
	proxies := netproxy.HttpxFromVars(netproxy.FromMap(r.env), false)
	status, text, err := llm.Post(w, body, llm.Timeout(r.envGet), proxies)
	if err != nil {
		var le *llm.Error
		if errors.As(err, &le) {
			llmFail(le.Msg)
		}
		unported("an llm_check " + err.Error())
	}
	reply, e := llm.ReadReply(w, status, text)
	if e != nil {
		llmFail(e.Msg)
	}
	parsed, derr := pyjson.LoadsPy(reply.Text, llmJSONDepth)
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

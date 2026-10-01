package gate

import (
	"errors"

	"daisugi-verify/internal/llm"
	"daisugi-verify/internal/llmcheck"
	"daisugi-verify/internal/pyjson"
)

// This file is llm_check.run_llm_check in the gate: the model call an
// llm_check predicate makes goes through internal/llmcheck, on the
// claude-code backend (`claude -p`) or the HTTP backend, and every failure
// is worded as the oracle words it, since the failure text becomes the
// deny reason (LLM-12). A call under settings the port does not read the
// oracle's way is denied undecided.

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
	c := loadConfig(pathJoin(r.dataHome(), "config.yaml"))
	return pyStrip(c.llmBackend)
}

// invokeModel is llm_check._invoke_model, through the shared
// internal/llmcheck: a call it does not make the oracle's way is denied
// undecided, and an exception it raises fails the check closed.
func (r *runner) invokeModel(rule string, payload *pyjson.Object) (bool, string) {
	claude := llm.New(llm.Env{Getenv: r.envGet, Home: r.home, LookPath: func(name string) (string, error) {
		if p := r.which(name); p != "" {
			return p, nil
		}
		return "", errors.New("not found")
	}})
	o := llmcheck.Invoke(llmcheck.Env{Getenv: r.envGet, Vars: r.env, Backend: r.llmBackend(), Claude: claude},
		rule, pyjson.Dumps(payload, true))
	if o.Unported != "" {
		unported(o.Unported)
	}
	if o.Raised {
		llmFail(o.Failure)
	}
	return o.Satisfied, o.Reason
}

// Package llmcheck is the oracle's llm_check.run_llm_check: the model call
// an llm_check predicate makes, with the verdict read from the reply and
// every failure worded as the oracle words it.
//
//   - claude-code: `claude -p --model=haiku` with the prompt on stdin, the
//     first JSON object of its stdout read as the verdict. A failed run is
//     a plain "not satisfied" whose reason starts "llm-check failed: ", as
//     the oracle catches EnvelopeGenerationError there.
//   - the HTTP backend (named api): one plain call through the model
//     client (internal/llm, the oracle's llm_client.py). Any failure is an
//     errored result, which the evaluator turns into an evaluation error.
//
// The gate (internal/gate) and the verifier the commands share
// (internal/verify) both call it, so there is one copy.
package llmcheck

import (
	"errors"

	"daisugi-verify/internal/llm"
	"daisugi-verify/internal/netproxy"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

const (
	// DefaultModel is the model the HTTP backend asks when
	// OPENDAISUGI_LLM_CHECK_MODEL is not set.
	DefaultModel = "anthropic/claude-haiku-4-5-20251001"
	// System is the verifier's system prompt.
	System = `You are a strict verifier. Answer in strict JSON: {"satisfied": true|false, "rationale": "short reason"}. No prose outside the JSON.`
	// jsonDepth is well under the nesting where json.loads, in the
	// verifier's thread, would raise RecursionError; deeper is undecided.
	jsonDepth = 900
	// claudeTimeout is the timeout_s _invoke_model gives claude -p.
	claudeTimeout = 60.0
)

// Env is the world one check runs in.
type Env struct {
	Getenv func(string) (string, bool)
	// Vars is the environment, for the proxies httpx reads.
	Vars map[string]string
	// Backend is resolve_backend() as the caller reads it.
	Backend string
	// Claude runs `claude -p` for the claude-code backend.
	Claude *llm.Client
}

// Outcome is what _invoke_model gives: a verdict, or the text of the
// exception it raised, or a call this binary does not make the oracle's
// way.
type Outcome struct {
	Satisfied bool
	Reason    string
	// Raised is true when _invoke_model raised; Failure is the
	// exception's str().
	Raised  bool
	Failure string
	// Unported names a call this binary does not make the oracle's way;
	// the caller treats the check as undecided.
	Unported string
}

// Result is llm_check.LLMCheckResult.
type Result struct {
	Satisfied bool
	Reason    string
	Errored   bool
}

// Run is run_llm_check(rule, payload): an exception becomes an errored
// result. payload is json.dumps(payload, default=str). The second value
// names a call this binary does not make the oracle's way, or is "".
func Run(e Env, rule, payload string) (Result, string) {
	o := Invoke(e, rule, payload)
	if o.Unported != "" {
		return Result{}, o.Unported
	}
	if o.Raised {
		return Result{Reason: "error: llm_check call failed: " + o.Failure, Errored: true}, ""
	}
	return Result{Satisfied: o.Satisfied, Reason: o.Reason}, ""
}

func raised(text string) Outcome { return Outcome{Raised: true, Failure: text} }

func unported(why string) Outcome { return Outcome{Unported: why} }

// User is the user prompt: the rule and the payload, cut to 4000 code
// points.
func User(rule, payload string) string {
	return "Rule:\n" + rule + "\n\nPlan payload (JSON):\n" + pystr.Slice(payload, 0, 4000) +
		"\n\nDoes the plan payload satisfy the rule?"
}

// Invoke is llm_check._invoke_model.
func Invoke(e Env, rule, payload string) Outcome {
	if llm.Renamed(e.Backend) {
		// resolve_backend raises on the old name: the check fails closed.
		return raised(llm.RenamedText)
	}
	user := User(rule, payload)
	if e.Backend == "claude-code" {
		return claudeCode(e, user)
	}
	model, ok := e.Getenv("OPENDAISUGI_LLM_CHECK_MODEL")
	if !ok {
		model = DefaultModel
	}
	for _, k := range []string{"SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if v, _ := e.Getenv(k); v != "" {
			return unported("an llm_check under " + k + ", which changes how httpx calls the model")
		}
	}
	w, le := llm.ResolveWire(model, "", "", e.Getenv)
	if le != nil {
		return raised(le.Msg)
	}
	zero := 0
	body := llm.Body(w, []llm.Message{{Role: "system", Content: System}, {Role: "user", Content: user}},
		llm.BodyOpts{MaxTokens: 200, Temperature: &zero})
	proxies := netproxy.HttpxFromVars(netproxy.OrderedFromMap(e.Vars))
	if r := proxies.Refusal(); r != nil && r.Invalid != "" {
		// httpx.InvalidURL escapes llm_check's except clause: how the
		// oracle's gate then answers is not modelled.
		return unported("an llm_check under a proxy setting httpx rejects (" + r.Invalid + ")")
	}
	status, text, err := llm.Post(w, body, llm.Timeout(e.Getenv), proxies)
	if err != nil {
		var me *llm.Error
		if errors.As(err, &me) {
			return raised(me.Msg)
		}
		return unported("an llm_check " + err.Error())
	}
	reply, le := llm.ReadReply(w, status, text)
	if le != nil {
		return raised(le.Msg)
	}
	parsed, derr := pyjson.LoadsPy(reply.Text, jsonDepth)
	if derr != nil {
		if derr.TooDeep {
			return unported("an llm_check reply nested past what json.loads reads")
		}
		return raised(derr.Error())
	}
	o, isObj := parsed.(*pyjson.Object)
	if !isObj {
		return raised("'" + pmodel.TypeName(parsed) + "' object has no attribute 'get'")
	}
	return verdict(o)
}

// claudeCode is _invoke_model's claude-code branch:
// call_claude_p_json_sync(prompt, timeout_s=60.0, model="haiku"), where an
// EnvelopeGenerationError is a verdict of not satisfied.
func claudeCode(e Env, user string) Outcome {
	prompt := "[system]\n" + System + "\n\n[user]\n" + user
	stdout, err := e.Claude.Sync(prompt, "haiku", claudeTimeout)
	if err == nil {
		var o *pyjson.Object
		o, err = llm.FirstJSONObject(stdout)
		if err == nil {
			return verdict(o)
		}
	}
	var me *llm.Error
	switch {
	case errors.As(err, &me):
		return Outcome{Reason: "llm-check failed: " + me.Msg}
	case errors.Is(err, llm.ErrUnsupported):
		return unported("an llm_check " + err.Error())
	}
	return raised(err.Error())
}

// verdict is (bool(parsed.get("satisfied", False)),
// str(parsed.get("rationale", ""))).
func verdict(o *pyjson.Object) Outcome {
	sat, has := o.Get("satisfied")
	if !has {
		sat = false
	}
	why, has := o.Get("rationale")
	if !has {
		why = ""
	}
	s, isStr := why.(string)
	if !isStr {
		s = pmodel.Repr(why)
	}
	return Outcome{Satisfied: pyjson.Truthy(sat), Reason: s}
}

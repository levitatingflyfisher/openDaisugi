package envgen

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"daisugi-verify/internal/llm"

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/verify"
)

func envOf(t *testing.T, text string) *pyjson.Object {
	t.Helper()
	v, err := pmodel.ValidateJSON("Envelope", pmodel.Envelope, text)
	if err != nil {
		t.Fatal(err.String())
	}
	return v.(*pyjson.Object)
}

const okEnv = `{"generated_by": "m", "task": "t", "permissions": {"file_read": ["/w/**"]}}`

func TestSelfConsistencyUnknownRejectsTheEnvelope(t *testing.T) {
	if ok, _ := selfConsistent(envOf(t, okEnv)); !ok {
		t.Fatal("a consistent envelope must pass")
	}
	old := checkSelfConsistency
	defer func() { checkSelfConsistency = old }()
	checkSelfConsistency = func(verify.Envelope, int) ([]verify.Violation, string) {
		return nil, "Z3 self-consistency check exceeded 500ms"
	}
	ok, why := selfConsistent(envOf(t, okEnv))
	if ok || why != "Z3 self-consistency check exceeded 500ms" {
		t.Fatalf("an unknown answer must fail closed, got %v %q", ok, why)
	}
}

func TestInconsistentEnvelopes(t *testing.T) {
	for _, text := range []string{
		`{"generated_by": "m", "task": "t", "permissions": {"shell_allowlist": ["ls"]}}`,
		`{"generated_by": "m", "task": "t", "permissions": {"max_execution_time_s": 0}}`,
		`{"generated_by": "m", "task": "t", "permissions": {"max_execution_time_s": 3601}}`,
		`{"generated_by": "m", "task": "t", "permissions": {}, "postconditions": [{"type": "file_exists", "path": "/x"}]}`,
	} {
		if ok, why := selfConsistent(envOf(t, text)); ok || why != "Envelope is internally inconsistent" {
			t.Errorf("%s: %v %q", text, ok, why)
		}
	}
}

func TestCacheKeyMatchesTheOracle(t *testing.T) {
	// make_cache_key(task="t", context=None, model="m", parent_envelope_id=None,
	// summarize=False, thinking_budget="standard")
	got := CacheKey(KeyArgs{Task: "t", Model: "m", Thinking: "standard"})
	if len(got) != 64 {
		t.Fatal(got)
	}
	name := "x"
	if CacheKey(KeyArgs{Task: "t", Model: "m", Thinking: "standard", Tier1: &name}) == got {
		t.Fatal("the tier1 name must change the key")
	}
}

func TestThinkingOpts(t *testing.T) {
	cases := []struct {
		model, budget, effort string
		thinking              int
	}{
		{"anthropic/claude-x", "deep", "", 16000},
		{"Claude-3", "deep", "", 16000},
		{"claude3", "deep", "", 0},
		{"anthropic/x", "light", "", 0},
		{"openai/o3-mini", "light", "low", 0},
		{"o4-mini", "deep", "high", 0},
		{"openai/gpt-4o", "deep", "", 0},
	}
	for _, c := range cases {
		th, ef := thinkingOpts(c.model, c.budget)
		if th != c.thinking || ef != c.effort {
			t.Errorf("%s %s: %d %q", c.model, c.budget, th, ef)
		}
	}
}

func TestHintsKeepTheNewestAndTheFirstStage(t *testing.T) {
	rec := func(ts float64, vs ...[2]string) *pyjson.Object {
		l := []any{}
		for _, v := range vs {
			l = append(l, pyjson.NewObject().Set("stage", v[0]).Set("message", v[1]))
		}
		return pyjson.NewObject().Set("timestamp", ts).Set("violations", l)
	}
	got := hintsBlock([]*pyjson.Object{rec(1, [2]string{"a", "m1"}), rec(2, [2]string{"b", "m2"}, [2]string{"c", "m1"})})
	want := "\n## Prior Rejections\n\nPrevious plans verified against envelopes for this task were rejected.\n" +
		"Generate an envelope that prevents these violations:\n\n- [a] m1\n- [b] m2"
	if got != want {
		t.Fatalf("%q", got)
	}
	if hintsBlock(nil) != "" {
		t.Fatal("no records, no block")
	}
}

func TestTier1DeadlineBoundsTheWholeCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(500)
	}))
	defer srv.Close()
	env := map[string]string{"OPENDAISUGI_LLM_BACKEND": "api"}
	c := llm.New(llm.Env{Getenv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }, Home: t.TempDir()})
	base := srv.URL + "/v1"
	t1 := NewTier1("local", &base, nil, "")
	if t1.Model != "openai/local" || t1.Name != "http:openai/local" {
		t.Fatalf("%+v", t1)
	}
	t1.Timeout = 200 * time.Millisecond
	start := time.Now()
	if got := t1.Generate(c, "t", nil); got != nil {
		t.Fatal("a call past its deadline must decline")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("the deadline did not end the call: %v", d)
	}
}

const planJSON = `{"id": "plan_1", "source": "script", "task": "t", "steps": [
 {"id": "s1", "type": "shell", "command": "make test", "depends_on": []},
 {"id": "s2", "type": "file_read", "path": "/work/a.txt", "depends_on": ["s1"]}]}`

func timeoutVerify(verify.ActionPlan, verify.Envelope, verify.VerifyOptions) verify.VerifyResultGo {
	return verify.VerifyResultGo{OK: true, Timeouts: []string{"Z3 plan check exceeded 500ms"}}
}

func callerEnv(t *testing.T) verify.Envelope {
	t.Helper()
	e, err := verify.ParseEnvelope([]byte(`{"id": "env_1", "generated_by": "t", "task": "t", "permissions":
		{"shell": true, "shell_allowlist": ["make"], "file_read": ["/work/**"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// PlanVerifies is the check recall makes on every plan it would serve, the
// template included: a Z3 unknown there is a miss (K1-3).
func TestPlanVerifiesFailsClosedOnAZ3Unknown(t *testing.T) {
	plan, _ := pyjson.LoadsPy(planJSON, 900)
	env := callerEnv(t)
	if !PlanVerifies(plan.(*pyjson.Object), env, 500) {
		t.Fatal("the plan verifies against the caller's envelope")
	}
	old := verifyPlan
	defer func() { verifyPlan = old }()
	verifyPlan = timeoutVerify
	if PlanVerifies(plan.(*pyjson.Object), env, 500) {
		t.Fatal("a verify that kept a Z3 timeout must not pass")
	}
}

func TestBindGivesTheTemplateOnAZ3Unknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"content": [{"type": "text", "text": "{\"values\": {\"path\": \"/work/b.txt\"}}"}], "stop_reason": "end_turn"}`))
	}))
	defer srv.Close()
	env := map[string]string{"OPENDAISUGI_LLM_BACKEND": "api", "ANTHROPIC_API_KEY": "sk-test-0000000000000000",
		"ANTHROPIC_API_BASE": srv.URL}
	c := llm.New(llm.Env{Getenv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }, Home: t.TempDir()})
	plan, _ := pyjson.LoadsPy(planJSON, 900)
	pw := pyjson.NewObject().Set("plan_template", plan).Set("parameters", []any{pyjson.NewObject().
		Set("name", "path").Set("step_index", pyjson.Int{Text: "1"}).Set("step_id", "s2").Set("field", "path").
		Set("head", "/work").Set("observed", []any{"/work/a.txt"})})
	path := func(p *pyjson.Object) any { return listOf(p.Value("steps"))[1].(*pyjson.Object).Value("path") }
	if got := Bind(c, pw, "read b", callerEnv(t), DefaultModel, 500); path(got) != "/work/b.txt" {
		t.Fatalf("the binding should hold: %v", path(got))
	}
	old := verifyPlan
	defer func() { verifyPlan = old }()
	verifyPlan = timeoutVerify
	if got := Bind(c, pw, "read b", callerEnv(t), DefaultModel, 500); path(got) != "/work/a.txt" {
		t.Fatalf("a Z3 unknown must give the frozen template: %v", path(got))
	}
}

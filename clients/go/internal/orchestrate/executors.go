package orchestrate

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"daisugi-verify/internal/llm"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/supervise"
)

// taskPrompt is orchestrator._task_step_prompt.
func taskPrompt(step *pyjson.Object) string {
	if p, _ := step.Value("prompt").(string); p != "" {
		return p + "\n\nComplete this subtask and respond with a direct, complete answer."
	}
	return pathways.DumpJSON(step)
}

// TaskExecutor is orchestrator.BudgetAwareDelegatingExecutor over
// DelegatingExecutor(json_mode=False): each task step sized against what
// the budget has left, asked of its model with up to two retries, and its
// spend recorded.
type TaskExecutor struct {
	LLM     *llm.Client
	Tracker *Tracker
	Ladder  Ladder
	mu      sync.Mutex
	// Live is the realized sizing of each step run, in run order.
	Live []Sizing
}

const maxRetries = 2

func (t *TaskExecutor) Run(step *pyjson.Object, timeoutS, maxOut int) (supervise.ExecResult, error) {
	pm, _ := step.Value("preferred_model").(string)
	sz := SizeStep(step, t.Ladder, t.Tracker, pm)
	t.mu.Lock()
	t.Live = append(t.Live, sz)
	t.mu.Unlock()
	if t.Tracker.Strict && !sz.Affordable {
		return supervise.ExecResult{RC: 1, Stdout: fmt.Sprintf("budget exhausted: cannot afford step '%s' at any tier",
			str(step, "id"))}, nil
	}
	sized := pyjson.NewObject()
	for _, k := range step.Keys() {
		sized.Set(k, step.Value(k))
	}
	sized.Set("preferred_model", sz.Model)
	res := t.delegate(sized, timeoutS, maxOut)
	tokens := sz.EstTokens
	if res.Tokens != nil {
		tokens = *res.Tokens
	}
	model := sz.Model
	if res.Model != nil {
		model = *res.Model
	}
	if err := t.Tracker.Record(str(step, "id"), model, tokens, res.CostUSD); err != nil {
		var over *ErrBudgetExceeded
		if !errors.As(err, &over) {
			return supervise.ExecResult{}, err
		}
	}
	return res, nil
}

func str(o *pyjson.Object, k string) string {
	s, _ := o.Value(k).(string)
	return s
}

// delegate is DelegatingExecutor.run.
func (t *TaskExecutor) delegate(step *pyjson.Object, timeoutS, maxOut int) supervise.ExecResult {
	return Delegate(t.LLM, step, taskPrompt(step), timeoutS, maxOut)
}

// TaskPrompt is orchestrator._task_step_prompt.
func TaskPrompt(step *pyjson.Object) string { return taskPrompt(step) }

// Delegate is DelegatingExecutor(json_mode=False).run with the prompt its
// template gave: the step's preferred model, up to two retries.
func Delegate(c *llm.Client, step *pyjson.Object, prompt string, timeoutS, maxOut int) supervise.ExecResult {
	maxTokens := max(256, maxOut/4)
	started := time.Now()
	model := str(step, "preferred_model")
	if model == "" {
		model = "haiku"
	}
	claude := c.Backend() == "claude-code"
	var lastErr string
	var tokens *int64
	var cost *float64
	for attempt := 1; attempt <= maxRetries+1; attempt++ {
		tokens, cost = nil, nil
		var content string
		var err error
		if claude {
			content, tokens, cost, err = c.Metered(prompt, model, float64(timeoutS))
		} else {
			var r *llm.Reply
			r, err = c.CompleteTimeout(model, []llm.Message{{Role: "user", Content: prompt}},
				llm.BodyOpts{MaxTokens: maxTokens}, float64(timeoutS))
			if err == nil {
				content, tokens = r.Text, r.Tokens
			}
		}
		if err != nil {
			lastErr = err.Error()
			if lastErr == "" {
				lastErr = "Exception"
			}
			continue
		}
		return supervise.ExecResult{RC: 0, Stdout: content, DurationMs: msSince(started), Model: &model,
			Tokens: tokens, CostUSD: cost}
	}
	return supervise.ExecResult{RC: 1, Stdout: "delegating_executor: exhausted retries: " + lastErr,
		DurationMs: msSince(started), Model: &model, Tokens: tokens, CostUSD: cost}
}

func msSince(t time.Time) float64 { return float64(time.Since(t).Nanoseconds()) / 1e6 }

// SkillExecutor is orchestration_executors.SkillExecutor with no handler
// registered: every skill step fails.
type SkillExecutor struct{}

func (SkillExecutor) Run(step *pyjson.Object, _, _ int) (supervise.ExecResult, error) {
	return supervise.ExecResult{RC: 1, Stdout: "no handler registered for skill " + pystr.Repr(str(step, "skill_id"))}, nil
}

// MCPExecutor is orchestration_executors.MCPExecutor with no transport.
type MCPExecutor struct{}

func (MCPExecutor) Run(*pyjson.Object, int, int) (supervise.ExecResult, error) {
	return supervise.ExecResult{RC: 1, Stdout: "no MCP transport configured; pass MCPExecutor(transport=...)"}, nil
}

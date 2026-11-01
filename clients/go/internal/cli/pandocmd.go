package cli

import (
	"bytes"
	"context"
	"daisugi-verify/internal/pandoskip"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"daisugi-verify/internal/agentprof"
	"daisugi-verify/internal/hermesdb"
	"daisugi-verify/internal/llm"
	"daisugi-verify/pando"
)

// pandoCmd is `daisugi pando`: Pando R2, the conversation memory, over the
// store under the data home (ADR-0023). The verbs live in package pando,
// which opens no socket; this file gives it the data home and the model
// behind tend.
func (e *Env) pandoCmd(args []string) error {
	env := pando.CLIEnv{
		Stdin: e.Stdin, Out: e.Stdout, Err: e.Stderr,
		DataHome: e.dataHome(), Home: e.env["HOME"],
		NewSummarizer: e.pandoSummarizer,
		Getenv:        func(k string) string { return e.env[k] },
		HermesSource: func(profile string) (pando.Source, error) {
			return &hermesdb.Source{Root: hermesdb.Root(e.env["HERMES_HOME"], e.env["HOME"]), Profile: profile}, nil
		},
	}
	if code := pando.Run(args, env); code != 0 {
		return exit(code)
	}
	return nil
}

// pandoSummarizer is the model behind `tend` and `import`. An empty profile
// is Claude Code through `claude -p` (haiku); any other name is an agent
// profile asked over HTTP.
func (e *Env) pandoSummarizer(profile string) (pando.Summarizer, error) {
	m := &pando.ModelSummarizer{Timeout: pando.CompactTimeout}
	if profile == "" || profile == agentprof.Default {
		m.Call = func(ctx context.Context, system, prompt string) (string, pando.CallUsage, error) {
			return e.claudeCall(ctx, "haiku", system, prompt)
		}
		return m, nil
	}
	profiles := agentprof.Resolve(agentprof.ConfiguredTable(e.dataHome()), e.lookup)
	p, why := profiles.Lookup(&profile)
	if p == nil {
		return nil, errors.New(why)
	}
	model, base, key := "", "", ""
	if p.Model != nil {
		model = *p.Model
	}
	if p.BaseURL != nil {
		base = *p.BaseURL
	}
	if p.Key != nil {
		key = *p.Key
	}
	maxTokens := pando.DefaultMaxTokens
	if p.MaxTokens != nil {
		maxTokens = *p.MaxTokens
	}
	opts := llm.BodyOpts{MaxTokens: maxTokens}
	if p.Thinking != nil {
		opts = thinkingOpts(*p.Thinking, p.Wire, maxTokens)
	}
	// timeout_s caps the call; the HTTP client waits at least that long.
	httpTimeout := llm.Timeout(e.lookup)
	if p.TimeoutS != nil {
		m.Timeout = time.Duration(*p.TimeoutS) * time.Second
		httpTimeout = max(httpTimeout, float64(*p.TimeoutS))
	}
	client := e.llmClient()
	m.Call = func(ctx context.Context, system, prompt string) (string, pando.CallUsage, error) {
		msgs := []llm.Message{{Role: "system", Content: system}, {Role: "user", Content: prompt}}
		reply, err := client.CompleteWith(model, base, key, msgs, opts, httpTimeout)
		if err != nil {
			var le *llm.Error
			if errors.As(err, &le) {
				return "", pando.CallUsage{}, &pando.CallError{Msg: le.Msg, Detail: le.Msg, Ran: true}
			}
			return "", pando.CallUsage{}, err
		}
		// A reply with no text that stopped for length spent all its tokens
		// on reasoning. It is a call that gave no line: the compactor counts
		// it as a fallback. A separate reasoning field is never read.
		if strings.TrimSpace(reply.Text) == "" && reply.Cut {
			return "", pando.CallUsage{Stop: "length"}, nil
		}
		return reply.Text, pando.CallUsage{}, nil
	}
	return m, nil
}

// claudeCall asks Claude Code for one answer: `claude -p` in an empty dir,
// no tools, no saved session.
func (e *Env) claudeCall(ctx context.Context, model, system, prompt string) (string, pando.CallUsage, error) {
	bin, err := lookPath("claude", e.env["PATH"])
	if err != nil {
		return "", pando.CallUsage{}, errors.New("tend needs Claude Code (claude) on PATH, or --profile NAME of an agent profile")
	}
	dir, err := os.MkdirTemp("", "pando-tend-")
	if err != nil {
		return "", pando.CallUsage{}, err
	}
	defer os.RemoveAll(dir)
	cctx, cancel := context.WithTimeout(ctx, 2*pando.CompactTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, "-p", "--model", model, "--tools", "", "--no-session-persistence",
		"--system-prompt", system)
	cmd.Dir = dir
	cmd.Env = pandoskip.Environ(e.Environ)
	cmd.Stdin = strings.NewReader(prompt)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	start := time.Now()
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 400 {
			msg = msg[len(msg)-400:]
		}
		return "", pando.CallUsage{}, &pando.CallError{Msg: fmt.Sprintf("claude -p failed after %s: %v", time.Since(start).Round(time.Second), err),
			Detail: msg, Stderr: msg, Ran: true}
	}
	return strings.TrimSpace(out.String()), pando.CallUsage{}, nil
}

// thinkingOpts is the request options for a profile's thinking value. On
// the openai wire it is reasoning_effort (off is "none"). On the anthropic
// wire off sends nothing and the others turn on extended thinking with a
// token budget; max_tokens grows by the budget so the summary keeps the
// tokens the profile gave it.
func thinkingOpts(level, wire string, maxTokens int) llm.BodyOpts {
	o := llm.BodyOpts{MaxTokens: maxTokens}
	if wire == "anthropic" {
		if b := ThinkingBudgets[level]; b > 0 {
			o.ThinkingBudget = b
			o.MaxTokens = maxTokens + b
		}
		return o
	}
	if level == "off" {
		level = "none"
	}
	o.ReasoningEffort = level
	return o
}

// ThinkingBudgets are the extended-thinking token budgets of the
// anthropic wire (the Messages API needs at least 1024).
var ThinkingBudgets = map[string]int{"low": 1024, "medium": 4096, "high": 16000}

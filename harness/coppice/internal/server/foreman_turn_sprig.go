package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/opendaisugi/coppice/internal/foreman"
)

// sprigTurn runs a turn on an operator profile: the Anthropic API, any
// OpenAI-style server (a local one too, or one on another machine), or
// Claude Code itself, all through sprig. The model reaches the same four
// tools: sprig starts `coppice foreman tools` as its MCP server, and that
// relay dials the turn socket. The relay is sprig's child, so the socket
// serves it with no change. sprig runs with no built-in tool, and a call to
// a tool it did not list fails the run, so the hook of the claude runner has
// no work here.
type sprigTurn struct {
	s       *Server
	profile string
	model   string
	// self is the command that runs `coppice foreman`; see claudeTurn.
	self []string
}

// sprigEnv is the environment of every sprig that coppice starts, the turn
// and the compactor: an allowlist, built here and nowhere else. PATH, HOME,
// USER, LOGNAME, SHELL, LANG, LC_*, TZ, TERM, TMPDIR and XDG_*; nothing
// else. No key variable, no proxy, no base URL and no model name reaches
// sprig, its model call or its MCP relay from coppice's own environment. A
// profile names its base_url, and its key goes on descriptor 3.
func sprigEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if slices.Contains(sprigEnvNames, name) || strings.HasPrefix(name, "LC_") || strings.HasPrefix(name, "XDG_") || sprigEnvKeep(name) {
			out = append(out, kv)
		}
	}
	return out
}

var sprigEnvNames = []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "TZ", "TERM", "TMPDIR"}

// sprigEnvKeep names more variables to pass on. It keeps none; a test that
// runs fakes which read their script from the environment sets it.
var sprigEnvKeep = func(string) bool { return false }

// defaultBase is the base URL of a wire whose profile names none, the one
// agent steps use. The claude-code wire has none.
func defaultBase(wire string) string {
	switch wire {
	case "anthropic":
		return "https://api.anthropic.com"
	case "openai":
		return "https://api.openai.com/v1"
	}
	return ""
}

// turnMaxTurns is how many model calls one turn may make: a few tool
// calls and the answer.
const turnMaxTurns = 8

func (c *sprigTurn) Run(ctx context.Context, in TurnInput) (TurnResult, error) {
	table, err := c.s.operatorBundle(ctx)
	if err != nil {
		return TurnResult{}, err
	}
	e, ok := table[c.profile]
	if !ok {
		return TurnResult{}, fmt.Errorf("the turn profile %q is not in the operator's agent profiles (a key variable it names may be unset)", c.profile)
	}
	if e.Wire == "claude-code" {
		return TurnResult{}, fmt.Errorf("the turn profile %q uses the claude-code wire. Leave turn_profile empty to run the turn on Claude Code", c.profile)
	}
	model := c.model
	if model == "" && e.Model != nil {
		model = *e.Model
	}
	sysPath, err := writeSystemFile(in.Dir, in.N, string(in.System))
	if err != nil {
		return TurnResult{}, fmt.Errorf("cannot write the turn's system prompt: %w", err)
	}
	defer os.Remove(sysPath)

	args := []string{"--wire", e.Wire}
	base := defaultBase(e.Wire)
	if e.BaseURL != nil && *e.BaseURL != "" {
		base = *e.BaseURL
	}
	if base != "" {
		args = append(args, "--base-url", base)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	var key []byte
	if e.Key != nil && *e.Key != "" {
		key = []byte(*e.Key)
		defer clear(key)
		args = append(args, "--key-fd", "3")
	}
	args = append(args, "--tools", "none", "--mcp-config", mcpConfigJSON(selfCmd(c.self), in.Socket),
		"--system-file", sysPath, "--json", "--max-turns", strconv.Itoa(turnMaxTurns), "-")

	wrap, werr := c.s.fenceCmd("sprig", in.Dir, []string{in.Dir}, key != nil)
	if werr != nil {
		return TurnResult{}, werr
	}
	stdout, stderr, code, err := runCmd(ctx, cmdOpts{Dir: in.Dir, Name: sprigBin(), Args: args, Stdin: in.Stdin,
		FD3: key, HasFD3: key != nil, Env: sprigEnv(), Started: in.Started, Wrap: wrap})
	if ctx.Err() != nil {
		return TurnResult{}, ctx.Err()
	}
	var we *waitError
	if errors.As(err, &we) {
		return TurnResult{}, fmt.Errorf("sprig did not finish cleanly: %w", err)
	}
	if err != nil {
		return TurnResult{}, fmt.Errorf("cannot start sprig: %w", err)
	}
	if code != 0 {
		return TurnResult{}, &runnerError{
			lead:   "sprig ended with an error",
			msg:    "sprig ended with an error: " + firstLine(stderr, fmt.Errorf("exit status %d", code)),
			detail: failDetail(stderr, stdout),
		}
	}
	var out struct {
		Answer string         `json:"answer"`
		Usage  map[string]int `json:"usage"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		return TurnResult{}, errors.New("sprig gave an answer that is not JSON")
	}
	return TurnResult{Text: out.Answer, Usage: foreman.CallUsage{
		Input: out.Usage["input_tokens"], Output: out.Usage["output_tokens"],
		CacheRead: out.Usage["cache_read_input_tokens"], CacheWrite: out.Usage["cache_creation_input_tokens"],
	}}, nil
}

package supervise

import (
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	"daisugi-verify/internal/agentprof"
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/install"
	"daisugi-verify/internal/llm"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/tree"
)

// agenticToolCapabilities is verify._AGENTIC_TOOL_CAPABILITIES: the
// permission a host tool needs.
var agenticToolCapabilities = map[string]string{
	"Bash":      "shell",
	"Read":      "file_read",
	"Glob":      "file_read",
	"Grep":      "file_read",
	"Write":     "file_write",
	"Edit":      "file_write",
	"MultiEdit": "file_write",
	"WebFetch":  "network",
	"WebSearch": "network",
	"Spawn":     "mcp_allowlist",
}

// edgeTimeoutMs is tree.DEFAULT_TIMEOUT_MS, the edge proof's Z3 budget.
const edgeTimeoutMs = 2000

// Agentic is agentic_executor.AgenticExecutor: an agentic step run as
// `claude -p` or sprig in its workspace, under the call-time gate in
// enforce mode. Every step runs as a delegation tree (tree mode): a fresh
// private directory holds gate/ (the gate root) and tree/ (the ledger);
// its root is the caller's envelope with the budget Tokens and Turns, and
// the sub-agent is the root's child under agentic-<step id>, proved,
// reserved, started and ended by the one starter.
type Agentic struct {
	// Envelope is the caller's envelope, the authorization ceiling.
	Envelope *pyjson.Object
	// Model is the sub-agent's model when neither the step nor its
	// profile names one ("haiku").
	Model string
	// Claude runs `claude -p`.
	Claude *llm.Client
	// Self is this binary: the gate hook runs `<Self> gate check`, and
	// sprig's spawn tool `<Self> tree run`.
	Self string
	// TempDir is where the tree directory is made ($TMPDIR, as mkdtemp
	// reads it).
	TempDir string
	// Runtime is the sub-agent: "claude" (claude -p; "" means it too) or
	// "sprig".
	Runtime string
	// Sprig is the sprig binary (DAISUGI_SPRIG, else sprig on PATH).
	Sprig string
	// Environ is the children's environment; nil is this process's.
	Environ []string
	// LookPath finds Sprig as the run's PATH finds it; nil starts Sprig as
	// it is named.
	LookPath func(string) (string, error)
	// Profiles is the operator's agent profiles, each key read once.
	Profiles agentprof.Profiles
	// Tokens and Turns are the budget of each step's tree; nil is none.
	Tokens, Turns *big.Int
}

// sprigTools are the host tools sprig has a tool for, and its name for
// each. Any other requested tool is left out of sprig's wall.
var sprigTools = map[string]string{"Read": "read", "Write": "write", "Edit": "edit", "Bash": "bash",
	"Spawn": "spawn"}

// SprigSpaceText is the failed step when a word of sprig's gate command
// would hold whitespace: sprig splits --gate-cmd at whitespace.
const SprigSpaceText = "the sprig gate command has a word with a space in it, and sprig splits that " +
	"command at spaces. Use a TMPDIR path and a daisugi path with no spaces."

func strList(v any) []string {
	var out []string
	if l, ok := v.([]any); ok {
		for _, x := range l {
			s, _ := x.(string)
			out = append(out, s)
		}
	}
	return out
}

// Run is AgenticExecutor.run. A sub-agent that fails is a failed step,
// never a swallowed one; the tree directory is kept.
func (a *Agentic) Run(step *pyjson.Object, timeoutS, maxOutputBytes int) (ExecResult, error) {
	started := time.Now()
	fail := func(msg string) (ExecResult, error) {
		return ExecResult{RC: 1, Stdout: TruncateOutput(msg, maxOutputBytes), DurationMs: ms(started)}, nil
	}
	var profileName *string
	if s, ok := step.Value("profile").(string); ok {
		profileName = &s
	}
	profile, why := a.Profiles.Lookup(profileName)
	workspace := str(step, "workspace")
	if st, err := os.Stat(workspace); err != nil || !st.IsDir() {
		return fail(fmt.Sprintf("agentic workspace '%s' does not exist or is not a directory", workspace))
	}
	if profile == nil {
		return fail("agentic sub-agent failed: " + why)
	}
	model, _ := step.Value("preferred_model").(string)
	if model == "" && profile.Model != nil {
		model = *profile.Model
	}
	if model == "" {
		model = a.Model
	}
	// A profile on another wire runs on sprig, whatever --agent says.
	runtimeName := a.Runtime
	if profile.Wire != "claude-code" {
		runtimeName = "sprig"
	}
	// The edge: the sub-agent's envelope must fit inside the caller's,
	// strict and fail closed, before anything starts. A step with no child
	// envelope restates the caller's own, which fits.
	proposed, _ := step.Value("child_envelope").(*pyjson.Object)
	if proposed == nil {
		proposed = a.Envelope
	}
	edge := tree.EdgeOK(a.Envelope, proposed, edgeTimeoutMs)
	if !edge.Holds || edge.Child == nil {
		return fail("the child envelope is refused: " + strings.Join(edge.Reasons, "; "))
	}
	perms, _ := edge.Child.Value("permissions").(*pyjson.Object)
	var allowed []string
	for _, name := range strList(step.Value("tools")) {
		if _, known := agenticToolCapabilities[name]; !known {
			continue
		}
		if _, ok := sprigTools[name]; runtimeName == "sprig" && !ok {
			continue // sprig has no tool for it: left out, never mapped
		}
		if runtimeName != "sprig" && name == "Spawn" {
			continue // a claude -p node is a leaf: it starts no children
		}
		if toolGranted(name, perms) {
			allowed = append(allowed, name)
		}
	}
	if len(allowed) == 0 {
		return fail(fmt.Sprintf("no requested tool is backed by the envelope (requested %s); nothing to delegate",
			pmodel.Repr(step.Value("tools"))))
	}
	if runtimeName == "sprig" {
		words := []string{a.Self, a.TempDir}
		for _, t := range allowed {
			if t == "Spawn" {
				words = append(words, a.Self)
			}
		}
		if hasSpace(words) {
			return fail(SprigSpaceText)
		}
	}
	// The tree lives outside the workspace: the sub-agent must not be able
	// to rewrite its hook settings, its envelope or the ledger
	// mid-session. gate/ and tree/ under one directory, so the gate guards
	// both, each private.
	dataDir, err := os.MkdirTemp(a.TempDir, "daisugi-agentic-")
	if err != nil {
		return ExecResult{}, err
	}
	gateRoot := gateroot.Join(dataDir, "gate")
	for _, d := range []string{gateRoot, gateroot.Join(dataDir, "tree")} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return ExecResult{}, err
		}
	}
	// The executor, not the sub-agent, picks the session the envelope
	// binds to, and pins the gate to it.
	session := "agentic-" + gateroot.SafeSessionID(str(step, "id"))
	if _, err := tree.MakeRoot(dataDir, gateRoot, a.Envelope, RootSession, a.Tokens, a.Turns); err != nil {
		return ExecResult{}, err
	}
	defer func() {
		tree.EndSubtree(dataDir, gateRoot, session, true)
		_, _ = tree.End(dataDir, gateRoot, RootSession, new(big.Int), new(big.Int))
	}()
	if runtimeName == "sprig" {
		return a.runSprigNode(step, proposed, profileName, model, workspace, dataDir, gateRoot, session,
			timeoutS, maxOutputBytes, started)
	}
	return a.runClaudeNode(step, proposed, profileName, model, allowed, workspace, dataDir, gateRoot, session,
		timeoutS, maxOutputBytes, started)
}

func (a *Agentic) runSprigNode(step, proposed *pyjson.Object, profileName *string, model, workspace, dataDir,
	gateRoot, session string, timeoutS, maxOutputBytes int, started time.Time) (ExecResult, error) {
	fail := func(msg string) (ExecResult, error) {
		return ExecResult{RC: 1, Stdout: TruncateOutput(msg, maxOutputBytes), DurationMs: ms(started)}, nil
	}
	var maxTurns *big.Int
	if mt, ok := step.Value("max_turns").(pyjson.Int); ok {
		maxTurns, _ = new(big.Int).SetString(mt.Text, 10)
	}
	s := &Starter{Self: a.Self, TempDir: a.TempDir, Sprig: a.Sprig, Environ: a.Environ, LookPath: a.LookPath}
	sess := session
	node, err := s.RunNode(dataDir, gateRoot, NodeOpts{Parent: RootSession, Session: &sess, Envelope: proposed,
		Profiles: a.Profiles, ProfileName: profileName, Model: model, Tokens: a.Tokens, Turns: a.Turns,
		Task: str(step, "prompt"), Tools: strList(step.Value("tools")), ToolsRepr: pmodel.Repr(step.Value("tools")),
		MaxTurns: maxTurns, Cwd: workspace, TimeoutS: timeoutS, DefaultModel: a.Model, TimeoutMs: tree.DefaultTimeoutMs})
	if err != nil {
		return ExecResult{}, err
	}
	switch node.Status {
	case "refused", "asked":
		return fail("the child envelope is refused: " + strings.Join(node.Reasons, "; "))
	case "started":
		return ExecResult{RC: 0, Stdout: TruncateOutput(*node.Answer, maxOutputBytes), DurationMs: ms(started)}, nil
	}
	return fail(node.Reasons[0])
}

func (a *Agentic) runClaudeNode(step, proposed *pyjson.Object, profileName *string, model string, allowed []string,
	workspace, dataDir, gateRoot, session string, timeoutS, maxOutputBytes int, started time.Time) (ExecResult, error) {
	fail := func(msg string) (ExecResult, error) {
		return ExecResult{RC: 1, Stdout: TruncateOutput(msg, maxOutputBytes), DurationMs: ms(started)}, nil
	}
	var profName *string
	if profiledName(profileName) {
		profName = profileName
	}
	m := model
	sess := session
	sp, err := tree.DoSpawnOpts(dataDir, gateRoot, proposed, RootSession, &sess, a.Tokens, a.Turns,
		tree.DefaultTimeoutMs, tree.SpawnOpts{Profile: profName, Model: &m})
	if err != nil {
		return ExecResult{}, err
	}
	if sp.Status != "started" {
		return fail("the child envelope is refused: " + strings.Join(sp.Reasons, "; "))
	}
	settings := install.SettingsJSON(a.Self, install.HookOptions{Mode: "enforce", Root: gateRoot,
		CapturesRoot: gateroot.Join(gateRoot, "captures"), Session: &sess})
	extra := []string{"--output-format", "json", "--settings", settings, "--allowedTools", strings.Join(allowed, " ")}
	var maxTurns *big.Int
	if mt, ok := step.Value("max_turns").(pyjson.Int); ok {
		maxTurns, _ = new(big.Int).SetString(mt.Text, 10)
	}
	if a.Turns != nil && (maxTurns == nil || a.Turns.Cmp(maxTurns) < 0) {
		maxTurns = a.Turns
	}
	if maxTurns != nil {
		extra = append(extra, "--max-turns", maxTurns.String())
	}
	env := childEnv(a.Environ, a.Profiles, false)
	raw, err := a.Claude.SyncInEnv(workspace, str(step, "prompt"), model, float64(timeoutS), env, extra...)
	if err != nil {
		return fail("agentic sub-agent failed: " + err.Error())
	}
	parsed, derr := pyjson.LoadsPy(raw, 900)
	if derr != nil {
		return fail("agentic sub-agent returned unparseable output: " + pystr.Repr(pystr.Slice(raw, 0, 300)))
	}
	obj, isObj := parsed.(*pyjson.Object)
	if !isObj {
		// obj.get raises AttributeError out of the executor.
		return ExecResult{}, fmt.Errorf("'%s' object has no attribute 'get'", pmodel.TypeName(parsed))
	}
	// The meter sums the token counts with int(); a count int() refuses
	// raises out of the executor.
	if usage, ok := obj.Value("usage").(*pyjson.Object); ok {
		for _, f := range []string{"input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"} {
			if err := pyInt(usage.Value(f)); err != nil {
				return ExecResult{}, err
			}
		}
	}
	var result string
	if r := obj.Value("result"); pyjson.Truthy(r) {
		if s, isStr := r.(string); isStr {
			result = s
		} else {
			result = pmodel.Repr(r)
		}
	}
	if pyjson.Truthy(obj.Value("is_error")) {
		return fail("agentic sub-agent reported is_error: " + pystr.Slice(result, 0, 500))
	}
	own, _ := ownUse(obj)
	var turns *big.Int
	if n, ok := obj.Value("num_turns").(pyjson.Int); ok {
		if v, okk := new(big.Int).SetString(n.Text, 10); okk {
			turns = clampCount(v)
		}
	}
	if _, err := tree.End(dataDir, gateRoot, session, own, turns); err != nil {
		return ExecResult{}, err
	}
	return ExecResult{RC: 0, Stdout: TruncateOutput(result, maxOutputBytes), DurationMs: ms(started)}, nil
}

// pyInt is int(v) on a decoded JSON value, for its error only: nil when
// int() takes it, or when it is None (left out of the sum).
func pyInt(v any) error {
	switch x := v.(type) {
	case nil, bool, pyjson.Int, float64, pyjson.Float:
		return nil
	case string:
		t := strings.ReplaceAll(strings.TrimSpace(x), "_", "")
		t = strings.TrimPrefix(strings.TrimPrefix(t, "+"), "-")
		if t != "" && strings.Trim(t, "0123456789") == "" {
			return nil
		}
		return fmt.Errorf("invalid literal for int() with base 10: %s", pystr.Repr(x))
	}
	return fmt.Errorf("int() argument must be a string, a bytes-like object or a real number, not '%s'",
		pmodel.TypeName(v))
}

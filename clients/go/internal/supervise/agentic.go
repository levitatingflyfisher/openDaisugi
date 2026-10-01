package supervise

import (
	"fmt"
	"os"
	"strings"
	"time"

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
}

// edgeTimeoutMs is tree.DEFAULT_TIMEOUT_MS, the edge proof's Z3 budget.
const edgeTimeoutMs = 2000

// Agentic is agentic_executor.AgenticExecutor: an agentic step run as
// `claude -p` in its workspace, under the call-time gate in enforce mode.
// The step's child envelope (the caller's own when it names none) is
// proved inside the caller's envelope before anything starts, registered
// in a fresh gate root outside the workspace under a session the executor
// picks, and the sub-agent's tool wall is computed from it.
type Agentic struct {
	// Envelope is the caller's envelope, the authorization ceiling.
	Envelope *pyjson.Object
	// Model is the sub-agent's model ("haiku").
	Model string
	// Claude runs `claude -p`.
	Claude *llm.Client
	// Self is this binary: the gate hook runs `<Self> gate check`.
	Self string
	// TempDir is where the gate root is made ($TMPDIR, as mkdtemp reads
	// it).
	TempDir string
}

// Run is AgenticExecutor.run. A sub-agent that fails is a failed step,
// never a swallowed one; the gate root is kept.
func (a *Agentic) Run(step *pyjson.Object, timeoutS, maxOutputBytes int) (ExecResult, error) {
	started := time.Now()
	fail := func(msg string) (ExecResult, error) {
		return ExecResult{RC: 1, Stdout: TruncateOutput(msg, maxOutputBytes), DurationMs: ms(started)}, nil
	}
	workspace := str(step, "workspace")
	if st, err := os.Stat(workspace); err != nil || !st.IsDir() {
		return fail(fmt.Sprintf("agentic workspace '%s' does not exist or is not a directory", workspace))
	}
	// The edge: the sub-agent's envelope must fit inside the caller's,
	// strict and fail closed, before anything starts. A step with no child
	// envelope restates the caller's own, which fits.
	child, _ := step.Value("child_envelope").(*pyjson.Object)
	if child == nil {
		child = a.Envelope
	}
	edge := tree.EdgeOK(a.Envelope, child, edgeTimeoutMs)
	if !edge.Holds || edge.Child == nil {
		return fail("the child envelope is refused: " + strings.Join(edge.Reasons, "; "))
	}
	registered := pyjson.NewObject()
	for _, k := range edge.Child.Keys() {
		registered.Set(k, edge.Child.Value(k))
	}
	registered.Set("parent_envelope", a.Envelope.Value("id"))

	tools, _ := step.Value("tools").([]any)
	perms, _ := registered.Value("permissions").(*pyjson.Object)
	var allowed []string
	for _, t := range tools {
		name, _ := t.(string)
		capName, known := agenticToolCapabilities[name]
		if !known || perms == nil {
			continue
		}
		if pyjson.Truthy(perms.Value(capName)) {
			allowed = append(allowed, name)
		}
	}
	if len(allowed) == 0 {
		return fail(fmt.Sprintf("no requested tool is backed by the envelope (requested %s); nothing to delegate",
			pmodel.Repr(step.Value("tools"))))
	}

	// The gate root is made outside the workspace: the sub-agent must not
	// be able to rewrite its own hook settings or envelope mid-session.
	root, err := os.MkdirTemp(a.TempDir, "daisugi-agentic-gate-")
	if err != nil {
		return ExecResult{}, err
	}
	// The executor, not the sub-agent, picks the session the envelope
	// binds to, and pins the gate to it.
	session := "agentic-" + gateroot.SafeSessionID(str(step, "id"))
	if _, _, err := gateroot.Register(registered, session, root); err != nil {
		return ExecResult{}, err
	}
	settings := install.SettingsJSON(a.Self, install.HookOptions{Mode: "enforce", Root: root,
		CapturesRoot: gateroot.Join(root, "captures"), Session: &session})
	extra := []string{"--output-format", "json", "--settings", settings, "--allowedTools", strings.Join(allowed, " ")}
	if mt, ok := step.Value("max_turns").(pyjson.Int); ok {
		extra = append(extra, "--max-turns", mt.Text)
	}
	raw, err := a.Claude.SyncIn(workspace, str(step, "prompt"), a.Model, float64(timeoutS), extra...)
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

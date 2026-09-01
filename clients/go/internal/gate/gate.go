// Package gate is a port of opendaisugi.gate: one hook payload in, the
// host's verdict contract out, with the same shadow log, session tree and
// coppice report the Python gate writes.
//
// The port answers every call itself; it never runs Python. Python stays
// the oracle the tests compare against. A call the port cannot decide is
// denied with a reason (see unported), never allowed.
//
// A Python exception the oracle would raise is a panic with a
// *pystr.Exception here, recovered where the oracle catches it and handled
// the way the oracle handles it.
package gate

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/shell"
	"daisugi-verify/internal/tsbash"
	"daisugi-verify/internal/verify"
)

// MaxPayloadBytes and MaxShellCommandChars are gate.MAX_PAYLOAD_BYTES and
// gate.MAX_SHELL_COMMAND_CHARS: past them the gate denies an input unread,
// so no verdict depends on how fast the box reads it.
const (
	MaxPayloadBytes      = 16 * 1024 * 1024
	MaxShellCommandChars = 256 * 1024
)

// unportedCall is the panic value for a call the port cannot decide.
type unportedCall struct{ reason string }

// unported denies the call: the port does not model what the oracle does
// here, so it refuses rather than guess.
func unported(reason string) { panic(unportedCall{reason}) }

// Result is what the gate process emits.
type Result struct {
	Stdout string
	Stderr string
	Exit   int
	// Native is false when the port could not decide the call and denied
	// it; Why says why.
	Native bool
	Why    string
	// Crashed is true when RunGuarded's child ended abnormally and the
	// call was denied for it.
	Crashed bool
}

// runner holds one call's inputs.
type runner struct {
	env         map[string]string
	home        string
	cwd         string
	root        string
	defaultRoot string

	mode           string
	fmt            string
	session        *string
	captures       *string
	verifyTimeoutS float64
	ask            bool
	askTimeoutS    float64
	checkpoints    bool

	t0 time.Time
	// verify is verifyRecord unless a test replaces it.
	verify func(*record, *envelope) []violation

	realMu    sync.Mutex
	realCache map[string]realAnswer

	// shellCache keeps this call's decompositions.
	shellCache shell.Cache

	// vacuityCache is vacuity._VACUITY_CACHE for this call, keyed by the
	// expression's JSON.
	vacuityCache map[string]string

	// depth is the Python frame depth the oracle's main thread is at in
	// the function being run. Functions that stand for a Python function
	// start with `defer r.enter()()`, so a walk deep enough to pass
	// Python's recursion limit raises RecursionError where the oracle's
	// does. The verifier's worker thread counts its own depth.
	depth int
}

// enter models a Python call from the current frame: one frame deeper,
// raising RecursionError past the limit. The returned func leaves it.
func (r *runner) enter() func() {
	r.depth++
	if r.depth > shell.RecursionLimit {
		r.depth--
		panic(pystr.RecursionError())
	}
	return r.leave
}

func (r *runner) leave() { r.depth-- }

// catch runs fn and returns the Python exception it raised, if any, as a
// try/except Exception block would. Anything else keeps unwinding.
func catch(fn func()) (exc *pystr.Exception) {
	defer func() {
		if p := recover(); p != nil {
			if e, ok := p.(*pystr.Exception); ok {
				exc = e
				return
			}
			panic(p)
		}
	}()
	fn()
	return nil
}

// verifyWithDeadline runs the verifier under --verify-timeout, as the
// Python gate runs it in a worker thread. late is true when the budget
// ran out; the worker is abandoned, as Python abandons its daemon thread.
//
// The verifier is gate._dispatch_verify: config.yaml's verifier_client is
// read first, and a client other than python runs beside the verdict
// (verifyVia). ok is false when the verdict denies, which a client can
// make so with no violation of its own.
func (r *runner) verifyWithDeadline(rec *record, env *envelope) (vs []violation, ok bool, late bool) {
	fn := r.verify
	if fn == nil {
		fn = r.verifyRecord
	}
	// The oracle starts its verifier thread and joins it for timeout_s. A
	// timeout the join cannot take raises there, and a budget under a
	// second is one the oracle's thread usually loses: the port denies
	// both ways at once, never looser than the oracle (see ADJUDICATIONS,
	// GT-30).
	switch t := r.verifyTimeoutS; {
	case math.IsNaN(t):
		panic(pystr.NewException("ValueError", "Invalid value NaN (not a number)"))
	case t*1e9 >= 9.223372036854775807e18:
		panic(pystr.NewException("OverflowError", "timestamp out of range for platform time_t"))
	case t < 1:
		return nil, false, true
	}
	var out []violation
	allOK := true
	if r.runBounded(func() {
		client := r.verifierClient()
		t := time.Now()
		out = fn(rec, env)
		r.recordVerify(rec, env, out)
		allOK = len(out) == 0
		if client != "python" {
			out, allOK = r.verifyVia(client, rec, env, out, time.Since(t).Seconds())
		}
	}) {
		return nil, false, true
	}
	return out, allOK, false
}

// pathMatches is verify._path_matches_any. A glob past the matcher's step
// limit raises GlobTooComplex, as the oracle's does.
func (r *runner) pathMatches(path string, globs []string) bool {
	defer func() {
		if p := recover(); p != nil {
			if g, ok := p.(verify.GlobTooComplex); ok {
				panic(pystr.NewException("GlobTooComplex", fmt.Sprintf(
					"file glob %s is too complex to match: more than %d steps", pystr.Repr(g.Glob), g.Limit)))
			}
			panic(p)
		}
	}()
	return verify.PathMatchesAny(path, globs)
}

// runBounded runs fn under the --verify-timeout budget. It is true when
// the budget ran out first; fn's worker is then abandoned (the process
// ends soon after). A panic in fn, a hand-off to Python included, is
// raised again here.
func (r *runner) runBounded(fn func()) (late bool) {
	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		fn()
	}()
	timer := time.NewTimer(time.Duration(r.verifyTimeoutS * float64(time.Second)))
	defer timer.Stop()
	select {
	case p := <-done:
		if p != nil {
			panic(p)
		}
		return false
	case <-timer.C:
		return true
	}
}

func (r *runner) now() time.Time { return time.Now() }

func (r *runner) elapsedMS() float64 {
	return float64(time.Since(r.t0).Nanoseconds()) / 1e6
}

// safeSession is hook._safe_session_id on a decoded JSON value.
func (r *runner) safeSession(v any) string {
	return safeSessionID(pyStrOr(v))
}

// catchArgvExit runs fn and returns the argparse exit it raised, if any.
func catchArgvExit(fn func()) (ex *argvExit) {
	defer func() {
		if p := recover(); p != nil {
			if e, ok := p.(argvExit); ok {
				ex = &e
				return
			}
			panic(p)
		}
	}()
	fn()
	return nil
}

// Run answers one call.
func Run(argv []string, stdin []byte, environ []string) Result {
	env := map[string]string{}
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	// CPython coerces a C locale to C.UTF-8 at startup and exports it to
	// its children; the grammar reads the same LC_CTYPE.
	if c := tsbash.PythonLocale(); c != "" {
		env["LC_CTYPE"] = c
	}
	res, why := runNative(argv, stdin, env)
	if why == "" {
		res.Native = true
		return res
	}
	out := DenyResult(DenyFormat(argv), "the Go gate cannot decide this call ("+why+"), so it denies it")
	out.Why = why
	return out
}

// DenyResult is the host's deny contract for format f, with reason. An
// unknown format gets the plain deny: exit 2 and a line on stderr.
func DenyResult(f, reason string) Result {
	return outcome(&decision{Allow: false, WouldDeny: true, Reason: reason}, f)
}

// FormatFromArgv is gate._fmt_from_argv: the --format value, or claude.
func FormatFromArgv(argv []string) string {
	for i, a := range argv {
		if a == "--format" && i+1 < len(argv) {
			return argv[i+1]
		}
		if strings.HasPrefix(a, "--format=") {
			return strings.SplitN(a, "=", 2)[1]
		}
	}
	return "claude"
}

func runNative(argv []string, stdin []byte, env map[string]string) (res Result, why string) {
	defer func() {
		if p := recover(); p != nil {
			if d, ok := p.(unportedCall); ok {
				why = d.reason
				return
			}
			why = fmt.Sprintf("unexpected: %v", p)
		}
	}()
	r := &runner{env: env, t0: time.Now()}
	// sys.argv as Python decodes it: undecodable bytes as surrogates.
	args := make([]string, len(argv))
	for i, a := range argv {
		args[i] = pystr.FSDecode([]byte(a))
	}
	var o options
	columns := terminalColumns(env)
	delete(env, ttyColumnsEnv)
	if ex := catchArgvExit(func() { o = parseArgv(args, columns) }); ex != nil {
		if ex.code == 0 {
			// --help: argparse exits 0 and run_argv lets the exit through.
			return Result{Stdout: ex.stdout, Exit: 0}, ""
		}
		// A malformed argv: argparse prints the usage and the error and
		// exits 2, which run_argv turns into the enforce escape, the same
		// escape gate._escape_outcome builds. --format never parsed either,
		// so this reads it the same best-effort way _fmt_from_argv does
		// (FormatFromArgv), not from the unparsed `o`. A stdout-block host
		// (hermes, openclaw) never reads the exit code. The plain exit-2
		// deny below is the only body claude/pi/opencode ever see, and it
		// is invisible to a stdout-block host, so that host gets its own
		// deny body on stdout at exit 0 instead, the same shape outcome()
		// already gives a stdout-block format everywhere else.
		f := FormatFromArgv(argv)
		reason := "gate escape: 2"
		if f == "hermes" || f == "openclaw" {
			return Result{Stdout: stdoutFor(f, true, reason) + "\n",
				Stderr: pystr.BackslashReplace(ex.stderr), Exit: 0}, ""
		}
		return Result{Stderr: pystr.BackslashReplace(ex.stderr) +
			"openDaisugi gate: DENIED (fail-closed on error): 2\n", Exit: 2}, ""
	}
	r.fmt = "claude"
	if o.format != nil {
		r.fmt = *o.format
	}
	r.verifyTimeoutS = 10
	if o.verifyTimeout != nil {
		r.verifyTimeoutS = *o.verifyTimeout
	}
	r.ask = o.ask
	r.askTimeoutS = 90
	if o.askTimeout != nil {
		r.askTimeoutS = *o.askTimeout
	}
	r.checkpoints = o.checkpoints
	r.session = o.session
	if o.captures != nil {
		c := pathStr(*o.captures)
		r.captures = &c
	}
	r.checkEnv()
	r.defaultRoot = pathJoin(r.pathHome(), ".opendaisugi/gate")
	r.root = r.defaultRoot
	if o.root != nil {
		r.root = pathStr(*o.root)
	}
	r.mode = r.resolveMode(o.mode)
	// gate_and_contract runs at frame 6 of `python -m opendaisugi.gate`:
	// runpy's two frames, the module, main and run_argv sit under it.
	r.depth = 5
	var pl *plan
	if exc := catch(func() { pl = r.decide(stdin) }); exc != nil {
		// gate_and_contract's own except: an error outside evaluate_call
		// (a registered envelope that does not load, say) writes nothing
		// and follows the mode's failure policy.
		d := r.deny("gate I/O error (denied fail-closed): " + exc.Msg)
		if r.mode != "enforce" {
			d = &decision{Allow: true, WouldDeny: true, Reason: "gate I/O error (shadow mode allows): " + exc.Msg,
				ElapsedMS: r.elapsedMS(), Tier: tierPermanent}
		}
		return outcome(d, r.fmt), ""
	}
	pl.apply(r)
	return pl.result, ""
}

// checkEnv hands the call to Python when the environment asks for a
// behavior the port does not carry.
func (r *runner) checkEnv() {
	// Path.home(): HOME as it is (relative or empty included), or this
	// user's password entry when HOME is unset.
	r.home = r.expanduser("~")
	if strings.HasPrefix(r.home, "~") {
		// Python cannot import the gate without a home directory: it
		// fails before it reads the call.
		unported("no home directory: HOME is unset and the password database has no entry")
	}
}

// environ is os.environ as the oracle's children get it.
func (r *runner) environ() []string {
	out := make([]string, 0, len(r.env))
	for k, v := range r.env {
		out = append(out, k+"="+v)
	}
	return out
}

// decision is gate.GateDecision.
type decision struct {
	Allow      bool
	WouldDeny  bool
	Reason     string
	ToolName   any // string or nil
	StepType   any // string or nil
	Detail     string
	ElapsedMS  float64
	Violations []violation
	EnvelopeID any
	PlanID     any
	PaneRule   bool
	Tier       string
	// An answered ask: the operator's edit of the call, who answered and
	// how that name is known.
	Ask          bool
	UpdatedInput any
	AnsweredBy   any
	WhoFrom      any
}

func (d *decision) clause() string {
	if len(d.Violations) > 0 {
		return d.Violations[0].Stage + ": " + d.Violations[0].Message
	}
	return d.Reason
}

func (d *decision) counterexample() *pyjson.Object {
	if len(d.Violations) > 0 && d.Violations[0].Detail != nil {
		return d.Violations[0].Detail
	}
	return pyjson.NewObject()
}

func (r *runner) deny(reason string) *decision {
	return &decision{Allow: r.mode == "shadow", WouldDeny: true, Reason: reason,
		ElapsedMS: r.elapsedMS(), Tier: tierPermanent}
}

func strOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// evaluateCall is gate.evaluate_call for a payload that is an object.
func (r *runner) evaluateCall(p *pyjson.Object, env *envelope, cwd string) (d *decision) {
	defer r.enter()()
	// Fail-closed: any error denies.
	if exc := catch(func() { d = r.evaluateCallBody(p, env, cwd) }); exc != nil {
		d = r.deny("gate internal error (denied fail-closed): " + exc.Msg)
	}
	return d
}

func (r *runner) evaluateCallBody(p *pyjson.Object, env *envelope, cwd string) *decision {
	tn := toolNameOf(p)
	if tn == nil {
		return r.deny("no tool name in hook payload")
	}
	if name, ok := tn.(string); ok && name == applyPatchTool {
		return r.decidePatch(p, name, env, cwd)
	}
	rec, ok := r.payloadToRecord(p, r.fmt)
	if !ok {
		rec = nil
	}
	// The record exists only for a str tool name.
	name, _ := tn.(string)
	return r.decideRecord(p, rec, name, env, cwd)
}

// decideRecord is gate._decide.
func (r *runner) decideRecord(p *pyjson.Object, rec *record, toolName string, env *envelope, cwd string) *decision {
	hard := func(reason string) *decision {
		d := &decision{Allow: false, WouldDeny: true, Reason: reason, ToolName: toolName,
			ElapsedMS: r.elapsedMS(), PaneRule: true, Tier: tierPermanent}
		if rec != nil {
			d.StepType = rec.StepType
			d.Detail = rec.Command
			if d.Detail == "" {
				d.Detail = rec.Path
			}
		}
		return d
	}
	defer r.enter()()
	if rec != nil && rec.StepType == "shell" {
		if c, ok := rec.CommandRaw.(string); ok && pystr.Len(c) > MaxShellCommandChars {
			d := r.deny(fmt.Sprintf("shell command is longer than %d characters; the gate does not read it",
				MaxShellCommandChars))
			d.ToolName, d.StepType, d.Detail = toolName, "shell", c
			return d
		}
	}
	// The rules run with no deadline of their own, as in the oracle. The
	// process has one (see the command's crash guard), so a rule that
	// hangs still ends in a deny.
	refusal := ""
	switch {
	case r.paneRuleHit(cwd, rec):
		refusal = paneRefusal
	case r.floorHit(cwd, rec, floorGuard):
		refusal = floorRefusal
	case r.floorHit(cwd, rec, opencodeGuard):
		refusal = opencodeRefusal
	case r.searchAboveSecretHit(cwd, rec):
		refusal = searchRefusal
	}
	if refusal != "" {
		return hard(refusal)
	}
	if rec == nil {
		d := r.deny("unrecognized tool " + pyjson.Repr(toolName) +
			" — not in the gate's classification map, denied by default")
		d.ToolName = toolName
		return d
	}
	// _decide works the workspace out before it calls evaluate_record, so
	// a path error there comes first.
	projectDir, set := r.env["CLAUDE_PROJECT_DIR"]
	root := r.workspaceRoot(p.Value("cwd"), projectDir, set)
	var callCwd *string
	if c, ok := p.Value("cwd").(string); ok {
		callCwd = &c
	}
	return r.evaluateRecord(rec, env, root, callCwd)
}

// evaluateRecord is gate.evaluate_record with its tier.
func (r *runner) evaluateRecord(rec *record, env *envelope, root *workspace, callCwd *string) *decision {
	defer r.enter()()
	detail := rec.Command
	if detail == "" {
		detail = rec.Path
	}
	if detail == "" {
		detail = rec.URL
	}
	planID := "plan_" + randomHex(4)
	var vs []violation
	var ok, late bool
	if exc := catch(func() {
		// _records_to_steps validates the step model before verify runs.
		rec.stepFieldCheck()
		vs, ok, late = r.verifyWithDeadline(rec, env)
	}); exc != nil {
		// _evaluate_record's fail-closed except.
		d := r.deny(r.internalError(exc.Msg))
		d.ToolName, d.StepType, d.Detail = rec.ToolName, rec.StepType, detail
		d.Tier = r.tier(rec, d, root, callCwd)
		return d
	}
	if late {
		// gate._verify_with_timeout's TimeoutError, as _evaluate_record
		// words it: a deny with no clause behind it, so permanent.
		d := r.deny(r.internalError("verifier exceeded the gate's inner time budget (" +
			pyjson.FloatRepr(r.verifyTimeoutS) + "s)"))
		d.ToolName, d.StepType, d.Detail = rec.ToolName, rec.StepType, detail
		d.Tier = r.tier(rec, d, root, callCwd)
		return d
	}
	var d *decision
	if ok {
		d = &decision{Allow: true, WouldDeny: false, Reason: "verified in envelope"}
	} else {
		parts := make([]string, len(vs))
		for i, v := range vs {
			parts[i] = v.Stage + ": " + v.Message
		}
		summary := strings.Join(parts, "; ")
		if summary == "" {
			summary = "verification failed"
		}
		d = r.deny(summary)
		d.Violations = vs
	}
	d.ToolName, d.StepType, d.Detail = rec.ToolName, rec.StepType, detail
	d.EnvelopeID, d.PlanID = env.ID, planID
	d.ElapsedMS = r.elapsedMS()
	d.Tier = r.tier(rec, d, root, callCwd)
	return d
}

// tier is gate._tier: an allow is silent, a deny with no violation behind
// it is permanent, and any other deny takes the tier of the call's effect
// class. Any error is permanent.
func (r *runner) tier(rec *record, d *decision, root *workspace, callCwd *string) (t string) {
	defer r.enter()()
	if exc := catch(func() {
		switch {
		case !d.WouldDeny:
			t = tierSilent
		case len(d.Violations) == 0:
			t = tierPermanent
		default:
			t = tierFor(r.effectClass(rec, root, callCwd))
		}
	}); exc != nil {
		t = tierPermanent
	}
	return t
}

var harnessByFmt = map[string]string{
	"claude": "claude-code", "codex": "codex", "hermes": "hermes", "openclaw": "openclaw", "pi": "pi",
}

func harnessOf(f string) string {
	if h, ok := harnessByFmt[f]; ok {
		return h
	}
	return f
}

// decide is gate.gate_and_contract up to its writes: it returns every
// line to write and the host outcome, and writes nothing.
func (r *runner) decide(stdin []byte) *plan {
	defer r.enter()()
	pl := &plan{}
	if r.isDisarmed() {
		d := &decision{Allow: true, Reason: "gate disarmed by operator (marker file present)",
			ElapsedMS: r.elapsedMS(), Tier: tierPermanent}
		pl.shadow(r, "no-session", d, nil, nil)
		pl.result = outcome(d, r.fmt)
		return pl
	}
	// raw.decode("utf-8", "replace"), then json.loads when the text is
	// not blank; any error leaves no payload.
	var payload any
	tooBig := len(stdin) > MaxPayloadBytes
	text := ""
	if !tooBig {
		text = pystr.DecodeReplace(stdin)
	}
	if pyStrip(text) != "" {
		if v, err := pyjson.Loads(text); err == nil {
			payload = v
		}
	}
	p, isObj := payload.(*pyjson.Object)
	var payloadSession any
	if isObj {
		payloadSession = p.Value("session_id")
	}
	var sessionID any = payloadSession
	if r.session != nil && *r.session != "" {
		sessionID = *r.session
	}
	// The rules read str(payload.get("cwd") or "").
	cwd := ""
	if isObj {
		cwd = pyStrOr(p.Value("cwd"))
	}
	env := r.loadEnvelope(sessionID)
	var d *decision
	switch {
	case env == nil:
		d = r.deny("no envelope registered for this session — run `daisugi gate register <envelope.json>` to authorize it, or `daisugi gate disarm` to switch the gate off")
	case tooBig:
		d = r.deny(fmt.Sprintf("hook payload is larger than %d bytes; the gate does not read it", MaxPayloadBytes))
	case payload == nil:
		d = r.deny("hook payload was not parseable JSON")
	case !isObj:
		d = r.deny("hook payload is not a JSON object")
	default:
		d = r.evaluateCall(p, env, cwd)
		if r.ask && r.mode == "enforce" && d.WouldDeny && !d.PaneRule {
			d = r.maybeAsk(p, d, sessionID)
		}
	}
	sid := r.safeSession(sessionID)
	join := pyjson.NewObject()
	if isObj {
		join = joinOf(p)
	}
	pl.shadow(r, sid, d, payloadSession, join)
	if isObj {
		pl.tree(r, sid, p, d)
		if r.checkpoints && d.Allow {
			pl.checkpoint = r.dueCheckpoint(p, sessionID)
		}
		if r.captures != nil && d.Allow {
			// hook.record_call, best-effort: an error writes nothing.
			_ = catch(func() {
				if rec, ok := r.payloadToRecord(p, "claude"); ok {
					pl.capture(r, rec)
				}
			})
		}
		pl.state(r, sid, p, d)
	}
	pl.result = outcome(d, r.fmt)
	return pl
}

func (r *runner) isDisarmed() bool {
	return exists(pathJoin(r.root, "DISARMED"))
}

// outcome is gate._outcome, with print()'s trailing newline.
func outcome(d *decision, f string) Result {
	denyNow := !d.Allow
	line := func(s string) string {
		if s == "" {
			return ""
		}
		// sys.stderr writes a lone surrogate as a backslash escape.
		return pystr.BackslashReplace(s) + "\n"
	}
	edited := pyjson.Truthy(d.UpdatedInput)
	noChannel := ". The operator edit cannot be carried on the " + pyjson.Repr(f) +
		" format. It has no updatedInput channel. Denied fail-closed rather than running the original input"
	switch f {
	case "claude", "pi", "opencode":
		if denyNow {
			return Result{Stderr: line("openDaisugi gate: DENIED — " + d.Reason), Exit: 2}
		}
		if edited {
			if f == "claude" {
				// The operator edited the call before allowing it.
				body := kv("hookSpecificOutput", kv(
					"hookEventName", "PreToolUse",
					"permissionDecision", "allow",
					"permissionDecisionReason", d.Reason,
					"updatedInput", d.UpdatedInput))
				return Result{Stdout: line(pyjson.Dumps(body, true)), Exit: 0}
			}
			return Result{Stderr: line("openDaisugi gate: DENIED: " + d.Reason + noChannel + "."), Exit: 2}
		}
		return Result{Stdout: line(stdoutFor(f, false, "")), Exit: 0}
	case "hermes", "openclaw":
		if edited {
			return Result{Stdout: line(stdoutFor(f, true, d.Reason+noChannel)), Exit: 0}
		}
		return Result{Stdout: line(stdoutFor(f, denyNow, d.Reason)), Exit: 0}
	}
	return Result{Stderr: line("openDaisugi gate: DENIED: unknown host format " + pyjson.Repr(f) +
		". Use --format claude, pi, opencode, hermes, or openclaw."), Exit: 2}
}

// stdoutFor is hook.stdout_for_format.
func stdoutFor(f string, block bool, reason string) string {
	switch f {
	case "hermes":
		if block {
			return pyjson.Dumps(kv("decision", "block", "action", "block", "reason", reason), true)
		}
		return "{}"
	case "openclaw":
		if block {
			return pyjson.Dumps(kv("block", true, "blockReason", reason), true)
		}
		return "{}"
	}
	return `{"continue": true}`
}

package gate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// This file is verifier_dispatch.verify_via: a compiled conformance client
// runs beside the verdict the gate already has, and may only tighten it.
// The gate's own verdict stands in for the oracle's; the client gets the
// same case JSON the oracle would send it, so the case id it must echo is
// the same.

// dispatchBudgetFraction is gate._DISPATCH_BUDGET_FRACTION.
const dispatchBudgetFraction = 0.5

// clientSpec is one row of bench.options.VERIFIER_CLIENTS.
type clientSpec struct {
	argv       []string
	probe      string
	buildSteps []string
	buildCwd   string
}

var verifierClients = map[string]clientSpec{
	"python":     {argv: []string{"python", "-m", "opendaisugi.conformance"}},
	"rust":       {argv: []string{"clients/rust/target/release/conform"}, probe: "clients/rust/target/release/conform", buildSteps: []string{"cargo build --release"}, buildCwd: "clients/rust"},
	"go":         {argv: []string{"clients/go/conform"}, probe: "clients/go/conform", buildSteps: []string{"go build -o conform ./cmd/conform"}, buildCwd: "clients/go"},
	"typescript": {argv: []string{"node", "clients/ts/dist/conform.js"}, probe: "clients/ts/dist/conform.js", buildSteps: []string{"npm install", "npm run build"}, buildCwd: "clients/ts"},
	"lean":       {argv: []string{"clients/lean/.lake/build/bin/conform"}, probe: "clients/lean/.lake/build/bin/conform", buildSteps: []string{"lake build"}, buildCwd: "clients/lean"},
}

// clientArgv is bench.options.gate_client_argv: the argv the gate sends a
// case to, or nil when this box has no such client. A compiled client is
// found only through OPENDAISUGI_<NAME>_CLIENT or as daisugi-conform-<name>
// on PATH, never next to the binary, so the gate decides alike wherever it
// sits.
func (r *runner) clientArgv(name string, spec clientSpec) []string {
	if spec.probe == "" {
		return spec.argv
	}
	path := r.env["OPENDAISUGI_"+strings.ToUpper(name)+"_CLIENT"]
	if path == "" {
		path = r.which("daisugi-conform-" + name)
	}
	if path == "" {
		return nil
	}
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
		return nil
	}
	if spec.argv[0] == "node" {
		node := r.which("node")
		if node == "" {
			return nil
		}
		return []string{node, path}
	}
	if syscall.Access(path, 1) != nil {
		return nil
	}
	return []string{path}
}

// verifyVia is verifier_dispatch.verify_via on the gate's own verdict vs
// for the one-step plan. oracleS is how long that verdict took.
func (r *runner) verifyVia(client string, rec *record, env *envelope, vs []violation, oracleS float64) ([]violation, bool) {
	timeoutS := r.verifyTimeoutS * dispatchBudgetFraction
	left := timeoutS - oracleS
	spec, known := verifierClients[client]
	var argv []string
	if known {
		argv = r.clientArgv(client, spec)
	}
	failure := ""
	var verdict *pyjson.Object
	switch {
	case !known:
		failure = "no client named " + pystr.Repr(client)
	case argv == nil:
		failure = "not built; cd " + spec.buildCwd + " && " + strings.Join(spec.buildSteps, " && ")
	case left <= 0:
		failure = fmt.Sprintf("no time left after the oracle, which took %.2f s of %s s", oracleS, pyG(timeoutS))
	default:
		// _case_for runs outside the client's try: what it raises is the
		// gate's internal error.
		c := r.verifyCase(rec, env, vs)
		var err string
		verdict, err = runClient(argv, c, left, r.environ())
		if err != "" {
			failure = err
			verdict = nil
		}
	}
	base := r.root
	oracleOK := len(vs) == 0
	if verdict == nil {
		r.recordDispatch(base, client, false, failure)
		return vs, oracleOK
	}
	r.recordDispatch(base, client, true, "")
	for _, x := range verdict.Value("violations").([]any) {
		v := x.(*pyjson.Object)
		stage := "unknown"
		if s, ok := v.Get("stage"); ok {
			stage = s.(string)
		}
		step := v.Value("step")
		vs = append(vs, violation{
			Stage:   stage,
			Message: "the " + client + " verifier client refused this step, " + stepDetail(rec, step),
			Detail:  kv("step", step, "client", client),
		})
	}
	// ok is the conjunction: a client deny with no violations of its own
	// still denies.
	return vs, oracleOK && verdict.Value("ok") == true
}

// stepDetail is verifier_dispatch._step_detail for the gate's one step.
func stepDetail(rec *record, stepID any) string {
	if s, ok := stepID.(string); !ok || s != "s0" {
		return "no matching step in the plan"
	}
	var attr, value string
	switch rec.StepType {
	case "shell":
		attr, value = "command", rec.Command
	case "file_read", "file_write":
		attr, value = "path", rec.Path
	case "network":
		attr, value = "url", rec.URL
	case "mcp":
		attr, value = "tool", rec.MCPTool
	}
	if value != "" {
		return attr + "=" + value
	}
	return "step type " + rec.StepType
}

// pyG is format(x, "g").
func pyG(x float64) string {
	if math.IsInf(x, 0) || math.IsNaN(x) {
		return map[bool]string{true: "inf", false: "-inf"}[x > 0]
	}
	s := strconv.FormatFloat(x, 'g', 6, 64)
	if strings.Contains(s, "e") {
		m, e, _ := strings.Cut(s, "e")
		if strings.Contains(m, ".") {
			m = strings.TrimRight(strings.TrimRight(m, "0"), ".")
		}
		sign := e[:1]
		digits := strings.TrimLeft(e[1:], "0")
		if len(digits) < 2 {
			digits = strings.Repeat("0", 2-len(digits)) + digits
		}
		return m + "e" + sign + digits
	}
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

// runClient is verifier_dispatch._run_client. It returns the verdict for
// the case, or the failure text.
func runClient(argv []string, c *pyjson.Object, timeoutS float64, environ []string) (*pyjson.Object, string) {
	input, eerr := pystr.EncodeUTF8(pyjson.Canonical(c) + "\n")
	if eerr != nil {
		// subprocess encodes the text before it starts the client.
		panic(eerr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutS*float64(time.Second)))
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = environ
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, "timed out after " + pyG(timeoutS) + " s"
	}
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			var pe *os.PathError
			if errors.As(err, &pe) {
				return nil, "could not start: " + osError(pe.Err, argv[0]).String()
			}
			var ee2 *exec.Error
			if errors.As(err, &ee2) {
				return nil, "could not start: " + osError(syscall.ENOENT, argv[0]).String()
			}
			return nil, "could not start: " + err.Error()
		}
		code := ee.ExitCode()
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			code = -int(ws.Signal())
		}
		errText := pystr.Strip(universalNewlines(pystr.DecodeReplace(stderr.Bytes())))
		return nil, fmt.Sprintf("exited %d: %s", code, pystr.Slice(errText, -200, math.MaxInt))
	}
	id := c.Value("id").(string)
	var matches []*pyjson.Object
	for _, line := range pystr.Splitlines(universalNewlines(pystr.DecodeReplace(stdout.Bytes()))) {
		if pystr.Strip(line) == "" {
			continue
		}
		v, jerr := pyjson.Loads(line)
		if jerr != nil {
			return nil, "wrote a non-JSON verdict: " + pystr.Repr(pystr.Slice(line, 0, 120))
		}
		if !wellFormed(v) {
			return nil, "wrote a malformed verdict: " + pystr.Repr(pystr.Slice(line, 0, 120))
		}
		o := v.(*pyjson.Object)
		if s, ok := o.Value("id").(string); ok && s == id {
			matches = append(matches, o)
		}
	}
	if len(matches) == 0 {
		return nil, "produced no verdict for the case"
	}
	if len(matches) > 1 {
		return nil, fmt.Sprintf("wrote %d verdicts for one case", len(matches))
	}
	v := matches[0]
	ok, has := v.Get("ok")
	if !has {
		e, hasErr := v.Get("error")
		if !hasErr {
			e = "no ok field"
		}
		return nil, "error verdict: " + pyStrOf(e)
	}
	if _, has := v.Get("violations"); !has {
		v.Set("violations", []any{})
	}
	if ok == true && pyjson.Truthy(v.Value("violations")) {
		return nil, fmt.Sprintf("inconsistent verdict: ok with %d violations", len(v.Value("violations").([]any)))
	}
	return v, ""
}

// wellFormed is verifier_dispatch._well_formed.
func wellFormed(v any) bool {
	o, ok := v.(*pyjson.Object)
	if !ok {
		return false
	}
	if _, has := o.Get("id"); !has {
		return false
	}
	if x, has := o.Get("ok"); has {
		if _, isBool := x.(bool); !isBool {
			return false
		}
	}
	vs, has := o.Get("violations")
	if !has {
		return true
	}
	list, isList := vs.([]any)
	if !isList {
		return false
	}
	for _, e := range list {
		eo, isObj := e.(*pyjson.Object)
		if !isObj {
			return false
		}
		if _, isStr := eo.Value("stage").(string); !isStr {
			return false
		}
	}
	return true
}

func universalNewlines(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

// recordDispatch is verifier_dispatch._record_dispatch: best effort.
func (r *runner) recordDispatch(root, client string, ok bool, errText string) {
	target := pathJoin(root, "verifier")
	enc, eerr := pystr.FSEncode(target)
	if eerr != nil {
		return
	}
	// mkdir(parents=True, exist_ok=True, mode=0o700): the parents get the
	// default mode, the last directory 0o700, both under the umask.
	if err := os.MkdirAll(filepath.Dir(string(enc)), 0o777); err != nil {
		return
	}
	if err := os.Mkdir(string(enc), 0o700); err != nil {
		if st, serr := os.Stat(string(enc)); serr != nil || !st.IsDir() {
			return
		}
	}
	body := pyjson.Dumps(kv("client", client, "ok", ok, "error", errText, "at", float64(time.Now().UnixNano())/1e9), true)
	_ = os.WriteFile(string(enc)+"/last_dispatch.json", []byte(body), 0o666)
}

// verifyCase is conformance.make_verify_case for the gate's plan and
// envelope, with vs as the oracle's verdict.
func (r *runner) verifyCase(rec *record, env *envelope, vs []violation) *pyjson.Object {
	step := pyjson.NewObject().
		Set("id", "s0").
		Set("depends_on", []any{}).
		Set("metadata", pyjson.NewObject()).
		Set("postcondition", nil).
		Set("preferred_model", nil).
		Set("type", rec.StepType)
	switch rec.StepType {
	case "shell":
		step.Set("command", rec.Command)
	case "file_read":
		step.Set("path", rec.Path)
	case "file_write":
		step.Set("path", rec.Path).Set("content", "")
	case "network":
		step.Set("url", rec.URL).Set("method", "GET").Set("headers", pyjson.NewObject())
	case "mcp":
		args := rec.Arguments
		if args == nil {
			args = pyjson.NewObject()
		}
		step.Set("server", rec.MCPServer).Set("tool", rec.MCPTool).Set("arguments", jsonMode(args))
	}
	plan := pyjson.NewObject().
		Set("id", "plan_case").
		Set("source", "call-time-gate").
		Set("task", env.Task).
		Set("steps", []any{step})
	envBody := jsonMode(env.Obj).(*pyjson.Object)
	envBody.Set("id", "env_case")
	norm := make([]any, len(vs))
	for i, v := range vs {
		norm[i] = pyjson.NewObject().Set("stage", v.Stage).Set("step", v.Detail.Value("step"))
	}
	body := pyjson.NewObject().
		Set("kind", "verify").
		Set("v", 1).
		Set("plan", plan).
		Set("envelope", envBody).
		Set("options", pyjson.NewObject().Set("strict", nil).Set("z3_timeout_ms", 500)).
		Set("expect", pyjson.NewObject().Set("ok", len(vs) == 0).Set("violations", norm))
	text, eerr := pystr.EncodeUTF8(pyjson.Canonical(body))
	if eerr != nil {
		panic(eerr)
	}
	sum := sha256.Sum256(text)
	body.Set("id", hex.EncodeToString(sum[:])[:16])
	return body
}

// jsonMode is model_dump(mode="json") of a validated value: a copy, with
// NaN and the infinities as null.
func jsonMode(v any) any {
	switch x := v.(type) {
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil
		}
		return x
	case pyjson.Float:
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return nil
		}
		return x
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = jsonMode(e)
		}
		return out
	case *pyjson.Object:
		out := pyjson.NewObject()
		for _, k := range x.Keys() {
			out.Set(k, jsonMode(x.Value(k)))
		}
		return out
	}
	return v
}

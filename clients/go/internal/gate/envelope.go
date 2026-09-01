package gate

import (
	"crypto/rand"
	"encoding/hex"
	"math/big"
	"os"
	"strings"

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// envelope is a registered envelope, validated as models.Envelope.
type envelope struct {
	ID     string
	Task   string
	Stakes string
	Policy string

	FileRead                []string
	FileWrite               []string
	Network                 bool
	NetworkHosts            []string
	Shell                   bool
	ShellAllowlist          []string
	ShellAllowDecomposition bool
	McpAllowlist            []string
	CustomStepAllowlist     []string
	// MaxExecutionTimeS is an int of any size, as its decimal text.
	MaxExecutionTimeS string

	WorkspaceBounds *[2][3]float64
	Obstacles       [][2][3]float64
	VelocityLimit   *float64
	// JointLimits keep their order: joint name, then (min, max).
	JointLimits    []jointLimit
	Invariants     []predicateItem
	Postconditions []predicateItem
	// Obj is the validated envelope, field for field.
	Obj *pyjson.Object
}

type jointLimit struct {
	Name   string
	Lo, Hi float64
}

// predicateItem is an Invariant or a Postcondition as the predicate stage
// reads it.
type predicateItem struct {
	Type        string
	Expr        any // the raw expr value, or nil
	Enforce     bool
	Description any // str or None
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		unported("no randomness")
	}
	return hex.EncodeToString(b)
}

// loadEnvelope is gate.load_envelope: the session's own file first, then
// default. nil means none is registered. A file that cannot be read or
// validated raises what Python raises.
func (r *runner) loadEnvelope(sessionID any) *envelope {
	var candidates []string
	if pyjson.Truthy(sessionID) {
		candidates = append(candidates, r.safeSession(sessionID))
	}
	candidates = append(candidates, "default")
	for _, name := range candidates {
		p := pathJoin(pathJoin(r.root, "envelopes"), name+".json")
		if exists(p) {
			return loadEnvelopeFile(p)
		}
	}
	return nil
}

// readText is Path.read_text(encoding="utf-8"): a strict decode, and
// universal newlines.
func readText(p string) string {
	enc, eerr := pystr.FSEncode(p)
	if eerr != nil {
		panic(eerr)
	}
	raw, err := os.ReadFile(string(enc))
	if err != nil {
		panic(osError(err, p))
	}
	text, derr := pystr.DecodeStrict(raw)
	if derr != nil {
		panic(derr)
	}
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
}

func loadEnvelopeFile(p string) *envelope {
	v, verr := pmodel.ValidateJSON("Envelope", pmodel.Envelope, readText(p))
	if verr != nil {
		panic(pystr.NewException("ValidationError", verr.String()))
	}
	return envelopeFrom(v.(*pyjson.Object))
}

func strs(v any) []string {
	xs := v.([]any)
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = x.(string)
	}
	return out
}

func box(v any) [2][3]float64 {
	var out [2][3]float64
	for i, corner := range v.([]any) {
		for j, x := range corner.([]any) {
			out[i][j] = x.(float64)
		}
	}
	return out
}

func envelopeFrom(o *pyjson.Object) *envelope {
	perms := o.Value("permissions").(*pyjson.Object)
	env := &envelope{
		ID:                      o.Value("id").(string),
		Task:                    o.Value("task").(string),
		Stakes:                  o.Value("stakes").(string),
		Policy:                  o.Value("shell_interpreter_policy").(string),
		FileRead:                strs(perms.Value("file_read")),
		FileWrite:               strs(perms.Value("file_write")),
		Network:                 perms.Value("network").(bool),
		NetworkHosts:            strs(perms.Value("network_hosts")),
		Shell:                   perms.Value("shell").(bool),
		ShellAllowlist:          strs(perms.Value("shell_allowlist")),
		ShellAllowDecomposition: perms.Value("shell_allow_decomposition").(bool),
		McpAllowlist:            strs(perms.Value("mcp_allowlist")),
		CustomStepAllowlist:     strs(perms.Value("custom_step_allowlist")),
		MaxExecutionTimeS:       perms.Value("max_execution_time_s").(pyjson.Int).Text,
		Obj:                     o,
	}
	if wb := perms.Value("workspace_bounds"); wb != nil {
		b := box(wb)
		env.WorkspaceBounds = &b
	}
	for _, ob := range perms.Value("obstacles").([]any) {
		env.Obstacles = append(env.Obstacles, box(ob))
	}
	if vl := perms.Value("velocity_limit"); vl != nil {
		f := vl.(float64)
		env.VelocityLimit = &f
	}
	jl := perms.Value("joint_limits").(*pyjson.Object)
	for _, k := range jl.Keys() {
		t := jl.Value(k).([]any)
		env.JointLimits = append(env.JointLimits, jointLimit{Name: k, Lo: t[0].(float64), Hi: t[1].(float64)})
	}
	for _, x := range o.Value("invariants").([]any) {
		i := x.(*pyjson.Object)
		env.Invariants = append(env.Invariants, predicateItem{Type: i.Value("type").(string), Expr: i.Value("expr"),
			Enforce: i.Value("enforce").(bool), Description: i.Value("description")})
	}
	for _, x := range o.Value("postconditions").([]any) {
		pc := x.(*pyjson.Object)
		env.Postconditions = append(env.Postconditions, predicateItem{Type: pc.Value("type").(string), Expr: pc.Value("expr"),
			Enforce: pc.Value("enforce").(bool), Description: pc.Value("description")})
	}
	return env
}

// maxTimeIn is 0 < max_execution_time_s <= 3600, on an int of any size.
func (e *envelope) maxTimeIn() bool {
	n, ok := new(big.Int).SetString(e.MaxExecutionTimeS, 10)
	if !ok {
		return false
	}
	return n.Sign() > 0 && n.Cmp(big.NewInt(3600)) <= 0
}

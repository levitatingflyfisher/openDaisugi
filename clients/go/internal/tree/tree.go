// Package tree is opendaisugi/tree.py: the delegation tree's edge rule,
// its ledger, and the operator's asks.
//
// An envelope here is a validated dump (pmodel.Envelope): every field in
// model order with its default, and no deadline key when it has none.
package tree

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/verify"
)

// DefaultTimeoutMs is tree.DEFAULT_TIMEOUT_MS.
const DefaultTimeoutMs = 2000

// AskAfter is tree.ASK_AFTER: refused proposals from one parent before the
// next refused one is an ask.
const AskAfter = 3

var stakesRank = map[string]int{"low": 0, "medium": 1, "high": 2, "physical": 3}
var policyRank = map[string]int{"strict": 0, "surface": 1, "allow": 2}

// metachars are subsumption._encode_shell_admission's metacharacters and
// substrings.
var metachars = []string{";", "|", "&", "`", "<", ">", "\n", "\r", "$("}

var sessionRe = regexp.MustCompile(`^[A-Za-z0-9_-](?:[A-Za-z0-9._-]{0,62}[A-Za-z0-9_-])?$`)

// ValidSession is tree.valid_session.
func ValidSession(s string) bool {
	return sessionRe.MatchString(s) && s != "default" && s != "no-session"
}

// Q is tree._q: json.dumps of a value.
func Q(v any) string { return pyjson.Dumps(v, true) }

func obj(v any) *pyjson.Object {
	if o, ok := v.(*pyjson.Object); ok {
		return o
	}
	return pyjson.NewObject()
}

func strs(v any) []string {
	var out []string
	switch l := v.(type) {
	case []any:
		for _, x := range l {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	case []string:
		out = append(out, l...)
	}
	return out
}

func anys(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// Num is a JSON number as a float64.
func Num(v any) (float64, bool) {
	switch x := v.(type) {
	case pyjson.Float:
		return float64(x), true
	case float64:
		return x, true
	case pyjson.Int:
		f, err := strconv.ParseFloat(x.Text, 64)
		if err != nil && !math.IsInf(f, 0) {
			return 0, false
		}
		return f, true
	case int:
		return float64(x), true
	}
	return 0, false
}

func bigOf(v any) *big.Int {
	switch x := v.(type) {
	case pyjson.Int:
		n, ok := new(big.Int).SetString(x.Text, 10)
		if ok {
			return n
		}
	case int:
		return big.NewInt(int64(x))
	case int64:
		return big.NewInt(x)
	}
	return new(big.Int)
}

func intText(v any) string { return bigOf(v).String() }

func truthy(v any) bool { b, _ := v.(bool); return b }

// Result is tree.EdgeResult, without the counterexample (Python only).
type Result struct {
	Holds     bool
	Reasons   []string
	Child     *pyjson.Object // the child as it registers; nil with no envelope
	Inherited bool
}

// Doc is EdgeResult.doc().
func (r *Result) Doc() *pyjson.Object {
	var deadline any
	if r.Child != nil {
		deadline = r.Child.Value("deadline")
	}
	reasons := anys(r.Reasons)
	if reasons == nil {
		reasons = []any{}
	}
	return pyjson.NewObject().Set("holds", r.Holds).Set("reasons", reasons).
		Set("deadline", deadline).Set("deadline_inherited", r.Inherited)
}

// Text is tree.edge_text.
func (r *Result) Text() string {
	var lines []string
	if r.Holds {
		lines = append(lines, "edge ok: the child fits inside the parent.")
		if r.Inherited && r.Child != nil {
			lines = append(lines, "The child takes the parent's deadline "+Q(r.Child.Value("deadline"))+".")
		}
	} else {
		lines = append(lines, "edge refused:")
		for _, x := range r.Reasons {
			lines = append(lines, "  "+x)
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

func canon(v any) string { return pyjson.CanonicalASCII(v) }

func box(v any) ([3]float64, [3]float64, bool) {
	var lo, hi [3]float64
	l, ok := v.([]any)
	if !ok || len(l) != 2 {
		return lo, hi, false
	}
	for i, corner := range l {
		c, _ := corner.([]any)
		for k := 0; k < 3 && k < len(c); k++ {
			f, _ := Num(c[k])
			if i == 0 {
				lo[k] = f
			} else {
				hi[k] = f
			}
		}
	}
	return lo, hi, true
}

func pair(v any) (float64, float64) {
	l, _ := v.([]any)
	var a, b float64
	if len(l) == 2 {
		a, _ = Num(l[0])
		b, _ = Num(l[1])
	}
	return a, b
}

func boxKey(v any) string {
	lo, hi, _ := box(v)
	var parts []string
	for _, f := range append(lo[:], hi[:]...) {
		if f == 0 {
			f = 0 // -0.0 is 0.0 in a Python set
		}
		parts = append(parts, strconv.FormatFloat(f, 'g', -1, 64))
	}
	return strings.Join(parts, ",")
}

func robotReason(parent, child *pyjson.Object) string {
	o, i := obj(parent.Value("permissions")), obj(child.Value("permissions"))
	if ow := o.Value("workspace_bounds"); ow != nil {
		iw := i.Value("workspace_bounds")
		olo, ohi, _ := box(ow)
		bad := iw == nil
		if !bad {
			ilo, ihi, _ := box(iw)
			for k := 0; k < 3; k++ {
				if ilo[k] < olo[k] || ihi[k] > ohi[k] {
					bad = true
				}
			}
		}
		if bad {
			return "robot: the child's workspace_bounds are not inside the parent's"
		}
	}
	for _, axis := range []string{"velocity_limit", "torque_limit"} {
		if ov := o.Value(axis); ov != nil {
			iv := i.Value(axis)
			of, _ := Num(ov)
			if f, _ := Num(iv); iv == nil || f > of {
				return "robot: the child's " + axis + " is not within the parent's"
			}
		}
	}
	oj, ij := obj(o.Value("joint_limits")), obj(i.Value("joint_limits"))
	for _, j := range oj.Keys() {
		olo, ohi := pair(oj.Value(j))
		iv, ok := ij.Get(j)
		if !ok {
			return "robot: the child's range for joint " + Q(j) + " is not within the parent's"
		}
		ilo, ihi := pair(iv)
		if ilo < olo || ihi > ohi {
			return "robot: the child's range for joint " + Q(j) + " is not within the parent's"
		}
	}
	have := map[string]bool{}
	for _, b := range listOf(i.Value("obstacles")) {
		have[boxKey(b)] = true
	}
	missing := map[string]bool{}
	for _, b := range listOf(o.Value("obstacles")) {
		if k := boxKey(b); !have[k] {
			missing[k] = true
		}
	}
	if len(missing) > 0 {
		return fmt.Sprintf("robot: the child drops %d of the parent's obstacles", len(missing))
	}
	return ""
}

func listOf(v any) []any {
	l, _ := v.([]any)
	return l
}

func scopeReasons(parent, child *pyjson.Object, timeoutMs int) []string {
	var out []string
	o, i := obj(parent.Value("permissions")), obj(child.Value("permissions"))
	if truthy(i.Value("shell_allow_decomposition")) && !truthy(o.Value("shell_allow_decomposition")) {
		out = append(out, "shell_allow_decomposition: the child allows compound commands; the parent does not")
	}
	for _, label := range []string{"file_read", "file_write", "mcp_allowlist"} {
		inner, outer := strs(i.Value(label)), strs(o.Value(label))
		if len(inner) == 0 {
			continue
		}
		bad := ""
		for _, g := range outer {
			if verify.GlobUnsupported(g) {
				bad = g
				break
			}
		}
		if bad != "" {
			out = append(out, label+": the parent's pattern "+Q(bad)+" has a shape the proof cannot read")
			continue
		}
		for _, p := range inner {
			fit := verify.PatternFits(p, outer, timeoutMs)
			if fit == verify.Unfinished {
				out = append(out, label+": the proof for the child's "+Q(p)+" did not finish")
				break
			}
			if fit == verify.DoesNotFit {
				out = append(out, label+": the child's "+Q(p)+" is not inside the parent's")
				break
			}
		}
	}
	if truthy(i.Value("network")) {
		ohosts, ihosts := strs(o.Value("network_hosts")), strs(i.Value("network_hosts"))
		switch {
		case !truthy(o.Value("network")):
			out = append(out, "network: the child uses the network; the parent does not")
		case len(ohosts) > 0 && len(ihosts) == 0:
			out = append(out, "network_hosts: the child allows any host; the parent allows only "+Q(anys(ohosts)))
		case len(ohosts) > 0:
			known := map[string]bool{}
			for _, h := range ohosts {
				known[strings.ToLower(h)] = true
			}
			var extra []string
			for _, h := range ihosts {
				if !known[strings.ToLower(h)] {
					extra = append(extra, h)
				}
			}
			if len(extra) > 0 {
				out = append(out, "network_hosts: the child adds "+Q(anys(extra)))
			}
		}
	}
	if truthy(i.Value("shell")) {
		if !truthy(o.Value("shell")) {
			out = append(out, "shell: the child runs shell commands; the parent does not")
		} else {
			heads := strs(o.Value("shell_allowlist"))
			var extra []string
			for _, h := range strs(i.Value("shell_allowlist")) {
				if hasMeta(h) {
					continue
				}
				fits := false
				for _, p := range heads {
					if h == p || strings.HasPrefix(h, p+" ") {
						fits = true
						break
					}
				}
				if !fits {
					extra = append(extra, h)
				}
			}
			if len(extra) > 0 {
				out = append(out, "shell_allowlist: the child adds "+Q(anys(extra)))
			}
		}
		if parent.Value("shell_interpreter_policy") == "strict" {
			set := map[string]bool{}
			for _, n := range strs(i.Value("shell_allowlist")) {
				if verify.ShellInterpreters[n] {
					set[n] = true
				}
			}
			if len(set) > 0 {
				var names []string
				for n := range set {
					names = append(names, n)
				}
				sort.Strings(names)
				out = append(out, "shell_interpreter_policy: the parent is strict and the child allows the interpreters "+Q(anys(names)))
			}
		}
	}
	var opaque []string
	for _, inv := range listOf(child.Value("invariants")) {
		x := obj(inv)
		t, _ := x.Value("type").(string)
		if truthy(x.Value("enforce")) && x.Value("expr") == nil && !verify.RecognizedOpaque(t) {
			opaque = append(opaque, t)
		}
	}
	if len(opaque) > 0 {
		out = append(out, "invariants: the child's "+Q(anys(opaque))+" have no expr, so they cannot be proved")
	}
	return out
}

func hasMeta(h string) bool {
	for _, m := range metachars {
		if strings.Contains(h, m) {
			return true
		}
	}
	return false
}

func cmpBig(a, b any) int { return bigOf(a).Cmp(bigOf(b)) }

// EdgeOK is tree.edge_ok. parent or child nil is a missing envelope.
func EdgeOK(parent, child *pyjson.Object, timeoutMs int) *Result {
	if parent == nil {
		return &Result{Reasons: []string{"envelope: the parent has no envelope"}}
	}
	if child == nil {
		return &Result{Reasons: []string{"envelope: the child declares no envelope"}}
	}
	var reasons []string
	ps, _ := parent.Value("stakes").(string)
	cs, _ := child.Value("stakes").(string)
	if stakesRank[cs] < stakesRank[ps] {
		reasons = append(reasons, "stakes: the child's "+Q(cs)+" is lower than the parent's "+Q(ps))
	}
	op, ip := obj(parent.Value("permissions")), obj(child.Value("permissions"))
	known := map[string]bool{}
	for _, s := range strs(op.Value("custom_step_allowlist")) {
		known[s] = true
	}
	extraSet := map[string]bool{}
	for _, s := range strs(ip.Value("custom_step_allowlist")) {
		if !known[s] {
			extraSet[s] = true
		}
	}
	if len(extraSet) > 0 {
		var extra []string
		for s := range extraSet {
			extra = append(extra, s)
		}
		sort.Strings(extra)
		reasons = append(reasons, "custom_step_allowlist: the child adds "+Q(anys(extra)))
	}
	for _, name := range []string{"max_execution_time_s", "max_output_size_mb"} {
		ov, iv := op.Value(name), ip.Value(name)
		if cmpBig(iv, ov) > 0 {
			reasons = append(reasons, fmt.Sprintf("%s: the child's %s is more than the parent's %s", name, intText(iv), intText(ov)))
		}
	}
	inherited := false
	effective := child
	if pd := parent.Value("deadline"); pd != nil {
		cd := child.Value("deadline")
		if cd == nil {
			effective = withKey(child, "deadline", pd)
			inherited = true
		} else {
			pf, _ := Num(pd)
			cf, _ := Num(cd)
			if cf > pf {
				reasons = append(reasons, "deadline: the child's "+Q(cd)+" is after the parent's "+Q(pd))
			}
		}
	}
	ppol, _ := parent.Value("shell_interpreter_policy").(string)
	cpol, _ := child.Value("shell_interpreter_policy").(string)
	if policyRank[cpol] > policyRank[ppol] {
		reasons = append(reasons, "shell_interpreter_policy: the child's "+Q(cpol)+" is looser than the parent's "+Q(ppol))
	}
	for _, label := range []string{"invariants", "postconditions"} {
		have := map[string]bool{}
		for _, x := range listOf(child.Value(label)) {
			have[canon(x)] = true
		}
		var missing []string
		for _, x := range listOf(parent.Value(label)) {
			xo := obj(x)
			if truthy(xo.Value("enforce")) && !have[canon(x)] {
				t, _ := xo.Value("type").(string)
				missing = append(missing, t)
			}
		}
		if len(missing) > 0 {
			reasons = append(reasons, label+": the child drops the parent's "+Q(anys(missing)))
		}
	}
	if r := robotReason(parent, child); r != "" {
		reasons = append(reasons, r)
	}
	reasons = append(reasons, scopeReasons(parent, child, timeoutMs)...)
	exprs := false
	for _, env := range []*pyjson.Object{parent, child} {
		for _, x := range listOf(env.Value("invariants")) {
			xo := obj(x)
			if truthy(xo.Value("enforce")) && xo.Value("expr") != nil {
				exprs = true
			}
		}
	}
	if len(reasons) == 0 && exprs {
		holds, unfinished := subsumes(parent, effective, timeoutMs)
		switch {
		case unfinished:
			reasons = append(reasons, fmt.Sprintf("proof: the proof did not finish in %d ms", timeoutMs))
		case !holds:
			reasons = append(reasons, "proof: the child admits a step the parent does not")
		}
	}
	return &Result{Holds: len(reasons) == 0, Reasons: reasons, Child: effective, Inherited: inherited}
}

func subsumes(parent, child *pyjson.Object, timeoutMs int) (holds, unfinished bool) {
	pe, err1 := verify.ParseEnvelope(json.RawMessage(pathways.DumpJSON(parent)))
	ce, err2 := verify.ParseEnvelope(json.RawMessage(pathways.DumpJSON(child)))
	if err1 != nil || err2 != nil {
		return false, true
	}
	return verify.SubsumesStrict(pe, ce, timeoutMs)
}

// withKey is a copy of o with k set: a new key goes last, as
// model_copy(update=...) dumps a field that was absent in model order,
// and deadline is the last field.
func withKey(o *pyjson.Object, k string, v any) *pyjson.Object {
	out := pyjson.NewObject()
	for _, key := range o.Keys() {
		out.Set(key, o.Value(key))
	}
	out.Set(k, v)
	return out
}

// WithKey is withKey for the commands.
func WithKey(o *pyjson.Object, k string, v any) *pyjson.Object { return withKey(o, k, v) }

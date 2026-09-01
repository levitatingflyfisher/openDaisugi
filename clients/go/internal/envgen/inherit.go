package envgen

import (
	"fmt"
	"math/big"
	"sort"
	"strings"

	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// This file is inheritance.verify_inheritance: a child envelope must
// tighten its parent. Envelopes are validated model_dump() values.

const inheritStage = "inheritance"

// InheritanceError is EnvelopeInheritanceError: the violation messages.
type InheritanceError struct{ Messages []string }

func (e *InheritanceError) Error() string {
	return "inheritance violations: " + strings.Join(e.Messages, "; ")
}

func strList(v any) []string {
	xs, _ := v.([]any)
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func sortedSet(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}

func obj(v any) *pyjson.Object {
	o, _ := v.(*pyjson.Object)
	if o == nil {
		return pyjson.NewObject()
	}
	return o
}

func bigInt(v any) *big.Int {
	n := new(big.Int)
	switch x := v.(type) {
	case pyjson.Int:
		n.SetString(x.Text, 10)
	case int:
		n.SetInt64(int64(x))
	case int64:
		n.SetInt64(x)
	}
	return n
}

func floatOf(v any) (float64, bool) {
	switch x := v.(type) {
	case pyjson.Float:
		return float64(x), true
	case float64:
		return x, true
	case pyjson.Int:
		f, _ := new(big.Float).SetInt(bigInt(x)).Float64()
		return f, true
	}
	return 0, false
}

// VerifyInheritance is verify_inheritance(child, parent): the violation
// messages, none when the child tightens the parent.
func VerifyInheritance(child, parent *pyjson.Object) []string {
	var out []string
	if parent.Value("parent_envelope") != nil {
		out = append(out, "parent envelope has its own parent_envelope; v0.1.2 supports only depth-1 inheritance")
	}
	cp, pp := obj(child.Value("permissions")), obj(parent.Value("permissions"))
	subset := func(field string) {
		parentSet := map[string]bool{}
		for _, s := range strList(pp.Value(field)) {
			parentSet[s] = true
		}
		var extras []string
		for _, s := range strList(cp.Value(field)) {
			if !parentSet[s] {
				extras = append(extras, s)
			}
		}
		for _, x := range sortedSet(extras) {
			out = append(out, fmt.Sprintf("%s: child glob %s not in parent's allowed set %s",
				field, pystr.Repr(x), pystr.ReprList(sortedSet(strList(pp.Value(field))))))
		}
	}
	boolLE := func(field string) {
		if pyjson.Truthy(cp.Value(field)) && !pyjson.Truthy(pp.Value(field)) {
			out = append(out, field+": child=True relaxes parent=False")
		}
	}
	intLE := func(field string) {
		c, p := bigInt(cp.Value(field)), bigInt(pp.Value(field))
		if c.Cmp(p) > 0 {
			out = append(out, fmt.Sprintf("%s: child=%s exceeds parent=%s", field, c.String(), p.String()))
		}
	}
	subset("file_read")
	subset("file_write")
	boolLE("network")
	ph, ch := strList(pp.Value("network_hosts")), strList(cp.Value("network_hosts"))
	if len(ph) > 0 {
		if len(ch) == 0 {
			out = append(out, fmt.Sprintf("network_hosts: child is empty (means any host) but parent restricts to %s",
				pystr.ReprList(sortedSet(ph))))
		} else {
			ps := map[string]bool{}
			for _, h := range ph {
				ps[h] = true
			}
			var extras []string
			for _, h := range ch {
				if !ps[h] {
					extras = append(extras, h)
				}
			}
			for _, x := range sortedSet(extras) {
				out = append(out, fmt.Sprintf("network_hosts: child host %s not in parent's allowed set %s",
					pystr.Repr(x), pystr.ReprList(sortedSet(ph))))
			}
		}
	}
	boolLE("shell")
	subset("shell_allowlist")
	boolLE("shell_allow_decomposition")
	intLE("max_execution_time_s")
	intLE("max_output_size_mb")
	subset("mcp_allowlist")
	if r := robotRelaxed(pp, cp); r != "" {
		out = append(out, "robot capability relaxed: "+r)
	}
	rank := map[string]int{"low": 0, "medium": 1, "high": 2, "physical": 3}
	cs, _ := child.Value("stakes").(string)
	ps, _ := parent.Value("stakes").(string)
	if rank[cs] < rank[ps] {
		out = append(out, fmt.Sprintf("stakes: child '%s' downgrades parent '%s'", cs, ps))
	}
	for _, field := range []string{"invariants", "postconditions"} {
		childKeys := map[string]bool{}
		for _, m := range listOf(child.Value(field)) {
			childKeys[pathways.DumpJSON(m)] = true
		}
		var missing []string
		seen := map[string]bool{}
		for _, m := range listOf(parent.Value(field)) {
			k := pathways.DumpJSON(m)
			if !childKeys[k] && !seen[k] {
				seen[k] = true
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			// A frozenset's order follows the string hash, which varies
			// per process; both sides list the members sorted (K1-5).
			sort.Strings(missing)
			parts := make([]string, len(missing))
			for i, k := range missing {
				parts[i] = pystr.Repr(k)
			}
			out = append(out, fmt.Sprintf("%s: child is missing parent %s frozenset({%s})", field, field, strings.Join(parts, ", ")))
		}
	}
	return out
}

func listOf(v any) []any {
	xs, _ := v.([]any)
	return xs
}

// triple is one (x, y, z) of a box as Python prints a tuple of floats.
func triple(v any) ([3]float64, string) {
	var f [3]float64
	xs := listOf(v)
	parts := make([]string, len(xs))
	for i, x := range xs {
		if i < 3 {
			f[i], _ = floatOf(x)
		}
		parts[i] = pmodel.Repr(x)
	}
	return f, "(" + strings.Join(parts, ", ") + ")"
}

func boxRepr(v any) (lo, hi [3]float64, text string) {
	xs := listOf(v)
	if len(xs) != 2 {
		return lo, hi, pmodel.Repr(v)
	}
	lo, a := triple(xs[0])
	hi, b := triple(xs[1])
	return lo, hi, "(" + a + ", " + b + ")"
}

// robotRelaxed is subsumption._robot_capability_violation(outer, inner),
// worded as the oracle words it.
func robotRelaxed(outer, inner *pyjson.Object) string {
	if ob := outer.Value("workspace_bounds"); ob != nil {
		ib := inner.Value("workspace_bounds")
		if ib == nil {
			return "inner declares no workspace_bounds but outer constrains the workspace (undeclared = unbounded → denied)"
		}
		oMin, oMax, ot := boxRepr(ob)
		iMin, iMax, it := boxRepr(ib)
		for k := 0; k < 3; k++ {
			if iMin[k] < oMin[k] || iMax[k] > oMax[k] {
				return fmt.Sprintf("inner workspace_bounds %s exceed outer %s", it, ot)
			}
		}
	}
	for _, axis := range []string{"velocity_limit", "torque_limit"} {
		ov := outer.Value(axis)
		if ov == nil {
			continue
		}
		iv := inner.Value(axis)
		if iv == nil {
			return fmt.Sprintf("inner declares no %s but outer caps it (undeclared → denied)", axis)
		}
		o, _ := floatOf(ov)
		i, _ := floatOf(iv)
		if i > o {
			return fmt.Sprintf("inner %s %s exceeds outer %s", axis, pmodel.Repr(iv), pmodel.Repr(ov))
		}
	}
	oj, ij := obj(outer.Value("joint_limits")), obj(inner.Value("joint_limits"))
	for _, joint := range oj.Keys() {
		ir, ok := ij.Get(joint)
		if !ok {
			return fmt.Sprintf("inner does not bound joint %s that outer limits (undeclared → denied)", pystr.Repr(joint))
		}
		or := listOf(oj.Value(joint))
		irl := listOf(ir)
		if len(or) != 2 || len(irl) != 2 {
			continue
		}
		olo, _ := floatOf(or[0])
		ohi, _ := floatOf(or[1])
		ilo, _ := floatOf(irl[0])
		ihi, _ := floatOf(irl[1])
		if ilo < olo || ihi > ohi {
			return fmt.Sprintf("inner joint %s range (%s,%s) exceeds outer (%s,%s)", pystr.Repr(joint),
				pmodel.Repr(irl[0]), pmodel.Repr(irl[1]), pmodel.Repr(or[0]), pmodel.Repr(or[1]))
		}
	}
	freeze := func(v any) map[string]bool {
		m := map[string]bool{}
		for _, b := range listOf(v) {
			lo, hi, _ := boxRepr(b)
			m[fmt.Sprint(lo, hi)] = true
		}
		return m
	}
	o, i := freeze(outer.Value("obstacles")), freeze(inner.Value("obstacles"))
	missing := 0
	for k := range o {
		if !i[k] {
			missing++
		}
	}
	if missing > 0 {
		return fmt.Sprintf("inner omits %d obstacle region(s) the outer forbids (undeclared forbidden region → denied)", missing)
	}
	return ""
}

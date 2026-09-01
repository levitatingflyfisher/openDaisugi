package distill

import (
	"sort"
	"strings"

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/tracejournal"
	"daisugi-verify/internal/verify"
)

// This file is pathway_params: typed data holes (ADR-0008) and the
// do-nothing salvage of divergent positions.

// capField is _CAP_FIELD: the one field a step type parameterizes.
var capField = map[string]string{"file_read": "path", "file_write": "path", "network": "url"}

// leafValueField is _LEAF_VALUE_FIELD.
var leafValueField = map[string]string{"shell": "command", "file_read": "path", "file_write": "path", "network": "url"}

// leafTools is _LEAF_TOOLS.
var leafTools = map[string][]string{
	"shell":      {"Bash"},
	"file_read":  {"Read", "Glob", "Grep"},
	"file_write": {"Read", "Write", "Edit"},
	"network":    {"WebFetch"},
}

const maxLeafVariants = 5

// dirname is posixpath.dirname.
func dirname(p string) string {
	i := strings.LastIndex(p, "/") + 1
	head := p[:i]
	if head != "" && head != strings.Repeat("/", len(head)) {
		head = strings.TrimRight(head, "/")
	}
	return head
}

// capabilityHead is _capability_head: the directory of a path, the
// scheme and host of a URL; ok false where Python returns None.
func capabilityHead(stepType, value string) (string, bool) {
	switch stepType {
	case "file_read", "file_write":
		d := dirname(value)
		return d, d != ""
	case "network":
		scheme, netloc, ok := urlsplit(value)
		if !ok || netloc == "" {
			return "", false
		}
		return scheme + "://" + netloc, true
	}
	return "", false
}

// urlsplit is the scheme and netloc of urllib.parse.urlsplit (Python
// 3.12). ok is false where it raises ValueError.
func urlsplit(url string) (scheme, netloc string, ok bool) {
	url = strings.TrimLeft(url, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\x0b\x0c\r\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f ")
	for _, c := range []string{"\t", "\r", "\n"} {
		url = strings.ReplaceAll(url, c, "")
	}
	if i := strings.Index(url, ":"); i > 0 {
		cand := url[:i]
		valid := isAlpha(cand[0])
		for j := 0; j < len(cand) && valid; j++ {
			c := cand[j]
			valid = isAlpha(c) || c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'
		}
		if valid {
			scheme = strings.ToLower(cand)
			url = url[i+1:]
		}
	}
	if strings.HasPrefix(url, "//") {
		rest := url[2:]
		end := len(rest)
		for _, d := range "/?#" {
			if k := strings.IndexRune(rest, d); k >= 0 && k < end {
				end = k
			}
		}
		netloc = rest[:end]
		if strings.Contains(netloc, "[") != strings.Contains(netloc, "]") {
			return "", "", false
		}
		if strings.Contains(netloc, "[") && !strings.Contains(netloc[strings.Index(netloc, "["):], "]") {
			return "", "", false
		}
	}
	return scheme, netloc, true
}

func isAlpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// orderedPlans is [topological_order(p) for p in plans]; the error is
// the one Python raises for the first plan that fails.
func orderedPlans(plans []*pyjson.Object) ([][]*pyjson.Object, error) {
	out := make([][]*pyjson.Object, len(plans))
	for i, p := range plans {
		o, err := tracejournal.TopoOrderErr(p)
		if err != nil {
			return nil, err
		}
		out[i] = o
	}
	return out, nil
}

func signature(steps []*pyjson.Object) string {
	types := make([]string, len(steps))
	for i, s := range steps {
		types[i] = s.Value("type").(string)
	}
	return strings.Join(types, "→")
}

func sameShape(ordered [][]*pyjson.Object) bool {
	for _, o := range ordered[1:] {
		if signature(o) != signature(ordered[0]) {
			return false
		}
	}
	return true
}

func strField(s *pyjson.Object, f string) (string, bool) {
	v, ok := s.Get(f)
	if !ok {
		return "", false
	}
	str, isStr := v.(string)
	return str, isStr
}

func sortedSet(vals []string) []string {
	set := map[string]bool{}
	for _, v := range vals {
		set[v] = true
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func parameter(step0 *pyjson.Object, i int, field, head string, values []string) *pyjson.Object {
	id := step0.Value("id").(string)
	obs := []any{}
	for _, v := range sortedSet(values) {
		obs = append(obs, v)
	}
	return pyjson.NewObject().Set("name", id+"."+field).Set("step_index", pyjson.Int{Text: itoa(i)}).
		Set("step_id", id).Set("field", field).Set("head", head).Set("observed", obs)
}

func itoa(i int) string {
	return pyjson.Dumps(i, true)
}

// headsOf is {_capability_head(t, v) for v in values}: the one head, or
// ok false when the heads differ or one is None.
func headsOf(stepType string, values []string) (string, bool) {
	head, first := "", true
	for _, v := range values {
		h, ok := capabilityHead(stepType, v)
		if !ok {
			return "", false
		}
		if first {
			head, first = h, false
		} else if h != head {
			return "", false
		}
	}
	return head, true
}

// fieldValues is [getattr(steps[i], field, None) for steps in ordered];
// ok false when any is None.
func fieldValues(ordered [][]*pyjson.Object, i int, field string) ([]string, bool) {
	vals := make([]string, len(ordered))
	for k, steps := range ordered {
		v, ok := strField(steps[i], field)
		if !ok {
			return nil, false
		}
		vals[k] = v
	}
	return vals, true
}

func allEqual(vals []string) bool {
	for _, v := range vals[1:] {
		if v != vals[0] {
			return false
		}
	}
	return true
}

// DiffPlansForParameters is diff_plans_for_parameters.
func DiffPlansForParameters(plans []*pyjson.Object) ([]any, error) {
	if len(plans) < 2 {
		return []any{}, nil
	}
	ordered, err := orderedPlans(plans)
	if err != nil {
		return nil, err
	}
	if !sameShape(ordered) {
		return []any{}, nil
	}
	params := []any{}
	for i, step0 := range ordered[0] {
		field, ok := capField[step0.Value("type").(string)]
		if !ok {
			continue
		}
		vals, ok := fieldValues(ordered, i, field)
		if !ok || allEqual(vals) {
			continue
		}
		head, ok := headsOf(step0.Value("type").(string), vals)
		if !ok {
			return []any{}, nil
		}
		params = append(params, parameter(step0, i, field, head, vals))
	}
	return params, nil
}

// RekeyToTemplate is rekey_to_template.
func RekeyToTemplate(params []any, template *pyjson.Object) ([]any, error) {
	if len(params) == 0 {
		return []any{}, nil
	}
	steps, err := tracejournal.TopoOrderErr(template)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, x := range params {
		p := x.(*pyjson.Object)
		idx := intOf(p.Value("step_index"))
		if idx >= len(steps) {
			return []any{}, nil
		}
		t := steps[idx]
		if capField[t.Value("type").(string)] != p.Value("field") {
			return []any{}, nil
		}
		q := pyjson.NewObject()
		for _, k := range p.Keys() {
			q.Set(k, p.Value(k))
		}
		q.Set("step_id", t.Value("id"))
		out = append(out, q)
	}
	return out, nil
}

func intOf(v any) int {
	n := 0
	for _, c := range v.(pyjson.Int).Text {
		n = n*10 + int(c-'0')
	}
	return n
}

// PlanDivergence is plan_divergence: the divergent positions and the
// typed holes.
func PlanDivergence(plans []*pyjson.Object) ([]int, []any, error) {
	if len(plans) < 2 {
		return nil, []any{}, nil
	}
	ordered, err := orderedPlans(plans)
	if err != nil {
		return nil, nil, err
	}
	if !sameShape(ordered) {
		return nil, []any{}, nil
	}
	var divergent []int
	params := []any{}
	for i, step0 := range ordered[0] {
		t := step0.Value("type").(string)
		field, ok := leafValueField[t]
		if !ok {
			continue
		}
		vals, ok := fieldValues(ordered, i, field)
		if !ok || allEqual(vals) {
			continue
		}
		if t == "shell" {
			divergent = append(divergent, i)
			continue
		}
		head, ok := headsOf(t, vals)
		if !ok {
			divergent = append(divergent, i)
			continue
		}
		params = append(params, parameter(step0, i, field, head, vals))
	}
	if len(divergent) > 0 && len(divergent) >= len(ordered[0]) {
		return nil, []any{}, nil
	}
	return divergent, params, nil
}

// SalvageWorkspace is salvage_workspace: the workspace of a salvaged
// leaf, the fixed prefix of a file_read glob. The prefix is the glob's
// leading segments before the first one with a glob character; a glob with
// none names one file and gives no prefix. The first prefix the globs admit
// under verify's own path matcher wins; a glob too complex to match gives
// none. false when no glob gives one.
func SalvageWorkspace(fileRead []string) (string, bool) {
	for _, glob := range fileRead {
		segs := strings.Split(glob, "/")
		n := 0
		for n < len(segs) && !verify.HasGlobChars(segs[n]) {
			n++
		}
		if n == len(segs) {
			continue
		}
		prefix := strings.Join(segs[:n], "/")
		if prefix == "" && strings.HasPrefix(glob, "/") {
			prefix = "/"
		}
		workspace := verify.Normpath(prefix)
		if admitted(workspace, fileRead) {
			return workspace, true
		}
	}
	return "", false
}

// admitted is _path_matches_any, a glob too complex to match read as no
// match.
func admitted(path string, globs []string) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			if _, tooComplex := r.(verify.GlobTooComplex); !tooComplex {
				panic(r)
			}
			ok = false
		}
	}()
	return verify.PathMatchesAny(path, globs)
}

// BuildDelegatedTemplate is build_delegated_template: the representative
// with each divergent position an AgenticStep leaf. The plan gets a fresh
// id, as ActionPlan's default does.
func BuildDelegatedTemplate(representative *pyjson.Object, divergent []int, plans []*pyjson.Object, workspace string) (*pyjson.Object, error) {
	ordered, err := tracejournal.TopoOrderErr(representative)
	if err != nil {
		return nil, err
	}
	all, err := orderedPlans(plans)
	if err != nil {
		return nil, err
	}
	isDiv := map[int]bool{}
	for _, d := range divergent {
		isDiv[d] = true
	}
	steps := []any{}
	for i, step := range ordered {
		if !isDiv[i] {
			steps = append(steps, step)
			continue
		}
		t := step.Value("type").(string)
		field := leafValueField[t]
		var vals []string
		for _, s := range all {
			if i < len(s) {
				v, _ := s[i].Get(field)
				vals = append(vals, pyStrValue(v))
			}
		}
		variants := sortedSet(vals)
		shown := variants
		if len(shown) > maxLeafVariants {
			shown = shown[:maxLeafVariants]
		}
		more := ""
		if len(variants) > len(shown) {
			more = " (+" + itoa(len(variants)-len(shown)) + " more)"
		}
		prompt := "Perform this step of the task. In past successful runs it was one of: " +
			strings.Join(shown, "; ") + more + ". Choose and execute the right equivalent for " +
			"the current task; stay within the granted tools and permissions."
		tools := []any{}
		for _, x := range leafTools[t] {
			tools = append(tools, x)
		}
		deps := append([]any{}, step.Value("depends_on").([]any)...)
		leaf := pyjson.NewObject().Set("id", step.Value("id")).Set("depends_on", deps).
			Set("type", "agentic").Set("prompt", prompt).Set("workspace", workspace).Set("tools", tools)
		v, verr := pmodel.StepTypes["agentic"].ValidateObject(leaf, pmodel.Python)
		if verr != nil {
			return nil, verr
		}
		steps = append(steps, v)
	}
	plan := pyjson.NewObject().Set("source", "distiller-salvage").Set("task", representative.Value("task")).Set("steps", steps)
	out, verr := pmodel.ActionPlan.ValidateObject(plan, pmodel.Python)
	if verr != nil {
		return nil, verr
	}
	return out, nil
}

// pyStrValue is str() of a step field's value.
func pyStrValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return pmodel.Repr(v)
}

// stringList is a validated list[str] value as Go strings.
func stringList(v any) []string {
	xs, _ := v.([]any)
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

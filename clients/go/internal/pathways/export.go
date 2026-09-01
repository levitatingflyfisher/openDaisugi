package pathways

import (
	"fmt"
	"math"
	"strings"
	"time"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// Formats is portability._SUPPORTED_FORMATS, in its order.
var Formats = []string{"json", "skill", "mermaid", "md", "smtlib"}

// BundleSchemaVersion is portability.BUNDLE_SCHEMA_VERSION.
const BundleSchemaVersion = 1

// Export is portability.export(pathway, fmt). version is what the bundle
// names as opendaisugi_version.
func Export(p *Pathway, format, version string) (string, error) {
	switch format {
	case "json":
		bundle := pyjson.NewObject().
			Set("opendaisugi_version", version).
			Set("schema_version", pyjson.Int{Text: "1"}).
			Set("pathway", JSONMode(p.Obj))
		return pyjson.DumpsIndent(bundle, 2, true), nil
	case "mermaid":
		return exportMermaid(p), nil
	case "md":
		return exportMD(p)
	case "smtlib":
		return exportSMTLIB(p, version), nil
	case "skill":
		return exportSkill(p, version)
	}
	return "", fmt.Errorf("unknown format %q", format)
}

// JSONMode is model_dump(mode="json"): the same values, with NaN and the
// infinities as None (pydantic's ser_json_inf_nan).
func JSONMode(v any) any {
	switch x := v.(type) {
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil
		}
	case pyjson.Float:
		f := float64(x)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil
		}
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = JSONMode(e)
		}
		return out
	case *pyjson.Object:
		out := pyjson.NewObject()
		for _, k := range x.Keys() {
			out.Set(k, JSONMode(x.Value(k)))
		}
		return out
	}
	return v
}

func obj(v any) *pyjson.Object { return v.(*pyjson.Object) }

func strList(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// pyBool is str(bool).
func pyBool(v any) string {
	if v.(bool) {
		return "True"
	}
	return "False"
}

// orEmptyList is f"{xs or '[]'}": the list's repr, or [] when empty.
func orEmptyList(v any) string {
	xs := strList(v)
	if len(xs) == 0 {
		return "[]"
	}
	return pystr.ReprList(xs)
}

// pyStr is str(v) for the plain values a step field holds.
func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case pyjson.Int:
		return x.Text
	case bool:
		return pyBool(x)
	case float64:
		return pyjson.FloatRepr(x)
	}
	return fmt.Sprint(v)
}

func truthy(v any) bool { return pyjson.Truthy(v) }

// stepDetail is portability._step_detail.
func stepDetail(step *pyjson.Object) string {
	kind, _ := step.Value("type").(string)
	get := func(k string) (any, bool) { return step.Get(k) }
	switch kind {
	case "task":
		if v, ok := get("prompt"); ok && truthy(v) {
			return pyStr(v)
		}
		return ""
	case "skill":
		if v, ok := get("skill_id"); ok && truthy(v) {
			return pyStr(v)
		}
		return ""
	case "mcp":
		server, _ := get("server")
		tool, _ := get("tool")
		return pyStr(server) + "/" + pyStr(tool)
	}
	for _, k := range []string{"command", "path", "url"} {
		if v, ok := get(k); ok && truthy(v) {
			return pyStr(v)
		}
	}
	return ""
}

func steps(p *Pathway) []*pyjson.Object {
	var out []*pyjson.Object
	for _, s := range obj(p.Obj.Value("plan_template")).Value("steps").([]any) {
		out = append(out, obj(s))
	}
	return out
}

func mermaidEscape(s string) string {
	s = strings.ReplaceAll(s, `"`, "&quot;")
	s = strings.ReplaceAll(s, "|", `\|`)
	return pystr.Slice(s, 0, 120)
}

func exportMermaid(p *Pathway) string {
	lines := []string{"```mermaid", "flowchart TD"}
	for _, st := range steps(p) {
		id := st.Value("id").(string)
		label := id + "<br/>" + st.Value("type").(string)
		if d := stepDetail(st); d != "" {
			label += "<br/><code>" + mermaidEscape(d) + "</code>"
		}
		lines = append(lines, "    "+id+"["+label+"]")
	}
	for _, st := range steps(p) {
		for _, dep := range strList(st.Value("depends_on")) {
			lines = append(lines, "    "+dep+" --> "+st.Value("id").(string))
		}
	}
	lines = append(lines, "```")
	perms := obj(obj(p.Obj.Value("envelope")).Value("permissions"))
	lines = append(lines, "", "### Permissions",
		"- shell: "+pyBool(perms.Value("shell"))+" (allowlist: "+orEmptyList(perms.Value("shell_allowlist"))+")",
		"- file_read: "+orEmptyList(perms.Value("file_read")),
		"- file_write: "+orEmptyList(perms.Value("file_write")),
		"- network: "+pyBool(perms.Value("network"))+" (hosts: "+orEmptyList(perms.Value("network_hosts"))+")")
	return strings.Join(lines, "\n")
}

// gmtime is time.strftime('%Y-%m-%d %H:%M:%S UTC', time.gmtime(t)): the
// time rounded down to a second.
func gmtime(t float64) (string, error) {
	if math.IsNaN(t) || math.IsInf(t, 0) || math.Abs(t) > 253402300799 {
		return "", &Invalid{"time.gmtime cannot convert distilled_at"}
	}
	sec := math.Floor(t)
	u := time.Unix(int64(sec), 0).UTC()
	if u.Year() < 1 {
		return "", fmt.Errorf("%w: distilled_at is before year 1", ErrUnreadable)
	}
	return fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d UTC", u.Year(), int(u.Month()), u.Day(), u.Hour(), u.Minute(), u.Second()), nil
}

func exportMD(p *Pathway) (string, error) {
	env := obj(p.Obj.Value("envelope"))
	perms := obj(env.Value("permissions"))
	when, err := gmtime(p.Obj.Value("distilled_at").(float64))
	if err != nil {
		return "", err
	}
	model := p.Obj.Value("embedding_model").(string)
	if model == "" {
		model = "(unspecified)"
	}
	parts := []string{
		"# Pathway: " + p.Task(),
		"",
		"- **ID:** `" + p.ID() + "`",
		"- **Distilled at:** " + when,
		"- **Hit count:** " + pyStr(p.Obj.Value("hit_count")),
		fmt.Sprintf("- **Source traces:** %d", len(p.Obj.Value("source_trace_ids").([]any))),
		"- **Embedding model:** " + model,
		"",
		"## Envelope",
		"",
		"- **Generator:** " + env.Value("generated_by").(string),
		"- **Permissions:**",
		"  - shell: " + pyBool(perms.Value("shell")) + " (allowlist: " + orEmptyList(perms.Value("shell_allowlist")) + ")",
		"  - file_read: " + orEmptyList(perms.Value("file_read")),
		"  - file_write: " + orEmptyList(perms.Value("file_write")),
		"  - network: " + pyBool(perms.Value("network")) + " (hosts: " + orEmptyList(perms.Value("network_hosts")) + ")",
	}
	if invs := env.Value("invariants").([]any); len(invs) > 0 {
		parts = append(parts, "- **Invariants:**")
		for _, i := range invs {
			inv := obj(i)
			parts = append(parts, "  - `"+pyStr(inv.Value("type"))+"`: "+pyStr(inv.Value("description")))
		}
	}
	if pcs := env.Value("postconditions").([]any); len(pcs) > 0 {
		parts = append(parts, "- **Postconditions:**")
		for _, x := range pcs {
			pc := obj(x)
			parts = append(parts, "  - `"+pyStr(pc.Value("type"))+"` → path="+pyStr(pc.Value("path"))+" expected="+pyStr(pc.Value("expected")))
		}
	}
	parts = append(parts, "", "## Plan template", "")
	for _, st := range steps(p) {
		parts = append(parts, "- `"+st.Value("id").(string)+"` ("+st.Value("type").(string)+"): `"+stepDetail(st)+"`")
		if deps := strList(st.Value("depends_on")); len(deps) > 0 {
			parts = append(parts, "  - depends on: "+strings.Join(deps, ", "))
		}
	}
	return strings.Join(parts, "\n") + "\n", nil
}

// exportSMTLIB is portability._export_smtlib: a comment header, then what
// z3py's Solver.to_smt2() prints for the four kinds of assertion it adds.
func exportSMTLIB(p *Pathway, version string) string {
	env := obj(p.Obj.Value("envelope"))
	perms := obj(env.Value("permissions"))
	var b strings.Builder
	fmt.Fprintf(&b, ";; openDaisugi pathway proof artifact\n;; pathway_id: %s\n;; task: %s\n;; envelope_id: %s\n;; opendaisugi_version: %s\n",
		p.ID(), p.Task(), env.Value("id").(string), version)
	b.WriteString("; benchmark generated from python API\n(set-info :status unknown)\n")
	b.WriteString("(declare-fun shell () Bool)\n(declare-fun can_write () Bool)\n")
	lower := func(v bool) string {
		if v {
			return "true"
		}
		return "false"
	}
	assert := func(name string, v bool) { fmt.Fprintf(&b, "(assert\n (= %s %s))\n", name, lower(v)) }
	assert("shell", perms.Value("shell").(bool))
	assert("can_write", len(perms.Value("file_write").([]any)) > 0)
	if len(perms.Value("shell_allowlist").([]any)) > 0 {
		assert("shell", true)
	}
	for _, x := range env.Value("postconditions").([]any) {
		if obj(x).Value("type") == "file_exists" {
			assert("can_write", true)
		}
	}
	b.WriteString("(check-sat)\n")
	return b.String()
}

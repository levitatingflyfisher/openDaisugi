package aliases

import "daisugi-verify/internal/pyjson"

// systemSource is system_aliases.py's seven aliases, in its order, each
// body as the raw dict it stores (typed placeholders such as
// `$max_scale` survive until substitution).
const systemSource = `[
{"name": "velocity_scale_bounded", "params": ["max_scale"],
 "description": "forall joint_move / cartesian_move steps: velocity_scale <= max_scale",
 "expr": {"op": "forall_steps", "pred": {"op": "implies",
   "a": {"op": "in_set", "path": "type", "values": ["joint_move", "cartesian_move"]},
   "b": {"op": "numeric_range", "path": "velocity_scale", "min": 0.0, "max": "$max_scale"}}}},
{"name": "never_impersonates", "params": ["principal"],
 "description": "outbound communication steps must not sign as the principal",
 "expr": {"op": "forall_steps", "pred": {"op": "implies",
   "a": {"op": "in_set", "path": "type", "values": ["email_send", "imessage_send", "shell"]},
   "b": {"op": "and", "children": [
     {"op": "not_equals", "path": "metadata.signature", "value": "$principal"},
     {"op": "not_matches", "path": "metadata.body", "regex": "(?i)(\\u2014|-)\\s*$principal"}]}}}},
{"name": "no_pii_regex", "params": [],
 "description": "forbid PII patterns (SSN, credit card) in content or metadata.body",
 "expr": {"op": "forall_steps", "pred": {"op": "and", "children": [
   {"op": "not_matches", "path": "content", "regex": "\\b\\d{3}-\\d{2}-\\d{4}\\b"},
   {"op": "not_matches", "path": "content", "regex": "\\b(?:\\d[ -]*?){13,16}\\b"},
   {"op": "not_matches", "path": "metadata.body", "regex": "\\b\\d{3}-\\d{2}-\\d{4}\\b"},
   {"op": "not_matches", "path": "metadata.body", "regex": "\\b(?:\\d[ -]*?){13,16}\\b"}]}}},
{"name": "no_secrets", "params": [],
 "description": "forbid common secret patterns (AWS keys, JWT, ssh private keys)",
 "expr": {"op": "forall_steps", "pred": {"op": "and", "children": [
   {"op": "not_matches", "path": "content", "regex": "AKIA[0-9A-Z]{16}"},
   {"op": "not_matches", "path": "content",
    "regex": "eyJ[A-Za-z0-9_-]{10,}\\.[A-Za-z0-9_-]{10,}\\.[A-Za-z0-9_-]{10,}"},
   {"op": "not_matches", "path": "content", "regex": "-----BEGIN (RSA |OPENSSH |EC )?PRIVATE KEY-----"},
   {"op": "not_matches", "path": "metadata.body", "regex": "AKIA[0-9A-Z]{16}"},
   {"op": "not_matches", "path": "metadata.body",
    "regex": "-----BEGIN (RSA |OPENSSH |EC )?PRIVATE KEY-----"}]}}},
{"name": "pytest_passes", "params": [],
 "description": "POSTCONDITION-ONLY: at least one completed step's output matches /passed/. Do NOT use as an Invariant — metadata.output is unset at Stage 1 verify.",
 "expr": {"op": "exists_step", "pred": {"op": "matches", "path": "metadata.output", "regex": "passed"}}},
{"name": "no_network_writes", "params": [],
 "description": "reject any network step that is not a GET",
 "expr": {"op": "forall_steps", "pred": {"op": "implies",
   "a": {"op": "equals", "path": "type", "value": "network"},
   "b": {"op": "equals", "path": "method", "value": "GET"}}}},
{"name": "structured_approval", "params": [],
 "description": "council-member outputs must be structured approval JSON",
 "expr": {"op": "forall_steps", "pred": {"op": "matches", "path": "metadata.output",
   "regex": "^\\s*\\{[^}]*\"approve\"\\s*:\\s*(true|false)"}}}
]`

// System is system_aliases' aliases, in the order load_system_aliases
// registers them.
func System() []Alias {
	v, err := pyjson.Loads(systemSource)
	if err != nil {
		panic("system aliases: " + err.Error())
	}
	var out []Alias
	for _, x := range v.([]any) {
		o := x.(*pyjson.Object)
		var params []string
		for _, p := range o.Value("params").([]any) {
			params = append(params, p.(string))
		}
		out = append(out, Alias{Name: o.Value("name").(string), Params: params, Expr: o.Value("expr"),
			Tier: "system", Description: o.Value("description").(string)})
	}
	return out
}

// LoadSystem is load_system_aliases: each system alias registered in
// order; the first refusal is returned.
func LoadSystem(r *Registry) error {
	for _, a := range System() {
		if err := r.Register(a); err != nil {
			return err
		}
	}
	return nil
}

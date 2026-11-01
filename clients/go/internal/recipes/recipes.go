// Package recipes holds the four recipe files and reads them as the
// oracle's opendaisugi.recipes does: a recipe is a JSON file whose steps
// each name one install primitive. The files are copied byte for byte from
// src/opendaisugi/recipes/; a test checks that.
package recipes

import (
	"embed"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"daisugi-verify/internal/install"
	"daisugi-verify/internal/pyjson"
)

//go:embed assets/*.json
var assets embed.FS

// StepKinds is recipes.STEP_KINDS.
var StepKinds = []string{"install", "harness", "config", "warden-init", "note"}

// ConfigKeys is recipes.CONFIG_KEYS: the keys a config step may set.
var ConfigKeys = []string{"routing_mode", "routing_model", "router_local_model",
	"switchyard_efficient_model", "switchyard_capable_model", "switchyard_api_key_env"}

// RoutingModes is recipes.ROUTING_MODES.
var RoutingModes = []string{"rules", "switchyard", "knn", "model", "off"}

var paramRE = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Names is recipes.names(): the shipped recipe names, sorted.
func Names() []string {
	entries, _ := assets.ReadDir("assets")
	var out []string
	for _, e := range entries {
		out = append(out, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(out)
	return out
}

// Text is the bytes of a shipped recipe file.
func Text(name string) (string, bool) {
	b, err := assets.ReadFile("assets/" + name + ".json")
	return string(b), err == nil
}

// Recipe is a validated recipe, as the file holds it.
type Recipe struct {
	Obj *pyjson.Object
}

func str(o *pyjson.Object, k string) string {
	s, _ := o.Value(k).(string)
	return s
}

// Name, Title and Doc are the recipe's strings.
func (r *Recipe) Name() string  { return str(r.Obj, "name") }
func (r *Recipe) Title() string { return str(r.Obj, "title") }
func (r *Recipe) Doc() string   { return str(r.Obj, "doc") }

// Slots is the slot table, in order: (slot, filled by).
func (r *Recipe) Slots() [][2]string {
	o, _ := r.Obj.Value("slots").(*pyjson.Object)
	var out [][2]string
	for _, k := range o.Keys() {
		s, _ := o.Value(k).(string)
		out = append(out, [2]string{k, s})
	}
	return out
}

// ParamNames is the params in file order.
func (r *Recipe) ParamNames() []string {
	o, _ := r.Obj.Value("params").(*pyjson.Object)
	return o.Keys()
}

// Param is one param's object.
func (r *Recipe) Param(name string) *pyjson.Object {
	o, _ := r.Obj.Value("params").(*pyjson.Object)
	p, _ := o.Value(name).(*pyjson.Object)
	return p
}

// Steps is the steps, each an object.
func (r *Recipe) Steps() []*pyjson.Object {
	list, _ := r.Obj.Value("steps").([]any)
	out := make([]*pyjson.Object, len(list))
	for i, s := range list {
		out[i], _ = s.(*pyjson.Object)
	}
	return out
}

func bad(file, where, why string) error { return fmt.Errorf("%s: %s: %s", file, where, why) }

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func checkStrings(file, where string, v any, declared map[string]bool) error {
	switch x := v.(type) {
	case string:
		for _, m := range paramRE.FindAllStringSubmatch(x, -1) {
			if !declared[m[1]] {
				return bad(file, where, "param '"+m[1]+"' is not declared")
			}
		}
	case []any:
		for i, e := range x {
			if err := checkStrings(file, fmt.Sprintf("%s[%d]", where, i), e, declared); err != nil {
				return err
			}
		}
	case *pyjson.Object:
		for _, k := range x.Keys() {
			if err := checkStrings(file, where+"."+k, x.Value(k), declared); err != nil {
				return err
			}
		}
	}
	return nil
}

func isStrList(v any) bool {
	list, ok := v.([]any)
	if !ok {
		return false
	}
	for _, e := range list {
		if _, ok := e.(string); !ok {
			return false
		}
	}
	return true
}

// Load is recipes.load: the recipe called name, validated.
func Load(name string) (*Recipe, error) {
	file := name + ".json"
	text, ok := Text(name)
	if !ok {
		return nil, fmt.Errorf("%s: no such recipe", file)
	}
	v, err := pyjson.Loads(text)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", file, err)
	}
	return Validate(v, file)
}

// Validate is recipes._validate.
func Validate(v any, file string) (*Recipe, error) {
	raw, ok := v.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("%s: the recipe is not an object", file)
	}
	for _, k := range []string{"name", "title", "doc"} {
		if _, ok := raw.Value(k).(string); !ok {
			return nil, bad(file, k, "must be a string")
		}
	}
	if _, ok := raw.Value("slots").(*pyjson.Object); !ok {
		return nil, bad(file, "slots", "must be an object")
	}
	params, ok := raw.Value("params").(*pyjson.Object)
	if !ok {
		return nil, bad(file, "params", "must be an object")
	}
	declared := map[string]bool{}
	for _, pn := range params.Keys() {
		declared[pn] = true
		p, ok := params.Value(pn).(*pyjson.Object)
		if !ok {
			return nil, bad(file, "params."+pn, "needs a help string")
		}
		if _, ok := p.Value("help").(string); !ok {
			return nil, bad(file, "params."+pn, "needs a help string")
		}
	}
	steps, ok := raw.Value("steps").([]any)
	if !ok {
		return nil, bad(file, "steps", "must be a list")
	}
	var known []string
	for _, c := range install.Components {
		known = append(known, c.Name)
	}
	for i, sv := range steps {
		where := fmt.Sprintf("steps[%d]", i)
		step, ok := sv.(*pyjson.Object)
		if !ok {
			return nil, bad(file, where, "must be an object")
		}
		kind, _ := step.Value("kind").(string)
		if !contains(StepKinds, kind) {
			return nil, fmt.Errorf("%s: %s.kind '%s' is not one of %s", file, where, pyStr(step.Value("kind")), strings.Join(StepKinds, ", "))
		}
		if w, has := step.Get("when"); has && w != nil {
			ws, _ := w.(string)
			if !declared[ws] {
				return nil, bad(file, where+".when", "param '"+ws+"' is not declared")
			}
		}
		switch kind {
		case "install":
			only, isList := step.Value("only").([]any)
			if !isList || len(only) == 0 || !isStrList(only) {
				return nil, bad(file, where+".only", "must be a list of component names")
			}
			for _, x := range only {
				if !contains(known, x.(string)) {
					return nil, bad(file, where+".only", "unknown component '"+x.(string)+"'")
				}
			}
			if rt, has := step.Get("runtime"); has && !isStrList(rt) {
				return nil, bad(file, where+".runtime", "must be a list of runtime names")
			}
			if en, has := step.Get("enforce"); has {
				if _, ok := en.(bool); !ok {
					return nil, bad(file, where+".enforce", "must be true or false")
				}
			}
			for _, k := range []string{"report", "base_url"} {
				if x, has := step.Get(k); has && x != nil {
					if _, ok := x.(string); !ok {
						return nil, bad(file, where+"."+k, "must be a string or null")
					}
				}
			}
		case "harness":
			n, _ := step.Value("name").(string)
			if n != "pi" && n != "opencode" {
				return nil, bad(file, where+".name", "'"+pyStr(step.Value("name"))+"' is not one of pi, opencode")
			}
		case "config":
			sets, isObj := step.Value("set").(*pyjson.Object)
			if !isObj || sets.Len() == 0 {
				return nil, bad(file, where+".set", "must be an object of config keys")
			}
			for _, k := range sets.Keys() {
				if !contains(ConfigKeys, k) {
					return nil, bad(file, where+".set."+k, "'"+k+"' is not a key a recipe may set")
				}
				if _, ok := sets.Value(k).(string); !ok {
					return nil, bad(file, where+".set."+k, "must be a string")
				}
			}
		case "warden-init":
			if _, ok := step.Value("workspace").(string); !ok {
				return nil, bad(file, where+".workspace", "must be a string")
			}
		case "note":
			if _, ok := step.Value("text").(string); !ok {
				return nil, bad(file, where+".text", "must be a string")
			}
		}
		rest := pyjson.NewObject()
		for _, k := range step.Keys() {
			if k != "kind" {
				rest.Set(k, step.Value(k))
			}
		}
		if err := checkStrings(file, where, rest, declared); err != nil {
			return nil, err
		}
	}
	return &Recipe{Obj: raw}, nil
}

func pyStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pyjson.Dumps(v, true)
}

// StepSummary is recipes._step_summary.
func StepSummary(step *pyjson.Object) string {
	switch str(step, "kind") {
	case "install":
		var parts []string
		var only []string
		for _, x := range step.Value("only").([]any) {
			only = append(only, x.(string))
		}
		parts = append(parts, "--only "+strings.Join(only, ","))
		if rt, _ := step.Value("runtime").([]any); len(rt) > 0 {
			var names []string
			for _, x := range rt {
				names = append(names, x.(string))
			}
			parts = append(parts, "--runtime "+strings.Join(names, ","))
		}
		if pyjson.Truthy(step.Value("enforce")) {
			parts = append(parts, "--enforce")
		}
		if s := str(step, "report"); s != "" {
			parts = append(parts, "--report "+s)
		}
		if s := str(step, "base_url"); s != "" {
			parts = append(parts, "--base-url "+s)
		}
		return strings.Join(parts, " ")
	case "harness":
		return str(step, "name")
	case "config":
		sets, _ := step.Value("set").(*pyjson.Object)
		var parts []string
		for _, k := range sets.Keys() {
			parts = append(parts, k+"="+str(sets, k))
		}
		return strings.Join(parts, " ")
	case "warden-init":
		return "workspace=" + str(step, "workspace")
	}
	return str(step, "text")
}

// Metavar is recipes._metavar.
func (r *Recipe) Metavar(param string) string {
	if p := r.Param(param); p != nil {
		if m, ok := p.Value("metavar").(string); ok {
			return m
		}
	}
	return "VALUE"
}

// RenderShow is recipes.render_show.
func (r *Recipe) RenderShow() string {
	lines := []string{r.Title(), "", "Slots"}
	for _, s := range r.Slots() {
		lines = append(lines, "  "+s[0]+": "+s[1])
	}
	if names := r.ParamNames(); len(names) > 0 {
		lines = append(lines, "", "Params")
		for _, pn := range names {
			p := r.Param(pn)
			tag := "optional"
			if pyjson.Truthy(p.Value("required")) {
				tag = "required"
			} else if d, has := p.Get("default"); has {
				tag = "default " + pyStr(d)
			}
			lines = append(lines, fmt.Sprintf("  %s (%s): %s", pn, tag, str(p, "help")))
		}
	}
	lines = append(lines, "", "Steps")
	for i, step := range r.Steps() {
		line := fmt.Sprintf("  %d. %s  %s", i+1, str(step, "kind"), StepSummary(step))
		if w := str(step, "when"); w != "" {
			line += "  (only with --set " + w + "=" + r.Metavar(w) + ")"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// Param values, in the recipe's param order.
type Params struct {
	Keys []string
	Vals map[string]string
}

// Has reports whether a param has a value.
func (p *Params) Has(k string) bool { _, ok := p.Vals[k]; return ok }

// Object is the params as the receipt writes them.
func (p *Params) Object() *pyjson.Object {
	o := pyjson.NewObject()
	for _, k := range p.Keys {
		o.Set(k, p.Vals[k])
	}
	return o
}

// ResolveParams is recipes.resolve_params. order is the --set keys in the
// order they were given; given holds their values (the last one wins).
func (r *Recipe) ResolveParams(order []string, given map[string]string) (*Params, error) {
	for _, k := range order {
		if r.Param(k) == nil {
			return nil, fmt.Errorf("recipe '%s' has no param '%s'", r.Name(), k)
		}
	}
	out := &Params{Vals: map[string]string{}}
	for _, pn := range r.ParamNames() {
		p := r.Param(pn)
		if v, ok := given[pn]; ok {
			out.Keys = append(out.Keys, pn)
			out.Vals[pn] = v
			continue
		}
		if d, has := p.Get("default"); has {
			out.Keys = append(out.Keys, pn)
			out.Vals[pn] = pyStr(d)
			continue
		}
		if pyjson.Truthy(p.Value("required")) {
			return nil, fmt.Errorf("recipe '%s' needs --set %s=%s", r.Name(), pn, r.Metavar(pn))
		}
	}
	return out, nil
}

// Fill is recipes._fill: each {param} in value replaced by its value.
func (r *Recipe) Fill(v any, params *Params, index int) (any, error) {
	switch x := v.(type) {
	case string:
		var ferr error
		out := paramRE.ReplaceAllStringFunc(x, func(m string) string {
			name := m[1 : len(m)-1]
			val, ok := params.Vals[name]
			if !ok {
				ferr = fmt.Errorf("step %d needs --set %s=%s", index, name, r.Metavar(name))
				return m
			}
			return val
		})
		return out, ferr
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			f, err := r.Fill(e, params, index)
			if err != nil {
				return nil, err
			}
			out[i] = f
		}
		return out, nil
	case *pyjson.Object:
		out := pyjson.NewObject()
		for _, k := range x.Keys() {
			f, err := r.Fill(x.Value(k), params, index)
			if err != nil {
				return nil, err
			}
			out.Set(k, f)
		}
		return out, nil
	}
	return v, nil
}

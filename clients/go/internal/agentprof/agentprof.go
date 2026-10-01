// Package agentprof is opendaisugi/agent_profiles.py: the operator's table
// of where a child agent runs.
//
// A profile names a wire (claude-code, anthropic or openai), an optional
// base URL, an optional default model and an optional key_env: the name
// of the variable that holds the key. The table is config.yaml's
// agent_profiles; a step or a parent names a profile, never a URL or a
// key. The root starter reads each key_env once (Resolve); below it a key
// moves only through an inherited pipe: the child's key on descriptor 3,
// and for a child that may start children the resolved table (the
// bundle) on descriptor 4. No profile is limited by a model's origin.
package agentprof

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/pyjson"
)

// Default is the built-in profile's name.
const Default = "default"

// Wires are the wires a profile may name.
var Wires = []string{"claude-code", "anthropic", "openai"}

var fields = []string{"wire", "base_url", "model", "key_env"}

// DefaultBase is the server a wire talks to when the profile names none.
var DefaultBase = map[string]string{"anthropic": "https://api.anthropic.com", "openai": "https://api.openai.com/v1"}

// Ambient is the variables that can hold a provider key or point a key
// elsewhere. A profiled sprig child gets none of them.
var Ambient = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY", "SPRIG_API_KEY",
	"ANTHROPIC_BASE_URL", "SPRIG_BACKEND", "SPRIG_MODEL",
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
var varRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidName is agent_profiles.valid_name.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// Profile is agent_profiles.Profile; nil fields are None.
type Profile struct {
	Name, Wire                  string
	BaseURL, Model, KeyEnv, Key *string
}

// Builtin is the built-in profile.
var Builtin = Profile{Name: Default, Wire: "claude-code"}

func optAny(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func (p Profile) entry() *pyjson.Object {
	return pyjson.NewObject().Set("wire", p.Wire).Set("base_url", optAny(p.BaseURL)).
		Set("model", optAny(p.Model)).Set("key_env", optAny(p.KeyEnv)).Set("key", optAny(p.Key))
}

// Profiles is the resolved table: the valid profiles with their keys
// read, and why each other text-named entry is not a profile.
type Profiles struct {
	Valid   map[string]Profile
	Invalid map[string]string
}

// Names is Profiles.names: the built-in and the valid names, sorted.
func (ps Profiles) Names() []string {
	out := []string{Default}
	for n := range ps.Valid {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// KeyNames is Profiles.key_names: every key_env the table names, sorted.
func (ps Profiles) KeyNames() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range ps.Valid {
		if p.KeyEnv != nil && *p.KeyEnv != "" && !seen[*p.KeyEnv] {
			seen[*p.KeyEnv] = true
			out = append(out, *p.KeyEnv)
		}
	}
	sort.Strings(out)
	return out
}

// Lookup is Profiles.lookup: the profile a step names ("" is the
// built-in), or why not.
func (ps Profiles) Lookup(name *string) (*Profile, string) {
	if name == nil || *name == Default {
		b := Builtin
		return &b, ""
	}
	n := *name
	if why, bad := ps.Invalid[n]; bad {
		return nil, "agent profile '" + n + "' is not valid: " + why
	}
	p, ok := ps.Valid[n]
	if !ok {
		return nil, "no agent profile named '" + n + "'; the profiles are " + strings.Join(ps.Names(), ", ")
	}
	if p.KeyEnv != nil && *p.KeyEnv != "" && p.Key == nil {
		return nil, "agent profile '" + n + "' names " + *p.KeyEnv + ", which is not set"
	}
	return &p, ""
}

// Bundle is Profiles.bundle: the table with its keys, as
// json.dumps(sort_keys=True, separators=(",", ":")).
func (ps Profiles) Bundle() []byte {
	doc := pyjson.NewObject().Set(Default, Builtin.entry())
	for n, p := range ps.Valid {
		doc.Set(n, p.entry())
	}
	return []byte(pyjson.CanonicalASCII(doc))
}

// OperatorBundle is agent_profiles.operator_bundle: the bundle an
// operator-side root starter hands to tree run, and one line for each
// profile left out of it (an invalid one, and one whose key_env is not
// set). The lines name the profile and the variable, never a key.
func OperatorBundle(ps Profiles) ([]byte, []string) {
	names := map[string]bool{}
	for n := range ps.Valid {
		names[n] = true
	}
	for n := range ps.Invalid {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	var notes []string
	doc := pyjson.NewObject().Set(Default, Builtin.entry())
	for _, n := range sorted {
		if why, bad := ps.Invalid[n]; bad {
			notes = append(notes, "agent profile '"+n+"' is left out: "+why)
			continue
		}
		p := ps.Valid[n]
		if p.KeyEnv != nil && *p.KeyEnv != "" && p.Key == nil {
			notes = append(notes, "agent profile '"+n+"' is left out: "+*p.KeyEnv+" is not set")
			continue
		}
		doc.Set(n, p.entry())
	}
	return []byte(pyjson.CanonicalASCII(doc)), notes
}

// ListRows is agent_profiles.list_rows: one row for each valid profile, by
// name, with no key and no key variable name. key_set is false only when
// the profile names a key variable that is not set.
func ListRows(ps Profiles) []any {
	names := make([]string, 0, len(ps.Valid))
	for n := range ps.Valid {
		names = append(names, n)
	}
	sort.Strings(names)
	rows := []any{}
	for _, n := range names {
		p := ps.Valid[n]
		rows = append(rows, pyjson.NewObject().Set("name", n).Set("wire", p.Wire).
			Set("base_url", optAny(p.BaseURL)).Set("model", optAny(p.Model)).
			Set("key_set", p.KeyEnv == nil || p.Key != nil))
	}
	return rows
}

func strVal(v config.Value) (*string, bool) {
	switch v.Kind {
	case config.Null:
		return nil, true
	case config.Str:
		s := v.Text
		return &s, true
	}
	return nil, false
}

// check is agent_profiles._check.
func check(name string, doc config.Value) string {
	if name == Default {
		return "default is built in"
	}
	if !ValidName(name) {
		return "its name is not 1 to 64 of A-Z a-z 0-9 . _ -"
	}
	if doc.Kind != config.Map {
		return "it is not a mapping"
	}
	var extra []string
	for _, k := range doc.Keys {
		known := false
		for _, f := range fields {
			known = known || k == f
		}
		if !known {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		return "it has an unknown field " + extra[0]
	}
	w, has := doc.Map["wire"]
	isWire := false
	if has && w.Kind == config.Str {
		for _, x := range Wires {
			isWire = isWire || x == w.Text
		}
	}
	if !isWire {
		return "wire must be claude-code, anthropic or openai"
	}
	for _, f := range []string{"base_url", "model", "key_env"} {
		if v, has := doc.Map[f]; has {
			if _, ok := strVal(v); !ok {
				return f + " must be text"
			}
		}
	}
	if v, has := doc.Map["key_env"]; has && v.Kind == config.Str && !varRe.MatchString(v.Text) {
		return "key_env must be a variable name"
	}
	if w.Text == "claude-code" {
		if v, has := doc.Map["base_url"]; has && v.Kind != config.Null {
			return "the claude-code wire takes no base_url"
		}
		if v, has := doc.Map["key_env"]; has && v.Kind != config.Null {
			return "the claude-code wire takes no key_env"
		}
	}
	return ""
}

// Resolve is agent_profiles.resolve: read the table raw (nil for none)
// and each key_env from lookup, once. An empty key counts as not set.
func Resolve(raw *config.Value, lookup func(string) (string, bool)) Profiles {
	ps := Profiles{Valid: map[string]Profile{}, Invalid: map[string]string{}}
	if raw == nil || raw.Kind != config.Map {
		return ps
	}
	for _, name := range raw.Keys {
		doc := raw.Map[name]
		if why := check(name, doc); why != "" {
			ps.Invalid[name] = why
			continue
		}
		p := Profile{Name: name, Wire: doc.Map["wire"].Text}
		p.BaseURL, _ = strVal(doc.Map["base_url"])
		p.Model, _ = strVal(doc.Map["model"])
		p.KeyEnv, _ = strVal(doc.Map["key_env"])
		if p.KeyEnv != nil && *p.KeyEnv != "" {
			if k, ok := lookup(*p.KeyEnv); ok && k != "" {
				p.Key = &k
			}
		}
		ps.Valid[name] = p
	}
	return ps
}

// FromBundle is agent_profiles.from_bundle: the table a starter got on its
// keys descriptor. A bundle that does not read is an empty table.
func FromBundle(data []byte) Profiles {
	ps := Profiles{Valid: map[string]Profile{}, Invalid: map[string]string{}}
	if !utf8.Valid(data) {
		return ps
	}
	v, err := pyjson.LoadsPy(string(data), 900)
	if err != nil {
		return ps
	}
	doc, ok := v.(*pyjson.Object)
	if !ok {
		return ps
	}
	for _, name := range doc.Keys() {
		e, isObj := doc.Value(name).(*pyjson.Object)
		if name == Default || !isObj || !ValidName(name) {
			continue
		}
		wire, _ := e.Value("wire").(string)
		isWire := false
		for _, x := range Wires {
			isWire = isWire || x == wire
		}
		if !isWire {
			continue
		}
		str := func(k string) *string {
			if s, ok := e.Value(k).(string); ok {
				return &s
			}
			return nil
		}
		ps.Valid[name] = Profile{Name: name, Wire: wire, BaseURL: str("base_url"), Model: str("model"),
			KeyEnv: str("key_env"), Key: str("key")}
	}
	return ps
}

// ConfiguredTable is agent_profiles.configured_table: config.yaml's
// agent_profiles as written, or nil. A config that does not read has no
// table.
func ConfiguredTable(dataHome string) *config.Value {
	raw, err := os.ReadFile(dataHome + "/config.yaml")
	if err != nil {
		return nil
	}
	doc, err := config.Parse(string(raw))
	if err != nil || doc == nil {
		return nil
	}
	v, ok := doc.Vals["agent_profiles"]
	if !ok {
		return nil
	}
	return &v
}

// ConfigText is agent_profiles.config_text: the names only.
func ConfigText(v config.Value) string {
	if v.Kind == config.Null || (v.Kind == config.Map && len(v.Keys) == 0 && v.NonStrKeys == 0) {
		return "none"
	}
	if v.Kind != config.Map {
		return "not a mapping"
	}
	names := append([]string{}, v.Keys...)
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// IsKeyName is agent_profiles.is_key_name: a variable name that may hold a
// credential, *_API_KEY or *_TOKEN. No child of the executor gets one.
func IsKeyName(name string) bool {
	return strings.HasSuffix(name, "_API_KEY") || strings.HasSuffix(name, "_TOKEN")
}

// Package envdefault is the one default permission a project gets (owner
// ruling 2026-11-06): the steward grants it to a known project without a
// card. A known project is one whose dir sits under a trusted root.
//
// The default envelope lets a worker read and write the project dir and
// run shell commands there, with no budget and the validator's step cap.
// It denies the verbs that publish or spend: Denied is the one list. The
// denial is an envelope invariant (forall_steps over the shell command), so
// the warden enforces it with the predicate algebra it already has, and no
// field is added to the envelope model.
//
// Wider says how a proposed envelope asks for more than the default. A
// proposal that is not wider is written without a card; one that is wider
// becomes a Needs-you card that names each way it is wider.
package envdefault

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// GrantedBy is the mark a default grant writes into a project's envelope
// file ("granted_by"). An owner's approve writes GrantedByOwner.
const (
	GrantedBy      = "default"
	GrantedByOwner = "owner"
)

// StepCapS is the per-step cap of the default envelope: the validator's
// default for max_execution_time_s. A proposal may not raise it without a
// card.
const StepCapS = 30

// OutputCapMB is the validator's default for max_output_size_mb.
const OutputCapMB = 10

// Stakes is the stakes of the default envelope, stated once.
const Stakes = "low"

// ShellHeads is the shell allowlist of the default envelope: any command
// named by one word, and any script in the project's top dir (./gradlew).
// A head with a slash elsewhere (/usr/bin/x, tools/x) is outside it.
var ShellHeads = []string{"*", "./*"}

// Verb is one denied verb: its name as the docs and the card say it, and the
// regex the warden searches the shell command for.
type Verb struct {
	Name  string
	Regex string
}

// The deny regexes read the command as the shell will see it, as far as a
// regex can. A word may carry quotes and backslashes inside it (git
// "push", git pu''sh, p\ush): w allows them between its letters. Words may
// be split by spaces or a backslash-newline. A tool's name counts wherever
// it starts a word, not only at the start of a command, so any wrapper in
// front of it (xargs, timeout, nice, ionice, flock, stdbuf, watch, env,
// sudo, setsid, bash -c '...') still matches. The cost is a false deny on a
// command that only mentions the verb, such as echo git push.

// sep is the space between two words.
const sep = `(\s|\\\n)+`

// wordStart is what may come right before a tool's name.
const wordStart = `(^|[\s;&|(` + "`" + `'"=\\])`

// q is what may sit between two letters of one word: a quote, a
// backslash, or a backslash-newline.
const q = `(["'\\]|\\\n)*`

// w is word as a regex that allows quotes and backslashes in it.
func w(word string) string {
	var b strings.Builder
	b.WriteString(q)
	for _, r := range word {
		b.WriteString(regexp.QuoteMeta(string(r)))
		b.WriteString(q)
	}
	return b.String()
}

// opts is a run of global options between a tool and its subcommand: git
// -C dir push, gh -R owner/repo release.
const opts = `(` + sep + `-\S+(` + sep + `[^-\s]\S*)?)*`

// cmd is tool, then options, then each sub word, then a word end.
func cmd(tool string, sub ...string) string {
	r := wordStart + w(tool)
	for k, s := range sub {
		if k == 0 {
			r += opts
		}
		r += sep + w(s)
	}
	return r + `\b`
}

// ghAPI is the regex for gh api calls that write a release, a ref, a file,
// a merge or a dispatch, or delete anything, or send a GraphQL mutation.
// Within one command (no ; & | or newline), either order of the write
// flag and the path counts. A flag may carry its value attached (-XPOST,
// -ftag=v), and a method may be in any case (--method Post).
func ghAPI() string {
	head := wordStart + w("gh") + opts + sep + w("api") + `\b`
	rest := `[^;&|\n]*`
	method := `(-X|--method)(=|\s*)["']?`
	write := `(` + method + `(` + ci("post") + `|` + ci("put") + `|` + ci("patch") + `|` + ci("delete") + `)` +
		`|\s(-[fF]\S*|--field(=|\s)|--raw-field(=|\s)|--input(=|\s)))`
	path := `/(releases|merges|merge|git/refs|contents|dispatches|pages)\b`
	del := method + ci("delete")
	mutation := `graphql\b` + rest + `\bmutation\b`
	return head + `(` + rest + write + rest + path + `|` + rest + path + rest + write + `|` + rest + del + `|` + rest + mutation + `)`
}

// ci is word as a regex that matches it in any case.
func ci(word string) string {
	var b strings.Builder
	for _, r := range word {
		lo, up := strings.ToLower(string(r)), strings.ToUpper(string(r))
		b.WriteString("[" + lo + up + "]")
	}
	return b.String()
}

// Denied is the one list of verbs the default denies: push, release,
// deploy, delete and money. Each is an invariant of every envelope the
// steward writes, unless the owner approved a proposal that allows it.
var Denied = []Verb{
	{"git push", cmd("git", "push")},
	{"gh release", cmd("gh", "release")},
	{"gh pr merge", cmd("gh", "pr", "merge")},
	{"gh repo delete", cmd("gh", "repo", "delete")},
	{"gh workflow run", cmd("gh", "workflow", "run")},
	// gh api reaches the same endpoints. A read (GET, the default) is
	// allowed; a write to a release or a merge, and any DELETE, are not. A
	// write needs a method flag or a field flag, so those are what count.
	{"gh api", ghAPI()},
	{"flutter build appbundle", cmd("flutter", "build", "appbundle")},
	{"fastlane", cmd("fastlane")},
	{"stripe", cmd("stripe")},
	{"npm publish", cmd("npm", "publish")},
	{"pnpm publish", cmd("pnpm", "publish")},
	{"yarn publish", wordStart + w("yarn") + opts + `(` + sep + w("npm") + `)?` + sep + w("publish") + `\b`},
	{"cargo publish", cmd("cargo", "publish")},
}

// VerbNames are the names of Denied, in order.
func VerbNames() []string {
	out := make([]string, len(Denied))
	for i, v := range Denied {
		out[i] = v.Name
	}
	return out
}

// invariantType is the invariant type of a denied verb: deny_git_push.
func invariantType(v Verb) string {
	return "deny_" + strings.ReplaceAll(v.Name, " ", "_")
}

// denyInvariant is the invariant that denies one verb.
func denyInvariant(v Verb) map[string]any {
	return map[string]any{
		"type":        invariantType(v),
		"description": v.Name + " is the owner's to run",
		"expr": map[string]any{"op": "forall_steps", "pred": map[string]any{"op": "implies",
			"a": map[string]any{"op": "equals", "path": "type", "value": "shell"},
			"b": map[string]any{"op": "not_matches", "path": "command", "regex": v.Regex}}},
	}
}

// DenyInvariants are the invariants for every denied verb but those in
// allow.
func DenyInvariants(allow []string) []any {
	skip := map[string]bool{}
	for _, a := range allow {
		skip[a] = true
	}
	var out []any
	for _, v := range Denied {
		if !skip[v.Name] {
			out = append(out, denyInvariant(v))
		}
	}
	return out
}

// CheckAllow refuses a name that is not a denied verb.
func CheckAllow(allow []string) error {
	known := map[string]bool{}
	for _, v := range Denied {
		known[v.Name] = true
	}
	for _, a := range allow {
		if !known[a] {
			return fmt.Errorf("%q is not a denied verb. They are: %s", a, strings.Join(VerbNames(), ", "))
		}
	}
	return nil
}

// Envelope is the default envelope for the project in dir, as JSON.
func Envelope(dir string) []byte {
	dir = filepath.Clean(dir)
	paths := []any{dir, dir + "/**"}
	env := map[string]any{
		"generated_by": "coppice default",
		"task":         "work in " + dir,
		"stakes":       Stakes,
		"permissions": map[string]any{
			"file_read": paths, "file_write": paths,
			"network": false, "shell": true, "shell_allowlist": toAny(ShellHeads),
			"shell_allow_decomposition": true,
			"max_execution_time_s":      StepCapS, "max_output_size_mb": OutputCapMB,
		},
		"invariants": DenyInvariants(nil),
	}
	b, _ := json.MarshalIndent(env, "", "  ")
	return b
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// OwnInvariants counts the invariants of an envelope that are not exactly
// one of the canonical deny invariants: what a proposal brings of its own.
func OwnInvariants(envelope []byte) int {
	var p struct {
		Invariants []json.RawMessage `json:"invariants"`
	}
	if json.Unmarshal(envelope, &p) != nil {
		return 0
	}
	return ownInvariants(p.Invariants)
}

// ownInvariants counts the invariants that are not exactly one of the
// canonical deny invariants.
func ownInvariants(invs []json.RawMessage) int {
	canon := map[string]bool{}
	for _, inv := range DenyInvariants(nil) {
		canon[canonJSON(inv)] = true
	}
	n := 0
	for _, raw := range invs {
		var v any
		if json.Unmarshal(raw, &v) != nil || !canon[canonJSON(v)] {
			n++
		}
	}
	return n
}

// canonJSON is v as JSON with sorted keys (encoding/json sorts map keys).
func canonJSON(v any) string {
	b, _ := json.Marshal(v)
	var x any
	_ = json.Unmarshal(b, &x)
	b, _ = json.Marshal(x)
	return string(b)
}

// isDenyType says whether t is the type of a denied verb's invariant, or
// shaped like one (deny_...).
func isDenyType(t string) bool { return strings.HasPrefix(t, "deny_") }

// WithDeny is the envelope with the canonical deny invariants appended, but
// those for the verbs in allow. Any invariant of a deny type the envelope
// carries is dropped first.
func WithDeny(envelope []byte, allow []string) ([]byte, error) {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(envelope, &env); err != nil || env == nil {
		return nil, errors.New("the envelope is not a JSON object")
	}
	var invs []map[string]any
	if raw, ok := env["invariants"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &invs); err != nil {
			return nil, errors.New("the envelope's invariants are not a list of objects")
		}
	}
	// The deny invariants come only from this package: a proposal's own
	// invariant of a deny type is dropped, whatever it says, and the
	// canonical one is appended.
	all := make([]any, 0, len(invs)+len(Denied))
	for _, inv := range invs {
		if t, _ := inv["type"].(string); isDenyType(t) {
			continue
		}
		all = append(all, inv)
	}
	all = append(all, DenyInvariants(allow)...)
	b, _ := json.Marshal(all)
	env["invariants"] = b
	return json.Marshal(env)
}

// proposal is the part of an envelope Wider reads.
type proposal struct {
	Permissions struct {
		Read      []string          `json:"file_read"`
		Write     []string          `json:"file_write"`
		Network   bool              `json:"network"`
		Hosts     []string          `json:"network_hosts"`
		Allowlist []string          `json:"shell_allowlist"`
		MCP       []string          `json:"mcp_allowlist"`
		Rules     []json.RawMessage `json:"mcp_rules"`
		Custom    []string          `json:"custom_step_allowlist"`
		TimeS     *float64          `json:"max_execution_time_s"`
		OutMB     *float64          `json:"max_output_size_mb"`
	} `json:"permissions"`
	Interp     string            `json:"shell_interpreter_policy"`
	Invariants []json.RawMessage `json:"invariants"`
}

// Wider is each way a proposal asks for more than the default envelope of
// dir: one plain line each, in a fixed order. Empty means it fits the
// default. tokens and turns are its budget; allow are the denied verbs it
// asks to run.
func Wider(envelope []byte, tokens, turns int, allow []string, dir string) []string {
	var p proposal
	if err := json.Unmarshal(envelope, &p); err != nil {
		return []string{"the envelope does not read"}
	}
	dir = filepath.Clean(dir)
	var out []string
	outside := func(paths []string) []string {
		var o []string
		for _, x := range paths {
			if !Within(x, dir) {
				o = append(o, x)
			}
		}
		return o
	}
	if o := outside(p.Permissions.Read); len(o) > 0 {
		out = append(out, "reads outside the project: "+strings.Join(o, ", "))
	}
	if o := outside(p.Permissions.Write); len(o) > 0 {
		out = append(out, "writes outside the project: "+strings.Join(o, ", "))
	}
	if tokens > 0 || turns > 0 {
		out = append(out, fmt.Sprintf("a budget: %d tokens, %d turns", tokens, turns))
	}
	if p.Permissions.Network || len(p.Permissions.Hosts) > 0 {
		w := "network"
		if len(p.Permissions.Hosts) > 0 {
			w += ": " + strings.Join(p.Permissions.Hosts, ", ")
		}
		out = append(out, w)
	}
	if len(allow) > 0 {
		out = append(out, "runs denied verbs: "+strings.Join(allow, ", "))
	}
	var heads []string
	for _, h := range p.Permissions.Allowlist {
		if !headWithin(h) {
			heads = append(heads, h)
		}
	}
	if len(heads) > 0 {
		out = append(out, "shell commands outside the default: "+strings.Join(heads, ", "))
	}
	if len(p.Permissions.MCP) > 0 || len(p.Permissions.Rules) > 0 {
		w := "MCP tools"
		if len(p.Permissions.MCP) > 0 {
			w += ": " + strings.Join(p.Permissions.MCP, ", ")
		}
		out = append(out, w)
	}
	if len(p.Permissions.Custom) > 0 {
		out = append(out, "custom steps: "+strings.Join(p.Permissions.Custom, ", "))
	}
	if p.Permissions.TimeS != nil && *p.Permissions.TimeS > StepCapS {
		out = append(out, fmt.Sprintf("steps up to %g s (the default is %d)", *p.Permissions.TimeS, StepCapS))
	}
	if p.Permissions.OutMB != nil && *p.Permissions.OutMB > OutputCapMB {
		out = append(out, fmt.Sprintf("output up to %g MB (the default is %d)", *p.Permissions.OutMB, OutputCapMB))
	}
	// The default's only invariants are the deny list, which this package
	// writes. A proposal that brings its own (a neutered deny_* among them)
	// is for the owner to read.
	if n := ownInvariants(p.Invariants); n > 0 {
		out = append(out, fmt.Sprintf("its own invariants (%d): the owner reads them; the deny list is always the default's", n))
	}
	if p.Interp == "allow" {
		out = append(out, "shell interpreters without a warning")
	}
	return out
}

// headWithin says whether the default's allowlist admits every head the
// pattern h admits. A denied tool named as a head (stripe, fastlane) is
// outside: the invariants would deny it anyway, so asking for it is asking
// for more.
func headWithin(h string) bool {
	for _, v := range Denied {
		if !strings.Contains(v.Name, " ") && h == v.Name {
			return false
		}
	}
	if h == "*" || h == "./*" {
		return true
	}
	if strings.Contains(h, "**") {
		return false
	}
	segs := strings.Split(h, "/")
	switch {
	case len(segs) == 1:
		return true
	case len(segs) == 2 && segs[0] == "." && segs[1] != "" && segs[1] != "..":
		return true
	}
	return false
}

// Within says whether the path pattern p stays in dir: dir itself, or a
// path or glob under it. Only an absolute pattern can be inside: the warden
// reads a relative one its own way (a bare ** covers everything), so a
// relative pattern is outside. A pattern with a .. segment is outside,
// checked before anything is cleaned. The part before the first glob
// character is resolved through symlinks, so a link in the project that
// points out of it is outside too.
func Within(p, dir string) bool {
	if p == "" || !filepath.IsAbs(p) {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return false
		}
	}
	dir = filepath.Clean(dir)
	c := filepath.Clean(p)
	if c != dir && !strings.HasPrefix(c, dir+"/") {
		return false
	}
	// The literal head: every segment before the first one with a glob.
	segs := strings.Split(c, "/")
	head := segs[:0:0]
	for _, seg := range segs {
		if strings.ContainsAny(seg, "*?[{") {
			break
		}
		head = append(head, seg)
	}
	lit := strings.Join(head, "/")
	if lit == "" {
		return false
	}
	rd := resolve(dir)
	rl := resolve(lit)
	return rl == rd || strings.HasPrefix(rl, rd+"/")
}

// resolve is p with symlinks resolved as far as p exists: the longest
// existing ancestor is resolved and the rest appended.
func resolve(p string) string {
	rest := ""
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// UnderRoot says whether dir is one of roots or sits under one. Both sides
// are compared as given and with symlinks resolved, so a link into a
// trusted root counts and a link out of one does not.
func UnderRoot(dir string, roots []string) bool {
	real := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return filepath.Clean(p)
	}
	d := real(dir)
	for _, r := range roots {
		if r == "" || !filepath.IsAbs(r) {
			continue
		}
		rr := real(r)
		if rr == "/" {
			continue
		}
		if d == rr || strings.HasPrefix(d, rr+"/") {
			return true
		}
	}
	return false
}

package gate

import (
	"strings"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/verify"
)

// This file ports gate_state_rule.py: no agent writes the gate's own
// state (the gate root, and each data dir's router, journal, tree,
// gateway, weave and envelope cache). Each function opens the frames of
// the oracle's, so a decomposition near the recursion limit raises here
// where it raises there.

// stateParts is gate_state_rule.STATE_PARTS.
var stateParts = []string{"gate", "router", "journal", "tree", "gateway", "weave", "envelope_cache.db"}

// readOnlyHeads is gate_state_rule.READ_ONLY.
var readOnlyHeads = set(":", "[", "b2sum", "basename", "cat", "cd", "cmp", "comm", "cut", "df", "diff",
	"dirname", "du", "echo", "egrep", "false", "fgrep", "find", "grep", "head", "hexdump", "jq", "ls",
	"md5sum", "nl", "od", "paste", "popd", "printf", "pushd", "pwd", "readlink", "realpath", "sha1sum",
	"sha256sum", "sha512sum", "stat", "strings", "tail", "test", "tr", "true", "wc")

// findActions is gate_state_rule.FIND_ACTIONS.
var findActions = set("-delete", "-exec", "-execdir", "-fls", "-fprint", "-fprint0", "-fprintf", "-ok", "-okdir")

// stateBoundary is gate_state_rule._BOUNDARY.
const stateBoundary = "/ \t\n\r'\"`;&|()<>:,"

// gateStateRefusal is gate_state_rule.refusal.
func gateStateRefusal(dir string) string {
	return "the gate's own state is under " + dir + "; only the operator writes there."
}

// stateEntry is one protected path as written and its spellings.
type stateEntry struct {
	name  string
	forms []string
}

func appendNew(out []string, s string) []string {
	for _, x := range out {
		if x == s {
			return out
		}
	}
	return append(out, s)
}

// stateDataDirs is gate_state_rule.data_dirs.
func (r *runner) stateDataDirs() []string {
	defer r.enter()()
	var dirs []string
	if basename(r.root) == "gate" {
		dirs = append(dirs, pathParent(r.root))
	}
	dirs = append(dirs, pathJoin(r.pathHome(), ".opendaisugi"))
	var out []string
	for _, d := range dirs {
		out = appendNew(out, normpath(r.expanduser(d)))
	}
	return out
}

// protectedDirs is gate_state_rule.protected_dirs.
func (r *runner) protectedDirs() []string {
	defer r.enter()()
	out := []string{normpath(r.expanduser(r.root))}
	for _, d := range r.stateDataDirs() {
		for _, part := range stateParts {
			out = appendNew(out, d+"/"+part)
		}
	}
	return out
}

// stateNamedRoots is gate_state_rule._named_roots.
func (r *runner) stateNamedRoots() []string {
	defer r.enter()()
	out := r.stateDataDirs()
	gate := normpath(r.expanduser(r.root))
	if basename(gate) != "gate" {
		out = appendNew(out, gate)
	}
	return out
}

// stateEntries is gate_state_rule._entries.
func (r *runner) stateEntries(paths []string) []stateEntry {
	defer r.enter()()
	out := make([]stateEntry, 0, len(paths))
	for _, p := range paths {
		out = append(out, stateEntry{name: p, forms: r.stateSpell(p)})
	}
	return out
}

// stateSpell is gate_state_rule._spell.
func (r *runner) stateSpell(p string) []string {
	defer r.enter()()
	return appendNew([]string{p}, r.realpath(p))
}

func stateUnder(p, top string) bool {
	return p == top || strings.HasPrefix(p, strings.TrimRight(top, "/")+"/")
}

// statePlaced is gate_state_rule._placed.
func (r *runner) statePlaced(raw, cwd string) []string {
	defer r.enter()()
	word := r.expandHome(raw)
	var out []string
	typed := normpath(r.expanduser(word))
	if isabs(typed) {
		out = append(out, typed)
	}
	base := cwd
	if base == "" {
		base = r.getwd()
	}
	if resolved, ok := r.resolvePath(word, &base); ok {
		out = appendNew(out, resolved)
	}
	return appendNew(out, normpath(join(base, r.expanduser(word))))
}

// stateHitOf is gate_state_rule._hit_of.
func (r *runner) stateHitOf(spellings []string, entries []stateEntry, above bool) (string, bool) {
	defer r.enter()()
	for _, e := range entries {
		for _, f := range e.forms {
			for _, s := range spellings {
				if stateUnder(s, f) || (above && stateUnder(f, s)) {
					return e.name, true
				}
			}
		}
	}
	return "", false
}

// stateNamesText is gate_state_rule._names_text.
func stateNamesText(text, top string) bool {
	start := 0
	for {
		k := strings.Index(text[start:], top)
		if k < 0 {
			return false
		}
		i := start + k
		j := i + len(top)
		if j == len(text) || strings.IndexByte(stateBoundary, text[j]) >= 0 {
			return true
		}
		start = i + 1
	}
}

// stateNames is gate_state_rule._names.
func (r *runner) stateNames(command, cwd string, forms []string) bool {
	defer r.enter()()
	roots := make([]string, len(forms))
	for i, f := range forms {
		roots[i] = pathStr(f)
	}
	g := guard{roots: func(*runner) []string { return roots }, names: never, text: never}
	expanded := r.expandHome(command)
	for _, f := range forms {
		if stateNamesText(expanded, f) {
			return true
		}
	}
	if cwd != "" && r.pathInFloor(".", cwd, g) {
		return true
	}
	hit, _ := r.shellHit(command, cwd, g)
	return hit
}

// stateLinkSources is gate_state_rule._link_sources.
func (r *runner) stateLinkSources(words []string) []string {
	defer r.enter()()
	head := basename(words[0])
	args := words[1:]
	if head == "cp" {
		linked := false
		for _, w := range args {
			if w == "--" {
				break
			}
			if w == "--link" || w == "--symbolic-link" {
				linked = true
			} else if strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--") &&
				(strings.Contains(w, "l") || strings.Contains(w, "s")) {
				linked = true
			}
		}
		if !linked {
			return nil
		}
	} else if head != "ln" {
		return nil
	}
	var out []string
	flags := true
	for _, w := range args {
		switch {
		case flags && w == "--":
			flags = false
		case flags && strings.HasPrefix(w, "-") && w != "-":
		default:
			out = append(out, w)
		}
	}
	return out
}

// stateShellScan is gate_state_rule._shell_scan.
func (r *runner) stateShellScan(command string) (bool, []string) {
	defer r.enter()()
	d, exc := r.decompose(command)
	if exc != nil {
		panic(exc)
	}
	if !d.OK {
		return false, nil
	}
	var links []string
	for _, text := range d.Commands {
		words, err := verify.ShlexSplit(text)
		if err != nil {
			return false, nil
		}
		i := 0
		for i < len(words) && isEnvAssign(words[i]) {
			i++
		}
		if i == len(words) {
			continue
		}
		head := basename(words[i])
		if verify.IsWriter(head) {
			links = append(links, r.stateLinkSources(words[i:])...)
			continue
		}
		if !readOnlyHeads[head] {
			return false, nil
		}
		if head == "find" {
			for _, w := range words[i+1:] {
				if findActions[w] {
					return false, nil
				}
			}
		}
	}
	return true, links
}

// stateShell is gate_state_rule._shell.
func (r *runner) stateShell(command, cwd string, entries, named []stateEntry) (string, bool) {
	defer r.enter()()
	hitRoot := ""
	found := false
	for _, n := range named {
		if r.stateNames(command, cwd, n.forms) {
			hitRoot, found = n.name, true
			break
		}
	}
	if !found {
		return "", false
	}
	step := pyjson.NewObject().Set("type", "shell").Set("command", command)
	writes, known := r.stepWritePaths(step, r.depth+1)
	if !known {
		return hitRoot, true
	}
	ok, links := r.stateShellScan(command)
	if !ok {
		return hitRoot, true
	}
	for _, w := range append(append([]string{}, writes...), links...) {
		if !isabs(r.expanduser(w)) && cwd == "" {
			return hitRoot, true
		}
		if got, hit := r.stateHitOf(r.statePlaced(w, cwd), entries, true); hit {
			return got, true
		}
	}
	return "", false
}

// gateStateHit is gate_state_rule.gate_state_hit: the protected path a
// tool call writes, as written. Any error is a hit on the gate root.
func (r *runner) gateStateHit(cwd string, rec *record) (dir string, hit bool) {
	defer r.enter()()
	if exc := catch(func() { dir, hit = r.gateStateBody(cwd, rec) }); exc != nil {
		return r.firstProtected(), true
	}
	return dir, hit
}

func (r *runner) gateStateBody(cwd string, rec *record) (string, bool) {
	if rec == nil {
		return "", false
	}
	switch rec.StepType {
	case "file_write":
		entries := r.stateEntries(r.protectedDirs())
		return r.stateHitOf(r.statePlaced(rec.Path, cwd), entries, true)
	case "shell":
		entries := r.stateEntries(r.protectedDirs())
		named := r.stateEntries(r.stateNamedRoots())
		return r.stateShell(rec.Command, cwd, entries, named)
	case "mcp":
		entries := r.stateEntries(r.protectedDirs())
		if cwd != "" {
			if got, hit := r.stateHitOf(r.statePlaced(".", cwd), entries, false); hit {
				return got, true
			}
		}
		base := cwd
		if base == "" {
			base = "/"
		}
		for _, v := range pyStrings(rec.Arguments, 0) {
			if got, hit := r.stateHitOf(r.statePlaced(v, base), entries, false); hit {
				return got, true
			}
		}
	}
	return "", false
}

// firstProtected is gate_state_rule._first_protected.
func (r *runner) firstProtected() string {
	defer r.enter()()
	var out string
	if exc := catch(func() { out = r.protectedDirs()[0] }); exc != nil {
		return r.root
	}
	return out
}

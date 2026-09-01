package gate

import (
	"strings"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/verify"
)

// This file ports the gate's hard-deny rules: pane_rule.py (no agent
// answers an ask or reads a secret) and floor_config.py (no agent edits
// the floor's config or OpenCode's). Every regex runs the oracle's own
// pattern with Python's re semantics (see pyregex.go).

const (
	paneRefusal     = "a pane can propose. It cannot allow."
	floorRefusal    = "this is the floor's own config. Edit it yourself."
	opencodeRefusal = "this is OpenCode's config or gate plugin. Edit it yourself."
)

// shellDeniesFromOutside is pane_rule.shell_denies_from_outside.
func shellDeniesFromOutside(command string) bool {
	if !pySearch("pane_rule._DENY_TEXT", command) {
		return false
	}
	return pySearch("pane_rule._OTHER_SERVER", command) || pySearch("pane_rule._WRAPPER_HEAD", command) ||
		pySearch("pane_rule._WRAPPER_ANYWHERE", command)
}

func secretFileSearch(s string) bool { return pySearch("pane_rule._SECRET_FILE", s) }

const webAnswerRoute = "/api/ask/answer"

var valueOptions = set("--socket", "--remote", "--data-dir")

// homeSpellings is pane_rule._home_spellings.
func (r *runner) homeSpellings(text string) string {
	home := r.pathHome()
	text = strings.ReplaceAll(text, "${HOME}", home)
	text = strings.ReplaceAll(text, "$HOME", home)
	return pySub("pane_rule._home_spellings#0", text, func(g []string) string { return g[1] + home })
}

func isAllowWords(words []string) bool {
	for i, w := range words {
		if basename(w) != "coppice" {
			continue
		}
		j := i + 1
		for j < len(words) && strings.HasPrefix(words[j], "--") {
			if valueOptions[words[j]] {
				j += 2
			} else {
				j++
			}
		}
		if j+1 < len(words) && words[j] == "agent" && words[j+1] == "allow" {
			return true
		}
	}
	return false
}

func webDoor(text string) bool {
	return pySearch("pane_rule._WEB_TOKEN_CMD", text) || secretFileSearch(text) || strings.Contains(text, webAnswerRoute)
}

// shellAllows is pane_rule.shell_allows.
func (r *runner) shellAllows(command string) bool {
	defer r.enter()()
	if webDoor(command) {
		return true
	}
	for _, c := range r.commands(command) {
		words, err := verify.ShlexSplit(c)
		if err != nil {
			words = pySplit(c)
		}
		if isAllowWords(words) {
			return true
		}
	}
	return pySearch("pane_rule._ALLOW_TEXT", command) || pySearch("pane_rule._WIRE_TEXT", command)
}

// commands is pane_rule._commands: the simple commands of a shell line,
// or the whole line when it does not decompose or the parser raises.
func (r *runner) commands(command string) []string {
	defer r.enter()()
	var cmds []string
	if exc := catch(func() {
		d, err := r.decompose(command)
		if err != nil {
			panic(err)
		}
		if d.OK && len(d.Commands) > 0 {
			cmds = d.Commands
		}
	}); exc != nil || cmds == nil {
		return []string{command}
	}
	return cmds
}

// gateRoots is pane_rule._roots: the gate root in force and the default,
// each as written and resolved.
func (r *runner) gateRoots() []string {
	defer r.enter()()
	var out []string
	add := func(p string) {
		for _, x := range out {
			if x == p {
				return
			}
		}
		out = append(out, p)
	}
	for _, g := range []string{r.root, r.defaultRoot} {
		e := pathStr(r.expanduser(g))
		add(e)
		add(r.resolve(e))
	}
	return out
}

func underAskDirs(p string, roots []string) bool {
	for _, root := range roots {
		for _, sub := range []string{"asks", "answers"} {
			if pathUnderOrEqual(p, pathJoin(root, sub)) {
				return true
			}
		}
	}
	return false
}

// pathTouchesAsks is pane_rule.path_touches_asks.
func (r *runner) pathTouchesAsks(raw, cwd string) bool {
	defer r.enter()()
	if raw == "" {
		return false
	}
	p := r.placed(raw, cwd)
	roots := r.gateRoots()
	n, res := pathStr(normpath(p)), r.resolve(p)
	return underAskDirs(n, roots) || underAskDirs(res, roots)
}

// placed is the pane rule's Path(_home_spellings(raw)).expanduser(), made
// absolute from cwd (or the process cwd).
func (r *runner) placed(raw, cwd string) string {
	p := r.pathExpanduser(pathStr(r.homeSpellings(raw)))
	if !isabs(p) {
		base := cwd
		if base == "" {
			base = r.getwd()
		}
		p = pathJoin(base, p)
	}
	return p
}

// pathNamesSecret is pane_rule.path_names_secret: a file path, as typed
// and as the OS resolves it from cwd, checked against secretFile. Both
// spellings go through the same normalize-then-resolve pass
// pathTouchesAsks runs for the gate's own ask files, so a doubled slash,
// a bare `.` segment, or a bogus segment a `..` undoes cannot spell a
// secret's path past webDoor's plain text match.
func (r *runner) pathNamesSecret(raw, cwd string) bool {
	defer r.enter()()
	if raw == "" {
		return false
	}
	if secretFileSearch(raw) {
		return true
	}
	p := r.placed(raw, cwd)
	normalized, resolved := normpath(p), r.resolve(p)
	return secretFileSearch(normalized) || secretFileSearch(resolved)
}

// secretSubdirs are pane_rule._SECRET_DIRS: each directory, under a
// coppice data directory, that holds a secret file.
var secretSubdirs = []string{"", "web", "web/ca", "web/tls", "voice"}

// coppiceSecretNames are pane_rule._SECRET_NAMES: each secret file,
// relative to its coppice data directory.
var coppiceSecretNames = []string{"web/token", "web/ca/ca.key", "web/ca/leaf.key", "web/tls/tailscale.key", "voice/token"}

// namesOnlyTools print file names, never contents (pane_rule._NAMES_ONLY).
var namesOnlyTools = set("Glob")

// oneFileTools open only the one file their path names (pane_rule._ONE_FILE).
var oneFileTools = set("Read", "read")

// customDataDirs is floor_config.custom_data_dirs for a gate that runs in
// the hook's own process: an absolute COPPICE_DATA_DIR, normalized. A
// relative one names nothing.
func (r *runner) customDataDirs() []string {
	defer r.enter()()
	v := r.env["COPPICE_DATA_DIR"]
	if v == "" || !isabs(v) {
		return nil
	}
	return []string{normpath(v)}
}

// coppiceDataDirs is floor_config.coppice_data_dirs.
func (r *runner) coppiceDataDirs() []string {
	defer r.enter()()
	return r.spellings(append([]string{pathJoin(r.pathHome(), ".opendaisugi/coppice")}, r.customDataDirs()...))
}

// secretDirs is pane_rule._secret_dirs.
func (r *runner) secretDirs() []string {
	defer r.enter()()
	var out []string
	add := func(p string) {
		for _, x := range out {
			if x == p {
				return
			}
		}
		out = append(out, p)
	}
	for _, d := range r.coppiceDataDirs() {
		for _, sub := range secretSubdirs {
			p := d
			if sub != "" {
				p = pathJoin(d, sub)
			}
			add(p)
			add(r.resolve(p))
		}
	}
	return out
}

// pathHoldsSecret is pane_rule.path_holds_secret.
func (r *runner) pathHoldsSecret(raw, cwd string) bool {
	defer r.enter()()
	if raw == "" {
		return false
	}
	p := r.placed(raw, cwd)
	dirs := r.secretDirs()
	in := func(s string) bool {
		for _, d := range dirs {
			if d == s {
				return true
			}
		}
		return false
	}
	// `normpath in dirs or resolve in dirs`: the resolve only runs when
	// the first test fails.
	return in(normpath(p)) || in(r.resolve(p))
}

// shellTouchesAsks is pane_rule.shell_touches_asks.
func (r *runner) shellTouchesAsks(command, cwd string) bool {
	defer r.enter()()
	text := r.homeSpellings(command)
	if pySearch("pane_rule._ASK_PATH_TEXT", text) {
		return true
	}
	roots := r.gateRoots()
	names := pySearch("pane_rule.shell_touches_asks#0", text)
	for _, root := range roots {
		for _, sub := range []string{"asks", "answers"} {
			if strings.Contains(text, root+"/"+sub) {
				return true
			}
		}
		if names && strings.Contains(text, root) {
			return true
		}
	}
	if names && cwd != "" {
		here := pathStr(normpath(r.pathExpanduser(pathStr(cwd))))
		for _, root := range roots {
			if pathUnderOrEqual(here, root) {
				return true
			}
		}
	}
	return false
}

// paneRuleHit is pane_rule.pane_rule_hit.
func (r *runner) paneRuleHit(cwd string, rec *record) (hit bool) {
	defer r.enter()()
	// Any error counts as a hit, so a broken check denies.
	if exc := catch(func() { hit = r.paneRuleBody(cwd, rec) }); exc != nil {
		return true
	}
	return hit
}

func (r *runner) paneRuleBody(cwd string, rec *record) bool {
	if rec == nil {
		return false
	}
	switch rec.StepType {
	case "shell":
		return r.shellAllows(rec.Command) || shellDeniesFromOutside(rec.Command) ||
			(r.shellGlobsSecret(rec.Command, cwd) && !r.searchRefuses(rec.Command, cwd)) ||
			r.shellTouchesAsks(rec.Command, cwd)
	case "file_write":
		return r.pathTouchesAsks(rec.Path, cwd)
	case "file_read":
		if webDoor(rec.Path) || r.pathNamesSecret(rec.Path, cwd) {
			return true
		}
		if namesOnlyTools[rec.ToolName] {
			return false
		}
		if r.pathHoldsSecret(rec.Path, cwd) {
			return true
		}
		if !oneFileTools[rec.ToolName] {
			if rec.BadPath {
				return true
			}
			// any(path_holds_secret(p, cwd) for p in paths): a generator frame.
			if r.genexpr(func() bool {
				for _, p := range rec.SearchPaths {
					if r.pathHoldsSecret(p, cwd) {
						return true
					}
				}
				return false
			}) {
				return true
			}
		}
		return !oneFileTools[rec.ToolName] && r.pathHoldsSecret(cwd, cwd)
	case "mcp":
		for _, v := range pyStrings(rec.Arguments, 0) {
			// any(... for v in ...): each test runs in the generator's frame.
			hit := func() bool {
				defer r.enter()()
				return webDoor(v) || r.pathNamesSecret(v, cwd) || r.pathTouchesAsks(v, cwd) || r.shellTouchesAsks(v, cwd)
			}()
			if hit {
				return true
			}
		}
	}
	return false
}

// --- floor_config ---------------------------------------------------------

type guard struct {
	roots func(r *runner) []string
	names func(p string) bool
	text  func(t string) bool
}

func (r *runner) spellings(in []string) []string {
	defer r.enter()()
	var out []string
	add := func(p string) {
		for _, x := range out {
			if x == p {
				return
			}
		}
		out = append(out, p)
	}
	for _, p := range in {
		e := r.pathExpanduser(pathStr(p))
		add(e)
		add(r.resolve(e))
	}
	return out
}

func (r *runner) floorRoots() []string {
	defer r.enter()()
	home := r.pathHome()
	var out []string
	for _, base := range []string{r.env["XDG_CONFIG_HOME"], home + "/.config"} {
		if base != "" {
			out = append(out, pathJoin(base, "coppice"))
		}
	}
	for _, base := range []string{r.env["XDG_DATA_HOME"], home + "/.local/share"} {
		if base != "" {
			out = append(out, pathJoin(base, "coppice"))
		}
	}
	out = append(out, pathJoin(home, ".opendaisugi/coppice"))
	out = append(out, r.customDataDirs()...)
	return r.spellings(out)
}

func (r *runner) opencodeRoots() []string {
	defer r.enter()()
	home := r.pathHome()
	var out []string
	for _, base := range []string{r.env["XDG_CONFIG_HOME"], home + "/.config"} {
		if base != "" {
			out = append(out, pathJoin(base, "opencode"))
		}
	}
	return r.spellings(out)
}

func opencodePath(p string) bool {
	parts := pathParts(p)
	if len(parts) == 0 {
		return false
	}
	last := parts[len(parts)-1]
	if last == "opencode.json" || last == "opencode.jsonc" || last == ".opencode" {
		return true
	}
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == ".opencode" {
			switch parts[i+1] {
			case "plugin", "plugins", "tool", "tools":
				return true
			}
		}
	}
	return false
}

func opencodeText(t string) bool {
	return pySearch("floor_config._OPENCODE_TEXT", t)
}

func never(string) bool { return false }

var (
	floorGuard    = guard{roots: (*runner).floorRoots, names: never, text: never}
	opencodeGuard = guard{roots: (*runner).opencodeRoots, names: opencodePath, text: opencodeText}
)

// expandHome is floor_config._expand_home.
func (r *runner) expandHome(text string) string {
	home := r.pathHome()
	text = pySub("floor_config._VARS", text, func(g []string) string {
		name := g[1]
		if name == "" {
			name = g[2]
		}
		if name == "HOME" {
			return home
		}
		return r.env[name]
	})
	var b strings.Builder
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c == '~' && (i == 0 || strings.IndexByte(" \t'\"=:(", text[i-1]) >= 0) &&
			(i+1 == len(text) || text[i+1] == '/') {
			b.WriteString(home)
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func (r *runner) inRoots(p string, g guard) bool {
	defer r.enter()()
	for _, root := range g.roots(r) {
		if p == root || strings.HasPrefix(p, strings.TrimRight(root, "/")+"/") {
			return true
		}
	}
	return g.names(p)
}

// pathInFloor is floor_config.path_in_floor.
func (r *runner) pathInFloor(raw, cwd string, g guard) bool {
	defer r.enter()()
	if raw == "" {
		return false
	}
	word := r.expandHome(raw)
	typed := normpath(r.expanduser(word))
	if isabs(typed) && r.inRoots(typed, g) {
		return true
	}
	base := cwd
	if base == "" {
		base = r.getwd()
	}
	if resolved, ok := r.resolvePath(word, &base); ok && r.inRoots(resolved, g) {
		return true
	}
	return r.inRoots(normpath(join(base, r.expanduser(word))), g)
}

var gitGlobalValue = set("-C", "-c", "--git-dir", "--work-tree", "--namespace", "--super-prefix", "--config-env")
var gitMessageVerbs = set("commit", "tag", "merge", "notes")

// gitMessageValues is floor_config._git_message_values.
func gitMessageValues(tokens []string) []string {
	if len(tokens) == 0 || tokens[0] != "git" {
		return nil
	}
	i := 1
	for i < len(tokens) && strings.HasPrefix(tokens[i], "-") {
		if gitGlobalValue[tokens[i]] {
			i += 2
		} else {
			i++
		}
	}
	if i >= len(tokens) {
		return nil
	}
	verb := tokens[i]
	i++
	if verb == "stash" {
		if i >= len(tokens) || (tokens[i] != "push" && tokens[i] != "save") {
			return nil
		}
		i++
	} else if !gitMessageVerbs[verb] {
		return nil
	}
	var out []string
	for i < len(tokens) {
		t := tokens[i]
		if t == "--" {
			break
		}
		if t == "-m" || t == "--message" ||
			(strings.HasPrefix(t, "-") && !strings.HasPrefix(t, "--") && strings.HasSuffix(t, "m") && len(t) > 2) {
			if i+1 < len(tokens) {
				out = append(out, tokens[i+1])
			}
			i += 2
			continue
		}
		if strings.HasPrefix(t, "--message=") {
			_, v, _ := strings.Cut(t, "=")
			out = append(out, v)
		} else if strings.HasPrefix(t, "-m") && len(t) > 2 {
			out = append(out, t[2:])
		}
		i++
	}
	return out
}

func withoutMessages(text string, messages []string) string {
	for _, m := range messages {
		if m != "" {
			text = strings.Replace(text, m, " ", 1)
		}
	}
	return text
}

func removeFirst(ss []string, s string) ([]string, bool) {
	for i, x := range ss {
		if x == s {
			return append(append([]string{}, ss[:i]...), ss[i+1:]...), true
		}
	}
	return ss, false
}

// namesRootPart is floor_config._names_root_part: a relative word that
// names a guarded directory by its last part. With the cwd unknown, such
// a word could land in the guarded directory.
func (r *runner) namesRootPart(word string, g guard) bool {
	parts := pathParts(normpath(word))
	for _, root := range g.roots(r) {
		name := basename(pathStr(root))
		for _, p := range parts {
			if p == name {
				return true
			}
		}
	}
	return false
}

// maxCwds is floor_config._MAX_CWDS and effects._MAX_CWDS: the most
// working directories one shell line is followed through.
const maxCwds = 32

// fixedGuard is floor_config._fixed: the guard with its roots worked out
// once, for one check.
func (r *runner) fixedGuard(g guard) guard {
	defer r.enter()()
	roots := g.roots(r)
	return guard{roots: func(*runner) []string { return roots }, names: g.names, text: g.text}
}

// containsCwd is Python's `here in cwds` over optional strings.
func containsCwd(cwds []*string, here *string) bool {
	for _, c := range cwds {
		if (c == nil) == (here == nil) && (c == nil || *c == *here) {
			return true
		}
	}
	return false
}

// shellHit is floor_config._shell_hit.
func (r *runner) shellHit(command, cwd string, g guard) (bool, []string) {
	defer r.enter()()
	d, exc := r.decompose(command)
	if exc != nil {
		// A line the rule cannot split (RecursionError near the limit) is
		// a hit. The oracle's one exception, ImportError for a missing
		// parser, cannot happen here.
		return true, nil
	}
	if !d.OK || len(d.Commands) == 0 {
		return false, nil
	}
	var here *string
	if cwd != "" {
		here = strp(cwd)
	}
	cwds := []*string{here}
	var messages []string
	orSlash := func(p *string) string {
		if p == nil || *p == "" {
			return "/"
		}
		return *p
	}
	for _, text := range d.Commands {
		raw, err := verify.ShlexSplit(text)
		if err != nil {
			// The words cannot be placed, so the cwd after them is unknown.
			here = nil
			if !containsCwd(cwds, here) {
				cwds = append(cwds, here)
			}
			continue
		}
		values := gitMessageValues(raw)
		messages = append(messages, values...)
		tokens := make([]string, len(raw))
		for i, t := range raw {
			tokens[i] = r.expandHome(t)
		}
		skip := append([]string{}, values...)
		for i, word := range tokens {
			orig := raw[i]
			var removed bool
			if skip, removed = removeFirst(skip, orig); removed {
				continue
			}
			if strings.HasPrefix(word, "-") && strings.Contains(word, "=") {
				_, word, _ = strings.Cut(word, "=")
				if strings.HasPrefix(orig, "--message=") {
					continue
				}
			}
			if isabs(r.expanduser(word)) || (here != nil && *here != "") {
				if r.pathInFloor(word, orSlash(here), g) {
					return true, messages
				}
			} else if r.namesRootPart(word, g) {
				return true, messages
			}
		}
		here = r.nextCwd(tokens, here)
		// Each distinct cwd once (84d1999): appending it per command made
		// the redirect check quadratic in the length of the line.
		if !containsCwd(cwds, here) {
			if len(cwds) >= maxCwds {
				// Past this many directories the gate stops following cd
				// (50684bb), as for a cd it cannot follow.
				here = nil
			}
			if !containsCwd(cwds, here) {
				cwds = append(cwds, here)
			}
		}
	}
	for _, p := range append(append([]string{}, d.Writes...), d.Reads...) {
		p = r.expandHome(p)
		for _, c := range cwds {
			if (isabs(r.expanduser(p)) || (c != nil && *c != "")) && r.pathInFloor(p, orSlash(c), g) {
				return true, messages
			}
			if (c == nil || *c == "") && !isabs(r.expanduser(p)) && r.namesRootPart(p, g) {
				return true, messages
			}
		}
	}
	return false, messages
}

// textNamesFloor is floor_config.text_names_floor.
func (r *runner) textNamesFloor(text, cwd string, g guard, freeText bool) bool {
	defer r.enter()()
	g = r.fixedGuard(g)
	expanded := r.expandHome(text)
	for _, root := range g.roots(r) {
		if strings.Contains(expanded, root) {
			return true
		}
	}
	if cwd != "" && r.pathInFloor(".", cwd, g) {
		return true
	}
	hit, messages := r.shellHit(text, cwd, g)
	if hit {
		return true
	}
	if freeText {
		return false
	}
	exp := make([]string, len(messages))
	for i, m := range messages {
		exp[i] = r.expandHome(m)
	}
	return g.text(withoutMessages(expanded, exp))
}

// floorHit is floor_config._hit.
func (r *runner) floorHit(cwd string, rec *record, g guard) (hit bool) {
	// floor_config_hit and opencode_plugin_hit call _hit.
	defer r.enter()()
	defer r.enter()()
	// Any error counts as a hit, so a broken check denies.
	if exc := catch(func() { hit = r.floorBody(cwd, rec, g) }); exc != nil {
		return true
	}
	return hit
}

func (r *runner) floorBody(cwd string, rec *record, g guard) bool {
	if rec == nil {
		return false
	}
	switch rec.StepType {
	case "file_write":
		return r.pathInFloor(rec.Path, cwd, g)
	case "shell":
		return r.textNamesFloor(rec.Command, cwd, g, false)
	case "mcp":
		if cwd != "" && r.pathInFloor(".", cwd, g) {
			return true
		}
		base := cwd
		if base == "" {
			base = "/"
		}
		for _, v := range pyStrings(rec.Arguments, 0) {
			// any(... for v in ...): each test runs in the generator's frame.
			hit := func() bool {
				defer r.enter()()
				return r.pathInFloor(v, base, g) || r.textNamesFloor(v, "", g, true)
			}()
			if hit {
				return true
			}
		}
	}
	return false
}

// pathHome is str(Path.home()).
func (r *runner) pathHome() string { return pathStr(r.home) }

// searchRefuses is pane_rule._search_refuses: true when the search rule
// refuses this line anyway, so its own reason is the one the agent sees.
func (r *runner) searchRefuses(command, cwd string) bool {
	defer r.enter()()
	return r.shellSearchReaches(command, cwd)
}

var _ = pyjson.Repr

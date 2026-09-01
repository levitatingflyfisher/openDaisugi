package gate

import (
	"strconv"
	"strings"

	"daisugi-verify/internal/lazyre"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/verify"
)

// This file ports search_rule.py: no search reaches coppice's secrets.

const searchRefusal = "this search reaches coppice's secrets. Search a narrower directory."

var (
	alwaysSearch = set("rg", "ag", "ack", "rgrep", "ugrep", "ug")
	grepFamily   = set("grep", "egrep", "fgrep", "zgrep")
	findExec     = set("-exec", "-execdir", "-ok", "-okdir")
	assignRe     = lazyre.New(`^[A-Za-z_][A-Za-z0-9_]*=`)
	programText  = lazyre.New(`(^|[^A-Za-z0-9_.-])(rg|ag|ack|rgrep|ugrep|ug|grep|egrep|fgrep|zgrep|find)($|[^A-Za-z0-9_.-])`)
	globRe       = lazyre.New(`[*?\[{]`)
	braceRe      = lazyre.New(`\{([^{}]*)\}`)
	seqRe        = lazyre.New(`^(-?[0-9]+|[A-Za-z])\.\.(-?[0-9]+|[A-Za-z])(\.\.(-?[0-9]+))?$`)
	digitsOpt    = lazyre.New(`^-[0-9]+$`)
	optLevel     = lazyre.New(`^-O[0-9]*$`)
	unresolvedRe = lazyre.New("\\$[A-Za-z_{(0-9@*#?!$-]|`")
	grepFlags    = setRunes("rRnilLcvwxsqHhoEFGPIazZUbTy")
	rgFlags      = setRunes("iIlLcvwxsqHhoFPazUbuSNnp0.")
	otherFlags   = setRunes("ilLcvwxHhonRr")
	longFlags    = set("--recursive", "--dereference-recursive", "--hidden", "--no-ignore", "--no-ignore-vcs",
		"--line-number", "--ignore-case", "--files-with-matches", "--files-without-match", "--count",
		"--invert-match", "--word-regexp", "--line-regexp", "--fixed-strings", "--follow", "--no-heading",
		"--with-filename", "--no-filename", "--files", "--unrestricted", "--null", "--text", "--smart-case",
		"--case-sensitive", "--no-messages", "--quiet", "--only-matching", "--multiline", "--json",
		"--vimgrep", "--search-zip", "--no-index")
	patternOpts    = []string{"-e", "-f", "--regexp", "--file", "--files"}
	searchPrefixes = set("env", "sudo", "doas", "timeout", "nice", "ionice", "stdbuf", "time", "command",
		"exec", "nohup", "xargs", "setsid", "chrt", "taskset", "strace", "busybox")
)

const (
	maxSearchDepth = 2
	maxBraces      = 64
)

func setRunes(s string) map[byte]bool {
	m := map[byte]bool{}
	for i := 0; i < len(s); i++ {
		m[s[i]] = true
	}
	return m
}

func flagsFor(head string) map[byte]bool {
	switch {
	case grepFamily[head]:
		return grepFlags
	case head == "rg":
		return rgFlags
	}
	return otherFlags
}

// takesValue is search_rule._takes_value.
func takesValue(w string, flags map[byte]bool) bool {
	if strings.HasPrefix(w, "--") {
		return !strings.Contains(w, "=") && !longFlags[w]
	}
	rs := pystr.Runes(w)
	for i := 1; i < len(rs); i++ {
		if rs[i] >= 128 || !flags[byte(rs[i])] {
			return i == len(rs)-1
		}
	}
	return false
}

// grepRecurses is search_rule._grep_recurses.
func grepRecurses(words []string) bool {
	for _, w := range words {
		if strings.Contains(w, "recurse") {
			return true
		}
		if strings.HasPrefix(w, "--") {
			name, _, _ := strings.Cut(w, "=")
			if len(name) > 2 && (strings.HasPrefix("--recursive", name) || strings.HasPrefix("--dereference-recursive", name)) {
				return true
			}
		} else if strings.HasPrefix(w, "-") && strings.ContainsAny(w[1:], "rR") {
			return true
		}
		if digitsOpt().MatchString(w) || strings.HasPrefix(w, "--depth") {
			return true
		}
	}
	return false
}

// givesPattern is search_rule._gives_pattern.
func givesPattern(words []string) bool {
	for _, w := range words {
		if w == "--" {
			return false
		}
		for _, o := range patternOpts {
			if w == o || strings.HasPrefix(w, o+"=") {
				return true
			}
		}
		if strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--") && strings.ContainsAny(w[1:], "ef") {
			return true
		}
	}
	return false
}

// toolRoots is search_rule._tool_roots: every word the tool may search.
func toolRoots(head string, args []string) []string {
	flags := flagsFor(head)
	var every, sure []string
	ended, valueNext := false, false
	for _, w := range args {
		if ended {
			every = append(every, w)
			sure = append(sure, w)
			continue
		}
		if w == "--" {
			ended, valueNext = true, false
			continue
		}
		if strings.HasPrefix(w, "-") && len(w) > 1 {
			valueNext = takesValue(w, flags)
			continue
		}
		every = append(every, w)
		if !valueNext {
			sure = append(sure, w)
		}
		valueNext = false
	}
	if len(sure) > 0 && !givesPattern(args) {
		sure = sure[1:]
	}
	if len(sure) == 0 {
		every = append(every, ".")
	}
	return every
}

// findRoots is search_rule._find_roots.
func findRoots(args []string) []string {
	var roots []string
	for i := 0; i < len(args); {
		w := args[i]
		if w == "-H" || w == "-L" || w == "-P" || w == "--" || optLevel().MatchString(w) {
			i++
			continue
		}
		if w == "-D" {
			i += 2
			continue
		}
		if strings.HasPrefix(w, "-") || strings.HasPrefix(w, "(") || strings.HasPrefix(w, "!") {
			break
		}
		roots = append(roots, w)
		i++
	}
	if len(roots) == 0 {
		roots = []string{"."}
	}
	return roots
}

// searchRootsOf is search_rule._search_roots. ok is false when the
// command is no search the rule knows.
func searchRootsOf(tokens []string) ([]string, bool) {
	words := make([]string, len(tokens))
	for i, t := range tokens {
		words[i] = strings.TrimPrefix(t, `\`)
	}
	start := 0
	for start < len(words) && assignRe().MatchString(words[start]) {
		start++
	}
	prefixed := false
	for i := start; i < len(words); i++ {
		head := basename(words[i])
		if i > start && !prefixed {
			prefixed = searchPrefixes[basename(words[i-1])]
			if !prefixed {
				continue
			}
		}
		rest := words[i+1:]
		if head == "git" {
			g := -1
			for j, x := range rest {
				if x == "grep" {
					g = j
					break
				}
			}
			if g < 0 {
				continue
			}
			var out []string
			for _, x := range rest[:g] {
				if !strings.HasPrefix(x, "-") {
					out = append(out, x)
				}
			}
			return append(out, toolRoots("rg", rest[g+1:])...), true
		}
		if alwaysSearch[head] || (grepFamily[head] && grepRecurses(rest)) {
			for _, x := range words[:i] {
				if basename(x) == "xargs" {
					return []string{"$(xargs)"}, true
				}
			}
			return toolRoots(head, rest), true
		}
		if head == "find" {
			for _, x := range rest {
				if findExec[x] {
					return findRoots(rest), true
				}
			}
		}
	}
	return nil, false
}

func seqDigits(x string) bool {
	x = strings.TrimLeft(x, "-")
	if x == "" {
		return false
	}
	for i := 0; i < len(x); i++ {
		if x[i] < '0' || x[i] > '9' {
			return false
		}
	}
	return true
}

// sequence is search_rule._sequence. ok is false when body is no
// sequence; a sequence the gate will not expand gives ["{"].
func (r *runner) sequence(body string) ([]string, bool) {
	defer r.enter()()
	m := seqRe().FindStringSubmatch(body)
	if m == nil {
		return nil, false
	}
	a, b, step, hasStep := m[1], m[2], m[4], m[3] != ""
	ints := seqDigits(a) && seqDigits(b)
	if !ints && (seqDigits(a) || seqDigits(b)) {
		return []string{"{"}, true
	}
	// Each any(...) below is a generator frame.
	if ints && r.genexpr(func() bool {
		for _, x := range []string{a, b} {
			d := strings.TrimLeft(x, "-")
			if len(d) > 1 && strings.HasPrefix(d, "0") {
				return true
			}
		}
		return false
	}) {
		return []string{"{"}, true
	}
	if r.genexpr(func() bool {
		for _, x := range []string{a, b, step} {
			if len(strings.TrimLeft(x, "-")) > 9 {
				return true
			}
		}
		return false
	}) {
		return []string{"{"}, true
	}
	var lo, hi int
	if ints {
		lo, _ = strconv.Atoi(a)
		hi, _ = strconv.Atoi(b)
	} else {
		lo, hi = int(a[0]), int(b[0])
	}
	n := 1
	if hasStep {
		n, _ = strconv.Atoi(step)
		if n < 0 {
			n = -n
		}
		if n == 0 {
			n = 1
		}
	}
	span := hi - lo
	if span < 0 {
		span = -span
	}
	if span/n+1 > maxBraces {
		return []string{"{"}, true
	}
	var out []string
	if hi >= lo {
		for v := lo; v <= hi; v += n {
			out = append(out, seqWord(v, ints))
		}
	} else {
		for v := lo; v >= hi; v -= n {
			out = append(out, seqWord(v, ints))
		}
	}
	return out, true
}

func seqWord(v int, ints bool) string {
	if ints {
		return strconv.Itoa(v)
	}
	return string(rune(v))
}

// braceExpand is search_rule._brace_expand.
func (r *runner) braceExpand(word string, out *[]string) bool {
	defer r.enter()()
	for _, m := range braceRe().FindAllStringSubmatchIndex(word, -1) {
		body := word[m[2]:m[3]]
		var alts []string
		if strings.Contains(body, ",") {
			alts = strings.Split(body, ",")
		} else {
			seq, ok := r.sequence(body)
			if !ok {
				continue
			}
			if len(seq) == 1 && seq[0] == "{" {
				return false
			}
			alts = seq
		}
		for _, part := range alts {
			if !r.braceExpand(word[:m[0]]+part+word[m[1]:], out) {
				return false
			}
			if len(*out) > maxBraces {
				return false
			}
		}
		return true
	}
	*out = append(*out, word)
	return true
}

// overflowReaches is search_rule._overflow_reaches.
func (r *runner) overflowReaches(word, base string, targets []string) bool {
	if !strings.Contains(word, "/") {
		for _, t := range targets {
			if t == base || strings.HasPrefix(t, strings.TrimRight(base, "/")+"/") {
				return true
			}
		}
		return false
	}
	pre := word
	if i := strings.Index(word, "{"); i >= 0 {
		pre = r.homeSpellings(word[:i])
	}
	pre = r.expanduser(pre)
	if !strings.HasPrefix(pre, "/") || globRe().MatchString(pre) {
		return true
	}
	if strings.TrimRight(normpath(pre), "/") != strings.TrimRight(pre, "/") && pre != "/" {
		return true
	}
	for _, t := range targets {
		if strings.HasPrefix(t, pre) {
			return true
		}
	}
	return false
}

// matchPart is search_rule._match_part, on code points. It recurses once
// per pattern character, and a * adds the frame of its any(...)
// generator.
func (r *runner) matchPart(pat, name string) bool {
	return r.matchRunes(pystr.Runes(pat), pystr.Runes(name))
}

func (r *runner) matchRunes(pat, name []rune) bool {
	defer r.enter()()
	if len(pat) == 0 {
		return len(name) == 0
	}
	c := pat[0]
	if c == '*' {
		return r.genexpr(func() bool {
			for i := 0; i <= len(name); i++ {
				if r.matchRunes(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		})
	}
	if len(name) == 0 {
		return false
	}
	if c == '?' {
		return r.matchRunes(pat[1:], name[1:])
	}
	if c == '[' {
		neg1 := len(pat) > 1 && (pat[1] == '!' || pat[1] == '^')
		from := 1
		if neg1 {
			from = 2
		}
		end := runeIndexFrom(pat, ']', from)
		if neg1 && end == 2 {
			end = runeIndexFrom(pat, ']', 3)
		}
		if end == -1 {
			return name[0] == '[' && r.matchRunes(pat[1:], name[1:])
		}
		body := pat[1:end]
		neg := len(body) > 0 && (body[0] == '!' || body[0] == '^')
		if neg {
			body = body[1:]
		}
		hit := false
		for j := 0; j < len(body); {
			if j+2 < len(body) && body[j+1] == '-' {
				if body[j] <= name[0] && name[0] <= body[j+2] {
					hit = true
				}
				j += 3
			} else {
				if body[j] == name[0] {
					hit = true
				}
				j++
			}
		}
		return hit != neg && r.matchRunes(pat[end+1:], name[1:])
	}
	return c == name[0] && r.matchRunes(pat[1:], name[1:])
}

// runeIndexFrom is Python's str.find(ch, start) on code points.
func runeIndexFrom(s []rune, ch rune, start int) int {
	for i := start; i < len(s); i++ {
		if s[i] == ch {
			return i
		}
	}
	return -1
}

func parts(p string) []string {
	var out []string
	for _, x := range strings.Split(p, "/") {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

// place is search_rule._place.
func (r *runner) place(word, cwd string) string {
	defer r.enter()()
	p := r.expanduser(r.homeSpellings(word))
	if !isabs(p) {
		base := cwd
		if base == "" {
			base = r.getwd()
		}
		p = join(base, p)
	}
	return p
}

// globMatches is search_rule.glob_matches.
func (r *runner) globMatches(word, base string, targets []string) bool {
	defer r.enter()()
	var alts []string
	if !r.braceExpand(word, &alts) {
		return r.overflowReaches(word, base, targets)
	}
	for _, alt := range alts {
		pp := parts(normpath(r.place(alt, base)))
		for _, t := range targets {
			tp := parts(t)
			if len(tp) != len(pp) {
				continue
			}
			// all(_match_part(a, b) for ...): a generator frame.
			ok := r.genexpr(func() bool {
				for k := range pp {
					if !r.matchPart(pp[k], tp[k]) {
						return false
					}
				}
				return true
			})
			if ok {
				return true
			}
		}
	}
	return false
}

// secretFiles is search_rule.secret_files.
func (r *runner) secretFiles() []string {
	defer r.enter()()
	var out []string
	for _, d := range r.dataDirs() {
		for _, s := range coppiceSecretNames {
			out = append(out, d+"/"+s)
		}
	}
	return out
}

// reachTargets is search_rule._reach_targets.
func (r *runner) reachTargets() []string {
	defer r.enter()()
	out := []string{"/"}
	seen := map[string]bool{"/": true}
	for _, f := range r.secretFiles() {
		ps := parts(f)
		for i := 1; i <= len(ps); i++ {
			p := "/" + strings.Join(ps[:i], "/")
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// dataDirs is search_rule._data_dirs.
func (r *runner) dataDirs() []string {
	defer r.enter()()
	return r.coppiceDataDirs()
}

// pathAboveSecret is search_rule.path_above_secret.
func (r *runner) pathAboveSecret(raw, cwd string) bool {
	defer r.enter()()
	if raw == "" {
		return false
	}
	p := r.place(raw, cwd)
	cands := []string{normpath(p), r.resolve(p)}
	// any(... for d in _data_dirs() for c in cands): _data_dirs runs in
	// this frame.
	for _, d := range r.dataDirs() {
		for _, c := range cands {
			if strings.HasPrefix(d, strings.TrimRight(c, "/")+"/") {
				return true
			}
		}
	}
	return false
}

func unresolvable(w string) bool {
	return unresolvedRe().MatchString(w) || strings.HasPrefix(w, "~")
}

// rootReaches is search_rule._root_reaches; here is nil when unknown.
func (r *runner) rootReaches(word string, here *string) bool {
	defer r.enter()()
	w := r.expandHome(word)
	if unresolvable(w) {
		return true
	}
	if !isabs(w) && here == nil {
		return true
	}
	base := "/"
	if here != nil && *here != "" {
		base = *here
	}
	if globRe().MatchString(w) {
		return r.globMatches(w, base, r.reachTargets())
	}
	return r.pathAboveSecret(w, base)
}

// lineReaches is search_rule._line_reaches.
func (r *runner) lineReaches(command string, cwd *string, depth int) bool {
	defer r.enter()()
	d, exc := r.decompose(command)
	if exc != nil {
		panic(exc)
	}
	if !d.OK || len(d.Commands) == 0 {
		return programText().MatchString(command)
	}
	here := cwd
	for _, text := range d.Commands {
		raw, err := verify.ShlexSplit(text)
		if err != nil {
			return programText().MatchString(command)
		}
		for _, w := range raw {
			if strings.Contains(w, " ") && programText().MatchString(w) {
				// Past the depth the gate follows, a line that still
				// names a search program is refused.
				if depth >= maxSearchDepth || r.lineReaches(w, here, depth+1) {
					return true
				}
			}
		}
		if roots, ok := searchRootsOf(raw); ok && r.genexpr(func() bool {
			for _, x := range roots {
				if r.rootReaches(x, here) {
					return true
				}
			}
			return false
		}) {
			return true
		}
		tokens := make([]string, len(raw))
		for i, t := range raw {
			tokens[i] = r.expandHome(t)
		}
		here = r.nextCwd(tokens, here)
	}
	return false
}

// shellSearchReaches is search_rule.shell_search_reaches.
func (r *runner) shellSearchReaches(command, cwd string) bool {
	defer r.enter()()
	var here *string
	if isabs(cwd) {
		here = strp(cwd)
	}
	return r.lineReaches(command, here, 0)
}

// searchAboveSecretHit is search_rule.search_above_secret_hit. Any error
// counts as a hit, so a broken check denies.
func (r *runner) searchAboveSecretHit(cwd string, rec *record) (hit bool) {
	defer r.enter()()
	if exc := catch(func() { hit = r.searchBody(cwd, rec) }); exc != nil {
		return true
	}
	return hit
}

func (r *runner) searchBody(cwd string, rec *record) bool {
	if rec == nil {
		return false
	}
	switch rec.StepType {
	case "shell":
		return r.shellSearchReaches(rec.Command, cwd)
	case "file_read":
		if namesOnlyTools[rec.ToolName] || oneFileTools[rec.ToolName] {
			return false
		}
		if rec.BadPath {
			return true
		}
		placed := true
		for _, p := range rec.SearchPaths {
			if !isabs(r.expanduser(r.homeSpellings(p))) {
				placed = false
			}
		}
		if !isabs(cwd) && (len(rec.SearchPaths) == 0 || !placed) {
			return true
		}
		if len(rec.SearchPaths) > 0 {
			return r.genexpr(func() bool {
				for _, p := range rec.SearchPaths {
					if r.pathAboveSecret(p, cwd) {
						return true
					}
				}
				return false
			})
		}
		return r.pathAboveSecret(cwd, cwd)
	}
	return false
}

// shellGlobsSecret is pane_rule.shell_globs_secret.
func (r *runner) shellGlobsSecret(command, cwd string) bool {
	defer r.enter()()
	d, exc := r.decompose(command)
	if exc != nil {
		panic(exc)
	}
	if !d.OK || len(d.Commands) == 0 {
		return false
	}
	files := r.secretFiles()
	var here *string
	if cwd != "" {
		here = strp(cwd)
	}
	for _, text := range d.Commands {
		raw, err := verify.ShlexSplit(text)
		if err != nil {
			return false
		}
		tokens := make([]string, len(raw))
		for i, t := range raw {
			tokens[i] = r.expandHome(t)
		}
		for _, w := range tokens {
			if strings.HasPrefix(w, "-") && strings.Contains(w, "=") {
				_, w, _ = strings.Cut(w, "=")
			}
			if !globRe().MatchString(w) || !(isabs(w) || (here != nil && *here != "")) {
				continue
			}
			base := "/"
			if here != nil && *here != "" {
				base = *here
			}
			if r.globMatches(w, base, files) {
				return true
			}
		}
		here = r.nextCwd(tokens, here)
	}
	return false
}

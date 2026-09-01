package gate

import (
	"strings"

	"daisugi-verify/internal/verify"
)

// This file ports opendaisugi/effects.py: the effect class of one call and
// the tier of a deny.

const (
	tierSilent    = "silent"
	tierUndoable  = "undoable"
	tierPermanent = "permanent"
)

var undoableClasses = set("read", "write_inside_workspace", "network_get", "test_run")

var severity = []string{
	"credential", "prod", "force_push", "delete", "push_shared", "unknown",
	"write_git_dir", "read_git_dir", "write_outside_workspace",
	"read_outside_workspace", "write_inside_workspace", "network_get",
	"test_run", "read",
}

var (
	credentialDirs  = set(".ssh", ".gnupg", ".aws", ".kube", ".docker", ".password-store", ".azure", ".config/gcloud")
	credentialFiles = set(".netrc", ".git-credentials", ".pypirc", ".npmrc", "credentials", "credentials.json", ".env")
	cwdHeads        = set("cd", "pushd", "popd")
	deleteHeads     = set("rm", "rmdir", "shred", "unlink", "truncate", "dd")
	prodHeads       = set("kubectl", "helm", "terraform", "pulumi", "ansible", "ansible-playbook", "fly",
		"flyctl", "heroku", "vercel", "netlify", "gcloud", "aws", "az", "doctl", "ssh", "scp", "rsync")
	credentialHeads = set("gpg", "pass", "ssh-keygen", "ssh-add", "security", "op")
	publish         = map[string]string{"npm": "publish", "pnpm": "publish", "yarn": "publish",
		"cargo": "publish", "uv": "publish", "twine": "upload", "docker": "push", "podman": "push"}
)

func set(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, s := range items {
		m[s] = true
	}
	return m
}

func tierFor(effect string) string {
	if undoableClasses[effect] {
		return tierUndoable
	}
	return tierPermanent
}

func worst(classes []string) string {
	if len(classes) == 0 {
		return "unknown"
	}
	rank := func(c string) int {
		for i, s := range severity {
			if s == c {
				return i
			}
		}
		return -1
	}
	best := classes[0]
	for _, c := range classes[1:] {
		if rank(c) < rank(best) {
			best = c
		}
	}
	return best
}

// workspace is effects.Workspace.
type workspace struct{ Cwd, Base string }

// resolvePath is effects._resolve; ok is false for a relative path with
// no absolute cwd.
func (r *runner) resolvePath(path string, cwd *string) (string, bool) {
	defer r.enter()()
	expanded := r.expanduser(path)
	if !isabs(expanded) {
		if cwd == nil || *cwd == "" || !isabs(*cwd) {
			return "", false
		}
		expanded = join(*cwd, expanded)
	}
	return r.realpath(expanded), true
}

func (r *runner) credentialText(norm string) bool {
	defer r.enter()()
	parts := nonEmpty(strings.Split(norm, "/"))
	for i, part := range parts {
		if credentialDirs[part] {
			return true
		}
		if i+1 < len(parts) && credentialDirs[part+"/"+parts[i+1]] {
			return true
		}
	}
	if len(parts) == 0 {
		return false
	}
	if strings.HasPrefix(norm, "/") && parts[0] == "proc" {
		for _, p := range parts[1:] {
			if p == "environ" {
				return true
			}
		}
	}
	if strings.HasPrefix(norm, "/") {
		home := r.expanduser("~")
		for _, h := range []string{normpath(home), r.realpath(home)} {
			if under(norm, join(h, ".config")) {
				return true
			}
		}
	}
	name := parts[len(parts)-1]
	low := strings.ToLower(name)
	if strings.HasSuffix(low, ".pem") || strings.HasSuffix(low, ".key") {
		return true
	}
	return credentialFiles[name] || strings.HasPrefix(name, ".env.")
}

func (r *runner) isCredentialPath(path string, cwd *string) bool {
	defer r.enter()()
	typed := strings.ReplaceAll(normpath(r.expanduser(path)), "\\", "/")
	if r.credentialText(typed) {
		return true
	}
	resolved, ok := r.resolvePath(path, cwd)
	return ok && r.credentialText(resolved)
}

func under(path, base string) bool {
	c, ok := commonpath(path, base)
	return ok && c == base
}

func (r *runner) inside(path string, root *workspace) bool {
	defer r.enter()()
	if root == nil || !isabs(root.Cwd) || !isabs(root.Base) || path == "" {
		return false
	}
	expanded := r.expanduser(path)
	if strings.ContainsAny(expanded, "~$") {
		return false
	}
	target := r.realpath(join(root.Cwd, expanded))
	base := r.realpath(root.Base)
	return under(target, base)
}

func underProc(path string) bool { return under(path, "/proc") }

func hasGitPart(rel string) bool {
	for _, part := range strings.Split(rel, "/") {
		if strings.ToLower(part) == ".git" {
			return true
		}
	}
	return false
}

func (r *runner) writeClass(path string, root *workspace, cwd *string) string {
	defer r.enter()()
	if underProc(normpath(r.expanduser(path))) {
		return "unknown"
	}
	here := cwd
	if root != nil {
		here = &root.Cwd
	}
	if resolved, ok := r.resolvePath(path, here); ok && underProc(resolved) {
		return "unknown"
	}
	if r.isCredentialPath(path, here) {
		return "credential"
	}
	if !r.inside(path, root) {
		return "write_outside_workspace"
	}
	target := r.realpath(join(root.Cwd, r.expanduser(path)))
	if hasGitPart(r.relpath(target, r.realpath(root.Base))) {
		return "write_git_dir"
	}
	return "write_inside_workspace"
}

func (r *runner) readPlace(path string, base *string, cwd *string) string {
	defer r.enter()()
	typed := normpath(r.expanduser(path))
	if underProc(typed) {
		return "unknown"
	}
	if r.credentialText(typed) {
		return "credential"
	}
	resolved, ok := r.resolvePath(path, cwd)
	if !ok {
		return "read_outside_workspace"
	}
	if underProc(resolved) {
		return "unknown"
	}
	if r.credentialText(resolved) {
		return "credential"
	}
	if base == nil {
		return "read_outside_workspace"
	}
	realBase := r.realpath(*base)
	if !under(resolved, realBase) {
		return "read_outside_workspace"
	}
	if hasGitPart(r.relpath(resolved, realBase)) {
		return "read_git_dir"
	}
	return "read"
}

// flags is effects.Flags.
type flags struct {
	short, shortValue, shortOptional string
	long, longValue, longOptional    map[string]bool
	singleDash, numeric              bool
	noPositional                     bool
}

type fl struct {
	short, shortValue, shortOptional string
	long, longValue, longOptional    []string
	singleDash, numeric              bool
	noPositional                     bool
}

func mk(f fl) flags {
	return flags{short: f.short, shortValue: f.shortValue, shortOptional: f.shortOptional,
		long: set(f.long...), longValue: set(f.longValue...), longOptional: set(f.longOptional...),
		singleDash: f.singleDash, numeric: f.numeric, noPositional: f.noPositional}
}

// optional is effects._optional: every value flag takes its value only
// when attached.
func optional(s flags) flags {
	lo := map[string]bool{}
	for k := range s.longValue {
		lo[k] = true
	}
	for k := range s.longOptional {
		lo[k] = true
	}
	return flags{short: s.short, long: s.long, longValue: map[string]bool{}, longOptional: lo,
		shortOptional: s.shortValue + s.shortOptional, singleDash: s.singleDash, numeric: s.numeric,
		noPositional: s.noPositional}
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// operands is effects._operands; ok is false when a flag is outside spec.
func operands(args []string, spec flags) ([]string, bool) {
	out := []string{}
	ended := false
	i := 0
	for i < len(args) {
		a := args[i]
		i++
		if ended || a == "-" || !strings.HasPrefix(a, "-") {
			if spec.noPositional {
				return nil, false
			}
			out = append(out, a)
			continue
		}
		if a == "--" {
			ended = true
			continue
		}
		if spec.numeric && isDigits(a[1:]) {
			continue
		}
		if strings.HasPrefix(a, "--") || spec.singleDash {
			name, _, eq := strings.Cut(a, "=")
			if spec.singleDash {
				name = "-" + strings.TrimLeft(name, "-")
			}
			if eq {
				if !spec.longValue[name] && !spec.longOptional[name] {
					return nil, false
				}
				continue
			}
			if spec.long[name] || spec.longOptional[name] {
				continue
			}
			if spec.longValue[name] {
				if i >= len(args) {
					return nil, false
				}
				i++
				continue
			}
			return nil, false
		}
		body := a[1:]
	letters:
		for j := 0; j < len(body); j++ {
			c := body[j]
			switch {
			case strings.IndexByte(spec.short, c) >= 0:
				continue
			case strings.IndexByte(spec.shortOptional, c) >= 0:
				break letters
			case strings.IndexByte(spec.shortValue, c) >= 0:
				if j+1 == len(body) {
					if i >= len(args) {
						return nil, false
					}
					i++
				}
				break letters
			default:
				return nil, false
			}
		}
	}
	return out, true
}

var readFlags = map[string]flags{
	"ls": mk(fl{short: "aAlhtrSd1FisGnogcuUvXx",
		long:         []string{"--all", "--almost-all", "--human-readable", "--directory", "--classify", "--group-directories-first"},
		longValue:    []string{"--sort", "--time-style"},
		longOptional: []string{"--color"}}),
	"cat":  mk(fl{short: "nbsAETv", long: []string{"--number", "--show-all"}}),
	"head": mk(fl{short: "qvz", shortValue: "nc", long: []string{"--quiet", "--verbose"}, longValue: []string{"--lines", "--bytes"}, numeric: true}),
	"tail": mk(fl{short: "qvzfF", shortValue: "nc", long: []string{"--quiet", "--verbose", "--follow"}, longValue: []string{"--lines", "--bytes"}, numeric: true}),
	"grep": mk(fl{short: "ivnlLcwxEFPosqHhIzab", shortValue: "eABCm",
		long: []string{"--ignore-case", "--line-number", "--count", "--files-with-matches", "--files-without-match",
			"--invert-match", "--word-regexp", "--fixed-strings", "--extended-regexp", "--perl-regexp",
			"--only-matching", "--quiet", "--no-filename", "--with-filename"},
		longValue: []string{"--include", "--exclude", "--exclude-dir", "--max-count", "--regexp", "--context",
			"--after-context", "--before-context"},
		longOptional: []string{"--color", "--colour"}}),
	"wc":     mk(fl{short: "lwcmL"}),
	"pwd":    mk(fl{short: "LP", noPositional: true}),
	"echo":   mk(fl{short: "neE"}),
	"printf": mk(fl{}),
	"stat":   mk(fl{short: "Ltf", shortValue: "c", longValue: []string{"--format", "--printf"}}),
	"file":   mk(fl{short: "bLiz", long: []string{"--mime", "--brief"}}),
	"sort": mk(fl{short: "bdfgiMhnRrVucsz", shortValue: "kt",
		long: []string{"--numeric-sort", "--reverse", "--unique", "--human-numeric-sort", "--version-sort",
			"--ignore-case", "--stable"},
		longValue: []string{"--key", "--field-separator"}}),
	"cut": mk(fl{short: "sz", shortValue: "dfbc", long: []string{"--complement", "--only-delimited"},
		longValue: []string{"--delimiter", "--fields", "--bytes", "--characters"}}),
	"diff": mk(fl{short: "uqNwbBiyas", shortValue: "U",
		long:      []string{"--brief", "--new-file", "--ignore-all-space", "--side-by-side", "--text"},
		longValue: []string{"--unified"}, longOptional: []string{"--color"}}),
	"which": mk(fl{short: "a"}),
	"du": mk(fl{short: "shackmxb", shortValue: "d",
		long:      []string{"--summarize", "--human-readable", "--all", "--total", "--apparent-size"},
		longValue: []string{"--max-depth"}}),
	"df":       mk(fl{short: "hTkail", long: []string{"--human-readable", "--print-type"}}),
	"whoami":   mk(fl{noPositional: true}),
	"true":     mk(fl{}),
	"false":    mk(fl{}),
	"basename": mk(fl{short: "az", shortValue: "s"}),
	"dirname":  mk(fl{short: "z"}),
	"realpath": mk(fl{short: "emsqz"}),
	"readlink": mk(fl{short: "femnqsz"}),
	"jq": mk(fl{short: "rcnesSCMaj",
		long: []string{"--raw-output", "--compact-output", "--null-input", "--exit-status", "--slurp",
			"--sort-keys", "--color-output", "--monochrome-output", "--ascii-output", "--join-output"}}),
	"cd":    mk(fl{}),
	"pushd": mk(fl{}),
	"popd":  mk(fl{noPositional: true}),
}

var makeFlags = map[string]flags{
	"mkdir": mk(fl{short: "pv", shortValue: "m", long: []string{"--parents", "--verbose"}, longValue: []string{"--mode"}}),
	"touch": mk(fl{short: "acm", shortValue: "dtr", longValue: []string{"--date", "--reference"}}),
}

var recursiveReaders = set("grep", "find", "du", "ls")

var findValue = set("-name", "-iname", "-path", "-ipath", "-wholename", "-iwholename", "-type", "-xtype",
	"-maxdepth", "-mindepth", "-newer", "-mtime", "-mmin", "-atime", "-amin", "-ctime", "-cmin", "-size",
	"-regex", "-iregex", "-user", "-group", "-perm")

var findPlain = set("-print", "-print0", "-empty", "-not", "-o", "-a", "-and", "-or", "-prune", "-true",
	"-false", "-readable", "-writable", "-executable", "-depth", "-xdev", "-mount", "-P", "(", ")", "!", ",")

var (
	pytestFlags = mk(fl{short: "qvxsl", shortValue: "kmrWn",
		long: []string{"--lf", "--last-failed", "--ff", "--failed-first", "--nf", "--sw", "--stepwise",
			"--no-header", "--co", "--collect-only", "--quiet", "--verbose", "--exitfirst", "--showlocals"},
		longValue: []string{"--maxfail", "--tb", "--durations", "--deselect", "--ignore", "--ignore-glob",
			"--color", "--capture", "--import-mode", "--timeout"}})
	ruffCheck = mk(fl{short: "q", long: []string{"--quiet", "--no-cache", "--statistics", "--preview", "--diff"},
		longValue: []string{"--select", "--ignore", "--extend-select", "--extend-ignore", "--output-format",
			"--target-version", "--line-length"}})
	ruffFormat = mk(fl{short: "q", long: []string{"--check", "--diff", "--quiet", "--preview", "--no-cache"},
		longValue: []string{"--line-length", "--target-version"}})
	mypyFlags = mk(fl{long: []string{"--strict", "--ignore-missing-imports", "--show-error-codes", "--pretty",
		"--no-error-summary", "--check-untyped-defs"}, longValue: []string{"--python-version"}})
	goTest = mk(fl{singleDash: true, long: []string{"-v", "-race", "-short", "-cover", "-failfast", "-json", "-benchmem"},
		longValue: []string{"-run", "-count", "-timeout", "-p", "-bench", "-cpu", "-tags", "-skip", "-parallel"}})
	goVet     = mk(fl{singleDash: true, long: []string{"-v", "-json"}, longValue: []string{"-tags"}})
	cargoTest = mk(fl{short: "q", shortValue: "pj",
		long: []string{"--release", "--quiet", "--all", "--workspace", "--lib", "--bins", "--no-fail-fast", "--doc",
			"--all-features", "--no-default-features", "--locked", "--frozen", "--offline"},
		longValue: []string{"--package", "--test", "--features", "--jobs"}})
	libtest = mk(fl{short: "q", long: []string{"--nocapture", "--ignored", "--include-ignored", "--exact",
		"--show-output", "--quiet"}, longValue: []string{"--test-threads", "--skip"}})
	uvRun     = mk(fl{short: "q", long: []string{"--no-sync", "--frozen", "--locked", "--offline", "--quiet"}})
	curlFlags = mk(fl{short: "sSLfIikv", shortValue: "HAem",
		long: []string{"--silent", "--show-error", "--location", "--fail", "--head", "--include", "--insecure",
			"--verbose", "--compressed", "--http1.1", "--http2"},
		longValue: []string{"--header", "--user-agent", "--referer", "--max-time", "--connect-timeout", "--retry"}})
)

func first(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func indexOf(ss []string, s string) int {
	for i, x := range ss {
		if x == s {
			return i
		}
	}
	return -1
}

func eqList(a []string, b ...string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func splitDashDash(rest []string) ([]string, []string) {
	cut := indexOf(rest, "--")
	if cut < 0 {
		return rest, nil
	}
	return rest[:cut], rest[cut+1:]
}

func firstNonFlag(rest []string) int {
	for i, a := range rest {
		if !strings.HasPrefix(a, "-") {
			return i
		}
	}
	return -1
}

func isTestRun(head string, args []string) bool {
	ok := func(a []string, f flags) bool { _, good := operands(a, f); return good }
	switch head {
	case "pytest":
		return ok(args, pytestFlags)
	case "ruff":
		if len(args) > 0 && args[0] == "check" {
			return ok(args[1:], ruffCheck)
		}
		if len(args) > 0 && args[0] == "format" && (indexOf(args, "--check") >= 0 || indexOf(args, "--diff") >= 0) {
			return ok(args[1:], ruffFormat)
		}
		return false
	case "mypy":
		return ok(args, mypyFlags)
	case "go":
		if len(args) > 0 && args[0] == "test" {
			return ok(args[1:], goTest)
		}
		if len(args) > 0 && args[0] == "vet" {
			return ok(args[1:], goVet)
		}
		return false
	case "cargo":
		if first(args) != "test" || len(args) == 0 {
			return false
		}
		a, b := splitDashDash(args[1:])
		return ok(a, cargoTest) && ok(b, libtest)
	case "npm", "pnpm", "yarn":
		return eqList(args, "test") || eqList(args, "run", "test")
	case "node":
		if len(args) == 0 || args[0] != "--test" {
			return false
		}
		for _, a := range args[1:] {
			if strings.HasPrefix(a, "-") {
				return false
			}
		}
		return true
	case "make":
		return eqList(args, "test") || eqList(args, "check")
	case "python", "python3":
		return len(args) >= 2 && args[0] == "-m" && args[1] == "pytest" && ok(args[2:], pytestFlags)
	case "uv":
		if len(args) == 0 || args[0] != "run" {
			return false
		}
		rest := args[1:]
		tool := firstNonFlag(rest)
		if tool < 0 || !ok(rest[:tool], uvRun) {
			return false
		}
		switch rest[tool] {
		case "pytest", "ruff", "mypy":
		default:
			return false
		}
		return isTestRun(rest[tool], rest[tool+1:])
	}
	return false
}

func testOperands(head string, args []string) ([]string, bool) {
	switch head {
	case "pytest":
		return operands(args, pytestFlags)
	case "ruff":
		if len(args) > 0 && args[0] == "check" {
			return operands(args[1:], ruffCheck)
		}
		return operands(tail(args), ruffFormat)
	case "mypy":
		return operands(args, mypyFlags)
	case "go":
		if len(args) > 0 && args[0] == "test" {
			return operands(args[1:], goTest)
		}
		return operands(tail(args), goVet)
	case "cargo":
		a, b := splitDashDash(tail(args))
		x, ok1 := operands(a, cargoTest)
		y, ok2 := operands(b, libtest)
		if !ok1 || !ok2 {
			return nil, false
		}
		return append(x, y...), true
	case "node":
		return tail(args), true
	case "python", "python3":
		if len(args) < 2 {
			return operands(nil, pytestFlags)
		}
		return operands(args[2:], pytestFlags)
	case "uv":
		rest := tail(args)
		tool := firstNonFlag(rest)
		if tool < 0 {
			return nil, false
		}
		return testOperands(rest[tool], rest[tool+1:])
	}
	return []string{}, true
}

func tail(a []string) []string {
	if len(a) == 0 {
		return nil
	}
	return a[1:]
}

func findPaths(args []string) []string {
	var out []string
	for _, a := range args {
		if a == "-P" {
			continue
		}
		if strings.HasPrefix(a, "-") || a == "(" || a == ")" || a == "!" || a == "," {
			break
		}
		out = append(out, a)
	}
	if len(out) == 0 {
		return []string{"."}
	}
	return out
}

func findClass(args []string) string {
	i := 0
	for i < len(args) {
		a := args[i]
		i++
		switch {
		case findValue[a]:
			if i >= len(args) {
				return "unknown"
			}
			i++
		case findPlain[a]:
		case strings.HasPrefix(a, "-"):
			return "unknown"
		}
	}
	return "read"
}

var gitRead = map[string]flags{
	"status": mk(fl{short: "sb", shortOptional: "u", long: []string{"--short", "--branch", "--ignored"},
		longOptional: []string{"--porcelain", "--untracked-files"}}),
	"log": mk(fl{short: "p", shortValue: "nSG",
		long: []string{"--oneline", "--stat", "--graph", "--decorate", "--all", "--patch", "--no-merges",
			"--reverse", "--name-only", "--name-status", "--shortstat", "--first-parent", "--no-color",
			"--abbrev-commit", "--follow", "--no-ext-diff"},
		longValue: []string{"--max-count", "--since", "--until", "--author", "--grep", "--format", "--pretty",
			"--date", "--decorate"},
		numeric: true}),
	"diff": mk(fl{short: "w", shortValue: "U",
		long: []string{"--stat", "--cached", "--staged", "--name-only", "--name-status", "--shortstat",
			"--no-color", "--numstat", "--check", "--no-ext-diff"},
		longValue: []string{"--unified", "--diff-filter", "--color", "--stat"}}),
	"show": mk(fl{short: "s", long: []string{"--stat", "--name-only", "--name-status", "--oneline", "--no-patch",
		"--no-ext-diff"}, longValue: []string{"--format", "--pretty"}}),
	"blame": mk(fl{short: "wse", shortValue: "L"}),
	"rev-parse": mk(fl{long: []string{"--abbrev-ref", "--show-toplevel", "--short", "--verify", "--git-dir",
		"--is-inside-work-tree", "--symbolic-full-name"}, longValue: []string{"--short"}}),
	"ls-files": mk(fl{short: "modscz", long: []string{"--others", "--modified", "--deleted", "--cached",
		"--exclude-standard", "--stage"}}),
	"grep": mk(fl{short: "nilwvEFcI", shortValue: "e", long: []string{"--cached", "--line-number", "--ignore-case",
		"--count", "--files-with-matches"}}),
	"describe": mk(fl{long: []string{"--tags", "--always", "--dirty", "--long"}, longValue: []string{"--abbrev", "--match"}}),
	"shortlog": mk(fl{short: "sne", long: []string{"--summary", "--numbered", "--email"}}),
	"cat-file": mk(fl{short: "tpse"}),
	"remote":   mk(fl{short: "v", noPositional: true}),
}

var gitWrite = map[string]flags{
	"add": mk(fl{short: "uAvnN", long: []string{"--all", "--update", "--verbose", "--dry-run", "--intent-to-add"}}),
	"commit": mk(fl{short: "aqvs", shortValue: "mF",
		long:      []string{"--all", "--quiet", "--verbose", "--signoff", "--allow-empty", "--no-edit", "--amend"},
		longValue: []string{"--message", "--author", "--file"}}),
	"merge": mk(fl{shortValue: "m", long: []string{"--no-ff", "--ff-only", "--ff", "--no-edit", "--squash", "--no-commit"},
		longValue: []string{"--message"}}),
	"rebase":      mk(fl{long: []string{"--continue"}, longValue: []string{"--onto"}}),
	"cherry-pick": mk(fl{short: "n", long: []string{"--no-commit", "--continue"}}),
	"revert":      mk(fl{short: "n", long: []string{"--no-edit", "--no-commit", "--continue"}}),
	"reset":       mk(fl{short: "q", long: []string{"--soft", "--mixed", "--quiet"}}),
	"switch":      mk(fl{shortValue: "c", long: []string{"--detach"}, longValue: []string{"--create"}}),
}

var gitTreeMovers = set("add", "commit", "merge", "rebase", "cherry-pick", "revert", "reset", "switch", "checkout", "stash")

var (
	gitBranchList = optional(mk(fl{short: "arv", long: []string{"--all", "--remotes", "--verbose", "--list", "--show-current"},
		longValue: []string{"--contains", "--merged", "--no-merged", "--sort"}}))
	gitTag       = optional(mk(fl{short: "la", shortValue: "m", long: []string{"--list"}, longValue: []string{"--message"}}))
	gitFetch     = optional(mk(fl{short: "qv", long: []string{"--all", "--tags", "--no-tags", "--quiet", "--verbose"}}))
	gitStashSave = optional(mk(fl{short: "uq", shortValue: "m", long: []string{"--include-untracked", "--quiet"},
		longValue: []string{"--message"}}))
	gitDelete = set("clean", "restore", "rm", "gc", "prune", "filter-branch", "update-ref")
)

func init() {
	for k, v := range gitRead {
		gitRead[k] = optional(v)
	}
	for k, v := range gitWrite {
		gitWrite[k] = optional(v)
	}
}

func movesTree(tokens []string) bool {
	if len(tokens) > 1 && tokens[0] == "git" && gitTreeMovers[tokens[1]] {
		return true
	}
	return len(tokens) > 0 && isTestRun(tokens[0], tokens[1:])
}

func isOutputWord(w string) bool {
	if strings.HasPrefix(w, "--out") {
		return true
	}
	return strings.HasPrefix(w, "-o") && !strings.HasPrefix(w, "--") && len(w) > 2
}

func isHTTPURL(w string) bool {
	low := strings.ToLower(w)
	return strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://")
}

func sendsAFile(w string) bool {
	if strings.HasPrefix(w, "@") {
		return true
	}
	if strings.HasPrefix(w, "--") {
		name, value, eq := strings.Cut(w, "=")
		return eq && strings.HasPrefix(value, "@") && (strings.HasPrefix(name, "--header") || strings.HasPrefix(name, "--data"))
	}
	return strings.HasPrefix(w, "-") && (strings.Contains(w, "H@") || strings.Contains(w, "d@"))
}

func gitClass(args []string) string {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "unknown"
	}
	sub, rest := args[0], args[1:]
	switch {
	case sub == "push":
		for _, a := range rest {
			switch a {
			case "-f", "--force", "--mirror", "--delete", "-d", "--prune":
				return "force_push"
			}
			if strings.HasPrefix(a, "--force") || strings.HasPrefix(a, "+") || strings.HasPrefix(a, ":") {
				return "force_push"
			}
		}
		return "push_shared"
	case gitDelete[sub]:
		return "delete"
	}
	if spec, ok := gitRead[sub]; ok {
		if _, good := operands(rest, spec); good {
			return "read"
		}
		return "unknown"
	}
	if spec, ok := gitWrite[sub]; ok {
		if _, good := operands(rest, spec); good {
			return "write_inside_workspace"
		}
		return "unknown"
	}
	switch sub {
	case "fetch":
		ops, good := operands(rest, gitFetch)
		if !good {
			return "unknown"
		}
		for _, o := range ops {
			if strings.Contains(o, ":") || strings.HasPrefix(o, "+") {
				return "unknown"
			}
		}
		return "network_get"
	case "branch":
		ops, good := operands(rest, gitBranchList)
		if !good || len(ops) > 2 {
			return "unknown"
		}
		if len(ops) > 0 {
			return "write_inside_workspace"
		}
		return "read"
	case "tag":
		ops, good := operands(rest, gitTag)
		if !good {
			return "unknown"
		}
		if len(ops) > 0 && indexOf(rest, "-l") < 0 && indexOf(rest, "--list") < 0 {
			return "write_inside_workspace"
		}
		return "read"
	case "stash":
		verb, tl := "push", rest
		if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
			verb, tl = rest[0], rest[1:]
		}
		switch verb {
		case "drop", "clear":
			return "delete"
		case "list", "show":
			if _, good := operands(tl, mk(fl{short: "p", long: []string{"--stat"}})); good {
				return "read"
			}
			return "unknown"
		case "pop", "apply":
			if _, good := operands(tl, mk(fl{long: []string{"--index"}})); good {
				return "write_inside_workspace"
			}
			return "unknown"
		case "push", "save":
			if _, good := operands(tl, gitStashSave); good {
				return "write_inside_workspace"
			}
			return "unknown"
		}
		return "unknown"
	case "checkout":
		if (len(rest) == 2 || len(rest) == 3) && (rest[0] == "-b" || rest[0] == "-B") {
			plain := true
			for _, x := range rest[1:] {
				if strings.HasPrefix(x, "-") {
					plain = false
				}
			}
			if plain {
				return "write_inside_workspace"
			}
		}
		return "delete"
	}
	return "unknown"
}

func (r *runner) coversHome(arg string, cwd *string) bool {
	defer r.enter()()
	target, ok := r.resolvePath(arg, cwd)
	if !ok {
		return false
	}
	home := r.realpath(r.expanduser("~"))
	c, good := commonpath(target, home)
	return good && c == target
}

func isWordByte(c byte) bool {
	return isAlphaByte(c) || (c >= '0' && c <= '9') || c == '_'
}

// jqEnv is effects._JQ_ENV.search: env not after a word character or $,
// and not before a word character; or $ENV not before a word character.
func jqEnv(a string) bool {
	for i := 0; i+3 <= len(a); i++ {
		if a[i:i+3] == "env" && (i == 0 || (!isWordByte(a[i-1]) && a[i-1] != '$')) &&
			(i+3 == len(a) || !isWordByte(a[i+3])) {
			return true
		}
	}
	for i := 0; i+4 <= len(a); i++ {
		if a[i:i+4] == "$ENV" && (i+4 == len(a) || !isWordByte(a[i+4])) {
			return true
		}
	}
	return false
}

var noPathHeads = set("echo", "printf", "true", "false", "which", "whoami", "pwd", "basename", "dirname")
var cwdReaders = set("ls", "du", "grep")

func strp(s string) *string { return &s }

// commandClass is effects._command_class.
func (r *runner) commandClass(tokens []string, root *workspace, cwd, base, cwdBase *string) string {
	defer r.enter()()
	if len(tokens) == 0 {
		return "unknown"
	}
	head, args := tokens[0], tokens[1:]
	if strings.ContainsAny(head, "=/") {
		return "unknown"
	}
	if head == "env" || head == "printenv" {
		return "credential"
	}
	if head == "jq" {
		for _, a := range args {
			if jqEnv(a) {
				return "credential"
			}
		}
	}
	if head == "curl" {
		for _, a := range args {
			if sendsAFile(a) {
				return "credential"
			}
		}
	}
	for _, a := range args {
		if isOutputWord(a) {
			return "unknown"
		}
	}
	for _, a := range args {
		if strings.ContainsAny(a, "$`") {
			return "unknown"
		}
	}
	for _, a := range args {
		paths := []string{a}
		if head == "git" && strings.Contains(a, ":") && !strings.Contains(a, "://") {
			_, revPath, _ := strings.Cut(a, ":")
			for _, seg := range strings.Split(strings.ReplaceAll(revPath, "\\", "/"), "/") {
				if seg == ".." {
					return "unknown"
				}
			}
			paths = append(paths, revPath)
		}
		// any(p and _is_credential_path(p, cwd) for p in paths)
		if r.genexpr(func() bool {
			for _, p := range paths {
				if p != "" && r.isCredentialPath(p, cwd) {
					return true
				}
			}
			return false
		}) {
			return "credential"
		}
	}
	if credentialHeads[head] {
		return "credential"
	}
	if head == "gh" {
		if first(args) == "auth" {
			return "credential"
		}
		return "push_shared"
	}
	if prodHeads[head] {
		return "prod"
	}
	if deleteHeads[head] {
		return "delete"
	}
	if verb, ok := publish[head]; ok && first(args) == verb && len(args) > 0 {
		return "push_shared"
	}
	if recursiveReaders[head] && r.genexpr(func() bool {
		for _, a := range args {
			if !strings.HasPrefix(a, "-") && r.coversHome(a, cwd) {
				return true
			}
		}
		return false
	}) {
		return "credential"
	}
	if head == "git" {
		cls := gitClass(args)
		if tierFor(cls) != tierUndoable {
			return cls
		}
		if !r.cwdInside(cwd, cwdBase) || !holdsGitDir(cwdBase) {
			return "unknown"
		}
		files, ok := gitFileValues(args)
		if !ok {
			return "unknown"
		}
		places := []string{}
		bad := false
		for _, p := range files {
			pl := r.readPlace(p, base, cwd)
			places = append(places, pl)
			if pl != "read" {
				bad = true
			}
		}
		if bad {
			return worst(append([]string{"unknown"}, places...))
		}
		return cls
	}
	if head == "curl" {
		ops, ok := operands(args, curlFlags)
		if !ok || len(ops) == 0 {
			return "unknown"
		}
		for _, o := range ops {
			if !isHTTPURL(o) {
				return "unknown"
			}
		}
		return "network_get"
	}
	if isTestRun(head, args) {
		if !r.cwdInside(cwd, cwdBase) {
			return "unknown"
		}
		ops, ok := testOperands(head, args)
		if !ok {
			return "unknown"
		}
		places := []string{}
		all := true
		for _, p := range ops {
			pl := r.readPlace(p, base, cwd)
			places = append(places, pl)
			if pl != "read" {
				all = false
			}
		}
		if all {
			return "test_run"
		}
		return worst(append([]string{"unknown"}, places...))
	}
	if head == "find" {
		cls := findClass(args)
		if cls != "read" {
			return cls
		}
		var places []string
		for _, p := range findPaths(args) {
			places = append(places, r.readPlace(p, base, cwd))
		}
		return worst(places)
	}
	if spec, ok := makeFlags[head]; ok {
		paths, good := operands(args, spec)
		if !good || len(paths) == 0 {
			return "unknown"
		}
		var classes []string
		for _, p := range paths {
			classes = append(classes, r.writeClass(p, root, cwd))
		}
		return worst(classes)
	}
	if spec, ok := readFlags[head]; ok {
		ops, good := operands(args, spec)
		if !good {
			return "unknown"
		}
		if noPathHeads[head] || cwdHeads[head] {
			return "read"
		}
		if len(ops) == 0 && cwdReaders[head] {
			ops = []string{"."}
		}
		// _worst(["read", *(_read_place(...) for p in ops)]): a generator.
		classes := []string{"read"}
		r.genexpr(func() bool {
			for _, p := range ops {
				classes = append(classes, r.readPlace(p, base, cwd))
			}
			return false
		})
		return worst(classes)
	}
	return "unknown"
}

func holdsGitDir(base *string) bool {
	return base != nil && *base != "" && lexists(join(*base, ".git"))
}

// gitFileValues is effects._git_file_values; ok is false when a -F or
// --file names no path.
func gitFileValues(args []string) ([]string, bool) {
	if first(args) != "commit" || len(args) == 0 {
		return []string{}, true
	}
	var out []string
	rest := args[1:]
	i := 0
	for i < len(rest) {
		a := rest[i]
		i++
		if a == "--" {
			break
		}
		var value *string
		switch {
		case a == "--file":
			if i < len(rest) {
				value = strp(rest[i])
			}
			i++
		case strings.HasPrefix(a, "--file="):
			_, v, _ := strings.Cut(a, "=")
			value = strp(v)
		case strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(strings.SplitN(a, "m", 2)[0], "F"):
			t := a[strings.Index(a, "F")+1:]
			if t != "" {
				value = strp(t)
			} else {
				if i < len(rest) {
					value = strp(rest[i])
				}
				i++
			}
		default:
			continue
		}
		if value == nil || *value == "" {
			return nil, false
		}
		out = append(out, *value)
	}
	return out, true
}

func (r *runner) cwdInside(cwd, base *string) bool {
	defer r.enter()()
	if cwd == nil || *cwd == "" || base == nil || *base == "" {
		return false
	}
	return under(r.realpath(*cwd), r.realpath(*base))
}

func isShellSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f' || (c >= 0x1c && c <= 0x1f)
}

// unsafeWords is effects._unsafe_words.
func unsafeWords(text string) bool {
	i, n := 0, len(text)
	start := true
	quote := byte(0)
	for i < n {
		c := text[i]
		if quote == '\'' {
			if c == '\'' {
				quote = 0
			}
			i++
			continue
		}
		if quote == '"' {
			if c == '\\' {
				i += 2
				continue
			}
			if c == '"' {
				quote = 0
			} else if c == '$' || c == '`' {
				return true
			}
			i++
			continue
		}
		if c == '\\' {
			i += 2
			start = false
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			start = false
			i++
			continue
		}
		if isShellSpace(c) {
			start = true
			i++
			continue
		}
		if strings.IndexByte("$`*?[{}", c) >= 0 {
			return true
		}
		if c == '~' && start {
			if i+1 < n && text[i+1] != '/' && !isShellSpace(text[i+1]) {
				return true
			}
		}
		start = false
		i++
	}
	return false
}

func unsafeRedirect(p string) bool {
	return strings.ContainsAny(p, "$`*?[{}") || (strings.HasPrefix(p, "~") && len(p) > 1 && p[1] != '/')
}

func (r *runner) isRelativeWord(w string) bool {
	if w == "" || strings.HasPrefix(w, "-") {
		return false
	}
	return !isabs(r.expanduser(w))
}

// nextCwd is effects._next_cwd; nil is None.
func (r *runner) nextCwd(tokens []string, cwd *string) *string {
	defer r.enter()()
	if len(tokens) == 0 || !cwdHeads[tokens[0]] {
		return cwd
	}
	args := tokens[1:]
	if tokens[0] == "popd" {
		return nil
	}
	var target string
	switch {
	case tokens[0] == "cd" && len(args) == 0:
		target = r.expanduser("~")
	case len(args) == 1 && !strings.HasPrefix(args[0], "-"):
		target = args[0]
	default:
		return nil
	}
	for _, seg := range strings.Split(strings.ReplaceAll(target, "\\", "/"), "/") {
		if seg == ".." {
			return nil
		}
	}
	expanded := r.expanduser(target)
	if !isabs(expanded) {
		if cwd == nil || *cwd == "" {
			return nil
		}
		expanded = join(*cwd, expanded)
	}
	logical := normpath(expanded)
	if r.realpath(logical) != logical {
		return nil
	}
	return &logical
}

// shellClass is effects._shell_class.
func (r *runner) shellClass(command string, root *workspace, cwd *string) string {
	defer r.enter()()
	if pyStrip(command) == "" {
		return "unknown"
	}
	// shell_parser_available() decomposes "a && b" two frames deeper.
	if d, exc := r.decomposeAt("a && b", r.depth+3); exc != nil || !d.OK {
		return "unknown"
	}
	dec, exc := r.decompose(command)
	if exc != nil || !dec.OK || len(dec.Commands) == 0 {
		return "unknown"
	}
	var tokenLists [][]string
	for _, c := range dec.Commands {
		toks, err := verify.ShlexSplit(c)
		if err != nil {
			return "unknown"
		}
		tokenLists = append(tokenLists, toks)
	}
	var base *string
	if root != nil {
		base = strp(root.Base)
	}
	movedCwd := false
	for _, t := range tokenLists {
		if len(t) > 0 && cwdHeads[t[0]] {
			movedCwd = true
		}
	}
	if movedCwd {
		root = nil
	}
	var classes []string
	here := root
	movedTree := false
	readBase := base
	cwds := []*string{cwd}
	lost := false
	for k, tokens := range tokenLists {
		text := dec.Commands[k]
		if unsafeWords(text) {
			classes = append(classes, "unknown")
		}
		if lost {
			for _, a := range tail(tokens) {
				if r.isRelativeWord(a) {
					classes = append(classes, "unknown")
					break
				}
			}
		}
		classes = append(classes, r.commandClass(tokens, here, cwd, readBase, base))
		if movesTree(tokens) {
			here = nil
			movedTree = true
			readBase = nil
		}
		cwd = r.nextCwd(tokens, cwd)
		// Each distinct cwd once, and past maxCwds the line counts as
		// moved to a place the gate cannot follow (50684bb).
		if !containsCwd(cwds, cwd) && len(cwds) >= maxCwds {
			cwd = nil
			lost = true
		}
		if !containsCwd(cwds, cwd) {
			cwds = append(cwds, cwd)
		}
		if len(tokens) > 0 && cwdHeads[tokens[0]] && cwd == nil {
			lost = true
		}
	}
	redirectRoot := root
	if movedTree {
		redirectRoot = nil
	}
	redirectCwd := cwds[0]
	if movedCwd {
		redirectCwd = nil
	}
	for _, p := range append(append([]string{}, dec.Writes...), dec.Reads...) {
		if unsafeRedirect(p) {
			classes = append(classes, "unknown")
		}
		if r.genexpr(func() bool {
			for _, c := range cwds {
				if r.isCredentialPath(p, c) {
					return true
				}
			}
			return false
		}) {
			classes = append(classes, "credential")
		}
		if lost && r.isRelativeWord(p) {
			classes = append(classes, "unknown")
		}
	}
	r.genexpr(func() bool {
		for _, p := range dec.Writes {
			classes = append(classes, r.writeClass(p, redirectRoot, redirectCwd))
		}
		return false
	})
	for _, p := range dec.Reads {
		r.genexpr(func() bool {
			for _, c := range cwds {
				classes = append(classes, r.readPlace(p, readBase, c))
			}
			return false
		})
	}
	return worst(classes)
}

// genexpr runs fn in the frame of a Python generator expression, one
// deeper than the caller: any(...), or a list extended from a generator.
func (r *runner) genexpr(fn func() bool) bool {
	defer r.enter()()
	return fn()
}

// workspaceRoot is effects.workspace_root.
func (r *runner) workspaceRoot(cwd any, fixed string, fixedSet bool) *workspace {
	defer r.enter()()
	c, ok := cwd.(string)
	if !ok || c == "" || !fixedSet || fixed == "" || !isabs(c) || !isabs(fixed) {
		return nil
	}
	a, b := r.realpath(c), r.realpath(fixed)
	if cp, good := commonpath(a, b); !good || cp != b {
		return nil
	}
	return &workspace{Cwd: a, Base: b}
}

// effectClass is effects.effect_class for a normalized record.
func (r *runner) effectClass(rec *record, root *workspace, callCwd *string) (cls string) {
	defer r.enter()()
	// A path the gate cannot resolve is unknown.
	if exc := catch(func() { cls = r.effectClassBody(rec, root, callCwd) }); exc != nil {
		return "unknown"
	}
	return cls
}

// effectClassBody is effects._effect_class.
func (r *runner) effectClassBody(rec *record, root *workspace, callCwd *string) string {
	defer r.enter()()
	here := callCwd
	var base *string
	if root != nil {
		here = strp(root.Cwd)
		base = strp(root.Base)
	}
	switch rec.StepType {
	case "file_read":
		if rec.Path == "" {
			return "unknown"
		}
		return r.readPlace(rec.Path, base, here)
	case "file_write":
		if rec.Path == "" {
			return "unknown"
		}
		return r.writeClass(rec.Path, root, here)
	case "network":
		if rec.ToolName == "WebFetch" || rec.ToolName == "WebSearch" {
			return "network_get"
		}
		return "unknown"
	case "shell":
		return r.shellClass(rec.Command, root, here)
	}
	return "unknown"
}

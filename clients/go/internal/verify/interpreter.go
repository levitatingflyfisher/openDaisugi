package verify

import "strings"

// SHELL_INTERPRETERS mirrors opendaisugi.models.SHELL_INTERPRETERS.
var ShellInterpreters = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true,
	"ksh": true, "csh": true, "tcsh": true, "xargs": true, "find": true,
	"python": true, "python3": true, "python2": true, "perl": true,
	"ruby": true, "node": true, "deno": true, "make": true, "awk": true,
	"gawk": true, "sed": true, "eval": true, "exec": true, "source": true,
	"env": true, "timeout": true, "nice": true, "nohup": true, "time": true,
	"stdbuf": true, "command": true, "setsid": true, "ionice": true,
	"sudo": true, "doas": true, "watch": true,
	"chroot": true, "flock": true, "busybox": true, "script": true,
	"parallel": true, "coproc": true, "trap": true,
	"systemd-run": true, "screen": true, "unbuffer": true, "expect": true,
	"runuser": true, "taskset": true, "chrt": true, "prlimit": true,
	"unshare": true, "nsenter": true, "entr": true, "batch": true,
}

var shellCInterpreters = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true,
	"fish": true, "csh": true, "tcsh": true,
}

var opaqueInterpreters = map[string]bool{
	"python": true, "python3": true, "python2": true, "perl": true,
	"ruby": true, "node": true, "deno": true, "awk": true, "gawk": true,
	"sed": true, "make": true, "eval": true, "exec": true, "source": true,
	"sudo": true, "doas": true, "watch": true,
	// Wrappers whose inner command nothing unwraps.
	"chroot": true, "flock": true, "busybox": true, "script": true,
	"parallel": true, "coproc": true, "trap": true,
	"systemd-run": true, "screen": true, "unbuffer": true, "expect": true,
	"runuser": true, "taskset": true, "chrt": true, "prlimit": true,
	"unshare": true, "nsenter": true, "entr": true, "batch": true,
}

// longOpts maps a long option to its kind: "n" takes no value, "v" takes a
// value (--name=V or the next word), "o" takes a value only as --name=V.
type longOpts map[string]string

var envLong = longOpts{
	"--ignore-environment": "n", "--null": "n", "--unset": "v", "--chdir": "v",
	"--split-string": "v", "--argv0": "v", "--block-signal": "o",
	"--default-signal": "o", "--ignore-signal": "o", "--list-signal-handling": "n",
	"--debug": "n", "--help": "n", "--version": "n",
}

var xargsLong = longOpts{
	"--null": "n", "--arg-file": "v", "--delimiter": "v", "--eof": "o",
	"--replace": "o", "--max-lines": "o", "--max-args": "v", "--open-tty": "n",
	"--max-procs": "v", "--interactive": "n", "--process-slot-var": "v",
	"--no-run-if-empty": "n", "--max-chars": "v", "--show-limits": "n",
	"--verbose": "n", "--exit": "n", "--help": "n", "--version": "n",
}

func withHelp(m longOpts) longOpts {
	m["--help"] = "n"
	m["--version"] = "n"
	return m
}

// wrapperSpec is interpreter_parse._TRANSPARENT_WRAPPERS: the short letters
// that take a value, the short letters that take one only attached, the long
// options, and the count of positionals before the command.
type wrapperSpec struct {
	shortValue, shortOptional string
	long                      longOpts
	positionalSkip            int
}

var transparentWrappers = map[string]wrapperSpec{
	"timeout": {"ks", "", withHelp(longOpts{
		"--foreground": "n", "--kill-after": "v", "--preserve-status": "n",
		"--signal": "v", "--verbose": "n",
	}), 1},
	"nice":  {"n", "", withHelp(longOpts{"--adjustment": "v"}), 0},
	"nohup": {"", "", withHelp(longOpts{}), 0},
	// GNU time: -o FILE writes FILE, -f FORMAT takes a format.
	"time": {"fo", "", withHelp(longOpts{
		"--append": "n", "--format": "v", "--output": "v", "--portability": "n",
		"--quiet": "n", "--verbose": "n",
	}), 0},
	"stdbuf":  {"ioe", "", withHelp(longOpts{"--input": "v", "--output": "v", "--error": "v"}), 0},
	"command": {"", "", longOpts{}, 0},
	"setsid":  {"", "", withHelp(longOpts{"--ctty": "n", "--fork": "n", "--wait": "n"}), 0},
	// util-linux ionice: -c, -n, -p, -P and -u take values; -t takes none.
	"ionice": {"cnpPu", "", withHelp(longOpts{
		"--class": "v", "--classdata": "v", "--pid": "v", "--pgid": "v",
		"--ignore": "n", "--uid": "v",
	}), 0},
}

const (
	xargsShortValue    = "nIPLdEsa"
	xargsShortOptional = "eil"
)

// optionHeads is interpreter_parse.OPTION_HEADS: the heads whose payload is
// opaque when an option cannot be followed.
func optionHead(head string) bool {
	if head == "env" || head == "xargs" {
		return true
	}
	_, ok := transparentWrappers[head]
	return ok
}

// resolveLong is interpreter_parse.resolve_long: the long option arg selects,
// as GNU getopt_long reads it. An exact name wins, otherwise a unique prefix;
// an ambiguous or unknown name, or a value given to an option that takes
// none, is not ok.
func resolveLong(arg string, table longOpts) (name string, value *string, kind string, ok bool) {
	n, v, eq := strings.Cut(arg, "=")
	full := n
	if _, exact := table[n]; !exact {
		found := ""
		count := 0
		for k := range table {
			if strings.HasPrefix(k, n) {
				found = k
				count++
			}
		}
		if len(n) <= 2 || count != 1 {
			return "", nil, "", false
		}
		full = found
	}
	kind = table[full]
	if eq && kind == "n" {
		return "", nil, "", false
	}
	if eq {
		return full, &v, kind, true
	}
	return full, nil, kind, true
}

type optValue struct {
	name  string
	value *string
}

// scanOptions is interpreter_parse.scan_options: the leading options of
// tokens[start:], the index of the first word that is not one, and ok false
// when a long option is ambiguous or unknown.
func scanOptions(tokens []string, start int, shortValue, shortOptional string, long longOpts) (int, []optValue, bool) {
	n := len(tokens)
	i := start
	var opts []optValue
	for i < n {
		t := tokens[i]
		if t == "--" {
			i++
			break
		}
		if strings.HasPrefix(t, "--") {
			name, value, kind, ok := resolveLong(t, long)
			if !ok {
				return 0, nil, false
			}
			i++
			if kind == "v" && value == nil && i < n {
				v := tokens[i]
				value = &v
				i++
			}
			opts = append(opts, optValue{name, value})
			continue
		}
		if strings.HasPrefix(t, "-") && t != "-" {
			i++
			for j := 1; j < len(t); j++ {
				c := t[j]
				rest := t[j+1:]
				if strings.IndexByte(shortValue, c) >= 0 {
					switch {
					case rest != "":
						opts = append(opts, optValue{"-" + string(c), &rest})
					case i < n:
						v := tokens[i]
						opts = append(opts, optValue{"-" + string(c), &v})
						i++
					default:
						opts = append(opts, optValue{"-" + string(c), nil})
					}
					break
				}
				if strings.IndexByte(shortOptional, c) >= 0 {
					var v *string
					if rest != "" {
						v = &rest
					}
					opts = append(opts, optValue{"-" + string(c), v})
					break
				}
				opts = append(opts, optValue{"-" + string(c), nil})
			}
			continue
		}
		break
	}
	return i, opts, true
}

func xargsOptions(tokens []string, start int) (int, []optValue, bool) {
	return scanOptions(tokens, start, xargsShortValue, xargsShortOptional, xargsLong)
}

func wrapperOptions(head string, tokens []string, start int) (int, []optValue, bool) {
	spec := transparentWrappers[head]
	return scanOptions(tokens, start, spec.shortValue, spec.shortOptional, spec.long)
}

var findExecFlags = setOf("-exec", "-execdir", "-ok", "-okdir")

func setOf(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, it := range items {
		m[it] = true
	}
	return m
}

// InterpreterPayload mirrors interpreter_parse.InterpreterPayload.
type InterpreterPayload struct {
	Head          string
	InnerCommands []string
	Opaque        bool
	// InnerKinds is interpreter_parse.InterpreterPayload.inner_kinds: for
	// find, "exec" or "execdir" per inner command; empty otherwise.
	InnerKinds []string
}

// ParseInterpreter ports interpreter_parse.parse_interpreter EXACTLY,
// including its shlex.split(posix=True) tokenization and shlex.quote
// re-joining of extracted argv tails.
func ParseInterpreter(command string) (*InterpreterPayload, bool) {
	stripped := strings.TrimSpace(command)
	if stripped == "" {
		return nil, false
	}
	tokens, err := posixSplit(stripped)
	if err != nil {
		return nil, false
	}
	if len(tokens) == 0 {
		return nil, false
	}
	head := tokens[0]
	if strings.HasPrefix(head, "/") {
		// An absolute path names the program: /usr/bin/env is env. A relative
		// path (./timeout, bin/env) is the project's own file and is not.
		head = head[strings.LastIndexByte(head, '/')+1:]
	}
	if !ShellInterpreters[head] {
		return nil, false
	}
	if opaqueInterpreters[head] {
		return &InterpreterPayload{Head: head, Opaque: true}, true
	}
	if shellCInterpreters[head] {
		return parseShellC(head, tokens), true
	}
	if head == "xargs" {
		return parseXargs(head, tokens), true
	}
	if head == "find" {
		return parseFind(head, tokens), true
	}
	if head == "env" {
		return parseEnv(head, tokens), true
	}
	if spec, ok := transparentWrappers[head]; ok {
		return parseWrapper(head, tokens, spec), true
	}
	return &InterpreterPayload{Head: head, Opaque: true}, true
}

func isAlpha(s string) bool {
	if s == "" {
		return false // matches Python's ''.isalpha() == False
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return false
		}
	}
	return true
}

// parseShellC ports interpreter_parse._parse_shell_c EXACTLY, including
// clustered short flags (-ec, -euxc) and the attached-argument form (-cSCRIPT).
func parseShellC(head string, tokens []string) *InterpreterPayload {
	for i := 1; i < len(tokens); i++ {
		tok := tokens[i]
		if len(tok) < 2 || tok[0] != '-' || tok[1] == '-' || !strings.Contains(tok, "c") {
			continue
		}
		cluster := tok[1:]
		cpos := strings.Index(cluster, "c")
		before := cluster[:cpos]
		if before != "" && !isAlpha(before) {
			continue
		}
		attached := cluster[cpos+1:]
		if attached != "" {
			return &InterpreterPayload{Head: head, InnerCommands: []string{attached}}
		}
		if i+1 < len(tokens) {
			return &InterpreterPayload{Head: head, InnerCommands: []string{tokens[i+1]}}
		}
		return &InterpreterPayload{Head: head}
	}
	return &InterpreterPayload{Head: head}
}

// parseWrapper ports interpreter_parse._parse_wrapper.
func parseWrapper(head string, tokens []string, spec wrapperSpec) *InterpreterPayload {
	i, _, ok := wrapperOptions(head, tokens, 1)
	if !ok {
		return &InterpreterPayload{Head: head, Opaque: true}
	}
	skip := spec.positionalSkip
	if remaining := len(tokens) - i; skip > remaining {
		skip = remaining
	}
	if skip < 0 {
		skip = 0
	}
	i += skip
	if i < len(tokens) {
		return &InterpreterPayload{Head: head, InnerCommands: []string{shlexJoin(tokens[i:])}}
	}
	return &InterpreterPayload{Head: head}
}

// parseXargs ports interpreter_parse._parse_xargs.
func parseXargs(head string, tokens []string) *InterpreterPayload {
	i, _, ok := xargsOptions(tokens, 1)
	if !ok {
		return &InterpreterPayload{Head: head, Opaque: true}
	}
	if i < len(tokens) {
		return &InterpreterPayload{Head: head, InnerCommands: []string{shlexJoin(tokens[i:])}}
	}
	return &InterpreterPayload{Head: head}
}

// parseFind ports interpreter_parse._parse_find.
func parseFind(head string, tokens []string) *InterpreterPayload {
	var inners, kinds []string
	i := 0
	for i < len(tokens) {
		if findExecFlags[tokens[i]] {
			flag := tokens[i]
			start := i + 1
			j := start
			for j < len(tokens) && tokens[j] != ";" && tokens[j] != "+" {
				j++
			}
			if j > start {
				inners = append(inners, shlexJoin(tokens[start:j]))
				if flag == "-execdir" || flag == "-okdir" {
					kinds = append(kinds, "execdir")
				} else {
					kinds = append(kinds, "exec")
				}
			}
			i = j + 1
		} else {
			i++
		}
	}
	return &InterpreterPayload{Head: head, InnerCommands: inners, InnerKinds: kinds}
}

// envValueShortFlags is interpreter_parse._ENV_VALUE_SHORT:
// the env options whose value is the next word (or the rest of the bundle).
const envValueShortFlags = "uCSa"

// parseEnv ports interpreter_parse._parse_env: the options that take a value
// consume it, and -S STRING is split into the words that take its place.
func parseEnv(head string, tokens []string) *InterpreterPayload {
	toks := append([]string{}, tokens...)
	splice := func(i, n int, words []string) {
		out := append([]string{}, toks[:i]...)
		out = append(out, words...)
		toks = append(out, toks[i+n:]...)
	}
	i := 1
	for i < len(toks) {
		t := toks[i]
		if strings.HasPrefix(t, "--") && t != "--" {
			name, value, kind, ok := resolveLong(t, envLong)
			if !ok {
				return &InterpreterPayload{Head: head, Opaque: true}
			}
			if name == "--split-string" {
				switch {
				case value != nil:
					splice(i, 1, envSplit(*value))
				case i+1 < len(toks):
					splice(i, 2, envSplit(toks[i+1]))
				default:
					i++
				}
				continue
			}
			if kind == "v" && value == nil {
				i += 2
			} else {
				i++
			}
			continue
		}
		if strings.HasPrefix(t, "-") && t != "-" {
			skip := 1
			for j := 1; j < len(t); j++ {
				c := t[j]
				if strings.IndexByte(envValueShortFlags, c) < 0 {
					continue
				}
				rest := t[j+1:]
				if c == 'S' {
					switch {
					case rest != "":
						splice(i, 1, envSplit(rest))
					case i+1 < len(toks):
						splice(i, 2, envSplit(toks[i+1]))
					default:
						i++
					}
					skip = 0
				} else if rest != "" {
					skip = 1
				} else {
					skip = 2
				}
				break
			}
			i += skip
			continue
		}
		if strings.Contains(t, "=") && !strings.HasPrefix(t, "=") {
			i++
			continue
		}
		break
	}
	if i < len(toks) {
		return &InterpreterPayload{Head: head, InnerCommands: []string{shlexJoin(toks[i:])}}
	}
	return &InterpreterPayload{Head: head}
}

// envSplit is interpreter_parse._env_split: the words of an env -S string,
// or the string as one word when they cannot be read exactly.
func envSplit(value string) []string {
	if strings.ContainsAny(value, "\\$#") {
		return []string{value}
	}
	words, err := posixSplit(value)
	if err != nil {
		return []string{value}
	}
	return words
}

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
}

type wrapperSpec struct {
	valueFlags     map[string]bool
	positionalSkip int
}

var transparentWrappers = map[string]wrapperSpec{
	"timeout": {valueFlags: setOf("-k", "--kill-after", "-s", "--signal"), positionalSkip: 1},
	"nice":    {valueFlags: setOf("-n", "--adjustment"), positionalSkip: 0},
	"nohup":   {valueFlags: setOf(), positionalSkip: 0},
	"time":    {valueFlags: setOf(), positionalSkip: 0},
	"stdbuf":  {valueFlags: setOf("-i", "-o", "-e"), positionalSkip: 0},
	"command": {valueFlags: setOf(), positionalSkip: 0},
	"setsid":  {valueFlags: setOf(), positionalSkip: 0},
	"ionice":  {valueFlags: setOf("-c", "-n", "-t"), positionalSkip: 0},
}

var xargsValueFlags = setOf(
	"-n", "-I", "-P", "-L", "-d", "-E", "-s", "-a",
	"--max-args", "--replace", "--max-procs", "--max-lines",
	"--delimiter", "--eof", "--max-chars", "--arg-file",
)

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
	i := 1
	for i < len(tokens) {
		t := tokens[i]
		if t == "--" {
			i++
			break
		}
		if strings.HasPrefix(t, "-") && t != "-" {
			if spec.valueFlags[t] && i+1 < len(tokens) {
				i += 2
				continue
			}
			i++
			continue
		}
		break
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
	i := 1
	for i < len(tokens) {
		t := tokens[i]
		if t == "--" {
			i++
			break
		}
		if strings.HasPrefix(t, "-") {
			if xargsValueFlags[t] && i+1 < len(tokens) {
				i += 2
				continue
			}
			i++
			continue
		}
		break
	}
	if i < len(tokens) {
		return &InterpreterPayload{Head: head, InnerCommands: []string{shlexJoin(tokens[i:])}}
	}
	return &InterpreterPayload{Head: head}
}

// parseFind ports interpreter_parse._parse_find.
func parseFind(head string, tokens []string) *InterpreterPayload {
	var inners []string
	i := 0
	for i < len(tokens) {
		if findExecFlags[tokens[i]] {
			start := i + 1
			j := start
			for j < len(tokens) && tokens[j] != ";" && tokens[j] != "+" {
				j++
			}
			if j > start {
				inners = append(inners, shlexJoin(tokens[start:j]))
			}
			i = j + 1
		} else {
			i++
		}
	}
	return &InterpreterPayload{Head: head, InnerCommands: inners}
}

// parseEnv ports interpreter_parse._parse_env.
func parseEnv(head string, tokens []string) *InterpreterPayload {
	i := 1
	for i < len(tokens) {
		t := tokens[i]
		if strings.HasPrefix(t, "-") {
			i++
			continue
		}
		if strings.Contains(t, "=") && !strings.HasPrefix(t, "=") {
			i++
			continue
		}
		break
	}
	if i < len(tokens) {
		return &InterpreterPayload{Head: head, InnerCommands: []string{shlexJoin(tokens[i:])}}
	}
	return &InterpreterPayload{Head: head}
}

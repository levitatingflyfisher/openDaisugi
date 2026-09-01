package config

import (
	"daisugi-verify/internal/lazyre"
	"strings"
)

// A gate hook command is found by its words, never by a substring, the
// way opendaisugi.config reads it. The command is split as the shell
// splits it (quotes, backslashes, runs of ; | & as their own words), each
// simple command is read on its own, and these forms are a gate hook:
//
//	[NAME=val ...] [env [opts] [NAME=val ...]] [uv run [opts]] python*|py [flags] -m opendaisugi.gate[_client] ARGS
//	[NAME=val ...] .../daisugi gate check ARGS            (this binary)
//	sh|bash|dash|zsh|ksh [opts] -c '<one of the above>'
//
// A command whose words hold opendaisugi.gate[_client] (alone, or joined
// to -m) in any other form is an unknown gate hook: it may gate, but its
// mode cannot be read, so status says so and uninstall refuses it.

var (
	reAssignment   = lazyre.New(`^[A-Za-z_][A-Za-z0-9_]*=`)
	rePython       = lazyre.New(`^(python[0-9.]*|py)$`)
	reJoinedModule = lazyre.New(`^-[A-Za-z]*m(opendaisugi\.gate(_client)?)$`)
)

const pyNoArgFlags = "bBdEhiIOPqRsSuvx"
const maxShellDepth = 3

var envValueOpts = map[string]bool{"-u": true, "--unset": true, "-C": true, "--chdir": true}

var uvValueOpts = map[string]bool{
	"--project": true, "--directory": true, "--python": true, "-p": true, "--with": true,
	"--with-editable": true, "--with-requirements": true, "--env-file": true, "--extra": true,
	"--group": true, "--only-group": true, "--no-group": true, "--package": true, "--index": true,
	"--default-index": true, "--index-url": true, "--extra-index-url": true, "--find-links": true,
	"-f": true, "--cache-dir": true, "--config-file": true, "-w": true,
}

func isGateModule(w string) bool { return w == "opendaisugi.gate" || w == "opendaisugi.gate_client" }

// token is one word of a command, or an operator (a run of ; | &).
type token struct {
	text string
	op   bool
}

// CommandTokens is config.command_tokens: the command split as a POSIX
// shell splits it. ok is false on an unclosed quote.
func CommandTokens(command string) (out []token, ok bool) {
	var cur strings.Builder
	have := false
	flush := func() {
		if have {
			out = append(out, token{text: cur.String()})
		}
		cur.Reset()
		have = false
	}
	// Python indexes code points; the special characters are all ASCII,
	// so byte indexing reads the same words.
	s := command
	n := len(s)
	for i := 0; i < n; {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			flush()
			i++
		case c == ';' || c == '|' || c == '&':
			flush()
			j := i
			for j < n && (s[j] == ';' || s[j] == '|' || s[j] == '&') {
				j++
			}
			out = append(out, token{text: s[i:j], op: true})
			i = j
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, false
			}
			cur.WriteString(s[i+1 : i+1+j])
			have = true
			i = i + 2 + j
		case c == '"':
			have = true
			i++
			for {
				if i >= n {
					return nil, false
				}
				c = s[i]
				if c == '"' {
					i++
					break
				}
				if c == '\\' && i+1 < n && strings.IndexByte("\"\\$`\n", s[i+1]) >= 0 {
					if s[i+1] != '\n' {
						cur.WriteByte(s[i+1])
					}
					i += 2
					continue
				}
				cur.WriteByte(c)
				i++
			}
		case c == '\\':
			if i+1 < n && s[i+1] != '\n' {
				// The escaped character may be a multi-byte one: copy it whole.
				k := i + 2
				for k < n && s[k]&0xC0 == 0x80 {
					k++
				}
				cur.WriteString(s[i+1 : k])
				have = true
				i = k
				continue
			}
			i += 2
		default:
			cur.WriteByte(c)
			have = true
			i++
		}
	}
	flush()
	return out, true
}

func segments(tokens []token) [][]string {
	var out [][]string
	var cur []string
	for _, t := range tokens {
		if t.op {
			if len(cur) > 0 {
				out = append(out, cur)
			}
			cur = nil
			continue
		}
		cur = append(cur, t.text)
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// skipEnv drops leading NAME=val words and an `env [opts] [NAME=val ...]`
// prefix.
func skipEnv(words []string) []string {
	for len(words) > 0 && reAssignment().MatchString(words[0]) {
		words = words[1:]
	}
	if len(words) > 0 && pathBase(words[0]) == "env" {
		words = words[1:]
		for len(words) > 0 && strings.HasPrefix(words[0], "-") {
			if words[0] == "--" {
				words = words[1:]
				break
			}
			if envValueOpts[words[0]] {
				words = words[min(2, len(words)):]
			} else {
				words = words[1:]
			}
		}
		for len(words) > 0 && reAssignment().MatchString(words[0]) {
			words = words[1:]
		}
	}
	return words
}

// pythonModuleArgs reads `python* [flags] -m MODULE ARGS`: the words after
// a gate MODULE, ok false otherwise.
func pythonModuleArgs(words []string) ([]string, bool) {
	for i := 1; i < len(words); i++ {
		w := words[i]
		if !strings.HasPrefix(w, "-") || w == "-" || strings.HasPrefix(w, "--") {
			return nil, false
		}
		rest := w[1:]
		module, found := "", false
	flags:
		for k := 0; k < len(rest); k++ {
			ch := rest[k]
			switch {
			case strings.IndexByte(pyNoArgFlags, ch) >= 0:
				continue
			case ch == 'm':
				module, found = rest[k+1:], true
				if module == "" {
					if i+1 >= len(words) {
						return nil, false
					}
					i++
					module = words[i]
				}
				break flags
			case ch == 'W' || ch == 'X':
				if rest[k+1:] == "" {
					i++
				}
				break flags
			default:
				return nil, false
			}
		}
		if found {
			if isGateModule(module) {
				return words[i+1:], true
			}
			return nil, false
		}
	}
	return nil, false
}

// Hook kinds.
const (
	KindNone    = ""
	KindGate    = "gate"
	KindUnknown = "unknown"
)

func segmentGateArgs(words []string, depth int) (string, []string) {
	words = skipEnv(words)
	if len(words) >= 2 && pathBase(words[0]) == "uv" && words[1] == "run" {
		words = words[2:]
		for len(words) > 0 && strings.HasPrefix(words[0], "-") {
			if words[0] == "--" {
				words = words[1:]
				break
			}
			if uvValueOpts[words[0]] {
				words = words[min(2, len(words)):]
			} else {
				words = words[1:]
			}
		}
		words = skipEnv(words)
	}
	switch {
	case len(words) > 0 && rePython().MatchString(pathBase(words[0])):
		if args, ok := pythonModuleArgs(words); ok {
			return KindGate, args
		}
	case len(words) >= 3 && pathBase(words[0]) == "daisugi" && words[1] == "gate" && words[2] == "check":
		return KindGate, words[3:]
	case len(words) > 0 && isShell(pathBase(words[0])) && depth < maxShellDepth:
		for i := 1; i < len(words); i++ {
			w := words[i]
			if !strings.HasPrefix(w, "-") || strings.HasPrefix(w, "--") {
				break
			}
			if strings.Contains(w[1:], "c") && i+1 < len(words) {
				return gateHook(words[i+1], depth+1)
			}
		}
	}
	for _, w := range words {
		if isGateModule(w) || reJoinedModule().MatchString(w) {
			return KindUnknown, nil
		}
	}
	return KindNone, nil
}

func isShell(b string) bool {
	switch b {
	case "sh", "bash", "dash", "zsh", "ksh":
		return true
	}
	return false
}

func gateHook(command string, depth int) (string, []string) {
	tokens, ok := CommandTokens(command)
	if !ok {
		if strings.Contains(command, "opendaisugi.gate") {
			return KindUnknown, nil
		}
		return KindNone, nil
	}
	unknown := false
	for _, seg := range segments(tokens) {
		kind, args := segmentGateArgs(seg, depth)
		if kind == KindGate {
			if args == nil {
				args = []string{}
			}
			return kind, args
		}
		unknown = unknown || kind == KindUnknown
	}
	if unknown {
		return KindUnknown, nil
	}
	return KindNone, nil
}

// GateHookKind is config.gate_hook_kind: KindGate, KindUnknown (holds the
// gate module in a form not read), or KindNone.
func GateHookKind(command any) string {
	s, isStr := command.(string)
	if !isStr {
		return KindNone
	}
	kind, _ := gateHook(s, 0)
	return kind
}

// GateHookArgs is config.gate_hook_args: the words after the gate entry
// point, up to the end of its simple command. ok is false when command is
// not a gate hook in a form read.
func GateHookArgs(command any) (args []string, ok bool) {
	s, isStr := command.(string)
	if !isStr {
		return nil, false
	}
	kind, args := gateHook(s, 0)
	return args, kind == KindGate
}

// IsGateHook reports whether command is a gate hook in a form read.
func IsGateHook(command any) bool {
	_, ok := GateHookArgs(command)
	return ok
}

// GateHookMode is config.gate_hook_mode: the last --mode a gate hook
// passes, as argparse reads it, or "" when there is none or it is not
// shadow or enforce.
func GateHookMode(command any) string {
	args, _ := GateHookArgs(command)
	mode := ""
	for i, w := range args {
		if w == "--mode" && i+1 < len(args) {
			mode = args[i+1]
		} else if v, ok := strings.CutPrefix(w, "--mode="); ok {
			mode = v
		}
	}
	if mode == "shadow" || mode == "enforce" {
		return mode
	}
	return ""
}

// IsRecordHook is config.is_record_hook: a `daisugi hook record` simple
// command, prefixes skipped; with event non-empty, only one passing
// --event <event>.
func IsRecordHook(command any, event string) bool {
	s, isStr := command.(string)
	if !isStr {
		return false
	}
	tokens, ok := CommandTokens(s)
	if !ok {
		return false
	}
	for _, seg := range segments(tokens) {
		words := skipEnv(seg)
		if len(words) < 3 || pathBase(words[0]) != "daisugi" || words[1] != "hook" || words[2] != "record" {
			continue
		}
		if event == "" {
			return true
		}
		for i, w := range words {
			if (w == "--event" && i+1 < len(words) && words[i+1] == event) || w == "--event="+event {
				return true
			}
		}
	}
	return false
}

// pathBase is the last "/"-separated part, as rsplit("/", 1)[-1].
func pathBase(p string) string {
	return p[strings.LastIndex(p, "/")+1:]
}

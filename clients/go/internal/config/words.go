package config

import (
	"daisugi-verify/internal/lazyre"
	"strings"
)

// A warden hook command is found by its words, never by a substring, the
// way opendaisugi.config reads it. The command is split as the shell
// splits it (quotes, backslashes, runs of ; | & as their own words), each
// simple command is read on its own, and these forms are a warden hook:
//
//	[NAME=val ...] [env [opts] [NAME=val ...]] [uv run [opts]] python*|py [flags] -m opendaisugi.warden[_client] ARGS
//	[NAME=val ...] .../daisugi warden check ARGS            (this binary)
//	[NAME=val ...] <any program> warden check ARGS, where the NAME=val words
//	  hold WardenHookMarker and ARGS hold each flag the installer always
//	  writes (this binary installed under another file name)
//	sh|bash|dash|zsh|ksh [opts] -c '<one of the above>'
//
// A command whose words hold opendaisugi.warden[_client] (alone, or joined
// to -m) in any other form is an unknown warden hook: it may warden, but its
// mode cannot be read, so status says so and uninstall refuses it.

var (
	reAssignment   = lazyre.New(`^[A-Za-z_][A-Za-z0-9_]*=`)
	rePython       = lazyre.New(`^(python[0-9.]*|py)$`)
	reJoinedModule = lazyre.New(`^-[A-Za-z]*m(opendaisugi\.warden(_client)?)$`)
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

func isWardenModule(w string) bool {
	return w == "opendaisugi.warden" || w == "opendaisugi.warden_client"
}

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
// a warden MODULE, ok false otherwise.
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
			if isWardenModule(module) {
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
	KindWarden  = "warden"
	KindUnknown = "unknown"
)

// installedFlags are the flags the installer writes into every hook.
var installedFlags = []string{"--mode", "--root", "--format", "--verify-timeout"}

// binaryWardenArgs reads `[NAME=val ...] PROGRAM warden check ARGS`: the ARGS
// when the program is named daisugi, or when the installer wrote the
// command (the marker is among the leading NAME=val words and ARGS hold
// each flag the installer always writes). A binary may be installed under
// any file name, so the name alone does not decide.
func binaryWardenArgs(seg []string) ([]string, bool) {
	words := skipEnv(seg)
	if len(words) < 3 || words[1] != "warden" || words[2] != "check" {
		return nil, false
	}
	args := words[3:]
	if pathBase(words[0]) == "daisugi" {
		return args, true
	}
	marked := false
	for _, w := range seg[:len(seg)-len(words)] {
		if w == WardenHookMarker {
			marked = true
		}
	}
	if !marked {
		return nil, false
	}
	for _, flag := range installedFlags {
		found := false
		for _, w := range args {
			if w == flag || strings.HasPrefix(w, flag+"=") {
				found = true
			}
		}
		if !found {
			return nil, false
		}
	}
	return args, true
}

func segmentWardenArgs(words []string, depth int) (string, []string) {
	binArgs, isBinary := binaryWardenArgs(words)
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
			return KindWarden, args
		}
	case len(words) >= 3 && pathBase(words[0]) == "daisugi" && words[1] == "warden" && words[2] == "check":
		return KindWarden, words[3:]
	case isBinary:
		return KindWarden, binArgs
	case len(words) > 0 && isShell(pathBase(words[0])) && depth < maxShellDepth:
		for i := 1; i < len(words); i++ {
			w := words[i]
			if !strings.HasPrefix(w, "-") || strings.HasPrefix(w, "--") {
				break
			}
			if strings.Contains(w[1:], "c") && i+1 < len(words) {
				return wardenHook(words[i+1], depth+1)
			}
		}
	}
	for _, w := range words {
		if isWardenModule(w) || reJoinedModule().MatchString(w) {
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

func wardenHook(command string, depth int) (string, []string) {
	tokens, ok := CommandTokens(command)
	if !ok {
		if strings.Contains(command, "opendaisugi.warden") {
			return KindUnknown, nil
		}
		return KindNone, nil
	}
	unknown := false
	for _, seg := range segments(tokens) {
		kind, args := segmentWardenArgs(seg, depth)
		if kind == KindWarden {
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

// WardenHookKind is config.warden_hook_kind: KindWarden, KindUnknown (holds the
// warden module in a form not read), or KindNone.
func WardenHookKind(command any) string {
	s, isStr := command.(string)
	if !isStr {
		return KindNone
	}
	kind, _ := wardenHook(s, 0)
	return kind
}

// WardenHookArgs is config.warden_hook_args: the words after the warden entry
// point, up to the end of its simple command. ok is false when command is
// not a warden hook in a form read.
func WardenHookArgs(command any) (args []string, ok bool) {
	s, isStr := command.(string)
	if !isStr {
		return nil, false
	}
	kind, args := wardenHook(s, 0)
	return args, kind == KindWarden
}

// IsWardenHook reports whether command is a warden hook in a form read.
func IsWardenHook(command any) bool {
	_, ok := WardenHookArgs(command)
	return ok
}

// WardenHookMode is config.warden_hook_mode: the last --mode a warden hook
// passes, as argparse reads it, or "" when there is none or it is not
// audit or enforce.
func WardenHookMode(command any) string {
	if mode := wardenHookModeWord(command); mode == "audit" || mode == "enforce" {
		return mode
	}
	return ""
}

// wardenHookModeWord is config._warden_hook_mode_word: the last --mode word
// a warden hook passes, valid or not.
func wardenHookModeWord(command any) string {
	args, _ := WardenHookArgs(command)
	mode := ""
	for i, w := range args {
		if w == "--mode" && i+1 < len(args) {
			mode = args[i+1]
		} else if v, ok := strings.CutPrefix(w, "--mode="); ok {
			mode = v
		}
	}
	return mode
}

// IsStaleModeHook is config.is_stale_mode_hook: a warden hook that passes
// --mode shadow, the old name of audit mode. The warden refuses that word,
// so the hook denies every call.
func IsStaleModeHook(command any) bool { return wardenHookModeWord(command) == "shadow" }

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
		if len(words) < 3 || !isEntryName(pathBase(words[0])) || words[1] != "hook" || words[2] != "record" {
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

// isEntryName is the names a daisugi entry point goes by: an exact set. A
// user's own wrapper such as daisugi-local is not ours.
func isEntryName(b string) bool {
	switch b {
	case "daisugi", "daisugi-py", "daisugi-go", "daisugi-rs":
		return true
	}
	return false
}

// IsEntryName is config.is_entry_name on a bare name or a path.
func IsEntryName(s string) bool { return isEntryName(pathBase(s)) }

// RecordHookPrograms is config.record_hook_programs: the program word of
// each `daisugi hook record` segment of a command, after any NAME=val and
// env prefix.
func RecordHookPrograms(command any) []string {
	s, isStr := command.(string)
	if !isStr {
		return nil
	}
	tokens, ok := CommandTokens(s)
	if !ok {
		return nil
	}
	var out []string
	for _, seg := range segments(tokens) {
		words := skipEnv(seg)
		if len(words) >= 3 && isEntryName(pathBase(words[0])) && words[1] == "hook" && words[2] == "record" {
			out = append(out, words[0])
		}
	}
	return out
}

// HookCommandPrograms is install._hook_command_programs: the program of
// each record hook segment, else the first program of a warden hook, else
// none.
func HookCommandPrograms(command any) []string {
	if found := RecordHookPrograms(command); len(found) > 0 {
		return found
	}
	if !IsWardenHook(command) {
		return nil
	}
	s, _ := command.(string)
	tokens, ok := CommandTokens(s)
	if !ok {
		return nil
	}
	segs := segments(tokens)
	if len(segs) == 0 {
		return nil
	}
	if words := skipEnv(segs[0]); len(words) > 0 {
		return words[:1]
	}
	return nil
}

// IsEntryWord is install._is_entry_word: a word whose last path part is one
// of those names.
func IsEntryWord(v any) bool {
	s, ok := v.(string)
	return ok && isEntryName(pathBase(s))
}

// pathBase is the last "/"-separated part, as rsplit("/", 1)[-1].
func pathBase(p string) string {
	return p[strings.LastIndex(p, "/")+1:]
}

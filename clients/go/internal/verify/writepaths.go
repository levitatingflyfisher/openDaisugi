package verify

import (
	"strings"
)

// This file is write_paths.py: the write paths of one step, which
// forall_writes reads. The writer tables (writerList, ddKeyList) are
// generated from write_paths.WRITERS and DD_KEYS (dialect_gen.go).

var writers = func() map[string]writerSpec {
	m := make(map[string]writerSpec, len(writerList))
	for _, w := range writerList {
		m[w.Name] = w
	}
	return m
}()

var ddKeys = func() map[string]bool {
	m := make(map[string]bool, len(ddKeyList))
	for _, k := range ddKeyList {
		m[k] = true
	}
	return m
}()

// cwdHeads is write_paths.CWD_HEADS.
var cwdHeads = map[string]bool{"cd": true, "pushd": true, "popd": true}

// StepWritePaths is write_paths.step_write_paths over a step record: the
// normalized write paths, or ok=false when they cannot be known. A base
// ("" for none) is an absolute normalized directory each relative path
// resolves against.
func StepWritePaths(step map[string]any, base string) (paths []string, ok bool) {
	switch step["type"] {
	case "file_write":
		p, isStr := step["path"].(string)
		if !isStr {
			return nil, false
		}
		paths = []string{posixNormpath(p)}
	case "shell":
		c, isStr := step["command"].(string)
		if !isStr {
			return nil, false
		}
		if paths, ok = shellWrites(c, 0, false); !ok {
			return nil, false
		}
	default:
		return []string{}, true
	}
	if base == "" {
		return paths, true
	}
	return ResolveWrites(paths, base)
}

// ResolveWrites is write_paths.resolve_writes: each relative path resolved
// against base; ok=false when a path starts with ~.
func ResolveWrites(paths []string, base string) ([]string, bool) {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		switch {
		case strings.HasPrefix(p, "/"):
			out = append(out, p)
		case strings.HasPrefix(p, "~"):
			return nil, false
		default:
			out = append(out, posixNormpath(base+"/"+p))
		}
	}
	return out, true
}

func shellWrites(command string, depth int, adds bool) ([]string, bool) {
	if depth > maxInterpreterDepth {
		return nil, false
	}
	stripped := strings.TrimSpace(command)
	if stripped == "" {
		return []string{}, true
	}
	if !hasShellMetachar(command) {
		return payloadWrites(stripped, depth, adds)
	}
	d := DecomposeCommand(command)
	if !d.OK {
		return nil, false
	}
	out := []string{}
	for _, p := range d.Writes {
		if !sanctionedWriteSinks[p] {
			out = append(out, posixNormpath(p))
		}
	}
	for _, simple := range d.Commands {
		inner, ok := payloadWrites(simple, depth, adds)
		if !ok {
			return nil, false
		}
		out = append(out, inner...)
	}
	if MovesCwd(d.Heads) && HasRelative(out) {
		return nil, false
	}
	return out, true
}

// MovesCwd is write_paths._moves_cwd: a head that moves the line's cwd.
func MovesCwd(heads []string) bool {
	for _, h := range heads {
		if cwdHeads[h] {
			return true
		}
	}
	return false
}

// HasRelative is write_paths._has_relative.
func HasRelative(paths []string) bool {
	for _, p := range paths {
		if !strings.HasPrefix(p, "/") {
			return true
		}
	}
	return false
}

func payloadWrites(command string, depth int, adds bool) ([]string, bool) {
	p, ok := ParseInterpreter(command)
	if !ok || p.Opaque {
		return OperandWrites(command, adds)
	}
	innerAdds := adds || p.Head == "xargs" || p.Head == "find"
	out := []string{}
	for _, inner := range p.InnerCommands {
		w, ok := shellWrites(inner, depth+1, innerAdds)
		if !ok {
			return nil, false
		}
		out = append(out, w...)
	}
	return out, true
}

// AddsOperands is write_paths._ADDS_OPERANDS: the wrappers that add
// operands the line does not show.
func AddsOperands(head string) bool { return head == "xargs" || head == "find" }

// OperandWrites is write_paths.operand_writes: the normalized files one
// simple command writes through its operands, or ok=false when unknown.
// adds is true under xargs or find -exec. A command not in the writer
// table writes none.
func OperandWrites(command string, adds bool) ([]string, bool) {
	tokens, err := posixSplit(command)
	if err != nil {
		return nil, false
	}
	i := 0
	for i < len(tokens) && isEnvAssignToken(tokens[i]) {
		i++
	}
	if i == len(tokens) {
		return []string{}, true
	}
	head := tokens[i]
	if k := strings.LastIndexByte(head, '/'); k >= 0 {
		head = head[k+1:]
	}
	spec, known := writers[head]
	if !known {
		return []string{}, true
	}
	if adds || UnsafeWords(command) {
		return nil, false
	}
	raw, ok := writerPaths(head, spec, tokens[i+1:])
	if !ok {
		return nil, false
	}
	out := []string{}
	for _, p := range raw {
		if !sanctionedWriteSinks[p] {
			out = append(out, posixNormpath(p))
		}
	}
	return out, true
}

type flagValue struct {
	name  string
	value *string
}

// parseFlags is write_paths._parse_flags: the flags (name and value) and
// the operands of args, or ok=false when a flag is outside spec or lacks
// its value.
func parseFlags(args []string, spec writerSpec) (opts []flagValue, ops []string, ok bool) {
	i := 0
	ended := false
	for i < len(args) {
		a := args[i]
		i++
		if ended || a == "-" || !strings.HasPrefix(a, "-") {
			ops = append(ops, a)
			continue
		}
		if a == "--" {
			ended = true
			continue
		}
		if strings.HasPrefix(a, "--") {
			name, value, eq := strings.Cut(a, "=")
			if eq {
				if contains(spec.LongValue, name) || contains(spec.LongOptional, name) {
					v := value
					opts = append(opts, flagValue{name, &v})
					continue
				}
				return nil, nil, false
			}
			if contains(spec.Long, name) || contains(spec.LongOptional, name) {
				opts = append(opts, flagValue{name, nil})
				continue
			}
			if contains(spec.LongValue, name) {
				if i >= len(args) {
					return nil, nil, false
				}
				v := args[i]
				opts = append(opts, flagValue{name, &v})
				i++
				continue
			}
			return nil, nil, false
		}
		body := a[1:]
	letters:
		for j := 0; j < len(body); j++ {
			c := body[j]
			if c >= 0x80 {
				return nil, nil, false
			}
			switch {
			case strings.IndexByte(spec.Short, c) >= 0:
				opts = append(opts, flagValue{"-" + string(c), nil})
			case strings.IndexByte(spec.ShortOptional, c) >= 0:
				v := body[j+1:]
				opts = append(opts, flagValue{"-" + string(c), &v})
				break letters
			case strings.IndexByte(spec.ShortValue, c) >= 0:
				rest := body[j+1:]
				if rest == "" {
					if i >= len(args) {
						return nil, nil, false
					}
					rest = args[i]
					i++
				}
				opts = append(opts, flagValue{"-" + string(c), &rest})
				break letters
			default:
				return nil, nil, false
			}
		}
	}
	return opts, ops, true
}

func writeBasename(p string) string {
	p = strings.TrimRight(p, "/")
	if k := strings.LastIndexByte(p, '/'); k >= 0 {
		return p[k+1:]
	}
	return p
}

// under is write_paths._under: dest, then dest/basename(source).
func writeUnder(dest string, sources []string) []string {
	out := []string{dest}
	for _, s := range sources {
		b := writeBasename(s)
		if b != "" && b != "." && b != ".." {
			out = append(out, dest+"/"+b)
		}
	}
	return out
}

// isRemote is write_paths._is_remote.
func isRemote(word string) bool {
	if strings.HasPrefix(word, "rsync://") {
		return true
	}
	colon := strings.IndexByte(word, ':')
	return colon > 0 && !strings.Contains(word[:colon], "/")
}

// writerPaths is write_paths._writer_paths: the raw paths one writing
// command writes, or ok=false when unknown.
func writerPaths(head string, spec writerSpec, args []string) ([]string, bool) {
	if spec.Rule == "dd" {
		out := []string{}
		for _, a := range args {
			key, value, eq := strings.Cut(a, "=")
			if strings.HasPrefix(a, "-") || !eq || !ddKeys[key] {
				return nil, false
			}
			if key == "of" {
				out = append(out, value)
			}
		}
		return out, true
	}
	opts, ops, ok := parseFlags(args, spec)
	if !ok {
		return nil, false
	}
	if ops == nil {
		ops = []string{}
	}
	has := func(names ...string) bool {
		for _, o := range opts {
			if contains(names, o.name) {
				return true
			}
		}
		return false
	}
	switch {
	case spec.Rule == "all":
		return ops, true
	case spec.Rule == "sed":
		var inplace []*string
		for _, o := range opts {
			if o.name == "-i" || o.name == "--in-place" {
				inplace = append(inplace, o.value)
			}
		}
		if len(inplace) == 0 {
			return []string{}, true
		}
		files := ops
		if !has("-e", "--expression", "-f", "--file") {
			if len(files) > 0 {
				files = files[1:]
			}
		}
		suffix := ""
		if last := inplace[len(inplace)-1]; last != nil {
			suffix = *last
		}
		if suffix == "" {
			return files, true
		}
		if strings.ContainsAny(suffix, "*/") {
			return nil, false
		}
		out := append([]string{}, files...)
		for _, f := range files {
			out = append(out, f+suffix)
		}
		return out, true
	case spec.Rule == "install" && has("-d", "--directory"):
		return ops, true
	}
	var target []string
	for _, o := range opts {
		if o.name == "-t" || o.name == "--target-directory" {
			v := ""
			if o.value != nil {
				v = *o.value
			}
			target = append(target, v)
		}
	}
	noTarget := has("-T", "--no-target-directory")
	if len(target) > 0 && noTarget {
		return nil, false
	}
	var dest string
	var sources []string
	switch {
	case len(target) > 0:
		dest, sources = target[len(target)-1], ops
	case head == "ln" && len(ops) == 1:
		return []string{writeBasename(ops[0])}, true
	case len(ops) < 2:
		return []string{}, true
	default:
		dest, sources = ops[len(ops)-1], ops[:len(ops)-1]
	}
	if spec.Rule == "rsync" {
		out := []string{}
		if has("--remove-source-files") {
			for _, s := range sources {
				if !isRemote(s) {
					out = append(out, s)
				}
			}
		}
		if !isRemote(dest) {
			out = append(out, writeUnder(dest, sources)...)
		}
		return out, true
	}
	written := writeUnder(dest, sources)
	if noTarget {
		written = []string{dest}
	}
	if spec.Rule == "move" {
		return append(append([]string{}, sources...), written...), true
	}
	return written, true
}

func isShellSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f' || (c >= 0x1c && c <= 0x1f)
}

// UnsafeWords is effects._unsafe_words: true when the shell would change a
// word before the command sees it: an unquoted glob or brace, a ~+, ~- or
// ~user at the start of a word, or a $ or backtick anywhere.
func UnsafeWords(text string) bool {
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

// ResolveTarget is dialect.resolve_target: the target glob placed from
// base ("" for none). The error is an UnsupportedGlob.
func ResolveTarget(target, base string) (string, error) {
	if base == "" || strings.HasPrefix(target, "/") || strings.HasPrefix(target, "**") {
		return target, nil
	}
	if strings.ContainsAny(base, "*?[]") {
		return "", UnsupportedGlob{"the working directory holds a glob character"}
	}
	if strings.HasPrefix(target, "~") {
		return "", UnsupportedGlob{"a target that starts with ~ cannot be placed from the cwd"}
	}
	if !strings.HasSuffix(target, "/**") {
		for _, s := range strings.Split(target, "/") {
			if s == "." || s == ".." {
				return "", UnsupportedGlob{"a relative target with a . or .. segment cannot be placed"}
			}
		}
	}
	return strings.TrimRight(base, "/") + "/" + target, nil
}

// DialectBase is verify._dialect_base: an absolute path, normalized, else "".
func DialectBase(base *string) string {
	if base == nil || !strings.HasPrefix(*base, "/") {
		return ""
	}
	return posixNormpath(*base)
}

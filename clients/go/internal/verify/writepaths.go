package verify

import (
	"strings"
)

// This file is write_paths.py: the accesses of one step. A shell step's
// reads and writes through operands and redirects, each a path or a tree,
// placed from a base, or a named reason why they are unknown (SG-1 to
// SG-6). forall_writes reads the write half. The tables (writerList,
// readerList, findArgList, the tar sets, the cwd movers and the reasons)
// are generated from the oracle (dialect_gen.go).

var writers = func() map[string]writerSpec {
	m := make(map[string]writerSpec, len(writerList))
	for _, w := range writerList {
		m[w.Name] = w
	}
	return m
}()

var readers = func() map[string]writerSpec {
	m := make(map[string]writerSpec, len(readerList))
	for _, w := range readerList {
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

var cwdMovers = func() map[string]bool {
	m := make(map[string]bool, len(cwdMoverList))
	for _, k := range cwdMoverList {
		m[k] = true
	}
	return m
}()

var findArgs = func() map[string]int {
	m := make(map[string]int, len(findArgList))
	for _, a := range findArgList {
		m[a.Name] = a.Count
	}
	return m
}()

// The reasons, by their place in write_paths.UNKNOWN_REASONS.
var (
	rGrammar   = unknownReasons[0]
	rExtra     = unknownReasons[1]
	rDepth     = unknownReasons[2]
	rSplit     = unknownReasons[3]
	rUnsafe    = unknownReasons[4]
	rFlag      = unknownReasons[5]
	rXargs     = unknownReasons[6]
	rFindExec  = unknownReasons[7]
	rExecdir   = unknownReasons[8]
	rMoves     = unknownReasons[9]
	rTilde     = unknownReasons[10]
	rFindArg   = unknownReasons[11]
	rTar       = unknownReasons[12]
	rRuns      = unknownReasons[13]
	rEmbed     = unknownReasons[14]
	rRemoteTar = unknownReasons[15]
	rOption    = unknownReasons[16]
	rSed       = unknownReasons[17]
)

// runners is write_paths.RUNNERS: the heads that run the command they are
// given, which the placer cannot read. Allowlisting one allows any command it
// runs.
var runners = map[string]bool{
	"exec": true, "eval": true, "builtin": true, "source": true, ".": true,
	"chroot": true, "flock": true, "busybox": true, "script": true,
	"parallel": true, "coproc": true, "trap": true,
	"sudo": true, "doas": true, "watch": true, "systemd-run": true,
	"screen": true, "unbuffer": true, "runuser": true, "taskset": true,
	"chrt": true, "prlimit": true, "unshare": true, "nsenter": true,
	"entr": true,
}

// stdinRunners is write_paths.STDIN_RUNNERS: heads that read their commands
// from standard input, unknown even with no argument.
var stdinRunners = map[string]bool{"batch": true, "expect": true}

var tmuxRuns = strings.Fields("new-session new new-window neww split-window splitw " +
	"send-keys send run-shell run respawn-pane respawnp respawn-window respawnw " +
	"pipe-pane pipep if-shell if display-popup popup display-menu menu source-file " +
	"source set-hook bind-key bind command-prompt confirm-before confirm choose-tree " +
	"choose-client choose-buffer")

var vimHeads = map[string]bool{
	"vim": true, "nvim": true, "vi": true, "view": true, "vimdiff": true,
	"ex": true, "rvim": true, "rview": true, "gvim": true,
}

const sshValue = "BbcDEeFIiJLlmOopQRSWw"

var gitValueLong = map[string]bool{
	"--git-dir": true, "--work-tree": true, "--namespace": true,
	"--super-prefix": true, "--attr-source": true,
}

// sshRunsText is write_paths._SSH_RUNS_RE (case-insensitive command|match|include|exec).
func sshRunsText(v string) bool {
	l := strings.ToLower(v)
	for _, w := range []string{"command", "match", "include", "exec"} {
		if strings.Contains(l, w) {
			return true
		}
	}
	return false
}

func gitRuns(args []string) bool {
	i := 0
	for i < len(args) {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			return false
		}
		if a == "-C" || gitValueLong[a] {
			i += 2
			continue
		}
		if strings.HasPrefix(a, "-c") || strings.HasPrefix(a, "--config-env") || strings.HasPrefix(a, "--exec-path") {
			return true
		}
		i++
	}
	return false
}

func vimRuns(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "+") || (strings.HasPrefix(a, "-") && a != "-") {
			return true
		}
	}
	return false
}

func sshRuns(args []string) bool {
	i := 0
	host := false
	ended := false
	for i < len(args) {
		a := args[i]
		i++
		if !ended && a == "--" {
			ended = true
			continue
		}
		if !ended && strings.HasPrefix(a, "-") && a != "-" {
			body := a[1:]
			for j := 0; j < len(body); j++ {
				c := body[j]
				if strings.IndexByte(sshValue, c) < 0 {
					continue
				}
				rest := body[j+1:]
				value := rest
				if rest == "" && i < len(args) {
					value = args[i]
				}
				if rest == "" {
					i++
				}
				if c == 'F' || c == 'E' || (c == 'o' && sshRunsText(value)) {
					return true
				}
				break
			}
			continue
		}
		if host {
			return true
		}
		host = true
	}
	return false
}

func suRuns(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "--command") || strings.HasPrefix(a, "--session-command") {
			return true
		}
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "c") {
			return true
		}
	}
	return false
}

func tmuxRunsCommand(args []string) bool {
	i := 0
	n := len(args)
	start := true
	first := true
	for i < n {
		a := args[i]
		i++
		if first && strings.HasPrefix(a, "-") && a != "-" {
			body := a[1:]
			for j := 0; j < len(body); j++ {
				c := body[j]
				if c == 'c' || c == 'f' {
					return true
				}
				if c == 'L' || c == 'S' || c == 'T' {
					if j == len(body)-1 {
						i++
					}
					break
				}
			}
			continue
		}
		first = false
		if a == ";" {
			start = true
			continue
		}
		if start {
			start = false
			for _, name := range tmuxRuns {
				if strings.HasPrefix(name, a) {
					return true
				}
			}
		}
	}
	return false
}

// runsACommand is write_paths.runs_a_command.
func runsACommand(head, rawHead string, args []string) bool {
	if stdinRunners[head] {
		return true
	}
	if runners[head] || runners[rawHead] {
		return len(args) > 0
	}
	switch {
	case head == "git":
		return gitRuns(args)
	case vimHeads[head]:
		return vimRuns(args)
	case head == "ssh":
		return sshRuns(args)
	case head == "su":
		return suRuns(args)
	case head == "tmux":
		return tmuxRunsCommand(args)
	case head == "crontab" || head == "at":
		return !(len(args) == 1 && args[0] == "-l")
	}
	return false
}

const parserMissingReason = "shell decomposition parser not installed (opendaisugi[shell])"

// Access is write_paths.Access.
type Access struct {
	Path string
	Tree bool
	Via  string // "operand" or "redirect"
}

// Accesses is write_paths.Accesses; an empty reason means known.
type Accesses struct {
	Reads, Writes []Access
	ReadsUnknown  string
	WritesUnknown string
}

type rawAccess struct {
	path  string
	tree  bool
	via   string
	bases []string
}

// Decomposer is shell_decompose.decompose_command at a Python frame depth.
// The oracle raises RecursionError near its frame limit, so a caller that
// models the frames passes one that panics with the exception; the plain
// one ignores the frame.
type Decomposer func(command string, frame int) Decomposition

func plainDecomposer(command string, _ int) Decomposition { return DecomposeCommand(command) }

type accCtx struct {
	reads, writes []rawAccess
	ru, wu        string
	movers        int
	decomp        Decomposer
	frame         int // the Python frame of shell_accesses
}

func (c *accCtx) unknownReads(reason string) {
	if c.ru == "" {
		c.ru = reason
	}
}

func (c *accCtx) unknownWrites(reason string) {
	if c.wu == "" {
		c.wu = reason
	}
}

func (c *accCtx) unknown(reason string, reads, writes bool) {
	if reads {
		c.unknownReads(reason)
	}
	if writes {
		c.unknownWrites(reason)
	}
}

// accNorm is write_paths._norm: normpath keeps a leading "//", and on Linux
// "//etc" is "/etc".
func accNorm(p string) string {
	out := posixNormpath(p)
	if strings.HasPrefix(out, "//") {
		return "/" + strings.TrimLeft(out, "/")
	}
	return out
}

func joinBase(base, path string) string {
	if strings.HasPrefix(path, "/") || base == "" {
		return accNorm(path)
	}
	return accNorm(base + "/" + path)
}

func (c *accCtx) finalize(hasBase bool) Accesses {
	rel := false
	for _, a := range append(append([]rawAccess{}, c.reads...), c.writes...) {
		if !strings.HasPrefix(a.path, "/") {
			rel = true
			break
		}
	}
	if c.movers > 0 && rel {
		c.unknown(rMoves, true, true)
	}
	place := func(raw []rawAccess, sinks map[string]bool, reads bool) []Access {
		out := []Access{}
		for _, r := range raw {
			for _, base := range r.bases {
				if strings.HasPrefix(r.path, "~") && hasBase {
					c.unknown(rTilde, reads, !reads)
					continue
				}
				full := joinBase(base, r.path)
				if sinks[full] {
					continue
				}
				a := Access{full, r.tree, r.via}
				dup := false
				for _, o := range out {
					if o == a {
						dup = true
						break
					}
				}
				if !dup {
					out = append(out, a)
				}
			}
		}
		return out
	}
	reads := place(c.reads, sanctionedReadSources, true)
	writes := place(c.writes, sanctionedWriteSinks, false)
	return Accesses{reads, writes, c.ru, c.wu}
}

// ShellAccesses is write_paths.shell_accesses: the reads and writes of one
// shell line, placed from base ("" for none).
func ShellAccesses(command, base string) Accesses {
	return ShellAccessesAt(command, base, plainDecomposer, 0)
}

// ShellAccessesAt is ShellAccesses with the decomposer and the Python frame
// depth of the shell_accesses call: a level decomposes at frame
// frame+2+2*depth.
func ShellAccessesAt(command, base string, decomp Decomposer, frame int) Accesses {
	c := &accCtx{decomp: decomp, frame: frame}
	accLevel(command, 0, []string{base}, "", c)
	return c.finalize(base != "")
}

// OperandAccesses is write_paths.operand_accesses: one simple command.
// under is "", "xargs", "find-exec" or "find-execdir".
func OperandAccesses(command, under string) Accesses {
	c := &accCtx{decomp: plainDecomposer}
	accSimple(command, 0, []string{""}, under, c, false)
	return c.finalize(false)
}

// StepAccesses is write_paths.step_accesses over a step record.
func StepAccesses(step map[string]any, base string) Accesses {
	return StepAccessesAt(step, base, plainDecomposer, 0)
}

// StepAccessesAt is StepAccesses with the decomposer and the Python frame
// depth of the step_accesses call.
func StepAccessesAt(step map[string]any, base string, decomp Decomposer, frame int) Accesses {
	kind, _ := step["type"].(string)
	switch kind {
	case "file_write", "file_read":
		path, ok := step["path"].(string)
		if !ok {
			return Accesses{ReadsUnknown: rSplit, WritesUnknown: rSplit}
		}
		c := &accCtx{}
		entry := []rawAccess{{path, false, "operand", []string{base}}}
		if kind == "file_write" {
			c.writes = entry
		} else {
			c.reads = entry
		}
		return c.finalize(base != "")
	case "shell":
		command, ok := step["command"].(string)
		if !ok {
			return Accesses{ReadsUnknown: rSplit, WritesUnknown: rSplit}
		}
		return ShellAccessesAt(command, base, decomp, frame+1)
	}
	return Accesses{}
}

// StepWritePaths is write_paths.step_write_paths over a step record: the
// normalized write paths, or ok=false when they cannot be known. A base
// ("" for none) is an absolute normalized directory each relative path
// resolves against.
func StepWritePaths(step map[string]any, base string) (paths []string, ok bool) {
	return StepWritePathsAt(step, base, plainDecomposer, 0)
}

// StepWritePathsAt is StepWritePaths with the decomposer and the Python
// frame depth of the step_write_paths call.
func StepWritePathsAt(step map[string]any, base string, decomp Decomposer, frame int) (paths []string, ok bool) {
	kind, _ := step["type"].(string)
	if kind == "file_write" {
		if _, isStr := step["path"].(string); !isStr {
			return nil, false
		}
	}
	if kind == "shell" {
		if _, isStr := step["command"].(string); !isStr {
			return nil, false
		}
	}
	acc := StepAccessesAt(step, base, decomp, frame+1)
	if acc.WritesUnknown != "" {
		return nil, false
	}
	paths = []string{}
	for _, a := range acc.Writes {
		paths = append(paths, a.Path)
	}
	return paths, true
}

// OperandWrites is write_paths.operand_writes: the normalized files one
// simple command writes through its operands, or ok=false when unknown.
func OperandWrites(command string, adds bool) ([]string, bool) {
	under := ""
	if adds {
		under = "xargs"
	}
	acc := OperandAccesses(command, under)
	if acc.WritesUnknown != "" {
		return nil, false
	}
	out := []string{}
	for _, a := range acc.Writes {
		out = append(out, a.Path)
	}
	return out, true
}

func accHeadOf(tokens []string) (int, string, string) {
	i := 0
	for i < len(tokens) && isEnvAssignToken(tokens[i]) {
		i++
	}
	if i == len(tokens) {
		return i, "", ""
	}
	raw := tokens[i]
	head := raw
	if k := strings.LastIndexByte(raw, '/'); k >= 0 {
		head = raw[k+1:]
	}
	return i, raw, head
}

func accLevel(command string, depth int, bases []string, under string, c *accCtx) {
	if depth > maxInterpreterDepth {
		c.unknown(rDepth, true, true)
		return
	}
	stripped := strings.TrimSpace(command)
	if stripped == "" {
		return
	}
	cmds := []string{stripped}
	if hasShellMetachar(command) {
		d := c.decomp(command, c.frame+2+2*depth)
		if !d.OK {
			if d.Reason == parserMissingReason {
				c.unknown(rExtra, true, true)
			} else {
				c.unknown(rGrammar, true, true)
			}
			return
		}
		cmds = d.Commands
		lead, hasLead := accLeadingCd(cmds, stripped)
		if hasLead && len(bases) == 1 {
			bases = []string{bases[0], joinBase(bases[0], lead)}
		} else {
			hasLead = false
		}
		for _, p := range d.Writes {
			c.writes = append(c.writes, rawAccess{p, false, "redirect", bases})
		}
		for _, p := range d.Reads {
			c.reads = append(c.reads, rawAccess{p, false, "redirect", bases})
		}
		for n, simple := range cmds {
			accSimple(simple, depth, bases, under, c, n == 0 && hasLead)
		}
		return
	}
	lead, hasLead := accLeadingCd(cmds, stripped)
	if hasLead && len(bases) == 1 {
		bases = []string{bases[0], joinBase(bases[0], lead)}
	} else {
		hasLead = false
	}
	accSimple(stripped, depth, bases, under, c, hasLead)
}

// accLeadingCd is write_paths._leading_cd: the literal operand of a first
// command that is `cd LIT`.
func accLeadingCd(cmds []string, line string) (string, bool) {
	if len(cmds) == 0 || !strings.HasPrefix(line, cmds[0]) {
		return "", false
	}
	tokens, err := posixSplit(cmds[0])
	if err != nil {
		return "", false
	}
	if len(tokens) != 2 || tokens[0] != "cd" || UnsafeWords(cmds[0]) {
		return "", false
	}
	lit := tokens[1]
	if lit == "" || strings.HasPrefix(lit, "-") || strings.HasPrefix(lit, "~") {
		return "", false
	}
	return lit, true
}

const envValueShort = "uCSa"

func accIsMover(head, rawHead string, args []string) bool {
	if cwdMovers[head] || cwdMovers[rawHead] {
		return true
	}
	if head != "env" {
		return false
	}
	i := 0
	for i < len(args) {
		a := args[i]
		i++
		switch {
		case strings.HasPrefix(a, "--"):
			name, value, kind, ok := resolveLong(a, envLong)
			if !ok {
				return false // the payload is opaque: the line is unknown anyway
			}
			if name == "--chdir" || name == "--split-string" {
				return true
			}
			if kind == "v" && value == nil {
				i++
			}
		case strings.HasPrefix(a, "-") && a != "-":
			for j := 1; j < len(a); j++ {
				c := a[j]
				if c == 'C' || c == 'S' {
					return true
				}
				if strings.IndexByte(envValueShort, c) >= 0 {
					if j == len(a)-1 {
						i++
					}
					break
				}
			}
		case !strings.Contains(a, "="):
			return false
		}
	}
	return false
}

// accStripProcessSubstitution is write_paths._strip_process_substitution:
// text without its <(...) and >(...) words, which are inner commands the
// decomposition checks on their own; ok=false when a span does not close.
func accStripProcessSubstitution(text string) (string, bool) {
	var out strings.Builder
	n := len(text)
	var quote byte
	for i := 0; i < n; i++ {
		c := text[i]
		switch {
		case quote != 0:
			out.WriteByte(c)
			if c == '\\' && quote == '"' && i+1 < n {
				out.WriteByte(text[i+1])
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '\\' && i+1 < n:
			out.WriteByte(c)
			out.WriteByte(text[i+1])
			i++
		case c == '\'' || c == '"':
			quote = c
			out.WriteByte(c)
		case (c == '<' || c == '>') && i+1 < n && text[i+1] == '(':
			depth := 0
			j := i + 1
			var q byte
			for j < n {
				d := text[j]
				switch {
				case q != 0:
					if d == '\\' && q == '"' && j+1 < n {
						j++
					} else if d == q {
						q = 0
					}
				case d == '\\' && j+1 < n:
					j++
				case d == '\'' || d == '"':
					q = d
				case d == '(':
					depth++
				case d == ')':
					depth--
				}
				if q == 0 && depth == 0 && j > i+1 && text[j] == ')' {
					break
				}
				j++
			}
			if j >= n {
				return "", false
			}
			out.WriteByte(' ')
			i = j
		default:
			out.WriteByte(c)
		}
	}
	return out.String(), true
}

func accSimple(text string, depth int, bases []string, under string, c *accCtx, skipMover bool) {
	unsub, ok := accStripProcessSubstitution(text)
	if !ok {
		c.unknown(rSplit, true, true)
		return
	}
	tokens, err := posixSplit(unsub)
	if err != nil {
		c.unknown(rSplit, true, true)
		return
	}
	i, rawHead, head := accHeadOf(tokens)
	if head == "" {
		return
	}
	args := tokens[i+1:]
	if !skipMover && accIsMover(head, rawHead, args) {
		c.movers++
		if head == "env" {
			c.unknown(rMoves, true, true)
			return
		}
	}
	if runsACommand(head, rawHead, args) {
		c.unknown(rRuns, true, true)
		return
	}
	wrapped := text
	if i > 0 {
		wrapped = shlexJoin(tokens[i:])
	}
	payload, ok := ParseInterpreter(wrapped)
	if !ok || payload.Opaque {
		if ok && optionHead(payload.Head) {
			c.unknown(rOption, true, true)
			return
		}
		accOperands(head, args, text, bases, under, c)
		return
	}
	if head == "find" {
		accFind(args, text, bases, under, c)
	} else if head == "xargs" {
		accXargsFiles(args, bases, c)
	} else if head == "time" {
		accTimeFiles(args, bases, c)
	}
	for n, inner := range payload.InnerCommands {
		kind := ""
		if n < len(payload.InnerKinds) {
			kind = payload.InnerKinds[n]
		}
		innerUnder := under
		switch {
		case under == "xargs" || head == "xargs":
			innerUnder = "xargs"
		case head == "find":
			if kind == "execdir" {
				innerUnder = "find-execdir"
			} else {
				innerUnder = "find-exec"
			}
		}
		accLevel(inner, depth+1, bases, innerUnder, c)
	}
}

func accXargsFiles(args []string, bases []string, c *accCtx) {
	_, opts, ok := xargsOptions(args, 0)
	if !ok {
		return
	}
	for _, o := range opts {
		if (o.name == "-a" || o.name == "--arg-file") && o.value != nil && *o.value != "-" {
			c.reads = append(c.reads, rawAccess{*o.value, false, "operand", bases})
		}
	}
}

// accTimeFiles is write_paths._time_files: GNU time -o FILE writes FILE.
func accTimeFiles(args []string, bases []string, c *accCtx) {
	_, opts, ok := wrapperOptions("time", args, 0)
	if !ok {
		return
	}
	for _, o := range opts {
		if (o.name == "-o" || o.name == "--output") && o.value != nil {
			c.writes = append(c.writes, rawAccess{*o.value, false, "operand", bases})
		}
	}
}

func accUnsafe(text string) bool { return UnsafeWords(strings.ReplaceAll(text, "{}", "_")) }

func accFind(args []string, text string, bases []string, under string, c *accCtx) {
	if under == "xargs" {
		c.unknown(strings.ReplaceAll(rXargs, "{head}", "find"), true, true)
		return
	}
	if accUnsafe(text) {
		c.unknown(rUnsafe, true, true)
		return
	}
	i := 0
	for i < len(args) && (args[i] == "-H" || args[i] == "-L" || args[i] == "-P") {
		i++
	}
	var starts []string
	for i < len(args) {
		if _, isArg := findArgs[args[i]]; isArg || strings.HasPrefix(args[i], "-") {
			break
		}
		starts = append(starts, args[i])
		i++
	}
	if len(starts) == 0 {
		starts = []string{"."}
	}
	var reads, writes []pair
	for _, s := range starts {
		reads = append(reads, pair{s, true})
	}
	n := len(args)
	for i < n {
		tok := args[i]
		count, known := findArgs[tok]
		if !known {
			c.unknown(rFindArg, true, true)
			return
		}
		if count < 0 {
			j := i + 1
			for j < n && args[j] != ";" && args[j] != "+" {
				j++
			}
			i = j + 1
			continue
		}
		if i+count > n-1 {
			c.unknown(rFindArg, true, true)
			return
		}
		switch tok {
		case "-delete":
			for _, s := range starts {
				writes = append(writes, pair{s, true})
			}
		case "-fprint", "-fprint0", "-fls", "-fprintf":
			writes = append(writes, pair{args[i+1], false})
		}
		i += 1 + count
	}
	for _, p := range reads {
		c.reads = append(c.reads, rawAccess{p.path, p.tree, "operand", bases})
	}
	for _, p := range writes {
		c.writes = append(c.writes, rawAccess{p.path, p.tree, "operand", bases})
	}
}

type pair struct {
	path string
	tree bool
}

func accOperands(head string, args []string, text string, bases []string, under string, c *accCtx) {
	_, isR := readers[head]
	_, isW := writers[head]
	if !isR && !isW {
		return
	}
	if under == "xargs" {
		c.unknown(strings.ReplaceAll(rXargs, "{head}", head), isR, isW)
		return
	}
	if accUnsafe(text) {
		c.unknown(rUnsafe, isR, isW)
		return
	}
	execing := under == "find-exec" || under == "find-execdir"
	if execing {
		for _, a := range args {
			if a != "{}" && strings.Contains(a, "{}") {
				c.unknown(rEmbed, isR, isW)
				return
			}
		}
		if isW {
			c.unknownWrites(rFindExec)
		}
	}
	got := headAccesses(head, args)
	if got.reason != "" {
		c.unknown(strings.ReplaceAll(got.reason, "{head}", head), isR, isW)
		return
	}
	if got.flagFail {
		// A flag outside the closed set can read or write a file the table
		// does not know (sort -o FILE, less -o FILE): both are unknown.
		c.unknown(strings.ReplaceAll(rFlag, "{head}", head), true, true)
		return
	}
	reads, writes := got.reads, got.writes
	if execing {
		// a bare {} is the file find found: find's own start tree covers it
		kept := []pair{}
		for _, p := range reads {
			if p.path != "{}" {
				kept = append(kept, p)
			}
		}
		reads = kept
	}
	if !isR {
		reads = nil
	}
	if !execing {
		for _, p := range writes {
			c.writes = append(c.writes, rawAccess{p.path, p.tree, "operand", bases})
		}
	}
	for _, p := range reads {
		if under == "find-execdir" && !strings.HasPrefix(p.path, "/") {
			for _, seg := range strings.Split(p.path, "/") {
				if seg == ".." {
					c.unknownReads(rExecdir)
					break
				}
			}
			continue
		}
		c.reads = append(c.reads, rawAccess{p.path, p.tree, "operand", bases})
	}
}

type headResult struct {
	reads, writes []pair
	flagFail      bool
	reason        string
}

func specFor(head string) writerSpec {
	if s, ok := writers[head]; ok {
		return s
	}
	return readers[head]
}

func hasName(opts []flagValue, names ...string) bool {
	for _, o := range opts {
		if contains(names, o.name) {
			return true
		}
	}
	return false
}

func pairs(ops []string, tree bool) []pair {
	out := make([]pair, 0, len(ops))
	for _, o := range ops {
		out = append(out, pair{o, tree})
	}
	return out
}

// headAccesses is write_paths._head_accesses.
func headAccesses(head string, args []string) headResult {
	spec := specFor(head)
	rule := spec.Rule
	switch rule {
	case "tar":
		return accTar(args)
	case "find":
		return headResult{}
	case "dd":
		var r headResult
		for _, a := range args {
			key, value, eq := strings.Cut(a, "=")
			if strings.HasPrefix(a, "-") || !eq || !ddKeys[key] {
				return headResult{flagFail: true}
			}
			if key == "if" {
				r.reads = append(r.reads, pair{value, false})
			} else if key == "of" {
				r.writes = append(r.writes, pair{value, false})
			}
		}
		return r
	}
	opts, ops, ok := parseFlags(args, spec)
	if !ok {
		return headResult{flagFail: true}
	}
	if rule == "inout" {
		if len(ops) > 2 {
			return headResult{flagFail: true}
		}
		var r headResult
		if len(ops) > 0 && ops[0] != "-" {
			r.reads = append(r.reads, pair{ops[0], false})
		}
		if len(ops) > 1 && ops[1] != "-" {
			r.writes = append(r.writes, pair{ops[1], false})
		}
		return r
	}
	if rule != "grep" && rule != "rg" && rule != "sed" {
		kept := []string{}
		for _, o := range ops {
			if o != "-" {
				kept = append(kept, o)
			}
		}
		ops = kept
	}
	switch rule {
	case "files":
		return headResult{reads: pairs(ops, false)}
	case "diff":
		return headResult{reads: pairs(ops, hasName(opts, "-r", "--recursive"))}
	case "grep", "rg":
		return headResult{reads: accGrep(rule, opts, ops)}
	case "ls":
		rec := hasName(opts, "-R", "--recursive")
		if len(ops) == 0 {
			ops = []string{"."}
		}
		return headResult{reads: pairs(ops, rec)}
	case "all":
		rec := head == "rm" && hasName(opts, "-r", "-R", "--recursive")
		return headResult{writes: pairs(ops, rec)}
	case "sed":
		var scripts []string
		hasFile := false
		for _, o := range opts {
			switch o.name {
			case "-e", "--expression":
				v := ""
				if o.value != nil {
					v = *o.value
				}
				scripts = append(scripts, v)
			case "-f", "--file":
				hasFile = true
			}
		}
		script := ""
		if len(scripts) > 0 {
			script = strings.Join(scripts, "\n")
		} else if len(ops) > 0 {
			script = ops[0]
		}
		if hasFile || sedScriptRisky(script) {
			return headResult{reason: rSed}
		}
		var inplace []*string
		for _, o := range opts {
			if o.name == "-i" || o.name == "--in-place" {
				inplace = append(inplace, o.value)
			}
		}
		scripted := hasName(opts, "-e", "--expression", "-f", "--file")
		var files []string
		src := ops
		if !scripted && len(src) > 0 {
			src = src[1:]
		} else if !scripted {
			src = nil
		}
		for _, f := range src {
			if f != "-" {
				files = append(files, f)
			}
		}
		var sreads []pair
		for _, o := range opts {
			if o.name == "-f" || o.name == "--file" {
				v := ""
				if o.value != nil {
					v = *o.value
				}
				sreads = append(sreads, pair{v, false})
			}
		}
		sreads = append(sreads, pairs(files, false)...)
		if len(inplace) == 0 {
			return headResult{reads: sreads}
		}
		suffix := ""
		if last := inplace[len(inplace)-1]; last != nil {
			suffix = *last
		}
		if suffix == "" {
			return headResult{reads: sreads, writes: pairs(files, false)}
		}
		if strings.ContainsAny(suffix, "*/") {
			return headResult{flagFail: true}
		}
		all := append([]string{}, files...)
		for _, f := range files {
			all = append(all, f+suffix)
		}
		return headResult{reads: sreads, writes: pairs(all, false)}
	case "install":
		if hasName(opts, "-d", "--directory") {
			return headResult{writes: pairs(ops, false)}
		}
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
	noTarget := hasName(opts, "-T", "--no-target-directory")
	if len(target) > 0 && noTarget {
		return headResult{flagFail: true}
	}
	var dest string
	var sources []string
	switch {
	case len(target) > 0:
		dest, sources = target[len(target)-1], ops
	case head == "ln" && len(ops) == 1:
		return headResult{writes: []pair{{writeBasename(ops[0]), false}}}
	case len(ops) < 2:
		return headResult{}
	default:
		dest, sources = ops[len(ops)-1], ops[:len(ops)-1]
	}
	if rule == "rsync" {
		var local []string
		for _, s := range sources {
			if !isRemote(s) {
				local = append(local, s)
			}
		}
		var rw []pair
		if hasName(opts, "--remove-source-files") {
			rw = pairs(local, true)
		}
		if !isRemote(dest) {
			rw = append(rw, pairs(writeUnder(dest, sources), true)...)
		}
		return headResult{reads: pairs(local, true), writes: rw}
	}
	if rule == "move" {
		written := writeUnder(dest, sources)
		if noTarget {
			written = []string{dest}
		}
		return headResult{
			reads:  pairs(sources, true),
			writes: pairs(append(append([]string{}, sources...), written...), true),
		}
	}
	rec := head == "cp" && hasName(opts, "-r", "-R", "-a", "--recursive", "--archive")
	var wr []pair
	if noTarget {
		wr = []pair{{dest, rec}}
	} else {
		wr = []pair{{dest, false}}
		wr = append(wr, pairs(writeUnder(dest, sources)[1:], rec)...)
	}
	var rd []pair
	if head == "cp" {
		rd = pairs(sources, rec)
	} else if rule == "install" {
		rd = pairs(sources, false)
	}
	return headResult{reads: rd, writes: wr}
}

func accGrep(rule string, opts []flagValue, ops []string) []pair {
	hasPattern := hasName(opts, "-e", "--regexp", "-f", "--file")
	if rule == "rg" && hasName(opts, "--files") {
		hasPattern = true
	}
	files := ops
	if !hasPattern {
		if len(ops) > 0 {
			files = ops[1:]
		} else {
			files = nil
		}
	}
	var out []pair
	for _, o := range opts {
		if o.name == "-f" || o.name == "--file" {
			v := ""
			if o.value != nil {
				v = *o.value
			}
			if v != "-" {
				out = append(out, pair{v, false})
			}
		}
	}
	recursive := true
	if rule != "rg" {
		recursive = hasName(opts, "-r", "-R", "--recursive", "--dereference-recursive")
		for _, o := range opts {
			if (o.name == "-d" || o.name == "--directories") && o.value != nil && *o.value == "recurse" {
				recursive = true
			}
		}
	}
	var kept []string
	for _, f := range files {
		if f != "-" {
			kept = append(kept, f)
		}
	}
	if len(kept) == 0 && recursive {
		kept = []string{"."}
	}
	return append(out, pairs(kept, recursive)...)
}

func accTar(args []string) headResult {
	var modes, archive, dirs, ops []string
	longModes := map[string]string{"--create": "c", "--extract": "x", "--list": "t"}
	i, n := 0, len(args)
	first := true
	fail := headResult{flagFail: true}
	for i < n {
		a := args[i]
		i++
		if a == "--" {
			ops = append(ops, args[i:]...)
			break
		}
		switch {
		case strings.HasPrefix(a, "--"):
			name, value, eq := strings.Cut(a, "=")
			switch {
			case longModes[name] != "" && !eq:
				modes = append(modes, longModes[name])
			case contains(tarLong, name) && !eq:
			case contains(tarLongValue, name):
				if !eq {
					if i >= n {
						return fail
					}
					value = args[i]
					i++
				}
				if name == "--file" {
					archive = append(archive, value)
				} else if name == "--directory" {
					if len(ops) > 0 {
						return fail
					}
					dirs = append(dirs, value)
				}
			default:
				return fail
			}
		case strings.HasPrefix(a, "-") && a != "-":
			body := a[1:]
			for j := 0; j < len(body); {
				ch := body[j]
				j++
				switch {
				case strings.IndexByte(tarModes, ch) >= 0:
					modes = append(modes, string(ch))
				case strings.IndexByte(tarFlags, ch) >= 0:
				case strings.IndexByte(tarValue, ch) >= 0:
					value := body[j:]
					if value == "" {
						if i >= n {
							return fail
						}
						value = args[i]
						i++
					}
					if ch == 'f' {
						archive = append(archive, value)
					} else {
						if len(ops) > 0 {
							return fail
						}
						dirs = append(dirs, value)
					}
					j = len(body)
				default:
					return fail
				}
			}
		case first && a != "-":
			for k := 0; k < len(a); k++ {
				ch := a[k]
				switch {
				case strings.IndexByte(tarModes, ch) >= 0:
					modes = append(modes, string(ch))
				case strings.IndexByte(tarFlags, ch) >= 0:
				case strings.IndexByte(tarValue, ch) >= 0:
					if i >= n {
						return fail
					}
					value := args[i]
					i++
					if ch == 'f' {
						archive = append(archive, value)
					} else {
						dirs = append(dirs, value)
					}
				default:
					return fail
				}
			}
		default:
			ops = append(ops, a)
		}
		first = false
	}
	if len(archive) > 1 || len(dirs) > 1 {
		return fail
	}
	if len(modes) != 1 || len(archive) == 0 {
		return headResult{reason: rTar}
	}
	where := archive[0]
	if isRemote(where) {
		return headResult{reason: rRemoteTar}
	}
	stream := where == "-"
	var r headResult
	switch modes[0] {
	case "c":
		if !stream {
			r.writes = append(r.writes, pair{where, false})
		}
		for _, o := range ops {
			if len(dirs) > 0 && !strings.HasPrefix(o, "/") {
				o = posixNormpath(dirs[0] + "/" + o)
			}
			r.reads = append(r.reads, pair{o, true})
		}
	default:
		if !stream {
			r.reads = append(r.reads, pair{where, false})
		}
		if modes[0] == "x" {
			dir := "."
			if len(dirs) > 0 {
				dir = dirs[0]
			}
			r.writes = append(r.writes, pair{dir, true})
		}
	}
	return r
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

// write_paths.sed_script_risky and its helpers: true when a sed script may
// read, write or run a file the placer cannot place (r, R, w, W, e, s flags w
// and e), or holds anything the reader does not understand. Runes, not bytes,
// so a non-ASCII delimiter reads as the oracle does.

func sedIn(c rune, set string) bool { return strings.ContainsRune(set, c) }

const sedDigits = "0123456789"
const sedPlain = "=dDgGhHnNpPxzF"
const sedNumbered = "lqQL"
const sedEnd = " \t\n;}#"

func sedSkipRegex(s []rune, j int, delim rune) int {
	n := len(s)
	for j < n {
		c := s[j]
		switch {
		case c == '\\':
			j += 2
		case c == '[':
			j++
			if j < n && s[j] == '^' {
				j++
			}
			if j < n && s[j] == ']' {
				j++
			}
			for j < n && s[j] != ']' {
				if s[j] == '[' && j+1 < n && sedIn(s[j+1], ":.=") {
					end := -1
					for k := j + 2; k+1 < n; k++ {
						if s[k] == s[j+1] && s[k+1] == ']' {
							end = k
							break
						}
					}
					if end < 0 {
						return -1
					}
					j = end + 2
				} else {
					j++
				}
			}
			if j >= n {
				return -1
			}
			j++
		case c == delim:
			return j + 1
		case c == '\n':
			return -1
		default:
			j++
		}
	}
	return -1
}

func sedSkipReplacement(s []rune, j int, delim rune) int {
	n := len(s)
	for j < n {
		c := s[j]
		if c == '\\' {
			j += 2
		} else if c == delim {
			return j + 1
		} else {
			j++
		}
	}
	return -1
}

func sedSkipLine(s []rune, j int, continued bool) int {
	n := len(s)
	for j < n && s[j] != '\n' {
		if continued && s[j] == '\\' {
			j += 2
		} else {
			j++
		}
	}
	if j > n {
		return n
	}
	return j
}

func sedAddress(s []rune, i int, second bool) (int, bool) {
	n := len(s)
	if i >= n {
		return i, true
	}
	c := s[i]
	if sedIn(c, sedDigits) {
		for i < n && sedIn(s[i], sedDigits) {
			i++
		}
		if i < n && s[i] == '~' {
			i++
			for i < n && sedIn(s[i], sedDigits) {
				i++
			}
		}
		return i, true
	}
	if c == '$' {
		return i + 1, true
	}
	if second && (c == '+' || c == '~') {
		i++
		for i < n && sedIn(s[i], sedDigits) {
			i++
		}
		return i, true
	}
	if c == '/' || c == '\\' {
		d := '/'
		if c == '\\' {
			i++
			if i >= n || s[i] == '\n' || s[i] == '\\' {
				return i, false
			}
			d = s[i]
		}
		i = sedSkipRegex(s, i+1, d)
		if i < 0 {
			return i, false
		}
		for i < n && (s[i] == 'I' || s[i] == 'M') {
			i++
		}
		return i, true
	}
	return i, true
}

func sedScriptRisky(script string) bool {
	s := []rune(script)
	n := len(s)
	i := 0
	for i < n {
		for i < n && sedIn(s[i], " \t\n;") {
			i++
		}
		if i >= n {
			break
		}
		if s[i] == '#' {
			i = sedSkipLine(s, i, false)
			continue
		}
		for k := 0; k < 2; k++ {
			var ok bool
			i, ok = sedAddress(s, i, k == 1)
			if !ok {
				return true
			}
			for i < n && (s[i] == ' ' || s[i] == '\t') {
				i++
			}
			if k == 0 {
				if i < n && s[i] == ',' {
					i++
					for i < n && (s[i] == ' ' || s[i] == '\t') {
						i++
					}
				} else {
					break
				}
			}
		}
		for i < n && sedIn(s[i], " \t!") {
			i++
		}
		if i >= n {
			return true
		}
		cmd := s[i]
		i++
		switch {
		case cmd == '{':
			continue
		case cmd == '}' || sedIn(cmd, sedPlain):
		case sedIn(cmd, sedNumbered):
			for i < n && sedIn(s[i], sedDigits) {
				i++
			}
		case cmd == 'a' || cmd == 'i' || cmd == 'c':
			i = sedSkipLine(s, i, true)
			continue
		case sedIn(cmd, "btT:"):
			for i < n && (s[i] == ' ' || s[i] == '\t') {
				i++
			}
			for i < n && !sedIn(s[i], " \t\n;") {
				i++
			}
			continue
		case cmd == 'y':
			if i >= n || s[i] == '\n' || s[i] == '\\' {
				return true
			}
			d := s[i]
			i = sedSkipReplacement(s, i+1, d)
			if i < 0 {
				return true
			}
			i = sedSkipReplacement(s, i, d)
			if i < 0 {
				return true
			}
		case cmd == 's':
			if i >= n || s[i] == '\n' || s[i] == '\\' {
				return true
			}
			d := s[i]
			i = sedSkipRegex(s, i+1, d)
			if i < 0 {
				return true
			}
			i = sedSkipReplacement(s, i, d)
			if i < 0 {
				return true
			}
			for i < n && (sedIn(s[i], sedDigits) || sedIn(s[i], "gpiImM")) {
				i++
			}
		default:
			return true
		}
		if i < n && !sedIn(s[i], sedEnd) {
			return true
		}
	}
	return false
}

package gate

import (
	"math"
	"strconv"
	"strings"
	"unicode"

	"daisugi-verify/internal/lazyre"
	"daisugi-verify/internal/pystr"
)

// This file is gate._build_parser's argparse parser, as Python 3.12's
// argparse reads a command line with it: unique prefixes of long options
// (allow_abbrev), --opt=value, -h and --help, the ambiguity and type and
// choice errors, and "unrecognized arguments". An error prints the usage
// and the message on stderr and exits 2, which run_argv turns into the
// fail-closed escape; --help prints the help on stdout and exits 0.

const prog = "opendaisugi.gate"

type argAction struct {
	opts    []string
	dest    string
	flag    bool // store_true
	help    bool
	typ     string // "", "float" or "path"
	choices []string
	metavar string
}

var argActions = []*argAction{
	{opts: []string{"-h", "--help"}, dest: "help", help: true},
	{opts: []string{"--mode"}, dest: "mode", choices: []string{"shadow", "enforce"}},
	{opts: []string{"--root"}, dest: "root", typ: "path", metavar: "ROOT"},
	{opts: []string{"--format"}, dest: "fmt", metavar: "FMT"},
	{opts: []string{"--verify-timeout"}, dest: "verify_timeout", typ: "float", metavar: "VERIFY_TIMEOUT"},
	{opts: []string{"--captures-root"}, dest: "captures_root", typ: "path", metavar: "CAPTURES_ROOT"},
	{opts: []string{"--session"}, dest: "session", metavar: "SESSION"},
	{opts: []string{"--ask"}, dest: "ask", flag: true},
	{opts: []string{"--ask-timeout"}, dest: "ask_timeout", typ: "float", metavar: "ASK_TIMEOUT"},
	{opts: []string{"--checkpoints"}, dest: "checkpoints", flag: true},
}

// optionOrder is parser._option_string_actions in insertion order.
var optionOrder = func() []string {
	var out []string
	for _, a := range argActions {
		out = append(out, a.opts...)
	}
	return out
}()

func actionFor(opt string) *argAction {
	for _, a := range argActions {
		for _, o := range a.opts {
			if o == opt {
				return a
			}
		}
	}
	return nil
}

// DenyFormat is the host format a deny of this argv must take, read as
// argparse reads it (an abbreviated or repeated --format included). What
// the port cannot read, whether argv argparse refuses or any other call
// it cannot decide, falls back to the first --format word, as
// gate._fmt_from_argv does. A stdout-block host (hermes, openclaw) never
// reads an exit code, so guessing "claude" here would give it a body it
// cannot read as a deny, the exact gap gate._escape_outcome's stdout-block
// branch exists to close.
func DenyFormat(argv []string) (f string) {
	args := make([]string, len(argv))
	for i, a := range argv {
		args[i] = pystr.FSDecode([]byte(a))
	}
	defer func() {
		if p := recover(); p != nil {
			switch p.(type) {
			case argvExit, unportedCall:
				f = FormatFromArgv(argv)
			default:
				panic(p)
			}
		}
	}()
	o := parseArgv(args, 80)
	if o.format != nil {
		return *o.format
	}
	return "claude"
}

// options is the parsed command line: the value strings argparse stored,
// already converted where the action has a type.
type options struct {
	mode, root, format, captures, session *string
	verifyTimeout, askTimeout             *float64
	ask, checkpoints                      bool
}

// argvExit is argparse ending the process: help (code 0) or an error
// (code 2), with what it printed.
type argvExit struct {
	code           int
	stdout, stderr string
}

type optTuple struct {
	action   *argAction
	opt      string
	sep      *string
	explicit *string
}

// argError is argparse.ArgumentError, as str() gives it.
type argError struct{ msg string }

func argumentError(a *argAction, msg string) {
	if a != nil {
		msg = "argument " + strings.Join(a.opts, "/") + ": " + msg
	}
	panic(argError{msg})
}

var negativeNumber = lazyre.New(`^-\d+$|^-\d*\.\d+$`)

// isNegativeNumber is parser._negative_number_matcher, whose \d is any
// Unicode decimal digit.
func isNegativeNumber(s string) bool {
	rs := pystr.Runes(s)
	if len(rs) < 2 || rs[0] != '-' {
		return false
	}
	digits := func(xs []rune) bool {
		for _, r := range xs {
			if !unicode.IsDigit(r) {
				return false
			}
		}
		return true
	}
	body := rs[1:]
	if digits(body) {
		return true
	}
	for i, r := range body {
		if r == '.' {
			return digits(body[:i]) && len(body[i+1:]) > 0 && digits(body[i+1:])
		}
	}
	return false
}

func argPartition(s, sep string) (string, *string, *string) {
	i := strings.Index(s, sep)
	if i < 0 {
		return s, nil, nil
	}
	return s[:i], strp(sep), strp(s[i+len(sep):])
}

// parseOptional is parser._parse_optional: nil for a positional.
func parseOptional(s string) []optTuple {
	if s == "" || s[0] != '-' {
		return nil
	}
	if a := actionFor(s); a != nil {
		return []optTuple{{a, s, nil, nil}}
	}
	if pystr.Len(s) == 1 {
		return nil
	}
	if o, sep, exp := argPartition(s, "="); sep != nil {
		if a := actionFor(o); a != nil {
			return []optTuple{{a, o, sep, exp}}
		}
	}
	if t := optionTuples(s); len(t) > 0 {
		return t
	}
	if isNegativeNumber(s) {
		return nil
	}
	if strings.Contains(s, " ") {
		return nil
	}
	return []optTuple{{nil, s, nil, nil}}
}

// optionTuples is parser._get_option_tuples.
func optionTuples(s string) []optTuple {
	var out []optTuple
	rs := pystr.Runes(s)
	if rs[1] == '-' {
		prefix, sep, exp := argPartition(s, "=")
		for _, o := range optionOrder {
			if strings.HasPrefix(o, prefix) {
				out = append(out, optTuple{actionFor(o), o, sep, exp})
			}
		}
		return out
	}
	prefix, sep, exp := argPartition(s, "=")
	short := pystr.FromRunes(rs[:2])
	shortExp := pystr.FromRunes(rs[2:])
	for _, o := range optionOrder {
		if o == short {
			out = append(out, optTuple{actionFor(o), o, strp(""), strp(shortExp)})
		} else if strings.HasPrefix(o, prefix) {
			out = append(out, optTuple{actionFor(o), o, sep, exp})
		}
	}
	return out
}

// pyFloat is float(s) for a str: nil when Python raises ValueError. Text
// that is not ASCII after strip() is left undecided.
func pyFloat(s string) *float64 {
	// float() first maps each Unicode decimal digit to its ASCII digit and
	// each whitespace character to a space; any other non-ASCII character
	// makes the text invalid.
	var ab []byte
	for _, r := range pystr.Runes(s) {
		switch {
		case pystr.IsSpace(r):
			ab = append(ab, ' ')
		case r < 0x80:
			ab = append(ab, byte(r))
		case unicode.IsDigit(r):
			ab = append(ab, byte('0'+digitValue(r)))
		default:
			return nil
		}
	}
	t := strings.TrimSpace(string(ab))
	low := strings.ToLower(t)
	sign := 1.0
	body := low
	if strings.HasPrefix(body, "+") || strings.HasPrefix(body, "-") {
		if body[0] == '-' {
			sign = -1
		}
		body = body[1:]
	}
	switch body {
	case "inf", "infinity":
		f := sign * math.Inf(1)
		return &f
	case "nan":
		f := math.NaN()
		return &f
	}
	if !floatLiteral().MatchString(t) {
		return nil
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(t, "_", ""), 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); !ok || ne.Err != strconv.ErrRange {
			return nil
		}
	}
	return &f
}

var floatLiteral = lazyre.New(`^[+-]?(?:[0-9](?:_?[0-9])*(?:\.(?:[0-9](?:_?[0-9])*)?)?|\.[0-9](?:_?[0-9])*)(?:[eE][+-]?[0-9](?:_?[0-9])*)?$`)

// parseArgv is parser.parse_args(argv). A help or an error panics with
// argvExit.
func parseArgv(argv []string, columns int) options {
	var o options
	defer func() {
		if p := recover(); p != nil {
			if e, ok := p.(argError); ok {
				panic(argvExit{code: 2, stderr: formatUsage(columns) + prog + ": error: " + e.msg + "\n"})
			}
			panic(p)
		}
	}()
	// The arg string pattern: O for an option, A for an argument, - for
	// the "--" that ends options.
	pattern := make([]byte, len(argv))
	tuples := map[int][]optTuple{}
	maxOpt := -1
	for i := 0; i < len(argv); i++ {
		if argv[i] == "--" {
			pattern[i] = '-'
			for j := i + 1; j < len(argv); j++ {
				pattern[j] = 'A'
			}
			break
		}
		if t := parseOptional(argv[i]); t != nil {
			tuples[i] = t
			pattern[i] = 'O'
			maxOpt = i
		} else {
			pattern[i] = 'A'
		}
	}
	var extras []string
	take := func(a *argAction, args []string) {
		if a.help {
			panic(argvExit{code: 0, stdout: formatHelp(columns)})
		}
		if a.flag {
			switch a.dest {
			case "ask":
				o.ask = true
			case "checkpoints":
				o.checkpoints = true
			}
			return
		}
		v := args[0]
		if a.typ == "float" {
			f := pyFloat(v)
			if f == nil {
				argumentError(a, "invalid float value: "+pystr.Repr(v))
			}
			if a.dest == "verify_timeout" {
				o.verifyTimeout = f
			} else {
				o.askTimeout = f
			}
			return
		}
		if a.choices != nil {
			ok := false
			for _, c := range a.choices {
				ok = ok || c == v
			}
			if !ok {
				argumentError(a, "invalid choice: "+pystr.Repr(v)+" (choose from "+strings.Join(a.choices, ", ")+")")
			}
		}
		s := strp(v)
		switch a.dest {
		case "mode":
			o.mode = s
		case "root":
			o.root = s
		case "fmt":
			o.format = s
		case "captures_root":
			o.captures = s
		case "session":
			o.session = s
		}
	}
	consume := func(i int) int {
		ts := tuples[i]
		if len(ts) > 1 {
			names := make([]string, len(ts))
			for k, t := range ts {
				names[k] = t.opt
			}
			argumentError(nil, "ambiguous option: "+argv[i]+" could match "+strings.Join(names, ", "))
		}
		t := ts[0]
		a, opt, sep, exp := t.action, t.opt, t.sep, t.explicit
		type took struct {
			a    *argAction
			args []string
		}
		var todo []took
		var stop int
		for {
			if a == nil {
				extras = append(extras, argv[i])
				return i + 1
			}
			nargs := 1
			if a.flag || a.help {
				nargs = 0
			}
			if exp != nil {
				ors := pystr.Runes(opt)
				if nargs == 0 && ors[1] != '-' && *exp != "" {
					if (sep != nil && *sep != "") || strings.HasPrefix(*exp, "-") {
						argumentError(a, "ignored explicit argument "+pystr.Repr(*exp))
					}
					todo = append(todo, took{a, nil})
					er := pystr.Runes(*exp)
					opt = "-" + string(er[0])
					if na := actionFor(opt); na != nil {
						a = na
						rest := pystr.FromRunes(er[1:])
						switch {
						case rest == "":
							sep, exp = nil, nil
						case rest[0] == '=':
							sep, exp = strp("="), strp(rest[1:])
						default:
							sep, exp = strp(""), strp(rest)
						}
						continue
					}
					extras = append(extras, "-"+*exp)
					stop = i + 1
					break
				}
				if nargs == 1 {
					todo = append(todo, took{a, []string{*exp}})
					stop = i + 1
					break
				}
				argumentError(a, "ignored explicit argument "+pystr.Repr(*exp))
			}
			if nargs == 1 {
				if i+1 >= len(argv) || pattern[i+1] != 'A' {
					argumentError(a, "expected one argument")
				}
				todo = append(todo, took{a, []string{argv[i+1]}})
				stop = i + 2
			} else {
				todo = append(todo, took{a, nil})
				stop = i + 1
			}
			break
		}
		for _, x := range todo {
			take(x.a, x.args)
		}
		return stop
	}
	start := 0
	for start <= maxOpt {
		next := -1
		for k := start; k <= maxOpt; k++ {
			if _, ok := tuples[k]; ok {
				next = k
				break
			}
		}
		if _, ok := tuples[start]; !ok {
			extras = append(extras, argv[start:next]...)
			start = next
		}
		start = consume(start)
	}
	extras = append(extras, argv[start:]...)
	if len(extras) > 0 {
		argumentError(nil, "unrecognized arguments: "+strings.Join(extras, " "))
	}
	return o
}

// formatUsage is parser.format_usage() at a terminal of columns: the
// usage line, wrapped as HelpFormatter wraps it.
func formatUsage(columns int) string {
	width := columns - 2
	prefix := "usage: "
	var parts []string
	for _, a := range argActions {
		var p string
		switch {
		case a.help:
			p = "[-h]"
		case a.flag:
			p = "[" + a.opts[0] + "]"
		case a.choices != nil:
			p = "[" + a.opts[0] + " {" + strings.Join(a.choices, ",") + "}]"
		default:
			p = "[" + a.opts[0] + " " + a.metavar + "]"
		}
		parts = append(parts, p)
	}
	usage := prog + " " + strings.Join(parts, " ")
	if len(prefix)+len(usage) <= width {
		return prefix + usage + "\n"
	}
	getLines := func(parts []string, indent string, withPrefix bool) []string {
		var lines, line []string
		lineLen := len(indent) - 1
		if withPrefix {
			lineLen = len(prefix) - 1
		}
		for _, p := range parts {
			if lineLen+1+len(p) > width && len(line) > 0 {
				lines = append(lines, indent+strings.Join(line, " "))
				line = nil
				lineLen = len(indent) - 1
			}
			line = append(line, p)
			lineLen += len(p) + 1
		}
		if len(line) > 0 {
			lines = append(lines, indent+strings.Join(line, " "))
		}
		if withPrefix {
			lines[0] = lines[0][len(indent):]
		}
		return lines
	}
	var lines []string
	if float64(len(prefix)+len(prog)) <= 0.75*float64(width) {
		indent := strings.Repeat(" ", len(prefix)+len(prog)+1)
		lines = getLines(append([]string{prog}, parts...), indent, true)
	} else {
		indent := strings.Repeat(" ", len(prefix))
		lines = getLines(parts, indent, false)
		if len(lines) > 1 {
			lines = getLines(parts, indent, false)
		}
		lines = append([]string{prog}, lines...)
	}
	return prefix + strings.Join(lines, "\n") + "\n"
}

// terminalColumns is shutil.get_terminal_size().columns as the oracle
// sees it: COLUMNS when it is a positive int, else the size of the
// terminal on stdout (which the parent passes down), else 80.
func terminalColumns(env map[string]string) int {
	if c, ok := pyIntText(env["COLUMNS"]); ok && c > 0 {
		return c
	}
	if c, ok := pyIntText(env[ttyColumnsEnv]); ok && c > 0 {
		return c
	}
	return 80
}

// ttyColumnsEnv carries the width of the terminal on the parent's stdout
// to the child, whose own stdout is a pipe.
const ttyColumnsEnv = "DAISUGI_GATE_TTY_COLUMNS"

// pyIntText is int(s) for plain ASCII text; anything else is not an int.
func pyIntText(s string) (int, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, false
	}
	for i := 0; i < len(t); i++ {
		if t[i] >= 0x80 {
			unported("a COLUMNS value with non-ASCII text")
		}
	}
	if strings.HasPrefix(t, "_") || strings.HasSuffix(t, "_") || strings.Contains(t, "__") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.ReplaceAll(t, "_", ""))
	if err != nil {
		return 0, false
	}
	return n, true
}

// invocation is HelpFormatter._format_action_invocation.
func (a *argAction) invocation() string {
	switch {
	case a.help:
		return "-h, --help"
	case a.flag:
		return a.opts[0]
	case a.choices != nil:
		return a.opts[0] + " {" + strings.Join(a.choices, ",") + "}"
	}
	return a.opts[0] + " " + a.metavar
}

// formatHelp is parser.format_help() at a terminal of columns: the usage,
// then the options section, each help wrapped as textwrap.wrap wraps it.
func formatHelp(columns int) string {
	width := columns - 2
	maxHelp := min(24, max(width-20, 4))
	actionMax := 0
	for _, a := range argActions {
		actionMax = max(actionMax, len(a.invocation())+2)
	}
	helpPos := min(actionMax+2, maxHelp)
	helpWidth := max(width-helpPos, 11)
	actionWidth := helpPos - 2 - 2
	var b strings.Builder
	b.WriteString(formatUsage(columns))
	b.WriteString("\noptions:\n")
	for _, a := range argActions {
		header := a.invocation()
		chunks := helpChunks[a.opts[0]]
		indentFirst := helpPos
		switch {
		case chunks == nil:
			b.WriteString("  " + header + "\n")
			continue
		case len(header) <= actionWidth:
			b.WriteString("  " + header + strings.Repeat(" ", actionWidth-len(header)) + "  ")
			indentFirst = 0
		default:
			b.WriteString("  " + header + "\n")
		}
		lines := wrapChunks(chunks, helpWidth)
		b.WriteString(strings.Repeat(" ", indentFirst) + lines[0] + "\n")
		for _, l := range lines[1:] {
			b.WriteString(strings.Repeat(" ", helpPos) + l + "\n")
		}
	}
	return b.String()
}

// wrapChunks is textwrap.TextWrapper(width)._wrap_chunks with the default
// settings: whitespace dropped at line edges, long words broken, after a
// hyphen where one fits.
func wrapChunks(in []string, width int) []string {
	chunks := make([]string, len(in))
	for i, c := range in {
		chunks[len(in)-1-i] = c // a stack: the next chunk is last
	}
	blank := func(s string) bool { return strings.TrimSpace(s) == "" }
	var lines []string
	for len(chunks) > 0 {
		var cur []string
		curLen := 0
		if blank(chunks[len(chunks)-1]) && len(lines) > 0 {
			chunks = chunks[:len(chunks)-1]
		}
		for len(chunks) > 0 {
			l := len(chunks[len(chunks)-1])
			if curLen+l > width {
				break
			}
			cur = append(cur, chunks[len(chunks)-1])
			chunks = chunks[:len(chunks)-1]
			curLen += l
		}
		if len(chunks) > 0 && len(chunks[len(chunks)-1]) > width {
			spaceLeft := width - curLen
			if width < 1 {
				spaceLeft = 1
			}
			end := spaceLeft
			chunk := chunks[len(chunks)-1]
			if len(chunk) > spaceLeft {
				if h := strings.LastIndex(chunk[:spaceLeft], "-"); h > 0 && strings.Trim(chunk[:h], "-") != "" {
					end = h + 1
				}
			}
			cur = append(cur, chunk[:end])
			chunks[len(chunks)-1] = chunk[end:]
			curLen = 0
			for _, c := range cur {
				curLen += len(c)
			}
		}
		if len(cur) > 0 && blank(cur[len(cur)-1]) {
			cur = cur[:len(cur)-1]
		}
		if len(cur) > 0 {
			lines = append(lines, strings.Join(cur, ""))
		}
	}
	return lines
}

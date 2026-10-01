package pyyaml

// The whole of PyYAML 6's yaml.safe_load on a str: the reader, scanner,
// parser, composer, resolver and SafeConstructor, translated from the
// Python source (yaml/reader.py, scanner.py, parser.py, composer.py,
// resolver.py, constructor.py) one method at a time, so the same text
// gives the same value or raises the same error with the same words.
//
// What the result model cannot hold is Unsupported: !!binary, !!omap,
// !!pairs and !!set values, a node that holds itself, a float or timestamp
// key, and nesting deep enough to reach Python's recursion limit.

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"daisugi-verify/internal/lazyre"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

const maxDepth = 200

// ---------------------------------------------------------------------------
// Marks and errors (yaml/error.py)
// ---------------------------------------------------------------------------

type mark struct {
	index, line, column int
	buf                 []rune
	ptr                 int
}

func isBreakZ(r rune) bool {
	return r == 0 || r == '\r' || r == '\n' || r == 0x85 || r == 0x2028 || r == 0x2029
}

func (m *mark) snippet() string {
	const indent, maxLength = 4, 75
	head, start := "", m.ptr
	for start > 0 && !isBreakZ(m.buf[start-1]) {
		start--
		if float64(m.ptr-start) > float64(maxLength)/2-1 {
			head = " ... "
			start += 5
			break
		}
	}
	tail, end := "", m.ptr
	for end < len(m.buf) && !isBreakZ(m.buf[end]) {
		end++
		if float64(end-m.ptr) > float64(maxLength)/2-1 {
			tail = " ... "
			end -= 5
			break
		}
	}
	return strings.Repeat(" ", indent) + head + string(m.buf[start:end]) + tail + "\n" +
		strings.Repeat(" ", indent+m.ptr-start+utf8.RuneCountInString(head)) + "^"
}

func (m *mark) String() string {
	return fmt.Sprintf("  in \"<unicode string>\", line %d, column %d:\n", m.line+1, m.column+1) + m.snippet()
}

func sameMark(a, b *mark) bool { return a.line == b.line && a.column == b.column }

// raise is MarkedYAMLError(context, context_mark, problem, problem_mark)
// raised as typ; an empty context or problem is None.
func raise(typ, context string, cmark *mark, problem string, pmark *mark) {
	var lines []string
	if context != "" {
		lines = append(lines, context)
	}
	if cmark != nil && (problem == "" || pmark == nil || !sameMark(cmark, pmark)) {
		lines = append(lines, cmark.String())
	}
	if problem != "" {
		lines = append(lines, problem)
	}
	if pmark != nil {
		lines = append(lines, pmark.String())
	}
	panic(pystr.NewException(typ, strings.Join(lines, "\n")))
}

func rr(r rune) string { return pystr.Repr(string(r)) }

// ---------------------------------------------------------------------------
// Tokens and events
// ---------------------------------------------------------------------------

type tokKind int

const (
	tDirective tokKind = iota
	tDocStart
	tDocEnd
	tStreamStart
	tStreamEnd
	tBlockSeqStart
	tBlockMapStart
	tBlockEnd
	tFlowSeqStart
	tFlowMapStart
	tFlowSeqEnd
	tFlowMapEnd
	tKey
	tValue
	tBlockEntry
	tFlowEntry
	tAlias
	tAnchor
	tTag
	tScalar
)

var tokID = [...]string{"<directive>", "<document start>", "<document end>", "<stream start>",
	"<stream end>", "<block sequence start>", "<block mapping start>", "<block end>", "[", "{",
	"]", "}", "?", ":", "-", ",", "<alias>", "<anchor>", "<tag>", "<scalar>"}

type token struct {
	kind       tokKind
	start, end *mark
	value      string // scalar, alias, anchor; directive name
	plain      bool
	style      rune
	handle     *string // tag handle (nil is None)
	suffix     string
	major      int // %YAML
	minor      int
	tagHandle  string // %TAG
	tagPrefix  string
}

type evKind int

const (
	eStreamStart evKind = iota
	eStreamEnd
	eDocStart
	eDocEnd
	eAlias
	eScalar
	eSeqStart
	eSeqEnd
	eMapStart
	eMapEnd
)

type event struct {
	kind       evKind
	start, end *mark
	anchor     *string
	tag        *string
	implicit0  bool
	implicit1  bool
	value      string
}

// ---------------------------------------------------------------------------
// The loader: reader + scanner + parser + composer + constructor
// ---------------------------------------------------------------------------

type possibleKey struct {
	tokenNumber int
	required    bool
	index       int
	line        int
	column      int
	mark        *mark
}

type loader struct {
	// reader
	buf                   []rune
	ptr, index, line, col int

	// scanner
	done          bool
	flowLevel     int
	tokens        []*token
	tokensTaken   int
	indent        int
	indents       []int
	allowSimple   bool
	possibleLevel []int // possible_simple_keys, in dict order
	possible      map[int]*possibleKey

	// unmodeled is the first value the result model does not hold. It is
	// refused only when the whole load ends without an error, since a later
	// error is what safe_load raises.
	unmodeled string

	// parser
	current    *event
	tagHandles map[string]string
	states     []func() *event
	marks      []*mark
	state      func() *event

	// composer
	anchors   map[string]*node
	composing map[*node]bool
	depth     int
}

func (l *loader) peek(i int) rune {
	if l.ptr+i >= len(l.buf) {
		unsupported("a read past the end of the text")
	}
	return l.buf[l.ptr+i]
}

func (l *loader) prefix(n int) string {
	end := l.ptr + n
	if end > len(l.buf) {
		end = len(l.buf)
	}
	return string(l.buf[l.ptr:end])
}

func (l *loader) forward(n int) {
	for ; n > 0; n-- {
		ch := l.buf[l.ptr]
		l.ptr++
		l.index++
		if ch == '\n' || ch == 0x85 || ch == 0x2028 || ch == 0x2029 || (ch == '\r' && l.buf[l.ptr] != '\n') {
			l.line++
			l.col = 0
		} else if ch != 0xFEFF {
			l.col++
		}
	}
}

func (l *loader) mark() *mark {
	return &mark{index: l.index, line: l.line, column: l.col, buf: l.buf, ptr: l.ptr}
}

// in reports whether r is one of set's characters; "\x00" in set means
// the end of the text.
func in(r rune, set string) bool { return strings.ContainsRune(set, r) }

const (
	breakz      = "\x00\r\n\u0085\u2028\u2029"
	blankz      = "\x00 \t\r\n\u0085\u2028\u2029"
	breaks      = "\r\n\u0085\u2028\u2029"
	alnum       = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
	hexDigits   = "0123456789ABCDEFabcdef"
	uriChars    = alnum + ";/?:@&=+$,_.!~*'()[]%"
	scanErr     = "ScannerError"
	parseErr    = "ParserError"
	composeErr  = "ComposerError"
	constructEr = "ConstructorError"
)

func isAlnum(r rune) bool { return r < 128 && in(r, alnum) }

// --- scanner public ---------------------------------------------------------

func (l *loader) checkToken(kinds ...tokKind) bool {
	for l.needMore() {
		l.fetchMore()
	}
	if len(l.tokens) > 0 {
		if len(kinds) == 0 {
			return true
		}
		for _, k := range kinds {
			if l.tokens[0].kind == k {
				return true
			}
		}
	}
	return false
}

func (l *loader) peekToken() *token {
	for l.needMore() {
		l.fetchMore()
	}
	if len(l.tokens) > 0 {
		return l.tokens[0]
	}
	return nil
}

func (l *loader) getToken() *token {
	for l.needMore() {
		l.fetchMore()
	}
	if len(l.tokens) > 0 {
		l.tokensTaken++
		t := l.tokens[0]
		l.tokens = l.tokens[1:]
		return t
	}
	return nil
}

func (l *loader) needMore() bool {
	if l.done {
		return false
	}
	if len(l.tokens) == 0 {
		return true
	}
	l.staleKeys()
	if n, ok := l.nextPossible(); ok && n == l.tokensTaken {
		return true
	}
	return false
}

func (l *loader) fetchMore() {
	l.scanToNextToken()
	l.staleKeys()
	l.unwindIndent(l.col)
	ch := l.peek(0)
	switch {
	case ch == 0:
		l.fetchStreamEnd()
	case ch == '%' && l.col == 0:
		l.fetchDirective()
	case ch == '-' && l.checkDocument("---"):
		l.fetchDocIndicator(tDocStart)
	case ch == '.' && l.checkDocument("..."):
		l.fetchDocIndicator(tDocEnd)
	case ch == '[':
		l.fetchFlowStart(tFlowSeqStart)
	case ch == '{':
		l.fetchFlowStart(tFlowMapStart)
	case ch == ']':
		l.fetchFlowEnd(tFlowSeqEnd)
	case ch == '}':
		l.fetchFlowEnd(tFlowMapEnd)
	case ch == ',':
		l.fetchFlowEntry()
	case ch == '-' && in(l.peek(1), blankz):
		l.fetchBlockEntry()
	case ch == '?' && (l.flowLevel > 0 || in(l.peek(1), blankz)):
		l.fetchKey()
	case ch == ':' && (l.flowLevel > 0 || in(l.peek(1), blankz)):
		l.fetchValue()
	case ch == '*':
		l.fetchAnchor(tAlias)
	case ch == '&':
		l.fetchAnchor(tAnchor)
	case ch == '!':
		l.fetchTag()
	case ch == '|' && l.flowLevel == 0:
		l.fetchBlockScalar('|')
	case ch == '>' && l.flowLevel == 0:
		l.fetchBlockScalar('>')
	case ch == '\'':
		l.fetchFlowScalar('\'')
	case ch == '"':
		l.fetchFlowScalar('"')
	case l.checkPlain():
		l.fetchPlain()
	default:
		raise(scanErr, "while scanning for the next token", nil,
			"found character "+rr(ch)+" that cannot start any token", l.mark())
	}
}

func (l *loader) checkDocument(ind string) bool {
	return l.col == 0 && l.prefix(3) == ind && in(l.peek(3), blankz)
}

// --- simple keys ------------------------------------------------------------

func (l *loader) nextPossible() (int, bool) {
	min, ok := 0, false
	for _, lv := range l.possibleLevel {
		k := l.possible[lv]
		if !ok || k.tokenNumber < min {
			min, ok = k.tokenNumber, true
		}
	}
	return min, ok
}

func (l *loader) delPossible(level int) {
	delete(l.possible, level)
	for i, lv := range l.possibleLevel {
		if lv == level {
			l.possibleLevel = append(l.possibleLevel[:i:i], l.possibleLevel[i+1:]...)
			return
		}
	}
}

func (l *loader) staleKeys() {
	for _, lv := range append([]int(nil), l.possibleLevel...) {
		k := l.possible[lv]
		if k.line != l.line || l.index-k.index > 1024 {
			if k.required {
				raise(scanErr, "while scanning a simple key", k.mark, "could not find expected ':'", l.mark())
			}
			l.delPossible(lv)
		}
	}
}

func (l *loader) saveSimpleKey() {
	required := l.flowLevel == 0 && l.indent == l.col
	if l.allowSimple {
		l.removeSimpleKey()
		k := &possibleKey{l.tokensTaken + len(l.tokens), required, l.index, l.line, l.col, l.mark()}
		l.possible[l.flowLevel] = k
		l.possibleLevel = append(l.possibleLevel, l.flowLevel)
	}
}

func (l *loader) removeSimpleKey() {
	if k, ok := l.possible[l.flowLevel]; ok {
		if k.required {
			raise(scanErr, "while scanning a simple key", k.mark, "could not find expected ':'", l.mark())
		}
		l.delPossible(l.flowLevel)
	}
}

// --- indentation -------------------------------------------------------------

func (l *loader) unwindIndent(column int) {
	if l.flowLevel > 0 {
		return
	}
	for l.indent > column {
		m := l.mark()
		l.indent = l.indents[len(l.indents)-1]
		l.indents = l.indents[:len(l.indents)-1]
		l.tokens = append(l.tokens, &token{kind: tBlockEnd, start: m, end: m})
	}
}

func (l *loader) addIndent(column int) bool {
	if l.indent < column {
		l.indents = append(l.indents, l.indent)
		l.indent = column
		return true
	}
	return false
}

// --- fetchers ----------------------------------------------------------------

func (l *loader) simple(kind tokKind) *token {
	s := l.mark()
	l.forward(1)
	return &token{kind: kind, start: s, end: l.mark()}
}

func (l *loader) fetchStreamEnd() {
	l.unwindIndent(-1)
	l.removeSimpleKey()
	l.allowSimple = false
	l.possible = map[int]*possibleKey{}
	l.possibleLevel = nil
	m := l.mark()
	l.tokens = append(l.tokens, &token{kind: tStreamEnd, start: m, end: m})
	l.done = true
}

func (l *loader) fetchDirective() {
	l.unwindIndent(-1)
	l.removeSimpleKey()
	l.allowSimple = false
	l.tokens = append(l.tokens, l.scanDirective())
}

func (l *loader) fetchDocIndicator(kind tokKind) {
	l.unwindIndent(-1)
	l.removeSimpleKey()
	l.allowSimple = false
	s := l.mark()
	l.forward(3)
	l.tokens = append(l.tokens, &token{kind: kind, start: s, end: l.mark()})
}

func (l *loader) fetchFlowStart(kind tokKind) {
	l.saveSimpleKey()
	l.flowLevel++
	l.allowSimple = true
	l.tokens = append(l.tokens, l.simple(kind))
}

func (l *loader) fetchFlowEnd(kind tokKind) {
	l.removeSimpleKey()
	l.flowLevel--
	l.allowSimple = false
	l.tokens = append(l.tokens, l.simple(kind))
}

func (l *loader) fetchFlowEntry() {
	l.allowSimple = true
	l.removeSimpleKey()
	l.tokens = append(l.tokens, l.simple(tFlowEntry))
}

func (l *loader) fetchBlockEntry() {
	if l.flowLevel == 0 {
		if !l.allowSimple {
			raise(scanErr, "", nil, "sequence entries are not allowed here", l.mark())
		}
		if l.addIndent(l.col) {
			m := l.mark()
			l.tokens = append(l.tokens, &token{kind: tBlockSeqStart, start: m, end: m})
		}
	}
	l.allowSimple = true
	l.removeSimpleKey()
	l.tokens = append(l.tokens, l.simple(tBlockEntry))
}

func (l *loader) fetchKey() {
	if l.flowLevel == 0 {
		if !l.allowSimple {
			raise(scanErr, "", nil, "mapping keys are not allowed here", l.mark())
		}
		if l.addIndent(l.col) {
			m := l.mark()
			l.tokens = append(l.tokens, &token{kind: tBlockMapStart, start: m, end: m})
		}
	}
	l.allowSimple = l.flowLevel == 0
	l.removeSimpleKey()
	l.tokens = append(l.tokens, l.simple(tKey))
}

func (l *loader) insertToken(at int, t *token) {
	l.tokens = append(l.tokens, nil)
	copy(l.tokens[at+1:], l.tokens[at:])
	l.tokens[at] = t
}

func (l *loader) fetchValue() {
	if k, ok := l.possible[l.flowLevel]; ok {
		l.delPossible(l.flowLevel)
		l.insertToken(k.tokenNumber-l.tokensTaken, &token{kind: tKey, start: k.mark, end: k.mark})
		if l.flowLevel == 0 && l.addIndent(k.column) {
			l.insertToken(k.tokenNumber-l.tokensTaken, &token{kind: tBlockMapStart, start: k.mark, end: k.mark})
		}
		l.allowSimple = false
	} else {
		if l.flowLevel == 0 {
			if !l.allowSimple {
				raise(scanErr, "", nil, "mapping values are not allowed here", l.mark())
			}
			if l.addIndent(l.col) {
				m := l.mark()
				l.tokens = append(l.tokens, &token{kind: tBlockMapStart, start: m, end: m})
			}
		}
		l.allowSimple = l.flowLevel == 0
		l.removeSimpleKey()
	}
	l.tokens = append(l.tokens, l.simple(tValue))
}

func (l *loader) fetchAnchor(kind tokKind) {
	l.saveSimpleKey()
	l.allowSimple = false
	l.tokens = append(l.tokens, l.scanAnchor(kind))
}

func (l *loader) fetchTag() {
	l.saveSimpleKey()
	l.allowSimple = false
	l.tokens = append(l.tokens, l.scanTag())
}

func (l *loader) fetchBlockScalar(style rune) {
	l.allowSimple = true
	l.removeSimpleKey()
	l.tokens = append(l.tokens, l.scanBlockScalar(style))
}

func (l *loader) fetchFlowScalar(style rune) {
	l.saveSimpleKey()
	l.allowSimple = false
	l.tokens = append(l.tokens, l.scanFlowScalar(style))
}

func (l *loader) fetchPlain() {
	l.saveSimpleKey()
	l.allowSimple = false
	l.tokens = append(l.tokens, l.scanPlain())
}

func (l *loader) checkPlain() bool {
	ch := l.peek(0)
	return !in(ch, "\x00 \t\r\n\u0085\u2028\u2029-?:,[]{}#&*!|>'\"%@`") ||
		(!in(l.peek(1), blankz) && (ch == '-' || (l.flowLevel == 0 && in(ch, "?:"))))
}

// --- scanners ----------------------------------------------------------------

func (l *loader) scanToNextToken() {
	if l.index == 0 && l.peek(0) == 0xFEFF {
		l.forward(1)
	}
	for {
		for l.peek(0) == ' ' {
			l.forward(1)
		}
		if l.peek(0) == '#' {
			for !in(l.peek(0), breakz) {
				l.forward(1)
			}
		}
		if l.scanLineBreak() != "" {
			if l.flowLevel == 0 {
				l.allowSimple = true
			}
		} else {
			return
		}
	}
}

func (l *loader) scanDirective() *token {
	start := l.mark()
	l.forward(1)
	name := l.scanDirectiveName(start)
	t := &token{kind: tDirective, start: start, value: name}
	switch name {
	case "YAML":
		t.major, t.minor = l.scanYAMLDirectiveValue(start)
		t.end = l.mark()
	case "TAG":
		t.tagHandle, t.tagPrefix = l.scanTagDirectiveValue(start)
		t.end = l.mark()
	default:
		t.end = l.mark()
		for !in(l.peek(0), breakz) {
			l.forward(1)
		}
	}
	l.scanDirectiveIgnoredLine(start)
	return t
}

func (l *loader) scanDirectiveName(start *mark) string {
	n := 0
	for isAlnum(l.peek(n)) {
		n++
	}
	if n == 0 {
		raise(scanErr, "while scanning a directive", start,
			"expected alphabetic or numeric character, but found "+rr(l.peek(n)), l.mark())
	}
	v := l.prefix(n)
	l.forward(n)
	if !in(l.peek(0), "\x00 \r\n\u0085\u2028\u2029") {
		raise(scanErr, "while scanning a directive", start,
			"expected alphabetic or numeric character, but found "+rr(l.peek(0)), l.mark())
	}
	return v
}

func (l *loader) scanYAMLDirectiveValue(start *mark) (int, int) {
	for l.peek(0) == ' ' {
		l.forward(1)
	}
	major := l.scanYAMLDirectiveNumber(start)
	if l.peek(0) != '.' {
		raise(scanErr, "while scanning a directive", start,
			"expected a digit or '.', but found "+rr(l.peek(0)), l.mark())
	}
	l.forward(1)
	minor := l.scanYAMLDirectiveNumber(start)
	if !in(l.peek(0), "\x00 \r\n\u0085\u2028\u2029") {
		raise(scanErr, "while scanning a directive", start,
			"expected a digit or ' ', but found "+rr(l.peek(0)), l.mark())
	}
	return major, minor
}

func (l *loader) scanYAMLDirectiveNumber(start *mark) int {
	ch := l.peek(0)
	if ch < '0' || ch > '9' {
		raise(scanErr, "while scanning a directive", start, "expected a digit, but found "+rr(ch), l.mark())
	}
	n := 0
	for c := l.peek(n); c >= '0' && c <= '9'; c = l.peek(n) {
		n++
	}
	v, err := strconv.Atoi(l.prefix(n))
	if err != nil {
		unsupported("a %YAML version number past an int")
	}
	l.forward(n)
	return v
}

func (l *loader) scanTagDirectiveValue(start *mark) (string, string) {
	for l.peek(0) == ' ' {
		l.forward(1)
	}
	handle := l.scanTagHandle("directive", start)
	if l.peek(0) != ' ' {
		raise(scanErr, "while scanning a directive", start, "expected ' ', but found "+rr(l.peek(0)), l.mark())
	}
	for l.peek(0) == ' ' {
		l.forward(1)
	}
	prefix := l.scanTagURI("directive", start)
	if !in(l.peek(0), "\x00 \r\n\u0085\u2028\u2029") {
		raise(scanErr, "while scanning a directive", start, "expected ' ', but found "+rr(l.peek(0)), l.mark())
	}
	return handle, prefix
}

func (l *loader) scanDirectiveIgnoredLine(start *mark) {
	for l.peek(0) == ' ' {
		l.forward(1)
	}
	if l.peek(0) == '#' {
		for !in(l.peek(0), breakz) {
			l.forward(1)
		}
	}
	if !in(l.peek(0), breakz) {
		raise(scanErr, "while scanning a directive", start,
			"expected a comment or a line break, but found "+rr(l.peek(0)), l.mark())
	}
	l.scanLineBreak()
}

func (l *loader) scanAnchor(kind tokKind) *token {
	start := l.mark()
	name := "anchor"
	if l.peek(0) == '*' {
		name = "alias"
	}
	l.forward(1)
	n := 0
	for isAlnum(l.peek(n)) {
		n++
	}
	if n == 0 {
		raise(scanErr, "while scanning an "+name, start,
			"expected alphabetic or numeric character, but found "+rr(l.peek(n)), l.mark())
	}
	v := l.prefix(n)
	l.forward(n)
	if !in(l.peek(0), "\x00 \t\r\n\u0085\u2028\u2029?:,]}%@`") {
		raise(scanErr, "while scanning an "+name, start,
			"expected alphabetic or numeric character, but found "+rr(l.peek(0)), l.mark())
	}
	return &token{kind: kind, start: start, end: l.mark(), value: v}
}

func (l *loader) scanTag() *token {
	start := l.mark()
	ch := l.peek(1)
	var handle *string
	var suffix string
	switch {
	case ch == '<':
		l.forward(2)
		suffix = l.scanTagURI("tag", start)
		if l.peek(0) != '>' {
			raise(scanErr, "while parsing a tag", start, "expected '>', but found "+rr(l.peek(0)), l.mark())
		}
		l.forward(1)
	case in(ch, blankz):
		suffix = "!"
		l.forward(1)
	default:
		n, useHandle := 1, false
		for !in(ch, "\x00 \r\n\u0085\u2028\u2029") {
			if ch == '!' {
				useHandle = true
				break
			}
			n++
			ch = l.peek(n)
		}
		h := "!"
		if useHandle {
			h = l.scanTagHandle("tag", start)
		} else {
			l.forward(1)
		}
		handle = &h
		suffix = l.scanTagURI("tag", start)
	}
	if !in(l.peek(0), "\x00 \r\n\u0085\u2028\u2029") {
		raise(scanErr, "while scanning a tag", start, "expected ' ', but found "+rr(l.peek(0)), l.mark())
	}
	return &token{kind: tTag, start: start, end: l.mark(), handle: handle, suffix: suffix}
}

func (l *loader) scanBlockScalar(style rune) *token {
	folded := style == '>'
	var chunks []string
	start := l.mark()
	l.forward(1)
	chomping, increment := l.scanBlockScalarIndicators(start)
	l.scanBlockScalarIgnoredLine(start)
	minIndent := l.indent + 1
	if minIndent < 1 {
		minIndent = 1
	}
	var brks []string
	var end *mark
	var indent int
	if increment == 0 {
		var maxIndent int
		brks, maxIndent, end = l.scanBlockScalarIndentation()
		indent = max(minIndent, maxIndent)
	} else {
		indent = minIndent + increment - 1
		brks, end = l.scanBlockScalarBreaks(indent)
	}
	lineBreak := ""
	for l.col == indent && l.peek(0) != 0 {
		chunks = append(chunks, brks...)
		leadingNonSpace := !in(l.peek(0), " \t")
		n := 0
		for !in(l.peek(n), breakz) {
			n++
		}
		chunks = append(chunks, l.prefix(n))
		l.forward(n)
		lineBreak = l.scanLineBreak()
		brks, end = l.scanBlockScalarBreaks(indent)
		if l.col == indent && l.peek(0) != 0 {
			if folded && lineBreak == "\n" && leadingNonSpace && !in(l.peek(0), " \t") {
				if len(brks) == 0 {
					chunks = append(chunks, " ")
				}
			} else {
				chunks = append(chunks, lineBreak)
			}
		} else {
			break
		}
	}
	if chomping != 'F' {
		chunks = append(chunks, lineBreak)
	}
	if chomping == 'T' {
		chunks = append(chunks, brks...)
	}
	return &token{kind: tScalar, value: strings.Join(chunks, ""), start: start, end: end, style: style}
}

// scanBlockScalarIndicators: chomping is 'T' (True), 'F' (False) or 0
// (None); increment 0 is None.
func (l *loader) scanBlockScalarIndicators(start *mark) (rune, int) {
	var chomping rune
	increment := 0
	zero := func() {
		raise(scanErr, "while scanning a block scalar", start,
			"expected indentation indicator in the range 1-9, but found 0", l.mark())
	}
	ch := l.peek(0)
	if ch == '+' || ch == '-' {
		chomping = 'F'
		if ch == '+' {
			chomping = 'T'
		}
		l.forward(1)
		ch = l.peek(0)
		if ch >= '0' && ch <= '9' {
			increment = int(ch - '0')
			if increment == 0 {
				zero()
			}
			l.forward(1)
		}
	} else if ch >= '0' && ch <= '9' {
		increment = int(ch - '0')
		if increment == 0 {
			zero()
		}
		l.forward(1)
		ch = l.peek(0)
		if ch == '+' || ch == '-' {
			chomping = 'F'
			if ch == '+' {
				chomping = 'T'
			}
			l.forward(1)
		}
	}
	if !in(l.peek(0), "\x00 \r\n\u0085\u2028\u2029") {
		raise(scanErr, "while scanning a block scalar", start,
			"expected chomping or indentation indicators, but found "+rr(l.peek(0)), l.mark())
	}
	return chomping, increment
}

func (l *loader) scanBlockScalarIgnoredLine(start *mark) {
	for l.peek(0) == ' ' {
		l.forward(1)
	}
	if l.peek(0) == '#' {
		for !in(l.peek(0), breakz) {
			l.forward(1)
		}
	}
	if !in(l.peek(0), breakz) {
		raise(scanErr, "while scanning a block scalar", start,
			"expected a comment or a line break, but found "+rr(l.peek(0)), l.mark())
	}
	l.scanLineBreak()
}

func (l *loader) scanBlockScalarIndentation() ([]string, int, *mark) {
	var chunks []string
	maxIndent := 0
	end := l.mark()
	for in(l.peek(0), " \r\n\u0085\u2028\u2029") && l.peek(0) != 0 {
		if l.peek(0) != ' ' {
			chunks = append(chunks, l.scanLineBreak())
			end = l.mark()
		} else {
			l.forward(1)
			if l.col > maxIndent {
				maxIndent = l.col
			}
		}
	}
	return chunks, maxIndent, end
}

func (l *loader) scanBlockScalarBreaks(indent int) ([]string, *mark) {
	var chunks []string
	end := l.mark()
	for l.col < indent && l.peek(0) == ' ' {
		l.forward(1)
	}
	for in(l.peek(0), breaks) && l.peek(0) != 0 {
		chunks = append(chunks, l.scanLineBreak())
		end = l.mark()
		for l.col < indent && l.peek(0) == ' ' {
			l.forward(1)
		}
	}
	return chunks, end
}

func (l *loader) scanFlowScalar(style rune) *token {
	double := style == '"'
	var chunks []string
	start := l.mark()
	quote := l.peek(0)
	l.forward(1)
	chunks = append(chunks, l.scanFlowScalarNonSpaces(double, start)...)
	for l.peek(0) != quote {
		chunks = append(chunks, l.scanFlowScalarSpaces(double, start)...)
		chunks = append(chunks, l.scanFlowScalarNonSpaces(double, start)...)
	}
	l.forward(1)
	return &token{kind: tScalar, value: strings.Join(chunks, ""), start: start, end: l.mark(), style: style}
}

var loadEscapes = map[rune]string{
	'0': "\x00", 'a': "\x07", 'b': "\x08", 't': "\x09", '\t': "\x09", 'n': "\x0a", 'v': "\x0b",
	'f': "\x0c", 'r': "\x0d", 'e': "\x1b", ' ': " ", '"': "\"", '\\': "\\", '/': "/",
	'N': "\u0085", '_': "\u00a0", 'L': "\u2028", 'P': "\u2029",
}

var escapeCodes = map[rune]int{'x': 2, 'u': 4, 'U': 8}

func (l *loader) scanFlowScalarNonSpaces(double bool, start *mark) []string {
	var chunks []string
	for {
		n := 0
		for !in(l.peek(n), "'\"\\\x00 \t\r\n\u0085\u2028\u2029") {
			n++
		}
		if n > 0 {
			chunks = append(chunks, l.prefix(n))
			l.forward(n)
		}
		ch := l.peek(0)
		switch {
		case !double && ch == '\'' && l.peek(1) == '\'':
			chunks = append(chunks, "'")
			l.forward(2)
		case (double && ch == '\'') || (!double && (ch == '"' || ch == '\\')):
			chunks = append(chunks, string(ch))
			l.forward(1)
		case double && ch == '\\':
			l.forward(1)
			ch = l.peek(0)
			if rep, ok := loadEscapes[ch]; ok {
				chunks = append(chunks, rep)
				l.forward(1)
			} else if width, ok := escapeCodes[ch]; ok {
				l.forward(1)
				for k := 0; k < width; k++ {
					if !in(l.peek(k), hexDigits) || l.peek(k) == 0 {
						raise(scanErr, "while scanning a double-quoted scalar", start,
							fmt.Sprintf("expected escape sequence of %d hexadecimal numbers, but found %s", width, rr(l.peek(k))),
							l.mark())
					}
				}
				code, _ := strconv.ParseUint(l.prefix(width), 16, 64)
				if code > 0x10FFFF {
					// chr() raises ValueError: not a YAML error.
					panic(pystr.NewException("ValueError", "chr() arg not in range(0x110000)"))
				}
				if code >= 0xD800 && code <= 0xDFFF {
					unsupported("an escape for a lone surrogate")
				}
				chunks = append(chunks, string(rune(code)))
				l.forward(width)
			} else if in(ch, breaks) && ch != 0 {
				l.scanLineBreak()
				chunks = append(chunks, l.scanFlowScalarBreaks(double, start)...)
			} else {
				raise(scanErr, "while scanning a double-quoted scalar", start,
					"found unknown escape character "+rr(ch), l.mark())
			}
		default:
			return chunks
		}
	}
}

func (l *loader) scanFlowScalarSpaces(double bool, start *mark) []string {
	var chunks []string
	n := 0
	for in(l.peek(n), " \t") && l.peek(n) != 0 {
		n++
	}
	ws := l.prefix(n)
	l.forward(n)
	ch := l.peek(0)
	if ch == 0 {
		raise(scanErr, "while scanning a quoted scalar", start, "found unexpected end of stream", l.mark())
	} else if in(ch, breaks) {
		lineBreak := l.scanLineBreak()
		brks := l.scanFlowScalarBreaks(double, start)
		if lineBreak != "\n" {
			chunks = append(chunks, lineBreak)
		} else if len(brks) == 0 {
			chunks = append(chunks, " ")
		}
		chunks = append(chunks, brks...)
	} else {
		chunks = append(chunks, ws)
	}
	return chunks
}

func (l *loader) scanFlowScalarBreaks(double bool, start *mark) []string {
	var chunks []string
	for {
		p := l.prefix(3)
		if (p == "---" || p == "...") && in(l.peek(3), blankz) {
			raise(scanErr, "while scanning a quoted scalar", start, "found unexpected document separator", l.mark())
		}
		for in(l.peek(0), " \t") && l.peek(0) != 0 {
			l.forward(1)
		}
		if in(l.peek(0), breaks) && l.peek(0) != 0 {
			chunks = append(chunks, l.scanLineBreak())
		} else {
			return chunks
		}
	}
}

func (l *loader) scanPlain() *token {
	var chunks []string
	start := l.mark()
	end := start
	indent := l.indent + 1
	var spaces []string
	for {
		n := 0
		if l.peek(0) == '#' {
			break
		}
		for {
			ch := l.peek(n)
			stop := blankz
			if l.flowLevel > 0 {
				stop = blankz + ",[]{}"
			}
			if in(ch, blankz) || (ch == ':' && in(l.peek(n+1), stop)) || (l.flowLevel > 0 && in(ch, ",?[]{}")) {
				break
			}
			n++
		}
		if n == 0 {
			break
		}
		l.allowSimple = false
		chunks = append(chunks, spaces...)
		chunks = append(chunks, l.prefix(n))
		l.forward(n)
		end = l.mark()
		var ok bool
		spaces, ok = l.scanPlainSpaces()
		if !ok || len(spaces) == 0 || l.peek(0) == '#' || (l.flowLevel == 0 && l.col < indent) {
			break
		}
	}
	return &token{kind: tScalar, value: strings.Join(chunks, ""), plain: true, start: start, end: end}
}

// scanPlainSpaces returns ok false where Python returns None.
func (l *loader) scanPlainSpaces() ([]string, bool) {
	var chunks []string
	n := 0
	for l.peek(n) == ' ' {
		n++
	}
	ws := l.prefix(n)
	l.forward(n)
	ch := l.peek(0)
	if in(ch, breaks) && ch != 0 {
		lineBreak := l.scanLineBreak()
		l.allowSimple = true
		sep := func() bool {
			p := l.prefix(3)
			return (p == "---" || p == "...") && in(l.peek(3), blankz)
		}
		if sep() {
			return nil, false
		}
		var brks []string
		for in(l.peek(0), " \r\n\u0085\u2028\u2029") && l.peek(0) != 0 {
			if l.peek(0) == ' ' {
				l.forward(1)
			} else {
				brks = append(brks, l.scanLineBreak())
				if sep() {
					return nil, false
				}
			}
		}
		if lineBreak != "\n" {
			chunks = append(chunks, lineBreak)
		} else if len(brks) == 0 {
			chunks = append(chunks, " ")
		}
		chunks = append(chunks, brks...)
	} else if ws != "" {
		chunks = append(chunks, ws)
	}
	return chunks, true
}

func (l *loader) scanTagHandle(name string, start *mark) string {
	ch := l.peek(0)
	if ch != '!' {
		raise(scanErr, "while scanning a "+name, start, "expected '!', but found "+rr(ch), l.mark())
	}
	n := 1
	ch = l.peek(n)
	if ch != ' ' {
		for isAlnum(ch) {
			n++
			ch = l.peek(n)
		}
		if ch != '!' {
			l.forward(n)
			raise(scanErr, "while scanning a "+name, start, "expected '!', but found "+rr(ch), l.mark())
		}
		n++
	}
	v := l.prefix(n)
	l.forward(n)
	return v
}

func (l *loader) scanTagURI(name string, start *mark) string {
	var chunks []string
	n := 0
	ch := l.peek(n)
	for ch < 128 && ch != 0 && in(ch, uriChars) {
		if ch == '%' {
			chunks = append(chunks, l.prefix(n))
			l.forward(n)
			n = 0
			chunks = append(chunks, l.scanURIEscapes(name, start))
		} else {
			n++
		}
		ch = l.peek(n)
	}
	if n > 0 {
		chunks = append(chunks, l.prefix(n))
		l.forward(n)
	}
	if len(chunks) == 0 {
		raise(scanErr, "while parsing a "+name, start, "expected URI, but found "+rr(ch), l.mark())
	}
	return strings.Join(chunks, "")
}

func (l *loader) scanURIEscapes(name string, start *mark) string {
	var codes []byte
	m := l.mark()
	for l.peek(0) == '%' {
		l.forward(1)
		for k := 0; k < 2; k++ {
			if !in(l.peek(k), hexDigits) || l.peek(k) == 0 {
				raise(scanErr, "while scanning a "+name, start,
					"expected URI escape sequence of 2 hexadecimal numbers, but found "+rr(l.peek(k)), l.mark())
			}
		}
		b, _ := strconv.ParseUint(l.prefix(2), 16, 8)
		codes = append(codes, byte(b))
		l.forward(2)
	}
	if !utf8.Valid(codes) {
		// The UnicodeDecodeError's words follow; this binary does not
		// write them.
		_ = m
		unsupported("a tag URI escape that is not UTF-8")
	}
	return string(codes)
}

func (l *loader) scanLineBreak() string {
	ch := l.peek(0)
	if ch == '\r' || ch == '\n' || ch == 0x85 {
		if l.prefix(2) == "\r\n" {
			l.forward(2)
		} else {
			l.forward(1)
		}
		return "\n"
	}
	if ch == 0x2028 || ch == 0x2029 {
		l.forward(1)
		return string(ch)
	}
	return ""
}

// ---------------------------------------------------------------------------
// The parser (yaml/parser.py)
// ---------------------------------------------------------------------------

var defaultTags = map[string]string{"!": "!", "!!": "tag:yaml.org,2002:"}

func (l *loader) checkEvent(kinds ...evKind) bool {
	if l.current == nil && l.state != nil {
		l.current = l.state()
	}
	if l.current != nil {
		if len(kinds) == 0 {
			return true
		}
		for _, k := range kinds {
			if l.current.kind == k {
				return true
			}
		}
	}
	return false
}

func (l *loader) peekEvent() *event {
	if l.current == nil && l.state != nil {
		l.current = l.state()
	}
	return l.current
}

func (l *loader) getEvent() *event {
	if l.current == nil && l.state != nil {
		l.current = l.state()
	}
	v := l.current
	l.current = nil
	return v
}

func (l *loader) pop() func() *event {
	s := l.states[len(l.states)-1]
	l.states = l.states[:len(l.states)-1]
	return s
}

func (l *loader) popMark() {
	l.marks = l.marks[:len(l.marks)-1]
}

func (l *loader) parseStreamStart() *event {
	t := l.getToken()
	l.state = l.parseImplicitDocumentStart
	return &event{kind: eStreamStart, start: t.start, end: t.end}
}

func (l *loader) parseImplicitDocumentStart() *event {
	if !l.checkToken(tDirective, tDocStart, tStreamEnd) {
		l.tagHandles = defaultTags
		t := l.peekToken()
		l.states = append(l.states, l.parseDocumentEnd)
		l.state = l.parseBlockNode
		return &event{kind: eDocStart, start: t.start, end: t.start}
	}
	return l.parseDocumentStart()
}

func (l *loader) parseDocumentStart() *event {
	for l.checkToken(tDocEnd) {
		l.getToken()
	}
	if !l.checkToken(tStreamEnd) {
		t := l.peekToken()
		start := t.start
		l.processDirectives()
		if !l.checkToken(tDocStart) {
			raise(parseErr, "", nil, "expected '<document start>', but found "+pystr.Repr(tokID[l.peekToken().kind]),
				l.peekToken().start)
		}
		t = l.getToken()
		l.states = append(l.states, l.parseDocumentEnd)
		l.state = l.parseDocumentContent
		return &event{kind: eDocStart, start: start, end: t.end}
	}
	t := l.getToken()
	l.state = nil
	return &event{kind: eStreamEnd, start: t.start, end: t.end}
}

func (l *loader) parseDocumentEnd() *event {
	t := l.peekToken()
	start, end := t.start, t.start
	if l.checkToken(tDocEnd) {
		t = l.getToken()
		end = t.end
	}
	l.state = l.parseDocumentStart
	return &event{kind: eDocEnd, start: start, end: end}
}

func (l *loader) parseDocumentContent() *event {
	if l.checkToken(tDirective, tDocStart, tDocEnd, tStreamEnd) {
		ev := l.emptyScalar(l.peekToken().start)
		l.state = l.pop()
		return ev
	}
	return l.parseBlockNode()
}

func (l *loader) processDirectives() {
	haveVersion := false
	l.tagHandles = map[string]string{}
	for l.checkToken(tDirective) {
		t := l.getToken()
		switch t.value {
		case "YAML":
			if haveVersion {
				raise(parseErr, "", nil, "found duplicate YAML directive", t.start)
			}
			if t.major != 1 {
				raise(parseErr, "", nil, "found incompatible YAML document (version 1.* is required)", t.start)
			}
			haveVersion = true
		case "TAG":
			if _, dup := l.tagHandles[t.tagHandle]; dup {
				raise(parseErr, "", nil, "duplicate tag handle "+pystr.Repr(t.tagHandle), t.start)
			}
			l.tagHandles[t.tagHandle] = t.tagPrefix
		}
	}
	for k, v := range defaultTags {
		if _, ok := l.tagHandles[k]; !ok {
			l.tagHandles[k] = v
		}
	}
}

func (l *loader) parseBlockNode() *event                { return l.parseNode(true, false) }
func (l *loader) parseFlowNode() *event                 { return l.parseNode(false, false) }
func (l *loader) parseBlockNodeOrIndentlessSeq() *event { return l.parseNode(true, true) }

func (l *loader) parseNode(block, indentlessSequence bool) *event {
	if l.checkToken(tAlias) {
		t := l.getToken()
		v := t.value
		l.state = l.pop()
		return &event{kind: eAlias, anchor: &v, start: t.start, end: t.end}
	}
	var anchor, tag *string
	var start, end, tagMark *mark
	var handle *string
	var suffix string
	hasTag := false
	if l.checkToken(tAnchor) {
		t := l.getToken()
		start, end = t.start, t.end
		v := t.value
		anchor = &v
		if l.checkToken(tTag) {
			t = l.getToken()
			tagMark, end = t.start, t.end
			handle, suffix, hasTag = t.handle, t.suffix, true
		}
	} else if l.checkToken(tTag) {
		t := l.getToken()
		start, tagMark, end = t.start, t.start, t.end
		handle, suffix, hasTag = t.handle, t.suffix, true
		if l.checkToken(tAnchor) {
			t = l.getToken()
			end = t.end
			v := t.value
			anchor = &v
		}
	}
	if hasTag {
		var full string
		if handle != nil {
			pre, ok := l.tagHandles[*handle]
			if !ok {
				raise(parseErr, "while parsing a node", start, "found undefined tag handle "+pystr.Repr(*handle), tagMark)
			}
			full = pre + suffix
		} else {
			full = suffix
		}
		tag = &full
	}
	if start == nil {
		start = l.peekToken().start
		end = start
	}
	implicit := tag == nil || *tag == "!"
	if indentlessSequence && l.checkToken(tBlockEntry) {
		end = l.peekToken().end
		l.state = l.parseIndentlessSequenceEntry
		return &event{kind: eSeqStart, anchor: anchor, tag: tag, implicit0: implicit, start: start, end: end}
	}
	if l.checkToken(tScalar) {
		t := l.getToken()
		end = t.end
		var i0, i1 bool
		switch {
		case (t.plain && tag == nil) || (tag != nil && *tag == "!"):
			i0, i1 = true, false
		case tag == nil:
			i0, i1 = false, true
		}
		l.state = l.pop()
		return &event{kind: eScalar, anchor: anchor, tag: tag, implicit0: i0, implicit1: i1, value: t.value,
			start: start, end: end}
	}
	switch {
	case l.checkToken(tFlowSeqStart):
		end = l.peekToken().end
		l.state = l.parseFlowSequenceFirstEntry
		return &event{kind: eSeqStart, anchor: anchor, tag: tag, implicit0: implicit, start: start, end: end}
	case l.checkToken(tFlowMapStart):
		end = l.peekToken().end
		l.state = l.parseFlowMappingFirstKey
		return &event{kind: eMapStart, anchor: anchor, tag: tag, implicit0: implicit, start: start, end: end}
	case block && l.checkToken(tBlockSeqStart):
		end = l.peekToken().start
		l.state = l.parseBlockSequenceFirstEntry
		return &event{kind: eSeqStart, anchor: anchor, tag: tag, implicit0: implicit, start: start, end: end}
	case block && l.checkToken(tBlockMapStart):
		end = l.peekToken().start
		l.state = l.parseBlockMappingFirstKey
		return &event{kind: eMapStart, anchor: anchor, tag: tag, implicit0: implicit, start: start, end: end}
	case anchor != nil || tag != nil:
		l.state = l.pop()
		return &event{kind: eScalar, anchor: anchor, tag: tag, implicit0: implicit, implicit1: false, value: "",
			start: start, end: end}
	}
	node := "flow"
	if block {
		node = "block"
	}
	t := l.peekToken()
	raise(parseErr, "while parsing a "+node+" node", start,
		"expected the node content, but found "+pystr.Repr(tokID[t.kind]), t.start)
	return nil
}

func (l *loader) parseBlockSequenceFirstEntry() *event {
	t := l.getToken()
	l.marks = append(l.marks, t.start)
	return l.parseBlockSequenceEntry()
}

func (l *loader) parseBlockSequenceEntry() *event {
	if l.checkToken(tBlockEntry) {
		t := l.getToken()
		if !l.checkToken(tBlockEntry, tBlockEnd) {
			l.states = append(l.states, l.parseBlockSequenceEntry)
			return l.parseBlockNode()
		}
		l.state = l.parseBlockSequenceEntry
		return l.emptyScalar(t.end)
	}
	if !l.checkToken(tBlockEnd) {
		t := l.peekToken()
		raise(parseErr, "while parsing a block collection", l.marks[len(l.marks)-1],
			"expected <block end>, but found "+pystr.Repr(tokID[t.kind]), t.start)
	}
	t := l.getToken()
	l.state = l.pop()
	l.popMark()
	return &event{kind: eSeqEnd, start: t.start, end: t.end}
}

func (l *loader) parseIndentlessSequenceEntry() *event {
	if l.checkToken(tBlockEntry) {
		t := l.getToken()
		if !l.checkToken(tBlockEntry, tKey, tValue, tBlockEnd) {
			l.states = append(l.states, l.parseIndentlessSequenceEntry)
			return l.parseBlockNode()
		}
		l.state = l.parseIndentlessSequenceEntry
		return l.emptyScalar(t.end)
	}
	t := l.peekToken()
	l.state = l.pop()
	return &event{kind: eSeqEnd, start: t.start, end: t.start}
}

func (l *loader) parseBlockMappingFirstKey() *event {
	t := l.getToken()
	l.marks = append(l.marks, t.start)
	return l.parseBlockMappingKey()
}

func (l *loader) parseBlockMappingKey() *event {
	if l.checkToken(tKey) {
		t := l.getToken()
		if !l.checkToken(tKey, tValue, tBlockEnd) {
			l.states = append(l.states, l.parseBlockMappingValue)
			return l.parseBlockNodeOrIndentlessSeq()
		}
		l.state = l.parseBlockMappingValue
		return l.emptyScalar(t.end)
	}
	if !l.checkToken(tBlockEnd) {
		t := l.peekToken()
		raise(parseErr, "while parsing a block mapping", l.marks[len(l.marks)-1],
			"expected <block end>, but found "+pystr.Repr(tokID[t.kind]), t.start)
	}
	t := l.getToken()
	l.state = l.pop()
	l.popMark()
	return &event{kind: eMapEnd, start: t.start, end: t.end}
}

func (l *loader) parseBlockMappingValue() *event {
	if l.checkToken(tValue) {
		t := l.getToken()
		if !l.checkToken(tKey, tValue, tBlockEnd) {
			l.states = append(l.states, l.parseBlockMappingKey)
			return l.parseBlockNodeOrIndentlessSeq()
		}
		l.state = l.parseBlockMappingKey
		return l.emptyScalar(t.end)
	}
	l.state = l.parseBlockMappingKey
	return l.emptyScalar(l.peekToken().start)
}

func (l *loader) parseFlowSequenceFirstEntry() *event {
	t := l.getToken()
	l.marks = append(l.marks, t.start)
	return l.parseFlowSequenceEntryAt(true)
}

func (l *loader) parseFlowSequenceEntry() *event { return l.parseFlowSequenceEntryAt(false) }

func (l *loader) parseFlowSequenceEntryAt(first bool) *event {
	if !l.checkToken(tFlowSeqEnd) {
		if !first {
			if l.checkToken(tFlowEntry) {
				l.getToken()
			} else {
				t := l.peekToken()
				raise(parseErr, "while parsing a flow sequence", l.marks[len(l.marks)-1],
					"expected ',' or ']', but got "+pystr.Repr(tokID[t.kind]), t.start)
			}
		}
		if l.checkToken(tKey) {
			t := l.peekToken()
			l.state = l.parseFlowSequenceEntryMappingKey
			return &event{kind: eMapStart, implicit0: true, start: t.start, end: t.end}
		} else if !l.checkToken(tFlowSeqEnd) {
			l.states = append(l.states, l.parseFlowSequenceEntry)
			return l.parseFlowNode()
		}
	}
	t := l.getToken()
	l.state = l.pop()
	l.popMark()
	return &event{kind: eSeqEnd, start: t.start, end: t.end}
}

func (l *loader) parseFlowSequenceEntryMappingKey() *event {
	t := l.getToken()
	if !l.checkToken(tValue, tFlowEntry, tFlowSeqEnd) {
		l.states = append(l.states, l.parseFlowSequenceEntryMappingValue)
		return l.parseFlowNode()
	}
	l.state = l.parseFlowSequenceEntryMappingValue
	return l.emptyScalar(t.end)
}

func (l *loader) parseFlowSequenceEntryMappingValue() *event {
	if l.checkToken(tValue) {
		t := l.getToken()
		if !l.checkToken(tFlowEntry, tFlowSeqEnd) {
			l.states = append(l.states, l.parseFlowSequenceEntryMappingEnd)
			return l.parseFlowNode()
		}
		l.state = l.parseFlowSequenceEntryMappingEnd
		return l.emptyScalar(t.end)
	}
	l.state = l.parseFlowSequenceEntryMappingEnd
	return l.emptyScalar(l.peekToken().start)
}

func (l *loader) parseFlowSequenceEntryMappingEnd() *event {
	l.state = l.parseFlowSequenceEntry
	t := l.peekToken()
	return &event{kind: eMapEnd, start: t.start, end: t.start}
}

func (l *loader) parseFlowMappingFirstKey() *event {
	t := l.getToken()
	l.marks = append(l.marks, t.start)
	return l.parseFlowMappingKeyAt(true)
}

func (l *loader) parseFlowMappingKey() *event { return l.parseFlowMappingKeyAt(false) }

func (l *loader) parseFlowMappingKeyAt(first bool) *event {
	if !l.checkToken(tFlowMapEnd) {
		if !first {
			if l.checkToken(tFlowEntry) {
				l.getToken()
			} else {
				t := l.peekToken()
				raise(parseErr, "while parsing a flow mapping", l.marks[len(l.marks)-1],
					"expected ',' or '}', but got "+pystr.Repr(tokID[t.kind]), t.start)
			}
		}
		if l.checkToken(tKey) {
			t := l.getToken()
			if !l.checkToken(tValue, tFlowEntry, tFlowMapEnd) {
				l.states = append(l.states, l.parseFlowMappingValue)
				return l.parseFlowNode()
			}
			l.state = l.parseFlowMappingValue
			return l.emptyScalar(t.end)
		} else if !l.checkToken(tFlowMapEnd) {
			l.states = append(l.states, l.parseFlowMappingEmptyValue)
			return l.parseFlowNode()
		}
	}
	t := l.getToken()
	l.state = l.pop()
	l.popMark()
	return &event{kind: eMapEnd, start: t.start, end: t.end}
}

func (l *loader) parseFlowMappingValue() *event {
	if l.checkToken(tValue) {
		t := l.getToken()
		if !l.checkToken(tFlowEntry, tFlowMapEnd) {
			l.states = append(l.states, l.parseFlowMappingKey)
			return l.parseFlowNode()
		}
		l.state = l.parseFlowMappingKey
		return l.emptyScalar(t.end)
	}
	l.state = l.parseFlowMappingKey
	return l.emptyScalar(l.peekToken().start)
}

func (l *loader) parseFlowMappingEmptyValue() *event {
	l.state = l.parseFlowMappingKey
	return l.emptyScalar(l.peekToken().start)
}

func (l *loader) emptyScalar(m *mark) *event {
	return &event{kind: eScalar, implicit0: true, implicit1: false, value: "", start: m, end: m}
}

// ---------------------------------------------------------------------------
// The composer (yaml/composer.py) and resolver (yaml/resolver.py)
// ---------------------------------------------------------------------------

type nodeKind int

const (
	nScalar nodeKind = iota
	nSeq
	nMap
)

var nodeID = [...]string{"scalar", "sequence", "mapping"}

type pair struct{ k, v *node }

type node struct {
	kind  nodeKind
	tag   string
	value string
	items []*node
	pairs []pair
	start *mark
}

func (l *loader) getSingleNode() *node {
	l.getEvent()
	var doc *node
	if !l.checkEvent(eStreamEnd) {
		doc = l.composeDocument()
	}
	if !l.checkEvent(eStreamEnd) {
		ev := l.getEvent()
		raise(composeErr, "expected a single document in the stream", doc.start, "but found another document", ev.start)
	}
	l.getEvent()
	return doc
}

func (l *loader) composeDocument() *node {
	l.getEvent()
	n := l.composeNode()
	l.getEvent()
	l.anchors = map[string]*node{}
	return n
}

func (l *loader) composeNode() *node {
	l.depth++
	if l.depth > maxDepth {
		unsupported("nesting deep enough to reach Python's recursion limit")
	}
	defer func() { l.depth-- }()
	if l.checkEvent(eAlias) {
		ev := l.getEvent()
		n, ok := l.anchors[*ev.anchor]
		if !ok {
			raise(composeErr, "", nil, "found undefined alias "+pystr.Repr(*ev.anchor), ev.start)
		}
		if l.composing[n] {
			l.refuseLater("a node that holds itself")
		}
		return n
	}
	ev := l.peekEvent()
	if ev.anchor != nil {
		if first, dup := l.anchors[*ev.anchor]; dup {
			raise(composeErr, "found duplicate anchor "+pystr.Repr(*ev.anchor)+"; first occurrence", first.start,
				"second occurrence", ev.start)
		}
	}
	switch {
	case l.checkEvent(eScalar):
		ev = l.getEvent()
		tag := ""
		if ev.tag == nil || *ev.tag == "!" {
			tag = resolveNodeTag(nScalar, ev.value, ev.implicit0)
		} else {
			tag = *ev.tag
		}
		n := &node{kind: nScalar, tag: tag, value: ev.value, start: ev.start}
		if ev.anchor != nil {
			l.anchors[*ev.anchor] = n
		}
		return n
	case l.checkEvent(eSeqStart):
		ev = l.getEvent()
		tag := "tag:yaml.org,2002:seq"
		if ev.tag != nil && *ev.tag != "!" {
			tag = *ev.tag
		}
		n := &node{kind: nSeq, tag: tag, start: ev.start}
		if ev.anchor != nil {
			l.anchors[*ev.anchor] = n
		}
		l.composing[n] = true
		for !l.checkEvent(eSeqEnd) {
			n.items = append(n.items, l.composeNode())
		}
		delete(l.composing, n)
		l.getEvent()
		return n
	default:
		ev = l.getEvent()
		tag := "tag:yaml.org,2002:map"
		if ev.tag != nil && *ev.tag != "!" {
			tag = *ev.tag
		}
		n := &node{kind: nMap, tag: tag, start: ev.start}
		if ev.anchor != nil {
			l.anchors[*ev.anchor] = n
		}
		l.composing[n] = true
		for !l.checkEvent(eMapEnd) {
			k := l.composeNode()
			v := l.composeNode()
			n.pairs = append(n.pairs, pair{k, v})
		}
		delete(l.composing, n)
		l.getEvent()
		return n
	}
}

// resolveNodeTag is Resolver.resolve for a scalar.
func resolveNodeTag(kind nodeKind, value string, implicit bool) string {
	if kind == nScalar && implicit {
		if strings.HasSuffix(value, "\n") {
			// Python's $ also matches before a last line break.
			unsupported("a resolved scalar that ends in a line break")
		}
		var first rune
		if value != "" {
			first, _ = utf8.DecodeRuneInString(value)
		}
		for _, r := range implicitResolvers {
			if value == "" {
				if !strings.Contains(r.firsts, "") || !r.empty {
					continue
				}
			} else if !strings.ContainsRune(r.firsts, first) {
				continue
			}
			if r.match(value) {
				return r.tag
			}
		}
	}
	return "tag:yaml.org,2002:str"
}

type implicitResolver struct {
	tag    string
	firsts string
	empty  bool // registered for the empty first character
	match  func(string) bool
}

var implicitResolvers = []implicitResolver{
	{"tag:yaml.org,2002:bool", "yYnNtTfFoO", false, func(s string) bool { return boolRe().MatchString(s) }},
	{"tag:yaml.org,2002:float", "-+0123456789.", false, func(s string) bool { return floatRe().MatchString(s) }},
	{"tag:yaml.org,2002:int", "-+0123456789", false, func(s string) bool { return intRe().MatchString(s) }},
	{"tag:yaml.org,2002:merge", "<", false, func(s string) bool { return s == "<<" }},
	{"tag:yaml.org,2002:null", "~nN", true, func(s string) bool { return nullRe().MatchString(s) }},
	{"tag:yaml.org,2002:timestamp", "0123456789", false, func(s string) bool { return timeRe().MatchString(s) }},
	{"tag:yaml.org,2002:value", "=", false, func(s string) bool { return s == "=" }},
	{"tag:yaml.org,2002:yaml", "!&*", false, func(s string) bool { return s == "!" || s == "&" || s == "*" }},
}

// ---------------------------------------------------------------------------
// The constructor (yaml/constructor.py, SafeConstructor)
// ---------------------------------------------------------------------------

// seqVal and mapVal are the list and dict a collection node constructs
// to, filled in later as Python's generators fill them.
type seqVal struct{ items []any }

type mapVal struct {
	keys []any
	vals []any
}

type constructor struct {
	built      map[*node]any
	inProgress map[*node]bool
	pending    []func()
	l          *loader
}

// refuseLater notes a value the result model does not hold; see unmodeled.
func (l *loader) refuseLater(why string) {
	if l.unmodeled == "" {
		l.unmodeled = why
	}
}

// otherVal is a value the result model does not hold (a set, an ordered
// map, pairs). Like a list or a set, it is not hashable.
type otherVal struct{}

const (
	tagNull      = "tag:yaml.org,2002:null"
	tagBool      = "tag:yaml.org,2002:bool"
	tagInt       = "tag:yaml.org,2002:int"
	tagFloat     = "tag:yaml.org,2002:float"
	tagStr       = "tag:yaml.org,2002:str"
	tagSeq       = "tag:yaml.org,2002:seq"
	tagMap       = "tag:yaml.org,2002:map"
	tagTimestamp = "tag:yaml.org,2002:timestamp"
	tagMerge     = "tag:yaml.org,2002:merge"
	tagValue     = "tag:yaml.org,2002:value"
)

func (c *constructor) document(n *node) any {
	data := c.object(n)
	for len(c.pending) > 0 {
		run := c.pending
		c.pending = nil
		for _, f := range run {
			f()
		}
	}
	return data
}

func (c *constructor) object(n *node) any {
	if v, ok := c.built[n]; ok {
		return v
	}
	if c.inProgress[n] {
		raise(constructEr, "", nil, "found unconstructable recursive node", n.start)
	}
	c.inProgress[n] = true
	var data any
	switch n.tag {
	case tagNull:
		c.scalar(n)
		data = nil
	case tagBool:
		v := c.scalar(n)
		switch pystr.Lower(v) {
		case "yes", "true", "on":
			data = true
		case "no", "false", "off":
			data = false
		default:
			// self.bool_values[value.lower()] raises KeyError.
			panic(pystr.NewException("KeyError", pystr.Repr(pystr.Lower(v))))
		}
	case tagInt:
		v := c.scalar(n)
		if !intRe().MatchString(v) || strings.HasSuffix(v, "\n") {
			if strings.ReplaceAll(v, "_", "") == "" {
				panic(pystr.NewException("IndexError", "string index out of range"))
			}
			unsupported("an !!int value Python's int() may read another way")
		}
		data = yamlInt(v)
	case tagFloat:
		v := c.scalar(n)
		if !floatRe().MatchString(v) || strings.HasSuffix(v, "\n") {
			floatError(v)
		}
		data = yamlFloat(v)
	case tagTimestamp:
		c.scalar(n)
		if n.kind != nScalar {
			unsupported("a !!timestamp on a collection")
		}
		data = timestamp(n.value)
	case tagStr:
		data = c.scalar(n)
	case tagSeq:
		s := &seqVal{}
		data = s
		c.pending = append(c.pending, func() {
			if n.kind != nSeq {
				raise(constructEr, "", nil, "expected a sequence node, but found "+nodeID[n.kind], n.start)
			}
			for _, child := range n.items {
				s.items = append(s.items, c.object(child))
			}
		})
	case tagMap:
		m := &mapVal{}
		data = m
		c.pending = append(c.pending, func() { c.mapping(n, m) })
	case "tag:yaml.org,2002:binary":
		// base64.decodebytes: its errors are not modeled.
		unsupported("a " + n.tag + " value")
	case "tag:yaml.org,2002:set":
		data = &otherVal{}
		c.pending = append(c.pending, func() {
			c.mapping(n, &mapVal{})
			c.l.refuseLater("a " + n.tag + " value")
		})
	case "tag:yaml.org,2002:omap", "tag:yaml.org,2002:pairs":
		data = &otherVal{}
		context := "while constructing an ordered map"
		if n.tag == "tag:yaml.org,2002:pairs" {
			context = "while constructing pairs"
		}
		c.pending = append(c.pending, func() {
			if n.kind != nSeq {
				raise(constructEr, context, n.start, "expected a sequence, but found "+nodeID[n.kind], n.start)
			}
			for _, sub := range n.items {
				if sub.kind != nMap {
					raise(constructEr, context, n.start, "expected a mapping of length 1, but found "+nodeID[sub.kind], sub.start)
				}
				if len(sub.pairs) != 1 {
					raise(constructEr, context, n.start,
						fmt.Sprintf("expected a single mapping item, but found %d items", len(sub.pairs)), sub.start)
				}
				c.object(sub.pairs[0].k)
				c.object(sub.pairs[0].v)
			}
			c.l.refuseLater("a " + n.tag + " value")
		})
	default:
		raise(constructEr, "", nil, "could not determine a constructor for the tag "+pystr.Repr(n.tag), n.start)
	}
	c.built[n] = data
	delete(c.inProgress, n)
	return data
}

// scalar is SafeConstructor.construct_scalar.
func (c *constructor) scalar(n *node) string {
	if n.kind == nMap {
		for _, p := range n.pairs {
			if p.k.tag == tagValue {
				return c.scalar(p.v)
			}
		}
	}
	if n.kind != nScalar {
		raise(constructEr, "", nil, "expected a scalar node, but found "+nodeID[n.kind], n.start)
	}
	return n.value
}

// flatten is SafeConstructor.flatten_mapping.
func (c *constructor) flatten(n *node) {
	var merge []pair
	i := 0
	for i < len(n.pairs) {
		k, v := n.pairs[i].k, n.pairs[i].v
		switch k.tag {
		case tagMerge:
			n.pairs = append(n.pairs[:i:i], n.pairs[i+1:]...)
			switch v.kind {
			case nMap:
				c.flatten(v)
				merge = append(merge, v.pairs...)
			case nSeq:
				var sub [][]pair
				for _, s := range v.items {
					if s.kind != nMap {
						raise(constructEr, "while constructing a mapping", n.start,
							"expected a mapping for merging, but found "+nodeID[s.kind], s.start)
					}
					c.flatten(s)
					sub = append(sub, s.pairs)
				}
				for j := len(sub) - 1; j >= 0; j-- {
					merge = append(merge, sub[j]...)
				}
			default:
				raise(constructEr, "while constructing a mapping", n.start,
					"expected a mapping or list of mappings for merging, but found "+nodeID[v.kind], v.start)
			}
		case tagValue:
			k.tag = tagStr
			i++
		default:
			i++
		}
	}
	if len(merge) > 0 {
		n.pairs = append(merge, n.pairs...)
	}
}

func (c *constructor) mapping(n *node, m *mapVal) {
	if n.kind == nMap {
		c.flatten(n)
	}
	if n.kind != nMap {
		raise(constructEr, "", nil, "expected a mapping node, but found "+nodeID[n.kind], n.start)
	}
	var keys, vals []any
	for _, p := range n.pairs {
		key := c.object(p.k)
		switch key.(type) {
		case *seqVal, *mapVal, *otherVal:
			raise(constructEr, "while constructing a mapping", n.start, "found unhashable key", p.k.start)
		}
		keys = append(keys, key)
		vals = append(vals, c.object(p.v))
	}
	// mapping = {...}; data.update(mapping): a repeated key keeps its first
	// place and takes the last value.
	for i, k := range keys {
		m.keys = append(m.keys, k)
		m.vals = append(m.vals, vals[i])
	}
}

// ---------------------------------------------------------------------------
// From the constructed values to the result model
// ---------------------------------------------------------------------------

func finish(v any, depth int) any {
	if depth > maxDepth {
		unsupported("nesting deep enough to reach Python's recursion limit")
	}
	switch x := v.(type) {
	case *seqVal:
		out := make([]any, len(x.items))
		for i, e := range x.items {
			out[i] = finish(e, depth+1)
		}
		return out
	case *mapVal:
		o := pyjson.NewObject()
		for i, k := range x.keys {
			o.Set(dictKey(k), finish(x.vals[i], depth+1))
		}
		return o
	}
	return v
}

// dictKey is the key a value makes in a dict. A key that is not a str is
// marked, so it never equals a field name; True, 1 and 1.0 are one key in
// Python, and the marks keep that for bool and int.
func dictKey(k any) string {
	switch x := k.(type) {
	case string:
		return x
	case nil:
		return "\x00None"
	case bool:
		if x {
			return "\x00True"
		}
		return "\x00False"
	case pyjson.Int:
		switch x.Text {
		case "1":
			return "\x00True"
		case "0":
			return "\x00False"
		}
		return "\x00int:" + x.Text
	case pyjson.Float:
		unsupported("a float key")
	case Timestamp:
		unsupported("a timestamp key")
	}
	unsupported("a key of another type")
	return ""
}

// loadFull is yaml.safe_load(text) for any text.
func loadFull(text string) any {
	buf := []rune(text)
	buf = append(buf, 0)
	l := &loader{
		buf:         buf,
		indent:      -1,
		allowSimple: true,
		possible:    map[int]*possibleKey{},
		anchors:     map[string]*node{},
		composing:   map[*node]bool{},
	}
	m := l.mark()
	l.tokens = append(l.tokens, &token{kind: tStreamStart, start: m, end: m})
	l.state = l.parseStreamStart
	n := l.getSingleNode()
	if n == nil {
		return nil
	}
	c := &constructor{built: map[*node]any{}, inProgress: map[*node]bool{}, l: l}
	data := c.document(n)
	if l.unmodeled != "" {
		unsupported(l.unmodeled)
	}
	return finish(data, 0)
}

// pyFloatRe is what Python's float() reads from lower-case ASCII text with
// no white space.
var pyFloatRe = lazyre.New(`^[-+]?(?:(?:[0-9]+\.?[0-9]*|\.[0-9]+)(?:e[-+]?[0-9]+)?|inf|infinity|nan)$`)

// floatError is construct_yaml_float on text the resolver's float pattern
// does not take: the error it raises, or a refusal where float() may read
// the text.
func floatError(v string) {
	s := pystr.Lower(strings.ReplaceAll(v, "_", ""))
	if s == "" {
		panic(pystr.NewException("IndexError", "string index out of range"))
	}
	if s[0] == '-' || s[0] == '+' {
		s = s[1:]
	}
	if s == ".inf" || s == ".nan" || strings.Contains(s, ":") || pyFloatRe().MatchString(s) {
		unsupported("a !!float value Python's float() may read another way")
	}
	for _, r := range s {
		if r <= ' ' || r >= 0x7f {
			unsupported("a !!float value Python's float() may read another way")
		}
	}
	panic(pystr.NewException("ValueError", "could not convert string to float: "+pystr.Repr(s)))
}

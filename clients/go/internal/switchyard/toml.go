package switchyard

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"
)

// This file is a translation of Python 3.12's tomllib (_parser.py and
// _re.py, by Taneli Hukkinen, MIT), one function at a time, so a text
// gives the value tomllib.loads gives, or a TOMLDecodeError where it
// raises one. Positions are byte offsets; an error's line and column
// count characters, as Python's do.

// TOMLDecodeError is tomllib.TOMLDecodeError.
type TOMLDecodeError struct{ Msg string }

func (e *TOMLDecodeError) Error() string { return e.Msg }

// TOMLValueError is the ValueError int() raises inside tomllib.loads for
// a decimal int past Python's 4300-digit limit. It is not a
// TOMLDecodeError, so the oracle's readers do not catch it.
type TOMLValueError struct{ Msg string }

func (e *TOMLValueError) Error() string { return e.Msg }

// Table is a TOML table (a Python dict): keys in insertion order.
type Table struct {
	Keys []string
	Vals map[string]any
}

func newTable() *Table { return &Table{Vals: map[string]any{}} }

// Get returns the value of k.
func (t *Table) Get(k string) (any, bool) {
	if t == nil {
		return nil, false
	}
	v, ok := t.Vals[k]
	return v, ok
}

func (t *Table) has(k string) bool { _, ok := t.Vals[k]; return ok }

func (t *Table) put(k string, v any) {
	if _, ok := t.Vals[k]; !ok {
		t.Keys = append(t.Keys, k)
	}
	t.Vals[k] = v
}

// BigInt is a TOML integer past 64 bits: tomllib's int() keeps any size.
type BigInt struct{ Text string }

// DateTime is a TOML date, time or date-time (datetime.date,
// datetime.time or datetime.datetime).
type DateTime struct {
	// Kind is "date", "time" or "datetime".
	Kind                     string
	Year, Month, Day         int
	Hour, Minute, Second, Us int
	// TZ is "" (none), "utc", or "offset" with OffsetMin.
	TZ        string
	OffsetMin int
}

// Repr is repr() of the value.
func (d DateTime) Repr() string {
	hms := func() string {
		s := fmt.Sprintf("%d, %d", d.Hour, d.Minute)
		if d.Second != 0 || d.Us != 0 {
			s += fmt.Sprintf(", %d", d.Second)
		}
		if d.Us != 0 {
			s += fmt.Sprintf(", %d", d.Us)
		}
		return s
	}
	switch d.Kind {
	case "date":
		return fmt.Sprintf("datetime.date(%d, %d, %d)", d.Year, d.Month, d.Day)
	case "time":
		return "datetime.time(" + hms() + ")"
	}
	s := fmt.Sprintf("datetime.datetime(%d, %d, %d, %s", d.Year, d.Month, d.Day, hms())
	switch d.TZ {
	case "utc":
		s += ", tzinfo=datetime.timezone.utc"
	case "offset":
		secs := d.OffsetMin * 60
		days := int(math.Floor(float64(secs) / 86400))
		rest := secs - days*86400
		var td string
		switch {
		case days != 0 && rest != 0:
			td = fmt.Sprintf("days=%d, seconds=%d", days, rest)
		case days != 0:
			td = fmt.Sprintf("days=%d", days)
		default:
			td = fmt.Sprintf("seconds=%d", rest)
		}
		s += ", tzinfo=datetime.timezone(datetime.timedelta(" + td + "))"
	}
	return s + ")"
}

// Str is str() of the value (isoformat, with a space in a date-time).
func (d DateTime) Str() string {
	date := fmt.Sprintf("%04d-%02d-%02d", d.Year, d.Month, d.Day)
	tm := fmt.Sprintf("%02d:%02d:%02d", d.Hour, d.Minute, d.Second)
	if d.Us != 0 {
		tm += fmt.Sprintf(".%06d", d.Us)
	}
	switch d.Kind {
	case "date":
		return date
	case "time":
		return tm
	}
	s := date + " " + tm
	switch d.TZ {
	case "utc":
		s += "+00:00"
	case "offset":
		m := d.OffsetMin
		sign := "+"
		if m < 0 {
			sign, m = "-", -m
		}
		s += fmt.Sprintf("%s%02d:%02d", sign, m/60, m%60)
	}
	return s
}

const (
	flagFrozen       = 0
	flagExplicitNest = 1
)

type flagNode struct {
	flags, recursive map[int]bool
	nested           map[string]*flagNode
}

func newFlagNode() *flagNode {
	return &flagNode{flags: map[int]bool{}, recursive: map[int]bool{}, nested: map[string]*flagNode{}}
}

type pending struct {
	key  string // the key's parts joined by \x00
	flag int
}

// tomlFlags is tomllib's Flags.
type tomlFlags struct {
	root    map[string]*flagNode
	pending []pending
	keys    map[string][]string
}

func newFlags() *tomlFlags {
	return &tomlFlags{root: map[string]*flagNode{}, keys: map[string][]string{}}
}

func joinKey(k []string) string { return strings.Join(k, "\x00") }

func (f *tomlFlags) addPending(key []string, flag int) {
	j := joinKey(key)
	for _, p := range f.pending {
		if p.key == j && p.flag == flag {
			return
		}
	}
	f.pending = append(f.pending, pending{j, flag})
	f.keys[j] = append([]string(nil), key...)
}

func (f *tomlFlags) finalizePending() {
	for _, p := range f.pending {
		f.set(f.keys[p.key], p.flag, false)
	}
	f.pending = nil
}

func (f *tomlFlags) unsetAll(key []string) {
	cont := f.root
	for _, k := range key[:len(key)-1] {
		n, ok := cont[k]
		if !ok {
			return
		}
		cont = n.nested
	}
	delete(cont, key[len(key)-1])
}

func (f *tomlFlags) set(key []string, flag int, recursive bool) {
	cont := f.root
	for _, k := range key[:len(key)-1] {
		if _, ok := cont[k]; !ok {
			cont[k] = newFlagNode()
		}
		cont = cont[k].nested
	}
	stem := key[len(key)-1]
	if _, ok := cont[stem]; !ok {
		cont[stem] = newFlagNode()
	}
	if recursive {
		cont[stem].recursive[flag] = true
	} else {
		cont[stem].flags[flag] = true
	}
}

func (f *tomlFlags) is(key []string, flag int) bool {
	if len(key) == 0 {
		return false
	}
	cont := f.root
	for _, k := range key[:len(key)-1] {
		n, ok := cont[k]
		if !ok {
			return false
		}
		if n.recursive[flag] {
			return true
		}
		cont = n.nested
	}
	if n, ok := cont[key[len(key)-1]]; ok {
		return n.flags[flag] || n.recursive[flag]
	}
	return false
}

// errNoNest is the KeyError of NestedDict.
type errNoNest struct{}

func (errNoNest) Error() string { return "no nest" }

func getOrCreateNest(root *Table, key []string, accessLists bool) (*Table, error) {
	var cont any = root
	for _, k := range key {
		t := cont.(*Table)
		if !t.has(k) {
			t.put(k, newTable())
		}
		cont = t.Vals[k]
		if l, ok := cont.([]any); ok && accessLists {
			cont = l[len(l)-1]
		}
		if _, ok := cont.(*Table); !ok {
			return nil, errNoNest{}
		}
	}
	return cont.(*Table), nil
}

func appendNestToList(root *Table, key []string) error {
	cont, err := getOrCreateNest(root, key[:len(key)-1], true)
	if err != nil {
		return err
	}
	last := key[len(key)-1]
	if v, ok := cont.Vals[last]; ok {
		l, isList := v.([]any)
		if !isList {
			return errNoNest{}
		}
		cont.Vals[last] = append(l, newTable())
		return nil
	}
	cont.put(last, []any{newTable()})
	return nil
}

type tparser struct {
	src string
}

func (p *tparser) at(pos int) (byte, bool) {
	if pos < len(p.src) {
		return p.src[pos], true
	}
	return 0, false
}

func (p *tparser) err(pos int, msg string) error {
	coord := "end of document"
	if pos < len(p.src) {
		line := strings.Count(p.src[:pos], "\n") + 1
		var col int
		if line == 1 {
			col = utf8.RuneCountInString(p.src[:pos]) + 1
		} else {
			col = utf8.RuneCountInString(p.src[strings.LastIndexByte(p.src[:pos], '\n'):pos])
		}
		coord = fmt.Sprintf("line %d, column %d", line, col)
	}
	return &TOMLDecodeError{fmt.Sprintf("%s (at %s)", msg, coord)}
}

func isCtrl(c byte) bool { return c < 32 || c == 127 }

func isBareKeyChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
}

func isTOMLWS(c byte) bool { return c == ' ' || c == '\t' }

func (p *tparser) skipWS(pos int) int {
	for pos < len(p.src) && isTOMLWS(p.src[pos]) {
		pos++
	}
	return pos
}

func (p *tparser) skipWSNL(pos int) int {
	for pos < len(p.src) && (isTOMLWS(p.src[pos]) || p.src[pos] == '\n') {
		pos++
	}
	return pos
}

// pyCharRepr is repr() of the one character at pos.
func (p *tparser) charRepr(pos int) string {
	r, _ := utf8.DecodeRuneInString(p.src[pos:])
	return pyCharRepr(r)
}

func pyCharRepr(r rune) string {
	switch {
	case r == '\'':
		return `"'"`
	case r == '\\':
		return `'\\'`
	case r == '\t':
		return `'\t'`
	case r == '\n':
		return `'\n'`
	case r == '\r':
		return `'\r'`
	case r < 32 || r == 127:
		return fmt.Sprintf(`'\x%02x'`, r)
	}
	return "'" + string(r) + "'"
}

// skipUntil is skip_until. errorOn is the set of bytes that are illegal
// before expect (control characters, here, minus the allowed ones).
func (p *tparser) skipUntil(pos int, expect string, errorOn func(byte) bool, errorOnEOF bool) (int, error) {
	newPos := strings.Index(p.src[pos:], expect)
	if newPos < 0 {
		newPos = len(p.src)
		if errorOnEOF {
			return 0, p.err(newPos, "Expected "+pyStrRepr(expect))
		}
	} else {
		newPos += pos
	}
	for i := pos; i < newPos; i++ {
		if errorOn(p.src[i]) {
			return 0, p.err(i, "Found invalid character "+p.charRepr(i))
		}
	}
	return newPos, nil
}

func pyStrRepr(s string) string {
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		return `"` + s + `"`
	}
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "\n", `\n`) + "'"
}

func illegalComment(c byte) bool   { return isCtrl(c) && c != '\t' }
func illegalBasic(c byte) bool     { return isCtrl(c) && c != '\t' }
func illegalMultiline(c byte) bool { return isCtrl(c) && c != '\t' && c != '\n' }

func (p *tparser) skipComment(pos int) (int, error) {
	if c, ok := p.at(pos); ok && c == '#' {
		return p.skipUntil(pos+1, "\n", illegalComment, false)
	}
	return pos, nil
}

func (p *tparser) skipCommentsAndArrayWS(pos int) (int, error) {
	for {
		before := pos
		pos = p.skipWSNL(pos)
		var err error
		if pos, err = p.skipComment(pos); err != nil {
			return 0, err
		}
		if pos == before {
			return pos, nil
		}
	}
}

// ParseTOML is tomllib.loads(text).
func ParseTOML(text string) (*Table, error) {
	p := &tparser{src: strings.ReplaceAll(text, "\r\n", "\n")}
	src := p.src
	pos := 0
	root := newTable()
	flags := newFlags()
	var header []string
	for {
		pos = p.skipWS(pos)
		c, ok := p.at(pos)
		if !ok {
			break
		}
		if c == '\n' {
			pos++
			continue
		}
		var err error
		switch {
		case isBareKeyChar(c) || c == '"' || c == '\'':
			if pos, err = p.keyValueRule(pos, root, flags, header); err != nil {
				return nil, err
			}
			pos = p.skipWS(pos)
		case c == '[':
			second, _ := p.at(pos + 1)
			flags.finalizePending()
			if second == '[' {
				pos, header, err = p.createListRule(pos, root, flags)
			} else {
				pos, header, err = p.createDictRule(pos, root, flags)
			}
			if err != nil {
				return nil, err
			}
			pos = p.skipWS(pos)
		case c != '#':
			return nil, p.err(pos, "Invalid statement")
		}
		if pos, err = p.skipComment(pos); err != nil {
			return nil, err
		}
		c, ok = p.at(pos)
		if !ok {
			break
		}
		if c != '\n' {
			return nil, p.err(pos, "Expected newline or end of document after a statement")
		}
		pos++
	}
	_ = src
	return root, nil
}

func keyRepr(k []string) string {
	parts := make([]string, len(k))
	for i, s := range k {
		parts[i] = pyStrQuote(s)
	}
	if len(parts) == 1 {
		return "(" + parts[0] + ",)"
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// pyStrQuote is repr() of a str (for an error message).
func pyStrQuote(s string) string {
	q := "'"
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		q = `"`
	}
	var b strings.Builder
	b.WriteString(q)
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case string(r) == q:
			b.WriteString(`\` + q)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 32 || r == 127:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteString(q)
	return b.String()
}

func (p *tparser) createDictRule(pos int, root *Table, flags *tomlFlags) (int, []string, error) {
	pos++
	pos = p.skipWS(pos)
	pos, key, err := p.parseKey(pos)
	if err != nil {
		return 0, nil, err
	}
	if flags.is(key, flagExplicitNest) || flags.is(key, flagFrozen) {
		return 0, nil, p.err(pos, "Cannot declare "+keyRepr(key)+" twice")
	}
	flags.set(key, flagExplicitNest, false)
	if _, err := getOrCreateNest(root, key, true); err != nil {
		return 0, nil, p.err(pos, "Cannot overwrite a value")
	}
	if !strings.HasPrefix(p.src[pos:], "]") {
		return 0, nil, p.err(pos, "Expected ']' at the end of a table declaration")
	}
	return pos + 1, key, nil
}

func (p *tparser) createListRule(pos int, root *Table, flags *tomlFlags) (int, []string, error) {
	pos += 2
	pos = p.skipWS(pos)
	pos, key, err := p.parseKey(pos)
	if err != nil {
		return 0, nil, err
	}
	if flags.is(key, flagFrozen) {
		return 0, nil, p.err(pos, "Cannot mutate immutable namespace "+keyRepr(key))
	}
	flags.unsetAll(key)
	flags.set(key, flagExplicitNest, false)
	if err := appendNestToList(root, key); err != nil {
		return 0, nil, p.err(pos, "Cannot overwrite a value")
	}
	if !strings.HasPrefix(p.src[pos:], "]]") {
		return 0, nil, p.err(pos, "Expected ']]' at the end of an array declaration")
	}
	return pos + 2, key, nil
}

func (p *tparser) keyValueRule(pos int, root *Table, flags *tomlFlags, header []string) (int, error) {
	pos, key, value, err := p.parseKeyValuePair(pos)
	if err != nil {
		return 0, err
	}
	parent, stem := key[:len(key)-1], key[len(key)-1]
	absParent := append(append([]string(nil), header...), parent...)
	for i := 1; i < len(key); i++ {
		contKey := append(append([]string(nil), header...), key[:i]...)
		if flags.is(contKey, flagExplicitNest) {
			return 0, p.err(pos, "Cannot redefine namespace "+keyRepr(contKey))
		}
		flags.addPending(contKey, flagExplicitNest)
	}
	if flags.is(absParent, flagFrozen) {
		return 0, p.err(pos, "Cannot mutate immutable namespace "+keyRepr(absParent))
	}
	nest, err := getOrCreateNest(root, absParent, true)
	if err != nil {
		return 0, p.err(pos, "Cannot overwrite a value")
	}
	if nest.has(stem) {
		return 0, p.err(pos, "Cannot overwrite a value")
	}
	switch value.(type) {
	case *Table, []any:
		flags.set(append(append([]string(nil), header...), key...), flagFrozen, true)
	}
	nest.put(stem, value)
	return pos, nil
}

func (p *tparser) parseKeyValuePair(pos int) (int, []string, any, error) {
	pos, key, err := p.parseKey(pos)
	if err != nil {
		return 0, nil, nil, err
	}
	if c, ok := p.at(pos); !ok || c != '=' {
		return 0, nil, nil, p.err(pos, "Expected '=' after a key in a key/value pair")
	}
	pos++
	pos = p.skipWS(pos)
	pos, value, err := p.parseValue(pos)
	if err != nil {
		return 0, nil, nil, err
	}
	return pos, key, value, nil
}

func (p *tparser) parseKey(pos int) (int, []string, error) {
	pos, part, err := p.parseKeyPart(pos)
	if err != nil {
		return 0, nil, err
	}
	key := []string{part}
	pos = p.skipWS(pos)
	for {
		if c, ok := p.at(pos); !ok || c != '.' {
			return pos, key, nil
		}
		pos++
		pos = p.skipWS(pos)
		if pos, part, err = p.parseKeyPart(pos); err != nil {
			return 0, nil, err
		}
		key = append(key, part)
		pos = p.skipWS(pos)
	}
}

func (p *tparser) parseKeyPart(pos int) (int, string, error) {
	c, ok := p.at(pos)
	switch {
	case ok && isBareKeyChar(c):
		start := pos
		for pos < len(p.src) && isBareKeyChar(p.src[pos]) {
			pos++
		}
		return pos, p.src[start:pos], nil
	case ok && c == '\'':
		return p.parseLiteralStr(pos)
	case ok && c == '"':
		return p.parseBasicStr(pos+1, false)
	}
	return 0, "", p.err(pos, "Invalid initial character for a key part")
}

func (p *tparser) parseArray(pos int) (int, []any, error) {
	pos++
	array := []any{}
	pos, err := p.skipCommentsAndArrayWS(pos)
	if err != nil {
		return 0, nil, err
	}
	if strings.HasPrefix(p.src[pos:], "]") {
		return pos + 1, array, nil
	}
	for {
		var val any
		if pos, val, err = p.parseValue(pos); err != nil {
			return 0, nil, err
		}
		array = append(array, val)
		if pos, err = p.skipCommentsAndArrayWS(pos); err != nil {
			return 0, nil, err
		}
		c, _ := p.at(pos)
		if c == ']' && pos < len(p.src) {
			return pos + 1, array, nil
		}
		if c != ',' || pos >= len(p.src) {
			return 0, nil, p.err(pos, "Unclosed array")
		}
		pos++
		if pos, err = p.skipCommentsAndArrayWS(pos); err != nil {
			return 0, nil, err
		}
		if strings.HasPrefix(p.src[pos:], "]") {
			return pos + 1, array, nil
		}
	}
}

func (p *tparser) parseInlineTable(pos int) (int, *Table, error) {
	pos++
	nested := newTable()
	flags := newFlags()
	pos = p.skipWS(pos)
	if strings.HasPrefix(p.src[pos:], "}") {
		return pos + 1, nested, nil
	}
	for {
		var key []string
		var value any
		var err error
		if pos, key, value, err = p.parseKeyValuePair(pos); err != nil {
			return 0, nil, err
		}
		parent, stem := key[:len(key)-1], key[len(key)-1]
		if flags.is(key, flagFrozen) {
			return 0, nil, p.err(pos, "Cannot mutate immutable namespace "+keyRepr(key))
		}
		nest, err := getOrCreateNest(nested, parent, false)
		if err != nil {
			return 0, nil, p.err(pos, "Cannot overwrite a value")
		}
		if nest.has(stem) {
			return 0, nil, p.err(pos, "Duplicate inline table key "+pyStrQuote(stem))
		}
		nest.put(stem, value)
		pos = p.skipWS(pos)
		c, _ := p.at(pos)
		if c == '}' && pos < len(p.src) {
			return pos + 1, nested, nil
		}
		if c != ',' || pos >= len(p.src) {
			return 0, nil, p.err(pos, "Unclosed inline table")
		}
		switch value.(type) {
		case *Table, []any:
			flags.set(key, flagFrozen, true)
		}
		pos++
		pos = p.skipWS(pos)
	}
}

var basicEscapes = map[string]string{
	`\b`: "\b", `\t`: "\t", `\n`: "\n", `\f`: "\f", `\r`: "\r", `\"`: `"`, `\\`: `\`,
}

func (p *tparser) parseBasicStrEscape(pos int, multiline bool) (int, string, error) {
	end := min(pos+2, len(p.src))
	id := p.src[pos:end]
	pos += 2
	if multiline && (id == `\ ` || id == "\\\t" || id == "\\\n") {
		if id != "\\\n" {
			pos = p.skipWS(pos)
			c, ok := p.at(pos)
			if !ok {
				return pos, "", nil
			}
			if c != '\n' {
				return 0, "", p.err(pos, `Unescaped '\' in a string`)
			}
			pos++
		}
		pos = p.skipWSNL(pos)
		return pos, "", nil
	}
	if id == `\u` {
		return p.parseHexChar(pos, 4)
	}
	if id == `\U` {
		return p.parseHexChar(pos, 8)
	}
	if r, ok := basicEscapes[id]; ok {
		return pos, r, nil
	}
	return 0, "", p.err(pos, `Unescaped '\' in a string`)
}

func (p *tparser) parseHexChar(pos, n int) (int, string, error) {
	// src[pos:pos+n] counts characters; a non-ASCII one is never a hex
	// digit, so bytes give the same answer.
	end := min(pos+n, len(p.src))
	hex := p.src[pos:end]
	if len(hex) != n {
		return 0, "", p.err(pos, "Invalid hex value")
	}
	for i := 0; i < len(hex); i++ {
		c := hex[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return 0, "", p.err(pos, "Invalid hex value")
		}
	}
	pos += n
	v, _ := strconv.ParseUint(hex, 16, 64)
	if !(v <= 55295 || v >= 57344 && v <= 1114111) {
		return 0, "", p.err(pos, "Escaped character is not a Unicode scalar value")
	}
	return pos, string(rune(v)), nil
}

func (p *tparser) parseLiteralStr(pos int) (int, string, error) {
	pos++
	start := pos
	pos, err := p.skipUntil(pos, "'", illegalBasic, true)
	if err != nil {
		return 0, "", err
	}
	return pos + 1, p.src[start:pos], nil
}

func (p *tparser) parseMultilineStr(pos int, literal bool) (int, string, error) {
	pos += 3
	if strings.HasPrefix(p.src[pos:], "\n") {
		pos++
	}
	var result, delim string
	if literal {
		delim = "'"
		end, err := p.skipUntil(pos, "'''", illegalMultiline, true)
		if err != nil {
			return 0, "", err
		}
		result = p.src[pos:end]
		pos = end + 3
	} else {
		delim = `"`
		var err error
		if pos, result, err = p.parseBasicStr(pos, true); err != nil {
			return 0, "", err
		}
	}
	if !strings.HasPrefix(p.src[pos:], delim) {
		return pos, result, nil
	}
	pos++
	if !strings.HasPrefix(p.src[pos:], delim) {
		return pos, result + delim, nil
	}
	pos++
	return pos, result + delim + delim, nil
}

func (p *tparser) parseBasicStr(pos int, multiline bool) (int, string, error) {
	errorOn := illegalBasic
	if multiline {
		errorOn = illegalMultiline
	}
	var result strings.Builder
	start := pos
	for {
		c, ok := p.at(pos)
		if !ok {
			return 0, "", p.err(pos, "Unterminated string")
		}
		if c == '"' {
			if !multiline {
				result.WriteString(p.src[start:pos])
				return pos + 1, result.String(), nil
			}
			if strings.HasPrefix(p.src[pos:], `"""`) {
				result.WriteString(p.src[start:pos])
				return pos + 3, result.String(), nil
			}
			pos++
			continue
		}
		if c == '\\' {
			result.WriteString(p.src[start:pos])
			var esc string
			var err error
			if pos, esc, err = p.parseBasicStrEscape(pos, multiline); err != nil {
				return 0, "", err
			}
			result.WriteString(esc)
			start = pos
			continue
		}
		if errorOn(c) {
			return 0, "", p.err(pos, "Illegal character "+p.charRepr(pos))
		}
		pos++
	}
}

func (p *tparser) parseValue(pos int) (int, any, error) {
	c, ok := p.at(pos)
	rest := p.src[pos:]
	if ok && c == '"' {
		if strings.HasPrefix(rest, `"""`) {
			return p.parseMultilineStr(pos, false)
		}
		return p.parseBasicStr(pos+1, false)
	}
	if ok && c == '\'' {
		if strings.HasPrefix(rest, "'''") {
			return p.parseMultilineStr(pos, true)
		}
		return p.parseLiteralStr(pos)
	}
	if ok && c == 't' && strings.HasPrefix(rest, "true") {
		return pos + 4, true, nil
	}
	if ok && c == 'f' && strings.HasPrefix(rest, "false") {
		return pos + 5, false, nil
	}
	if ok && c == '[' {
		return p.parseArray(pos)
	}
	if ok && c == '{' {
		return p.parseInlineTable(pos)
	}
	if end, dt, matched := matchDatetime(rest); matched {
		if !validDate(dt) {
			return 0, nil, p.err(pos, "Invalid date or datetime")
		}
		return pos + end, dt, nil
	}
	if end, dt, matched := matchLocalTime(rest); matched {
		return pos + end, dt, nil
	}
	if end, isFloat, matched := matchNumber(rest); matched {
		text := strings.ReplaceAll(rest[:end], "_", "")
		if isFloat {
			f, _ := strconv.ParseFloat(text, 64) // float() of the match; a value past range is inf
			return pos + end, f, nil
		}
		if digits := strings.TrimLeft(text, "+-"); !strings.HasPrefix(digits, "0x") &&
			!strings.HasPrefix(digits, "0o") && !strings.HasPrefix(digits, "0b") && len(digits) > 4300 {
			// int(text, 0) past Python's 4300-digit limit.
			return 0, nil, &TOMLValueError{fmt.Sprintf("Exceeds the limit (4300 digits) for integer string conversion: "+
				"value has %d digits; use sys.set_int_max_str_digits() to increase the limit", len(digits))}
		}
		return pos + end, parseTOMLInt(text), nil
	}
	if len(rest) >= 3 && (rest[:3] == "inf" || rest[:3] == "nan") {
		return pos + 3, specialFloat(rest[:3]), nil
	}
	if len(rest) >= 4 {
		switch rest[:4] {
		case "-inf", "+inf", "-nan", "+nan":
			return pos + 4, specialFloat(rest[:4]), nil
		}
	}
	return 0, nil, p.err(pos, "Invalid value")
}

func specialFloat(s string) float64 {
	switch s {
	case "inf", "+inf":
		return math.Inf(1)
	case "-inf":
		return math.Inf(-1)
	}
	return math.NaN()
}

// parseTOMLInt is int(text, 0): an int64 when it fits, else a BigInt.
func parseTOMLInt(text string) any {
	neg := strings.HasPrefix(text, "-")
	t := strings.TrimLeft(text, "+-")
	base := 10
	if len(t) > 1 && t[0] == '0' {
		switch t[1] {
		case 'x':
			base, t = 16, t[2:]
		case 'o':
			base, t = 8, t[2:]
		case 'b':
			base, t = 2, t[2:]
		}
	}
	n, _ := new(big.Int).SetString(t, base)
	if neg {
		n.Neg(n)
	}
	if n.IsInt64() {
		return n.Int64()
	}
	return BigInt{Text: n.String()}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func digitsAt(s string, i, n int) bool {
	if i+n > len(s) {
		return false
	}
	for k := i; k < i+n; k++ {
		if !isDigit(s[k]) {
			return false
		}
	}
	return true
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

// matchTime is _TIME_RE_STR at the start of s: the length matched.
func matchTime(s string, dt *DateTime) (int, bool) {
	if !digitsAt(s, 0, 2) || len(s) < 8 || s[2] != ':' || !digitsAt(s, 3, 2) || s[5] != ':' || !digitsAt(s, 6, 2) {
		return 0, false
	}
	h, m, sec := atoi(s[0:2]), atoi(s[3:5]), atoi(s[6:8])
	if h > 23 || m > 59 || sec > 59 {
		return 0, false
	}
	dt.Hour, dt.Minute, dt.Second = h, m, sec
	n := 8
	if n < len(s) && s[n] == '.' && n+1 < len(s) && isDigit(s[n+1]) {
		k := n + 1
		for k < len(s) && isDigit(s[k]) {
			k++
		}
		frac := s[n+1 : k]
		if len(frac) > 6 {
			frac = frac[:6]
		}
		dt.Us = atoi(frac + strings.Repeat("0", 6-len(frac)))
		n = k
	}
	return n, true
}

// matchDatetime is RE_DATETIME.match: a date, then optionally a time and
// an offset.
func matchDatetime(s string) (int, DateTime, bool) {
	var dt DateTime
	if !digitsAt(s, 0, 4) || len(s) < 10 || s[4] != '-' || !digitsAt(s, 5, 2) || s[7] != '-' || !digitsAt(s, 8, 2) {
		return 0, dt, false
	}
	dt.Year, dt.Month, dt.Day = atoi(s[0:4]), atoi(s[5:7]), atoi(s[8:10])
	if dt.Month < 1 || dt.Month > 12 || dt.Day < 1 || dt.Day > 31 {
		return 0, dt, false
	}
	dt.Kind = "date"
	n := 10
	if n < len(s) && (s[n] == 'T' || s[n] == 't' || s[n] == ' ') {
		t := dt
		if k, ok := matchTime(s[n+1:], &t); ok {
			t.Kind = "datetime"
			e := n + 1 + k
			if e < len(s) && (s[e] == 'Z' || s[e] == 'z') {
				t.TZ = "utc"
				e++
			} else if e+6 <= len(s) && (s[e] == '+' || s[e] == '-') && digitsAt(s, e+1, 2) && s[e+3] == ':' && digitsAt(s, e+4, 2) &&
				atoi(s[e+1:e+3]) <= 23 && atoi(s[e+4:e+6]) <= 59 {
				mins := atoi(s[e+1:e+3])*60 + atoi(s[e+4:e+6])
				if s[e] == '-' {
					mins = -mins
				}
				if mins == 0 {
					t.TZ = "utc"
				} else {
					t.TZ, t.OffsetMin = "offset", mins
				}
				e += 6
			}
			return e, t, true
		}
	}
	return n, dt, true
}

func matchLocalTime(s string) (int, DateTime, bool) {
	dt := DateTime{Kind: "time"}
	n, ok := matchTime(s, &dt)
	return n, dt, ok
}

// validDate is whether datetime() takes the date (the day exists in the
// month).
func validDate(dt DateTime) bool {
	days := []int{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}[dt.Month-1]
	y := dt.Year
	if dt.Month == 2 && (y%4 == 0 && (y%100 != 0 || y%400 == 0)) {
		days = 29
	}
	return dt.Year >= 1 && dt.Day <= days
}

// matchNumber is RE_NUMBER.match: the length matched and whether the
// float part is there.
func matchNumber(s string) (int, bool, bool) {
	tail := func(i int, digit func(byte) bool) int {
		// digit (_? digit)*
		for i < len(s) {
			if digit(s[i]) {
				i++
			} else if s[i] == '_' && i+1 < len(s) && digit(s[i+1]) {
				i += 2
			} else {
				break
			}
		}
		return i
	}
	if len(s) >= 3 && s[0] == '0' {
		var digit func(byte) bool
		switch s[1] {
		case 'x':
			digit = func(c byte) bool { return isDigit(c) || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }
		case 'b':
			digit = func(c byte) bool { return c == '0' || c == '1' }
		case 'o':
			digit = func(c byte) bool { return c >= '0' && c <= '7' }
		}
		if digit != nil && digit(s[2]) {
			return tail(3, digit), false, true
		}
	}
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	switch {
	case i < len(s) && s[i] == '0':
		i++
	case i < len(s) && s[i] >= '1' && s[i] <= '9':
		i = tail(i+1, isDigit)
	default:
		return 0, false, false
	}
	floatStart := i
	if i+1 < len(s) && s[i] == '.' && isDigit(s[i+1]) {
		i = tail(i+2, isDigit)
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		if j < len(s) && isDigit(s[j]) {
			i = tail(j+1, isDigit)
		}
	}
	return i, i > floatStart, true
}

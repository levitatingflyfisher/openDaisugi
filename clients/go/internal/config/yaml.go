// Package config reads ~/.opendaisugi/config.yaml the way
// opendaisugi.config.load_config does, for the settings the gate commands
// read, and finds the gate hook mode installed in Claude Code's settings.
//
// Python validates the whole file with pydantic: one bad field anywhere
// makes load_config raise. So this package reads YAML as PyYAML's
// SafeLoader does, inside a subset it models exactly, and reports
// everything else as ErrUnsupported, for the caller to refuse with a
// reason.
package config

import (
	"daisugi-verify/internal/lazyre"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ErrUnsupported marks YAML outside the modeled subset. The caller must
// not guess: it refuses the command and says why.
var ErrUnsupported = errors.New("config.yaml uses YAML this binary does not read yet")

// ErrInvalid marks a file load_config is certain to refuse.
var ErrInvalid = errors.New("config.yaml does not validate")

// Kind is the type PyYAML gives a node.
type Kind int

const (
	Null Kind = iota
	Bool
	Int
	Float
	Str
	Seq
	Map
)

// Value is one YAML node. Keys of a mapping keep their first position; a
// repeated key takes its last value, as the SafeLoader's dict does. Only
// string keys are kept by name; NonStrKeys counts the others.
type Value struct {
	Kind       Kind
	B          bool
	Text       string // an Int's digits, a Float's text, a Str's value
	Items      []Value
	Keys       []string
	Map        map[string]Value
	NonStrKeys int
}

// Doc is a parsed top-level mapping (kept for callers that read one).
type Doc struct {
	Keys []string
	Vals map[string]Value
}

type yline struct {
	indent int
	text   string // without the indent; comments still in
	num    int
}

type yparser struct {
	lines []yline
	i     int
}

func unsupported() error { return ErrUnsupported }

// ParseYAML reads one document. The subset: block mappings and sequences
// at any depth, flow sequences and mappings on one line, plain scalars
// (continued over more-indented lines), single- and double-quoted scalars
// on one line, comments, and one leading "---". Anchors, aliases, tags,
// block scalars, directives, several documents, tabs and complex keys
// are outside it.
func ParseYAML(text string) (Value, error) {
	if !utf8.ValidString(text) || strings.HasPrefix(text, "\xef\xbb\xbf") {
		return Value{}, ErrUnsupported
	}
	for _, r := range text {
		if r == '\t' || r == '\r' || r == 0x7f || (r < 0x20 && r != '\n') || (r >= 0x80 && r <= 0x9f) ||
			r == 0xfeff || r == 0xfffe || r == 0xffff || r == 0x2028 || r == 0x2029 || r == 0x85 {
			return Value{}, ErrUnsupported
		}
	}
	p := &yparser{}
	started := false
	for n, raw := range strings.Split(text, "\n") {
		t := strings.TrimLeft(raw, " ")
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		ind := len(raw) - len(t)
		if ind == 0 && (strings.HasPrefix(t, "---") || strings.HasPrefix(t, "...") || strings.HasPrefix(t, "%")) {
			if !started && (t == "---" || strings.HasPrefix(t, "--- #")) {
				started = true
				continue
			}
			return Value{}, ErrUnsupported
		}
		started = true
		p.lines = append(p.lines, yline{indent: ind, text: strings.TrimRight(t, " "), num: n})
	}
	if len(p.lines) == 0 {
		return Value{Kind: Null}, nil
	}
	if t := p.lines[0].text; t[0] == '[' || t[0] == '{' {
		// A flow document, such as JSON: its line breaks fold to spaces.
		parts := make([]string, len(p.lines))
		for i, l := range p.lines {
			parts[i] = l.text
		}
		v, rest, err := flowNode(strings.Join(parts, " "))
		if err != nil {
			return Value{}, err
		}
		if !onlyComment(rest) {
			return Value{}, ErrUnsupported
		}
		return v, nil
	}
	v, err := p.block(p.lines[0].indent)
	if err != nil {
		return Value{}, err
	}
	if p.i != len(p.lines) {
		return Value{}, ErrUnsupported
	}
	return v, nil
}

// Parse reads a file whose top level must be a mapping (or empty).
func Parse(text string) (*Doc, error) {
	v, err := ParseYAML(text)
	if err != nil {
		return nil, err
	}
	switch v.Kind {
	case Map:
		return &Doc{Keys: v.Keys, Vals: v.Map}, nil
	case Null:
		return &Doc{Vals: map[string]Value{}}, nil
	}
	return nil, &TopLevelError{v}
}

// TopLevelError is a document whose top level is not a mapping.
type TopLevelError struct{ V Value }

func (e *TopLevelError) Error() string { return "config.yaml does not hold a mapping" }

func isSeqLine(t string) bool { return t == "-" || strings.HasPrefix(t, "- ") }

// block parses the node starting at the current line, at indent ind.
func (p *yparser) block(ind int) (Value, error) {
	l := p.lines[p.i]
	if l.indent != ind {
		return Value{}, ErrUnsupported
	}
	if isSeqLine(l.text) {
		return p.sequence(ind)
	}
	if _, _, ok, err := splitKey(l.text); err != nil {
		return Value{}, err
	} else if ok {
		return p.mapping(ind)
	}
	// A scalar node on its own lines.
	p.i++
	return p.scalarWithContinuation(l.text, ind-1)
}

// splitKey finds "key: rest" or "key:" in a block line. ok is false when
// the line is not a mapping entry.
func splitKey(t string) (key Value, rest string, ok bool, err error) {
	if t == "" {
		return
	}
	switch t[0] {
	case '?', '&', '*', '!', '|', '>', '%', '@', '`':
		return Value{}, "", false, ErrUnsupported
	case '\'', '"':
		end, qerr := quotedEnd(t)
		if qerr != nil {
			return Value{}, "", false, qerr
		}
		after := t[end:]
		if after == ":" || strings.HasPrefix(after, ": ") {
			k, kerr := quoted(t[:end])
			if kerr != nil {
				return Value{}, "", false, kerr
			}
			return k, strings.TrimLeft(strings.TrimPrefix(after, ":"), " "), true, nil
		}
		return Value{}, "", false, nil
	case '[', '{':
		return Value{}, "", false, nil
	}
	// A plain key ends at the first ": " or a final ":".
	k := strings.Index(t, ": ")
	if k < 0 && strings.HasSuffix(t, ":") {
		k = len(t) - 1
	}
	if k < 0 {
		return Value{}, "", false, nil
	}
	if c := strings.Index(t, " #"); c >= 0 && c < k {
		return Value{}, "", false, nil
	}
	keyText := strings.TrimRight(t[:k], " ")
	if keyText == "" || strings.ContainsAny(keyText, "[]{},#") || strings.HasPrefix(keyText, "- ") {
		return Value{}, "", false, ErrUnsupported
	}
	key, err = plain(keyText)
	if err != nil {
		return Value{}, "", false, err
	}
	return key, strings.TrimLeft(t[k+1:], " "), true, nil
}

func (p *yparser) mapping(ind int) (Value, error) {
	m := Value{Kind: Map, Map: map[string]Value{}}
	for p.i < len(p.lines) {
		l := p.lines[p.i]
		if l.indent < ind {
			break
		}
		if l.indent > ind {
			return Value{}, ErrUnsupported
		}
		key, rest, ok, err := splitKey(l.text)
		if err != nil {
			return Value{}, err
		}
		if !ok {
			return Value{}, ErrUnsupported
		}
		p.i++
		var v Value
		if rest == "" || strings.HasPrefix(rest, "#") {
			v, err = p.nested(ind, true)
		} else {
			v, err = p.scalarWithContinuation(rest, ind)
		}
		if err != nil {
			return Value{}, err
		}
		if key.Kind != Str {
			if key.Kind == Seq || key.Kind == Map {
				return Value{}, ErrUnsupported
			}
			m.NonStrKeys++
			continue
		}
		if _, dup := m.Map[key.Text]; !dup {
			m.Keys = append(m.Keys, key.Text)
		}
		m.Map[key.Text] = v
	}
	return m, nil
}

// nested is the value of "key:" with nothing after it: a block on the
// more-indented lines below, a sequence at the same indent (mapping values
// only), or null.
func (p *yparser) nested(ind int, inMapping bool) (Value, error) {
	if p.i >= len(p.lines) {
		return Value{Kind: Null}, nil
	}
	l := p.lines[p.i]
	if l.indent > ind {
		return p.block(l.indent)
	}
	if inMapping && l.indent == ind && isSeqLine(l.text) {
		return p.sequence(ind)
	}
	return Value{Kind: Null}, nil
}

func (p *yparser) sequence(ind int) (Value, error) {
	s := Value{Kind: Seq}
	for p.i < len(p.lines) {
		l := p.lines[p.i]
		if l.indent != ind || !isSeqLine(l.text) {
			if l.indent > ind {
				return Value{}, ErrUnsupported
			}
			break
		}
		rest := strings.TrimLeft(strings.TrimPrefix(l.text, "-"), " ")
		if rest == "" || strings.HasPrefix(rest, "#") {
			p.i++
			v, err := p.nested(ind, false)
			if err != nil {
				return Value{}, err
			}
			s.Items = append(s.Items, v)
			continue
		}
		// "- key: v" or "- - x": the item is a block node whose first
		// line starts at the column after "- ".
		col := ind + (len(l.text) - len(rest))
		if _, _, ok, err := splitKey(rest); err != nil {
			return Value{}, err
		} else if ok || isSeqLine(rest) {
			p.lines[p.i] = yline{indent: col, text: rest, num: l.num}
			v, err := p.block(col)
			if err != nil {
				return Value{}, err
			}
			s.Items = append(s.Items, v)
			continue
		}
		p.i++
		v, err := p.scalarWithContinuation(rest, ind)
		if err != nil {
			return Value{}, err
		}
		s.Items = append(s.Items, v)
	}
	return s, nil
}

// scalarWithContinuation reads a value written after "key: " or "- ": a
// flow collection or quoted scalar on this line, or a plain scalar that
// may continue on lines indented more than parent.
func (p *yparser) scalarWithContinuation(t string, parent int) (Value, error) {
	switch t[0] {
	case '[', '{':
		v, rest, err := flowNode(t)
		if err != nil {
			return Value{}, err
		}
		if !onlyComment(rest) {
			return Value{}, ErrUnsupported
		}
		return v, nil
	case '\'', '"':
		end, err := quotedEnd(t)
		if err != nil {
			return Value{}, err
		}
		if !onlyComment(t[end:]) {
			return Value{}, ErrUnsupported
		}
		return quoted(t[:end])
	case '&', '*', '!', '|', '>', '%', '@', '`', '?', ',', ']', '}', '#':
		return Value{}, ErrUnsupported
	}
	text := t
	if c := strings.Index(text, " #"); c >= 0 {
		// A comment ends the scalar; a continuation after it is not read.
		text = strings.TrimRight(text[:c], " ")
		if p.i < len(p.lines) && p.lines[p.i].indent > parent {
			return Value{}, ErrUnsupported
		}
		return plainValue(text)
	}
	parts := []string{text}
	for p.i < len(p.lines) && p.lines[p.i].indent > parent {
		c := p.lines[p.i].text
		if strings.Contains(c, ": ") || strings.HasSuffix(c, ":") || strings.Contains(c, " #") || isSeqLine(c) {
			return Value{}, ErrUnsupported
		}
		parts = append(parts, c)
		p.i++
	}
	joined := strings.Join(parts, " ")
	if len(parts) > 1 {
		// A multi-line plain scalar is always a string.
		if err := plainOK(joined); err != nil {
			return Value{}, err
		}
		return Value{Kind: Str, Text: joined}, nil
	}
	return plainValue(joined)
}

func plainValue(t string) (Value, error) {
	if strings.Contains(t, ": ") || strings.HasSuffix(t, ":") {
		return Value{}, ErrUnsupported // "mapping values are not allowed here"
	}
	return plain(t)
}

func plainOK(t string) error {
	if t == "" {
		return ErrUnsupported
	}
	if strings.ContainsAny(t[:1], "-?:,[]{}#&*!|>'\"%@`") {
		if !(t[0] == '-' && len(t) > 1 && t[1] != ' ') {
			return ErrUnsupported
		}
	}
	return nil
}

var (
	reInt   = lazyre.New(`^(?:[-+]?0b[0-1_]+|[-+]?0[0-7_]+|[-+]?(?:0|[1-9][0-9_]*)|[-+]?0x[0-9a-fA-F_]+|[-+]?[1-9][0-9_]*(?::[0-5]?[0-9])+)$`)
	reFloat = lazyre.New(`^(?:[-+]?(?:[0-9][0-9_]*)\.[0-9_]*(?:[eE][-+][0-9]+)?|\.[0-9][0-9_]*(?:[eE][-+][0-9]+)?|[-+]?[0-9][0-9_]*(?::[0-5]?[0-9])+\.[0-9_]*|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))$`)
	reTime  = lazyre.New(`^(?:[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]|[0-9][0-9][0-9][0-9]-[0-9][0-9]?-[0-9][0-9]?(?:[Tt]|[ \t]+)[0-9][0-9]?:[0-9][0-9]:[0-9][0-9](?:\.[0-9]*)?(?:[ \t]*(?:Z|[-+][0-9][0-9]?(?::[0-9][0-9])?))?)$`)
	reDec   = lazyre.New(`^[-+]?(?:0|[1-9][0-9]*)$`)
	reFlt   = lazyre.New(`^[-+]?[0-9]+\.[0-9]*(?:[eE][-+][0-9]+)?$`)
)

// plain resolves a plain scalar with PyYAML's implicit resolvers. Forms
// it resolves to types this package does not carry (octal, hex,
// sexagesimal, timestamps, merge keys) are refused.
func plain(t string) (Value, error) {
	if err := plainOK(t); err != nil {
		return Value{}, err
	}
	switch t {
	case "~", "null", "Null", "NULL":
		return Value{Kind: Null}, nil
	case "yes", "Yes", "YES", "true", "True", "TRUE", "on", "On", "ON":
		return Value{Kind: Bool, B: true}, nil
	case "no", "No", "NO", "false", "False", "FALSE", "off", "Off", "OFF":
		return Value{Kind: Bool, B: false}, nil
	case "<<", "=":
		return Value{}, ErrUnsupported
	}
	if reInt().MatchString(t) {
		if reDec().MatchString(t) {
			return Value{Kind: Int, Text: strings.TrimPrefix(t, "+")}, nil
		}
		return Value{}, ErrUnsupported
	}
	if reFloat().MatchString(t) {
		if reFlt().MatchString(t) {
			return Value{Kind: Float, Text: t}, nil
		}
		// .nan and .inf, written as strconv.ParseFloat reads them.
		switch strings.TrimLeft(strings.ToLower(t), "+-") {
		case ".nan":
			return Value{Kind: Float, Text: "NaN"}, nil
		case ".inf":
			if strings.HasPrefix(t, "-") {
				return Value{Kind: Float, Text: "-Inf"}, nil
			}
			return Value{Kind: Float, Text: "+Inf"}, nil
		}
		return Value{}, ErrUnsupported
	}
	if reTime().MatchString(t) {
		return Value{}, ErrUnsupported
	}
	return Value{Kind: Str, Text: t}, nil
}

// quotedEnd returns the index after the closing quote of the quoted
// scalar t starts with.
func quotedEnd(t string) (int, error) {
	q := t[0]
	for i := 1; i < len(t); i++ {
		switch {
		case q == '\'' && t[i] == '\'':
			if i+1 < len(t) && t[i+1] == '\'' {
				i++
				continue
			}
			return i + 1, nil
		case q == '"' && t[i] == '\\':
			i++
		case q == '"' && t[i] == '"':
			return i + 1, nil
		}
	}
	// A quoted scalar over several lines is outside the subset.
	return 0, ErrUnsupported
}

// quoted decodes a whole one-line quoted scalar.
func quoted(t string) (Value, error) {
	body := t[1 : len(t)-1]
	if t[0] == '\'' {
		return Value{Kind: Str, Text: strings.ReplaceAll(body, "''", "'")}, nil
	}
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(body) {
			return Value{}, ErrUnsupported
		}
		switch body[i] {
		case '0':
			b.WriteByte(0)
		case 'a':
			b.WriteByte(7)
		case 'b':
			b.WriteByte(8)
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case 'v':
			b.WriteByte(11)
		case 'f':
			b.WriteByte(12)
		case 'r':
			b.WriteByte('\r')
		case 'e':
			b.WriteByte(27)
		case ' ', '"', '\\', '/':
			b.WriteByte(body[i])
		case 'x', 'u', 'U':
			n := map[byte]int{'x': 2, 'u': 4, 'U': 8}[body[i]]
			if i+1+n > len(body) {
				return Value{}, ErrUnsupported
			}
			code, err := strconv.ParseUint(body[i+1:i+1+n], 16, 32)
			if err != nil || (code >= 0xd800 && code <= 0xdfff) || code > 0x10ffff {
				return Value{}, ErrUnsupported
			}
			b.WriteRune(rune(code))
			i += n
		default:
			return Value{}, ErrUnsupported
		}
	}
	return Value{Kind: Str, Text: b.String()}, nil
}

// flowNode reads one flow collection or scalar from the start of t and
// returns what follows it.
func flowNode(t string) (Value, string, error) {
	t = strings.TrimLeft(t, " ")
	if t == "" {
		return Value{}, "", ErrUnsupported
	}
	switch t[0] {
	case '[':
		s := Value{Kind: Seq}
		rest := strings.TrimLeft(t[1:], " ")
		for {
			if strings.HasPrefix(rest, "]") {
				return s, rest[1:], nil
			}
			v, r, err := flowNode(rest)
			if err != nil {
				return Value{}, "", err
			}
			if strings.HasPrefix(strings.TrimLeft(r, " "), ": ") {
				return Value{}, "", ErrUnsupported // a single-pair mapping
			}
			s.Items = append(s.Items, v)
			rest = strings.TrimLeft(r, " ")
			if strings.HasPrefix(rest, ",") {
				rest = strings.TrimLeft(rest[1:], " ")
				continue
			}
			if !strings.HasPrefix(rest, "]") {
				return Value{}, "", ErrUnsupported
			}
		}
	case '{':
		m := Value{Kind: Map, Map: map[string]Value{}}
		rest := strings.TrimLeft(t[1:], " ")
		for {
			if strings.HasPrefix(rest, "}") {
				return m, rest[1:], nil
			}
			k, r, err := flowNode(rest)
			if err != nil {
				return Value{}, "", err
			}
			r = strings.TrimLeft(r, " ")
			if !strings.HasPrefix(r, ":") {
				return Value{}, "", ErrUnsupported
			}
			v, r2, err := flowNode(r[1:])
			if err != nil {
				return Value{}, "", err
			}
			switch k.Kind {
			case Str:
				if _, dup := m.Map[k.Text]; !dup {
					m.Keys = append(m.Keys, k.Text)
				}
				m.Map[k.Text] = v
			case Seq, Map:
				return Value{}, "", ErrUnsupported
			default:
				m.NonStrKeys++
			}
			rest = strings.TrimLeft(r2, " ")
			if strings.HasPrefix(rest, ",") {
				rest = strings.TrimLeft(rest[1:], " ")
				continue
			}
			if !strings.HasPrefix(rest, "}") {
				return Value{}, "", ErrUnsupported
			}
		}
	case '\'', '"':
		end, err := quotedEnd(t)
		if err != nil {
			return Value{}, "", err
		}
		v, err := quoted(t[:end])
		return v, t[end:], err
	}
	// A plain scalar in flow context ends at , [ ] { } or ": ".
	end := len(t)
	for i := 0; i < len(t); i++ {
		c := t[i]
		if strings.IndexByte(",[]{}", c) >= 0 || (c == ':' && (i+1 == len(t) || t[i+1] == ' ' || strings.IndexByte(",[]{}", t[i+1]) >= 0)) ||
			(c == '#' && i > 0 && t[i-1] == ' ') {
			end = i
			break
		}
	}
	text := strings.TrimRight(t[:end], " ")
	if text == "" {
		return Value{}, "", ErrUnsupported
	}
	v, err := plain(text)
	return v, t[end:], err
}

func onlyComment(s string) bool {
	t := strings.TrimLeft(s, " ")
	return t == "" || (strings.HasPrefix(t, "#") && len(t) < len(s))
}

package switchyard

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// This file reads the part of TOML a Switchyard deployment file uses:
// comments, [table] and [a.b] headers, bare, quoted and dotted keys, and
// values that are basic or literal strings, integers, floats, booleans,
// one-line arrays and inline tables. Anything else (multi-line strings,
// dates, arrays of tables) is refused, never guessed at: ErrTOML says
// the file is outside this reader.

// ErrTOML marks a file this reader does not read.
var ErrTOML = errors.New("not TOML this binary reads")

// Table is a TOML table: keys in file order.
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

func (t *Table) set(k string, v any) error {
	if _, dup := t.Vals[k]; dup {
		return fmt.Errorf("%w: key %q is defined twice", ErrTOML, k)
	}
	t.Keys = append(t.Keys, k)
	t.Vals[k] = v
	return nil
}

// sub returns the table under k, making it; defined says whether it was
// already declared by a header.
func (t *Table) sub(k string) (*Table, error) {
	if v, ok := t.Vals[k]; ok {
		st, ok := v.(*Table)
		if !ok {
			return nil, fmt.Errorf("%w: %q is not a table", ErrTOML, k)
		}
		return st, nil
	}
	st := newTable()
	t.Keys = append(t.Keys, k)
	t.Vals[k] = st
	return st, nil
}

// ParseTOML reads text as tomllib.loads does, within this reader's part
// of TOML.
func ParseTOML(text string) (*Table, error) {
	if !utf8.ValidString(text) {
		return nil, fmt.Errorf("%w: the file is not UTF-8", ErrTOML)
	}
	root := newTable()
	cur := root
	declared := map[string]bool{}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for n, raw := range lines {
		p := &tparser{s: raw, line: n + 1}
		p.ws()
		if p.done() || p.peek() == '#' {
			continue
		}
		if p.peek() == '[' {
			p.i++
			if p.peek() == '[' {
				return nil, p.errf("arrays of tables")
			}
			keys, err := p.key()
			if err != nil {
				return nil, err
			}
			p.ws()
			if p.peek() != ']' {
				return nil, p.errf("a table header")
			}
			p.i++
			if err := p.end(); err != nil {
				return nil, err
			}
			name := strings.Join(keys, "\x00")
			if declared[name] {
				return nil, p.errf("a table declared twice")
			}
			declared[name] = true
			t := root
			for _, k := range keys {
				if t, err = t.sub(k); err != nil {
					return nil, err
				}
			}
			cur = t
			continue
		}
		keys, err := p.key()
		if err != nil {
			return nil, err
		}
		p.ws()
		if p.peek() != '=' {
			return nil, p.errf("a key = value line")
		}
		p.i++
		p.ws()
		v, err := p.value(0)
		if err != nil {
			return nil, err
		}
		if err := p.end(); err != nil {
			return nil, err
		}
		t := cur
		for _, k := range keys[:len(keys)-1] {
			if t, err = t.sub(k); err != nil {
				return nil, err
			}
		}
		if err := t.set(keys[len(keys)-1], v); err != nil {
			return nil, err
		}
	}
	return root, nil
}

type tparser struct {
	s    string
	i    int
	line int
}

func (p *tparser) errf(what string) error {
	return fmt.Errorf("%w: line %d holds %s this reader does not read", ErrTOML, p.line, what)
}

func (p *tparser) done() bool { return p.i >= len(p.s) }
func (p *tparser) peek() byte {
	if p.done() {
		return 0
	}
	return p.s[p.i]
}
func (p *tparser) ws() {
	for !p.done() && (p.s[p.i] == ' ' || p.s[p.i] == '\t') {
		p.i++
	}
}

func (p *tparser) end() error {
	p.ws()
	if p.done() || p.peek() == '#' {
		return nil
	}
	return p.errf("text after a value")
}

func isBare(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

func (p *tparser) key() ([]string, error) {
	var keys []string
	for {
		p.ws()
		switch c := p.peek(); {
		case c == '"':
			s, err := p.basic()
			if err != nil {
				return nil, err
			}
			keys = append(keys, s)
		case c == '\'':
			s, err := p.literal()
			if err != nil {
				return nil, err
			}
			keys = append(keys, s)
		case isBare(c):
			st := p.i
			for !p.done() && isBare(p.peek()) {
				p.i++
			}
			keys = append(keys, p.s[st:p.i])
		default:
			return nil, p.errf("a key")
		}
		p.ws()
		if p.peek() != '.' {
			return keys, nil
		}
		p.i++
	}
}

func (p *tparser) basic() (string, error) {
	if strings.HasPrefix(p.s[p.i:], `"""`) {
		return "", p.errf("a multi-line string")
	}
	p.i++
	var b strings.Builder
	for {
		if p.done() {
			return "", p.errf("an unclosed string")
		}
		c := p.s[p.i]
		switch {
		case c == '"':
			p.i++
			return b.String(), nil
		case c == '\\':
			p.i++
			if p.done() {
				return "", p.errf("an unclosed string")
			}
			e := p.s[p.i]
			p.i++
			switch e {
			case 'b':
				b.WriteByte('\b')
			case 't':
				b.WriteByte('\t')
			case 'n':
				b.WriteByte('\n')
			case 'f':
				b.WriteByte('\f')
			case 'r':
				b.WriteByte('\r')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case 'u', 'U':
				n := 4
				if e == 'U' {
					n = 8
				}
				if p.i+n > len(p.s) {
					return "", p.errf("a bad escape")
				}
				v, err := strconv.ParseUint(p.s[p.i:p.i+n], 16, 32)
				r := rune(v)
				if err != nil || !utf8.ValidRune(r) {
					return "", p.errf("a bad escape")
				}
				b.WriteRune(r)
				p.i += n
			default:
				return "", p.errf("a bad escape")
			}
		case c < 0x20 && c != '\t' || c == 0x7f:
			return "", p.errf("a control character in a string")
		default:
			b.WriteByte(c)
			p.i++
		}
	}
}

func (p *tparser) literal() (string, error) {
	if strings.HasPrefix(p.s[p.i:], "'''") {
		return "", p.errf("a multi-line string")
	}
	p.i++
	st := p.i
	for !p.done() && p.peek() != '\'' {
		if c := p.peek(); c < 0x20 && c != '\t' || c == 0x7f {
			return "", p.errf("a control character in a string")
		}
		p.i++
	}
	if p.done() {
		return "", p.errf("an unclosed string")
	}
	s := p.s[st:p.i]
	p.i++
	return s, nil
}

func (p *tparser) value(depth int) (any, error) {
	if depth > 32 {
		return nil, p.errf("nesting")
	}
	switch c := p.peek(); {
	case c == '"':
		return p.basic()
	case c == '\'':
		return p.literal()
	case c == '[':
		p.i++
		var out []any
		for {
			p.ws()
			if p.peek() == ']' {
				p.i++
				return out, nil
			}
			v, err := p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
			p.ws()
			if p.peek() == ',' {
				p.i++
				continue
			}
			if p.peek() == ']' {
				p.i++
				return out, nil
			}
			return nil, p.errf("an array")
		}
	case c == '{':
		p.i++
		t := newTable()
		p.ws()
		if p.peek() == '}' {
			p.i++
			return t, nil
		}
		for {
			keys, err := p.key()
			if err != nil {
				return nil, err
			}
			p.ws()
			if p.peek() != '=' {
				return nil, p.errf("an inline table")
			}
			p.i++
			p.ws()
			v, err := p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			tt := t
			for _, k := range keys[:len(keys)-1] {
				if tt, err = tt.sub(k); err != nil {
					return nil, err
				}
			}
			if err := tt.set(keys[len(keys)-1], v); err != nil {
				return nil, err
			}
			p.ws()
			if p.peek() == ',' {
				p.i++
				continue
			}
			if p.peek() == '}' {
				p.i++
				return t, nil
			}
			return nil, p.errf("an inline table")
		}
	}
	st := p.i
	for !p.done() && !strings.ContainsRune(" \t,]}#", rune(p.peek())) {
		p.i++
	}
	word := p.s[st:p.i]
	switch word {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	clean := strings.ReplaceAll(word, "_", "")
	if clean != "" && !strings.Contains(word, "__") && !strings.HasPrefix(word, "_") && !strings.HasSuffix(word, "_") {
		digits := strings.TrimLeft(clean, "+-")
		if digits != "" && strings.Trim(digits, "0123456789") == "" && (len(digits) == 1 || digits[0] != '0') {
			if n, err := strconv.ParseInt(clean, 10, 64); err == nil {
				return n, nil
			}
		}
		if strings.ContainsAny(digits, ".eE") && strings.Trim(digits, "0123456789.eE+-") == "" &&
			digits[0] >= '0' && digits[0] <= '9' {
			if f, err := strconv.ParseFloat(clean, 64); err == nil {
				return f, nil
			}
		}
	}
	return nil, p.errf("a value")
}

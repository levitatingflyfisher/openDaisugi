package pyyaml

import (
	"strconv"
	"strings"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// LoadDumped is yaml.safe_load for text in the form SafeDump writes:
// block mappings and sequences, empty flow collections, and plain,
// single-quoted and double-quoted scalars, folded over lines as the
// emitter folds them. It reads the text, then dumps what it read and
// accepts the reading only when that gives the same text back (a final
// line break aside), so a text in any other form, whatever safe_load
// would make of it, is Unsupported rather than guessed at.
func LoadDumped(text string) (v any, why *Unsupported) {
	defer func() {
		if p := recover(); p != nil {
			switch x := p.(type) {
			case *Unsupported:
				v, why = nil, x
			case *pystr.Exception:
				v, why = nil, &Unsupported{"a value safe_load raises on: " + x.Error()}
			default:
				panic(p)
			}
		}
	}()
	if nonPrintable(text) >= 0 {
		unsupported("a character the YAML reader rejects")
	}
	r := &dreader{lines: strings.Split(text, "\n")}
	if r.next() < 0 {
		unsupported("an empty document")
	}
	v = r.block(0)
	if r.next() >= 0 {
		unsupported("text after the document")
	}
	again, bad := SafeDump(v)
	if bad != nil {
		return nil, bad
	}
	if again != text && again != text+"\n" {
		unsupported("a text not in the form yaml.safe_dump writes")
	}
	return v, nil
}

type dreader struct {
	lines []string
	i     int
}

func indentOf(l string) int { return len(l) - len(strings.TrimLeft(l, " ")) }

// next is the indent of the next line with content, or -1 at the end.
func (r *dreader) next() int {
	for r.i < len(r.lines) {
		l := r.lines[r.i]
		if strings.ContainsAny(l, "\t\r") {
			unsupported("a tab or a carriage return")
		}
		if strings.TrimLeft(l, " ") == "" {
			r.i++
			continue
		}
		return indentOf(l)
	}
	return -1
}

func isSeqItem(c string) bool { return c == "-" || strings.HasPrefix(c, "- ") }

// block reads the collection whose first line is the next one, at an
// indent of at least min.
func (r *dreader) block(min int) any {
	k := r.next()
	if k < min {
		unsupported("a block that is not indented")
	}
	if isSeqItem(r.lines[r.i][k:]) {
		return r.sequence(k)
	}
	return r.mapping(k)
}

func (r *dreader) sequence(k int) []any {
	out := []any{}
	for {
		at := r.next()
		if at != k || !isSeqItem(r.lines[r.i][k:]) {
			return out
		}
		l := r.lines[r.i]
		if len(l) == k+1 {
			unsupported("an empty sequence item")
		}
		out = append(out, r.inline(k+2, k))
	}
}

// inline reads the node that starts at column col of the current line,
// inside a parent at indent parent.
func (r *dreader) inline(col, parent int) any {
	l := r.lines[r.i]
	s := l[col:]
	switch {
	case isSeqItem(s):
		r.lines[r.i] = strings.Repeat(" ", col) + s
		return r.sequence(col)
	case s == "[]":
		r.i++
		return []any{}
	case s == "{}":
		r.i++
		return pyjson.NewObject()
	case isMappingEntry(s):
		r.lines[r.i] = strings.Repeat(" ", col) + s
		return r.mapping(col)
	}
	return r.scalar(col, parent)
}

// isMappingEntry: the text is a key and a colon. A quoted key is read to
// its end; a plain key ends at ": " or a final ":".
func isMappingEntry(s string) bool {
	if s == "" {
		return false
	}
	if s[0] == '\'' || s[0] == '"' {
		end := quotedEnd(s)
		return end > 0 && end < len(s) && s[end] == ':'
	}
	return strings.Contains(s, ": ") || strings.HasSuffix(s, ":")
}

// quotedEnd is the index just past a one-line quoted token, or -1.
func quotedEnd(s string) int {
	q := s[0]
	for i := 1; i < len(s); i++ {
		switch {
		case q == '"' && s[i] == '\\':
			i++
		case s[i] == q && q == '\'' && i+1 < len(s) && s[i+1] == '\'':
			i++
		case s[i] == q:
			return i + 1
		}
	}
	return -1
}

func (r *dreader) mapping(k int) any {
	out := pyjson.NewObject()
	for {
		at := r.next()
		if at != k || isSeqItem(r.lines[r.i][k:]) {
			return out
		}
		l := r.lines[r.i]
		s := l[k:]
		var key, rest string
		switch s[0] {
		case '\'', '"':
			end := quotedEnd(s)
			if end < 0 || end >= len(s) || s[end] != ':' {
				unsupported("a key that is not a simple key")
			}
			key, _ = decodeLine(s[1:end-1], s[0] == '"', true)
			rest = s[end+1:]
		case '?':
			unsupported("a complex key")
		default:
			c := strings.Index(s, ": ")
			if c < 0 {
				if !strings.HasSuffix(s, ":") {
					unsupported("a line with no key")
				}
				c = len(s) - 1
			}
			key = s[:c]
			if resolveTag(key) != "str" {
				unsupported("a key that is not a str")
			}
			rest = s[c+1:]
		}
		if _, dup := out.Get(key); dup {
			unsupported("a repeated key")
		}
		if rest == "" {
			r.i++
			nx := r.next()
			switch {
			case nx > k:
				out.Set(key, r.block(nx))
			case nx == k && isSeqItem(r.lines[r.i][k:]):
				out.Set(key, r.sequence(k))
			default:
				out.Set(key, nil)
			}
			continue
		}
		if rest[0] != ' ' {
			unsupported("a key not followed by a space")
		}
		col := len(l) - len(rest) + 1
		out.Set(key, r.inline(col, k))
	}
}

// scalar reads a scalar from column col of the current line; its
// continuation lines are indented past parent.
func (r *dreader) scalar(col, parent int) any {
	l := r.lines[r.i]
	s := l[col:]
	if s[0] == '\'' || s[0] == '"' {
		return r.quoted(col, parent)
	}
	parts := []string{strings.TrimRight(s, " ")}
	r.i++
	for r.i < len(r.lines) {
		c := r.lines[r.i]
		if strings.TrimLeft(c, " ") == "" || indentOf(c) <= parent {
			break
		}
		parts = append(parts, strings.Trim(c, " "))
		r.i++
	}
	return resolve(strings.Join(parts, " "))
}

// quoted reads a quoted scalar over as many lines as it takes.
func (r *dreader) quoted(col, parent int) string {
	l := r.lines[r.i]
	q := l[col]
	double := q == '"'
	var pieces []string
	cur := l[col+1:]
	for {
		end := closeIn(cur, q)
		if end >= 0 {
			pieces = append(pieces, cur[:end])
			if strings.TrimRight(cur[end+1:], " ") != "" {
				unsupported("text after a quoted value")
			}
			r.i++
			return foldQuoted(pieces, double)
		}
		pieces = append(pieces, cur)
		r.i++
		if r.i >= len(r.lines) {
			unsupported("a quoted value that does not close")
		}
		cur = r.lines[r.i]
		if strings.TrimLeft(cur, " ") != "" && indentOf(cur) <= parent {
			unsupported("a quoted value's line that is not indented")
		}
	}
}

// closeIn is the index of the closing quote in one line of a quoted
// scalar, or -1.
func closeIn(s string, q byte) int {
	for i := 0; i < len(s); i++ {
		switch {
		case q == '"' && s[i] == '\\':
			i++
		case q == '\'' && s[i] == '\'' && i+1 < len(s) && s[i+1] == '\'':
			i++
		case s[i] == q:
			return i
		}
	}
	return -1
}

// foldQuoted joins the lines of a quoted scalar as YAML folds a flow
// scalar: white space around a line break goes, a single break is a
// space, each empty line between is a line feed, and in double quotes a
// break after a backslash is nothing.
func foldQuoted(pieces []string, double bool) string {
	var out strings.Builder
	fold, empties := false, 0
	for n, s := range pieces {
		last := n == len(pieces)-1
		if n > 0 {
			s = strings.TrimLeft(s, " ")
		}
		if n > 0 && !last && s == "" && fold {
			empties++
			continue
		}
		if fold {
			if empties == 0 {
				out.WriteByte(' ')
			} else {
				out.WriteString(strings.Repeat("\n", empties))
			}
		}
		fold, empties = false, 0
		text, escapedBreak := decodeLine(s, double, last)
		out.WriteString(text)
		fold = !last && !escapedBreak
	}
	return out.String()
}

// decodeLine reads one line of a quoted scalar: its escapes, and its
// trailing white space dropped when a plain line break follows. It
// reports a final backslash, which escapes the line break.
func decodeLine(s string, double, last bool) (string, bool) {
	var b []byte
	trail := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case !double && c == '\'' && i+1 < len(s) && s[i+1] == '\'':
			b = append(b, '\'')
			i++
			trail = 0
		case double && c == '\\':
			if i+1 >= len(s) {
				if last {
					unsupported("a backslash before the closing quote")
				}
				return string(b), true
			}
			e := s[i+1]
			if rep, ok := escapes[e]; ok {
				b = append(b, rep...)
				i++
				trail = 0
				continue
			}
			width := map[byte]int{'x': 2, 'u': 4, 'U': 8}[e]
			if width == 0 || i+2+width > len(s) {
				unsupported("an escape the reader does not read")
			}
			n, err := strconv.ParseUint(s[i+2:i+2+width], 16, 32)
			if err != nil || n > 0x10ffff || strings.ContainsAny(s[i+2:i+2+width], "+-") {
				unsupported("an escape the reader does not read")
			}
			b = pystr.AppendRune(b, rune(n))
			i += 1 + width
			trail = 0
		case c == ' ':
			b = append(b, c)
			trail++
		default:
			b = append(b, c)
			trail = 0
		}
	}
	if !last {
		b = b[:len(b)-trail]
	}
	return string(b), false
}

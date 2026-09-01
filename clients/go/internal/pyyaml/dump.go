package pyyaml

import (
	"fmt"
	"math"
	"strings"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// SafeDump is yaml.safe_dump(v, sort_keys=False, default_flow_style=False):
// PyYAML 6's representer, serializer and emitter for the values json
// gives (nil, bool, string, pyjson.Int, float64, pyjson.Float, []any and
// *pyjson.Object), with its defaults: indent 2, width 80, no unicode
// (so non-ASCII text is written double-quoted with escapes), no aliases
// (none of these values is shared). It panics with *Unsupported for a
// value outside that set.
func SafeDump(v any) (out string, why *Unsupported) {
	defer func() {
		if p := recover(); p != nil {
			if u, ok := p.(*Unsupported); ok {
				out, why = "", u
				return
			}
			panic(p)
		}
	}()
	e := &emitter{indent: -1, whitespace: true, indention: true}
	e.node(v, true, false, false, false)
	// DocumentEnd of an implicit document, then StreamEnd.
	e.writeIndent()
	if e.openEnded {
		e.writeIndicator("...", true, false, false)
		e.writeIndent()
	}
	return e.b.String(), nil
}

const (
	bestIndent = 2
	bestWidth  = 80
)

// emitter is yaml.emitter.Emitter's state, the parts a dump of these
// values reaches. column counts code points, as Python's len does.
type emitter struct {
	b                                     strings.Builder
	indents                               []int
	indent                                int // -1 is None
	flowLevel                             int
	rootCtx, seqCtx, mapCtx, simpleKeyCtx bool
	column                                int
	whitespace, indention                 bool
	openEnded                             bool
}

func (e *emitter) increaseIndent(flow, indentless bool) {
	e.indents = append(e.indents, e.indent)
	if e.indent < 0 {
		if flow {
			e.indent = bestIndent
		} else {
			e.indent = 0
		}
	} else if !indentless {
		e.indent += bestIndent
	}
}

func (e *emitter) popIndent() {
	e.indent = e.indents[len(e.indents)-1]
	e.indents = e.indents[:len(e.indents)-1]
}

func (e *emitter) write(s string) {
	e.column += pystr.Len(s)
	e.b.WriteString(s)
}

func (e *emitter) writeIndicator(ind string, needWhitespace, whitespace, indention bool) {
	data := ind
	if !e.whitespace && needWhitespace {
		data = " " + ind
	}
	e.whitespace = whitespace
	e.indention = e.indention && indention
	e.openEnded = false
	e.write(data)
}

func (e *emitter) writeIndent() {
	indent := max(e.indent, 0)
	if !e.indention || e.column > indent || (e.column == indent && !e.whitespace) {
		e.writeLineBreak("\n")
	}
	if e.column < indent {
		e.whitespace = true
		e.b.WriteString(strings.Repeat(" ", indent-e.column))
		e.column = indent
	}
}

func (e *emitter) writeLineBreak(data string) {
	e.whitespace = true
	e.indention = true
	e.column = 0
	e.b.WriteString(data)
}

// node is expect_node for one value.
func (e *emitter) node(v any, root, seq, mapping, simpleKey bool) {
	e.rootCtx, e.seqCtx, e.mapCtx, e.simpleKeyCtx = root, seq, mapping, simpleKey
	switch x := v.(type) {
	case []any:
		if e.flowLevel > 0 || len(x) == 0 {
			e.flowSequence(x)
		} else {
			e.blockSequence(x)
		}
	case *pyjson.Object:
		if e.flowLevel > 0 || x.Len() == 0 {
			e.flowMapping(x)
		} else {
			e.blockMapping(x)
		}
	default:
		tag, value := represent(v)
		e.scalar(tag, value)
	}
}

// represent is SafeRepresenter for a scalar: its tag and text.
func represent(v any) (tag, value string) {
	switch x := v.(type) {
	case nil:
		return "null", "null"
	case bool:
		if x {
			return "bool", "true"
		}
		return "bool", "false"
	case pyjson.Int:
		return "int", x.Text
	case float64:
		return "float", floatText(x)
	case pyjson.Float:
		return "float", floatText(float64(x))
	case string:
		return "str", x
	}
	panic(&Unsupported{fmt.Sprintf("a %T value", v)})
}

// floatText is represent_float.
func floatText(f float64) string {
	switch {
	case math.IsNaN(f):
		return ".nan"
	case math.IsInf(f, 1):
		return ".inf"
	case math.IsInf(f, -1):
		return "-.inf"
	}
	v := strings.ToLower(pyjson.FloatRepr(f))
	if !strings.Contains(v, ".") && strings.Contains(v, "e") {
		v = strings.Replace(v, "e", ".0e", 1)
	}
	return v
}

// tagLen is len(prepare_tag(tag)): "!!" and the short name.
func tagLen(tag string) int { return 2 + len(tag) }

func (e *emitter) flowSequence(xs []any) {
	e.writeIndicator("[", true, true, false)
	e.flowLevel++
	e.increaseIndent(true, false)
	for i, x := range xs {
		if i > 0 {
			e.writeIndicator(",", false, false, false)
		}
		if e.column > bestWidth {
			e.writeIndent()
		}
		e.node(x, false, true, false, false)
	}
	e.popIndent()
	e.flowLevel--
	e.writeIndicator("]", false, false, false)
}

func (e *emitter) flowMapping(o *pyjson.Object) {
	e.writeIndicator("{", true, true, false)
	e.flowLevel++
	e.increaseIndent(true, false)
	for i, k := range o.Keys() {
		if i > 0 {
			e.writeIndicator(",", false, false, false)
		}
		if e.column > bestWidth {
			e.writeIndent()
		}
		if !simpleKey(k) {
			panic(&Unsupported{"a key too long or too odd for a simple key"})
		}
		e.node(k, false, false, true, true)
		e.writeIndicator(":", false, false, false)
		e.node(o.Value(k), false, false, true, false)
	}
	e.popIndent()
	e.flowLevel--
	e.writeIndicator("}", false, false, false)
}

func (e *emitter) blockSequence(xs []any) {
	indentless := e.mapCtx && !e.indention
	e.increaseIndent(false, indentless)
	for _, x := range xs {
		e.writeIndent()
		e.writeIndicator("-", true, false, true)
		e.node(x, false, true, false, false)
	}
	e.popIndent()
}

func (e *emitter) blockMapping(o *pyjson.Object) {
	e.increaseIndent(false, false)
	for _, k := range o.Keys() {
		e.writeIndent()
		if simpleKey(k) {
			e.node(k, false, false, true, true)
			e.writeIndicator(":", false, false, false)
			e.node(o.Value(k), false, false, true, false)
			continue
		}
		e.writeIndicator("?", true, false, true)
		e.node(k, false, false, true, false)
		e.writeIndent()
		e.writeIndicator(":", true, false, true)
		e.node(o.Value(k), false, false, true, false)
	}
	e.popIndent()
}

// simpleKey is check_simple_key for a str key.
func simpleKey(k string) bool {
	a := analyze(k)
	return tagLen("str")+pystr.Len(k) < 128 && !a.empty && !a.multiline
}

func (e *emitter) scalar(tag, value string) {
	a := analyze(value)
	implicitPlain := resolveTag(value) == tag
	implicitQuoted := tag == "str"
	style := e.chooseStyle(a, implicitPlain)
	if !((style == "" && implicitPlain) || (style != "" && implicitQuoted)) {
		panic(&Unsupported{"a scalar that needs an explicit tag"})
	}
	e.increaseIndent(true, false)
	split := !e.simpleKeyCtx
	switch style {
	case `"`:
		e.writeDoubleQuoted(pystr.Runes(value), split)
	case "'":
		e.writeSingleQuoted(pystr.Runes(value), split)
	default:
		e.writePlain(pystr.Runes(value), split)
	}
	e.popIndent()
}

func (e *emitter) chooseStyle(a analysis, implicitPlain bool) string {
	if implicitPlain {
		if !(e.simpleKeyCtx && (a.empty || a.multiline)) &&
			((e.flowLevel > 0 && a.allowFlowPlain) || (e.flowLevel == 0 && a.allowBlockPlain)) {
			return ""
		}
	}
	if a.allowSingleQuoted && !(e.simpleKeyCtx && a.multiline) {
		return "'"
	}
	return `"`
}

type analysis struct {
	empty, multiline                                   bool
	allowFlowPlain, allowBlockPlain, allowSingleQuoted bool
}

func isBreak(r rune) bool { return r == '\n' || r == 0x85 || r == 0x2028 || r == 0x2029 }

func isBlankOrEnd(r rune) bool {
	return r == 0 || r == ' ' || r == '\t' || r == '\r' || isBreak(r)
}

// analyze is Emitter.analyze_scalar with allow_unicode off.
func analyze(s string) analysis {
	if s == "" {
		return analysis{empty: true, allowBlockPlain: true, allowSingleQuoted: true}
	}
	rs := pystr.Runes(s)
	var blockInd, flowInd, lineBreaks, special bool
	var leadingSpace, leadingBreak, trailingSpace, trailingBreak, breakSpace, spaceBreak bool
	if strings.HasPrefix(s, "---") || strings.HasPrefix(s, "...") {
		blockInd, flowInd = true, true
	}
	precededByWS := true
	followedByWS := len(rs) == 1 || isBlankOrEnd(rs[1])
	prevSpace, prevBreak := false, false
	for i, ch := range rs {
		if i == 0 {
			if strings.ContainsRune("#,[]{}&*!|>'\"%@`", ch) {
				flowInd, blockInd = true, true
			}
			if ch == '?' || ch == ':' {
				flowInd = true
				if followedByWS {
					blockInd = true
				}
			}
			if ch == '-' && followedByWS {
				flowInd, blockInd = true, true
			}
		} else {
			if strings.ContainsRune(",?[]{}", ch) {
				flowInd = true
			}
			if ch == ':' {
				flowInd = true
				if followedByWS {
					blockInd = true
				}
			}
			if ch == '#' && precededByWS {
				flowInd, blockInd = true, true
			}
		}
		if isBreak(ch) {
			lineBreaks = true
		}
		if !(ch == '\n' || (ch >= 0x20 && ch <= 0x7e)) {
			// Any other character is special: allow_unicode is off.
			special = true
		}
		switch {
		case ch == ' ':
			if i == 0 {
				leadingSpace = true
			}
			if i == len(rs)-1 {
				trailingSpace = true
			}
			if prevBreak {
				breakSpace = true
			}
			prevSpace, prevBreak = true, false
		case isBreak(ch):
			if i == 0 {
				leadingBreak = true
			}
			if i == len(rs)-1 {
				trailingBreak = true
			}
			if prevSpace {
				spaceBreak = true
			}
			prevSpace, prevBreak = false, true
		default:
			prevSpace, prevBreak = false, false
		}
		precededByWS = isBlankOrEnd(ch)
		followedByWS = i+2 >= len(rs) || isBlankOrEnd(rs[i+2])
	}
	a := analysis{multiline: lineBreaks, allowFlowPlain: true, allowBlockPlain: true, allowSingleQuoted: true}
	if leadingSpace || leadingBreak || trailingSpace || trailingBreak {
		a.allowFlowPlain, a.allowBlockPlain = false, false
	}
	if breakSpace {
		a.allowFlowPlain, a.allowBlockPlain, a.allowSingleQuoted = false, false, false
	}
	if spaceBreak || special {
		a.allowFlowPlain, a.allowBlockPlain, a.allowSingleQuoted = false, false, false
	}
	if lineBreaks {
		a.allowFlowPlain, a.allowBlockPlain = false, false
	}
	if flowInd {
		a.allowFlowPlain = false
	}
	if blockInd {
		a.allowBlockPlain = false
	}
	return a
}

// resolveTag is Resolver.resolve(ScalarNode, value, (True, False)): the
// short name of the tag a plain scalar with this text would get.
func resolveTag(v string) string {
	match := func(re interface{ MatchString(string) bool }) bool {
		// Python's $ also matches before a final newline.
		return re.MatchString(v) || (strings.HasSuffix(v, "\n") && re.MatchString(v[:len(v)-1]))
	}
	if v == "" {
		return "null"
	}
	first, _ := pystr.DecodeRune(v)
	type resolver struct {
		tag   string
		first string
		ok    func() bool
	}
	for _, r := range []resolver{
		{"bool", "yYnNtTfFoO", func() bool { return match(boolRe()) }},
		{"float", "-+0123456789.", func() bool { return match(floatRe()) }},
		{"int", "-+0123456789", func() bool { return match(intRe()) }},
		{"merge", "<", func() bool { return v == "<<" || v == "<<\n" }},
		{"null", "~nN", func() bool { return match(nullRe()) }},
		{"timestamp", "0123456789", func() bool { return match(timeRe()) }},
		{"value", "=", func() bool { return v == "=" || v == "=\n" }},
		{"yaml", "!&*", func() bool {
			return strings.Trim(strings.TrimSuffix(v, "\n"), "!&*") == "" && len(strings.TrimSuffix(v, "\n")) == 1
		}},
	} {
		if strings.ContainsRune(r.first, first) && r.ok() {
			return r.tag
		}
	}
	return "str"
}

func (e *emitter) writeSingleQuoted(text []rune, split bool) {
	e.writeIndicator("'", true, false, false)
	spaces, breaks := false, false
	start, end := 0, 0
	n := len(text)
	for end <= n {
		var ch rune = -1
		if end < n {
			ch = text[end]
		}
		switch {
		case spaces:
			if ch == -1 || ch != ' ' {
				if start+1 == end && e.column > bestWidth && split && start != 0 && end != n {
					e.writeIndent()
				} else {
					e.write(pystr.FromRunes(text[start:end]))
				}
				start = end
			}
		case breaks:
			if ch == -1 || !isBreak(ch) {
				if text[start] == '\n' {
					e.writeLineBreak("\n")
				}
				for _, br := range text[start:end] {
					e.writeLineBreak(pystr.FromRunes([]rune{br}))
				}
				e.writeIndent()
				start = end
			}
		default:
			if ch == -1 || ch == ' ' || isBreak(ch) || ch == '\'' {
				if start < end {
					e.write(pystr.FromRunes(text[start:end]))
					start = end
				}
			}
		}
		if ch == '\'' {
			e.write("''")
			start = end + 1
		}
		if ch != -1 {
			spaces = ch == ' '
			breaks = isBreak(ch)
		}
		end++
	}
	e.writeIndicator("'", false, false, false)
}

var escapeReplacements = map[rune]string{
	0: "0", 0x07: "a", 0x08: "b", 0x09: "t", 0x0a: "n", 0x0b: "v", 0x0c: "f", 0x0d: "r", 0x1b: "e",
	'"': `"`, '\\': `\`, 0x85: "N", 0xa0: "_", 0x2028: "L", 0x2029: "P",
}

func (e *emitter) writeDoubleQuoted(text []rune, split bool) {
	e.writeIndicator(`"`, true, false, false)
	start, end := 0, 0
	n := len(text)
	for end <= n {
		var ch rune = -1
		if end < n {
			ch = text[end]
		}
		if ch == -1 || ch == '"' || ch == '\\' || ch == 0x85 || ch == 0x2028 || ch == 0x2029 || ch == 0xfeff ||
			!(ch >= 0x20 && ch <= 0x7e) {
			if start < end {
				e.write(pystr.FromRunes(text[start:end]))
				start = end
			}
			if ch != -1 {
				var data string
				if rep, ok := escapeReplacements[ch]; ok {
					data = `\` + rep
				} else if ch <= 0xff {
					data = fmt.Sprintf(`\x%02X`, ch)
				} else if ch <= 0xffff {
					data = fmt.Sprintf(`\u%04X`, ch)
				} else {
					data = fmt.Sprintf(`\U%08X`, ch)
				}
				e.write(data)
				start = end + 1
			}
		}
		if 0 < end && end < n-1 && (ch == ' ' || start >= end) && e.column+(end-start) > bestWidth && split {
			data := `\`
			if start < end {
				// text[start:end] is empty in Python when start is past end.
				data = pystr.FromRunes(text[start:end]) + `\`
			}
			if start < end {
				start = end
			}
			e.write(data)
			e.writeIndent()
			e.whitespace = false
			e.indention = false
			if text[start] == ' ' {
				e.write(`\`)
			}
		}
		end++
	}
	e.writeIndicator(`"`, false, false, false)
}

func (e *emitter) writePlain(text []rune, split bool) {
	if e.rootCtx {
		e.openEnded = true
	}
	if len(text) == 0 {
		return
	}
	if !e.whitespace {
		e.write(" ")
	}
	e.whitespace = false
	e.indention = false
	spaces, breaks := false, false
	start, end := 0, 0
	n := len(text)
	for end <= n {
		var ch rune = -1
		if end < n {
			ch = text[end]
		}
		switch {
		case spaces:
			if ch != ' ' {
				if start+1 == end && e.column > bestWidth && split {
					e.writeIndent()
					e.whitespace = false
					e.indention = false
				} else {
					e.write(pystr.FromRunes(text[start:end]))
				}
				start = end
			}
		case breaks:
			if !isBreak(ch) {
				if text[start] == '\n' {
					e.writeLineBreak("\n")
				}
				for _, br := range text[start:end] {
					e.writeLineBreak(pystr.FromRunes([]rune{br}))
				}
				e.writeIndent()
				e.whitespace = false
				e.indention = false
				start = end
			}
		default:
			if ch == -1 || ch == ' ' || isBreak(ch) {
				e.write(pystr.FromRunes(text[start:end]))
				start = end
			}
		}
		if ch != -1 {
			spaces = ch == ' '
			breaks = isBreak(ch)
		}
		end++
	}
}

// Package pyyaml reads the YAML a config file holds the way PyYAML 6's
// yaml.safe_load does, for a plain subset of YAML: a block mapping of
// simple keys, one level of nested block mapping, and scalar values
// (plain, single-quoted or double-quoted, on one line), with comments and
// blank lines. Plain scalars resolve by PyYAML's YAML 1.1 rules, so yes
// and on are booleans and 0x10 is an int.
//
// Load answers in three ways: the value safe_load gives, the exception it
// raises (only where the text is certain to make PyYAML raise), or
// Unsupported, for text outside the subset. The caller must not guess at
// an Unsupported text.
package pyyaml

import (
	"math"
	"math/big"
	"strconv"
	"strings"

	"daisugi-verify/internal/lazyre"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// Unsupported is text outside the subset.
type Unsupported struct{ Why string }

func (u *Unsupported) Error() string { return u.Why }

func unsupported(why string) { panic(&Unsupported{why}) }

// Load is yaml.safe_load(text). v is nil, bool, string, pyjson.Int,
// pyjson.Float or *pyjson.Object.
func Load(text string) (v any, exc *pystr.Exception, why *Unsupported) {
	defer func() {
		if p := recover(); p != nil {
			if u, ok := p.(*Unsupported); ok {
				v, exc, why = nil, nil, u
				return
			}
			if e, ok := p.(*pystr.Exception); ok {
				// A constructor error: the whole load raises.
				v, exc, why = nil, e, nil
				return
			}
			panic(p)
		}
	}()
	if i := nonPrintable(text); i >= 0 {
		// yaml.reader.Reader.check_printable raises ReaderError.
		return nil, pystr.NewException("ReaderError", "unacceptable character"), nil
	}
	text = strings.TrimPrefix(text, "\ufeff")
	p := &parser{}
	for _, raw := range strings.Split(text, "\n") {
		p.lines = append(p.lines, raw)
	}
	return p.document(), nil, nil
}

// nonPrintable is the index of the first character yaml.reader rejects:
// anything outside [\t\n\r\x20-\x7e\x85\xa0-퟿-�] and the
// astral planes. A surrogate is rejected too.
func nonPrintable(s string) int {
	for i := 0; i < len(s); {
		r, n := pystr.DecodeRune(s[i:])
		ok := r == '\t' || r == '\n' || r == '\r' || (r >= 0x20 && r <= 0x7e) || r == 0x85 ||
			(r >= 0xa0 && r <= 0xd7ff) || (r >= 0xe000 && r <= 0xfffd) || (r >= 0x10000 && r <= 0x10ffff)
		if !ok || (r == 0xfffd && n == 1) {
			return i
		}
		i += n
	}
	return -1
}

type parser struct {
	lines []string
	i     int
}

// content skips blank and comment lines and reports the next line's
// indent, or -1 at the end.
func (p *parser) content() int {
	for p.i < len(p.lines) {
		l := p.lines[p.i]
		if strings.ContainsAny(l, "\t\r") {
			unsupported("a tab or a carriage return")
		}
		t := strings.TrimLeft(l, " ")
		if t == "" || strings.HasPrefix(t, "#") {
			p.i++
			continue
		}
		return len(l) - len(t)
	}
	return -1
}

func (p *parser) document() any {
	ind := p.content()
	if ind < 0 {
		return nil
	}
	if ind != 0 {
		unsupported("an indented first line")
	}
	first := p.lines[p.i]
	if strings.HasPrefix(first, "---") || strings.HasPrefix(first, "...") || strings.HasPrefix(first, "%") {
		unsupported("a document marker or directive")
	}
	return p.mapping(0, true)
}

var keyRe = lazyre.New(`^[A-Za-z_][A-Za-z0-9_]*$`)

// mapping reads the block mapping whose keys sit at indent ind.
func (p *parser) mapping(ind int, top bool) *pyjson.Object {
	out := pyjson.NewObject()
	for {
		at := p.content()
		if at < 0 || at < ind {
			return out
		}
		if at > ind {
			unsupported("an indent the mapping does not open")
		}
		line := p.lines[p.i][ind:]
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			unsupported("a line with no key")
		}
		key := line[:colon]
		if !keyRe().MatchString(key) {
			unsupported("a key that is not a plain name")
		}
		rest := line[colon+1:]
		if rest != "" && rest[0] != ' ' {
			unsupported("a key not followed by a space")
		}
		rest = strings.TrimLeft(rest, " ")
		p.i++
		var val any
		if rest == "" || strings.HasPrefix(rest, "#") {
			next := p.content()
			if next > ind {
				if !top {
					unsupported("a mapping nested two levels deep")
				}
				val = p.mapping(next, false)
			} else {
				val = nil
			}
		} else {
			val = scalar(rest)
			if next := p.content(); next > ind {
				unsupported("a value that goes on past its line")
			}
		}
		// PyYAML's SafeLoader keeps the last of repeated keys. A resolved
		// key such as true or null is not a str; the caller only looks up
		// field names, so the text serves as the key.
		out.Set(keyText(key), val)
	}
}

// keyText marks a key that YAML resolves to something other than a str,
// so it can never equal a field name.
func keyText(k string) string {
	switch v := resolve(k).(type) {
	case string:
		return k
	case bool:
		// True and False, as dict keys, are one key per value.
		if v {
			return "\x00True"
		}
		return "\x00False"
	}
	return "\x00None"
}

func scalar(rest string) any {
	switch rest[0] {
	case '\'':
		return quoted(rest, false)
	case '"':
		return quoted(rest, true)
	case '[', ']', '{', '}', ',', '#', '&', '*', '!', '|', '>', '%', '@', '`':
		unsupported("a value that starts with an indicator")
	case '?', ':', '-':
		if len(rest) == 1 || rest[1] == ' ' {
			unsupported("a value that starts with an indicator")
		}
	}
	text := rest
	if i := strings.Index(text, " #"); i >= 0 {
		text = text[:i]
	}
	text = strings.TrimRight(text, " ")
	if strings.Contains(text, ": ") || strings.HasSuffix(text, ":") {
		unsupported("a colon inside a plain value")
	}
	return resolve(text)
}

// tail checks what follows a quoted scalar: spaces, then a comment or
// nothing.
func tail(s string) {
	t := strings.TrimLeft(s, " ")
	if t == "" {
		return
	}
	if strings.HasPrefix(t, "#") && len(t) < len(s) {
		return
	}
	unsupported("text after a quoted value")
}

var escapes = map[byte]string{
	'0': "\x00", 'a': "\x07", 'b': "\x08", 't': "\t", '\t': "\t", 'n': "\n", 'v': "\x0b", 'f': "\x0c",
	'r': "\r", 'e': "\x1b", ' ': " ", '"': "\"", '/': "/", '\\': "\\", 'N': "\u0085", '_': " ",
	'L': " ", 'P': " ",
}

func quoted(s string, double bool) string {
	var b strings.Builder
	q := s[0]
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c == q {
			if !double && i+1 < len(s) && s[i+1] == '\'' {
				b.WriteByte('\'')
				i++
				continue
			}
			tail(s[i+1:])
			return b.String()
		}
		if double && c == '\\' {
			if i+1 >= len(s) {
				unsupported("a line break inside a quoted value")
			}
			e := s[i+1]
			if rep, ok := escapes[e]; ok {
				b.WriteString(rep)
				i++
				continue
			}
			width := map[byte]int{'x': 2, 'u': 4, 'U': 8}[e]
			if width == 0 || i+2+width > len(s) {
				unsupported("an escape the subset does not read")
			}
			hex := s[i+2 : i+2+width]
			n, err := strconv.ParseUint(hex, 16, 32)
			if err != nil || strings.ContainsAny(hex, "+-") {
				unsupported("an escape the subset does not read")
			}
			r := rune(n)
			if r > 0x10ffff {
				unsupported("an escape past the last code point")
			}
			b.Write(pystr.AppendRune(nil, r))
			i += 1 + width
			continue
		}
		b.WriteByte(c)
	}
	unsupported("a quoted value that does not close on its line")
	return ""
}

var (
	boolRe  = lazyre.New(`^(?:yes|Yes|YES|no|No|NO|true|True|TRUE|false|False|FALSE|on|On|ON|off|Off|OFF)$`)
	floatRe = lazyre.New(`^(?:[-+]?(?:[0-9][0-9_]*)\.[0-9_]*(?:[eE][-+][0-9]+)?|\.[0-9][0-9_]*(?:[eE][-+][0-9]+)?|[-+]?[0-9][0-9_]*(?::[0-5]?[0-9])+\.[0-9_]*|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))$`)
	intRe   = lazyre.New(`^(?:[-+]?0b[0-1_]+|[-+]?0[0-7_]+|[-+]?(?:0|[1-9][0-9_]*)|[-+]?0x[0-9a-fA-F_]+|[-+]?[1-9][0-9_]*(?::[0-5]?[0-9])+)$`)
	nullRe  = lazyre.New(`^(?:~|null|Null|NULL|)$`)
	timeRe  = lazyre.New(`^(?:[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]|[0-9][0-9][0-9][0-9]-[0-9][0-9]?-[0-9][0-9]?(?:[Tt]|[ \t]+)[0-9][0-9]?:[0-9][0-9]:[0-9][0-9](?:\.[0-9]*)?(?:[ \t]*(?:Z|[-+][0-9][0-9]?(?::[0-9][0-9])?))?)$`)
)

// resolve is the implicit resolver and SafeConstructor for a plain scalar.
func resolve(v string) any {
	switch {
	case boolRe().MatchString(v):
		switch strings.ToLower(v) {
		case "yes", "true", "on":
			return true
		}
		return false
	case floatRe().MatchString(v):
		return yamlFloat(v)
	case intRe().MatchString(v):
		return yamlInt(v)
	case v == "<<":
		unsupported("a merge key")
	case nullRe().MatchString(v):
		return nil
	case timeRe().MatchString(v):
		return timestamp(v)
	case v == "=":
		// SafeConstructor has no constructor for the value tag.
		panic(pystr.NewException("ConstructorError", "could not determine a constructor for the tag 'tag:yaml.org,2002:value'"))
	}
	return v
}

func yamlInt(v string) any {
	v = strings.ReplaceAll(v, "_", "")
	sign := ""
	if v[0] == '-' || v[0] == '+' {
		if v[0] == '-' {
			sign = "-"
		}
		v = v[1:]
	}
	n := new(big.Int)
	var ok bool
	switch {
	case v == "0":
		return pyjson.Int{Text: "0"}
	case strings.HasPrefix(v, "0b"):
		_, ok = n.SetString(v[2:], 2)
	case strings.HasPrefix(v, "0x"):
		_, ok = n.SetString(v[2:], 16)
	case v[0] == '0':
		_, ok = n.SetString(v, 8)
	case strings.Contains(v, ":"):
		ok = true
		for _, part := range strings.Split(v, ":") {
			d, err := strconv.ParseInt(part, 10, 64)
			if err != nil {
				unsupported("a base 60 int the subset does not read")
			}
			n.Mul(n, big.NewInt(60))
			n.Add(n, big.NewInt(d))
		}
	default:
		_, ok = n.SetString(v, 10)
	}
	if !ok {
		unsupported("an int with no digits")
	}
	if sign == "-" {
		n.Neg(n)
	}
	return pyjson.Int{Text: n.String()}
}

func yamlFloat(v string) any {
	v = strings.ToLower(strings.ReplaceAll(v, "_", ""))
	sign := 1.0
	if v[0] == '-' || v[0] == '+' {
		if v[0] == '-' {
			sign = -1
		}
		v = v[1:]
	}
	switch {
	case v == ".inf":
		return pyjson.Float(sign * math.Inf(1))
	case v == ".nan":
		return pyjson.Float(math.NaN())
	case strings.Contains(v, ":"):
		parts := strings.Split(v, ":")
		value, base := 0.0, 1.0
		for i := len(parts) - 1; i >= 0; i-- {
			d, err := strconv.ParseFloat(parts[i], 64)
			if err != nil {
				unsupported("a base 60 float the subset does not read")
			}
			value += d * base
			base *= 60
		}
		return pyjson.Float(sign * value)
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); !ok || ne.Err != strconv.ErrRange {
			unsupported("a float the subset does not read")
		}
	}
	return pyjson.Float(sign * f)
}

// Timestamp is a datetime.date or datetime.datetime value. No config
// field accepts one.
type Timestamp struct{ Text string }

var timestampRe = lazyre.New(`^([0-9][0-9][0-9][0-9])-([0-9][0-9]?)-([0-9][0-9]?)(?:(?:[Tt]|[ \t]+)([0-9][0-9]?):([0-9][0-9]):([0-9][0-9])(?:\.([0-9]*))?(?:[ \t]*(Z|([-+])([0-9][0-9]?)(?::([0-9][0-9]))?))?)?$`)

// timestamp is SafeConstructor.construct_yaml_timestamp: it raises
// ValueError where datetime does.
func timestamp(v string) any {
	m := timestampRe().FindStringSubmatch(v)
	if m == nil {
		panic(pystr.NewException("AttributeError", "'NoneType' object has no attribute 'groupdict'"))
	}
	atoi := func(s string) int {
		n, _ := strconv.Atoi(s)
		return n
	}
	year, month, day := atoi(m[1]), atoi(m[2]), atoi(m[3])
	bad := func(msg string) { panic(pystr.NewException("ValueError", msg)) }
	if year < 1 {
		bad("year 0 is out of range")
	}
	if month < 1 || month > 12 {
		bad("month must be in 1..12")
	}
	days := [...]int{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}[month-1]
	if month == 2 && (year%4 == 0 && (year%100 != 0 || year%400 == 0)) {
		days = 29
	}
	if day < 1 || day > days {
		bad("day is out of range for month")
	}
	if m[4] == "" {
		return Timestamp{v}
	}
	if atoi(m[4]) > 23 {
		bad("hour must be in 0..23")
	}
	if atoi(m[5]) > 59 {
		bad("minute must be in 0..59")
	}
	if atoi(m[6]) > 59 {
		bad("second must be in 0..59")
	}
	if m[9] != "" && atoi(m[10])*60+atoi(m[11]) >= 24*60 {
		bad("offset must be a timedelta strictly between -timedelta(hours=24) and timedelta(hours=24)")
	}
	return Timestamp{v}
}

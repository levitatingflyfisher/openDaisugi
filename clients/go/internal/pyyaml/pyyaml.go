// Package pyyaml reads YAML the way PyYAML 6's yaml.safe_load does (full.go
// is a translation of its loader), and writes it the way yaml.safe_dump
// does (dump.go). Plain scalars resolve by PyYAML's YAML 1.1 rules, so yes
// and on are booleans and 0x10 is an int.
//
// Load answers in three ways: the value safe_load gives, the exception it
// raises with str(exc) as its words, or Unsupported, for a value the result
// model does not hold. The caller must not guess at an Unsupported text.
package pyyaml

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"

	"daisugi-verify/internal/lazyre"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// Unsupported is text outside the subset.
type Unsupported struct{ Why string }

func (u *Unsupported) Error() string { return u.Why }

func unsupported(why string) { panic(&Unsupported{why}) }

// Load is yaml.safe_load(text). v is nil, bool, string, pyjson.Int,
// pyjson.Float, Timestamp, []any or *pyjson.Object (a key that is not a
// str is marked with a leading NUL, see dictKey).
func Load(text string) (v any, exc *pystr.Exception, why *Unsupported) {
	defer func() {
		if p := recover(); p != nil {
			if u, ok := p.(*Unsupported); ok {
				v, exc, why = nil, nil, u
				return
			}
			if e, ok := p.(*pystr.Exception); ok {
				v, exc, why = nil, e, nil
				return
			}
			panic(p)
		}
	}()
	if i := nonPrintable(text); i >= 0 {
		// yaml.reader.Reader.check_printable raises ReaderError.
		r, _ := pystr.DecodeRune(text[i:])
		return nil, pystr.NewException("ReaderError", fmt.Sprintf(
			"unacceptable character #x%04x: special characters are not allowed\n  in \"<unicode string>\", position %d",
			r, utf8.RuneCountInString(text[:i]))), nil
	}
	return loadFull(text), nil, nil
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

// maxIntDigits is Python 3.12's sys.int_info.default_max_str_digits.
const maxIntDigits = 4300

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
		// int(value) reads at most 4300 decimal digits (Python 3.12's
		// sys.int_info.default_max_str_digits); past that it raises.
		if len(v) > maxIntDigits {
			panic(pystr.NewException("ValueError", fmt.Sprintf("Exceeds the limit (%d digits) for integer string "+
				"conversion: value has %d digits; use sys.set_int_max_str_digits() to increase the limit", maxIntDigits, len(v))))
		}
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
	// construct_yaml_timestamp makes the timezone before the datetime, so
	// a bad offset raises before a bad date.
	if m[4] != "" && m[9] != "" {
		if off := atoi(m[10])*60 + atoi(m[11]); off >= 24*60 {
			bad("offset must be a timedelta strictly between -timedelta(hours=24) and timedelta(hours=24), not " +
				offsetRepr(off, m[9] == "-") + ".")
		}
	}
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
	return Timestamp{v}
}

// offsetRepr is repr(datetime.timedelta(minutes=sign*mins)).
func offsetRepr(mins int, negative bool) string {
	secs := mins * 60
	if negative {
		secs = -secs
	}
	days := secs / 86400
	rem := secs % 86400
	if rem < 0 {
		rem += 86400
		days--
	}
	var parts []string
	if days != 0 {
		parts = append(parts, "days="+strconv.Itoa(days))
	}
	if rem != 0 {
		parts = append(parts, "seconds="+strconv.Itoa(rem))
	}
	if len(parts) == 0 {
		return "datetime.timedelta(0)"
	}
	return "datetime.timedelta(" + strings.Join(parts, ", ") + ")"
}

// Plain reports whether v holds only what JSON holds: no Timestamp and no
// key that is not a str, at any depth.
func Plain(v any) bool {
	switch x := v.(type) {
	case Timestamp:
		return false
	case []any:
		for _, e := range x {
			if !Plain(e) {
				return false
			}
		}
	case *pyjson.Object:
		for _, k := range x.Keys() {
			if strings.HasPrefix(k, "\x00") || !Plain(x.Value(k)) {
				return false
			}
		}
	}
	return true
}

// Caught reports whether `except (yaml.YAMLError, ValueError)` catches
// exc: PyYAML's own errors and a ValueError (a date out of range).
func Caught(exc *pystr.Exception) bool {
	switch exc.Type {
	case "ScannerError", "ParserError", "ComposerError", "ConstructorError", "ReaderError", "ValueError":
		return true
	}
	return false
}

// FirstLine is str(exc).splitlines()[0].
func FirstLine(exc *pystr.Exception) string {
	if lines := pystr.Splitlines(exc.Msg); len(lines) > 0 {
		return lines[0]
	}
	return ""
}

// Qualified is the exception's class as a traceback's last line names it.
func Qualified(exc *pystr.Exception) string {
	switch exc.Type {
	case "ScannerError":
		return "yaml.scanner.ScannerError"
	case "ParserError":
		return "yaml.parser.ParserError"
	case "ComposerError":
		return "yaml.composer.ComposerError"
	case "ConstructorError":
		return "yaml.constructor.ConstructorError"
	case "ReaderError":
		return "yaml.reader.ReaderError"
	}
	return exc.Type
}

// escapes are the one-letter escapes of a double-quoted scalar (dumped.go).
var escapes = map[byte]string{
	'0': "\x00", 'a': "\x07", 'b': "\x08", 't': "\t", '\t': "\t", 'n': "\n", 'v': "\x0b", 'f': "\x0c",
	'r': "\r", 'e': "\x1b", ' ': " ", '"': "\"", '/': "/", '\\': "\\", 'N': "\u0085", '_': " ",
	'L': " ", 'P': " ",
}

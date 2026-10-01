package voice

import (
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode"

	"daisugi-verify/internal/lazyre"
	"daisugi-verify/internal/pystr"
)

// digitValue is the value of a Unicode decimal digit: its distance from
// the zero of its run.
func digitValue(c rune) int {
	for base := c; base >= c-9; base-- {
		if unicode.IsDigit(base) && (base == 0 || !unicode.IsDigit(base-1)) {
			return int(c - base)
		}
	}
	return 0
}

// asciiNumeric maps each Unicode decimal digit to its ASCII digit and each
// whitespace character to a space, as int() and float() do first. Any
// other non-ASCII character makes the text invalid.
func asciiNumeric(s string) (string, bool) {
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
			return "", false
		}
	}
	return string(ab), true
}

var floatLiteral = lazyre.New(`^[+-]?(?:[0-9](?:_?[0-9])*(?:\.(?:[0-9](?:_?[0-9])*)?)?|\.[0-9](?:_?[0-9])*)(?:[eE][+-]?[0-9](?:_?[0-9])*)?$`)

var intLiteral = lazyre.New(`^[+-]?[0-9](?:_?[0-9])*$`)

// PyFloat is float(s) for a str. ok is false where Python raises
// ValueError.
func PyFloat(s string) (float64, bool) {
	a, ok := asciiNumeric(s)
	if !ok {
		return 0, false
	}
	t := strings.Trim(a, " ")
	body := strings.ToLower(t)
	sign := 1.0
	if strings.HasPrefix(body, "+") || strings.HasPrefix(body, "-") {
		if body[0] == '-' {
			sign = -1
		}
		body = body[1:]
	}
	switch body {
	case "inf", "infinity":
		return sign * math.Inf(1), true
	case "nan":
		return math.NaN(), true
	}
	if !floatLiteral().MatchString(t) {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(t, "_", ""), 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); !ok || ne.Err != strconv.ErrRange {
			return 0, false
		}
	}
	return f, true
}

// PyInt is int(s) for a str: nil where Python raises ValueError.
func PyInt(s string) *big.Int {
	a, ok := asciiNumeric(s)
	if !ok {
		return nil
	}
	t := strings.Trim(a, " ")
	if !intLiteral().MatchString(t) {
		return nil
	}
	n, ok := new(big.Int).SetString(strings.TrimPrefix(strings.ReplaceAll(t, "_", ""), "+"), 10)
	if !ok {
		return nil
	}
	return n
}

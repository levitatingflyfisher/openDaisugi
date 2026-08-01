package verify

import (
	"errors"
	"strings"
)

// posixSplit ports Python's shlex.split(s, posix=True) EXACTLY for the
// configuration opendaisugi.interpreter_parse actually uses:
// whitespace_split=True (set by shlex.split itself), commenters='' (comments
// argument defaults to False in shlex.split, so '#' is an ordinary
// character), punctuation_chars='' (shlex default).
//
// Under that configuration cpython's read_token state machine collapses:
// wordchars membership never matters (whitespace_split=True makes the
// wordchars branch and its fallback identical), so this is a small state
// machine over {space, word, single-quote, double-quote, escape} — see the
// derivation in the Go client's session notes / ADJUDICATIONS if this ever
// needs re-deriving. Returns an error exactly where Python raises ValueError
// ("No closing quotation" / "No escaped character") — the caller (
// parse_interpreter) treats that as "not parseable", matching the oracle's
// `except ValueError: return None`.
func posixSplit(s string) ([]string, error) {
	const (
		stSpace = iota
		stWord
		stSingle
		stDouble
		stEscape
	)
	runes := []rune(s)
	n := len(runes)
	i := 0
	state := stSpace
	escapedState := stWord
	var token []rune
	var tokens []string

	isWS := func(r rune) bool { return r == ' ' || r == '\t' || r == '\r' || r == '\n' }

	for {
		hasCh := i < n
		var ch rune
		if hasCh {
			ch = runes[i]
		}
		switch state {
		case stSpace:
			if !hasCh {
				return tokens, nil // clean EOF between tokens
			}
			i++
			switch {
			case isWS(ch):
				// stay in space state, nothing to do
			case ch == '\\':
				escapedState = stWord
				state = stEscape
			case ch == '\'':
				state = stSingle
			case ch == '"':
				state = stDouble
			default:
				token = append(token, ch)
				state = stWord
			}
		case stSingle:
			if !hasCh {
				return nil, errors.New("no closing quotation")
			}
			i++
			if ch == '\'' {
				state = stWord
			} else {
				token = append(token, ch)
			}
		case stDouble:
			if !hasCh {
				return nil, errors.New("no closing quotation")
			}
			i++
			if ch == '"' {
				state = stWord
			} else if ch == '\\' {
				escapedState = stDouble
				state = stEscape
			} else {
				token = append(token, ch)
			}
		case stEscape:
			if !hasCh {
				return nil, errors.New("no escaped character")
			}
			i++
			if escapedState == stDouble {
				// Inside double quotes, backslash is meaningful only before
				// '\\' or '"' — anything else keeps the backslash literally.
				if ch != '\\' && ch != '"' {
					token = append(token, '\\')
				}
				token = append(token, ch)
			} else {
				token = append(token, ch)
			}
			state = escapedState
		case stWord:
			if !hasCh {
				tokens = append(tokens, string(token))
				return tokens, nil
			}
			switch {
			case isWS(ch):
				i++
				tokens = append(tokens, string(token))
				token = nil
				state = stSpace
			case ch == '\'':
				i++
				state = stSingle
			case ch == '"':
				i++
				state = stDouble
			case ch == '\\':
				i++
				escapedState = stWord
				state = stEscape
			default:
				i++
				token = append(token, ch)
			}
		}
	}
}

// shlexQuote ports Python's shlex.quote exactly: empty string -> "''";
// a string with no characters outside [A-Za-z0-9_@%+=:,./-] is returned
// unquoted; otherwise it is single-quoted with embedded quotes escaped as
// '"'"'.
func shlexQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !isShlexSafe(r) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func isShlexSafe(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	switch r {
	case '_', '@', '%', '+', '=', ':', ',', '.', '/', '-':
		return true
	}
	return false
}

// shlexJoin quotes and joins tokens with a single space — the Go analogue of
// interpreter_parse.py's `" ".join(shlex.quote(t) for t in tokens)`.
func shlexJoin(tokens []string) string {
	quoted := make([]string, len(tokens))
	for i, t := range tokens {
		quoted[i] = shlexQuote(t)
	}
	return strings.Join(quoted, " ")
}

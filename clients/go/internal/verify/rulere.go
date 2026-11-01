package verify

import (
	"errors"
	"regexp"
	"sync"
	"unicode/utf8"
)

// The regex of an MCP argument rule is one small subset (RuleRegexSupported,
// rulesubset.go), run whole string on RE2 (opendaisugi/rule_regex.py). Inside
// the subset Python's RE2 binding, Go regexp and Rust regex give the same
// answers, and RE2 takes time linear in the input. A pattern outside the
// subset is refused with one reason, InvalidRegexReason. The pattern is not
// rewritten.

type ruleCompiled struct {
	re  *regexp.Regexp
	err error
}

var ruleRegexCache sync.Map

// ruleRegex is the cached whole-string compile of a rule regex.
func ruleRegex(pattern string) (*regexp.Regexp, error) {
	if c, ok := ruleRegexCache.Load(pattern); ok {
		r := c.(ruleCompiled)
		return r.re, r.err
	}
	re, err := compileRuleSubset(pattern)
	ruleRegexCache.Store(pattern, ruleCompiled{re, err})
	return re, err
}

var errOutsideSubset = errors.New("outside the supported subset")

// compileRuleSubset is the whole-string compile of a pattern in the subset.
func compileRuleSubset(pattern string) (*regexp.Regexp, error) {
	if !RuleRegexSupported(pattern) {
		return nil, errOutsideSubset
	}
	return regexp.Compile(`\A(?:` + pattern + `)\z`)
}

// RuleRegexValid is rule_regex.compiles: the pattern is in the subset and RE2
// compiles it.
func RuleRegexValid(pattern string) bool {
	_, err := ruleRegex(pattern)
	return err == nil
}

// RuleRegexFull is rule_regex.fullmatch: the pattern covers all of s.
func RuleRegexFull(pattern, s string) (bool, error) {
	re, err := ruleRegex(pattern)
	if err != nil {
		return false, err
	}
	return re.MatchString(s), nil
}

// RuleTextRefusal is what predicate_z3._regex_hit checks after the length cap:
// a pattern outside the subset, then a value that is not valid UTF-8 (a lone
// surrogate is not). "" when the rule may run.
func RuleTextRefusal(pattern, s, path string) string {
	if !RuleRegexValid(pattern) {
		return "argument rule refused: " + InvalidRegexReason(pattern)
	}
	if !utf8.ValidString(s) {
		return "argument rule refused: argument '" + path + "' is not valid UTF-8 text"
	}
	return ""
}

// InvalidRegexReason is rule_regex.invalid_reason: the one reason text for a
// pattern outside the subset.
func InvalidRegexReason(pattern string) string {
	return "regex '" + pattern + "' is outside the supported subset"
}

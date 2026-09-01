package verify

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// This file ports opendaisugi/dialect.py and write_paths.py: the words of
// the system dialect, the glob translation that fills their typed hole,
// and the write paths of one step that forall_writes reads. The tables
// are in dialect_gen.go.

// WordFor is dialect.word_for: the word an invariant type names.
func WordFor(typeName string) (string, bool) {
	if _, ok := dialectWords[typeName]; ok {
		return typeName, true
	}
	w, ok := dialectSynonyms[typeName]
	return w, ok
}

// WordTarget is dialect.target_of: the given target, else the default.
func WordTarget(word string, target *string) string {
	if target != nil && *target != "" {
		return *target
	}
	return dialectWords[word].Default
}

// UnsupportedGlob is dialect.UnsupportedGlob; its text is the oracle's.
type UnsupportedGlob struct{ Msg string }

func (e UnsupportedGlob) Error() string { return e.Msg }

const regexSpecials = "\\.^$*+?{}[]|()"

func globEscape(s string) string {
	var b strings.Builder
	for _, c := range s {
		if strings.ContainsRune(regexSpecials, c) {
			b.WriteByte('\\')
		}
		b.WriteRune(c)
	}
	return b.String()
}

func globSegment(seg string) string {
	var b strings.Builder
	for _, c := range seg {
		switch c {
		case '*':
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(globEscape(string(c)))
		}
	}
	return b.String()
}

const anySegments = "(?:/[^/]*)*"
const globEnd = `\Z`

// elem is one glob segment: nil for "**", else its regex.
type globElem = *string

func globRest(elems []globElem) string {
	var b strings.Builder
	for _, e := range elems {
		if e == nil {
			b.WriteString(anySegments)
		} else {
			b.WriteString("/" + *e)
		}
	}
	return b.String()
}

func globFirst(elems []globElem) string {
	if len(elems) == 0 {
		return ""
	}
	head, tail := elems[0], elems[1:]
	if head != nil {
		return *head + globRest(tail)
	}
	return "(?:" + globFirst(tail) + "|[^/]*" + anySegments + globRest(tail) + ")"
}

// GlobRegex is dialect.glob_regex.
func GlobRegex(glob string) (string, error) {
	if glob == "" {
		return "", UnsupportedGlob{"the glob is empty"}
	}
	if utf8.RuneCountInString(glob) > maxGlobChars {
		return "", UnsupportedGlob{fmt.Sprintf("the glob is longer than %d characters", maxGlobChars)}
	}
	if strings.ContainsAny(glob, "[]") {
		return "", UnsupportedGlob{"character classes ([...]) are not supported"}
	}
	if strings.Count(glob, "*") > maxGlobStars {
		return "", UnsupportedGlob{fmt.Sprintf("the glob has more than %d stars", maxGlobStars)}
	}
	if strings.HasSuffix(glob, "/**") {
		raw := glob[:len(glob)-3]
		if raw == "" {
			return "^" + anySegments[:len(anySegments)-1] + "+" + globEnd, nil
		}
		prefix := posixNormpath(raw)
		if prefix == "." {
			return "", UnsupportedGlob{"a /** glob whose prefix is . is not supported"}
		}
		return "^" + globEscape(prefix) + anySegments + globEnd, nil
	}
	segs := strings.Split(glob, "/")
	double := 0
	for _, s := range segs {
		if s == "**" {
			double++
		}
	}
	if double > maxGlobDoubleStars {
		return "", UnsupportedGlob{fmt.Sprintf("the glob has more than %d ** segments", maxGlobDoubleStars)}
	}
	elems := make([]globElem, len(segs))
	for i, s := range segs {
		if s != "**" {
			r := globSegment(s)
			elems[i] = &r
		}
	}
	return "^(?:" + globFirst(elems) + ")" + globEnd, nil
}

// fillHoles is dialect._fill: a string that is exactly "$name" becomes
// the hole's value.
func fillHoles(v any, holes map[string]string) any {
	switch x := v.(type) {
	case string:
		if strings.HasPrefix(x, "$") {
			if val, ok := holes[x[1:]]; ok {
				return val
			}
		}
		return x
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = fillHoles(e, holes)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = fillHoles(e, holes)
		}
		return out
	}
	return v
}

// UnfoldWordJSON is the JSON of dialect.unfold's kernel term of
// word(target) placed from base ("" for none), before parse_expression.
// The error is an UnsupportedGlob.
func UnfoldWordJSON(word, target, base string) (string, error) {
	w := dialectWords[word]
	placed, err := ResolveTarget(target, base)
	if err != nil {
		return "", err
	}
	regex, err := GlobRegex(placed)
	if err != nil {
		return "", err
	}
	var body any
	if err := json.Unmarshal([]byte(w.Body), &body); err != nil {
		return "", err
	}
	raw, err := json.Marshal(fillHoles(body, map[string]string{w.Param: regex}))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// UnfoldWord is dialect.unfold: the kernel term of word(target), placed
// from base ("" for none). The error is an UnsupportedGlob.
func UnfoldWord(word, target, base string) (Expression, error) {
	raw, err := UnfoldWordJSON(word, target, base)
	if err != nil {
		return nil, err
	}
	expr, err := ParseExpression(json.RawMessage(raw))
	if err != nil || base == "" {
		return expr, err
	}
	return setWriteBase(expr, base), nil
}

// setWriteBase is dialect._set_base: every forall_writes in expr gets the
// base its writes resolve against.
func setWriteBase(expr Expression, base string) Expression {
	switch e := expr.(type) {
	case ForallWrites:
		return ForallWrites{Pred: setWriteBase(e.Pred, base), Base: base}
	case ForallSteps:
		e.Pred = setWriteBase(e.Pred, base)
		return e
	case ExistsStep:
		e.Pred = setWriteBase(e.Pred, base)
		return e
	case ForallOutputs:
		e.Pred = setWriteBase(e.Pred, base)
		return e
	case And:
		kids := make([]Expression, len(e.Children))
		for i, c := range e.Children {
			kids[i] = setWriteBase(c, base)
		}
		e.Children = kids
		return e
	case Or:
		kids := make([]Expression, len(e.Children))
		for i, c := range e.Children {
			kids[i] = setWriteBase(c, base)
		}
		e.Children = kids
		return e
	case Not:
		e.Child = setWriteBase(e.Child, base)
		return e
	case Implies:
		e.A = setWriteBase(e.A, base)
		e.B = setWriteBase(e.B, base)
		return e
	}
	return expr
}

// wordWitness is dialect.witness.
func wordWitness(regex string, plan ActionPlan, base string) string {
	re, err := compilePyRegex(regex)
	for _, s := range plan.Steps {
		d := dumpedStep(s)
		id, _ := d["id"].(string)
		writes, ok := StepWritePaths(d, base)
		if !ok {
			return fmt.Sprintf("step '%s' has write paths that cannot be read", id)
		}
		for _, p := range writes {
			if err == nil && re.MatchString(p) {
				return fmt.Sprintf("step '%s' writes '%s'", id, p)
			}
		}
	}
	return "no step names the write"
}

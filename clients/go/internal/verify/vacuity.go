package verify

import (
	"fmt"
	"strconv"
	"strings"
)

// VacuityVerdict mirrors vacuity.Verdict.
type VacuityVerdict string

const (
	Tautology    VacuityVerdict = "tautology"
	Contradiction VacuityVerdict = "contradiction"
	NonTrivial   VacuityVerdict = "non_trivial"
)

// smtCompiler emits SMT-LIB2 text for a predicate Expression compiled over
// a single FULLY SYMBOLIC scope (mirrors predicate_z3._Scope(concrete=nil)
// as used by both vacuity.py and subsumption.py's ctx-string encoding).
// Each distinct path gets its own declared variable, memoized so repeat
// references share one symbol (matching _Scope.vars caching).
type smtCompiler struct {
	prefix     string
	stringVars map[string]string
	realVars   map[string]string
	decls      []string
	soft       []string
}

func newSMTCompiler(prefix string) *smtCompiler {
	return &smtCompiler{prefix: prefix, stringVars: map[string]string{}, realVars: map[string]string{}}
}

func (c *smtCompiler) varName(path, suffix string) string {
	return c.prefix + "__" + strings.ReplaceAll(path, ".", "__") + suffix
}

func (c *smtCompiler) stringVar(path string) string {
	if v, ok := c.stringVars[path]; ok {
		return v
	}
	name := c.varName(path, "")
	c.stringVars[path] = name
	c.decls = append(c.decls, fmt.Sprintf("(declare-const %s String)", name))
	return name
}

func (c *smtCompiler) realVar(path string) string {
	if v, ok := c.realVars[path]; ok {
		return v
	}
	name := c.varName(path, "__real")
	c.realVars[path] = name
	c.decls = append(c.decls, fmt.Sprintf("(declare-const %s Real)", name))
	return name
}

func (c *smtCompiler) softBool(tag string) string {
	name := fmt.Sprintf("%s__soft__%s__%d", c.prefix, tag, len(c.soft))
	c.soft = append(c.soft, name)
	c.decls = append(c.decls, fmt.Sprintf("(declare-const %s Bool)", name))
	return name
}

func isNumericJSON(v interface{}) bool {
	_, ok := v.(float64)
	return ok
}

func smtNumLit(v interface{}) (string, error) {
	f, ok := v.(float64)
	if !ok {
		return "", fmt.Errorf("expected numeric value, got %T", v)
	}
	return smtRealLiteral(f), nil
}

// smtRealLiteral formats a float64 as an SMT-LIB2 Real literal (negative
// numbers need the "(- N)" form; SMT-LIB2 has no unary-minus numeral syntax).
func smtRealLiteral(f float64) string {
	neg := f < 0
	if neg {
		f = -f
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	if neg {
		return "(- " + s + ")"
	}
	return s
}

func pythonStr(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return "None"
	}
	if f, ok := v.(float64); ok {
		return smtRealLiteral(f) // best-effort; untested by the corpus
	}
	return fmt.Sprint(v)
}

// compileScalar ports predicate_z3._compile_scalar for a fully symbolic
// scope (concrete=None) — the shape both vacuity.py and subsumption.py's
// invariant compilation use. Matches/NotMatches always compile to a free
// ("soft") Bool rather than a real regex-to-Z3-regex translation
// (regex_to_z3.py is not ported): this is exactly the oracle's own
// fallback for regex shapes its translator can't handle, and is SOUND for
// tautology/contradiction classification whenever the regex isn't the sole
// source of the predicate's determinism — true of every predicate in the
// current corpus (see clients/go/README.md for the honest accounting).
func (c *smtCompiler) compileScalar(expr Expression) (string, error) {
	switch e := expr.(type) {
	case Equals:
		if isNumericJSON(e.Value) {
			lit, err := smtNumLit(e.Value)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("(= %s %s)", c.realVar(e.Path), lit), nil
		}
		return fmt.Sprintf("(= %s %s)", c.stringVar(e.Path), smtQuoteString(pythonStr(e.Value))), nil
	case NotEquals:
		// F-3 fixed oracle-side 2026-08-21: NotEquals now branches
		// numeric-vs-string exactly like Equals (see ADJUDICATIONS.md).
		if isNumericJSON(e.Value) {
			lit, err := smtNumLit(e.Value)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("(not (= %s %s))", c.realVar(e.Path), lit), nil
		}
		return fmt.Sprintf("(not (= %s %s))", c.stringVar(e.Path), smtQuoteString(pythonStr(e.Value))), nil
	case InSet:
		if len(e.Values) == 0 {
			return "false", nil
		}
		var terms []string
		if isNumericJSON(e.Values[0]) {
			v := c.realVar(e.Path)
			for _, val := range e.Values {
				lit, err := smtNumLit(val)
				if err != nil {
					return "", err
				}
				terms = append(terms, fmt.Sprintf("(= %s %s)", v, lit))
			}
		} else {
			v := c.stringVar(e.Path)
			for _, val := range e.Values {
				terms = append(terms, fmt.Sprintf("(= %s %s)", v, smtQuoteString(pythonStr(val))))
			}
		}
		return smtOr(terms), nil
	case NotInSet:
		if len(e.Values) == 0 {
			return "true", nil
		}
		var terms []string
		if isNumericJSON(e.Values[0]) {
			v := c.realVar(e.Path)
			for _, val := range e.Values {
				lit, err := smtNumLit(val)
				if err != nil {
					return "", err
				}
				terms = append(terms, fmt.Sprintf("(not (= %s %s))", v, lit))
			}
		} else {
			v := c.stringVar(e.Path)
			for _, val := range e.Values {
				terms = append(terms, fmt.Sprintf("(not (= %s %s))", v, smtQuoteString(pythonStr(val))))
			}
		}
		return smtAnd(terms), nil
	case Matches:
		return c.softBool("matches"), nil
	case NotMatches:
		return fmt.Sprintf("(not %s)", c.softBool("not_matches")), nil
	case NumericRange:
		v := c.realVar(e.Path)
		return fmt.Sprintf("(and (>= %s %s) (<= %s %s))", v, smtRealLiteral(e.Min), v, smtRealLiteral(e.Max)), nil
	case LengthRange:
		v := c.stringVar(e.Path)
		lo := fmt.Sprintf("(>= (str.len %s) %d)", v, e.Min)
		if e.Max == nil {
			return lo, nil
		}
		return fmt.Sprintf("(and %s (<= (str.len %s) %d))", lo, v, *e.Max), nil
	case Exists:
		return "true", nil
	case IsEmpty:
		return c.softBool("is_empty"), nil
	case And:
		if len(e.Children) == 0 {
			return "true", nil
		}
		terms := make([]string, len(e.Children))
		for i, ch := range e.Children {
			t, err := c.compileScalar(ch)
			if err != nil {
				return "", err
			}
			terms[i] = t
		}
		return smtAnd(terms), nil
	case Or:
		if len(e.Children) == 0 {
			return "false", nil
		}
		terms := make([]string, len(e.Children))
		for i, ch := range e.Children {
			t, err := c.compileScalar(ch)
			if err != nil {
				return "", err
			}
			terms[i] = t
		}
		return smtOr(terms), nil
	case Not:
		t, err := c.compileScalar(e.Child)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("(not %s)", t), nil
	case Implies:
		a, err := c.compileScalar(e.A)
		if err != nil {
			return "", err
		}
		b, err := c.compileScalar(e.B)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("(=> %s %s)", a, b), nil
	case LLMCheck:
		return c.softBool("llm_check"), nil
	case AliasRef:
		return "", fmt.Errorf("unresolved alias reference %q; resolve aliases before compilation", e.Name)
	default:
		return "", fmt.Errorf("unknown scalar predicate op: %v", expr.Op())
	}
}

func smtAnd(terms []string) string {
	if len(terms) == 1 {
		return terms[0]
	}
	return "(and " + strings.Join(terms, " ") + ")"
}

func smtOr(terms []string) string {
	if len(terms) == 1 {
		return terms[0]
	}
	return "(or " + strings.Join(terms, " ") + ")"
}

// stripQuantifier ports vacuity._compute_vacuity's outer-quantifier peel:
// vacuity operates on the per-step/per-output predicate, not the plan-level
// quantifier wrapping it.
func stripQuantifier(expr Expression) Expression {
	switch e := expr.(type) {
	case ForallSteps:
		return e.Pred
	case ExistsStep:
		return e.Pred
	case ForallOutputs:
		return e.Pred
	default:
		return expr
	}
}

// CheckVacuity ports vacuity.check_vacuity (no caching — the corpus doesn't
// exercise the volume that would need it, and correctness doesn't depend
// on it).
func CheckVacuity(z3c *Z3Client, expr Expression, timeoutMs int) (VacuityVerdict, error) {
	inner := stripQuantifier(expr)
	c := newSMTCompiler("vac")
	term, err := c.compileScalar(inner)
	if err != nil {
		return NonTrivial, err
	}

	// Contradiction check: is `term` itself satisfiable?
	sat, err := z3c.CheckSat(strings.Join(c.decls, "\n")+"\n(assert "+term+")", timeoutMs)
	if err != nil {
		return NonTrivial, err
	}
	if sat == "unsat" {
		return Contradiction, nil
	}

	// Tautology check: is (not term) satisfiable? Assumptions (none here —
	// a fully symbolic scope adds none) are deliberately excluded so a
	// predicate is only called a tautology unconditionally.
	notSat, err := z3c.CheckSat(strings.Join(c.decls, "\n")+"\n(assert (not "+term+"))", timeoutMs)
	if err != nil {
		return NonTrivial, err
	}
	if notSat == "unsat" {
		return Tautology, nil
	}
	return NonTrivial, nil
}

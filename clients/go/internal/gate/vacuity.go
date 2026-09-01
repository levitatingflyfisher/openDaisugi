package gate

import (
	"fmt"
	"strings"

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pyre"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/z3"
)

// vacuityTimeoutMS is check_vacuity's default Z3 budget per solver.
const vacuityTimeoutMS = 500

// vacuity is vacuity.check_vacuity: "tautology", "contradiction" or
// "non_trivial". Any error while building or solving (an op the symbolic
// compiler does not take, a sort mismatch, a numeral Z3 refuses) is
// non_trivial, as the oracle's except makes it.
func (r *runner) vacuity(e any) (verdict string) {
	key := ""
	if o, ok := e.(*pyjson.Object); ok {
		key = pyjson.Dumps(o, true)
		if v, hit := r.vacuityCache[key]; hit {
			return v
		}
		defer func() {
			if r.vacuityCache == nil {
				r.vacuityCache = map[string]string{}
			}
			r.vacuityCache[key] = verdict
		}()
	}
	verdict = "non_trivial"
	func() {
		defer func() {
			if p := recover(); p != nil {
				switch p.(type) {
				case *pystr.Exception, *z3.Exception:
					verdict = "non_trivial"
				default:
					panic(p)
				}
			}
		}()
		z3.Lock()
		defer z3.Unlock()
		inner := e
		switch opOf(e) {
		case "forall_steps", "exists_step", "forall_outputs":
			inner = e.(*pyjson.Object).Value("pred")
		}
		var soft []string
		sc := &vscope{prefix: "vac", vars: map[string]z3.Expr{}}
		term := compileScalar(inner, sc, &soft, "vac")
		s := z3.NewSolver(vacuityTimeoutMS)
		s.Add(term)
		if s.Check() == z3.Unsat {
			verdict = "contradiction"
			return
		}
		t := z3.NewSolver(vacuityTimeoutMS)
		t.Add(z3.Not(term))
		if t.Check() == z3.Unsat {
			verdict = "tautology"
			return
		}
		verdict = "non_trivial"
	}()
	return verdict
}

// vscope is predicate_z3._Scope with no concrete step.
type vscope struct {
	prefix string
	vars   map[string]z3.Expr
}

func (s *vscope) varName(path string) string {
	return s.prefix + "__" + strings.ReplaceAll(path, ".", "__")
}

func (s *vscope) str(path string) z3.Expr {
	name := s.varName(path)
	if v, ok := s.vars[name]; ok {
		return v
	}
	v := z3.StringConst(name)
	s.vars[name] = v
	return v
}

func (s *vscope) real(path string) z3.Expr {
	name := s.varName(path)
	if v, ok := s.vars[name+"__real"]; ok {
		return v
	}
	v := z3.RealConst(name)
	s.vars[name+"__real"] = v
	return v
}

func isNumberValue(v any) bool {
	switch v.(type) {
	case pyjson.Int, pyjson.Float, int, float64:
		return true
	}
	return false
}

// z3Lit is predicate_z3._z3_lit.
func z3Lit(v any) z3.Expr {
	switch x := v.(type) {
	case bool:
		return z3.BoolVal(x)
	case pyjson.Int:
		return z3.IntVal(x.Text)
	case pyjson.Float:
		return z3.RealVal(pmodel.FloatRepr(float64(x)))
	case float64:
		return z3.RealVal(pmodel.FloatRepr(x))
	}
	return z3.StringVal(pystr.Runes(pyStrOf(v)))
}

// coerce is z3py's _coerce_exprs for two terms: Int widens to Real and
// Bool to an arithmetic sort; any other mix raises "sort mismatch".
func coerce(a, b z3.Expr) (z3.Expr, z3.Expr) {
	ka, kb := a.Kind(), b.Kind()
	if ka == kb {
		if ka == z3.SeqSort || ka == z3.ReSort {
			return a, b
		}
		return a, b
	}
	arith := func(k z3.SortKind) bool { return k == z3.IntSort || k == z3.RealSort }
	toSort := func(e z3.Expr, k z3.SortKind, target z3.SortKind) z3.Expr {
		switch {
		case k == target:
			return e
		case k == z3.IntSort && target == z3.RealSort:
			return z3.ToReal(e)
		case k == z3.BoolSort && target == z3.IntSort:
			return z3.If(e, z3.IntVal("1"), z3.IntVal("0"))
		case k == z3.BoolSort && target == z3.RealSort:
			return z3.ToReal(z3.If(e, z3.IntVal("1"), z3.IntVal("0")))
		}
		panic(&z3.Exception{Msg: "Z3 Integer/Real expression expected"})
	}
	var target z3.SortKind
	switch {
	case arith(ka) && arith(kb):
		target = z3.RealSort
	case ka == z3.BoolSort && arith(kb):
		target = kb
	case kb == z3.BoolSort && arith(ka):
		target = ka
	default:
		panic(&z3.Exception{Msg: "sort mismatch"})
	}
	return toSort(a, ka, target), toSort(b, kb, target)
}

func z3Eq(a, b z3.Expr) z3.Expr { a, b = coerce(a, b); return z3.Eq(a, b) }
func z3Ne(a, b z3.Expr) z3.Expr { a, b = coerce(a, b); return z3.Distinct(a, b) }

// compileScalar is predicate_z3._compile_scalar on a symbolic scope.
func compileScalar(e any, sc *vscope, soft *[]string, softPrefix string) z3.Expr {
	o, _ := e.(*pyjson.Object)
	softBool := func(kind string) z3.Expr {
		name := fmt.Sprintf("%s__%s__%d", softPrefix, kind, len(*soft))
		*soft = append(*soft, name)
		return z3.BoolConst(name)
	}
	path := func() string { return o.Value("path").(string) }
	switch opOf(e) {
	case "equals", "not_equals":
		v := o.Value("value")
		var variable z3.Expr
		if isNumberValue(v) {
			variable = sc.real(path())
		} else {
			variable = sc.str(path())
		}
		if opOf(e) == "equals" {
			return z3Eq(variable, z3Lit(v))
		}
		return z3Ne(variable, z3Lit(v))
	case "in_set", "not_in_set":
		values := o.Value("values").([]any)
		var variable z3.Expr
		if len(values) > 0 && isNumberValue(values[0]) {
			variable = sc.real(path())
		} else {
			variable = sc.str(path())
		}
		if len(values) == 0 {
			return z3.BoolVal(opOf(e) == "not_in_set")
		}
		terms := make([]z3.Expr, len(values))
		for i, v := range values {
			if opOf(e) == "in_set" {
				terms[i] = z3Eq(variable, z3Lit(v))
			} else {
				terms[i] = z3Ne(variable, z3Lit(v))
			}
		}
		if opOf(e) == "in_set" {
			return z3.Or(terms...)
		}
		return z3.And(terms...)
	case "matches", "not_matches":
		variable := sc.str(path())
		re, unsupported := translateRegex(o.Value("regex").(string))
		if unsupported {
			if opOf(e) == "matches" {
				return softBool("matches")
			}
			return z3.Not(softBool("not_matches"))
		}
		if opOf(e) == "matches" {
			return z3.InRe(variable, re)
		}
		return z3.Not(z3.InRe(variable, re))
	case "numeric_range":
		variable := sc.real(path())
		lo := z3.RealVal(pmodel.FloatRepr(o.Value("min").(float64)))
		hi := z3.RealVal(pmodel.FloatRepr(o.Value("max").(float64)))
		return z3.And(z3.Ge(variable, lo), z3.Le(variable, hi))
	case "length_range":
		variable := sc.str(path())
		length := z3.Length(variable)
		bounds := []z3.Expr{z3.Ge(length, z3.IntVal(o.Value("min").(pyjson.Int).Text))}
		if mx, ok := o.Value("max").(pyjson.Int); ok {
			bounds = append(bounds, z3.Le(length, z3.IntVal(mx.Text)))
		}
		if len(bounds) > 1 {
			return z3.And(bounds...)
		}
		return bounds[0]
	case "exists":
		return z3.BoolVal(true)
	case "is_empty":
		return softBool("is_empty")
	case "and", "or":
		children := o.Value("children").([]any)
		if len(children) == 0 {
			return z3.BoolVal(opOf(e) == "and")
		}
		terms := make([]z3.Expr, len(children))
		for i, c := range children {
			terms[i] = compileScalar(c, sc, soft, softPrefix)
		}
		if opOf(e) == "and" {
			return z3.And(terms...)
		}
		return z3.Or(terms...)
	case "not":
		return z3.Not(compileScalar(o.Value("child"), sc, soft, softPrefix))
	case "implies":
		a := compileScalar(o.Value("a"), sc, soft, softPrefix)
		b := compileScalar(o.Value("b"), sc, soft, softPrefix)
		return z3.Implies(a, b)
	case "llm_check":
		return softBool("llm_check")
	case "alias":
		valueError(fmt.Sprintf("unresolved alias reference '%s'; resolve aliases before compilation", o.Value("name").(string)))
	}
	valueError("unknown scalar predicate op")
	return z3.Expr{}
}

// translateRegex is regex_to_z3.translate. unsupported is true where the
// oracle raises UnsupportedRegexError (and the caller uses a free Bool).
// Any other error (z3py's own assertions, a numeral Z3 refuses) panics.
func translateRegex(pattern string) (re z3.Expr, unsupported bool) {
	// translate wraps sre_parse.parse in `except Exception`, so any parse
	// error, RecursionError included, is UnsupportedRegexError.
	p, err := pyre.Parse(pattern, 0, 14)
	if err != nil {
		if err.Type == "unported" {
			unported(err.Msg)
		}
		return z3.Expr{}, true
	}
	if p.State().Flags&^pyre.FlagUnicode != 0 {
		return z3.Expr{}, true
	}
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(errUnsupportedRegex); ok {
				re, unsupported = z3.Expr{}, true
				return
			}
			panic(r)
		}
	}()
	items := p.Data
	anchorStart, anchorEnd := false, false
	if len(items) > 0 && items[0].Op == pyre.AT && (items[0].Lit == pyre.AT_BEGINNING || items[0].Lit == pyre.AT_BEGINNING_STRING) {
		anchorStart = true
		items = items[1:]
	}
	if len(items) > 0 {
		last := items[len(items)-1]
		if last.Op == pyre.AT && (last.Lit == pyre.AT_END || last.Lit == pyre.AT_END_STRING) {
			anchorEnd = true
			items = items[:len(items)-1]
		}
	}
	body := patternToRegex(items)
	left, right := z3.Star(anyChar()), z3.Star(anyChar())
	if anchorStart {
		left = z3.Re(nil)
	}
	if anchorEnd {
		right = z3.Re(nil)
	}
	return z3.Concat(left, body, right), false
}

type errUnsupportedRegex struct{ msg string }

func unsupportedRegex(msg string) { panic(errUnsupportedRegex{msg}) }

func anyChar() z3.Expr {
	return z3.Intersect(z3.Range(0x00, 0xFFFF), z3.Complement(z3.Re([]rune{'\n'})))
}

func complementChar(r z3.Expr) z3.Expr { return z3.Intersect(anyChar(), z3.Complement(r)) }

func digitRe() z3.Expr { return z3.Range('0', '9') }

func wordRe() z3.Expr {
	return z3.Union(z3.Range('a', 'z'), z3.Range('A', 'Z'), z3.Range('0', '9'), z3.Re([]rune{'_'}))
}

func spaceRe() z3.Expr {
	return z3.Union(z3.Re([]rune{' '}), z3.Re([]rune{'\t'}), z3.Re([]rune{'\n'}), z3.Re([]rune{'\r'}))
}

func categoryRegex(cat int) z3.Expr {
	switch cat {
	case pyre.CATEGORY_DIGIT:
		return digitRe()
	case pyre.CATEGORY_NOT_DIGIT:
		return complementChar(digitRe())
	case pyre.CATEGORY_WORD:
		return wordRe()
	case pyre.CATEGORY_NOT_WORD:
		return complementChar(wordRe())
	case pyre.CATEGORY_SPACE:
		return spaceRe()
	case pyre.CATEGORY_NOT_SPACE:
		return complementChar(spaceRe())
	}
	unsupportedRegex("unsupported category")
	return z3.Expr{}
}

func patternToRegex(items []pyre.Item) z3.Expr {
	var pieces []z3.Expr
	for _, it := range items {
		if it.Op == pyre.AT {
			switch it.Lit {
			case pyre.AT_BEGINNING, pyre.AT_END, pyre.AT_BEGINNING_STRING, pyre.AT_END_STRING:
				continue
			}
			unsupportedRegex("regex anchor not supported")
		}
		pieces = append(pieces, nodeToRegex(it))
	}
	switch len(pieces) {
	case 0:
		return z3.Re(nil)
	case 1:
		return pieces[0]
	}
	return z3.Concat(pieces...)
}

func nodeToRegex(it pyre.Item) z3.Expr {
	switch it.Op {
	case pyre.LITERAL:
		return z3.Re([]rune{rune(it.Lit)})
	case pyre.NOT_LITERAL:
		return complementChar(z3.Re([]rune{rune(it.Lit)}))
	case pyre.ANY:
		return anyChar()
	case pyre.IN:
		items := it.Set
		negate := len(items) > 0 && items[0].Op == pyre.NEGATE
		if negate {
			items = items[1:]
		}
		var pieces []z3.Expr
		for _, s := range items {
			switch s.Op {
			case pyre.LITERAL:
				pieces = append(pieces, z3.Re([]rune{rune(s.Lit)}))
			case pyre.RANGE:
				pieces = append(pieces, z3.Range(rune(s.Lo), rune(s.Hi)))
			case pyre.CATEGORY:
				pieces = append(pieces, categoryRegex(s.Lit))
			default:
				unsupportedRegex("unsupported char-class element")
			}
		}
		if len(pieces) == 0 {
			unsupportedRegex("empty character class")
		}
		u := pieces[0]
		if len(pieces) > 1 {
			u = z3.Union(pieces...)
		}
		if negate {
			return complementChar(u)
		}
		return u
	case pyre.BRANCH:
		var pieces []z3.Expr
		for _, b := range it.Branches {
			pieces = append(pieces, patternToRegex(b.Data))
		}
		if len(pieces) == 0 {
			unsupportedRegex("empty branch")
		}
		if len(pieces) == 1 {
			return pieces[0]
		}
		return z3.Union(pieces...)
	case pyre.MAX_REPEAT, pyre.MIN_REPEAT:
		inner := patternToRegex(it.Sub.Data)
		lo, hi := it.Lo, it.Hi
		if hi == pyre.MaxRepeat {
			switch lo {
			case 0:
				return z3.Star(inner)
			case 1:
				return z3.Plus(inner)
			}
			args := make([]z3.Expr, 0, lo+1)
			for i := 0; i < lo; i++ {
				args = append(args, inner)
			}
			return z3.Concat(append(args, z3.Star(inner))...)
		}
		if lo == 0 && hi == 1 {
			return z3.Option(inner)
		}
		if lo == hi {
			if lo == 0 {
				return z3.Re(nil)
			}
			args := make([]z3.Expr, lo)
			for i := range args {
				args[i] = inner
			}
			return z3.Concat(args...)
		}
		return z3.Loop(inner, lo, hi)
	case pyre.SUBPATTERN:
		if it.AddFlags != 0 || it.DelFlags != 0 {
			unsupportedRegex("inline regex flags (?i) / (?m) not supported")
		}
		return patternToRegex(it.Sub.Data)
	case pyre.CATEGORY:
		return categoryRegex(it.Lit)
	case pyre.AT:
		switch it.Lit {
		case pyre.AT_BEGINNING, pyre.AT_END, pyre.AT_BEGINNING_STRING, pyre.AT_END_STRING:
			return z3.Re(nil)
		}
		unsupportedRegex("anchor not supported")
	case pyre.ASSERT, pyre.ASSERT_NOT:
		unsupportedRegex("lookahead/lookbehind not supported")
	case pyre.GROUPREF:
		unsupportedRegex("backreferences not supported")
	}
	unsupportedRegex("unsupported regex op")
	return z3.Expr{}
}

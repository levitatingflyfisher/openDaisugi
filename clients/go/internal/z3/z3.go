// Package z3 binds the part of Z3's C API the gate's verifier uses, the
// way z3py builds the same terms, so a check here asks Z3 exactly the
// question the oracle asks.
//
// Z3 5.1.0 is built from its pinned release source by
// clients/go/scripts/native.sh and linked statically, with zig's C++
// runtime, so the binary needs no libz3 or libstdc++ at run time.
//
// Every term lives in one process-wide context, as z3py's main_ctx does.
// Reference counts are taken and never dropped: the gate is a short
// process that builds a few dozen terms.
package z3

/*
#cgo CFLAGS: -I${SRCDIR}/../../.native/include
#cgo LDFLAGS: ${SRCDIR}/../../.native/lib/libz3.a ${SRCDIR}/../../.native/lib/libc++.a ${SRCDIR}/../../.native/lib/libc++abi.a ${SRCDIR}/../../.native/lib/libunwind.a -lm -lpthread -ldl
#include <stdlib.h>
#include <z3.h>

static Z3_context new_context(void) {
	Z3_config cfg = Z3_mk_config();
	Z3_context c = Z3_mk_context_rc(cfg);
	Z3_set_error_handler(c, NULL);
	Z3_set_ast_print_mode(c, Z3_PRINT_SMTLIB2_COMPLIANT);
	Z3_del_config(cfg);
	return c;
}
*/
import "C"

import (
	"fmt"
	"strings"
	"sync"
	"unsafe"
)

// Exception is z3py's Z3Exception.
type Exception struct{ Msg string }

func (e *Exception) Error() string { return e.Msg }

// Result is a solver answer.
type Result int

const (
	Unsat   Result = -1
	Unknown Result = 0
	Sat     Result = 1
)

// Sort kinds, as Z3_get_sort_kind returns them.
type SortKind int

const (
	BoolSort    SortKind = C.Z3_BOOL_SORT
	IntSort     SortKind = C.Z3_INT_SORT
	RealSort    SortKind = C.Z3_REAL_SORT
	SeqSort     SortKind = C.Z3_SEQ_SORT
	ReSort      SortKind = C.Z3_RE_SORT
	UnknownSort SortKind = -1
)

var (
	mu  sync.Mutex
	ctx C.Z3_context
)

// Lock serializes every use of the shared context, as z3_checks'
// Z3_SOLVE_LOCK does in the oracle. Callers hold it around term building
// and solving.
func Lock()   { mu.Lock(); ensure() }
func Unlock() { mu.Unlock() }

func ensure() {
	if ctx == nil {
		ctx = C.new_context()
	}
}

// check raises the pending Z3 error, as z3core's per-call check does.
func check() {
	if code := C.Z3_get_error_code(ctx); code != C.Z3_OK {
		msg := C.GoString(C.Z3_get_error_msg(ctx, code))
		C.Z3_set_error(ctx, C.Z3_OK)
		panic(&Exception{Msg: msg})
	}
}

// Expr is a Z3 term.
type Expr struct{ a C.Z3_ast }

func wrap(a C.Z3_ast) Expr {
	check()
	C.Z3_inc_ref(ctx, a)
	return Expr{a}
}

// Kind is the kind of the term's sort.
func (e Expr) Kind() SortKind {
	s := C.Z3_get_sort(ctx, e.a)
	check()
	return SortKind(C.Z3_get_sort_kind(ctx, s))
}

// IsStringSort reports a sequence of characters.
func (e Expr) IsStringSort() bool {
	s := C.Z3_get_sort(ctx, e.a)
	return bool(C.Z3_is_string_sort(ctx, s))
}

func sym(name string) C.Z3_symbol {
	cs := C.CString(name)
	defer C.free(unsafe.Pointer(cs))
	return C.Z3_mk_string_symbol(ctx, cs)
}

// BoolConst is z3.Bool(name).
func BoolConst(name string) Expr {
	return wrap(C.Z3_mk_const(ctx, sym(name), C.Z3_mk_bool_sort(ctx)))
}

// IntConst is z3.Int(name).
func IntConst(name string) Expr {
	return wrap(C.Z3_mk_const(ctx, sym(name), C.Z3_mk_int_sort(ctx)))
}

// RealConst is z3.Real(name).
func RealConst(name string) Expr {
	return wrap(C.Z3_mk_const(ctx, sym(name), C.Z3_mk_real_sort(ctx)))
}

// StringConst is z3.String(name).
func StringConst(name string) Expr {
	return wrap(C.Z3_mk_const(ctx, sym(name), C.Z3_mk_string_sort(ctx)))
}

// BoolVal is z3.BoolVal(b).
func BoolVal(b bool) Expr {
	if b {
		return wrap(C.Z3_mk_true(ctx))
	}
	return wrap(C.Z3_mk_false(ctx))
}

func numeral(text string, sort C.Z3_sort) Expr {
	cs := C.CString(text)
	defer C.free(unsafe.Pointer(cs))
	return wrap(C.Z3_mk_numeral(ctx, cs, sort))
}

// IntVal is z3.IntVal of a value whose str() is text.
func IntVal(text string) Expr { return numeral(text, C.Z3_mk_int_sort(ctx)) }

// RealVal is z3.RealVal of a value whose str() is text.
func RealVal(text string) Expr { return numeral(text, C.Z3_mk_real_sort(ctx)) }

// StringVal is z3.StringVal(s): every character outside printable ASCII
// is written as \u{hex} and the result handed to Z3_mk_string, which
// reads those escapes back.
func StringVal(runes []rune) Expr {
	var b strings.Builder
	for _, r := range runes {
		if r >= 32 && r < 127 {
			b.WriteRune(r)
		} else {
			fmt.Fprintf(&b, "\\u{%x}", r)
		}
	}
	cs := C.CString(b.String())
	defer C.free(unsafe.Pointer(cs))
	return wrap(C.Z3_mk_string(ctx, cs))
}

func arr(es []Expr) (*C.Z3_ast, C.uint) {
	if len(es) == 0 {
		return nil, 0
	}
	p := (*C.Z3_ast)(C.malloc(C.size_t(len(es)) * C.size_t(unsafe.Sizeof(es[0].a))))
	s := unsafe.Slice(p, len(es))
	for i, e := range es {
		s[i] = e.a
	}
	return p, C.uint(len(es))
}

// Eq is Z3_mk_eq on two terms of one sort.
func Eq(a, b Expr) Expr { return wrap(C.Z3_mk_eq(ctx, a.a, b.a)) }

// Distinct is Z3_mk_distinct of two terms, as z3py's != builds it.
func Distinct(a, b Expr) Expr {
	p, n := arr([]Expr{a, b})
	defer C.free(unsafe.Pointer(p))
	return wrap(C.Z3_mk_distinct(ctx, n, p))
}

// Not is Z3_mk_not.
func Not(a Expr) Expr { return wrap(C.Z3_mk_not(ctx, a.a)) }

// And is Z3_mk_and over args, one argument included.
func And(args ...Expr) Expr {
	p, n := arr(args)
	defer C.free(unsafe.Pointer(p))
	return wrap(C.Z3_mk_and(ctx, n, p))
}

// Or is Z3_mk_or over args, one argument included.
func Or(args ...Expr) Expr {
	p, n := arr(args)
	defer C.free(unsafe.Pointer(p))
	return wrap(C.Z3_mk_or(ctx, n, p))
}

// Implies is Z3_mk_implies.
func Implies(a, b Expr) Expr { return wrap(C.Z3_mk_implies(ctx, a.a, b.a)) }

// Ge, Le and Gt are the arithmetic comparisons.
func Ge(a, b Expr) Expr { return wrap(C.Z3_mk_ge(ctx, a.a, b.a)) }
func Le(a, b Expr) Expr { return wrap(C.Z3_mk_le(ctx, a.a, b.a)) }
func Gt(a, b Expr) Expr { return wrap(C.Z3_mk_gt(ctx, a.a, b.a)) }

// ToReal is z3.ToReal.
func ToReal(a Expr) Expr { return wrap(C.Z3_mk_int2real(ctx, a.a)) }

// If is z3.If(c, a, b).
func If(c, a, b Expr) Expr { return wrap(C.Z3_mk_ite(ctx, c.a, a.a, b.a)) }

// Length is z3.Length.
func Length(s Expr) Expr { return wrap(C.Z3_mk_seq_length(ctx, s.a)) }

// InRe is z3.InRe.
func InRe(s, re Expr) Expr { return wrap(C.Z3_mk_seq_in_re(ctx, s.a, re.a)) }

// Re is z3.Re of a string value.
func Re(runes []rune) Expr { return wrap(C.Z3_mk_seq_to_re(ctx, StringVal(runes).a)) }

// Range is z3.Range of two one-character strings.
func Range(lo, hi rune) Expr {
	return wrap(C.Z3_mk_re_range(ctx, StringVal([]rune{lo}).a, StringVal([]rune{hi}).a))
}

// Star, Plus, Option and Complement are the regex operators.
func Star(r Expr) Expr       { return wrap(C.Z3_mk_re_star(ctx, r.a)) }
func Plus(r Expr) Expr       { return wrap(C.Z3_mk_re_plus(ctx, r.a)) }
func Option(r Expr) Expr     { return wrap(C.Z3_mk_re_option(ctx, r.a)) }
func Complement(r Expr) Expr { return wrap(C.Z3_mk_re_complement(ctx, r.a)) }

// Loop is z3.Loop(r, lo, hi).
func Loop(r Expr, lo, hi int) Expr { return wrap(C.Z3_mk_re_loop(ctx, r.a, C.uint(lo), C.uint(hi))) }

// Concat is z3.Concat over regexes: at least two arguments, as z3py's
// debug assertion demands.
func Concat(args ...Expr) Expr {
	if len(args) < 2 {
		panic(&Exception{Msg: "At least two arguments expected."})
	}
	p, n := arr(args)
	defer C.free(unsafe.Pointer(p))
	return wrap(C.Z3_mk_re_concat(ctx, n, p))
}

// Union is z3.Union over regexes: one argument is returned as it is.
func Union(args ...Expr) Expr {
	if len(args) == 0 {
		panic(&Exception{Msg: "At least one argument expected."})
	}
	if len(args) == 1 {
		return args[0]
	}
	p, n := arr(args)
	defer C.free(unsafe.Pointer(p))
	return wrap(C.Z3_mk_re_union(ctx, n, p))
}

// Intersect is z3.Intersect over regexes.
func Intersect(args ...Expr) Expr {
	if len(args) == 0 {
		panic(&Exception{Msg: "At least one argument expected."})
	}
	if len(args) == 1 {
		return args[0]
	}
	p, n := arr(args)
	defer C.free(unsafe.Pointer(p))
	return wrap(C.Z3_mk_re_intersect(ctx, n, p))
}

// Solver is z3.Solver().
type Solver struct{ s C.Z3_solver }

// NewSolver is z3.Solver() with solver.set("timeout", timeoutMS).
func NewSolver(timeoutMS int) Solver {
	s := C.Z3_mk_solver(ctx)
	check()
	C.Z3_solver_inc_ref(ctx, s)
	p := C.Z3_mk_params(ctx)
	C.Z3_params_inc_ref(ctx, p)
	cs := C.CString("timeout")
	defer C.free(unsafe.Pointer(cs))
	C.Z3_params_set_uint(ctx, p, C.Z3_mk_string_symbol(ctx, cs), C.uint(timeoutMS))
	C.Z3_solver_set_params(ctx, s, p)
	check()
	return Solver{s}
}

// Add is solver.add.
func (s Solver) Add(es ...Expr) {
	for _, e := range es {
		C.Z3_solver_assert(ctx, s.s, e.a)
		check()
	}
}

// Check is solver.check().
func (s Solver) Check() Result {
	r := C.Z3_solver_check(ctx, s.s)
	check()
	return Result(r)
}

// EvalSMTLIB2 runs SMT-LIB2 commands in the shared context's command
// interpreter, as `z3 -in` runs them, and returns what they print. The
// interpreter keeps its state between calls (push and pop scope it).
func EvalSMTLIB2(cmds string) (string, error) {
	Lock()
	defer Unlock()
	cs := C.CString(cmds)
	defer C.free(unsafe.Pointer(cs))
	r := C.Z3_eval_smtlib2_string(ctx, cs)
	if code := C.Z3_get_error_code(ctx); code != C.Z3_OK {
		msg := C.GoString(C.Z3_get_error_msg(ctx, code))
		C.Z3_set_error(ctx, C.Z3_OK)
		return C.GoString(r), &Exception{Msg: msg}
	}
	return C.GoString(r), nil
}

// Version is Z3's full version string.
func Version() string {
	mu.Lock()
	defer mu.Unlock()
	return C.GoString(C.Z3_get_full_version())
}

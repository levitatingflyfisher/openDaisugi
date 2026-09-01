//! Z3, linked in, called the way z3py calls it.
//!
//! The Python oracle builds its formulas through z3py. This module makes
//! the same C API calls with the same arguments: a string literal is
//! escaped as `StringVal` escapes it, a number is parsed by Z3 from the
//! text Python's `str()` gives, and a solver gets its timeout as a uint
//! parameter. Where z3py raises before it calls Z3 (two sorts it cannot
//! join, a `Concat` of fewer than two terms), this module returns the same
//! error, so a caller that catches it takes the same branch.
//!
//! Every term is kept alive until the context ends. A context lives for
//! one check, so nothing is freed early and nothing leaks past the call.

#![allow(non_snake_case)]

use std::ffi::{CStr, CString};
use z3_sys::*;

/// A Z3 error, as z3py raises it (`Z3Exception`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Z3Error(pub String);

impl std::fmt::Display for Z3Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

pub type Z3Result<T> = Result<T, Z3Error>;

/// The sort of a term, as far as the checks use them.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Sort {
    Bool,
    Int,
    Real,
    Str,
    Re,
}

/// One term and its sort.
#[derive(Debug, Clone, Copy)]
pub struct Term {
    ast: Z3_ast,
    pub sort: Sort,
}

/// `z3.sat`, `z3.unsat` or `z3.unknown`.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Check {
    Sat,
    Unsat,
    Unknown,
}

unsafe extern "C" fn no_handler(_c: Z3_context, _e: Z3_error_code) {}

/// One Z3 context.
pub struct Ctx {
    c: Z3_context,
    solvers: Vec<Z3_solver>,
}

// A context is used by one thread at a time; it moves into the thread that
// runs a bounded check.
unsafe impl Send for Ctx {}

impl Drop for Ctx {
    fn drop(&mut self) {
        unsafe {
            for s in self.solvers.drain(..) {
                Z3_solver_dec_ref(self.c, s);
            }
            Z3_del_context(self.c);
        }
    }
}

impl Ctx {
    /// `z3.Context()`: a reference-counted context with no global options.
    pub fn new() -> Z3Result<Ctx> {
        unsafe {
            let conf = Z3_mk_config().ok_or_else(|| Z3Error("Z3 could not make a config".into()))?;
            let c = Z3_mk_context_rc(conf);
            Z3_del_config(conf);
            let c = c.ok_or_else(|| Z3Error("Z3 could not make a context".into()))?;
            Z3_set_error_handler(c, Some(no_handler));
            Ok(Ctx { c, solvers: Vec::new() })
        }
    }

    fn err(&self) -> Z3Result<()> {
        unsafe {
            let code = Z3_get_error_code(self.c);
            if code == ErrorCode::Ok {
                return Ok(());
            }
            let msg = Z3_get_error_msg(self.c, code);
            let text = if msg.is_null() {
                format!("{code:?}")
            } else {
                CStr::from_ptr(msg).to_string_lossy().into_owned()
            };
            // z3py keeps the message as bytes, so str(exc) is b'...'.
            Err(Z3Error(format!("b'{text}'")))
        }
    }

    fn keep(&self, a: Option<Z3_ast>, sort: Sort) -> Z3Result<Term> {
        self.err()?;
        let ast = a.ok_or_else(|| Z3Error("Z3 returned no term".into()))?;
        unsafe { Z3_inc_ref(self.c, ast) };
        Ok(Term { ast, sort })
    }

    fn z3_sort(&self, s: Sort) -> Z3Result<Z3_sort> {
        unsafe {
            let r = match s {
                Sort::Bool => Z3_mk_bool_sort(self.c),
                Sort::Int => Z3_mk_int_sort(self.c),
                Sort::Real => Z3_mk_real_sort(self.c),
                Sort::Str => Z3_mk_string_sort(self.c),
                Sort::Re => {
                    let st = Z3_mk_string_sort(self.c).ok_or_else(|| Z3Error("no string sort".into()))?;
                    Z3_mk_re_sort(self.c, st)
                }
            };
            self.err()?;
            r.ok_or_else(|| Z3Error("Z3 returned no sort".into()))
        }
    }

    fn cstr(s: &str) -> Z3Result<CString> {
        CString::new(s).map_err(|_| Z3Error("embedded null character".into()))
    }

    /// `z3.Bool(name)`, `z3.Int(name)`, `z3.Real(name)`, `z3.String(name)`.
    pub fn constant(&self, name: &str, sort: Sort) -> Z3Result<Term> {
        let n = Self::cstr(name)?;
        unsafe {
            let sym = Z3_mk_string_symbol(self.c, n.as_ptr());
            self.err()?;
            let sym = sym.ok_or_else(|| Z3Error("Z3 returned no symbol".into()))?;
            let st = self.z3_sort(sort)?;
            self.keep(Z3_mk_const(self.c, sym, st), sort)
        }
    }

    /// `z3.BoolVal(b)`.
    pub fn bool_val(&self, b: bool) -> Z3Result<Term> {
        unsafe { self.keep(if b { Z3_mk_true(self.c) } else { Z3_mk_false(self.c) }, Sort::Bool) }
    }

    /// `Z3_mk_numeral(text)` of an Int or a Real sort: `IntVal` passes
    /// `str(int)`, `RealVal` passes `str(float)` or `str(int)`.
    pub fn numeral(&self, text: &str, sort: Sort) -> Z3Result<Term> {
        let t = Self::cstr(text)?;
        let st = self.z3_sort(sort)?;
        unsafe { self.keep(Z3_mk_numeral(self.c, t.as_ptr(), st), sort) }
    }

    /// `z3.StringVal(s)`: printable ASCII as is, everything else as `\u{hex}`.
    pub fn string_val(&self, s: &str) -> Z3Result<Term> {
        self.string_val_chars(s.chars().map(|c| c as u32))
    }

    /// `z3.StringVal` of a Python str given as code points, which may hold
    /// lone surrogates.
    pub fn string_val_chars(&self, cps: impl IntoIterator<Item = u32>) -> Z3Result<Term> {
        let mut out = String::new();
        for c in cps {
            if (32..127).contains(&c) {
                out.push(c as u8 as char);
            } else {
                out.push_str(&format!("\\u{{{c:x}}}"));
            }
        }
        let t = Self::cstr(&out)?;
        unsafe { self.keep(Z3_mk_string(self.c, t.as_ptr()), Sort::Str) }
    }

    /// Two arithmetic terms joined the way z3py joins them: an Int next to
    /// a Real becomes a Real. Any other mix is z3py's `sort mismatch`.
    fn coerce(&self, a: Term, b: Term) -> Z3Result<(Term, Term)> {
        if a.sort == b.sort {
            return Ok((a, b));
        }
        let to_real = |t: Term| -> Z3Result<Term> { unsafe { self.keep(Z3_mk_int2real(self.c, t.ast), Sort::Real) } };
        // A Bool joins arithmetic as If(b, 1, 0), ToReal'd for a Real.
        let to_int = |t: Term| -> Z3Result<Term> {
            let one = self.numeral("1", Sort::Int)?;
            let zero = self.numeral("0", Sort::Int)?;
            unsafe { self.keep(Z3_mk_ite(self.c, t.ast, one.ast, zero.ast), Sort::Int) }
        };
        match (a.sort, b.sort) {
            (Sort::Int, Sort::Real) => Ok((to_real(a)?, b)),
            (Sort::Real, Sort::Int) => Ok((a, to_real(b)?)),
            (Sort::Bool, Sort::Int) => Ok((to_int(a)?, b)),
            (Sort::Int, Sort::Bool) => Ok((a, to_int(b)?)),
            (Sort::Bool, Sort::Real) => Ok((to_real(to_int(a)?)?, b)),
            (Sort::Real, Sort::Bool) => Ok((a, to_real(to_int(b)?)?)),
            _ => Err(Z3Error("sort mismatch".into())),
        }
    }

    /// `a == b`.
    pub fn eq(&self, a: Term, b: Term) -> Z3Result<Term> {
        let (a, b) = self.coerce(a, b)?;
        unsafe { self.keep(Z3_mk_eq(self.c, a.ast, b.ast), Sort::Bool) }
    }

    /// `a != b`, which z3py builds as `Distinct(a, b)`.
    pub fn ne(&self, a: Term, b: Term) -> Z3Result<Term> {
        let (a, b) = self.coerce(a, b)?;
        let args = [a.ast, b.ast];
        unsafe { self.keep(Z3_mk_distinct(self.c, 2, args.as_ptr()), Sort::Bool) }
    }

    /// `a >= b`, `a <= b`, `a > b`.
    pub fn ge(&self, a: Term, b: Term) -> Z3Result<Term> {
        let (a, b) = self.coerce(a, b)?;
        unsafe { self.keep(Z3_mk_ge(self.c, a.ast, b.ast), Sort::Bool) }
    }
    pub fn le(&self, a: Term, b: Term) -> Z3Result<Term> {
        let (a, b) = self.coerce(a, b)?;
        unsafe { self.keep(Z3_mk_le(self.c, a.ast, b.ast), Sort::Bool) }
    }
    pub fn gt(&self, a: Term, b: Term) -> Z3Result<Term> {
        let (a, b) = self.coerce(a, b)?;
        unsafe { self.keep(Z3_mk_gt(self.c, a.ast, b.ast), Sort::Bool) }
    }

    fn bools(ts: &[Term]) -> Z3Result<Vec<Z3_ast>> {
        if ts.iter().any(|t| t.sort != Sort::Bool) {
            return Err(Z3Error("Value cannot be converted into a Z3 Boolean value".into()));
        }
        Ok(ts.iter().map(|t| t.ast).collect())
    }

    /// `z3.And(*ts)`.
    pub fn and(&self, ts: &[Term]) -> Z3Result<Term> {
        if ts.is_empty() {
            return Err(Z3Error("At least one of the arguments must be a Z3 expression or probe".into()));
        }
        let v = Self::bools(ts)?;
        unsafe { self.keep(Z3_mk_and(self.c, v.len() as u32, v.as_ptr()), Sort::Bool) }
    }

    /// `z3.Or(*ts)`.
    pub fn or(&self, ts: &[Term]) -> Z3Result<Term> {
        if ts.is_empty() {
            return Err(Z3Error("At least one of the arguments must be a Z3 expression or probe".into()));
        }
        let v = Self::bools(ts)?;
        unsafe { self.keep(Z3_mk_or(self.c, v.len() as u32, v.as_ptr()), Sort::Bool) }
    }

    /// `z3.Not(t)`.
    pub fn not(&self, t: Term) -> Z3Result<Term> {
        Self::bools(&[t])?;
        unsafe { self.keep(Z3_mk_not(self.c, t.ast), Sort::Bool) }
    }

    /// `z3.Implies(a, b)`.
    pub fn implies(&self, a: Term, b: Term) -> Z3Result<Term> {
        Self::bools(&[a, b])?;
        unsafe { self.keep(Z3_mk_implies(self.c, a.ast, b.ast), Sort::Bool) }
    }

    /// `z3.Length(s)`.
    pub fn length(&self, s: Term) -> Z3Result<Term> {
        if s.sort != Sort::Str {
            return Err(Z3Error("Non-sequence passed as a sequence".into()));
        }
        unsafe { self.keep(Z3_mk_seq_length(self.c, s.ast), Sort::Int) }
    }

    /// `z3.InRe(s, re)`.
    pub fn in_re(&self, s: Term, re: Term) -> Z3Result<Term> {
        if s.sort != Sort::Str {
            return Err(Z3Error("Non-sequence passed as a sequence".into()));
        }
        unsafe { self.keep(Z3_mk_seq_in_re(self.c, s.ast, re.ast), Sort::Bool) }
    }

    /// `z3.Re(s)` of a Python str given as code points.
    pub fn re_lit(&self, cps: impl IntoIterator<Item = u32>) -> Z3Result<Term> {
        let s = self.string_val_chars(cps)?;
        unsafe { self.keep(Z3_mk_seq_to_re(self.c, s.ast), Sort::Re) }
    }

    /// `z3.Range(chr(lo), chr(hi))`.
    pub fn re_range(&self, lo: u32, hi: u32) -> Z3Result<Term> {
        let a = self.string_val_chars([lo])?;
        let b = self.string_val_chars([hi])?;
        unsafe { self.keep(Z3_mk_re_range(self.c, a.ast, b.ast), Sort::Re) }
    }

    fn res(ts: &[Term]) -> Z3Result<Vec<Z3_ast>> {
        if ts.iter().any(|t| t.sort != Sort::Re) {
            return Err(Z3Error("All arguments must be regular expressions.".into()));
        }
        Ok(ts.iter().map(|t| t.ast).collect())
    }

    /// `z3.Union(*ts)`: one argument is returned as is.
    pub fn re_union(&self, ts: &[Term]) -> Z3Result<Term> {
        if ts.is_empty() {
            return Err(Z3Error("At least one argument expected.".into()));
        }
        let v = Self::res(ts)?;
        if ts.len() == 1 {
            return Ok(ts[0]);
        }
        unsafe { self.keep(Z3_mk_re_union(self.c, v.len() as u32, v.as_ptr()), Sort::Re) }
    }

    /// `z3.Intersect(*ts)`: one argument is returned as is.
    pub fn re_intersect(&self, ts: &[Term]) -> Z3Result<Term> {
        if ts.is_empty() {
            return Err(Z3Error("At least one argument expected.".into()));
        }
        let v = Self::res(ts)?;
        if ts.len() == 1 {
            return Ok(ts[0]);
        }
        unsafe { self.keep(Z3_mk_re_intersect(self.c, v.len() as u32, v.as_ptr()), Sort::Re) }
    }

    /// `z3.Concat(*ts)` of regexes: fewer than two is z3py's assertion.
    pub fn re_concat(&self, ts: &[Term]) -> Z3Result<Term> {
        if ts.len() < 2 {
            return Err(Z3Error("At least two arguments expected.".into()));
        }
        let v = Self::res(ts)?;
        unsafe { self.keep(Z3_mk_re_concat(self.c, v.len() as u32, v.as_ptr()), Sort::Re) }
    }

    pub fn re_star(&self, t: Term) -> Z3Result<Term> {
        unsafe { self.keep(Z3_mk_re_star(self.c, t.ast), Sort::Re) }
    }
    pub fn re_plus(&self, t: Term) -> Z3Result<Term> {
        unsafe { self.keep(Z3_mk_re_plus(self.c, t.ast), Sort::Re) }
    }
    pub fn re_option(&self, t: Term) -> Z3Result<Term> {
        unsafe { self.keep(Z3_mk_re_option(self.c, t.ast), Sort::Re) }
    }
    pub fn re_complement(&self, t: Term) -> Z3Result<Term> {
        unsafe { self.keep(Z3_mk_re_complement(self.c, t.ast), Sort::Re) }
    }
    /// `z3.Loop(t, lo, hi)`.
    pub fn re_loop(&self, t: Term, lo: u32, hi: u32) -> Z3Result<Term> {
        unsafe { self.keep(Z3_mk_re_loop(self.c, t.ast, lo, hi), Sort::Re) }
    }

    /// `z3.Solver()` with `solver.set("timeout", ms)`, the terms added, and
    /// `solver.check()`. Returns the result and, for unsat, the size of the
    /// unsat core (always empty: no term is tracked).
    pub fn check(&mut self, timeout_ms: u32, terms: &[Term]) -> Z3Result<Check> {
        unsafe {
            let s = Z3_mk_solver(self.c);
            self.err()?;
            let s = s.ok_or_else(|| Z3Error("Z3 returned no solver".into()))?;
            Z3_solver_inc_ref(self.c, s);
            self.solvers.push(s);
            let p = Z3_mk_params(self.c);
            self.err()?;
            let p = p.ok_or_else(|| Z3Error("Z3 returned no params".into()))?;
            Z3_params_inc_ref(self.c, p);
            let key = Self::cstr("timeout")?;
            let sym = Z3_mk_string_symbol(self.c, key.as_ptr());
            self.err()?;
            let sym = sym.ok_or_else(|| Z3Error("Z3 returned no symbol".into()))?;
            Z3_params_set_uint(self.c, p, sym, timeout_ms);
            Z3_solver_set_params(self.c, s, p);
            Z3_params_dec_ref(self.c, p);
            self.err()?;
            for t in terms {
                if t.sort != Sort::Bool {
                    return Err(Z3Error("Value cannot be converted into a Z3 Boolean value".into()));
                }
                Z3_solver_assert(self.c, s, t.ast);
                self.err()?;
            }
            let r = Z3_solver_check(self.c, s);
            self.err()?;
            Ok(match r {
                Z3_L_TRUE => Check::Sat,
                Z3_L_FALSE => Check::Unsat,
                _ => Check::Unknown,
            })
        }
    }
}

/// The process's one context, as z3py's `main_ctx()`: every check shares
/// it, so a name means the same constant in each, as in the oracle. Making
/// a context costs about 9 ms, so `prewarm` can start it early on a thread
/// of its own.
static SHARED: std::sync::OnceLock<std::sync::Mutex<Option<Ctx>>> = std::sync::OnceLock::new();

fn shared() -> &'static std::sync::Mutex<Option<Ctx>> {
    SHARED.get_or_init(|| std::sync::Mutex::new(None))
}

/// Starts making the shared context on a thread of its own.
pub fn prewarm() {
    let _ = std::thread::Builder::new().spawn(|| {
        let _ = with_main(|_| ());
    });
}

/// Runs `f` on the shared context, made on first use.
pub fn with_main<T>(f: impl FnOnce(&mut Ctx) -> T) -> Z3Result<T> {
    let mut g = shared().lock().map_err(|_| Z3Error("the Z3 context lock is poisoned".into()))?;
    if g.is_none() {
        *g = Some(Ctx::new()?);
    }
    Ok(f(g.as_mut().expect("made above")))
}

/// Runs an SMT-LIB2 script on a fresh context of the linked Z3, as
/// `z3 -in` would, and returns what it prints.
pub fn eval_smtlib2(script: &str) -> Z3Result<String> {
    let c = Ctx::new()?;
    let s = Ctx::cstr(script)?;
    unsafe {
        let out = Z3_eval_smtlib2_string(c.c, s.as_ptr());
        c.err()?;
        if out.is_null() {
            return Err(Z3Error("Z3 printed nothing".into()));
        }
        Ok(CStr::from_ptr(out).to_string_lossy().into_owned())
    }
}

/// Python's `str()` of an int, for `IntVal`.
pub fn int_text(v: i128) -> String {
    v.to_string()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_linked_z3_decides_a_small_formula() {
        let mut c = Ctx::new().unwrap();
        let x = c.constant("shell", Sort::Bool).unwrap();
        let t = c.bool_val(true).unwrap();
        let f = c.bool_val(false).unwrap();
        let a = c.eq(x, t).unwrap();
        let b = c.eq(x, f).unwrap();
        assert_eq!(c.check(500, &[a]).unwrap(), Check::Sat);
        assert_eq!(c.check(500, &[a, b]).unwrap(), Check::Unsat);
    }

    #[test]
    fn numerals_parse_as_z3py_passes_them() {
        let mut c = Ctx::new().unwrap();
        let r = c.constant("r", Sort::Real).unwrap();
        let v = c.numeral("1e+20", Sort::Real).unwrap();
        let e = c.eq(r, v).unwrap();
        assert_eq!(c.check(500, &[e]).unwrap(), Check::Sat);
        assert_eq!(c.numeral("inf", Sort::Real).unwrap_err().0, "b'parser error'");
        let i = c.numeral("3", Sort::Int).unwrap();
        let e = c.eq(r, i).unwrap();
        assert_eq!(c.check(500, &[e]).unwrap(), Check::Sat);
    }

    #[test]
    fn strings_and_regexes() {
        let mut c = Ctx::new().unwrap();
        let s = c.constant("s", Sort::Str).unwrap();
        let lit = c.string_val("ab\u{e9}").unwrap();
        let len = c.length(lit).unwrap();
        let three = c.numeral("3", Sort::Int).unwrap();
        let e = c.eq(len, three).unwrap();
        assert_eq!(c.check(500, &[e]).unwrap(), Check::Sat);
        let digit = c.re_range('0' as u32, '9' as u32).unwrap();
        let plus = c.re_plus(digit).unwrap();
        let m = c.in_re(s, plus).unwrap();
        let x = c.string_val("x").unwrap();
        let is_x = c.eq(s, x).unwrap();
        assert_eq!(c.check(500, &[m, is_x]).unwrap(), Check::Unsat);
        let one = c.re_lit([97]).unwrap();
        assert_eq!(c.re_concat(&[one]).unwrap_err().0, "At least two arguments expected.");
        let b = c.bool_val(true).unwrap();
        assert_eq!(c.eq(s, b).unwrap_err().0, "sort mismatch");
    }
}

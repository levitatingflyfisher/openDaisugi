package z3

import (
	"strings"
	"testing"
)

func TestVersionIsPinned(t *testing.T) {
	if v := Version(); !strings.HasPrefix(v, "5.1.0") {
		t.Fatalf("Z3 version %q, want 5.1.0", v)
	}
}

func TestSelfConsistencyShape(t *testing.T) {
	Lock()
	defer Unlock()
	s := NewSolver(500)
	shell := BoolConst("shell")
	maxTime := IntConst("max_time")
	s.Add(Eq(shell, BoolVal(false)), Eq(shell, BoolVal(true)))
	s.Add(Eq(maxTime, IntVal("60")), Gt(maxTime, IntVal("0")), Le(maxTime, IntVal("3600")))
	if r := s.Check(); r != Unsat {
		t.Fatalf("got %v, want unsat", r)
	}
	s2 := NewSolver(500)
	s2.Add(Eq(maxTime, IntVal("60")), Gt(maxTime, IntVal("0")))
	if r := s2.Check(); r != Sat {
		t.Fatalf("got %v, want sat", r)
	}
}

func TestRegexMembership(t *testing.T) {
	Lock()
	defer Unlock()
	x := StringConst("x")
	re := Concat(Star(Range(0, 0xFFFF)), Re([]rune("ab")), Star(Range(0, 0xFFFF)))
	s := NewSolver(500)
	s.Add(InRe(x, re), Not(InRe(x, Plus(Range('a', 'b')))))
	if r := s.Check(); r != Sat {
		t.Fatalf("got %v", r)
	}
}

func TestBadNumeralRaises(t *testing.T) {
	Lock()
	defer Unlock()
	defer func() {
		if _, ok := recover().(*Exception); !ok {
			t.Fatal("want a Z3 exception")
		}
	}()
	RealVal("inf")
}

func TestConcatOfOneRaisesAsZ3pyDoes(t *testing.T) {
	Lock()
	defer Unlock()
	defer func() {
		e, ok := recover().(*Exception)
		if !ok || e.Msg != "At least two arguments expected." {
			t.Fatalf("got %v", e)
		}
	}()
	Concat(Re([]rune("a")))
}

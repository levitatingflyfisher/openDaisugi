package z3

import (
	"strings"
	"testing"
	"time"
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

// Z3's command interpreter swaps in its own SIGINT handler for the length of
// a check-sat, set with signal(), which drops SA_ONSTACK. A Ctrl-C in that
// window kills a Go process ("non-Go code set up signal handler without
// SA_ONSTACK flag"). The context turns the handler off with the global
// ctrl_c parameter, so Go's disposition must hold for the whole check.
func TestASlowCheckLeavesGosSigintDispositionAlone(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		EvalSMTLIB2("(set-option :timeout 400)(declare-const x Int)" +
			"(assert (= (* x x x x x) 123456789012345678901234567))(check-sat)")
	}()
	polls := 0
	for {
		select {
		case <-done:
			if polls < 20 {
				t.Fatalf("the check ended after %d polls: too fast to prove anything", polls)
			}
			return
		default:
			polls++
			if sigintFlags()&saOnstack == 0 {
				t.Fatalf("SIGINT lost SA_ONSTACK while Z3 solved (poll %d)", polls)
			}
			time.Sleep(50 * time.Microsecond)
		}
	}
}

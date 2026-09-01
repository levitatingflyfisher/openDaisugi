package gate

import (
	"testing"

	"daisugi-verify/internal/z3"
)

// The gate decides z3_checks.check_envelope_self_consistency and
// check_plan_against_envelope directly: every term is a pinned constant,
// so Z3 answers sat or unsat at once. This test asks Z3 the oracle's own
// formulas, built the way z3py builds them, over every combination of the
// inputs they read, and checks the direct answers agree.
func TestGroundChecksAgreeWithZ3(t *testing.T) {
	z3.Lock()
	defer z3.Unlock()
	times := []string{"-5", "0", "1", "30", "3600", "3601", "100000000000000000000000", "-100000000000000000000000"}
	postconditions := [][]predicateItem{nil, {{Type: "file_exists"}}, {{Type: "other"}}, {{Type: "other"}, {Type: "file_exists"}}}
	n := 0
	for _, shell := range []bool{false, true} {
		for _, writes := range [][]string{nil, {"/work/**"}} {
			for _, allow := range [][]string{nil, {"ls"}} {
				for _, pcs := range postconditions {
					for _, tm := range times {
						env := &envelope{Shell: shell, FileWrite: writes, ShellAllowlist: allow,
							Postconditions: pcs, MaxExecutionTimeS: tm}
						// check_envelope_self_consistency
						s := z3.NewSolver(500)
						sh, cw := z3.BoolConst("shell"), z3.BoolConst("can_write")
						s.Add(z3.Eq(sh, z3.BoolVal(shell)))
						s.Add(z3.Eq(cw, z3.BoolVal(len(writes) > 0)))
						if len(allow) > 0 {
							s.Add(z3.Eq(sh, z3.BoolVal(true)))
						}
						for _, pc := range pcs {
							if pc.Type == "file_exists" {
								s.Add(z3.Eq(cw, z3.BoolVal(true)))
							}
						}
						mt := z3.IntConst("max_time")
						s.Add(z3.Eq(mt, z3.IntVal(tm)))
						s.Add(z3.Gt(mt, z3.IntVal("0")))
						s.Add(z3.Le(mt, z3.IntVal("3600")))
						r := s.Check()
						if r == z3.Unknown {
							t.Fatal("Z3 answered unknown on a ground formula")
						}
						if (r == z3.Sat) != selfConsistent(env) {
							t.Errorf("self-consistency %+v: Z3 %v, direct %v", env, r, selfConsistent(env))
						}
						n++
						// check_plan_against_envelope, for each step type the gate makes
						for _, step := range []string{"shell", "file_read", "file_write", "network", "mcp"} {
							s := z3.NewSolver(500)
							sa, wa := z3.BoolConst("shell_available"), z3.BoolConst("write_available")
							s.Add(z3.Eq(sa, z3.BoolVal(shell)))
							s.Add(z3.Eq(wa, z3.BoolVal(len(writes) > 0)))
							if step == "shell" {
								s.Add(z3.Eq(sa, z3.BoolVal(true)))
							}
							if step == "file_write" {
								s.Add(z3.Eq(wa, z3.BoolVal(true)))
							}
							r := s.Check()
							if r == z3.Unknown || (r == z3.Sat) != planReachable(step, env) {
								t.Errorf("plan %s %+v: Z3 %v, direct %v", step, env, r, planReachable(step, env))
							}
							n++
						}
					}
				}
			}
		}
	}
	t.Logf("%d formulas agree", n)
}

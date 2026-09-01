package verify

import (
	"encoding/json"
	"math/big"
	"regexp"
	"testing"

	"daisugi-verify/internal/z3"
)

// Subsumption covers stakes, custom step types and both budgets, as the
// oracle's envelope_subsumes does: each grants authority the Z3 formula
// cannot see.

func fieldsEnv(stakes string) Envelope {
	e := DefaultEnvelope()
	e.Stakes = stakes
	e.Permissions.Shell = true
	e.Permissions.ShellAllowlist = []string{"ls"}
	return e
}

var z3Marker = regexp.MustCompile(`\(echo "(DAISUGI-MARK-\d+)"\)`)

func linkedZ3() *Z3Client { return &Z3Client{eval: z3.EvalSMTLIB2} }

func TestSubsumptionNeverLowersTheStakes(t *testing.T) {
	order := []string{"low", "medium", "high", "physical"}
	for oi, o := range order {
		for ii, i := range order {
			holds, err := EnvelopeSubsumes(linkedZ3(), fieldsEnv(o), fieldsEnv(i), 2000, false)
			if err != nil || holds != (ii >= oi) {
				t.Fatalf("outer %s inner %s: holds=%v err=%v", o, i, holds, err)
			}
		}
	}
	if holds, _ := EnvelopeSubsumes(linkedZ3(), fieldsEnv("low"), fieldsEnv("extreme"), 2000, false); holds {
		t.Fatal("a stakes value subsumption does not know must not hold")
	}
}

func TestSubsumptionChecksTheCustomStepList(t *testing.T) {
	outer, inner := fieldsEnv("low"), fieldsEnv("low")
	outer.Permissions.CustomStepAllowlist = []string{"a", "b"}
	inner.Permissions.CustomStepAllowlist = []string{"b"}
	if holds, err := EnvelopeSubsumes(linkedZ3(), outer, inner, 2000, false); err != nil || !holds {
		t.Fatalf("a narrower list must hold: %v %v", holds, err)
	}
	inner.Permissions.CustomStepAllowlist = []string{"b", "launch"}
	if holds, _ := EnvelopeSubsumes(linkedZ3(), outer, inner, 2000, false); holds {
		t.Fatal("a wider list must not hold")
	}
}

func TestSubsumptionChecksTheBudgets(t *testing.T) {
	outer, inner := fieldsEnv("low"), fieldsEnv("low")
	outer.Permissions.MaxExecutionTimeS = big.NewInt(10)
	if holds, _ := EnvelopeSubsumes(linkedZ3(), outer, inner, 2000, false); holds {
		t.Fatal("a default 30 s callee under a 10 s caller must not hold")
	}
	inner.Permissions.MaxExecutionTimeS = big.NewInt(10)
	if holds, err := EnvelopeSubsumes(linkedZ3(), outer, inner, 2000, false); err != nil || !holds {
		t.Fatalf("an equal time must hold: %v %v", holds, err)
	}
	for _, c := range []struct {
		outer, inner string
		holds        bool
	}{{"10", "10", true}, {"10", "9", true}, {"10", "11", false}, {"10", "10.0", true},
		{"10", "1e1", true}, {"10", "1e2", false}, {"10", "", false}, {"99999999999999999999", "99999999999999999998", true}} {
		o, i := fieldsEnv("low"), fieldsEnv("low")
		o.Permissions.MaxOutputSizeMB = json.Number(c.outer)
		i.Permissions.MaxOutputSizeMB = json.Number(c.inner)
		holds, err := EnvelopeSubsumes(linkedZ3(), o, i, 2000, false)
		if err != nil || holds != c.holds {
			t.Fatalf("output %s under %s: holds=%v err=%v", c.inner, c.outer, holds, err)
		}
	}
}

// A contract whose final check answers unknown is a violation under
// lenient mode too, and its text is kept as a timeout, not a warning.
func TestADelegationTimeoutIsAViolationInEveryMode(t *testing.T) {
	_, _ = sharedZ3() // run the once, then put a stand-in in its place
	oldC, oldErr := z3Shared, z3Err
	defer func() { z3Shared, z3Err = oldC, oldErr }()
	z3Err = nil
	z3Shared = &Z3Client{eval: func(cmds string) (string, error) {
		m := z3Marker.FindStringSubmatch(cmds)
		if m == nil {
			return "", nil
		}
		return "unknown\n" + m[1] + "\n", nil
	}}
	for _, strict := range []bool{false, true} {
		env := fieldsEnv("low")
		contract := fieldsEnv("low")
		plan := ActionPlan{Steps: []Step{stepRaw("k1", "skill", map[string]interface{}{
			"skill_id": "s", "skill_input": map[string]interface{}{}, "contract_envelope": contract})}}
		var warnings, timeouts []string
		v := CheckSkillDelegations(plan, env, strict, 7, &warnings, &timeouts, nil)
		if len(v) != 1 || v[0].Message != "verifier timed out (skill-delegation subsumption); raise the Z3 timeout" {
			t.Fatalf("strict=%v: want one timeout violation, got %+v", strict, v)
		}
		if len(warnings) != 0 || len(timeouts) != 1 || timeouts[0] != "Z3 subsumption check exceeded 7ms" {
			t.Fatalf("strict=%v: warnings %v timeouts %v", strict, warnings, timeouts)
		}
	}
}

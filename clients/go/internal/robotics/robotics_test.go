//go:build mujoco

package robotics

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
)

func fixture(name string) string {
	_, here, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(here), "../../../../tests/fixtures/mjcf", name)
}

// step validates one step the way ActionPlan.steps does.
func step(t *testing.T, text string) *pyjson.Object {
	t.Helper()
	raw, err := pyjson.Loads(`{"source": "t", "task": "t", "steps": [` + text + `]}`)
	if err != nil {
		t.Fatal(err)
	}
	v, verr := pmodel.Validate("ActionPlan", pmodel.ActionPlan, raw, pmodel.Python)
	if verr != nil {
		t.Fatal(verr)
	}
	return v.(*pyjson.Object).Value("steps").([]any)[0].(*pyjson.Object)
}

// The damped least-squares update numpy computes: J J^T by fused
// multiply-adds, the solve by the left-looking LU of OpenBLAS, then J^T x.
// The vectors are numpy's own answers.
func TestDampedStepMatchesNumpy(t *testing.T) {
	cases := []struct {
		jacp    []float64
		err     [3]float64
		damping float64
		dq      []float64
	}{
		{jacp: []float64{-0.35233447033367526, -0.6983016521509962, 0.3018689460797075, -0.8551274266649145, 0.0717640086133784, -0.2686221661748289}, err: [3]float64{-0.17680043009011728, 0.0029742932757680918, -0.18500173662320607}, damping: 0.1, dq: []float64{0.23525914250335522, 0.13681062611887018}},
		{jacp: []float64{-0.13270863267522826, -0.8602891528507621, -0.8185739733122699, -0.15096162171497207, 0.6537042493440761, -0.7523960777007088, -0.5535220707859709, 0.2548664448111786, 0.8954178849140113}, err: [3]float64{0.03084117944699946, -0.04132781013968795, 0.19050204223716805}, damping: 0.1, dq: []float64{-0.2599415834835072, -0.0525509041310724, 0.06274993525161676}},
	}
	for i, c := range cases {
		got := dampedStep(c.jacp, c.err, c.damping*c.damping)
		for j := range c.dq {
			if got[j] != c.dq[j] {
				t.Fatalf("case %d: dq = %v, want %v", i, got, c.dq)
			}
		}
	}
}

func TestReprs(t *testing.T) {
	if got := tupleRepr([]float64{0.35, 0.2, 0}); got != "(0.35, 0.2, 0.0)" {
		t.Fatal(got)
	}
	if got := pairsRepr([][2]string{{"tip", "wall"}, {"geom#0", "it's"}}); got != `[('tip', 'wall'), ('geom#0', "it's")]` {
		t.Fatal(got)
	}
	if got := (Num{V: pyjson.Float(0.1)}).Repr(); got != "0.1" {
		t.Fatal(got)
	}
	if got := (Num{V: pyjson.Int{Text: "50"}}).Repr(); got != "50" {
		t.Fatal(got)
	}
}

func newArm(t *testing.T, o Options) *MuJoCo {
	t.Helper()
	x, err := NewMuJoCo(fixture("two_joint_arm.xml"), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(x.Close)
	return x
}

func TestMissingMJCF(t *testing.T) {
	_, err := NewMuJoCo(fixture("absent.xml"), DefaultOptions())
	pe, ok := err.(*PyError)
	if !ok || pe.Type != "ValueError" || !strings.Contains(pe.Msg, "Error opening file") {
		t.Fatalf("err = %#v", err)
	}
}

func TestJointMoveAndReset(t *testing.T) {
	x := newArm(t, DefaultOptions())
	r, err := x.Run(step(t, `{"type": "joint_move", "id": "m", "joint_targets": {"j1": 0.5}}`), 30, 1<<20)
	if err != nil || r.RC != 0 || !strings.HasPrefix(r.Stdout, "joint_move complete; positions={'j1': 0.49") {
		t.Fatalf("%v %+v", err, r)
	}
	r, err = x.Run(step(t, `{"type": "sim_reset", "id": "r", "seed": 3}`), 30, 1<<20)
	if err != nil || r.Stdout != "sim reset (seed=3)" || x.Data().QPos()[0] != 0 {
		t.Fatalf("%v %+v", err, r)
	}
}

func TestUnknownJointIsKeyError(t *testing.T) {
	x := newArm(t, DefaultOptions())
	_, err := x.Run(step(t, `{"type": "joint_move", "id": "m", "joint_targets": {"nope": 0.5}}`), 30, 1<<20)
	pe, ok := err.(*PyError)
	if !ok || pe.Type != "KeyError" || pe.Error() != `"Joint 'nope' not found in MJCF"` {
		t.Fatalf("err = %#v", err)
	}
}

func TestGripperAndTorque(t *testing.T) {
	o := DefaultOptions()
	o.TorqueLimit = &Num{V: pyjson.Float(0.01)}
	x := newArm(t, o)
	r, err := x.Run(step(t, `{"type": "gripper", "id": "g", "action": "close"}`), 30, 1<<20)
	if err != nil || r.RC != RCTorqueViolation || !strings.HasPrefix(r.Stdout, "torque_limit violated during gripper g: peak |actuator_force|=") ||
		!strings.HasSuffix(r.Stdout, " > limit=0.01") {
		t.Fatalf("%v %+v", err, r)
	}
}

func TestCartesianIKFailure(t *testing.T) {
	x := newArm(t, DefaultOptions())
	r, err := x.Run(step(t, `{"type": "cartesian_move", "id": "c", "target_position": [2, 2, 0]}`), 30, 1<<20)
	if err != nil || r.RC != RCIKFailed || r.Stdout != "cartesian_move c IK failed: did not converge in 200 iters (residual=3.0752, tol=0.001)" {
		t.Fatalf("%v %+v", err, r)
	}
}

func TestStepTypeRefused(t *testing.T) {
	x := newArm(t, DefaultOptions())
	_, err := x.Run(step(t, `{"type": "vla", "id": "v", "task": "wave"}`), 30, 1<<20)
	pe, ok := err.(*PyError)
	if !ok || pe.Type != "TypeError" || pe.Msg != "MuJoCoExecutor cannot run step of type VLAStep" {
		t.Fatalf("err = %#v", err)
	}
}

func TestConfigure(t *testing.T) {
	x := newArm(t, DefaultOptions())
	env := pyjson.NewObject().Set("permissions", pyjson.NewObject().Set("torque_limit", 2.5).
		Set("obstacles", []any{[]any{[]any{1.0, 1.0, 1.0}, []any{2.0, 2.0, 2.0}}}))
	x.Configure(env)
	if x.Opt.TorqueLimit == nil || x.Opt.TorqueLimit.Repr() != "2.5" || !x.Opt.ForbidContacts {
		t.Fatalf("%+v", x.Opt)
	}
}

func TestMockVLA(t *testing.T) {
	v, err := NewMockVLA(fixture("two_joint_arm.xml"), 10)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	r, err := v.Run(step(t, `{"type": "vla", "id": "v", "task": "reach", "target_pose": [0.3, -0.2, 0]}`), 30, 1<<20)
	if err != nil || r.RC != 0 {
		t.Fatalf("%v %+v", err, r)
	}
	var ev map[string]any
	if err := json.Unmarshal([]byte(r.Stdout), &ev); err != nil {
		t.Fatal(err)
	}
	if ev["actions_requested"] != 10.0 || ev["actions_executed"] != 10.0 || ev["contact_count"] != 0.0 {
		t.Fatalf("%v", ev)
	}
	if !strings.HasPrefix(r.Stdout, `{"task": "reach", "actions_requested": 10, "actions_executed": 10, "max_actions": 50, "observation_initial": {"qpos": [0.0, 0.0, 0.0]`) {
		t.Fatal(r.Stdout)
	}
	r, _ = v.Run(step(t, `{"type": "joint_move", "id": "m", "joint_targets": {"j1": 0.1}}`), 30, 1<<20)
	if r.RC != 1 || r.Stdout != "VLAExecutorBase: not a VLAStep (JointMoveStep)" {
		t.Fatalf("%+v", r)
	}
}

func TestBaseVLA(t *testing.T) {
	v, err := NewVLABase(fixture("two_joint_arm.xml"), 200, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	r, _ := v.Run(step(t, `{"type": "vla", "id": "v", "task": "reach"}`), 30, 1<<20)
	if r.RC != 1 || r.Stdout != "VLAExecutorBase._predict_actions not implemented" {
		t.Fatalf("%+v", r)
	}
	none, _ := NewVLABase("", 200, nil)
	r, _ = none.Run(step(t, `{"type": "vla", "id": "v", "task": "reach"}`), 30, 1<<20)
	if r.RC != 1 || r.Stdout != "VLAExecutorBase: no MJCF loaded; pass mjcf_path" {
		t.Fatalf("%+v", r)
	}
}

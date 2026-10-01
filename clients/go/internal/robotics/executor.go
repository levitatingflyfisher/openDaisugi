//go:build mujoco

// Package robotics is executor_mujoco.py and vla_executor.py: the step
// executors that run robot steps against a MuJoCo simulation, and the
// scaffolding for a Vision-Language-Action policy with its deterministic
// mock. The oracle's commands build none of them (run, orchestrate and
// weave fail a robot step with "no executor for kind ..."); a caller hands
// them to a supervisor, as a Python caller hands robotics_executors to
// Supervisor. The package builds only with the mujoco build tag.
package robotics

import (
	"fmt"
	"math"
	"strings"
	"time"

	"daisugi-verify/internal/mujoco"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/supervise"
)

// Return codes for rollout-time violations (executor_mujoco.RC_*).
const (
	RCOK               = 0
	RCTorqueViolation  = 3
	RCContactViolation = 4
	RCIKFailed         = 5
)

// Options are MuJoCoExecutor's keyword arguments.
type Options struct {
	SettleSteps int
	PositionTol float64
	// TorqueLimit is nil for None.
	TorqueLimit    *Num
	ForbidContacts bool
	EEBody         string
	IKMaxIter      int
	IKTol          Num
	IKDamping      float64
}

// DefaultOptions are the constructor's defaults.
func DefaultOptions() Options {
	return Options{SettleSteps: 2000, PositionTol: 0.05, EEBody: "end_effector", IKMaxIter: 200,
		IKTol: Num{V: pyjson.Float(1e-3)}, IKDamping: 0.1}
}

// MuJoCo is MuJoCoExecutor: one model and one data, kept across steps so a
// joint_move starts where the last step left the arm.
type MuJoCo struct {
	Opt   Options
	MJCF  string
	model *mujoco.Model
	data  *mujoco.Data
}

// NewMuJoCo loads the MJCF at path. A file MuJoCo cannot load is the
// ValueError MjModel.from_xml_path raises, with MuJoCo's message.
func NewMuJoCo(path string, o Options) (*MuJoCo, error) {
	m, err := mujoco.LoadXML(path)
	if err != nil {
		return nil, &PyError{Type: "ValueError", Msg: err.Error()}
	}
	return &MuJoCo{Opt: o, MJCF: path, model: m, data: mujoco.NewData(m)}, nil
}

// Close frees the simulation.
func (x *MuJoCo) Close() {
	x.data.Close()
	x.model.Close()
}

// Model and Data are the simulation, for a caller that reads its state.
func (x *MuJoCo) Model() *mujoco.Model { return x.model }
func (x *MuJoCo) Data() *mujoco.Data   { return x.data }

// Configure is configure_from_envelope: an envelope's torque_limit
// replaces the executor's, and a non-empty obstacles list forbids
// contacts. What the envelope leaves out stays as it was.
func (x *MuJoCo) Configure(env *pyjson.Object) {
	perms, _ := env.Value("permissions").(*pyjson.Object)
	if perms == nil {
		return
	}
	switch v := perms.Value("torque_limit").(type) {
	case float64:
		x.Opt.TorqueLimit = &Num{V: pyjson.Float(v)}
	case pyjson.Float, pyjson.Int:
		x.Opt.TorqueLimit = &Num{V: v}
	}
	if obs, ok := perms.Value("obstacles").([]any); ok && len(obs) > 0 {
		x.Opt.ForbidContacts = true
	}
}

// outcome is _RolloutOutcome.
type outcome struct {
	stdout string
	rc     int
}

// Run is MuJoCoExecutor.run. An error is the exception the oracle raises.
func (x *MuJoCo) Run(step *pyjson.Object, _, _ int) (supervise.ExecResult, error) {
	start := time.Now()
	var o outcome
	var err error
	switch strOf(step, "type") {
	case "sim_reset":
		o = x.reset(step)
	case "joint_move":
		o, err = x.jointMove(step)
	case "gripper":
		o, err = x.gripper(step)
	case "cartesian_move":
		o = x.cartesianMove(step)
	default:
		err = &PyError{Type: "TypeError", Msg: "MuJoCoExecutor cannot run step of type " + className(step)}
	}
	if err != nil {
		return supervise.ExecResult{}, err
	}
	return supervise.ExecResult{RC: o.rc, Stdout: o.stdout,
		DurationMs: float64(time.Since(start).Nanoseconds()) / 1e6}, nil
}

func (x *MuJoCo) reset(step *pyjson.Object) outcome {
	mujoco.Reset(x.model, x.data)
	mujoco.Forward(x.model, x.data)
	seed := "None"
	if s, ok := step.Value("seed").(pyjson.Int); ok {
		seed = s.Text
	}
	return outcome{stdout: "sim reset (seed=" + seed + ")"}
}

func (x *MuJoCo) jointMove(step *pyjson.Object) (outcome, error) {
	targets, _ := step.Value("joint_targets").(*pyjson.Object)
	names := targets.Keys()
	for _, name := range names {
		aid, err := x.actuatorForJoint(name)
		if err != nil {
			return outcome{}, err
		}
		x.data.Ctrl()[aid] = floatOf(targets.Value(name))
	}
	if v := x.stepWithGuards(x.Opt.SettleSteps, "joint_move "+strOf(step, "id")); v != nil {
		return *v, nil
	}
	parts := make([]string, len(names))
	for i, name := range names {
		jid := x.model.Name2ID(mujoco.ObjJoint, name)
		parts[i] = pystr.Repr(name) + ": " + reprF(x.data.QPos()[x.model.JntQposAdr(jid)])
	}
	return outcome{stdout: "joint_move complete; positions={" + strings.Join(parts, ", ") + "}"}, nil
}

func (x *MuJoCo) gripper(step *pyjson.Object) (outcome, error) {
	action := strOf(step, "action")
	var applied []string
	for aid := 0; aid < x.model.NU(); aid++ {
		name, _ := x.model.ID2Name(mujoco.ObjActuator, aid)
		if !strings.HasPrefix(name, "a_grip") {
			continue
		}
		lo, hi, err := x.gripperRange(aid)
		if err != nil {
			return outcome{}, err
		}
		target := lo
		if action == "open" {
			target = hi
		}
		x.data.Ctrl()[aid] = target
		applied = append(applied, name+"="+reprF(target))
	}
	hold := pyInt(floatOf(step.Value("hold_s")) / x.model.Timestep())
	if hold < 1 {
		hold = 1
	}
	if v := x.stepWithGuards(hold, "gripper "+strOf(step, "id")); v != nil {
		return *v, nil
	}
	return outcome{stdout: "gripper " + action + "; applied=[" + strings.Join(applied, ", ") + "]"}, nil
}

// pyInt is int() of a float: toward zero.
func pyInt(f float64) int { return int(math.Trunc(f)) }

func (x *MuJoCo) cartesianMove(step *pyjson.Object) outcome {
	target := floatsOf(step.Value("target_position"))
	q, err := x.solveIK(target)
	if err != nil {
		return outcome{rc: RCIKFailed, stdout: "cartesian_move " + strOf(step, "id") + " IK failed: " + err.Error()}
	}
	for aid := 0; aid < x.model.NU(); aid++ {
		jid := x.model.ActuatorTrnID(aid)
		if x.model.JntType(jid) == mujoco.JntHinge {
			x.data.Ctrl()[aid] = q[x.model.JntQposAdr(jid)]
		}
	}
	if v := x.stepWithGuards(x.Opt.SettleSteps, "cartesian_move "+strOf(step, "id")); v != nil {
		return *v
	}
	ee := x.data.XPos(x.model.Name2ID(mujoco.ObjBody, x.Opt.EEBody))
	return outcome{stdout: "cartesian_move reached " + tupleRepr(ee[:]) + " (target=" + tupleRepr(target) + ")"}
}

// solveIK is _solve_ik_position: damped least squares on the ee body's
// position, on a scratch copy of the state, forward kinematics only.
func (x *MuJoCo) solveIK(target []float64) ([]float64, error) {
	ee := x.model.Name2ID(mujoco.ObjBody, x.Opt.EEBody)
	if ee < 0 {
		return nil, fmt.Errorf("ee_body %s not found in MJCF", pystr.Repr(x.Opt.EEBody))
	}
	scratch := mujoco.NewData(x.model)
	defer scratch.Close()
	copy(scratch.QPos(), x.data.QPos())
	d2 := x.Opt.IKDamping * x.Opt.IKDamping
	tol := x.Opt.IKTol.F()
	nv := x.model.NV()
	residual := func() ([3]float64, float64) {
		p := scratch.XPos(ee)
		e := [3]float64{target[0] - p[0], target[1] - p[1], target[2] - p[2]}
		return e, norm3(e)
	}
	for it := 0; it < x.Opt.IKMaxIter; it++ {
		mujoco.Forward(x.model, scratch)
		e, n := residual()
		if n < tol {
			return append([]float64(nil), scratch.QPos()...), nil
		}
		dq := dampedStep(mujoco.JacBody(x.model, scratch, ee), e, d2)
		q := scratch.QPos()
		for j := 0; j < nv; j++ {
			q[j] += dq[j]
		}
	}
	mujoco.Forward(x.model, scratch)
	_, n := residual()
	if n < tol {
		return append([]float64(nil), scratch.QPos()...), nil
	}
	return nil, fmt.Errorf("did not converge in %d iters (residual=%.4f, tol=%s)", x.Opt.IKMaxIter, n, x.Opt.IKTol.Repr())
}

// stepWithGuards is _step_with_guards: n physics steps, the torque and
// contact guards after each; nil when no guard fired.
func (x *MuJoCo) stepWithGuards(n int, where string) *outcome {
	for i := 0; i < n; i++ {
		mujoco.Step(x.model, x.data)
		if x.Opt.TorqueLimit != nil {
			peak := 0.0
			for k, f := range x.data.ActuatorForce() {
				if a := math.Abs(f); k == 0 || a > peak {
					peak = a
				}
			}
			if peak > x.Opt.TorqueLimit.F() {
				return &outcome{rc: RCTorqueViolation, stdout: fmt.Sprintf(
					"torque_limit violated during %s: peak |actuator_force|=%.3f > limit=%s", where, peak, x.Opt.TorqueLimit.Repr())}
			}
		}
		if x.Opt.ForbidContacts && x.data.NCon() > 0 {
			nc := x.data.NCon()
			return &outcome{rc: RCContactViolation, stdout: fmt.Sprintf(
				"contact detected during %s (ncon=%d); pairs=%s", where, nc, pairsRepr(x.contactPairs(nc)))}
		}
	}
	return nil
}

func (x *MuJoCo) contactPairs(n int) [][2]string {
	out := make([][2]string, n)
	for i := 0; i < n; i++ {
		g1, g2 := x.data.ContactGeoms(i)
		out[i] = [2]string{x.geomName(g1), x.geomName(g2)}
	}
	return out
}

func (x *MuJoCo) geomName(g int) string {
	if n, ok := x.model.ID2Name(mujoco.ObjGeom, g); ok {
		return n
	}
	return fmt.Sprintf("geom#%d", g)
}

// gripperRange is _gripper_target_range: the joint range, else the
// actuator's ctrlrange, else a ValueError.
func (x *MuJoCo) gripperRange(aid int) (float64, float64, error) {
	jid := x.model.ActuatorTrnID(aid)
	if x.model.JntLimited(jid) {
		lo, hi := x.model.JntRange(jid)
		return lo, hi, nil
	}
	if x.model.ActuatorCtrlLimited(aid) {
		lo, hi := x.model.ActuatorCtrlRange(aid)
		return lo, hi, nil
	}
	name, ok := x.model.ID2Name(mujoco.ObjActuator, aid)
	if !ok {
		name = fmt.Sprintf("actuator#%d", aid)
	}
	return 0, 0, &PyError{Type: "ValueError", Msg: "Gripper actuator " + pystr.Repr(name) +
		" has neither joint range nor ctrlrange — cannot infer open/close targets"}
}

// actuatorForJoint is _actuator_id_for_joint.
func (x *MuJoCo) actuatorForJoint(name string) (int, error) {
	jid := x.model.Name2ID(mujoco.ObjJoint, name)
	if jid < 0 {
		return 0, keyError("Joint " + pystr.Repr(name) + " not found in MJCF")
	}
	for aid := 0; aid < x.model.NU(); aid++ {
		if x.model.ActuatorTrnID(aid) == jid {
			return aid, nil
		}
	}
	return 0, keyError("No actuator drives joint " + pystr.Repr(name))
}

// reprF is repr() of a float.
func reprF(f float64) string { return pmodel.FloatRepr(f) }

// Executors is robotics_executors: one MuJoCo executor wired to every
// robot step kind, so the four kinds share one simulation.
func Executors(path string, o Options) (map[string]supervise.Executor, *MuJoCo, error) {
	x, err := NewMuJoCo(path, o)
	if err != nil {
		return nil, nil, err
	}
	return map[string]supervise.Executor{"sim_reset": x, "joint_move": x, "cartesian_move": x, "gripper": x}, x, nil
}

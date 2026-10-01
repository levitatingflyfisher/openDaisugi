//go:build mujoco

package robotics

import (
	"errors"
	"strings"
	"time"

	"daisugi-verify/internal/mujoco"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/supervise"
)

// Action is one action of a rollout: joint targets in order.
type Action struct {
	Joints  []string
	Targets []float64
}

// ErrNotImplemented is NotImplementedError from _predict_actions.
var ErrNotImplemented = errors.New("NotImplementedError")

// Predictor is _predict_actions: the actions for a step, given the
// observation. A *PyError carries the exception's class and text; any
// other error is reported by its text.
type Predictor func(v *VLA, step *pyjson.Object, obs *pyjson.Object) ([]Action, error)

// VLA is VLAExecutorBase: MuJoCo loading, the rollout loop and the
// evidence packaging, around a policy's Predict. With no Predict it is the
// bare base class, whose _predict_actions is not implemented.
type VLA struct {
	MJCFPath         string
	MaxActionsGlobal int
	Predict          Predictor
	model            *mujoco.Model
	data             *mujoco.Data
	jointNames       []string
}

// NewVLABase loads the MJCF at path when path is not empty, as the base
// class does when mjcf_path is truthy.
func NewVLABase(path string, maxActionsGlobal int, predict Predictor) (*VLA, error) {
	v := &VLA{MJCFPath: path, MaxActionsGlobal: maxActionsGlobal, Predict: predict}
	if path == "" {
		return v, nil
	}
	m, err := mujoco.LoadXML(path)
	if err != nil {
		return nil, &PyError{Type: "ValueError", Msg: err.Error()}
	}
	v.model, v.data = m, mujoco.NewData(m)
	for j := 0; j < m.NJnt(); j++ {
		if n, ok := m.ID2Name(mujoco.ObjJoint, j); ok {
			v.jointNames = append(v.jointNames, n)
		}
	}
	return v, nil
}

// NewMockVLA is MockVLAExecutor(mjcf_path=path, num_actions=n).
func NewMockVLA(path string, numActions int) (*VLA, error) {
	return NewVLABase(path, 200, MockPredictor(numActions))
}

// Close frees the simulation.
func (v *VLA) Close() {
	if v.data != nil {
		v.data.Close()
		v.model.Close()
	}
}

// Model, Data and JointNames are the simulation and the named joints.
func (v *VLA) Model() *mujoco.Model { return v.model }
func (v *VLA) Data() *mujoco.Data   { return v.data }
func (v *VLA) JointNames() []string { return v.jointNames }

// Configure is configure_from_envelope, a no-op in the base class.
func (v *VLA) Configure(*pyjson.Object) {}

func floatList(xs []float64) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = pyjson.Float(x)
	}
	return out
}

func (v *VLA) eeID() int { return v.model.Name2ID(mujoco.ObjBody, "end_effector") }

// observation is _current_observation: proprioception only.
func (v *VLA) observation() *pyjson.Object {
	obs := pyjson.NewObject()
	if v.data == nil {
		return obs
	}
	obs.Set("qpos", floatList(v.data.QPos())).Set("qvel", floatList(v.data.QVel()))
	if ee := v.eeID(); ee >= 0 {
		p := v.data.XPos(ee)
		obs.Set("end_effector_xyz", floatList(p[:]))
	}
	return obs
}

// apply is _apply_action: set the targets of the joints that exist, then
// one physics step.
func (v *VLA) apply(a Action) {
	for i, name := range a.Joints {
		jid := v.model.Name2ID(mujoco.ObjJoint, name)
		if jid < 0 {
			continue
		}
		for aid := 0; aid < v.model.NU(); aid++ {
			if v.model.ActuatorTrnID(aid) == jid {
				v.data.Ctrl()[aid] = a.Targets[i]
				break
			}
		}
	}
	mujoco.Step(v.model, v.data)
}

func (v *VLA) finalState(ev *pyjson.Object) {
	ev.Set("qpos_final", floatList(v.data.QPos()))
	if ee := v.eeID(); ee >= 0 {
		p := v.data.XPos(ee)
		ev.Set("end_effector_xyz_final", floatList(p[:]))
	}
	ev.Set("contact_count", v.data.NCon())
}

func intOf(x any) int {
	if i, ok := x.(pyjson.Int); ok {
		return int(Num{V: i}.F())
	}
	return 0
}

// Run is VLAExecutorBase.run.
func (v *VLA) Run(step *pyjson.Object, timeoutS, _ int) (supervise.ExecResult, error) {
	if strOf(step, "type") != "vla" {
		return supervise.ExecResult{RC: 1, Stdout: "VLAExecutorBase: not a VLAStep (" + className(step) + ")"}, nil
	}
	started := time.Now()
	if v.data == nil {
		return supervise.ExecResult{RC: 1, Stdout: "VLAExecutorBase: no MJCF loaded; pass mjcf_path"}, nil
	}
	obs := v.observation()
	if v.Predict == nil {
		return supervise.ExecResult{RC: 1, Stdout: "VLAExecutorBase._predict_actions not implemented"}, nil
	}
	actions, err := v.Predict(v, step, obs)
	if errors.Is(err, ErrNotImplemented) {
		return supervise.ExecResult{RC: 1, Stdout: "VLAExecutorBase._predict_actions not implemented"}, nil
	}
	if err != nil {
		typ := "Exception"
		var pe *PyError
		if errors.As(err, &pe) {
			typ = pe.Type
		}
		return supervise.ExecResult{RC: 1, Stdout: "VLA inference error: " + typ + ": " + err.Error()}, nil
	}
	maxActions := intOf(step.Value("max_actions"))
	limit := maxActions
	if v.MaxActionsGlobal < limit {
		limit = v.MaxActionsGlobal
	}
	actions = pySliceTo(actions, limit)
	timeout := floatOf(step.Value("timeout_s"))
	if float64(timeoutS) < timeout {
		timeout = float64(timeoutS)
	}
	deadline := started.Add(time.Duration(timeout * float64(time.Second)))
	executed := 0
	for _, a := range actions {
		if time.Now().After(deadline) {
			break
		}
		v.apply(a)
		executed++
	}
	ev := pyjson.NewObject().Set("task", strOf(step, "task")).Set("actions_requested", len(actions)).
		Set("actions_executed", executed).Set("max_actions", step.Value("max_actions")).Set("observation_initial", obs)
	v.finalState(ev)
	return supervise.ExecResult{RC: 0, Stdout: pyjson.Dumps(ev, true),
		DurationMs: float64(time.Since(started).Nanoseconds()) / 1e6, TimedOut: executed < len(actions)}, nil
}

// pySliceTo is xs[:n] with Python's meaning of a negative n.
func pySliceTo(xs []Action, n int) []Action {
	if n < 0 {
		n += len(xs)
		if n < 0 {
			n = 0
		}
	}
	if n > len(xs) {
		n = len(xs)
	}
	return xs[:n]
}

// MockPredictor is MockVLAExecutor._predict_actions: n actions that walk
// the first two non-gripper joints in a straight line from the observed
// qpos (by position: qpos[0] and qpos[1]) to target_pose's first two
// components, or to zero with no target pose.
func MockPredictor(n int) Predictor {
	return func(v *VLA, step *pyjson.Object, obs *pyjson.Object) ([]Action, error) {
		if len(v.jointNames) == 0 {
			return nil, nil
		}
		j1Target, j2Target := 0.0, 0.0
		if tp := step.Value("target_pose"); tp != nil {
			xs := floatsOf(tp)
			j1Target, j2Target = xs[0], xs[1]
		}
		var controllable []string
		for _, name := range v.jointNames {
			if !strings.HasPrefix(name, "j_grip") {
				controllable = append(controllable, name)
			}
		}
		if len(controllable) == 0 {
			return nil, nil
		}
		var start []float64
		if q, ok := obs.Value("qpos").([]any); ok {
			start = make([]float64, len(q))
			for i, x := range q {
				start[i] = floatOf(x)
			}
		} else {
			start = make([]float64, len(v.jointNames))
		}
		j1Now, j2Now := 0.0, 0.0
		if len(start) > 0 {
			j1Now = start[0]
		}
		if len(start) > 1 {
			j2Now = start[1]
		}
		var out []Action
		for i := 1; i <= n; i++ {
			t := float64(i) / float64(n)
			a := Action{Joints: []string{controllable[0]}, Targets: []float64{j1Now + float64(t*(j1Target-j1Now))}}
			if len(controllable) >= 2 {
				a.Joints = append(a.Joints, controllable[1])
				a.Targets = append(a.Targets, j2Now+float64(t*(j2Target-j2Now)))
			}
			out = append(out, a)
		}
		return out, nil
	}
}

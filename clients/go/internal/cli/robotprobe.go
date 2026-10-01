//go:build mujoco

package cli

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"daisugi-verify/internal/mujoco"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/robotics"
	"daisugi-verify/internal/supervise"
	"daisugi-verify/internal/tracejournal"
)

// RobotProbe answers the robotics cases (clients/robotics_cases.py): it
// reads CASES, a JSON line per case, and writes {"id", "result"} per case,
// each result in the shape the oracle's run_case gives it.
func RobotProbe(e *Env) int {
	if len(e.Args) != 2 {
		e.errf("usage: robot-probe CASES_JSONL REPO_ROOT\n")
		return 2
	}
	f, err := os.Open(e.Args[0])
	if err != nil {
		e.errf("%v\n", err)
		return 1
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		raw, err := pyjson.Loads(sc.Text())
		if err != nil {
			e.errf("%v\n", err)
			return 1
		}
		c := raw.(*pyjson.Object)
		res, err := robotCase(c, e.Args[1])
		if err != nil {
			e.errf("case %v: %v\n", c.Value("name"), err)
			return 1
		}
		e.out("%s\n", pyjson.Dumps(pyjson.NewObject().Set("id", c.Value("id")).Set("result", res), true))
	}
	if err := sc.Err(); err != nil {
		e.errf("%v\n", err)
		return 1
	}
	return 0
}

func robotCase(c *pyjson.Object, root string) (any, error) {
	switch c.Value("kind") {
	case "steps":
		return robotSteps(c, root)
	case "vla":
		return robotVLA(c, root)
	case "run":
		return robotRun(c, root)
	case "render":
		return robotRender(c, root)
	}
	return nil, fmt.Errorf("no case kind %v", c.Value("kind"))
}

func mjcfOf(c *pyjson.Object, root string) string {
	p, _ := c.Value("mjcf").(string)
	if p == "" {
		return ""
	}
	return filepath.Join(root, p)
}

// robotOptions is MuJoCoExecutor's keyword arguments from the case.
func robotOptions(c *pyjson.Object) robotics.Options {
	o := robotics.DefaultOptions()
	kw, _ := c.Value("executor").(*pyjson.Object)
	if kw == nil {
		return o
	}
	num := func(v any) float64 { return robotics.Num{V: v}.F() }
	for _, k := range kw.Keys() {
		v := kw.Value(k)
		switch k {
		case "settle_steps":
			o.SettleSteps = int(num(v))
		case "position_tol":
			o.PositionTol = num(v)
		case "torque_limit":
			if v != nil {
				o.TorqueLimit = &robotics.Num{V: v}
			}
		case "forbid_contacts":
			o.ForbidContacts = v == true
		case "ee_body":
			o.EEBody, _ = v.(string)
		case "ik_max_iter":
			o.IKMaxIter = int(num(v))
		case "ik_tol":
			o.IKTol = robotics.Num{V: v}
		case "ik_damping":
			o.IKDamping = num(v)
		}
	}
	return o
}

func guards(o robotics.Options) *pyjson.Object {
	var tl any
	if o.TorqueLimit != nil {
		tl = o.TorqueLimit.JSON()
	}
	return pyjson.NewObject().Set("torque_limit", tl).Set("forbid_contacts", o.ForbidContacts)
}

func floats(xs []float64) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = pyjson.Float(x)
	}
	return out
}

func strList(v any) []string {
	xs, _ := v.([]any)
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

// robotState is the simulation state a case ends with.
func robotState(m *mujoco.Model, d *mujoco.Data, c *pyjson.Object) *pyjson.Object {
	named := func(kind mujoco.ObjType, names []string, pos func(int) [3]float64) *pyjson.Object {
		out := pyjson.NewObject()
		for _, n := range names {
			if i := m.Name2ID(kind, n); i < 0 {
				out.Set(n, nil)
			} else {
				p := pos(i)
				out.Set(n, floats(p[:]))
			}
		}
		return out
	}
	return pyjson.NewObject().Set("qpos", floats(d.QPos())).Set("qvel", floats(d.QVel())).
		Set("ctrl", floats(d.Ctrl())).Set("ncon", d.NCon()).
		Set("bodies", named(mujoco.ObjBody, strList(c.Value("bodies")), d.XPos)).
		Set("sites", named(mujoco.ObjSite, strList(c.Value("sites")), d.SiteXPos))
}

// caseSteps validates the case's steps as ActionPlan.steps.
func caseSteps(c *pyjson.Object) ([]any, error) {
	plan := pyjson.NewObject().Set("source", "robotics-case").Set("task", "robotics case").Set("steps", c.Value("steps"))
	v, err := pmodel.Validate("ActionPlan", pmodel.ActionPlan, plan, pmodel.Python)
	if err != nil {
		return nil, err
	}
	steps, _ := v.(*pyjson.Object).Value("steps").([]any)
	return steps, nil
}

func pyErrObject(err error) *pyjson.Object {
	var pe *robotics.PyError
	if errors.As(err, &pe) {
		return pyjson.NewObject().Set("error", pe.Type).Set("message", pe.Msg)
	}
	return pyjson.NewObject().Set("error", "Exception").Set("message", err.Error())
}

func resultObject(r supervise.ExecResult) *pyjson.Object {
	return pyjson.NewObject().Set("rc", r.RC).Set("stdout", r.Stdout).Set("timed_out", r.TimedOut)
}

func robotSteps(c *pyjson.Object, root string) (any, error) {
	x, err := robotics.NewMuJoCo(mjcfOf(c, root), robotOptions(c))
	if err != nil {
		return pyjson.NewObject().Set("made", pyErrObject(err)), nil
	}
	defer x.Close()
	out := pyjson.NewObject()
	if raw := c.Value("envelope"); raw != nil {
		ev, verr := pmodel.Validate("Envelope", pmodel.Envelope, raw, pmodel.Python)
		if verr != nil {
			return nil, verr
		}
		x.Configure(ev.(*pyjson.Object))
	}
	out.Set("guards", guards(x.Opt))
	steps, err := caseSteps(c)
	if err != nil {
		return nil, err
	}
	results := []any{}
	for _, s := range steps {
		r, err := x.Run(s.(*pyjson.Object), 30, 10*1024*1024)
		if err != nil {
			results = append(results, pyErrObject(err))
		} else {
			results = append(results, pyjson.NewObject().Set("ok", resultObject(r)))
		}
	}
	out.Set("results", results)
	out.Set("state", robotState(x.Model(), x.Data(), c))
	return out, nil
}

func newCaseVLA(c *pyjson.Object, root string) (*robotics.VLA, error) {
	cfg, _ := c.Value("vla").(*pyjson.Object)
	if cfg == nil {
		cfg = pyjson.NewObject()
	}
	path := mjcfOf(c, root)
	if path == "" {
		path, _ = cfg.Value("mjcf_path").(string)
	}
	kw, _ := cfg.Value("kwargs").(*pyjson.Object)
	if kw == nil {
		kw = pyjson.NewObject()
	}
	intKw := func(k string, d int) int {
		if v, ok := kw.Get(k); ok {
			return int(robotics.Num{V: v}.F())
		}
		return d
	}
	if pyjson.Truthy(cfg.Value("base")) {
		return robotics.NewVLABase(path, intKw("max_actions_global", 200), nil)
	}
	return robotics.NewVLABase(path, 200, robotics.MockPredictor(intKw("num_actions", 10)))
}

func robotVLA(c *pyjson.Object, root string) (any, error) {
	v, err := newCaseVLA(c, root)
	if err != nil {
		return nil, err
	}
	defer v.Close()
	steps, err := caseSteps(c)
	if err != nil {
		return nil, err
	}
	results := []any{}
	for _, s := range steps {
		r, err := v.Run(s.(*pyjson.Object), 30, 10*1024*1024)
		if err != nil {
			return nil, err
		}
		results = append(results, resultObject(r))
	}
	names := []any{}
	for _, n := range v.JointNames() {
		names = append(names, n)
	}
	out := pyjson.NewObject().Set("joint_names", names).Set("results", results)
	if v.Data() != nil {
		out.Set("state", robotState(v.Model(), v.Data(), c))
	}
	return out, nil
}

// receiptDump is Receipt.model_dump(mode="json").
func receiptDump(r tracejournal.ReceiptRow) *pyjson.Object {
	var reversal any
	if r.Reversal != nil {
		reversal = r.Reversal
	}
	return pyjson.NewObject().Set("step_id", r.StepID).Set("run_id", r.RunID).Set("timestamp", r.Timestamp).
		Set("evidence", r.Evidence).Set("evidence_hash", r.EvidenceHash).Set("verify_result", r.VerifyResult).
		Set("verify_details", r.VerifyDetails).Set("model_id", r.ModelID).Set("effect_class", r.EffectClass).
		Set("reversibility", r.Reversibility).Set("reversal", reversal)
}

func robotRun(c *pyjson.Object, root string) (any, error) {
	executors, x, err := robotics.Executors(mjcfOf(c, root), robotOptions(c))
	if err != nil {
		return nil, err
	}
	defer x.Close()
	var vla *robotics.VLA
	if c.Value("vla") != nil {
		vla, err = newCaseVLA(c, root)
		if err != nil {
			return nil, err
		}
		defer vla.Close()
		executors["vla"] = vla
	}
	ev, verr := pmodel.Validate("Envelope", pmodel.Envelope, c.Value("envelope"), pmodel.Python)
	if verr != nil {
		return nil, verr
	}
	pv, verr := pmodel.Validate("ActionPlan", pmodel.ActionPlan, c.Value("plan"), pmodel.Python)
	if verr != nil {
		return nil, verr
	}
	dir, err := os.MkdirTemp("", "robot-probe-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	j, err := tracejournal.Open(filepath.Join(dir, "data"))
	if err != nil {
		return nil, err
	}
	defer j.Close()
	pre, why := prepare(pv.(*pyjson.Object), ev.(*pyjson.Object))
	if why != "" {
		return nil, errors.New(why)
	}
	sup := &supervise.Supervisor{Executors: executors, Journal: j, Z3TimeoutMs: 500, StepTimeoutS: 30,
		MaxOutputBytes: 10 * 1024 * 1024, Approval: supervise.Always{}}
	s := sup.Run(pre.plan, pre.env, pre.venv, pre.verification)
	if sup.LogErr != nil {
		return nil, sup.LogErr
	}
	rows, err := j.Receipts(s.ID)
	if err != nil {
		return nil, err
	}
	receipts := []any{}
	for _, r := range rows {
		receipts = append(receipts, receiptDump(r))
	}
	out := pyjson.NewObject().Set("session", s.JSON()).Set("receipts", receipts).Set("guards", guards(x.Opt)).
		Set("state", robotState(x.Model(), x.Data(), c))
	if vla != nil {
		out.Set("vla_state", robotState(vla.Model(), vla.Data(), c))
	}
	return out, nil
}

// robotRender runs the case's steps, then draws its camera: the image's
// SHA-256, its size and how many colors it holds, or the error.
func robotRender(c *pyjson.Object, root string) (any, error) {
	x, err := robotics.NewMuJoCo(mjcfOf(c, root), robotOptions(c))
	if err != nil {
		return pyjson.NewObject().Set("made", pyErrObject(err)), nil
	}
	defer x.Close()
	steps, err := caseSteps(c)
	if err != nil {
		return nil, err
	}
	for _, s := range steps {
		if _, err := x.Run(s.(*pyjson.Object), 30, 10*1024*1024); err != nil {
			return pyErrObject(err), nil
		}
	}
	mujoco.Forward(x.Model(), x.Data())
	cam := -1
	if name, ok := c.Value("camera").(string); ok {
		cam = x.Model().Name2ID(mujoco.ObjCamera, name)
		if cam < 0 {
			return pyjson.NewObject().Set("error", "ValueError").Set("message", `The camera "`+name+`" does not exist.`), nil
		}
	}
	w := int(robotics.Num{V: c.Value("width")}.F())
	h := int(robotics.Num{V: c.Value("height")}.F())
	r, err := mujoco.NewRenderer(x.Model(), w, h)
	if err != nil {
		return pyjson.NewObject().Set("error", "ValueError").Set("message", err.Error()), nil
	}
	defer r.Close()
	rgb, err := r.Render(x.Data(), cam)
	if err != nil {
		return pyjson.NewObject().Set("error", "ValueError").Set("message", err.Error()), nil
	}
	colors := map[[3]byte]bool{}
	for i := 0; i+2 < len(rgb); i += 3 {
		colors[[3]byte{rgb[i], rgb[i+1], rgb[i+2]}] = true
	}
	sum := sha256.Sum256(rgb)
	return pyjson.NewObject().Set("sha256", hex.EncodeToString(sum[:])).Set("width", w).Set("height", h).
		Set("colors", len(colors)), nil
}

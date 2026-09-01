package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"daisugi-verify/internal/delegate"
	"daisugi-verify/internal/distill"
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/gateway"
	"daisugi-verify/internal/llm"
	"daisugi-verify/internal/orchestrate"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/supervise"
	"daisugi-verify/internal/tracejournal"
	"daisugi-verify/internal/verify"
)

// The slot rules of weave.py (WV-1).
var (
	weaveSlotTypes   = []string{"path", "string", "number", "list[path]", "list[string]", "list[number]"}
	weaveOutputKinds = map[string]bool{"shell": true, "file_read": true, "network": true, "task": true}
	weaveSlotName    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	weaveMaxInt      = new(big.Int).Lsh(big.NewInt(1), 53)
)

const (
	weaveMaxText = 4096
	weaveMaxList = 64
)

// weaveInputFields is weave.INPUT_FIELDS: the fields a slot may fill, by
// step type, and the slot types each takes.
// weaveModelKinds is weave.MODEL_KINDS: step kinds whose output is model
// text. Their slot values may fill a path or a URL, never a file's content.
var weaveModelKinds = map[string]bool{"task": true, "agentic": true}

func weaveInputFields(kind, field string) ([]string, bool) {
	switch {
	case kind == "file_read" && field == "path", kind == "file_write" && field == "path":
		return []string{"path"}, true
	case kind == "file_write" && field == "content":
		return weaveSlotTypes, true
	case kind == "network" && field == "url":
		return []string{"string"}, true
	}
	return nil, false
}

type slotRef struct{ field, src, slot string }

// weaveSpec is weave.SlotSpec, in the plan's order.
type weaveSpec struct {
	outputs  map[string]*pyjson.Object // step id -> {name: type}
	inputs   map[string][]slotRef
	attempts map[string]int // step id -> N
}

type weaveErr struct{ msg string }

func (w *weaveErr) Error() string { return w.msg }

func jdump(v any) string { return pyjson.Dumps(v, true) }

// weaveAncestors is weave._ancestors.
func weaveAncestors(steps []*pyjson.Object, sid string) map[string]bool {
	deps := map[string][]string{}
	for _, s := range steps {
		var d []string
		for _, x := range asAnyList(s.Value("depends_on")) {
			if t, ok := x.(string); ok {
				d = append(d, t)
			}
		}
		deps[str(s, "id")] = d
	}
	seen := map[string]bool{}
	todo := append([]string{}, deps[sid]...)
	for len(todo) > 0 {
		d := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if seen[d] {
			continue
		}
		seen[d] = true
		todo = append(todo, deps[d]...)
	}
	return seen
}

func asAnyList(v any) []any {
	l, _ := v.([]any)
	return l
}

func str(o *pyjson.Object, k string) string {
	s, _ := o.Value(k).(string)
	return s
}

func weaveHead(kind, value string) (string, bool) { return distill.CapabilityHead(kind, value) }

// weaveReadSlots is weave.read_slots.
func weaveReadSlots(raw []any, steps []*pyjson.Object) (*weaveSpec, error) {
	spec := &weaveSpec{outputs: map[string]*pyjson.Object{}, inputs: map[string][]slotRef{}, attempts: map[string]int{}}
	byID := map[string]*pyjson.Object{}
	for _, s := range steps {
		byID[str(s, "id")] = s
	}
	for i, r := range raw {
		ro, ok := r.(*pyjson.Object)
		if !ok || i >= len(steps) {
			continue
		}
		step := steps[i]
		sid, kind := str(step, "id"), str(step, "type")
		outs, has := ro.Get("outputs")
		if !has || outs == nil {
			continue
		}
		oo, ok := outs.(*pyjson.Object)
		if !ok {
			return nil, &weaveErr{"step " + sid + ": outputs must be an object"}
		}
		if len(oo.Keys()) > 0 && !weaveOutputKinds[kind] {
			return nil, &weaveErr{fmt.Sprintf("step %s: a %s step cannot declare outputs", sid, kind)}
		}
		for _, name := range oo.Keys() {
			if !weaveSlotName.MatchString(name) {
				return nil, &weaveErr{fmt.Sprintf("step %s: a slot name is 1 to 32 of a-z 0-9 _, starting with a letter: %s", sid, jdump(name))}
			}
			typ, isStr := oo.Value(name).(string)
			known := false
			for _, t := range weaveSlotTypes {
				known = known || (isStr && t == typ)
			}
			if !known {
				return nil, &weaveErr{fmt.Sprintf("step %s: slot %s has type %s; the types are %s", sid, name,
					jdump(oo.Value(name)), strings.Join(weaveSlotTypes, ", "))}
			}
		}
		spec.outputs[sid] = oo
	}
	for i, r := range raw {
		ro, ok := r.(*pyjson.Object)
		if !ok || i >= len(steps) {
			continue
		}
		step := steps[i]
		sid, kind := str(step, "id"), str(step, "type")
		ins, has := ro.Get("inputs")
		if !has || ins == nil {
			continue
		}
		io, ok := ins.(*pyjson.Object)
		if !ok {
			return nil, &weaveErr{"step " + sid + ": inputs must be an object"}
		}
		up := weaveAncestors(steps, sid)
		var got []slotRef
		for _, fname := range io.Keys() {
			takes, ok := weaveInputFields(kind, fname)
			if !ok {
				return nil, &weaveErr{fmt.Sprintf("step %s: a slot cannot fill %s.%s; a slot fills data only "+
					"(a file path, a file's content, a URL), never a command or a prompt", sid, kind, fname)}
			}
			ref, isStr := io.Value(fname).(string)
			if !isStr || !strings.Contains(ref, ".") {
				return nil, &weaveErr{fmt.Sprintf("step %s: input %s must name a slot as STEP.SLOT", sid, fname)}
			}
			dot := strings.LastIndex(ref, ".")
			src, slot := ref[:dot], ref[dot+1:]
			if byID[src] == nil {
				return nil, &weaveErr{fmt.Sprintf("step %s: input %s names no step %s", sid, fname, jdump(src))}
			}
			if !up[src] {
				return nil, &weaveErr{fmt.Sprintf("step %s: input %s names step %s, which it does not depend on", sid, fname, src)}
			}
			var typ string
			if o := spec.outputs[src]; o != nil {
				typ, _ = o.Value(slot).(string)
				if _, has := o.Get(slot); !has {
					typ = ""
				}
			}
			if typ == "" {
				return nil, &weaveErr{fmt.Sprintf("step %s: step %s declares no slot %s", sid, src, jdump(slot))}
			}
			okType := false
			for _, t := range takes {
				okType = okType || t == typ
			}
			if !okType {
				return nil, &weaveErr{fmt.Sprintf("step %s: %s.%s takes %s, not %s (%s)", sid, kind, fname,
					strings.Join(takes, " or "), typ, ref)}
			}
			if srcKind := str(byID[src], "type"); fname == "content" && weaveModelKinds[srcKind] {
				return nil, &weaveErr{fmt.Sprintf("step %s: %s.%s cannot take %s: %s is a %s step, and model text "+
					"never fills a file's content (it may fill a path or a URL)", sid, kind, fname, ref, src, srcKind)}
			}
			if fname == "path" || fname == "url" {
				if _, ok := weaveHead(kind, str(step, fname)); !ok {
					what := "host"
					if fname == "path" {
						what = "directory"
					}
					return nil, &weaveErr{fmt.Sprintf("step %s: %s.%s %s has no %s for a slot to keep", sid, kind, fname,
						jdump(str(step, fname)), what)}
				}
			}
			got = append(got, slotRef{fname, src, slot})
		}
		if len(got) > 0 {
			spec.inputs[sid] = got
		}
	}
	if err := weaveReadAttempts(raw, steps, spec); err != nil {
		return nil, err
	}
	return spec, nil
}

// weaveCheckValue is weave._check_value.
func weaveCheckValue(typ string, v any) bool {
	if strings.HasPrefix(typ, "list[") {
		inner := typ[5 : len(typ)-1]
		l, ok := v.([]any)
		if !ok || len(l) > weaveMaxList {
			return false
		}
		for _, x := range l {
			if !weaveCheckValue(inner, x) {
				return false
			}
		}
		return true
	}
	if typ == "number" {
		switch x := v.(type) {
		case pyjson.Int:
			n, ok := new(big.Int).SetString(x.Text, 10)
			return ok && new(big.Int).Abs(n).Cmp(weaveMaxInt) <= 0
		case pyjson.Float:
			f := float64(x)
			return !math.IsNaN(f) && !math.IsInf(f, 0)
		}
		return false
	}
	s, ok := v.(string)
	if !ok {
		return false
	}
	rs := pystr.Runes(s)
	if len(rs) > weaveMaxText || strings.ContainsRune(s, 0) {
		return false
	}
	if typ == "string" {
		return true
	}
	if !strings.HasPrefix(s, "/") {
		return false
	}
	for _, part := range strings.Split(s, "/") {
		if part == ".." {
			return false
		}
	}
	for _, r := range rs {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}

// weaveCollect is weave.collect: the slot values in a step's output, or
// why there are none.
func weaveCollect(outputs *pyjson.Object, stdout string, fence bool) (*pyjson.Object, string) {
	text := pystr.Strip(stdout)
	if fence {
		text = delegate.StripFence(stdout)
	}
	v, err := pyjson.Loads(text)
	obj, isObj := v.(*pyjson.Object)
	if err != nil || !isObj {
		return nil, "the output is not one JSON object"
	}
	got := pyjson.NewObject()
	for _, name := range outputs.Keys() {
		typ, _ := outputs.Value(name).(string)
		val, has := obj.Get(name)
		if !has {
			return nil, "the output has no slot " + name
		}
		if !weaveCheckValue(typ, val) {
			return nil, fmt.Sprintf("slot %s is not a %s", name, typ)
		}
		got.Set(name, val)
	}
	return got, ""
}

// weavePrior is weave.Prior.
type weavePrior struct {
	runs      []string
	started   map[string][]string  // step -> the runs that marked it
	done      map[string][2]string // run, stdout
	receipted map[[2]string]bool   // (run, step)
}

// unreceipted is Prior.unreceipted: the first run that marked sid
// started and holds no receipt for it.
func (p *weavePrior) unreceipted(sid string) (string, bool) {
	for _, run := range p.started[sid] {
		if !p.receipted[[2]string{run, sid}] {
			return run, true
		}
	}
	return "", false
}

// weaveReadMarks is the state file part of weave.read_prior; nil for a
// missing file.
func weaveReadMarks(path string) (*weavePrior, error) {
	p := &weavePrior{started: map[string][]string{}, done: map[string][2]string{}, receipted: map[[2]string]bool{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return p, nil
		}
		return nil, err
	}
	text, derr := pystr.DecodeStrict(raw)
	if derr != nil {
		return nil, errors.New("the weave state file is not UTF-8")
	}
	for _, line := range pystr.Splitlines(text) {
		v, err := pyjson.Loads(line)
		if err != nil {
			if errors.Is(err, pyjson.ErrUnsupported) {
				return nil, errors.New("the weave state file holds JSON this binary does not read")
			}
			continue
		}
		o, ok := v.(*pyjson.Object)
		if !ok {
			continue
		}
		run, ok1 := o.Value("run").(string)
		sid, ok2 := o.Value("step").(string)
		if !ok1 || !ok2 {
			continue
		}
		seen := false
		for _, r := range p.runs {
			seen = seen || r == run
		}
		if !seen {
			p.runs = append(p.runs, run)
		}
		marked := false
		for _, r := range p.started[sid] {
			marked = marked || r == run
		}
		if !marked {
			p.started[sid] = append(p.started[sid], run)
		}
	}
	return p, nil
}

func (p *weavePrior) readReceipts(j *tracejournal.Journal) error {
	for _, run := range p.runs {
		rows, err := j.Receipts(run)
		if err != nil {
			return err
		}
		for _, r := range rows {
			p.receipted[[2]string{run, r.StepID}] = true
			if r.VerifyResult && r.Evidence != nil && r.Evidence.Value("status") == "succeeded" {
				out, _ := r.Evidence.Value("stdout").(string)
				p.done[r.StepID] = [2]string{run, out}
			}
		}
	}
	return nil
}

// weaveHook is weave.WeaveHook.
type weaveHook struct {
	spec    *weaveSpec
	state   string
	prior   *weavePrior
	rerun   map[string]bool
	slots   *pyjson.Object // step -> {name: value}
	skipped *pyjson.Object // step -> run
	filled  *pyjson.Object // step -> {field: value}
	// plan and venv are the plan and envelope the run verified; current
	// holds each step as it runs, a filled step in its placeholder's place.
	plan    *pyjson.Object
	venv    verify.Envelope
	current map[string]*pyjson.Object
	// For attempts: where cards go, the plan hash, the project, this run,
	// the choice each step with attempts made, and the choices the
	// operator answered at the ask.
	dataDir, digest, project, runID string
	choices                         *pyjson.Object
	answered                        map[string]bool
}

// Checked is WeaveHook.checked: a filled step that passed its per-step
// verify is verified again as part of the whole plan, with every value
// filled so far. A Z3 check that does not finish fails closed (K2-2).
func (h *weaveHook) Checked(step *pyjson.Object) (*supervise.Outcome, string) {
	sid := str(step, "id")
	if _, filled := h.filled.Get(sid); !filled || h.plan == nil {
		return nil, ""
	}
	h.current[sid] = step
	again := pyjson.NewObject()
	for _, k := range h.plan.Keys() {
		again.Set(k, h.plan.Value(k))
	}
	var steps []any
	for _, s := range asAnyList(h.plan.Value("steps")) {
		so, _ := s.(*pyjson.Object)
		if so == nil {
			continue
		}
		if c := h.current[str(so, "id")]; c != nil {
			steps = append(steps, c)
		} else {
			steps = append(steps, so)
		}
	}
	again.Set("steps", steps)
	vplan, err := verify.ParsePlan(json.RawMessage(pathways.DumpJSON(again)))
	msg := ""
	if err != nil {
		msg = "the filled plan does not read: " + err.Error()
	} else {
		r := verify.Verify(vplan, h.venv, verify.VerifyOptions{Z3TimeoutMs: 500})
		switch {
		case !r.OK && len(r.Violations) > 0:
			msg = r.Violations[0].Message
		case !r.OK:
			msg = "rejected"
		case len(r.Timeouts) > 0:
			msg = r.Timeouts[0]
		}
	}
	if msg == "" {
		return nil, ""
	}
	return weaveOutcome(sid, "rejected_halted", sptr("rejected: the filled plan: "+msg)), supervise.Halted
}

func weaveOutcome(sid, status string, errText *string) *supervise.Outcome {
	return &supervise.Outcome{StepID: sid, Status: status, StartedAt: supervise.NowISO(), Error: errText}
}

func sptr(s string) *string { return &s }

func (h *weaveHook) verdict(step *pyjson.Object) string {
	kind, sid := str(step, "type"), str(step, "id")
	if h.prior == nil || kind == "file_read" || kind == "network" {
		return "run"
	}
	if _, ok := h.prior.done[sid]; ok {
		return "skip"
	}
	if _, ok := h.prior.unreceipted(sid); ok && kind != "task" && !h.rerun[sid] {
		return "ask"
	}
	return "run"
}

func (h *weaveHook) Prepare(step *pyjson.Object) (*pyjson.Object, *supervise.Outcome, *supervise.Outcome, string) {
	sid, kind := str(step, "id"), str(step, "type")
	switch h.verdict(step) {
	case "skip":
		d := h.prior.done[sid]
		if outs := h.spec.outputs[sid]; outs != nil && len(outs.Keys()) > 0 {
			got, why := weaveCollect(outs, d[1], kind == "task")
			if why != "" {
				return nil, nil, weaveOutcome(sid, "aborted", sptr("resume: the receipt in "+d[0]+": "+why)), supervise.Aborted
			}
			h.slots.Set(sid, got)
		}
		h.skipped.Set(sid, d[0])
		h.openCardOf(step)
		return nil, weaveOutcome(sid, "skipped", nil), nil, ""
	case "ask":
		run, _ := h.prior.unreceipted(sid)
		msg := fmt.Sprintf("resume: step %s (%s) started in %s and has no receipt, so it may have run. "+
			"Check it, then run again with --rerun %s", sid, kind, run, sid)
		return nil, nil, weaveOutcome(sid, "aborted", &msg), supervise.Aborted
	}
	ins := h.spec.inputs[sid]
	if len(ins) == 0 {
		return step, nil, nil, ""
	}
	update := pyjson.NewObject()
	for _, in := range ins {
		var value any
		has := false
		if o, ok := h.slots.Value(in.src).(*pyjson.Object); ok {
			value, has = o.Get(in.slot)
		}
		if !has {
			msg := fmt.Sprintf("rejected: slot %s.%s has no value", in.src, in.slot)
			return nil, nil, weaveOutcome(sid, "rejected_halted", &msg), supervise.Halted
		}
		if in.field == "content" {
			if s, ok := value.(string); ok {
				update.Set(in.field, s)
			} else {
				update.Set(in.field, jdump(value))
			}
			continue
		}
		s, _ := value.(string)
		newHead, ok1 := weaveHead(kind, s)
		oldHead, ok2 := weaveHead(kind, str(step, in.field))
		if ok1 != ok2 || newHead != oldHead {
			what := "scheme and host"
			if in.field == "path" {
				what = "directory"
			}
			msg := fmt.Sprintf("rejected: slot %s.%s would change the %s of %s.%s: %s", in.src, in.slot, what, kind,
				in.field, jdump(value))
			return nil, nil, weaveOutcome(sid, "rejected_halted", &msg), supervise.Halted
		}
		update.Set(in.field, s)
	}
	h.filled.Set(sid, update)
	next := pyjson.NewObject()
	for _, k := range step.Keys() {
		next.Set(k, step.Value(k))
	}
	for _, k := range update.Keys() {
		next.Set(k, update.Value(k))
	}
	return next, nil, nil, ""
}

// Started is WeaveHook.started: the step marked started, synced; why
// not, when the mark failed.
func (h *weaveHook) Started(step *pyjson.Object, runID string) string {
	h.runID = runID
	fail := func(err error) string { return "the start mark was not written: " + err.Error() }
	if err := os.MkdirAll(filepath.Dir(h.state), 0o777); err != nil {
		return fail(err)
	}
	f, err := os.OpenFile(h.state, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return fail(err)
	}
	line := pyjson.Dumps(pyjson.NewObject().Set("run", runID).Set("step", str(step, "id")), true) + "\n"
	_, err = f.WriteString(line)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fail(err)
	}
	return ""
}

func (h *weaveHook) Finish(step *pyjson.Object, out supervise.Outcome) supervise.Outcome {
	sid := str(step, "id")
	outs := h.spec.outputs[sid]
	if outs == nil || len(outs.Keys()) == 0 || out.Status != supervise.Succeeded {
		return out
	}
	got, why := weaveCollect(outs, out.Stdout, str(step, "type") == "task")
	if why != "" {
		out.Status = supervise.Failed
		out.Error = sptr("slot outputs: " + why)
		return out
	}
	h.slots.Set(sid, got)
	return out
}

// weaveTaskPrompt is weave.task_prompt without an attempt.
func weaveTaskPrompt(step *pyjson.Object, outputs *pyjson.Object) string {
	if outputs == nil || len(outputs.Keys()) == 0 {
		return orchestrate.TaskPrompt(step)
	}
	var keys []string
	for _, k := range outputs.Keys() {
		t, _ := outputs.Value(k).(string)
		keys = append(keys, k+" ("+t+")")
	}
	return str(step, "prompt") + "\n\nAnswer with one JSON object and nothing else. Its keys and their types: " +
		strings.Join(keys, ", ") + ". A path is absolute."
}

// weaveTask is the task executor weave wires: DelegatingExecutor with
// weave's prompt.
type weaveTask struct {
	llm  *llm.Client
	spec *weaveSpec
	hook *weaveHook
}

// Run is weave.AttemptsExecutor.run: a step with attempts asks N times,
// each prompt naming its attempt, and the hook ranks the answers.
func (t weaveTask) Run(step *pyjson.Object, timeoutS, maxOut int) (supervise.ExecResult, error) {
	sid := str(step, "id")
	prompt := weaveTaskPrompt(step, t.spec.outputs[sid])
	n := t.spec.attempts[sid]
	if n == 0 {
		return orchestrate.Delegate(t.llm, step, prompt, timeoutS, maxOut), nil
	}
	var results []supervise.ExecResult
	for k := 1; k <= n; k++ {
		results = append(results, orchestrate.Delegate(t.llm, step,
			fmt.Sprintf("%s\n\nThis is attempt %d of %d.", prompt, k, n), timeoutS, maxOut))
	}
	return t.hook.choose(step, results), nil
}

// weaveCmd is `daisugi weave PLAN -e ENVELOPE`.
func (e *Env) weaveCmd(args []string) error {
	const cmd = "weave"
	opts := []opt{
		{names: []string{"--envelope", "-e"}, value: true, metavar: "PATH", help: "Path to envelope YAML."},
		{names: []string{"--data-dir"}, value: true, metavar: "PATH", help: "Root data directory for the journal."},
		{names: []string{"--yes", "-y"}, help: "Auto-approve every step (sets DAISUGI_APPROVE=always for this run)."},
		{names: []string{"--json"}, help: "Emit the run session as JSON on stdout."},
		{names: []string{"--resume"}, help: "Skip steps an earlier run of this plan file finished."},
		{names: []string{"--rerun"}, value: true, multiple: true, metavar: "TEXT", help: "On resume, run this started step again (repeatable)."},
		{names: []string{"--max-parallel"}, value: true, metavar: "INTEGER", help: "Run up to this many independent steps of a level at once."},
	}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usageArgs(cmd, "PLAN_PATH", err)
	}
	if p.help {
		return e.cmdHelp(cmd, " PLAN_PATH", "Run the plan tree in PLAN against ENVELOPE, with typed slots between steps.", opts)
	}
	maxPar, err := clickInt(p, "--max-parallel", 1)
	if err != nil {
		return e.usageArgs(cmd, "PLAN_PATH", err)
	}
	if len(p.args) < 1 {
		return e.usageArgs(cmd, "PLAN_PATH", &usageError{"Missing argument 'PLAN_PATH'."})
	}
	planPath := p.args[0]
	if err := clickPath("'plan_path'", planPath); err != nil {
		return e.usageArgs(cmd, "PLAN_PATH", err)
	}
	if !p.has("--envelope") {
		return e.usageArgs(cmd, "PLAN_PATH", &usageError{"Missing option '--envelope' / '-e'."})
	}
	envPath := p.str("--envelope", "")
	if err := clickPath("'--envelope' / '-e'", envPath); err != nil {
		return e.usageArgs(cmd, "PLAN_PATH", err)
	}
	dataDir := gateroot.PathStr(p.str("--data-dir", filepath.Join(e.home, ".opendaisugi")))
	env, perr, refusal := loadModelYAML(envPath, "Envelope", pmodel.Envelope)
	if refusal != "" {
		return e.refuse(cmd, fmt.Errorf("%s", refusal))
	}
	if perr != "" {
		return e.fail3("Tried to read the envelope "+envPath+".", "It did not parse: "+perr, "Fix the file and run again.", 2)
	}
	raw, err := os.ReadFile(planPath)
	if err != nil {
		return e.refuse(cmd, fmt.Errorf("%s cannot be read: %v", planPath, err))
	}
	text, derr := pystr.DecodeStrict(raw)
	if derr != nil {
		return e.refuse(cmd, fmt.Errorf("%s is not UTF-8", planPath))
	}
	planFail := func(why string) error {
		return e.fail3("Tried to read the plan "+planPath+".", "It did not parse: "+why, "Fix the file and run again.", 2)
	}
	obj, jerr := pyjson.LoadsPy(text, 900)
	if jerr != nil {
		if jerr.TooDeep || jerr.NotJSON {
			return e.refuse(cmd, fmt.Errorf("%s holds JSON this binary does not read", planPath))
		}
		return planFail(strings.SplitN(jerr.Error(), "\n", 2)[0])
	}
	po, ok := obj.(*pyjson.Object)
	if !ok {
		return planFail("the plan is not a JSON object")
	}
	dumped, verr := pmodel.Validate("ActionPlan", pmodel.ActionPlan, po, pmodel.Python)
	if verr != nil {
		return planFail(strings.SplitN(verr.String(), "\n", 2)[0])
	}
	plan := dumped.(*pyjson.Object)
	var steps []*pyjson.Object
	for _, s := range asAnyList(plan.Value("steps")) {
		if so, ok := s.(*pyjson.Object); ok {
			steps = append(steps, so)
		}
	}
	spec, serr := weaveReadSlots(asAnyList(po.Value("steps")), steps)
	if serr != nil {
		return e.fail3("Tried to weave the plan "+planPath+".", serr.Error(), "Fix the plan and run again.", 2)
	}
	if maxPar < 1 {
		e.echoErr("Error: --max-parallel must be 1 or more.\n")
		return exit(2)
	}
	if maxPar > 1 {
		return e.refuse(cmd, errors.New("--max-parallel above 1 is not in this binary yet (K2-4)"))
	}
	for _, s := range steps {
		if str(s, "type") == "agentic" {
			return e.refuse(cmd, errors.New("an agentic step: this binary does not run one (K2-7)"))
		}
	}
	models := pyjson.NewObject()
	for _, s := range steps {
		if str(s, "type") != "task" {
			continue
		}
		if m, _ := s.Value("preferred_model").(string); m == "" {
			model := gateway.DefaultCheapModel
			if gateway.EstimateDifficulty(str(s, "prompt")) >= 0.5 {
				model = gateway.DefaultFrontierModel
			}
			s.Set("preferred_model", model)
		}
		models.Set(str(s, "id"), s.Value("preferred_model"))
	}
	if p.flag("--yes") {
		e.env["DAISUGI_APPROVE"] = "always"
		e.Environ = append(e.Environ, "DAISUGI_APPROVE=always")
	}
	sum := sha256.Sum256(raw)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	state := gateroot.Join(gateroot.Join(dataDir, "weave"), "sha256-"+hex.EncodeToString(sum[:])+".jsonl")
	var prior *weavePrior
	if p.flag("--resume") {
		if prior, err = weaveReadMarks(state); err != nil {
			return e.refuse(cmd, err)
		}
	}
	pre, why := prepare(plan, env)
	if why != "" {
		return e.refuse(cmd, fmt.Errorf("%s", why))
	}
	var fallback supervise.Fallback
	if fs, _ := env.Value("fallback").(*pyjson.Object); fs != nil && fs.Value("strategy") == "tier2_recompute" {
		fallback = supervise.Recompute(e.llmClient(), env, pre.venv, 500)
	}
	j, err := e.openJournal(cmd, dataDir)
	if err != nil {
		return err
	}
	defer j.Close()
	if prior != nil {
		if err := prior.readReceipts(j); err != nil {
			return e.fail(cmd, err)
		}
	}
	rerun := map[string]bool{}
	for _, r := range p.vals["--rerun"] {
		rerun[r] = true
	}
	cwd := e.cwd()
	hook := &weaveHook{spec: spec, state: state, prior: prior, rerun: rerun, slots: pyjson.NewObject(),
		skipped: pyjson.NewObject(), filled: pyjson.NewObject(), plan: pre.plan, venv: pre.venv,
		current: map[string]*pyjson.Object{}, dataDir: dataDir, digest: digest, project: cwd,
		choices: pyjson.NewObject(), answered: map[string]bool{}}
	executors := supervise.DefaultExecutors()
	executors["shell"] = supervise.Shell{Environ: e.Environ}
	executors["task"] = weaveTask{llm: e.llmClient(), spec: spec, hook: hook}
	approval := supervise.Default{Getenv: e.lookup, Stdin: e.Stdin, Stdout: e.Stdout, Terminal: e.terminal}
	sup := &supervise.Supervisor{Executors: executors, Journal: j, Z3TimeoutMs: 500, StepTimeoutS: 30,
		MaxOutputBytes: 10 * 1024 * 1024, Fallback: fallback, Hook: hook,
		Approval: weaveChoiceAsk{inner: approval, hook: hook, cwd: cwd, stdin: e.Stdin, stdout: e.Stdout,
			terminal: e.terminal}}
	sess := sup.Run(pre.plan, pre.env, pre.venv, pre.verification)
	if sup.LogErr != nil {
		return e.fail(cmd, sup.LogErr)
	}
	if p.flag("--json") {
		o := sess.JSON()
		o.Set("weave", pyjson.NewObject().Set("plan_hash", digest).Set("state", state).Set("models", models).
			Set("skipped", hook.skipped).Set("filled", hook.filled).Set("slots", hook.slots).
			Set("choices", hook.choices))
		e.out("%s\n", pyjson.DumpsIndent(o, 2, true))
	} else {
		e.echo("Run %s (%s)\n", sess.ID, sess.Status)
		for _, o := range sess.Steps {
			if o.Status == "skipped" {
				run, _ := hook.skipped.Value(o.StepID).(string)
				e.echo("  %s: skipped (receipt in %s)\n", o.StepID, run)
				continue
			}
			rc := "None"
			if o.RC != nil {
				rc = strconv.Itoa(*o.RC)
			}
			e.echo("  %s: %s (rc=%s, approved_by=%s, %s ms)\n", o.StepID, o.Status, rc, pyNone(o.ApprovedBy),
				strconv.FormatFloat(o.DurationMs, 'f', 1, 64))
			filled, _ := hook.filled.Value(o.StepID).(*pyjson.Object)
			for _, in := range spec.inputs[o.StepID] {
				if filled != nil {
					if _, has := filled.Get(in.field); has {
						e.echo("      filled %s from %s.%s\n", in.field, in.src, in.slot)
					}
				}
			}
			if ch, ok := hook.choices.Value(o.StepID).(*pyjson.Object); ok && ch.Value("chosen") != nil {
				card := "; no card"
				if id, ok := ch.Value("choice_id").(string); ok {
					card = "; card " + id
				}
				e.echo("      attempts: chose %s (%s)%s\n", ch.Value("chosen"), ch.Value("status"), card)
			}
			if o.Error != nil && *o.Error != "" {
				e.echo("      error: %s\n", *o.Error)
			}
			if o.Stdout != "" {
				for i, ln := range pystr.Splitlines(pystr.RStrip(o.Stdout)) {
					if i == 5 {
						break
					}
					e.echo("      %s\n", ln)
				}
			}
		}
		if sess.TraceID != nil {
			e.echo("Journal: %s\n", *sess.TraceID)
		}
	}
	switch sess.Status {
	case supervise.Succeeded:
		return nil
	case supervise.Rejected:
		vs, _ := sess.Verification.Value("violations").([]any)
		for _, v := range vs {
			o := v.(*pyjson.Object)
			e.echoErr("  %s: %s\n", o.Value("stage"), o.Value("message"))
		}
		first := "Verification rejected the plan."
		if len(vs) > 0 {
			o := vs[0].(*pyjson.Object)
			first = fmt.Sprintf("[%s] %s", o.Value("stage"), o.Value("message"))
		}
		return e.fail3("Tried to weave the plan "+planPath+".", first,
			"Edit the plan or widen the envelope; see the violation above.", 2)
	case supervise.Aborted:
		return exit(130)
	}
	return exit(1)
}

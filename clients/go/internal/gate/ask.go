package gate

import (
	"context"
	"errors"
	"math"
	"math/big"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"daisugi-verify/internal/lazyre"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/statefile"
)

// maybeAsk is gate._maybe_ask: a would-deny is handed to a present
// operator for at most --ask-timeout seconds. With no tool_use_id or no
// present operator the deny stands and nothing is written.
func (r *runner) maybeAsk(p *pyjson.Object, d *decision, sessionID any) *decision {
	tidV := p.Value("tool_use_id")
	if !pyjson.Truthy(tidV) || !r.operatorPresent() {
		return d
	}
	// GR-R-10: an asks/ or answers/ the gate refuses (a link, another user's,
	// writable by others) takes no ask, so the deny stands.
	if !r.channelOK() {
		return d
	}
	if math.IsNaN(r.askTimeoutS) || math.Abs(r.askTimeoutS) >= 9e18 {
		// int(timeout_s) raises on NaN and the infinities after the wait.
		unported("an --ask-timeout the port does not wait out")
	}
	tid := pyStrOf(tidV)
	deadline := pyTime(time.Now()) + r.askTimeoutS
	if refused := r.postAsk(tid, pyjson.NewObject().
		Set("sessionId", p.Value("session_id")).
		Set("toolName", d.ToolName).
		Set("detail", d.Detail).
		Set("reason", d.Reason).
		Set("clause", d.clause()).
		Set("counterexample", d.counterexample()).
		Set("toolInput", p.Value("tool_input")).
		Set("tier", d.Tier), deadline); refused {
		return d
	}
	_ = catch(func() { r.reportBlocked(p, d, sessionID, tid, deadline) })
	remaining := math.Max(0, deadline-pyTime(time.Now()))
	reply := r.waitAnswer(tid, remaining)
	out := *d
	var by, whoFrom any
	if reply != nil {
		b, w := whoOf(reply.Value("by"), reply.Value("whoFrom"))
		by, whoFrom = b, w
	}
	out.AnsweredBy, out.WhoFrom = by, whoFrom
	if reply != nil && pyEq(reply.Value("decision"), "allow") {
		why := reply.Value("reason")
		if !pyjson.Truthy(why) {
			why = "no reason given"
		}
		if pyEq(reply.Value("scope"), "task") && d.Tier == tierUndoable {
			r.proposeForTask(p, d, tid)
		}
		out.Allow = true
		out.Ask = true
		out.Reason = "allowed by operator: " + pyStrOf(why)
		out.UpdatedInput = nil
		if u := reply.Value("updatedInput"); pyjson.Truthy(u) {
			out.UpdatedInput = u
		}
		return &out
	}
	why := "operator did not answer within " + strconv.FormatInt(int64(r.askTimeoutS), 10) + " s"
	if reply != nil {
		why = "operator denied"
	}
	out.Ask = reply != nil
	out.Reason = d.Reason + " (" + why + ")"
	return &out
}

// editRefusal is gate.EDIT_REFUSAL.
const editRefusal = "the operator's edit does not pass the gate"

// recheckEdit is gate._recheck_edit: the operator's edit is a new call,
// so it goes through the whole decision again. An edit that would be
// denied, or that is not a JSON object, is denied, with no second ask.
func (r *runner) recheckEdit(p *pyjson.Object, d *decision, env *envelope, cwd string) *decision {
	edit, ok := d.UpdatedInput.(*pyjson.Object)
	if !ok {
		out := *d
		out.Allow = r.mode == "audit"
		out.Reason = editRefusal + ": the edit is not a JSON object"
		out.UpdatedInput = nil
		return &out
	}
	edited := pyjson.NewObject()
	for _, k := range p.Keys() {
		edited.Set(k, p.Value(k))
	}
	edited.Set("tool_input", edit)
	again := r.evaluateCall(edited, env, cwd)
	if !again.WouldDeny {
		return d
	}
	out := *again
	out.Ask = true
	out.Reason = editRefusal + ": " + again.Reason
	out.AnsweredBy, out.WhoFrom = d.AnsweredBy, d.WhoFrom
	return &out
}

// askSafe is ask._safe.
func askSafe(raw any) string {
	s := ""
	if pyjson.Truthy(raw) {
		s = pyStrOf(raw)
	}
	var b strings.Builder
	for _, c := range pystr.Runes(s) {
		if c < 0x80 && (c == '.' || c == '_' || c == '-' || ('0' <= c && c <= '9') || ('A' <= c && c <= 'Z') || ('a' <= c && c <= 'z')) {
			b.WriteRune(c)
		} else {
			b.WriteByte('_')
		}
	}
	out := pyHead(strings.Trim(b.String(), "."), 128)
	if out == "" {
		return "none"
	}
	return out
}

func fsEnc(p string) string {
	b, err := pystr.FSEncode(p)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// askRoot is the gate root as the statefile routines spell it.
func (r *runner) askRoot() string { return pathStr(r.root) }

// askLoad is ask._load: the JSON object in an open file and its mtime, both
// from the fd, which it closes. A file that is not readable JSON, or not an
// object, is nil.
func askLoad(fd int) (*pyjson.Object, float64) {
	var st syscall.Stat_t
	_ = syscall.Fstat(fd, &st)
	mtime := float64(st.Mtim.Sec) + float64(st.Mtim.Nsec)/1e9
	raw, err := statefile.ReadAll(fd)
	syscall.Close(fd)
	if err != nil {
		return nil, mtime
	}
	text, derr := pystr.DecodeStrict(raw)
	if derr != nil {
		return nil, mtime
	}
	v, jerr := pyjson.Loads(text)
	if jerr != nil {
		return nil, mtime
	}
	o, _ := v.(*pyjson.Object)
	return o, mtime
}

// askRead is ask._read: the body and mtime of <root>/<dirs>/<name> through
// the checked open. have is false for a file that is missing or refused (a
// refusal leaves the gate's note).
func (r *runner) askRead(dirs []string, name string) (body *pyjson.Object, mtime float64, have bool) {
	fd, _ := statefile.Probe(r.askRoot(), dirs, name, -1)
	if fd < 0 {
		return nil, 0, false
	}
	body, mtime = askLoad(fd)
	return body, mtime, true
}

// askWrite is ask._write: the body written through the directory fd, a link
// at the name replaced. The error is a *statefile.WriteRefused for a path
// the gate refuses.
func (r *runner) askWrite(sub, name string, body *pyjson.Object) error {
	_, err := statefile.WriteFile(r.askRoot(), []string{sub}, name, pyjson.Dumps(body, true), true)
	return err
}

// channelOK is ask.channel_ok: false when asks/ or answers/ exists and the
// gate refuses it. Missing is fine: the first ask makes them.
func (r *runner) channelOK() bool {
	for _, sub := range []string{"asks", "answers"} {
		fds, why := statefile.ProbeDirs(r.askRoot(), sub)
		statefile.Close(fds)
		if fds == nil && why != "" {
			return false
		}
	}
	return true
}

// askDrop is ask._drop: <root>/<sub>/<name> removed through the directory
// fd. A directory the gate refuses is left alone; any other failure raises.
func (r *runner) askDrop(sub, name string) {
	if _, err := statefile.UnlinkFile(r.askRoot(), []string{sub}, name); err != nil && writeRefusedErr(err) == nil {
		panic(osError(err, pathJoin(pathJoin(r.root, sub), name)))
	}
}

func writeRefusedErr(err error) *statefile.WriteRefused {
	var wr *statefile.WriteRefused
	if errors.As(err, &wr) {
		return wr
	}
	return nil
}

// unlinkAtMissingOK is os.unlink(name, dir_fd=fd) with FileNotFoundError
// caught: any other error raises.
func unlinkAtMissingOK(dfd int, name, shown string) {
	if err := syscall.Unlinkat(dfd, name); err != nil && err != syscall.ENOENT {
		panic(osError(err, shown))
	}
}

// entryExists is true when name is an entry in the directory dfd, a
// dangling link included.
func entryExists(dfd int, name string) bool {
	fd, err := syscall.Openat(dfd, name, 0x200000|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	syscall.Close(fd)
	return true
}

// sweepExpired is ask._sweep_expired.
func (r *runner) sweepExpired(now float64) {
	root := r.askRoot()
	afds, _ := statefile.ProbeDirs(root, "asks")
	asksFD := -1
	if afds != nil {
		asksFD = afds[len(afds)-1]
		defer statefile.Close(afds)
		noteDir := statefile.NoteDir(root, "asks")
		for _, name := range statefile.ListJSON(asksFD) {
			var body *pyjson.Object
			if fd, _ := statefile.ProbeFileIn(asksFD, noteDir, name, -1); fd >= 0 {
				body, _ = askLoad(fd)
			}
			deadline := 0.0
			if body != nil {
				if v, ok := body.Get("deadline"); ok {
					if f, ok := pyFloatOf(v); ok {
						deadline = f
					}
				}
			}
			if body == nil || deadline <= now {
				unlinkAtMissingOK(asksFD, name, pathJoin(pathJoin(r.root, "asks"), name))
				r.askDrop("answers", name)
			}
		}
	}
	if wfds, _ := statefile.ProbeDirs(root, "answers"); wfds != nil {
		defer statefile.Close(wfds)
		wfd := wfds[len(wfds)-1]
		for _, name := range statefile.ListJSON(wfd) {
			if asksFD < 0 || !entryExists(asksFD, name) {
				unlinkAtMissingOK(wfd, name, pathJoin(pathJoin(r.root, "answers"), name))
			}
		}
	}
}

// pyFloatOf is float(v) for a JSON value; false where Python raises.
func pyFloatOf(v any) (float64, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case pyjson.Int:
		f, ok := new(big.Float).SetString(x.Text)
		if !ok {
			return 0, false
		}
		out, acc := f.Float64()
		if math.IsInf(out, 0) && acc != big.Exact {
			// float() of an int past the float range raises OverflowError.
			return 0, false
		}
		return out, true
	case pyjson.Float:
		return float64(x), true
	case string:
		f := pyFloat(x)
		if f == nil {
			return 0, false
		}
		return *f, true
	}
	return 0, false
}

// postAsk is ask.post_ask. refused is true when the gate refuses asks/ (the
// deny stands).
func (r *runner) postAsk(tid string, question *pyjson.Object, deadline float64) (refused bool) {
	r.sweepExpired(pyTime(time.Now()))
	body := pyjson.NewObject().
		Set("toolUseId", tid).
		Set("nonce", randomHex(16)).
		Set("postedAt", pyjson.Float(pyTime(time.Now()))).
		Set("deadline", pyjson.Float(deadline))
	for _, k := range question.Keys() {
		body.Set(k, question.Value(k))
	}
	name := askSafe(tid) + ".json"
	if err := r.askWrite("asks", name, body); err != nil {
		if writeRefusedErr(err) != nil {
			return true
		}
		panic(osError(err, pathJoin(pathJoin(r.root, "asks"), name)))
	}
	return false
}

var (
	whoName   = lazyre.New(`^[A-Za-z0-9_-]{1,32}$`)
	whoPane   = lazyre.New(`^pane(:[A-Za-z0-9._:-]{1,128})?$`)
	whoPlugin = lazyre.New(`^plugin:[A-Za-z0-9._:-]{1,128}$`)
)

// whoOf is ask.who_of.
func whoOf(by, whoFrom any) (string, string) {
	w, ok1 := whoFrom.(string)
	b, ok2 := by.(string)
	if !ok1 || !ok2 {
		return "local", "none"
	}
	var rule *regexp.Regexp
	switch w {
	case "token", "socket":
		rule = whoName()
	case "pane":
		rule = whoPane()
	case "plugin":
		rule = whoPlugin()
	}
	if rule == nil || !rule.MatchString(b) {
		return "local", "none"
	}
	return b, w
}

// consumeAsk is ask._consume.
func (r *runner) consumeAsk(tid string) {
	name := askSafe(tid) + ".json"
	r.askDrop("asks", name)
	r.askDrop("answers", name)
}

// waitAnswer is ask.wait_answer: an answer is honored only when it names
// allow or deny, echoes the ask's nonce and is no older than the ask. An
// answer file that is there but refused, or a refused answers/, is not an
// answer: the wait ends with none.
func (r *runner) waitAnswer(tid string, timeoutS float64) *pyjson.Object {
	name := askSafe(tid) + ".json"
	askBody, askM, askOK := r.askRead([]string{"asks"}, name)
	var nonce any
	if askBody != nil {
		nonce = askBody.Value("nonce")
	}
	end := time.Now().Add(time.Duration(timeoutS * float64(time.Second)))
	for {
		fd, refusal := statefile.Probe(r.askRoot(), []string{"answers"}, name, -1)
		if fd >= 0 || refusal != "" {
			var body *pyjson.Object
			var ansM float64
			if fd >= 0 {
				body, ansM = askLoad(fd)
			}
			valid := body != nil &&
				(pyEq(body.Value("decision"), "allow") || pyEq(body.Value("decision"), "deny")) &&
				nonce != nil && pyEq(body.Value("nonce"), nonce) &&
				askOK && fd >= 0 && ansM >= askM
			r.consumeAsk(tid)
			if !valid {
				return nil
			}
			out := pyjson.NewObject()
			for _, k := range body.Keys() {
				if k != "nonce" {
					out.Set(k, body.Value(k))
				}
			}
			return out
		}
		if !time.Now().Before(end) {
			r.consumeAsk(tid)
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// proposeForTask is gate._propose_for_task: best effort.
func (r *runner) proposeForTask(p *pyjson.Object, d *decision, tid string) {
	_ = catch(func() {
		id := randomHex(4)
		now := pyTime(time.Now())
		if err := r.askWrite("proposals", id+".json", pyjson.NewObject().
			Set("id", id).
			Set("kind", "allow-pattern").
			Set("scope", "task").
			Set("expiresAt", pyjson.Float(now+30*86400)).
			Set("createdAt", pyjson.Float(pyTime(time.Now()))).
			Set("sessionId", p.Value("session_id")).
			Set("toolUseId", tid).
			Set("toolInput", p.Value("tool_input")).
			Set("clause", d.clause()).
			Set("tier", d.Tier)); err != nil {
			panic(osError(err, pathJoin(pathJoin(r.root, "proposals"), id+".json")))
		}
	})
}

// operatorPresent is ask.operator_present: a heartbeat file under 15 s old
// naming a live pid, read through the checked open (GR-R-10).
func (r *runner) operatorPresent() bool {
	body, mtime, have := r.askRead(nil, "operator.json")
	if body == nil || body.Len() == 0 || !have {
		return false
	}
	age := float64(time.Now().UnixNano())/1e9 - mtime
	if age > 15 {
		return false
	}
	var pid int64
	switch x := body.Value("pid").(type) {
	case bool:
		if x {
			pid = 1
		}
	case pyjson.Int:
		n, ok := new(big.Int).SetString(x.Text, 10)
		if !ok || !n.IsInt64() || n.Int64() < -1<<31 || n.Int64() >= 1<<31 {
			// os.kill raises OverflowError past a C int.
			unported("an operator pid past a C int")
		}
		pid = n.Int64()
	default:
		return false
	}
	err := syscall.Kill(int(pid), 0)
	switch {
	case err == nil, errors.Is(err, syscall.EPERM):
		return true
	case errors.Is(err, syscall.ESRCH):
		return false
	}
	panic(osError(err, ""))
}

// isRepo is checkpoints.is_repo: git rev-parse --is-inside-work-tree,
// hardened the same way every call in checkpoint.go is (gitSafeArgs, the
// two GIT_CONFIG_* overrides, the checkpoint's own shared deadline). This
// runs in the same WORKSPACE repo a checkpoint's other git calls do,
// before checkpoints.py even knows whether one is due, and it runs twice
// (dueCheckpoint, then snapshot), so it must not carry a timeout of its
// own outside that shared budget.
func (r *runner) isRepo(dir string) bool {
	enc, eerr := pystr.FSEncode(dir)
	if eerr != nil {
		panic(eerr)
	}
	ctx, cancel := context.WithDeadline(context.Background(), r.gitDeadline())
	defer cancel()
	argv := append(append([]string{"-C", string(enc)}, gitSafeArgs...), "rev-parse", "--is-inside-work-tree")
	cmd := exec.CommandContext(ctx, "git", argv...)
	inGroup(cmd)
	cmd.Env = append(r.environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	text, derr := pystr.DecodeStrict(out)
	if derr != nil {
		panic(derr)
	}
	return strings.TrimRight(universalNewlines(text), "\n") == "true"
}

package gate

import (
	"context"
	"errors"
	"math"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"daisugi-verify/internal/lazyre"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// maybeAsk is gate._maybe_ask: a would-deny is handed to a present
// operator for at most --ask-timeout seconds. With no tool_use_id or no
// present operator the deny stands and nothing is written.
func (r *runner) maybeAsk(p *pyjson.Object, d *decision, sessionID any) *decision {
	tidV := p.Value("tool_use_id")
	if !pyjson.Truthy(tidV) || !r.operatorPresent() {
		return d
	}
	if math.IsNaN(r.askTimeoutS) || math.Abs(r.askTimeoutS) >= 9e18 {
		// int(timeout_s) raises on NaN and the infinities after the wait.
		unported("an --ask-timeout the port does not wait out")
	}
	tid := pyStrOf(tidV)
	deadline := pyTime(time.Now()) + r.askTimeoutS
	r.postAsk(tid, pyjson.NewObject().
		Set("sessionId", p.Value("session_id")).
		Set("toolName", d.ToolName).
		Set("detail", d.Detail).
		Set("reason", d.Reason).
		Set("clause", d.clause()).
		Set("counterexample", d.counterexample()).
		Set("toolInput", p.Value("tool_input")).
		Set("tier", d.Tier), deadline)
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

// askMkdir is ask._mkdir.
func askMkdir(dir string) {
	enc := fsEnc(dir)
	if err := os.MkdirAll(filepath.Dir(enc), 0o777); err != nil {
		panic(osError(err, dir))
	}
	if err := os.Mkdir(enc, 0o700); err != nil {
		if st, serr := os.Stat(enc); serr != nil || !st.IsDir() {
			panic(osError(err, dir))
		}
	}
	_ = os.Chmod(enc, 0o700)
}

func fsEnc(p string) string {
	b, err := pystr.FSEncode(p)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// askWriteJSON is ask._write_json: a temp file beside the target, then a
// rename, so a reader never sees half a file.
func askWriteJSON(path string, body *pyjson.Object) {
	askMkdir(pathParent(path))
	name := path[strings.LastIndex(path, "/")+1:]
	stem := name
	if i := strings.LastIndex(name, "."); i > 0 {
		stem = name[:i]
	}
	tmp := pathJoin(pathParent(path), stem+".tmp")
	if err := os.WriteFile(fsEnc(tmp), []byte(pyjson.Dumps(body, true)), 0o666); err != nil {
		panic(osError(err, tmp))
	}
	_ = os.Chmod(fsEnc(tmp), 0o600)
	if err := os.Rename(fsEnc(tmp), fsEnc(path)); err != nil {
		panic(osError(err, tmp))
	}
}

// unlinkMissingOK is Path.unlink with FileNotFoundError caught: any other
// error raises.
func unlinkMissingOK(p string) {
	if err := syscall.Unlink(fsEnc(p)); err != nil && err != syscall.ENOENT {
		panic(osError(err, p))
	}
}

// globJSON is Path(dir).glob("*.json"): every entry whose name ends in
// .json, hidden ones included, in directory order.
func globJSON(dir string) []string {
	ents, err := os.ReadDir(fsEnc(dir))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if n := pystr.FSDecode([]byte(e.Name())); strings.HasSuffix(n, ".json") {
			out = append(out, n)
		}
	}
	return out
}

// sweepExpired is ask._sweep_expired.
func (r *runner) sweepExpired(now float64) {
	asks, answers := pathJoin(r.root, "asks"), pathJoin(r.root, "answers")
	if exists(asks) {
		for _, name := range globJSON(asks) {
			p := pathJoin(asks, name)
			body := readJSONObject(p)
			deadline := 0.0
			if body != nil {
				if v, ok := body.Get("deadline"); ok {
					if f, ok := pyFloatOf(v); ok {
						deadline = f
					}
				}
			}
			if body == nil || deadline <= now {
				unlinkMissingOK(p)
				unlinkMissingOK(pathJoin(answers, name))
			}
		}
	}
	if exists(answers) {
		for _, name := range globJSON(answers) {
			if !exists(pathJoin(asks, name)) {
				unlinkMissingOK(pathJoin(answers, name))
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

// postAsk is ask.post_ask.
func (r *runner) postAsk(tid string, question *pyjson.Object, deadline float64) {
	r.sweepExpired(pyTime(time.Now()))
	body := pyjson.NewObject().
		Set("toolUseId", tid).
		Set("nonce", randomHex(16)).
		Set("postedAt", pyjson.Float(pyTime(time.Now()))).
		Set("deadline", pyjson.Float(deadline))
	for _, k := range question.Keys() {
		body.Set(k, question.Value(k))
	}
	askWriteJSON(pathJoin(pathJoin(r.root, "asks"), askSafe(tid)+".json"), body)
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
	unlinkMissingOK(pathJoin(pathJoin(r.root, "asks"), name))
	unlinkMissingOK(pathJoin(pathJoin(r.root, "answers"), name))
}

func mtime(p string) (float64, bool) {
	st, err := os.Stat(fsEnc(p))
	if err != nil {
		return 0, false
	}
	return float64(st.ModTime().UnixNano()) / 1e9, true
}

// waitAnswer is ask.wait_answer: an answer is honored only when it names
// allow or deny, echoes the ask's nonce and is no older than the ask.
func (r *runner) waitAnswer(tid string, timeoutS float64) *pyjson.Object {
	name := askSafe(tid) + ".json"
	askPath := pathJoin(pathJoin(r.root, "asks"), name)
	answerPath := pathJoin(pathJoin(r.root, "answers"), name)
	askBody := readJSONObject(askPath)
	var nonce any
	if askBody != nil {
		nonce = askBody.Value("nonce")
	}
	askM, askOK := mtime(askPath)
	end := time.Now().Add(time.Duration(timeoutS * float64(time.Second)))
	for {
		if exists(answerPath) {
			body := readJSONObject(answerPath)
			ansM, ansOK := mtime(answerPath)
			valid := body != nil &&
				(pyEq(body.Value("decision"), "allow") || pyEq(body.Value("decision"), "deny")) &&
				nonce != nil && pyEq(body.Value("nonce"), nonce) &&
				askOK && ansOK && ansM >= askM
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
		askWriteJSON(pathJoin(pathJoin(r.root, "proposals"), id+".json"), pyjson.NewObject().
			Set("id", id).
			Set("kind", "allow-pattern").
			Set("scope", "task").
			Set("expiresAt", pyjson.Float(now+30*86400)).
			Set("createdAt", pyjson.Float(pyTime(time.Now()))).
			Set("sessionId", p.Value("session_id")).
			Set("toolUseId", tid).
			Set("toolInput", p.Value("tool_input")).
			Set("clause", d.clause()).
			Set("tier", d.Tier))
	})
}

// readJSONObject is ask._read_json: the file's JSON object, or nil for a
// file that cannot be read, is not JSON, or is not an object.
func readJSONObject(p string) *pyjson.Object {
	enc, eerr := pystr.FSEncode(p)
	if eerr != nil {
		panic(eerr)
	}
	raw, err := os.ReadFile(string(enc))
	if err != nil {
		return nil
	}
	text, derr := pystr.DecodeStrict(raw)
	if derr != nil {
		return nil
	}
	v, jerr := pyjson.Loads(text)
	if jerr != nil {
		return nil
	}
	o, _ := v.(*pyjson.Object)
	return o
}

// operatorPresent is ask.operator_present: a heartbeat file under 15 s old
// naming a live pid.
func (r *runner) operatorPresent() bool {
	p := pathJoin(r.root, "operator.json")
	body := readJSONObject(p)
	if body == nil || body.Len() == 0 {
		return false
	}
	enc, _ := pystr.FSEncode(p)
	st, err := os.Stat(string(enc))
	if err != nil {
		return false
	}
	age := float64(time.Now().UnixNano())/1e9 - float64(st.ModTime().UnixNano())/1e9
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
	err = syscall.Kill(int(pid), 0)
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

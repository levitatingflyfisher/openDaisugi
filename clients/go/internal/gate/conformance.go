package gate

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sort"
	"strconv"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/shell"
)

// Live conformance recording (conformance._record): with
// OPENDAISUGI_CONFORMANCE_RECORD naming a directory, every verification
// and every decomposition the gate makes is appended there as a case line,
// in the order the oracle makes them. Recording never changes a verdict.

const recordEnv = "OPENDAISUGI_CONFORMANCE_RECORD"

func (r *runner) recording() bool { return r.env[recordEnv] != "" }

// caseID is conformance.case_id; false where encoding the body raises.
func caseID(body *pyjson.Object) (string, bool) {
	text, eerr := pystr.EncodeUTF8(pyjson.Canonical(body))
	if eerr != nil {
		return "", false
	}
	sum := sha256.Sum256(text)
	return hex.EncodeToString(sum[:])[:16], true
}

// record is conformance._record: one canonical line in
// cases-<pid>.jsonl, best-effort.
func (r *runner) record(body *pyjson.Object) {
	dir := fsEncOr(r.env[recordEnv])
	if dir == "" || os.MkdirAll(dir, 0o777) != nil {
		return
	}
	f, err := os.OpenFile(dir+"/cases-"+strconv.Itoa(os.Getpid())+".jsonl", os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
	if err != nil {
		return
	}
	defer f.Close()
	line := pyjson.Canonical(body) + "\n"
	if pystr.HasSurrogate(line) {
		return // the utf-8 encoder raises before anything is written
	}
	_, _ = f.WriteString(line)
}

func fsEncOr(p string) string {
	b, err := pystr.FSEncode(p)
	if err != nil {
		return ""
	}
	return string(b)
}

// recordDecompose is conformance.record_decompose for one successful
// decompose_command call.
func (r *runner) recordDecompose(command string, d shell.Decomposition) {
	if !r.recording() {
		return
	}
	expect := pyjson.NewObject().Set("ok", d.OK)
	if d.OK {
		reads := append([]string{}, d.Reads...)
		writes := append([]string{}, d.Writes...)
		sort.Strings(reads)
		sort.Strings(writes)
		expect.Set("heads", strList(d.Heads)).
			Set("commands", strList(d.Commands)).
			Set("reads", strList(reads)).
			Set("writes", strList(writes))
	}
	body := pyjson.NewObject().
		Set("kind", "decompose").
		Set("v", 1).
		Set("command", command).
		Set("expect", expect)
	id, ok := caseID(body)
	if !ok {
		return
	}
	r.record(body.Set("id", id))
}

func strList(xs []string) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}

// recordVerify is conformance.record_verify for the gate's one-step plan,
// with vs as the verdict verify() returned.
func (r *runner) recordVerify(rec *record, env *envelope, vs []violation) {
	if !r.recording() {
		return
	}
	_ = catch(func() { r.record(r.verifyCase(rec, env, vs)) })
}

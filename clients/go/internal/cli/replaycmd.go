package cli

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"daisugi-verify/internal/gate"
	"daisugi-verify/internal/journal"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/pyyaml"
)

// gateReplay is `daisugi gate replay CAPTURES --envelope ENV [--json]`:
// a captured session through the gate, offline, in audit terms
// (gate.replay_captures). Nothing runs and nothing is written.
func (e *Env) gateReplay(args []string) error {
	const cmd = "gate replay"
	const argName = "CAPTURES_JSONL"
	opts := []opt{
		{names: []string{"--envelope"}, value: true, metavar: "PATH",
			help: "Envelope to evaluate against (JSON/YAML)."},
		{names: []string{"--json"}, help: "Emit the full report as JSON."},
	}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usageArgs(cmd, argName, err)
	}
	if p.help {
		return e.cmdHelp(cmd, " "+argName, "Replay a captured session through the gate offline (nothing executes).", opts)
	}
	if len(p.args) < 1 {
		return e.usageArgs(cmd, argName, &usageError{"Missing argument '" + argName + "'."})
	}
	if !p.has("--envelope") {
		return e.usageArgs(cmd, argName, &usageError{"Missing option '--envelope'."})
	}
	envObj, crash, why := replayEnvelope(p.str("--envelope", ""))
	if why != "" {
		return e.refuse(cmd, fmt.Errorf("%s", why))
	}
	if crash != "" {
		e.errf("Traceback (most recent call last): ...\n%s\n", crash)
		return exit(1)
	}
	recs, rc, why := gate.Replay(p.args[0], envObj, e.env)
	if why != "" {
		return e.refuse(cmd, fmt.Errorf("%s", why))
	}
	if rc != nil {
		e.errf("Traceback (most recent call last): ...\n%s\n", rc.Exc)
		return exit(1)
	}
	rep, err := journal.FromRecords(recs)
	if err != nil {
		return e.refuse(cmd, err)
	}
	if p.flag("--json") {
		e.out("%s\n", rep.JSON())
		return nil
	}
	e.out("calls=%d allowed=%d would_deny=%d false_positive_candidates=%d\n",
		len(rep.Records), len(rep.Records)-len(rep.Denied), len(rep.Denied), len(rep.FP))
	fp := map[*pyjson.Object]bool{}
	for _, r := range rep.FP {
		fp[r] = true
	}
	for _, r := range rep.Denied {
		mark := ""
		if fp[r] || equalToAny(r, rep.FP) {
			mark = " [FP-candidate]"
		}
		detail := "''"
		if v, ok := r.Get("detail"); ok {
			detail = pmodel.Repr(v)
		}
		e.out("  DENY%s %s %s: %s\n", mark, replayStr(r.Value("tool_name")), detail, replayStr(r.Value("reason")))
	}
	return nil
}

// replayStr is str(v) for a decoded JSON value.
func replayStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pmodel.Repr(v)
}

// equalToAny is Python's `r in list`: equality, not identity.
func equalToAny(r *pyjson.Object, l []*pyjson.Object) bool {
	a := pyjson.Canonical(r)
	for _, x := range l {
		if pyjson.Canonical(x) == a {
			return true
		}
	}
	return false
}

// replayEnvelope is Envelope(**yaml.safe_load(path.read_text())) as the
// replay command reads it: the validated dump, the last traceback line
// the oracle prints when it raises, or a refusal.
func replayEnvelope(path string) (env *pyjson.Object, crash, why string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "FileNotFoundError: [Errno 2] No such file or directory: " + pystr.Repr(path), ""
		}
		return nil, "", fmt.Sprintf("%s cannot be read: %v", path, err)
	}
	if !utf8.Valid(raw) {
		return nil, "", path + " is not UTF-8"
	}
	text := strings.ReplaceAll(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\r", "\n")
	v, exc, unsup := pyyaml.Load(text)
	if unsup != nil {
		return nil, "", fmt.Sprintf("%s holds YAML this binary does not read (%s)", path, unsup.Why)
	}
	if exc != nil {
		return nil, pyyaml.Qualified(exc) + ": " + exc.Msg, ""
	}
	o, ok := v.(*pyjson.Object)
	if ok && !pyyaml.Plain(o) {
		return nil, "", path + " holds a date or a key that is not text"
	}
	if !ok {
		return nil, "TypeError: opendaisugi.models.Envelope() argument after ** must be a mapping, not " +
			pmodel.TypeName(v), ""
	}
	out, verr := pmodel.Validate("Envelope", pmodel.Envelope, o, pmodel.Python)
	if verr != nil {
		return nil, "pydantic_core._pydantic_core.ValidationError: " + firstLine(verr.String()), ""
	}
	return out.(*pyjson.Object), "", ""
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}

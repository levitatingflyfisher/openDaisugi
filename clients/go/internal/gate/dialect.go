package gate

import (
	"fmt"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/verify"
)

// This file is the gate's part of the system dialect: verify._audit_word,
// verify._enforce_word and verify._word_verdict for the gate's one-step
// plan, and write_paths.step_write_paths with the Python frame depth of
// each call, since the shell decomposition raises RecursionError at the
// oracle's depth (ADJUDICATIONS GT-21).
//
// Frames in the verifier's worker thread: _check_predicate_item 9,
// _audit_word and _enforce_word 10, _word_verdict 11, evaluate_predicate
// and witness 12.

// wordVerdict is verify._word_verdict.
func (r *runner) wordVerdict(word, target string, rec *record, env *envelope) (bool, string) {
	raw, err := verify.UnfoldWordJSON(word, target, r.dialectBase)
	if err != nil {
		return false, fmt.Sprintf("the target is not a supported glob (%s)", err.Error())
	}
	// The unfolded forall_writes resolves its writes against the base
	// (dialect._set_base); a hand-written one never does.
	r.wordBase = r.dialectBase
	defer func() { r.wordBase = "" }()
	v, lerr := pyjson.Loads(raw)
	if lerr != nil {
		panic(lerr)
	}
	expr := parseExpression(v)
	var holds bool
	if exc := catch(func() { holds = r.evaluatePredicate(expr, stepDump(rec), env, 12) }); exc != nil {
		return false, "evaluation error: " + exc.Msg
	}
	if holds {
		return true, ""
	}
	placed, _ := verify.ResolveTarget(target, r.dialectBase)
	regex, _ := verify.GlobRegex(placed)
	return false, r.wordWitness(regex, rec)
}

// wordWitness is dialect.witness on the one step s0, its frame at 12.
func (r *runner) wordWitness(regex string, rec *record) string {
	writes, known := r.stepWritePaths(stepDump(rec), 13)
	if !known {
		return "step 's0' has write paths that cannot be read"
	}
	for _, p := range writes {
		if reSearch(regex, p) {
			return fmt.Sprintf("step 's0' writes '%s'", p)
		}
	}
	return "no step names the write"
}

func wordTarget(word string, target any) string {
	s, _ := target.(string)
	return verify.WordTarget(word, &s)
}

// auditWord is verify._audit_word: a would-deny is recorded, never a verdict.
func (r *runner) auditWord(it predicateItem, word string, rec *record, env *envelope) {
	glob := wordTarget(word, it.Target)
	holds, why := r.wordVerdict(word, glob, rec, env)
	if holds {
		return
	}
	r.wordAudit = append(r.wordAudit, fmt.Sprintf("%sinvariant '%s' is %s('%s'); %s; enforcing would deny",
		verify.DialectAuditPrefix, it.Type, word, glob, why))
}

// enforceWord is verify._enforce_word.
func (r *runner) enforceWord(label string, it predicateItem, word string, rec *record, env *envelope, pin string) []violation {
	glob := wordTarget(word, it.Target)
	if pin != verify.DialectHash {
		return []violation{{Stage: "predicate",
			Message: fmt.Sprintf("%s '%s' is the word %s, but the enforced dialect '%s' is not this build's dialect '%s'",
				label, it.Type, word, pin, verify.DialectHash),
			Detail: kv(label, it.Type, "word", word, "reason", "dialect_pin_mismatch",
				"suggested_remediation", fmt.Sprintf(
					"read the definitions of dialect '%s', then set dialect_enforce: %s in config.yaml",
					verify.DialectHash, verify.DialectHash))}}
	}
	holds, why := r.wordVerdict(word, glob, rec, env)
	if holds {
		return nil
	}
	return []violation{{Stage: "predicate",
		Message: fmt.Sprintf("%s '%s' violated: %s('%s'); %s", label, it.Type, word, glob, why),
		Detail: kv(label, it.Type, "word", word, "target", glob, "dialect", verify.DialectHash,
			"reason", "word_violated")}}
}

// stepWritePaths is write_paths.step_write_paths, its frame at fd. The
// base is the word's (r.wordBase), "" outside a word.
func (r *runner) stepWritePaths(step *pyjson.Object, fd int) ([]string, bool) {
	var paths []string
	switch step.Value("type") {
	case "file_write":
		p, isStr := step.Value("path").(string)
		if !isStr {
			return nil, false
		}
		paths = []string{normpath(p)}
	case "shell":
		c, isStr := step.Value("command").(string)
		if !isStr {
			return nil, false
		}
		var known bool
		if paths, known = r.shellWrites(c, 0, false, fd+1); !known {
			return nil, false
		}
	default:
		return []string{}, true
	}
	if r.wordBase == "" {
		return paths, true
	}
	return verify.ResolveWrites(paths, r.wordBase)
}

// shellWrites is write_paths._shell_writes, its frame at fd.
func (r *runner) shellWrites(command string, depth int, adds bool, fd int) ([]string, bool) {
	if depth > maxInterpreterDepth {
		return nil, false
	}
	stripped := pyStrip(command)
	if stripped == "" {
		return []string{}, true
	}
	if !verify.HasShellMetachar(command) {
		return r.payloadWrites(stripped, depth, adds, fd+1)
	}
	d, exc := r.decomposeAt(command, fd+1)
	if exc != nil {
		panic(exc)
	}
	if !d.OK {
		return nil, false
	}
	out := []string{}
	for _, p := range d.Writes {
		if !sanctionedWriteSinks[p] {
			out = append(out, normpath(p))
		}
	}
	for _, simple := range d.Commands {
		inner, known := r.payloadWrites(simple, depth, adds, fd+1)
		if !known {
			return nil, false
		}
		out = append(out, inner...)
	}
	if verify.MovesCwd(d.Heads) && verify.HasRelative(out) {
		return nil, false
	}
	return out, true
}

// payloadWrites is write_paths._payload_writes, its frame at fd.
func (r *runner) payloadWrites(command string, depth int, adds bool, fd int) ([]string, bool) {
	p, ok := verify.ParseInterpreter(command)
	if !ok || p.Opaque {
		return verify.OperandWrites(command, adds)
	}
	innerAdds := adds || verify.AddsOperands(p.Head)
	out := []string{}
	for _, inner := range p.InnerCommands {
		w, known := r.shellWrites(inner, depth+1, innerAdds, fd+1)
		if !known {
			return nil, false
		}
		out = append(out, w...)
	}
	return out, true
}

package gate

import (
	"context"
	"math"
	"math/big"
	"os"
	"os/exec"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// This file is gate._maybe_checkpoint and the parts of checkpoints.py and
// claude_transcript.py it runs: at most once per new prompt, the
// workspace is committed to a private ref through a temporary index, and
// the session tree records it. Every failure is swallowed, as the oracle
// swallows it: a checkpoint never changes the verdict.

// checkpointDue is what _maybe_checkpoint reads before it writes: the
// workspace, the last real prompt of the transcript, and the state file
// that remembers the last prompt checkpointed. nil means none is due.
type checkpointDue struct {
	cwd, prompt, sid, statePath string
}

// dueCheckpoint runs _maybe_checkpoint's reads, which have no effects.
func (r *runner) dueCheckpoint(p *pyjson.Object, sessionID any) (due *checkpointDue) {
	_ = catch(func() {
		cwd, tpath := p.Value("cwd"), p.Value("transcript_path")
		if !pyjson.Truthy(cwd) || !pyjson.Truthy(tpath) {
			return
		}
		c, ok := cwd.(string)
		if !ok {
			return // Path(cwd) raises TypeError.
		}
		t, ok := tpath.(string)
		if !ok {
			return
		}
		if !r.isRepo(pathStr(c)) {
			return
		}
		prompt := lastPromptUUID(pathStr(t))
		if prompt == "" {
			return
		}
		if !pyjson.Truthy(sessionID) {
			sessionID = p.Value("session_id")
		}
		sid := r.safeSession(sessionID)
		state := pathJoin(pathJoin(r.root, "checkpoint-state"), sid+".json")
		if exists(state) {
			v, err := pyjson.Loads(readText(state))
			if err == pyjson.ErrUnsupported {
				unported("a checkpoint state file the port does not read")
			}
			if err != nil {
				return
			}
			o, isObj := v.(*pyjson.Object)
			if !isObj {
				return // .get on a non-dict raises.
			}
			if pyEq(o.Value("prompt"), prompt) {
				return
			}
		}
		due = &checkpointDue{cwd: pathStr(c), prompt: prompt, sid: sid, statePath: state}
	})
	return due
}

// lastPromptUUID is last_prompt_uuid(read_turns(path)): the uuid of the
// last user turn that is text and not a tool result, or "". What
// read_turns raises reaches the caller.
func lastPromptUUID(path string) string {
	enc, eerr := pystr.FSEncode(path)
	if eerr != nil {
		panic(eerr)
	}
	raw, err := os.ReadFile(string(enc))
	if err != nil {
		return ""
	}
	text, derr := pystr.DecodeStrict(raw)
	if derr != nil {
		return ""
	}
	last := ""
	for _, line := range pystr.Splitlines(universalNewlines(text)) {
		if pyStrip(line) == "" {
			continue
		}
		v, jerr := pyjson.Loads(line)
		if jerr != nil {
			if strings.HasPrefix(jerr.Error(), "maximum recursion depth") || jerr == pyjson.ErrUnsupported {
				unported("a transcript line nested past what json.loads reads")
			}
			continue
		}
		if nesting(v) > pyjson.MaxDepth-20 {
			unported("a transcript line nested near what json.loads reads")
		}
		row, ok := v.(*pyjson.Object)
		if !ok {
			continue
		}
		kind := row.Value("type")
		if !pyEq(kind, "user") && !pyEq(kind, "assistant") {
			continue
		}
		msg, ok := row.Value("message").(*pyjson.Object)
		if !ok || !pyjson.Truthy(row.Value("uuid")) {
			continue
		}
		body, results := turnContent(msg)
		uuid := pyStrOf(row.Value("uuid"))
		pyStrOf(row.Value("timestamp"))
		if kind == "assistant" {
			turnUsage(msg)
			continue
		}
		if body != "" && !results {
			last = uuid
		}
	}
	return last
}

func nesting(v any) int {
	switch x := v.(type) {
	case []any:
		m := 0
		for _, e := range x {
			m = max(m, nesting(e))
		}
		return m + 1
	case *pyjson.Object:
		m := 0
		for _, k := range x.Keys() {
			m = max(m, nesting(x.Value(k)))
		}
		return m + 1
	}
	return 0
}

// turnContent is claude_transcript._content: the text (cut to 400 code
// points) and whether any block is a tool result with an id.
func turnContent(msg *pyjson.Object) (string, bool) {
	content := msg.Value("content")
	if s, ok := content.(string); ok {
		return pyHead(s, 400), false
	}
	var items []any
	switch x := content.(type) {
	case nil:
	case []any:
		items = x
	case *pyjson.Object:
		// Iterating a dict yields its keys, none of them a dict.
		if x.Len() == 0 {
			break
		}
		return "", false
	case bool:
		if x {
			panic(pystr.NewException("TypeError", "'bool' object is not iterable"))
		}
	case pyjson.Int, pyjson.Float:
		if pyjson.Truthy(x) {
			panic(pystr.NewException("TypeError", "object is not iterable"))
		}
	}
	var texts []string
	results := false
	for _, b := range items {
		block, ok := b.(*pyjson.Object)
		if !ok {
			continue
		}
		switch k := block.Value("type"); {
		case pyEq(k, "text"):
			t, has := block.Get("text")
			if !has {
				t = ""
			}
			texts = append(texts, pyStrOf(t))
		case pyEq(k, "tool_use"):
		case pyEq(k, "tool_result") && pyjson.Truthy(block.Value("tool_use_id")):
			pyStrOf(block.Value("tool_use_id"))
			results = true
		}
	}
	return pyHead(strings.Join(texts, " "), 400), results
}

// turnUsage is claude_transcript._usage, for what it raises.
func turnUsage(msg *pyjson.Object) {
	u := msg.Value("usage")
	if !pyjson.Truthy(u) {
		return
	}
	o, ok := u.(*pyjson.Object)
	if !ok {
		panic(pystr.NewException("AttributeError", "object has no attribute 'get'"))
	}
	for _, k := range []string{"input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens", "output_tokens"} {
		v := o.Value(k)
		if !pyjson.Truthy(v) {
			continue
		}
		switch x := v.(type) {
		case bool, pyjson.Int:
		case pyjson.Float:
			if math.IsInf(float64(x), 0) || math.IsNaN(float64(x)) {
				panic(pystr.NewException("ValueError", "cannot convert float to integer"))
			}
		case string:
			if _, ok := pyIntFromText(x); !ok {
				panic(pystr.NewException("ValueError", "invalid literal for int()"))
			}
		default:
			panic(pystr.NewException("TypeError", "int() argument must be a string or a number"))
		}
	}
}

// pyIntFromText is int(s) for a str: whitespace, a sign, underscores
// between digits, and any Unicode decimal digit.
func pyIntFromText(s string) (*big.Int, bool) {
	t := []rune(pyStrip(s))
	if len(t) > 0 && (t[0] == '+' || t[0] == '-') {
		t = t[1:]
	}
	if len(t) == 0 || t[0] == '_' || t[len(t)-1] == '_' {
		return nil, false
	}
	digits := make([]byte, 0, len(t))
	for i, c := range t {
		if c == '_' {
			if t[i-1] == '_' {
				return nil, false
			}
			continue
		}
		if !unicode.IsDigit(c) {
			return nil, false
		}
		digits = append(digits, byte('0'+digitValue(c)))
	}
	n, ok := new(big.Int).SetString(string(digits), 10)
	return n, ok
}

func digitValue(c rune) int {
	for base := c; base >= c-9; base-- {
		if unicode.IsDigit(base) && (base == 0 || !unicode.IsDigit(base-1)) {
			return int(c - base)
		}
	}
	return 0
}

// takeCheckpoint is the rest of _maybe_checkpoint, after the verdict and
// the tree are written: the snapshot, its tree entry, and the state file.
func (r *runner) takeCheckpoint(due *checkpointDue) {
	_ = catch(func() {
		treeFile := pathJoin(pathJoin(pathParent(r.root), "sessions"), safeSessionID(due.sid)+".jsonl")
		if !exists(treeFile) {
			return // SessionTree.open raises FileNotFoundError.
		}
		var head any
		failed := false
		func() {
			defer func() {
				if recover() != nil {
					// The tree is not text Python reads: head() raises.
					failed = true
				}
			}()
			head = readHead(treeFile)
		}()
		if failed {
			return
		}
		entryID := "root"
		if pyjson.Truthy(head) {
			s, ok := head.(string)
			if !ok {
				return // _safe iterates the id; a non-str raises.
			}
			entryID = s
		}
		cp, ok := r.snapshot(due.cwd, due.sid, entryID)
		if !ok {
			return
		}
		entry := pyjson.NewObject().
			Set("type", "checkpoint").
			Set("id", randomHex(4)).
			Set("parentId", head).
			Set("ts", pyjson.Float(pyTime(time.Now()))).
			Set("ref", cp.ref).
			Set("commit", cp.commit).
			Set("coversCount", len(cp.covers)).
			Set("skipped", capSkipped(cp.skipped)).
			Set("skippedCount", len(cp.skipped)).
			Set("promptUuid", due.prompt)
		if appendText(treeFile, dumpsLine(entry, false)) != nil {
			return
		}
		if mkdirPrivate(pathParent(due.statePath)) != nil {
			return
		}
		body := pyjson.Dumps(pyjson.NewObject().Set("prompt", due.prompt), true)
		if os.WriteFile(fsEnc(due.statePath), []byte(body), 0o666) != nil {
			return
		}
		_ = os.Chmod(fsEnc(due.statePath), 0o600)
	})
}

// capSkipped is gate._cap_skipped_for_line.
func capSkipped(skipped []string) []any {
	out := []any{}
	used := 0
	for _, name := range skipped {
		cost := pystr.Len(pyjson.Dumps(name, true)) + 1
		if used+cost > 1500 {
			break
		}
		out = append(out, name)
		used += cost
	}
	return out
}

type checkpoint struct {
	ref, commit     string
	covers, skipped []string
}

// gitSafeArgs is checkpoints._SAFE_GIT_ARGS: `-c` overrides every git call
// in this file passes, so the WORKSPACE repo's own local config cannot run
// a program of its choosing during a checkpoint (a poisoned
// core.fsmonitor is the PoC; a config-defined hook and a clean/smudge
// filter driver are the same shape of hole, one command-line override
// away). See the Python oracle's module-level comment above
// _SAFE_GIT_ARGS for the full reasoning, including why --no-textconv is
// NOT here (no command this file runs reads it) and why filter drivers
// need their own enumeration (filterOverrides below) rather than a fixed
// flag: their names are chosen by the repo, not by git.
var gitSafeArgs = []string{
	"-c", "core.fsmonitor=false",
	"-c", "core.untrackedCache=false",
	"-c", "core.hooksPath=/dev/null",
	"-c", "hook.post-index-change.enabled=false",
	"-c", "hook.reference-transaction.enabled=false",
}

// git is checkpoints._git: stdout without its trailing newlines, or false
// where the oracle's subprocess.run(check=True) would raise: a nonzero
// exit, or (new here) the call running past deadline. A timeout never
// changes a verdict. Every caller already treats "false" the same way it
// treats a git failure, which was always possible here.
func (r *runner) git(repo string, deadline time.Time, env []string, args ...string) (string, bool) {
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	argv := append(append([]string{"-C", fsEnc(repo)}, gitSafeArgs...), args...)
	cmd := exec.CommandContext(ctx, "git", argv...)
	cmd.Env = append(append(r.environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"), env...)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	text, derr := pystr.DecodeStrict(out)
	if derr != nil {
		return "", false
	}
	return strings.TrimRight(universalNewlines(text), "\n"), true
}

// filterOverrides is checkpoints._filter_overrides: `-c` args that turn
// every filter driver THIS repo's own local config defines into a safe
// no-op. required=false rides along: an empty clean/smudge/process value
// alone can still fail the call, see the oracle's docstring.
func (r *runner) filterOverrides(repo string, deadline time.Time, env []string) []string {
	raw, ok := r.git(repo, deadline, env, "config", "-z", "--get-regexp", `^filter\.`)
	if !ok {
		return nil
	}
	names := map[string]bool{}
	for _, record := range strings.Split(raw, "\x00") {
		if record == "" {
			continue
		}
		key, _, _ := strings.Cut(record, "\n")
		parts := strings.Split(key, ".")
		if len(parts) >= 3 && parts[0] == "filter" {
			names[strings.Join(parts[1:len(parts)-1], ".")] = true
		}
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	var out []string
	for _, n := range sorted {
		out = append(out,
			"-c", "filter."+n+".clean=",
			"-c", "filter."+n+".smudge=",
			"-c", "filter."+n+".process=",
			"-c", "filter."+n+".required=false")
	}
	return out
}

// checkpointChildDeadline is ChildDeadline, recomputed from this runner's
// own already-resolved budget fields rather than raw argv (the runner has
// no argv to re-parse). The parent kills the whole call's child at this
// point, and RunGuarded's parent then denies it; see gitDeadline.
func (r *runner) checkpointChildDeadline() time.Time {
	budget := r.verifyTimeoutS
	if r.ask {
		budget += max(r.askTimeoutS, 0)
	}
	if math.IsNaN(budget) || budget > 3600 {
		budget = 3600
	}
	budget = max(budget, 1) + 3
	return r.t0.Add(time.Duration(budget * float64(time.Second)))
}

// gitDeadlineMargin and gitDeadlineCeiling bound a checkpoint's OWN share
// of the child's remaining time. The margin leaves time for the frame to
// still get written after the checkpoint gives up. The ceiling stops a
// generous --ask budget from letting one checkpoint's git calls run for
// (say) a minute just because there is slack to spend. Below the margin,
// no time is spent at all: the checkpoint is skipped this call, same as
// any other best-effort failure.
const (
	gitDeadlineMargin  = 2 * time.Second
	gitDeadlineCeiling = 5 * time.Second
)

// gitDeadline is the ONE deadline a whole checkpoint's sequence of git
// calls shares (passed to every git call snapshot makes below), not a
// fresh budget per call. See checkpoints.snapshot's timeout_s docstring in
// the oracle: a repo with more files, more calls, must not add up to
// minutes just by having more of them. Unlike the oracle, which answers
// to no child process, this port's checkpoint runs inside the SAME child
// RunGuarded will kill at checkpointChildDeadline, and the parent reads
// that kill as a DENY of a call this gate already decided to allow. This
// budget leaves room for the child to still finish and write its frame
// well before that happens.
func (r *runner) gitDeadline() time.Time {
	remaining := time.Until(r.checkpointChildDeadline()) - gitDeadlineMargin
	if remaining > gitDeadlineCeiling {
		remaining = gitDeadlineCeiling
	}
	if remaining < 0 {
		remaining = 0
	}
	return time.Now().Add(remaining)
}

// refSafe is checkpoints._safe.
func refSafe(raw string) string {
	var b strings.Builder
	for _, c := range pystr.Runes(raw) {
		if unicode.IsLetter(c) || unicode.IsNumber(c) || c == '.' || c == '_' || c == '-' {
			b.Write(pystr.AppendRune(nil, c))
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

// snapshot is checkpoints.snapshot with its defaults: the whole work tree
// committed to refs/daisugi/checkpoints/<session>/<entry> through a fresh
// index, with files over 5,000,000 bytes (symlinks never) left out.
func (r *runner) snapshot(repo, sid, entryID string) (checkpoint, bool) {
	if !r.isRepo(repo) {
		return checkpoint{}, false
	}
	deadline := r.gitDeadline()
	top, ok := r.git(repo, deadline, nil, "rev-parse", "--show-toplevel")
	if !ok || top == "" {
		return checkpoint{}, false
	}
	repo = pathStr(top)
	ref := "refs/daisugi/checkpoints/" + refSafe(sid) + "/" + refSafe(entryID)
	gitDir, ok := r.git(repo, deadline, nil, "rev-parse", "--git-dir")
	if !ok {
		return checkpoint{}, false
	}
	if !isabs(gitDir) {
		gitDir = pathJoin(repo, gitDir)
	}
	f, err := os.CreateTemp(fsEnc(gitDir), "daisugi-index-")
	if err != nil {
		return checkpoint{}, false
	}
	index := f.Name()
	f.Close()
	_ = syscall.Unlink(index)
	defer syscall.Unlink(index)
	env := []string{"GIT_INDEX_FILE=" + index}
	// Computed once and passed to EVERY call below, not just add: git's own
	// racy-git protection can make a LATER call in this sequence (write-tree,
	// empirically, against the Python oracle) re-read and re-convert a file
	// to confirm its blob rather than trust a cached stat, running the clean
	// filter again from the repo's live config on a call that didn't carry
	// its own override.
	filterArgs := r.filterOverrides(repo, deadline, env)
	addArgv := append(append([]string{}, filterArgs...), "add", "-A", "--", ".")
	if _, ok := r.git(repo, deadline, env, addArgv...); !ok {
		return checkpoint{}, false
	}
	lsArgv := append(append([]string{}, filterArgs...), "ls-files", "-z")
	listed, ok := r.git(repo, deadline, env, lsArgv...)
	if !ok {
		return checkpoint{}, false
	}
	var covers, skipped []string
	for _, name := range strings.Split(listed, "\x00") {
		if name == "" {
			continue
		}
		var st syscall.Stat_t
		if syscall.Lstat(fsEnc(pathJoin(repo, name)), &st) != nil {
			skipped = append(skipped, name)
			continue
		}
		if st.Mode&syscall.S_IFMT != syscall.S_IFLNK && st.Size > 5_000_000 {
			skipped = append(skipped, name)
			continue
		}
		covers = append(covers, name)
	}
	if len(skipped) > 0 {
		rmArgv := append(append([]string{}, filterArgs...), append([]string{"rm", "--cached", "-q", "--"}, skipped...)...)
		if _, ok := r.git(repo, deadline, env, rmArgv...); !ok {
			return checkpoint{}, false
		}
	}
	wtArgv := append(append([]string{}, filterArgs...), "write-tree")
	tree, ok := r.git(repo, deadline, env, wtArgv...)
	if !ok {
		return checkpoint{}, false
	}
	parent, hasParent := r.git(repo, deadline, nil, "rev-parse", "--verify", "-q", "HEAD")
	args := append(append([]string{}, filterArgs...), "commit-tree", tree, "-m",
		"daisugi checkpoints "+sid+"/"+entryID, "--no-gpg-sign")
	if hasParent && parent != "" {
		args = append(args, "-p", parent)
	}
	author := append(env, "GIT_AUTHOR_NAME=daisugi", "GIT_AUTHOR_EMAIL=daisugi@localhost",
		"GIT_COMMITTER_NAME=daisugi", "GIT_COMMITTER_EMAIL=daisugi@localhost")
	commit, ok := r.git(repo, deadline, author, args...)
	if !ok {
		return checkpoint{}, false
	}
	urArgv := append(append([]string{}, filterArgs...), "update-ref", ref, commit)
	if _, ok := r.git(repo, deadline, nil, urArgv...); !ok {
		return checkpoint{}, false
	}
	sort.Strings(covers)
	sort.Strings(skipped)
	return checkpoint{ref: ref, commit: commit, covers: covers, skipped: skipped}, true
}

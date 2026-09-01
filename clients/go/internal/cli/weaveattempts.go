package cli

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/rank"
	"daisugi-verify/internal/supervise"
)

// weave.MIN_ATTEMPTS, MAX_ATTEMPTS and the id a step with attempts needs.
const (
	weaveMinAttempts = 2
	weaveMaxAttempts = 8
)

var weaveAttemptStepID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,29}$`)

// weaveReadAttempts is the attempts half of weave.read_slots.
func weaveReadAttempts(raw []any, steps []*pyjson.Object, spec *weaveSpec) error {
	for i, r := range raw {
		ro, ok := r.(*pyjson.Object)
		if !ok || i >= len(steps) || ro.Value("attempts") == nil {
			continue
		}
		step := steps[i]
		sid := str(step, "id")
		if str(step, "type") != "task" {
			return &weaveErr{"step " + sid + ": only a task step runs attempts"}
		}
		n := 0
		if x, ok := ro.Value("attempts").(pyjson.Int); ok {
			if b, ok := new(big.Int).SetString(x.Text, 10); ok && b.IsInt64() {
				if v := b.Int64(); v >= weaveMinAttempts && v <= weaveMaxAttempts {
					n = int(v)
				}
			}
		}
		if n == 0 {
			return &weaveErr{fmt.Sprintf("step %s: attempts must be a whole number from %d to %d", sid,
				weaveMinAttempts, weaveMaxAttempts)}
		}
		if !weaveAttemptStepID.MatchString(sid) {
			return &weaveErr{"step " + sid + ": a step with attempts needs an id of 1 to 29 of A-Z a-z 0-9 . _ -"}
		}
		spec.attempts[sid] = n
	}
	return nil
}

func (h *weaveHook) rankingID(sid string) string {
	d := h.digest
	if i := strings.LastIndex(d, ":"); i >= 0 {
		d = d[i+1:]
	}
	if len(d) > 16 {
		d = d[:16]
	}
	return "weave:" + d + ":" + sid
}

// steps is the plan's steps.
func (h *weaveHook) steps() []*pyjson.Object {
	var out []*pyjson.Object
	if h.plan == nil {
		return out
	}
	for _, s := range asAnyList(h.plan.Value("steps")) {
		if so, ok := s.(*pyjson.Object); ok {
			out = append(out, so)
		}
	}
	return out
}

// below is WeaveHook.below: every step that depends on sid, in id order.
func (h *weaveHook) below(sid string) []any {
	steps := h.steps()
	var ids []string
	for _, s := range steps {
		if weaveAncestors(steps, str(s, "id"))[sid] {
			ids = append(ids, str(s, "id"))
		}
	}
	sort.Strings(ids)
	out := []any{}
	for _, id := range ids {
		out = append(out, id)
	}
	return out
}

func weaveChoice(cid any, chosen any, status any, recorded bool, ranking any) *pyjson.Object {
	return pyjson.NewObject().Set("choice_id", cid).Set("chosen", chosen).Set("status", status).
		Set("recorded", recorded).Set("ranking", ranking)
}

// choose is WeaveHook.choose: rank a step's attempts, record the choice,
// and give the leader's result; a failed result when none survived.
func (h *weaveHook) choose(step *pyjson.Object, results []supervise.ExecResult) supervise.ExecResult {
	sid := str(step, "id")
	outs := h.spec.outputs[sid]
	atts := []any{}
	for k, res := range results {
		pass := func(ok bool) string {
			if ok {
				return "pass"
			}
			return "fail"
		}
		tests := []any{pyjson.NewObject().Set("name", "the call answered").Set("required", true).
			Set("result", pass(res.RC == 0))}
		if outs != nil && len(outs.Keys()) > 0 {
			ok := res.RC == 0
			if ok {
				_, why := weaveCollect(outs, res.Stdout, true)
				ok = why == ""
			}
			tests = append(tests, pyjson.NewObject().Set("name", "the slot outputs read").Set("required", true).
				Set("result", pass(ok)))
		}
		sum := sha256.Sum256([]byte(res.Stdout))
		author := ""
		if res.Model != nil {
			author = *res.Model
		}
		atts = append(atts, pyjson.NewObject().Set("id", fmt.Sprintf("%s#%d", sid, k+1)).
			Set("content_hash", "sha256:"+hex.EncodeToString(sum[:])).Set("author", author).Set("tests", tests).
			Set("where", pyjson.NewObject().Set("kind", "output").Set("text", res.Stdout)))
	}
	doc := pyjson.NewObject().Set("ranking_id", h.rankingID(sid)).Set("task", str(step, "prompt")).
		Set("project", h.project).Set("attempts", atts).Set("comparisons", []any{})
	r, err := rank.Parse(doc)
	if err != nil {
		first := results[0]
		return supervise.ExecResult{RC: 1, Stdout: "attempts: " + err.Error(), DurationMs: first.DurationMs, Model: first.Model}
	}
	var warnings []string
	var owner []rank.Pair
	if h.dataDir != "" {
		byID := map[string]*rank.Attempt{}
		for _, a := range r.Attempts {
			byID[a.ID] = a
		}
		owner = rank.OwnerAnswers(h.dataDir, r.ID, byID, &warnings)
	}
	res := rank.Fit(r, nil, owner, warnings)
	if res.Value("status") == "none_survived" {
		var parts []string
		for _, e := range res.Value("eliminated").([]any) {
			eo := e.(*pyjson.Object)
			var rs []string
			for _, x := range eo.Value("reasons").([]any) {
				rs = append(rs, x.(string))
			}
			parts = append(parts, fmt.Sprintf("%s: %s", eo.Value("id"), strings.Join(rs, ", ")))
		}
		h.choices.Set(sid, weaveChoice(nil, nil, "none_survived", false, res))
		first := results[0]
		return supervise.ExecResult{RC: 1, Stdout: "attempts: none survived: " + strings.Join(parts, "; "),
			DurationMs: first.DurationMs, Model: first.Model}
	}
	var cid any
	recorded := false
	if h.dataDir != "" {
		t := rankNow()
		rows := rank.Sweep(h.dataDir, t)
		if len(res.Value("order").([]any)) > 1 {
			run := pyjson.NewObject().Set("run_id", h.runID).Set("step", sid).Set("downstream", h.below(sid))
			row := rank.OpenedRow(r, res, t, run)
			id := row.Value("choice_id").(string)
			cid = id
			known := false
			for _, c := range rank.ReadCards(h.dataDir) {
				known = known || c.ID() == id
			}
			if !known {
				rows = append(rows, row)
				recorded = true
			}
		}
		if err := rank.AppendRows(h.dataDir, rows); err != nil {
			first := results[0]
			return supervise.ExecResult{RC: 1, Stdout: "attempts: the card was not written: " + err.Error(),
				DurationMs: first.DurationMs, Model: first.Model}
		}
	}
	leader := res.Value("leader").(string)
	h.choices.Set(sid, weaveChoice(cid, leader, res.Value("status"), recorded, res))
	k, _ := strconv.Atoi(leader[strings.LastIndex(leader, "#")+1:])
	return results[k-1]
}

// openCardOf is WeaveHook._open_card_of: for a step with attempts that
// resume skips, the choice an earlier run left open.
func (h *weaveHook) openCardOf(step *pyjson.Object) {
	sid := str(step, "id")
	if h.spec.attempts[sid] == 0 || h.dataDir == "" {
		return
	}
	rid := h.rankingID(sid)
	t := rankNow()
	cards := rank.ReadCards(h.dataDir)
	for i := len(cards) - 1; i >= 0; i-- {
		card := cards[i]
		if card.Opened.Value("ranking_id") != rid {
			continue
		}
		if card.Close == nil && card.Answer == nil && rank.Decay(card, h.dataDir, t) == nil {
			h.choices.Set(sid, weaveChoice(card.ID(), card.Opened.Value("chosen"), card.Opened.Value("status"), false, nil))
		}
		return
	}
}

// openChoiceAbove is WeaveHook.open_choice_above.
func (h *weaveHook) openChoiceAbove(step *pyjson.Object) string {
	anc := weaveAncestors(h.steps(), str(step, "id"))
	var ids []string
	for a := range anc {
		ids = append(ids, a)
	}
	sort.Strings(ids)
	for _, a := range ids {
		c, _ := h.choices.Value(a).(*pyjson.Object)
		if c == nil || c.Value("choice_id") == nil || h.answered[a] {
			continue
		}
		return a
	}
	return ""
}

// pyNormpath is os.path.normpath on POSIX.
func pyNormpath(p string) string {
	if p == "" {
		return "."
	}
	initial := 0
	if strings.HasPrefix(p, "/") {
		initial = 1
		if strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///") {
			initial = 2
		}
	}
	var comps []string
	for _, c := range strings.Split(p, "/") {
		if c == "" || c == "." {
			continue
		}
		if c != ".." || (initial == 0 && len(comps) == 0) || (len(comps) > 0 && comps[len(comps)-1] == "..") {
			comps = append(comps, c)
		} else if len(comps) > 0 {
			comps = comps[:len(comps)-1]
		}
	}
	out := strings.Repeat("/", initial) + strings.Join(comps, "/")
	if out == "" {
		return "."
	}
	return out
}

// weaveStepTier is weave.step_tier.
func weaveStepTier(step *pyjson.Object, cwd string) string {
	switch str(step, "type") {
	case "file_read", "network", "task":
		return "undoable"
	case "file_write":
		root := pyNormpath(cwd)
		path := str(step, "path")
		if !strings.HasPrefix(path, "/") {
			path = filepath.Join(root, path)
		}
		p := pyNormpath(path)
		if p == root || strings.HasPrefix(p, strings.TrimRight(root, "/")+"/") {
			return "undoable"
		}
	}
	return "permanent"
}

// weaveChoiceAsk is weave.ChoiceAsk: in front of the default approval, a
// step that cannot be undone below an open choice goes to the terminal ask.
type weaveChoiceAsk struct {
	inner    supervise.Approver
	hook     *weaveHook
	cwd      string
	stdin    io.Reader
	stdout   io.Writer
	terminal func() bool
}

func (a weaveChoiceAsk) Decide(step, env *pyjson.Object) (supervise.Decision, error) {
	src := a.hook.openChoiceAbove(step)
	if src == "" || weaveStepTier(step, a.cwd) == "undoable" {
		return a.inner.Decide(step, env)
	}
	c := a.hook.choices.Value(src).(*pyjson.Object)
	cid, _ := c.Value("choice_id").(string)
	if a.terminal == nil || !a.terminal() {
		return supervise.Decision{Approved: false, ApprovedBy: "denied", Reason: fmt.Sprintf(
			"step %s cannot be undone and the choice %s on step %s is open; answer it in a terminal, or review "+
				"it with daisugi rank queue and run again", str(step, "id"), cid, src)}, nil
	}
	fmt.Fprintf(a.stdout, "Step %s kept %v of its attempts (card %s).\n", src, c.Value("chosen"), cid)
	what := str(step, "command")
	if what == "" {
		what = str(step, "path")
	}
	if what == "" {
		what = str(step, "url")
	}
	if what == "" {
		what = str(step, "id")
	}
	fmt.Fprintf(a.stdout, "Approve step %s (%s)? [y/N] ", pystr.Repr(str(step, "id")), what)
	line, err := bufio.NewReader(a.stdin).ReadString('\n')
	if err != nil && line == "" {
		return supervise.Decision{}, errors.New("EOFError: EOF when reading a line")
	}
	answer := pystr.Lower(pystr.Strip(strings.TrimSuffix(line, "\n")))
	ok := answer == "y" || answer == "yes"
	if ok && a.hook.dataDir != "" {
		rid := a.hook.rankingID(src)
		_ = rank.AppendRows(a.hook.dataDir, []*pyjson.Object{pyjson.NewObject().Set("choice_id", cid).
			Set("ranking_id", rid).Set("event", "confirmed").Set("how", "permanent_ask").Set("ts", rankNow())})
		a.hook.answered[src] = true
	}
	return supervise.Decision{Approved: ok, ApprovedBy: "tty", Reason: "user answered " + pystr.Repr(answer)}, nil
}

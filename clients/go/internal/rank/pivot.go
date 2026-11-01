package rank

import (
	"bytes"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"daisugi-verify/internal/branchgit"
	"daisugi-verify/internal/deeds"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/tracejournal"
)

type pivotRow struct {
	step, rev string
	at        float64
	rjson     *string
}

// pivotRows is rank_pivot._rows: (step, reversibility, timestamp,
// reversal_json) of a run, read only.
func pivotRows(dataDir, runID string) []pivotRow {
	abs, err := filepath.Abs(filepath.Join(dataDir, "journal", "index.db"))
	if err != nil {
		return nil
	}
	if _, err := os.Stat(abs); err != nil {
		return nil
	}
	con, err := sql.Open("sqlite3", (&url.URL{Scheme: "file", Path: abs}).String()+"?mode=ro")
	if err != nil {
		return nil
	}
	defer con.Close()
	rows, err := con.Query("SELECT step_id, reversibility, timestamp, reversal_json FROM receipts "+
		"WHERE run_id = ? ORDER BY timestamp, step_id", runID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []pivotRow
	for rows.Next() {
		var step, rev, at, rj any
		if rows.Scan(&step, &rev, &at, &rj) != nil {
			return nil
		}
		var t float64
		switch x := at.(type) {
		case float64:
			t = x
		case int64:
			t = float64(x)
		default:
			continue
		}
		str := func(v any) string {
			switch x := v.(type) {
			case string:
				return x
			case []byte:
				return string(x)
			case nil:
				return ""
			}
			return fmt.Sprint(v)
		}
		r := pivotRow{step: str(step), rev: str(rev), at: t}
		switch x := rj.(type) {
		case string:
			r.rjson = &x
		case []byte:
			s := string(x)
			r.rjson = &s
		}
		out = append(out, r)
	}
	if rows.Err() != nil {
		return nil
	}
	return out
}

func (c *Card) whereOf(attempt string) *pyjson.Object {
	w, ok := c.facts().Value("where").(*pyjson.Object)
	if !ok {
		return nil
	}
	o, _ := w.Value(attempt).(*pyjson.Object)
	return o
}

// PivotPlan is rank_pivot.plan. The error is the oracle's RankError.
func PivotPlan(card *Card, dataDir, to string) (*pyjson.Object, error) {
	chosen := card.chosen()
	inSurv := false
	var others []string
	for _, s := range card.survivors() {
		id := s.Value("id").(string)
		inSurv = inSurv || id == to
		if id != chosen {
			others = append(others, id)
		}
	}
	if !inSurv || to == chosen {
		return nil, &Error{fmt.Sprintf("%s is not another attempt that survived; the others are %s.", to, Join(others))}
	}
	sc := SwitchCost(card, dataDir, to)
	facts := card.facts()
	run, _ := facts.Value("run").(*pyjson.Object)
	undo := []any{}
	skipped := []any{}
	if run != nil {
		rid, ok1 := run.Value("run_id").(string)
		downL, ok2 := run.Value("downstream").([]any)
		if ok1 && ok2 {
			down := map[string]bool{}
			for _, d := range downL {
				if s, ok := d.(string); ok {
					down[s] = true
				}
			}
			runs := []string{rid}
			for _, r := range card.Resumed {
				if r != rid {
					runs = append(runs, r)
				}
			}
			type later struct {
				rid string
				pivotRow
			}
			var ls []later
			for _, r := range runs {
				for _, row := range pivotRows(dataDir, r) {
					if down[row.step] {
						ls = append(ls, later{r, row})
					}
				}
			}
			sort.SliceStable(ls, func(i, j int) bool {
				if ls[i].at != ls[j].at {
					return ls[i].at > ls[j].at
				}
				return ls[i].step > ls[j].step
			})
			for _, l := range ls {
				switch l.rev {
				case "reversible":
					path := ""
					if l.rjson != nil {
						if v, err := pyjson.LoadsPy(*l.rjson, 900); err == nil {
							if o, ok := v.(*pyjson.Object); ok {
								if p, ok := o.Value("path").(string); ok {
									path = p
								} else if v, has := o.Get("path"); has {
									path = fmt.Sprint(v)
								}
							}
						}
					}
					undo = append(undo, pyjson.NewObject().Set("run_id", l.rid).Set("step_id", l.step).Set("path", path))
				case "irreversible":
					skipped = append(skipped, pyjson.NewObject().Set("run_id", l.rid).Set("step_id", l.step))
				}
			}
		}
	}
	var applyV any
	alt, cur := card.whereOf(to), card.whereOf(chosen)
	if alt != nil && alt.Value("kind") == "output" {
		if t, ok := alt.Value("text").(string); ok {
			applyV = pyjson.NewObject().Set("kind", "output").Set("text", t)
		}
	}
	if applyV == nil && alt != nil && cur != nil && alt.Value("kind") == "worktree" && cur.Value("kind") == "worktree" {
		ap, ok1 := alt.Value("path").(string)
		cp, ok2 := cur.Value("path").(string)
		ab, ok3 := alt.Value("base").(string)
		ar, ok4 := alt.Value("repo").(string)
		cb, ok5 := cur.Value("base").(string)
		cr, ok6 := cur.Value("repo").(string)
		if ok1 && ok2 && ok3 && ok4 && ok5 && ok6 && branchgit.Usable(ab, ar) && branchgit.Usable(cb, cr) {
			applyV = pyjson.NewObject().Set("kind", "worktree_patch").Set("from", ap).Set("from_base", ab).
				Set("from_repo", ar).Set("onto", cp).Set("onto_base", cb).Set("onto_repo", cr)
		}
	}
	var followUp any
	if sc.Cost == FollowUp || applyV == nil {
		if sc.Cost == FollowUp {
			followUp = fmt.Sprintf("Follow-up task: redo the work of choice %s with %s in place of %s. %s",
				card.ID(), to, chosen, sc.Text)
		} else {
			followUp = fmt.Sprintf("Follow-up task: apply %s in place of %s by hand; its place "+
				"cannot be patched onto the chosen one.", to, chosen)
		}
	}
	return pyjson.NewObject().Set("choice_id", card.ID()).Set("to", to).Set("chosen", chosen).
		Set("cost", sc.Cost).Set("text", sc.Text).Set("undo", undo).Set("skipped", skipped).
		Set("apply", applyV).Set("follow_up", followUp), nil
}

func gitRun(input []byte, argv ...string) (stdout []byte, stderr string, ok bool) {
	return gitRunEnv(nil, input, argv...)
}

func gitRunEnv(env []string, input []byte, argv ...string) (stdout []byte, stderr string, ok bool) {
	cmd := exec.Command("git", branchgit.Guarded(argv...)...)
	cmd.Env = env
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.Bytes(), strings.TrimSpace(pystr.DecodeReplace(errb.Bytes())), err == nil
}

func namesOf(path, base, repo string) []string {
	out, ok := branchgit.Diff(path, base, repo, []string{"--name-only", "--no-renames", "-z", "--cached"}, false, 0)
	if !ok {
		return nil
	}
	var names []string
	for _, f := range strings.Split(pystr.DecodeReplace(out), "\x00") {
		if f != "" {
			names = append(names, f)
		}
	}
	sort.Strings(names)
	return names
}

func contains(ss []string, x string) bool {
	for _, s := range ss {
		if s == x {
			return true
		}
	}
	return false
}

// pivotPatch is rank_pivot._patch: take the chosen edits out of the chosen
// worktree and put the alternative's in, both read against their recorded
// base commits. When the alternative does not apply, every touched path goes
// back to the base and the chosen edits are applied again, as adopt puts a
// tree back. It returns nil on success.
func pivotPatch(a *pyjson.Object, done []any) *pyjson.Object {
	onto, from := a.Value("onto").(string), a.Value("from").(string)
	ob, orp := a.Value("onto_base").(string), a.Value("onto_repo").(string)
	fb, frp := a.Value("from_base").(string), a.Value("from_repo").(string)
	fail := func(why string) *pyjson.Object {
		return pyjson.NewObject().Set("applied", false).Set("undone", done).Set("why", why)
	}
	for _, w := range [][2]string{{onto, orp}, {from, frp}} {
		if !branchgit.Owns(w[0], w[1]) {
			return fail(w[0] + " no longer belongs to " + w[1] + "; nothing was changed")
		}
	}
	var env []string // the inherited environment: no child store is read
	for _, w := range []string{onto, from} {
		branchgit.Stage(w, false)
	}
	mine, mok := branchgit.Diff(onto, ob, orp, []string{"--cached"}, false, 0)
	theirs, tok := branchgit.Diff(from, fb, frp, []string{"--cached"}, false, 0)
	if !mok || !tok {
		return fail("git diff failed; nothing was changed")
	}
	mineFiles, theirsFiles := namesOf(onto, ob, orp), namesOf(from, fb, frp)
	before := map[string]bool{}
	for _, f := range theirsFiles {
		if _, err := os.Lstat(filepath.Join(onto, f)); err == nil {
			before[f] = true
		}
	}
	if len(mine) > 0 {
		if _, e, ok := gitRunEnv(env, mine, "-C", onto, "apply", "-R", "--index"); !ok {
			return fail("git apply -R failed, the chosen edits stay: " + e)
		}
	}
	_, e, ok := gitRunEnv(env, theirs, "-C", onto, "apply", "--3way", "--index")
	if ok {
		return nil
	}
	why := "git apply failed: " + e
	seen := map[string]bool{}
	var touched []string
	for _, f := range append(append([]string{}, mineFiles...), theirsFiles...) {
		if !seen[f] {
			seen[f] = true
			touched = append(touched, f)
		}
	}
	sort.Strings(touched)
	tr, _, _ := gitRun(nil, append([]string{"-C", onto, "ls-tree", "-r", "--name-only", "-z", ob, "--"}, touched...)...)
	tracked := map[string]bool{}
	for _, f := range strings.Split(pystr.DecodeReplace([]byte(tr)), "\x00") {
		tracked[f] = true
	}
	gitRun(nil, append([]string{"-C", onto, "reset", "-q", "--"}, touched...)...)
	for _, f := range touched {
		if tracked[f] {
			gitRun(nil, "-C", onto, "checkout", ob, "--", f)
		} else if contains(mineFiles, f) || !before[f] {
			// The chosen edit made this file, or the failed merge wrote it (the
			// alternative's new file). A file only the alternative adds may
			// already have been in the tree (an ignored one): it is not ours to
			// remove.
			_ = os.Remove(filepath.Join(onto, f))
		}
	}
	if len(mine) > 0 {
		if _, be, bok := gitRunEnv(env, mine, "-C", onto, "apply", "--index"); !bok {
			why += "; the chosen edits could not be put back: " + be
		} else {
			why += "; the chosen edits are back"
		}
	}
	return fail(why)
}

// PivotApply is rank_pivot.apply.
func PivotApply(p *pyjson.Object, dataDir string) *pyjson.Object {
	if p.Value("cost") == FollowUp || p.Value("follow_up") != nil {
		return pyjson.NewObject().Set("applied", false).Set("follow_up", p.Value("follow_up")).Set("why", p.Value("text"))
	}
	done := []any{}
	undo := p.Value("undo").([]any)
	if len(undo) > 0 {
		j, err := tracejournal.OpenReadOnly(dataDir)
		if err != nil {
			return pyjson.NewObject().Set("applied", false).Set("why", err.Error())
		}
		defer j.Close()
		byStep := map[[2]string]tracejournal.ReceiptRow{}
		seen := map[string]bool{}
		for _, u := range undo {
			rid := u.(*pyjson.Object).Value("run_id").(string)
			if seen[rid] {
				continue
			}
			seen[rid] = true
			rows, err := j.Receipts(rid)
			if err != nil {
				return pyjson.NewObject().Set("applied", false).Set("why", err.Error())
			}
			for _, r := range rows {
				byStep[[2]string{r.RunID, r.StepID}] = r
			}
		}
		for _, uv := range undo {
			u := uv.(*pyjson.Object)
			step := u.Value("step_id").(string)
			r, ok := byStep[[2]string{u.Value("run_id").(string), step}]
			if !ok || r.Reversal == nil || r.Reversibility != "reversible" {
				return pyjson.NewObject().Set("applied", false).Set("why", fmt.Sprintf("step %s has no reversal; stopped", step))
			}
			h, verr := deeds.ParseHandle(r.Reversal)
			if verr != nil {
				return pyjson.NewObject().Set("applied", false).Set("why", verr.Error())
			}
			if err := deeds.Apply(h); err != nil {
				return pyjson.NewObject().Set("applied", false).Set("why", err.Error())
			}
			done = append(done, step)
		}
	}
	a, _ := p.Value("apply").(*pyjson.Object)
	if a != nil && a.Value("kind") == "worktree_patch" {
		if r := pivotPatch(a, done); r != nil {
			return r
		}
	}
	var kind any
	if a != nil {
		kind = a.Value("kind")
	}
	return pyjson.NewObject().Set("applied", true).Set("undone", done).Set("applied_kind", kind)
}

// PivotText is rank_pivot.plan_text.
func PivotText(p *pyjson.Object) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Pivot %s: %s to %s (%s)\n%s\n", p.Value("choice_id"), p.Value("chosen"), p.Value("to"),
		p.Value("cost"), p.Value("text"))
	for _, u := range p.Value("undo").([]any) {
		o := u.(*pyjson.Object)
		fmt.Fprintf(&b, "  undo step %s of %s: %s\n", o.Value("step_id"), o.Value("run_id"), o.Value("path"))
	}
	for _, u := range p.Value("skipped").([]any) {
		o := u.(*pyjson.Object)
		fmt.Fprintf(&b, "  cannot undo step %s of %s\n", o.Value("step_id"), o.Value("run_id"))
	}
	if a, ok := p.Value("apply").(*pyjson.Object); ok {
		if a.Value("kind") == "worktree_patch" {
			fmt.Fprintf(&b, "  then apply %s onto %s\n", a.Value("from"), a.Value("onto"))
		} else {
			b.WriteString("  then use the alternative's output\n")
		}
	}
	if f, ok := p.Value("follow_up").(string); ok && f != "" {
		b.WriteString(f + "\n")
	}
	return b.String()
}

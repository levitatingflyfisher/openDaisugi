package rank

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"daisugi-verify/internal/agentprof"
	"daisugi-verify/internal/branchgit"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// DiffCap is rank_judge.DIFF_CAP.
const DiffCap = 256 * 1024

// JudgeSystemText is rank_judge.JUDGE_SYSTEM_TEXT.
const JudgeSystemText = "You compare two answers to one task. " +
	"Decide which answer does the task better. " +
	"Length, polish and confidence do not count. " +
	"Reply with one line: A, B or tie."

// Msg is one chat message.
type Msg struct{ Role, Content string }

// Prompt is rank_judge.prompt: the same bytes in every port.
func Prompt(task, first, second string) []Msg {
	user := "Task:\n" + task + "\n\nAnswer A:\n" + first + "\n\nAnswer B:\n" + second +
		"\n\nWhich answer does the task better? Reply with one line: A, B or tie."
	return []Msg{{"system", JudgeSystemText}, {"user", user}}
}

// Artifact is rank_judge.artifact: the text a judge reads for one attempt.
func Artifact(a *Attempt, warnings *[]string) string {
	if w, ok := a.Where.(*pyjson.Object); ok && a.hasWhere {
		if w.Value("kind") == "output" {
			if t, ok := w.Value("text").(string); ok {
				return t
			}
		}
		if w.Value("kind") == "worktree" {
			// The base and the repository come from the attempt the parent
			// wrote, never from the worktree, which its child can rewrite.
			p, ok1 := w.Value("path").(string)
			base, ok2 := w.Value("base").(string)
			repo, ok3 := w.Value("repo").(string)
			if ok1 && ok2 && ok3 && branchgit.Usable(base, repo) {
				if raw, ok := branchgit.Diff(p, base, repo, nil, true, 60*time.Second); ok {
					text := pystr.DecodeReplace(raw[:min(len(raw), DiffCap)])
					if len(raw) > DiffCap {
						text += "\n[cut at 256 KiB]"
					}
					return text
				}
			}
		}
	}
	if warnings != nil {
		*warnings = append(*warnings, fmt.Sprintf("attempt %s has no artifact a judge can read", a.ID))
	}
	return ""
}

// ParseVerdict is rank_judge.parse_verdict: "a", "b" or "tie", else "".
func ParseVerdict(text string) string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	for _, line := range strings.Split(text, "\n") {
		line = pystr.Strip(line)
		if line == "" {
			continue
		}
		line = strings.ToLower(pystr.Strip(strings.TrimRight(line, ".")))
		if line == "a" || line == "b" || line == "tie" {
			return line
		}
		return ""
	}
	return ""
}

// CallFn is one model call: wire model, base URL, key, messages -> text.
type CallFn func(model, baseURL, apiKey string, msgs []Msg) (string, error)

// JudgeResult is what rank_judge.judge_once returns.
type JudgeResult struct {
	Recorded bool
	Pair     []string
	Judge    string
	Outcomes []string
	Why      string
	Reason   string
	Fit      *pyjson.Object
}

func nothing(why string, fit *pyjson.Object, reason string) *JudgeResult {
	if reason == "" {
		reason = "nothing"
	}
	return &JudgeResult{Why: why, Reason: reason, Fit: fit}
}

// fitFor is rank_judge._fit_for: the fit with the file's votes, the
// journal's votes and the owner's answers. Returns the fit and the number of
// votes so far.
func fitFor(r *Ranking, dataDir string, warnings []string) (*pyjson.Object, int) {
	comps := append([]Comparison{}, r.Comparisons...)
	journal, bad := JournalComparisons(dataDir, r.ID)
	comps = append(comps, journal...)
	if bad > 0 {
		warnings = append(warnings, fmt.Sprintf("%d rows of the comparison journal did not read; skipped", bad))
	}
	owner := r.Owner
	if !r.HasOwner {
		byID := map[string]*Attempt{}
		for _, a := range r.Attempts {
			byID[a.ID] = a
		}
		owner = OwnerAnswers(dataDir, r.ID, byID, &warnings)
	}
	r2 := *r
	r2.Comparisons, r2.HasComparisons = comps, true
	r2.Owner, r2.HasOwner = owner, true
	var weights map[string]float64
	if len(r.Comparisons) == 0 {
		set := map[string]bool{}
		for _, j := range r.Policy.Judges {
			set[j] = true
		}
		for _, c := range comps {
			set[c.Judge] = true
		}
		var judges []string
		for j := range set {
			judges = append(judges, j)
		}
		sort.Strings(judges)
		if len(judges) > 0 {
			weights, _ = WeightsFor(dataDir, judges, SeedOf(&r2, comps, owner))
		}
	}
	return FitW(&r2, nil, nil, warnings, weights), len(comps)
}

// FitDoc is rank_judge.fit_doc's fit part.
func FitDoc(r *Ranking, dataDir string) *pyjson.Object {
	res, _ := fitFor(r, dataDir, nil)
	return res
}

func splitEntry(entry string) (string, string, bool) {
	name, model, ok := strings.Cut(entry, "/")
	return name, model, ok
}

// JudgeOnce is rank_judge.judge_once. doc is the parsed attempts file; the
// caller passes a fresh parse each time (the oracle parses on every call).
func JudgeOnce(parse func() (*Ranking, error), dataDir string, profiles agentprof.Profiles, getenv func(string) string,
	judge *string, now float64, call CallFn) (*JudgeResult, error) {
	r, err := parse()
	if err != nil {
		return nil, err
	}
	if judge != nil {
		r.Policy.Judges = []string{*judge}
	}
	if len(r.Policy.Judges) == 0 {
		return nothing("policy.judges names no judge.", nil, ""), nil
	}
	_, trust := JudgeTrust(dataDir, TrustFloor, TrustMinCards)
	var dropped, kept []string
	for _, j := range r.Policy.Judges {
		if t, ok := trust[j]; ok && t.Dropped {
			dropped = append(dropped, j)
		} else {
			kept = append(kept, j)
		}
	}
	r.Policy.Judges = kept
	warnings := []string{}
	res, nSoFar := fitFor(r, dataDir, warnings)
	nxt, _ := res.Value("next").(*pyjson.Object)
	if nxt == nil {
		if len(dropped) > 0 && res.Value("stop") == "judges_exhausted" {
			j := dropped[0]
			return nothing(fmt.Sprintf("judge %s is dropped: %s", j, trust[j].Why), res, ""), nil
		}
		return nothing(fmt.Sprintf("nothing to judge: %s", anyStr(res.Value("stop"))), res, ""), nil
	}
	entry := nxt.Value("judge").(string)
	pair := nxt.Value("pair").([]any)
	firstID, secondID := pair[0].(string), pair[1].(string)
	byID := map[string]*Attempt{}
	for _, a := range r.Attempts {
		byID[a.ID] = a
	}
	fa, fb := byID[firstID], byID[secondID]
	profName, model, hasModel := splitEntry(entry)
	pn := profName
	profile, why := profiles.Lookup(&pn)
	if profile == nil {
		return nothing(fmt.Sprintf("judge %s: %s", entry, why), res, ""), nil
	}
	if profile.Wire == "claude-code" {
		return nothing(fmt.Sprintf("judge %s: profile %s uses the claude-code wire; a judge needs the anthropic or openai wire.",
			entry, profName), res, ""), nil
	}
	if !hasModel && profile.Model != nil {
		model = *profile.Model
	} else if !hasModel {
		model = ""
	}
	if model == "" {
		return nothing(fmt.Sprintf("judge %s: no model; name PROFILE/MODEL.", entry), res, ""), nil
	}
	fam := FamilyOf(model)
	for _, a := range []*Attempt{fa, fb} {
		if strings.TrimSpace(pystr.Strip(a.Author)) == "" {
			return nothing(fmt.Sprintf("attempt %s has no author, so the other-family rule cannot hold.", a.ID), res, ""), nil
		}
		if FamilyOf(a.Author) == fam {
			return nothing(fmt.Sprintf("judge %s is of the same family (%s) as the author of %s.", entry, fam, a.ID), res, ""), nil
		}
	}
	texts := map[string]string{fa.ID: Artifact(fa, &warnings), fb.ID: Artifact(fb, &warnings)}
	if texts[fa.ID] == "" && texts[fb.ID] == "" {
		return nothing("no attempt has an artifact a judge can read.", res, ""), nil
	}
	if texts[fa.ID] == "" || texts[fb.ID] == "" {
		return nothing("an attempt has no artifact a judge can read.", res, ""), nil
	}
	wireModel := "openai/" + model
	if profile.Wire == "anthropic" {
		wireModel = "anthropic/" + model
	}
	key := ""
	if profile.Key != nil {
		key = *profile.Key
	} else if profile.KeyEnv != nil && *profile.KeyEnv != "" {
		key = getenv(*profile.KeyEnv)
	}
	baseURL := ""
	if profile.BaseURL != nil {
		baseURL = *profile.BaseURL
	}
	ordered := [2][2]*Attempt{{fa, fb}, {fb, fa}}
	var verdicts [2]string
	for k, ab := range ordered {
		text, err := call(wireModel, baseURL, key, Prompt(r.Task, texts[ab[0].ID], texts[ab[1].ID]))
		if err != nil {
			return nothing(fmt.Sprintf("the judge call failed (%s); no vote recorded.", errText(err)), res, "failed"), nil
		}
		v := ParseVerdict(text)
		if v == "" {
			return nothing("the judge reply was not one line of A, B or tie; no vote recorded.", res, "unparsable"), nil
		}
		verdicts[k] = v
	}
	sum := sha256.Sum256([]byte(pyjson.CanonicalASCII([]any{r.ID, firstID, secondID, entry, nSoFar})))
	pairID := "pj_" + hex.EncodeToString(sum[:])[:12]
	var rows []*pyjson.Object
	var outcomes []string
	for k, ab := range ordered {
		shown := "ab"
		if k == 1 {
			shown = "ba"
		}
		rows = append(rows, pyjson.NewObject().Set("ranking_id", r.ID).Set("a", ab[0].ID).Set("b", ab[1].ID).
			Set("a_hash", ab[0].ContentHash).Set("b_hash", ab[1].ContentHash).Set("shown", shown).
			Set("outcome", verdicts[k]).Set("judge", entry).Set("pair_id", pairID).
			Set("lengths", []any{pyLen(texts[ab[0].ID]), pyLen(texts[ab[1].ID])}).Set("ts", now))
		switch verdicts[k] {
		case "a":
			outcomes = append(outcomes, ab[0].ID)
		case "b":
			outcomes = append(outcomes, ab[1].ID)
		default:
			outcomes = append(outcomes, "tie")
		}
	}
	if err := AppendComparisons(dataDir, rows); err != nil {
		return nil, err
	}
	after, _ := fitFor(r, dataDir, nil)
	return &JudgeResult{Recorded: true, Pair: []string{firstID, secondID}, Judge: entry, Outcomes: outcomes,
		Why:    fmt.Sprintf("recorded two calls of %s on %s and %s.", entry, firstID, secondID),
		Reason: "recorded", Fit: after}, nil
}

// pyLen is len(str) in code points.
func pyLen(s string) int { return len([]rune(s)) }

func errText(err error) string {
	return fmt.Sprintf("%s: %s", pyErrName(err), err.Error())
}

// pyErrName names the exception class the oracle would report.
func pyErrName(err error) string {
	type named interface{ PyName() string }
	if n, ok := err.(named); ok {
		return n.PyName()
	}
	return "Error"
}

// AppendComparisons is rank.append_comparisons.
func AppendComparisons(dataDir string, rows []*pyjson.Object) error {
	if len(rows) == 0 {
		return nil
	}
	d := RankingsDir(dataDir)
	if err := os.MkdirAll(d, 0o777); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(d, "comparisons.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(pyjson.Dumps(r, true))
		b.WriteByte('\n')
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

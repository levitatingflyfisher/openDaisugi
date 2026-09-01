package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/rank"
)

var rankHelp = `Usage: daisugi rank [OPTIONS] COMMAND [ARGS]...

  Rank N attempts at one task, record the choice, and review it later.

Options:
  --help  Show this message and exit.

Commands:
  fit     Rank the attempts: eliminate, then quality, then the judges'...
  choose  Rank the attempts, take the leader, and open a card when there...
  queue   List the open cards: what was chosen, why, and what switching...
  record  Record the owner's answer on a card.
`

var rankRecordHelp = `Usage: daisugi rank record [OPTIONS] COMMAND [ARGS]...

  Record the owner's answer on a card. Only the owner runs these.

Options:
  --help  Show this message and exit.

Commands:
  pick  Pick an attempt on a card: the chosen one confirms it, another...
  drop  Drop a card: the chosen attempt is kept, recorded as confirmed.
`

// rankNow is time.time().
func rankNow() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// rankCmd is `daisugi rank`: rank.py through its commands.
func (e *Env) rankCmd(args []string) error {
	if len(args) == 0 {
		e.out("%s", rankHelp)
		return exit(2)
	}
	switch args[0] {
	case "--help":
		e.out("%s", rankHelp)
		return nil
	case "fit":
		return e.rankFit(args[1:], false)
	case "choose":
		return e.rankFit(args[1:], true)
	case "queue":
		return e.rankQueue(args[1:])
	case "record":
		return e.rankRecord(args[1:])
	}
	e.errf("Usage: daisugi rank [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi rank --help' for help.\n\nError: No such command '%s'.\n", args[0])
	return exit(2)
}

func rankOpts() []opt {
	return []opt{
		{names: []string{"--data-dir"}, value: true, metavar: "PATH", help: "Daisugi data directory."},
		{names: []string{"--json"}, help: "Machine-readable JSON output."},
	}
}

func (e *Env) rankDataDir(p *parsed) string {
	return gateroot.PathStr(p.str("--data-dir", filepath.Join(e.home, ".opendaisugi")))
}

// rankRead is cli._rank_read: the attempts file, checked.
func (e *Env) rankRead(cmd, path string) (*rank.Ranking, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, e.refuse(cmd, fmt.Errorf("%s cannot be read: %v", path, err))
	}
	text, derr := pystr.DecodeStrict(raw)
	if derr != nil {
		return nil, e.fail3("Tried to read the attempts in "+path+".", "It did not parse: "+derr.String(), "Fix the file and run again.", 2)
	}
	doc, jerr := pyjson.LoadsPy(text, 900)
	if jerr != nil {
		if jerr.TooDeep || jerr.NotJSON {
			return nil, e.refuse(cmd, fmt.Errorf("%s holds JSON this binary does not read", path))
		}
		return nil, e.fail3("Tried to read the attempts in "+path+".", "It did not parse: "+strings.SplitN(jerr.Error(), "\n", 2)[0], "Fix the file and run again.", 2)
	}
	r, perr := rank.Parse(doc)
	if perr != nil {
		return nil, e.fail3("Tried to rank the attempts in "+path+".", perr.Error(), "Fix the file and run again.", 2)
	}
	return r, nil
}

// rankFitOf is cli._rank_fit: the journal stands in for what the file lacks.
func rankFitOf(r *rank.Ranking, dataDir string) *pyjson.Object {
	var warnings []string
	var comps []rank.Comparison
	if !r.HasComparisons {
		var bad int
		comps, bad = rank.JournalComparisons(dataDir, r.ID)
		if bad > 0 {
			warnings = append(warnings, fmt.Sprintf("%d rows of the comparison journal did not read; skipped", bad))
		}
	}
	var owner []rank.Pair
	if !r.HasOwner {
		byID := map[string]*rank.Attempt{}
		for _, a := range r.Attempts {
			byID[a.ID] = a
		}
		owner = rank.OwnerAnswers(dataDir, r.ID, byID, &warnings)
	}
	return rank.Fit(r, comps, owner, warnings)
}

// rankFit is `rank fit` and, with choose, `rank choose`.
func (e *Env) rankFit(args []string, choose bool) error {
	cmd, summary := "rank fit", "Rank the attempts: eliminate, then quality, then the judges' votes, then cost."
	if choose {
		cmd, summary = "rank choose", "Rank the attempts, take the leader, and open a card when there was a choice."
	}
	opts := rankOpts()
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usageArgs(cmd, "ATTEMPTS_PATH", err)
	}
	if p.help {
		return e.cmdHelp(cmd, " ATTEMPTS_PATH", summary, opts)
	}
	if len(p.args) < 1 {
		return e.usageArgs(cmd, "ATTEMPTS_PATH", &usageError{"Missing argument 'ATTEMPTS_PATH'."})
	}
	path := p.args[0]
	if err := clickPath("'attempts_path'", path); err != nil {
		return e.usageArgs(cmd, "ATTEMPTS_PATH", err)
	}
	r, err := e.rankRead(cmd, path)
	if err != nil {
		return err
	}
	dataDir := e.rankDataDir(p)
	res := rankFitOf(r, dataDir)
	code := rank.Exit[res.Value("status").(string)]
	if !choose {
		if p.flag("--json") {
			e.out("%s\n", pyjson.DumpsIndent(res, 2, true))
		} else {
			e.out("%s", rank.FitText(res))
		}
		return exitOrNil(code)
	}
	t := rankNow()
	rows := rank.Sweep(dataDir, t)
	var cid any
	recorded := false
	if order := res.Value("order").([]any); len(order) > 1 {
		row := rank.OpenedRow(r, res, t, nil)
		id := row.Value("choice_id").(string)
		cid = id
		known := false
		for _, c := range rank.ReadCards(dataDir) {
			known = known || c.ID() == id
		}
		if !known {
			rows = append(rows, row)
			recorded = true
		}
	}
	if err := rank.AppendRows(dataDir, rows); err != nil {
		return e.fail(cmd, err)
	}
	if p.flag("--json") {
		e.out("%s\n", pyjson.DumpsIndent(pyjson.NewObject().Set("ranking", res).Set("choice_id", cid).
			Set("recorded", recorded), 2, true))
	} else {
		e.out("%s", rank.FitText(res))
		switch {
		case cid == nil:
			e.out("No choice to record: fewer than two attempts survived.\n")
		case recorded:
			e.out("Chose %s; card %s is open.\n", res.Value("leader"), cid)
		default:
			e.out("Choice %s is already recorded.\n", cid)
		}
	}
	return exitOrNil(code)
}

func exitOrNil(code int) error {
	if code == 0 {
		return nil
	}
	return exit(code)
}

// rankQueue is `rank queue`.
func (e *Env) rankQueue(args []string) error {
	const cmd = "rank queue"
	opts := append([]opt{{names: []string{"--sort"}, value: true, metavar: "TEXT",
		help: "reversibility, impact, date or project."}}, rankOpts()...)
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "List the open cards: what was chosen, why, and what switching would do.", opts)
	}
	by := p.str("--sort", "reversibility")
	ok := false
	for _, s := range rank.Sorts {
		ok = ok || s == by
	}
	if !ok {
		e.errf("Error: --sort must be reversibility, impact, date or project.\n")
		return exit(2)
	}
	views, decayed := rank.Queue(e.rankDataDir(p), rankNow(), by)
	if p.flag("--json") {
		e.out("%s\n", pyjson.DumpsIndent(rank.QueueJSON(views, decayed), 2, true))
	} else {
		e.out("%s", rank.QueueText(views, decayed))
	}
	return nil
}

// rankRecord is `rank record pick|drop`.
func (e *Env) rankRecord(args []string) error {
	if len(args) == 0 {
		e.out("%s", rankRecordHelp)
		return exit(2)
	}
	var cmd, summary string
	n := 1
	switch args[0] {
	case "--help":
		e.out("%s", rankRecordHelp)
		return nil
	case "pick":
		cmd, summary, n = "rank record pick", "Pick an attempt on a card: the chosen one confirms it, another overrides it.", 2
	case "drop":
		cmd, summary = "rank record drop", "Drop a card: the chosen attempt is kept, recorded as confirmed."
	default:
		e.errf("Usage: daisugi rank record [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi rank record --help' for help.\n\nError: No such command '%s'.\n", args[0])
		return exit(2)
	}
	opts := rankOpts()
	names := "CHOICE"
	if n == 2 {
		names = "CHOICE ATTEMPT"
	}
	p, err := parseArgs(args[1:], opts, n)
	if err != nil {
		return e.usageArgs(cmd, names, err)
	}
	if p.help {
		return e.cmdHelp(cmd, " "+names, summary, opts)
	}
	if len(p.args) < 1 {
		return e.usageArgs(cmd, names, &usageError{"Missing argument 'CHOICE'."})
	}
	if n == 2 && len(p.args) < 2 {
		return e.usageArgs(cmd, names, &usageError{"Missing argument 'ATTEMPT'."})
	}
	pick := ""
	if n == 2 {
		pick = p.args[1]
	}
	return e.rankAnswer(cmd, e.rankDataDir(p), p.args[0], pick, n == 2, p.flag("--json"))
}

// rankAnswer is cli._rank_answer.
func (e *Env) rankAnswer(cmd, dataDir, cid, pick string, hasPick, jsonOut bool) error {
	t := rankNow()
	if err := rank.AppendRows(dataDir, rank.Sweep(dataDir, t)); err != nil {
		return e.fail(cmd, err)
	}
	var card *rank.Card
	for _, c := range rank.ReadCards(dataDir) {
		if c.ID() == cid {
			card = c
			break
		}
	}
	what := "Tried to record an answer on choice " + cid + "."
	if card == nil {
		return e.fail3(what, "There is no choice "+cid+".", "Run daisugi rank queue to see the open cards.", 1)
	}
	if card.Answer != nil {
		return e.fail3(what, fmt.Sprintf("It is answered already: %s (%s).", card.Answer.Value("event"), card.Answer.Value("how")),
			"An answer stands; record a new choice to change the work.", 1)
	}
	chosen := card.Opened.Value("chosen").(string)
	rid := card.Opened.Value("ranking_id")
	var surv []string
	for _, s := range card.Opened.Value("options").(*pyjson.Object).Value("survivors").([]any) {
		surv = append(surv, s.(*pyjson.Object).Value("id").(string))
	}
	if hasPick {
		found := false
		for _, s := range surv {
			found = found || s == pick
		}
		if !found {
			return e.fail3(what, fmt.Sprintf("%s is not an attempt that survived; the survivors are %s.", pick, rank.Join(surv)),
				"Pick one of them and run again.", 2)
		}
	}
	if !hasPick || pick == chosen {
		how := "drop"
		if hasPick {
			how = "answer"
		}
		row := pyjson.NewObject().Set("choice_id", cid).Set("ranking_id", rid).Set("event", "confirmed").
			Set("how", how).Set("ts", t)
		if err := rank.AppendRows(dataDir, []*pyjson.Object{row}); err != nil {
			return e.fail(cmd, err)
		}
		if jsonOut {
			e.out("%s\n", pyjson.DumpsIndent(pyjson.NewObject().Set("choice_id", cid).Set("event", "confirmed").
				Set("chosen", chosen), 2, true))
		} else {
			e.out("Kept %s on %s; recorded as confirmed.\n", chosen, cid)
		}
		return nil
	}
	sc := rank.SwitchCost(card, dataDir, pick)
	row := pyjson.NewObject().Set("choice_id", cid).Set("ranking_id", rid).Set("event", "overridden").
		Set("pick", pick).Set("how", "answer").Set("ts", t)
	if err := rank.AppendRows(dataDir, []*pyjson.Object{row}); err != nil {
		return e.fail(cmd, err)
	}
	if jsonOut {
		e.out("%s\n", pyjson.DumpsIndent(pyjson.NewObject().Set("choice_id", cid).Set("event", "overridden").
			Set("chosen", chosen).Set("pick", pick).Set("switch", pyjson.NewObject().Set("cost", sc.Cost).
			Set("undo_steps", sc.UndoSteps).Set("text", sc.Text)), 2, true))
	} else {
		e.out("Recorded on %s: switch from %s to %s.\n", cid, chosen, pick)
		e.out("What switching will do (%s): %s\n", sc.Cost, sc.Text)
		e.out("The pick is recorded; the work is not changed yet.\n")
	}
	return nil
}

package rank

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// DecaySeconds is rank.DECAY_SECONDS.
const DecaySeconds = 7 * 86400

// Switch costs.
const (
	Cheap    = "cheap"
	Costly   = "costly"
	FollowUp = "follow_up_only"
)

// RankingsDir is rank.rankings_dir.
func RankingsDir(dataDir string) string { return filepath.Join(dataDir, "journal", "rankings") }

// readRows is rank._read_rows: the JSON object rows of a JSONL file (read
// as text with replacement and universal newlines) and how many lines did
// not read.
func readRows(path string) ([]*pyjson.Object, int) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, 0
	}
	text := pystr.DecodeReplace(raw)
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	var rows []*pyjson.Object
	bad := 0
	for _, line := range strings.Split(text, "\n") {
		if pystr.Strip(line) == "" {
			continue
		}
		v, jerr := pyjson.LoadsPy(line, 900)
		if jerr != nil {
			bad++
			continue
		}
		if o, ok := v.(*pyjson.Object); ok {
			rows = append(rows, o)
		} else {
			bad++
		}
	}
	return rows, bad
}

// JournalComparisons is rank.journal_comparisons.
func JournalComparisons(dataDir, rankingID string) ([]Comparison, int) {
	rows, bad := readRows(filepath.Join(RankingsDir(dataDir), "comparisons.jsonl"))
	out := []Comparison{}
	for _, row := range rows {
		if s, ok := row.Value("ranking_id").(string); !ok || s != rankingID {
			continue
		}
		c, err := ParseComparison(row, len(out))
		if err != nil {
			bad++
			continue
		}
		out = append(out, c)
	}
	return out, bad
}

// ChoiceID is rank.choice_id.
func ChoiceID(rankingID string, survivors [][2]string) string {
	s := append([][2]string{}, survivors...)
	sort.Slice(s, func(i, j int) bool {
		if s[i][0] != s[j][0] {
			return s[i][0] < s[j][0]
		}
		return s[i][1] < s[j][1]
	})
	var l []any
	for _, x := range s {
		l = append(l, []any{x[0], x[1]})
	}
	if l == nil {
		l = []any{}
	}
	sum := sha256.Sum256([]byte(pyjson.CanonicalASCII([]any{rankingID, l})))
	return "ch_" + hex.EncodeToString(sum[:])[:12]
}

// Card is rank.Card: the fold of one choice's rows.
type Card struct {
	Opened, Close, Answer *pyjson.Object
	// Resumed holds the runs that resumed the plan while the choice was
	// open, each once, in file order.
	Resumed []string
}

// ID is the card's choice id.
func (c *Card) ID() string { return c.Opened.Value("choice_id").(string) }

func (c *Card) ts() float64 {
	f, _ := Finite(c.Opened.Value("ts"))
	return f
}

func (c *Card) survivors() []*pyjson.Object {
	var out []*pyjson.Object
	for _, s := range c.Opened.Value("options").(*pyjson.Object).Value("survivors").([]any) {
		out = append(out, s.(*pyjson.Object))
	}
	return out
}

func (c *Card) chosen() string { return c.Opened.Value("chosen").(string) }

func (c *Card) facts() *pyjson.Object {
	if f, ok := c.Opened.Value("facts").(*pyjson.Object); ok {
		return f
	}
	return pyjson.NewObject()
}

func validOpen(row *pyjson.Object) bool {
	for _, k := range []string{"choice_id", "ranking_id", "chosen"} {
		if _, ok := row.Value(k).(string); !ok {
			return false
		}
	}
	if _, ok := Finite(row.Value("ts")); !ok {
		return false
	}
	opts, ok := row.Value("options").(*pyjson.Object)
	if !ok {
		return false
	}
	surv, ok := opts.Value("survivors").([]any)
	if !ok {
		return false
	}
	for _, s := range surv {
		so, ok := s.(*pyjson.Object)
		if !ok {
			return false
		}
		if _, ok := so.Value("id").(string); !ok {
			return false
		}
		if _, ok := so.Value("content_hash").(string); !ok {
			return false
		}
	}
	return true
}

// ReadCards is rank.read_cards.
func ReadCards(dataDir string) []*Card {
	rows, _ := readRows(filepath.Join(RankingsDir(dataDir), "choices.jsonl"))
	cards := map[string]*Card{}
	var order []string
	for _, row := range rows {
		cid, ok := row.Value("choice_id").(string)
		if !ok {
			continue
		}
		ev := row.Value("event")
		if ev == "opened" {
			if cards[cid] == nil && validOpen(row) {
				cards[cid] = &Card{Opened: row}
				order = append(order, cid)
			}
			continue
		}
		card := cards[cid]
		evs, _ := ev.(string)
		if card != nil && evs == "resumed" {
			if run, ok := row.Value("run_id").(string); ok && !slices.Contains(card.Resumed, run) {
				card.Resumed = append(card.Resumed, run)
			}
			continue
		}
		if card == nil || (evs != "confirmed" && evs != "overridden" && evs != "ignored") {
			continue
		}
		how, _ := row.Value("how").(string)
		_, howIsStr := row.Value("how").(string)
		if evs == "confirmed" || evs == "overridden" {
			if !howIsStr || (how != "answer" && how != "drop" && how != "permanent_ask") {
				continue
			}
			if _, ok := row.Value("pick").(string); evs == "overridden" && !ok {
				continue
			}
			if card.Answer == nil {
				card.Answer = row
			}
		} else if !howIsStr || how != "decay" {
			continue
		}
		if card.Close == nil {
			card.Close = row
		}
	}
	out := make([]*Card, 0, len(order))
	for _, cid := range order {
		out = append(out, cards[cid])
	}
	return out
}

// OwnerAnswers is rank.owner_answers.
func OwnerAnswers(dataDir, rankingID string, byID map[string]*Attempt, warnings *[]string) []Pair {
	out := []Pair{}
	for _, card := range ReadCards(dataDir) {
		if card.Opened.Value("ranking_id") != rankingID || card.Answer == nil {
			continue
		}
		var ids []string
		hashes := map[string]string{}
		for _, s := range card.survivors() {
			id := s.Value("id").(string)
			if _, seen := hashes[id]; !seen {
				ids = append(ids, id)
			}
			hashes[id] = s.Value("content_hash").(string)
		}
		changed := false
		for _, id := range ids {
			if byID[id] == nil || byID[id].ContentHash != hashes[id] {
				changed = true
				break
			}
		}
		if changed {
			*warnings = append(*warnings, fmt.Sprintf("the owner's answer on %s binds to attempts that changed; ignored", card.ID()))
			continue
		}
		chosen := card.chosen()
		var pairs []Pair
		if card.Answer.Value("event") == "confirmed" {
			for _, id := range ids {
				if id != chosen {
					pairs = append(pairs, Pair{chosen, id})
				}
			}
		} else {
			pick := card.Answer.Value("pick").(string)
			if _, ok := hashes[pick]; !ok || pick == chosen {
				continue
			}
			pairs = []Pair{{pick, chosen}}
		}
		for _, p := range pairs {
			dup := false
			for _, q := range out {
				dup = dup || q == p
			}
			if !dup {
				out = append(out, p)
			}
		}
	}
	return out
}

type receipt struct {
	step, rev string
	at        float64
}

// receipts is rank._receipts: a run's receipts from the index, read-only.
func receipts(dataDir, runID string) []receipt {
	db := filepath.Join(dataDir, "journal", "index.db")
	abs, err := filepath.Abs(db)
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
	rows, err := con.Query("SELECT step_id, reversibility, timestamp FROM receipts WHERE run_id = ? "+
		"ORDER BY timestamp, step_id", runID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []receipt
	for rows.Next() {
		var step, rev, at any
		if err := rows.Scan(&step, &rev, &at); err != nil {
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
		s := ""
		switch x := step.(type) {
		case string:
			s = x
		case []byte:
			s = string(x)
		default:
			s = fmt.Sprint(x)
		}
		r := ""
		switch x := rev.(type) {
		case string:
			r = x
		case []byte:
			r = string(x)
		}
		out = append(out, receipt{s, r, t})
	}
	if rows.Err() != nil {
		return nil
	}
	return out
}

// Switch is a switch cost.
type Switch struct {
	Cost      string
	UndoSteps int
	FiredAt   *float64
	Text      string
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// SwitchCost is rank.switch_cost; to is "" for the cheapest alternative.
func SwitchCost(card *Card, dataDir, to string) Switch {
	facts := card.facts()
	chosen := card.chosen()
	var alts []string
	for _, s := range card.survivors() {
		if id := s.Value("id").(string); id != chosen {
			alts = append(alts, id)
		}
	}
	if to != "" {
		alts = []string{to}
	}
	where, _ := facts.Value("where").(*pyjson.Object)
	var gone []string
	for _, a := range alts {
		w, _ := where.Value(a).(*pyjson.Object)
		if p, ok := w.Value("path").(string); ok && !exists(p) {
			gone = append(gone, a)
		}
	}
	var later []receipt
	if run, ok := facts.Value("run").(*pyjson.Object); ok && run.Len() > 0 {
		rid, ok1 := run.Value("run_id").(string)
		down, ok2 := run.Value("downstream").([]any)
		if ok1 && ok2 {
			in := map[string]bool{}
			for _, d := range down {
				if s, ok := d.(string); ok {
					in[s] = true
				}
			}
			runs := []string{rid}
			for _, r := range card.Resumed {
				if r != rid {
					runs = append(runs, r)
				}
			}
			for _, run := range runs {
				for _, r := range receipts(dataDir, run) {
					if in[r.step] {
						later = append(later, r)
					}
				}
			}
			sort.SliceStable(later, func(i, j int) bool {
				if later[i].at != later[j].at {
					return later[i].at < later[j].at
				}
				return later[i].step < later[j].step
			})
		}
	}
	var hard, undo []receipt
	for _, r := range later {
		if r.rev != "none" && r.rev != "reversible" {
			hard = append(hard, r)
		}
		if r.rev == "reversible" {
			undo = append(undo, r)
		}
	}
	if len(hard) > 0 {
		at := hard[0].at
		return Switch{FollowUp, 0, &at, "Cannot undo: step " + hard[0].step +
			" ran on this choice and cannot be undone. Switching starts a follow-up task."}
	}
	if len(alts) > 0 && len(gone) == len(alts) {
		return Switch{FollowUp, 0, nil, "Cannot undo: the place of " + Join(gone) +
			" is gone. Switching starts a follow-up task."}
	}
	if len(undo) > 0 {
		set := map[string]bool{}
		var names []string
		for _, r := range undo {
			if !set[r.step] {
				set[r.step] = true
				names = append(names, r.step)
			}
		}
		sort.Strings(names)
		return Switch{Costly, len(undo), nil, "Switching undoes the later steps " + Join(names) +
			" and applies the alternative."}
	}
	return Switch{Cheap, 0, nil, "Switching applies the alternative; no later step that changes anything ran on this choice."}
}

// Decay is rank.decay: the ignored row an open card's decay would append.
func Decay(card *Card, dataDir string, now float64) *pyjson.Object {
	sc := SwitchCost(card, dataDir, "")
	type fire struct {
		at      float64
		trigger string
	}
	var fires []fire
	if sc.Cost == FollowUp {
		at := now
		if sc.FiredAt != nil {
			at = *sc.FiredAt
		}
		fires = append(fires, fire{at, "follow_up_only"})
	}
	due := card.ts() + DecaySeconds
	if now >= due {
		fires = append(fires, fire{due, "time"})
	}
	if len(fires) == 0 {
		return nil
	}
	best := fires[0]
	for _, f := range fires[1:] {
		if f.at < best.at || (f.at == best.at && f.trigger < best.trigger) {
			best = f
		}
	}
	return pyjson.NewObject().Set("choice_id", card.ID()).Set("ranking_id", card.Opened.Value("ranking_id")).
		Set("event", "ignored").Set("how", "decay").Set("trigger", best.trigger).Set("fired_at", best.at).Set("ts", now)
}

// AppendRows is rank.append_rows.
func AppendRows(dataDir string, rows []*pyjson.Object) error {
	if len(rows) == 0 {
		return nil
	}
	d := RankingsDir(dataDir)
	if err := os.MkdirAll(d, 0o777); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(d, "choices.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
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

// Sweep is rank.sweep.
func Sweep(dataDir string, now float64) []*pyjson.Object {
	var rows []*pyjson.Object
	for _, card := range ReadCards(dataDir) {
		if card.Close == nil {
			if row := Decay(card, dataDir, now); row != nil {
				rows = append(rows, row)
			}
		}
	}
	return rows
}

// OpenedRow is rank.opened_row.
func OpenedRow(r *Ranking, res *pyjson.Object, now float64, run *pyjson.Object) *pyjson.Object {
	byID := map[string]*Attempt{}
	for _, a := range r.Attempts {
		byID[a.ID] = a
	}
	order := res.Value("order").([]any)
	var surv [][2]string
	survOut := []any{}
	cost := pyjson.NewObject()
	summary := pyjson.NewObject()
	where := pyjson.NewObject()
	for _, mv := range order {
		m := mv.(string)
		a := byID[m]
		surv = append(surv, [2]string{m, a.ContentHash})
		survOut = append(survOut, pyjson.NewObject().Set("id", m).Set("content_hash", a.ContentHash))
		cost.Set(m, a.Cost)
		if a.Summary != "" {
			summary.Set(m, a.Summary)
		}
		if a.hasWhere {
			where.Set(m, a.Where)
		}
	}
	var runV any
	if run != nil {
		runV = run
	}
	return pyjson.NewObject().Set("choice_id", ChoiceID(r.ID, surv)).Set("ranking_id", r.ID).
		Set("project", r.Project).Set("task", r.Task).Set("event", "opened").
		Set("options", pyjson.NewObject().Set("survivors", survOut).Set("eliminated", res.Value("eliminated"))).
		Set("chosen", res.Value("leader")).Set("status", res.Value("status")).
		Set("facts", pyjson.NewObject().Set("order", order).Set("decided_by", res.Value("decided_by")).
			Set("quality_tiers", res.Value("quality_tiers")).Set("confidence", res.Value("confidence")).
			Set("cost", cost).Set("summary", summary).Set("where", where).Set("run", runV)).
		Set("ts", now)
}

var whyText = map[string]string{
	"quality":    "%[1]s passed more optional tests or features than %[2]s.",
	"preference": "The judges preferred %[1]s to %[2]s.",
	"cost":       "Nothing but cost separated %[1]s and %[2]s; %[1]s cost less.",
	"tie":        "Nothing separated %[1]s and %[2]s; %[1]s came first by name.",
	"owner":      "You placed %[1]s before %[2]s.",
}

func (c *Card) named(m string) string {
	summ, _ := c.facts().Value("summary").(*pyjson.Object)
	if s, ok := summ.Value(m).(string); ok && s != "" {
		return m + " (" + s + ")"
	}
	return m
}

func boundedInt(v any) (int64, bool) {
	n, ok := isInt(v)
	if !ok || n.Sign() < 0 || n.Cmp(limit53) > 0 {
		return 0, false
	}
	return n.Int64(), true
}

// View is one card as the queue shows it.
type View struct {
	Obj        *pyjson.Object
	cost       string
	openedAt   float64
	confidence *float64
	impact     int64
	project    string
	id         string
}

// CardView is rank.card_view.
func CardView(card *Card, dataDir string, now float64) View {
	o := card.Opened
	openedAt := card.ts()
	facts := card.facts()
	chosen := card.chosen()
	var alts []string
	for _, s := range card.survivors() {
		if id := s.Value("id").(string); id != chosen {
			alts = append(alts, id)
		}
	}
	var elim []string
	if el, ok := o.Value("options").(*pyjson.Object).Value("eliminated").([]any); ok {
		for _, e := range el {
			if eo, ok := e.(*pyjson.Object); ok {
				if id, ok := eo.Value("id").(string); ok {
					elim = append(elim, id)
				}
			}
		}
	}
	var named []string
	for _, a := range alts {
		named = append(named, card.named(a))
	}
	head := "Kept " + card.named(chosen) + " over " + Join(named)
	if len(elim) > 0 {
		head += "; " + Join(elim) + " failed a required check"
	}
	head += "."
	order, _ := facts.Value("order").([]any)
	decided, _ := facts.Value("decided_by").([]any)
	why := ""
	if len(order) > 1 && len(decided) > 0 {
		d, _ := decided[0].(string)
		x, okx := order[0].(string)
		y, oky := order[1].(string)
		if t, ok := whyText[d]; ok && okx && oky {
			why = fmt.Sprintf(t, x, y)
		}
	}
	var conf *float64
	if f, ok := Finite(facts.Value("confidence")); ok {
		conf = &f
		why = pystr.Strip(why + " Confidence " + pyjson.FloatRepr(R9(f)) + ".")
	}
	sc := SwitchCost(card, dataDir, "")
	costs, _ := facts.Value("cost").(*pyjson.Object)
	linesOf := func(m string) (int64, bool) {
		c, _ := costs.Value(m).(*pyjson.Object)
		return boundedInt(c.Value("diff_lines"))
	}
	var impact int64
	if base, ok := linesOf(chosen); ok {
		for _, a := range alts {
			if la, ok := linesOf(a); ok {
				d := la - base
				if d < 0 {
					d = -d
				}
				if d > impact {
					impact = d
				}
			}
		}
	}
	str := func(k string) string {
		s, _ := o.Value(k).(string)
		return s
	}
	var status, confV any
	if s, ok := o.Value("status").(string); ok {
		status = s
	}
	if conf != nil {
		confV = *conf
	}
	days := math.Ceil((openedAt + DecaySeconds - now) / 86400)
	if days < 0 {
		days = 0
	}
	obj := pyjson.NewObject().Set("choice_id", card.ID()).Set("ranking_id", o.Value("ranking_id")).
		Set("project", str("project")).Set("task", str("task")).Set("chosen", chosen).
		Set("alternatives", strList(alts)).Set("eliminated", strList(elim)).Set("status", status).
		Set("confidence", confV).Set("opened_at", openedAt).Set("decays_in_days", int(days)).
		Set("headline", head).Set("why", why).
		Set("switch", pyjson.NewObject().Set("cost", sc.Cost).Set("undo_steps", sc.UndoSteps).Set("text", sc.Text)).
		Set("impact_lines", int(impact))
	return View{obj, sc.Cost, openedAt, conf, impact, str("project"), card.ID()}
}

// Sorts is rank.SORTS.
var Sorts = []string{"reversibility", "impact", "date", "project"}

var costRank = map[string]int{Cheap: 0, Costly: 1, FollowUp: 2}

// SortCards is rank.sort_cards.
func SortCards(vs []View, by string) {
	sort.SliceStable(vs, func(i, j int) bool {
		a, b := vs[i], vs[j]
		switch by {
		case "reversibility":
			if costRank[a.cost] != costRank[b.cost] {
				return costRank[a.cost] < costRank[b.cost]
			}
			if a.openedAt != b.openedAt {
				return a.openedAt < b.openedAt
			}
		case "impact":
			if a.impact != b.impact {
				return a.impact > b.impact
			}
			ca, cb := 1.0, 1.0
			if a.confidence != nil {
				ca = *a.confidence
			}
			if b.confidence != nil {
				cb = *b.confidence
			}
			if ca != cb {
				return ca < cb
			}
		case "date":
			if a.openedAt != b.openedAt {
				return a.openedAt > b.openedAt
			}
		default:
			if a.project != b.project {
				return a.project < b.project
			}
			if a.openedAt != b.openedAt {
				return a.openedAt > b.openedAt
			}
		}
		return a.id < b.id
	})
}

// Queue is rank.queue: the open cards, and how many decayed unread.
func Queue(dataDir string, now float64, by string) ([]View, int) {
	var views []View
	decayed := 0
	for _, card := range ReadCards(dataDir) {
		if card.Close != nil {
			continue
		}
		if Decay(card, dataDir, now) != nil {
			decayed++
			continue
		}
		views = append(views, CardView(card, dataDir, now))
	}
	SortCards(views, by)
	return views, decayed
}

// QueueJSON is the queue as `rank queue --json` prints it.
func QueueJSON(views []View, decayed int) *pyjson.Object {
	cards := []any{}
	for _, v := range views {
		cards = append(cards, v.Obj)
	}
	return pyjson.NewObject().Set("cards", cards).Set("decayed", decayed)
}

func isoUTC(t float64) string {
	return time.Unix(int64(math.Floor(t)), 0).UTC().Format("2006-01-02T15:04:05Z")
}

// QueueText is rank.queue_text.
func QueueText(views []View, decayed int) string {
	var b strings.Builder
	if len(views) == 0 {
		b.WriteString("No open cards.\n")
	} else {
		fmt.Fprintf(&b, "%d open cards:\n", len(views))
	}
	for _, v := range views {
		o := v.Obj
		where := ""
		if v.project != "" {
			where = ", " + v.project
		}
		fmt.Fprintf(&b, "%s (%s%s), opened %s\n", v.id, o.Value("ranking_id"), where, isoUTC(v.openedAt))
		fmt.Fprintf(&b, "  %s\n", o.Value("headline"))
		if w := o.Value("why").(string); w != "" {
			fmt.Fprintf(&b, "  Why: %s\n", w)
		}
		sw := o.Value("switch").(*pyjson.Object)
		fmt.Fprintf(&b, "  Switching (%s): %s\n", sw.Value("cost"), sw.Value("text"))
		fmt.Fprintf(&b, "  Decays in %v days unless switching gets expensive first.\n", o.Value("decays_in_days"))
	}
	if decayed > 0 {
		fmt.Fprintf(&b, "%d cards decayed unread; the next writer records them as ignored.\n", decayed)
	}
	return b.String()
}

// ErrNoChoice is the error of an answer on a choice that does not exist.
var ErrNoChoice = errors.New("no such choice")

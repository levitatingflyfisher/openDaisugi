package rank

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"daisugi-verify/internal/pyjson"
)

// ShowDoc is rank.show_doc: one ranking as the journal holds it. Reads only.
// The error text is the oracle's RankError text.
func ShowDoc(dataDir string, rankingID, choiceID *string, now float64) (*pyjson.Object, error) {
	if (rankingID == nil) == (choiceID == nil) {
		return nil, &Error{"Give --ranking or --choice, not both."}
	}
	cards := ReadCards(dataDir)
	var card *Card
	rid := ""
	if choiceID != nil {
		for _, c := range cards {
			if c.ID() == *choiceID {
				card = c
				break
			}
		}
		if card == nil {
			return nil, &Error{"No choice " + *choiceID + "."}
		}
		rid, _ = card.Opened.Value("ranking_id").(string)
	} else {
		rid = *rankingID
		for _, c := range cards {
			if c.Opened.Value("ranking_id") == rid {
				card = c
				break
			}
		}
	}
	compRows, bad := readRows(filepath.Join(RankingsDir(dataDir), "comparisons.jsonl"))
	comps := []any{}
	for _, r := range compRows {
		if r.Value("ranking_id") == rid {
			comps = append(comps, r)
		}
	}
	choiceRows, _ := readRows(filepath.Join(RankingsDir(dataDir), "choices.jsonl"))
	choices := []any{}
	for _, r := range choiceRows {
		if r.Value("ranking_id") == rid {
			choices = append(choices, r)
		}
	}
	if len(comps) == 0 && len(choices) == 0 {
		return nil, &Error{"No ranking " + rid + "."}
	}
	warnings := []string{}
	if bad > 0 {
		warnings = append(warnings, fmt.Sprintf("%d comparison rows did not read", bad))
	}
	answers := []any{}
	var cardV any
	if card != nil {
		byID := map[string]*Attempt{}
		for _, s := range card.survivors() {
			id, _ := s.Value("id").(string)
			h, _ := s.Value("content_hash").(string)
			byID[id] = &Attempt{ID: id, ContentHash: h}
		}
		for _, p := range OwnerAnswers(dataDir, rid, byID, &warnings) {
			answers = append(answers, []any{p.Win, p.Lose})
		}
		cardV = CardView(card, dataDir, now).Obj
	}
	return pyjson.NewObject().Set("ranking_id", rid).Set("comparisons", comps).Set("choices", choices).
		Set("card", cardV).Set("owner_answers", answers).Set("warnings", strList(warnings)), nil
}

func anyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	}
	return fmt.Sprint(pyjson.Dumps(v, true))
}

// ShowText is rank.show_text.
func ShowText(doc *pyjson.Object) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Ranking %s\n", doc.Value("ranking_id"))
	comps := doc.Value("comparisons").([]any)
	fmt.Fprintf(&b, "Votes: %d comparison rows.\n", len(comps))
	for _, c := range comps {
		o := c.(*pyjson.Object)
		fmt.Fprintf(&b, "  %s %s shown %s: %s vs %s -> %s\n", anyStr(o.Value("judge")), anyStr(o.Value("pair_id")),
			anyStr(o.Value("shown")), anyStr(o.Value("a")), anyStr(o.Value("b")), anyStr(o.Value("outcome")))
	}
	choices := doc.Value("choices").([]any)
	fmt.Fprintf(&b, "Choices: %d rows.\n", len(choices))
	for _, c := range choices {
		o := c.(*pyjson.Object)
		fmt.Fprintf(&b, "  %s %s\n", anyStr(o.Value("choice_id")), anyStr(o.Value("event")))
	}
	if card, ok := doc.Value("card").(*pyjson.Object); ok {
		fmt.Fprintf(&b, "Card %s: %s\n", card.Value("choice_id"), card.Value("headline"))
	}
	for _, p := range doc.Value("owner_answers").([]any) {
		pp := p.([]any)
		fmt.Fprintf(&b, "Owner: %s before %s\n", pp[0], pp[1])
	}
	for _, w := range doc.Value("warnings").([]any) {
		fmt.Fprintf(&b, "Warning: %s\n", w)
	}
	return b.String()
}

// Now is time.time().
func Now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

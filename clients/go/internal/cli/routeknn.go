package cli

import (
	"strconv"

	"daisugi-verify/internal/gateway"
	"daisugi-verify/internal/pyjson"
)

// knnTurn is one booked turn in route_knn.outcome_summary's recent list.
type knnTurn struct{ turnID, model, outcome string }

// knnState is route_knn.outcome_summary: counts by outcome and the last
// few booked turns, oldest first.
type knnState struct {
	path                       string
	booked, good, bad, unknown int
	recent                     []knnTurn
}

// knnSummary is outcome_summary for the table at path.
func knnSummary(path string) (*knnState, error) {
	rows, err := (&gateway.OutcomeStore{Path: path}).All(gateway.KnnMaxRows)
	if err != nil {
		return nil, err
	}
	st := &knnState{path: path, booked: len(rows)}
	for _, r := range rows {
		switch r.Outcome {
		case gateway.KnnGood:
			st.good++
		case gateway.KnnBad:
			st.bad++
		case gateway.KnnUnknown:
			st.unknown++
		}
	}
	n := len(rows)
	if n > 10 {
		n = 10
	}
	for i := n - 1; i >= 0; i-- {
		st.recent = append(st.recent, knnTurn{rows[i].TurnID, rows[i].Model, rows[i].Outcome})
	}
	return st, nil
}

func (k *knnState) object() *pyjson.Object {
	rs := make([]any, len(k.recent))
	for i, t := range k.recent {
		rs[i] = pyjson.NewObject().Set("turn_id", t.turnID).Set("model", t.model).Set("outcome", t.outcome)
	}
	return pyjson.NewObject().Set("path", k.path).
		Set("booked", pyjson.Int{Text: strconv.Itoa(k.booked)}).Set("good", pyjson.Int{Text: strconv.Itoa(k.good)}).
		Set("bad", pyjson.Int{Text: strconv.Itoa(k.bad)}).Set("unknown", pyjson.Int{Text: strconv.Itoa(k.unknown)}).
		Set("recent", rs)
}

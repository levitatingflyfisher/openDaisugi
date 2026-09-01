package rank

import (
	"testing"

	"daisugi-verify/internal/pyjson"
)

func TestSplitMixKnownValues(t *testing.T) {
	g := &SplitMix64{0}
	want := []uint64{0xE220A8397B1DCDAF, 0x6E789E6AA1B965F4, 0x06C45D188009454F}
	for i, w := range want {
		if got := g.Next(); got != w {
			t.Fatalf("draw %d: got %x, want %x", i, got, w)
		}
	}
}

func doc(t *testing.T, text string) *Ranking {
	t.Helper()
	v, err := pyjson.LoadsPy(text, 900)
	if err != nil {
		t.Fatal(err)
	}
	r, perr := Parse(v)
	if perr != nil {
		t.Fatal(perr)
	}
	return r
}

func TestParseRefusesADuplicateID(t *testing.T) {
	v, _ := pyjson.LoadsPy(`{"ranking_id": "r", "attempts": [{"id": "a", "content_hash": "x"}, {"id": "a", "content_hash": "y"}]}`, 900)
	if _, err := Parse(v); err == nil || err.Error() != "attempt id a is used twice" {
		t.Fatalf("got %v", err)
	}
}

func TestVotesHoldOnlyInBothOrders(t *testing.T) {
	r := doc(t, `{"ranking_id": "r", "attempts": [{"id": "a", "content_hash": "ha"}, {"id": "b", "content_hash": "hb"}],
	 "comparisons": [
	  {"a": "a", "b": "b", "a_hash": "ha", "b_hash": "hb", "shown": "ab", "outcome": "b", "judge": "j", "pair_id": "p"},
	  {"a": "b", "b": "a", "a_hash": "hb", "b_hash": "ha", "shown": "ba", "outcome": "a", "judge": "j", "pair_id": "p"}],
	 "policy": {"bootstrap": 50}}`)
	res := Fit(r, nil, nil, nil)
	order := res.Value("order").([]any)
	if order[0] != "b" {
		t.Fatalf("order %v", order)
	}
	if res.Value("status") != "ranked" {
		t.Fatalf("status %v", res.Value("status"))
	}
}

func TestNoSurvivorChoosesNothing(t *testing.T) {
	r := doc(t, `{"ranking_id": "r", "attempts": [{"id": "a", "content_hash": "ha", "tests": [{"name": "t", "required": true, "result": "fail"}]}]}`)
	res := Fit(r, nil, nil, nil)
	if res.Value("status") != "none_survived" || res.Value("leader") != nil {
		t.Fatalf("got %v", pyjson.Dumps(res, true))
	}
}

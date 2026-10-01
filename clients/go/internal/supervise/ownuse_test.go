package supervise

import (
	"testing"

	"daisugi-verify/internal/pyjson"
)

// A negative count makes the token report no report; the sum caps at 2**53.
func TestOwnUseTakesNoNegativeCountAndCapsTheSum(t *testing.T) {
	read := func(s string) *pyjson.Object {
		v, err := pyjson.Loads(s)
		if err != nil {
			t.Fatal(err)
		}
		return v.(*pyjson.Object)
	}
	tok, turns := ownUse(read(`{"usage": {"input_tokens": 300, "output_tokens": -250}, "model_turns": 2}`))
	if tok != nil || turns == nil || turns.Int64() != 2 {
		t.Fatalf("negative: %v %v", tok, turns)
	}
	tok, _ = ownUse(read(`{"usage": {"input_tokens": 4611686018427387904, "output_tokens": 4611686018427387904}}`))
	if tok == nil || tok.Cmp(maxCount) != 0 {
		t.Fatalf("huge: %v", tok)
	}
}

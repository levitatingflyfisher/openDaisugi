package strata

import (
	"strings"
	"testing"

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
)

func emit(t *testing.T, s *Store, kind, content, status string, pinned bool, tags ...string) *pyjson.Object {
	ts := []any{}
	for _, x := range tags {
		ts = append(ts, x)
	}
	o, err := s.Emit(kind, content, "", status, ts, pinned)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// Reconstruction keeps the pinned strata and open constraints, then fills
// the budget by relevance, and shows them in the order found.
func TestReconstructKeepsConstraintsAndFillsByRelevance(t *testing.T) {
	s := &Store{}
	emit(t, s, "fact", "old fact", "open", false)
	emit(t, s, "constraint", "never touch prod", "open", false)
	emit(t, s, "fact", "the DB is Postgres", "open", false, "db")
	emit(t, s, "hypothesis", "cache bug", "ruled_out", false)
	two := int64(2)
	r := s.Reconstruct(&two, []string{"db"}, "")
	var got []string
	for _, x := range r.Value("strata").([]any) {
		got = append(got, x.(*pyjson.Object).Value("content").(string))
	}
	if strings.Join(got, "|") != "never touch prod|the DB is Postgres" {
		t.Fatalf("chose %v", got)
	}
	if !strings.HasPrefix(r.Value("note").(string), "Reconstruction is lossy: 2 stratum(s) dropped") {
		t.Fatal(r.Value("note"))
	}
	if len(s.Reconstruct(nil, nil, "").Value("dropped_ids").([]any)) != 0 {
		t.Fatal("no budget dropped something")
	}
}

// A store round-trips through its JSON; text that is not JSON does not
// read.
func TestAStoreRoundTripsThroughItsJSON(t *testing.T) {
	s := &Store{}
	emit(t, s, "goal", "ship ☃", "open", true, "a")
	back, err := FromJSON(s.ToJSON())
	if err != nil || back.ToJSON() != s.ToJSON() {
		t.Fatalf("%v %q", err, back.ToJSON())
	}
	if _, err := FromJSON("{"); err == nil {
		t.Fatal("bad JSON read")
	}
}

// An emit that does not validate still takes a sequence number, and an
// unknown id is KeyError.
func TestAnEmitThatDoesNotValidateStillTakesANumber(t *testing.T) {
	s := &Store{}
	if _, err := s.Emit("rumor", "x", "", "open", nil, false); err == nil {
		t.Fatal("a rumor validated")
	}
	o := emit(t, s, "fact", "y", "open", false)
	if o.Value("seq").(pyjson.Int).Text != "2" {
		t.Fatal(o.Value("seq"))
	}
	if _, err := s.SetStatus("nope", "resolved"); err == nil {
		t.Fatal("an unknown id was found")
	}
}

// Only a constraint that tightens the envelope is promoted.
func TestOnlyAConstraintThatTightensIsPromoted(t *testing.T) {
	v, _ := pyjson.Loads(`{"generated_by": "t", "task": "t", "permissions": {"file_write": ["/a/**", "/b/**"]}}`)
	ev, verr := pmodel.Validate("Envelope", pmodel.Envelope, v, pmodel.Python)
	if verr != nil {
		t.Fatal(verr)
	}
	env := ev.(*pyjson.Object)
	s := &Store{}
	fact := emit(t, s, "fact", "f", "open", false)
	if r, err := Promote(env, fact, nil, nil, nil, nil); err != nil || r.OK {
		t.Fatalf("a fact was promoted: %v", err)
	}
	c := emit(t, s, "constraint", "no /b", "open", false)
	same, err := Promote(env, c, nil, nil, nil, nil)
	if err != nil || same.OK || !strings.Contains(same.Reason, "no enforceable effect") {
		t.Fatalf("an empty promotion: %+v %v", same, err)
	}
	r, err := Promote(env, c, nil, []string{"/b/**"}, nil, nil)
	if err != nil || !r.OK || c.Value("status") != "promoted" {
		t.Fatalf("%+v %v", r, err)
	}
	if again, _ := Promote(env, c, nil, []string{"/a/**"}, nil, nil); again.OK {
		t.Fatal("a promoted constraint was promoted again")
	}
}

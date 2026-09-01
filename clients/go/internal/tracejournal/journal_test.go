package tracejournal

import (
	"testing"

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
)

func TestSinceISO(t *testing.T) {
	for _, c := range []struct {
		in   float64
		want string
	}{
		{0, "1970-01-01T00:00:00Z"},
		{1700000000, "2023-11-14T22:13:20Z"},
		{1700000000.5, "2023-11-14T22:13:20.500000Z"},
		{1700000000.0000004, "2023-11-14T22:13:20Z"},
		{1700000000.9999996, "2023-11-14T22:13:21Z"},
		{-1.25, "1969-12-31T23:59:58.750000Z"},
	} {
		if got := SinceISO(c.in); got != c.want {
			t.Errorf("SinceISO(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func plan(t *testing.T, text string) *pyjson.Object {
	t.Helper()
	v, err := pmodel.ValidateJSON("ActionPlan", pmodel.ActionPlan, text)
	if err != nil {
		t.Fatal(err)
	}
	return v.(*pyjson.Object)
}

func TestTopoOrder(t *testing.T) {
	// networkx's generations: roots in node order, then children in the
	// order their edges were added.
	p := plan(t, `{"id":"p","source":"s","task":"t","steps":[
		{"id":"c","type":"task","prompt":"x","depends_on":["a","b"]},
		{"id":"b","type":"task","prompt":"x"},
		{"id":"a","type":"shell","command":"ls"},
		{"id":"d","type":"task","prompt":"x","depends_on":["a"]}]}`)
	sig, ok := StructureSignature(p)
	if !ok || sig != "task→shell→task→task" {
		t.Fatalf("signature %q %v", sig, ok)
	}
	order, _ := TopoOrder(p)
	ids := ""
	for _, s := range order {
		ids += s.Value("id").(string)
	}
	if ids != "bacd" {
		t.Fatalf("order %s", ids)
	}
	cyc := plan(t, `{"id":"p","source":"s","task":"t","steps":[
		{"id":"a","type":"task","prompt":"x","depends_on":["b"]},{"id":"b","type":"task","prompt":"x","depends_on":["a"]}]}`)
	if _, err := TopoOrderErr(cyc); err == nil || err.Error() != "Plan has a cycle; run verify(plan, envelope) before supervising" {
		t.Fatalf("cycle: %v", err)
	}
	unknown := plan(t, `{"id":"p","source":"s","task":"t","steps":[{"id":"a","type":"task","prompt":"x","depends_on":["zz"]}]}`)
	if _, err := TopoOrderErr(unknown); err == nil || err.Error() != "'zz'" {
		t.Fatalf("unknown: %v", err)
	}
}

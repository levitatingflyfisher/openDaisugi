package foreman

import "testing"

func TestViewTextLeavesOutTheSkippedMessageAndNothingElse(t *testing.T) {
	c := openT(t, t.TempDir(), &fakeSummarizer{}, newClock(), nil)
	for _, w := range []string{"one", "two", "three"} {
		if _, err := c.Append(ctx, KindUser, w); err != nil {
			t.Fatal(err)
		}
	}
	id := func(s string) string { return s }
	all := c.ViewText(id, -1)
	if len(all) != 3 || all[1].Text != "1+1|two\n" {
		t.Fatalf("all = %+v", all)
	}
	got := c.ViewText(id, 1)
	if len(got) != 2 || got[0].Text != "0+1|one\n" || got[1].Text != "2+1|three\n" {
		t.Fatalf("skip 1 = %+v", got)
	}
	if c.ViewSize() != len(all[0].Text)+len(all[1].Text)+len(all[2].Text) {
		t.Fatal("ViewSize counts the skipped line: it must not change")
	}
}

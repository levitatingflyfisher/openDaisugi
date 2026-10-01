package foreman

import (
	"reflect"
	"testing"
)

// rbEntry is one entry of the rollback list: the oldest tick it keeps and
// its one-bit flag. The list is newest first.
type rbEntry struct {
	keep  bool
	state int64
	older *rbEntry
}

// rbPush is the rollback push, a binary counter: a 0 absorbs the new tick,
// a 1 carries its own tick down to the older entries. It is written here
// from the recipe's prose, as the reference the merge rule must match.
func rbPush(tick int64, l *rbEntry) *rbEntry {
	if l == nil {
		return &rbEntry{state: tick}
	}
	if !l.keep {
		return &rbEntry{keep: true, state: l.state, older: l.older}
	}
	return &rbEntry{state: tick, older: rbPush(l.state, l.older)}
}

// rbLines turns the list after tick t into view lines, oldest first. Each
// kept tick starts a line that runs to the next newer kept tick.
func rbLines(t *testing.T, l *rbEntry, tick int64) []NodeID {
	t.Helper()
	var starts []int64
	for e := l; e != nil; e = e.older {
		starts = append([]int64{e.state}, starts...)
	}
	out := make([]NodeID, 0, len(starts))
	for k, s := range starts {
		end := tick + 1
		if k+1 < len(starts) {
			end = starts[k+1]
		}
		n := end - s
		if n&(n-1) != 0 || s%n != 0 {
			t.Fatalf("tick %d: push line %d+%d is not a tree node", tick, s, n)
		}
		lv := 0
		for int64(1)<<lv < n {
			lv++
		}
		out = append(out, NodeID{L: lv, I: s / n})
	}
	return out
}

func allBuilt(NodeID) bool { return true }

// The merge order must equal the rollback push order at every step for
// t = 0..20000, with the push list's length as the budget.
func TestMergeOrderEqualsRollbackPush(t *testing.T) {
	var list *rbEntry
	var view []NodeID
	for tick := int64(0); tick <= 20000; tick++ {
		list = rbPush(tick, list)
		want := rbLines(t, list, tick)
		view = append(view, NodeID{L: 0, I: tick})
		total := tick + 1 // T is the message count, not the newest index
		for len(view) > len(want) {
			k := MostDue(view, total, allBuilt)
			if k < 0 {
				t.Fatalf("tick %d: no pair to merge in %v", tick, view)
			}
			view = mergeAt(view, k)
		}
		if !reflect.DeepEqual(view, want) {
			t.Fatalf("tick %d: merge order gives %v, push gives %v", tick, view, want)
		}
	}
}

// Measuring from the pair's first message is the known wrong rule; it must
// not match. This guards the test itself against passing for any rule.
func TestFirstMessageRuleDiverges(t *testing.T) {
	var list *rbEntry
	var view []NodeID
	match := 0
	for tick := int64(0); tick <= 2000; tick++ {
		list = rbPush(tick, list)
		want := rbLines(t, list, tick)
		view = append(view, NodeID{L: 0, I: tick})
		for len(view) > len(want) {
			best, bestNum, bestL := -1, int64(0), 0
			for k := 0; k+1 < len(view); k++ {
				if !siblings(view[k], view[k+1]) {
					continue
				}
				num := tick + 1 - view[k].First()
				if best < 0 || dueLess(bestNum, bestL, num, view[k].L) {
					best, bestNum, bestL = k, num, view[k].L
				}
			}
			view = mergeAt(view, best)
		}
		if reflect.DeepEqual(view, want) {
			match++
		}
	}
	if match == 2001 {
		t.Fatal("the first-message rule matched push at every step; the test cannot tell rules apart")
	}
}

func TestDueLessIsExact(t *testing.T) {
	cases := []struct {
		a     int64
		la    int
		b     int64
		lb    int
		aLess bool
	}{
		{1, 0, 1, 0, false},
		{1, 1, 1, 0, true},   // 0.5 < 1
		{2, 1, 1, 0, false},  // 1 == 1
		{3, 1, 1, 0, false},  // 1.5 > 1
		{1, 0, 3, 1, true},   // 1 < 1.5
		{1, 62, 0, 0, false}, // tiny > 0
		{0, 0, 1, 62, true},
		{1 << 62, 62, 1, 0, false}, // equal, no overflow
		{(1 << 62) + 1, 62, 1, 0, false},
		{(1 << 62) - 1, 62, 1, 0, true},
	}
	for _, c := range cases {
		if got := dueLess(c.a, c.la, c.b, c.lb); got != c.aLess {
			t.Errorf("dueLess(%d/2^%d, %d/2^%d) = %v, want %v", c.a, c.la, c.b, c.lb, got, c.aLess)
		}
	}
}

func TestMostDueSkipsUnbuiltParents(t *testing.T) {
	view := []NodeID{{1, 0}, {1, 1}, {0, 4}, {0, 5}}
	// Both pairs can merge; the old one is more due.
	if k := MostDue(view, 6, allBuilt); k != 0 {
		t.Fatalf("MostDue = %d, want 0", k)
	}
	notOld := func(id NodeID) bool { return id != NodeID{2, 0} }
	if k := MostDue(view, 6, notOld); k != 2 {
		t.Fatalf("MostDue with (2,0) unbuilt = %d, want 2", k)
	}
	none := func(NodeID) bool { return false }
	if k := MostDue(view, 6, none); k != -1 {
		t.Fatalf("MostDue with nothing built = %d, want -1", k)
	}
}

func TestNodeIDNames(t *testing.T) {
	id := NodeID{L: 3, I: 5}
	if id.First() != 40 || id.Count() != 8 || id.Last() != 47 || id.Name() != "40+8" {
		t.Fatalf("NodeID{3,5}: first %d count %d last %d name %q", id.First(), id.Count(), id.Last(), id.Name())
	}
	got, err := ParseName("40+8")
	if err != nil || got != id {
		t.Fatalf("ParseName(40+8) = %v, %v", got, err)
	}
	for _, bad := range []string{"", "40", "40+0", "40+3", "41+8", "-8+8", "x+1", "8+x", "0+9223372036854775807"} {
		if _, err := ParseName(bad); err == nil {
			t.Errorf("ParseName(%q) took a bad name", bad)
		}
	}
	if p := id.Parent(); p != (NodeID{4, 2}) {
		t.Fatalf("Parent = %v", p)
	}
	l, r := id.Children()
	if l != (NodeID{2, 10}) || r != (NodeID{2, 11}) {
		t.Fatalf("Children = %v %v", l, r)
	}
}

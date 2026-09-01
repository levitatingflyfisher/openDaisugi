package tiles

import (
	"errors"
	"testing"
)

func count(slots []string, pane string) int {
	n := 0
	for _, s := range slots {
		if s == pane {
			n++
		}
	}
	return n
}

func TestFillReplacesTheFocusedSlotOnly(t *testing.T) {
	ts := New("focus", 2)
	ts.Fill("p1")
	ts.Focus = 1
	ts.Fill("p2")
	ts.Focus = 0
	ts.Fill("p3")
	if ts.Slots[0] != "p3" || ts.Slots[1] != "p2" {
		t.Fatalf("slots %v", ts.Slots)
	}
}

func TestAPaneHasAtMostOneTile(t *testing.T) {
	ts := New("focus", 2)
	ts.Fill("p1")
	ts.Focus = 1
	ts.Fill("p1")
	if n := count(ts.Slots, "p1"); n != 1 {
		t.Fatalf("p1 in %d slots", n)
	}
}

func TestSwapKeepsFocusWithThePane(t *testing.T) {
	ts := New("focus", 2)
	ts.Fill("p1")
	ts.Focus = 1
	ts.Fill("p2")
	ts.Focus = 0
	if err := ts.Swap(0, 1); err != nil {
		t.Fatal(err)
	}
	if ts.Focused() != "p1" || ts.Slots[1] != "p1" {
		t.Fatalf("focus did not follow: %v focus=%d", ts.Slots, ts.Focus)
	}
}

func TestLockedRefusesSwap(t *testing.T) {
	ts := New("focus", 2)
	ts.Locked = true
	if err := ts.Swap(0, 1); err == nil {
		t.Fatal("swap while locked")
	}
}

func TestEmptySlotWinsOverFocus(t *testing.T) {
	ts := New("focus", 2)
	ts.Fill("p1")
	ts.Fill("p2")
	if ts.Slots[1] != "p2" || ts.Slots[0] != "p1" {
		t.Fatalf("empty slot not used: %v", ts.Slots)
	}
}

func TestNewMakesTheSlotsEachLayoutStartsWith(t *testing.T) {
	if got := len(New(Focus, 3).Slots); got != 3 {
		t.Fatalf("focus 3: %d slots", got)
	}
	if got := len(New(Focus, 0).Slots); got != 1 {
		t.Fatalf("focus 0 clamps to 1, got %d", got)
	}
	if got := len(New(One, 3).Slots); got != 1 {
		t.Fatalf("one: %d slots", got)
	}
	if got := len(New(All, 3).Slots); got != 0 {
		t.Fatalf("all: %d slots", got)
	}
	if ts := New(Focus, 2); ts.Focused() != "" {
		t.Fatalf("a new tile set has a focused pane: %q", ts.Focused())
	}
}

func TestFillUnderAllAppendsAndUnderOneReplaces(t *testing.T) {
	all := New(All, 0)
	all.Fill("p1")
	all.Fill("p2")
	if len(all.Slots) != 2 || all.Slots[1] != "p2" {
		t.Fatalf("all: %v", all.Slots)
	}
	one := New(One, 0)
	one.Fill("p1")
	one.Fill("p2")
	if len(one.Slots) != 1 || one.Slots[0] != "p2" {
		t.Fatalf("one: %v", one.Slots)
	}
}

func TestFillFocusesTheSlotItFilled(t *testing.T) {
	ts := New(Focus, 3)
	ts.Fill("p1")
	ts.Fill("p2")
	if ts.Focus != 1 {
		t.Fatalf("focus %d after filling slot 1", ts.Focus)
	}
	ts.Fill("p1")
	if ts.Focus != 0 {
		t.Fatalf("focus %d after filling a pane that already has a slot", ts.Focus)
	}
}

func TestFillIgnoresAnEmptyName(t *testing.T) {
	ts := New(Focus, 2)
	ts.Fill("")
	if ts.Slots[0] != "" || ts.Slots[1] != "" {
		t.Fatalf("an empty name changed the slots: %v", ts.Slots)
	}
	if i := ts.Open(""); i != -1 {
		t.Fatalf("Open of an empty name returned %d", i)
	}
}

func TestOpenAppendsAndFocusesANewSlot(t *testing.T) {
	ts := New(Focus, 2)
	ts.Fill("p1")
	ts.Fill("p2")
	if i := ts.Open("p3"); i != 2 || ts.Focus != 2 || len(ts.Slots) != 3 {
		t.Fatalf("open returned %d, slots %v, focus %d", i, ts.Slots, ts.Focus)
	}
}

func TestOpenOnAPaneAlreadyInASlotReturnsThatSlot(t *testing.T) {
	ts := New(Focus, 2)
	ts.Fill("p1")
	ts.Fill("p2")
	if i := ts.Open("p1"); i != 0 || ts.Focus != 0 || len(ts.Slots) != 2 {
		t.Fatalf("open returned %d, slots %v, focus %d", i, ts.Slots, ts.Focus)
	}
}

func TestOpenUnderOneReplacesSlotZero(t *testing.T) {
	ts := New(One, 0)
	ts.Fill("p1")
	if i := ts.Open("p2"); i != 0 || len(ts.Slots) != 1 || ts.Slots[0] != "p2" {
		t.Fatalf("open returned %d, slots %v", i, ts.Slots)
	}
}

func TestCloseSlotUnderAllRemovesTheSlot(t *testing.T) {
	ts := New(All, 0)
	ts.Fill("p1")
	ts.Fill("p2")
	ts.Fill("p3")
	ts.Focus = 2
	ts.CloseSlot(2)
	if len(ts.Slots) != 2 || ts.Focus != 1 {
		t.Fatalf("slots %v focus %d", ts.Slots, ts.Focus)
	}
	ts.CloseSlot(0)
	if len(ts.Slots) != 1 || ts.Slots[0] != "p2" || ts.Focus != 0 {
		t.Fatalf("slots %v focus %d", ts.Slots, ts.Focus)
	}
	ts.CloseSlot(0)
	if len(ts.Slots) != 0 || ts.Focus != 0 || ts.Focused() != "" {
		t.Fatalf("slots %v focus %d", ts.Slots, ts.Focus)
	}
}

func TestCloseSlotUnderFocusEmptiesTheSlot(t *testing.T) {
	ts := New(Focus, 2)
	ts.Fill("p1")
	ts.Fill("p2")
	ts.CloseSlot(0)
	if len(ts.Slots) != 2 || ts.Slots[0] != "" || ts.Slots[1] != "p2" {
		t.Fatalf("slots %v", ts.Slots)
	}
	ts.CloseSlot(5)
	if len(ts.Slots) != 2 {
		t.Fatalf("an index outside the slots changed them: %v", ts.Slots)
	}
}

func TestCloseSlotClampsFocusUnderAll(t *testing.T) {
	ts := New(All, 0)
	ts.Fill("p1")
	ts.Fill("p2")
	ts.Focus = 1
	ts.CloseSlot(1)
	if ts.Focus != 0 || ts.Focused() != "p1" {
		t.Fatalf("focus %d focused %q", ts.Focus, ts.Focused())
	}
}

func TestSwapRefusesAnIndexOutsideTheSlots(t *testing.T) {
	ts := New(Focus, 2)
	err := ts.Swap(0, 2)
	if err == nil {
		t.Fatal("swap with an index outside the slots was accepted")
	}
	if got := err.Error(); got != "Slot 2 is outside the slots 0 to 1." {
		t.Fatalf("error text %q", got)
	}
	if err := ts.Swap(-1, 0); err == nil {
		t.Fatal("swap with a negative index was accepted")
	}
}

func TestLockedRefusesRotateAndReset(t *testing.T) {
	ts := New(Focus, 2)
	ts.Fill("p1")
	ts.Locked = true
	if err := ts.Rotate(); !errors.Is(err, ErrLocked) {
		t.Fatalf("rotate while locked: %v", err)
	}
	if err := ts.Reset([]string{"p2"}); !errors.Is(err, ErrLocked) {
		t.Fatalf("reset while locked: %v", err)
	}
	if err := ts.Swap(0, 1); !errors.Is(err, ErrLocked) {
		t.Fatalf("swap while locked: %v", err)
	}
	if ts.Slots[0] != "p1" {
		t.Fatalf("a refused call changed the slots: %v", ts.Slots)
	}
}

func TestRotateMovesEverySlotRightAndKeepsFocusWithItsPane(t *testing.T) {
	ts := New(Focus, 3)
	ts.Fill("p1")
	ts.Fill("p2")
	ts.Fill("p3")
	ts.Focus = 1
	if err := ts.Rotate(); err != nil {
		t.Fatal(err)
	}
	want := []string{"p3", "p1", "p2"}
	for i := range want {
		if ts.Slots[i] != want[i] {
			t.Fatalf("slots %v", ts.Slots)
		}
	}
	if ts.Focus != 2 || ts.Focused() != "p2" {
		t.Fatalf("focus %d focused %q", ts.Focus, ts.Focused())
	}
	empty := New(All, 0)
	if err := empty.Rotate(); err != nil {
		t.Fatalf("rotate with no slots: %v", err)
	}
}

func TestResetUnderEachLayout(t *testing.T) {
	order := []string{"a", "b", "c"}

	all := New(All, 0)
	all.Fill("z")
	all.Focus = 0
	if err := all.Reset(order); err != nil {
		t.Fatal(err)
	}
	if len(all.Slots) != 3 || all.Slots[2] != "c" || all.Focus != 0 {
		t.Fatalf("all: %v focus %d", all.Slots, all.Focus)
	}
	order[0] = "changed"
	if all.Slots[0] != "a" {
		t.Fatal("reset under all shares the caller's slice")
	}

	focus := New(Focus, 2)
	focus.Fill("z")
	focus.Focus = 1
	if err := focus.Reset([]string{"a", "b", "c"}); err != nil {
		t.Fatal(err)
	}
	if len(focus.Slots) != 2 || focus.Slots[0] != "a" || focus.Slots[1] != "b" || focus.Focus != 0 {
		t.Fatalf("focus: %v focus %d", focus.Slots, focus.Focus)
	}
	wide := New(Focus, 3)
	wide.Fill("z")
	if err := wide.Reset([]string{"a"}); err != nil {
		t.Fatal(err)
	}
	if wide.Slots[0] != "a" || wide.Slots[1] != "" || wide.Slots[2] != "" {
		t.Fatalf("focus with a short order: %v", wide.Slots)
	}

	one := New(One, 0)
	one.Fill("z")
	if err := one.Reset([]string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if len(one.Slots) != 1 || one.Slots[0] != "a" || one.Focus != 0 {
		t.Fatalf("one: %v focus %d", one.Slots, one.Focus)
	}
	if err := one.Reset(nil); err != nil {
		t.Fatal(err)
	}
	if one.Slots[0] != "" {
		t.Fatalf("one with an empty order: %v", one.Slots)
	}
}

func TestSlotOfAndShown(t *testing.T) {
	ts := New(Focus, 3)
	ts.Fill("p1")
	ts.Fill("p2")
	if i, ok := ts.SlotOf("p2"); !ok || i != 1 {
		t.Fatalf("SlotOf p2 = %d, %v", i, ok)
	}
	if _, ok := ts.SlotOf("p9"); ok {
		t.Fatal("SlotOf found a pane that has no slot")
	}
	if _, ok := ts.SlotOf(""); ok {
		t.Fatal("SlotOf matched an empty slot to an empty name")
	}
	if got := ts.Shown(2); len(got) != 2 || got[0] != "p1" || got[1] != "p2" {
		t.Fatalf("Shown(2) = %v", got)
	}
	if got := ts.Shown(5); len(got) != 3 {
		t.Fatalf("Shown(5) = %v", got)
	}
	if got := ts.Shown(0); len(got) != 0 {
		t.Fatalf("Shown(0) = %v", got)
	}
	if got := ts.Shown(-1); len(got) != 0 {
		t.Fatalf("Shown(-1) = %v", got)
	}
	got := ts.Shown(1)
	got[0] = "changed"
	if ts.Slots[0] != "p1" {
		t.Fatal("Shown shares the slots slice")
	}
}

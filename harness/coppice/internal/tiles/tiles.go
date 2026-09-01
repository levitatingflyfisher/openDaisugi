// Package tiles is the slot model behind the floor. A tile set is a list
// of slots, each holding one pane id or nothing, plus a focus index. The
// floor shows the first slots the width allows. A slot never moves on its
// own: only Swap, Rotate, and Reset rearrange, and a lock refuses all
// three. The model never closes a pane and never talks to a server. The
// JavaScript module tiles.js keeps the same rules for the phone page.
package tiles

import (
	"errors"
	"fmt"
)

// Layout names how the floor gives panes their slots.
type Layout string

const (
	// All gives every pane its own slot. Fill appends.
	All Layout = "all"
	// Focus keeps a fixed number of slots. Fill takes the first empty
	// slot, else the focused one.
	Focus Layout = "focus"
	// One keeps a single slot. Fill replaces it.
	One Layout = "one"
)

// ErrLocked is the refusal every rearranging call returns while Locked.
var ErrLocked = errors.New("Layout is locked. Unlock first.")

// Tiles is one tile set. Slots holds a pane id per slot, or "" for an
// empty slot. Focus is the index of the focused slot.
type Tiles struct {
	Slots  []string
	Focus  int
	Layout Layout
	Locked bool
}

// New makes a tile set for layout. Under Focus it makes n empty slots and
// clamps n below 1 to 1. Under One it makes one slot. Under All it makes
// none. A layout that is not one of the three reads as Focus.
func New(layout Layout, n int) *Tiles {
	switch layout {
	case All:
		return &Tiles{Slots: []string{}, Layout: All}
	case One:
		return &Tiles{Slots: make([]string, 1), Layout: One}
	}
	if n < 1 {
		n = 1
	}
	return &Tiles{Slots: make([]string, n), Layout: Focus}
}

// Fill puts pane in a slot and focuses that slot. A pane that already has
// a slot keeps it and only gains focus. Otherwise All appends a slot, One
// replaces slot 0, and Focus takes the first empty slot, else the focused
// slot. An empty name changes nothing.
func (t *Tiles) Fill(pane string) {
	if pane == "" {
		return
	}
	if i, ok := t.SlotOf(pane); ok {
		t.Focus = i
		return
	}
	switch t.Layout {
	case All:
		t.Slots = append(t.Slots, pane)
		t.Focus = len(t.Slots) - 1
		return
	case One:
		if len(t.Slots) == 0 {
			t.Slots = make([]string, 1)
		}
		t.Slots[0] = pane
		t.Focus = 0
		return
	}
	for i, s := range t.Slots {
		if s == "" {
			t.Slots[i] = pane
			t.Focus = i
			return
		}
	}
	if len(t.Slots) == 0 {
		t.Slots = make([]string, 1)
	}
	t.Focus = t.clamp(t.Focus)
	t.Slots[t.Focus] = pane
}

// Open gives pane a slot of its own and returns the slot index. A pane
// that already has a slot gains focus and returns its index. Under One it
// behaves as Fill and returns 0. Otherwise a new slot is appended and
// focused. An empty name returns -1.
func (t *Tiles) Open(pane string) int {
	if pane == "" {
		return -1
	}
	if i, ok := t.SlotOf(pane); ok {
		t.Focus = i
		return i
	}
	if t.Layout == One {
		t.Fill(pane)
		return 0
	}
	t.Slots = append(t.Slots, pane)
	t.Focus = len(t.Slots) - 1
	return t.Focus
}

// CloseSlot empties slot i under Focus and One, and removes it under All.
// Focus stays inside the slots. An index outside the slots changes nothing.
func (t *Tiles) CloseSlot(i int) {
	if i < 0 || i >= len(t.Slots) {
		return
	}
	if t.Layout == All {
		t.Slots = append(t.Slots[:i], t.Slots[i+1:]...)
	} else {
		t.Slots[i] = ""
	}
	t.Focus = t.clamp(t.Focus)
}

// Swap exchanges slots i and j. Focus follows its pane, so Focused
// returns the same pane after the call. It refuses while Locked and
// refuses an index outside the slots.
func (t *Tiles) Swap(i, j int) error {
	if t.Locked {
		return ErrLocked
	}
	for _, k := range []int{i, j} {
		if k < 0 || k >= len(t.Slots) {
			return fmt.Errorf("Slot %d is outside the slots 0 to %d.", k, len(t.Slots)-1)
		}
	}
	t.Slots[i], t.Slots[j] = t.Slots[j], t.Slots[i]
	switch t.Focus {
	case i:
		t.Focus = j
	case j:
		t.Focus = i
	}
	return nil
}

// Rotate moves every slot one to the right. The last slot wraps to the
// front. Focus follows its pane. It refuses while Locked.
func (t *Tiles) Rotate() error {
	if t.Locked {
		return ErrLocked
	}
	n := len(t.Slots)
	if n < 2 {
		return nil
	}
	last := t.Slots[n-1]
	copy(t.Slots[1:], t.Slots[:n-1])
	t.Slots[0] = last
	t.Focus = (t.Focus + 1) % n
	return nil
}

// Reset fills the slots from order and moves focus to 0. Under All the
// slots become a copy of order. Under Focus the first slots take the
// first entries of order and the rest stay empty. Under One slot 0 takes
// the first entry. It refuses while Locked.
func (t *Tiles) Reset(order []string) error {
	if t.Locked {
		return ErrLocked
	}
	switch t.Layout {
	case All:
		t.Slots = append([]string{}, order...)
	case One:
		t.Slots = make([]string, 1)
		if len(order) > 0 {
			t.Slots[0] = order[0]
		}
	default:
		for i := range t.Slots {
			if i < len(order) {
				t.Slots[i] = order[i]
			} else {
				t.Slots[i] = ""
			}
		}
	}
	t.Focus = 0
	return nil
}

// Focused returns the pane in the focused slot, or "" when that slot is
// empty or there are no slots.
func (t *Tiles) Focused() string {
	if t.Focus < 0 || t.Focus >= len(t.Slots) {
		return ""
	}
	return t.Slots[t.Focus]
}

// SlotOf returns the slot index that holds pane. An empty name never
// matches an empty slot.
func (t *Tiles) SlotOf(pane string) (int, bool) {
	if pane == "" {
		return 0, false
	}
	for i, s := range t.Slots {
		if s == pane {
			return i, true
		}
	}
	return 0, false
}

// Shown returns a copy of the first n slots, fewer when there are fewer.
func (t *Tiles) Shown(n int) []string {
	if n < 0 {
		n = 0
	}
	if n > len(t.Slots) {
		n = len(t.Slots)
	}
	return append([]string{}, t.Slots[:n]...)
}

// clamp returns i moved inside the slots. With no slots it returns 0.
func (t *Tiles) clamp(i int) int {
	if i >= len(t.Slots) {
		i = len(t.Slots) - 1
	}
	if i < 0 {
		i = 0
	}
	return i
}

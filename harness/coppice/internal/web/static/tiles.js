// The slot model behind the floor page. A tile set is a plain object:
// slots, a list of pane ids with '' for an empty slot; focus, the index of
// the focused slot; layout, one of all, focus, one; locked. The floor
// shows the first slots the width allows. A slot never moves on its own:
// only swap, rotate, and reset rearrange, and a lock refuses all three.
// Nothing here closes a pane or talks to a server. The Go package
// internal/tiles keeps the same rules for the terminal floor.

export const LAYOUTS = ['all', 'focus', 'one'];
export const LOCKED = 'Layout is locked. Unlock first.';

const refused = (reason) => ({ ok: false, reason });
const OK = Object.freeze({ ok: true });

// newTiles makes a tile set for layout. Under focus it makes n empty
// slots and clamps n below 1 to 1. Under one it makes one slot. Under all
// it makes none. A layout that is not one of the three reads as focus.
export function newTiles(layout, n) {
  if (layout === 'all') return { slots: [], focus: 0, layout: 'all', locked: false };
  if (layout === 'one') return { slots: [''], focus: 0, layout: 'one', locked: false };
  const count = Number.isInteger(n) && n >= 1 ? n : 1;
  return { slots: Array.from({ length: count }, () => ''), focus: 0, layout: 'focus', locked: false };
}

function clamp(t, i) {
  if (i >= t.slots.length) i = t.slots.length - 1;
  if (i < 0) i = 0;
  return i;
}

// fill puts pane in a slot and focuses that slot. A pane that already has
// a slot keeps it and only gains focus. Otherwise all appends a slot, one
// replaces slot 0, and focus takes the first empty slot, else the focused
// slot. An empty name changes nothing.
export function fill(t, pane) {
  if (!pane) return;
  const have = slotOf(t, pane);
  if (have >= 0) { t.focus = have; return; }
  if (t.layout === 'all') {
    t.slots.push(pane);
    t.focus = t.slots.length - 1;
    return;
  }
  if (t.layout === 'one') {
    t.slots = [pane];
    t.focus = 0;
    return;
  }
  const empty = t.slots.indexOf('');
  if (empty >= 0) {
    t.slots[empty] = pane;
    t.focus = empty;
    return;
  }
  if (t.slots.length === 0) t.slots = [''];
  t.focus = clamp(t, t.focus);
  t.slots[t.focus] = pane;
}

// open gives pane a slot of its own and returns the slot index. A pane
// that already has a slot gains focus and returns its index. Under one it
// behaves as fill and returns 0. Otherwise a new slot is appended and
// focused. An empty name returns -1.
export function open(t, pane) {
  if (!pane) return -1;
  const have = slotOf(t, pane);
  if (have >= 0) { t.focus = have; return have; }
  if (t.layout === 'one') { fill(t, pane); return 0; }
  t.slots.push(pane);
  t.focus = t.slots.length - 1;
  return t.focus;
}

// closeSlot empties slot i under focus and one, and removes it under all.
// Focus stays inside the slots. An index outside the slots changes nothing.
export function closeSlot(t, i) {
  if (!Number.isInteger(i) || i < 0 || i >= t.slots.length) return;
  if (t.layout === 'all') t.slots.splice(i, 1);
  else t.slots[i] = '';
  t.focus = clamp(t, t.focus);
}

// swap exchanges slots i and j. Focus follows its pane, so focused returns
// the same pane after the call. It refuses while locked and refuses an
// index outside the slots.
export function swap(t, i, j) {
  if (t.locked) return refused(LOCKED);
  for (const k of [i, j]) {
    if (!Number.isInteger(k) || k < 0 || k >= t.slots.length) {
      return refused('Slot ' + k + ' is outside the slots 0 to ' + (t.slots.length - 1) + '.');
    }
  }
  [t.slots[i], t.slots[j]] = [t.slots[j], t.slots[i]];
  if (t.focus === i) t.focus = j;
  else if (t.focus === j) t.focus = i;
  return OK;
}

// rotate moves every slot one to the right. The last slot wraps to the
// front. Focus follows its pane. It refuses while locked.
export function rotate(t) {
  if (t.locked) return refused(LOCKED);
  const n = t.slots.length;
  if (n < 2) return OK;
  t.slots.unshift(t.slots.pop());
  t.focus = (t.focus + 1) % n;
  return OK;
}

// reset fills the slots from order and moves focus to 0. Under all the
// slots become a copy of order. Under focus the first slots take the first
// entries of order and the rest stay empty. Under one slot 0 takes the
// first entry. It refuses while locked.
export function reset(t, order) {
  if (t.locked) return refused(LOCKED);
  const list = Array.isArray(order) ? order : [];
  if (t.layout === 'all') t.slots = list.slice();
  else if (t.layout === 'one') t.slots = [list[0] || ''];
  else t.slots = t.slots.map((_, i) => list[i] || '');
  t.focus = 0;
  return OK;
}

// focused returns the pane in the focused slot, or '' when that slot is
// empty or there are no slots.
export function focused(t) {
  return t.slots[t.focus] || '';
}

// slotOf returns the slot index that holds pane, or -1. An empty name
// never matches an empty slot.
export function slotOf(t, pane) {
  if (!pane) return -1;
  return t.slots.indexOf(pane);
}

// shown returns a copy of the first n slots, fewer when there are fewer.
export function shown(t, n) {
  const count = Number.isInteger(n) && n > 0 ? n : 0;
  return t.slots.slice(0, count);
}

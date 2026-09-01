import test from 'node:test';
import assert from 'node:assert/strict';
import {
  LOCKED, newTiles, fill, open, closeSlot, swap, rotate, reset, focused, slotOf, shown,
} from '../tiles.js';

// These cases mirror internal/tiles/tiles_test.go one for one. The Go
// model drives the terminal floor and this one drives the phone page, and
// the two must give the same slots for the same clicks.

const count = (slots, pane) => slots.filter((s) => s === pane).length;

test('fill replaces the focused slot only', () => {
  const t = newTiles('focus', 2);
  fill(t, 'p1'); t.focus = 1; fill(t, 'p2'); t.focus = 0;
  fill(t, 'p3');
  assert.deepEqual(t.slots, ['p3', 'p2']);
});

test('a pane has at most one tile', () => {
  const t = newTiles('focus', 2);
  fill(t, 'p1'); t.focus = 1;
  fill(t, 'p1');
  assert.equal(count(t.slots, 'p1'), 1);
});

test('swap keeps focus with the pane', () => {
  const t = newTiles('focus', 2);
  fill(t, 'p1'); t.focus = 1; fill(t, 'p2'); t.focus = 0;
  assert.deepEqual(swap(t, 0, 1), { ok: true });
  assert.equal(focused(t), 'p1');
  assert.equal(t.slots[1], 'p1');
});

test('locked refuses swap', () => {
  const t = newTiles('focus', 2);
  t.locked = true;
  const r = swap(t, 0, 1);
  assert.equal(r.ok, false);
  assert.equal(r.reason, 'Layout is locked. Unlock first.');
  assert.equal(r.reason, LOCKED);
});

test('an empty slot wins over focus', () => {
  const t = newTiles('focus', 2);
  fill(t, 'p1');
  fill(t, 'p2');
  assert.deepEqual(t.slots, ['p1', 'p2']);
});

test('newTiles makes the slots each layout starts with', () => {
  assert.deepEqual(newTiles('focus', 3), { slots: ['', '', ''], focus: 0, layout: 'focus', locked: false });
  assert.equal(newTiles('focus', 0).slots.length, 1);
  assert.deepEqual(newTiles('one', 3).slots, ['']);
  assert.deepEqual(newTiles('all', 3).slots, []);
  assert.equal(newTiles('nonsense', 2).layout, 'focus');
  assert.equal(focused(newTiles('focus', 2)), '');
});

test('fill under all appends and under one replaces', () => {
  const all = newTiles('all', 0);
  fill(all, 'p1'); fill(all, 'p2');
  assert.deepEqual(all.slots, ['p1', 'p2']);
  const one = newTiles('one', 0);
  fill(one, 'p1'); fill(one, 'p2');
  assert.deepEqual(one.slots, ['p2']);
});

test('fill focuses the slot it filled', () => {
  const t = newTiles('focus', 3);
  fill(t, 'p1'); fill(t, 'p2');
  assert.equal(t.focus, 1);
  fill(t, 'p1');
  assert.equal(t.focus, 0);
});

test('fill and open ignore an empty name', () => {
  const t = newTiles('focus', 2);
  fill(t, '');
  fill(t, undefined);
  assert.deepEqual(t.slots, ['', '']);
  assert.equal(open(t, ''), -1);
  assert.deepEqual(t.slots, ['', '']);
});

test('open appends and focuses a new slot', () => {
  const t = newTiles('focus', 2);
  fill(t, 'p1'); fill(t, 'p2');
  assert.equal(open(t, 'p3'), 2);
  assert.equal(t.focus, 2);
  assert.deepEqual(t.slots, ['p1', 'p2', 'p3']);
});

test('open on a pane already in a slot returns that slot', () => {
  const t = newTiles('focus', 2);
  fill(t, 'p1'); fill(t, 'p2');
  assert.equal(open(t, 'p1'), 0);
  assert.equal(t.focus, 0);
  assert.equal(t.slots.length, 2);
});

test('open under one replaces slot zero', () => {
  const t = newTiles('one', 0);
  fill(t, 'p1');
  assert.equal(open(t, 'p2'), 0);
  assert.deepEqual(t.slots, ['p2']);
});

test('closeSlot under all removes the slot and clamps focus', () => {
  const t = newTiles('all', 0);
  fill(t, 'p1'); fill(t, 'p2'); fill(t, 'p3');
  t.focus = 2;
  closeSlot(t, 2);
  assert.deepEqual(t.slots, ['p1', 'p2']);
  assert.equal(t.focus, 1);
  closeSlot(t, 0);
  assert.deepEqual(t.slots, ['p2']);
  assert.equal(t.focus, 0);
  closeSlot(t, 0);
  assert.deepEqual(t.slots, []);
  assert.equal(t.focus, 0);
  assert.equal(focused(t), '');
});

test('closeSlot under focus empties the slot', () => {
  const t = newTiles('focus', 2);
  fill(t, 'p1'); fill(t, 'p2');
  closeSlot(t, 0);
  assert.deepEqual(t.slots, ['', 'p2']);
  closeSlot(t, 5);
  closeSlot(t, -1);
  assert.deepEqual(t.slots, ['', 'p2']);
});

test('swap refuses an index outside the slots', () => {
  const t = newTiles('focus', 2);
  const r = swap(t, 0, 2);
  assert.equal(r.ok, false);
  assert.equal(r.reason, 'Slot 2 is outside the slots 0 to 1.');
  assert.equal(swap(t, -1, 0).ok, false);
  assert.equal(swap(t, 0, 1.5).ok, false);
});

test('locked refuses rotate and reset and changes nothing', () => {
  const t = newTiles('focus', 2);
  fill(t, 'p1');
  t.locked = true;
  assert.deepEqual(rotate(t), { ok: false, reason: LOCKED });
  assert.deepEqual(reset(t, ['p2']), { ok: false, reason: LOCKED });
  assert.deepEqual(t.slots, ['p1', '']);
});

test('rotate moves every slot right and keeps focus with its pane', () => {
  const t = newTiles('focus', 3);
  fill(t, 'p1'); fill(t, 'p2'); fill(t, 'p3');
  t.focus = 1;
  assert.deepEqual(rotate(t), { ok: true });
  assert.deepEqual(t.slots, ['p3', 'p1', 'p2']);
  assert.equal(t.focus, 2);
  assert.equal(focused(t), 'p2');
  assert.deepEqual(rotate(newTiles('all', 0)), { ok: true });
});

test('reset under each layout', () => {
  const order = ['a', 'b', 'c'];
  const all = newTiles('all', 0);
  fill(all, 'z');
  assert.deepEqual(reset(all, order), { ok: true });
  assert.deepEqual(all.slots, ['a', 'b', 'c']);
  assert.equal(all.focus, 0);
  order[0] = 'changed';
  assert.equal(all.slots[0], 'a', 'reset under all shares the caller\'s array');

  const focus = newTiles('focus', 2);
  fill(focus, 'z'); focus.focus = 1;
  reset(focus, ['a', 'b', 'c']);
  assert.deepEqual(focus.slots, ['a', 'b']);
  assert.equal(focus.focus, 0);
  const wide = newTiles('focus', 3);
  fill(wide, 'z');
  reset(wide, ['a']);
  assert.deepEqual(wide.slots, ['a', '', '']);

  const one = newTiles('one', 0);
  fill(one, 'z');
  reset(one, ['a', 'b']);
  assert.deepEqual(one.slots, ['a']);
  assert.equal(one.focus, 0);
  reset(one, []);
  assert.deepEqual(one.slots, ['']);
});

test('slotOf and shown', () => {
  const t = newTiles('focus', 3);
  fill(t, 'p1'); fill(t, 'p2');
  assert.equal(slotOf(t, 'p2'), 1);
  assert.equal(slotOf(t, 'p9'), -1);
  assert.equal(slotOf(t, ''), -1);
  assert.deepEqual(shown(t, 2), ['p1', 'p2']);
  assert.deepEqual(shown(t, 5), ['p1', 'p2', '']);
  assert.deepEqual(shown(t, 0), []);
  assert.deepEqual(shown(t, -1), []);
  const got = shown(t, 1);
  got[0] = 'changed';
  assert.equal(t.slots[0], 'p1', 'shown shares the slots array');
});

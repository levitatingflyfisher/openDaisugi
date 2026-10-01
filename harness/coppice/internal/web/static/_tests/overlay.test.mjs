import test from 'node:test';
import assert from 'node:assert/strict';
import {
  overlayNext, escCloses, journalRows, readPinned, savePinned,
  JOURNAL, COLONY, JOURNAL_KEPT, JOURNAL_SOURCE, isPanel, panelEsc, PANELS,
} from '../overlay.js';

test('an overlay opens with its selection, closes, and the journal keeps its agent', () => {
  let s = overlayNext(undefined, { type: 'open', id: 'tree', sel: ['a', '', 'b'] });
  assert.deepEqual(s, { open: 'tree', sel: ['a', 'b'], pane: '', pinned: false });
  s = overlayNext(s, { type: 'open', id: JOURNAL, pane: 'w1:p1' });
  assert.equal(s.open, JOURNAL);
  assert.equal(s.pane, 'w1:p1');
  s = overlayNext(s, { type: 'close' });
  assert.deepEqual(s, { open: '', sel: [], pane: '', pinned: false });
  assert.deepEqual(overlayNext(s, { type: 'open', id: '' }), s, 'an open with no id changed the state');
  assert.deepEqual(overlayNext(s, { type: 'bogus' }), s);
});

test('pinning the colony closes its overlay, and other overlays stay open', () => {
  let s = overlayNext(undefined, { type: 'open', id: COLONY });
  s = overlayNext(s, { type: 'pin' });
  assert.deepEqual([s.open, s.pinned], ['', true]);
  s = overlayNext(s, { type: 'open', id: 'kanban' });
  s = overlayNext(s, { type: 'pin' });
  assert.deepEqual([s.open, s.pinned], ['kanban', true]);
  s = overlayNext(s, { type: 'unpin' });
  assert.equal(s.pinned, false);
});

test('Esc closes an overlay only while the keys are not in a window', () => {
  assert.equal(escCloses({ open: 'tree' }, 'rail'), true);
  assert.equal(escCloses({ open: 'tree' }, 'window'), false);
  assert.equal(escCloses({ open: '' }, 'rail'), false);
  assert.equal(escCloses(null, 'rail'), false);
});

test('the journal rows are the server entries, newest first, for one agent or all', () => {
  const entries = [
    { at: 10, pane: 'a', decision: 'allow', tool: 'Read', clause: '' },
    { at: 12, pane: 'b', decision: 'deny', tool: 'Bash', clause: 'net', mode: 'enforcing' },
    { at: 13, pane: 'a', decision: 'ask', tool: 'Bash', clause: '' },
    { at: 'x', pane: 'a', decision: 'allow' },
    { pane: 'a', decision: 'allow' },
    null,
  ];
  assert.deepEqual(journalRows(entries).map((e) => [e.pane, e.decision, e.ts]), [['a', 'ask', 13], ['b', 'deny', 12], ['a', 'allow', 10]]);
  assert.deepEqual(journalRows(entries, 'b').map((e) => [e.clause, e.mode]), [['net', 'enforcing']]);
  assert.deepEqual(journalRows(null), []);
  const odd = journalRows([
    { at: 1, pane: 'a', decision: 'ask' },
    { at: 2, pane: 'a', decision: 'x" onclick="y' },
    { at: 3, pane: 'a', decision: 'deny gate-allow' },
  ]);
  assert.deepEqual(odd.map((e) => e.kind), ['other', 'other', 'ask']);
  assert.equal(odd[0].decision, 'deny gate-allow');
});

test('the journal shows at most what the server keeps, and says what it lists', () => {
  const many = [];
  for (let i = 0; i < JOURNAL_KEPT + 7; i += 1) many.push({ at: i, pane: 'a', decision: 'allow' });
  const rows = journalRows(many);
  assert.equal(rows.length, JOURNAL_KEPT);
  assert.equal(rows[0].ts, JOURNAL_KEPT + 6);
  assert.match(JOURNAL_SOURCE, /verdicts the gate and each agent's loop report/);
  assert.match(JOURNAL_SOURCE, /100 per agent, 500 in all, oldest first/);
  assert.match(JOURNAL_SOURCE, /restart/);
});

test('the colony pin is kept in storage, and broken storage reads as not pinned', () => {
  const store = new Map();
  const storage = { getItem: (k) => store.get(k) ?? null, setItem: (k, v) => store.set(k, v) };
  assert.equal(readPinned(storage), false);
  savePinned(storage, true);
  assert.equal(readPinned(storage), true);
  const broken = { getItem: () => { throw new Error('no'); }, setItem: () => { throw new Error('no'); } };
  assert.equal(readPinned(broken), false);
  savePinned(broken, true);
});

test('New and Settings are panels over the floor, and Esc closes them unless a window has the keys', () => {
  assert.deepEqual([...PANELS], ['new', 'settings']);
  assert.equal(isPanel('new'), true);
  assert.equal(isPanel('settings'), true);
  assert.equal(isPanel('roster'), false);
  assert.equal(isPanel('pane'), false);
  assert.equal(panelEsc('new', 'rail'), true);
  assert.equal(panelEsc('settings', 'rail'), true);
  assert.equal(panelEsc('new', 'window'), false);
  assert.equal(panelEsc('roster', 'rail'), false);
});

import test from 'node:test';
import assert from 'node:assert/strict';
import {
  overlayNext, escCloses, verdictOfDetail, journalFromEvents, seeVerdicts, journal, readPinned, savePinned,
  JOURNAL, COLONY, JOURNAL_KEPT,
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

test('a gate report detail reads as its verdict', () => {
  assert.deepEqual(verdictOfDetail('verdict=allow'), { decision: 'allow', clause: '' });
  assert.deepEqual(verdictOfDetail('verdict=deny clause=shell: curl | sh'), { decision: 'deny', clause: 'shell: curl | sh' });
  assert.deepEqual(verdictOfDetail('verdict=deny'), { decision: 'deny', clause: '' });
  assert.equal(verdictOfDetail('output in the last 5 s'), null);
  assert.equal(verdictOfDetail('verdict=allowed'), null);
  assert.equal(verdictOfDetail(undefined), null);
});

test('the journal takes gate verdicts and asks from state events, newest first', () => {
  const events = [
    { event: 'state', pane: 'a', source: 'gate', state: 'working', detail: 'verdict=allow', ts: 10 },
    { event: 'state', pane: 'a', source: 'process', state: 'working', detail: 'verdict=allow', ts: 11 },
    { event: 'state', pane: 'b', source: 'gate', state: 'working', detail: 'verdict=deny clause=net', ts: 12 },
    { event: 'state', pane: 'a', source: 'gate', state: 'blocked', detail: 'awaiting operator', ask: { tool: 'Bash', summary: 'git push' }, ts: 13 },
    { event: 'note', pane: 'a', text: 'verdict=allow', ts: 14 },
    { event: 'state', pane: 'a', source: 'gate', state: 'working', detail: 'no verdict here', ts: 15 },
  ];
  assert.deepEqual(journalFromEvents(events).map((e) => [e.pane, e.decision, e.ts]), [['a', 'ask', 13], ['b', 'deny', 12], ['a', 'allow', 10]]);
  const ask = journalFromEvents(events, 'a')[0];
  assert.deepEqual([ask.tool, ask.what], ['Bash', 'git push']);
  assert.deepEqual(journalFromEvents(events, 'b').map((e) => e.clause), ['net']);
  assert.deepEqual(journalFromEvents(null), []);
});

test('the page keeps each new last verdict it sees once', () => {
  const seen = seeVerdicts(new Map(), [{ id: 'a', gate: { decision: 'deny', tool: 'Bash', clause: 'net', at: 20 } }, { id: 'b' }]);
  seeVerdicts(seen, [{ id: 'a', gate: { decision: 'deny', tool: 'Bash', clause: 'net', at: 20 } }]);
  seeVerdicts(seen, [{ id: 'a', gate: { decision: 'allow', tool: 'Read', clause: '', at: 25 } }, { id: 'c', gate: { decision: 'what', at: 1 } }]);
  assert.deepEqual([...seen.values()].map((e) => [e.pane, e.decision, e.tool, e.ts]), [['a', 'deny', 'Bash', 20], ['a', 'allow', 'Read', 25]]);
  const many = new Map();
  for (let i = 0; i < JOURNAL_KEPT + 5; i += 1) seeVerdicts(many, [{ id: 'a', gate: { decision: 'allow', at: i } }]);
  assert.equal(many.size, JOURNAL_KEPT);
});

test('the journal merges both sources and a seen verdict wins over the same event', () => {
  const fromEvents = journalFromEvents([
    { event: 'state', pane: 'a', source: 'gate', state: 'working', detail: 'verdict=deny clause=net', ts: 21 },
    { event: 'state', pane: 'a', source: 'gate', state: 'working', detail: 'verdict=allow', ts: 5 },
  ]);
  const seen = seeVerdicts(new Map(), [{ id: 'a', gate: { decision: 'deny', tool: 'Bash', clause: 'net', at: 20 } }, { id: 'b', gate: { decision: 'allow', tool: 'Read', at: 30 } }]);
  assert.deepEqual(journal(fromEvents, seen).map((e) => [e.pane, e.decision, e.tool, e.ts]), [['b', 'allow', 'Read', 30], ['a', 'deny', 'Bash', 20], ['a', 'allow', '', 5]]);
  assert.deepEqual(journal(fromEvents, seen, 'b').map((e) => e.pane), ['b']);
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

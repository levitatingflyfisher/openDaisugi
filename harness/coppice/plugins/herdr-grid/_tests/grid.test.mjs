import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { slots, watched, shape, rosterRows } from '../grid.js';
import { newTiles, fill } from '../../../internal/web/static/tiles.js';
import { sortPanes } from '../../../internal/web/static/roster.js';

const PANES = [
  { id: 'a', label: 'docs', state: 'idle' },
  { id: 'b', label: 'auth', state: 'blocked' },
  { id: 'c', state: 'working' },
  { id: 'd', state: 'strange' },
];

test('the grid gives every pane a slot', () => {
  const s = slots(PANES);
  assert.equal(s.length, PANES.length);
  assert.deepEqual([...s].sort(), PANES.map((p) => p.id).sort());
  const many = Array.from({ length: 14 }, (_, i) => ({ id: 'p' + i, state: 'working' }));
  assert.equal(slots(many).length, 14);
});

test('the slots are the floor tiles under layout all, in roster order', () => {
  const t = newTiles('all', 0);
  for (const p of sortPanes(PANES)) fill(t, p.id);
  assert.deepEqual(slots(PANES), t.slots);
  assert.equal(slots(PANES)[0], 'b');
});

test('the first nine slots are watched, and no more', () => {
  const many = Array.from({ length: 14 }, (_, i) => ({ id: 'p' + i, state: 'working' }));
  assert.deepEqual(watched(many), slots(many).slice(0, 9));
  assert.deepEqual(watched(PANES), slots(PANES));
});

// The failure this names: a closed pane among the live nine is refused on
// every refresh and draws a blank screen as if it were live.
test('a closed pane keeps its slot and never joins the live nine', () => {
  const many = Array.from({ length: 11 }, (_, i) => ({ id: 'p' + i, state: 'working', closed: i === 0 }));
  const live = watched(many);
  assert.equal(live.length, 9);
  assert.ok(!live.includes('p0'));
  assert.deepEqual(live, slots(many).filter((id) => id !== 'p0').slice(0, 9));
  assert.ok(slots(many).includes('p0'));
});

test('the grid shape holds every slot, as square as it can', () => {
  assert.deepEqual(shape(0), { cols: 0, rows: 0 });
  assert.deepEqual(shape(1), { cols: 1, rows: 1 });
  assert.deepEqual(shape(4), { cols: 2, rows: 2 });
  assert.deepEqual(shape(5), { cols: 3, rows: 2 });
  assert.deepEqual(shape(9), { cols: 3, rows: 3 });
  assert.deepEqual(shape(10), { cols: 4, rows: 3 });
});

test('each roster row carries the state word', () => {
  assert.deepEqual(rosterRows(PANES), [
    { id: 'b', label: 'auth', word: 'blocked' },
    { id: 'c', label: 'c', word: 'working' },
    { id: 'a', label: 'docs', word: 'idle' },
    { id: 'd', label: 'd', word: 'unknown' },
  ]);
});

test('grid.js stays under 200 lines and asks the server for nothing', () => {
  const src = readFileSync(new URL('../grid.js', import.meta.url), 'utf8');
  assert.ok(src.split('\n').length < 200);
  for (const f of ['../grid.js', '../grid-view.js']) {
    const s = readFileSync(new URL(f, import.meta.url), 'utf8');
    for (const word of ['localStorage', 'fetch(', 'WebSocket', 'coppice.token', 'send_text']) {
      assert.ok(!s.includes(word), f + ' uses ' + word);
    }
  }
});

test('the page gives every pane a slot, watches the first nine once, and draws their frames', async () => {
  const { reset, element } = await import('../../../internal/web/static/_tests/browser-stub.mjs');
  const { mountGrid } = await import('../grid-view.js');
  reset();
  const many = Array.from({ length: 11 }, (_, i) => ({ id: 'p' + i, state: i === 4 ? 'blocked' : 'working' }));
  const sent = [];
  let onData = null;
  let onFrame = null;
  let onWatch = null;
  const view = {
    panes: () => many, sel: () => [],
    onData: (fn) => { onData = fn; }, onFrame: (fn) => { onFrame = fn; }, onWatch: (fn) => { onWatch = fn; },
    watch: (ids) => sent.push(['watch', ids]), setSel: (ids) => sent.push(['sel', ids]),
    openPane: (id) => sent.push(['open', id]),
  };
  const tiles = element('grid-tiles');
  mountGrid(element('grid-status'), element('grid-roster'), tiles, view);
  onData();
  onData();
  assert.equal(tiles.children.length, 11);
  assert.deepEqual(sent, [['watch', slots(many).slice(0, 9)]]);
  assert.equal(tiles.children[0].dataset.pane, 'p4');
  assert.match(tiles.children[10].textContent, /Nine panes at most/);
  assert.match(tiles.children[0].textContent, /Waiting for the picture/);
  onWatch({ live: ['p4'], refused: ['p0'] });
  assert.doesNotMatch(tiles.children[0].textContent, /Waiting/);
  const refusedSlot = tiles.children.find((c) => c.dataset.pane === 'p0');
  assert.match(refusedSlot.textContent, /could not show this pane live/);
  assert.doesNotThrow(() => onFrame({ pane: 'p4', cols: 2, rows: 1, lines: ['ok'] }));
  // After a drop the picture is no longer live, and the slot says so.
  onWatch({ live: [], refused: [] });
  assert.match(tiles.children[0].textContent, /Waiting for the picture/);
  tiles.children[0].dispatchEvent({ type: 'click' });
  element('grid-roster').children[0].dispatchEvent({ type: 'click' });
  assert.deepEqual(sent.slice(1), [['open', 'p4'], ['sel', ['p4']]]);
});

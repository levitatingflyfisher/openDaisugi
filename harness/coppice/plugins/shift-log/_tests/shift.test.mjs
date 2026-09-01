import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { cells, LEVEL, levelOf, UNKNOWN, coverText } from '../shift.js';

const NOW = 100000;
const at = (min) => NOW - min * 60;

test('two working events eleven minutes apart fill two cells with a gap between', () => {
  const rows = cells([
    { event: 'state', pane: 'a', state: 'working', ts: at(20) },
    { event: 'state', pane: 'a', state: 'working', ts: at(9) },
  ], NOW);
  assert.equal(rows.length, 1);
  const row = rows[0].cells;
  assert.equal(row.length, 16);
  assert.equal(row[12], LEVEL.working);
  assert.equal(row[13], 0);
  assert.equal(row[14], LEVEL.working);
  assert.equal(row.filter((c) => c > 0).length, 2);
});

test('a blocked event marks its cell ask, and a deny in the detail marks deny', () => {
  const rows = cells([
    { event: 'state', pane: 'a', state: 'blocked', ts: at(3) },
    { event: 'state', pane: 'b', state: 'working', ts: at(3) },
    { event: 'state', pane: 'b', state: 'blocked', detail: 'verdict=deny clause=shell.deny[2]', ts: at(4) },
    { event: 'state', pane: 'c', state: 'blocked', detail: 'Allow or deny?', ts: at(4) },
  ], NOW);
  const by = new Map(rows.map((r) => [r.pane, r.cells]));
  assert.equal(by.get('a')[15], LEVEL.ask);
  assert.equal(by.get('b')[15], LEVEL.deny);
  // An ask whose words hold deny is still an ask. Only the gate's verdict is a deny.
  assert.equal(by.get('c')[15], LEVEL.ask);
  assert.deepEqual(LEVEL, { working: 1, ask: 2, deny: 3 });
});

test('the strongest mark in a cell wins, and only state events count', () => {
  assert.equal(levelOf({ event: 'state', state: 'idle' }), 0);
  assert.equal(levelOf({ event: 'note', state: 'blocked' }), 0);
  const rows = cells([
    { event: 'state', pane: 'a', state: 'blocked', ts: at(1) },
    { event: 'state', pane: 'a', state: 'working', ts: at(1.5) },
    { event: 'note', pane: 'z', text: 'x', ts: at(1) },
  ], NOW);
  assert.deepEqual(rows.map((r) => r.pane), ['a']);
  assert.equal(rows[0].cells[15], LEVEL.ask);
});

test('events outside the window, or with no time, are left out', () => {
  const rows = cells([
    { event: 'state', pane: 'old', state: 'working', ts: at(81) },
    { event: 'state', pane: 'late', state: 'working', ts: NOW + 60 },
    { event: 'state', pane: 'none', state: 'working' },
  ], NOW);
  assert.deepEqual(rows, []);
});

test('the window and the cell size can change, and a pane that only idles still has a row', () => {
  const rows = cells([
    { event: 'state', pane: 'a', state: 'working', ts: at(10) },
    { event: 'state', pane: 'b', state: 'idle', ts: at(1) },
  ], NOW, 60, 10);
  assert.equal(rows[0].cells.length, 6);
  assert.equal(rows[0].cells[5], LEVEL.working);
  assert.deepEqual(rows[1].cells, [0, 0, 0, 0, 0, 0]);
});

test('shift.js stays under 200 lines and asks the server for nothing', () => {
  const src = readFileSync(new URL('../shift.js', import.meta.url), 'utf8');
  assert.ok(src.split('\n').length < 200);
  for (const f of ['../shift.js', '../shift-view.js']) {
    const s = readFileSync(new URL(f, import.meta.url), 'utf8');
    for (const word of ['localStorage', 'fetch(', 'WebSocket', 'coppice.token']) {
      assert.ok(!s.includes(word), f + ' uses ' + word);
    }
  }
});

test('the page draws one row per pane with its marks, and a click selects the pane', async () => {
  const { reset, element } = await import('../../../internal/web/static/_tests/browser-stub.mjs');
  const { mountShift, MARKS } = await import('../shift-view.js');
  reset();
  const sent = [];
  let onData = null;
  const view = {
    events: () => [
      { event: 'state', pane: 'w1:p1', state: 'blocked', ts: at(2) },
      { event: 'state', pane: 'w1:p1', state: 'working', ts: at(30) },
    ],
    panes: () => [{ id: 'w1:p1', label: 'auth' }],
    from: () => at(90),
    onData: (fn) => { onData = fn; },
    setSel: (ids) => sent.push(['sel', ids]), openPane: (id) => sent.push(['open', id]),
  };
  const grid = element('shift-grid');
  const status = element('shift-status');
  mountShift(status, grid, element('shift-legend'), view, () => NOW);
  onData();
  assert.equal(grid.children.length, 2);
  const row = grid.children[1];
  assert.equal(row.children[0].textContent, 'auth');
  const marks = row.children.slice(1).map((c) => c.style.background || '');
  assert.equal(marks.length, 16);
  assert.equal(marks[15], MARKS[LEVEL.ask]);
  assert.equal(marks[10], MARKS[LEVEL.working]);
  assert.equal(marks.filter(Boolean).length, 2);
  assert.equal(row.children.slice(1).filter((c) => /unknown/.test(c.className)).length, 0);
  row.children[0].dispatchEvent({ type: 'click' });
  assert.deepEqual(sent, [['sel', ['w1:p1']]]);
  assert.equal(status.textContent, '');
});

// The failure this names: a span the ring never saw drawn as a quiet one.
test('a cell before from is unknown unless an event marked it, and no from makes every empty cell unknown', () => {
  const ev = [{ event: 'state', pane: 'a', state: 'working', ts: at(70) }, { event: 'state', pane: 'a', state: 'working', ts: at(3) }];
  const part = cells(ev, NOW, 80, 5, at(30))[0].cells;
  assert.equal(part[2], LEVEL.working, 'a mark is a fact even before from');
  assert.equal(part[0], UNKNOWN);
  assert.equal(part[9], UNKNOWN, 'the cell from 35 to 30 minutes ago starts before from');
  assert.equal(part[10], 0, 'the cell from 30 to 25 minutes ago is held');
  assert.equal(part[15], LEVEL.working);
  const none = cells(ev, NOW, 80, 5, null)[0].cells;
  assert.equal(none[5], UNKNOWN);
  assert.equal(none[15], LEVEL.working);
  const full = cells(ev, NOW, 80, 5, at(90))[0].cells;
  assert.equal(full.filter((c) => c === UNKNOWN).length, 0);
});

test('the status says no state changes only for a span the ring holds', () => {
  assert.equal(coverText(null, NOW, 80, 0), 'The floor has no history from the server. Cells with no mark are unknown.');
  assert.equal(coverText(at(90), NOW, 80, 0), 'No state changes in the last 80 minutes.');
  assert.equal(coverText(at(90), NOW, 80, 2), '');
  assert.equal(coverText(at(30), NOW, 80, 0), 'No state changes in the last 30 minutes. The server holds nothing before that, so the cells before it are unknown.');
  assert.equal(coverText(at(30), NOW, 80, 3), 'The server holds nothing from before 30 minutes ago, so the cells before it are unknown.');
});

test('with no history from the server the page draws unknown cells and says so', async () => {
  const { reset, element } = await import('../../../internal/web/static/_tests/browser-stub.mjs');
  const { mountShift } = await import('../shift-view.js');
  reset();
  let onData = null;
  const view = {
    events: () => [{ event: 'state', pane: 'w1:p1', state: 'working', ts: at(2) }],
    panes: () => [{ id: 'w1:p1', label: 'auth' }],
    from: () => null,
    onData: (fn) => { onData = fn; }, setSel: () => {}, openPane: () => {},
  };
  const grid = element('shift-grid');
  const status = element('shift-status');
  mountShift(status, grid, element('shift-legend'), view, () => NOW);
  onData();
  const cellsDrawn = grid.children[1].children.slice(1);
  assert.equal(cellsDrawn.filter((c) => /unknown/.test(c.className)).length, 15);
  assert.equal(status.textContent, 'The floor has no history from the server. Cells with no mark are unknown.');
  assert.match(element('shift-legend').textContent, /working.*ask.*deny.*unknown/);
});

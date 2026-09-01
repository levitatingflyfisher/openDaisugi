import test from 'node:test';
import assert from 'node:assert/strict';
import { reset, element } from './browser-stub.mjs';
import { tileCount, cellPxFor, prefsFrom, mountFloor } from '../floor.js';
import { render, paneRows } from '../roster.js';
import { gridToText } from '../grid.js';

// The floor page: the roster rail plus as many live tiles as the width
// allows. These scenarios drive mountFloor against the fake DOM with a
// recording rpc and api, the same way screens-connect.test.mjs drives the
// roster and the pane screen through a real boot().

// stub starts one scenario. The rail is the existing #roster list, nested
// inside #screen-roster the way index.html nests it, and painted through
// the roster's own render, the way mountRoster paints it in the app.
function stub({ panes, width }) {
  reset();
  global.window.innerWidth = width;
  const root = element('screen-roster');
  root.append(element('roster'), element('roster-empty'));
  render(paneRows(panes, 1));
  const calls = [];
  const state = {
    panes,
    width,
    rpc: async (cmd, fields) => { calls.push({ cmd, ...fields }); return {}; },
    api: async (path, init) => { calls.push({ path, init }); return {}; },
    status: (msg) => { element('status').textContent = msg; },
  };
  const doc = { body: root, querySelector: (s) => root.querySelector(s) };
  return { doc, state, calls };
}

function p(id, state, ask) {
  return { id, label: id, state, harness: 'claude', cwd: '/w/' + id, ts: 1, ask };
}

function click(doc, sel) {
  const el = doc.querySelector(sel);
  assert.ok(el, 'nothing matches ' + sel);
  el.dispatchEvent({ type: 'click', button: 0, ctrlKey: false });
}

function ctrlClick(doc, sel) {
  const el = doc.querySelector(sel);
  assert.ok(el, 'nothing matches ' + sel);
  el.dispatchEvent({ type: 'click', button: 0, ctrlKey: true });
}

function middleClick(doc, sel) {
  const el = doc.querySelector(sel);
  assert.ok(el, 'nothing matches ' + sel);
  el.dispatchEvent({ type: 'auxclick', button: 1 });
}

function tilesOf(doc) {
  return doc.body.querySelectorAll('.tile').map((t) => t.dataset.tile || '');
}

const focusedTile = (doc) => doc.querySelector('.focused').dataset.tile;
const attaches = (calls) => calls.filter((c) => c.cmd === 'pane.attach');
const detaches = (calls) => calls.filter((c) => c.cmd === 'pane.detach');
const settle = () => new Promise((r) => setImmediate(r));

test('a row click fills the focused tile and keeps the other', () => {
  const wide = stub({ panes: [p('a', 'working'), p('b', 'working'), p('c', 'working')], width: 1500 });
  const floor = mountFloor(wide.doc.body, wide.state);
  assert.deepEqual(tilesOf(wide.doc), ['a', 'b', 'c']);
  assert.equal(floor.tiles.focus, 0);
  click(wide.doc, '[data-row="c"]');
  assert.deepEqual(tilesOf(wide.doc), ['a', 'b', 'c']);
  assert.equal(focusedTile(wide.doc), 'c');
  assert.equal(global.location.hash, '', 'a row click opened the pane screen');

  const two = stub({ panes: [p('a', 'working'), p('b', 'working'), p('c', 'working')], width: 1000 });
  mountFloor(two.doc.body, two.state);
  assert.deepEqual(tilesOf(two.doc), ['a', 'b']);
  click(two.doc, '[data-row="c"]');
  assert.deepEqual(tilesOf(two.doc), ['c', 'b']);
  assert.equal(focusedTile(two.doc), 'c');
});

test('middle click opens a new tile', () => {
  const { doc, state } = stub({ panes: [p('a', 'working'), p('b', 'working'), p('c', 'working')], width: 1000 });
  const floor = mountFloor(doc.body, state);
  assert.deepEqual(floor.tiles.slots, ['a', 'b']);
  middleClick(doc, '[data-row="c"]');
  assert.deepEqual(floor.tiles.slots, ['a', 'b', 'c']);
  assert.equal(tilesOf(doc).length, 2);
  middleClick(doc, '[data-row="c"]');
  assert.equal(floor.tiles.slots.length, 3, 'a second middle click gave c a second tile');
});

test('a ctrl click opens a new tile the same way', () => {
  const { doc, state } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 1500 });
  const floor = mountFloor(doc.body, state);
  assert.deepEqual(floor.tiles.slots, ['a', 'b', '']);
  ctrlClick(doc, '[data-row="b"]');
  assert.deepEqual(floor.tiles.slots, ['a', 'b', ''], 'a pane already shown gained a second tile');
  assert.equal(focusedTile(doc), 'b');
});

test('narrow widths get one tile, and a phone gets none', () => {
  assert.equal(tileCount(800), 1);
  assert.equal(tileCount(1200), 2);
  assert.equal(tileCount(1600), 3);
  assert.equal(tileCount(599), 0);
  assert.equal(tileCount(600), 1);
  assert.equal(tileCount(0), 0);
  assert.equal(tileCount(899), 1);
  assert.equal(tileCount(900), 2);
  assert.equal(tileCount(1399), 2);
  assert.equal(tileCount(1400), 3);
});

test('an ask shows as an amber bar on the tile, never a dialog', () => {
  const { doc, state } = stub({ panes: [p('a', 'blocked', { summary: 'git push --force', gate: { verdict: 'deny', rule: 3 } })], width: 1500 });
  mountFloor(doc.body, state);
  assert.ok(doc.querySelector('[data-tile="a"] .askbar').textContent.includes('rule 3'));
  assert.equal(doc.querySelector('dialog'), null);
  const bar = doc.querySelector('[data-tile="a"] .askbar');
  assert.ok(bar.textContent.includes('git push --force'));
  assert.ok(bar.textContent.includes('The gate says no, rule 3'));
  assert.equal(doc.querySelector('[data-tile="a"] .dot').className, 'dot dot-blocked');
});

test('an allow verdict and a plain ask each read as their own line', () => {
  const { doc, state } = stub({
    panes: [
      p('a', 'blocked', { id: 't1', summary: 'ls', gate: { verdict: 'allow' } }),
      p('b', 'blocked', { id: 't2', summary: 'rm -rf build/' }),
      p('c', 'working'),
    ],
    width: 1500,
  });
  mountFloor(doc.body, state);
  assert.ok(doc.querySelector('[data-tile="a"] .askbar').textContent.includes('The gate says yes'));
  const plain = doc.querySelector('[data-tile="b"] .askbar').textContent;
  assert.ok(plain.includes('rm -rf build/'));
  assert.ok(!plain.includes('The gate says'));
  assert.equal(doc.querySelector('[data-tile="c"] .askbar'), null);
});

test('the tile header names the pane, the harness, and the last path segment', () => {
  const { doc, state } = stub({ panes: [{ id: 'w1:p1', label: 'auth fix', state: 'working', harness: 'codex', cwd: '/home/op/repo', ts: 1 }], width: 800 });
  mountFloor(doc.body, state);
  const head = doc.querySelector('[data-tile="w1:p1"] .head').textContent;
  assert.ok(head.includes('auth fix'));
  assert.ok(head.includes('codex'));
  assert.ok(head.includes('repo'));
  assert.ok(!head.includes('/home/op'));
});

test('an empty slot says so', () => {
  const { doc, state } = stub({ panes: [p('a', 'working')], width: 1000 });
  mountFloor(doc.body, state);
  assert.deepEqual(tilesOf(doc), ['a', '']);
  const empty = doc.body.querySelectorAll('.tile')[1];
  assert.ok(empty.className.includes('empty'));
  assert.equal(empty.textContent, 'Empty. Click a row.');
});

// A tile attach carries no size. The same pane is open in the operator's
// terminal, and a size here would resize it. The tile draws whatever grid
// arrives, scaled to its own width.
test('a tile that gets a pane attaches with no size, and never twice', async () => {
  const { doc, state, calls } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 1000 });
  const floor = mountFloor(doc.body, state);
  await settle();
  assert.deepEqual(attaches(calls), [
    { cmd: 'pane.attach', pane: 'a' },
    { cmd: 'pane.attach', pane: 'b' },
  ]);
  floor.paint(state.panes);
  click(doc, '[data-row="b"]');
  await settle();
  assert.equal(attaches(calls).length, 2, 'a repaint or a click re-attached a pane already attached');
  assert.equal(detaches(calls).length, 0);
});

test('a tile that loses its pane detaches it', async () => {
  const { doc, state, calls } = stub({ panes: [p('a', 'working'), p('b', 'working'), p('c', 'working')], width: 1000 });
  mountFloor(doc.body, state);
  await settle();
  click(doc, '[data-row="c"]');
  await settle();
  assert.deepEqual(detaches(calls), [{ cmd: 'pane.detach', pane: 'a' }]);
  assert.deepEqual(attaches(calls).map((c) => c.pane), ['a', 'b', 'c']);
});

test('a pane gone from the list empties its tile and detaches', async () => {
  const { doc, state, calls } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 1000 });
  const floor = mountFloor(doc.body, state);
  await settle();
  floor.paint([p('b', 'working')]);
  await settle();
  assert.deepEqual(tilesOf(doc), ['', 'b']);
  assert.deepEqual(detaches(calls), [{ cmd: 'pane.detach', pane: 'a' }]);
  floor.paint([p('b', 'working'), p('d', 'idle')]);
  assert.deepEqual(tilesOf(doc), ['d', 'b'], 'a new pane did not take the empty slot');
});

test('a frame event for a tile reaches applyFrame and the tile mirror', async () => {
  const { doc, state } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 1000 });
  const floor = mountFloor(doc.body, state);
  await settle();
  floor.onEvent({
    event: 'frame', pane: 'b', seq: 1, cols: 3, rows: 1, cursor: [0, 0],
    rows_changed: { 0: [['h', '', '', 0], ['i', '', '', 0], [' ', '', '', 0]] },
  });
  assert.equal(doc.querySelector('[data-tile="b"] .mirror').textContent, 'hi');
  assert.equal(doc.querySelector('[data-tile="a"] .mirror').textContent, '');
  floor.onEvent({ event: 'frame', pane: 'zzz', cols: 1, rows: 1, rows_changed: { 0: [['x', '', '', 0]] } });
  assert.equal(doc.querySelector('[data-tile="b"] .mirror').textContent, 'hi');
  const grid = floor.gridOf('b');
  assert.equal(gridToText(grid), 'hi');
});

test('a state event refreshes that tile header and ask bar', () => {
  const { doc, state } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 1000 });
  const floor = mountFloor(doc.body, state);
  assert.equal(doc.querySelector('[data-tile="a"] .askbar'), null);
  floor.onEvent({
    event: 'state', pane: 'a', state: 'blocked', source: 'gate', harness: 'claude', ts: 2,
    ask: { id: 'toolu_9', tool: 'Bash', summary: 'rm -rf /' },
  });
  assert.equal(doc.querySelector('[data-tile="a"] .dot').className, 'dot dot-blocked');
  assert.ok(doc.querySelector('[data-tile="a"] .askbar').textContent.includes('rm -rf /'));
  floor.onEvent({ event: 'state', pane: 'a', state: 'idle', source: 'gate', harness: 'claude', ts: 3 });
  assert.equal(doc.querySelector('[data-tile="a"] .askbar'), null);
  assert.equal(doc.querySelector('[data-tile="a"] .dot').className, 'dot dot-idle');
});

test('detachAll sends one pane.detach per attached tile', async () => {
  const { doc, state, calls } = stub({ panes: [p('a', 'working'), p('b', 'working'), p('c', 'working')], width: 1000 });
  const floor = mountFloor(doc.body, state);
  await settle();
  floor.detachAll();
  assert.deepEqual(detaches(calls).map((c) => c.pane), ['a', 'b']);
  floor.detachAll();
  assert.equal(detaches(calls).length, 2, 'a second detachAll sent detaches again');
  floor.paint(state.panes);
  await settle();
  assert.deepEqual(attaches(calls).map((c) => c.pane), ['a', 'b', 'a', 'b']);
});

test('a hidden floor never attaches', async () => {
  const { doc, state, calls } = stub({ panes: [p('a', 'working')], width: 1000 });
  doc.body.hidden = true;
  const floor = mountFloor(doc.body, state);
  await settle();
  assert.equal(attaches(calls).length, 0);
  doc.body.hidden = false;
  floor.paint(state.panes);
  await settle();
  assert.deepEqual(attaches(calls).map((c) => c.pane), ['a']);
});

test('an attach the server refuses is not held, and the tile says why', async () => {
  const { doc, state, calls } = stub({ panes: [p('a', 'working')], width: 800 });
  state.rpc = async (cmd, fields) => {
    calls.push({ cmd, ...fields });
    if (cmd === 'pane.attach') throw new Error('Not connected. Check the token in Settings.');
    return {};
  };
  const floor = mountFloor(doc.body, state);
  await settle();
  assert.equal(doc.querySelector('[data-tile="a"] .note').textContent, 'Not connected. Check the token in Settings.');
  assert.equal(element('status').textContent, '');
  floor.detachAll();
  assert.equal(detaches(calls).length, 0, 'a failed attach was detached as if it were live');
  state.rpc = async (cmd, fields) => { calls.push({ cmd, ...fields }); return {}; };
  floor.paint(state.panes);
  await settle();
  assert.equal(attaches(calls).length, 2, 'a failed attach was not retried on the next paint');
  assert.equal(doc.querySelector('[data-tile="a"] .note').textContent, '');
});

test('Deny posts the exact body and Reply routes to the pane', async () => {
  const { doc, state, calls } = stub({ panes: [p('a', 'blocked', { id: 'toolu_01ABC', summary: 'rm -rf build/' })], width: 800 });
  mountFloor(doc.body, state);
  const buttons = doc.body.querySelectorAll('[data-tile="a"] button');
  assert.deepEqual(buttons.map((b) => b.textContent), ['Deny', 'Reply']);
  buttons[0].dispatchEvent({ type: 'click' });
  await settle();
  const posts = calls.filter((c) => c.path);
  assert.equal(posts.length, 1);
  assert.equal(posts[0].path, '/api/ask/answer');
  assert.equal(posts[0].init.method, 'POST');
  assert.deepEqual(JSON.parse(posts[0].init.body), { tool_use_id: 'toolu_01ABC', decision: 'deny', reason: 'denied from the floor' });
  assert.equal(element('status').textContent, 'Denied.');
  buttons[1].dispatchEvent({ type: 'click' });
  assert.equal(global.location.hash, '#/pane/a');
});

test('Deny with no ask id posts nothing', async () => {
  const { doc, state, calls } = stub({ panes: [p('a', 'blocked', { summary: 'rm -rf build/' })], width: 800 });
  mountFloor(doc.body, state);
  doc.body.querySelectorAll('[data-tile="a"] button')[0].dispatchEvent({ type: 'click' });
  await settle();
  assert.equal(calls.filter((c) => c.path).length, 0);
});

test('n denies and r replies on the focused tile, and never from a text field', async () => {
  const { doc, state, calls } = stub({
    panes: [p('a', 'blocked', { id: 'toolu_A', summary: 'a' }), p('b', 'blocked', { id: 'toolu_B', summary: 'b' })],
    width: 1000,
  });
  mountFloor(doc.body, state);
  click(doc, '[data-row="b"]');
  global.document.dispatchEvent({ type: 'keydown', key: 'n', target: { tagName: 'TEXTAREA' } });
  await settle();
  assert.equal(calls.filter((c) => c.path).length, 0, 'a keystroke inside a text field denied an ask');
  global.document.dispatchEvent({ type: 'keydown', key: 'n', target: { tagName: 'BODY' } });
  await settle();
  const posts = calls.filter((c) => c.path);
  assert.equal(posts.length, 1);
  assert.equal(JSON.parse(posts[0].init.body).tool_use_id, 'toolu_B');
  global.document.dispatchEvent({ type: 'keydown', key: 'r', target: { tagName: 'BODY' } });
  assert.equal(global.location.hash, '#/pane/b');
  global.location.hash = '';
  doc.body.hidden = true;
  global.document.dispatchEvent({ type: 'keydown', key: 'r', target: { tagName: 'BODY' } });
  assert.equal(global.location.hash, '', 'a keystroke on another screen acted on the floor');
});

test('layout and lock round-trip through localStorage', () => {
  const first = stub({ panes: [p('a', 'working'), p('b', 'working'), p('c', 'working')], width: 1500 });
  const floor = mountFloor(first.doc.body, first.state);
  assert.deepEqual(prefsFrom(global.localStorage.getItem('coppice.floor')), { layout: 'focus', locked: false });
  const select = first.doc.querySelector('#floor-layout');
  const lock = first.doc.querySelector('#floor-lock');
  assert.equal(lock.textContent, 'Lock');
  select.value = 'one';
  select.dispatchEvent({ type: 'change' });
  assert.equal(floor.tiles.layout, 'one');
  assert.deepEqual(floor.tiles.slots, ['a']);
  assert.deepEqual(tilesOf(first.doc), ['a']);
  lock.dispatchEvent({ type: 'click' });
  assert.equal(lock.textContent, 'Unlock');
  assert.equal(floor.tiles.locked, true);
  assert.equal(global.localStorage.getItem('coppice.floor'), JSON.stringify({ layout: 'one', locked: true }));

  select.value = 'all';
  select.dispatchEvent({ type: 'change' });
  assert.equal(floor.tiles.layout, 'one', 'a locked layout changed');
  assert.equal(select.value, 'one');
  assert.equal(element('status').textContent, 'Layout is locked. Unlock first.');

  const saved = global.localStorage.getItem('coppice.floor');
  reset();
  global.localStorage.setItem('coppice.floor', saved);
  const second = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 1500 });
  global.localStorage.setItem('coppice.floor', saved);
  const again = mountFloor(second.doc.body, second.state);
  assert.equal(again.tiles.layout, 'one');
  assert.equal(again.tiles.locked, true);
  assert.deepEqual(again.tiles.slots, ['a']);
  assert.equal(second.doc.querySelector('#floor-lock').textContent, 'Unlock');
  assert.equal(second.doc.querySelector('#floor-layout').value, 'one');
});

test('an unreadable stored value reads as the default', () => {
  assert.deepEqual(prefsFrom(null), { layout: 'focus', locked: false });
  assert.deepEqual(prefsFrom('not json'), { layout: 'focus', locked: false });
  assert.deepEqual(prefsFrom('[]'), { layout: 'focus', locked: false });
  assert.deepEqual(prefsFrom('{"layout":"sideways","locked":"yes"}'), { layout: 'focus', locked: false });
  assert.deepEqual(prefsFrom('{"layout":"all","locked":true}'), { layout: 'all', locked: true });
});

test('changing the layout keeps the shown panes in roster order', () => {
  const { doc, state } = stub({ panes: [p('a', 'working'), p('b', 'blocked', { summary: 'x' }), p('c', 'working')], width: 1000 });
  const floor = mountFloor(doc.body, state);
  assert.deepEqual(floor.tiles.slots, ['b', 'a']);
  const select = doc.querySelector('#floor-layout');
  select.value = 'all';
  select.dispatchEvent({ type: 'change' });
  assert.equal(floor.tiles.layout, 'all');
  assert.deepEqual(floor.tiles.slots, ['b', 'a']);
  floor.paint(state.panes);
  assert.deepEqual(floor.tiles.slots, ['b', 'a', 'c'], 'under all a paint did not give every pane a slot');
  assert.deepEqual(tilesOf(doc), ['b', 'a']);
});

test('release hands an attach out without a wire message, and adopt takes one in', async () => {
  const { doc, state, calls } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 1000 });
  const floor = mountFloor(doc.body, state);
  await settle();
  floor.onEvent({ event: 'frame', pane: 'a', seq: 1, cols: 2, rows: 1, cursor: [0, 0], rows_changed: { 0: [['h', '', '', 0], ['i', '', '', 0]] } });
  const before = calls.length;
  const grid = floor.release('a');
  assert.equal(gridToText(grid), 'hi');
  assert.equal(calls.length, before, 'release sent a message');
  assert.equal(floor.release('a'), null, 'a second release found the pane still attached');
  assert.equal(floor.release('zzz'), null);
  floor.detachAll();
  assert.deepEqual(detaches(calls).map((c) => c.pane), ['b'], 'a released pane was detached');

  floor.adopt('a', grid);
  assert.equal(calls.length, before + 1, 'adopt sent a message');
  assert.equal(doc.querySelector('[data-tile="a"] .mirror').textContent, 'hi');
  floor.onEvent({ event: 'frame', pane: 'a', seq: 2, cols: 2, rows: 1, cursor: [0, 0], rows_changed: { 0: [['o', '', '', 0], ['k', '', '', 0]] } });
  assert.equal(doc.querySelector('[data-tile="a"] .mirror').textContent, 'ok');
  floor.paint(state.panes);
  await settle();
  assert.deepEqual(attaches(calls).map((c) => c.pane), ['a', 'b', 'b'], 'the adopted pane was attached again');
  assert.equal(detaches(calls).filter((c) => c.pane === 'a').length, 0);
});

test('an adopted pane with no slot is detached by the next paint', async () => {
  const { doc, state, calls } = stub({ panes: [p('a', 'working'), p('b', 'working'), p('c', 'working')], width: 1000 });
  const floor = mountFloor(doc.body, state);
  await settle();
  floor.adopt('c', null);
  floor.paint(state.panes);
  await settle();
  assert.deepEqual(detaches(calls).map((c) => c.pane), ['c']);
});

// cellPxFor scales the drawing so the grid's width fits the canvas width.
// drawGrid paints a glyph six tenths of the cell size wide, so the cell
// size that fits cols glyphs into the width is width over cols times 0.6.
// With no measured width, before layout or in a test, it keeps the pane
// screen's own cell size.
test('cellPxFor fits the grid to the canvas width and never goes below 4', () => {
  assert.equal(cellPxFor(400, 100), 6);
  assert.equal(cellPxFor(400, 50), 13);
  assert.equal(cellPxFor(400, 200), 4);
  assert.equal(cellPxFor(1000, 3), 555);
  assert.equal(cellPxFor(0, 80), 14);
  assert.equal(cellPxFor(undefined, 80), 14);
  assert.equal(cellPxFor(400, 0), 14);
});

// The failure this names: a late refusal of an attach that is no longer
// the current one dropped the live attach from the set, so every frame
// for that pane was ignored until the next paint attached a third time.
test('a late refusal of a stale attach leaves the live attach alone', async () => {
  const { doc, state, calls } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 800 });
  let rejectFirst = null;
  let firstAttach = true;
  state.rpc = (cmd, fields) => {
    calls.push({ cmd, ...fields });
    if (cmd === 'pane.attach' && fields.pane === 'a' && firstAttach) {
      firstAttach = false;
      return new Promise((_, reject) => { rejectFirst = reject; });
    }
    return Promise.resolve({});
  };
  const floor = mountFloor(doc.body, state);
  click(doc, '[data-row="b"]');
  click(doc, '[data-row="a"]');
  await settle();
  assert.deepEqual(calls.map((c) => [c.cmd, c.pane]), [
    ['pane.attach', 'a'], ['pane.detach', 'a'], ['pane.attach', 'b'], ['pane.detach', 'b'], ['pane.attach', 'a'],
  ]);
  rejectFirst(new Error('The server did not answer in 15 s.'));
  await settle();
  floor.onEvent({ event: 'frame', pane: 'a', seq: 1, cols: 2, rows: 1, cursor: [0, 0], rows_changed: { 0: [['o', '', '', 0], ['k', '', '', 0]] } });
  assert.equal(doc.querySelector('[data-tile="a"] .mirror').textContent, 'ok');
  assert.equal(doc.querySelector('[data-tile="a"] .note').textContent, '', 'a stale refusal wrote its note on the live tile');
  floor.detachAll();
  assert.deepEqual(detaches(calls).map((c) => c.pane), ['a', 'b', 'a'], 'detachAll skipped the live attach');
});

// With no tile there is nothing to fill, so a row opens the pane screen
// the way it did before the floor. The rail is the whole page.
test('with zero tiles a row click opens the pane screen and no tile grid renders', async () => {
  const { doc, state, calls } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 500 });
  const floor = mountFloor(doc.body, state);
  await settle();
  assert.equal(doc.querySelector('#tiles'), null);
  assert.deepEqual(tilesOf(doc), []);
  assert.equal(floor.canShow(), false);
  assert.equal(attaches(calls).length, 0);
  click(doc, '[data-row="b"]');
  assert.equal(global.location.hash, '#/pane/b');
  assert.deepEqual(tilesOf(doc), []);
  floor.paint(state.panes);
  await settle();
  assert.equal(attaches(calls).length, 0, 'a paint attached a pane with no tile to show it');
});

// The pane screen holds the prompt box, the steer box, and the key
// drawer, and a phone has no ctrl key and no r key. The tile's header is
// the way there at every width.
test('a click on a tile header opens the pane screen', () => {
  const { doc, state } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 1000 });
  mountFloor(doc.body, state);
  click(doc, '[data-tile="b"] .head');
  assert.equal(global.location.hash, '#/pane/b');
});

// With no tile shown there is no focused tile to act on, even though the
// model keeps one slot, so n and r do nothing at a phone width.
test('with zero tiles n and r act on nothing', async () => {
  const { doc, state, calls } = stub({ panes: [p('a', 'blocked', { id: 'toolu_A', summary: 'a' })], width: 500 });
  mountFloor(doc.body, state);
  await settle();
  global.document.dispatchEvent({ type: 'keydown', key: 'n', target: { tagName: 'BODY' } });
  await settle();
  assert.equal(calls.filter((c) => c.path).length, 0, 'n denied an ask no tile shows');
  global.document.dispatchEvent({ type: 'keydown', key: 'r', target: { tagName: 'BODY' } });
  assert.equal(global.location.hash, '', 'r replied to a pane no tile shows');
});

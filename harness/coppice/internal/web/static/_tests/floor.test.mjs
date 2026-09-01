import test from 'node:test';
import assert from 'node:assert/strict';
import { reset, element, liveTimers } from './browser-stub.mjs';
import { tileCount, prefsFrom, mountFloor, keyBytes, titleFor, NO_AGENTS, ENDED_CLEAR_MS } from '../floor.js';
import { mountViews } from '../views.js';
import { render, paneRows } from '../roster.js';
import { gridToText } from '../grid.js';

// The floor page: the rail plus as many live windows as fit at a readable
// size. The stub has no layout, so the window count comes from the page
// size: at 900 px high, 800 px wide holds one window, 1000 and 1200 hold
// two, and 1500 holds four. These scenarios drive mountFloor against the fake DOM with a
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
    status: (msg) => {
      element('status').textContent = msg && typeof msg === 'object' ? msg.text : msg;
      element('status').actions = msg && typeof msg === 'object' && Array.isArray(msg.actions) ? msg.actions : [];
    },
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

test('a ctrl click or a middle click puts the row in a window too', () => {
  const { doc, state } = stub({ panes: [p('a', 'working'), p('b', 'working'), p('c', 'working')], width: 1000 });
  const floor = mountFloor(doc.body, state);
  assert.deepEqual(floor.tiles.slots, ['a', 'b']);
  middleClick(doc, '[data-row="c"]');
  assert.deepEqual(tilesOf(doc), ['c', 'b']);
  assert.equal(floor.typing(), 'c');
  ctrlClick(doc, '[data-row="b"]');
  assert.deepEqual(tilesOf(doc), ['c', 'b']);
  assert.equal(floor.typing(), 'b');
});


test('the window count comes from the size, and a phone gets none', () => {
  assert.equal(tileCount(0), 0);
  assert.equal(tileCount(599, 900), 0);
  assert.equal(tileCount(844, 390), 0, 'a phone on its side got a window');
  assert.equal(tileCount(600, 900), 1);
  assert.equal(tileCount(899, 900), 1);
  assert.equal(tileCount(1000, 900), 2);
  assert.equal(tileCount(1440, 900), 4);
  assert.equal(tileCount(1920, 1080), 4);
  assert.equal(tileCount(1440, 500), 2, 'a short page holds one row of windows');
  assert.equal(tileCount(2560, 1440), 9);
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

test('the tile header names the pane, and its second line the harness, the state and the last path segment', () => {
  const { doc, state } = stub({ panes: [{ id: 'w1:p1', label: 'auth fix', state: 'working', harness: 'codex', cwd: '/home/op/repo', ts: 1 }], width: 800 });
  mountFloor(doc.body, state);
  assert.ok(doc.querySelector('[data-tile="w1:p1"] .head').textContent.includes('auth fix'));
  const line2 = doc.querySelector('[data-tile="w1:p1"] .head2').textContent;
  assert.ok(line2.includes('codex'));
  assert.ok(line2.includes('working'));
  assert.ok(line2.includes('repo'));
  assert.ok(!line2.includes('/home/op'));
});

test('the floor shows only windows that hold an agent, and says how to start one when none runs', async () => {
  const { doc, state } = stub({ panes: [p('a', 'working')], width: 1500 });
  const floor = mountFloor(doc.body, state);
  assert.deepEqual(tilesOf(doc), ['a']);
  assert.equal(doc.body.querySelectorAll('.tile.empty').length, 0);
  floor.paint([]);
  for (let i = 0; i < 3; i++) await settle();
  const tiles = doc.body.querySelectorAll('.tile');
  assert.equal(tiles.length, 1);
  assert.equal(tiles[0].textContent, NO_AGENTS);
  assert.ok(!NO_AGENTS.includes('Click a row'));
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
  assert.deepEqual(tilesOf(doc), ['b'], 'a closed pane left a blank window');
  assert.deepEqual(detaches(calls), [{ cmd: 'pane.detach', pane: 'a' }]);
  floor.paint([p('b', 'working'), p('d', 'idle')]);
  assert.deepEqual(tilesOf(doc), ['b', 'd'], 'a new pane did not take the free window');
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

test('Deny posts the exact body and Reply gives the window the keys in place', async () => {
  const { doc, state, calls } = stub({ panes: [p('a', 'blocked', { id: 'toolu_01ABC', summary: 'rm -rf build/' })], width: 800 });
  const floor = mountFloor(doc.body, state);
  const buttons = doc.body.querySelectorAll('[data-tile="a"] .askbar button');
  assert.deepEqual(buttons.map((b) => b.textContent), ['Deny', 'Reply']);
  buttons[0].dispatchEvent({ type: 'click' });
  await settle();
  const posts = calls.filter((c) => c.path);
  assert.equal(posts.length, 1);
  assert.equal(posts[0].path, '/api/ask/answer');
  assert.equal(posts[0].init.method, 'POST');
  assert.deepEqual(JSON.parse(posts[0].init.body), { tool_use_id: 'toolu_01ABC', decision: 'deny', reason: 'denied from the floor' });
  assert.equal(element('status').textContent, 'Denied.');
  const before = global.location.hash;
  key(' ', { ctrlKey: true, code: 'Space' });
  assert.equal(floor.keys(), 'rail');
  const reply = doc.body.querySelectorAll('[data-tile="a"] .askbar button').find((b) => b.textContent === 'Reply');
  reply.dispatchEvent({ type: 'click' });
  assert.equal(floor.typing(), 'a', 'Reply did not give the window the keys');
  assert.equal(global.location.hash, before, 'Reply left the floor');
});

test('Deny with no ask id posts nothing', async () => {
  const { doc, state, calls } = stub({ panes: [p('a', 'blocked', { summary: 'rm -rf build/' })], width: 800 });
  mountFloor(doc.body, state);
  doc.body.querySelectorAll('[data-tile="a"] .askbar button')[0].dispatchEvent({ type: 'click' });
  await settle();
  assert.equal(calls.filter((c) => c.path).length, 0);
});

test('after leave, n denies the selected row, never from a text field, and r does nothing', async () => {
  const { doc, state, calls } = stub({
    panes: [p('a', 'blocked', { id: 'toolu_A', summary: 'a' }), p('b', 'blocked', { id: 'toolu_B', summary: 'b' })],
    width: 1000,
  });
  const floor = mountFloor(doc.body, state);
  click(doc, '[data-row="b"]');
  assert.equal(floor.typing(), 'b');
  key(' ', { ctrlKey: true, code: 'Space' });
  assert.equal(floor.railSelected(), 'b');
  global.document.dispatchEvent({ type: 'keydown', key: 'n', target: { tagName: 'TEXTAREA' } });
  await settle();
  assert.equal(calls.filter((c) => c.path).length, 0, 'a keystroke inside a text field denied an ask');
  key('n');
  await settle();
  const posts = calls.filter((c) => c.path);
  assert.equal(posts.length, 1);
  assert.equal(JSON.parse(posts[0].init.body).tool_use_id, 'toolu_B');
  key('r');
  assert.equal(global.location.hash, '', 'r left the floor');
  doc.body.hidden = true;
  key('n');
  await settle();
  assert.equal(calls.filter((c) => c.path).length, 1, 'a keystroke on another screen acted on the floor');
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
  const one = (x) => ({ ...x, cwd: '/w/one' });
  const { doc, state } = stub({ panes: [one(p('a', 'working')), one(p('b', 'blocked', { summary: 'x' })), one(p('c', 'working'))], width: 1000 });
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

// Nothing on the desktop floor opens a new page. The header selects its
// window.
test('a click on a window header selects it and gives it the keys', () => {
  const { doc, state } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 1000 });
  const floor = mountFloor(doc.body, state);
  click(doc, '[data-tile="b"] .head');
  assert.equal(global.location.hash, '');
  assert.equal(floor.typing(), 'b');
  assert.equal(focusedTile(doc), 'b');
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

// key presses one key on the page with the target and modifiers given.
function key(k, extra = {}) {
  const evt = { type: 'keydown', key: k, target: { tagName: 'BODY' }, ctrlKey: false, altKey: false, metaKey: false, shiftKey: false, prevented: false, ...extra };
  evt.preventDefault = () => { evt.prevented = true; };
  global.document.dispatchEvent(evt);
  return evt;
}

const texts = (calls, pane) => calls.filter((c) => c.cmd === 'pane.send_text' && c.pane === pane);
const typingTile = (doc) => doc.body.querySelectorAll('.typing').filter((t) => t.tagName === 'SECTION').map((t) => t.dataset.tile);

test('keyBytes turns a key into the bytes a terminal sends', () => {
  const k = (key, extra = {}) => keyBytes({ key, ctrlKey: false, altKey: false, metaKey: false, shiftKey: false, ...extra });
  assert.equal(k('a'), 'a');
  assert.equal(k('A', { shiftKey: true }), 'A');
  assert.equal(k('é'), 'é');
  assert.equal(k(' '), ' ');
  assert.equal(k('Enter'), '\r');
  assert.equal(k('Backspace'), '\x7f');
  assert.equal(k('Tab'), '\t');
  assert.equal(k('Tab', { shiftKey: true }), '\x1b[Z');
  assert.equal(k('Escape'), '\x1b');
  assert.equal(k('ArrowUp'), '\x1b[A');
  assert.equal(k('ArrowDown'), '\x1b[B');
  assert.equal(k('ArrowRight'), '\x1b[C');
  assert.equal(k('ArrowLeft'), '\x1b[D');
  assert.equal(k('c', { ctrlKey: true }), '\x03');
  assert.equal(k('C', { ctrlKey: true, shiftKey: true }), '\x03');
  assert.equal(k('Shift', { shiftKey: true }), null);
  assert.equal(k('F5'), null);
  assert.equal(k('v', { metaKey: true }), null);
  assert.equal(k('x', { altKey: true }), null);
});

test('a click on a window gives it the keys, and its border and header say typing here', () => {
  const { doc, state } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 1500 });
  const floor = mountFloor(doc.body, state);
  assert.deepEqual(typingTile(doc), ['a'], 'the keys did not start on the first window');
  click(doc, '[data-tile="b"] .tile-body');
  assert.equal(floor.typing(), 'b');
  assert.deepEqual(typingTile(doc), ['b']);
  assert.equal(focusedTile(doc), 'b');
  assert.ok(doc.querySelector('[data-tile="b"] .head').textContent.includes('typing here'));
  assert.ok(!doc.querySelector('[data-tile="a"] .head').textContent.includes('typing'));
  click(doc, '[data-tile="a"] .tile-body');
  assert.deepEqual(typingTile(doc), ['a'], 'a click on another window did not move the keys');
});

test('keys typed into a typing tile go to its pane as raw bytes', async () => {
  const { doc, state, calls } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 1500 });
  mountFloor(doc.body, state);
  click(doc, '[data-tile="b"] .tile-body');
  for (const [k, extra] of [['h'], ['i'], ['Enter'], ['Backspace'], ['Tab'], ['Escape'], ['ArrowUp'], ['c', { ctrlKey: true }]]) {
    const evt = key(k, extra);
    assert.ok(evt.prevented, k + ' was left to the browser');
  }
  for (let i = 0; i < 20; i++) await settle();
  const sent = texts(calls, 'b');
  assert.equal(sent.map((c) => c.text).join(''), 'hi\r\x7f\t\x1b\x1b[A\x03');
  assert.ok(sent.every((c) => c.enter === false), 'a typed key asked the server for an Enter');
  assert.deepEqual(texts(calls, 'a'), []);
});

test('typed text waits for the reply to the text before it', async () => {
  const { doc, state, calls } = stub({ panes: [p('a', 'working')], width: 1500 });
  let release = null;
  state.rpc = (cmd, fields) => {
    calls.push({ cmd, ...fields });
    if (cmd !== 'pane.send_text') return Promise.resolve({});
    return new Promise((r) => { release = r; });
  };
  mountFloor(doc.body, state);
  click(doc, '[data-tile="a"] .tile-body');
  key('x');
  key('y');
  for (let i = 0; i < 5; i++) await settle();
  assert.deepEqual(texts(calls, 'a').map((c) => c.text), ['x']);
  release({});
  for (let i = 0; i < 5; i++) await settle();
  assert.deepEqual(texts(calls, 'a').map((c) => c.text), ['x', 'y']);
});

test('ctrl-space gives the keyboard back, and n then acts on the roster again', async () => {
  const { doc, state, calls } = stub({ panes: [p('a', 'blocked', { id: 't1', summary: 'ls' })], width: 1500 });
  const floor = mountFloor(doc.body, state);
  click(doc, '[data-tile="a"] .tile-body');
  key('n');
  for (let i = 0; i < 5; i++) await settle();
  assert.deepEqual(texts(calls, 'a').map((c) => c.text), ['n']);
  assert.equal(calls.filter((c) => c.path).length, 0, 'n denied the ask while the tile typed');
  const leave = key(' ', { ctrlKey: true, code: 'Space' });
  assert.ok(leave.prevented);
  assert.equal(floor.typing(), null);
  assert.deepEqual(typingTile(doc), []);
  key('n');
  for (let i = 0; i < 5; i++) await settle();
  assert.equal(texts(calls, 'a').length, 1, 'n reached the pane after the leave key');
  assert.equal(calls.filter((c) => c.path === '/api/ask/answer').length, 1);
});

test('a click on a row puts its agent in a window with the keys', () => {
  const { doc, state } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 1500 });
  const floor = mountFloor(doc.body, state);
  click(doc, '[data-tile="a"] .tile-body');
  click(doc, '[data-row="b"]');
  assert.equal(floor.typing(), 'b');
  key(' ', { ctrlKey: true, code: 'Space' });
  middleClick(doc, '[data-row="a"]');
  assert.equal(floor.typing(), 'a');
});

test('a focused text field keeps its own keys while a tile types', async () => {
  const { doc, state, calls } = stub({ panes: [p('a', 'working')], width: 1500 });
  mountFloor(doc.body, state);
  click(doc, '[data-tile="a"] .tile-body');
  for (const tagName of ['INPUT', 'TEXTAREA', 'SELECT']) {
    const evt = key('q', { target: { tagName } });
    assert.ok(!evt.prevented, tagName + ' lost its key');
  }
  for (let i = 0; i < 5; i++) await settle();
  assert.deepEqual(texts(calls, 'a'), []);
});

test('a typing window whose agent leaves gives the keys to the rail', async () => {
  const { doc, state, calls } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 1500 });
  const floor = mountFloor(doc.body, state);
  click(doc, '[data-tile="b"] .tile-body');
  floor.paint([p('a', 'working')]);
  assert.equal(floor.typing(), null);
  assert.equal(floor.keys(), 'rail');
  key('z');
  await settle();
  assert.deepEqual(typingTile(doc), []);
  assert.deepEqual(texts(calls, 'a'), [], 'a key after the agent left went to another agent');
});

test('a width change redraws the tiles for the new width', async () => {
  const { doc, state, calls } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 500 });
  const floor = mountFloor(doc.body, state);
  assert.equal(floor.canShow(), false);
  floor.setWidth(1500);
  await settle();
  assert.equal(floor.canShow(), true);
  assert.deepEqual(tilesOf(doc).filter(Boolean), ['a', 'b']);
  assert.deepEqual(attaches(calls).map((c) => c.pane).sort(), ['a', 'b']);
  assert.equal(attaches(calls).some((c) => c.cols !== undefined || c.rows !== undefined), false, 'a web tile sent a size');
  click(doc, '[data-tile="a"] .tile-body');
  floor.setWidth(390);
  await settle();
  assert.equal(floor.canShow(), false);
  assert.equal(floor.typing(), null, 'a tile kept the keyboard on a phone width');
  assert.equal(doc.querySelector('#floor').hidden, true);
  assert.deepEqual(detaches(calls).map((c) => c.pane).sort(), ['a', 'b']);
  key('q');
  await settle();
  assert.deepEqual(texts(calls, 'a'), []);
});

test('a click on the page outside every tile gives the keyboard back', () => {
  const { doc, state } = stub({ panes: [p('a', 'working')], width: 1500 });
  const floor = mountFloor(doc.body, state);
  click(doc, '[data-tile="a"] .tile-body');
  const tile = doc.querySelector('[data-tile="a"]');
  doc.body.dispatchEvent({ type: 'click', target: { closest: (sel) => (sel === '.tile' ? tile : null) } });
  assert.equal(floor.typing(), 'a', 'a click inside the tile gave the keyboard away');
  doc.body.dispatchEvent({ type: 'click', target: { closest: () => null } });
  assert.equal(floor.typing(), null, 'a click on the controls kept the keyboard in the tile');
});

test('a paste into a typing tile goes to its pane as one text', async () => {
  const { doc, state, calls } = stub({ panes: [p('a', 'working')], width: 1500 });
  mountFloor(doc.body, state);
  click(doc, '[data-tile="a"] .tile-body');
  const v = key('v', { ctrlKey: true });
  assert.equal(v.prevented, false, 'ctrl-v was taken from the browser, so no paste can follow');
  const evt = { type: 'paste', target: { tagName: 'BODY' }, prevented: false, clipboardData: { getData: (t) => (t === 'text/plain' ? 'one\ntwo\r\nthree' : '') } };
  evt.preventDefault = () => { evt.prevented = true; };
  global.document.dispatchEvent(evt);
  assert.ok(evt.prevented);
  for (let i = 0; i < 5; i++) await settle();
  assert.deepEqual(texts(calls, 'a').map((c) => c.text), ['one\rtwo\rthree']);
  const field = { type: 'paste', target: { tagName: 'TEXTAREA' }, clipboardData: { getData: () => 'x' }, preventDefault() { throw new Error('a field lost its paste'); } };
  global.document.dispatchEvent(field);
  for (let i = 0; i < 5; i++) await settle();
  assert.equal(texts(calls, 'a').length, 1);
});

test('a note from the foreman draws dim under its tile header, the last three only', () => {
  const { doc, state } = stub({ panes: [p('floor', 'working'), p('b', 'working')], width: 1000 });
  const floor = mountFloor(doc.body, state);
  for (const text of ['floor › one', 'floor › two', 'floor › pane.create  docs  pi', 'floor › four']) {
    floor.onEvent({ event: 'note', pane: 'floor', text, ts: 1 });
  }
  const notes = doc.body.querySelectorAll('[data-tile="floor"] .note-line').map((n) => n.textContent);
  assert.deepEqual(notes, ['floor › two', 'floor › pane.create  docs  pi', 'floor › four']);
  assert.equal(doc.body.querySelectorAll('[data-tile="b"] .note-line').length, 0);
});

test('seedNotes draws the kept notes under the tile of the pane that wrote them', () => {
  const { doc, state } = stub({ panes: [p('floor', 'working')], width: 1000 });
  const floor = mountFloor(doc.body, state);
  floor.seedNotes([
    { event: 'note', pane: 'floor', text: 'floor › pane.list', ts: 1 },
    { event: 'note', text: 'an operator note', ts: 2 },
  ]);
  const notes = doc.body.querySelectorAll('[data-tile="floor"] .note-line').map((n) => n.textContent);
  assert.deepEqual(notes, ['floor › pane.list']);
});

test('the tab title carries the count of panes that need the operator', () => {
  const { doc, state } = stub({ panes: [p('a', 'blocked', { id: 't1', summary: 'ls' }), p('b', 'blocked', { id: 't2', summary: 'ls' }), p('c', 'working')], width: 1200 });
  const floor = mountFloor(doc.body, state);
  assert.equal(global.document.title, '2 · coppice');
  floor.paint([p('a', 'working'), p('c', 'working')]);
  assert.equal(global.document.title, 'coppice');
  assert.equal(titleFor([p('a', 'blocked', { id: 't', summary: 'x' })]), '1 · coppice');
  assert.equal(titleFor(null), 'coppice');
});

test('the rail holds one link per view, and none for the minimap', async () => {
  reset();
  const list = element('view-list');
  const views = mountViews(list, {
    api: async () => ({ views: [{ id: 'tree', title: 'Tree' }, { id: 'kanban', title: 'Kanban' }, { id: 'minimap', title: 'Minimap' }] }),
    selection: () => [],
    go: () => {},
    skip: (v) => v.id === 'minimap',
  });
  await views.load();
  assert.deepEqual(list.querySelectorAll('button').map((b) => b.dataset.view), ['tree', 'kanban']);
  assert.equal(list.hidden, false);
});

test('layout and lock sit in the rail footer when the page has one', () => {
  const { doc, state } = stub({ panes: [p('a', 'working')], width: 1200 });
  const foot = element('rail-foot');
  doc.body.append(foot);
  mountFloor(doc.body, state);
  assert.ok(foot.querySelector('#floor-lock'), 'the lock is not in the rail footer');
  assert.ok(foot.querySelector('#floor-layout'), 'the layout is not in the rail footer');
  assert.equal(foot.hidden, false);
});

function drag(fromHead, toTile) {
  fromHead.dispatchEvent({ type: 'dragstart', dataTransfer: { setData() {}, effectAllowed: '' } });
  toTile.dispatchEvent({ type: 'dragover', preventDefault() {} });
  toTile.dispatchEvent({ type: 'drop', preventDefault() {} });
}

test('a tile header drags onto another tile to swap them, and the lock stops the drag', () => {
  const { doc, state } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 1200 });
  const floor = mountFloor(doc.body, state);
  assert.deepEqual(tilesOf(doc), ['a', 'b']);
  const headOf = (id) => doc.querySelector('[data-tile="' + id + '"]').querySelector('.head');
  assert.equal(headOf('a').draggable, true);
  drag(headOf('a'), doc.querySelector('[data-tile="b"]'));
  assert.deepEqual(tilesOf(doc), ['b', 'a']);
  doc.querySelector('#floor-lock').dispatchEvent({ type: 'click' });
  assert.equal(floor.tiles.locked, true);
  // A browser fires no dragstart on a header that is not draggable, so the
  // header stays draggable and the lock refuses the drag as it starts.
  let refused = 0;
  element('status').textContent = '';
  headOf('b').dispatchEvent({ type: 'dragstart', preventDefault() { refused += 1; }, dataTransfer: { setData() {} } });
  assert.equal(refused, 1, 'a locked tile started a drag');
  drag(headOf('b'), doc.querySelector('[data-tile="a"]'));
  assert.deepEqual(tilesOf(doc), ['b', 'a'], 'a locked floor swapped tiles');
  assert.equal(element('status').textContent, 'Layout is locked. Unlock first.', 'a refused drag said nothing');
});

test('a drag cut by a repaint swaps nothing on a later drop', () => {
  const { doc, state } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 1200 });
  const floor = mountFloor(doc.body, state);
  const head = doc.querySelector('[data-tile="a"]').querySelector('.head');
  head.dispatchEvent({ type: 'dragstart', dataTransfer: { setData() {}, effectAllowed: '' } });
  floor.paint([p('a', 'working'), p('b', 'working')]);
  const b = doc.querySelector('[data-tile="b"]');
  b.dispatchEvent({ type: 'dragover', preventDefault() {} });
  b.dispatchEvent({ type: 'drop', preventDefault() {} });
  assert.deepEqual(tilesOf(doc), ['a', 'b'], 'a drop after a repaint swapped from the old drag');
});

test('a harness-held ask bar answers over the operator connection, never in a shell', async () => {
  const { doc, state, calls } = stub({
    panes: [p('a', 'blocked', { id: 'per_1', summary: 'rm -rf build', tier: 'permanent', holder: 'harness' })],
    width: 1500,
  });
  mountFloor(doc.body, state);
  const bar = doc.querySelector('[data-tile="a"] .askbar');
  assert.ok(bar.textContent.includes('The harness holds this ask itself'));
  assert.ok(!bar.textContent.includes('coppice agent'));
  assert.ok(!bar.textContent.includes('gone'));
  const labels = bar.querySelectorAll('button').map((b) => b.textContent);
  assert.ok(labels.includes('Deny') && !labels.some((l) => l.includes('shell')), 'labels ' + labels);
  assert.equal(bar.querySelectorAll('input').length, 1, 'a permanent ask needs the name field');
  bar.querySelectorAll('button').find((b) => b.textContent === 'Deny').dispatchEvent({ type: 'click' });
  for (let i = 0; i < 5; i += 1) await Promise.resolve();
  assert.deepEqual(calls.filter((c) => c.cmd && c.cmd.startsWith('agent.')),
    [{ cmd: 'agent.deny', pane: 'a', ask: 'per_1', reason: 'denied from the phone' }]);
  assert.equal(calls.filter((c) => c.path).length, 0, 'a held ask went to the gate channel');
});

// endedStub is a floor whose server answers pane.list with ended: true from
// the records given, and every other call with {}.
function endedStub(panes, endedRecords, width = 1500) {
  const s = stub({ panes, width });
  s.state.rpc = async (cmd, fields) => {
    s.calls.push({ cmd, ...fields });
    if (cmd === 'pane.list' && fields && fields.ended) return { panes: endedRecords };
    return {};
  };
  return s;
}
const endedReads = (calls) => calls.filter((c) => c.cmd === 'pane.list' && c.ended === true);
const drain = async () => { for (let i = 0; i < 4; i++) await settle(); };

test('windows fill with agents that need you, then working ones, then the rest', () => {
  const { doc, state } = stub({
    panes: [p('i', 'idle'), p('w', 'working'), p('b', 'blocked', { id: 't', summary: 'x' }), p('u', 'unknown')],
    width: 1000,
  });
  const floor = mountFloor(doc.body, state);
  assert.deepEqual(floor.tiles.slots, ['b', 'w']);
  assert.deepEqual(tilesOf(doc), ['b', 'w']);
});

test('the keys start on the first agent that needs you', () => {
  const { doc, state } = stub({
    panes: [p('w', 'working'), p('b', 'blocked', { id: 't', summary: 'x' })],
    width: 1500,
  });
  const floor = mountFloor(doc.body, state);
  assert.equal(floor.typing(), 'b');
  assert.deepEqual(typingTile(doc), ['b']);
});

test('the keys start on the first window when nothing needs you, and on the rail with no agent', () => {
  const one = stub({ panes: [p('w', 'working'), p('i', 'idle')], width: 1500 });
  const floor = mountFloor(one.doc.body, one.state);
  assert.equal(floor.typing(), 'w');

  const none = stub({ panes: [], width: 1500 });
  const empty = mountFloor(none.doc.body, none.state);
  assert.equal(empty.typing(), null);
  assert.equal(empty.keys(), 'rail');
});

test('the keys find their start place once, and a later ask does not take them', () => {
  const { doc, state } = stub({ panes: [p('w', 'working'), p('i', 'idle')], width: 1500 });
  const floor = mountFloor(doc.body, state);
  click(doc, '[data-tile="i"] .tile-body');
  floor.paint([p('w', 'working'), p('i', 'idle'), p('b', 'blocked', { id: 't', summary: 'x' })]);
  assert.equal(floor.typing(), 'i', 'a new ask moved the keys');
  assert.deepEqual(tilesOf(doc), ['w', 'i', 'b'], 'the new agent did not take a free window');
});

test('the keys wait for the floor to show before they find their start place', () => {
  const { doc, state } = stub({ panes: [p('w', 'working'), p('b', 'blocked', { id: 't', summary: 'x' })], width: 1500 });
  doc.body.hidden = true;
  const floor = mountFloor(doc.body, state);
  assert.equal(floor.typing(), null);
  doc.body.hidden = false;
  floor.paint(state.panes);
  assert.equal(floor.typing(), 'b');
});

test('an agent that ends on its own leaves its window with one line, and the keys go to the rail', async () => {
  const { doc, state, calls } = endedStub([p('a', 'working'), p('b', 'working')], [{ id: 'b', label: 'sh-b', exit_code: 3, ended_at: 9 }]);
  const floor = mountFloor(doc.body, state);
  click(doc, '[data-tile="b"] .tile-body');
  floor.onEvent({ event: 'state', pane: 'b', state: 'done', source: 'process', harness: 'shell', ts: 2, detail: 'exit=3' });
  await drain();
  assert.equal(floor.typing(), null);
  assert.equal(floor.keys(), 'rail');
  floor.paint([p('a', 'working')]);
  await drain();
  assert.equal(floor.railSelected(), 'a', 'the rail selected no live row');
  assert.ok(doc.querySelector('[data-row="a"]').className.includes('selected'), 'the rail was not drawn again with the selected row');
  const line = doc.querySelector('[data-ended="b"]');
  assert.ok(line, 'no window shows the line');
  assert.equal(line.querySelector('.ended-line').textContent, 'sh-b ended (exit 3)');
  assert.ok(line.className.includes('amber'), 'a non-zero exit is not amber');
  assert.deepEqual(detaches(calls).map((c) => c.pane), ['b']);
  // The next pane list leaves the ended agent out. It never fills a window.
  floor.paint([p('a', 'working')]);
  await drain();
  const still = doc.querySelector('[data-ended="b"]');
  assert.ok(still, 'a non-zero line went away on its own');
  assert.equal(endedReads(calls).length, 1, 'the ended list was read more than once');
  still.dispatchEvent({ type: 'click' });
  assert.equal(doc.querySelector('[data-ended="b"]'), null, 'a click did not clear the line');
  assert.deepEqual(tilesOf(doc), ['a']);
  assert.equal(attaches(calls).filter((c) => c.pane === 'b').length, 1, 'the ended agent was attached again');
});

test('the line of an agent that ended well clears itself', async () => {
  const { doc, state } = endedStub([p('a', 'working'), p('b', 'working')], [{ id: 'b', label: 'b', exit_code: 0, ended_at: 9 }]);
  const floor = mountFloor(doc.body, state);
  floor.paint([p('a', 'working')]);
  await drain();
  const line = doc.querySelector('[data-ended="b"]');
  assert.equal(line.querySelector('.ended-line').textContent, 'b ended (exit 0)');
  assert.ok(!line.className.includes('amber'));
  const timer = liveTimers().find((t) => t.ms === ENDED_CLEAR_MS);
  assert.ok(timer, 'no timer clears the line');
  timer.fn();
  assert.equal(doc.querySelector('[data-ended="b"]'), null);
});

test('an agent the operator closed leaves no line', async () => {
  const { doc, state, calls } = endedStub([p('a', 'working'), p('b', 'working')], []);
  const floor = mountFloor(doc.body, state);
  floor.onEvent({ event: 'state', pane: 'b', state: 'done', source: 'process', harness: 'shell', ts: 2 });
  await drain();
  assert.equal(doc.querySelector('[data-ended="b"]'), null);
  assert.equal(endedReads(calls).length, 1);
});

test('an agent with no window that ends says so on the status line, with Resume and Forget', async () => {
  const { doc, state } = endedStub([p('a', 'working'), p('b', 'working'), p('c', 'working')], [{ id: 'c', label: 'c', exit_code: 1 }], 1000);
  const floor = mountFloor(doc.body, state);
  assert.equal(floor.windowOf('c'), 0);
  floor.onEvent({ event: 'state', pane: 'c', state: 'done', source: 'process', harness: 'shell', ts: 2 });
  await drain();
  assert.equal(element('status').textContent, 'c ended (exit 1)');
  assert.deepEqual(element('status').actions.map((x) => [x.kind, x.pane]), [['resume', 'c'], ['forget', 'c']]);
});

test('a refused attach is not tried again until the agent is put in a window again', async () => {
  const { doc, state, calls } = stub({ panes: [p('a', 'working')], width: 1500 });
  state.rpc = async (cmd, fields) => {
    calls.push({ cmd, ...fields });
    if (cmd === 'pane.attach') {
      const err = new Error('attach refused');
      err.code = 'internal';
      throw err;
    }
    return {};
  };
  const floor = mountFloor(doc.body, state);
  await drain();
  for (let i = 0; i < 3; i++) floor.paint(state.panes);
  await drain();
  assert.equal(attaches(calls).length, 1, 'a refused attach was tried on every paint');
  assert.equal(doc.querySelector('[data-tile="a"] .note').textContent, 'attach refused');
  click(doc, '[data-row="a"]');
  await drain();
  assert.equal(attaches(calls).length, 2, 'putting the agent in a window did not try again');
});

test('an attach refused because the pane ended reads the ended list', async () => {
  const { doc, state, calls } = stub({ panes: [p('a', 'working')], width: 1500 });
  state.rpc = async (cmd, fields) => {
    calls.push({ cmd, ...fields });
    if (cmd === 'pane.attach') {
      const err = new Error('pane a has closed.');
      err.code = 'pane_closed';
      throw err;
    }
    if (cmd === 'pane.list') return { panes: [{ id: 'a', label: 'a', exit_code: 2 }] };
    return {};
  };
  mountFloor(doc.body, state);
  await drain();
  assert.equal(doc.querySelector('[data-ended="a"] .ended-line').textContent, 'a ended (exit 2)');
});

test('after leave the arrows move the rail row, and Enter puts it in a window with the keys', () => {
  const { doc, state } = stub({ panes: [p('a', 'working'), p('b', 'working'), p('c', 'working')], width: 1000 });
  const floor = mountFloor(doc.body, state);
  assert.equal(floor.typing(), 'a');
  const leave = key(' ', { ctrlKey: true, code: 'Space' });
  assert.ok(leave.prevented);
  assert.equal(floor.railSelected(), 'a');
  render(paneRows(state.panes, 1));
  assert.ok(doc.querySelector('[data-row="a"]').className.includes('selected'), 'the selected row has no border');
  key('ArrowDown');
  key('j');
  assert.equal(floor.railSelected(), 'c');
  key('ArrowDown');
  assert.equal(floor.railSelected(), 'c', 'the selection ran off the end');
  key('k');
  assert.equal(floor.railSelected(), 'b');
  const enter = key('Enter');
  assert.ok(enter.prevented);
  assert.equal(floor.typing(), 'b');
  assert.equal(floor.railSelected(), '');
});

test('after leave a number puts the selected row in that window', () => {
  const { doc, state } = stub({ panes: [p('a', 'working'), p('b', 'working'), p('c', 'working')], width: 1000 });
  const floor = mountFloor(doc.body, state);
  key(' ', { ctrlKey: true, code: 'Space' });
  key('ArrowDown');
  key('ArrowDown');
  assert.equal(floor.railSelected(), 'c');
  key('2');
  assert.deepEqual(tilesOf(doc), ['a', 'c']);
  assert.equal(floor.typing(), 'c');
  key(' ', { ctrlKey: true, code: 'Space' });
  key('k');
  key('k');
  assert.equal(floor.railSelected(), 'a');
  key('2');
  assert.deepEqual(tilesOf(doc), ['c', 'a'], 'an agent already in a window did not swap places');
  key(' ', { ctrlKey: true, code: 'Space' });
  key('7');
  assert.deepEqual(tilesOf(doc), ['c', 'a'], 'a number past the windows did something');
});

test('ctrl-space on the rail gives the keys back to the selected window', () => {
  const { state, doc } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 1000 });
  const floor = mountFloor(doc.body, state);
  click(doc, '[data-tile="b"] .tile-body');
  key(' ', { ctrlKey: true, code: 'Space' });
  assert.equal(floor.typing(), null);
  key(' ', { ctrlKey: true, code: 'Space' });
  assert.equal(floor.typing(), 'b');
});

test('a row shows the number of the window its agent is in', () => {
  const { doc, state } = stub({ panes: [p('a', 'working'), p('b', 'working'), p('c', 'working')], width: 1000 });
  const floor = mountFloor(doc.body, state);
  const winOf = (id) => {
    const mark = doc.querySelector('[data-row="' + id + '"]').querySelector('.win');
    return mark ? mark.textContent : '';
  };
  assert.equal(floor.windowOf('a'), 1);
  assert.equal(floor.windowOf('b'), 2);
  assert.equal(floor.windowOf('c'), 0);
  render(paneRows(state.panes, 1));
  assert.equal(winOf('a'), '1');
  assert.equal(winOf('b'), '2');
  assert.equal(winOf('c'), '');
  assert.equal(doc.querySelector('[data-tile="b"] .num').textContent, '2', 'the window header has no number');
});

test('the windows grid takes the shape of the agents it shows', async () => {
  const { doc, state } = stub({ panes: [p('a', 'working'), p('b', 'working'), p('c', 'working')], width: 1500 });
  const floor = mountFloor(doc.body, state);
  const grid = doc.querySelector('#tiles');
  assert.equal(grid.dataset.cols, '2');
  assert.equal(grid.dataset.rows, '2');
  const last = doc.querySelector('[data-tile="c"]');
  assert.equal(last.style.gridColumn, 'span 2', 'the last window does not take the spare cell');
  floor.paint([p('a', 'working'), p('b', 'working')]);
  await drain();
  assert.equal(grid.dataset.cols, '1', 'two agents do not sit one above the other at full width');
});

test('a shrinking page keeps the typing agent in a window', async () => {
  const { doc, state } = stub({ panes: [p('a', 'working'), p('b', 'working'), p('c', 'working'), p('d', 'working')], width: 1500 });
  const floor = mountFloor(doc.body, state);
  assert.deepEqual(tilesOf(doc), ['a', 'b', 'c', 'd']);
  click(doc, '[data-tile="d"] .tile-body');
  assert.equal(floor.typing(), 'd');
  floor.setSize(1000, 900);
  await drain();
  assert.equal(tilesOf(doc).length, 2);
  assert.equal(floor.typing(), 'd', 'the keys moved to another agent when the page shrank');
  assert.ok(tilesOf(doc).includes('d'));
});

test('on the rail, Enter on a focused button stays with the button', async () => {
  const { doc, state } = stub({ panes: [p('a', 'working'), p('b', 'working'), p('c', 'working')], width: 1000 });
  const floor = mountFloor(doc.body, state);
  key(' ', { ctrlKey: true, code: 'Space' });
  key('ArrowDown');
  key('ArrowDown');
  assert.equal(floor.railSelected(), 'c');
  for (const target of [{ tagName: 'BUTTON' }, { tagName: 'A' }, { tagName: 'DIV', isContentEditable: true }]) {
    const evt = key('Enter', { target });
    assert.equal(evt.prevented, false, target.tagName + ' lost its Enter');
  }
  assert.deepEqual(tilesOf(doc), ['a', 'b'], 'Enter on a button put a row in a window');
  assert.equal(floor.typing(), null);
});

test('on the rail, Enter on a row that has focus takes that row', () => {
  const { doc, state } = stub({ panes: [p('a', 'working'), p('b', 'working'), p('c', 'working')], width: 1000 });
  const floor = mountFloor(doc.body, state);
  key(' ', { ctrlKey: true, code: 'Space' });
  assert.equal(floor.railSelected(), 'a');
  const row = doc.querySelector('[data-row="c"]');
  const target = { tagName: 'LI', closest: (sel) => (sel === '[data-row]' ? row : null) };
  key('Enter', { target });
  assert.equal(floor.typing(), 'c', 'Enter took the selected row, not the focused one');
});

test('an agent known to be gone never fills a window from a stale list', async () => {
  const { doc, state } = endedStub([p('a', 'working'), p('b', 'working')], [{ id: 'b', label: 'b', exit_code: 3 }], 1000);
  const floor = mountFloor(doc.body, state);
  key(' ', { ctrlKey: true, code: 'Space' });
  key('ArrowDown');
  assert.equal(floor.railSelected(), 'b');
  floor.onEvent({ event: 'state', pane: 'b', state: 'done', source: 'process', harness: 'shell', ts: 2 });
  await drain();
  assert.equal(floor.railSelected(), 'a', 'the rail kept an agent that ended');
  doc.querySelector('[data-ended="b"]').dispatchEvent({ type: 'click' });
  // A pane list read before the end still names b.
  floor.paint([p('a', 'working'), p('b', 'working')]);
  await drain();
  assert.deepEqual(tilesOf(doc), ['a'], 'an ended agent came back into a window');
  floor.fillRow('b');
  floor.putRow('b', 1);
  assert.deepEqual(tilesOf(doc), ['a']);
});

test('the keys taking a window blur a focused text field', () => {
  let blurred = 0;
  global.document.activeElement = { tagName: 'INPUT', blur() { blurred += 1; } };
  try {
    const { doc, state } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 1000 });
    mountFloor(doc.body, state);
    assert.equal(blurred, 1, 'the start left a text field focused under typing here');
    click(doc, '[data-tile="b"] .tile-body');
    assert.equal(blurred, 2);
  } finally {
    delete global.document.activeElement;
  }
});

// sizedBody gives a window body a layout the stub does not have.
function sizedBody(view, w, h) {
  view.clientWidth = w;
  view.clientHeight = h;
  view.scrollTop = 0;
  view.scrollLeft = 0;
}

function tallFrame(pane, cursor) {
  const rows = {};
  for (let y = 0; y < 40; y++) rows[y] = [['x', '', '', 0]];
  return { event: 'frame', pane, seq: 1, cols: 120, rows: 40, cursor, rows_changed: rows };
}

test('a window follows the cursor down only while it is at the bottom, and sideways when the cursor leaves the right edge', async () => {
  const { doc, state } = stub({ panes: [p('a', 'working')], width: 1000 });
  const floor = mountFloor(doc.body, state);
  await drain();
  const body = doc.querySelector('[data-tile="a"] .tile-body');
  sizedBody(body, 700, 100);
  floor.onEvent(tallFrame('a', [0, 39]));
  // 40 rows of a 14 px line: the bottom is at 560.
  assert.equal(body.scrollTop, 460, 'the first frame did not show the cursor');
  body.scrollTop = 0;
  floor.onEvent(tallFrame('a', [0, 39]));
  assert.equal(body.scrollTop, 0, 'a frame pulled the view down while the reader scrolled up');
  body.scrollTop = 460;
  floor.onEvent(tallFrame('a', [110, 39]));
  assert.ok(body.scrollLeft + 700 >= 111 * 7, 'the cursor column is off the right edge');
  floor.onEvent(tallFrame('a', [2, 39]));
  assert.ok(body.scrollLeft <= 2 * 7, 'the cursor column is off the left edge');
});

test('a change of pixel ratio draws every window again', async () => {
  const queries = [];
  global.window.matchMedia = (q) => {
    const mq = { q, listeners: [], addEventListener(type, fn) { mq.listeners.push(fn); } };
    queries.push(mq);
    return mq;
  };
  try {
    const { doc, state } = stub({ panes: [p('a', 'working')], width: 1000 });
    const floor = mountFloor(doc.body, state);
    await drain();
    floor.onEvent({ event: 'frame', pane: 'a', seq: 1, cols: 2, rows: 1, cursor: [0, 0], rows_changed: { 0: [['h', '', '', 0], ['i', '', '', 0]] } });
    assert.ok(queries.length >= 1 && /resolution: 1dppx/.test(queries[queries.length - 1].q), 'the floor does not watch the pixel ratio');
    global.window.devicePixelRatio = 2;
    queries[queries.length - 1].listeners.forEach((fn) => fn());
    assert.ok(/resolution: 2dppx/.test(queries[queries.length - 1].q), 'the floor did not watch the new ratio');
  } finally {
    delete global.window.matchMedia;
    delete global.window.devicePixelRatio;
  }
});

test('the selected rail row says so to a screen reader', () => {
  const { doc, state } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 1000 });
  const floor = mountFloor(doc.body, state);
  key(' ', { ctrlKey: true, code: 'Space' });
  assert.equal(floor.railSelected(), 'a');
  assert.equal(doc.querySelector('[data-row="a"]').getAttribute('aria-current'), 'true');
  assert.equal(doc.querySelector('[data-row="b"]').getAttribute('aria-current'), null);
});

test('a window laid out after its first frame still shows the cursor', async () => {
  const { doc, state } = stub({ panes: [p('a', 'working')], width: 1000 });
  const floor = mountFloor(doc.body, state);
  await drain();
  floor.onEvent(tallFrame('a', [0, 39]));
  const body = doc.querySelector('[data-tile="a"] .tile-body');
  sizedBody(body, 700, 100);
  floor.paint(state.panes);
  assert.equal(body.scrollTop, 460, 'the window stayed at the top, away from the cursor');
});

// A browser resets the scroll of an element that leaves the page and comes
// back, which is what redrawing the windows does.
test('a window keeps its scroll when the windows are drawn again', async () => {
  const { doc, state } = stub({ panes: [p('a', 'working')], width: 1000 });
  const floor = mountFloor(doc.body, state);
  await drain();
  const body = doc.querySelector('[data-tile="a"] .tile-body');
  sizedBody(body, 700, 100);
  floor.onEvent(tallFrame('a', [110, 39]));
  const top = body.scrollTop;
  const left = body.scrollLeft;
  assert.ok(top > 0 && left > 0);
  const grid = doc.querySelector('#tiles');
  const put = grid.replaceChildren.bind(grid);
  grid.replaceChildren = (...nodes) => {
    body.scrollTop = 0;
    body.scrollLeft = 0;
    put(...nodes);
  };
  floor.paint(state.panes);
  assert.equal(body.scrollTop, top, 'a redraw lost the scroll');
  assert.equal(body.scrollLeft, left);
});

test('a window header shows the stack bar and the gate mark, and a segment opens its details in place', async () => {
  const row = {
    ...p('a', 'working'),
    stack: { loop: 'claude', model: 'claude-fake-2', router: 'direct', daisugi: { mode: 'enforcing', armed: true } },
    gate: { decision: 'deny', tool: 'Bash', clause: 'shell: curl', at: Date.now() / 1000 },
    tokens: { fresh: 13, cache_read: 300, cache_write: 20, out: 12 },
  };
  const { doc, state } = stub({ panes: [row], width: 800 });
  const opened = [];
  state.journal = (pane) => opened.push(pane);
  mountFloor(doc.body, state);
  const segs = doc.body.querySelectorAll('[data-tile="a"] .stackbar .seg');
  assert.deepEqual(segs.map((b) => b.textContent), ['claude', 'enforcing', 'direct', 'claude-fake-2']);
  assert.ok(segs[1].className.includes('h-ok'), 'a deny coloured the gate: ' + segs[1].className);
  assert.ok(doc.querySelector('[data-tile="a"] .head2 .stackbar'), 'the bar is not on the second line');
  assert.ok(segs[0].className.includes('lit'), 'the light is not on the loop while working');
  const mark = doc.querySelector('[data-tile="a"] .gate');
  assert.equal(mark.textContent, '✕');
  assert.equal(mark.title, 'Last gate verdict: deny Bash. Clause: shell: curl');
  assert.equal(doc.querySelector('[data-tile="a"] .gate-tip').textContent, 'Last gate verdict: deny Bash. Clause: shell: curl');
  assert.equal(doc.querySelector('[data-tile="a"] .stack-details'), null);
  segs[3].dispatchEvent({ type: 'click' });
  const box = doc.querySelector('[data-tile="a"] .stack-details');
  assert.ok(box, 'the model details did not open');
  assert.ok(box.textContent.includes('cache read'));
  assert.ok(box.textContent.includes('Swap is not built yet.'));
  assert.ok(doc.querySelector('[data-tile="a"] .seg-model').className.includes('open'));
  doc.querySelector('[data-tile="a"] .gate').dispatchEvent({ type: 'click' });
  const daisugi = doc.querySelector('[data-tile="a"] .stack-details');
  assert.equal(daisugi.dataset.details, 'daisugi');
  assert.ok(daisugi.textContent.includes('shell: curl'));
  daisugi.querySelectorAll('button').find((b) => b.textContent === 'Journal').dispatchEvent({ type: 'click' });
  assert.deepEqual(opened, ['a']);
});

test('a window whose agent has no stack shows its harness word and no bar', async () => {
  const { doc, state } = stub({ panes: [p('a', 'idle')], width: 800 });
  mountFloor(doc.body, state);
  assert.equal(doc.querySelector('[data-tile="a"] .stackbar'), null);
  assert.equal(doc.querySelector('[data-tile="a"] .gate'), null);
  assert.ok(doc.querySelector('[data-tile="a"] .head2').textContent.includes('claude'));
});

test('a window with a headless agent sends no keys, and its own line sends whole messages', async () => {
  const hl = { ...p('h', 'idle'), kind: 'headless', harness: 'sprig' };
  const { doc, state, calls } = stub({ panes: [hl, p('a', 'working')], width: 1500 });
  mountFloor(doc.body, state);
  click(doc, '[data-tile="h"] .tile-body');
  const tile = doc.querySelector('[data-tile="h"]');
  assert.match(tile.textContent, /This agent takes whole messages. Type below and press Enter./);
  const input = tile.querySelector('.prompt-input');
  assert.ok(input, 'the headless window has no line to type in');
  key('x');
  key('Enter');
  await settle();
  assert.equal(texts(calls, 'h').length, 0, 'a raw key reached a headless agent');
  input.value = 'tidy the docs';
  const evt = { type: 'keydown', key: 'Enter', target: input, preventDefault() {}, stopPropagation() {} };
  input.dispatchEvent(evt);
  await settle();
  assert.deepEqual(calls.filter((c) => c.cmd === 'agent.prompt').map((c) => [c.pane, c.text]), [['h', 'tidy the docs']]);
  assert.equal(input.value, '');
  assert.equal(doc.querySelector('[data-tile="a"] .prompt-input'), null, 'a pty window grew a message line');
});

import test from 'node:test';
import assert from 'node:assert/strict';
import { reset, element, setFetch, TOKEN, freshApp, lastSocket } from './browser-stub.mjs';
import { mountFloor, FOLD_KEY } from '../floor.js';
import { render, paneRows, renderRecent } from '../roster.js';
import { ROW_TYPE } from '../rail.js';
import { mountNewButton, NO_DEFAULT } from '../newpane.js';

// The rail on the floor page: a tree that folds, a x that stops an agent
// after one confirm, rows that drag onto windows, labels renamed in place,
// Recent at the foot, and New.

const TASKS = [
  { id: 't1', label: 'review', parent: '', state: 'blocked' },
  { id: 't2', label: 'docs', parent: 't1', state: 'blocked' },
  { id: 't3', label: 'other', parent: '', state: 'working' },
];

function p(id, state, extra = {}) {
  return { id, label: id, state, harness: 'claude', cwd: '/src/one', ts: 1, ...extra };
}

function stub({ panes, width = 1500, tasks, rpc }) {
  reset();
  global.window.innerWidth = width;
  const root = element('screen-roster');
  root.append(element('queue-head'), element('roster'), element('roster-empty'));
  element('queue-head').append(element('queue-title'), element('queue-sub'));
  const calls = [];
  const state = {
    panes,
    width,
    tasks,
    rpc: async (cmd, fields) => {
      calls.push({ cmd, ...fields });
      return rpc ? rpc(cmd, fields) : {};
    },
    api: async () => ({}),
    status: (msg) => { element('status').textContent = msg; },
  };
  const floor = mountFloor(root, state);
  render(paneRows(panes, 1));
  return { root, state, calls, floor };
}

const settle = () => new Promise((r) => setImmediate(r));
const drain = async () => { for (let i = 0; i < 4; i++) await settle(); };
const railItems = (root) => root.querySelector('#roster').children;
const railText = (root) => railItems(root).map((li) => (li.dataset.group ? 'g:' + li.querySelector('.glabel').textContent : li.dataset.row));
const tilesOf = (root) => root.querySelectorAll('.tile').map((t) => t.dataset.tile || '');

function key(k, extra = {}) {
  const evt = { type: 'keydown', key: k, target: { tagName: 'BODY' }, ctrlKey: false, altKey: false, metaKey: false, shiftKey: false, prevented: false, ...extra };
  evt.preventDefault = () => { evt.prevented = true; };
  global.document.dispatchEvent(evt);
  return evt;
}

function click(el, extra = {}) {
  assert.ok(el, 'nothing to click');
  el.dispatchEvent({ type: 'click', button: 0, ctrlKey: false, detail: 1, stopPropagation() {}, ...extra });
}

test('agents in two projects group under each project, and the tasks come first', () => {
  const panes = [p('a', 'idle', { task: 't2' }), p('b', 'working', { task: 't1' }), p('c', 'idle', { cwd: '/src/glean' }), p('d', 'working', { cwd: '/src/trellis' })];
  const { root } = stub({ panes, tasks: TASKS });
  assert.deepEqual(railText(root), ['g:review', 'b', 'g:docs', 'a', 'g:other', 'g:glean', 'c', 'g:trellis', 'd']);
  const review = root.querySelector('[data-group="task:t1"]');
  assert.equal(review.querySelector('.fold').getAttribute('aria-label'), 'review, 1 working, 1 idle');
  assert.equal(review.querySelector('.fold').getAttribute('aria-expanded'), 'true');
});

test('a group folds to one line, stays folded across a reload, and the arrows skip what it hides', () => {
  const panes = [p('a', 'idle', { cwd: '/src/glean' }), p('b', 'idle', { cwd: '/src/glean' }), p('c', 'idle', { cwd: '/src/trellis' })];
  const { root, floor } = stub({ panes, width: 1000 });
  click(root.querySelector('[data-group="dir:/src/glean"] .fold'));
  assert.deepEqual(railText(root), ['g:glean', 'g:trellis', 'c']);
  assert.equal(root.querySelector('[data-group="dir:/src/glean"] .fold').getAttribute('aria-expanded'), 'false');
  assert.deepEqual(JSON.parse(global.localStorage.getItem(FOLD_KEY)), ['dir:/src/glean']);
  key(' ', { ctrlKey: true, code: 'Space' });
  key('ArrowUp');
  key('ArrowUp');
  assert.equal(floor.railSelected(), 'c', 'the arrows reached a folded row');
  // A new floor in the same browser keeps the fold.
  const saved = global.localStorage.getItem(FOLD_KEY);
  const again = stub({ panes, width: 1000 });
  global.localStorage.setItem(FOLD_KEY, saved);
  const floor2 = mountFloor(again.root, again.state);
  assert.ok(floor2.folded().has('dir:/src/glean'));
});

test('a row shows no harness the server does not know', () => {
  const { root } = stub({ panes: [p('a', 'idle', { harness: '' }), p('b', 'idle', { harness: 'unknown' }), p('c', 'idle')] });
  assert.equal(root.querySelector('[data-row="a"]').querySelector('.harness'), null);
  assert.equal(root.querySelector('[data-row="b"]').querySelector('.harness'), null);
  assert.equal(root.querySelector('[data-row="c"]').querySelector('.harness').textContent, 'claude');
  assert.ok(!root.querySelector('#roster').textContent.includes('unknown'));
});

test('the x on a row asks once, and Enter stops the agent and takes it off the floor', async () => {
  const { root, floor, calls } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 1000 });
  click(root.querySelector('[data-row="b"] .kill'));
  assert.equal(floor.killPending(), 'b');
  const row = root.querySelector('[data-row="b"]');
  assert.equal(row.querySelector('.confirm-text').textContent, 'Stop b? Enter stops it, Esc keeps it.');
  assert.ok(root.querySelector('[data-tile="b"]').querySelector('.confirm'), 'the window header shows no confirm');
  const enter = key('Enter');
  assert.ok(enter.prevented);
  await drain();
  assert.deepEqual(calls.filter((c) => c.cmd === 'pane.close').map((c) => c.pane), ['b']);
  assert.equal(calls.filter((c) => c.cmd === 'pane.send_text').length, 0, 'Enter reached an agent');
  assert.equal(root.querySelector('[data-row="b"]'), null, 'the stopped agent is still in the rail');
  assert.deepEqual(tilesOf(root), ['a']);
  assert.equal(calls.filter((c) => c.cmd === 'pane.list' && c.ended).length, 0, 'a stopped agent was looked up as ended');
  assert.equal(root.querySelector('[data-ended="b"]'), null, 'a stopped agent left an ended line');
  // The next list still holds it until the server drops it: it stays gone.
  floor.paint([p('a', 'working'), p('b', 'working')]);
  assert.deepEqual(tilesOf(root), ['a']);
  assert.equal(element('status').textContent, 'Stopped b.');
});

// The stopped process reports done before the close is answered, and the
// ended list may still hold it for a moment. A stop is not an ending: no
// ended line, and no lookup.
test('an agent being stopped that reports done leaves no ended line', async () => {
  let answer;
  const { root, floor, calls } = stub({
    panes: [p('a', 'working'), p('b', 'working')],
    width: 1000,
    rpc: (cmd, f) => {
      if (cmd === 'pane.close') return new Promise((r) => { answer = r; });
      if (cmd === 'pane.list' && f && f.ended) return { panes: [{ id: 'a', label: 'a', exit_code: 143, ended_at: 1 }] };
      return {};
    },
  });
  assert.equal(floor.typing(), 'a');
  click(root.querySelector('[data-tile="a"] .head .kill'));
  key('Enter');
  floor.onEvent({ event: 'state', pane: 'a', state: 'done', source: 'process', ts: 2 });
  floor.paint([p('b', 'working')]);
  await drain();
  answer({ pane: 'a', closed: true });
  await drain();
  assert.equal(root.querySelector('[data-ended="a"]'), null, 'a stopped agent shows an ended line');
  assert.equal(calls.filter((c) => c.cmd === 'pane.list' && c.ended).length, 0, 'a stopped agent was looked up');
  assert.deepEqual(tilesOf(root), ['b']);
  assert.equal(element('status').textContent, 'Stopped a.');
});

test('Esc keeps the agent, and while typing in a window Esc never reaches it', async () => {
  const { root, floor, calls } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 1000 });
  assert.equal(floor.typing(), 'a');
  click(root.querySelector('[data-tile="a"] .head .kill'));
  assert.equal(floor.killPending(), 'a');
  const esc = key('Escape');
  assert.ok(esc.prevented);
  await drain();
  assert.equal(floor.killPending(), '');
  assert.equal(calls.filter((c) => c.cmd === 'pane.close').length, 0);
  assert.equal(calls.filter((c) => c.cmd === 'pane.send_text').length, 0, 'Esc reached the agent');
  assert.equal(root.querySelector('[data-tile="a"]').querySelector('.confirm'), null);
});

test('Keep keeps and Stop stops, by mouse', async () => {
  const { root, floor, calls } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 1000 });
  click(root.querySelector('[data-row="b"] .kill'));
  click(root.querySelector('[data-row="b"] .keep'));
  assert.equal(floor.killPending(), '');
  click(root.querySelector('[data-row="b"] .kill'));
  click(root.querySelector('[data-row="b"] .stop'));
  await drain();
  assert.deepEqual(calls.filter((c) => c.cmd === 'pane.close').map((c) => c.pane), ['b']);
});

test('on the rail Delete asks to stop the selected agent', () => {
  const { floor } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 1000 });
  key(' ', { ctrlKey: true, code: 'Space' });
  key('ArrowDown');
  key('Delete');
  assert.equal(floor.killPending(), 'b');
});

test('a refused stop says why and keeps the agent', async () => {
  const { root, floor } = stub({
    panes: [p('a', 'working'), p('b', 'working')],
    width: 1000,
    rpc: (cmd) => { if (cmd === 'pane.close') throw new Error('no pane "b". Run: coppice pane list'); return {}; },
  });
  click(root.querySelector('[data-row="b"] .kill'));
  key('Enter');
  await drain();
  assert.equal(element('status').textContent, 'no pane "b". Run: coppice pane list');
  assert.ok(root.querySelector('[data-row="b"]'));
  assert.equal(floor.killPending(), '');
});

const dragData = (pane) => ({
  types: [ROW_TYPE, 'text/plain'],
  getData: (t) => (t === ROW_TYPE ? pane : ''),
});

test('a row dragged onto a window puts its agent there with the keys', () => {
  const { root, floor } = stub({ panes: [p('a', 'working'), p('b', 'working'), p('c', 'working')], width: 1000 });
  assert.deepEqual(tilesOf(root), ['a', 'b']);
  const row = root.querySelector('[data-row="c"]');
  assert.equal(row.draggable, true);
  const set = {};
  row.dispatchEvent({ type: 'dragstart', dataTransfer: { setData: (t, v) => { set[t] = v; } } });
  assert.equal(set[ROW_TYPE], 'c');
  const win = root.querySelector('[data-tile="b"]');
  let over = 0;
  win.dispatchEvent({ type: 'dragover', dataTransfer: dragData('c'), preventDefault() { over += 1; } });
  assert.equal(over, 1, 'the window refused the row');
  win.dispatchEvent({ type: 'drop', dataTransfer: dragData('c'), preventDefault() {} });
  assert.deepEqual(tilesOf(root), ['a', 'c']);
  assert.equal(floor.typing(), 'c');
});

test('a drag of anything else is not taken by a window', () => {
  const { root } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 1000 });
  let over = 0;
  root.querySelector('[data-tile="b"]').dispatchEvent({ type: 'dragover', dataTransfer: { types: ['text/plain'] }, preventDefault() { over += 1; } });
  assert.equal(over, 0);
});

test('F2 renames the selected row in place: Enter saves, Esc keeps the old name', async () => {
  const { root, floor, calls } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 1000 });
  key(' ', { ctrlKey: true, code: 'Space' });
  key('ArrowDown');
  key('F2');
  assert.deepEqual(floor.editing(), { pane: 'b', where: 'row' });
  const input = root.querySelector('[data-row="b"] .rename');
  assert.ok(input, 'no field in the row');
  assert.equal(input.value, 'b');
  // A fresh list while the field is open does not draw the rail again.
  render(paneRows([p('a', 'working'), p('b', 'working')], 1));
  assert.equal(root.querySelector('[data-row="b"] .rename'), input, 'a redraw took the field away');
  input.value = '  docs writer ';
  input.dispatchEvent({ type: 'keydown', key: 'Enter', preventDefault() {}, stopPropagation() {} });
  await drain();
  assert.deepEqual(calls.filter((c) => c.cmd === 'pane.rename'), [{ cmd: 'pane.rename', pane: 'b', label: 'docs writer' }]);
  assert.equal(floor.editing(), null);
  assert.equal(root.querySelector('[data-row="b"] .label').textContent, 'docs writer');
  key('F2');
  const again = root.querySelector('[data-row="b"] .rename');
  again.value = 'other';
  again.dispatchEvent({ type: 'keydown', key: 'Escape', preventDefault() {}, stopPropagation() {} });
  await drain();
  assert.equal(calls.filter((c) => c.cmd === 'pane.rename').length, 1, 'Esc saved');
});

// In the app the rail draws from the roster's own copy of the list. The
// new name shows there at once, not only after the next refresh.
test('a saved name shows in a rail drawn from the page list', async () => {
  const panes = [p('a', 'working'), p('b', 'working')];
  reset();
  global.window.innerWidth = 1000;
  const root = element('screen-roster');
  root.append(element('queue-head'), element('roster'), element('roster-empty'));
  element('queue-head').append(element('queue-title'), element('queue-sub'));
  const state = {
    panes, width: 1000, rpc: async () => ({}), api: async () => ({}),
    status: () => {}, rail: () => render(paneRows(panes, 1)),
  };
  const floor = mountFloor(root, state);
  render(paneRows(panes, 1));
  floor.startRename('b', 'row');
  const input = root.querySelector('[data-row="b"] .rename');
  input.value = 'bee';
  input.dispatchEvent({ type: 'keydown', key: 'Enter', preventDefault() {}, stopPropagation() {} });
  assert.equal(root.querySelector('[data-row="b"] .label').textContent, 'bee');
});

test('a double click on a window label renames it there', async () => {
  const { root, floor, calls } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 1000 });
  const label = root.querySelector('[data-tile="b"] .head .label');
  label.dispatchEvent({ type: 'dblclick', stopPropagation() {} });
  assert.deepEqual(floor.editing(), { pane: 'b', where: 'head' });
  const input = root.querySelector('[data-tile="b"] .head .rename');
  assert.ok(input, 'no field in the header');
  floor.onEvent({ event: 'state', pane: 'b', state: 'idle', source: 'process', ts: 3 });
  assert.equal(root.querySelector('[data-tile="b"] .head .rename'), input, 'a state event took the field away');
  input.value = 'bee';
  input.dispatchEvent({ type: 'blur' });
  await drain();
  assert.deepEqual(calls.filter((c) => c.cmd === 'pane.rename').map((c) => c.label), ['bee']);
  assert.equal(root.querySelector('[data-tile="b"] .head .label').textContent, 'bee');
});

test('Recent lists the ended agents with Resume and Forget, and hides with none', () => {
  reset();
  element('recent');
  element('recent-list');
  element('recent-count');
  const done = [];
  const act = { resume: (r) => done.push(['resume', r.id]), forget: (r) => done.push(['forget', r.id]) };
  renderRecent([], act, 100);
  assert.equal(element('recent').hidden, true);
  renderRecent([
    { id: 'x', label: 'sh-x', cwd: '/src/one', exit_code: 3, ended_at: 40 },
    { id: 'y', label: 'sh-y', cwd: '/src/two', exit_code: 0, ended_at: 10 },
  ], act, 100);
  assert.equal(element('recent').hidden, false);
  assert.equal(element('recent-count').textContent, '2');
  const rows = element('recent-list').children;
  assert.equal(rows[0].textContent.includes('one · exit 3 · 1m ago'), true);
  assert.ok(rows[0].className.includes('amber'));
  assert.ok(!rows[1].className.includes('amber'));
  click(rows[0].querySelector('.resume'));
  click(rows[1].querySelector('.forget'));
  assert.deepEqual(done, [['resume', 'x'], ['forget', 'y']]);
});

test('opening Recent clears the ended lines', async () => {
  const { root, floor } = stub({
    panes: [p('a', 'working'), p('b', 'working')],
    width: 1000,
    rpc: (cmd, f) => (cmd === 'pane.list' && f && f.ended ? { panes: [{ id: 'b', label: 'b', exit_code: 2, ended_at: 1 }] } : {}),
  });
  floor.paint([p('a', 'working')]);
  await drain();
  assert.ok(root.querySelector('[data-ended="b"]'));
  floor.seenRecent();
  assert.equal(root.querySelector('[data-ended="b"]'), null);
});

// The booted page reads the ended list beside the live one and fills
// Recent, and Resume all and Clear all call the server.
test('a booted page fills Recent, and Resume all and Clear all reach the server', async () => {
  reset();
  global.localStorage.setItem('coppice.token', TOKEN);
  for (const id of ['recent', 'recent-list', 'recent-count', 'recent-resume-all', 'recent-clear', 'recent-confirm', 'roster', 'roster-empty']) element(id);
  setFetch(async () => ({ ok: true, status: 200, json: async () => ({ panes: [] }) }));
  await freshApp();
  await settle();
  const ws = lastSocket();
  const sent = [];
  ws.send = (data) => {
    const m = JSON.parse(data);
    sent.push(m);
    let result = {};
    if (m.cmd === 'pane.list') result = { panes: m.ended ? [{ id: 'x', label: 'sh-x', cwd: '/w', exit_code: 1, ended_at: 1 }] : [] };
    if (m.cmd === 'task.list') result = { tasks: [] };
    if (m.cmd === 'pane.resume') result = { pane: 'n1', resumed: true };
    if (m.cmd === 'pane.forget') result = { forgot: 1 };
    setImmediate(() => ws.fire('message', { data: JSON.stringify({ id: m.id, ok: true, result }) }));
  };
  ws.simulateOpen();
  await drain();
  assert.ok(sent.some((m) => m.cmd === 'pane.list' && m.ended === true), 'Recent was never read');
  assert.equal(element('recent').hidden, false);
  assert.equal(element('recent-count').textContent, '1');
  click(element('recent-resume-all'));
  await drain();
  assert.deepEqual(sent.filter((m) => m.cmd === 'pane.resume').map((m) => m.pane), ['x']);
  // Clear all asks first, and Keep or Esc keeps the agents.
  click(element('recent-clear'));
  await drain();
  const box = element('recent-confirm');
  assert.equal(box.hidden, false);
  assert.ok(box.textContent.includes('Forget 1 ended agent? Enter forgets it, Esc keeps it.'), box.textContent);
  assert.ok(!sent.some((m) => m.cmd === 'pane.forget'), 'Clear all forgot before the owner said so');
  click(box.querySelectorAll('button').find((b) => b.textContent === 'Keep'));
  await drain();
  assert.equal(box.hidden, true);
  assert.ok(!sent.some((m) => m.cmd === 'pane.forget'), 'Keep forgot the agents');
  click(element('recent-clear'));
  box.dispatchEvent({ type: 'keydown', key: 'Escape' });
  assert.equal(box.hidden, true);
  click(element('recent-clear'));
  click(box.querySelectorAll('button').find((b) => b.textContent === 'Forget'));
  await drain();
  assert.ok(sent.some((m) => m.cmd === 'pane.forget' && m.ended === true));
  assert.equal(element('status').textContent, 'Forgot 1 ended agent.');
});

function newStub(reply) {
  reset();
  for (const id of ['go-new', 'new-menu-btn', 'new-menu']) element(id);
  element('new-menu').hidden = true;
  const calls = [];
  const made = [];
  const said = [];
  const went = [];
  let near = 'w1:p2';
  const api = mountNewButton({
    rpc: async (cmd, fields) => {
      calls.push({ cmd, ...fields });
      return reply(cmd, fields);
    },
    status: (m) => said.push(m),
    near: () => near,
    made: (pane) => made.push(pane),
    go: (h) => went.push(h),
  });
  return { calls, made, said, went, api, setNear: (v) => { near = v; } };
}

test('New starts the default harness near the selected agent in one click', async () => {
  const s = newStub((cmd) => (cmd === 'pane.create' ? { pane: 'w1:p9' } : {}));
  click(element('go-new'));
  await drain();
  assert.deepEqual(s.calls, [{ cmd: 'pane.create', kind: 'pty', near: 'w1:p2' }]);
  assert.deepEqual(s.made, ['w1:p9']);
  s.setNear('');
  click(element('go-new'));
  await drain();
  assert.deepEqual(s.calls[1], { cmd: 'pane.create', kind: 'pty' }, 'with nothing selected New sends no near');
});

test('the project menu lists pinned projects first and starts the default harness in the one chosen', async () => {
  const s = newStub((cmd) => {
    if (cmd === 'project.list') {
      return {
        default: 'claude',
        projects: [{ path: '/r/recent', name: 'recent', pinned: false }, { path: '/p/pinned', name: 'pinned', pinned: true }],
      };
    }
    if (cmd === 'pane.create') return { pane: 'w1:p7' };
    return {};
  });
  click(element('new-menu-btn'));
  await drain();
  const items = element('new-menu').children;
  assert.equal(element('new-menu').hidden, false);
  assert.deepEqual(items.map((b) => b.querySelector('.name').textContent), ['pinned', 'recent', 'More...']);
  click(items[0]);
  await drain();
  assert.deepEqual(s.calls.find((c) => c.cmd === 'pane.create'), { cmd: 'pane.create', kind: 'pty', cwd: '/p/pinned', harness: 'claude' });
  assert.deepEqual(s.made, ['w1:p7']);
  assert.equal(element('new-menu').hidden, true);
  click(element('new-menu-btn'));
  await drain();
  click(element('new-menu').children[2]);
  assert.deepEqual(s.went, ['#/new'], 'More did not open the form');
});

test('the project menu with no default harness says what to do', async () => {
  const s = newStub((cmd) => (cmd === 'project.list' ? { default: '', projects: [{ path: '/p/a', name: 'a', pinned: true }] } : {}));
  click(element('new-menu-btn'));
  await drain();
  click(element('new-menu').children[0]);
  await drain();
  assert.equal(s.calls.filter((c) => c.cmd === 'pane.create').length, 0);
  assert.deepEqual(s.said, [NO_DEFAULT]);
});

test('a new agent takes a window with the keys once it lists', () => {
  const { floor, root } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 1000 });
  floor.want('c');
  floor.paint([p('a', 'working'), p('b', 'working'), p('c', 'idle')]);
  assert.equal(floor.typing(), 'c');
  assert.ok(tilesOf(root).includes('c'));
});

test('New starts near the agent being typed into, else the selected row', () => {
  const { floor } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 1000 });
  assert.equal(floor.selectedPane(), 'a');
  key(' ', { ctrlKey: true, code: 'Space' });
  key('ArrowDown');
  assert.equal(floor.selectedPane(), 'b');
});

// A kill that waits for its confirm takes Enter only from the page, a rail
// row or its own buttons. Enter on a menu item is the menu's.
test('Enter on a menu item while a kill waits is the menu item\'s', async () => {
  const { floor, calls } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 1000 });
  floor.askKill('a');
  const target = { tagName: 'BUTTON', closest: (s) => (s === '[role="menu"]' ? {} : null) };
  const enter = key('Enter', { target });
  await drain();
  assert.equal(calls.filter((c) => c.cmd === 'pane.close').length, 0, 'Enter on a menu item stopped an agent');
  assert.equal(enter.prevented, false, 'the menu item lost its Enter');
});

test('a click anywhere outside the confirm keeps the agent, even one that stops its own click', () => {
  const { floor } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 1000 });
  floor.askKill('a');
  global.document.dispatchEvent({ type: 'click', target: { closest: (s) => (s === '[data-confirm]' ? {} : null) } });
  assert.equal(floor.killPending(), 'a', 'a click on the confirm cancelled it');
  global.document.dispatchEvent({ type: 'click', target: { closest: () => null } });
  assert.equal(floor.killPending(), '');
});

// A window keeps its place when the windows are drawn again, so a drop
// reads the window's place when it lands.
test('a row dropped after a header swap lands in the window it was dropped on', () => {
  const { root } = stub({ panes: [p('a', 'working'), p('b', 'working'), p('c', 'working')], width: 1000 });
  assert.deepEqual(tilesOf(root), ['a', 'b']);
  root.querySelector('[data-tile="b"] .head').dispatchEvent({ type: 'dragstart', dataTransfer: { setData() {} } });
  root.querySelector('[data-tile="a"]').dispatchEvent({ type: 'drop', dataTransfer: { types: ['text/plain'], getData: () => '' }, preventDefault() {} });
  assert.deepEqual(tilesOf(root), ['b', 'a']);
  root.querySelector('[data-tile="b"]').dispatchEvent({ type: 'drop', dataTransfer: dragData('c'), preventDefault() {} });
  assert.deepEqual(tilesOf(root), ['c', 'a'], 'the row landed in another window');
});

// bootApp starts the app on the stub socket. reply(m) is a result, or an
// Error for a refusal.
async function bootApp(reply) {
  reset();
  global.localStorage.setItem('coppice.token', TOKEN);
  for (const id of ['recent', 'recent-list', 'recent-count', 'recent-resume-all', 'recent-clear', 'roster', 'roster-empty', 'screen-roster']) element(id);
  setFetch(async () => ({ ok: true, status: 200, json: async () => ({ panes: [] }) }));
  await freshApp();
  await settle();
  const ws = lastSocket();
  const sent = [];
  ws.send = (data) => {
    const m = JSON.parse(data);
    sent.push(m);
    const r = reply(m);
    const frame = r instanceof Error
      ? { id: m.id, ok: false, error: { code: 'bad_request', message: r.message } }
      : { id: m.id, ok: true, result: r || {} };
    setImmediate(() => ws.fire('message', { data: JSON.stringify(frame) }));
  };
  ws.simulateOpen();
  await drain();
  return { ws, sent };
}

test('Resume all says how many resumed and why one failed, and a second click waits', async () => {
  const ended = [{ id: 'x', label: 'sh-x', exit_code: 1, ended_at: 1 }, { id: 'y', label: 'sh-y', exit_code: 1, ended_at: 1 }];
  const { sent } = await bootApp((m) => {
    if (m.cmd === 'pane.list') return { panes: m.ended ? ended : [] };
    if (m.cmd === 'task.list') return { tasks: [] };
    if (m.cmd === 'pane.resume') return m.pane === 'y' ? new Error('spawn failed: no such file') : { pane: 'n1', resumed: true };
    return {};
  });
  click(element('recent-resume-all'));
  click(element('recent-resume-all'));
  await drain();
  await drain();
  assert.deepEqual(sent.filter((m) => m.cmd === 'pane.resume').map((m) => m.pane), ['x', 'y'], 'a second click resumed again');
  assert.equal(element('status').textContent, 'Resumed 1 of 2. spawn failed: no such file');
});

test('a state event redraws the rail row at once, before the next list', async () => {
  const live = [{ id: 'a', label: 'a', state: 'idle', cwd: '/w', ts: 1 }];
  const { ws } = await bootApp((m) => {
    if (m.cmd === 'pane.list') return { panes: m.ended ? [] : live };
    if (m.cmd === 'task.list') return { tasks: [] };
    return {};
  });
  assert.ok(element('roster').querySelector('[data-row="a"]').querySelector('.dot-idle'));
  ws.fire('message', { data: JSON.stringify({ event: 'state', pane: 'a', state: 'working', source: 'process', ts: 2 }) });
  assert.ok(element('roster').querySelector('[data-row="a"]').querySelector('.dot-working'), 'the rail dot waited for the next list');
});

test('the ended list is read after an agent ends, not on every refresh', async () => {
  const { ws, sent } = await bootApp((m) => {
    if (m.cmd === 'pane.list') return { panes: m.ended ? [] : [{ id: 'a', label: 'a', state: 'idle', cwd: '/w', ts: 1 }] };
    if (m.cmd === 'task.list') return { tasks: [] };
    return {};
  });
  const endedReads = () => sent.filter((m) => m.cmd === 'pane.list' && m.ended === true).length;
  assert.equal(endedReads(), 1);
  global.window.dispatchEvent(new CustomEvent('coppice:refresh'));
  await drain();
  assert.equal(endedReads(), 1, 'a refresh read the ended list again');
  // The floor looks the agent up once itself; the next refresh reads the
  // list for Recent.
  ws.fire('message', { data: JSON.stringify({ event: 'state', pane: 'a', state: 'done', source: 'process', ts: 3 }) });
  await drain();
  const before = endedReads();
  global.window.dispatchEvent(new CustomEvent('coppice:refresh'));
  await drain();
  assert.equal(endedReads(), before + 1, 'an agent that ended did not read the ended list');
});

test('the rail keeps its rows when nothing changed', () => {
  const { root } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 1000 });
  const first = root.querySelector('[data-row="a"]');
  render(paneRows([p('a', 'working'), p('b', 'working')], 1));
  assert.equal(root.querySelector('[data-row="a"]'), first, 'the rail was rebuilt with nothing changed');
  render(paneRows([p('a', 'idle'), p('b', 'working')], 1));
  assert.notEqual(root.querySelector('[data-row="a"]'), first);
});

test('while a row is renamed the rest of the rail still draws', () => {
  const { root, floor } = stub({ panes: [p('a', 'working'), p('b', 'working')], width: 1000 });
  floor.startRename('b', 'row');
  const input = root.querySelector('[data-row="b"] .rename');
  render(paneRows([p('a', 'working'), p('b', 'working'), p('c', 'idle')], 1));
  assert.ok(root.querySelector('[data-row="c"]'), 'a new agent did not draw while a row was renamed');
  assert.equal(root.querySelector('[data-row="b"] .rename'), input, 'the field was rebuilt');
});

test('a failed project list says why in the menu', async () => {
  newStub((cmd) => { if (cmd === 'project.list') throw new Error('Not connected. Check the token in Settings.'); return {}; });
  click(element('new-menu-btn'));
  await drain();
  const text = element('new-menu').textContent;
  assert.ok(text.includes('Not connected. Check the token in Settings.'), text);
  assert.ok(!text.includes('No projects yet'));
});

test('the empty rail names New', () => {
  stub({ panes: [], width: 1000 });
  assert.equal(element('roster-empty').textContent, 'No agents yet. Press New to start one.');
});

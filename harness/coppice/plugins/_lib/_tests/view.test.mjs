import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import {
  connect, selection, stateColor, stateWord, STATE_COLORS, age, byNeed, WATCH_MAX,
} from '../view.js';
import { ageLabel, sortPanes } from '../../../internal/web/static/roster.js';

// fakeWindow is a framed page: a parent that records each post, and a
// message listener list a test can fire.
function fakeWindow() {
  const posted = [];
  const listeners = [];
  const parent = { postMessage: (msg, origin) => posted.push({ msg, origin }) };
  const keys = [];
  const win = {
    parent,
    addEventListener: (type, fn) => {
      if (type === 'message') listeners.push(fn);
      if (type === 'keydown') keys.push(fn);
    },
  };
  const fire = (source, data) => { for (const fn of listeners) fn({ source, data }); };
  const press = (e) => { for (const fn of keys) fn(e); };
  return { win, parent, posted, fire, press };
}

test('Esc in a view asks the floor to close it, and other keys ask nothing', () => {
  const { win, posted, press } = fakeWindow();
  const view = connect(win);
  press({ key: 'a' });
  press({ key: 'Escape', defaultPrevented: true });
  assert.deepEqual(posted, []);
  press({ key: 'Escape' });
  assert.deepEqual(posted.map((p) => p.msg), [{ type: 'close' }]);
  view.close();
  assert.equal(posted.length, 2);
});

test('selection.read parses the sel of a view hash', () => {
  assert.deepEqual(selection.read('#/view/x?sel=a,b'), ['a', 'b']);
  assert.deepEqual(selection.read('#/view/x?sel=w1%3Ap1'), ['w1:p1']);
  assert.deepEqual(selection.read('#/view/x'), []);
  assert.deepEqual(selection.read(''), []);
});

test('selection.write rewrites the hash and keeps the view id', () => {
  assert.equal(selection.write('#/view/x?sel=a', ['b', 'c']), '#/view/x?sel=b,c');
  assert.equal(selection.write('#/view/x', ['w1:p1']), '#/view/x?sel=w1%3Ap1');
  assert.equal(selection.write('#/view/x?sel=a', []), '#/view/x');
});

test('stateColor gives the five roster colours and throws on anything else', () => {
  const css = readFileSync(new URL('../../../internal/web/static/app.css', import.meta.url), 'utf8');
  for (const word of ['blocked', 'working', 'idle', 'done', 'unknown']) {
    const m = new RegExp('--' + word + ':\\s*(#[0-9a-fA-F]{6})').exec(css);
    assert.ok(m, 'app.css has no --' + word);
    assert.equal(stateColor(word), m[1]);
  }
  assert.equal(stateColor('blocked'), '#d98b4a');
  assert.equal(Object.keys(STATE_COLORS).length, 5);
  for (const bad of ['nonsense', '', undefined, null, 'Blocked']) {
    assert.throws(() => stateColor(bad));
  }
});

test('stateWord reads a state the roster does not know as unknown', () => {
  assert.equal(stateWord('blocked'), 'blocked');
  assert.equal(stateWord(''), 'unknown');
  assert.equal(stateWord(undefined), 'unknown');
  assert.equal(stateWord('paused'), 'unknown');
  assert.equal(stateColor(stateWord('paused')), STATE_COLORS.unknown);
});

test('age and byNeed read as the roster reads', () => {
  for (const s of [0, 59, 60, 3599, 3600, 86399, 86400, -4, 12.7]) assert.equal(age(s), ageLabel(s));
  const panes = [
    { id: 'a', state: 'done' }, { id: 'b', state: 'blocked' }, { id: 'c', state: 'working' },
    { id: 'd', state: 'odd' }, { id: 'e', state: 'blocked' },
  ];
  assert.deepEqual(byNeed(panes).map((p) => p.id), sortPanes(panes).map((p) => p.id));
});

test('connect takes data only from the parent frame', () => {
  const { win, parent, fire } = fakeWindow();
  const view = connect(win);
  const seen = [];
  view.onData((d) => seen.push(d));
  fire({}, { type: 'coppice.data', panes: [{ id: 'x' }], tasks: [], sel: [], events: [] });
  assert.equal(seen.length, 0);
  assert.deepEqual(view.panes(), []);
  fire(parent, { type: 'coppice.data', panes: [{ id: 'p1' }], tasks: [{ id: 't1' }], sel: ['p1'], events: [{ event: 'state' }] });
  assert.equal(seen.length, 1);
  assert.deepEqual(view.panes(), [{ id: 'p1' }]);
  assert.deepEqual(view.tasks(), [{ id: 't1' }]);
  assert.deepEqual(view.sel(), ['p1']);
  assert.deepEqual(view.events(), [{ event: 'state' }]);
  fire(parent, { type: 'coppice.data', panes: 'bad', tasks: null });
  assert.deepEqual(view.panes(), []);
  assert.deepEqual(view.sel(), []);
});

test('connect hands frames of watched panes to onFrame', () => {
  const { win, parent, fire } = fakeWindow();
  const view = connect(win);
  const frames = [];
  view.onFrame((f) => frames.push(f.pane));
  fire(parent, { type: 'coppice.frame', pane: 'p1', lines: ['hi'] });
  fire({}, { type: 'coppice.frame', pane: 'p2', lines: ['no'] });
  fire(parent, { type: 'coppice.frame', lines: ['no pane'] });
  assert.deepEqual(frames, ['p1']);
});

test('a view posts only open-pane, set-sel and watch, to its parent', () => {
  const { win, posted } = fakeWindow();
  const view = connect(win);
  view.openPane('p1');
  view.setSel(['p1', 'p2']);
  view.watch(['a', 'b', 'c', 'd', 'e', 'f', 'g', 'h', 'i', 'j', 'k']);
  assert.deepEqual(posted.map((p) => p.msg.type), ['open-pane', 'set-sel', 'watch']);
  assert.deepEqual(posted[0].msg, { type: 'open-pane', pane: 'p1' });
  assert.deepEqual(posted[1].msg, { type: 'set-sel', sel: ['p1', 'p2'] });
  assert.equal(posted[2].msg.panes.length, WATCH_MAX);
  assert.equal(WATCH_MAX, 9);
  for (const p of posted) assert.equal(p.origin, '*');
});

test('a page that is not framed posts nothing', () => {
  const win = { addEventListener: () => {} };
  win.parent = win;
  const view = connect(win);
  assert.equal(view.framed, false);
  assert.doesNotThrow(() => view.openPane('p1'));
});

test('the library holds no token and asks the server for nothing', () => {
  const src = readFileSync(new URL('../view.js', import.meta.url), 'utf8');
  for (const word of ['localStorage', 'fetch(', 'WebSocket', 'coppice.token', 'XMLHttpRequest']) {
    assert.ok(!src.includes(word), 'view.js uses ' + word);
  }
});

test('connect reads from, and null when the floor does not know it', () => {
  const { win, parent, fire } = fakeWindow();
  const view = connect(win);
  fire(parent, { type: 'coppice.data', panes: [], events: [], from: 12.5 });
  assert.equal(view.from(), 12.5);
  fire(parent, { type: 'coppice.data', panes: [], events: [], from: null });
  assert.equal(view.from(), null);
  fire(parent, { type: 'coppice.data', panes: [], events: [] });
  assert.equal(view.from(), null);
});

test('connect hands the watch state to onWatch', () => {
  const { win, parent, fire } = fakeWindow();
  const view = connect(win);
  const seen = [];
  view.onWatch((st) => seen.push(st));
  fire(parent, { type: 'coppice.watch', live: ['a'], refused: ['b'] });
  fire({}, { type: 'coppice.watch', live: ['x'], refused: [] });
  assert.deepEqual(seen, [{ live: ['a'], refused: ['b'] }]);
});

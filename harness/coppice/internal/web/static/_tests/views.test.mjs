import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { reset, setFetch, element, TOKEN, freshApp, lastSocket } from './browser-stub.mjs';
import { route } from '../app.js';
import { viewHash, viewSrc, viewAction, mountViews, mountViewHost, WATCH_MAX } from '../views.js';
import { treeLines, markSelected, mountTree } from '../../../../plugins/tree/tree.js';

const fixture = JSON.parse(readFileSync(new URL('./fixtures/tree.json', import.meta.url), 'utf8'));
const PANES = [
  { id: 'w1:p1', label: 'auth', state: 'working', started: 1, harness: 'claude' },
  { id: 'w2:p1', label: 'docs', state: 'idle', started: 2, harness: 'claude' },
];
const TASKS = [{ id: 't1', label: 'auth', state: 'working', panes: ['w1:p1'] }];

const tick = () => new Promise((r) => setImmediate(r));

// lastData is the newest coppice.data post. Each entry is a message, or
// { msg } as bootAt records it.
function lastData(posted) {
  const msgs = posted.map((p) => (p && p.msg ? p.msg : p)).filter((m) => m.type === 'coppice.data');
  return msgs[msgs.length - 1];
}

test('route reads a view and the panes it carries', () => {
  assert.deepEqual(route('#/view/tree?sel=w1%3Ap1,w2%3Ap1'), { screen: 'view', view: 'tree', sel: ['w1:p1', 'w2:p1'] });
  assert.deepEqual(route('#/view/tree'), { screen: 'view', view: 'tree', sel: [] });
  assert.deepEqual(route('#/view/'), { screen: 'roster' });
});

test('viewHash carries the selection; the frame address never does', () => {
  assert.equal(viewHash('tree', ['w1:p1', 'w2:p1']), '#/view/tree?sel=w1%3Ap1,w2%3Ap1');
  assert.equal(viewHash('tree', []), '#/view/tree');
  assert.equal(viewSrc('tree'), '/plugins/tree/');
});

test('the tree view draws the same lines the terminal floor draws', () => {
  const lines = treeLines(fixture.tasks, fixture.panes).map((l) => l.text);
  assert.deepEqual(lines, fixture.lines);
});

test('viewAction takes open-pane and set-sel, each checked against the pane list, and close', () => {
  assert.deepEqual(viewAction({ type: 'close' }, PANES), { kind: 'close' });
  assert.deepEqual(viewAction({ type: 'open-pane', pane: 'w2:p1' }, PANES), { kind: 'open', pane: 'w2:p1' });
  assert.equal(viewAction({ type: 'open-pane', pane: 'ghost' }, PANES), null);
  assert.deepEqual(viewAction({ type: 'set-sel', sel: ['w2:p1', 'ghost', 7] }, PANES), { kind: 'sel', sel: ['w2:p1'] });
  assert.equal(viewAction({ type: 'set-sel', sel: 'w2:p1' }, PANES), null);
  for (const bad of [null, 'open-pane', { type: 'agent.allow', pane: 'w1:p1' }, { type: 'send', text: 'y' }]) {
    assert.equal(viewAction(bad, PANES), null);
  }
});

test('a refused view list stays hidden', async () => {
  reset();
  const list = element('view-list');
  const views = mountViews(list, { api: async () => { throw new Error('Bad token.'); }, selection: () => [], go: () => {} });
  await views.load();
  assert.equal(list.hidden, true);
});

// bootAt opens the app at hash with a floor that knows PANES and TASKS and
// one view, and a frame whose window records what the floor posts.
async function bootAt(hash, views = [{ id: 'tree', title: 'Tree' }]) {
  reset();
  global.localStorage.setItem('coppice.token', TOKEN);
  global.location.hash = hash;
  const posted = [];
  const frame = element('view-frame');
  frame.contentWindow = { postMessage: (msg, origin) => posted.push({ msg, origin }) };
  const mini = element('minimap-frame');
  mini.contentWindow = { postMessage: (msg, origin) => mini.posted.push({ msg, origin }) };
  mini.posted = [];
  setFetch(async (path) => ({
    ok: true,
    status: 200,
    json: async () => {
      if (path === '/api/views') return { views };
      if (path.startsWith('/api/events')) return { events: [{ event: 'state', pane: 'w1:p1', state: 'working', ts: 9 }], from: 7 };
      if (path === '/api/tasks') return { tasks: TASKS };
      return { panes: PANES };
    },
  }));
  await freshApp();
  for (let i = 0; i < 5; i++) await tick();
  return { frame, posted, mini };
}

// The failure this names: a switch that drops the panes the tiles show
// makes the operator find them again by hand. It runs through the real
// floor, so a selection that stopped reading the tiles would fail here.
test('switching to the tree with two tiles filled carries both panes', async () => {
  await bootAt('#/roster');
  const button = element('view-list').querySelector('[data-view="tree"]');
  assert.ok(button, 'the rail shows no Tree button');
  const { frame, posted } = { frame: element('view-frame'), posted: [] };
  frame.contentWindow = { postMessage: (msg) => posted.push(msg) };
  button.dispatchEvent({ type: 'click' });
  for (let i = 0; i < 5; i++) await tick();
  assert.equal(global.location.hash, '#/roster', 'a view button left the floor');
  assert.equal(element('overlay').hidden, false, 'the view did not open over the floor');
  assert.equal(frame.src, '/plugins/tree/');
  frame.dispatchEvent({ type: 'load' });
  assert.deepEqual([...lastData(posted).sel].sort(), ['w1:p1', 'w2:p1']);
});

test('the floor posts panes, tasks and sel to the frame, and never the token', async () => {
  const { frame, posted } = await bootAt('#/view/tree?sel=w1%3Ap1');
  assert.equal(frame.src, '/plugins/tree/');
  frame.dispatchEvent({ type: 'load' });
  await tick();
  const last = { msg: lastData(posted) };
  assert.ok(last.msg, 'nothing was posted');
  assert.equal(last.msg.type, 'coppice.data');
  assert.deepEqual(last.msg.sel, ['w1:p1']);
  assert.deepEqual(last.msg.panes.map((p) => p.id), ['w1:p1', 'w2:p1']);
  assert.deepEqual(last.msg.tasks, TASKS);
  for (const p of posted) assert.ok(!JSON.stringify(p.msg).includes(TOKEN), 'a post carried the token');
  // The exact key set: a new key, a token among them, fails here.
  for (const p of posted.filter((x) => x.msg.type === 'coppice.data')) {
    assert.deepEqual(Object.keys(p.msg).sort(), ['events', 'from', 'panes', 'sel', 'tasks', 'type', 'view']);
  }
  for (const p of posted.filter((x) => x.msg.type === 'coppice.watch')) {
    assert.deepEqual(Object.keys(p.msg).sort(), ['live', 'refused', 'type', 'view']);
  }
  assert.deepEqual([...new Set(posted.map((x) => x.msg.type))].sort(), ['coppice.data', 'coppice.watch']);
  // The tree asked for no ring, so the floor does not claim to know the history.
  assert.equal(last.msg.from, null);
});

test('the floor posts the events it holds, and never a frame', async () => {
  const { frame, posted } = await bootAt('#/view/tree');
  const hold = (msg) => global.window.coppice.state.onEvent(msg);
  hold({ event: 'state', pane: 'w1:p1', state: 'blocked', ts: 3 });
  hold({ event: 'frame', pane: 'w1:p1', rows: ['secret screen'] });
  hold({ event: 'note', pane: 'w1:p1', text: 'turn-budget › paused', ts: 4 });
  frame.dispatchEvent({ type: 'load' });
  const last = lastData(posted);
  assert.deepEqual(last.events.map((e) => e.event), ['state', 'note']);
  assert.ok(!JSON.stringify(last).includes('secret screen'));
});

test('the floor keeps only the newest events', async () => {
  const { frame, posted } = await bootAt('#/view/tree');
  for (let i = 0; i < 300; i++) global.window.coppice.state.onEvent({ event: 'state', pane: 'w1:p1', state: 'working', ts: i });
  frame.dispatchEvent({ type: 'load' });
  const events = lastData(posted).events;
  assert.equal(events.length, 100);
  assert.equal(events[events.length - 1].ts, 299);
});

test('the floor opens a pane a view names in a window, and only a pane it knows, and only from its frame', async () => {
  const { frame, posted } = await bootAt('#/view/tree');
  assert.equal(global.location.hash, '#/roster', 'a view link stayed a page of its own');
  assert.equal(element('overlay').hidden, false, 'the view did not open over the floor');
  const send = (source, data) => global.window.dispatchEvent({ type: 'message', source, data });
  send({}, { type: 'open-pane', pane: 'w2:p1' });
  assert.equal(element('overlay').hidden, false);
  send(frame.contentWindow, { type: 'open-pane', pane: 'ghost' });
  assert.equal(element('overlay').hidden, false);
  send(frame.contentWindow, { type: 'agent.allow', pane: 'w1:p1' });
  assert.equal(element('overlay').hidden, false);
  const before = posted.length;
  send(frame.contentWindow, { type: 'set-sel', sel: ['w2:p1', 'ghost'] });
  await tick();
  assert.deepEqual(lastData(posted).sel, ['w2:p1']);
  assert.ok(posted.length > before);
  assert.equal(global.location.hash, '#/roster', 'a selection rewrote the address');
  send(frame.contentWindow, { type: 'open-pane', pane: 'w2:p1' });
  assert.equal(global.location.hash, '#/roster', 'opening a pane left the floor');
  assert.equal(element('overlay').hidden, true, 'the overlay stayed over the window it filled');
});

test('closing a view blanks its frame, so the page stops its work', async () => {
  const { frame } = await bootAt('#/view/tree');
  assert.equal(frame.src, '/plugins/tree/');
  element('overlay-close').dispatchEvent({ type: 'click' });
  assert.equal(element('overlay').hidden, true);
  assert.equal(frame.src, 'about:blank');
  assert.equal(frame.dataset.src, '');
  global.location.hash = '#/view/tree';
  global.window.dispatchEvent(new global.CustomEvent('coppice:route'));
  assert.equal(frame.src, '/plugins/tree/');
  // A view asks to close, as Esc in its frame does.
  global.window.dispatchEvent({ type: 'message', source: frame.contentWindow, data: { type: 'close' } });
  assert.equal(element('overlay').hidden, true);
  assert.equal(frame.src, 'about:blank');
  // Leaving the floor closes it too.
  global.location.hash = '#/view/tree';
  global.window.dispatchEvent(new global.CustomEvent('coppice:route'));
  global.location.hash = '#/settings';
  global.window.dispatchEvent(new global.CustomEvent('coppice:route'));
  assert.equal(element('overlay').hidden, true);
  assert.equal(frame.src, 'about:blank');
});

test('Esc closes a view while the rail has the keys, and never while a window types', async () => {
  const { frame } = await bootAt('#/roster');
  const esc = () => global.document.dispatchEvent({ type: 'keydown', key: 'Escape', target: { tagName: 'BODY' }, preventDefault() {} });
  global.location.hash = '#/view/tree';
  global.window.dispatchEvent(new global.CustomEvent('coppice:route'));
  assert.equal(element('overlay').hidden, false);
  // The floor starts with the keys in a window, where Esc is the agent's.
  esc();
  assert.equal(element('overlay').hidden, false, 'Esc for the agent closed the overlay');
  global.document.dispatchEvent({ type: 'keydown', key: ' ', code: 'Space', ctrlKey: true, target: { tagName: 'BODY' }, preventDefault() {} });
  esc();
  assert.equal(element('overlay').hidden, true, 'Esc on the rail left the overlay open');
  assert.equal(frame.src, 'about:blank');
});

test('the tree view renders what the floor posts and marks the selected panes', () => {
  reset();
  const status = element('tree-status');
  const list = element('tree-lines');
  const tree = mountTree(status, list, { post: () => {} });
  assert.equal(status.textContent, 'Open this view from the floor page. It shows what the floor page sends it.');
  tree.render({ type: 'coppice.data', tasks: fixture.tasks, panes: fixture.panes, sel: ['w1:p1'] });
  assert.equal(status.textContent, '');
  assert.equal(list.children.length, fixture.lines.length);
  const marked = list.children.filter((li) => li.className === 'sel');
  assert.equal(marked.length, 1);
  assert.match(marked[0].textContent, /claude/);
  const lines = markSelected(treeLines(fixture.tasks, fixture.panes), ['w1:p1', 'w2:p1']);
  assert.deepEqual(lines.filter((l) => l.selected).map((l) => l.pane), ['w1:p1', 'w2:p1']);
});

test('a click on a pane line asks the floor to open that pane', () => {
  reset();
  const sent = [];
  const tree = mountTree(element('tree-status'), element('tree-lines'), { post: (m) => sent.push(m) });
  tree.render({ type: 'coppice.data', tasks: fixture.tasks, panes: fixture.panes, sel: [] });
  const line = element('tree-lines').children.find((li) => li.dataset.pane === 'w2:p1');
  line.dispatchEvent({ type: 'click' });
  assert.deepEqual(sent, [{ type: 'open-pane', pane: 'w2:p1' }]);
});

test('the tree view holds no token and asks the server for nothing', () => {
  const src = readFileSync(new URL('../../../../plugins/tree/tree.js', import.meta.url), 'utf8');
  for (const word of ['localStorage', 'fetch(', 'WebSocket', 'coppice.token']) {
    assert.ok(!src.includes(word), 'tree.js uses ' + word);
  }
});

test('a window that takes a pane whose view-only attach is on the wire attaches it again once that attach is answered', async () => {
  reset();
  const open = [];
  const reattached = [];
  let held = [];
  const host = mountViewHost(element('view-frame'), {
    api: async (path) => (path === '/api/tasks' ? { tasks: [] } : { panes: PANES }),
    panes: () => PANES, go: () => {}, replace: () => {},
    every: () => 1, cancel: () => {}, later: (fn) => { fn(); return 1; },
    rpc: () => new Promise((resolve) => open.push(resolve)),
    held: () => held,
    settled: () => Promise.resolve(),
    picture: () => null,
    reattach: (panes, after) => { after.then(() => reattached.push(...panes)); },
  });
  element('view-frame').contentWindow = { postMessage: () => {} };
  host.show('herdr-grid', []);
  host.onMessage({ source: element('view-frame').contentWindow, data: { type: 'watch', panes: ['w2:p1'] } });
  for (let i = 0; i < 6; i++) await tick();
  assert.equal(open.length, 1, 'the view-only attach was not sent');
  held = ['w2:p1'];
  host.floorChanged();
  for (let i = 0; i < 6; i++) await tick();
  assert.deepEqual(reattached, [], 'the window attached again before the racing attach was answered');
  open.shift()({});
  for (let i = 0; i < 6; i++) await tick();
  assert.deepEqual(reattached, ['w2:p1']);
});

test('the journal opens over the floor, says where its lines come from, and lists gate verdicts', async () => {
  await bootAt('#/roster');
  const button = element('view-list').querySelector('[data-view=":journal"]');
  assert.ok(button, 'the rail shows no Journal button');
  button.dispatchEvent({ type: 'click' });
  for (let i = 0; i < 5; i++) await tick();
  assert.equal(global.location.hash, '#/roster');
  assert.equal(element('overlay').hidden, false);
  assert.equal(element('overlay-title').textContent, 'Journal');
  assert.equal(element('view-frame').hidden, true);
  const text = element('journal').textContent;
  assert.ok(text.includes('The server keeps no verdict history.'), text);
  global.window.coppice.state.onEvent({ event: 'state', pane: 'w1:p1', source: 'gate', state: 'working', detail: 'verdict=deny clause=net', ts: 50 });
  assert.ok(element('journal').textContent.includes('clause net'), element('journal').textContent);
  // Two reports at one moment with other details are two lines.
  global.window.coppice.state.onEvent({ event: 'state', pane: 'w1:p1', source: 'gate', state: 'working', detail: 'verdict=deny clause=disk', ts: 50 });
  assert.ok(element('journal').textContent.includes('clause disk'), 'a second report at the same moment was dropped');
});

test('the overlay takes the focus when it opens and gives it back when it closes', async () => {
  await bootAt('#/roster');
  const was = element('view-list').querySelector('[data-view="tree"]');
  let back = 0;
  was.focus = () => { back += 1; };
  let took = 0;
  element('overlay-close').focus = () => { took += 1; };
  global.document.activeElement = was;
  was.dispatchEvent({ type: 'click' });
  assert.equal(took, 1, 'the overlay did not take the focus');
  element('overlay-close').dispatchEvent({ type: 'click' });
  assert.equal(back, 1, 'closing did not give the focus back');
  delete global.document.activeElement;
});

test('the view frame, the colony strip and the minimap frame are sandboxed with scripts only', () => {
  const html = readFileSync(new URL('../index.html', import.meta.url), 'utf8');
  for (const id of ['view-frame', 'strip-frame', 'minimap-frame']) {
    const tag = new RegExp('<iframe[^>]*id="' + id + '"[^>]*>').exec(html);
    assert.ok(tag, 'no frame ' + id);
    assert.match(tag[0], /sandbox="allow-scripts"/);
    assert.ok(!tag[0].includes('allow-same-origin'));
  }
});

const WITH_MINIMAP = [{ id: 'tree', title: 'Tree' }, { id: 'minimap', title: 'Minimap' }];

test('with the minimap on, the rail shows it on the floor and a dot click fills a tile', async () => {
  const { mini } = await bootAt('#/roster', WITH_MINIMAP);
  assert.equal(mini.hidden, false);
  // It has its own place, so the rail list gives it no button.
  assert.equal(element('view-list').querySelector('[data-view="minimap"]'), null);
  assert.ok(element('view-list').querySelector('[data-view="tree"]'));
  assert.equal(mini.src, '/plugins/minimap/');
  mini.dispatchEvent({ type: 'load' });
  await tick();
  const last = mini.posted[mini.posted.length - 1];
  assert.ok(last, 'nothing was posted to the minimap');
  assert.equal(last.msg.view, 'minimap');
  assert.deepEqual([...last.msg.sel].sort(), ['w1:p1', 'w2:p1']);
  assert.ok(!JSON.stringify(last.msg).includes(TOKEN));
  const send = (data) => global.window.dispatchEvent({ type: 'message', source: mini.contentWindow, data });
  // A dot click fills a tile on the floor. It does not leave the floor.
  send({ type: 'open-pane', pane: 'w2:p1' });
  assert.equal(global.location.hash, '#/roster');
  // The minimap never moves the page to a view.
  send({ type: 'set-sel', sel: ['w1:p1'] });
  assert.equal(global.location.hash, '#/roster');
});

test('a host with its own open and no select hands the pane over and drops a selection', async () => {
  reset();
  const opened = [];
  const went = [];
  const replaced = [];
  const frame = element('view-frame');
  frame.contentWindow = { postMessage: () => {} };
  const host = mountViewHost(frame, {
    api: async () => ({ panes: PANES, tasks: TASKS }),
    panes: () => PANES, go: (h) => went.push(h), replace: (h) => replaced.push(h),
    every: () => 1, cancel: () => {},
    open: (pane) => opened.push(pane), select: false, sel: () => ['w1:p1'],
  });
  await host.show('minimap', []);
  host.onMessage({ source: frame.contentWindow, data: { type: 'open-pane', pane: 'w2:p1' } });
  host.onMessage({ source: frame.contentWindow, data: { type: 'set-sel', sel: ['w2:p1'] } });
  assert.deepEqual(opened, ['w2:p1']);
  assert.deepEqual(went, []);
  assert.deepEqual(replaced, []);
});

test('the minimap leaves with the floor, and stays hidden when it is off', async () => {
  const { mini } = await bootAt('#/roster', WITH_MINIMAP);
  global.location.hash = '#/settings';
  global.window.dispatchEvent(new global.CustomEvent('coppice:route'));
  assert.equal(mini.hidden, true);
  assert.equal(mini.src, 'about:blank');
  const off = await bootAt('#/roster');
  assert.equal(off.mini.hidden, true);
});

// The failure this names: a shown view that keeps asking with a token the
// server no longer takes earns a ban strike each time, and the ban locks
// the floor page out too.
test('the view refresh stops once the server refuses the token', async () => {
  reset();
  const cancelled = [];
  const host = mountViewHost(element('view-frame'), {
    api: async () => { const e = new Error('Bad token.'); e.status = 401; throw e; },
    panes: () => [], go: () => {}, replace: () => {},
    every: () => 9, cancel: (id) => cancelled.push(id),
  });
  await host.show('tree', []);
  assert.deepEqual(cancelled, [9]);
});

test('the view refresh keeps going after a failure that is not about the token', async () => {
  reset();
  const cancelled = [];
  const host = mountViewHost(element('view-frame'), {
    api: async () => { const e = new Error('no answer'); e.status = 502; throw e; },
    panes: () => [], go: () => {}, replace: () => {},
    every: () => 9, cancel: (id) => cancelled.push(id),
  });
  await host.show('tree', []);
  assert.deepEqual(cancelled, []);
  host.hide();
  assert.deepEqual(cancelled, [9]);
});

// hostWith runs a host over a fake api that answers events from ring and
// records every path it is asked for.
function hostWith(answer, ringFor = (id) => id === 'shift-log') {
  reset();
  const paths = [];
  const posted = [];
  const frame = element('view-frame');
  frame.contentWindow = { postMessage: (msg) => posted.push(msg) };
  let tick = null;
  let flush = null;
  const host = mountViewHost(frame, {
    api: async (path) => { paths.push(path); return answer(path); },
    panes: () => PANES, go: () => {}, replace: () => {},
    every: (fn) => { tick = fn; return 1; }, cancel: () => {},
    later: (fn) => { flush = fn; return 1; },
    ring: ringFor,
  });
  return { host, paths, posted, frame, refresh: () => tick(), flush: () => { const f = flush; flush = null; if (f) f(); } };
}

test('the floor reads the event ring, then asks only for what is new', async () => {
  let ring = [{ event: 'state', pane: 'w1:p1', state: 'working', ts: 10 }];
  const { host, paths, posted, refresh, flush } = hostWith((path) => {
    if (path.startsWith('/api/events')) return { events: ring, from: 5 };
    if (path === '/api/tasks') return { tasks: TASKS };
    return { panes: PANES };
  });
  await host.show('shift-log', []);
  assert.ok(paths.includes('/api/events?since=0'), paths.join(' '));
  assert.deepEqual(lastData(posted).events.map((e) => e.ts), [10]);
  assert.equal(lastData(posted).from, 5);
  // A state event heard on the socket that the ring already holds is not
  // posted twice. A newer one is. Many events make one post.
  const before = posted.length;
  host.onEvent({ event: 'state', pane: 'w1:p1', state: 'working', ts: 10 });
  host.onEvent({ event: 'state', pane: 'w1:p1', state: 'blocked', ts: 12 });
  host.onEvent({ event: 'note', pane: 'w1:p1', text: 'n', ts: 11 });
  assert.equal(posted.length, before, 'an event posted at once');
  flush();
  assert.equal(posted.length, before + 1);
  assert.deepEqual(lastData(posted).events.map((e) => e.ts), [10, 12, 11]);
  ring = [{ event: 'state', pane: 'w1:p1', state: 'blocked', ts: 12 }];
  await refresh();
  assert.equal(paths.filter((p) => p.startsWith('/api/events')).pop(), '/api/events?since=10');
  assert.deepEqual(lastData(posted).events.map((e) => e.ts), [10, 12, 11]);
  for (const msg of posted) {
    assert.deepEqual(Object.keys(msg).sort(), ['events', 'from', 'panes', 'sel', 'tasks', 'type', 'view']);
  }
});

// The failure this names: a refused ring posted as an empty list draws an
// empty hour the socket never reported.
test('a refused event ring is posted as unknown, never as an empty history', async () => {
  let refuse = false;
  const { host, posted, refresh, paths } = hostWith((path) => {
    if (path.startsWith('/api/events')) {
      if (refuse) { const e = new Error('The event ring is not running.'); e.status = 503; throw e; }
      return { events: [{ event: 'state', pane: 'w1:p1', ts: 10 }], from: 4 };
    }
    if (path === '/api/tasks') return { tasks: TASKS };
    return { panes: PANES };
  });
  await host.show('shift-log', []);
  assert.equal(lastData(posted).from, 4);
  refuse = true;
  await refresh();
  const last = lastData(posted);
  assert.equal(last.from, null);
  assert.deepEqual(last.events, []);
  assert.deepEqual(last.tasks, TASKS);
  refuse = false;
  await refresh();
  assert.equal(paths.filter((p) => p.startsWith('/api/events')).pop(), '/api/events?since=0');
  assert.equal(lastData(posted).from, 4);
});

test('a view that did not ask for the ring is never sent it', async () => {
  const { host, posted, paths } = hostWith((path) => {
    if (path.startsWith('/api/events')) return { events: [{ event: 'state', pane: 'w1:p1', ts: 10 }], from: 1 };
    if (path === '/api/tasks') return { tasks: TASKS };
    return { panes: PANES };
  });
  await host.show('kanban', []);
  assert.ok(!paths.some((p) => p.startsWith('/api/events')));
  assert.deepEqual(lastData(posted).events, []);
  assert.equal(lastData(posted).from, null);
});

const NINE_PLUS = ['a', 'b', 'c', 'd', 'e', 'f', 'g', 'h', 'i', 'j', 'k'].map((id) => ({ id, state: 'idle' }));

test('viewAction takes a watch of known panes, nine at most', () => {
  assert.deepEqual(viewAction({ type: 'watch', panes: ['w1:p1', 'ghost', 'w1:p1', 3] }, PANES), { kind: 'watch', panes: ['w1:p1'] });
  assert.deepEqual(viewAction({ type: 'watch', panes: [] }, PANES), { kind: 'watch', panes: [] });
  assert.equal(viewAction({ type: 'watch', panes: 'w1:p1' }, PANES), null);
  const all = viewAction({ type: 'watch', panes: NINE_PLUS.map((p) => p.id) }, NINE_PLUS);
  assert.equal(all.panes.length, WATCH_MAX);
  assert.equal(WATCH_MAX, 9);
});

// watchHost runs a host whose rpc records each call and answers only when
// the test says so, so the order of calls on the wire is seen.
function watchHost(extra = {}) {
  reset();
  const calls = [];
  const posted = [];
  const frame = element('view-frame');
  frame.contentWindow = { postMessage: (msg) => posted.push(msg) };
  let tick = null;
  let flush = null;
  const list = { panes: PANES };
  const rpc = (cmd, fields) => new Promise((resolve, reject) => {
    calls.push({ cmd, fields, resolve, reject });
  });
  const host = mountViewHost(frame, {
    api: async (path) => (path === '/api/tasks' ? { tasks: TASKS } : { panes: list.panes }),
    panes: () => list.panes, go: () => {}, replace: () => {},
    every: (fn) => { tick = fn; return 1; }, cancel: () => {},
    later: (fn) => { flush = fn; return 1; },
    rpc, ...extra,
  });
  const send = (data) => host.onMessage({ source: frame.contentWindow, data });
  // answer resolves every call still waiting, one at a time, in order.
  const answer = async () => {
    for (let i = 0; i < 20; i++) {
      await tick0();
      const open = calls.find((c) => !c.done);
      if (!open) break;
      open.done = true;
      open.resolve({});
    }
  };
  const flushNow = () => { const f = flush; flush = null; if (f) f(); };
  const watchPosts = () => posted.filter((m) => m.type === 'coppice.watch');
  return { host, calls, posted, send, answer, refresh: () => tick(), flush: flushNow, list, watchPosts };
}
const tick0 = () => new Promise((r) => setImmediate(r));

test('a watch attaches view-only, and the floor posts that pane frames to the view', async () => {
  const { host, calls, posted, send, answer, flush } = watchHost();
  await host.show('herdr-grid', []);
  send({ type: 'watch', panes: ['w1:p1', 'ghost'] });
  await answer();
  assert.deepEqual(calls.map((c) => [c.cmd, c.fields]), [['pane.attach', { pane: 'w1:p1', view_only: true }]]);
  host.onEvent({ event: 'frame', pane: 'w1:p1', cols: 3, rows: 2, rows_changed: { 0: [['h', '', '', 0], ['i', '', '', 0]] } });
  host.onEvent({ event: 'frame', pane: 'w2:p1', cols: 3, rows: 1, rows_changed: { 0: [['x', '', '', 0]] } });
  assert.equal(posted.filter((m) => m.type === 'coppice.frame').length, 0, 'a frame posted at once');
  flush();
  const frames = posted.filter((m) => m.type === 'coppice.frame');
  assert.equal(frames.length, 1);
  assert.deepEqual(frames[0], { type: 'coppice.frame', view: 'herdr-grid', pane: 'w1:p1', cols: 3, rows: 2, lines: ['hi', ''] });
  // The exact key set of a frame post: a new key fails here.
  assert.deepEqual(Object.keys(frames[0]).sort(), ['cols', 'lines', 'pane', 'rows', 'type', 'view']);
  for (const m of posted) assert.ok(!JSON.stringify(m).includes(TOKEN));
});

test('a new watch replaces the old one, and the host waits for each reply before the next call', async () => {
  const { host, calls, send, answer } = watchHost();
  await host.show('herdr-grid', []);
  send({ type: 'watch', panes: ['w1:p1'] });
  send({ type: 'watch', panes: ['w2:p1'] });
  await answer();
  // The attach of w1 was dropped before it went out, so it needs no detach.
  assert.deepEqual(calls.map((c) => c.cmd + ' ' + c.fields.pane), ['pane.attach w2:p1']);
  send({ type: 'watch', panes: ['w1:p1'] });
  // Only the detach is on the wire until it is answered.
  for (let i = 0; i < 6; i++) await tick0();
  assert.deepEqual(calls.slice(1).map((c) => c.cmd + ' ' + c.fields.pane), ['pane.detach w2:p1']);
  await answer();
  assert.deepEqual(calls.map((c) => c.cmd + ' ' + c.fields.pane), [
    'pane.attach w2:p1', 'pane.detach w2:p1', 'pane.attach w1:p1',
  ]);
  // A watch of the same pane sends nothing.
  send({ type: 'watch', panes: ['w1:p1'] });
  await answer();
  assert.equal(calls.length, 3);
});

test('hiding a view ends its watch and hands back the wait for the detach', async () => {
  const { host, calls, send, answer, posted } = watchHost();
  await host.show('herdr-grid', []);
  send({ type: 'watch', panes: ['w1:p1'] });
  await answer();
  const gone = host.hide();
  assert.ok(gone && typeof gone.then === 'function', 'hide gave nothing to wait on');
  let done = false;
  gone.then(() => { done = true; });
  await tick0();
  assert.equal(done, false);
  assert.deepEqual(calls.map((c) => c.cmd), ['pane.attach', 'pane.detach']);
  await answer();
  assert.equal(done, true);
  // A frame after the watch ended is never posted.
  const before = posted.length;
  host.onEvent({ event: 'frame', pane: 'w1:p1', cols: 1, rows: 1, rows_changed: { 0: [['z', '', '', 0]] } });
  assert.equal(posted.length, before);
  // With nothing watched, hide has nothing to wait on.
  assert.equal(host.hide(), null);
});

test('switching to another view ends the old watch', async () => {
  const { host, calls, send, answer } = watchHost();
  await host.show('herdr-grid', []);
  send({ type: 'watch', panes: ['w1:p1'] });
  await answer();
  await host.show('inbox', []);
  await answer();
  assert.deepEqual(calls.map((c) => c.cmd), ['pane.attach', 'pane.detach']);
});

test('a view waits for the floor detaches before it attaches', async () => {
  let release = null;
  const gate = new Promise((r) => { release = r; });
  const { host, calls, send, answer } = watchHost();
  await host.show('herdr-grid', [], gate);
  send({ type: 'watch', panes: ['w1:p1'] });
  await tick0();
  assert.equal(calls.length, 0);
  release();
  await tick0();
  await answer();
  assert.equal(calls.length, 1);
});

// The failure this names: a closed pane is refused every refresh for as
// long as the view is open, and its slot reads as live.
test('a refused attach is told to the view and not tried again until the pane list changes', async () => {
  const { host, calls, send, refresh, list, watchPosts } = watchHost();
  await host.show('herdr-grid', []);
  send({ type: 'watch', panes: ['w1:p1'] });
  await tick0();
  calls[0].done = true;
  calls[0].reject(new Error('the pane is closed'));
  await tick0();
  await tick0();
  assert.deepEqual(watchPosts().pop(), { type: 'coppice.watch', view: 'herdr-grid', live: [], refused: ['w1:p1'] });
  await refresh();
  await tick0();
  assert.deepEqual(calls.map((c) => c.cmd), ['pane.attach']);
  list.panes = [...PANES, { id: 'w3:p1', state: 'idle' }];
  await refresh();
  await tick0();
  assert.deepEqual(calls.map((c) => c.cmd), ['pane.attach', 'pane.attach']);
});

test('an answered attach is told to the view as live', async () => {
  const { host, send, answer, watchPosts } = watchHost();
  await host.show('herdr-grid', []);
  send({ type: 'watch', panes: ['w1:p1'] });
  await answer();
  assert.deepEqual(watchPosts().pop(), { type: 'coppice.watch', view: 'herdr-grid', live: ['w1:p1'], refused: [] });
});

test('a refresh drops a watched pane the pane list no longer holds', async () => {
  const { host, calls, send, answer, refresh, list } = watchHost();
  await host.show('herdr-grid', []);
  send({ type: 'watch', panes: ['w1:p1', 'w2:p1'] });
  await answer();
  list.panes = PANES.filter((p) => p.id !== 'w2:p1');
  await refresh();
  await answer();
  assert.deepEqual(calls.map((c) => c.cmd + ' ' + c.fields.pane), [
    'pane.attach w1:p1', 'pane.attach w2:p1', 'pane.detach w2:p1',
  ]);
});

test('a detach queued before the socket dropped is never sent on the new one', async () => {
  const { host, calls, send, answer } = watchHost();
  await host.show('herdr-grid', []);
  send({ type: 'watch', panes: ['w1:p1'] });
  await tick0();
  // The attach is on the wire. A new watch queues its detach behind it.
  send({ type: 'watch', panes: [] });
  host.dropped();
  calls[0].done = true;
  calls[0].resolve({});
  await answer();
  assert.deepEqual(calls.map((c) => c.cmd), ['pane.attach']);
});

test('after the socket drops, the host forgets its attaches and makes them again', async () => {
  const { host, calls, send, answer, refresh, watchPosts } = watchHost();
  await host.show('herdr-grid', []);
  send({ type: 'watch', panes: ['w1:p1'] });
  await answer();
  host.dropped();
  // The view is told the picture is no longer live.
  assert.deepEqual(watchPosts().pop().live, []);
  await refresh();
  await answer();
  assert.deepEqual(calls.map((c) => c.cmd), ['pane.attach', 'pane.attach']);
});

test('a host that may not watch sends nothing for a watch', async () => {
  const { host, calls, send, answer } = watchHost({ watch: false });
  await host.show('minimap', []);
  send({ type: 'watch', panes: ['w1:p1'] });
  await answer();
  assert.deepEqual(calls, []);
});

// wire opens the app's socket and records each request line. answer
// replies ok to every request not yet answered, with PANES for a
// pane.list.
function wire() {
  const ws = lastSocket();
  const lines = [];
  ws.send = (s) => lines.push(JSON.parse(s));
  ws.simulateOpen();
  const answered = new Set();
  const answer = (pred = () => true) => {
    for (const l of lines) {
      if (answered.has(l.id) || !pred(l)) continue;
      answered.add(l.id);
      const result = l.cmd === 'pane.list' ? { panes: PANES } : {};
      ws.fire('message', { data: JSON.stringify({ id: l.id, ok: true, result }) });
    }
  };
  const settle = async () => { for (let i = 0; i < 6; i++) await tick(); };
  return { lines, answer, settle };
}

const GRID = [{ id: 'herdr-grid', title: 'Grid' }];
const attaches = (lines) => lines.filter((l) => l.cmd === 'pane.attach');

// The failure this names: a watch detach that lands after the floor's own
// attach for the same pane stops that tile. The floor attaches only once
// the view's detaches are answered, even when a pane list comes in while
// it waits.
test('a view over the floor never attaches or detaches a pane a window holds, and shows it from the window', async () => {
  await bootAt('#/roster', GRID);
  const { lines, answer, settle } = wire();
  global.window.dispatchEvent(new global.CustomEvent('coppice:panes'));
  await settle();
  answer();
  await settle();
  assert.deepEqual(attaches(lines).map((l) => l.pane).sort(), ['w1:p1', 'w2:p1']);
  const before = lines.length;
  global.location.hash = '#/view/herdr-grid';
  global.window.dispatchEvent(new global.CustomEvent('coppice:route'));
  const frame = element('view-frame');
  const posted = [];
  frame.contentWindow = { postMessage: (msg) => posted.push(msg) };
  global.window.dispatchEvent({ type: 'message', source: frame.contentWindow, data: { type: 'watch', panes: ['w1:p1', 'w2:p1'] } });
  await settle();
  answer();
  await settle();
  const after = lines.slice(before).filter((l) => l.cmd === 'pane.attach' || l.cmd === 'pane.detach');
  assert.deepEqual(after, [], 'the view touched a pane a window holds');
  const told = posted.filter((m) => m.type === 'coppice.watch').pop();
  assert.deepEqual([...told.live].sort(), ['w1:p1', 'w2:p1']);
  element('overlay-close').dispatchEvent({ type: 'click' });
  await settle();
  assert.deepEqual(lines.slice(before).filter((l) => l.cmd === 'pane.detach'), [], 'closing the view detached a window');
});

test('a view watches view-only a pane no window holds, and lets it go with no detach once a window takes it', async () => {
  reset();
  const host = mountViewHost(element('view-frame'), {
    api: async (path) => (path === '/api/tasks' ? { tasks: [] } : { panes: PANES }),
    panes: () => PANES, go: () => {}, replace: () => {},
    every: () => 1, cancel: () => {}, later: (fn) => { fn(); return 1; },
    rpc: async (cmd, fields) => { calls.push([cmd, fields.pane, fields.view_only === true]); return {}; },
    held: () => held,
    settled: () => Promise.resolve(),
    picture: () => null,
  });
  const calls = [];
  let held = ['w1:p1'];
  const frame = element('view-frame');
  frame.contentWindow = { postMessage: () => {} };
  await host.show('herdr-grid', []);
  host.onMessage({ source: frame.contentWindow, data: { type: 'watch', panes: ['w1:p1', 'w2:p1'] } });
  for (let i = 0; i < 6; i++) await tick();
  assert.deepEqual(calls, [['pane.attach', 'w2:p1', true]]);
  held = ['w1:p1', 'w2:p1'];
  host.floorChanged();
  for (let i = 0; i < 6; i++) await tick();
  assert.deepEqual(calls, [['pane.attach', 'w2:p1', true]], 'the watch detached a pane a window took');
  held = ['w1:p1'];
  host.floorChanged();
  for (let i = 0; i < 6; i++) await tick();
  assert.deepEqual(calls, [['pane.attach', 'w2:p1', true], ['pane.attach', 'w2:p1', true]], 'the watch did not take back a pane the window let go');
  host.hide();
  for (let i = 0; i < 6; i++) await tick();
  assert.deepEqual(calls.slice(2), [['pane.detach', 'w2:p1', false]]);
});

test('the floor reads the ring for a view whose manifest asks for it', async () => {
  const { frame, posted } = await bootAt('#/roster', [{ id: 'shift-log', title: 'Shift log', ring: true }]);
  global.location.hash = '#/view/shift-log';
  global.window.dispatchEvent(new global.CustomEvent('coppice:route'));
  for (let i = 0; i < 5; i++) await tick();
  frame.dispatchEvent({ type: 'load' });
  const last = lastData(posted);
  assert.equal(last.from, 7);
  assert.deepEqual(last.events.map((e) => e.ts), [9]);
});

// The rail is one element in the first column, so the minimap sits at its
// foot, under the view list, and never under the floor.
test('the minimap sits in the rail, after the view list', () => {
  const html = readFileSync(new URL('../index.html', import.meta.url), 'utf8');
  const rail = /<div id="rail">([\s\S]*?)<\/div>/.exec(html);
  assert.ok(rail, 'no rail');
  const inner = rail[1];
  for (const id of ['roster', 'roster-empty', 'view-list', 'rail-foot', 'minimap-frame']) {
    assert.ok(inner.includes('id="' + id + '"'), id + ' is not in the rail');
  }
  // The views, then the layout and lock footer, then the minimap at the foot.
  assert.ok(inner.indexOf('id="view-list"') < inner.indexOf('id="rail-foot"'));
  assert.ok(inner.indexOf('id="rail-foot"') < inner.indexOf('id="minimap-frame"'));
  const css = readFileSync(new URL('../app.css', import.meta.url), 'utf8');
  assert.match(css, /#rail \{ grid-column: 1; grid-row: 1;/);
  assert.match(css, /#floor \{ grid-column: 2; grid-row: 1;/);
});

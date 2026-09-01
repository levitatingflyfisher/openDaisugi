import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import {
  reset, setFetch, element, TOKEN, freshApp, lastSocket, liveIntervals, liveTimers,
} from './browser-stub.mjs';

// This file drives the roster and the pane screen through a real boot(),
// the same way app-connect.test.mjs drives the reconnect logic. Reading the
// screens' pure functions in isolation, which grid.test.mjs, roster.test.mjs
// and pane.test.mjs already do, cannot see what only shows up once app.js,
// roster.js and pane.js run together: what the router fires before the
// socket is open, and what a click on a real button actually sends.

const PANES = JSON.parse(
  readFileSync(new URL('./fixtures/pane_list.json', import.meta.url), 'utf8'),
).result.panes;

// The failure this names: a push notification links straight at a blocked
// pane. boot() dispatches its first route event before the socket is open,
// so a pane screen that only attached from that one event would sit on a
// raw pane id forever, with no Allow and Deny box and no way to answer the
// ask that is waiting on it.
test('a cold start into a blocked pane attaches once the socket opens and shows the ask', async () => {
  reset();
  global.localStorage.setItem('coppice.token', TOKEN);
  global.location.hash = '#/pane/w1:p2';
  await freshApp();

  const ws = lastSocket();
  const sent = [];
  ws.send = (data) => { sent.push(JSON.parse(data)); };

  ws.simulateOpen();

  const attach = sent.find((m) => m.cmd === 'pane.attach');
  assert.ok(attach, 'pane.attach was never sent');
  assert.equal(attach.pane, 'w1:p2');

  const paneList = sent.find((m) => m.cmd === 'pane.list');
  assert.ok(paneList, 'pane.list was never sent');
  ws.fire('message', { data: JSON.stringify({ id: paneList.id, ok: true, result: { panes: PANES } }) });
  await new Promise((r) => setImmediate(r));

  assert.equal(element('ask').hidden, false);
  assert.equal(element('ask-summary').textContent, 'rm -rf build/');
  assert.equal(element('pane-chip').textContent, 'blocked');
});

// The failure this names: a pane's attachment lives in the upstream
// session, and a new websocket opens a new session. The same-pane guard in
// open() exists so a repeat route event does not blank the grid mid-stream,
// but it also matches the one event that means the opposite, a reconnect
// after the old session is already gone, and would leave the grid frozen
// on the last frame before the drop, forever, until the operator routes
// away and back.
test('a reconnect while parked on a pane sends pane.attach again on the new socket', async () => {
  reset();
  global.localStorage.setItem('coppice.token', TOKEN);
  // A pane id distinct from every other test in this file: pane.js's
  // current is a module-level variable, not reset between scenarios in the
  // same process, so reusing an id another test already opened would trip
  // the same-pane guard on residual state rather than on anything this
  // test does.
  global.location.hash = '#/pane/w9:reconnect-test';
  await freshApp();

  const sent = [];
  let ws = lastSocket();
  ws.send = (data) => { sent.push(JSON.parse(data)); };
  ws.simulateOpen();
  const firstAttach = sent.find((m) => m.cmd === 'pane.attach');
  assert.ok(firstAttach, 'the first pane.attach was never sent');

  // The attach must actually resolve before the drop, the same as a real
  // pane that has been streaming frames for a while. An attach still
  // pending when the socket closes gets rejected by rejectAllPending on its
  // own, which resets current as an incidental side effect and would hide
  // the defect this test exists to catch.
  ws.fire('message', { data: JSON.stringify({ id: firstAttach.id, ok: true, result: {} }) });
  await new Promise((r) => setImmediate(r));

  ws.simulateClose('coppice-server closed the connection');
  await new Promise((r) => setImmediate(r));
  const timer = liveTimers()[liveTimers().length - 1];
  assert.ok(timer, 'no reconnect was scheduled after the upstream closed the connection');
  timer.fn();

  ws = lastSocket();
  ws.send = (data) => { sent.push(JSON.parse(data)); };
  ws.simulateOpen();

  const attaches = sent.filter((m) => m.cmd === 'pane.attach');
  assert.equal(attaches.length, 2, 'a reconnect while parked on the pane never re-attached');
});

// The same defect, reached from the other trigger: a fresh token saved
// from Settings while parked on a pane. reconnect() closes the live socket
// itself; in the stub that close is a no-op that fires no close event, the
// same as a real socket's close() call, whose own close event arrives
// asynchronously, after reconnect() has already swapped in the new socket.
// Nothing must depend on that event to clear current, or the same-pane
// guard blocks the re-attach here too.
test('a reconnect() while parked on a pane sends pane.attach again on the new socket', async () => {
  reset();
  global.localStorage.setItem('coppice.token', TOKEN);
  global.location.hash = '#/pane/w9:reconnect-token-test';
  await freshApp();

  const sent = [];
  const first = lastSocket();
  first.send = (data) => { sent.push(JSON.parse(data)); };
  first.simulateOpen();
  const firstAttach = sent.find((m) => m.cmd === 'pane.attach');
  assert.ok(firstAttach, 'the first pane.attach was never sent');
  first.fire('message', { data: JSON.stringify({ id: firstAttach.id, ok: true, result: {} }) });
  await new Promise((r) => setImmediate(r));

  global.window.coppice.reconnect(TOKEN);
  const second = lastSocket();
  assert.notEqual(second, first, 'reconnect() did not open a new socket');
  second.send = (data) => { sent.push(JSON.parse(data)); };
  second.simulateOpen();

  const attaches = sent.filter((m) => m.cmd === 'pane.attach');
  assert.equal(attaches.length, 2, 'reconnect() while parked on the pane never re-attached');
});

// The Allow and Deny box is the single most consequential, least reversible
// action the phone offers. A renamed body key or URL would decode to an
// empty tool_use_id on the server, which answers a live ask with a lie.
test('Allow posts exactly tool_use_id and decision, and clears the box on success', async () => {
  reset();
  global.localStorage.setItem('coppice.token', TOKEN);
  await freshApp();
  lastSocket().simulateOpen();

  const calls = [];
  global.window.coppice.api = async (path, options) => {
    calls.push({ path, body: JSON.parse(options.body) });
    return { ok: true };
  };

  element('ask').dataset.askId = 'toolu_01ABC';
  element('ask').hidden = false;
  element('allow').dispatchEvent({ type: 'click' });
  await new Promise((r) => setImmediate(r));

  assert.equal(calls.length, 1);
  assert.equal(calls[0].path, '/api/ask/answer');
  assert.equal(calls[0].body.tool_use_id, 'toolu_01ABC');
  assert.equal(calls[0].body.decision, 'allow');
  assert.equal(element('ask').hidden, true);
  assert.equal(element('ask').dataset.askId, '');
});

// The row does not carry an ask, so there is nothing to answer. A stray
// click here must never reach the server.
test('a click with no ask id posts nothing', async () => {
  reset();
  global.localStorage.setItem('coppice.token', TOKEN);
  await freshApp();
  lastSocket().simulateOpen();

  const calls = [];
  global.window.coppice.api = async (...args) => { calls.push(args); return {}; };

  element('ask').dataset.askId = '';
  element('deny').dispatchEvent({ type: 'click' });
  await new Promise((r) => setImmediate(r));

  assert.equal(calls.length, 0);
});

// The failure this names: the roster's first paint used to go over the
// socket, which is still CONNECTING at boot, so rpc rejected with a message
// naming the operator's token before the operator had done anything wrong.
test('the roster paints its first frame over the plain HTTP endpoint before the socket opens', async () => {
  reset();
  global.localStorage.setItem('coppice.token', TOKEN);
  setFetch(async (path) => {
    assert.equal(path, '/api/panes');
    return { ok: true, status: 200, json: async () => ({ panes: PANES }) };
  });
  await freshApp();
  await new Promise((r) => setImmediate(r));

  assert.equal(element('roster-empty').hidden, true);
  assert.equal(element('roster').children.length, 3);
  assert.notEqual(element('status').textContent, 'Not connected. Check the token in Settings.');
  assert.equal(element('status').textContent, '');
});

// A refusal is a real error, not an empty household. Painting the empty
// banner over a refusal would send the operator hunting a bug that is not
// theirs, and it would erase a roster that was showing correctly a moment
// before.
test('a 502 from the first paint reads as an error, and never overwrites the roster', async () => {
  reset();
  global.localStorage.setItem('coppice.token', TOKEN);
  element('roster').children = ['a card the operator was already looking at'];
  element('roster-empty').hidden = true;
  setFetch(async () => ({ ok: false, status: 502, json: async () => ({ error: 'server refused: nope' }) }));
  await freshApp();
  await new Promise((r) => setImmediate(r));

  assert.equal(element('status').textContent, 'server refused: nope');
  assert.deepEqual(element('roster').children, ['a card the operator was already looking at']);
  assert.equal(element('roster-empty').hidden, true);
});

// The socket path can degrade the same way the HTTP path can: coppice-server
// could in principle answer ok:true with a result that is not an object.
// That is not "no panes yet", and refresh() must not paint it as one.
test('a malformed result over the socket reads as an error, and never paints an empty roster', async () => {
  reset();
  global.localStorage.setItem('coppice.token', TOKEN);
  element('roster').children = ['a card the operator was already looking at'];
  element('roster-empty').hidden = true;
  await freshApp();

  const ws = lastSocket();
  const sent = [];
  ws.send = (data) => { sent.push(JSON.parse(data)); };
  ws.simulateOpen();

  const paneList = sent.find((m) => m.cmd === 'pane.list');
  assert.ok(paneList, 'pane.list was never sent');
  ws.fire('message', { data: JSON.stringify({ id: paneList.id, ok: true, result: 'oops' }) });
  await new Promise((r) => setImmediate(r));
  await new Promise((r) => setImmediate(r));

  assert.equal(element('status').textContent, 'coppice-server sent a reply the phone could not read. Run coppice server status.');
  assert.deepEqual(element('roster').children, ['a card the operator was already looking at']);
  assert.equal(element('roster-empty').hidden, true);
});

// app.js's rpc() resolves a missing result as an empty object, not
// undefined, so a reply with no result key at all reaches roster.js looking
// exactly like a real, deliberately empty {panes: []}. It is not:
// coppice-server always sets panes, even to an empty array, so a reply
// missing the key entirely is malformed the same way a non-object result
// is.
test('an ok reply with no result key at all reads as an error, not an empty roster', async () => {
  reset();
  global.localStorage.setItem('coppice.token', TOKEN);
  element('roster').children = ['a card the operator was already looking at'];
  element('roster-empty').hidden = true;
  await freshApp();

  const ws = lastSocket();
  const sent = [];
  ws.send = (data) => { sent.push(JSON.parse(data)); };
  ws.simulateOpen();

  const paneList = sent.find((m) => m.cmd === 'pane.list');
  assert.ok(paneList, 'pane.list was never sent');
  ws.fire('message', { data: JSON.stringify({ id: paneList.id, ok: true }) });
  await new Promise((r) => setImmediate(r));
  await new Promise((r) => setImmediate(r));

  assert.equal(element('status').textContent, 'coppice-server sent a reply the phone could not read. Run coppice server status.');
  assert.deepEqual(element('roster').children, ['a card the operator was already looking at']);
  assert.equal(element('roster-empty').hidden, true);
});

// The header keeps a Settings button visible while the Settings screen is
// already showing, and a New button visible while the New screen is
// already showing. Neither button does anything useful pointed at the
// screen it already opened.
test('the header hides Settings while on Settings, and New while on New', async () => {
  reset();
  global.localStorage.setItem('coppice.token', TOKEN);
  await freshApp();

  assert.equal(element('go-settings').hidden, false);
  assert.equal(element('go-new').hidden, false);

  global.location.hash = '#/settings';
  global.window.dispatchEvent(new global.CustomEvent('coppice:route'));
  assert.equal(element('go-settings').hidden, true);
  assert.equal(element('go-new').hidden, false);

  global.location.hash = '#/new';
  global.window.dispatchEvent(new global.CustomEvent('coppice:route'));
  assert.equal(element('go-settings').hidden, false);
  assert.equal(element('go-new').hidden, true);

  global.location.hash = '#/roster';
  global.window.dispatchEvent(new global.CustomEvent('coppice:route'));
  assert.equal(element('go-settings').hidden, false);
  assert.equal(element('go-new').hidden, false);
});

// Nothing else ticks the age text on a card between refreshes, so the
// roster reruns its own paint on a timer while it is the visible screen,
// and that timer must not run forever once the operator has moved on.
test('the roster ticker runs on the roster screen and stops on a route away', async () => {
  reset();
  global.localStorage.setItem('coppice.token', TOKEN);
  setFetch(async () => ({ ok: true, status: 200, json: async () => ({ panes: [] }) }));
  await freshApp();
  await new Promise((r) => setImmediate(r));

  assert.equal(liveIntervals().length, 1, 'the roster ticker did not start while showing the roster');

  global.location.hash = '#/settings';
  global.window.dispatchEvent(new global.CustomEvent('coppice:route'));

  assert.equal(liveIntervals().length, 0, 'the roster ticker kept running after a route away');
});

// The floor is what the roster route shows: the rail plus the tiles the
// width allows. The stub's width is 1200, two tiles. This drives the whole
// path through boot(): the first pane.list fills two tiles and attaches
// both, a frame reaches the tile's mirror, and a route to a pane the floor
// holds hands that attach to the pane screen. The server runs each request
// on its own goroutine, so a detach and an attach for one pane on one
// socket may run in either order; the hand-over is what keeps the pair off
// the wire.
test('the floor attaches its tiles, paints frames, and hands a tile over to the pane screen', async () => {
  reset();
  global.localStorage.setItem('coppice.token', TOKEN);
  setFetch(async () => ({ ok: true, status: 200, json: async () => ({ panes: [] }) }));
  await freshApp();
  await new Promise((r) => setImmediate(r));
  assert.equal(element('title').textContent, 'Floor');

  const ws = lastSocket();
  const sent = [];
  ws.send = (data) => { sent.push(JSON.parse(data)); };
  ws.simulateOpen();
  const paneList = sent.find((m) => m.cmd === 'pane.list');
  ws.fire('message', { data: JSON.stringify({ id: paneList.id, ok: true, result: { panes: PANES } }) });
  await new Promise((r) => setImmediate(r));

  const attaches = sent.filter((m) => m.cmd === 'pane.attach');
  assert.deepEqual(attaches.map((m) => m.pane), ['w1:p2', 'w1:p1'], 'the blocked pane and the next one fill the two tiles');
  assert.equal('cols' in attaches[0], false, 'a tile attach carries a size');
  assert.equal('rows' in attaches[0], false, 'a tile attach carries a size');

  ws.fire('message', { data: JSON.stringify({
    event: 'frame', pane: 'w1:p2', seq: 1, cols: 2, rows: 1, cursor: [0, 0],
    rows_changed: { 0: [['o', '', '', 0], ['k', '', '', 0]] },
  }) });
  const tile = element('screen-roster').querySelector('[data-tile="w1:p2"]');
  assert.ok(tile, 'no tile for the blocked pane');
  assert.equal(tile.querySelector('.mirror').textContent, 'ok');
  assert.ok(tile.querySelector('.askbar').textContent.includes('rm -rf build/'));
  ws.fire('message', { data: JSON.stringify({
    event: 'frame', pane: 'w1:p1', seq: 1, cols: 2, rows: 1, cursor: [0, 0],
    rows_changed: { 0: [['g', '', '', 0], ['o', '', '', 0]] },
  }) });

  global.location.hash = '#/pane/w1:p1';
  global.window.dispatchEvent(new global.CustomEvent('coppice:route'));
  await new Promise((r) => setImmediate(r));
  const tail = sent.slice(-2).map((m) => [m.cmd, m.pane]);
  assert.deepEqual(tail, [['pane.detach', 'w1:p2'], ['pane.attach', 'w1:p1']]);
  assert.equal(element('screen-roster').hidden, true);
  assert.equal(element('grid-text').textContent, 'go', 'the pane screen did not start from the tile\'s grid');

  global.location.hash = '#/roster';
  global.window.dispatchEvent(new global.CustomEvent('coppice:route'));
  await new Promise((r) => setImmediate(r));
  const back = sent.slice(-1).map((m) => [m.cmd, m.pane]);
  assert.deepEqual(back, [['pane.attach', 'w1:p2']]);
  assert.equal(sent.filter((m) => m.cmd === 'pane.detach' && m.pane === 'w1:p1').length, 0);
  const tileAgain = element('screen-roster').querySelector('[data-tile="w1:p1"]');
  assert.ok(tileAgain, 'the handed-back pane lost its tile');
});

// firstList opens the socket, answers the first pane.list with panes, and
// returns the recorder. Every hand-over test below starts from this.
async function floorWith(panes) {
  reset();
  global.localStorage.setItem('coppice.token', TOKEN);
  setFetch(async () => ({ ok: true, status: 200, json: async () => ({ panes: [] }) }));
  await freshApp();
  await new Promise((r) => setImmediate(r));
  const ws = lastSocket();
  const sent = [];
  ws.send = (data) => { sent.push(JSON.parse(data)); };
  ws.simulateOpen();
  const paneList = sent.find((m) => m.cmd === 'pane.list');
  ws.fire('message', { data: JSON.stringify({ id: paneList.id, ok: true, result: { panes } }) });
  await new Promise((r) => setImmediate(r));
  return { ws, sent };
}

const go = async (hash) => {
  global.location.hash = hash;
  global.window.dispatchEvent(new global.CustomEvent('coppice:route'));
  await new Promise((r) => setImmediate(r));
};

// Leaving the floor for a pane a tile holds: no detach for that pane, ever.
test('leaving the floor for a tile pane never sends pane.detach for it', async () => {
  const x = { id: 'w7:x', label: 'x', state: 'working', harness: 'claude', cwd: '/w/x', ts: 1 };
  const y = { id: 'w7:y', label: 'y', state: 'working', harness: 'claude', cwd: '/w/y', ts: 1 };
  const { sent } = await floorWith([x, y]);
  assert.deepEqual(sent.filter((m) => m.cmd === 'pane.attach').map((m) => m.pane), ['w7:x', 'w7:y']);
  await go('#/pane/w7:x');
  assert.equal(sent.filter((m) => m.cmd === 'pane.detach' && m.pane === 'w7:x').length, 0);
  assert.deepEqual(sent.filter((m) => m.cmd === 'pane.detach').map((m) => m.pane), ['w7:y']);
  assert.equal(sent.filter((m) => m.cmd === 'pane.attach' && m.pane === 'w7:x').length, 2, 'the pane screen did not send its own attach');
  await go('#/settings');
  assert.deepEqual(sent.filter((m) => m.cmd === 'pane.detach').map((m) => m.pane), ['w7:y', 'w7:x'], 'leaving the pane screen for Settings did not detach');
});

// Returning to the floor from a pane it will show: no detach for that
// pane, ever, and no second attach from the floor either.
test('returning to the floor from a pane never sends pane.detach for it', async () => {
  const x = { id: 'w8:x', label: 'x', state: 'working', harness: 'claude', cwd: '/w/x', ts: 1 };
  const y = { id: 'w8:y', label: 'y', state: 'working', harness: 'claude', cwd: '/w/y', ts: 1 };
  const { ws, sent } = await floorWith([x, y]);
  await go('#/pane/w8:x');
  ws.fire('message', { data: JSON.stringify({
    event: 'frame', pane: 'w8:x', seq: 1, cols: 2, rows: 1, cursor: [0, 0],
    rows_changed: { 0: [['h', '', '', 0], ['i', '', '', 0]] },
  }) });
  assert.equal(element('grid-text').textContent, 'hi');
  const before = sent.length;
  await go('#/roster');
  assert.equal(sent.filter((m) => m.cmd === 'pane.detach' && m.pane === 'w8:x').length, 0);
  const wire = sent.slice(before).filter((m) => m.cmd === 'pane.attach' || m.cmd === 'pane.detach');
  assert.deepEqual(wire.map((m) => [m.cmd, m.pane]), [['pane.attach', 'w8:y']]);
  const tile = element('screen-roster').querySelector('[data-tile="w8:x"]');
  assert.equal(tile.querySelector('.mirror').textContent, 'hi', 'the tile did not start from the pane screen\'s grid');
  ws.fire('message', { data: JSON.stringify({
    event: 'frame', pane: 'w8:x', seq: 2, cols: 2, rows: 1, cursor: [0, 0], rows_changed: { 0: [['o', '', '', 0], ['k', '', '', 0]] },
  }) });
  assert.equal(tile.querySelector('.mirror').textContent, 'ok', 'a delta after the hand-back did not reach the tile');
});

// The failure this names: the floor keeps its own attached set, and a new
// socket opens a new upstream session that holds none of those attaches.
// Without clearing the set on a drop, the paint after the reconnect sees
// every shown pane already attached and sends nothing, and the tiles stay
// frozen on their last frame. This is the floor's counterpart of the pane
// screen test above.
test('a drop while on the floor attaches every tile again on the new socket', async () => {
  reset();
  global.localStorage.setItem('coppice.token', TOKEN);
  setFetch(async () => ({ ok: true, status: 200, json: async () => ({ panes: [] }) }));
  await freshApp();
  await new Promise((r) => setImmediate(r));

  const sent = [];
  let ws = lastSocket();
  ws.send = (data) => { sent.push(JSON.parse(data)); };
  ws.simulateOpen();
  let paneList = sent.find((m) => m.cmd === 'pane.list');
  ws.fire('message', { data: JSON.stringify({ id: paneList.id, ok: true, result: { panes: PANES } }) });
  await new Promise((r) => setImmediate(r));
  const first = sent.filter((m) => m.cmd === 'pane.attach');
  assert.equal(first.length, 2, 'the floor did not attach its two tiles');
  for (const a of first) ws.fire('message', { data: JSON.stringify({ id: a.id, ok: true, result: {} }) });
  await new Promise((r) => setImmediate(r));

  ws.simulateClose('coppice-server closed the connection');
  await new Promise((r) => setImmediate(r));
  const timer = liveTimers()[liveTimers().length - 1];
  assert.ok(timer, 'no reconnect was scheduled');
  timer.fn();

  ws = lastSocket();
  const again = [];
  ws.send = (data) => { const m = JSON.parse(data); sent.push(m); again.push(m); };
  ws.simulateOpen();
  paneList = again.find((m) => m.cmd === 'pane.list');
  assert.ok(paneList, 'pane.list was never sent on the new socket');
  ws.fire('message', { data: JSON.stringify({ id: paneList.id, ok: true, result: { panes: PANES } }) });
  await new Promise((r) => setImmediate(r));

  assert.deepEqual(again.filter((m) => m.cmd === 'pane.attach').map((m) => m.pane), ['w1:p2', 'w1:p1']);
  assert.equal(again.filter((m) => m.cmd === 'pane.detach').length, 0, 'a detach was sent for a session the drop already ended');
});

// The same defect reached from a fresh token saved in Settings while the
// floor is up: reconnect() closes the live socket itself, and the floor's
// attaches die with it.
test('a reconnect() while on the floor attaches every tile again on the new socket', async () => {
  reset();
  global.localStorage.setItem('coppice.token', TOKEN);
  setFetch(async () => ({ ok: true, status: 200, json: async () => ({ panes: [] }) }));
  await freshApp();
  await new Promise((r) => setImmediate(r));

  const sent = [];
  const first = lastSocket();
  first.send = (data) => { sent.push(JSON.parse(data)); };
  first.simulateOpen();
  let paneList = sent.find((m) => m.cmd === 'pane.list');
  first.fire('message', { data: JSON.stringify({ id: paneList.id, ok: true, result: { panes: PANES } }) });
  await new Promise((r) => setImmediate(r));
  assert.equal(sent.filter((m) => m.cmd === 'pane.attach').length, 2);

  global.window.coppice.reconnect(TOKEN);
  const second = lastSocket();
  assert.notEqual(second, first, 'reconnect() did not open a new socket');
  const again = [];
  second.send = (data) => { again.push(JSON.parse(data)); };
  second.simulateOpen();
  paneList = again.find((m) => m.cmd === 'pane.list');
  second.fire('message', { data: JSON.stringify({ id: paneList.id, ok: true, result: { panes: PANES } }) });
  await new Promise((r) => setImmediate(r));

  assert.deepEqual(again.filter((m) => m.cmd === 'pane.attach').map((m) => m.pane), ['w1:p2', 'w1:p1']);
});

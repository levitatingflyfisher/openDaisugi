import test from 'node:test';
import assert from 'node:assert/strict';
import {
  reset, setFetch, element, TOKEN, freshApp, lastSocket, liveTimers, sockets,
} from './browser-stub.mjs';

// Neither Blocking 2 defect lived in nextRetry's arithmetic, which
// sw-policy.test.mjs and app.test.mjs already cover as a pure function.
// Both lived in which event resets the backoff and what the stop branch
// does to the screen, and only a real open-then-close cycle, driven end to
// end through a stubbed socket, exercises either line.

test('five upstream-died closes after a completed handshake back off to 30 seconds', async () => {
  reset();
  global.localStorage.setItem('coppice.token', TOKEN);
  await freshApp();

  const delays = [];
  for (let i = 0; i < 5; i++) {
    const ws = lastSocket();
    ws.simulateOpen();
    ws.simulateClose('coppice-server is not reachable');
    await new Promise((r) => setImmediate(r));
    const timer = liveTimers()[liveTimers().length - 1];
    delays.push(timer.ms);
    timer.fn(); // the real setTimeout callback: connect()
  }
  assert.deepEqual(delays, [2000, 4000, 8000, 16000, 30000]);
});

test('a rejected token shows the settings screen and schedules no reconnect', async () => {
  reset();
  setFetch(async () => ({ status: 401 }));
  global.localStorage.setItem('coppice.token', TOKEN);
  await freshApp();

  const ws = lastSocket();
  ws.simulateOpen();
  // A handshake the server refuses outright reaches the browser as a
  // blank reason, which is what sends afterClose to the token probe
  // instead of the upstream branch.
  ws.simulateClose('');
  await new Promise((r) => setImmediate(r));
  await new Promise((r) => setImmediate(r));

  assert.equal(element('screen-settings').hidden, false);
  assert.equal(element('screen-roster').hidden, true);
  assert.equal(liveTimers().length, 0);
});

// The failure this names: a cold open with no token spent one of the
// operator's own three ban strikes. connect() already refuses to open a
// socket with no token, and leaves its own sentence on the status line, but
// the roster's route handler asked the plain HTTP endpoint anyway, with
// "Bearer null" as the token, and that request reached the guard and
// counted as a failure against the phone's own address.
test('a cold start with no token makes no fetch, opens no socket, and keeps the no-token sentence', async () => {
  reset();
  const calls = [];
  setFetch(async (path, options) => {
    calls.push({ path, auth: options && options.headers && options.headers.Authorization });
    return { ok: false, status: 401, json: async () => ({}) };
  });
  await freshApp();
  await new Promise((r) => setImmediate(r));

  assert.deepEqual(calls, [], 'a fetch was made with no token stored');
  assert.equal(sockets.length, 0, 'a socket was opened with no token stored');
  assert.equal(element('status').textContent, 'No token. Open Settings and paste one.');
});

// A closed socket object left on state.ws is cosmetic today: connect()'s
// reentry guard checks readyState, and rpc rejects on anything but OPEN, so
// nothing reads the stale reference wrongly. Clearing it anyway is what a
// caller that only checks truthiness, now or later, needs to see a closed
// socket as no socket at all.
test('the close listener clears state.ws', async () => {
  reset();
  global.localStorage.setItem('coppice.token', TOKEN);
  await freshApp();
  const ws = lastSocket();
  ws.simulateOpen();
  assert.equal(global.window.coppice.state.ws, ws);

  ws.simulateClose('coppice-server closed the connection');
  assert.equal(global.window.coppice.state.ws, null);
});

// The failure this names: reconnect() replaces state.ws with a new socket
// before the old one's own close event has a chance to arrive. In a real
// browser that event still comes, asynchronously, some time after close()
// was called. If its listener nulled state.ws unconditionally, that late
// event would clobber the reference to the new socket that already
// replaced it.
test('a belated close from a socket reconnect() already replaced does not clear the new one', async () => {
  reset();
  global.localStorage.setItem('coppice.token', TOKEN);
  await freshApp();
  const first = lastSocket();
  first.simulateOpen();

  global.window.coppice.reconnect(TOKEN);
  const second = lastSocket();
  assert.notEqual(second, first, 'reconnect did not open a new socket');
  assert.equal(global.window.coppice.state.ws, second);

  first.simulateClose('');
  assert.equal(global.window.coppice.state.ws, second, 'a stale close nulled the socket that replaced it');
});

test('a second connect() while the socket is connecting, open, or closing opens no second socket', async () => {
  reset();
  global.localStorage.setItem('coppice.token', TOKEN);
  await freshApp();
  // connect is not one of app.js's named exports. window.coppice is the
  // one place it reaches outside the module, the same door the shell's own
  // Back, New, and Settings buttons use.
  const { connect } = global.window.coppice;

  const first = lastSocket();
  assert.equal(first.readyState, 0); // CONNECTING
  connect();
  assert.equal(lastSocket(), first, 'a second call while CONNECTING made a new socket');

  first.simulateOpen();
  connect();
  assert.equal(lastSocket(), first, 'a second call while OPEN made a new socket');

  first.readyState = 2; // CLOSING
  connect();
  assert.equal(lastSocket(), first, 'a second call while CLOSING made a new socket');
});

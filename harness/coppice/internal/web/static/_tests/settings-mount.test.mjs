import test from 'node:test';
import assert from 'node:assert/strict';
import {
  reset, setFetch, element, TOKEN, freshApp, lastSocket, sockets, liveTimers,
} from './browser-stub.mjs';

// settings.test.mjs proves wsUrl and tokenFromHash on their own. This file
// drives the Settings screen through a real boot(), the same way
// screens-connect.test.mjs drives the roster and the pane screen, to prove
// what only shows up once app.js and settings.js run together: what the
// push button actually paints for each server answer, what Save and Forget
// actually do to storage, and what a rejected token leaves on screen.

// The failure this names: a push failure painted as success, or a success
// painted as a failure, sends the operator waiting on a notification that
// will never arrive, or gives up on one that is already on its way.
test('push test shows a distinct sentence for the off, refused, and sent outcomes', async () => {
  reset();
  global.localStorage.setItem('coppice.token', TOKEN);
  await freshApp();

  global.window.coppice.api = async () => {
    throw new Error('Push is off. Restart the server with --ntfy URL and --ntfy-topic NAME.');
  };
  element('test-push').dispatchEvent({ type: 'click' });
  await new Promise((r) => setImmediate(r));
  assert.equal(element('status').textContent, 'Push is off. Restart the server with --ntfy URL and --ntfy-topic NAME.');

  global.window.coppice.api = async () => {
    throw new Error('ntfy did not accept the message. Check the URL and the token.');
  };
  element('test-push').dispatchEvent({ type: 'click' });
  await new Promise((r) => setImmediate(r));
  assert.equal(element('status').textContent, 'ntfy did not accept the message. Check the URL and the token.');
  assert.notEqual(element('status').textContent, 'Sent. Watch the ntfy app.');

  global.window.coppice.api = async () => ({ ok: true });
  element('test-push').dispatchEvent({ type: 'click' });
  await new Promise((r) => setImmediate(r));
  assert.equal(element('status').textContent, 'Sent. Watch the ntfy app.');
});

// The failure this names: the operator reaches Settings with a blank box,
// taps the button that says Save, and is told it worked. It did not: the
// previously stored token is now gone and the app can no longer connect.
test('an empty Save is refused and stores nothing', async () => {
  reset();
  await freshApp();

  element('set-token').value = '   ';
  element('save-settings').dispatchEvent({ type: 'click' });

  assert.equal(element('status').textContent, 'Paste a token first, or tap Forget token to clear it.');
  assert.equal(global.localStorage.getItem('coppice.token'), null);
});

// A typo'd token that looks close enough is worse than an obvious empty
// box: it stores something that will never connect and gives no sign why.
test('a malformed token is refused with its own sentence and not stored', async () => {
  reset();
  await freshApp();

  element('set-token').value = 'not-a-real-token';
  element('save-settings').dispatchEvent({ type: 'click' });

  assert.equal(element('status').textContent, 'That token is not the right shape. Run coppice web token.');
  assert.equal(global.localStorage.getItem('coppice.token'), null);
});

test('Forget removes the stored token and empties the field', async () => {
  reset();
  global.localStorage.setItem('coppice.token', TOKEN);
  await freshApp();

  element('set-token').value = TOKEN;
  element('forget-token').dispatchEvent({ type: 'click' });

  assert.equal(global.localStorage.getItem('coppice.token'), null);
  assert.equal(element('set-token').value, '');
  assert.equal(element('status').textContent, 'Token forgotten. Scan the QR again.');
});

// The failure this names: the token is rejected, the app throws the
// operator onto Settings, and the screen shows a header that still says
// Panes, no statement of which server this is, and a token box that gives
// no sign whether a token was ever stored.
test('a rejected token opens a Settings screen that already knows the server and the stored token', async () => {
  reset();
  setFetch(async () => ({ status: 401 }));
  global.localStorage.setItem('coppice.token', TOKEN);
  await freshApp();

  const ws = lastSocket();
  ws.simulateOpen();
  ws.simulateClose('');
  await new Promise((r) => setImmediate(r));
  await new Promise((r) => setImmediate(r));

  assert.equal(element('screen-settings').hidden, false);
  assert.equal(element('set-origin').textContent, 'Server ' + global.location.origin);
  assert.equal(element('set-token').value, TOKEN);
  assert.equal(element('title').textContent, 'Settings');
});

// The failure this names: the operator pastes a good token after a
// rejection, taps Save, and the app stays latched off because nothing on
// the save path ever lifted the stop the rejection set.
test('after a 401 latch, saving a well-formed token opens a new socket', async () => {
  reset();
  setFetch(async () => ({ status: 401 }));
  global.localStorage.setItem('coppice.token', TOKEN);
  await freshApp();

  const first = lastSocket();
  first.simulateOpen();
  first.simulateClose('');
  await new Promise((r) => setImmediate(r));
  await new Promise((r) => setImmediate(r));

  assert.equal(element('screen-settings').hidden, false);

  const fresh = 'C'.repeat(43);
  element('set-token').value = fresh;
  element('save-settings').dispatchEvent({ type: 'click' });

  const second = lastSocket();
  assert.notEqual(second, first, 'saving a good token after a 401 latch opened no new socket');
  assert.equal(second.readyState, 0);
  assert.equal(element('status').textContent, 'Saved. Reconnecting.');
});

// The failure this names: the operator opens Settings from a screen where
// the socket is still connected, pastes a different good token, and taps
// Save. Without a deliberate close, connect()'s own reentry guard refuses
// to open a second socket while the old one under the old token is still
// live, so the phone reports Reconnecting and does nothing.
test('saving a fresh token while connected closes the old socket and opens exactly one new one', async () => {
  reset();
  global.localStorage.setItem('coppice.token', TOKEN);
  await freshApp();

  const first = lastSocket();
  first.simulateOpen();

  const closeCalls = [];
  first.close = () => {
    closeCalls.push(true);
    first.simulateClose('');
  };

  const before = sockets.length;
  const fresh = 'E'.repeat(43);
  element('set-token').value = fresh;
  element('save-settings').dispatchEvent({ type: 'click' });

  assert.equal(closeCalls.length, 1, 'the old socket was never closed');
  assert.equal(sockets.length, before + 1, 'a save while connected opened other than exactly one new socket');
  const second = lastSocket();
  assert.notEqual(second, first);
  assert.ok(
    second.protocols.includes('daisugi.bearer.' + fresh),
    'the new socket did not carry the new token',
  );

  // A retry, if one were scheduled, would only appear after the close
  // listener's own await chain settles, not in the same tick as the click.
  await new Promise((r) => setImmediate(r));
  await new Promise((r) => setImmediate(r));
  assert.equal(liveTimers().length, 0, 'the deliberate close scheduled a retry with the old token');

  // The deliberate flag must not stay latched. A close after this one is
  // an ordinary server-side drop on the new socket, and it has to retry
  // the ordinary way, or a real drop here would go unanswered forever.
  second.simulateClose('');
  await new Promise((r) => setImmediate(r));
  await new Promise((r) => setImmediate(r));
  assert.equal(liveTimers().length, 1, 'a genuine drop on the new socket scheduled no retry');
});

// The failure this names: a scanned token that never gets stripped from
// the address bar rides along in a screenshot or a shared link.
test('a scanned token in the hash is stored and the hash is replaced', async () => {
  reset();
  const scanned = 'D'.repeat(43);
  global.location.hash = '#t=' + scanned;
  const calls = [];
  const original = global.history.replaceState;
  global.history.replaceState = (...args) => { calls.push(args); };

  await freshApp();

  global.history.replaceState = original;

  assert.deepEqual(calls[0], [null, '', '#/roster']);
  assert.equal(global.localStorage.getItem('coppice.token'), scanned);
});

import test from 'node:test';
import assert from 'node:assert/strict';
import {
  reset, element, TOKEN, freshApp, lastSocket,
} from './browser-stub.mjs';

// newpane.test.mjs proves the shape createCommand builds. This file drives
// the New screen through a real boot(), the same way screens-connect.test.mjs
// drives the roster and the pane screen, to prove that shape actually
// reaches the socket, that a refusal reaches the operator, and that a
// success moves the phone to the new pane.

// The failure this names: a renamed field, or a broken destructuring
// between createCommand and the socket send, would leave Create silently
// sending the wrong request while every unit test on createCommand itself
// stays green.
test('Create sends pane.create with cwd, label, kind, env, and cmd_argv', async () => {
  reset();
  global.localStorage.setItem('coppice.token', TOKEN);
  global.location.hash = '#/new';
  await freshApp();

  const ws = lastSocket();
  const sent = [];
  ws.send = (data) => { sent.push(JSON.parse(data)); };
  ws.simulateOpen();

  element('new-cwd').value = '/repo';
  element('new-label').value = 'auth fix';
  element('new-kind').value = 'pty';
  element('new-command').value = 'claude --resume';
  element('create').dispatchEvent({ type: 'click' });
  await new Promise((r) => setImmediate(r));

  const create = sent.find((m) => m.cmd === 'pane.create');
  assert.ok(create, 'pane.create was never sent');
  assert.equal(create.cwd, '/repo');
  assert.equal(create.label, 'auth fix');
  assert.equal(create.kind, 'pty');
  assert.deepEqual(create.env, {});
  assert.deepEqual(create.cmd_argv, ['claude', '--resume']);
});

// The failure this names: a pane created in the wrong directory, or a
// server refusal that never reaches the operator, leaves them tapping
// Create again and again for a reason the phone has but never says.
test('a rejected create leaves the refusal in status and the hash unchanged', async () => {
  reset();
  global.localStorage.setItem('coppice.token', TOKEN);
  global.location.hash = '#/new';
  await freshApp();

  const ws = lastSocket();
  const sent = [];
  ws.send = (data) => { sent.push(JSON.parse(data)); };
  ws.simulateOpen();

  element('new-cwd').value = '/repo';
  element('new-kind').value = 'pty';
  element('new-command').value = 'claude';
  element('create').dispatchEvent({ type: 'click' });
  await new Promise((r) => setImmediate(r));

  const create = sent.find((m) => m.cmd === 'pane.create');
  assert.ok(create, 'pane.create was never sent');
  ws.fire('message', {
    data: JSON.stringify({
      id: create.id,
      ok: false,
      error: { message: 'a pty pane needs cmd_argv. Put the command after -- on the command line.' },
    }),
  });
  await new Promise((r) => setImmediate(r));

  assert.equal(element('status').textContent, 'a pty pane needs cmd_argv. Put the command after -- on the command line.');
  assert.equal(global.location.hash, '#/new');
});

// The failure this names: the operator taps Create, a pane really starts,
// and the phone never moves to it, or forgets the directory it just used.
test('a successful create moves the hash to the new pane and remembers the cwd', async () => {
  reset();
  global.localStorage.setItem('coppice.token', TOKEN);
  global.location.hash = '#/new';
  await freshApp();

  const ws = lastSocket();
  const sent = [];
  ws.send = (data) => { sent.push(JSON.parse(data)); };
  ws.simulateOpen();

  element('new-cwd').value = '/repo';
  element('new-kind').value = 'pty';
  element('new-command').value = 'claude';
  element('create').dispatchEvent({ type: 'click' });
  await new Promise((r) => setImmediate(r));

  const create = sent.find((m) => m.cmd === 'pane.create');
  assert.ok(create, 'pane.create was never sent');
  ws.fire('message', { data: JSON.stringify({ id: create.id, ok: true, result: { pane: 'w1:p9' } }) });
  await new Promise((r) => setImmediate(r));

  assert.equal(global.location.hash, '#/pane/w1%3Ap9');
  assert.deepEqual(JSON.parse(global.localStorage.getItem('coppice.cwds')), ['/repo']);
});

// The failure this names: the New screen always shows one label with
// nothing beneath it, and the operator types into the wrong field or
// believes the screen failed to render.
test('the command and harness labels hide with their own controls', async () => {
  reset();
  global.localStorage.setItem('coppice.token', TOKEN);
  await freshApp();

  assert.equal(element('new-command').hidden, false);
  assert.equal(element('new-command-label').hidden, false);
  assert.equal(element('new-harness').hidden, true);
  assert.equal(element('new-harness-label').hidden, true);

  element('new-kind').value = 'headless';
  element('new-kind').dispatchEvent({ type: 'change' });

  assert.equal(element('new-command').hidden, true);
  assert.equal(element('new-command-label').hidden, true);
  assert.equal(element('new-harness').hidden, false);
  assert.equal(element('new-harness-label').hidden, false);
});

import test from 'node:test';
import assert from 'node:assert/strict';
import { reset, element, TOKEN, freshApp, lastSocket } from './browser-stub.mjs';
import { mountFloor, rowFromEvent } from '../floor.js';
import { render, paneRows } from '../roster.js';
import { stateOf, trustFor, trustCommand, isQuestionRefusal, questionMessage, TRUST_YES, TRUST_NO } from '../pane.js';

// A pane on Claude's folder trust screen offers two answers, in a window
// on the floor and on the phone: Trust this folder, and Not now, which
// presses Esc and ends Claude. Both send pane.trust.

const settle = () => new Promise((r) => setImmediate(r));

const DETAIL = 'asks to trust this folder (rule=coppice_first_run_trust region=whole_recent)';

function trustPane(id) {
  return { id, label: id, state: 'blocked', source: 'manifest', detail: DETAIL, harness: 'claude', cwd: '/w/' + id, ts: 1 };
}

test('trustFor sees the trust rule on a blocked pane with no ask, and nothing else', () => {
  assert.equal(trustFor(stateOf(trustPane('a'))), true);
  assert.equal(trustFor(stateOf({ ...trustPane('a'), state: 'idle' })), false);
  assert.equal(trustFor(stateOf({ ...trustPane('a'), detail: 'rule=live_blocked_form region=x' })), false);
  assert.equal(trustFor(stateOf({ ...trustPane('a'), ask: { id: 't1', summary: 'ls' } })), false);
  assert.deepEqual(trustCommand('w1:p1', true), { cmd: 'pane.trust', pane: 'w1:p1', trust: true });
  assert.deepEqual(trustCommand('w1:p1', false), { cmd: 'pane.trust', pane: 'w1:p1', trust: false });
  assert.match(TRUST_NO.title, /ends Claude/);
});

test('the refusal of a prompt to a pane on a question carries a fix that shows the pane', () => {
  const e = new Error('claude-x is waiting on a question on its screen. Answer it first: coppice pane trust w1:p1, or open it.');
  assert.equal(isQuestionRefusal(e), true);
  assert.equal(isQuestionRefusal(new Error('the connection closed')), false);
  const m = questionMessage(e.message, 'w1:p1');
  assert.equal(m.text, e.message);
  assert.deepEqual(m.actions, [{ kind: 'open', label: 'Show the question', pane: 'w1:p1' }]);
});

test('a floor window on the trust screen offers both answers, and each sends pane.trust', async () => {
  reset();
  global.window.innerWidth = 800;
  const root = element('screen-roster');
  root.append(element('roster'), element('roster-empty'));
  const panes = [trustPane('a')];
  render(paneRows(panes, 1));
  const calls = [];
  const state = {
    panes,
    width: 800,
    rpc: async (cmd, fields) => { calls.push({ cmd, ...fields }); return {}; },
    api: async (path, init) => { calls.push({ path, init }); return {}; },
    status: (msg) => { element('status').textContent = msg && typeof msg === 'object' ? msg.text : msg; },
  };
  mountFloor(root, state);
  const bar = root.querySelector('[data-tile="a"] .askbar');
  assert.ok(bar, 'the window shows no bar for the trust screen');
  assert.match(bar.textContent, /asks to trust this folder/);
  const buttons = root.querySelectorAll('[data-tile="a"] .askbar button');
  assert.deepEqual(buttons.map((b) => b.textContent), [TRUST_YES.label, TRUST_NO.label]);
  assert.match(buttons[1].title, /ends Claude/);
  buttons[0].dispatchEvent({ type: 'click' });
  await settle();
  assert.deepEqual(calls.filter((c) => c.cmd === 'pane.trust'), [{ cmd: 'pane.trust', pane: 'a', trust: true }]);
  assert.equal(element('status').textContent, 'Trusted the folder. Claude starts.');
  buttons[1].dispatchEvent({ type: 'click' });
  await settle();
  assert.deepEqual(calls.filter((c) => c.cmd === 'pane.trust').pop(), { cmd: 'pane.trust', pane: 'a', trust: false });
});

test('the phone view of a pane on the trust screen shows the answers, and Trust sends pane.trust', async () => {
  reset();
  global.window.innerWidth = 390;
  global.localStorage.setItem('coppice.token', TOKEN);
  global.location.hash = '#/pane/b';
  await freshApp();
  const ws = lastSocket();
  const sent = [];
  ws.send = (data) => { sent.push(JSON.parse(data)); };
  ws.simulateOpen();
  const list = sent.find((m) => m.cmd === 'pane.list');
  ws.fire('message', { data: JSON.stringify({ id: list.id, ok: true, result: { panes: [trustPane('b')] } }) });
  await settle();
  assert.equal(element('trust').hidden, false, 'the trust answers are hidden');
  assert.equal(element('ask').hidden, true);
  element('trust-yes').dispatchEvent({ type: 'click' });
  await settle();
  const req = sent.filter((m) => m.cmd === 'pane.trust').pop();
  assert.ok(req, 'Trust sent no pane.trust');
  assert.equal(req.pane, 'b');
  assert.equal(req.trust, true);
  assert.equal(element('trust-yes').disabled, true, 'Trust stayed on while its answer was on its way');
  assert.equal(element('trust-no').disabled, true);
  element('trust-no').dispatchEvent({ type: 'click' });
  await settle();
  assert.equal(sent.filter((m) => m.cmd === 'pane.trust').length, 1, 'a second tap sent a second answer');
  ws.fire('message', { data: JSON.stringify({ id: req.id, ok: true, result: { pane: 'b', trusted: true } }) });
  await settle();
  await settle();
  assert.equal(element('trust-yes').disabled, false);
});

test('a state event brings the trust bar to a floor window, and a later event with no detail takes it away', () => {
  reset();
  global.window.innerWidth = 800;
  const root = element('screen-roster');
  root.append(element('roster'), element('roster-empty'));
  const start = { id: 'a', label: 'a', state: 'working', source: 'process', harness: 'claude', cwd: '/w/a', ts: 1 };
  render(paneRows([start], 1));
  const state = { panes: [start], width: 800, rpc: async () => ({}), api: async () => ({}), status: () => {} };
  const floor = mountFloor(root, state);
  assert.equal(root.querySelector('[data-tile="a"] .askbar'), null);
  floor.onEvent({ event: 'state', pane: 'a', state: 'blocked', source: 'manifest', harness: 'claude', ts: 2, detail: DETAIL });
  const bar = root.querySelector('[data-tile="a"] .askbar');
  assert.ok(bar && /asks to trust this folder/.test(bar.textContent), 'no trust bar after the state event');
  floor.onEvent({ event: 'state', pane: 'a', state: 'blocked', source: 'manifest', harness: 'claude', ts: 3 });
  assert.equal(root.querySelector('[data-tile="a"] .askbar'), null);
});

test('a floor window turns both trust buttons off while an answer is on its way', async () => {
  reset();
  global.window.innerWidth = 800;
  const root = element('screen-roster');
  root.append(element('roster'), element('roster-empty'));
  const panes = [trustPane('a')];
  render(paneRows(panes, 1));
  const calls = [];
  let finish;
  const state = {
    panes, width: 800,
    rpc: (cmd, fields) => { calls.push({ cmd, ...fields }); return new Promise((r) => { finish = r; }); },
    api: async () => ({}), status: () => {},
  };
  mountFloor(root, state);
  root.querySelectorAll('[data-tile="a"] .askbar button')[0].dispatchEvent({ type: 'click' });
  const during = root.querySelectorAll('[data-tile="a"] .askbar button');
  assert.deepEqual(during.map((b) => b.disabled), [true, true]);
  during[0].dispatchEvent({ type: 'click' });
  assert.equal(calls.filter((c) => c.cmd === 'pane.trust').length, 1, 'a second click sent a second answer');
  finish({});
  await settle();
  await settle();
  assert.deepEqual(root.querySelectorAll('[data-tile="a"] .askbar button').map((b) => b.disabled), [false, false]);
});

test('the phone says a prompt was queued until the agent is ready', async () => {
  reset();
  global.window.innerWidth = 390;
  global.localStorage.setItem('coppice.token', TOKEN);
  global.location.hash = '#/roster';
  await freshApp();
  const ws = lastSocket();
  const sent = [];
  ws.send = (data) => { sent.push(JSON.parse(data)); };
  ws.simulateOpen();
  const list = sent.find((m) => m.cmd === 'pane.list');
  const row = { id: 'b', label: 'claude-b', state: 'working', source: 'process', harness: 'claude', cwd: '/w/b', ts: 1 };
  ws.fire('message', { data: JSON.stringify({ id: list.id, ok: true, result: { panes: [row] } }) });
  await settle();
  global.location.hash = '#/pane/b';
  global.window.dispatchEvent(new global.CustomEvent('coppice:route'));
  await settle();
  element('text').value = 'Reply with pong.';
  element('send-enter').dispatchEvent({ type: 'click' });
  await settle();
  const req = sent.filter((m) => m.cmd === 'pane.send_text').pop();
  assert.ok(req, 'Enter sent no pane.send_text');
  ws.fire('message', { data: JSON.stringify({ id: req.id, ok: true, result: { sent: 0, queued: 1, note: 'queued until claude-b is ready' } }) });
  await settle();
  await settle();
  assert.equal(element('status').textContent, 'Queued until claude-b is ready. It goes by itself.');
});

test('rowFromEvent copies the detail, and clears it when the event has none', () => {
  const row = rowFromEvent({ id: 'a', detail: 'old' }, { state: 'blocked', detail: DETAIL });
  assert.equal(row.detail, DETAIL);
  assert.equal(rowFromEvent(row, { state: 'idle' }).detail, '');
});

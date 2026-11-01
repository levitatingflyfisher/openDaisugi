import test from 'node:test';
import assert from 'node:assert/strict';
import { reset, element } from './browser-stub.mjs';
import { render, paneRows, needKind, needsList, setFloor } from '../roster.js';
import { needsYou, markAnswered, trustFor, forgetAnswered } from '../pane.js';

// The "Needs you" cards: an agent that waits on the owner shows at the top of
// the floor, on a phone and on the wide page, with its name, the one-line ask
// and a way to answer from the card. A card goes when it is answered.

const TRUST_DETAIL = 'asks to trust this folder (rule=coppice_first_run_trust region=whole_recent)';

function raw(id, state, extra = {}) {
  return { id, label: id, state, harness: 'claude', cwd: '/w/' + id, ts: 1, source: 'manifest', ...extra };
}

function stub() {
  reset();
  setFloor(null);
  element('roster');
  element('needs');
  element('roster-empty');
  const calls = [];
  global.window.coppice = {
    rpc: async (cmd, fields) => { calls.push({ cmd, ...fields }); return {}; },
    api: async () => ({}),
    status: (msg) => { element('status').textContent = msg; },
    state: {},
  };
  return calls;
}

const click = (b) => b.dispatchEvent({ type: 'click', stopPropagation() {} });
const buttons = (card) => card.querySelectorAll('button');
const byText = (card, text) => buttons(card).find((b) => b.textContent === text);
const tick = async () => { for (let i = 0; i < 6; i += 1) await Promise.resolve(); };

test('what waits on the owner: a permission ask, a trust screen, a screen block, a question', () => {
  const rows = paneRows([
    raw('a', 'blocked', { ask: { id: 'k1', tool: 'Bash', summary: 'rm x', tier: 'undoable' }, source: 'warden' }),
    raw('t', 'blocked', { detail: TRUST_DETAIL }),
    raw('s', 'blocked'),
    raw('q', 'idle', { last_ask: 'Want me to push it?' }),
    raw('i', 'idle'),
    raw('w', 'working', { last_ask: 'a stale question' }),
  ], 10);
  assert.deepEqual(rows.map(needKind), ['permission', 'trust', 'screen', 'question', '', '']);
  assert.deepEqual(needsList(rows).map((r) => r.id), ['a', 't', 's', 'q']);
});

test('the cards are in the needs list on a wide page and on a phone', () => {
  for (const phone of [false, true]) {
    stub();
    render(paneRows([raw('q', 'idle', { last_ask: 'Which file?' }), raw('w', 'working')], 10), { phone });
    const cards = element('needs').children;
    assert.deepEqual(cards.map((c) => c.dataset.pane), ['q'], 'phone ' + phone);
    assert.equal(element('needs').hidden, false);
    assert.equal(element('queue-title').textContent, '1 needs you');
    assert.equal(element('roster').querySelectorAll('.row').length, 2, 'every agent stays a row');
  }
});

test('a question card shows the ask and sends the answer to the agent, then goes', async () => {
  const calls = stub();
  const rows = () => paneRows([raw('q', 'idle', { last_ask: 'Want me to push it?' })], 10);
  render(rows());
  const card = element('needs').querySelector('[data-pane="q"]');
  assert.ok(card.textContent.includes('Want me to push it?'));
  const input = card.querySelector('input');
  assert.ok(input, 'the card is not typeable');
  click(byText(card, 'Send'));
  await tick();
  assert.deepEqual(calls, [], 'an empty answer was sent');
  assert.equal(element('status').textContent, 'Type an answer first.');
  input.value = 'yes, push';
  click(byText(card, 'Send'));
  await tick();
  assert.deepEqual(calls, [{ cmd: 'agent.prompt', pane: 'q', text: 'yes, push' }]);
  assert.equal(element('status').textContent, 'Sent to q.');
  render(rows());
  assert.equal(element('needs').children.length, 0, 'the answered card stayed');
  assert.equal(element('needs').hidden, true);
});

test('an answered question comes back only when the agent asks something else', () => {
  stub();
  const row = (line) => ({ id: 'q', state: 'idle', last_ask: line });
  assert.equal(needsYou(row('One?')), true);
  markAnswered(row('One?'));
  assert.equal(needsYou(row('One?')), false);
  assert.equal(needsYou(row('Two?')), true);
});

test('a failed answer keeps the card and says why', async () => {
  const calls = stub();
  global.window.coppice.rpc = async (cmd, fields) => { calls.push({ cmd, ...fields }); throw new Error('q is waiting on a question on its screen.'); };
  render(paneRows([raw('f', 'idle', { last_ask: 'Which?' })], 10));
  const card = element('needs').querySelector('[data-pane="f"]');
  card.querySelector('input').value = 'a';
  const send = byText(card, 'Send');
  click(send);
  await tick();
  assert.equal(element('status').textContent, 'q is waiting on a question on its screen.');
  assert.equal(send.disabled, false, 'the Send control stays dead after a failure');
  render(paneRows([raw('f', 'idle', { last_ask: 'Which?' })], 10));
  assert.equal(element('needs').children.length, 1);
});

test('a trust card asks to trust the folder and answers over pane.trust', async () => {
  const calls = stub();
  const rows = paneRows([raw('t', 'blocked', { detail: TRUST_DETAIL })], 10);
  assert.equal(trustFor({ state: 'blocked', detail: TRUST_DETAIL }), true);
  render(rows);
  const card = element('needs').querySelector('[data-pane="t"]');
  assert.ok(card.textContent.includes('Asks to trust this folder.'));
  assert.ok(card.textContent.includes('/w/t'));
  assert.deepEqual(buttons(card).map((b) => b.textContent).filter((x) => x !== '×'), ['Trust this folder', 'Not now', 'Open']);
  const yes = byText(card, 'Trust this folder');
  click(yes);
  assert.equal(yes.disabled, true, 'a second tap would send the answer twice');
  await tick();
  assert.deepEqual(calls, [{ cmd: 'pane.trust', pane: 't', trust: true }]);
  assert.equal(element('status').textContent, 'Trusted the folder for t.');
});

test('Not now on a trust card says it ends the agent', async () => {
  const calls = stub();
  render(paneRows([raw('t', 'blocked', { detail: TRUST_DETAIL })], 10));
  click(byText(element('needs').querySelector('[data-pane="t"]'), 'Not now'));
  await tick();
  assert.deepEqual(calls, [{ cmd: 'pane.trust', pane: 't', trust: false }]);
  assert.equal(element('status').textContent, 'Told t not now. It ends.');
});

test('a block with no ask to answer here has an Open control that puts the agent in a window', () => {
  stub();
  const filled = [];
  setFloor({ canShow: () => true, fillRow: (id) => filled.push(id), railSelected: () => '', killPending: () => '', editing: () => null, windowOf: () => 0, folded: () => new Set(), tasks: () => [], isGone: () => false });
  try {
    render(paneRows([raw('s', 'blocked')], 10));
    const card = element('needs').querySelector('[data-pane="s"]');
    assert.ok(card.textContent.includes('Open it to answer'));
    click(byText(card, 'Open'));
    assert.deepEqual(filled, ['s']);
  } finally {
    setFloor(null);
  }
});

test('text typed in one card survives another card coming and going', () => {
  stub();
  const q = raw('q', 'idle', { last_ask: 'Which file?' });
  render(paneRows([q], 10));
  const card = element('needs').querySelector('[data-pane="q"]');
  card.querySelector('input').value = 'half an ans';
  render(paneRows([q, raw('t', 'blocked', { detail: TRUST_DETAIL })], 10));
  assert.equal(element('needs').children.length, 2);
  const again = element('needs').querySelector('[data-pane="q"]');
  assert.equal(again, card, 'the question card was drawn again');
  assert.equal(again.querySelector('input').value, 'half an ans');
  render(paneRows([q], 10));
  assert.equal(element('needs').querySelector('[data-pane="q"]'), card);
});

test('an answered question counts again once the agent has worked and asks the same line', async () => {
  stub();
  const q = () => paneRows([raw('q', 'idle', { last_ask: 'Which file?' })], 10);
  render(q());
  const card = element('needs').querySelector('[data-pane="q"]');
  card.querySelector('input').value = 'a.txt';
  click(byText(card, 'Send'));
  await tick();
  render(q());
  assert.equal(element('needs').children.length, 0);
  // The roster calls this on a working state event for the pane.
  forgetAnswered('q');
  assert.equal(needKind(q()[0]), 'question');
});

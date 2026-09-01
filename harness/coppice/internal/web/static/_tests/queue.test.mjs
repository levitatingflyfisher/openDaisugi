import test from 'node:test';
import assert from 'node:assert/strict';
import { reset, element } from './browser-stub.mjs';
import { render, paneRows, queueTitle, queueSub, queueCounts, setFloor } from '../roster.js';

// The attention queue: on a phone the roster is the home screen. Asks come
// first as cards, each with who asks, the summary in code, the gate line,
// and Deny first. Working rows follow, compact, and done rows come last.

function p(id, state, ask) {
  return { id, label: id, state, harness: 'claude', cwd: '/w/one', ts: 1, ask };
}

function ask(id, extra = {}) {
  return { id: 'toolu_' + id, tool: 'Bash', summary: 'rm -rf ' + id, tier: 'undoable', ...extra };
}

const PANES = [
  p('w1', 'working'),
  p('b1', 'blocked', ask('b1', { gate: { verdict: 'deny', rule: 3 } })),
  p('w2', 'working'),
  p('d1', 'done'),
  p('b2', 'blocked', ask('b2', { tier: 'permanent' })),
  p('w3', 'working'),
];

const classes = (el) => String(el.className || '').split(/\s+/);

test('two blocked panes render two ask cards before any row', () => {
  reset();
  render(paneRows(PANES, 10));
  const kids = element('roster').children;
  assert.equal(element('roster').querySelectorAll('.ask').length, 2);
  const kinds = kids.map((k) => (classes(k).includes('ask') ? 'ask' : classes(k).includes('row') ? 'row' : '?'));
  assert.deepEqual(kinds, ['ask', 'ask', 'row', 'row', 'row', 'row']);
  assert.deepEqual(kids.map((k) => k.dataset.pane), ['b1', 'b2', 'w1', 'w2', 'w3', 'd1'], 'done rows are not last');
  for (const card of kids.slice(0, 2)) {
    assert.equal(card.querySelectorAll('.row').length, 0, 'an ask card holds a row');
  }
});

test('the first button in every ask card is Deny', () => {
  reset();
  render(paneRows(PANES, 10));
  for (const card of element('roster').querySelectorAll('.ask')) {
    const first = card.querySelectorAll('button')[0];
    assert.ok(first, 'an ask card has no button');
    assert.equal(first.textContent, 'Deny');
  }
});

test('the queue head says how many need you and how many work', () => {
  reset();
  render(paneRows(PANES, 10));
  assert.equal(element('queue-title').textContent, '2 need you');
  assert.equal(element('queue-sub').textContent, '3 working · 1 done');
  assert.equal(element('queue-head').hidden, false);
  assert.equal(queueTitle({ blocked: 1 }), '1 needs you');
  assert.equal(queueSub(queueCounts(paneRows([p('a', 'working'), p('b', 'idle')], 1))), '1 working · 1 idle');
});

test('an ask card names who asks, shows the summary in code, and the gate line', () => {
  reset();
  render(paneRows(PANES, 10));
  const card = element('roster').querySelector('[data-pane="b1"]');
  assert.ok(card.querySelector('.who').textContent.includes('b1'));
  assert.ok(card.querySelector('.who').textContent.includes('claude'));
  const code = card.querySelector('code');
  assert.ok(code, 'the summary is not in code');
  assert.equal(code.textContent, 'rm -rf b1');
  assert.equal(card.querySelector('.verdict').textContent, 'The gate says no, rule 3');
  const other = element('roster').querySelector('[data-pane="b2"]');
  assert.equal(other.querySelector('.verdict'), null, 'an ask with no gate verdict drew a gate line');
});

test('a working row is compact: its name, its state word, and no answer controls', () => {
  reset();
  render(paneRows(PANES, 10));
  const row = element('roster').querySelector('[data-pane="w1"]');
  assert.ok(classes(row).includes('row'));
  assert.equal(row.dataset.row, 'w1');
  assert.ok(row.textContent.includes('working'));
  assert.equal(row.querySelectorAll('button').length, 0);
});

test('Look on an ask card opens that pane at its ask', () => {
  reset();
  global.location.hash = '#/roster';
  render(paneRows(PANES, 10));
  const card = element('roster').querySelector('[data-pane="b1"]');
  const look = card.querySelectorAll('button').find((b) => b.textContent === 'Look');
  look.dispatchEvent({ type: 'click', stopPropagation() {} });
  assert.equal(global.location.hash, '#/pane/b1?ask=toolu_b1');
});

// On the wide floor the page is one screen: Look puts the agent in a
// window with the keys and never leaves for the pane screen.
test('Look on an ask card on the wide floor puts the agent in a window', () => {
  reset();
  global.location.hash = '#/roster';
  const filled = [];
  setFloor({ canShow: () => true, fillRow: (id) => filled.push(id) });
  try {
    render(paneRows(PANES, 10));
    const card = element('roster').querySelector('[data-pane="b1"]');
    const look = card.querySelectorAll('button').find((b) => b.textContent === 'Look');
    look.dispatchEvent({ type: 'click', stopPropagation() {} });
    assert.deepEqual(filled, ['b1']);
    assert.equal(global.location.hash, '#/roster');
  } finally {
    setFloor(null);
  }
});

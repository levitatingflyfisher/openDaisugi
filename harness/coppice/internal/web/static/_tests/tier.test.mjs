import test from 'node:test';
import assert from 'node:assert/strict';
import { reset, element } from './browser-stub.mjs';
import { mountFloor } from '../floor.js';
import { render, paneRows, setFloor } from '../roster.js';
import { askFor, stateOf, tierOf, paneName } from '../pane.js';

// Three shapes of ask on the floor page and the phone. Deny is always the
// first control and one tap. Allow is one tap only on an undoable ask. A
// permanent ask shows a field for the pane name instead of Allow.

const settle = () => new Promise((r) => setImmediate(r));

function p(id, tier) {
  const ask = { id: 'toolu_' + id, tool: 'Bash', summary: 'git push --force' };
  if (tier !== undefined) ask.tier = tier;
  return { id, label: 'gate-refactor', state: 'blocked', harness: 'claude', cwd: '/w/' + id, ts: 1, ask };
}

function floorStub(panes) {
  reset();
  global.window.innerWidth = 1500;
  const root = element('screen-roster');
  root.append(element('roster'), element('roster-empty'));
  render(paneRows(panes, 1));
  const calls = [];
  const state = {
    panes,
    width: 1500,
    rpc: async (cmd, fields) => { calls.push({ cmd, ...fields }); return {}; },
    api: async (path, init) => { calls.push({ path, body: JSON.parse(init.body) }); return {}; },
    status: (msg) => { element('status').textContent = msg; },
  };
  const doc = { body: root, querySelector: (s) => root.querySelector(s) };
  mountFloor(root, state);
  return { doc, calls };
}

// cardStub paints the phone roster with window.coppice recording answers.
// cardStub draws phone cards: no floor with windows, so a card's Look
// opens the pane screen.
function cardStub(panes) {
  reset();
  setFloor(null);
  element('roster');
  element('roster-empty');
  const calls = [];
  global.window.coppice = {
    api: async (path, init) => { calls.push({ path, body: JSON.parse(init.body) }); return {}; },
    status: (msg) => { element('status').textContent = msg; },
    state: {},
  };
  render(paneRows(panes, 1));
  return { list: element('roster'), calls };
}

// The x that stops an agent sits on every card and is not an answer.
const answers = (nodes) => nodes.filter((n) => n.tagName === 'BUTTON' && !String(n.className).split(/\s+/).includes('kill'));
const labels = (nodes) => answers(nodes).map((b) => b.textContent);

test('a missing or unknown tier is permanent', () => {
  assert.equal(tierOf({ tier: 'undoable' }), 'undoable');
  assert.equal(tierOf({ tier: 'permanent' }), 'permanent');
  assert.equal(tierOf({}), 'permanent');
  assert.equal(tierOf({ tier: 'silent' }), 'permanent');
  assert.equal(tierOf(null), 'permanent');
  assert.equal(askFor(stateOf(p('a'))).tier, 'permanent');
  assert.equal(askFor(stateOf(p('a', 'undoable'))).tier, 'undoable');
});

test('a pane is named by its label, else its id', () => {
  assert.equal(paneName({ id: 'w1:p1', label: 'gate-refactor' }), 'gate-refactor');
  assert.equal(paneName({ id: 'w1:p1', label: '' }), 'w1:p1');
});

test('floor: a permanent ask shows Deny first, a name field, and no Allow', () => {
  const { doc } = floorStub([p('a', 'permanent')]);
  const bar = doc.querySelector('[data-tile="a"] .askbar');
  assert.ok(bar, 'no ask bar');
  const buttons = labels(bar.querySelectorAll('button'));
  assert.equal(buttons[0], 'Deny');
  assert.ok(!buttons.includes('Allow'), 'a permanent ask offered Allow: ' + buttons);
  const input = bar.querySelector('input');
  assert.ok(input, 'no name field');
  assert.ok(bar.textContent.includes('This cannot be undone'));
});

test('floor: an ask with no tier reads as permanent', () => {
  const { doc } = floorStub([p('a')]);
  const bar = doc.querySelector('[data-tile="a"] .askbar');
  assert.ok(!labels(bar.querySelectorAll('button')).includes('Allow'));
  assert.ok(bar.querySelector('input'));
});

test('floor: an undoable ask shows Deny first and Allow', () => {
  const { doc } = floorStub([p('a', 'undoable')]);
  const bar = doc.querySelector('[data-tile="a"] .askbar');
  const buttons = labels(bar.querySelectorAll('button'));
  assert.equal(buttons[0], 'Deny');
  assert.ok(buttons.includes('Allow'));
  assert.equal(bar.querySelector('input'), null);
});

test('floor: Allow on an undoable ask posts the pane and no confirm', async () => {
  const { doc, calls } = floorStub([p('a', 'undoable')]);
  const allow = doc.querySelector('[data-tile="a"] .askbar').querySelectorAll('button').find((b) => b.textContent === 'Allow');
  allow.dispatchEvent({ type: 'click' });
  await settle();
  const posts = calls.filter((c) => c.path === '/api/ask/answer');
  assert.equal(posts.length, 1);
  assert.equal(posts[0].body.decision, 'allow');
  assert.equal(posts[0].body.tool_use_id, 'toolu_a');
  assert.equal(posts[0].body.pane, 'a');
  assert.equal(posts[0].body.confirm, undefined);
});

test('floor: the name then Enter allows a permanent ask, and a wrong name posts nothing', async () => {
  const { doc, calls } = floorStub([p('a', 'permanent')]);
  const input = doc.querySelector('[data-tile="a"] .askbar').querySelector('input');
  input.value = 'gate-refactr';
  input.dispatchEvent({ type: 'keydown', key: 'Enter', stopPropagation() {}, preventDefault() {} });
  await settle();
  assert.equal(calls.filter((c) => c.path).length, 0, 'a wrong name posted');
  assert.ok(element('status').textContent.includes('not the pane name'));
  input.value = 'gate-refactor';
  input.dispatchEvent({ type: 'keydown', key: 'Enter', stopPropagation() {}, preventDefault() {} });
  await settle();
  const posts = calls.filter((c) => c.path === '/api/ask/answer');
  assert.equal(posts.length, 1);
  assert.equal(posts[0].body.decision, 'allow');
  assert.equal(posts[0].body.confirm, 'gate-refactor');
});

test('phone card: a permanent ask shows Deny, Look, and a name field, no Allow', () => {
  const { list } = cardStub([p('a', 'permanent')]);
  const card = list.querySelector('[data-pane="a"]');
  const buttons = labels(card.querySelectorAll('button'));
  assert.deepEqual(buttons, ['Deny', 'Look']);
  assert.ok(card.querySelector('input'), 'no name field');
  assert.ok(card.querySelector('.deny').className.includes('big'));
});

test('phone card: an undoable ask shows Deny, Look, Allow', () => {
  const { list } = cardStub([p('a', 'undoable')]);
  const card = list.querySelector('[data-pane="a"]');
  assert.deepEqual(labels(card.querySelectorAll('button')), ['Deny', 'Look', 'Allow']);
  assert.equal(card.querySelector('input'), null);
});

test('phone card: Deny posts a deny and Look opens the pane at its ask', async () => {
  const { list, calls } = cardStub([p('a', 'permanent')]);
  const card = list.querySelector('[data-pane="a"]');
  const [deny, look] = answers(card.querySelectorAll('button'));
  let stopped = 0;
  const ev = { type: 'click', stopPropagation() { stopped += 1; } };
  deny.dispatchEvent(ev);
  await settle();
  assert.equal(calls.length, 1);
  assert.deepEqual(calls[0].body, { tool_use_id: 'toolu_a', decision: 'deny', pane: 'a', reason: 'denied from the phone' });
  look.dispatchEvent(ev);
  assert.equal(global.location.hash, '#/pane/a?ask=toolu_a');
  assert.equal(stopped, 2, 'a card button let its click reach the card');
});

test('phone card: a pane with no ask has no answer controls', () => {
  const { list } = cardStub([{ id: 'b', label: 'b', state: 'working', ts: 1 }]);
  const card = list.querySelector('[data-pane="b"]');
  assert.equal(answers(card.querySelectorAll('button')).length, 0);
});

test('floor: typed text in the name field survives a repaint of the same ask', () => {
  const { doc } = floorStub([p('a', 'permanent')]);
  const input = doc.querySelector('[data-tile="a"] .askbar').querySelector('input');
  input.value = 'gate-ref';
  input.dispatchEvent({ type: 'input' });
  const again = floorStub([p('a', 'permanent')]).doc.querySelector('[data-tile="a"] .askbar').querySelector('input');
  assert.equal(again.value, 'gate-ref');
  const other = floorStub([{ ...p('b', 'permanent') }]).doc.querySelector('[data-tile="b"] .askbar').querySelector('input');
  assert.equal(other.value, '', 'text typed for one ask showed on another');
});

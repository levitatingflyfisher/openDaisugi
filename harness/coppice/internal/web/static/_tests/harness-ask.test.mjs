import test from 'node:test';
import assert from 'node:assert/strict';
import { reset, element } from './browser-stub.mjs';
import { askFor, heldAnswer, HELD_BY_HARNESS } from '../pane.js';
import { render, paneRows } from '../roster.js';

// A harness can hold an ask itself, such as OpenCode's permission prompt.
// The gate has no file for it, so the page answers it through
// coppice-server with agent.allow or agent.deny over its own operator
// connection. A shell the page starts is a pane, and a pane cannot allow.

function heldPane(id) {
  return {
    id, label: id, state: 'blocked', harness: 'opencode', cwd: '/w/' + id, ts: 1,
    ask: { id: 'per_1', tool: 'bash', summary: 'rm -rf build', tier: 'permanent', holder: 'harness' },
  };
}

test('askFor marks an ask a harness holds', () => {
  const st = { state: 'blocked', ask: heldPane('o1').ask };
  assert.equal(askFor(st).held, true);
  assert.equal(askFor({ state: 'blocked', ask: { id: 'toolu_1' } }).held, false);
});

test('a held ask is answered with agent.allow or agent.deny, and a permanent allow carries the name', () => {
  assert.deepEqual(heldAnswer('w1:p1', 'per_1', 'deny'),
    { cmd: 'agent.deny', fields: { pane: 'w1:p1', ask: 'per_1', reason: 'denied from the phone' } });
  assert.deepEqual(heldAnswer('w1:p1', 'per_1', 'allow', 'w1:p1'),
    { cmd: 'agent.allow', fields: { pane: 'w1:p1', ask: 'per_1', reason: 'allowed from the phone', confirm: 'w1:p1' } });
});

test('a phone card for a harness-held ask offers Deny, Look and the name field, and sends the answer over the operator socket', async () => {
  reset();
  const calls = [];
  const apiCalls = [];
  globalThis.window.coppice = {
    ...(globalThis.window.coppice || {}),
    rpc: async (cmd, fields) => { calls.push([cmd, fields]); return {}; },
    api: async (path) => { apiCalls.push(path); return {}; },
    status: () => {},
  };
  render(paneRows([heldPane('o1')], 10));
  const card = element('roster').querySelector('[data-pane="o1"]');
  const labels = card.querySelectorAll('button').map((b) => b.textContent);
  assert.ok(labels.includes('Deny') && labels.includes('Look'), labels.join(','));
  assert.ok(!labels.some((l) => l.includes('shell')), labels.join(','));
  assert.ok(card.textContent.includes(HELD_BY_HARNESS));
  assert.ok(!card.textContent.includes('gone'));
  card.querySelectorAll('button').find((b) => b.textContent === 'Deny').dispatchEvent({ type: 'click' });
  for (let i = 0; i < 5; i += 1) await Promise.resolve();
  assert.deepEqual(calls, [['agent.deny', { pane: 'o1', ask: 'per_1', reason: 'denied from the phone' }]]);
  assert.deepEqual(apiCalls, []);
});

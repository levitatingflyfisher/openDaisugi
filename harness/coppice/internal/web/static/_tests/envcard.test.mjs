import test from 'node:test';
import assert from 'node:assert/strict';
import { reset, element } from './browser-stub.mjs';
import { envelopeCard, mountEnvelopes, runLine, reachLine, budgetLine } from '../envcard.js';

const P = {
  project: 'trellis', task: 'build trellis', at: '2026-11-05T10:00:00Z', replaces: false,
  read: ['/src/trellis'], write: ['/src/trellis/lib'],
  run: { shell: true, allow: ['go test'], mcp: [] }, reach: { network: true, hosts: ['pub.dev'] },
  tokens: 50000, turns: 30,
};

const text = (el) => {
  let out = el.textContent || '';
  for (const c of el.children || []) out += ' ' + text(c);
  return out;
};

test('the lines say what may run and reach, and the budget', () => {
  assert.equal(runLine({ shell: true, allow: ['go test'] }), 'shell: go test');
  assert.equal(runLine({ shell: true, allow: [] }), 'any shell command');
  assert.equal(runLine({ shell: false }), 'nothing');
  assert.equal(reachLine({ network: false }), 'no network');
  assert.equal(reachLine({ network: true, hosts: [] }), 'any host');
  assert.equal(budgetLine({ tokens: 0, turns: 0 }), 'no budget set');
  assert.equal(budgetLine({ tokens: 50000, turns: 30 }), '50,000 tokens, 30 turns');
});

test('a card shows the project, what it may do, and Approve and Decline that answer', () => {
  reset();
  const got = [];
  const li = envelopeCard(P, { approve: (p) => got.push('approve ' + p.project), decline: (p) => got.push('decline ' + p.project) });
  const t = text(li);
  for (const want of ['Envelope for trellis', '/src/trellis', '/src/trellis/lib', 'go test', 'pub.dev', '50,000 tokens']) {
    assert.ok(t.includes(want), want + ' missing from: ' + t);
  }
  const buttons = [];
  const walk = (e) => { if (e.tagName === 'BUTTON') buttons.push(e); for (const c of e.children || []) walk(c); };
  walk(li);
  assert.deepEqual(buttons.map((b) => b.textContent), ['Approve', 'Decline']);
  buttons[0].dispatchEvent({ type: 'click' });
  buttons[1].dispatchEvent({ type: 'click' });
  assert.deepEqual(got, ['approve trellis', 'decline trellis']);
});

test('a card says how the proposal is wider than the default, and what stays denied', () => {
  reset();
  const li = envelopeCard({ ...P, wider: ['network: pub.dev', 'a budget: 50000 tokens, 30 turns'], denied: ['git push', 'stripe'] },
    { approve: () => {}, decline: () => {} });
  const t = text(li);
  for (const want of ['Asks for more than the default', 'network: pub.dev', 'a budget: 50000 tokens, 30 turns', 'Still denied', 'git push, stripe']) {
    assert.ok(t.includes(want), want + ' missing from: ' + t);
  }
});

test('the list shows pending cards, answers with the proposal it read, and reads again', async () => {
  reset();
  const el = element('envelopes');
  const calls = [];
  let pending = [P];
  const rpc = async (cmd, f) => {
    calls.push([cmd, f]);
    if (cmd === 'envelope.pending') return { proposals: pending };
    pending = [];
    return {};
  };
  const m = mountEnvelopes({ el, rpc, connected: () => true });
  await m.read();
  assert.equal(el.hidden, false);
  assert.equal(el.children.length, 1);
  const first = el.children[0];
  await m.read();
  assert.equal(el.children[0], first, 'the same card was drawn again');
  const approve = [];
  const walk = (e) => { if (e.tagName === 'BUTTON' && e.textContent === 'Approve') approve.push(e); for (const c of e.children || []) walk(c); };
  walk(first);
  approve[0].dispatchEvent({ type: 'click' });
  await new Promise((r) => setImmediate(r));
  await new Promise((r) => setImmediate(r));
  assert.deepEqual(calls.find((c) => c[0] === 'envelope.approve'), ['envelope.approve', { project: 'trellis', at: P.at }]);
  assert.equal(el.hidden, true, 'the answered card stays');
});

test('a refusal leaves the list empty and hidden', async () => {
  reset();
  const el = element('envelopes');
  const m = mountEnvelopes({ el, rpc: async () => { throw new Error('no'); }, connected: () => true });
  await m.read();
  assert.equal(el.hidden, true);
});

test('a card shows an Also line for the fields that widen', () => {
  reset();
  const li = envelopeCard({ ...P, also: ['compound shell commands allowed, each command checked', 'limits: 900 s per step, 10 MB output'] }, { approve() {}, decline() {} });
  const t = text(li);
  assert.ok(t.includes('Also'), t);
  assert.ok(t.includes('compound shell commands allowed'), t);
  assert.ok(t.includes('900 s per step'), t);
  const none = text(envelopeCard(P, { approve() {}, decline() {} }));
  assert.ok(!none.includes('Also'), 'no Also row without lines: ' + none);
});

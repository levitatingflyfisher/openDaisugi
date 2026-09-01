import test from 'node:test';
import assert from 'node:assert/strict';
import { reset, element, TOKEN, freshApp, lastSocket } from './browser-stub.mjs';

// The lock-screen card opens the app cold at #/pane/<id>?ask=<ask id>. Once
// the pane list lands, the ask box is marked and brought into view. An ask
// the pane no longer shows is named as gone, and no other ask is marked.

const settle = () => new Promise((r) => setImmediate(r));

function p(id, state, ask) {
  return { id, label: id, state, harness: 'claude', cwd: '/w/' + id, ts: 1, ask };
}

async function bootAt(hash, panes) {
  reset();
  global.window.innerWidth = 390;
  global.localStorage.setItem('coppice.token', TOKEN);
  global.location.hash = hash;
  const scrolled = [];
  element('ask').scrollIntoView = () => scrolled.push(true);
  await freshApp();
  const ws = lastSocket();
  const sent = [];
  ws.send = (data) => { sent.push(JSON.parse(data)); };
  ws.simulateOpen();
  const list = sent.find((m) => m.cmd === 'pane.list');
  ws.fire('message', { data: JSON.stringify({ id: list.id, ok: true, result: { panes } }) });
  await settle();
  return { scrolled, sent };
}

test('a cold link to an ask marks the ask box once the pane list lands', async () => {
  const { scrolled, sent } = await bootAt('#/pane/b?ask=t1', [p('a', 'working'), p('b', 'blocked', { id: 't1', summary: 'ls', tier: 'undoable' })]);
  assert.equal(element('screen-pane').hidden, false);
  assert.equal(element('ask').hidden, false);
  assert.match(element('ask').className, /\bhere\b/);
  assert.equal(scrolled.length, 1);
  const attach = sent.filter((m) => m.cmd === 'pane.attach').pop();
  assert.equal(attach.pane, 'b', 'the ask id rode into the pane id');
});

test('a link to an ask the pane no longer shows says it is gone and marks nothing', async () => {
  const { scrolled } = await bootAt('#/pane/b?ask=old', [p('b', 'blocked', { id: 't2', summary: 'ls' })]);
  assert.doesNotMatch(String(element('ask').className), /\bhere\b/);
  assert.equal(scrolled.length, 0);
  assert.match(element('status').textContent, /That ask is gone/);
});

test('an ask the link found and the operator then answered is not called gone', async () => {
  await bootAt('#/pane/b?ask=t1', [p('b', 'blocked', { id: 't1', summary: 'ls', tier: 'undoable' })]);
  const ws = lastSocket();
  element('status').textContent = 'Denied.';
  ws.fire('message', { data: JSON.stringify({ event: 'state', pane: 'b', state: 'working', ts: 2 }) });
  await settle();
  assert.equal(element('status').textContent, 'Denied.');
  assert.doesNotMatch(String(element('ask').className), /\bhere\b/);
});

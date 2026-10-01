import test from 'node:test';
import assert from 'node:assert/strict';
import { reset, element } from './browser-stub.mjs';
import { importView, mountImportBar, mountChat } from '../chat.js';

// The import bar (FM-R-71): the owner's choice to bring old Claude Code and
// Codex chats into the foreman. Offered once with a yes and a no, a pause
// while it runs, a resume while it is paused, nothing when it is declined
// or done. The page never starts it by itself.

const settle = () => new Promise((r) => setImmediate(r));
const off = { state: 'off', sessions: 3, messages: 120, estimating: false, done: 0, total: 0 };

test('the offer names the chats and the messages, with a yes and a no', () => {
  const v = importView(off);
  assert.equal(v.text, 'Import 3 past chats (about 120 messages)?');
  assert.deepEqual(v.buttons.map((b) => b.action), ['accept', 'decline']);
  assert.equal(importView({ ...off, sessions: 1, messages: 1 }).text, 'Import 1 past chat (about 1 message)?');
});

test('an accept that found nothing says so, with no button', () => {
  const v = importView({ ...off, note: 'nothing new to import' });
  assert.equal(v.text, 'Nothing new to import.');
  assert.deepEqual(v.buttons, []);
});

test('nothing is offered while the scan runs, when there is nothing, or after a no', () => {
  assert.equal(importView({ ...off, estimating: true }), null);
  assert.equal(importView({ ...off, sessions: 0, messages: 0 }), null);
  assert.equal(importView({ ...off, state: 'declined' }), null);
  assert.equal(importView({ state: 'done', done: 5, total: 5 }), null);
  assert.equal(importView(null), null);
  assert.equal(importView({}), null);
});

test('a running import shows done of total and a pause; a paused one a resume', () => {
  const run = importView({ state: 'running', done: 12, total: 120 });
  assert.equal(run.text, 'Importing past chats: 12 of 120 messages.');
  assert.deepEqual(run.buttons.map((b) => b.action), ['pause']);
  const held = importView({ state: 'paused', done: 12, total: 120 });
  assert.equal(held.text, 'Import paused: 12 of 120 messages.');
  assert.deepEqual(held.buttons.map((b) => b.action), ['resume']);
  assert.equal(importView({ state: 'paused', done: 0, total: 0 }), null);
});

function bar(replies) {
  reset();
  const el = element('import-bar');
  const calls = [];
  const rpc = async (cmd, fields) => {
    calls.push({ cmd, ...fields });
    const r = replies[fields.action];
    if (r instanceof Error) throw r;
    return r;
  };
  return { m: mountImportBar(el, rpc), el, calls };
}

test('the bar is hidden until the server offers, and a click sends its action', async () => {
  const { m, el, calls } = bar({ status: off, accept: { state: 'running', done: 0, total: 120 } });
  assert.equal(el.hidden, true);
  await m.refresh();
  assert.equal(el.hidden, false);
  assert.match(el.textContent, /Import 3 past chats/);
  const yes = el.querySelector('.import-accept');
  yes.dispatchEvent({ type: 'click' });
  await settle();
  await settle();
  assert.deepEqual(calls, [{ cmd: 'floor.import', action: 'status' }, { cmd: 'floor.import', action: 'accept' }]);
  assert.match(el.textContent, /Importing past chats: 0 of 120 messages/);
});

test('No thanks sends decline and the bar goes away', async () => {
  const { m, el, calls } = bar({ status: off, decline: { state: 'declined', done: 0, total: 0 } });
  await m.refresh();
  el.querySelector('.import-decline').dispatchEvent({ type: 'click' });
  await settle();
  await settle();
  assert.equal(calls[1].action, 'decline');
  assert.equal(el.hidden, true);
  assert.equal(el.children.length, 0);
});

test('a server that cannot import leaves the bar empty', async () => {
  const { m, el } = bar({ status: new Error('import needs the foreman in chat mode') });
  await m.refresh();
  assert.equal(el.hidden, true);
});

test('the chat asks about the import after each read, and only when it has a bar', async () => {
  reset();
  const calls = [];
  const rpc = async (cmd, fields) => {
    calls.push(cmd);
    return cmd === 'floor.import' ? off : { messages: [], more: false };
  };
  const bare = mountChat({ list: element('chat-list'), rpc, panes: () => [], open: () => {}, me: () => '', on: () => true });
  await bare.read();
  assert.deepEqual(calls, ['floor.chat']);
  calls.length = 0;
  const withBar = mountChat({ list: element('chat-list'), rpc, panes: () => [], open: () => {}, me: () => '', on: () => true, importBar: element('import-bar') });
  await withBar.read();
  await settle();
  assert.deepEqual(calls, ['floor.chat', 'floor.import']);
  assert.equal(element('import-bar').hidden, false);
});

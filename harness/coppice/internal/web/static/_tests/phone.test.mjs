import test from 'node:test';
import assert from 'node:assert/strict';
import { reset, element, TOKEN, freshApp, lastSocket, liveTimers } from './browser-stub.mjs';
import { endedMessage, ENDED_CLEAR_MS } from '../floor.js';

// The whole page at phone size: the overview is home, an agent's sheet
// slides in over it and never hides it, the sheet goes back, and an agent
// that ends under its sheet takes the owner home with the line that says so.

function p(id, state, ask) {
  return { id, label: id, state, harness: 'claude', cwd: '/w/' + id, ts: 1, ask };
}

const PANES = [p('a', 'working'), p('b', 'blocked', { id: 't1', summary: 'ls', tier: 'undoable' }), p('c', 'idle'),
  { ...p('h', 'idle'), kind: 'headless', harness: 'sprig' }];
const settle = () => new Promise((r) => setImmediate(r));
const route = () => global.window.dispatchEvent(new global.CustomEvent('coppice:route'));

async function boot(width, height) {
  reset();
  global.window.innerWidth = width;
  global.window.innerHeight = height;
  global.localStorage.setItem('coppice.token', TOKEN);
  global.location.hash = '#/roster';
  await freshApp();
  const ws = lastSocket();
  const sent = [];
  ws.send = (data) => { sent.push(JSON.parse(data)); };
  ws.simulateOpen();
  const list = sent.find((m) => m.cmd === 'pane.list');
  ws.fire('message', { data: JSON.stringify({ id: list.id, ok: true, result: { panes: PANES } }) });
  await settle();
  return { ws, sent };
}

test('a phone opens an agent as a sheet over the overview, which stays drawn', async () => {
  const { sent } = await boot(390, 844);
  assert.equal(element('screen-roster').hidden, false);
  assert.equal(element('needs').hidden, false, 'the queue that needs you is not at the top');
  assert.deepEqual(element('needs').querySelectorAll('.ask').map((li) => li.dataset.row), ['b']);
  const row = element('roster').querySelector('[data-row="a"]');
  row.dispatchEvent({ type: 'click' });
  assert.equal(global.location.hash, '#/pane/a');
  route();
  await settle();
  assert.equal(element('screen-pane').hidden, false);
  assert.equal(element('screen-roster').hidden, false, 'the sheet hid the overview');
  assert.equal(element('sheet-edge').hidden, false);
  assert.equal(element('back').hidden, true);
  const attach = sent.filter((m) => m.cmd === 'pane.attach').pop();
  assert.equal(attach.pane, 'a');
  assert.equal(attach.cols, undefined, 'the phone sized the pane');
});

test('the sheet goes back by stepping back when a tap opened it, and by replacing a cold link', async () => {
  await boot(390, 844);
  let backs = 0;
  global.history.back = () => {
    backs += 1;
    global.location.hash = '#/roster';
    route();
  };
  global.location.hash = '#/pane/a';
  route();
  await settle();
  element('sheet-edge').dispatchEvent({ type: 'click' });
  assert.equal(backs, 1, 'a sheet a tap opened did not step back');
  assert.equal(element('screen-pane').hidden, true);
  // A cold link: the page boots straight into the sheet.
  reset();
  global.window.innerWidth = 390;
  global.window.innerHeight = 844;
  global.localStorage.setItem('coppice.token', TOKEN);
  global.location.hash = '#/pane/b';
  await freshApp();
  backs = 0;
  element('sheet-back').dispatchEvent({ type: 'click' });
  assert.equal(backs, 0, 'a cold link stepped back off the page');
  assert.equal(global.location.hash, '#/roster');
  assert.equal(element('screen-pane').hidden, true);
  delete global.history.back;
});

test('an agent that ends under its sheet takes the owner home with Resume and Forget', async () => {
  const { ws } = await boot(390, 844);
  global.location.hash = '#/pane/a';
  route();
  await settle();
  ws.fire('message', { data: JSON.stringify({ event: 'state', pane: 'a', state: 'done', source: 'process' }) });
  await settle();
  assert.equal(global.location.hash, '#/roster');
  assert.match(element('status').textContent, /a ended/);
  const fixes = element('status-fix').querySelectorAll('button').map((b) => b.textContent);
  assert.deepEqual(fixes, ['Resume', 'Forget']);
});

// coppice:gone is what the pane screen sends when its agent ends. The
// page goes back to the overview, says the agent ended with its exit
// code once the ended list gives it, and gives the focus back to the row.
test('coppice:gone under an open sheet goes home with the exit code and the focus on the row', async () => {
  const { ws, sent } = await boot(390, 844);
  global.location.hash = '#/pane/a';
  route();
  await settle();
  let focused = null;
  const row = element('roster').querySelector('[data-row="a"]');
  row.focus = () => { focused = 'row a'; };
  global.window.dispatchEvent(new global.CustomEvent('coppice:gone', { detail: { pane: 'a', label: 'a' } }));
  await settle();
  assert.equal(global.location.hash, '#/roster');
  assert.equal(element('screen-pane').hidden, true);
  assert.equal(focused, 'row a', 'the focus did not go back to the row');
  const ended = sent.filter((m) => m.cmd === 'pane.list' && m.ended === true).pop();
  assert.ok(ended, 'the ended list was never read');
  ws.fire('message', { data: JSON.stringify({ id: ended.id, ok: true, result: { panes: [{ id: 'a', label: 'a', exit_code: 3, ended_at: 1 }] } }) });
  await settle();
  assert.equal(element('status').textContent, 'a ended (exit 3)');
  const fixes = element('status-fix').querySelectorAll('button').map((b) => b.textContent);
  assert.deepEqual(fixes, ['Resume', 'Forget']);
});

test('the Enter button sends the text and Enter in one request, and the keyboard Enter only types', async () => {
  const { ws, sent } = await boot(390, 844);
  global.location.hash = '#/pane/a';
  route();
  await settle();
  element('text').value = 'hello';
  element('compose').dispatchEvent({ type: 'submit', preventDefault() {} });
  await settle();
  element('text').value = 'go';
  element('send-enter').dispatchEvent({ type: 'click' });
  await settle();
  const first = sent.filter((m) => m.cmd === 'pane.send_text');
  assert.equal(first.length, 1, 'the second send went before the first was answered');
  ws.fire('message', { data: JSON.stringify({ id: first[0].id, ok: true, result: { sent: 5 } }) });
  await settle();
  const texts = sent.filter((m) => m.cmd === 'pane.send_text').map((m) => [m.text, m.enter]);
  assert.deepEqual(texts, [['hello', false], ['go', true]]);
  assert.equal(element('text').value, '', 'the field kept typed text');
});

test('a wide page keeps the pane screen whole and the overview hidden', async () => {
  await boot(1200, 900);
  global.location.hash = '#/pane/a';
  route();
  await settle();
  assert.equal(element('screen-roster').hidden, true);
  assert.equal(element('sheet-edge').hidden, true);
});

test('a headless agent\'s sheet sends whole messages and hides the keys it cannot take', async () => {
  const { ws, sent } = await boot(390, 844);
  global.location.hash = '#/pane/h';
  route();
  await settle();
  assert.equal(element('keys-toggle').hidden, true, 'the key drawer shows for a headless agent');
  assert.equal(element('keys').hidden, true);
  assert.match(element('pane-note').textContent, /whole messages/);
  assert.equal(element('pane-note').hidden, false);
  element('text').value = 'tidy the docs';
  element('compose').dispatchEvent({ type: 'submit', preventDefault() {} });
  await settle();
  const first = sent.filter((m) => m.cmd === 'agent.prompt');
  assert.deepEqual(first.map((m) => [m.pane, m.text]), [['h', 'tidy the docs']]);
  ws.fire('message', { data: JSON.stringify({ id: first[0].id, ok: true, result: {} }) });
  await settle();
  element('text').value = 'and then stop';
  element('send-enter').dispatchEvent({ type: 'click' });
  await settle();
  assert.deepEqual(sent.filter((m) => m.cmd === 'agent.prompt').map((m) => m.text), ['tidy the docs', 'and then stop']);
  assert.equal(sent.filter((m) => m.cmd === 'pane.send_text' || m.cmd === 'pane.send_keys').length, 0, 'raw text reached a headless agent');
  // A pty agent's sheet keeps its keys and no note.
  global.location.hash = '#/pane/a';
  route();
  await settle();
  assert.equal(element('keys-toggle').hidden, false);
  assert.equal(element('pane-note').hidden, true);
});

test('an open sheet is a modal dialog over an inert overview, and the edge stays live', async () => {
  await boot(390, 844);
  let focused = null;
  element('sheet-back').focus = () => { focused = 'sheet-back'; };
  global.location.hash = '#/pane/a';
  route();
  await settle();
  const sheet = element('screen-pane');
  assert.equal(sheet.getAttribute('role'), 'dialog');
  assert.equal(sheet.getAttribute('aria-modal'), 'true');
  assert.equal(sheet.getAttribute('aria-labelledby'), 'pane-label');
  assert.equal(element('screen-roster').inert, true);
  assert.equal(element('bar').inert, true);
  assert.notEqual(element('sheet-edge').inert, true);
  assert.equal(focused, 'sheet-back');
  const row = element('roster').querySelector('[data-row="a"]');
  row.focus = () => { focused = 'row a'; };
  global.location.hash = '#/roster';
  route();
  await settle();
  assert.equal(element('screen-roster').inert, false);
  assert.equal(element('bar').inert, false);
  assert.equal(sheet.getAttribute('role'), null);
  assert.equal(focused, 'row a', 'the focus did not go back to the row');
});

test('a view open over the overview closes when a sheet slides in', async () => {
  await boot(390, 844);
  global.window.coppice.journal('');
  assert.equal(element('overlay').hidden, false, 'the journal did not open');
  global.location.hash = '#/pane/a';
  route();
  await settle();
  assert.equal(element('overlay').hidden, true, 'the view stayed open over the sheet');
  global.window.coppice.journal('a');
  assert.equal(element('overlay').hidden, false, 'the journal from the sheet did not open');
});

// Voice on a headless agent's sheet lands in its message field and is not
// sent: the owner reads it and sends it as one whole message.
test('voice text lands in a headless agent\'s message field and is not sent', async () => {
  global.navigator.mediaDevices = { getUserMedia: async () => ({ getTracks: () => [{ stop() {} }] }) };
  global.MediaRecorder = class {
    constructor() { this.mimeType = 'audio/webm'; this.l = {}; }
    addEventListener(name, fn) { this.l[name] = fn; }
    start() {}
    stop() {
      queueMicrotask(() => {
        if (this.l.dataavailable) this.l.dataavailable({ data: new Blob(['clip']) });
        if (this.l.stop) this.l.stop();
      });
    }
  };
  const { sent } = await boot(390, 844);
  const api = global.window.coppice.api;
  global.window.coppice.api = async (path, options) => (String(path).startsWith('/api/voice/transcribe')
    ? { text: 'tidy the docs' }
    : api(path, options));
  global.location.hash = '#/pane/h';
  route();
  await settle();
  element('text').value = '';
  element('record').dispatchEvent({ type: 'click' });
  await settle();
  element('record').dispatchEvent({ type: 'click' });
  await settle();
  await settle();
  assert.equal(element('text').value, 'tidy the docs');
  assert.equal(sent.filter((m) => ['agent.prompt', 'pane.send_text', 'pane.send_keys'].includes(m.cmd)).length, 0,
    'voice text was sent before the owner sent it');
  delete global.MediaRecorder;
  delete global.navigator.mediaDevices;
});

// The status line honors a timed clear: a good exit's ended line goes
// after ENDED_CLEAR_MS, a bad exit's stays, and a later line is never
// cleared by an older timer.
test('a good exit\'s ended line on the status line clears after its time, a bad one stays', async () => {
  await boot(390, 844);
  const status = global.window.coppice.status;
  const timed = () => liveTimers().filter((t) => t.ms === ENDED_CLEAR_MS);
  const before = timed().length;
  status(endedMessage('w1:p1', 'fine', 0));
  assert.equal(element('status').textContent, 'fine ended (exit 0)');
  const mine = timed().slice(before);
  assert.equal(mine.length, 1, 'no timer for a good exit');
  mine[0].fn();
  assert.equal(element('status').textContent, '');
  const n = timed().length;
  status(endedMessage('w1:p1', 'bad', 3));
  assert.equal(timed().length, n, 'a bad exit set a timer');
  status(endedMessage('w1:p1', 'fine', 0));
  const older = timed().slice(-1)[0];
  status('Allowed.');
  older.fn();
  assert.equal(element('status').textContent, 'Allowed.', 'an old timer cleared a newer line');
});

import test from 'node:test';
import assert from 'node:assert/strict';
import { reset, element, TOKEN, freshApp, lastSocket, liveTimers } from './browser-stub.mjs';
import {
  attachCommand, messagesCommand, fitLine, barFor, viewFor, READING_LINE, SCREEN_LINE, FIT_ALONE, FIT_SHARED,
} from '../pane.js';
import { READING_FIRST } from '../dock.js';

// The reading view and fit-when-alone. On a phone, an agent whose
// transcript the server reads opens on the reading view, with
// `From the transcript.` and a Screen button. Screen attaches, asking the
// server to fit the agent to the sheet, and the reply's fitted says what the
// sheet line is. The ask bar, the trust bar and the compose bar work over
// the reading view.

const settle = () => new Promise((r) => setImmediate(r));
const route = () => global.window.dispatchEvent(new global.CustomEvent('coppice:route'));

function p(id, state, ask) {
  return { id, label: id, state, harness: 'claude', cwd: '/w/' + id, ts: 1, ask };
}

const MESSAGES = [
  { id: 't/1', at: 10, role: 'owner', text: 'fix test_sync, it fails on CI' },
  { id: 't/2', at: 20, role: 'agent', text: 'The test reads a clock. I made it take a fake clock.' },
  { id: 't/3', at: 21, role: 'agent', text: '', tool: 'Bash ran pytest tests/test_sync.py' },
];

// boot opens the page at width by 844. The panes named in transcripts have
// a transcript; asking for any other pane's is refused. Every reply but
// pane.messages and pane.attach is empty ok, and a test answers those two
// through attachReply.
async function boot(width, transcripts = {}) {
  reset();
  global.window.innerWidth = width;
  global.window.innerHeight = 844;
  global.localStorage.setItem('coppice.token', TOKEN);
  global.location.hash = '#/roster';
  element('canvas-wrap').clientWidth = 350;
  element('canvas-wrap').clientHeight = 600;
  await freshApp();
  const ws = lastSocket();
  const sent = [];
  const world = { ws, sent, attachReply: { attached: true }, transcripts, hold: false, held: [] };
  world.answer = (m) => ws.fire('message', { data: JSON.stringify({ id: m.id, ok: true, result: world.attachReply }) });
  const reply = (id, body) => queueMicrotask(() => ws.fire('message', { data: JSON.stringify({ id, ...body }) }));
  ws.send = (data) => {
    const m = JSON.parse(data);
    sent.push(m);
    if (m.cmd === 'pane.messages') {
      if (world.transcripts[m.pane]) reply(m.id, { ok: true, result: { messages: world.transcripts[m.pane], source: 'transcript', more: false } });
      else reply(m.id, { ok: false, error: { code: 'bad_request', message: m.pane + ' has no transcript to read. Its hook reported none.' } });
    } else if (m.cmd === 'pane.attach') {
      // A test may hold the attach unanswered, and answer it later.
      if (world.hold) world.held.push(m);
      else reply(m.id, { ok: true, result: world.attachReply });
    } else if (m.cmd === 'pane.detach') {
      reply(m.id, { ok: true, result: { pane: m.pane, attached: false } });
    }
  };
  ws.simulateOpen();
  const list = sent.find((m) => m.cmd === 'pane.list');
  ws.fire('message', { data: JSON.stringify({ id: list.id, ok: true, result: { panes: [p('a', 'working'), p('b', 'working'), p('c', 'blocked', { id: 't1', summary: 'git push', tier: 'undoable' })] } }) });
  await settle();
  return world;
}

async function openSheet(id) {
  global.location.hash = '#/pane/' + id;
  route();
  await settle();
  await settle();
}

const calls = (w, cmd) => w.sent.filter((m) => m.cmd === cmd);

test('the pure rules: the attach carries a fit, the lines follow fitted, the view follows the transcript', () => {
  assert.deepEqual(attachCommand({ id: 'w1:p1' }), { cmd: 'pane.attach', pane: 'w1:p1' });
  assert.deepEqual(attachCommand({ id: 'w1:p1' }, { cols: 43, rows: 40 }), { cmd: 'pane.attach', pane: 'w1:p1', fit: { cols: 43, rows: 40 } });
  assert.equal(messagesCommand({ id: 'w1:p1' }).cmd, 'pane.messages');
  assert.equal(fitLine(true), 'Sized to this phone.');
  assert.equal(fitLine(false), 'Not resized: open elsewhere, or sized by hand.');
  assert.equal(fitLine(undefined), '');
  assert.deepEqual(barFor({ view: 'reading', transcript: true, fitted: null }), { line: 'From the transcript.', toggle: 'Screen' });
  assert.deepEqual(barFor({ view: 'screen', transcript: true, fitted: true }), { line: FIT_ALONE, toggle: 'Messages' });
  assert.deepEqual(barFor({ view: 'screen', transcript: true, fitted: false }), { line: FIT_SHARED, toggle: 'Messages' });
  assert.deepEqual(barFor({ view: 'screen', transcript: true, fitted: null }), { line: SCREEN_LINE, toggle: 'Messages' });
  assert.deepEqual(barFor({ view: 'screen', transcript: false, fitted: true }), { line: FIT_ALONE, toggle: '' });
  assert.deepEqual(barFor({ view: 'screen', transcript: false, fitted: null }), { line: '', toggle: '' });
  assert.deepEqual(barFor({ view: 'pending', transcript: false, fitted: null }), { line: '', toggle: '' });
  assert.equal(viewFor({ phone: true, readingFirst: true, transcript: true }), 'reading');
  assert.equal(viewFor({ phone: true, readingFirst: false, transcript: true }), 'screen');
  assert.equal(viewFor({ phone: true, readingFirst: true, transcript: false }), 'screen');
  assert.equal(viewFor({ phone: false, readingFirst: true, transcript: true }), 'screen');
  assert.equal(READING_LINE, 'From the transcript.');
});

test('reading first is on: the owner ruled for it, and one constant in dock.js flips it', () => {
  assert.equal(READING_FIRST, true);
});

test('an agent with a transcript opens on the reading view on a phone, and attaches nothing', async () => {
  const w = await boot(390, { a: MESSAGES });
  await openSheet('a');
  assert.equal(calls(w, 'pane.messages').length, 1);
  assert.equal(calls(w, 'pane.attach').length, 0, 'the reading view attached');
  assert.equal(element('reading').hidden, false);
  assert.equal(element('canvas-wrap').hidden, true);
  assert.equal(element('view-bar').hidden, false);
  assert.equal(element('view-line').textContent, 'From the transcript.');
  assert.equal(element('view-toggle').textContent, 'Screen');
  const text = element('reading-list').textContent;
  assert.match(text, /fix test_sync, it fails on CI/);
  assert.match(text, /The test reads a clock\./);
  assert.match(text, /Bash ran pytest tests\/test_sync\.py/);
  assert.equal(element('reading-list').querySelectorAll('.chip-agent').length, 0);
});

test('Screen attaches with a fit and shows the live canvas; fitted true says the sheet is sized to the phone', async () => {
  const w = await boot(390, { a: MESSAGES });
  w.attachReply = { attached: true, fitted: true };
  await openSheet('a');
  element('view-toggle').dispatchEvent({ type: 'click' });
  await settle();
  const attach = calls(w, 'pane.attach').pop();
  assert.ok(attach, 'Screen sent no attach');
  assert.equal(attach.pane, 'a');
  assert.ok(attach.fit && attach.fit.cols >= 20 && attach.fit.cols <= Math.floor(350 / 8), 'the fit is wider than the body: ' + JSON.stringify(attach.fit));
  assert.ok(attach.fit.rows >= 6);
  assert.equal(element('canvas-wrap').hidden, false);
  assert.equal(element('reading').hidden, true);
  assert.equal(element('view-line').textContent, 'Sized to this phone.');
  assert.equal(element('view-toggle').textContent, 'Messages');
});

test('fitted false says the agent is open on the desktop and was not resized', async () => {
  const w = await boot(390, { a: MESSAGES });
  w.attachReply = { attached: true, fitted: false };
  await openSheet('a');
  element('view-toggle').dispatchEvent({ type: 'click' });
  await settle();
  assert.equal(element('view-line').textContent, 'Not resized: open elsewhere, or sized by hand.');
});

test('Messages goes back to the reading view and detaches, so the old size comes back', async () => {
  const w = await boot(390, { a: MESSAGES });
  w.attachReply = { attached: true, fitted: true };
  await openSheet('a');
  element('view-toggle').dispatchEvent({ type: 'click' });
  await settle();
  element('view-toggle').dispatchEvent({ type: 'click' });
  await settle();
  assert.equal(calls(w, 'pane.detach').length, 1);
  assert.equal(element('reading').hidden, false);
  assert.equal(element('canvas-wrap').hidden, true);
  assert.equal(element('view-line').textContent, 'From the transcript.');
});

test('leaving a reading view sends no detach, and leaving the screen sends one', async () => {
  const w = await boot(390, { a: MESSAGES });
  await openSheet('a');
  global.location.hash = '#/roster';
  route();
  await settle();
  assert.equal(calls(w, 'pane.detach').length, 0, 'a reading view held no attach, so nothing detaches');
  await openSheet('a');
  element('view-toggle').dispatchEvent({ type: 'click' });
  await settle();
  global.location.hash = '#/roster';
  route();
  await settle();
  await settle();
  assert.equal(calls(w, 'pane.detach').length, 1);
});

test('an agent with no transcript opens on the screen with a fit, and shows no switch', async () => {
  const w = await boot(390, {});
  w.attachReply = { attached: true, fitted: true };
  await openSheet('b');
  const attach = calls(w, 'pane.attach').pop();
  assert.ok(attach && attach.fit, 'no fit on a phone');
  assert.equal(element('canvas-wrap').hidden, false);
  assert.equal(element('reading').hidden, true);
  assert.equal(element('view-line').textContent, 'Sized to this phone.');
  assert.equal(element('view-toggle').hidden, true, 'a switch for an agent with no reading view');
});

test('a wider page opens on the screen, asks for no transcript and never fits', async () => {
  const w = await boot(1200, { a: MESSAGES });
  await openSheet('a');
  assert.equal(calls(w, 'pane.messages').length, 0);
  const attach = calls(w, 'pane.attach').pop();
  assert.ok(attach, 'no attach');
  assert.equal(attach.fit, undefined);
  assert.equal(element('view-bar').hidden, true);
});

test('the ask bar draws over the reading view, and Allow answers it', async () => {
  const w = await boot(390, { c: MESSAGES });
  await openSheet('c');
  assert.equal(element('reading').hidden, false);
  assert.equal(element('ask').hidden, false);
  assert.equal(element('ask-summary').textContent, 'git push');
  const answers = [];
  global.fetch = async (url, init) => { answers.push({ url: String(url), body: JSON.parse(init.body) }); return { ok: true, status: 200, json: async () => ({}) }; };
  element('allow').dispatchEvent({ type: 'click' });
  await settle();
  await settle();
  const sent = answers.find((a) => a.url.includes('/api/ask/answer'));
  assert.ok(sent, 'Allow sent no answer from the reading view');
  assert.equal(sent.body.decision, 'allow');
  assert.equal(sent.body.tool_use_id, 't1');
  assert.equal(element('reading').hidden, false, 'answering took the reading view away');
});

test('Deny answers the ask from the reading view too', async () => {
  const w = await boot(390, { c: MESSAGES });
  await openSheet('c');
  assert.equal(element('ask').hidden, false);
  const answers = [];
  global.fetch = async (url, init) => { answers.push({ url: String(url), body: JSON.parse(init.body) }); return { ok: true, status: 200, json: async () => ({}) }; };
  element('deny').dispatchEvent({ type: 'click' });
  await settle();
  await settle();
  const sent = answers.find((a) => a.url.includes('/api/ask/answer'));
  assert.ok(sent, 'Deny sent no answer from the reading view');
  assert.equal(sent.body.decision, 'deny');
  assert.equal(sent.body.tool_use_id, 't1');
  assert.equal(element('reading').hidden, false, 'answering took the reading view away');
  assert.equal(calls(w, 'pane.attach').length, 0);
});

test('a trust screen shows its bar over the reading view, and Trust sends pane.trust from it', async () => {
  const w = await boot(390, { a: MESSAGES });
  await openSheet('a');
  w.ws.fire('message', { data: JSON.stringify({
    event: 'state', pane: 'a', state: 'blocked', source: 'manifest', harness: 'claude', ts: 2,
    detail: 'asks to trust this folder (rule=coppice_first_run_trust region=whole_recent)',
  }) });
  await settle();
  assert.equal(element('trust').hidden, false);
  assert.equal(element('reading').hidden, false);
  element('trust-yes').dispatchEvent({ type: 'click' });
  await settle();
  const req = calls(w, 'pane.trust').pop();
  assert.ok(req, 'Trust sent no pane.trust');
  assert.deepEqual([req.pane, req.trust], ['a', true]);
  assert.equal(element('reading').hidden, false, 'answering took the reading view away');
});

test('the compose bar types into the agent from the reading view', async () => {
  const w = await boot(390, { a: MESSAGES });
  await openSheet('a');
  element('text').value = 'run the suite again';
  element('send-enter').dispatchEvent({ type: 'click' });
  await settle();
  const send = calls(w, 'pane.send_text').pop();
  assert.ok(send, 'nothing was typed into the agent');
  assert.equal(send.pane, 'a');
  assert.equal(send.text, 'run the suite again');
  assert.equal(send.enter, true);
  assert.equal(element('reading').hidden, false);
});

test('a messages event for the open agent reads the transcript again; another agent\'s does not', async () => {
  const w = await boot(390, { a: MESSAGES });
  await openSheet('a');
  const before = calls(w, 'pane.messages').length;
  w.transcripts.a = [...MESSAGES, { id: 't/4', at: 30, role: 'agent', text: 'All 212 tests pass now.' }];
  w.ws.fire('message', { data: JSON.stringify({ event: 'messages', pane: 'b', chat: false, ts: 3 }) });
  await settle();
  assert.equal(calls(w, 'pane.messages').length, before, 'another agent\'s event read the transcript');
  w.ws.fire('message', { data: JSON.stringify({ event: 'messages', pane: 'a', chat: false, ts: 4 }) });
  await settle();
  await settle();
  assert.equal(calls(w, 'pane.messages').length, before + 1);
  assert.match(element('reading-list').textContent, /All 212 tests pass now\./);
});

test('the chat on home still ignores a transcript event that is not for the chat', async () => {
  const w = await boot(390, { a: MESSAGES });
  const reads = () => calls(w, 'floor.chat').length;
  await settle();
  const before = reads();
  w.ws.fire('message', { data: JSON.stringify({ event: 'messages', pane: 'a', chat: false, ts: 3 }) });
  await settle();
  assert.equal(reads(), before);
});

test('turning the phone asks the server to fit the new width, once the width changed', async () => {
  const w = await boot(390, {});
  w.attachReply = { attached: true, fitted: true };
  await openSheet('b');
  const first = calls(w, 'pane.attach').pop();
  element('canvas-wrap').clientWidth = 700;
  global.window.dispatchEvent({ type: 'resize' });
  await settle();
  const second = calls(w, 'pane.attach').pop();
  assert.notEqual(second, first, 'a new width sent no new fit');
  assert.ok(second.fit.cols > first.fit.cols);
  const count = calls(w, 'pane.attach').length;
  global.window.dispatchEvent({ type: 'resize' });
  await settle();
  assert.equal(calls(w, 'pane.attach').length, count, 'the same width sent a fit again');
});

const at = (w, cmd) => w.sent.map((m, i) => (m.cmd === cmd ? i : -1)).filter((i) => i >= 0);

test('a detach waits for an attach that is still unanswered, so it never overtakes it', async () => {
  const w = await boot(390, {});
  w.hold = true;
  await openSheet('b');
  assert.equal(w.held.length, 1);
  global.location.hash = '#/settings';
  route();
  await settle();
  assert.equal(calls(w, 'pane.detach').length, 0, 'the detach went before the attach was answered');
  w.answer(w.held[0]);
  await settle();
  await settle();
  const detach = at(w, 'pane.detach');
  assert.equal(detach.length, 1);
  assert.ok(detach[0] > at(w, 'pane.attach')[0], 'the detach is behind its attach on the wire');
});

test('Messages while the attach is unanswered detaches only after the answer', async () => {
  const w = await boot(390, { a: MESSAGES });
  await openSheet('a');
  w.hold = true;
  element('view-toggle').dispatchEvent({ type: 'click' });
  await settle();
  assert.equal(w.held.length, 1);
  element('view-toggle').dispatchEvent({ type: 'click' });
  await settle();
  assert.equal(calls(w, 'pane.detach').length, 0);
  w.answer(w.held[0]);
  await settle();
  await settle();
  assert.equal(calls(w, 'pane.detach').length, 1);
  assert.equal(element('reading').hidden, false);
});

test('opening the same agent again waits for the detach of the last sheet', async () => {
  const w = await boot(390, {});
  w.attachReply = { attached: true, fitted: true };
  await openSheet('b');
  global.location.hash = '#/settings';
  route();
  await openSheet('b');
  const detach = at(w, 'pane.detach');
  const attach = at(w, 'pane.attach');
  assert.equal(detach.length, 1);
  assert.equal(attach.length, 2);
  assert.ok(detach[0] < attach[1], 'the second attach went before the first detach');
});

test('someone else opening or leaving the agent makes the sheet ask for the fit again, and the line follows', async () => {
  const w = await boot(390, {});
  w.attachReply = { attached: true, fitted: true };
  await openSheet('b');
  assert.equal(element('view-line').textContent, 'Sized to this phone.');
  const before = calls(w, 'pane.attach').length;
  w.attachReply = { attached: true, fitted: false };
  w.ws.fire('message', { data: JSON.stringify({ event: 'presence', pane: 'b', looking: ['desk'] }) });
  await settle();
  await settle();
  assert.equal(calls(w, 'pane.attach').length, before + 1);
  assert.ok(calls(w, 'pane.attach').pop().fit, 'the new attach carries no fit');
  assert.equal(element('view-line').textContent, 'Not resized: open elsewhere, or sized by hand.');
  w.attachReply = { attached: true, fitted: true };
  w.ws.fire('message', { data: JSON.stringify({ event: 'presence', pane: 'b', looking: [] }) });
  await settle();
  await settle();
  assert.equal(element('view-line').textContent, 'Sized to this phone.');
  w.ws.fire('message', { data: JSON.stringify({ event: 'presence', pane: 'a', looking: ['x'] }) });
  await settle();
  assert.equal(calls(w, 'pane.attach').length, before + 2, 'another agent\'s presence sent an attach');
});

test('going home from a fitted sheet detaches it instead of handing the fit to the floor', async () => {
  const w = await boot(390, {});
  w.attachReply = { attached: true, fitted: true };
  await openSheet('b');
  global.location.hash = '#/roster';
  route();
  await settle();
  await settle();
  assert.deepEqual(calls(w, 'pane.detach').map((m) => m.pane), ['b']);
});

const lastOp = (w, pane) => w.sent.filter((m) => m.pane === pane && (m.cmd === 'pane.attach' || m.cmd === 'pane.detach')).map((m) => m.cmd).pop();
const answerAll = async (w) => {
  for (const m of w.held.splice(0)) w.answer(m);
  await settle();
  await settle();
  await settle();
};

test('a refit and then Messages inside one round trip detach only after the refit is answered', async () => {
  const w = await boot(390, { a: MESSAGES });
  w.attachReply = { attached: true, fitted: true };
  await openSheet('a');
  element('view-toggle').dispatchEvent({ type: 'click' });
  await settle();
  w.hold = true;
  w.ws.fire('message', { data: JSON.stringify({ event: 'presence', pane: 'a', looking: ['desk'] }) });
  await settle();
  assert.equal(w.held.length, 1, 'the refit was not sent');
  element('view-toggle').dispatchEvent({ type: 'click' });
  await settle();
  assert.equal(calls(w, 'pane.detach').length, 0, 'the detach overtook the refit');
  await answerAll(w);
  assert.equal(calls(w, 'pane.detach').length, 1);
  assert.equal(lastOp(w, 'a'), 'pane.detach');
  assert.equal(element('reading').hidden, false);
});

test('leaving and opening the same agent again inside one round trip leaves it attached once, with its line', async () => {
  const w = await boot(390, {});
  w.attachReply = { attached: true, fitted: true };
  w.hold = true;
  await openSheet('b');
  global.location.hash = '#/settings';
  route();
  await openSheet('b');
  await answerAll(w);
  assert.equal(lastOp(w, 'b'), 'pane.attach', 'a detach came after the last attach');
  assert.equal(element('view-line').textContent, 'Sized to this phone.');
});

test('Screen, Messages and Screen again inside one round trip leave the agent attached and never detach it', async () => {
  const w = await boot(390, { a: MESSAGES });
  w.attachReply = { attached: true, fitted: true };
  await openSheet('a');
  w.hold = true;
  const tap = () => element('view-toggle').dispatchEvent({ type: 'click' });
  tap();
  tap();
  tap();
  await answerAll(w);
  assert.equal(calls(w, 'pane.detach').length, 0);
  assert.equal(lastOp(w, 'a'), 'pane.attach');
  assert.equal(element('canvas-wrap').hidden, false);
  assert.equal(element('view-line').textContent, 'Sized to this phone.');
});

test('leave b, open c, leave c, open b inside one round trip: b ends attached and c ends detached', async () => {
  const w = await boot(390, {});
  w.hold = true;
  await openSheet('b');
  global.location.hash = '#/settings';
  route();
  await openSheet('c');
  global.location.hash = '#/settings';
  route();
  await openSheet('b');
  await answerAll(w);
  assert.equal(lastOp(w, 'b'), 'pane.attach');
  assert.equal(lastOp(w, 'c'), 'pane.detach');
});

test('an attach that timed out and lands later is detached when nothing is wanted', async () => {
  const w = await boot(390, {});
  w.hold = true;
  await openSheet('b');
  global.location.hash = '#/settings';
  route();
  await settle();
  assert.equal(calls(w, 'pane.detach').length, 0);
  // The page's own fifteen second timer is what rejects the attach.
  for (const t of liveTimers().filter((x) => x.ms === 15000)) t.fn();
  await settle();
  await settle();
  assert.equal(lastOp(w, 'b'), 'pane.detach', 'the attach may have landed, so the queue takes it back');
});

test('on the phone a sheet handed to the hidden floor with its attach unanswered is detached only after the answer', async () => {
  const w = await boot(390, {});
  w.attachReply = { attached: true, fitted: true };
  w.hold = true;
  await openSheet('b');
  global.location.hash = '#/roster';
  route();
  await settle();
  assert.equal(calls(w, 'pane.detach').length, 0, 'the floor detached before the attach was answered');
  await answerAll(w);
  assert.equal(lastOp(w, 'b'), 'pane.detach', 'the fit and the frames were stranded');
});

test('leaving through the floor and opening the agent again inside one round trip ends attached', async () => {
  const w = await boot(390, {});
  w.attachReply = { attached: true, fitted: true };
  w.hold = true;
  await openSheet('b');
  global.location.hash = '#/roster';
  route();
  await openSheet('b');
  await answerAll(w);
  await answerAll(w);
  assert.equal(lastOp(w, 'b'), 'pane.attach', 'the floor\'s detach landed after the new sheet\'s attach');
  assert.equal(element('view-line').textContent, 'Sized to this phone.');
});

test('leaving, handing over, opening again and leaving again ends with the agent detached', async () => {
  const w = await boot(390, {});
  w.hold = true;
  await openSheet('b');
  global.location.hash = '#/roster';
  route();
  await openSheet('b');
  global.location.hash = '#/settings';
  route();
  await answerAll(w);
  await answerAll(w);
  assert.equal(lastOp(w, 'b'), 'pane.detach', 'the first attach landed last and was stranded');
});

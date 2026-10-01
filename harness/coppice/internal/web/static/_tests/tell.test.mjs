import test from 'node:test';
import assert from 'node:assert/strict';
import { reset, element } from './browser-stub.mjs';
import { applyReply } from '../record.js';
import { classifyTell, plumbingCommand, talkCommand, talkSay, mountTell, SLASH_PLUMBING } from '../newpane.js';

// Tell the floor: the chat bar. Everything typed or said goes to the
// foreman through floor.talk, which starts the foreman when none runs.
// Plumbing needs a leading slash: /claude opens a pane, /close docs closes
// one. The terminal floor's prompt line keeps its words, and the old rule
// stays behind the one switch. A transcription fills the bar and never
// sends.

const settle = () => new Promise((r) => setImmediate(r));

const PANES = [
  { id: 'w1:p1', label: 'floor', cwd: '/repo', state: 'working' },
  { id: 'w1:p2', label: 'docs', cwd: '/repo/docs', state: 'idle' },
];

// stub mounts the bar with window.coppice recording every rpc and api call.
function stub({ talk = { pane: 'w1:p1', label: 'foreman', started: false, queued: 1 }, rpcFails = null } = {}) {
  reset();
  const rpcs = [];
  const apis = [];
  global.window.coppice = {
    state: { panes: PANES, token: 'T' },
    rpc: async (cmd, fields) => {
      rpcs.push({ cmd, ...fields });
      if (rpcFails) throw rpcFails;
      return cmd === 'floor.talk' ? talk : { pane: 'w1:p7' };
    },
    api: async (path, init) => {
      // The mic asks whether voice can run when it mounts. That is not
      // what these tests count.
      if (path === '/api/voice/status') return { ready: true };
      apis.push({ path, init });
      if (path.startsWith('/api/voice/transcribe')) return { text: 'close the docs pane' };
      return {};
    },
    status: (msg) => { element('status').textContent = msg; },
  };
  mountTell();
  return { rpcs, apis };
}

async function tell(text) {
  element('tell-text').value = text;
  element('tell').dispatchEvent({ type: 'submit', preventDefault() {} });
  await settle();
  await settle();
}

test('with the switch off, plumbing reads the way the terminal floor reads it', () => {
  assert.equal(classifyTell('claude', false), 'plumbing');
  assert.equal(classifyTell('codex --resume', false), 'plumbing');
  assert.equal(classifyTell('close docs', false), 'plumbing');
  assert.equal(classifyTell('close the old docs pane when it is done', false), 'talk');
  assert.equal(classifyTell('fix the flaky test', false), 'talk');
  assert.equal(classifyTell('   ', false), 'empty');
});

test('the slash rule is on: a plain sentence is always talk, and plumbing needs a leading slash', () => {
  assert.equal(SLASH_PLUMBING, true);
  // The old trap: a sentence led by a harness name started that harness.
  assert.equal(classifyTell('claude can you check it'), 'talk');
  assert.equal(classifyTell('claude'), 'talk');
  assert.equal(classifyTell('close docs'), 'talk');
  assert.equal(classifyTell('/claude'), 'plumbing');
  assert.equal(classifyTell('/close docs'), 'plumbing');
  assert.equal(classifyTell('/codex --resume'), 'plumbing');
  assert.equal(classifyTell('  /close docs'), 'plumbing');
  assert.equal(classifyTell('/'), 'empty');
  assert.equal(classifyTell('   '), 'empty');
  assert.equal(classifyTell('fix the flaky test'), 'talk');
});

test('only a slash and a known word is plumbing: a path or any other slash line is talk', () => {
  assert.equal(classifyTell('/etc/hosts looks wrong'), 'talk');
  assert.equal(classifyTell('/tmp is full, can you look'), 'talk');
  assert.equal(classifyTell('/claudette is not a harness'), 'talk');
  assert.equal(classifyTell('/open codex'), 'plumbing');
  // A terminal verb is still plumbing, so its line says where it works.
  assert.equal(classifyTell('/layout all'), 'plumbing');
});

test('a sentence led by a path goes to the foreman whole', async () => {
  const { rpcs } = stub();
  await tell('/etc/hosts looks wrong');
  assert.deepEqual(rpcs, [{ cmd: 'floor.talk', text: '/etc/hosts looks wrong' }]);
});

test('a slash line plans the same request as the bare words', () => {
  assert.deepEqual(plumbingCommand('/close docs', { panes: PANES, cwd: '/repo' }).req, { cmd: 'pane.close', pane: 'w1:p2' });
  assert.deepEqual(plumbingCommand('/claude', { panes: PANES, cwd: '/repo' }).req,
    { cmd: 'pane.create', kind: 'pty', harness: 'claude', cwd: '/repo' });
  assert.match(plumbingCommand('/layout all', { panes: PANES, cwd: '/repo' }).say, /^\/layout works on the terminal floor\. Here, type \/claude, \/close NAME, or a sentence for the foreman\.$/);
  assert.match(plumbingCommand('/close', { panes: PANES, cwd: '/repo' }).say, /\/close docs/);
});

test('a sentence led by claude goes to the foreman and opens nothing', async () => {
  const { rpcs } = stub();
  await tell('claude can you check it');
  assert.deepEqual(rpcs, [{ cmd: 'floor.talk', text: 'claude can you check it' }]);
});

test('a harness name opens a pane in the directory the phone knows, and close names a pane by label', () => {
  assert.deepEqual(plumbingCommand('claude', { panes: PANES, cwd: '/repo' }).req,
    { cmd: 'pane.create', kind: 'pty', harness: 'claude', cwd: '/repo' });
  assert.deepEqual(plumbingCommand('codex --resume', { panes: PANES, cwd: '/repo' }).req,
    { cmd: 'pane.create', kind: 'pty', harness: 'codex', cwd: '/repo', args: ['--resume'] });
  assert.deepEqual(plumbingCommand('close docs', { panes: PANES, cwd: '/repo' }).req, { cmd: 'pane.close', pane: 'w1:p2' });
  assert.deepEqual(plumbingCommand('close w1:p2', { panes: PANES, cwd: '/repo' }).req, { cmd: 'pane.close', pane: 'w1:p2' });
  assert.equal(plumbingCommand('close nothing', { panes: PANES, cwd: '/repo' }).req, undefined);
  // With no directory known here, the request names none and the server
  // picks one, as the one-click New does.
  const bare = plumbingCommand('claude', { panes: PANES, cwd: '' });
  assert.deepEqual(bare.req, { cmd: 'pane.create', kind: 'pty', harness: 'claude' });
  assert.ok(!/Open New/.test(bare.say), bare.say);
  assert.match(plumbingCommand('layout all', { panes: PANES, cwd: '/repo' }).say, /terminal floor/);
});

test('talk is floor.talk, and the reply says where the words went', () => {
  assert.deepEqual(talkCommand('fix the flaky test').req, { cmd: 'floor.talk', text: 'fix the flaky test' });
  assert.equal(talkSay({ label: 'foreman', started: true }), 'foreman starts and gets its page. Your words go once it is ready.');
  assert.equal(talkSay({ label: 'foreman', queued: 3 }), 'Sent to foreman. 3 sentences wait for it, yours last.');
  assert.equal(talkSay({ label: 'foreman', queued: 1 }), 'Sent to foreman.');
  assert.equal(talkSay({ label: 'foreman', note: 'foreman waits on a question.' }), 'foreman waits on a question.');
});

test('a sentence sends floor.talk even when no foreman runs, and the bar clears', async () => {
  const { rpcs, apis } = stub();
  await tell('fix the flaky test');
  assert.equal(apis.length, 0, 'talk read the old foreman route');
  assert.deepEqual(rpcs, [{ cmd: 'floor.talk', text: 'fix the flaky test' }]);
  assert.equal(element('tell-text').value, '');
  assert.equal(element('status').textContent, 'Sent to foreman.');
});

test('a talk that starts the foreman opens its window', async () => {
  stub({ talk: { pane: 'w1:p9', label: 'foreman', started: true, queued: 1 } });
  global.location.hash = '';
  await tell('start an agent in trellis');
  assert.equal(global.location.hash, '#/pane/w1%3Ap9');
  assert.match(element('status').textContent, /starts and gets its page/);
});

test('/claude still opens a pane, and talk does not', async () => {
  const { rpcs } = stub();
  global.localStorage.setItem('coppice.cwds', JSON.stringify(['/repo']));
  await tell('/claude');
  assert.equal(rpcs.length, 1);
  assert.equal(rpcs[0].cmd, 'pane.create');
  assert.equal(rpcs[0].cwd, '/repo');
});

test('on a phone a talk that starts the foreman stays on home, where the chat is', async () => {
  stub({ talk: { pane: 'w1:p9', label: 'foreman', started: true, queued: 1 } });
  global.window.coppice.phone = () => true;
  global.location.hash = '#/roster';
  await tell('start an agent in trellis');
  assert.equal(global.location.hash, '#/roster');
});

test('a sentence shows in the chat at once and is marked sent when floor.talk answers', async () => {
  const { rpcs } = stub();
  const marks = [];
  global.window.coppice.chat = { sent: (text) => { marks.push('show ' + text); return { ok: () => marks.push('ok'), fail: () => marks.push('fail') }; } };
  await tell('fix the flaky test');
  assert.equal(rpcs.length, 1);
  assert.deepEqual(marks, ['show fix the flaky test', 'ok']);
  // Plumbing is not chat.
  marks.length = 0;
  await tell('/close docs');
  assert.deepEqual(marks, []);
});

test('a refused sentence leaves the chat and stays in the bar', async () => {
  stub({ rpcFails: new Error('the foreman has 32 sentences waiting already. Look at its window, then talk again.') });
  const marks = [];
  global.window.coppice.chat = { sent: () => ({ ok: () => marks.push('ok'), fail: () => marks.push('fail') }) };
  await tell('fix it');
  assert.deepEqual(marks, ['fail']);
  assert.equal(element('tell-text').value, 'fix it');
});

test('a talk the server refuses keeps the sentence and shows the reason', async () => {
  stub({ rpcFails: new Error('no default harness in /x/coppice.toml, so no foreman can start. Run coppice open HARNESS once to set one.') });
  await tell('fix it');
  assert.equal(element('tell-text').value, 'fix it');
  assert.match(element('status').textContent, /no default harness/);
});

test('a transcription fills the bar and does not send', async () => {
  class FakeRecorder {
    constructor() { this.mimeType = 'audio/webm'; this.l = {}; }
    addEventListener(n, fn) { this.l[n] = fn; }
    start() {}
    stop() {
      queueMicrotask(() => {
        this.l.dataavailable({ data: new Blob(['x']) });
        this.l.stop();
      });
    }
  }
  const { rpcs, apis } = stub();
  global.navigator.mediaDevices = { getUserMedia: async () => ({ getTracks: () => [] }) };
  global.MediaRecorder = FakeRecorder;
  const mic = element('tell-mic');
  mic.dispatchEvent({ type: 'click' });
  await settle();
  mic.dispatchEvent({ type: 'click' });
  await settle();
  await settle();
  assert.equal(element('tell-text').value, 'close the docs pane');
  assert.ok(apis.some((a) => a.path.startsWith('/api/voice/transcribe')));
  assert.equal(rpcs.length, 0, 'a transcription sent on its own');
  assert.equal(element('text').value, '', 'the pane prompt box took the tell bar text');
});

test('close refuses a label that names more than one live pane, and asks for the id', () => {
  const panes = [...PANES, { id: 'w1:p5', label: 'docs', cwd: '/x', state: 'working' },
    { id: 'w1:p6', label: 'docs', cwd: '/x', state: 'done', closed: true }];
  const plan = plumbingCommand('close docs', { panes, cwd: '/repo' });
  assert.equal(plan.req, undefined, 'a shared label closed a pane');
  assert.equal(plan.say, 'Two panes are called docs. Close one by id: w1:p2 or w1:p5.');
  assert.deepEqual(plumbingCommand('close w1:p5', { panes, cwd: '/repo' }).req, { cmd: 'pane.close', pane: 'w1:p5' });
});

// The page never guesses a directory from the running panes. With none
// set here it sends none, and the server picks one by its one-click rule.
test('a harness name never guesses a directory from the running panes', async () => {
  const { rpcs } = stub();
  await tell('/claude');
  assert.equal(rpcs.length, 1);
  assert.equal(rpcs[0].cwd, undefined, 'the page guessed a directory');
  assert.match(element('status').textContent, /where you last worked/);
});

test('a second clip that extends the tell bar says so', () => {
  assert.equal(applyReply({ text: 'more' }, 'some', 'tell bar').status, 'Added to the tell bar.');
  assert.equal(applyReply({ text: 'more' }, 'some').status, 'Added to the prompt box.');
});

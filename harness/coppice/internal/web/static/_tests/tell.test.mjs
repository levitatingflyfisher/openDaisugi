import test from 'node:test';
import assert from 'node:assert/strict';
import { reset, element } from './browser-stub.mjs';
import { applyReply } from '../record.js';
import { classifyTell, plumbingCommand, talkCommand, talkSay, mountTell } from '../newpane.js';

// Tell the floor: one bar on home. Plumbing runs first, the way the
// terminal floor reads a prompt line: a harness name opens a pane, close
// closes one. Any other sentence goes to floor.talk, which starts the
// foreman when none runs. A transcription fills the bar and never sends.

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

test('plumbing reads the way the terminal floor reads it', () => {
  assert.equal(classifyTell('claude'), 'plumbing');
  assert.equal(classifyTell('codex --resume'), 'plumbing');
  assert.equal(classifyTell('close docs'), 'plumbing');
  assert.equal(classifyTell('close the old docs pane when it is done'), 'talk');
  assert.equal(classifyTell('fix the flaky test'), 'talk');
  assert.equal(classifyTell('   '), 'empty');
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

test('claude still opens a pane, and talk does not', async () => {
  const { rpcs } = stub();
  global.localStorage.setItem('coppice.cwds', JSON.stringify(['/repo']));
  await tell('claude');
  assert.equal(rpcs.length, 1);
  assert.equal(rpcs[0].cmd, 'pane.create');
  assert.equal(rpcs[0].cwd, '/repo');
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
  await tell('claude');
  assert.equal(rpcs.length, 1);
  assert.equal(rpcs[0].cwd, undefined, 'the page guessed a directory');
  assert.match(element('status').textContent, /where you last worked/);
});

test('a second clip that extends the tell bar says so', () => {
  assert.equal(applyReply({ text: 'more' }, 'some', 'tell bar').status, 'Added to the tell bar.');
  assert.equal(applyReply({ text: 'more' }, 'some').status, 'Added to the prompt box.');
});

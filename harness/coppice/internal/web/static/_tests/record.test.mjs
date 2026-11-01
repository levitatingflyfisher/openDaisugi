import test from 'node:test';
import assert from 'node:assert/strict';
import { reset, element, timers, intervals } from './browser-stub.mjs';
import { createSpeech } from '../speech.js';
import { mountRecord, clipStore, clipLocks, CAP_MARGIN_S, clock, extensionFor, whyFailed, statusForReply, contentTypeFor, canRecord, applyReply, voiceDownMessage, cleanTranscript, retryVoice, noEngine, timing, LIVE_TEXT_OLD, tailText, SPEECH_BITS, LIVE_TEXT_OLD_WEB } from '../record.js';

const CLIP_BYTES = 'clip-bytes';

// FakeMediaRecorder stands in for the browser's own MediaRecorder. stop()
// returns immediately, the same as a real recorder, and delivers the whole
// clip on dataavailable, then fires stop, on a later microtask. That
// ordering is the point: finish() in record.js depends on every chunk
// already being collected by the time the stop event runs, and a
// synchronous fake could not tell that guarantee apart from a version that
// posts right after calling stop() with nothing collected yet.
class FakeMediaRecorder {
  constructor(stream) {
    this.stream = stream;
    this.mimeType = 'audio/webm;codecs=opus';
    this._listeners = {};
  }
  addEventListener(name, fn) { this._listeners[name] = fn; }
  start() {}
  stop() {
    queueMicrotask(() => {
      if (this._listeners.dataavailable) this._listeners.dataavailable({ data: new Blob([CLIP_BYTES]) });
      if (this._listeners.stop) this._listeners.stop();
    });
  }
}

function fakeStream(onStop) {
  return { getTracks: () => [{ stop: onStop || (() => {}) }] };
}

function installRecordingSupport() {
  global.navigator.mediaDevices = { getUserMedia: async () => fakeStream() };
  global.MediaRecorder = FakeMediaRecorder;
}

const flush = () => new Promise((r) => setImmediate(r));

test('statusForReply is quiet when cleanup did not change what was heard', () => {
  assert.equal(statusForReply({ text: 'run the tests', raw_text: 'run the tests' }), '');
});

test('statusForReply says what the engine heard when cleanup changed it', () => {
  assert.equal(
    statusForReply({ text: 'run the tests', raw_text: 'run the tests please' }),
    'The engine heard: run the tests please',
  );
});

test('statusForReply is quiet when the reply carries no raw_text at all', () => {
  assert.equal(statusForReply({ text: 'run the tests' }), '');
});

test('contentTypeFor reads the recorder\'s own mimeType', () => {
  assert.equal(contentTypeFor({ mimeType: 'audio/ogg;codecs=opus' }), 'audio/ogg;codecs=opus');
});

test('contentTypeFor falls back to audio/webm with no recorder or no mimeType', () => {
  assert.equal(contentTypeFor(null), 'audio/webm');
  assert.equal(contentTypeFor({}), 'audio/webm');
});

test('canRecord needs both getUserMedia and MediaRecorder', () => {
  installRecordingSupport();
  assert.equal(canRecord(), true);

  global.navigator.mediaDevices = {};
  assert.equal(canRecord(), false);

  installRecordingSupport();
  delete global.MediaRecorder;
  assert.equal(canRecord(), false);
});

// The failure this names: a silent clip, a muted microphone or a pocket
// dial, must never erase a prompt the operator already typed.
test('applyReply leaves an empty prompt box untouched and names the fix', () => {
  const result = applyReply({ text: '', raw_text: '' }, 'half a draft');
  assert.equal(result.text, 'half a draft');
  assert.equal(result.status, 'No speech was heard. Hold Record longer and speak closer to the microphone.');
});

// The failure this names: a second short clip must add to the first one,
// not throw it away, so two quick taps build one prompt.
test('applyReply appends to text already in the box with one space', () => {
  const result = applyReply({ text: 'the tests', raw_text: 'the tests' }, 'run  ');
  assert.equal(result.text, 'run the tests');
  assert.equal(result.status, 'Added to the prompt box.');
});

test('applyReply replaces an empty prompt box outright', () => {
  const result = applyReply({ text: 'run the tests', raw_text: 'run the tests please' }, '');
  assert.equal(result.text, 'run the tests');
  assert.equal(result.status, 'The engine heard: run the tests please');
});

// The failure this names: a renamed field, or a broken destructuring
// between the recorded blob and the fetch call, would leave the phone
// silently posting the wrong bytes or the wrong header while every pure
// test above stays green.
test('a full record and stop posts the whole clip and fills in the reply text', async () => {
  reset();
  installRecordingSupport();
  const calls = [];
  global.window.coppice = {
    api: async (path, options) => {
      calls.push({ path, options });
      return { text: 'run the tests', raw_text: 'run the tests please' };
    },
    status: (msg) => { global.window.coppice.lastStatus = msg; },
  };

  mountRecord();
  element('record').dispatchEvent({ type: 'click' }); // start
  await flush();
  assert.equal(element('record').textContent, 'Stop', 'the button did not show Stop while recording');

  element('record').dispatchEvent({ type: 'click' }); // stop, posts, resolves
  await flush();

  const posts = calls.filter((c) => c.path.startsWith('/api/voice/transcribe'));
  assert.equal(posts.length, 1);
  calls.splice(0, calls.length, ...posts);
  assert.equal(calls[0].path, '/api/voice/transcribe?cleanup=1');
  assert.equal(calls[0].options.method, 'POST');
  assert.equal(calls[0].options.headers['Content-Type'], 'audio/webm;codecs=opus');
  assert.ok(calls[0].options.body instanceof Blob, 'the request body was not a Blob');
  assert.equal(calls[0].options.body.size, Buffer.byteLength(CLIP_BYTES), 'the posted clip was not the whole recording');

  assert.equal(element('text').value, 'run the tests');
  assert.equal(global.window.coppice.lastStatus, 'The engine heard: run the tests please');
  assert.equal(element('record').textContent, 'Record', 'the button did not return to Record once sent');
});

// The failure this names: a phone that already has the text right in every
// ordinary clip must not show a second, redundant line on the status bar.
test('a clip cleanup did not change leaves the status line blank', async () => {
  reset();
  installRecordingSupport();
  global.window.coppice = {
    api: async () => ({ text: 'run the tests', raw_text: 'run the tests' }),
    status: (msg) => { global.window.coppice.lastStatus = msg; },
  };

  mountRecord();
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  element('record').dispatchEvent({ type: 'click' });
  await flush();

  assert.equal(global.window.coppice.lastStatus, '');
});

// The failure this names: a silent clip from a pocketed phone must not wipe
// out a prompt the operator was in the middle of typing.
test('an empty transcript leaves a half-typed prompt box alone', async () => {
  reset();
  installRecordingSupport();
  global.window.coppice = {
    api: async () => ({ text: '', raw_text: '' }),
    status: (msg) => { global.window.coppice.lastStatus = msg; },
  };
  element('text').value = 'do not lose this';

  mountRecord();
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  element('record').dispatchEvent({ type: 'click' });
  await flush();

  assert.equal(element('text').value, 'do not lose this');
  // No text landed, so the clip is kept and the line offers it again.
  assert.match(global.window.coppice.lastStatus.text, /^No speech was heard\. Your recording is kept\./);
  assert.deepEqual(global.window.coppice.lastStatus.actions.map((a) => a.label), ['Send again', 'Save']);
});

// The failure this names: a phone with no microphone permission must tell
// the operator exactly what to do, not fail silently or throw past record.js
// into the rest of the page.
test('a denied microphone leaves one sentence on the status line and never starts recording', async () => {
  reset();
  global.navigator.mediaDevices = { getUserMedia: async () => { throw new Error('NotAllowedError'); } };
  global.MediaRecorder = FakeMediaRecorder;
  const calls = [];
  global.window.coppice = {
    api: async (...args) => { calls.push(args); return {}; },
    status: (msg) => { global.window.coppice.lastStatus = msg; },
  };
  element('record').textContent = 'Record'; // the label index.html ships with

  mountRecord();
  element('record').dispatchEvent({ type: 'click' });
  await flush();

  assert.equal(
    global.window.coppice.lastStatus,
    'The microphone was not allowed. Allow it for this page, then tap Record again.',
  );
  assert.equal(element('record').textContent, 'Record');
  assert.equal(calls.filter((c) => String(c[0]).startsWith('/api/voice/transcribe')).length, 0, 'a denied microphone still posted a clip');
});

// The failure this names: a browser with no MediaRecorder at all must get
// a sentence naming the fix, not a ReferenceError from calling a
// constructor that does not exist.
test('no MediaRecorder support leaves one sentence and never asks for the microphone', async () => {
  reset();
  let getUserMediaCalled = false;
  global.navigator.mediaDevices = {
    getUserMedia: async () => { getUserMediaCalled = true; return fakeStream(); },
  };
  delete global.MediaRecorder;
  global.window.coppice = {
    api: async () => ({}),
    status: (msg) => { global.window.coppice.lastStatus = msg; },
  };

  mountRecord();
  element('record').dispatchEvent({ type: 'click' });
  await flush();

  assert.equal(global.window.coppice.lastStatus, 'This browser cannot record audio. Try a newer browser or device.');
  assert.equal(getUserMediaCalled, false, 'the microphone was requested on a browser with no MediaRecorder');
});

// Over plain http from another machine the browser gives no microphone at
// all. The mic says that this is why, and that the fix is on the box, not
// that the browser is too old.
test('a page that is not a secure context says the mic needs https and names the fix on the box', async () => {
  reset();
  let getUserMediaCalled = false;
  delete global.navigator.mediaDevices;
  global.window.isSecureContext = false;
  global.window.coppice = {
    api: async () => ({}),
    status: (msg) => { global.window.coppice.lastStatus = msg; },
  };
  try {
    mountRecord();
    element('record').dispatchEvent({ type: 'click' });
    await flush();
  } finally {
    delete global.window.isSecureContext;
  }
  const m = global.window.coppice.lastStatus;
  assert.equal(typeof m, 'object', 'the mic shows a message with its fix');
  assert.ok(m.text.startsWith('The mic needs https here.'), m.text);
  assert.ok(m.text.includes('coppice web serve --tls tailscale'), m.text);
  assert.equal(m.away, true, 'the fix is on the box');
  assert.equal(getUserMediaCalled, false);
});

// The failure this names: a transcribe the server refuses for another
// reason, a clip too large, say, must leave the server's own sentence on
// the status line, with no voice Retry, since starting voice changes nothing. The clip is kept.
test('a refused transcribe reply leaves the server\'s own sentence on the status line', async () => {
  reset();
  installRecordingSupport();
  global.window.coppice = {
    api: async () => { const e = new Error('The clip could not be read. Record it again.'); e.status = 400; throw e; },
    status: (msg) => { global.window.coppice.lastStatus = msg; },
  };

  mountRecord();
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  element('record').dispatchEvent({ type: 'click' });
  await flush();

  const refused = global.window.coppice.lastStatus;
  assert.equal(refused.text, 'The clip could not be read. Record it again. Your recording is kept.');
  assert.ok(!refused.actions.some((a) => a.kind === 'voice'), 'no voice Retry for a refused clip');
  assert.equal(element('record').textContent, 'Record', 'the button was stuck off Record after a refusal');
  assert.equal(element('text').value, '', 'a refused reply still wrote into the prompt box');
});

// The failure this names: the text goes into a box that is later typed
// into a pty, where a newline is Enter and an escape drives the agent's
// screen. Only printable text may land there.
test('cleanTranscript keeps only printable text', () => {
  assert.equal(cleanTranscript('a\r\nb\x1b[31mc'), 'a b[31mc');
  assert.equal(cleanTranscript('one\ttwo'), 'one two');
  assert.equal(cleanTranscript('  hi\x00\x07\x7f there  '), 'hi there');
  assert.equal(cleanTranscript('café \u009bx\u0085y'), 'café xy');
  assert.equal(cleanTranscript('\n\n'), '');
});

test('applyReply puts only printable text in the box', () => {
  const result = applyReply({ text: 'a\r\nb\x1b[31mc' }, 'run');
  assert.equal(result.text, 'run a b[31mc');
  assert.equal(applyReply({ text: '\x1b\n' }, 'kept').text, 'kept');
});

test('voiceDownMessage reads a status reply into a message with its fix', () => {
  assert.equal(voiceDownMessage({ ready: true, message: '' }), null);
  assert.equal(voiceDownMessage({ text: 'hello' }), null);
  assert.equal(voiceDownMessage(null), null);
  const starting = voiceDownMessage({ ready: false, message: 'Voice is starting. Wait a moment, then press Retry.' });
  assert.equal(starting.text, 'Voice is starting. Wait a moment, then press Retry.');
  assert.deepEqual(starting.actions, [{ kind: 'voice', label: 'Retry' }]);
  assert.equal(voiceDownMessage({ ready: false }).text, 'Voice cannot run now. Press Retry.');
  const extra = voiceDownMessage({ ready: false, message: 'x', command: "pip install 'opendaisugi[voice]'" });
  assert.deepEqual(extra.actions.map((a) => a.kind), ['shell', 'voice']);
  const off = voiceDownMessage({ ready: false, state: 'off', away: true, message: 'Voice is off in coppice.toml.' });
  assert.deepEqual(off.actions, []);
  assert.equal(off.away, true);
});

test('retryVoice asks the server to start voice and says what came of it', async () => {
  const calls = [];
  let up = false;
  const api = async (path, options) => {
    calls.push([path, options.method]);
    return up ? { ready: true } : { ready: false, message: 'Voice is starting. Wait a moment, then press Retry.' };
  };
  assert.equal((await retryVoice(api)).text, 'Voice is starting. Wait a moment, then press Retry.');
  up = true;
  assert.equal(await retryVoice(api), 'Voice is ready. Tap the mic.');
  assert.deepEqual(calls[0], ['/api/voice/retry', 'POST']);
});

// The failure this names: a mic that cannot work was a dead control. It
// says why, never asks for the microphone, and Retry starts voice again.
test('a mic with voice down says why and Retry brings it back', async () => {
  reset();
  let asked = false;
  global.navigator.mediaDevices = { getUserMedia: async () => { asked = true; return fakeStream(); } };
  global.MediaRecorder = FakeMediaRecorder;
  let ready = false;
  const calls = [];
  const downLine = "Voice needs daisugi's voice extra. Install it with pip install 'opendaisugi[voice]', then press Retry.";
  global.window.coppice = {
    api: async (path, options) => {
      calls.push({ path, options });
      if (path === '/api/voice/retry') ready = true;
      return ready ? { ready: true, state: 'ready', message: '' }
        : { ready: false, state: 'down', message: downLine, command: "pip install 'opendaisugi[voice]'" };
    },
    status: (msg) => { global.window.coppice.lastStatus = msg; },
  };
  element('record').textContent = 'Record';

  mountRecord();
  await flush();
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  const shown = global.window.coppice.lastStatus;
  assert.equal(shown.text, downLine);
  assert.deepEqual(shown.actions.map((a) => [a.kind, a.command]),
    [['shell', "pip install 'opendaisugi[voice]'"], ['voice', undefined]]);
  assert.equal(asked, false, 'the microphone was asked for while voice is down');
  assert.equal(element('record').textContent, 'Record');

  // The Retry on the status line is what the app runs.
  assert.equal(await retryVoice(global.window.coppice.api), 'Voice is ready. Tap the mic.');
  assert.ok(calls.some((c) => c.path === '/api/voice/retry' && c.options.method === 'POST'), 'Retry sent nothing');

  element('record').dispatchEvent({ type: 'click' });
  await flush();
  assert.equal(asked, true, 'the mic did not record once voice was back');
});

// The failure this names: voice went down after the page asked. The clip
// comes back 503 with the reason, and Retry shows beside it.
test('a clip that finds voice down shows the reason and Retry', async () => {
  reset();
  installRecordingSupport();
  const downLine = 'Could not reach the voice server at http://127.0.0.1:1. Press Retry.';
  global.window.coppice = {
    api: async (path) => {
      if (path.startsWith('/api/voice/transcribe')) { const e = new Error(downLine); e.status = 503; throw e; }
      return { ready: true, state: 'ready', message: '' };
    },
    status: (msg) => { global.window.coppice.lastStatus = msg; },
  };

  mountRecord();
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  element('record').dispatchEvent({ type: 'click' });
  await flush();

  assert.equal(global.window.coppice.lastStatus.text, downLine);
  assert.deepEqual(global.window.coppice.lastStatus.actions.map((a) => a.label), ['Retry', 'Send again', 'Save']);
  assert.equal(global.window.coppice.lastStatus.actions[0].kind, 'voice');
});

// The failure this names: voice runs but has no speech engine, so the clip
// comes back 503 engine_unavailable. The mic said the server's own line, a
// program name and a Retry that cannot help. It says why and what fixes it.
test('a clip that finds no speech engine says so and where the fix is, with no Retry', async () => {
  reset();
  installRecordingSupport();
  const calls = [];
  global.window.coppice = {
    api: async (path) => {
      calls.push(path);
      if (path.startsWith('/api/voice/transcribe')) {
        const e = new Error('moonshine-cli is not on PATH. Build it with scripts/install.sh --voice, then try again.');
        e.status = 503;
        e.code = 'engine_unavailable';
        throw e;
      }
      return { ready: true, state: 'ready', message: '' };
    },
    status: (msg) => { global.window.coppice.lastStatus = msg; },
  };

  mountRecord();
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  element('record').dispatchEvent({ type: 'click' });
  await flush();

  const s = global.window.coppice.lastStatus;
  assert.match(s.text, /^No speech engine here\./);
  assert.match(s.text, /scripts\/install\.sh --voice/);
  assert.match(s.text, /On the computer that runs coppice/);
  assert.deepEqual((s.actions || []).map((a) => a.label), ['Save'], 'Retry cannot install an engine; the audio can still be saved');
});

test('noEngine tells a missing engine from a model that is still loading', () => {
  const e = (code, message) => Object.assign(new Error(message), { status: 503, code });
  assert.equal(noEngine(e('engine_unavailable', 'moonshine-cli is not on PATH. Build it.')), true);
  assert.equal(noEngine(e('engine_unavailable', 'The Python voice extra is not installed.')), true);
  assert.equal(noEngine(e('engine_loading', 'The model is loading.')), false);
  assert.equal(noEngine(e('engine_unavailable', 'A model file is missing.')), false);
  assert.equal(noEngine(new Error('x')), false);
});

// The failure this names: the operator taps Stop, the transcription takes a
// while, and a tap on the button in that window starts a second recorder.
// The label then reads Record once the first send resolves while the
// second recorder is still live, and the very next tap, meant to start a
// new clip, stops that hidden one instead.
test('a tap during a send starts no second recorder and the label stays Sending', async () => {
  reset();
  let recorderCount = 0;
  class CountingRecorder extends FakeMediaRecorder {
    constructor(...args) { super(...args); recorderCount += 1; }
  }
  global.navigator.mediaDevices = { getUserMedia: async () => fakeStream() };
  global.MediaRecorder = CountingRecorder;

  let resolveApi;
  global.window.coppice = {
    api: () => new Promise((resolve) => { resolveApi = resolve; }),
    status: (msg) => { global.window.coppice.lastStatus = msg; },
  };

  mountRecord();
  element('record').dispatchEvent({ type: 'click' }); // start
  await flush();
  assert.equal(recorderCount, 1);

  element('record').dispatchEvent({ type: 'click' }); // stop, begins sending, hangs on the api call
  await flush();
  assert.equal(element('record').textContent, 'Sending');

  element('record').dispatchEvent({ type: 'click' }); // a tap while sending: must do nothing
  await flush();
  assert.equal(recorderCount, 1, 'a tap during a send started a second recorder');
  assert.equal(element('record').textContent, 'Sending', 'the label moved during a pending send');

  resolveApi({ text: 'done' });
  await flush();
  assert.equal(element('record').textContent, 'Record');
});

// The failure this names: the operator taps Record twice while the
// browser's own permission prompt is still on screen. A version that keeps
// no state during that wait asks for the microphone twice, and stopping the
// one stream it knows about leaves the other one open with the recording
// indicator still lit and nothing on screen that can close it.
test('two taps during the permission wait leave exactly one live stream, and Stop closes it', async () => {
  reset();
  let getUserMediaCalls = 0;
  let resolveGetUserMedia;
  let tracksStopped = 0;
  global.navigator.mediaDevices = {
    getUserMedia: () => {
      getUserMediaCalls += 1;
      return new Promise((resolve) => { resolveGetUserMedia = resolve; });
    },
  };
  global.MediaRecorder = FakeMediaRecorder;
  global.window.coppice = {
    api: async () => ({ text: 'done', raw_text: 'done' }),
    status: (msg) => { global.window.coppice.lastStatus = msg; },
  };

  mountRecord();
  element('record').dispatchEvent({ type: 'click' }); // start, awaits permission
  await flush();
  element('record').dispatchEvent({ type: 'click' }); // a second tap during the wait: must do nothing
  await flush();

  assert.equal(getUserMediaCalls, 1, 'a second tap during the permission wait asked for the microphone again');

  resolveGetUserMedia(fakeStream(() => { tracksStopped += 1; }));
  await flush();
  assert.equal(element('record').textContent, 'Stop', 'recording never started once permission arrived');

  element('record').dispatchEvent({ type: 'click' }); // Stop
  await flush();
  await flush();

  assert.equal(tracksStopped, 1, 'the microphone stream was not closed exactly once');
});

test('Retry on a starting voice follows it to Voice is ready', async () => {
  reset();
  const said = [];
  let step = 0;
  const replies = [
    { ready: false, state: 'starting', message: 'Voice is starting.' },
    { ready: false, state: 'starting', message: 'Voice is starting.', fetch: { done: 41000000, total: 160000000 } },
    { ready: true, state: 'ready', message: '' },
  ];
  const api = async () => replies[Math.min(step++, replies.length - 1)];
  const first = await retryVoice(api, (m) => said.push(m));
  assert.equal(first.text, 'Voice is starting.');
  for (let i = 0; i < 5 && said.at(-1)?.text !== 'Voice is ready.'; i += 1) {
    await flush();
    const t = timers.filter((x) => !x.cancelled).pop();
    t.cancelled = true;
    t.fn();
    await flush();
  }
  assert.equal(said.at(-1).text, 'Voice is ready.');
  assert.ok(said.some((m) => m.text === 'Getting the speech model, 41 of 160 MB.'));
});

test('voiceDownMessage says how far the speech model is, with no Retry', () => {
  const m = voiceDownMessage({ ready: false, state: 'starting', message: 'Voice is starting.', fetch: { done: 41000000, total: 160000000 } });
  assert.equal(m.text, 'Getting the speech model, 41 of 160 MB.');
  assert.deepEqual(m.actions, []);
  // Megabytes are 10**6, rounded half up.
  assert.equal(voiceDownMessage({ ready: false, state: 'starting', fetch: { done: 499999, total: 1500000 } }).text,
    'Getting the speech model, 0 of 2 MB.');
  // A starting voice with no fetch keeps its Retry line.
  assert.deepEqual(voiceDownMessage({ ready: false, state: 'starting', message: 'x' }).actions, [{ kind: 'voice', label: 'Retry' }]);
});

// The failure this names: the voice banner showed on every load, before
// the owner had pressed the mic.
test('the page says nothing about voice until the first Mic press', async () => {
  reset();
  installRecordingSupport();
  const said = [];
  global.window.coppice = {
    api: async () => ({ ready: false, state: 'down', message: "Voice needs daisugi's voice extra. Press Retry." }),
    status: (msg) => said.push(msg),
  };
  mountRecord();
  await flush();
  assert.deepEqual(said, [], 'a voice banner showed before the first Mic press');
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  assert.equal(said.length, 1);
  assert.equal(said[0].text, "Voice needs daisugi's voice extra. Press Retry.");
});

// The failure this names: the first start was a silent minute. The mic
// follows the download, then says Voice is ready.
test('a mic pressed while voice gets its model follows the download and ends with Voice is ready', async () => {
  reset();
  installRecordingSupport();
  const said = [];
  let step = 0;
  const replies = [
    { ready: false, state: 'starting', message: 'Voice is starting.', fetch: { done: 41000000, total: 160000000 } },
    { ready: false, state: 'starting', message: 'Voice is starting.', fetch: { done: 100000000, total: 160000000 } },
    { ready: true, state: 'ready', message: '' },
  ];
  global.window.coppice = {
    api: async () => replies[Math.min(step++, replies.length - 1)],
    status: (msg) => said.push(msg),
  };
  mountRecord(); // step 0 is read here, silently
  await flush();
  assert.deepEqual(said, []);
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  assert.equal(said.at(-1).text, 'Getting the speech model, 100 of 160 MB.');
  for (let i = 0; i < 4 && said.at(-1).text !== 'Voice is ready.'; i += 1) {
    const t = timers.filter((x) => !x.cancelled).pop();
    t.cancelled = true;
    t.fn();
    await flush();
  }
  assert.equal(said.at(-1).text, 'Voice is ready.');
});

// The failure this names: the chat bar's mic could send what it heard. It
// only fills the chat box; a Send is the owner's.
test('the chat bar mic fills the chat box and sends nothing', async () => {
  reset();
  installRecordingSupport();
  const calls = [];
  global.window.coppice = {
    api: async (path) => {
      calls.push(path);
      return path.startsWith('/api/voice/transcribe') ? { text: 'check the tests' } : { ready: true };
    },
    status: () => {},
  };
  element('tell-mic').textContent = 'Mic';
  mountRecord('tell-mic', 'tell-text', 'chat box');
  await flush();
  element('tell-mic').dispatchEvent({ type: 'click' });
  await flush();
  element('tell-mic').dispatchEvent({ type: 'click' });
  await flush();
  await flush();
  assert.equal(element('tell-text').value, 'check the tests');
  assert.ok(calls.every((c) => c.startsWith('/api/voice/')), 'the mic called ' + calls.join(', '));
});

// A clip that fills the tell bar marks the next message to the steward as
// spoken, so its reply is read aloud. A clip into a pane's own prompt box
// is for that agent, and marks nothing.
test('the tell bar mic marks the message as spoken', async () => {
  reset();
  installRecordingSupport();
  const speech = { marks: 0, markVoiced() { this.marks += 1; }, pressMic() {} };
  global.window.coppice = { api: async (path) => (path.startsWith('/api/voice/transcribe') ? { text: 'check the tests' } : { ready: true }), status: () => {}, speech };
  element('tell-mic').textContent = 'Mic';
  mountRecord('tell-mic', 'tell-text', 'tell bar', true);
  await flush();
  element('tell-mic').dispatchEvent({ type: 'click' });
  await flush();
  element('tell-mic').dispatchEvent({ type: 'click' });
  await flush();
  await flush();
  assert.equal(speech.marks, 1);
});

test('a pane prompt mic marks nothing, so a typed message to the steward is not read aloud', async () => {
  reset();
  installRecordingSupport();
  const spoken = [];
  const chat = [];
  const speech = createSpeech({
    say: async () => { throw new Error('no server'); },
    info: async () => ({ mode: 'browser' }),
    chat: async () => chat.slice(),
    audio: () => null,
    synth: () => ({ speak: async (t) => { spoken.push(t); }, cancel() {}, unlock() {} }),
    status: () => {},
    storage: null,
  });
  global.window.coppice = { api: async (path) => (path.startsWith('/api/voice/transcribe') ? { text: 'echo hello' } : { ready: true }), status: () => {}, speech };
  mountRecord(); // the pane's own record button and prompt box
  await flush();
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  await flush();
  assert.equal(element('text').value, 'echo hello');
  // The owner then types a sentence to the steward, and the steward answers.
  await speech.beforeSend();
  chat.push({ id: 1, role: 'agent', text: 'The steward answers the typed sentence.' });
  await speech.onEvent({ event: 'messages', chat: true });
  await flush();
  assert.deepEqual(spoken, []);
});

// The failure this names: the owner talked for over a minute, the server
// said no, and the recording was gone. A failed clip stays, and Send again
// posts the very same bytes.
test('a failed clip is kept and Send again posts the same blob', async () => {
  reset();
  installRecordingSupport();
  const posts = [];
  let fail = true;
  global.window.coppice = {
    api: async (path, options) => {
      if (!path.startsWith('/api/voice/transcribe')) return { ready: true, state: 'ready', message: '' };
      posts.push(options.body);
      if (fail) { const e = new Error('The server answered 500.'); e.status = 500; throw e; }
      return { text: 'a long prompt', raw_text: 'a long prompt' };
    },
    status: (msg) => { global.window.coppice.lastStatus = msg; },
  };

  mountRecord();
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  element('record').dispatchEvent({ type: 'click' });
  await flush();

  const s = global.window.coppice.lastStatus;
  assert.match(s.text, /The server answered 500\. Your recording is kept\./);
  assert.deepEqual(s.actions.map((a) => a.label), ['Send again', 'Save']);
  assert.equal(element('text').value, '', 'no text can have landed yet');
  assert.equal(element('record').textContent, 'Record');

  fail = false;
  await s.actions[0].run();
  await flush();
  assert.equal(posts.length, 2);
  assert.equal(posts[1], posts[0], 'Send again must post the same blob object');
  assert.equal(posts[1].size, Buffer.byteLength(CLIP_BYTES));
  assert.equal(element('text').value, 'a long prompt');
});

test('a clip that failed to send blocks a new recording until it is sent, saved or discarded', async () => {
  reset();
  installRecordingSupport();
  let asked = 0;
  global.navigator.mediaDevices = { getUserMedia: async () => { asked += 1; return fakeStream(); } };
  global.window.coppice = {
    api: async (path) => {
      if (path.startsWith('/api/voice/transcribe')) { const e = new Error('The clip is longer than 20 minutes. Send a shorter clip.'); e.status = 413; throw e; }
      return { ready: true, state: 'ready', message: '' };
    },
    status: (msg) => { global.window.coppice.lastStatus = msg; },
  };
  mountRecord();
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  assert.equal(asked, 1);
  assert.match(global.window.coppice.lastStatus.text, /longer than 20 minutes/);

  element('record').dispatchEvent({ type: 'click' });
  await flush();
  assert.equal(asked, 1, 'a new clip must not start over a clip with no text');
  const pending = global.window.coppice.lastStatus;
  assert.deepEqual(pending.actions.map((a) => a.label), ['Send again', 'Save', 'Discard']);

  pending.actions[2].run(); // Discard
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  assert.equal(asked, 2);
});

test('a network failure keeps the clip and Save downloads the audio', async () => {
  reset();
  installRecordingSupport();
  const saved = [];
  global.URL.createObjectURL = (b) => { saved.push(b); return 'blob:x'; };
  global.URL.revokeObjectURL = () => {};
  global.document.createElement = (tag) => ({ tag, click() { saved.push(this.download); } });
  global.window.coppice = {
    api: async (path) => {
      if (path.startsWith('/api/voice/transcribe')) throw new TypeError('Failed to fetch');
      return { ready: true, state: 'ready', message: '' };
    },
    status: (msg) => { global.window.coppice.lastStatus = msg; },
  };
  mountRecord();
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  const s = global.window.coppice.lastStatus;
  assert.match(s.text, /did not reach the server \(Failed to fetch\)\. Your recording is kept\./);
  s.actions[1].run(); // Save
  assert.ok(saved[0] instanceof Blob);
  assert.match(saved[1], /^coppice-clip-.*\.webm$/);
  // Save keeps the clip: a download can do nothing without an error.
  const after = global.window.coppice.lastStatus;
  assert.match(after.text, /^Saved to your downloads\. Discard the recording once you have the file\./);
  assert.deepEqual(after.actions.map((a) => a.label), ['Send again', 'Discard']);
});

test('recording shows the elapsed time on the button', async () => {
  reset();
  installRecordingSupport();
  global.window.coppice = { api: async () => ({ ready: true }), status: () => {} };
  mountRecord();
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  assert.equal(element('record').textContent, 'Stop');
  const live = intervals.filter((i) => !i.cancelled);
  assert.ok(live.length > 0, 'no ticker runs while recording');
});

test('clock, extensionFor and whyFailed', () => {
  assert.equal(clock(0), '0:00');
  assert.equal(clock(125.9), '2:05');
  assert.equal(extensionFor('audio/webm;codecs=opus'), 'webm');
  assert.equal(extensionFor('audio/mp4'), 'm4a');
  assert.equal(whyFailed(Object.assign(new Error('Too big.'), { status: 413 })), 'Too big.');
  assert.equal(whyFailed(new TypeError('Failed to fetch')), 'The clip did not reach the server (Failed to fetch).');
});

// fakeStore is an in-memory IndexedDB stand-in with the clipStore backend shape.
function fakeStore() {
  const data = new Map();
  return {
    data,
    get: async (k) => data.get(k),
    put: async (k, v) => { data.set(k, v); return true; },
    del: async (k) => { data.delete(k); return true; },
    keys: async () => [...data.keys()],
  };
}

// M2: voice answered 503, then the status call still says down. The clip's
// buttons must stay.
test('a 503 then a status that is still down keeps Send again and Save', async () => {
  reset();
  installRecordingSupport();
  global.window.coppice = {
    api: async (path) => {
      if (path.startsWith('/api/voice/transcribe')) { const e = new Error('Voice is loading.'); e.status = 503; throw e; }
      return { ready: false, state: 'starting', message: 'Voice is starting. Press Retry.', action: 'retry' };
    },
    status: (msg) => { global.window.coppice.lastStatus = msg; },
  };
  mountRecord();
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  await flush();
  const labels = global.window.coppice.lastStatus.actions.map((a) => a.label);
  assert.ok(labels.includes('Send again') && labels.includes('Save'), 'got ' + labels);
});

// M3: the clip is stored while it sends, survives a reload, and is removed
// once its text is in the box.
test('a stored clip comes back after a reload and goes once its text lands', async () => {
  reset();
  installRecordingSupport();
  const store = fakeStore();
  clipStore.backend = store;
  try {
    let fail = true;
    global.window.coppice = {
      api: async (path) => {
        if (!path.startsWith('/api/voice/transcribe')) return { ready: true, state: 'ready' };
        if (fail) { const e = new Error('Dropped.'); e.status = 500; throw e; }
        return { text: 'recovered words', raw_text: 'recovered words' };
      },
      status: (msg) => { global.window.coppice.lastStatus = msg; },
    };
    mountRecord();
    element('record').dispatchEvent({ type: 'click' });
    await flush();
    element('record').dispatchEvent({ type: 'click' });
    await flush();
    assert.equal(store.data.size, 1, 'the clip must be stored before it is sent');

    // A reload: a fresh mount finds the clip.
    global.window.coppice.lastStatus = '';
    mountRecord();
    await flush();
    const line = global.window.coppice.lastStatus;
    assert.match(line.text, /recording from before/);
    assert.deepEqual(line.actions.map((a) => a.label), ['Send again', 'Save', 'Discard']);
    fail = false;
    await line.actions[0].run();
    await flush();
    assert.equal(element('text').value, 'recovered words');
    assert.equal(store.data.size, 0, 'the stored clip must go once its text landed');
  } finally {
    clipStore.backend = null;
  }
});

test('without storage the clip stays in memory and storage errors are swallowed', async () => {
  reset();
  installRecordingSupport();
  clipStore.backend = { get: async () => { throw new Error('no'); }, put: async () => { throw new Error('no'); }, del: async () => { throw new Error('no'); } };
  try {
    global.window.coppice = {
      api: async (path) => {
        if (path.startsWith('/api/voice/transcribe')) { const e = new Error('x'); e.status = 500; throw e; }
        return { ready: true };
      },
      status: (msg) => { global.window.coppice.lastStatus = msg; },
    };
    mountRecord();
    await flush();
    element('record').dispatchEvent({ type: 'click' });
    await flush();
    element('record').dispatchEvent({ type: 'click' });
    await flush();
    assert.deepEqual(global.window.coppice.lastStatus.actions.map((a) => a.label), ['Send again', 'Save']);
  } finally {
    clipStore.backend = null;
  }
});

test('Discard removes the stored clip', async () => {
  reset();
  installRecordingSupport();
  const store = fakeStore();
  clipStore.backend = store;
  try {
    global.window.coppice = {
      api: async (path) => {
        if (path.startsWith('/api/voice/transcribe')) { const e = new Error('x'); e.status = 500; throw e; }
        return { ready: true };
      },
      status: (msg) => { global.window.coppice.lastStatus = msg; },
    };
    mountRecord();
    element('record').dispatchEvent({ type: 'click' });
    await flush();
    element('record').dispatchEvent({ type: 'click' });
    await flush();
    element('record').dispatchEvent({ type: 'click' });
    await flush();
    global.window.coppice.lastStatus.actions[2].run(); // Discard
    await flush();
    assert.equal(store.data.size, 0);
  } finally {
    clipStore.backend = null;
  }
});

// M6: the cap comes from the status reply; a warning two minutes before the
// stop, and a stop a margin before the cap.
test('the recorder warns two minutes before it stops, and stops a margin before the server cap', async () => {
  reset();
  installRecordingSupport();
  const realNow = Date.now;
  let now = 1_000_000;
  Date.now = () => now;
  try {
    const posts = [];
    global.window.coppice = {
      api: async (path, options) => {
        if (path.startsWith('/api/voice/transcribe')) { posts.push(options.body); return { text: 'long dictation', raw_text: 'long dictation' }; }
        return { ready: true, state: 'ready', max_seconds: 1200 };
      },
      status: (msg) => { global.window.coppice.lastStatus = msg; },
    };
    mountRecord();
    await flush();
    element('record').dispatchEvent({ type: 'click' });
    await flush();
    const tick = () => intervals.filter((i) => !i.cancelled).at(-1).fn();
    const stopAt = 1200 - CAP_MARGIN_S;
    assert.ok(CAP_MARGIN_S >= 5, 'the margin must cover the recorder delay');
    now += (stopAt - 120) * 1000;
    tick();
    assert.match(global.window.coppice.lastStatus.text, /2 minutes left/);
    assert.equal(element('record').textContent, 'Stop 17:50/19:50');
    assert.equal(posts.length, 0);
    now += 119_000;
    tick();
    assert.equal(posts.length, 0, 'the page must not stop before its own stop time');
    now += 1_000; // the stop time, shy of the server cap
    tick();
    assert.match(global.window.coppice.lastStatus.text, /20 minute limit/);
    await flush();
    await flush();
    assert.equal(posts.length, 1, 'the clip must be sent before the cap');
    assert.equal(element('text').value, 'long dictation');
  } finally {
    Date.now = realNow;
  }
});

test('the limit shows on the button only near the end', async () => {
  reset();
  installRecordingSupport();
  const realNow = Date.now;
  let now = 1_000_000;
  Date.now = () => now;
  try {
    global.window.coppice = {
      api: async () => ({ ready: true, state: 'ready', max_seconds: 1200 }),
      status: (msg) => { global.window.coppice.lastStatus = msg; },
    };
    mountRecord();
    await flush();
    element('record').dispatchEvent({ type: 'click' });
    await flush();
    now += 61_000;
    intervals.filter((i) => !i.cancelled).at(-1).fn();
    assert.equal(element('record').textContent, 'Stop 1:01');
  } finally {
    Date.now = realNow;
  }
});

// N2: a reply with gaps puts the text in the box and keeps the clip.
test('a reply with gaps keeps the clip and says how many', async () => {
  reset();
  installRecordingSupport();
  const store = fakeStore();
  clipStore.backend = store;
  try {
    global.window.coppice = {
      api: async (path) => {
        if (path.startsWith('/api/voice/transcribe')) return { text: 'some words', raw_text: 'some words', gaps: 2 };
        return { ready: true, state: 'ready' };
      },
      status: (msg) => { global.window.coppice.lastStatus = msg; },
    };
    mountRecord();
    element('record').dispatchEvent({ type: 'click' });
    await flush();
    element('record').dispatchEvent({ type: 'click' });
    await flush();
    await flush();
    assert.equal(element('text').value, 'some words');
    const line = global.window.coppice.lastStatus;
    assert.match(line.text, /2 parts of the recording could not be heard/);
    assert.deepEqual(line.actions.map((a) => a.label), ['Send again', 'Save', 'Discard']);
    assert.equal(store.data.size, 1, 'the clip must stay stored');
  } finally {
    clipStore.backend = null;
  }
});

// A clip cut at the server's cap keeps the audio for Save.
test('a reply that cut the end keeps the clip and says how much', async () => {
  reset();
  installRecordingSupport();
  const store = fakeStore();
  clipStore.backend = store;
  try {
    global.window.coppice = {
      api: async (path) => {
        if (path.startsWith('/api/voice/transcribe')) return { text: 'most words', raw_text: 'most words', cut_s: 7 };
        return { ready: true, state: 'ready' };
      },
      status: (msg) => { global.window.coppice.lastStatus = msg; },
    };
    mountRecord();
    element('record').dispatchEvent({ type: 'click' });
    await flush();
    element('record').dispatchEvent({ type: 'click' });
    await flush();
    await flush();
    assert.equal(element('text').value, 'most words');
    const line = global.window.coppice.lastStatus;
    assert.match(line.text, /last 7 s/);
    assert.deepEqual(line.actions.map((a) => a.label), ['Save', 'Discard']);
    assert.equal(store.data.size, 1);
  } finally {
    clipStore.backend = null;
  }
});

// A clip let go before the browser grants its lock must not hold the lock.
test('a lock granted after the clip was let go is released at once', async () => {
  let grantLater = null;
  let held = false;
  let freed = false;
  clipLocks.backend = {
    request(name, fn) {
      return new Promise((resolve) => {
        grantLater = () => {
          held = true;
          resolve(fn({ name }).then(() => { freed = true; }));
        };
      });
    },
  };
  try {
    const letGo = clipLocks.hold('a:1');
    letGo();
    grantLater();
    await flush();
    assert.ok(held && freed, 'the late grant must let go at once');
  } finally {
    clipLocks.backend = null;
  }
});

// A reply with both gaps and a cut says both.
test('a reply with gaps and a cut says both and offers Send again', async () => {
  reset();
  installRecordingSupport();
  const store = fakeStore();
  clipStore.backend = store;
  try {
    global.window.coppice = {
      api: async (path) => {
        if (path.startsWith('/api/voice/transcribe')) return { text: 'some words', raw_text: 'some words', gaps: 1, cut_s: 15 };
        return { ready: true, state: 'ready' };
      },
      status: (msg) => { global.window.coppice.lastStatus = msg; },
    };
    mountRecord();
    element('record').dispatchEvent({ type: 'click' });
    await flush();
    element('record').dispatchEvent({ type: 'click' });
    await flush();
    await flush();
    const line = global.window.coppice.lastStatus;
    assert.match(line.text, /1 part of the recording could not be heard/);
    assert.match(line.text, /last 15 s was past the limit/);
    assert.deepEqual(line.actions.map((a) => a.label), ['Send again', 'Save', 'Discard']);
  } finally {
    clipStore.backend = null;
  }
});

// Send again after a reply with gaps put the whole text in the box twice.
// The clip replaces the text it put there before.
test('Send again for the same clip replaces its earlier text, never appends a copy', async () => {
  reset();
  installRecordingSupport();
  const store = fakeStore();
  clipStore.backend = store;
  try {
    let gaps = 1;
    global.window.coppice = {
      api: async (path) => {
        if (path.startsWith('/api/voice/transcribe')) {
          return gaps ? { text: 'first half only', raw_text: 'first half only', gaps }
            : { text: 'first half and second half', raw_text: 'first half and second half' };
        }
        return { ready: true, state: 'ready' };
      },
      status: (msg) => { global.window.coppice.lastStatus = msg; },
    };
    mountRecord();
    element('text').value = 'my draft';
    element('record').dispatchEvent({ type: 'click' });
    await flush();
    element('record').dispatchEvent({ type: 'click' });
    await flush();
    await flush();
    assert.equal(element('text').value, 'my draft first half only');
    gaps = 0;
    await global.window.coppice.lastStatus.actions[0].run();
    await flush();
    await flush();
    assert.equal(element('text').value, 'my draft first half and second half');
  } finally {
    clipStore.backend = null;
  }
});

// Send again takes out only the clip's own text. The operator's line breaks
// and spacing elsewhere in the box stay as they were.
test('Send again keeps the operator line breaks around the clip text', async () => {
  reset();
  installRecordingSupport();
  const store = fakeStore();
  clipStore.backend = store;
  try {
    let gaps = 1;
    global.window.coppice = {
      api: async (path) => {
        if (path.startsWith('/api/voice/transcribe')) {
          return gaps ? { text: 'first half only', raw_text: 'first half only', gaps }
            : { text: 'whole sentence', raw_text: 'whole sentence' };
        }
        return { ready: true, state: 'ready' };
      },
      status: (msg) => { global.window.coppice.lastStatus = msg; },
    };
    mountRecord();
    element('text').value = 'line one\n\nline  two';
    element('record').dispatchEvent({ type: 'click' });
    await flush();
    element('record').dispatchEvent({ type: 'click' });
    await flush();
    await flush();
    gaps = 0;
    await global.window.coppice.lastStatus.actions[0].run();
    await flush();
    await flush();
    assert.ok(element('text').value.startsWith('line one\n\nline  two'), JSON.stringify(element('text').value));
    assert.equal((element('text').value.match(/sentence/g) || []).length, 1);
    assert.ok(!element('text').value.includes('first half'));
  } finally {
    clipStore.backend = null;
  }
});

// fakeLocks is a Web Locks stand-in: a held name is not available.
function fakeLocks() {
  const held = new Set();
  return {
    held,
    request(name, optionsOrFn, maybeFn) {
      const fn = typeof optionsOrFn === 'function' ? optionsOrFn : maybeFn;
      const opts = typeof optionsOrFn === 'function' ? {} : optionsOrFn;
      if (held.has(name)) return opts.ifAvailable ? Promise.resolve(fn(null)) : new Promise(() => {});
      if (opts.ifAvailable) return Promise.resolve(fn({ name }));
      held.add(name);
      return Promise.resolve(fn({ name })).finally(() => held.delete(name));
    },
  };
}

// N4: two tabs, one backend. A tab deletes only its own clip, and a tab does
// not offer the clip another live tab holds.
test('two tabs keep their own clips and one does not offer the other\'s', async () => {
  reset();
  installRecordingSupport();
  const store = fakeStore();
  clipStore.backend = store;
  clipLocks.backend = fakeLocks();
  try {
    let fail = true;
    global.window.coppice = {
      api: async (path) => {
        if (!path.startsWith('/api/voice/transcribe')) return { ready: true, state: 'ready' };
        if (fail) { const e = new Error('Dropped.'); e.status = 500; throw e; }
        return { text: 'words', raw_text: 'words' };
      },
      status: (msg) => { global.window.coppice.lastStatus = msg; },
    };
    // Tab A records a clip that fails to send; it stays stored and held.
    mountRecord();
    element('record').dispatchEvent({ type: 'click' });
    await flush();
    element('record').dispatchEvent({ type: 'click' });
    await flush();
    await flush();
    assert.equal(store.data.size, 1);
    const aKey = [...store.data.keys()][0];

    // Tab B loads while A holds the clip: B is not offered it.
    global.window.coppice.lastStatus = '';
    mountRecord();
    await flush();
    await flush();
    assert.equal(global.window.coppice.lastStatus, '', 'a clip a live tab holds must not be offered');
    assert.equal(store.data.has(aKey), true);
    assert.equal(aKey.startsWith('record:'), true);
  } finally {
    clipStore.backend = null;
    clipLocks.backend = null;
  }
});

test('a tab deletes only its own clip', async () => {
  reset();
  installRecordingSupport();
  const store = fakeStore();
  clipStore.backend = store;
  try {
    // Another tab's clip sits in the store under its own key.
    store.data.set('record:other', { blob: new Blob(['x']), type: 'audio/webm', seconds: 3 });
    global.window.coppice = {
      api: async (path) => (path.startsWith('/api/voice/transcribe') ? { text: 'mine', raw_text: 'mine' } : { ready: true, state: 'ready' }),
      status: (msg) => { global.window.coppice.lastStatus = msg; },
    };
    mountRecord();
    await flush();
    // This tab recovered the other clip (no live lock) and discards it.
    global.window.coppice.lastStatus.actions[2].run();
    await flush();
    assert.equal(store.data.size, 0);
    // Record a new clip of its own, send it, and the store is clean again.
    element('record').dispatchEvent({ type: 'click' });
    await flush();
    element('record').dispatchEvent({ type: 'click' });
    await flush();
    await flush();
    assert.equal(element('text').value, 'mine');
    assert.equal(store.data.size, 0);
  } finally {
    clipStore.backend = null;
  }
});

// N9: a tap before the stored-clip lookup answers must not be undone by it.
test('a clip recovered after recording began does not replace the new recording', async () => {
  reset();
  installRecordingSupport();
  const store = fakeStore();
  store.data.set('record:old', { blob: new Blob(['old']), type: 'audio/webm', seconds: 3 });
  let gate;
  const slowKeys = new Promise((r) => { gate = r; });
  const real = store.keys;
  store.keys = async () => { await slowKeys; return real(); };
  clipStore.backend = store;
  try {
    global.window.coppice = {
      api: async (path) => (path.startsWith('/api/voice/transcribe') ? { text: 'new words', raw_text: 'new words' } : { ready: true, state: 'ready' }),
      status: (msg) => { global.window.coppice.lastStatus = msg; },
    };
    mountRecord();
    element('record').dispatchEvent({ type: 'click' }); // start recording before the lookup answers
    await flush();
    gate();
    await flush();
    await flush();
    assert.doesNotMatch(String((global.window.coppice.lastStatus || {}).text || ''), /recording from before/);
    element('record').dispatchEvent({ type: 'click' });
    await flush();
    await flush();
    assert.equal(element('text').value, 'new words');
    assert.equal(store.data.has('record:old'), true, 'the old clip must still be stored');
  } finally {
    clipStore.backend = null;
  }
});

// ChunkRecorder hands out audio while it records, as a real MediaRecorder
// started with a timeslice does. emit is one chunk; stop sends the last
// chunk, then the stop event.
class ChunkRecorder extends FakeMediaRecorder {
  constructor(stream) { super(stream); ChunkRecorder.last = this; }
  start(slice) { this.slice = slice; }
  emit(text) { this._listeners.dataavailable({ data: new Blob([text]) }); }
  stop() {
    queueMicrotask(() => {
      this.emit('-end');
      this._listeners.stop();
    });
  }
}

function installChunkRecorder() {
  global.navigator.mediaDevices = { getUserMedia: async () => fakeStream() };
  global.MediaRecorder = ChunkRecorder;
}

const partialTimer = () => timers.filter((t) => !t.cancelled && t.ms === timing.partialMs).pop();
const firePartial = async () => {
  const t = partialTimer();
  assert.ok(t, 'no partial is scheduled');
  t.cancelled = true;
  t.fn();
  await flush();
  await flush();
};

// The owner sees the words grow under the button while they talk, and the
// final text lands in the box once they stop.
test('while recording, the clip so far goes to /api/voice/partial and its text shows under the button', async () => {
  reset();
  installChunkRecorder();
  const partials = [];
  const replies = [{ text: 'run the', seq: 1 }, { text: 'run the tests', seq: 2 }];
  global.window.coppice = {
    api: async (path, options) => {
      if (path.startsWith('/api/voice/partial')) { partials.push({ path, options }); return replies.shift(); }
      if (path.startsWith('/api/voice/transcribe')) return { text: 'run the tests please', raw_text: 'run the tests please' };
      return { ready: true, state: 'ready' };
    },
    status: (msg) => { global.window.coppice.lastStatus = msg; },
  };
  mountRecord();
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  assert.ok(ChunkRecorder.last.slice > 0, 'the recorder must hand out audio while it records');
  const line = element('record-partial');
  assert.equal(line.hidden, false);
  assert.equal(line.textContent, 'Listening…');

  ChunkRecorder.last.emit('aa');
  ChunkRecorder.last.emit('bb');
  await firePartial();
  assert.equal(partials.length, 1);
  assert.equal(partials[0].path, '/api/voice/partial?seq=1');
  assert.equal(partials[0].options.method, 'POST');
  assert.equal(partials[0].options.headers['Content-Type'], 'audio/webm;codecs=opus');
  assert.equal(partials[0].options.body.size, 4, 'the partial must carry every chunk so far');
  assert.equal(line.textContent, 'run the');

  ChunkRecorder.last.emit('cc');
  await firePartial();
  assert.equal(partials[1].path, '/api/voice/partial?seq=2');
  assert.equal(partials[1].options.body.size, 6);
  assert.equal(line.textContent, 'run the tests');
  assert.equal(element('text').value, '', 'a partial never touches the box');

  element('record').dispatchEvent({ type: 'click' });
  await flush();
  await flush();
  assert.equal(element('text').value, 'run the tests please');
  assert.equal(line.hidden, true);
  assert.equal(line.textContent, '');
  assert.equal(partialTimer(), undefined, 'no partial runs once the clip is sent');
});

test('no partial goes out before there is any audio', async () => {
  reset();
  installChunkRecorder();
  const partials = [];
  global.window.coppice = {
    api: async (path) => { if (path.startsWith('/api/voice/partial')) partials.push(path); return { ready: true }; },
    status: () => {},
  };
  mountRecord();
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  await firePartial();
  assert.equal(partials.length, 0);
  assert.ok(partialTimer(), 'the next partial must still be scheduled');
});

test('a partial reply older than the one shown is dropped', async () => {
  reset();
  installChunkRecorder();
  const replies = [{ text: 'newer words', seq: 5 }, { text: 'older', seq: 3 }];
  global.window.coppice = {
    api: async (path) => (path.startsWith('/api/voice/partial') ? replies.shift() : { ready: true }),
    status: () => {},
  };
  mountRecord();
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  ChunkRecorder.last.emit('aa');
  await firePartial();
  await firePartial();
  assert.equal(element('record-partial').textContent, 'newer words');
});

// The owner taps Stop while a partial is on its way. Its late reply must
// not paint over the final text or bring the line back.
test('a partial that comes back after Stop never repaints the line', async () => {
  reset();
  installChunkRecorder();
  let answer;
  global.window.coppice = {
    api: (path) => {
      if (path.startsWith('/api/voice/partial')) return new Promise((r) => { answer = r; });
      if (path.startsWith('/api/voice/transcribe')) return Promise.resolve({ text: 'final words', raw_text: 'final words' });
      return Promise.resolve({ ready: true });
    },
    status: () => {},
  };
  mountRecord();
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  ChunkRecorder.last.emit('aa');
  await firePartial();
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  await flush();
  assert.equal(element('text').value, 'final words');
  answer({ text: 'stale', seq: 1 });
  await flush();
  assert.equal(element('record-partial').hidden, true);
  assert.equal(element('record-partial').textContent, '');
  assert.equal(partialTimer(), undefined);
});

// An older voice server answers 404 to /partial. The page says so once,
// sends no more partials, and the clip still works as before.
test('an older voice server: the first partial 404 shows one line and recording goes on', async () => {
  reset();
  installChunkRecorder();
  let partials = 0;
  global.window.coppice = {
    api: async (path) => {
      if (path.startsWith('/api/voice/partial')) {
        partials += 1;
        const e = new Error('This server answers /health, /transcribe, and /deliver.');
        e.status = 404;
        throw e;
      }
      if (path.startsWith('/api/voice/transcribe')) return { text: 'as before', raw_text: 'as before' };
      return { ready: true };
    },
    status: (msg) => { global.window.coppice.lastStatus = msg; },
  };
  mountRecord();
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  ChunkRecorder.last.emit('aa');
  await firePartial();
  assert.equal(element('record-partial').textContent, LIVE_TEXT_OLD);
  assert.equal(LIVE_TEXT_OLD, 'Live text needs a newer voice server.');
  assert.equal(partialTimer(), undefined, 'no more partials after a 404');
  assert.equal(partials, 1);
  assert.equal(element('record').textContent, 'Stop', 'recording goes on');
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  await flush();
  assert.equal(element('text').value, 'as before');
  assert.equal(element('record-partial').hidden, true);
});

// A partial that fails for any other reason says nothing: the next one is
// on its way, and the status line is for the clip.
test('a partial that fails quietly keeps the line and tries again', async () => {
  reset();
  installChunkRecorder();
  global.window.coppice = {
    api: async (path) => {
      if (path.startsWith('/api/voice/partial')) { const e = new Error('busy'); e.status = 409; throw e; }
      return { ready: true };
    },
    status: (msg) => { global.window.coppice.lastStatus = msg; },
  };
  mountRecord();
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  global.window.coppice.lastStatus = undefined;
  ChunkRecorder.last.emit('aa');
  await firePartial();
  assert.equal(element('record-partial').textContent, 'Listening…');
  assert.equal(global.window.coppice.lastStatus, undefined);
  assert.ok(partialTimer(), 'the next partial must be scheduled');
});

test('the tell bar mic shows its words under its own button', async () => {
  reset();
  installChunkRecorder();
  global.window.coppice = {
    api: async (path) => (path.startsWith('/api/voice/partial') ? { text: 'hello steward', seq: 1 } : { ready: true }),
    status: () => {},
  };
  mountRecord('tell-mic', 'tell-text', 'tell bar', true);
  element('tell-mic').textContent = 'Mic';
  element('tell-mic').dispatchEvent({ type: 'click' });
  await flush();
  ChunkRecorder.last.emit('aa');
  await firePartial();
  assert.equal(element('tell-mic-partial').textContent, 'hello steward');
  assert.equal(element('record-partial').textContent, '');
});

test('tailText keeps the end of a long partial', () => {
  assert.equal(tailText('short', 20), 'short');
  const long = 'one two three four five six seven eight nine ten';
  const got = tailText(long, 20);
  assert.ok(got.startsWith('…'), got);
  assert.ok(got.endsWith('nine ten'), got);
  assert.ok(got.length <= 21, got);
  assert.ok(!got.slice(1).startsWith(' '), got);
});

// Every partial carries the whole recording so far, so the recorder asks
// for a speech bitrate, not the browser's music default. A browser that
// refuses the option still records.
test('the recorder asks for a speech bitrate and falls back when refused', async () => {
  reset();
  const made = [];
  class PickyRecorder extends ChunkRecorder {
    constructor(stream, options) {
      if (options && PickyRecorder.refuse) throw new Error('NotSupportedError');
      super(stream);
      made.push(options);
    }
  }
  global.navigator.mediaDevices = { getUserMedia: async () => fakeStream() };
  global.MediaRecorder = PickyRecorder;
  global.window.coppice = { api: async () => ({ ready: true }), status: () => {} };
  mountRecord();
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  assert.deepEqual(made[0], { audioBitsPerSecond: SPEECH_BITS });
  assert.ok(SPEECH_BITS >= 32000 && SPEECH_BITS <= 64000);

  reset();
  PickyRecorder.refuse = true;
  made.length = 0;
  global.window.coppice = { api: async () => ({ ready: true }), status: () => {} };
  mountRecord();
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  assert.equal(made.length, 1);
  assert.equal(made[0], undefined);
  assert.equal(element('record').textContent, 'Stop');
});

// A refused token must not be spent on partials: three failures ban the
// address, and the final clip would then meet "wait 60 seconds" instead of
// the sentence that names the fix. A refused partial stops the partials
// of that recording and hides the line, and the clip shows its own error.
test('a partial refused for its token stops the partials, and the clip shows its own error', async () => {
  for (const status of [401, 403, 429]) {
    reset();
    installChunkRecorder();
    let partials = 0;
    global.window.coppice = {
      api: async (path) => {
        if (path.startsWith('/api/voice/partial')) { partials += 1; const e = new Error('refused'); e.status = status; throw e; }
        if (path.startsWith('/api/voice/transcribe')) { const e = new Error('This request needs a bearer token. Run coppice web token on the box that runs this server.'); e.status = 401; throw e; }
        return { ready: true };
      },
      status: (msg) => { global.window.coppice.lastStatus = msg; },
    };
    mountRecord();
    element('record').dispatchEvent({ type: 'click' });
    await flush();
    ChunkRecorder.last.emit('aa');
    await firePartial();
    assert.equal(partials, 1);
    assert.equal(partialTimer(), undefined, status + ': no more partials');
    // A line that says Listening and never changes would look live.
    assert.equal(element('record-partial').hidden, true, status + ': the line must hide');
    assert.equal(element('record-partial').textContent, '');
    element('record').dispatchEvent({ type: 'click' });
    await flush();
    await flush();
    assert.match(global.window.coppice.lastStatus.text, /coppice web token/);
  }
});

// coppice-rs serves this same page but has no /api/voice/partial: its
// router answers 405 for a POST there. The page must not upload the
// recording every 1.5 s for nothing: it says so once and stops.
test('a web server with no partial route says so once and stops', async () => {
  reset();
  installChunkRecorder();
  let partials = 0;
  global.window.coppice = {
    api: async (path) => {
      if (path.startsWith('/api/voice/partial')) { partials += 1; const e = new Error('HTTP 405'); e.status = 405; throw e; }
      if (path.startsWith('/api/voice/transcribe')) return { text: 'still works', raw_text: 'still works' };
      return { ready: true };
    },
    status: () => {},
  };
  mountRecord();
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  ChunkRecorder.last.emit('aa');
  await firePartial();
  assert.equal(partials, 1);
  assert.equal(partialTimer(), undefined);
  assert.equal(element('record-partial').textContent, LIVE_TEXT_OLD_WEB);
  assert.equal(LIVE_TEXT_OLD_WEB, 'Live text needs a newer coppice.');
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  await flush();
  assert.equal(element('text').value, 'still works');
});

// Stop aborts a partial still uploading, so it does not share the uplink
// with the clip.
test('Stop aborts the partial still on its way', async () => {
  reset();
  installChunkRecorder();
  let signal = null;
  global.window.coppice = {
    api: (path, options) => {
      if (path.startsWith('/api/voice/partial')) { signal = options.signal; return new Promise(() => {}); }
      if (path.startsWith('/api/voice/transcribe')) return Promise.resolve({ text: 'done', raw_text: 'done' });
      return Promise.resolve({ ready: true });
    },
    status: () => {},
  };
  mountRecord();
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  ChunkRecorder.last.emit('aa');
  await firePartial();
  assert.ok(signal, 'the partial must carry an abort signal');
  assert.equal(signal.aborted, false);
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  await flush();
  assert.equal(signal.aborted, true);
  assert.equal(element('text').value, 'done');
});

// The status reply already says when the voice server has no live text.
// The page then shows that line from the start and sends no partials.
test('a status that knows live text is off sends no partials', async () => {
  reset();
  installChunkRecorder();
  let partials = 0;
  global.window.coppice = {
    api: async (path) => {
      if (path.startsWith('/api/voice/partial')) { partials += 1; return { text: 'x', seq: 1 }; }
      if (path.startsWith('/api/voice/status')) return { ready: true, state: 'ready', max_seconds: 1200, partial: false, partial_known: true };
      return { ready: true };
    },
    status: () => {},
  };
  mountRecord();
  await flush();
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  assert.equal(element('record-partial').textContent, LIVE_TEXT_OLD);
  assert.equal(partialTimer(), undefined);
  assert.equal(partials, 0);
});

// A status that could not reach the voice server knows nothing: the page
// still tries live text.
test('a status that does not know about live text still tries it', async () => {
  reset();
  installChunkRecorder();
  global.window.coppice = {
    api: async (path) => {
      if (path.startsWith('/api/voice/partial')) return { text: 'words', seq: 1 };
      if (path.startsWith('/api/voice/status')) return { ready: true, state: 'ready', max_seconds: 1200, partial: false };
      return { ready: true };
    },
    status: () => {},
  };
  mountRecord();
  await flush();
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  ChunkRecorder.last.emit('aa');
  await firePartial();
  assert.equal(element('record-partial').textContent, 'words');
});

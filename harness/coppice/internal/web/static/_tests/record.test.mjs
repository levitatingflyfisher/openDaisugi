import test from 'node:test';
import assert from 'node:assert/strict';
import { reset, element } from './browser-stub.mjs';
import { mountRecord, statusForReply, contentTypeFor, canRecord, applyReply } from '../record.js';

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

  assert.equal(calls.length, 1);
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
  assert.equal(
    global.window.coppice.lastStatus,
    'No speech was heard. Hold Record longer and speak closer to the microphone.',
  );
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
  assert.equal(calls.length, 0, 'a denied microphone still posted a clip');
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

// The failure this names: the 409 the server sends when no voice server is
// configured, and the 502 it sends when the voice server cannot be
// reached, both reach window.coppice.api as a rejected promise whose
// message is already the sentence the operator should read. Anything that
// swallowed that message, or replaced it with a generic one, would leave
// the operator with no next step.
test('a refused transcribe reply leaves the server\'s own sentence on the status line', async () => {
  reset();
  installRecordingSupport();
  global.window.coppice = {
    api: async () => { throw new Error('Voice is not set up. Run daisugi voice serve, then start coppice web serve again with --voice-url.'); },
    status: (msg) => { global.window.coppice.lastStatus = msg; },
  };

  mountRecord();
  element('record').dispatchEvent({ type: 'click' });
  await flush();
  element('record').dispatchEvent({ type: 'click' });
  await flush();

  assert.equal(
    global.window.coppice.lastStatus,
    'Voice is not set up. Run daisugi voice serve, then start coppice web serve again with --voice-url.',
  );
  assert.equal(element('record').textContent, 'Record', 'the button was stuck off Record after a refusal');
  assert.equal(element('text').value, '', 'a refused reply still wrote into the prompt box');
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

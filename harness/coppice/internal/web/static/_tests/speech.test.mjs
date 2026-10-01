import test from 'node:test';
import assert from 'node:assert/strict';
import { splitSentences, parseFrame, createSpeech, READ_ALOUD_KEY } from '../speech.js';

const flush = async () => { for (let i = 0; i < 12; i++) await new Promise((r) => setImmediate(r)); };

function memory() {
  const m = new Map();
  return { getItem: (k) => (m.has(k) ? m.get(k) : null), setItem: (k, v) => m.set(k, String(v)), m };
}

// A fake browser voice: speak resolves when the test calls finish().
function fakeSynth() {
  const s = { spoken: [], cancels: 0, unlocks: 0, waiting: [], auto: true };
  s.api = {
    speak: (text) => {
      s.spoken.push(text);
      if (s.auto) return Promise.resolve();
      return new Promise((r) => s.waiting.push(r));
    },
    cancel: () => { s.cancels += 1; s.waiting.splice(0).forEach((r) => r()); },
    unlock: () => { s.unlocks += 1; },
  };
  return s;
}

// A fake WebAudio context: a source ends when the test calls end(), or at
// once when auto is set.
function fakeAudio(auto = true) {
  const a = { played: [], stops: 0, decodeFails: false, sources: [] };
  a.ctx = {
    state: 'running',
    destination: {},
    decodeAudioData: async (buf) => {
      if (a.decodeFails) throw new Error('bad audio');
      return { bytes: new Uint8Array(buf) };
    },
    createBuffer: () => ({}),
    createBufferSource: () => {
      const src = {
        connect() {},
        start() {
          if (!(src.buffer && src.buffer.bytes)) return; // the silent unlock blip
          a.played.push(String.fromCharCode(...src.buffer.bytes));
          if (auto) queueMicrotask(() => src.onended && src.onended());
        },
        stop() { if (!(src.buffer && src.buffer.bytes)) return; a.stops += 1; if (src.onended) src.onended(); },
      };
      a.sources.push(src);
      return src;
    },
    resume: async () => {},
  };
  return a;
}

function bytes(s) { return { audio: new TextEncoder().encode(s).buffer, type: 'audio/mpeg' }; }

function make(over = {}) {
  const synth = over.synth || fakeSynth();
  const audio = over.audio || fakeAudio();
  const lines = [];
  const chat = { msgs: [] };
  const speech = createSpeech({
    say: over.say || (async (text) => bytes(text)),
    info: over.info || (async () => ({ mode: 'server', url: 'http://gpu:8880/v1/audio/speech', model: 'kokoro', voice: 'af_heart' })),
    chat: async () => chat.msgs.slice(),
    audio: () => audio.ctx,
    synth: () => synth.api,
    status: (l) => lines.push(l),
    storage: over.storage || memory(),
    connected: () => true,
  });
  return { speech, synth, audio, lines, chat };
}

const agent = (id, text, extra = {}) => ({ id, role: 'agent', text, ...extra });
const owner = (id, text) => ({ id, role: 'owner', text });

test('splitSentences cuts at sentence ends and lines, joins short pieces, caps long ones', () => {
  assert.deepEqual(splitSentences('Hello there. How are you? Fine!\nNew line here'), ['Hello there.', 'How are you?', 'Fine! New line here']);
  assert.deepEqual(splitSentences('Dr. Smith went home. Then he slept.'), ['Dr. Smith went home.', 'Then he slept.']);
  assert.deepEqual(splitSentences('   \n\n '), []);
  const long = splitSentences('word '.repeat(200), 100);
  assert.ok(long.length > 5 && long.every((p) => p.length <= 100), JSON.stringify(long.map((p) => p.length)));
  assert.equal(splitSentences('a\u0007b control').join(' '), 'a b control');
});

test('parseFrame reads the header and the audio, and refuses anything else', () => {
  const head = new TextEncoder().encode('{"event":"speech","id":"w3","type":"audio/mpeg"}');
  const audio = new TextEncoder().encode('ID3');
  const buf = new Uint8Array(4 + head.length + audio.length);
  new DataView(buf.buffer).setUint32(0, head.length);
  buf.set(head, 4);
  buf.set(audio, 4 + head.length);
  const f = parseFrame(buf.buffer);
  assert.equal(f.id, 'w3');
  assert.equal(f.type, 'audio/mpeg');
  assert.equal(new TextDecoder().decode(f.audio), 'ID3');
  assert.equal(parseFrame(new ArrayBuffer(3)), null);
  assert.equal(parseFrame('text'), null);
  const bad = new Uint8Array(buf);
  bad[4] = 0x7b + 1;
  assert.equal(parseFrame(bad.buffer), null);
  const wrong = new TextEncoder().encode('{"event":"x","id":"a"}');
  const b2 = new Uint8Array(4 + wrong.length + 1);
  new DataView(b2.buffer).setUint32(0, wrong.length);
  b2.set(wrong, 4);
  assert.equal(parseFrame(b2.buffer), null);
});

test('a reply to a message that came by voice is spoken; the replies already there are not', async () => {
  const { speech, synth, chat } = make({ info: async () => ({ mode: 'browser' }) });
  chat.msgs = [owner(1, 'hi'), agent(2, 'An old reply here.')];
  await speech.refresh();
  await speech.prime();
  speech.markVoiced();
  await speech.beforeSend('run the tests');
  chat.msgs.push(owner(3, 'run the tests'), agent(4, 'Running the tests now. It takes a minute.'));
  await speech.onEvent({ event: 'messages', chat: true });
  await flush();
  assert.deepEqual(synth.spoken, ['Running the tests now.', 'It takes a minute.']);
});

test('a reply to a typed message is never spoken, unless Read replies aloud is on', async () => {
  const store = memory();
  const { speech, synth, chat } = make({ info: async () => ({ mode: 'browser' }), storage: store });
  await speech.prime();
  await speech.beforeSend();
  chat.msgs.push(agent(1, 'A typed answer arrives.'));
  await speech.onEvent({ event: 'messages', chat: true });
  await flush();
  assert.deepEqual(synth.spoken, []);
  speech.setReadAloud(true);
  assert.equal(store.getItem(READ_ALOUD_KEY), '1');
  chat.msgs.push(agent(2, 'Another typed answer arrives.'));
  await speech.onEvent({ event: 'messages', chat: true });
  await flush();
  assert.deepEqual(synth.spoken, ['Another typed answer arrives.']);
});

test('a send that fails leaves the mic mark as it was', async () => {
  const { speech, synth, chat } = make({ info: async () => ({ mode: 'browser' }) });
  await speech.prime();
  speech.markVoiced();
  const tok = await speech.beforeSend('check it');
  speech.undoSend(tok);
  await speech.beforeSend('check it');
  chat.msgs.push(owner(1, 'check it'), agent(2, 'The words still count as spoken.'));
  await speech.onEvent({ event: 'messages', chat: true });
  await flush();
  assert.equal(synth.spoken.length, 1);
});

test('tool lines and owner lines are not read aloud', async () => {
  const { speech, synth, chat } = make({ info: async () => ({ mode: 'browser' }) });
  speech.setReadAloud(true);
  await speech.prime();
  chat.msgs.push(owner(1, 'say something'), agent(2, 'bash ls', { tool: 'bash' }), { id: 3, role: 'note', text: 'A note.' });
  await speech.onEvent({ event: 'messages', chat: true });
  await flush();
  assert.deepEqual(synth.spoken, []);
});

test('the first mic press unlocks sound once', () => {
  const { speech, synth } = make();
  speech.pressMic();
  speech.pressMic();
  assert.equal(synth.unlocks, 1);
});

test('the browser voice says once that a locked screen stops it', async () => {
  const { speech, lines } = make({ info: async () => ({ mode: 'browser' }) });
  speech.speak('First reply is here.');
  await flush();
  speech.speak('Second reply is here.');
  await flush();
  assert.equal(lines.filter((l) => /screen locks/.test(l)).length, 1);
});

test('a speech server plays the sentences in order, asking for the next while one plays', async () => {
  const audio = fakeAudio(false);
  const order = [];
  const { speech } = make({
    audio,
    say: async (text) => { order.push('ask ' + text); return bytes(text); },
  });
  await speech.refresh();
  speech.speak('One is first. Two is second. Three is third.');
  await flush();
  // The first plays; the second is already asked for.
  assert.deepEqual(order, ['ask One is first.', 'ask Two is second.']);
  assert.deepEqual(audio.played, ['One is first.']);
  audio.sources[audio.sources.length - 1].onended();
  await flush();
  audio.sources[audio.sources.length - 1].onended();
  await flush();
  audio.sources[audio.sources.length - 1].onended();
  await flush();
  assert.deepEqual(audio.played.slice(-3), ['One is first.', 'Two is second.', 'Three is third.']);
  assert.deepEqual(order.slice(-1), ['ask Three is third.']);
});

test('a mic press stops speech at once, in the browser voice and in the server voice', async () => {
  const synth = fakeSynth();
  synth.auto = false;
  const b = make({ synth, info: async () => ({ mode: 'browser' }) });
  b.speech.speak('A long reply is here. It has two sentences.');
  await flush();
  assert.equal(synth.spoken.length, 1);
  assert.equal(b.speech.speaking(), true);
  b.speech.pressMic();
  await flush();
  assert.equal(synth.cancels >= 1, true);
  assert.equal(synth.spoken.length, 1, 'the second sentence is never spoken');
  assert.equal(b.speech.speaking(), false);

  const audio = fakeAudio(false);
  const s = make({ audio });
  await s.speech.refresh();
  s.speech.speak('Server voice sentence one. Sentence two.');
  await flush();
  assert.equal(audio.played.length, 1);
  s.speech.pressMic();
  await flush();
  assert.equal(audio.stops, 1);
  assert.equal(audio.played.length, 1);
  assert.equal(s.speech.speaking(), false);
});

test('when the speech server fails, the rest of the reply is spoken by the browser with one line that says why', async () => {
  let n = 0;
  const { speech, synth, lines, audio } = make({
    say: async (text) => {
      n += 1;
      if (n === 2) throw new Error('The speech server at gpu:8880 did not answer. Check that it runs and that speak_url in coppice.toml names it.');
      return bytes(text);
    },
  });
  await speech.refresh();
  speech.speak('Sentence one is ok. Sentence two fails. Sentence three too.');
  await flush();
  assert.deepEqual(audio.played, ['Sentence one is ok.']);
  assert.deepEqual(synth.spoken, ['Sentence two fails.', 'Sentence three too.']);
  const why = lines.filter((l) => /did not answer/.test(l));
  assert.equal(why.length, 1);
  assert.match(why[0], /speak_url/);
  assert.match(why[0], /browser's voice/);
  // One line, and not the lock note on top of it.
  assert.equal(lines.length, 1, JSON.stringify(lines));
});

test('audio the browser cannot decode falls back the same way', async () => {
  const audio = fakeAudio();
  audio.decodeFails = true;
  const { speech, synth, lines } = make({ audio });
  await speech.refresh();
  speech.speak('Only sentence is here.');
  await flush();
  assert.deepEqual(synth.spoken, ['Only sentence is here.']);
  assert.equal(lines.length, 1, JSON.stringify(lines));
  assert.match(lines[0], /could not play/);
  assert.match(lines[0], /speak_url/, 'the line names the fix');
});

test('describe says where replies are spoken and never shows a key', async () => {
  const { speech } = make({ info: async () => ({ mode: 'server', url: 'http://gpu:8880/v1/audio/speech', model: 'kokoro', voice: 'af_heart', profile: 'tts' }) });
  await speech.refresh();
  assert.match(speech.describe(), /Speech server http:\/\/gpu:8880\/v1\/audio\/speech\. Model kokoro, voice af_heart\./);
  assert.match(speech.describe(), /profile tts/);
  assert.match(speech.describe(), /Each reply's text is sent to http:\/\/gpu:8880\/v1\/audio\/speech\./);
  const b = make({ info: async () => ({ mode: 'browser', line: '[voice] speak_url is not an address.' }) });
  await b.speech.refresh();
  assert.match(b.speech.describe(), /^\[voice\] speak_url is not an address\. Replies are spoken with this browser/);
  assert.doesNotMatch(b.speech.describe(), /is sent to/);
});

test('the first tap on a page whose sound is still suspended does not use up the unlock', async () => {
  const audio = fakeAudio();
  let allowed = false;
  let resumes = 0;
  audio.ctx.state = 'suspended';
  audio.ctx.resume = async () => {
    resumes += 1;
    if (allowed) audio.ctx.state = 'running';
  };
  const { speech, synth } = make({ audio });
  speech.pressMic();
  await flush();
  speech.pressMic();
  await flush();
  assert.equal(resumes, 2, 'the second tap tries again');
  assert.equal(speech.unlocked(), false);
  allowed = true;
  speech.pressMic();
  await flush();
  assert.equal(speech.unlocked(), true);
  speech.pressMic();
  await flush();
  assert.equal(resumes, 3, 'once sound runs, no more tries');
  assert.equal(synth.unlocks, 1, 'the silent utterance is said once');
});

// The mark is armed on the owner row the send produced: the first new owner
// row with the same words (the send reply carries no id). Agent rows after
// it are spoken until the next owner row.
test('the voiced mark waits for our own owner row: another page\'s line before it, and its reply, stay silent', async () => {
  const { speech, synth, chat } = make({ info: async () => ({ mode: 'browser' }) });
  await speech.prime();
  speech.markVoiced();
  await speech.beforeSend('run the tests');
  chat.msgs.push(owner(5, 'a line from another page'), agent(6, 'The reply to the other page.'),
    owner(7, 'run  the\ttests'), agent(8, 'The reply to ours.'));
  await speech.onEvent({ event: 'messages', chat: true });
  await flush();
  assert.deepEqual(synth.spoken, ['The reply to ours.']);
});

test('a read that finishes late still finds our row and speaks the reply that follows', async () => {
  let release;
  const gate = new Promise((r) => { release = r; });
  const { speech, synth, chat } = make({ info: async () => ({ mode: 'browser' }) });
  await speech.prime();
  speech.markVoiced();
  await speech.beforeSend('run the tests');
  // The first read sees only our row; the reply comes in a later read.
  chat.msgs.push(owner(1, 'run the tests'));
  const first = speech.onEvent({ event: 'messages', chat: true });
  await flush();
  assert.deepEqual(synth.spoken, []);
  chat.msgs.push(agent(2, 'The reply came late.'));
  release();
  await gate;
  await first;
  await speech.onEvent({ event: 'messages', chat: true });
  await flush();
  assert.deepEqual(synth.spoken, ['The reply came late.']);
});

test('a typed line after the voiced one ends the mark, and its reply stays silent', async () => {
  const { speech, synth, chat } = make({ info: async () => ({ mode: 'browser' }) });
  await speech.prime();
  speech.markVoiced();
  await speech.beforeSend('run the tests');
  chat.msgs.push(owner(1, 'run the tests'), agent(2, 'First reply is spoken.'));
  await speech.onEvent({ event: 'messages', chat: true });
  await flush();
  assert.deepEqual(synth.spoken, ['First reply is spoken.']);
  await speech.beforeSend('a typed line');
  chat.msgs.push(owner(3, 'a typed line'), agent(4, 'An answer to the typed line.'));
  await speech.onEvent({ event: 'messages', chat: true });
  await flush();
  assert.deepEqual(synth.spoken, ['First reply is spoken.']);
});

test('a send right after the mic waits for the read the mic started, so our row is never taken for an old one', async () => {
  let release;
  const gate = new Promise((r) => { release = r; });
  const msgs = [];
  const spoken = [];
  const speech = createSpeech({
    say: async () => { throw new Error('no server'); },
    info: async () => ({ mode: 'browser' }),
    // A read that answers late, with the chat as it is when it answers.
    chat: async () => { await gate; return msgs.slice(); },
    audio: () => null,
    synth: () => ({ speak: async (t) => { spoken.push(t); }, cancel() {}, unlock() {} }),
    status: () => {},
    storage: memory(),
  });
  speech.markVoiced();
  const sending = speech.beforeSend('run the tests');
  assert.ok(sending && typeof sending.then === 'function', 'the send does not wait');
  let done = false;
  sending.then(() => { done = true; });
  await flush();
  assert.equal(done, false, 'the send went before the read ended');
  release();
  await sending;
  // Our row exists only after the send.
  msgs.push(owner(1, 'run the tests'), agent(2, 'The reply is spoken.'));
  await speech.onEvent({ event: 'messages', chat: true });
  await flush();
  assert.deepEqual(spoken, ['The reply is spoken.']);
});

test('a send with no read out returns at once', () => {
  const { speech } = make({ info: async () => ({ mode: 'browser' }) });
  const tok = speech.beforeSend('typed');
  assert.equal(typeof tok.then, 'undefined');
});

test('an owner row from elsewhere after ours ends the mark too', async () => {
  const { speech, synth, chat } = make({ info: async () => ({ mode: 'browser' }) });
  await speech.prime();
  speech.markVoiced();
  await speech.beforeSend('run the tests');
  chat.msgs.push(owner(1, 'run the tests'), agent(2, 'Ours is spoken.'), owner(3, 'someone else'), agent(4, 'Theirs is not.'));
  await speech.onEvent({ event: 'messages', chat: true });
  await flush();
  assert.deepEqual(synth.spoken, ['Ours is spoken.']);
});

// The foreman answers aloud. One rule, on the desk and on the phone: a
// reply is spoken when the message it answers came by voice (the mic filled
// the tell bar), and never otherwise, unless Settings has "Read replies
// aloud" on. A tap on a reply's Listen button speaks that reply. Speech
// stops at the next mic press or a tap on Stop.
//
// With no speech server the page speaks with the browser's own voice,
// sentence by sentence. With [voice] speak_url set, each sentence is asked
// of that server through the coppice server (speech.say), which answers
// the audio as a binary frame on the socket. The page plays the frames in
// order with WebAudio. If the server fails, the rest of that reply is
// spoken with the browser's voice, and one line says why and what to fix.
//
// The page keeps no reply on the phone: the only thing stored is the
// "Read replies aloud" choice.

import { cleanTalk } from './chat.js';

export const READ_ALOUD_KEY = 'coppice.readAloud';
const NOTED_KEY = 'coppice.speechNoted';

// SENTENCE_MAX is the longest piece asked of a speech server or an
// utterance; a longer sentence is cut at its last space.
export const SENTENCE_MAX = 280;

// splitSentences cuts a reply into the pieces that are spoken one by one.
// Line breaks end a piece, and so do . ! ? and the ellipsis when a space
// follows. Very short pieces join the next one, so "Dr." never plays alone.
export function splitSentences(text, max = SENTENCE_MAX) {
  const flat = String(text || '').replace(/[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f]/g, ' ');
  const raw = [];
  for (const line of flat.split(/\n+/)) {
    const clean = line.replace(/\s+/g, ' ').trim();
    if (!clean) continue;
    raw.push(...clean.split(/(?<=[.!?…])\s+/));
  }
  const joined = [];
  let carry = '';
  for (const piece of raw) {
    const p = carry ? carry + ' ' + piece : piece;
    if (p.length < 12) {
      carry = p;
      continue;
    }
    carry = '';
    joined.push(p);
  }
  if (carry) joined.push(carry);
  const out = [];
  for (let p of joined) {
    while (p.length > max) {
      let cut = p.lastIndexOf(' ', max);
      if (cut < max / 2) cut = max;
      out.push(p.slice(0, cut).trim());
      p = p.slice(cut).trim();
    }
    if (p) out.push(p);
  }
  return out;
}

// parseFrame reads a binary socket frame: 4 bytes big-endian N, N bytes of
// JSON header, then the audio. It returns {id, type, audio}, or null for
// anything else.
export function parseFrame(buf) {
  if (!(buf instanceof ArrayBuffer) || buf.byteLength < 5) return null;
  const n = new DataView(buf).getUint32(0);
  if (n === 0 || 4 + n >= buf.byteLength) return null;
  let head;
  try {
    head = JSON.parse(new TextDecoder().decode(new Uint8Array(buf, 4, n)));
  } catch {
    return null;
  }
  if (!head || head.event !== 'speech' || typeof head.id !== 'string') return null;
  return { id: head.id, type: String(head.type || ''), audio: buf.slice(4 + n) };
}

// createSpeech holds the rules above. deps: say(text) asks the server for
// one sentence and resolves {audio, type}; info() resolves speech.status;
// chat(limit) resolves the foreman chat's messages; audio() is a WebAudio
// context or null; synth() is {speak(text) -> Promise, cancel(), unlock()}
// or null; status(line) shows a line; storage is localStorage or a fake.
export function createSpeech(deps) {
  const st = {
    mode: 'browser', url: '', model: '', voice: '', profile: '', line: '',
    gen: 0, loop: -1, queue: [], source: null, unlocked: false, synthUnlocked: false, noted: false,
    armed: false, voiced: false, want: '', anchored: false, seen: null, reading: false, again: false,
  };
  const listeners = new Set();
  const emit = () => { for (const l of listeners) l(api.speaking()); };
  const store = {
    get(k) { try { return deps.storage ? deps.storage.getItem(k) : null; } catch { return null; } },
    set(k, v) { try { if (deps.storage) deps.storage.setItem(k, v); } catch { /* the choice just does not stick */ } },
  };
  const readAloud = () => store.get(READ_ALOUD_KEY) === '1';
  const say = (line) => { if (deps.status) deps.status(line); };

  // noteLock says once that the browser's own voice stops with the screen.
  function noteLock() {
    if (st.noted || store.get(NOTED_KEY)) return;
    st.noted = true;
    store.set(NOTED_KEY, '1');
    say("Speaking with this browser's voice. It stops if the screen locks.");
  }

  function fail(e) {
    const why = (e && e.message) || 'The speech server did not answer.';
    say(why + " This reply is spoken with the browser's voice.");
  }

  async function play(gen, got) {
    const ctx = deps.audio();
    if (!ctx) throw new Error("This browser cannot play the speech server's audio. Clear speak_url in coppice.toml to use the browser's voice.");
    if (ctx.state === 'suspended' && ctx.resume) await ctx.resume();
    let buf;
    try {
      buf = await ctx.decodeAudioData(got.audio.slice(0));
    } catch {
      throw new Error("This browser could not play the speech server's audio. Check that it sends mp3 or wav, or clear speak_url in coppice.toml.");
    }
    if (gen !== st.gen) return;
    const src = ctx.createBufferSource();
    src.buffer = buf;
    src.connect(ctx.destination);
    st.source = src;
    await new Promise((resolve) => {
      src.onended = resolve;
      src.start(0);
    });
    if (st.source === src) st.source = null;
  }

  function fetchOne(text) {
    const p = deps.say(text);
    p.catch(() => {});
    return p;
  }

  // viaServer plays the parts in order, asking for the next while one
  // plays. It returns null when done or stopped, or the parts left when the
  // server failed.
  async function viaServer(gen, parts) {
    let next = fetchOne(parts[0]);
    for (let i = 0; i < parts.length; i++) {
      let got;
      try {
        got = await next;
      } catch (e) {
        if (gen !== st.gen) return null;
        fail(e);
        return parts.slice(i);
      }
      if (gen !== st.gen) return null;
      next = i + 1 < parts.length ? fetchOne(parts[i + 1]) : null;
      try {
        await play(gen, got);
      } catch (e) {
        if (gen !== st.gen) return null;
        fail(e);
        return parts.slice(i);
      }
    }
    return null;
  }

  // fellBack is true when the speech server failed: its line already says
  // why and what to fix, so the lock note stays out of it.
  async function viaBrowser(gen, parts, fellBack) {
    const synth = deps.synth();
    if (!synth) {
      say('This browser cannot speak aloud.');
      return;
    }
    if (!fellBack) noteLock();
    for (const part of parts) {
      if (gen !== st.gen) return;
      await synth.speak(part);
    }
  }

  async function speakText(gen, text) {
    let parts = splitSentences(text);
    if (parts.length === 0) return;
    let fellBack = false;
    if (st.mode === 'server' && deps.say && (!deps.connected || deps.connected())) {
      parts = await viaServer(gen, parts);
      if (parts === null) return;
      fellBack = true;
    }
    await viaBrowser(gen, parts, fellBack);
  }

  function start() {
    const gen = st.gen;
    if (st.loop === gen) return;
    st.loop = gen;
    emit();
    (async () => {
      while (gen === st.gen && st.queue.length > 0) {
        try {
          await speakText(gen, st.queue.shift());
        } catch (e) {
          say((e && e.message) || 'Speech stopped.');
        }
      }
      if (st.loop === gen) {
        st.loop = -1;
        emit();
      }
    })();
  }

  function enqueue(text) {
    st.queue.push(String(text));
    start();
  }

  function stop() {
    st.gen += 1;
    st.queue = [];
    if (st.loop !== -1) st.loop = -1;
    const synth = deps.synth();
    if (synth) synth.cancel();
    if (st.source) {
      const s = st.source;
      st.source = null;
      try { s.stop(); } catch { /* it had ended already */ }
    }
    emit();
  }

  // unlock runs on the first mic press or tap, where the browser lets a
  // page make sound: a silent utterance, and a resumed audio context.
  // It latches only once the audio context runs: a first tap that the
  // browser does not count as a gesture (a pointer down, say) tries again
  // on the next.
  function unlock() {
    if (st.unlocked) return;
    const synth = deps.synth();
    if (synth && synth.unlock && !st.synthUnlocked) {
      st.synthUnlocked = true;
      synth.unlock();
    }
    const ctx = deps.audio();
    if (!ctx) {
      st.unlocked = true;
      return;
    }
    const check = () => { if (ctx.state === 'running') st.unlocked = true; };
    if (ctx.resume) {
      Promise.resolve(ctx.resume()).then(check, () => {});
    } else {
      check();
    }
    try {
      const src = ctx.createBufferSource();
      src.buffer = ctx.createBuffer(1, 1, 22050);
      src.connect(ctx.destination);
      src.start(0);
    } catch { /* a context that cannot play the blip is still resumed */ }
  }

  function handle(msgs) {
    const first = st.seen === null;
    if (first) st.seen = new Set();
    for (const m of Array.isArray(msgs) ? msgs : []) {
      if (!m || typeof m !== 'object') continue;
      const id = String(m.id);
      if (st.seen.has(id)) continue;
      st.seen.add(id);
      if (first) continue;
      // The mark is armed on the owner row our send produced: the first new
      // owner row with our words. An owner row before it is another page's
      // and changes nothing. Agent rows after ours are spoken until the next
      // owner row, whoever sent it, ends the mark.
      if (m.role === 'owner' && st.armed) {
        if (st.anchored) {
          st.armed = false;
          st.anchored = false;
        } else if (cleanTalk(m.text) === st.want) {
          st.anchored = true;
        }
      }
      if (m.role === 'agent' && !m.tool && m.text && ((st.armed && st.anchored) || readAloud())) enqueue(String(m.text));
    }
  }

  // readNew reads the chat and speaks the replies that are new. The first
  // read only learns what is there.
  function readNew() {
    if (!deps.chat) return Promise.resolve();
    if (st.reading) {
      st.again = true;
      return st.readP;
    }
    st.reading = true;
    st.readP = (async () => {
      try {
        do {
          st.again = false;
          let msgs;
          try {
            msgs = await deps.chat(30);
          } catch {
            return;
          }
          handle(msgs);
        } while (st.again);
      } finally {
        st.reading = false;
      }
    })();
    return st.readP;
  }

  const api = {
    // refresh asks the server how replies are spoken.
    async refresh() {
      try {
        const r = await deps.info();
        st.mode = r && r.mode === 'server' ? 'server' : 'browser';
        st.url = (r && r.url) || '';
        st.model = (r && r.model) || '';
        st.voice = (r && r.voice) || '';
        st.profile = (r && r.profile) || '';
        st.line = (r && r.line) || '';
      } catch {
        st.mode = 'browser';
        st.line = '';
      }
      emit();
    },
    // prime learns the replies that are already there, so they are not
    // spoken when the page opens or when the owner first sends.
    prime: () => readNew(),
    onEvent(msg) {
      // The chat is read only while a reply may be spoken.
      if (msg && msg.event === 'messages' && msg.chat === true && (st.armed || readAloud())) return readNew();
      return undefined;
    },
    // markVoiced runs when the mic puts words in the tell bar. It learns the
    // chat as it is now, long before the send, so that our own owner row,
    // when it comes, is newer than what was learned.
    markVoiced() {
      st.voiced = true;
      st.seen = null;
      readNew();
    },
    // beforeSend runs when the owner sends a line to the foreman. The
    // replies to it are spoken when the mic filled the line. It returns
    // what undoSend needs if the send fails.
    //
    // When the read that markVoiced started is still out, the send waits for
    // it (two seconds at most), so our own owner row can never be taken for
    // an old one. Then it returns a promise; any other send returns at once.
    beforeSend(text) {
      const tok = { armed: st.armed, voiced: st.voiced, want: st.want, anchored: st.anchored };
      const waits = st.voiced && st.reading;
      st.armed = st.voiced;
      st.voiced = false;
      st.want = cleanTalk(text);
      st.anchored = false;
      if (!waits) return tok;
      return Promise.race([st.readP, new Promise((r) => setTimeout(r, 2000))]).then(() => tok);
    },
    undoSend(tok) {
      if (!tok) return;
      st.armed = tok.armed;
      st.voiced = tok.voiced;
      st.want = tok.want;
      st.anchored = tok.anchored;
    },
    pressMic() {
      unlock();
      stop();
    },
    unlock,
    unlocked: () => st.unlocked,
    stop,
    speak(text) {
      unlock();
      stop();
      enqueue(text);
    },
    speaking: () => st.loop !== -1,
    readAloud,
    setReadAloud(on) {
      store.set(READ_ALOUD_KEY, on ? '1' : '0');
      if (on) {
        st.seen = null;
        readNew();
      }
    },
    mode: () => st.mode,
    // debug is the state a failed check reads.
    debug: () => ({ armed: st.armed, anchored: st.anchored, want: st.want, voiced: st.voiced, seen: st.seen ? st.seen.size : null, queue: st.queue.length, reading: st.reading, loop: st.loop }),
    describe() {
      if (st.mode === 'server') {
        let s = 'Speech server ' + st.url + '. Model ' + st.model + ', voice ' + st.voice + '.';
        s += " Each reply's text is sent to " + st.url + '.';
        if (st.profile) s += ' Its key comes from the profile ' + st.profile + '.';
        return s;
      }
      const base = "Replies are spoken with this browser's own voice. Set [voice] speak_url in coppice.toml for a better one.";
      return st.line ? st.line + ' ' + base : base;
    },
    on(fn) {
      listeners.add(fn);
      return () => listeners.delete(fn);
    },
  };
  return api;
}

// mountSpeech builds the page's speech from the browser's own parts and
// wires the Stop button. rpc is the page's request function.
export function mountSpeech({ rpc, status, storage, connected }) {
  const w = typeof window !== 'undefined' ? window : {};
  let ctx = null;
  const speech = createSpeech({
    say: (text) => rpc('speech.say', { text }),
    info: () => rpc('speech.status'),
    chat: async (limit) => {
      const r = await rpc('floor.chat', { limit });
      return Array.isArray(r && r.messages) ? r.messages : [];
    },
    audio: () => {
      if (ctx) return ctx;
      const C = w.AudioContext || w.webkitAudioContext;
      if (!C) return null;
      try { ctx = new C(); } catch { ctx = null; }
      return ctx;
    },
    synth: () => {
      const ss = w.speechSynthesis;
      if (!ss || typeof w.SpeechSynthesisUtterance !== 'function') return null;
      return {
        speak: (text) => new Promise((resolve) => {
          const u = new w.SpeechSynthesisUtterance(text);
          u.onend = resolve;
          u.onerror = resolve;
          ss.speak(u);
        }),
        cancel: () => ss.cancel(),
        unlock: () => {
          const u = new w.SpeechSynthesisUtterance(' ');
          u.volume = 0;
          ss.speak(u);
        },
      };
    },
    status,
    storage,
    connected,
  });
  const stopBtn = typeof document !== 'undefined' ? document.getElementById('speech-stop') : null;
  if (stopBtn) {
    stopBtn.addEventListener('click', () => speech.stop());
    speech.on((on) => { stopBtn.hidden = !on; });
  }
  // A tap anywhere unlocks sound, as a mic press does. A browser counts a
  // release or a click as the gesture, not a press, so those are listened
  // for, until the unlock holds.
  if (typeof document !== 'undefined' && document.addEventListener) {
    const kinds = ['pointerup', 'click', 'touchend', 'keydown'];
    const tap = () => {
      speech.unlock();
      if (speech.unlocked() && typeof document.removeEventListener === 'function') {
        for (const k of kinds) document.removeEventListener(k, tap, true);
      }
    };
    for (const k of kinds) document.addEventListener(k, tap, true);
  }
  return speech;
}

import { MESSAGES, clipAction } from './messages.js';

// The phone's own record buttons. One tap starts a clip with
// MediaRecorder, a second tap stops it and posts it through
// window.coppice.api to /api/voice/transcribe, which the coppice web server
// forwards to the voice server. The text that comes back lands in a text
// box for the operator to read before anything is sent.

// statusForReply decides what the status line says once a clip comes back
// as text. The text always lands in the prompt box; the status line only
// speaks up when cleanup changed what the engine actually heard, so the
// operator can catch a cleanup mistake without reading two boxes on every
// single clip.
export function statusForReply(body) {
  const text = body.text || '';
  const raw = body.raw_text || '';
  if (raw && raw !== text) return 'The engine heard: ' + raw;
  return '';
}

// contentTypeFor is the Content-Type the clip is posted with. A real
// MediaRecorder always reports its own mimeType once it exists; the
// fallback only matters for a recorder that never got the chance to.
export function contentTypeFor(recorder) {
  return (recorder && recorder.mimeType) || 'audio/webm';
}

// canRecord is true only when this browser can both ask for the microphone
// and record from it. A tap on a browser missing either gets one sentence
// instead of a crash from calling a function that does not exist here.
export function canRecord() {
  return !!(
    typeof navigator !== 'undefined'
    && navigator.mediaDevices
    && navigator.mediaDevices.getUserMedia
    && typeof MediaRecorder !== 'undefined'
  );
}

// cleanTranscript makes a transcript safe to type into a pty, where a CR
// or LF is Enter and an ESC drives the agent's screen. CR, LF and Tab
// become a space. Every other C0 character, DEL and U+0080 to U+009F are
// dropped. Runs of space become one, and the ends are trimmed.
export function cleanTranscript(text) {
  return String(text || '')
    .replace(/[\r\n\t]/g, ' ')
    // eslint-disable-next-line no-control-regex
    .replace(/[\x00-\x1f\x7f-\x9f]/g, '')
    .replace(/\s+/g, ' ')
    .trim();
}

// applyReply decides what the prompt box and the status line become once a
// clip comes back as text. A silent clip never overwrites what was already
// typed there: the operator's draft survives a misfire, and the status line
// says so. Text already in the box is extended, never replaced, so two
// short clips in a row build one prompt instead of the phone forgetting the
// first one.
export function applyReply(body, existingText, where = 'prompt box') {
  const text = cleanTranscript(body.text || '');
  if (!text) {
    return {
      text: existingText || '',
      status: 'No speech was heard. Hold Record longer and speak closer to the microphone.',
    };
  }
  const trimmed = (existingText || '').trim();
  if (trimmed) {
    return { text: trimmed + ' ' + text, status: 'Added to the ' + where + '.' };
  }
  return { text, status: statusForReply(body) };
}

// voiceDownMessage is the message the mic shows when a voice status reply
// says voice cannot run, or null when it can. It carries the fix: a shell
// with the server's command typed, and Retry, or words that the fix is on
// the box. A reply with no ready field comes from a server that does not
// say, and the mic then just tries.
export function voiceDownMessage(body) {
  if (!body || body.ready !== false) return null;
  // Voice is getting its speech model: say how far, with no Retry, since
  // waiting is the fix.
  if (body.state === 'starting' && body.fetch && body.fetch.total > 0) {
    return MESSAGES.voiceFetching(body.fetch.done, body.fetch.total);
  }
  return MESSAGES.voiceDown(body.message || 'Voice cannot run now. Press Retry.', body.command || '', Boolean(body.away));
}

// retryVoice is the Retry action: it asks the server to start voice, and
// returns what the status line says after.
export async function retryVoice(api, say) {
  const body = await api('/api/voice/retry', { method: 'POST' });
  // A voice that is starting is followed, as after a Mic press.
  if (say && body && body.state === 'starting') followStart(api, say);
  return voiceDownMessage(body) || 'Voice is ready. Tap the mic.';
}

// followStart asks again while voice starts, so the status line follows the
// model download and ends with Voice is ready. note gets each message (null
// when voice can run). One follow runs at a time.
let following = false;
export async function followStart(api, say, note = () => {}) {
  if (following) return;
  following = true;
  try {
    for (;;) {
      await new Promise((r) => setTimeout(r, timing.pollMs));
      let body;
      try {
        body = await api('/api/voice/status');
      } catch {
        break;
      }
      const m = voiceDownMessage(body);
      note(m);
      if (m) say(m);
      if (body && body.state === 'starting') continue;
      if (!m) say(MESSAGES.voiceReady());
      break;
    }
  } finally {
    following = false;
  }
}

// VOICE_DOWN is the status a transcribe reply carries when voice cannot
// run. Its message already says why and ends with Retry.
const VOICE_DOWN = 503;

// noEngine is true for a transcribe failure that says the speech engine is
// not there: the voice server's engine_unavailable with a program or a
// package that is missing. A model that is still loading is not this.
export function noEngine(e) {
  return Boolean(e) && e.code === 'engine_unavailable' && /not on PATH|not installed/.test(String(e.message || ''));
}

// timing is how long the mic waits between asks while voice starts, how
// often a recording hands out its audio, and how long it waits between
// asks for the text so far. A test shortens them.
export const timing = { pollMs: 1000, sliceMs: 500, partialMs: 1500 };

// SPEECH_BITS is the bitrate the recorder asks for. Every partial carries
// the whole recording so far, and a browser's default is set for music
// (128 kbps in Chrome): at 48 kbps five minutes is under 2 MB, and speech
// loses nothing a speech engine hears.
export const SPEECH_BITS = 48000;

// newRecorder asks for SPEECH_BITS, and records at the browser's own rate
// when the browser refuses the option.
function newRecorder(stream) {
  try {
    return new MediaRecorder(stream, { audioBitsPerSecond: SPEECH_BITS });
  } catch {
    return new MediaRecorder(stream);
  }
}

// LIVE_TEXT_OLD is the line under the mic when the voice server is too old
// to show the text while the owner talks. The clip still works as before.
export const LIVE_TEXT_OLD = 'Live text needs a newer voice server.';

// LIVE_TEXT_OLD_WEB is that line when the web server itself has no
// partial route (coppice-rs answers 405 there).
export const LIVE_TEXT_OLD_WEB = 'Live text needs a newer coppice.';

// REFUSED are the statuses that end the partials of a recording quietly:
// the token was refused, or the address is banned for refused tokens.
const REFUSED = [401, 403, 429];

// LISTENING is the line under the mic while it records, before any words.
export const LISTENING = 'Listening…';

// tailText is the end of text, at most max characters, starting on a word
// and marked with an ellipsis when the start was cut. Words grow at the
// end, so the end is what the owner reads.
export function tailText(text, max = 160) {
  const t = String(text || '');
  if (t.length <= max) return t;
  let tail = t.slice(t.length - (max - 1));
  const sp = tail.indexOf(' ');
  if (sp >= 0 && sp < tail.length - 1) tail = tail.slice(sp + 1);
  return '…' + tail;
}

// clock is a count of seconds as m:ss.
export function clock(seconds) {
  const s = Math.max(0, Math.floor(seconds));
  return Math.floor(s / 60) + ':' + String(s % 60).padStart(2, '0');
}

// extensionFor is the file extension for a clip's Content-Type.
export function extensionFor(type) {
  const t = String(type || '').toLowerCase();
  if (t.includes('webm')) return 'webm';
  if (t.includes('ogg')) return 'ogg';
  if (t.includes('mp4') || t.includes('m4a') || t.includes('aac')) return 'm4a';
  if (t.includes('mpeg') || t.includes('mp3')) return 'mp3';
  if (t.includes('wav')) return 'wav';
  return 'bin';
}

// saveClip hands the audio to the browser as a download, so a clip that
// cannot become text is never lost. It returns true when the download began.
export function saveClip(clip) {
  try {
    const url = URL.createObjectURL(clip.blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = 'coppice-clip-' + new Date().toISOString().replace(/[:.]/g, '-') + '.' + extensionFor(clip.type);
    a.click();
    setTimeout(() => URL.revokeObjectURL(url), 60000);
    return true;
  } catch {
    return false;
  }
}

// whyFailed is the one line that says what failed for a clip.
export function whyFailed(e) {
  const m = String((e && e.message) || '').trim();
  if (e && e.status) return m || 'The server answered ' + e.status + '.';
  return 'The clip did not reach the server' + (m ? ' (' + m + ').' : '.');
}

// clipStore keeps a recording in IndexedDB until its text is in the box, so
// a reload or a dropped tab does not lose it. Every call is wrapped: a
// browser with no storage, or a private window, makes each one answer
// nothing, and the clip then lives in memory only. backend is for tests.
export const clipStore = {
  backend: null,
  db: null,
  open() {
    if (this.backend) return Promise.resolve(this.backend);
    if (this.db) return this.db;
    this.db = new Promise((resolve) => {
      try {
        if (typeof indexedDB === 'undefined' || !indexedDB) return resolve(null);
        const req = indexedDB.open('coppice-clips', 1);
        req.onupgradeneeded = () => req.result.createObjectStore('clips');
        req.onsuccess = () => resolve({
          // run answers the request's result when want is set (null when
          // there is none), and true for a write that finished.
          run: (mode, fn, want = false) => new Promise((res) => {
            try {
              const tx = req.result.transaction('clips', mode);
              const r = fn(tx.objectStore('clips'));
              tx.oncomplete = () => res(want ? (r && r.result !== undefined ? r.result : null) : true);
              tx.onerror = () => res(null);
              tx.onabort = () => res(null);
            } catch { res(null); }
          }),
        });
        req.onerror = () => resolve(null);
        req.onblocked = () => resolve(null);
      } catch { resolve(null); }
    });
    return this.db;
  },
  // get answers the stored record, or null.
  async get(key) {
    try {
      const b = await this.open();
      if (!b) return null;
      if (b.get) return (await b.get(key)) || null;
      return (await b.run('readonly', (st) => st.get(key), true)) || null;
    } catch { return null; }
  },
  // keys answers the stored keys that start with prefix, oldest first (the
  // key ends in the time the clip was made, so a plain sort is by age).
  async keys(prefix) {
    try {
      const b = await this.open();
      if (!b) return [];
      const all = b.keys ? await b.keys() : await b.run('readonly', (st) => st.getAllKeys(), true);
      return (all || []).filter((k) => typeof k === 'string' && k.startsWith(prefix)).sort();
    } catch { return []; }
  },
  // put answers true when the record is stored.
  async put(key, rec) {
    try {
      const b = await this.open();
      if (!b) return false;
      if (b.put) return Boolean(await b.put(key, rec));
      return Boolean(await b.run('readwrite', (st) => st.put(rec, key)));
    } catch { return false; }
  },
  async del(key) {
    try {
      const b = await this.open();
      if (!b) return false;
      if (b.del) return Boolean(await b.del(key));
      return Boolean(await b.run('readwrite', (st) => st.delete(key)));
    } catch { return false; }
  },
};

// clipLocks tells one tab which clips another live tab holds. A tab holds a
// Web Lock named for its clip until the tab closes or lets the clip go. A
// browser with no Web Locks holds nothing and sees every clip as free.
// backend is for tests.
export const clipLocks = {
  backend: null,
  manager() {
    return this.backend || (typeof navigator !== 'undefined' && navigator.locks) || null;
  },
  // hold takes the clip's lock and answers a function that lets it go.
  hold(id) {
    const m = this.manager();
    if (!m) return () => {};
    let letGo = () => {};
    // released is set when the clip goes before the browser grants the lock:
    // the grant then lets go at once and holds nothing.
    let released = false;
    try {
      Promise.resolve(m.request('coppice-clip:' + id, () => new Promise((res) => {
        if (released) res(); else letGo = res;
      }))).catch(() => {});
    } catch { /* no lock: the clip is simply not marked */ }
    return () => { released = true; letGo(); };
  },
  // free answers true when no live tab holds the clip.
  async free(id) {
    const m = this.manager();
    if (!m) return true;
    try {
      return Boolean(await m.request('coppice-clip:' + id, { ifAvailable: true }, (lock) => lock !== null));
    } catch { return true; }
  },
};

const IDLE = 'idle';
const STARTING = 'starting';
const RECORDING = 'recording';
const SENDING = 'sending';

// CAP_MARGIN_S is how long before the server's cap the page stops. The clip
// that results must fit the cap with room for the recorder's start-up and
// stop delay, so a clip the page itself recorded is not cut by the server.
export const CAP_MARGIN_S = 10;

function stopTracks(s) {
  if (s) s.getTracks().forEach((t) => t.stop());
}

// mountRecord wires one record button to one text box. voiced is true for
// the tell bar: words the mic adds there mark the message as spoken. Each button keeps
// its own recorder, so the pane screen's button and the tell bar's never
// share a clip. The text that comes back only fills the box. Nothing is
// sent until the operator sends it.
export function mountRecord(buttonId = 'record', boxId = 'text', where = 'prompt box', voiced = false) {
  const button = document.getElementById(buttonId);
  let state = IDLE;
  let recorder = null;
  let stream = null;
  let chunks = [];
  // clip is the last recording, kept until its text is in the box. A clip
  // that failed stays here for Send again and Save.
  let clip = null;
  // stored is true when the clip is also in IndexedDB, under clipId. Each
  // clip has its own key, so one tab never deletes another tab's clip.
  let stored = false;
  let clipId = '';
  let unhold = null;
  // placed is the text this clip put into the box, while the clip is kept.
  // Send again takes it out first, so the box never holds two copies.
  let placed = '';
  // maxSeconds is the longest clip the server takes, from its status reply.
  let maxSeconds = 0;
  let warned = false;
  let startedAt = 0;
  let ticker = null;
  // The line under the mic that shows the text so far. take is the number
  // of the recording now running: a reply for an older one is dropped.
  // seq numbers each partial of a recording, and shownSeq is the newest
  // one on the line, so a reply older than it is dropped too.
  let partialLine = null;
  let take = 0;
  let seq = 0;
  let shownSeq = 0;
  let partialTimer = null;
  // inFlight aborts the partial still uploading, when Stop comes.
  let inFlight = null;
  // liveOff is true when the status said the voice server has no live
  // text: a recording then says so and sends no partials.
  let liveOff = false;
  const stopTicker = () => { if (ticker !== null) clearInterval(ticker); ticker = null; };
  const line = () => {
    if (partialLine) return partialLine;
    partialLine = document.getElementById(buttonId + '-partial');
    return partialLine;
  };
  const showPartial = (text) => {
    const el = line();
    if (!el) return;
    el.textContent = tailText(text);
    el.hidden = false;
  };
  const clearPartial = () => {
    if (partialTimer !== null) clearTimeout(partialTimer);
    partialTimer = null;
    const el = line();
    if (!el) return;
    el.textContent = '';
    el.hidden = true;
  };
  // askPartial posts the recording so far, all its chunks in one blob (a
  // later chunk alone does not decode), and shows the text that comes
  // back. One partial is in flight at a time: the next is asked for a
  // moment after this one answers. Nothing here touches the clip, the box
  // or the status line. A 404 is a voice server too old for live text: the
  // line says so and this recording asks no more. A refused token also
  // ends them and hides the line. Any other failure says nothing, and the
  // next partial is on its way.
  const schedulePartial = (mine) => {
    partialTimer = setTimeout(() => askPartial(mine), timing.partialMs);
  };
  const askPartial = async (mine) => {
    partialTimer = null;
    if (mine !== take || state !== RECORDING) return;
    if (!chunks.length) { schedulePartial(mine); return; }
    seq += 1;
    const type = contentTypeFor(recorder);
    const abort = typeof AbortController === 'undefined' ? null : new AbortController();
    inFlight = abort;
    try {
      const body = await window.coppice.api('/api/voice/partial?seq=' + seq, {
        method: 'POST',
        headers: { 'Content-Type': type },
        body: new Blob(chunks, { type }),
        signal: abort ? abort.signal : undefined,
      });
      if (mine !== take || state !== RECORDING) return;
      const n = Number(body && body.seq) || 0;
      if (n >= shownSeq) {
        shownSeq = n;
        showPartial(cleanTranscript(body.text) || LISTENING);
      }
    } catch (e) {
      if (mine !== take || state !== RECORDING) return;
      if (e && e.status === 404) { showPartial(LIVE_TEXT_OLD); return; }
      if (e && e.status === 405) { showPartial(LIVE_TEXT_OLD_WEB); return; }
      // A refused token stops the partials of this recording and hides
      // the line, so no Listening stays up that will never change. Each
      // refusal counts toward the server's ban, and the clip must still
      // meet the sentence that names the fix, not "wait 60 seconds".
      if (e && REFUSED.includes(e.status)) { clearPartial(); return; }
    }
    schedulePartial(mine);
  };
  const tickLabel = (word, limit) => {
    stopTicker();
    const t0 = Date.now();
    ticker = setInterval(() => {
      const secs = (Date.now() - t0) / 1000;
      const cap = limit && maxSeconds > 0;
      // The page stops a margin before the server's cap.
      const stopAt = Math.max(1, maxSeconds - CAP_MARGIN_S);
      const nearing = cap && secs >= stopAt - 120;
      // The limit shows only near the end, so the label leaves room for the
      // text box on a narrow screen.
      button.textContent = word + ' ' + clock(secs) + (nearing ? '/' + clock(stopAt) : '');
      if (!cap) return;
      if (secs >= stopAt) {
        stopTicker();
        window.coppice.status(MESSAGES.clipLimit(maxSeconds));
        if (state === RECORDING) {
          state = SENDING;
          recorder.stop();
        }
      } else if (!warned && nearing) {
        warned = true;
        window.coppice.status(MESSAGES.clipNearLimit(stopAt - secs));
      }
    }, 1000);
  };

  // release lets the clip go, here and in storage.
  const release = () => {
    clip = null;
    // Only this clip's own key goes. Another tab's clip has another key.
    if (stored && clipId) clipStore.del(clipId);
    stored = false;
    if (unhold) unhold();
    unhold = null;
    clipId = '';
    placed = '';
  };
  // A tab that closes while a clip is not in storage loses it: ask first.
  if (typeof window !== 'undefined' && window.addEventListener) {
    window.addEventListener('beforeunload', (e) => {
      if (clip && !stored) { e.preventDefault(); e.returnValue = ''; }
    });
  }

  // down is the message that says why voice cannot run, or null. Its fix
  // buttons, Retry among them, show on the status line.
  let down = null;
  // pressed is true after the first Mic press. Until then the page asks
  // whether voice can run and says nothing about it, so a load never
  // shows a voice banner.
  let pressed = false;
  // withClip adds the kept clip's buttons to a voice-down line, so the
  // recording stays within reach whichever call said voice was down.
  const withClip = (m) => {
    if (!m || !clip || m.noClip) return m;
    const have = (l) => m.actions.some((a) => a.label === l);
    const extra = [];
    if (!have('Send again') && !m.noResend) extra.push(clipAction('Send again', run.again));
    if (!have('Save')) extra.push(clipAction('Save', run.save));
    return extra.length ? { ...m, actions: [...m.actions, ...extra] } : m;
  };
  const setDown = (m) => {
    down = m ? withClip(m) : m;
    if (down && pressed) window.coppice.status(down);
  };
  // check asks the server whether voice can run, and answers the reply, or
  // null when the request failed. A request that fails leaves what the mic
  // knew as it was: the mic then just tries, and a clip that cannot be
  // heard says why.
  const check = async (path, options) => {
    try {
      const body = await window.coppice.api(path, options);
      if (body && Number(body.max_seconds) > 0) maxSeconds = Number(body.max_seconds);
      // Only a status that reached the voice server knows: one that could
      // not ask leaves live text to the first partial.
      if (body && body.partial_known === true) liveOff = body.partial === false;
      setDown(voiceDownMessage(body));
      return body;
    } catch {
      // Nothing is known, so nothing changes.
      return null;
    }
  };
  const watch = () => followStart(window.coppice.api, window.coppice.status, (m) => { down = m; });
  // A page with no token yet asks nothing: the mic learns on its first
  // clip instead.
  const app = window.coppice;
  if (!(app && app.state && !app.state.token)) check('/api/voice/status');

  // finish runs once MediaRecorder's own stop event fires, after every
  // last chunk of audio has already reached dataavailable. Posting from
  // here, rather than right after calling stop(), is what guarantees the
  // blob this function builds holds the whole clip.
  const finish = async () => {
    state = SENDING;
    // A partial still on its way belongs to a recording that is over. The
    // line keeps its last words while the clip is sent.
    take += 1;
    if (partialTimer !== null) clearTimeout(partialTimer);
    partialTimer = null;
    // A partial still uploading would share the uplink with the clip.
    if (inFlight) inFlight.abort();
    inFlight = null;
    stopTracks(stream);
    stream = null;
    stopTicker();
    const type = contentTypeFor(recorder);
    clip = { blob: new Blob(chunks, { type }), type, seconds: (Date.now() - startedAt) / 1000 };
    clipId = buttonId + ':' + Date.now().toString(36).padStart(10, '0') + Math.random().toString(36).slice(2, 8);
    unhold = clipLocks.hold(clipId);
    stored = await clipStore.put(clipId, { blob: clip.blob, type: clip.type, seconds: clip.seconds });
    await deliver();
  };

  // deliver posts the kept clip. The clip is released only when its text has
  // landed in the box. Any failure keeps it and says what failed, with the
  // buttons that send it again or save it.
  const run = {
    again: () => resend(),
    save: () => {
      if (!clip) return;
      // Save does not release the clip: a download can do nothing without
      // an error. Discard lets it go.
      if (saveClip(clip)) window.coppice.status(MESSAGES.clipSaved(run));
      else window.coppice.status('The audio could not be saved here. Send it again instead.');
    },
    discard: () => { release(); window.coppice.status(''); },
  };
  const resend = async () => {
    if (!clip || state !== IDLE) return;
    state = SENDING;
    await deliver();
  };
  // withoutPlaced is the box text with this clip's earlier text taken out
  // (its last occurrence), so what the operator typed or kept stays.
  const withoutPlaced = (value) => {
    if (!placed) return value;
    const i = value.lastIndexOf(placed);
    if (i < 0) return value;
    // Only the join where the text was taken out is folded. The operator's
    // own line breaks and spacing elsewhere stay.
    const left = value.slice(0, i).replace(/\s+$/, '');
    const right = value.slice(i + placed.length).replace(/^\s+/, '');
    return left && right ? left + ' ' + right : left + right;
  };
  const deliver = async () => {
    button.textContent = 'Sending';
    tickLabel('Sending');
    if (clip.seconds > 20) window.coppice.status(MESSAGES.clipWorking(clip.seconds));
    try {
      const body = await window.coppice.api('/api/voice/transcribe?cleanup=1', {
        method: 'POST',
        headers: { 'Content-Type': clip.type },
        body: clip.blob,
      });
      const box = document.getElementById(boxId);
      // A clip sent again replaces the text it put in the box before.
      const existing = withoutPlaced(box.value);
      const result = applyReply(body, existing, where);
      // Words the mic added mean the reply to this message is read aloud.
      // Only the tell bar goes to the steward: a pane's own prompt box is for
      // that agent, and its words are not a message to read a reply to.
      if (voiced && result.text !== box.value && window.coppice.speech) window.coppice.speech.markVoiced();
      if (!cleanTranscript(body.text)) {
        // No text landed, so the clip is not released.
        window.coppice.status(MESSAGES.clipKept('No speech was heard.', run));
        return;
      }
      box.value = result.text;
      placed = cleanTranscript(body.text);
      const gaps = Number(body.gaps) || 0;
      const cut = Number(body.cut_s) || 0;
      if (gaps > 0) {
        // The text is in the box, but some of the audio was not heard. The
        // clip stays so it can be sent again or saved.
        window.coppice.status(MESSAGES.clipGaps(gaps, run, cut));
        return;
      }
      if (cut > 0) {
        window.coppice.status(MESSAGES.clipCut(cut, run));
        return;
      }
      release();
      window.coppice.status(result.status);
    } catch (e) {
      if (e.status === VOICE_DOWN && noEngine(e)) {
        // The engine is missing. The server's own words name a program;
        // this line says what to do about it, with no Retry. Sending again
        // cannot help, but the audio can still be saved.
        const m = MESSAGES.voiceNoEngine();
        const kept = { ...m, actions: [clipAction('Save', run.save)], noResend: true };
        setDown(kept);
        window.coppice.status(kept);
      } else if (e.status === VOICE_DOWN) {
        // The reply's words show first; the status call then brings the
        // fix the server names. The clip's buttons come after the fix.
        const m = MESSAGES.voiceDown(e.message, '', false);
        setDown(m);
        await check('/api/voice/status');
      } else window.coppice.status(MESSAGES.clipKept(whyFailed(e), run));
    } finally {
      stopTicker();
      clearPartial();
      state = IDLE;
      button.textContent = idleText;
    }
  };

  // start asks for the microphone and only then builds the recorder, both
  // in one try, so a refusal at either step leaves no stream open and one
  // sentence on the status line. The click handler only calls start from
  // idle and sets starting before the first await, so no second start can
  // begin while this one waits for the permission sheet.
  const start = async () => {
    state = STARTING;
    let newStream = null;
    try {
      newStream = await navigator.mediaDevices.getUserMedia({ audio: true });
      recorder = newRecorder(newStream);
    } catch {
      stopTracks(newStream);
      state = IDLE;
      window.coppice.status('The microphone was not allowed. Allow it for this page, then tap ' + idleText + ' again.');
      return;
    }
    stream = newStream;
    chunks = [];
    recorder.addEventListener('dataavailable', (e) => {
      if (e.data && e.data.size > 0) chunks.push(e.data);
    });
    recorder.addEventListener('stop', () => finish());
    // A timeslice makes the recorder hand out its audio while it records,
    // so the text so far can be asked for.
    recorder.start(timing.sliceMs);
    state = RECORDING;
    take += 1;
    seq = 0;
    shownSeq = 0;
    if (liveOff) showPartial(LIVE_TEXT_OLD);
    else {
      showPartial(LISTENING);
      schedulePartial(take);
    }
    startedAt = Date.now();
    warned = false;
    if (!maxSeconds) check('/api/voice/status');
    button.textContent = 'Stop';
    tickLabel('Stop', true);
    window.coppice.status('');
  };

  // A clip kept from before a reload or a dropped tab comes back as one
  // line: send it again, save it, or discard it.
  const recover = async () => {
    const ids = await clipStore.keys(buttonId + ':');
    for (const id of ids) {
      // Not while this tab records or sends, and not a clip another live
      // tab holds.
      if (clip || state !== IDLE) return;
      if (!(await clipLocks.free(id))) continue;
      const rec = await clipStore.get(id);
      if (clip || state !== IDLE) return;
      if (!rec || !rec.blob) continue;
      clip = { blob: rec.blob, type: rec.type || 'audio/webm', seconds: Number(rec.seconds) || 0 };
      clipId = id;
      stored = true;
      unhold = clipLocks.hold(id);
      window.coppice.status(MESSAGES.clipRecovered(run));
      return;
    }
  };
  recover();

  const idleText = button.textContent || 'Record';
  button.addEventListener('click', async () => {
    pressed = true;
    // A mic press unlocks sound and stops any reply being read aloud.
    if (window.coppice.speech) window.coppice.speech.pressMic();
    // Over plain http from another machine the browser gives no
    // microphone at all. Say that this is why, and where the fix is,
    // before the browser is blamed. An older browser with no
    // isSecureContext is taken as secure.
    if (typeof window !== 'undefined' && window.isSecureContext === false) {
      window.coppice.status(MESSAGES.micNeedsHttps());
      return;
    }
    if (!canRecord()) {
      window.coppice.status('This browser cannot record audio. Try a newer browser or device.');
      return;
    }
    // A tap while the microphone permission is pending, or while a clip is
    // sending, does nothing. Acting on it would start a second recorder or
    // stop one that is not there yet.
    if (state === IDLE) {
      if (clip) {
        window.coppice.status(MESSAGES.clipPending(run));
        return;
      }
      if (down) {
        // Voice was down when last asked. It may have come up since, so
        // ask again before saying so.
        state = STARTING;
        const body = await check('/api/voice/status');
        state = IDLE;
        if (down) {
          if (body && body.state === 'starting') watch();
          return;
        }
      }
      start();
      return;
    }
    if (state === RECORDING) {
      state = SENDING;
      recorder.stop();
    }
  });
}

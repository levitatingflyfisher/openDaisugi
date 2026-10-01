import { MESSAGES } from './messages.js';

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

// timing is how long the mic waits between asks while voice starts. A test
// shortens it.
export const timing = { pollMs: 1000 };

const IDLE = 'idle';
const STARTING = 'starting';
const RECORDING = 'recording';
const SENDING = 'sending';

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

  // down is the message that says why voice cannot run, or null. Its fix
  // buttons, Retry among them, show on the status line.
  let down = null;
  // pressed is true after the first Mic press. Until then the page asks
  // whether voice can run and says nothing about it, so a load never
  // shows a voice banner.
  let pressed = false;
  const setDown = (m) => {
    down = m;
    if (m && pressed) window.coppice.status(m);
  };
  // check asks the server whether voice can run, and answers the reply, or
  // null when the request failed. A request that fails leaves what the mic
  // knew as it was: the mic then just tries, and a clip that cannot be
  // heard says why.
  const check = async (path, options) => {
    try {
      const body = await window.coppice.api(path, options);
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
    stopTracks(stream);
    stream = null;
    button.textContent = 'Sending';
    const type = contentTypeFor(recorder);
    const blob = new Blob(chunks, { type });
    try {
      const body = await window.coppice.api('/api/voice/transcribe?cleanup=1', {
        method: 'POST',
        headers: { 'Content-Type': type },
        body: blob,
      });
      const box = document.getElementById(boxId);
      const result = applyReply(body, box.value, where);
      // Words the mic added mean the reply to this message is read aloud.
      // Only the tell bar goes to the foreman: a pane's own prompt box is for
      // that agent, and its words are not a message to read a reply to.
      if (voiced && result.text !== box.value && window.coppice.speech) window.coppice.speech.markVoiced();
      box.value = result.text;
      window.coppice.status(result.status);
    } catch (e) {
      if (e.status === VOICE_DOWN) {
        // The reply's words show first; the status call then brings the
        // fix the server names.
        setDown(MESSAGES.voiceDown(e.message, '', false));
        await check('/api/voice/status');
      } else window.coppice.status(e.message);
    } finally {
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
      recorder = new MediaRecorder(newStream);
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
    recorder.start();
    state = RECORDING;
    button.textContent = 'Stop';
    window.coppice.status('');
  };

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

// The phone's own record button. One tap starts a clip with MediaRecorder,
// a second tap stops it and posts it through window.coppice.api to
// /api/voice/transcribe, which the coppice web server forwards to the
// voice server. The text that comes back lands in the prompt box for the
// operator to read before anything is sent to the pane.

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

// applyReply decides what the prompt box and the status line become once a
// clip comes back as text. A silent clip never overwrites what was already
// typed there: the operator's draft survives a misfire, and the status line
// says so. Text already in the box is extended, never replaced, so two
// short clips in a row build one prompt instead of the phone forgetting the
// first one.
export function applyReply(body, existingText) {
  const text = body.text || '';
  if (!text) {
    return {
      text: existingText || '',
      status: 'No speech was heard. Hold Record longer and speak closer to the microphone.',
    };
  }
  const trimmed = (existingText || '').trim();
  if (trimmed) {
    return { text: trimmed + ' ' + text, status: 'Added to the prompt box.' };
  }
  return { text, status: statusForReply(body) };
}

const IDLE = 'idle';
const STARTING = 'starting';
const RECORDING = 'recording';
const SENDING = 'sending';

let state = IDLE;
let recorder = null;
let stream = null;
let chunks = [];

function stopTracks(s) {
  if (s) s.getTracks().forEach((t) => t.stop());
}

// finish runs once MediaRecorder's own stop event fires, after every last
// chunk of audio has already reached dataavailable. Posting from here,
// rather than right after calling stop(), is what guarantees the blob
// this function builds holds the whole clip.
async function finish(button) {
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
    const box = document.getElementById('text');
    const result = applyReply(body, box.value);
    box.value = result.text;
    window.coppice.status(result.status);
  } catch (e) {
    window.coppice.status(e.message);
  } finally {
    state = IDLE;
    button.textContent = 'Record';
  }
}

// start asks for the microphone and only then builds the recorder, both in
// one try, so a refusal at either step leaves no stream open and one
// sentence on the status line. The click handler only calls start from
// idle and sets starting before the first await, so no second start can
// begin while this one waits for the permission sheet.
async function start(button) {
  state = STARTING;
  let newStream = null;
  try {
    newStream = await navigator.mediaDevices.getUserMedia({ audio: true });
    recorder = new MediaRecorder(newStream);
  } catch {
    stopTracks(newStream);
    state = IDLE;
    window.coppice.status('The microphone was not allowed. Allow it for this page, then tap Record again.');
    return;
  }
  stream = newStream;
  chunks = [];
  recorder.addEventListener('dataavailable', (e) => {
    if (e.data && e.data.size > 0) chunks.push(e.data);
  });
  recorder.addEventListener('stop', () => finish(button));
  recorder.start();
  state = RECORDING;
  button.textContent = 'Stop';
  window.coppice.status('');
}

export function mountRecord() {
  const button = document.getElementById('record');
  button.addEventListener('click', () => {
    if (!canRecord()) {
      window.coppice.status('This browser cannot record audio. Try a newer browser or device.');
      return;
    }
    // A tap while the microphone permission is pending, or while a clip is
    // sending, does nothing. Acting on it would start a second recorder or
    // stop one that is not there yet.
    if (state === IDLE) {
      start(button);
      return;
    }
    if (state === RECORDING) {
      state = SENDING;
      recorder.stop();
    }
  });
}

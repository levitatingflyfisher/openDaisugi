// Where a key goes on the floor. The keys belong either to the selected
// window, which sends every key to its agent, or to the rail. ctrl-space
// moves them between the two.

// NAMED_KEYS are the bytes a terminal sends for the keys that are not
// text.
const NAMED_KEYS = {
  Enter: '\r', Backspace: '\x7f', Tab: '\t', Escape: '\x1b',
  ArrowUp: '\x1b[A', ArrowDown: '\x1b[B', ArrowRight: '\x1b[C', ArrowLeft: '\x1b[D',
  Home: '\x1b[H', End: '\x1b[F', Delete: '\x1b[3~', PageUp: '\x1b[5~', PageDown: '\x1b[6~',
};

// keyBytes is the bytes a terminal sends for one keydown event, or null
// for a key the page leaves to the browser. Printable text goes as it is.
// ctrl and a letter is that letter's control byte. shift-Tab is the back
// tab sequence. A key with alt or meta held is left alone.
export function keyBytes(e) {
  if (!e || e.altKey || e.metaKey) return null;
  const k = String(e.key || '');
  if (e.ctrlKey) {
    return /^[a-zA-Z]$/.test(k) ? String.fromCharCode(k.toLowerCase().charCodeAt(0) - 96) : null;
  }
  if (k === 'Tab' && e.shiftKey) return '\x1b[Z';
  if (Object.prototype.hasOwnProperty.call(NAMED_KEYS, k)) return NAMED_KEYS[k];
  return [...k].length === 1 ? k : null;
}

// isPaste is true for ctrl-v or meta-v, with or without shift: the keys
// that paste in a browser.
export function isPaste(e) {
  return Boolean(e && (e.ctrlKey || e.metaKey) && !e.altKey && String(e.key).toLowerCase() === 'v');
}

// isLeave is true for ctrl-space, the key that moves the keys between the
// selected window and the rail.
export function isLeave(e) {
  return Boolean(e && e.ctrlKey && !e.altKey && !e.metaKey && (e.key === ' ' || e.code === 'Space'));
}

const RAIL_MOVES = { ArrowDown: 1, j: 1, ArrowUp: -1, k: -1 };

// routeKey is what one keydown does. mode is 'window' when the selected
// window has the keys, or 'rail'. It returns null for a key the page
// leaves to the browser, or one of:
//   {kind: 'send', bytes}   send bytes to the selected window's agent
//   {kind: 'leave'}         give the keys to the rail
//   {kind: 'move', by}      move the rail selection
//   {kind: 'enter'}         put the selected row in a window and type there
//   {kind: 'put', window}   put the selected row in window number window+1
//   {kind: 'deny'}          deny the selected row's ask
//   {kind: 'kill'}          ask to stop the selected row's agent
//   {kind: 'rename'}        edit the selected row's label in place
// ctrl-w stops an agent only where the browser lets a page have it, as in
// an installed app window. Delete works everywhere.
export function routeKey(e, mode) {
  if (!e) return null;
  if (mode === 'window') {
    if (isPaste(e)) return null;
    if (isLeave(e)) return { kind: 'leave' };
    const bytes = keyBytes(e);
    return bytes === null ? null : { kind: 'send', bytes };
  }
  if (isLeave(e)) return { kind: 'enter' };
  const k = String(e.key || '');
  if (e.ctrlKey && !e.altKey && !e.metaKey && !e.shiftKey && k.toLowerCase() === 'w') return { kind: 'kill' };
  if (e.ctrlKey || e.metaKey || e.altKey) return null;
  if (k === 'Delete') return { kind: 'kill' };
  if (k === 'F2') return { kind: 'rename' };
  if (Object.prototype.hasOwnProperty.call(RAIL_MOVES, k)) return { kind: 'move', by: RAIL_MOVES[k] };
  if (k === 'Enter') return { kind: 'enter' };
  if (/^[1-9]$/.test(k)) return { kind: 'put', window: Number(k) - 1 };
  if (k === 'n') return { kind: 'deny' };
  return null;
}

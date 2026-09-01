// The colony: every pane is a forager on a field. Where a forager stands
// comes from a hash of its id, so it means nothing and never moves. Its
// colour is its state, and its bar is the time since the socket last gave
// a state for it, which is a fact. A drag box selects foragers.

// PAD is the pixel margin a forager keeps from the edge of the field.
const PAD = 16;

// HOUR is the quiet time at which the bar is full.
const HOUR = 3600;

// RADIUS is the pixel radius of one forager.
export const RADIUS = 8;

// hash is FNV-1a over the text, a 32 bit unsigned number.
function hash(text) {
  let h = 0x811c9dc5;
  for (let i = 0; i < text.length; i++) {
    h ^= text.charCodeAt(i);
    h = Math.imul(h, 0x01000193) >>> 0;
  }
  return h >>> 0;
}

// place puts each pane on a w by h field. The place depends only on the
// pane id, the field size and the seed, so a new pane moves no other.
export function place(panes, w, h, seed) {
  const spanX = Math.max(1, w - 2 * PAD);
  const spanY = Math.max(1, h - 2 * PAD);
  return (panes || [])
    .filter((p) => p && typeof p.id === 'string')
    .map((p) => ({
      id: p.id,
      x: PAD + (hash(seed + ':x:' + p.id) % 10000) / 10000 * spanX,
      y: PAD + (hash(seed + ':y:' + p.id) % 10000) / 10000 * spanY,
    }));
}

// inBox is the ids of the positions inside box, dragged in any direction.
export function inBox(positions, box) {
  const x0 = Math.min(box.x0, box.x1);
  const x1 = Math.max(box.x0, box.x1);
  const y0 = Math.min(box.y0, box.y1);
  const y1 = Math.max(box.y0, box.y1);
  return (positions || [])
    .filter((p) => p.x >= x0 && p.x <= x1 && p.y >= y0 && p.y <= y1)
    .map((p) => p.id);
}

// bar maps quiet seconds to a length from 0 to 1, full at an hour. With
// no quiet time known it is null, and no bar is drawn.
export function bar(quiet) {
  if (quiet === null || quiet === undefined || Number.isNaN(Number(quiet))) return null;
  return Math.min(1, Math.max(0, Number(quiet) / HOUR));
}

// quietFor is the seconds from the newest state the socket gave for pane
// to now: the pane's own ts, or a newer state event. It is null when
// there is neither.
export function quietFor(pane, events, now) {
  let last = typeof pane.ts === 'number' ? pane.ts : null;
  for (const e of events || []) {
    if (!e || e.event !== 'state' || e.pane !== pane.id || typeof e.ts !== 'number') continue;
    if (last === null || e.ts > last) last = e.ts;
  }
  return last === null ? null : Math.max(0, now - last);
}

// foragerAt is the id of the forager under a click at x, y, or ''.
export function foragerAt(positions, x, y) {
  let best = '';
  let bestD = RADIUS + 4;
  for (const p of positions || []) {
    const d = Math.hypot(p.x - x, p.y - y);
    if (d <= bestD) {
      best = p.id;
      bestD = d;
    }
  }
  return best;
}

// STRIP_H is the tallest field drawn as a strip: one row of foragers.
export const STRIP_H = 120;

// STRIP_ROW is the height of one strip forager: its dot, the quiet bar
// under it, and one 11 px line of label.
export const STRIP_ROW = 2 * RADIUS + 4 + 11;

// isStrip is true for a field height that is a strip.
export function isStrip(h) {
  const v = Number(h) || 0;
  return v > 0 && v <= STRIP_H;
}

// stripPlace puts each pane in one row across a w by h strip, in list
// order, each in a slot of equal width with its dot at the slot's middle
// and room under it for its label.
export function stripPlace(panes, w, h) {
  const list = (panes || []).filter((p) => p && typeof p.id === 'string');
  if (list.length === 0) return [];
  const slot = Math.max(1, w) / list.length;
  // The dot, a gap and one line of label, set in the middle of the height.
  const y = Math.max(RADIUS + 2, Math.round((h - STRIP_ROW) / 2) + RADIUS);
  return list.map((p, i) => ({ id: p.id, x: slot * (i + 0.5), y, slot }));
}

// fitLabel is text cut to fit max pixels as measure(text) reads them,
// ending in an ellipsis when it was cut, or '' when not even that fits.
export function fitLabel(text, max, measure) {
  const t = String(text || '');
  if (measure(t) <= max) return t;
  for (let n = t.length - 1; n > 0; n--) {
    const cut = t.slice(0, n) + '…';
    if (measure(cut) <= max) return cut;
  }
  return '';
}

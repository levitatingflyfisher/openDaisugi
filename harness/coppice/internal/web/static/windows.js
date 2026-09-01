// The pure rules behind the floor's windows: how big a cell is, how many
// windows fit, which agents fill them first, and where the keys start.
// Nothing here touches the page or a server.

// MIN_CELL_W is the readable size: no window draws a cell narrower than
// this many CSS pixels. A window too narrow for its agent scrolls sideways.
export const MIN_CELL_W = 7;
// MIN_COLS and MIN_ROWS are the smallest window worth making, in cells at
// the readable size.
export const MIN_COLS = 80;
export const MIN_ROWS = 24;
// MAX_FONT caps the font in a wide window, so one agent alone does not
// draw giant text.
export const MAX_FONT = 15;
// PREFERRED_FONT is the font before a window has a measured width.
export const PREFERRED_FONT = 14;
// LINE is the line height as a share of the font size.
export const LINE = 1.2;
// ADVANCE is the glyph advance as a share of the font size when nothing
// was measured. Most monospace faces sit near it.
export const ADVANCE = 0.6;
// The room a window takes around its text: border, padding, and the
// header's two lines, the name and the stack bar.
export const CHROME_W = 20;
export const CHROME_H = 64;
// GAP is the space between two windows.
export const GAP = 8;
// MAX_WINDOWS is the most windows the floor makes. Number keys reach nine.
export const MAX_WINDOWS = 9;

const goodAdvance = (a) => (Number.isFinite(a) && a > 0 ? a : ADVANCE);
// lineOf is the line height for a font. The small take keeps a float
// such as 14.000000000000002 from rounding up a whole pixel.
const lineOf = (font) => Math.ceil(font * LINE - 1e-6);

// cellFor is the cell a window draws at: w and h in CSS pixels, the font
// size in CSS pixels, and the device pixel ratio. The cell fits cols
// columns into widthPx, but never below MIN_CELL_W and never past
// MAX_FONT. advance is the measured glyph advance per pixel of font.
export function cellFor(widthPx, cols, advance, dpr) {
  const adv = goodAdvance(advance);
  const width = Number(widthPx) || 0;
  let w = width > 0 && cols > 0 ? width / cols : adv * PREFERRED_FONT;
  w = Math.min(Math.max(w, MIN_CELL_W), adv * MAX_FONT);
  const font = w / adv;
  return { w, h: lineOf(font), font, dpr: Number(dpr) > 0 ? Number(dpr) : 1 };
}

// windowCapacity is the grid of windows an area of areaW by areaH CSS
// pixels holds, each at least MIN_COLS by MIN_ROWS readable cells. It is
// at least one window and at most MAX_WINDOWS.
export function windowCapacity(areaW, areaH, advance) {
  const minCell = { w: MIN_CELL_W, h: lineOf(MIN_CELL_W / goodAdvance(advance)) };
  const minW = MIN_COLS * minCell.w + CHROME_W;
  const minH = MIN_ROWS * minCell.h + CHROME_H;
  let cols = Math.max(1, Math.floor(((Number(areaW) || 0) + GAP) / (minW + GAP)));
  let rows = Math.max(1, Math.floor(((Number(areaH) || 0) + GAP) / (minH + GAP)));
  while (cols * rows > MAX_WINDOWS) {
    if (rows >= cols) rows -= 1;
    else cols -= 1;
  }
  return { cols, rows };
}

// windowShape is how k windows sit inside a capacity: the fewest columns
// that still hold them, so each window is as wide as it can be, since a
// terminal line needs width more than height.
export function windowShape(k, cap) {
  const most = cap.cols * cap.rows;
  const n = Math.min(Math.max(1, k), most);
  let cols = 1;
  while (cols * cap.rows < n) cols += 1;
  return { cols, rows: Math.ceil(n / cols) };
}

// fillOrder is the order agents take empty windows: the ones that need
// you, then the working ones, then the rest, each in rail order. rows are
// rail rows, where a held ask already reads as working.
export function fillOrder(rows) {
  const list = Array.isArray(rows) ? rows.filter((r) => r && r.id) : [];
  const need = list.filter((r) => r.state === 'blocked');
  const work = list.filter((r) => r.state === 'working');
  const rest = list.filter((r) => r.state !== 'blocked' && r.state !== 'working');
  return [...need, ...work, ...rest].map((r) => r.id);
}

// ENDED marks a slot that shows an ended agent's line instead of an agent.
// No pane id starts with it.
const ENDED = '\u0000ended:';

export const endedSlot = (pane) => ENDED + pane;
export const isEndedSlot = (slot) => typeof slot === 'string' && slot.startsWith(ENDED);
// paneOfSlot is the pane a slot names, live or ended, or ''.
export const paneOfSlot = (slot) => (isEndedSlot(slot) ? slot.slice(ENDED.length) : slot || '');
// livePanes is the panes the slots hold, with empty and ended slots left out.
export const livePanes = (slots) => (Array.isArray(slots) ? slots : []).filter((s) => s && !isEndedSlot(s));

// startSelect is the slot the keyboard starts on: the first that holds an
// agent that needs you, else the first that holds an agent, else -1 for
// the rail. needs is a set of pane ids.
export function startSelect(slots, needs) {
  const list = Array.isArray(slots) ? slots : [];
  const live = (s) => s && !isEndedSlot(s);
  const need = list.findIndex((s) => live(s) && needs && needs.has(s));
  if (need >= 0) return need;
  return list.findIndex(live);
}

// exitText is how an exit code reads: exit 3, or killed for the -1 the
// server reports for a process a signal ended.
export function exitText(code) {
  return code < 0 ? 'killed' : 'exit ' + code;
}

// endedLine is the one line a window shows when its agent ends on its own.
export function endedLine(label, exitCode) {
  const name = label || 'agent';
  return Number.isInteger(exitCode) ? name + ' ended (' + exitText(exitCode) + ')' : name + ' ended';
}

// PHONE_W is the width under which a page is a phone. PHONE_SHORT_W and
// PHONE_SHORT_H catch a phone on its side: wider than PHONE_W but too
// short for a rail above a window.
export const PHONE_W = 600;
export const PHONE_SHORT_W = 900;
export const PHONE_SHORT_H = 500;

// phoneSize is true for a page the phone layout serves: the overview with
// an agent sliding in over it, and no windows. A page with no known height
// is judged by its width alone.
export function phoneSize(widthPx, heightPx) {
  const w = Number(widthPx) || 0;
  const h = Number(heightPx) || 0;
  if (w < PHONE_W) return true;
  return w < PHONE_SHORT_W && h > 0 && h < PHONE_SHORT_H;
}

// scrollFollow is where a window's body scrolls so the cursor stays in
// sight. It follows the cursor row down while the reader has not scrolled
// away from where the last follow put it, and moves sideways, with a
// margin of eight cells, when the cursor moves past an edge. s holds the
// shown size, the grid's cols and rows, the cell, the cursor, the scroll
// now (top, left), and autoTop and lastX from the last call. It returns
// the new top and left, and autoTop and lastX for the next call.
export function scrollFollow(s) {
  let top = Number(s.top) || 0;
  let left = Number(s.left) || 0;
  let autoTop = s.autoTop;
  const shownH = Number(s.shownH) || 0;
  const gridH = s.rows * s.cellH;
  if (shownH > 0 && gridH > shownH) {
    const following = autoTop === undefined || top === autoTop || top + shownH >= gridH - s.cellH;
    if (following) {
      top = Math.min(gridH - shownH, Math.max(0, (s.cursor[1] + 1) * s.cellH - shownH));
      autoTop = top;
    }
  }
  const shownW = Number(s.shownW) || 0;
  const gridW = s.cols * s.cellW;
  const cx = s.cursor[0];
  if (shownW > 0 && gridW > shownW && cx !== s.lastX) {
    const at = cx * s.cellW;
    const margin = 8 * s.cellW;
    if (at + s.cellW > left + shownW) left = Math.min(gridW - shownW, at + s.cellW - shownW + margin);
    else if (at < left) left = Math.max(0, at - margin);
  }
  return { top, left, autoTop, lastX: cx };
}

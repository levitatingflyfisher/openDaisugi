// The pane grid, painted on a canvas. The first frame after an attach
// carries every row, and the rest carry only the rows that changed.

// The seven frame cell attribute bits, matching PINS.md and vt.go.
export const ATTR = { BOLD: 1, FAINT: 2, ITALIC: 4, UNDERLINE: 8, BLINK: 16, INVERSE: 32, STRIKE: 64 };

const BLANK = [' ', '', '', 0];
const PAPER = '#0b0f10';
const INK = '#e8eae7';

export function newGrid(cols, rows) {
  return {
    cols, rows, cursor: [0, 0],
    cells: Array.from({ length: rows }, () => Array.from({ length: cols }, () => BLANK)),
  };
}

// applyFrame folds one frame into the grid and returns it. A frame for a
// different size starts a fresh grid rather than mixing two shapes, and a row
// index outside the grid is dropped: a malformed frame must not blank a pane
// the operator is reading.
export function applyFrame(grid, frame) {
  let g = grid;
  if (!g || g.cols !== frame.cols || g.rows !== frame.rows) g = newGrid(frame.cols, frame.rows);
  const changed = frame.rows_changed || {};
  for (const key of Object.keys(changed)) {
    const y = Number(key);
    if (!Number.isInteger(y) || y < 0 || y >= g.rows) continue;
    const row = changed[key] || [];
    for (let x = 0; x < g.cols; x++) g.cells[y][x] = row[x] || BLANK;
  }
  if (Array.isArray(frame.cursor)) g.cursor = [frame.cursor[0] | 0, frame.cursor[1] | 0];
  return g;
}

// gridToText is what the hidden mirror in the page shows. A canvas is opaque
// to a screen reader and to a test, and this is the fix for both.
export function gridToText(grid) {
  return grid.cells
    .map((row) => row.map((c) => (c && c[0]) || ' ').join('').replace(/\s+$/, ''))
    .join('\n');
}

// FONT is the face every grid draws in. ui-monospace does not resolve on
// most Linux browsers, so the list names the common Linux faces too.
export const FONT = 'ui-monospace, SFMono-Regular, Menlo, Consolas, "DejaVu Sans Mono", "Liberation Mono", monospace';

// measureAdvance is the glyph advance per pixel of font for FONT, read
// from a 2D context, or six tenths when the context cannot measure.
export function measureAdvance(ctx) {
  if (!ctx || typeof ctx.measureText !== 'function') return 0.6;
  ctx.font = '100px ' + FONT;
  const m = ctx.measureText('M');
  const w = m && Number(m.width);
  return w > 0 ? w / 100 : 0.6;
}

// cellOf reads the cell argument of drawGrid. A number is the old cell
// height, with the glyph six tenths of it wide, at one device pixel per
// CSS pixel. An object is a measured cell: w, h and font in CSS pixels,
// and dpr.
function cellOf(cell) {
  if (typeof cell === 'number') {
    const w = Math.max(4, Math.round(cell * 0.6));
    const h = Math.max(6, Math.round(cell));
    return { w, h, font: h - 2, dpr: 1 };
  }
  return { w: cell.w, h: cell.h, font: cell.font, dpr: cell.dpr > 0 ? cell.dpr : 1 };
}

// drawGrid paints every cell once. The canvas holds device pixels and is
// laid out in CSS pixels, so text is sharp at any pixel ratio. Each cell
// edge is rounded to a whole device pixel, so no gap shows between two
// cells. The canvas is resized only when its size changes. Bold and italic
// change the font, faint paints the text at a lower alpha, underline and
// strike each draw one line at a different height, inverse swaps paper and
// ink, and blink draws nothing extra: a flashing letter is hard to read.
export function drawGrid(ctx, grid, cell) {
  const c = cellOf(cell);
  const dw = c.w * c.dpr;
  const dh = c.h * c.dpr;
  const width = Math.round(grid.cols * dw);
  const height = Math.round(grid.rows * dh);
  const canvas = ctx.canvas;
  if (canvas.width !== width) canvas.width = width;
  if (canvas.height !== height) canvas.height = height;
  if (canvas.style) {
    const cssW = (width / c.dpr) + 'px';
    const cssH = (height / c.dpr) + 'px';
    if (canvas.style.width !== cssW) canvas.style.width = cssW;
    if (canvas.style.height !== cssH) canvas.style.height = cssH;
  }
  const px = Math.max(1, Math.round(c.font * c.dpr));
  const lift = Math.max(0, Math.floor((dh - px) / 2));
  const xs = Array.from({ length: grid.cols + 1 }, (_, x) => Math.round(x * dw));
  ctx.textBaseline = 'top';
  let font = '';
  for (let y = 0; y < grid.rows; y++) {
    const top = Math.round(y * dh);
    const bottom = Math.round((y + 1) * dh);
    const rowH = bottom - top;
    for (let x = 0; x < grid.cols; x++) {
      const [text, fg, bg, attrs] = grid.cells[y][x] || BLANK;
      const inverse = (attrs & ATTR.INVERSE) !== 0 || (grid.cursor[0] === x && grid.cursor[1] === y);
      const paper = inverse ? (fg || INK) : (bg || PAPER);
      const ink = inverse ? (bg || PAPER) : (fg || INK);
      const left = xs[x];
      const cellW = xs[x + 1] - left;
      ctx.globalAlpha = 1;
      ctx.fillStyle = paper;
      ctx.fillRect(left, top, cellW, rowH);
      if (text && text !== ' ') {
        ctx.fillStyle = ink;
        ctx.globalAlpha = (attrs & ATTR.FAINT) ? 0.6 : 1;
        const want = ((attrs & ATTR.BOLD) ? 'bold ' : '') + ((attrs & ATTR.ITALIC) ? 'italic ' : '') + px + 'px ' + FONT;
        if (want !== font) {
          ctx.font = want;
          font = want;
        }
        ctx.fillText(text, left, top + lift);
        ctx.globalAlpha = 1;
      }
      if (attrs & ATTR.UNDERLINE) {
        ctx.fillStyle = ink;
        ctx.fillRect(left, bottom - 1, cellW, 1);
      }
      if (attrs & ATTR.STRIKE) {
        ctx.fillStyle = ink;
        ctx.fillRect(left, top + Math.floor(rowH / 2), cellW, 1);
      }
    }
  }
}

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

// drawGrid paints every cell once. Bold and italic change the font, faint
// paints the text at a lower alpha, underline and strike each draw one line
// at a different height, inverse swaps paper and ink, and blink draws
// nothing extra: a phone screen is not the place for flashing text.
export function drawGrid(ctx, grid, cellPx) {
  const w = Math.max(4, Math.round(cellPx * 0.6));
  const h = Math.max(6, Math.round(cellPx));
  ctx.canvas.width = grid.cols * w;
  ctx.canvas.height = grid.rows * h;
  ctx.textBaseline = 'top';
  for (let y = 0; y < grid.rows; y++) {
    for (let x = 0; x < grid.cols; x++) {
      const [text, fg, bg, attrs] = grid.cells[y][x] || BLANK;
      const inverse = (attrs & ATTR.INVERSE) !== 0 || (grid.cursor[0] === x && grid.cursor[1] === y);
      const paper = inverse ? (fg || INK) : (bg || PAPER);
      const ink = inverse ? (bg || PAPER) : (fg || INK);
      ctx.globalAlpha = 1;
      ctx.fillStyle = paper;
      ctx.fillRect(x * w, y * h, w, h);
      if (text && text !== ' ') {
        ctx.fillStyle = ink;
        ctx.globalAlpha = (attrs & ATTR.FAINT) ? 0.6 : 1;
        ctx.font = ((attrs & ATTR.BOLD) ? 'bold ' : '') + ((attrs & ATTR.ITALIC) ? 'italic ' : '')
          + (h - 2) + 'px ui-monospace, Menlo, monospace';
        ctx.fillText(text, x * w, y * h);
        ctx.globalAlpha = 1;
      }
      if (attrs & ATTR.UNDERLINE) {
        ctx.fillStyle = ink;
        ctx.fillRect(x * w, y * h + h - 1, w, 1);
      }
      if (attrs & ATTR.STRIKE) {
        ctx.fillStyle = ink;
        ctx.fillRect(x * w, y * h + Math.floor(h / 2), w, 1);
      }
      // Blink draws nothing extra. A steady letter is easier to read here
      // than a flashing one, and a static image cannot flash anyway.
    }
  }
}

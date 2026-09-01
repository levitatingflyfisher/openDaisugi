// A picture of a pane screen, drawn from the lines the floor posts for a
// watched pane. It draws the text and nothing else: no colour and no
// cursor, since the post carries only the text of each row.

const PAPER = '#0b0f10';
const INK = '#e8eae7';
// GLYPH is a glyph's width as a share of the cell size.
const GLYPH = 0.6;

// cellPx is the largest cell size, never below 4, that fits cols by rows
// cells into w by h pixels.
export function cellPx(cols, rows, w, h) {
  if (!cols || !rows || cols < 1 || rows < 1) return 4;
  return Math.max(4, Math.floor(Math.min(h / rows, w / (cols * GLYPH))));
}

// drawPicture paints pic, { cols, rows, lines }, on canvas at the size the
// canvas has on the page. A blank line draws nothing, and no line past
// rows is drawn.
export function drawPicture(canvas, pic) {
  const w = canvas.clientWidth || 320;
  const h = canvas.clientHeight || 200;
  canvas.width = w;
  canvas.height = h;
  const ctx = canvas.getContext('2d');
  ctx.fillStyle = PAPER;
  ctx.fillRect(0, 0, w, h);
  if (!pic) return;
  const size = cellPx(pic.cols, pic.rows, w, h);
  ctx.fillStyle = INK;
  ctx.textBaseline = 'top';
  ctx.font = Math.max(4, size - 2) + 'px ui-monospace, Menlo, monospace';
  const lines = Array.isArray(pic.lines) ? pic.lines.slice(0, pic.rows) : [];
  lines.forEach((line, y) => {
    if (line) ctx.fillText(String(line), 0, y * size);
  });
}

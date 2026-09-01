import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { ATTR, newGrid, applyFrame, gridToText, drawGrid, measureAdvance } from '../grid.js';

// The seven bits come from PINS.md, which is read from vt.go by
// static_test.go. Reading the same line here means a bump to that row and a
// stale grid.js disagree in the same test run that catches the drift in Go.
const PINS = readFileSync(new URL('../../../../PINS.md', import.meta.url), 'utf8');
const BITS_LINE = 'bold 1, faint 2, italic 4, underline 8, blink 16, inverse 32, strike 64';

test('PINS.md still records the seven attrs bits this test reads', () => {
  assert.ok(PINS.includes(BITS_LINE), 'PINS.md attrs row changed; update this test and grid.js together');
});

test('the attrs bits match the PINS.md attribute row', () => {
  assert.equal(ATTR.BOLD, 1);
  assert.equal(ATTR.FAINT, 2);
  assert.equal(ATTR.ITALIC, 4);
  assert.equal(ATTR.UNDERLINE, 8);
  assert.equal(ATTR.BLINK, 16);
  assert.equal(ATTR.INVERSE, 32);
  assert.equal(ATTR.STRIKE, 64);
});

test('a first full frame fills the grid', () => {
  const frame = {
    pane: 'w1:p1', seq: 1, cols: 3, rows: 2, cursor: [1, 0],
    rows_changed: { 0: [['h', '', '', 0], ['i', '', '', 0], [' ', '', '', 0]], 1: [['o', '', '', 0], ['k', '', '', 0], [' ', '', '', 0]] },
  };
  const g = applyFrame(null, frame);
  assert.equal(gridToText(g), 'hi\nok');
  assert.deepEqual(g.cursor, [1, 0]);
});

test('a later frame changes only the rows it names', () => {
  let g = applyFrame(null, {
    cols: 2, rows: 2, cursor: [0, 0],
    rows_changed: { 0: [['a', '', '', 0], ['b', '', '', 0]], 1: [['c', '', '', 0], ['d', '', '', 0]] },
  });
  g = applyFrame(g, { cols: 2, rows: 2, cursor: [0, 1], rows_changed: { 1: [['x', '', '', 0], ['y', '', '', 0]] } });
  assert.equal(gridToText(g), 'ab\nxy');
});

// The failure this names: a frame that names a row outside the grid must not
// throw and blank the whole pane.
test('a row index outside the grid is dropped, not thrown', () => {
  const g = applyFrame(null, { cols: 2, rows: 1, cursor: [0, 0], rows_changed: { 0: [['a', '', '', 0], ['b', '', '', 0]], 9: [['z', '', '', 0]] } });
  assert.equal(gridToText(g), 'ab');
});

test('a resize starts a fresh grid rather than mixing two sizes', () => {
  let g = applyFrame(null, { cols: 2, rows: 1, cursor: [0, 0], rows_changed: { 0: [['a', '', '', 0], ['b', '', '', 0]] } });
  g = applyFrame(g, { cols: 4, rows: 1, cursor: [0, 0], rows_changed: { 0: [['w', '', '', 0], ['x', '', '', 0], ['y', '', '', 0], ['z', '', '', 0]] } });
  assert.equal(g.cols, 4);
  assert.equal(gridToText(g), 'wxyz');
});

// A minimal 2D context that records every call in order, so a test can read
// back what drawGrid painted without a real canvas. get/set track the
// property values in effect at the moment each draw call runs, the way a
// real context reads its own current fillStyle and globalAlpha at draw time.
function fakeCtx() {
  const calls = [];
  const props = { fillStyle: null, font: null, globalAlpha: 1, textBaseline: null };
  return {
    calls,
    canvas: { width: 0, height: 0 },
    get fillStyle() { return props.fillStyle; },
    set fillStyle(v) { props.fillStyle = v; },
    get font() { return props.font; },
    set font(v) { props.font = v; },
    get globalAlpha() { return props.globalAlpha; },
    set globalAlpha(v) { props.globalAlpha = v; },
    get textBaseline() { return props.textBaseline; },
    set textBaseline(v) { props.textBaseline = v; },
    fillRect(x, y, w, h) { calls.push({ op: 'fillRect', x, y, w, h, fillStyle: props.fillStyle, globalAlpha: props.globalAlpha }); },
    fillText(text, x, y) { calls.push({ op: 'fillText', text, x, y, fillStyle: props.fillStyle, font: props.font, globalAlpha: props.globalAlpha }); },
  };
}

function oneCellGrid(attrs) {
  return { cols: 1, rows: 1, cursor: [9, 9], cells: [[['A', '', '', attrs]]] };
}

test('a bold italic cell carries both weights in the font string', () => {
  const ctx = fakeCtx();
  drawGrid(ctx, oneCellGrid(ATTR.BOLD | ATTR.ITALIC), 10);
  const text = ctx.calls.find((c) => c.op === 'fillText');
  assert.match(text.font, /bold/);
  assert.match(text.font, /italic/);
});

test('a faint cell paints its text at a reduced alpha', () => {
  const ctx = fakeCtx();
  drawGrid(ctx, oneCellGrid(ATTR.FAINT), 10);
  const text = ctx.calls.find((c) => c.op === 'fillText');
  assert.ok(text.globalAlpha < 1, 'faint text should paint below full alpha');
  assert.ok(text.globalAlpha > 0, 'faint text should still be visible');
});

test('a blink cell paints exactly like a plain cell', () => {
  const plain = fakeCtx();
  drawGrid(plain, oneCellGrid(0), 10);
  const blink = fakeCtx();
  drawGrid(blink, oneCellGrid(ATTR.BLINK), 10);
  assert.deepEqual(blink.calls, plain.calls);
});

test('a strike cell draws a line through the middle, not at the bottom', () => {
  const ctx = fakeCtx();
  drawGrid(ctx, oneCellGrid(ATTR.STRIKE), 10);
  const lines = ctx.calls.filter((c) => c.op === 'fillRect' && c.y > 0);
  assert.equal(lines.length, 1);
  assert.ok(lines[0].y < 9, 'a strike line at the same row as underline is not a strike line');
});

test('an underline cell draws a line at the bottom of the cell', () => {
  const ctx = fakeCtx();
  drawGrid(ctx, oneCellGrid(ATTR.UNDERLINE), 10);
  const lines = ctx.calls.filter((c) => c.op === 'fillRect' && c.y > 0);
  assert.equal(lines.length, 1);
  assert.equal(lines[0].y, 9);
});

test('an inverse cell swaps paper and ink', () => {
  const ctx = fakeCtx();
  const grid = { cols: 1, rows: 1, cursor: [9, 9], cells: [[['A', '#111111', '#eeeeee', ATTR.INVERSE]]] };
  drawGrid(ctx, grid, 10);
  const bg = ctx.calls.find((c) => c.op === 'fillRect');
  const text = ctx.calls.find((c) => c.op === 'fillText');
  assert.equal(bg.fillStyle, '#111111');
  assert.equal(text.fillStyle, '#eeeeee');
});

// sizedCanvas counts every write to its width and height, since a write
// clears the canvas and costs a new buffer even when the size is the same.
function sizedCanvas() {
  const c = { writes: 0, style: {}, w: 0, h: 0 };
  Object.defineProperty(c, 'width', { get() { return c.w; }, set(v) { c.writes += 1; c.w = v; } });
  Object.defineProperty(c, 'height', { get() { return c.h; }, set(v) { c.writes += 1; c.h = v; } });
  return c;
}

test('a measured cell draws at the device pixel ratio, sized in CSS pixels', () => {
  const ctx = fakeCtx();
  ctx.canvas = sizedCanvas();
  const grid = { cols: 3, rows: 2, cursor: [9, 9], cells: [[['a', '', '', 0], ['b', '', '', 0], [' ', '', '', 0]], [[' ', '', '', 0], [' ', '', '', 0], [' ', '', '', 0]]] };
  drawGrid(ctx, grid, { w: 8, h: 16, font: 13, dpr: 2 });
  assert.equal(ctx.canvas.width, 48, 'backing store is device pixels');
  assert.equal(ctx.canvas.height, 64);
  assert.equal(ctx.canvas.style.width, '24px', 'the page lays it out in CSS pixels');
  assert.equal(ctx.canvas.style.height, '32px');
  const texts = ctx.calls.filter((c) => c.op === 'fillText');
  assert.equal(texts.length, 2);
  assert.match(texts[0].font, /^26px /, 'the font is scaled by the ratio too');
  assert.equal(texts[1].x, 16, 'the second column starts one device cell in');
});

test('a redraw at the same size does not reallocate the canvas', () => {
  const ctx = fakeCtx();
  ctx.canvas = sizedCanvas();
  const grid = oneCellGrid(0);
  drawGrid(ctx, grid, { w: 8, h: 16, font: 13, dpr: 1 });
  const first = ctx.canvas.writes;
  drawGrid(ctx, grid, { w: 8, h: 16, font: 13, dpr: 1 });
  assert.equal(ctx.canvas.writes, first);
  drawGrid(ctx, grid, { w: 9, h: 16, font: 14, dpr: 1 });
  assert.ok(ctx.canvas.writes > first, 'a new size does reallocate');
});

test('measureAdvance reads the glyph advance per pixel of font', () => {
  const ctx = { font: '', measureText() { return { width: 60.2 }; } };
  assert.ok(Math.abs(measureAdvance(ctx) - 0.602) < 1e-9);
  assert.equal(measureAdvance({ font: '', measureText() { return { width: 0 }; } }), 0.6);
  assert.equal(measureAdvance({}), 0.6, 'a context that cannot measure reads as the common advance');
  assert.equal(measureAdvance(null), 0.6);
});

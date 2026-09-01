import test from 'node:test';
import assert from 'node:assert/strict';
import { cellPx, drawPicture } from '../picture.js';

test('the cell size fits the whole screen of a pane into the box', () => {
  const px = cellPx(80, 24, 480, 300);
  assert.ok(80 * px * 0.6 <= 480 && 24 * px <= 300, 'size ' + px);
  assert.ok(80 * (px + 1) * 0.6 > 480 || 24 * (px + 1) > 300, 'size ' + px + ' is not the largest');
  assert.equal(cellPx(0, 0, 100, 100), 4);
  assert.equal(cellPx(500, 200, 10, 10), 4);
});

test('a picture draws each line once, at the cell size', () => {
  const texts = [];
  const ctx = new Proxy({ fillText: (t, x, y) => texts.push([t, y]) }, {
    get: (o, k) => (k in o ? o[k] : () => {}), set: () => true,
  });
  const canvas = { clientWidth: 480, clientHeight: 300, getContext: () => ctx };
  drawPicture(canvas, { cols: 80, rows: 2, lines: ['hi', '', 'extra'] });
  assert.deepEqual(texts.map((t) => t[0]), ['hi']);
  assert.equal(canvas.width, 480);
});

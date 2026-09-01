import test from 'node:test';
import assert from 'node:assert/strict';
import {
  MIN_CELL_W, cellFor, windowCapacity, windowShape, fillOrder, startSelect,
  endedLine, endedSlot, isEndedSlot, paneOfSlot, livePanes, phoneSize, scrollFollow,
} from '../windows.js';
import { routeKey } from '../keys.js';

// A monospace advance of six tenths of the font size, the common one.
const ADV = 0.6;

test('the readable cell is at least 7 px wide', () => {
  assert.equal(MIN_CELL_W, 7);
});

test('cellFor fits the columns into the width', () => {
  const c = cellFor(960, 120, ADV, 1);
  assert.equal(c.w, 8);
  assert.ok(Math.abs(c.font - 8 / ADV) < 1e-9);
  assert.ok(c.h >= c.font, 'a line is at least as tall as the font');
  assert.equal(c.dpr, 1);
});

test('cellFor never draws below the readable size', () => {
  const c = cellFor(400, 120, ADV, 1);
  assert.equal(c.w, MIN_CELL_W);
  assert.equal(cellFor(0, 80, ADV, 1).w >= MIN_CELL_W, true);
});

test('cellFor caps a wide window at a comfortable font', () => {
  const c = cellFor(4000, 80, ADV, 1);
  assert.ok(c.font <= 15 + 1e-9, 'font ' + c.font);
  assert.ok(c.w < 4000 / 80);
});

test('cellFor takes the measured advance, not a fixed ratio', () => {
  const narrow = cellFor(400, 120, 0.5, 1);
  const wide = cellFor(400, 120, 0.7, 1);
  assert.equal(narrow.w, MIN_CELL_W);
  assert.equal(wide.w, MIN_CELL_W);
  assert.ok(narrow.font > wide.font, 'a narrower face needs a bigger font for the same cell');
  assert.ok(Math.abs(narrow.font - 14) < 1e-9);
});

test('cellFor keeps the device pixel ratio and reads a bad one as 1', () => {
  assert.equal(cellFor(960, 120, ADV, 2).dpr, 2);
  assert.equal(cellFor(960, 120, ADV, 0).dpr, 1);
  assert.equal(cellFor(960, 120, ADV, undefined).dpr, 1);
  assert.equal(cellFor(960, 120, undefined, 1).w, 8, 'no measured advance reads as the common one');
});

test('windowCapacity counts windows by size, not a fixed three', () => {
  const small = windowCapacity(700, 500, ADV);
  assert.deepEqual([small.cols, small.rows], [1, 1]);
  // The windows area a 1440 by 900 page leaves beside the rail.
  const laptop = windowCapacity(1180, 815, ADV);
  assert.deepEqual([laptop.cols, laptop.rows], [2, 2]);
  const big = windowCapacity(2500, 1300, ADV);
  assert.ok(big.cols * big.rows > 4);
  assert.ok(big.cols * big.rows <= 9, 'number keys reach nine windows at most');
});

test('windowCapacity gives at least one window, even a cramped one', () => {
  const c = windowCapacity(100, 100, ADV);
  assert.deepEqual([c.cols, c.rows], [1, 1]);
});

test('windowShape uses the fewest columns, so each window is as wide as it can be', () => {
  const cap = { cols: 2, rows: 2 };
  assert.deepEqual(windowShape(0, cap), { cols: 1, rows: 1 });
  assert.deepEqual(windowShape(1, cap), { cols: 1, rows: 1 });
  assert.deepEqual(windowShape(2, cap), { cols: 1, rows: 2 });
  assert.deepEqual(windowShape(3, cap), { cols: 2, rows: 2 });
  assert.deepEqual(windowShape(4, cap), { cols: 2, rows: 2 });
  assert.deepEqual(windowShape(9, cap), { cols: 2, rows: 2 }, 'never past the capacity');
  assert.deepEqual(windowShape(2, { cols: 3, rows: 1 }), { cols: 2, rows: 1 });
});

test('fillOrder puts agents that need you first, then working, then the rest, in rail order', () => {
  const rows = [
    { id: 'i1', state: 'idle' },
    { id: 'w1', state: 'working' },
    { id: 'b1', state: 'blocked' },
    { id: 'u1', state: 'unknown' },
    { id: 'w2', state: 'working' },
    { id: 'b2', state: 'blocked' },
  ];
  assert.deepEqual(fillOrder(rows), ['b1', 'b2', 'w1', 'w2', 'i1', 'u1']);
  assert.deepEqual(fillOrder([]), []);
  assert.deepEqual(fillOrder(null), []);
});

test('startSelect picks the first window that needs you, else the first window, else the rail', () => {
  assert.equal(startSelect(['a', 'b', 'c'], new Set(['c'])), 2);
  assert.equal(startSelect(['a', 'b'], new Set()), 0);
  assert.equal(startSelect(['', 'b'], new Set()), 1);
  assert.equal(startSelect([endedSlot('x'), 'b'], new Set()), 1, 'an ended line is not a window to type in');
  assert.equal(startSelect([], new Set()), -1);
  assert.equal(startSelect(['', ''], new Set()), -1);
});

test('an ended slot is not a pane', () => {
  const s = endedSlot('w1:p2');
  assert.ok(isEndedSlot(s));
  assert.equal(paneOfSlot(s), 'w1:p2');
  assert.ok(!isEndedSlot('w1:p2'));
  assert.ok(!isEndedSlot(''));
  assert.equal(paneOfSlot(''), '');
  assert.equal(paneOfSlot('w1:p3'), 'w1:p3');
  assert.deepEqual(livePanes(['a', '', s, 'b']), ['a', 'b']);
});

test('endedLine says who ended and how', () => {
  assert.equal(endedLine('claude-trellis', 0), 'claude-trellis ended (exit 0)');
  assert.equal(endedLine('sh-a', 3), 'sh-a ended (exit 3)');
  assert.equal(endedLine('sh-a', null), 'sh-a ended');
  assert.equal(endedLine('', 1), 'agent ended (exit 1)');
});

const key = (k, mods = {}) => ({ key: k, ctrlKey: false, altKey: false, metaKey: false, shiftKey: false, ...mods });

test('in a window every key goes to its agent except the leave key', () => {
  assert.deepEqual(routeKey(key('a'), 'window'), { kind: 'send', bytes: 'a' });
  assert.deepEqual(routeKey(key('Enter'), 'window'), { kind: 'send', bytes: '\r' });
  assert.deepEqual(routeKey(key('1'), 'window'), { kind: 'send', bytes: '1' });
  assert.deepEqual(routeKey(key('n'), 'window'), { kind: 'send', bytes: 'n' });
  assert.deepEqual(routeKey(key('Escape'), 'window'), { kind: 'send', bytes: '\x1b' });
  assert.deepEqual(routeKey(key('c', { ctrlKey: true }), 'window'), { kind: 'send', bytes: '\x03' });
  assert.deepEqual(routeKey(key(' ', { ctrlKey: true }), 'window'), { kind: 'leave' });
  assert.equal(routeKey(key('v', { ctrlKey: true }), 'window'), null, 'a paste stays with the browser');
  assert.equal(routeKey(key('Shift'), 'window'), null);
});

test('after leave the rail has the keys', () => {
  assert.deepEqual(routeKey(key('ArrowDown'), 'rail'), { kind: 'move', by: 1 });
  assert.deepEqual(routeKey(key('j'), 'rail'), { kind: 'move', by: 1 });
  assert.deepEqual(routeKey(key('ArrowUp'), 'rail'), { kind: 'move', by: -1 });
  assert.deepEqual(routeKey(key('k'), 'rail'), { kind: 'move', by: -1 });
  assert.deepEqual(routeKey(key('Enter'), 'rail'), { kind: 'enter' });
  assert.deepEqual(routeKey(key(' ', { ctrlKey: true }), 'rail'), { kind: 'enter' });
  assert.deepEqual(routeKey(key('3'), 'rail'), { kind: 'put', window: 2 });
  assert.deepEqual(routeKey(key('9'), 'rail'), { kind: 'put', window: 8 });
  assert.equal(routeKey(key('0'), 'rail'), null);
  assert.deepEqual(routeKey(key('n'), 'rail'), { kind: 'deny' });
  assert.equal(routeKey(key('x'), 'rail'), null);
  assert.equal(routeKey(key('j', { ctrlKey: true }), 'rail'), null);
  assert.equal(routeKey(key('Tab'), 'rail'), null, 'tab keeps moving browser focus');
});

test('a phone is under 600 px wide, or a short page under 900 px wide', () => {
  assert.equal(phoneSize(390, 844), true);
  assert.equal(phoneSize(360, 780), true);
  assert.equal(phoneSize(599, 1200), true);
  assert.equal(phoneSize(844, 390), true, 'a phone on its side read as a tablet');
  assert.equal(phoneSize(899, 499), true);
  assert.equal(phoneSize(899, 500), false);
  assert.equal(phoneSize(600, 900), false);
  assert.equal(phoneSize(1440, 400), false, 'a short desktop window read as a phone');
  assert.equal(phoneSize(700), false, 'a page with no known height read as a phone');
  assert.equal(phoneSize(0), true);
});

// A view of a grid 120 by 60 at a 7 by 12 px cell in a body 350 by 480 px.
const follow = (over) => ({
  shownW: 350, shownH: 480, cols: 120, rows: 60, cellW: 7, cellH: 12,
  cursor: [0, 59], top: 0, left: 0, autoTop: undefined, lastX: undefined, ...over,
});

test('scrollFollow keeps the cursor row in sight while the reader has not scrolled away', () => {
  const r = scrollFollow(follow());
  assert.equal(r.top, 60 * 12 - 480);
  assert.equal(r.autoTop, r.top);
  const up = scrollFollow(follow({ top: 0, autoTop: 240 }));
  assert.equal(up.top, 0, 'a reader who scrolled up was pulled back down');
});

test('scrollFollow moves sideways when the cursor passes an edge, with a margin', () => {
  const right = scrollFollow(follow({ cursor: [70, 59], lastX: 0 }));
  assert.ok(right.left > 0);
  assert.ok(70 * 7 + 7 <= right.left + 350, 'the cursor is past the right edge');
  const back = scrollFollow(follow({ cursor: [2, 59], lastX: 70, left: right.left }));
  assert.equal(back.left, 0);
  const still = scrollFollow(follow({ cursor: [70, 59], lastX: 70, left: 5 }));
  assert.equal(still.left, 5, 'a cursor that did not move moved the view');
  assert.equal(still.lastX, 70);
});

test('scrollFollow leaves a grid that fits alone', () => {
  const r = scrollFollow(follow({ cols: 40, rows: 10, top: 0, left: 0, cursor: [39, 9] }));
  assert.equal(r.top, 0);
  assert.equal(r.left, 0);
});

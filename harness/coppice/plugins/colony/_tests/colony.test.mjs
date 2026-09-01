import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { place, inBox, bar, quietFor, foragerAt, stripPlace, fitLabel, isStrip, STRIP_H, RADIUS } from '../colony.js';

const PANES = [{ id: 'w1:p1' }, { id: 'w1:p2' }, { id: 'w2:p1' }];

test('placement is stable for the same ids, and stays on the field', () => {
  const a = place(PANES, 400, 300, 7);
  const b = place([...PANES].reverse(), 400, 300, 7);
  for (const p of a) {
    const q = b.find((x) => x.id === p.id);
    assert.deepEqual(q, p);
    assert.ok(p.x >= 0 && p.x <= 400 && p.y >= 0 && p.y <= 300);
  }
  // One pane more moves no pane already placed.
  const c = place([...PANES, { id: 'w3:p1' }], 400, 300, 7);
  for (const p of a) assert.deepEqual(c.find((x) => x.id === p.id), p);
  assert.notDeepEqual(place(PANES, 400, 300, 8), a);
});

test('a box around two of three positions selects those two, dragged either way', () => {
  const pos = [{ id: 'a', x: 10, y: 10 }, { id: 'b', x: 50, y: 40 }, { id: 'c', x: 200, y: 200 }];
  assert.deepEqual(inBox(pos, { x0: 0, y0: 0, x1: 60, y1: 60 }), ['a', 'b']);
  assert.deepEqual(inBox(pos, { x0: 60, y0: 60, x1: 0, y1: 0 }), ['a', 'b']);
  assert.deepEqual(inBox(pos, { x0: 300, y0: 300, x1: 400, y1: 400 }), []);
});

test('the bar is 0 at no quiet and 1 at an hour', () => {
  assert.equal(bar(0), 0);
  assert.equal(bar(3600), 1);
  assert.equal(bar(7200), 1);
  assert.equal(bar(1800), 0.5);
  assert.equal(bar(-5), 0);
  // With no fact there is no bar.
  assert.equal(bar(null), null);
});

test('quiet time counts from the newest state event the socket gave for the pane', () => {
  const events = [
    { event: 'state', pane: 'a', ts: 100 },
    { event: 'note', pane: 'a', ts: 150 },
    { event: 'state', pane: 'a', ts: 130 },
    { event: 'state', pane: 'b', ts: 140 },
  ];
  assert.equal(quietFor({ id: 'a', ts: 90 }, events, 200), 70);
  assert.equal(quietFor({ id: 'a', ts: 180 }, events, 200), 20);
  assert.equal(quietFor({ id: 'z' }, events, 200), null);
});

test('a click finds the forager under it', () => {
  const pos = [{ id: 'a', x: 10, y: 10 }, { id: 'b', x: 50, y: 40 }];
  assert.equal(foragerAt(pos, 12, 9), 'a');
  assert.equal(foragerAt(pos, 300, 300), '');
});

test('the colony only selects and opens: no prompt line, no move', () => {
  for (const f of ['../colony.js', '../colony-view.js', '../index.html']) {
    const src = readFileSync(new URL(f, import.meta.url), 'utf8');
    for (const word of ['task.move', 'send_text', 'pane.send', 'prompt', 'drop']) {
      assert.ok(!src.includes(word), f + ' uses ' + word);
    }
  }
});

test('colony.js stays under 200 lines and asks the server for nothing', () => {
  const src = readFileSync(new URL('../colony.js', import.meta.url), 'utf8');
  assert.ok(src.split('\n').length < 200);
  for (const f of ['../colony.js', '../colony-view.js']) {
    const s = readFileSync(new URL(f, import.meta.url), 'utf8');
    for (const word of ['localStorage', 'fetch(', 'WebSocket', 'coppice.token']) {
      assert.ok(!s.includes(word), f + ' uses ' + word);
    }
  }
});

// fakeCanvas records listeners and swallows every draw call.
function fakeCanvas(w, h) {
  const listeners = {};
  const ctx = new Proxy({}, { get: () => () => {}, set: () => true });
  return {
    clientWidth: w, clientHeight: h, width: 0, height: 0,
    getContext: () => ctx,
    getBoundingClientRect: () => ({ left: 0, top: 0 }),
    addEventListener: (type, fn) => { (listeners[type] ||= []).push(fn); },
    fire: (type, e) => { for (const fn of listeners[type] || []) fn(e); },
  };
}

test('a drag box posts the foragers inside it as the selection, and nothing else', async () => {
  const { mountColony } = await import('../colony-view.js');
  const panes = [{ id: 'a', state: 'working', ts: 10 }, { id: 'b', state: 'blocked' }, { id: 'c', state: 'idle' }];
  const sent = [];
  let onData = null;
  const view = {
    panes: () => panes, tasks: () => [], events: () => [], sel: () => [],
    onData: (fn) => { onData = fn; },
    setSel: (ids) => sent.push(['sel', ids]), openPane: (id) => sent.push(['open', id]),
  };
  const canvas = fakeCanvas(400, 300);
  mountColony({ textContent: '' }, canvas, view, () => 100);
  onData();
  canvas.fire('pointerdown', { clientX: 0, clientY: 0 });
  canvas.fire('pointermove', { clientX: 400, clientY: 300 });
  canvas.fire('pointerup', {});
  assert.deepEqual(sent, [['sel', ['a', 'b', 'c']]]);
  const at = place(panes, 400, 300, 1).find((p) => p.id === 'b');
  canvas.fire('dblclick', { clientX: at.x, clientY: at.y });
  assert.deepEqual(sent[1], ['open', 'b']);
});

test('a strip puts the foragers in one evenly spaced row, each with its own slot', () => {
  const pos = stripPlace(PANES, 300, 60);
  assert.deepEqual(pos.map((p) => p.id), PANES.map((p) => p.id));
  assert.deepEqual(pos.map((p) => p.x), [50, 150, 250]);
  assert.ok(pos.every((p) => p.y === pos[0].y && p.y > 0 && p.y < 60));
  assert.ok(pos.every((p) => p.slot === 100));
  assert.deepEqual(stripPlace([], 300, 60), []);
  // A 39 px strip holds the dot and its label line whole.
  const short = stripPlace(PANES, 300, 39)[0];
  assert.ok(short.y - RADIUS >= 0 && short.y + RADIUS + 4 + 11 <= 39, 'a short strip cut its labels');
});

test('a strip is a field no taller than STRIP_H', () => {
  assert.equal(isStrip(STRIP_H), true);
  assert.equal(isStrip(40), true);
  assert.equal(isStrip(STRIP_H + 1), false);
  assert.equal(isStrip(0), false, 'a field not laid out yet read as a strip');
});

test('a label longer than its slot ends in an ellipsis and fits', () => {
  const measure = (t) => t.length * 7;
  assert.equal(fitLabel('notes', 70, measure), 'notes');
  const cut = fitLabel('podcast-sync', 50, measure);
  assert.ok(cut.endsWith('…'));
  assert.ok(measure(cut) <= 50);
  assert.equal(fitLabel('abc', 3, measure), '');
});

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { dots, hit } from '../minimap.js';
import { layout } from '../../tree/graph.js';
import { STATE_COLORS } from '../../_lib/view.js';

const fixture = JSON.parse(readFileSync(new URL('../../../internal/web/static/_tests/fixtures/tree.json', import.meta.url), 'utf8'));

test('the minimap is one dot per node of the tree, in the node state colour', () => {
  const ds = dots(fixture.tasks, fixture.panes, 200, 80);
  const nodes = layout(fixture.tasks, fixture.panes).nodes;
  assert.equal(ds.length, nodes.length);
  const by = new Map(ds.map((d) => [d.id, d]));
  assert.equal(by.get('t1').color, STATE_COLORS.blocked);
  assert.equal(by.get('w2:p1').color, STATE_COLORS.working);
  assert.equal(by.get('w1:p2').color, STATE_COLORS.idle);
  // A task with no pane has no state, and so no colour.
  assert.equal(by.get('t3').color, null);
  for (const d of ds) {
    assert.ok(d.x >= 0 && d.x <= 200 && d.y >= 0 && d.y <= 80, d.id + ' is off the map');
  }
});

test('a click on a dot returns its id, and a click on nothing returns empty', () => {
  const ds = dots(fixture.tasks, fixture.panes, 200, 80);
  for (const d of ds) assert.equal(hit(ds, d.x + 1, d.y - 1), d.id);
  assert.equal(hit(ds, -50, -50), '');
});

// A dot and the ring that marks it as selected must fit on the map, or the
// map draws clipped circles at its edges.
test('every dot and its selection ring stay inside the map', () => {
  for (const [w, h] of [[200, 80], [240, 96], [120, 40]]) {
    const ds = dots(fixture.tasks, fixture.panes, w, h);
    for (const d of ds) {
      const reach = d.r + 3;
      assert.ok(d.x - reach >= -1e-9 && d.x + reach <= w + 1e-9, d.id + ' is clipped sideways at ' + w + 'x' + h);
      assert.ok(d.y - reach >= -1e-9 && d.y + reach <= h + 1e-9, d.id + ' is clipped at the top or foot at ' + w + 'x' + h);
    }
  }
  const flat = dots([], [{ id: 'a', state: 'idle' }, { id: 'b', state: 'idle' }, { id: 'c', state: 'idle' }], 240, 96);
  for (const d of flat) assert.ok(d.x - d.r - 3 >= 0 && d.x + d.r + 3 <= 240, d.id + ' is clipped');
});

test('an empty floor is an empty map', () => {
  assert.deepEqual(dots([], [], 200, 80), []);
  assert.equal(hit([], 1, 1), '');
});

test('minimap.js stays under 200 lines and asks the server for nothing', () => {
  const src = readFileSync(new URL('../minimap.js', import.meta.url), 'utf8');
  assert.ok(src.split('\n').length < 200);
  for (const f of ['../minimap.js', '../minimap-view.js']) {
    const s = readFileSync(new URL(f, import.meta.url), 'utf8');
    for (const word of ['localStorage', 'fetch(', 'WebSocket', 'coppice.token']) {
      assert.ok(!s.includes(word), f + ' uses ' + word);
    }
  }
});

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { layout, project, nodeAt, UNIT } from '../graph.js';

const fixture = JSON.parse(readFileSync(new URL('../../../internal/web/static/_tests/fixtures/tree.json', import.meta.url), 'utf8'));

const TEAM = {
  tasks: [
    { id: 'lead', label: 'lead', state: 'working', panes: [] },
    { id: 'a', label: 'a', parent: 'lead', state: 'working', panes: ['p1'] },
    { id: 'b', label: 'b', parent: 'lead', state: 'blocked', panes: ['p2'] },
  ],
  panes: [{ id: 'p1', state: 'working' }, { id: 'p2', state: 'blocked' }],
};

const byId = (lay) => new Map(lay.nodes.map((n) => [n.id, n]));

test('a team with two children lays out with the children below the parent', () => {
  const lay = layout(TEAM.tasks, TEAM.panes);
  const n = byId(lay);
  assert.ok(n.get('a').y > n.get('lead').y);
  assert.ok(n.get('b').y > n.get('lead').y);
  assert.ok(n.get('a').x < n.get('lead').x && n.get('lead').x < n.get('b').x, 'the parent sits between its children');
  assert.deepEqual(lay.edges.filter((e) => e[0] === 'lead').map((e) => e[1]).sort(), ['a', 'b']);
});

test('no two nodes overlap', () => {
  for (const { tasks, panes } of [TEAM, fixture]) {
    const lay = layout(tasks, panes);
    for (const a of lay.nodes) {
      for (const b of lay.nodes) {
        if (a === b || a.y !== b.y) continue;
        assert.ok(Math.abs(a.x - b.x) >= 1, a.id + ' and ' + b.id + ' overlap');
      }
    }
  }
});

test('every task and every pane is one node with the state the socket gave', () => {
  const lay = layout(fixture.tasks, fixture.panes);
  const n = byId(lay);
  assert.equal(lay.nodes.length, fixture.tasks.length + fixture.panes.length);
  assert.equal(n.get('t1').state, 'blocked');
  assert.equal(n.get('t1').kind, 'task');
  assert.equal(n.get('w2:p1').state, 'working');
  assert.equal(n.get('w2:p1').kind, 'pane');
  assert.equal(n.get('t3').state, '');
});

test('a pane in no task still has a node, and a cycle of tasks ends', () => {
  const lay = layout(
    [{ id: 'x', parent: 'y', panes: [] }, { id: 'y', parent: 'x', panes: [] }],
    [{ id: 'loose', state: 'idle' }],
  );
  assert.deepEqual(lay.nodes.map((n) => n.id).sort(), ['loose', 'x', 'y']);
});

test('a click finds the node under it after a pan', () => {
  const lay = layout(TEAM.tasks, TEAM.panes);
  const pan = { x: 30, y: 12 };
  const placed = project(lay, pan);
  const lead = placed.find((p) => p.id === 'lead');
  assert.equal(lead.px, 30 + byId(lay).get('lead').x * UNIT + UNIT / 2);
  assert.equal(nodeAt(placed, lead.px + 2, lead.py - 2), lead);
  assert.equal(nodeAt(placed, -500, -500), null);
});

test('the tree modules and the layout they use each stay under 200 lines', () => {
  for (const f of ['../graph.js', '../tree.js', '../../_lib/layout.js']) {
    const src = readFileSync(new URL(f, import.meta.url), 'utf8');
    assert.ok(src.split('\n').length < 200, f);
  }
});

test('the graph holds no token and asks the server for nothing', () => {
  for (const f of ['../graph.js', '../graph-view.js']) {
    const src = readFileSync(new URL(f, import.meta.url), 'utf8');
    for (const word of ['localStorage', 'fetch(', 'WebSocket', 'coppice.token']) {
      assert.ok(!src.includes(word), f + ' uses ' + word);
    }
  }
});

import test from 'node:test';
import assert from 'node:assert/strict';
import { forView, eventForView } from '../views.js';
import { byNeed } from '../../../../plugins/_lib/view.js';
import { focusPane } from '../../../../plugins/inbox/inbox.js';

// A view is given a held ask as working, so every view sorts, colours
// and counts it the way the rail does, with held still on it.

const HELD = { by: 'w1:p9', task: 't1' };
const PANES = [
  { id: 'a', state: 'blocked', held: HELD },
  { id: 'b', state: 'blocked' },
  { id: 'c', state: 'working' },
];

test('forView gives a held ask to a view as working and keeps held', () => {
  const out = forView(PANES);
  assert.deepEqual(out.map((p) => p.state), ['working', 'blocked', 'working']);
  assert.deepEqual(out[0].held, HELD);
  assert.equal(PANES[0].state, 'blocked', 'forView changed the list it was given');
  assert.deepEqual(byNeed(out).map((p) => p.id), ['b', 'a', 'c']);
  assert.equal(focusPane({ panes: ['a', 'b'] }, out), 'b');
});

test('eventForView does the same for a state event', () => {
  assert.equal(eventForView({ event: 'state', pane: 'a', state: 'blocked', held: HELD }).state, 'working');
  assert.equal(eventForView({ event: 'state', pane: 'b', state: 'blocked' }).state, 'blocked');
  assert.equal(eventForView({ event: 'note', pane: 'a', text: 'x' }).text, 'x');
});

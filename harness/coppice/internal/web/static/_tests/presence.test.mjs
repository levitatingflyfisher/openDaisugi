import test from 'node:test';
import assert from 'node:assert/strict';
import { reset, element } from './browser-stub.mjs';
import { mountFloor, lookingLine } from '../floor.js';
import { render, paneRows } from '../roster.js';

// A tile another person has open says who looks at it. The floor page's
// own name never shows: that person is the one reading the page.

function stub(panes, me) {
  reset();
  global.window.innerWidth = 1000;
  const root = element('screen-roster');
  root.append(element('roster'), element('roster-empty'));
  render(paneRows(panes, 1));
  const state = {
    panes, width: 1000, me,
    rpc: async () => ({}),
    api: async () => ({}),
    status: () => {},
  };
  return { doc: { querySelector: (s) => root.querySelector(s) }, root, state };
}

function p(id, looking) {
  return { id, label: id, state: 'working', harness: 'claude', cwd: '/w/' + id, ts: 1, looking };
}

test('lookingLine names everyone but the reader', () => {
  assert.equal(lookingLine(['alice'], ''), '· alice looking');
  assert.equal(lookingLine(['alice', 'bob'], ''), '· alice, bob looking');
  assert.equal(lookingLine(['alice', 'bob'], 'alice'), '· bob looking');
  assert.equal(lookingLine(['alice'], 'alice'), '');
  assert.equal(lookingLine([], 'alice'), '');
  assert.equal(lookingLine(undefined, ''), '');
  assert.equal(lookingLine(['alice', 7, ''], ''), '· alice looking');
});

test('a tile another person has open says who is looking', () => {
  const { doc, root, state } = stub([p('a', ['alice', 'bob']), p('b')], 'bob');
  mountFloor(root, state);
  const head = doc.querySelector('[data-tile="a"] .head').textContent;
  assert.ok(head.includes('· alice looking'), head);
  assert.ok(!head.includes('bob'), head);
  assert.ok(!doc.querySelector('[data-tile="b"] .head').textContent.includes('looking'));
});

test('a presence event redraws the tile header, and a state event keeps it', () => {
  const { doc, root, state } = stub([p('a'), p('b')], '');
  const floor = mountFloor(root, state);
  assert.ok(!doc.querySelector('[data-tile="a"] .head').textContent.includes('looking'));
  floor.onEvent({ event: 'presence', pane: 'a', looking: ['carol'] });
  assert.ok(doc.querySelector('[data-tile="a"] .head').textContent.includes('· carol looking'));
  floor.onEvent({ event: 'state', pane: 'a', state: 'idle', source: 'process', harness: 'claude', ts: 2 });
  assert.ok(doc.querySelector('[data-tile="a"] .head').textContent.includes('· carol looking'));
  floor.onEvent({ event: 'presence', pane: 'a', looking: [] });
  assert.ok(!doc.querySelector('[data-tile="a"] .head').textContent.includes('looking'));
});

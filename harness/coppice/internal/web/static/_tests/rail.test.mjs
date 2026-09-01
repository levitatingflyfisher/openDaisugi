import test from 'node:test';
import assert from 'node:assert/strict';
import {
  railEntries, railOrder, treeOrder, groupCounts, countParts, killKey, projectMenu, dropAction, recentLine, ROW_TYPE,
} from '../rail.js';
import { routeKey } from '../keys.js';

// The rail as a tree: tasks with their agents under them, projects when
// there are no tasks. These are the pure rules; roster.js draws them.

const r = (id, state, extra = {}) => ({ id, label: id, state, task: '', cwd: '/w/one', ...extra });

const kinds = (entries) => entries.map((e) => (e.type === 'group' ? 'g:' + e.label : e.row.id));

test('with no tasks and one project the rail is a flat list, blocked first', () => {
  const rows = [r('a', 'idle'), r('b', 'blocked'), r('c', 'working')];
  const entries = railEntries(rows, [], new Set());
  assert.deepEqual(kinds(entries), ['b', 'c', 'a']);
  assert.ok(entries.every((e) => e.depth === 0));
});

test('with no tasks the agents group by project, the groups by name', () => {
  const rows = [
    r('a', 'idle', { cwd: '/src/trellis' }),
    r('b', 'working', { cwd: '/src/glean' }),
    r('c', 'blocked', { cwd: '/src/trellis' }),
  ];
  const entries = railEntries(rows, [], new Set());
  assert.deepEqual(kinds(entries), ['g:glean', 'b', 'g:trellis', 'c', 'a']);
  const rowDepths = entries.filter((e) => e.type === 'row').map((e) => e.depth);
  assert.deepEqual(rowDepths, [1, 1, 1]);
  const trellis = entries.find((e) => e.label === 'trellis');
  assert.deepEqual(trellis.counts, { blocked: 1, working: 0, idle: 1, unknown: 0, done: 0 });
  assert.equal(trellis.key, 'dir:/src/trellis');
});

test('an agent with no directory groups under a plain name', () => {
  const entries = railEntries([r('a', 'idle', { cwd: '' }), r('b', 'idle', { cwd: '/x/y' })], [], new Set());
  assert.deepEqual(kinds(entries), ['g:y', 'b', 'g:no directory', 'a']);
});

const TASKS = [
  { id: 't1', label: 'review', parent: '' },
  { id: 't2', label: 'docs', parent: 't1' },
  { id: 't3', label: 'other', parent: '' },
];

test('tasks form the tree, agents under their task, loose agents by project after', () => {
  const rows = [
    r('a', 'idle', { task: 't2' }),
    r('b', 'working', { task: 't1' }),
    r('c', 'blocked', { task: 't2' }),
    r('d', 'idle', { cwd: '/src/glean' }),
  ];
  const entries = railEntries(rows, TASKS, new Set());
  assert.deepEqual(kinds(entries), ['g:review', 'b', 'g:docs', 'c', 'a', 'g:other', 'g:glean', 'd']);
  const depth = Object.fromEntries(entries.map((e) => [e.type === 'group' ? e.label : e.row.id, e.depth]));
  assert.deepEqual(depth, { review: 0, b: 1, docs: 1, c: 2, a: 2, other: 0, glean: 0, d: 1 });
  // A task counts its own agents and those of every task under it.
  const review = entries.find((e) => e.label === 'review');
  assert.equal(review.counts.blocked, 1);
  assert.equal(review.counts.working, 1);
  assert.equal(review.counts.idle, 1);
  assert.equal(review.total, 3);
  assert.equal(entries.find((e) => e.label === 'other').total, 0);
});

test('a folded group is one line with its counts, and hides what is under it', () => {
  const rows = [r('a', 'idle', { task: 't2' }), r('b', 'working', { task: 't1' }), r('d', 'idle', { task: 't3' })];
  const entries = railEntries(rows, TASKS, new Set(['task:t1']));
  assert.deepEqual(kinds(entries), ['g:review', 'g:other', 'd']);
  const review = entries.find((e) => e.label === 'review');
  assert.equal(review.folded, true);
  assert.equal(review.total, 2);
  assert.deepEqual(railOrder(entries), ['d']);
});

test('a task whose parent the list lacks sits at the top', () => {
  const entries = railEntries([r('a', 'idle', { task: 'x' })], [{ id: 'x', label: 'orphan', parent: 'gone' }], new Set());
  assert.deepEqual(kinds(entries), ['g:orphan', 'a']);
});

test('a loop in the parents still ends', () => {
  const loop = [{ id: 'p', label: 'p', parent: 'q' }, { id: 'q', label: 'q', parent: 'p' }];
  const entries = railEntries([r('a', 'idle', { task: 'p' })], loop, new Set());
  assert.ok(entries.length <= 3);
});

test('treeOrder is every agent in tree order, folded or not', () => {
  const rows = [r('a', 'idle', { task: 't2' }), r('b', 'working', { task: 't1' }), r('d', 'idle')];
  assert.deepEqual(treeOrder(rows, TASKS), ['b', 'a', 'd']);
});

test('groupCounts counts by state word, and an odd state counts as unknown', () => {
  assert.deepEqual(groupCounts([r('a', 'idle'), r('b', 'nonsense'), r('c', 'blocked')]),
    { blocked: 1, working: 0, idle: 1, unknown: 1, done: 0 });
});

test('countParts names each state that has an agent, asks first', () => {
  assert.deepEqual(countParts({ blocked: 2, working: 1, idle: 3, unknown: 0, done: 0 }), [
    { state: 'blocked', n: 2, text: '2 ask', words: '2 need you' },
    { state: 'working', n: 1, text: '1', words: '1 working' },
    { state: 'idle', n: 3, text: '3', words: '3 idle' },
  ]);
  assert.deepEqual(countParts({ blocked: 1, working: 0, idle: 0, unknown: 0, done: 0 })[0].words, '1 needs you');
  assert.deepEqual(countParts({ blocked: 0, working: 0, idle: 0, unknown: 0, done: 0 }), []);
});

test('the kill confirm: Enter stops, Esc keeps, other keys leave it', () => {
  assert.equal(killKey({ key: 'Enter' }), 'stop');
  assert.equal(killKey({ key: 'Escape' }), 'keep');
  assert.equal(killKey({ key: 'a' }), null);
  assert.equal(killKey({ key: 'Enter', ctrlKey: true }), null);
  assert.equal(killKey(null), null);
  assert.equal(killKey({ key: 'Enter', isComposing: true }), null, 'an input method took the Enter');
});

test('the project menu lists pinned projects first, each path once', () => {
  const list = [
    { path: '/r/one', name: 'one', pinned: false },
    { path: '/p/two', name: 'two', pinned: true },
    { path: '/r/one', name: 'one', pinned: false },
    { path: '', name: '', pinned: false },
    { path: '/p/three', name: '', pinned: true },
  ];
  assert.deepEqual(projectMenu(list), [
    { path: '/p/two', name: 'two', pinned: true },
    { path: '/p/three', name: 'three', pinned: true },
    { path: '/r/one', name: 'one', pinned: false },
  ]);
  assert.deepEqual(projectMenu(null), []);
  const many = Array.from({ length: 30 }, (_, i) => ({ path: '/d/' + i, name: String(i), pinned: false }));
  assert.equal(projectMenu(many).length, 12);
});

test('a drop on a window: a dragged header swaps, a dragged row puts its agent there', () => {
  assert.deepEqual(dropAction({ slot: 0, pane: '' }, 2), { kind: 'swap', from: 0, to: 2 });
  assert.equal(dropAction({ slot: 1, pane: '' }, 1), null, 'a header dropped on its own window does nothing');
  assert.deepEqual(dropAction({ slot: null, pane: 'w1:p3' }, 1), { kind: 'put', pane: 'w1:p3', window: 1 });
  assert.equal(dropAction({ slot: null, pane: '' }, 1), null, 'a drop of something else does nothing');
  assert.equal(dropAction(null, 1), null);
  assert.equal(typeof ROW_TYPE, 'string');
});

test('a Recent row says the project, how it ended, and how long ago', () => {
  assert.equal(recentLine({ cwd: '/src/trellis', exit_code: 3, ended_at: 1000 }, 1000 + 7200), 'trellis · exit 3 · 2h ago');
  assert.equal(recentLine({ cwd: '/src/trellis', exit_code: null, ended_at: 1000 }, 1030), 'trellis · 30s ago');
  assert.equal(recentLine({ cwd: '', exit_code: 0 }, 5), 'exit 0');
});

test('on the rail Delete and ctrl-w stop the selected agent, and F2 renames it', () => {
  const key = (k, mods = {}) => ({ key: k, ctrlKey: false, altKey: false, metaKey: false, shiftKey: false, ...mods });
  assert.deepEqual(routeKey(key('Delete'), 'rail'), { kind: 'kill' });
  assert.deepEqual(routeKey(key('w', { ctrlKey: true }), 'rail'), { kind: 'kill' });
  assert.deepEqual(routeKey(key('F2'), 'rail'), { kind: 'rename' });
  // In a window those keys belong to the agent.
  assert.deepEqual(routeKey(key('w', { ctrlKey: true }), 'window'), { kind: 'send', bytes: '\x17' });
  assert.deepEqual(routeKey(key('Delete'), 'window'), { kind: 'send', bytes: '\x1b[3~' });
});

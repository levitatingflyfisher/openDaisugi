import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { columns, COLUMNS, HINT } from '../kanban.js';

const PANES = [
  { id: 'p1', state: 'blocked' }, { id: 'p2', state: 'working' },
  { id: 'p3', state: 'done' }, { id: 'p4', state: 'idle' },
];

const where = (cols, id) => cols.find((c) => c.cards.some((k) => k.id === id)).name;

test('the columns are the four states and nothing else, in order', () => {
  assert.deepEqual(COLUMNS, ['inbox', 'working', 'needs you', 'done']);
  assert.deepEqual(columns([], []).map((c) => c.name), COLUMNS);
});

test('a task with a blocked pane lands in needs you', () => {
  const cols = columns([{ id: 't1', state: 'blocked', panes: ['p1', 'p2'] }], PANES);
  assert.equal(where(cols, 't1'), 'needs you');
});

test('a task with no panes lands in inbox', () => {
  const cols = columns([{ id: 't0', state: '', panes: [] }], PANES);
  assert.equal(where(cols, 't0'), 'inbox');
});

test('the server state places a task; its panes place it when the server gave none', () => {
  const tasks = [
    { id: 'a', state: 'working', panes: ['p2'] },
    { id: 'b', state: 'done', panes: ['p3'] },
    { id: 'c', state: 'idle', panes: ['p4'] },
    { id: 'd', panes: ['p3', 'p1'] },
    { id: 'e', state: 'odd', panes: ['p2'] },
  ];
  const cols = columns(tasks, PANES);
  assert.equal(where(cols, 'a'), 'working');
  assert.equal(where(cols, 'b'), 'done');
  assert.equal(where(cols, 'c'), 'working');
  assert.equal(where(cols, 'd'), 'needs you');
  assert.equal(where(cols, 'e'), 'working');
});

test('a card carries its label, its panes and its ahead count', () => {
  const cols = columns([{ id: 't1', label: 'auth', state: 'working', panes: ['p2', 'ghost'], ahead: 3 }], PANES);
  const card = cols[1].cards[0];
  assert.deepEqual(card, { id: 't1', label: 'auth', state: 'working', panes: ['p2'], ahead: 3 });
});

test('the kanban has no drag: the columns are states the server owns', () => {
  assert.equal(HINT, 'columns are states. prompt the pane to move it.');
  for (const f of ['../kanban.js', '../kanban-view.js', '../index.html']) {
    const src = readFileSync(new URL(f, import.meta.url), 'utf8');
    for (const word of ['drag', 'drop', 'task.move', 'send_text']) {
      assert.ok(!src.toLowerCase().includes(word), f + ' uses ' + word);
    }
  }
});

test('kanban.js stays under 200 lines and asks the server for nothing', () => {
  const src = readFileSync(new URL('../kanban.js', import.meta.url), 'utf8');
  assert.ok(src.split('\n').length < 200);
  for (const f of ['../kanban.js', '../kanban-view.js']) {
    const s = readFileSync(new URL(f, import.meta.url), 'utf8');
    for (const word of ['localStorage', 'fetch(', 'WebSocket', 'coppice.token']) {
      assert.ok(!s.includes(word), f + ' uses ' + word);
    }
  }
});

test('the board draws each card in its column, and a click sets the selection', async () => {
  const { reset, element } = await import('../../../internal/web/static/_tests/browser-stub.mjs');
  const { mountKanban } = await import('../kanban-view.js');
  reset();
  const sent = [];
  let onData = null;
  const view = {
    tasks: () => [{ id: 't1', label: 'auth', state: 'blocked', panes: ['p1'] }, { id: 't2', label: 'docs', panes: [] }],
    panes: () => PANES,
    sel: () => ['p1'],
    onData: (fn) => { onData = fn; },
    setSel: (ids) => sent.push(['sel', ids]),
    openPane: (id) => sent.push(['open', id]),
  };
  const board = element('kanban-board');
  mountKanban(element('kanban-status'), board, element('kanban-hint'), view);
  onData();
  assert.equal(board.children.length, 4);
  assert.match(board.children[0].textContent, /inbox · 1docs/);
  assert.match(board.children[2].textContent, /needs you · 1auth/);
  const card = board.children[2].children[1];
  assert.match(card.className, /sel/);
  card.dispatchEvent({ type: 'click' });
  card.dispatchEvent({ type: 'dblclick' });
  assert.deepEqual(sent, [['sel', ['p1']], ['open', 'p1']]);
  assert.equal(element('kanban-hint').textContent, HINT);
});

// A task whose state is unknown sits in working, and its card says the
// word, so the column title never stands for a state the card does not have.
test('a card in working whose state is not working says its state', async () => {
  const { reset, element } = await import('../../../internal/web/static/_tests/browser-stub.mjs');
  const { mountKanban } = await import('../kanban-view.js');
  reset();
  let onData = null;
  const view = {
    tasks: () => [{ id: 'u', label: 'odd', state: 'unknown', panes: [] }, { id: 'w', label: 'run', state: 'working', panes: [] }],
    panes: () => [], sel: () => [],
    onData: (fn) => { onData = fn; }, setSel: () => {}, openPane: () => {},
  };
  const board = element('kanban-board');
  mountKanban(element('kanban-status'), board, element('kanban-hint'), view);
  onData();
  const [odd, run] = board.children[1].children.slice(1);
  assert.match(odd.textContent, /unknown/);
  assert.doesNotMatch(run.textContent, /working/);
});

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { rows, jumpToNeed, focusPane, selectedRow } from '../inbox.js';

const PANES = [
  { id: 'p1', state: 'working' }, { id: 'p2', state: 'blocked' },
  { id: 'p3', state: 'idle' }, { id: 'p4', state: 'done' },
];
const TASKS = [
  { id: 'done', label: 'done', worktree: '/w/repo-done', ahead: 0, state: 'done', panes: ['p4'] },
  { id: 'work', label: 'work', worktree: '/w/repo-work', ahead: 2, state: 'working', panes: ['p1'] },
  { id: 'nowt', label: 'no worktree', state: 'blocked', panes: ['p2'] },
  { id: 'ask', label: 'ask', worktree: '/w/repo-ask', ahead: 5, state: 'blocked', panes: ['p3', 'p2'] },
];

test('rows are the tasks with a worktree, blocked first', () => {
  const r = rows(TASKS, PANES);
  assert.deepEqual(r.map((x) => x.id), ['ask', 'work', 'done']);
  assert.deepEqual(r[0], {
    id: 'ask', label: 'ask', worktree: 'repo-ask', ahead: 5, state: 'blocked', panes: ['p3', 'p2'],
  });
});

test('jumpToNeed is the index of the first row that needs you', () => {
  assert.equal(jumpToNeed(rows(TASKS, PANES)), 0);
  const quiet = rows(TASKS.filter((t) => t.state !== 'blocked'), PANES);
  assert.equal(jumpToNeed(quiet), -1);
  assert.equal(jumpToNeed([{ state: 'working' }, { state: 'blocked' }]), 1);
});

test('the live picture is the open row pane that needs a person most', () => {
  const r = rows(TASKS, PANES);
  assert.equal(focusPane(r[0], PANES), 'p2');
  assert.equal(focusPane({ panes: [] }, PANES), '');
  const closed = PANES.map((p) => (p.id === 'p2' ? { ...p, closed: true } : p));
  assert.equal(focusPane(r[0], closed), 'p3');
  assert.equal(focusPane({ panes: ['p2'] }, closed), '');
});

test('the selected row is the first that holds a selected pane', () => {
  const r = rows(TASKS, PANES);
  assert.equal(selectedRow(r, ['p1']), 1);
  assert.equal(selectedRow(r, ['ghost']), -1);
  assert.equal(selectedRow(r, []), -1);
});

test('the inbox shows ahead and no pull request: no view makes the server reach the network', () => {
  for (const row of rows(TASKS, PANES)) assert.ok(!('pr' in row));
  for (const f of ['../inbox.js', '../inbox-view.js']) {
    const s = readFileSync(new URL(f, import.meta.url), 'utf8');
    for (const word of ['pr_number', 'pull', 'gh ']) assert.ok(!s.includes(word), f + ' uses ' + word);
  }
});

test('inbox.js stays under 200 lines and asks the server for nothing', () => {
  const src = readFileSync(new URL('../inbox.js', import.meta.url), 'utf8');
  assert.ok(src.split('\n').length < 200);
  for (const f of ['../inbox.js', '../inbox-view.js']) {
    const s = readFileSync(new URL(f, import.meta.url), 'utf8');
    for (const word of ['localStorage', 'fetch(', 'WebSocket', 'coppice.token', 'send_text']) {
      assert.ok(!s.includes(word), f + ' uses ' + word);
    }
  }
});

test('the page selects through the floor, jumps with !, and watches the selected pane', async () => {
  const { reset, element } = await import('../../../internal/web/static/_tests/browser-stub.mjs');
  const { mountInbox } = await import('../inbox-view.js');
  reset();
  let sel = [];
  const sent = [];
  let onData = null;
  let onFrame = null;
  let onWatch = null;
  const view = {
    tasks: () => TASKS, panes: () => PANES, sel: () => sel,
    onData: (fn) => { onData = fn; }, onFrame: (fn) => { onFrame = fn; }, onWatch: (fn) => { onWatch = fn; },
    watch: (ids) => sent.push(['watch', ids]), setSel: (ids) => sent.push(['sel', ids]),
    openPane: (id) => sent.push(['open', id]),
  };
  const list = element('inbox-rows');
  const inbox = mountInbox(element('inbox-status'), list, element('inbox-live-head'), element('inbox-picture'), view);
  onData();
  assert.equal(list.children.length, 3);
  assert.deepEqual(sent, [], 'nothing selected, nothing watched');
  inbox.onKey({ key: '!' });
  assert.deepEqual(sent, [['sel', ['p3', 'p2']]]);
  // The floor sends the selection back, and the view watches the neediest pane.
  sel = ['p3', 'p2'];
  onData();
  assert.deepEqual(sent[1], ['watch', ['p2']]);
  assert.match(list.children[0].className, /sel/);
  onData();
  assert.equal(sent.length, 2, 'the same selection watched twice');
  const head = element('inbox-live-head');
  assert.equal(head.textContent, 'Waiting for the picture of p2.');
  onWatch({ live: ['p2'], refused: [] });
  assert.equal(head.textContent, 'live: p2');
  assert.doesNotThrow(() => onFrame({ pane: 'p2', cols: 2, rows: 1, lines: ['ok'] }));
  onWatch({ live: [], refused: ['p2'] });
  assert.equal(head.textContent, 'The floor could not show p2 live.');
  list.children[1].dispatchEvent({ type: 'dblclick' });
  assert.deepEqual(sent[2], ['open', 'p1']);
});

import test from 'node:test';
import assert from 'node:assert/strict';
import { reset, element } from './browser-stub.mjs';
import { render, paneRows } from '../roster.js';
import { titleFor, rowFromEvent } from '../floor.js';
import { needsYou, heldLine } from '../pane.js';
import { phoneQueue } from '../dock.js';

// A held ask is one a foreman hears first. It waits under working as
// waiting on the task's foreman, and it is not the operator's until the
// hold ends.

const HELD = { by: 'w1:p9', task: 't1', task_label: 'review-team', since: 40, until: 160 };

function p(id, state, held) {
  return {
    id, label: id, state, harness: 'claude', ts: 100,
    ask: state === 'blocked' ? { id: 'ask-' + id, summary: 'rm -rf build/', tier: 'undoable' } : null,
    held,
  };
}

const PANES = [p('a', 'blocked', HELD), p('b', 'blocked'), p('c', 'working')];

test('needsYou is a blocked pane whose ask no foreman holds', () => {
  assert.equal(needsYou(PANES[0]), false);
  assert.equal(needsYou(PANES[1]), true);
  assert.equal(needsYou(PANES[2]), false);
  assert.equal(needsYou(null), false);
});

test('the waiting line names the task and how long the ask has waited', () => {
  assert.equal(heldLine(HELD, 100), "waiting on review-team's foreman · 1m");
  assert.equal(heldLine({ task: 't7' }, 100), "waiting on t7's foreman");
});

test('a held pane is a working row with its waiting line, never an ask card', () => {
  reset();
  const root = element('screen-roster');
  root.append(element('queue-head'), element('roster'), element('roster-empty'));
  element('queue-head').append(element('queue-title'), element('queue-sub'));
  const rows = paneRows(PANES, 100);
  assert.equal(rows[0].state, 'working');
  assert.equal(rows[0].held, "waiting on review-team's foreman · 1m");
  render(rows);
  assert.equal(element('queue-title').textContent, '1 needs you');
  const cards = root.querySelectorAll('.ask').map((li) => li.dataset.row);
  assert.deepEqual(cards, ['b']);
  const held = root.querySelector('[data-row="a"]');
  assert.match(held.textContent, /waiting on review-team's foreman/);
});

test('the tab title and the phone queue count only what needs the operator', () => {
  assert.equal(titleFor(PANES), '1 · coppice');
  assert.deepEqual(phoneQueue(paneRows(PANES, 100)).map((r) => r.id), ['b']);
});

test('a state event carries the hold, and one with no hold ends it', () => {
  const held = rowFromEvent(PANES[1], { event: 'state', pane: 'b', state: 'blocked', ask: PANES[1].ask, held: HELD });
  assert.deepEqual(held.held, HELD);
  const free = rowFromEvent(held, { event: 'state', pane: 'b', state: 'blocked', ask: PANES[1].ask });
  assert.equal(free.held, null);
  assert.equal(needsYou(free), true);
});

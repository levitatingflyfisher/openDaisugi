import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { reset, element } from './browser-stub.mjs';
import { endedLine } from '../windows.js';
import { confirmText, clearAllText, BLOCKED_BY_GATE, BLOCKED_OWN_QUESTION, blockedLine, render, paneRows } from '../roster.js';
import { ENDED_CLEAR_MS, endedMessage } from '../floor.js';
import { recentLine } from '../rail.js';

// The TUI keeps its own copy of these words. Both sides hold theirs to
// testdata/words.json, so the owner reads the same line on each.
test('the shared words match the TUI', () => {
  const w = JSON.parse(readFileSync(new URL('../../../../testdata/words.json', import.meta.url), 'utf8'));
  const got = {
    ended: endedLine('fine', null),
    ended_exit_0: endedLine('fine', 0),
    ended_exit_3: endedLine('fine', 3),
    ended_killed: endedLine('fine', -1),
    kill_confirm: confirmText('fine'),
    clear_all_confirm_2: clearAllText(2),
    clear_all_confirm_1: clearAllText(1),
    blocked_by_gate: BLOCKED_BY_GATE,
    blocked_own_question: BLOCKED_OWN_QUESTION,
    ended_clear_ms: ENDED_CLEAR_MS,
  };
  const want = Object.fromEntries(Object.entries(w).filter(([k]) => !k.startsWith('_')));
  assert.deepEqual(got, want);
});

test('Recent says killed for a process a signal ended', () => {
  assert.equal(recentLine({ cwd: '/w/app', exit_code: -1 }, 0), 'app · killed');
  assert.equal(recentLine({ cwd: '/w/app', exit_code: 2 }, 0), 'app · exit 2');
});

test('only a block the gate found blames the gate', () => {
  assert.equal(blockedLine('gate'), BLOCKED_BY_GATE);
  for (const src of ['manifest', 'headless', 'process', '']) assert.equal(blockedLine(src), BLOCKED_OWN_QUESTION);
});

test('an ended line for a good exit clears after its time, a bad one stays', () => {
  assert.equal(endedMessage('w1:p1', 'fine', 0).clearAfter, ENDED_CLEAR_MS);
  assert.equal(endedMessage('w1:p1', 'fine').clearAfter, ENDED_CLEAR_MS);
  assert.equal(endedMessage('w1:p1', 'fine', 3).clearAfter, undefined);
  assert.equal(endedMessage('w1:p1', 'fine', -1).clearAfter, undefined);
});

test('a card for a block the screen found says the agent asks, not the gate', () => {
  reset();
  render(paneRows([{ id: 'm1', label: 'm1', state: 'blocked', source: 'manifest', harness: 'claude', cwd: '/w/m1', ts: 1 }], 10));
  const card = element('roster').querySelector('[data-pane="m1"]');
  assert.ok(card.textContent.includes(BLOCKED_OWN_QUESTION), card.textContent);
  assert.ok(!card.textContent.includes('gate'), card.textContent);
});

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { sortPanes, stateChip, ageLabel, paneRows, render, panesFrom } from '../roster.js';
import { reset, element } from './browser-stub.mjs';

const REPLY = JSON.parse(readFileSync(new URL('./fixtures/pane_list.json', import.meta.url), 'utf8'));
const PANES = REPLY.result.panes;
const NOW = 1757300060;

// These four run against a real server's literal pane.list reply. The shape
// is flat: state is a string on the row, not a nested PaneStateEvent. Reading
// the nested shape would make every pane unknown, put no blocked pane first,
// and leave askSummary empty, so the roster would look calm while the gate
// was holding an ask.
test('a real pane.list row reads as the state it says it is', () => {
  const rows = paneRows(PANES, NOW);
  const blocked = rows.find((r) => r.id === 'w1:p2');
  assert.equal(blocked.state, 'blocked');
  assert.equal(blocked.harness, 'codex');
  assert.equal(blocked.source, 'gate');
  assert.equal(blocked.label, 'db migration');
});

test('a real blocked row carries its ask summary to the card', () => {
  const blocked = paneRows(PANES, NOW).find((r) => r.id === 'w1:p2');
  assert.equal(blocked.askSummary, 'rm -rf build/');
  assert.notEqual(blocked.askSummary, '');
});

test('a real pane.list puts the blocked pane at the top', () => {
  const order = sortPanes(paneRows(PANES, NOW)).map((r) => r.id);
  assert.equal(order[0], 'w1:p2');
  assert.deepEqual(order, ['w1:p2', 'w1:p1', 'w1:p3']);
});

test('a row the state store has never seen shows unknown with no age', () => {
  const rows = paneRows(PANES, NOW);
  const fresh = rows.find((r) => r.id === 'w1:p3');
  assert.equal(fresh.state, 'unknown');
  assert.equal(fresh.age, null);              // no ts on the row, so no age is invented
  assert.equal(stateChip(fresh.state).word, 'unknown');
  const working = rows.find((r) => r.id === 'w1:p1');
  assert.equal(working.age, 60);              // NOW minus the row's ts
});

test('blocked panes come first and the rest keep their order', () => {
  const panes = [
    { id: 'a', state: 'working' },
    { id: 'b', state: 'idle' },
    { id: 'c', state: 'blocked' },
    { id: 'd', state: 'working' },
  ];
  assert.deepEqual(sortPanes(panes).map((p) => p.id), ['c', 'a', 'd', 'b']);
});

// Unknown is what the floor shows when it has no source, and it is never
// dressed up as idle.
test('an unknown or missing state reads as unknown, never idle', () => {
  assert.equal(stateChip('unknown').word, 'unknown');
  assert.equal(stateChip(undefined).word, 'unknown');
  assert.equal(stateChip('nonsense').word, 'unknown');
});

// Colour alone is not a signal. Every chip carries the word too.
test('every chip carries a word as well as a class', () => {
  for (const s of ['blocked', 'working', 'idle', 'done', 'unknown']) {
    const chip = stateChip(s);
    assert.equal(chip.word, s);
    assert.ok(chip.cls.includes('chip-' + s));
  }
});

test('ages read in the largest useful unit', () => {
  assert.equal(ageLabel(4), '4s');
  assert.equal(ageLabel(125), '2m');
  assert.equal(ageLabel(7300), '2h');
  assert.equal(ageLabel(200000), '2d');
  assert.equal(ageLabel(-5), '0s');
});

// An empty list says so. A household with no panes needs an instruction, not
// a blank screen with no other affordance.
test('render shows the empty banner with no cards, and hides it with cards blocked first', () => {
  reset();
  render([]);
  assert.equal(element('roster-empty').hidden, false);
  assert.equal(element('roster').children.length, 0);

  render(paneRows(PANES, NOW));
  assert.equal(element('roster-empty').hidden, true);
  assert.equal(element('roster').children.length, 3);
  assert.equal(element('roster').children[0].dataset.pane, 'w1:p2');
});

// The failure this names: an ok reply whose result is not an object is not
// "no panes yet". Reading it as one would paint an empty roster over a
// reply the phone cannot make sense of, the same mistake a refusal already
// avoids.
test('panesFrom reads a real result and rejects anything that is not one', () => {
  assert.deepEqual(panesFrom({ panes: PANES }), PANES);
  assert.deepEqual(panesFrom({ panes: [] }), []);
  assert.equal(panesFrom({}), null, 'a result with no panes array is not a real empty roster');
  assert.equal(panesFrom('oops'), null);
  assert.equal(panesFrom(null), null);
  assert.equal(panesFrom(undefined), null);
  assert.equal(panesFrom([]), null);
});

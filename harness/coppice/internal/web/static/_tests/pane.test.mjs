import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { attachCommand, typeCommand, enterCommand, keysCommand, KEYS, stateOf, askFor, isHeadless, typeFor, enterFor, HEADLESS_NOTE } from '../pane.js';

const PANES = JSON.parse(
  readFileSync(new URL('./fixtures/pane_list.json', import.meta.url), 'utf8'),
).result.panes;
const blockedRow = PANES.find((p) => p.id === 'w1:p2');
const workingRow = PANES.find((p) => p.id === 'w1:p1');

// One state event exactly as the server's stateEvent type puts it on the
// wire: the envelope key plus a flattened PaneStateEvent, same field names
// as a pane.list row.
const stateEvent = {
  event: 'state', v: 1, ts: 1757300030.0, session_id: 'd41c', harness_session_id: null,
  harness: 'codex', pane: 'w1:p2', state: 'blocked', source: 'gate',
  detail: 'verdict=deny clause=shell.deny[2]',
  ask: { id: 'toolu_01ABC', tool: 'Bash', summary: 'rm -rf build/', deadline: 1757300090.0 },
};

// The failure this names: reading pane.list as a nested PaneStateEvent makes
// stateOf return unknown for every pane, and the Allow and Deny box never
// appears on the pane the operator just tapped. A gate blocked is one shot,
// so there is no second event to rescue it.
test('stateOf reads a flat pane.list row', () => {
  const st = stateOf(blockedRow);
  assert.equal(st.state, 'blocked');
  assert.equal(st.source, 'gate');
  assert.equal(st.harness, 'codex');
  assert.equal(st.ts, 1757300030.0);
  assert.equal(st.ask.id, 'toolu_01ABC');
});

test('stateOf reads a state event the same way, with no wrapper', () => {
  const st = stateOf(stateEvent);
  assert.equal(st.state, 'blocked');
  assert.equal(st.ask.summary, 'rm -rf build/');
  assert.equal(st.harness, 'codex');
});

test('the Allow and Deny box opens on a real blocked row and carries the ask id', () => {
  const ask = askFor(stateOf(blockedRow));
  assert.equal(ask.visible, true);
  assert.equal(ask.summary, 'rm -rf build/');
  assert.equal(ask.askId, 'toolu_01ABC');
});

test('the Allow and Deny box stays shut on a pane that is not blocked', () => {
  assert.equal(askFor(stateOf(workingRow)).visible, false);
  assert.equal(askFor(stateOf(undefined)).visible, false);
  // Blocked with no ask on the row is still not something to answer.
  assert.equal(askFor(stateOf({ state: 'blocked' })).visible, false);
});


// The same pane is on the operator's terminal. The phone watches and
// zooms; it never reshapes.
test('attach never sends a size', () => {
  const req = attachCommand({ id: 'w1:p1' });
  assert.equal(req.cmd, 'pane.attach');
  assert.equal(req.pane, 'w1:p1');
  assert.equal('cols' in req, false);
  assert.equal('rows' in req, false);
});

// The phone's keyboard Enter types the text into the agent and never
// presses Enter, so it can never send half a thought. The Enter button
// types the text and presses Enter in one request, so the two can never
// arrive in the other order.
test('typing sends the text with no Enter', () => {
  assert.deepEqual(typeCommand({ id: 'w1:p1' }, 'run the tests'), { cmd: 'pane.send_text', pane: 'w1:p1', text: 'run the tests', enter: false });
});

test('the Enter button sends the text and the Enter in one request, and a bare Enter with no text', () => {
  assert.deepEqual(enterCommand({ id: 'w1:p1' }, 'yes'), { cmd: 'pane.send_text', pane: 'w1:p1', text: 'yes', enter: true });
  assert.deepEqual(enterCommand({ id: 'w1:p1' }, ''), { cmd: 'pane.send_text', pane: 'w1:p1', text: '', enter: true });
});

test('the key drawer sends Esc, Tab, the arrows, ctrl-c and Enter by the names the server knows', () => {
  assert.deepEqual(KEYS.map((k) => k.key), ['esc', 'tab', 'left', 'up', 'down', 'right', 'ctrl+c', 'enter']);
  for (const k of KEYS) assert.ok(k.label && k.title, 'a key has no label or title: ' + k.key);
  assert.deepEqual(keysCommand({ id: 'w1:p1' }, 'ctrl+c'), { cmd: 'pane.send_keys', pane: 'w1:p1', keys: ['ctrl+c'] });
});

// A headless agent reads whole messages through its adapter. Raw text or
// keys would reach its stdin, or be refused, and would skip the guard that
// typed text never answers an ask. So the bar sends it agent.prompt, on
// either Enter, and never with no text.
test('a headless agent gets agent.prompt from either Enter, and a pty agent gets typed text', () => {
  const pty = { id: 'w1:p1', kind: 'pty' };
  const hl = { id: 'w1:p2', kind: 'headless' };
  assert.equal(isHeadless(hl), true);
  assert.equal(isHeadless(pty), false);
  assert.equal(isHeadless(null), false);
  assert.deepEqual(typeFor(hl, 'tidy docs'), { cmd: 'agent.prompt', pane: 'w1:p2', text: 'tidy docs' });
  assert.deepEqual(enterFor(hl, 'tidy docs'), { cmd: 'agent.prompt', pane: 'w1:p2', text: 'tidy docs' });
  assert.equal(enterFor(hl, ''), null, 'an empty message went to a headless agent');
  assert.equal(typeFor(hl, ''), null);
  assert.deepEqual(typeFor(pty, 'ls'), typeCommand(pty, 'ls'));
  assert.deepEqual(enterFor(pty, ''), enterCommand(pty, ''));
  assert.equal(typeFor(pty, ''), null);
  assert.match(HEADLESS_NOTE, /whole messages/);
});

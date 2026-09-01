import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { attachCommand, promptCommand, steerCommand, keysCommand, KEYS, stateOf, askFor } from '../pane.js';

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

// agent.prompt means send text. A shell pane answers it too, so there is
// one command and no branch to get wrong.
test('prompt is agent.prompt whatever the pane is', () => {
  for (const pane of [{ id: 'w1:p1' }, { id: 'w1:p2', kind: 'headless' }, { id: 'w1:p3', kind: 'pty' }]) {
    const req = promptCommand(pane, 'run the tests');
    assert.equal(req.cmd, 'agent.prompt');
    assert.equal(req.pane, pane.id);
    assert.equal(req.text, 'run the tests');
  }
});

test('steer is raw text with no turn boundary', () => {
  const req = steerCommand({ id: 'w1:p1' }, 'stop, do the other thing');
  assert.equal(req.cmd, 'pane.send_text');
  assert.equal(req.pane, 'w1:p1');
  assert.equal(req.text, 'stop, do the other thing');
  assert.equal(req.enter, true);
});

test('the keys drawer sends the four names the backends map', () => {
  assert.deepEqual(KEYS, ['enter', 'esc', 'ctrl+c', 'tab']);
  assert.deepEqual(keysCommand({ id: 'w1:p1' }, 'ctrl+c'), { cmd: 'pane.send_keys', pane: 'w1:p1', keys: ['ctrl+c'] });
});

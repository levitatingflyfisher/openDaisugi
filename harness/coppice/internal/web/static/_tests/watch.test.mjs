import test from 'node:test';
import assert from 'node:assert/strict';
import { mountWatch } from '../watch.js';

const tick = () => new Promise((r) => setImmediate(r));
const settle = async () => { for (let i = 0; i < 8; i++) await tick(); };

// fakeRpc holds each call open until the test answers it.
function fakeRpc() {
  const log = [];
  const open = [];
  const rpc = (cmd, fields) => new Promise((resolve) => {
    log.push(cmd.replace('pane.', '') + ' ' + fields.pane + (fields.view_only ? ' vo' : ''));
    open.push(resolve);
  });
  const answer = () => { const r = open.shift(); if (r) r({}); };
  return { rpc, log, answer };
}

// The failure this names: a window takes pane b while the watch's attach
// for b still waits in the queue. Sending that view-only attach after the
// window's own attach would leave the window unable to type.
test('a released pane whose attach waits in the queue is never attached', async () => {
  const { rpc, log, answer } = fakeRpc();
  const w = mountWatch(rpc, () => {});
  w.set(['a', 'b']);
  await settle();
  assert.deepEqual(log, ['attach a vo']);
  const racing = w.release(['b']);
  assert.deepEqual(racing, [], 'b was never sent, so nothing races');
  answer();
  await settle();
  assert.deepEqual(log, ['attach a vo'], 'the watch sent a view-only attach for a released pane');
});

test('a released pane whose attach is on the wire is named, so the window attaches again after it', async () => {
  const { rpc, log, answer } = fakeRpc();
  const w = mountWatch(rpc, () => {});
  w.set(['a']);
  await settle();
  assert.deepEqual(log, ['attach a vo']);
  const racing = w.release(['a']);
  assert.deepEqual(racing, ['a']);
  let after = false;
  w.quiet().then(() => { after = true; });
  await settle();
  assert.equal(after, false, 'quiet settled before the attach on the wire was answered');
  answer();
  await settle();
  assert.equal(after, true);
});

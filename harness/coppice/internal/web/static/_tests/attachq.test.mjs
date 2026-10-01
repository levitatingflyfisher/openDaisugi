import test from 'node:test';
import assert from 'node:assert/strict';
import { createQueue } from '../attachq.js';

// The queue alone, with a fake socket whose answers the test gives by hand.

const settle = () => new Promise((r) => setImmediate(r));

function world(extra = {}) {
  const w = { sent: [], held: [], alive: true, open: true, ...extra };
  w.rpc = (cmd, fields) => new Promise((resolve, reject) => {
    const m = { cmd, ...fields, resolve, reject };
    w.sent.push(m);
    w.held.push(m);
  });
  w.q = createQueue(w.rpc, () => w.alive, () => w.open);
  w.ops = () => w.sent.map((m) => m.cmd + ':' + m.pane);
  w.answer = async (n = 1) => {
    for (let i = 0; i < n; i += 1) {
      const m = w.held.shift();
      if (m) m.resolve({ attached: true, fitted: true });
      await settle();
    }
  };
  return w;
}

test('the floor\'s detach waits behind an attach the sheet handed over, and a later reply is not the sheet\'s', async () => {
  const w = world();
  w.q.want('p', null, () => {}, () => {});
  w.q.handOver('p');
  const floor = w.q.run('p', () => w.rpc('pane.detach', { pane: 'p' }));
  await settle();
  assert.deepEqual(w.ops(), ['pane.attach:p'], 'the detach went before the attach was answered');
  await w.answer();
  assert.deepEqual(w.ops(), ['pane.attach:p', 'pane.detach:p']);
  await w.answer();
  await floor;
});

test('hand over, open again and leave: the first attach lands last and is still taken back', async () => {
  const w = world();
  w.q.want('p', null, () => {}, () => {});
  w.q.handOver('p');
  w.q.run('p', () => w.rpc('pane.detach', { pane: 'p' }));
  w.q.want('p', null, () => {}, () => {});
  w.q.leave('p');
  for (let i = 0; i < 5; i += 1) await w.answer();
  assert.equal(w.ops().pop(), 'pane.detach:p');
  assert.equal(w.ops()[0], 'pane.attach:p');
});

test('hand over and open again: the floor\'s detach is followed by the sheet\'s own attach', async () => {
  const w = world();
  w.q.want('p', null, () => {}, () => {});
  w.q.handOver('p');
  w.q.run('p', () => w.rpc('pane.detach', { pane: 'p' }));
  w.q.want('p', null, () => {}, () => {});
  for (let i = 0; i < 5; i += 1) await w.answer();
  assert.deepEqual(w.ops(), ['pane.attach:p', 'pane.detach:p', 'pane.attach:p']);
});

test('a timeout is taken back, a refusal and a closed socket at the start are not', async () => {
  const w = world();
  const errors = [];
  w.q.want('t', null, () => {}, (e) => errors.push(e));
  w.held.shift().reject(new Error('timeout'));
  await settle();
  await settle();
  assert.deepEqual(w.ops(), ['pane.attach:t', 'pane.detach:t'], 'a timed out attach may have landed');
  assert.equal(errors.length, 1);

  const r = world();
  r.q.want('r', null, () => {}, () => {});
  r.held.shift().reject(Object.assign(new Error('no'), { code: 'bad_request' }));
  await settle();
  await settle();
  assert.deepEqual(r.ops(), ['pane.attach:r'], 'a refused attach was detached');

  const c = world({ open: false });
  c.q.want('c', null, () => {}, () => {});
  c.held.shift().reject(new Error('not connected'));
  await settle();
  await settle();
  assert.deepEqual(c.ops(), ['pane.attach:c'], 'an attach that never left was detached');
});

test('a failure of an older wish does not call the newer sheet\'s onError, and the newer wish still attaches', async () => {
  const w = world();
  const older = [];
  const newer = [];
  w.q.want('p', null, () => {}, (e) => older.push(e));
  w.q.leave('p');
  w.q.want('p', null, () => {}, (e) => newer.push(e));
  w.held.shift().reject(new Error('timeout'));
  for (let i = 0; i < 5; i += 1) await w.answer();
  assert.deepEqual([older.length, newer.length], [0, 0]);
  assert.equal(w.ops().pop(), 'pane.attach:p', 'the newer wish was not attached');
});

test('once the socket is not the page\'s, nothing more is sent, the floor\'s work included', async () => {
  const w = world();
  w.q.want('p', null, () => {}, () => {});
  const floor = w.q.run('p', () => w.rpc('pane.detach', { pane: 'p' }));
  w.q.leave('p');
  w.alive = false;
  w.held.shift().reject(new Error('socket closed'));
  await settle();
  await settle();
  await floor;
  assert.deepEqual(w.ops(), ['pane.attach:p'], 'a step sent on a new socket');
});

test('a floor attach the sheet takes and leaves before it is answered is still detached', async () => {
  // The floor attaches p (J1, unanswered). The pane opens p: the floor
  // releases it with no request and the sheet wants it (J2). The operator
  // opens another agent before J1 answers, so the sheet leaves p (J3).
  const w = world();
  const floor = w.q.run('p', () => w.rpc('pane.attach', { pane: 'p' }));
  w.q.want('p', null, () => {}, () => {});
  w.q.leave('p');
  await settle();
  assert.deepEqual(w.ops(), ['pane.attach:p'], 'a request went before the floor\'s attach was answered');
  for (let i = 0; i < 5; i += 1) await w.answer();
  await floor;
  assert.equal(w.ops().pop(), 'pane.detach:p', 'the floor\'s attach was left with nobody to own it');
});

test('a hand over and back, left before the old attach answers, still sends a detach', async () => {
  const w = world();
  w.q.want('p', null, () => {}, () => {});
  w.q.handOver('p');
  w.q.want('p', null, () => {}, () => {});
  w.q.leave('p');
  for (let i = 0; i < 5; i += 1) await w.answer();
  assert.equal(w.ops().pop(), 'pane.detach:p');
});

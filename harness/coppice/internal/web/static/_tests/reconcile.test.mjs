import test from 'node:test';
import assert from 'node:assert/strict';
import { reconcile, changed } from '../reconcile.js';

// A parent that records every removal and insertion.
function parent(kids) {
  const log = [];
  const p = {
    children: kids.slice(),
    removeChild(c) { log.push('remove ' + c.id); p.children.splice(p.children.indexOf(c), 1); },
    insertBefore(n, at) {
      log.push('insert ' + n.id);
      const i = p.children.indexOf(n);
      if (i >= 0) p.children.splice(i, 1);
      const j = at ? p.children.indexOf(at) : p.children.length;
      p.children.splice(j, 0, n);
    },
    replaceChildren(...n) { log.push('replace'); p.children = n; },
  };
  return { p, log };
}
const n = (id) => ({ id });

test('reconcile leaves a node that stays where it is, and swaps only the others', () => {
  const [head, body, line] = [n('head'), n('body'), n('line')];
  const { p, log } = parent([head, body, line]);
  const head2 = n('head2');
  reconcile(p, [head2, body, null, line]);
  assert.deepEqual(p.children, [head2, body, line]);
  assert.ok(!log.includes('remove body') && !log.includes('insert body'), 'the body was moved: ' + log);
  assert.ok(!log.includes('remove line') && !log.includes('insert line'), 'the line was moved: ' + log);
});

test('reconcile with nothing new does nothing', () => {
  const kids = [n('a'), n('b')];
  const { p, log } = parent(kids);
  reconcile(p, kids);
  assert.deepEqual(log, []);
});

test('reconcile puts nodes in order and drops those no longer wanted', () => {
  const [a, b, c] = [n('a'), n('b'), n('c')];
  const { p } = parent([a, b, c]);
  reconcile(p, [c, a]);
  assert.deepEqual(p.children, [c, a]);
});

test('changed is true once for a signature, and again for another owner', () => {
  const slot = {};
  assert.equal(changed(slot, 'x', 1), true);
  assert.equal(changed(slot, 'x', 1), false);
  assert.equal(changed(slot, 'y', 1), true);
  assert.equal(changed(slot, 'y', 2), true);
});

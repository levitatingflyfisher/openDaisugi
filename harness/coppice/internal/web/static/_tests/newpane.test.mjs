import test from 'node:test';
import assert from 'node:assert/strict';
import { createCommand, recentCwds } from '../newpane.js';

test('a pty pane sends the command line as argv', () => {
  const req = createCommand({ cwd: '/repo', label: 'auth fix', kind: 'pty', command: 'claude --resume', harness: 'claude-code' });
  assert.deepEqual(req, {
    cmd: 'pane.create', cwd: '/repo', label: 'auth fix', kind: 'pty',
    env: {}, cmd_argv: ['claude', '--resume'],
  });
});

test('a headless pane sends the harness name instead of argv', () => {
  const req = createCommand({ cwd: '/repo', label: '', kind: 'headless', command: 'ignored', harness: 'codex' });
  assert.equal(req.kind, 'headless');
  assert.equal(req.harness, 'codex');
  assert.deepEqual(req.cmd_argv, []);
});

// The failure this names: a pane created in the wrong directory does real
// work in the wrong repository.
test('a blank directory is refused rather than defaulted', () => {
  assert.throws(() => createCommand({ cwd: '  ', kind: 'pty', command: 'claude' }), /directory/i);
});

// The failure this names: clearing the command box expecting Create to
// refuse instead starts a bare shell in the chosen repository.
test('a blank command is refused rather than defaulted to a shell', () => {
  assert.throws(() => createCommand({ cwd: '/repo', kind: 'pty', command: '   ' }), /command/i);
});

test('recent directories put the live panes first and keep ten', () => {
  const panes = [{ cwd: '/a' }, { cwd: '/b' }, { cwd: '/a' }];
  const stored = ['/b', '/c', '/d'];
  assert.deepEqual(recentCwds(panes, stored), ['/a', '/b', '/c', '/d']);
  const many = Array.from({ length: 30 }, (_, i) => '/dir' + i);
  assert.equal(recentCwds([], many).length, 10);
});

import test from 'node:test';
import assert from 'node:assert/strict';
import { SHELL, shouldCache } from '../sw-policy.js';

test('SHELL is the exact list the shell needs offline', () => {
  assert.deepEqual(SHELL, [
    '/', '/app.css', '/app.js', '/grid.js', '/chips.js', '/roster.js', '/pane.js',
    '/tiles.js', '/floor.js',
    '/newpane.js', '/settings.js', '/record.js', '/sw-policy.js', '/manifest.webmanifest',
    '/icons/icon-192.png', '/icons/icon-512.png',
  ]);
});

test('shouldCache is true for every path in SHELL', () => {
  for (const path of SHELL) {
    assert.equal(shouldCache(path), true, path);
  }
});

test('shouldCache is false for every API and websocket path', () => {
  for (const path of [
    '/api/panes', '/api/token/check', '/api/ask/answer', '/api/push/test',
    '/api/voice/transcribe', '/ws', '/sw.js',
  ]) {
    assert.equal(shouldCache(path), false, path);
  }
});

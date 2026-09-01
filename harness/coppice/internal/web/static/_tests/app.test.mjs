import test from 'node:test';
import assert from 'node:assert/strict';
import { bearerProtocols, route, nextRetry, WS_SUBPROTOCOL, WS_BEARER_PREFIX } from '../app.js';

// This import is itself a check. window and document are both absent
// under plain Node, and it still succeeds. If the module touched either at
// import time, node --test would never reach a single assertion below.

test('bearerProtocols offers the safe name first and the token in the second', () => {
  assert.deepEqual(bearerProtocols('tok123'), [WS_SUBPROTOCOL, WS_BEARER_PREFIX + 'tok123']);
});

test('route reads a pane id out of an encoded hash', () => {
  assert.deepEqual(route('#/pane/w1%3Ap1'), { screen: 'pane', pane: 'w1:p1' });
});

test('route falls back to the roster for an empty or unknown hash', () => {
  assert.deepEqual(route(''), { screen: 'roster' });
  assert.deepEqual(route('#/nowhere'), { screen: 'roster' });
});

test('route reads new and settings', () => {
  assert.deepEqual(route('#/new'), { screen: 'new' });
  assert.deepEqual(route('#/settings'), { screen: 'settings' });
});

test('nextRetry grows an upstream backoff from 2000 to a 30000 ceiling, then holds', () => {
  const seen = [];
  let retryMs = 2000;
  for (let i = 0; i < 6; i++) {
    const decision = nextRetry('upstream', retryMs);
    seen.push(decision.delayMs);
    retryMs = decision.retryMs;
  }
  assert.deepEqual(seen, [2000, 4000, 8000, 16000, 30000, 30000]);
});

test('nextRetry treats an unclassified close the same as an upstream one', () => {
  assert.deepEqual(nextRetry('unknown', 2000), { delayMs: 2000, retryMs: 4000 });
});

test('nextRetry stops on a rejected token', () => {
  assert.deepEqual(nextRetry('rejected', 8000), { stop: true });
});

test('nextRetry waits a fixed minute on a ban, and does not grow the backoff', () => {
  assert.deepEqual(nextRetry('banned', 16000), { delayMs: 60000, retryMs: 16000 });
});

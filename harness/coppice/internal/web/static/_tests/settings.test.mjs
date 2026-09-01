import test from 'node:test';
import assert from 'node:assert/strict';
import { wsUrl, tokenFromHash } from '../settings.js';

test('the websocket follows the page scheme', () => {
  assert.equal(wsUrl('https://box.tail1234.ts.net:8443'), 'wss://box.tail1234.ts.net:8443/ws');
  assert.equal(wsUrl('http://127.0.0.1:8443'), 'ws://127.0.0.1:8443/ws');
});

// The failure this names: storing junk from the hash would leave the app
// permanently unable to connect with no way to tell why.
test('only a well formed token is taken from the hash', () => {
  const good = 'A'.repeat(43);
  assert.equal(tokenFromHash('#t=' + good), good);
  assert.equal(tokenFromHash('#t=short'), null);
  assert.equal(tokenFromHash('#/roster'), null);
  assert.equal(tokenFromHash(''), null);
  assert.equal(tokenFromHash('#t=' + 'A'.repeat(42) + '='), null);
});

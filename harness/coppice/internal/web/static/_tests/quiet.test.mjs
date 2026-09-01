import test from 'node:test';
import assert from 'node:assert/strict';
import { reset, element } from './browser-stub.mjs';
import { render, paneRows, lastAskLine, askClock, ASK_OVERLAP } from '../roster.js';

// The quiet screen: with no pane blocked, home says so in words, with the
// working count and the age of the last ask. A span the server's event
// ring did not see reads as unknown, never as no ask.

function p(id, state, ask) {
  return { id, label: id, state, harness: 'claude', cwd: '/w/one', ts: 1, ask };
}

const NOW = 100000;

test('no blocked pane renders the quiet screen with the working count and no empty list', () => {
  reset();
  render(paneRows([p('a', 'working'), p('b', 'working'), p('c', 'idle')], 10));
  const quiet = element('quiet');
  assert.equal(quiet.hidden, false);
  assert.ok(String(quiet.className).split(/\s+/).includes('quiet'));
  assert.ok(quiet.textContent.includes('Nothing needs you.'));
  assert.ok(quiet.textContent.includes('2 working. I will ping when one asks.'));
  assert.ok(quiet.textContent.includes('last ask unknown'), 'an unread ring read as known');
  assert.equal(element('queue-head').hidden, true);
  assert.equal(element('roster').querySelectorAll('.ask').length, 0);
  assert.equal(element('roster').children.length, 3);
  assert.equal(element('roster').hidden, false);
});

test('with no panes at all the empty banner shows, and no quiet screen and no empty list', () => {
  reset();
  render([]);
  assert.equal(element('quiet').hidden, true);
  assert.equal(element('roster').hidden, true, 'an empty list shows');
  assert.equal(element('roster-empty').hidden, false);
});

test('a blocked pane hides the quiet screen', () => {
  reset();
  render(paneRows([p('a', 'working'), p('b', 'blocked', { id: 't', summary: 'ls' })], 10));
  assert.equal(element('quiet').hidden, true);
  assert.equal(element('queue-head').hidden, false);
});

test('the quiet screen carries the last ask line it is given', () => {
  reset();
  render(paneRows([p('a', 'working')], 10), { askLine: 'last ask 5m ago' });
  assert.ok(element('quiet').textContent.includes('last ask 5m ago'));
  assert.ok(element('quiet').textContent.includes('1 working.'));
});

test('lastAskLine names the age, a quiet span, or unknown', () => {
  assert.equal(lastAskLine({ known: true, last: NOW - 720, from: NOW - 7200 }, NOW), 'last ask 12m ago');
  assert.equal(lastAskLine({ known: true, last: null, from: NOW - 7200 }, NOW), 'no ask in the last 2h');
  assert.equal(lastAskLine({ known: false, last: null, from: null }, NOW), 'last ask unknown');
  assert.equal(lastAskLine({ known: true, last: null, from: null }, NOW), 'last ask unknown');
});

test('the ask clock reads the whole ring once, then from the newest less the overlap', async () => {
  const paths = [];
  const replies = [
    { events: [{ event: 'state', pane: 'a', state: 'blocked', ts: 50 }, { event: 'state', pane: 'a', state: 'working', ts: 60 }], from: 10 },
    { events: [{ event: 'state', pane: 'b', state: 'working', ts: 70 }], from: 10 },
  ];
  const clock = askClock(async (path) => { paths.push(path); return replies.shift(); });
  await clock.read();
  assert.deepEqual(clock.state(), { known: true, last: 50, from: 10 });
  await clock.read();
  assert.deepEqual(paths, ['/api/events?since=0', '/api/events?since=' + Math.max(0, 60 - ASK_OVERLAP)]);
  assert.deepEqual(clock.state(), { known: true, last: 50, from: 10 });
});

test('a refused ring reads as unknown, and a refused token stops the reads', async () => {
  let calls = 0;
  const clock = askClock(async () => {
    calls += 1;
    const err = new Error(calls === 1 ? 'The event ring is not running.' : 'Bad token.');
    err.status = calls === 1 ? 503 : 401;
    throw err;
  });
  await clock.read();
  assert.equal(clock.state().known, false);
  await clock.read();
  await clock.read();
  assert.equal(calls, 2, 'the clock kept asking with a refused token');
  assert.equal(clock.state().known, false);
});

test('on a phone the quiet screen reads the ring once with the token and shows the last ask', async () => {
  const { setFetch, TOKEN, freshApp } = await import('./browser-stub.mjs');
  reset();
  global.window.innerWidth = 390;
  global.localStorage.setItem('coppice.token', TOKEN);
  global.location.hash = '#/roster';
  const now = Date.now() / 1000;
  const seen = [];
  setFetch(async (path, init) => {
    seen.push({ path, auth: init && init.headers && init.headers.Authorization });
    const body = path === '/api/panes'
      ? { panes: [p('a', 'working')] }
      : { events: [{ event: 'state', pane: 'a', state: 'blocked', ts: now - 125 }], from: now - 3600 };
    return { ok: true, status: 200, json: async () => body };
  });
  await freshApp();
  for (let i = 0; i < 5; i++) await new Promise((r) => setImmediate(r));
  const reads = seen.filter((s) => s.path.startsWith('/api/events'));
  assert.equal(reads.length, 1);
  assert.equal(reads[0].path, '/api/events?since=0');
  assert.equal(reads[0].auth, 'Bearer ' + TOKEN);
  assert.ok(element('quiet').textContent.includes('last ask 2m ago'), element('quiet').textContent);
  assert.equal(element('screen-roster').hidden, false);
});

// After the ring restarts, from moves forward. An ask seen before that
// time cannot be the last ask: the gap may hold a newer one.
test('an ask older than the unbroken span is not called the last ask', () => {
  assert.equal(lastAskLine({ known: true, last: NOW - 2400, from: NOW - 600 }, NOW), 'no ask in the last 10m');
  assert.equal(lastAskLine({ known: true, last: NOW - 600, from: NOW - 600 }, NOW), 'last ask 10m ago');
});

// Each event carries its own reporter's time, so an event can land after
// one stamped later. The clock reads back over an overlap to see it.
test('a blocked event that lands after a newer stamped one still counts', async () => {
  const replies = [
    { events: [{ event: 'state', pane: 'b', state: 'working', ts: 1000 }], from: 10 },
    { events: [{ event: 'state', pane: 'b', state: 'working', ts: 1000 }, { event: 'state', pane: 'a', state: 'blocked', ts: 990 }], from: 10 },
  ];
  const paths = [];
  const clock = askClock(async (path) => { paths.push(path); return replies.shift(); });
  await clock.read();
  await clock.read();
  assert.equal(paths[1], '/api/events?since=' + (1000 - ASK_OVERLAP));
  assert.equal(clock.state().last, 990);
});

test('a clock stopped by a refused token starts again on restart', async () => {
  let calls = 0;
  const clock = askClock(async () => {
    calls += 1;
    if (calls === 1) { const e = new Error('Bad token.'); e.status = 401; throw e; }
    return { events: [], from: 5 };
  });
  await clock.read();
  await clock.read();
  assert.equal(calls, 1);
  clock.restart();
  await clock.read();
  assert.equal(calls, 2);
  assert.equal(clock.state().known, true);
});

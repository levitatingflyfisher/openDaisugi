import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { describe, poolText, answerText, seenText, paintMachines, mountMachines, NO_PROFILE, MACHINES_MS, KEY_NOT_SET } from '../machines.js';
import { reset, element } from './browser-stub.mjs';

const REPLY = {
  tailscale: 'ok',
  machines: [
    { name: 'box', online: true, os: 'linux', self: true, profiles: [] },
    { name: 'gpu', online: true, os: 'linux', self: false, profiles: [{ name: 'desk', address: 'gpu.tail1.ts.net:8000', answers: true, answers_ms: 12 }], voice: { address: 'gpu:9000', answers: false, answers_ms: null } },
    { name: 'nas', online: false, os: 'windows', self: false, last_seen: '2026-10-01T10:00:00Z', profiles: [{ name: 'dead', address: '127.0.0.5:9', answers: false, answers_ms: null, key_set: false }] },
  ],
};

test('a probe reads as its connect time, or as no answer', () => {
  assert.equal(answerText({ answers: true, answers_ms: 12 }), '12 ms');
  assert.equal(answerText({ answers: true, answers_ms: 0 }), 'under 1 ms');
  assert.equal(answerText({ answers: false, answers_ms: null }), 'No answer');
  assert.equal(answerText(null), 'No answer');
});

test('a machine that is offline shows when it was last seen; a bad time shows nothing', () => {
  assert.match(seenText('2026-10-01T10:00:00Z'), /^last seen 2026-10-0\d \d\d:\d\d$/);
  assert.equal(seenText('nonsense'), '');
  assert.equal(seenText(''), '');
});

test('describe lists every machine with its profiles and its voice', () => {
  const v = describe(REPLY);
  assert.equal(v.line, '');
  assert.deepEqual(v.rows.map((r) => [r.name, r.online, r.self]), [['box', true, true], ['gpu', true, false], ['nas', false, false]]);
  assert.deepEqual(v.rows[1].profiles.map((p) => [p.name, p.text, p.ok]), [['desk', '12 ms', true]]);
  assert.deepEqual([v.rows[1].voice.name, v.rows[1].voice.text], ['voice', 'No answer']);
  assert.match(v.rows[2].seen, /^last seen /);
  assert.equal(v.rows[1].seen, '');
});

test('describe lists a speech server under its machine', () => {
  const v = describe({ tailscale: 'ok', machines: [{ name: 'gpu', online: true, os: 'linux', profiles: [], speak: { address: 'gpu:8880', answers: true, answers_ms: 4 } }] });
  assert.deepEqual([v.rows[0].speak.name, v.rows[0].speak.address, v.rows[0].speak.ok], ['speech', 'gpu:8880', true]);
});

test('tailscale absent or off is one line with the fix, and no rows', () => {
  const line = 'Tailscale is not on this box. Install Tailscale, then open Machines again.';
  const v = describe({ tailscale: 'absent', machines: [], line });
  assert.equal(v.line, line);
  assert.deepEqual(v.rows, []);
  assert.match(describe(null).line, /^The machines are not known\. Reconnect to the server, then open Machines again\.$/);
});

test('a machine with no profile says so, and with unknown profiles it does not claim that', () => {
  const list = document.createElement('ul');
  paintMachines(list, describe(REPLY));
  const box = list.children[0];
  const none = box.children[1].children[0];
  assert.equal(none.textContent, NO_PROFILE);
  paintMachines(list, describe({ ...REPLY, profiles_line: 'The agent profiles are not known. Check that daisugi runs, then open Machines again.' }));
  assert.equal(list.children[0].children[1].children[0].textContent, 'Profiles not known.');
});

test('a profile whose key is not set says so; one that needs no key shows nothing extra', () => {
  const list = document.createElement('ul');
  paintMachines(list, describe(REPLY));
  const texts = (li) => JSON.stringify(li);
  assert.match(texts(list.children[2]), new RegExp(KEY_NOT_SET));
  assert.doesNotMatch(texts(list.children[1]), new RegExp(KEY_NOT_SET));
});

test('the panel has no control that changes anything', () => {
  const html = readFileSync(new URL('../index.html', import.meta.url), 'utf8');
  const panel = html.slice(html.indexOf('id="machines"'), html.indexOf('</section>', html.indexOf('id="machines"')));
  assert.deepEqual([...panel.matchAll(/<(button|input|select|form|textarea)\b/g)].map((m) => m[1]), ['button']);
  assert.match(panel, /id="machines-close"/);
});

function mount(rpc) {
  reset();
  const timers = [];
  const d = {
    el: element('machines'), list: document.createElement('ul'), line: document.createElement('p'), note: document.createElement('p'),
    close: element('machines-close'), rpc, connected: () => true, keys: () => 'rail',
    later: (fn, ms) => { timers.push({ fn, ms }); return timers.length - 1; }, cancel: (id) => { if (timers[id]) timers[id].fn = null; },
  };
  return { d, timers, up: mountMachines(d) };
}

test('opening the panel reads the verb, draws it, and reads again on a timer; closing stops it', async () => {
  const asked = [];
  const { d, timers, up } = mount(async (cmd) => { asked.push(cmd); return REPLY; });
  up.open();
  assert.equal(d.el.hidden, false);
  await up.read();
  assert.deepEqual(asked.slice(0, 1), ['floor.machines']);
  assert.equal(d.list.children.length, 3);
  assert.equal(d.line.hidden, true);
  assert.equal(timers[timers.length - 1].ms, MACHINES_MS);
  up.close();
  assert.equal(d.el.hidden, true);
  assert.equal(timers[timers.length - 1].fn, null);
});

test('a refusal or a dead socket says the machines are not known', async () => {
  const { d, up } = mount(async () => { throw new Error('closed'); });
  up.open();
  await up.read();
  assert.match(d.line.textContent, /Reconnect to the server/);
  assert.equal(d.line.hidden, false);
});

test('Esc closes the panel except while the keys are in a window; the x closes it', async () => {
  const { d, up } = mount(async () => REPLY);
  up.open();
  d.keys = () => 'window';
  document.dispatchEvent({ type: 'keydown', key: 'Escape' });
  assert.equal(up.isOpen(), true);
  d.keys = () => 'rail';
  document.dispatchEvent({ type: 'keydown', key: 'Escape' });
  assert.equal(up.isOpen(), false);
  up.open();
  d.close.dispatchEvent({ type: 'click' });
  assert.equal(up.isOpen(), false);
});

test('a worker pool entry reads with its slots and why it is cooling', () => {
  assert.equal(poolText({ pool: 'desk', name: 'a', slots: 2, running: 1, state: 'busy' }), 'desk/a: 1 of 2 slots, busy');
  assert.equal(
    poolText({ pool: 'desk', name: 'b', slots: 1, running: 0, state: 'cooling', until: '2026-10-24T12:15:00Z', reason: 'cannot connect' }),
    'desk/b: 0 of 1 slots, cooling until 12:15 UTC (cannot connect)',
  );
  const v = describe({ tailscale: 'off', line: 'x', machines: [], pools: [{ pool: 'p', name: 'e', slots: 1, running: 0, state: 'cooling', until: '2026-10-24T12:15:00Z' }] });
  assert.equal(v.pools.length, 1);
  assert.equal(v.pools[0].down, true);
  assert.equal(describe({ tailscale: 'ok', machines: [] }).pools.length, 0);
});

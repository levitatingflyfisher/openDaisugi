import test from 'node:test';
import assert from 'node:assert/strict';
import {
  segments, gateMark, light, details, fmtTokens, ago, factsItems, stackBarEl, gateMarkEls, detailsBox, liveCounts, mountFacts, paintHint,
} from '../stackbar.js';
import { needsYou } from '../pane.js';
import { reset } from './browser-stub.mjs';

const NOW = 1000;
const row = (extra) => ({
  id: 'w1:p1', state: 'working', harness: 'claude',
  stack: { loop: 'claude', model: 'claude-fake-2', router: 'gateway', daisugi: { mode: 'enforcing', armed: true } },
  ...extra,
});
const byKey = (segs) => Object.fromEntries(segs.map((s) => [s.key, s]));

test('a row with no stack draws no bar', () => {
  assert.deepEqual(segments({ id: 'a', state: 'idle' }, null, NOW), []);
  assert.deepEqual(segments(null, null, NOW), []);
});

test('the bar runs loop, daisugi, router, model, and the words are the choice', () => {
  const segs = segments(row(), { gateway: { url: 'http://127.0.0.1:8787', answers: true } }, NOW);
  assert.deepEqual(segs.map((s) => s.key), ['loop', 'daisugi', 'router', 'model']);
  // The daisugi step's place in the bar names it, so its word is the mode.
  assert.deepEqual(segs.map((s) => s.word), ['claude', 'enforcing', 'gateway', 'claude-fake-2']);
  assert.deepEqual(segs.map((s) => s.health), ['ok', 'ok', 'ok', 'ok']);
});

test('a field the server does not know is grey and says so, never guessed', () => {
  const s = byKey(segments(row({ stack: { loop: 'claude', daisugi: { mode: 'off', armed: true } } }), null, NOW));
  assert.equal(s.model.word, 'model ?');
  assert.equal(s.model.health, 'off');
  assert.equal(s.router.word, 'router ?');
  assert.equal(s.router.health, 'off');
  assert.equal(s.daisugi.word, 'off');
  assert.equal(s.daisugi.health, 'off');
});

test('watching is green, a disarmed gate and a codex gate are amber', () => {
  const watch = byKey(segments(row({ stack: { loop: 'claude', daisugi: { mode: 'watching', armed: true } } }), null, NOW));
  assert.equal(watch.daisugi.word, 'watching');
  assert.equal(watch.daisugi.health, 'ok');
  const off = byKey(segments(row({ stack: { loop: 'claude', daisugi: { mode: 'enforcing', armed: false } } }), null, NOW));
  assert.equal(off.daisugi.word, 'enforcing · disarmed');
  assert.equal(off.daisugi.health, 'warn');
  const codex = byKey(segments(row({ stack: { loop: 'codex', daisugi: { mode: 'enforcing', armed: true } } }), null, NOW));
  assert.equal(codex.daisugi.health, 'warn');
  assert.match(codex.daisugi.why, /fail open/);
});

// A deny is a verdict, not the gate's health: the gate that denied is
// working. The mark beside the bar shows the verdict.
test('a deny leaves daisugi green', () => {
  const fresh = row({ gate: { decision: 'deny', tool: 'Bash', clause: 'shell: curl', at: NOW - 5 } });
  assert.equal(byKey(segments(fresh, null, NOW)).daisugi.health, 'ok');
});

test('a gateway that does not answer turns the router red; direct stays green', () => {
  assert.equal(byKey(segments(row(), { gateway: { url: 'u', answers: false } }, NOW)).router.health, 'bad');
  assert.equal(byKey(segments(row({ stack: { loop: 'claude', router: 'switchyard' } }), { gateway: { url: 'u', answers: false } }, NOW)).router.health, 'bad');
  assert.equal(byKey(segments(row({ stack: { loop: 'claude', router: 'direct' } }), { gateway: { url: 'u', answers: false } }, NOW)).router.health, 'ok');
});

test('an agent that ended is grey all along its loop', () => {
  assert.equal(byKey(segments(row({ state: 'done' }), null, NOW)).loop.health, 'off');
});

test('the gate mark shows the last verdict with its tool and clause', () => {
  assert.equal(gateMark(null), null);
  assert.equal(gateMark({ decision: 'maybe' }), null);
  const d = gateMark({ decision: 'deny', tool: 'Bash', clause: 'shell: curl', at: 1 });
  assert.deepEqual([d.glyph, d.decision], ['✕', 'deny']);
  assert.equal(d.tip, 'Last gate verdict: deny Bash. Clause: shell: curl');
  assert.equal(gateMark({ decision: 'allow', tool: 'Read', clause: '' }).tip, 'Last gate verdict: allow Read');
  assert.equal(gateMark({ decision: 'allow' }).glyph, '✓');
  // An ask has its own glyph, apart from the ? of a field not known.
  assert.equal(gateMark({ decision: 'ask', tool: 'Bash' }).glyph, '⧗');
});

test('the light sits on the loop while working and on daisugi while an ask waits for you', () => {
  assert.equal(light(row({ state: 'working' })), 'loop');
  assert.equal(light(row({ state: 'blocked', ask: { id: 'x' } })), 'daisugi');
  assert.equal(light(row({ state: 'blocked', ask: { id: 'x' }, held: { by: 'f' } })), 'loop');
  assert.equal(light(row({ state: 'idle' })), '');
  assert.equal(light({ state: 'working' }), '', 'no bar, no light');
});

test('details say what is known, and say swap is not built', () => {
  const r = row({ gate: { decision: 'deny', tool: 'Bash', clause: 'shell: curl', at: NOW - 30 }, tokens: { fresh: 13, cache_read: 3000, cache_write: 20, out: 1200 } });
  const d = details('daisugi', r, null, NOW);
  assert.deepEqual(d.lines, [['mode', 'enforcing'], ['armed', 'yes'], ['last verdict', 'deny Bash'], ['clause', 'shell: curl'], ['when', '30 s ago']]);
  assert.match(d.note, /not built/);
  const m = details('model', r, null, NOW);
  assert.deepEqual(m.lines, [['model', 'claude-fake-2'], ['fresh in', '13'], ['cache read', '3k'], ['cache write', '20'], ['out', '1.2k']]);
  assert.equal(m.note, 'Swap is not built yet.');
  const g = details('router', r, { gateway: { url: 'http://127.0.0.1:8787', answers: true } }, NOW);
  assert.deepEqual(g.lines, [['route', 'gateway'], ['gateway', 'http://127.0.0.1:8787'], ['answers', 'yes']]);
  assert.equal(details('loop', r, null, NOW).note, 'Swap is not built yet.');
  const none = details('model', row({ stack: { loop: 'claude' } }), null, NOW);
  assert.deepEqual(none.lines, [['model', 'not reported yet'], ['tokens', 'not reported yet']]);
  const noGate = details('daisugi', row({ stack: { loop: 'claude' } }), null, NOW);
  assert.deepEqual(noGate.lines, [['mode', 'not known'], ['last verdict', 'none yet']]);
});

test('token counts and ages read short', () => {
  assert.equal(fmtTokens(0), '0');
  assert.equal(fmtTokens(999), '999');
  assert.equal(fmtTokens(1000), '1k');
  assert.equal(fmtTokens(12345), '12.3k');
  assert.equal(fmtTokens(2500000), '2.5M');
  assert.equal(fmtTokens(-1), '0');
  assert.equal(ago(3), 'just now');
  assert.equal(ago(45), '45 s ago');
  assert.equal(ago(125), '2 min ago');
  assert.equal(ago(7300), '2 h ago');
});

test('the header row reads the floor facts, with colour for health only', () => {
  const items = factsItems({
    daisugi: { mode: 'watching', armed: true, enforcing: 1, watching: 2, off: 0 },
    working: 2, needing_you: 1, tokens_today: { fresh: 1000, cache_read: 1200000, cache_write: 3000, out: 5000 },
    gateway: { url: 'http://127.0.0.1:8787', answers: true },
  });
  assert.deepEqual(items.map((i) => [i.key, i.text, i.health]), [
    ['daisugi', 'daisugi · 1 enforcing · 2 watching', 'ok'],
    ['gateway', 'gateway answers', 'ok'],
    ['working', '2 working', 'ok'],
    ['needing', '1 needs you', 'warn'],
    ['tokens', 'tokens today 1.2M', 'plain'],
  ]);
  assert.equal(items[0].title, 'Agents by mode: enforcing 1, watching 2, off 0');
  assert.match(items[4].title, /cache read 1.2M/);
});

test('the header row says what it does not know', () => {
  const items = factsItems({ daisugi: { mode: 'enforcing', armed: false, enforcing: 2 }, working: 0, needing_you: 2, tokens_today: {} });
  const k = Object.fromEntries(items.map((i) => [i.key, i]));
  assert.equal(k.daisugi.text, 'daisugi · 2 enforcing · disarmed');
  assert.equal(k.daisugi.health, 'warn');
  const some = factsItems({ daisugi: { mode: 'off', armed: true, enforcing: 2, watching: 0, off: 1 } })[0];
  assert.deepEqual([some.text, some.health], ['daisugi · 2 enforcing · 1 off', 'warn']);
  const none = factsItems({ daisugi: { mode: 'off', armed: true, enforcing: 0, watching: 0, off: 0 } })[0];
  assert.deepEqual([none.text, none.health], ['daisugi · no agents', 'off']);
  const allOff = factsItems({ daisugi: { mode: 'off', armed: true, off: 3 } })[0];
  assert.deepEqual([allOff.text, allOff.health], ['daisugi · 3 off', 'off']);
  assert.equal(k.gateway.text, 'no gateway');
  assert.equal(k.gateway.health, 'off');
  assert.equal(k.working.health, 'off');
  assert.equal(k.needing.text, '2 need you');
  assert.equal(k.tokens.text, 'tokens today 0');
  const silent = factsItems({ daisugi: { mode: 'off', armed: true }, gateway: { url: 'http://x:1', answers: false } });
  assert.equal(silent.find((i) => i.key === 'gateway').text, 'gateway silent');
  assert.equal(silent.find((i) => i.key === 'gateway').health, 'bad');
  const unknown = factsItems({ daisugi: { mode: 'off', armed: true }, gateway: { url: 'http://box:1' } });
  assert.equal(unknown.find((i) => i.key === 'gateway').text, 'gateway ?');
  assert.deepEqual(factsItems(null), []);
  assert.deepEqual(factsItems({}), []);
});

test('the stack bar, the gate mark and the details build the same for a window and the phone', () => {
  reset();
  const r = row({ gate: { decision: 'deny', tool: 'Bash', clause: 'shell: curl', at: NOW - 5 } });
  const segs = segments(r, null);
  const opened = [];
  const bar = stackBarEl(segs, { open: 'daisugi', lit: 'loop', onSeg: (k) => opened.push(k) });
  const buttons = bar.querySelectorAll('button');
  assert.deepEqual(buttons.map((b) => b.textContent), segs.map((s) => s.word));
  assert.match(buttons[1].className, /\bopen\b/);
  assert.match(buttons[0].className, /\blit\b/);
  let stopped = false;
  buttons[3].dispatchEvent({ type: 'click', stopPropagation: () => { stopped = true; } });
  assert.deepEqual(opened, ['model']);
  assert.ok(stopped, 'a segment click reached the header under it');
  const clicks = [];
  const [mark, tip] = gateMarkEls(gateMark(r.gate), () => clicks.push('mark'));
  assert.equal(mark.textContent, '✕');
  assert.match(tip.textContent, /Bash/);
  mark.dispatchEvent({ type: 'click' });
  assert.deepEqual(clicks, ['mark']);
  const box = detailsBox(details('daisugi', r, null, NOW), { onJournal: () => clicks.push('journal'), onClose: () => clicks.push('close') });
  const [journal, close] = box.querySelectorAll('button');
  assert.equal(journal.textContent, 'Journal');
  journal.dispatchEvent({ type: 'click' });
  close.dispatchEvent({ type: 'click' });
  assert.deepEqual(clicks, ['mark', 'journal', 'close']);
  const plain = detailsBox(details('model', r, null, NOW), { onClose: () => {} });
  assert.equal(plain.querySelectorAll('button').length, 1, 'the model details grew a Journal button');
});

test('the header counts needs you from the same rows as the rail', () => {
  const rows = [
    { id: 'w1:p1', state: 'blocked', held: { hold: 'h1', task: 't1' } },
    { id: 'w1:p2', state: 'blocked', source: 'manifest' },
    { id: 'w1:p3', state: 'blocked', source: 'gate', parent_pane: 'w1:p1' },
    { id: 'w1:p4', state: 'working' },
    { id: 'w1:p5', state: 'idle' },
  ];
  assert.equal(liveCounts(rows).needing, rows.filter(needsYou).length);
  assert.deepEqual(liveCounts(rows), { working: 2, needing: 2 });
  // The rows win over a facts reply that is a beat old.
  const k = Object.fromEntries(factsItems({ daisugi: { mode: 'off' }, working: 0, needing_you: 3 }, rows).map((i) => [i.key, i.text]));
  assert.equal(k.needing, '2 need you');
  assert.equal(k.working, '2 working');
});

test('an agent no gate hook reports for names the command that installs one', () => {
  const off = details('daisugi', row({ stack: { loop: 'claude', daisugi: { mode: 'off', armed: true } } }), null, NOW);
  assert.match(off.note, /daisugi install --gate/);
  assert.match(off.note, /on the computer that runs coppice/i);
  const on = details('daisugi', row(), null, NOW);
  assert.doesNotMatch(on.note, /install/);
});

test('an empty floor with no gate hook shows the hint, with a button that types the command', () => {
  const none = factsItems({ daisugi: { mode: 'off', armed: true, enforcing: 0, watching: 0, off: 0, installed: false } });
  const hint = none.find((it) => it.key === 'gate-hint');
  assert.ok(hint, 'no hint item');
  assert.equal(hint.text, 'daisugi is off: no agent is guarded. Run: daisugi install --gate');
  assert.deepEqual([hint.action.kind, hint.action.command], ['shell', 'daisugi install --gate']);
  // Installed, or an agent guarded, or a server that does not say: no hint.
  for (const d of [
    { mode: 'off', armed: true, enforcing: 0, watching: 0, off: 0, installed: true },
    { mode: 'watching', armed: true, enforcing: 0, watching: 1, off: 0, installed: false },
    { mode: 'off', armed: true, enforcing: 0, watching: 0, off: 0 },
  ]) {
    assert.equal(factsItems({ daisugi: d }).some((it) => it.key === 'gate-hint'), false, JSON.stringify(d));
  }
});

test('mountFacts gives an item with an action a button that runs it', async () => {
  reset();
  const el = document.createElement('ul');
  const ran = [];
  const up = mountFacts(el, {
    rpc: async () => ({ daisugi: { mode: 'off', armed: true, enforcing: 0, watching: 0, off: 0, installed: false } }),
    connected: () => true,
    later: () => 0,
    cancel: () => {},
    onAction: (a) => ran.push(a),
  });
  await up.read();
  const li = [...el.children].find((c) => String(c.className).includes('fact-gate-hint'));
  assert.ok(li, 'no hint item painted');
  const button = [...li.children].find((c) => c.tagName === 'BUTTON');
  assert.ok(button, 'no button');
  button.dispatchEvent({ type: 'click' });
  assert.deepEqual(ran.map((a) => a.command), ['daisugi install --gate']);
});

test('on a 390 px phone the rail carries the install hint, with the typed-not-run button', async () => {
  reset();
  const { isPhone } = await import('../dock.js');
  assert.equal(isPhone(390, 844), true, '390 x 844 is a phone');
  const { readFileSync } = await import('node:fs');
  const html = readFileSync(new URL('../index.html', import.meta.url), 'utf8');
  const css = readFileSync(new URL('../app.css', import.meta.url), 'utf8');
  // The hint sits in the rail, above the agent list, and the phone rule
  // that hides the header row does not hide it.
  assert.ok(html.indexOf('id="gate-hint"') > html.indexOf('id="rail"') && html.indexOf('id="gate-hint"') < html.indexOf('id="roster"'));
  assert.match(css, /body\.phone #facts \{ display: none; \}/);
  assert.doesNotMatch(css, /body\.phone #gate-hint \{ display: none/);
  assert.match(css, /@media \(min-width: 600px\) \{ body:not\(\.phone\) #gate-hint \{ display: none; \} \}/);

  const hint = document.createElement('p');
  hint.hidden = true;
  const ran = [];
  const el = document.createElement('ul');
  const up = mountFacts(el, {
    rpc: async () => ({ daisugi: { mode: 'off', armed: true, enforcing: 0, watching: 0, off: 0, installed: false } }),
    connected: () => true,
    later: () => 0,
    cancel: () => {},
    onAction: (a) => ran.push(a),
    hintEl: hint,
  });
  await up.read();
  assert.equal(hint.hidden, false);
  const words = [...hint.children].find((c) => c.tagName === 'SPAN');
  assert.equal(words.textContent, 'daisugi is off: no agent is guarded. Run: daisugi install --gate');
  const button = [...hint.children].find((c) => c.tagName === 'BUTTON');
  button.dispatchEvent({ type: 'click' });
  assert.deepEqual(ran.map((a) => [a.kind, a.command]), [['shell', 'daisugi install --gate']]);
  // Once a hook is installed the hint goes.
  paintHint(hint, factsItems({ daisugi: { mode: 'off', armed: true, enforcing: 0, watching: 0, off: 0, installed: true } }), () => {});
  assert.equal(hint.hidden, true);
});

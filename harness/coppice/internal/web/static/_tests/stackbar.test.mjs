import test from 'node:test';
import assert from 'node:assert/strict';
import {
  segments, gateMark, light, details, fmtTokens, ago, factsItems, stackBarEl, gateMarkEls, detailsBox, liveCounts, mountFacts, paintHint,
  modeChoice, modeAsk, modeDone, MODE_OFF_NOTE,
  modelSwap, routerSwap, modelDone, routerDone, routerAsk, ROUTER_NOTE, SWAP_NOTE, MODELS,
  loopSwap, LOOPS, LOOP_NOTE, moveLine, moveDone,
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
  // The mode is set here; off never is.
  assert.equal(d.note, MODE_OFF_NOTE);
  assert.match(d.note, /daisugi gate disarm/);
  const m = details('model', r, null, NOW);
  assert.deepEqual(m.lines, [['model', 'claude-fake-2'], ['fresh in', '13'], ['cache read', '3k'], ['cache write', '20'], ['out', '1.2k']]);
  assert.equal(m.note, 'The change goes in at the next idle prompt.');
  const g = details('router', r, { gateway: { url: 'http://127.0.0.1:8787', answers: true } }, NOW);
  assert.deepEqual(g.lines, [['route', 'gateway'], ['gateway', 'http://127.0.0.1:8787'], ['answers', 'yes']]);
  assert.equal(details('loop', r, null, NOW).note, LOOP_NOTE);
  assert.equal(details('loop', row({ closed: true }), null, NOW).note, SWAP_NOTE);
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

// The header names a saving only from the gateway's own figure. With no
// figure, a bad one or none of the right kind, there is no item and no
// number.
test('the header shows tokens saved only from a gateway figure', () => {
  const base = { daisugi: { mode: 'off', armed: true } };
  const keys = (f) => factsItems(f).map((i) => i.key);
  assert.equal(keys(base).includes('saved'), false);
  for (const bad of [null, {}, { tokens: 'x' }, { tokens: -1 }, { tokens: null }, 12, 'many']) {
    assert.equal(keys({ ...base, saved_today: bad }).includes('saved'), false, JSON.stringify(bad));
  }
  const items = factsItems({ ...base, saved_today: { tokens: 12345 } });
  const saved = items.find((i) => i.key === 'saved');
  assert.deepEqual([saved.text, saved.health], ['saved 12.3k', 'plain']);
  assert.match(saved.title, /gateway/);
  assert.equal(items.findIndex((i) => i.key === 'saved'), items.findIndex((i) => i.key === 'tokens') + 1);
  const zero = factsItems({ ...base, saved_today: { tokens: 0 } }).find((i) => i.key === 'saved');
  assert.equal(zero.text, 'saved 0');
});

// Under 7 days left on the phone's certificate the header warns, with
// Renew, which types the command in a shell and does not run it. With no
// such fact there is no item.
test('the header row warns about the phone certificate with Renew', () => {
  const base = { daisugi: { mode: 'off', armed: true } };
  assert.equal(factsItems(base).find((i) => i.key === 'phone-cert'), undefined);
  const soon = factsItems({ ...base, phone_cert_days: 3 }).find((i) => i.key === 'phone-cert');
  assert.equal(soon.text, 'The phone certificate runs out in 3 days.');
  assert.equal(soon.health, 'warn');
  assert.deepEqual(soon.action, { kind: 'shell', label: 'Renew', command: 'coppice web cert tailscale' });
  const one = factsItems({ ...base, phone_cert_days: 1 }).find((i) => i.key === 'phone-cert');
  assert.equal(one.text, 'The phone certificate runs out in 1 day.');
  const today = factsItems({ ...base, phone_cert_days: 0 }).find((i) => i.key === 'phone-cert');
  assert.equal(today.text, 'The phone certificate runs out today.');
  const gone = factsItems({ ...base, phone_cert_days: -2 }).find((i) => i.key === 'phone-cert');
  assert.equal(gone.text, 'The phone certificate has run out. The phone cannot reach the box until it is renewed.');
  assert.equal(gone.health, 'bad');
  assert.equal(factsItems({ ...base, phone_cert_days: 'x' }).find((i) => i.key === 'phone-cert'), undefined);
});

// The header row is hidden on a phone, so the rail's list header carries
// the certificate warning there, as it carries the install hint.
test('paintHint carries the phone certificate warning too', () => {
  reset();
  const hint = document.createElement('p');
  hint.hidden = true;
  const ran = [];
  paintHint(hint, factsItems({ daisugi: { mode: 'off', armed: true, enforcing: 1 }, phone_cert_days: 2 }), (a) => ran.push(a));
  assert.equal(hint.hidden, false);
  const words = [...hint.children].filter((c) => c.tagName === 'SPAN').map((c) => c.textContent);
  assert.deepEqual(words, ['The phone certificate runs out in 2 days.']);
  [...hint.children].find((c) => c.tagName === 'BUTTON').dispatchEvent({ type: 'click' });
  assert.deepEqual(ran.map((a) => [a.kind, a.label, a.command]), [['shell', 'Renew', 'coppice web cert tailscale']]);
  // With both, the rail carries both.
  paintHint(hint, factsItems({ daisugi: { mode: 'off', armed: true, installed: false }, phone_cert_days: 2 }), () => {});
  assert.equal([...hint.children].filter((c) => c.tagName === 'SPAN').length, 2);
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
  // that hides most of the header row's facts does not hide it.
  assert.ok(html.indexOf('id="gate-hint"') > html.indexOf('id="rail"') && html.indexOf('id="gate-hint"') < html.indexOf('id="roster"'));
  assert.match(css, /body\.phone #facts \.fact \{ display: none; \}/);
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

test('the mode row lowers an enforcing agent to watching and back, and never offers off', () => {
  const enf = modeChoice(row({ label: 'auth fix' }));
  assert.deepEqual(enf, { next: 'watching', label: 'Watch only', ask: 'Watch only? The gate will log the calls of auth fix and stop none. While it watches, it can also change the gate for other agents.' });
  // The marker wins over the reported mode: the gate reads it on the next call.
  const marked = row({ label: 'auth fix', stack: { loop: 'claude', daisugi: { mode: 'enforcing', armed: true, marker: 'watching' } } });
  assert.deepEqual(modeChoice(marked), { next: 'enforcing', label: 'Enforce', ask: '' });
  const d = details('daisugi', marked, null, NOW);
  assert.deepEqual(d.lines[0], ['mode', 'enforcing']);
  assert.deepEqual(d.lines[1], ['next call', 'watching']);
  assert.deepEqual(d.mode, modeChoice(marked));
  // No gate hook, no agent, an ended agent: nothing to set.
  assert.equal(modeChoice(row({ stack: { loop: 'claude', daisugi: { mode: 'off', armed: true } } })), null);
  assert.equal(modeChoice(row({ stack: { loop: 'claude' } })), null);
  assert.equal(modeChoice(row({ closed: true })), null);
  assert.equal(modeChoice(null), null);
  assert.equal(modeChoice(row({ id: 'w1:p4' })).ask, 'Watch only? The gate will log the calls of w1:p4 and stop none. While it watches, it can also change the gate for other agents.');
  assert.equal(modeAsk('x'), 'Watch only? The gate will log the calls of x and stop none. While it watches, it can also change the gate for other agents.');
  assert.equal(modeDone('auth fix', 'watching'), 'auth fix watches from its next tool call. The gate logs its calls and stops none.');
  assert.equal(modeDone('auth fix', 'enforcing'), "auth fix goes back to the gate's own mode from its next tool call.");
});

test('Watch only asks once; Enforce does not ask', () => {
  reset();
  const sent = [];
  const asks = [];
  const r = row({ label: 'auth fix' });
  // The first click asks: the box draws again with asking true.
  let box = detailsBox(details('daisugi', r, null, NOW), { onClose: () => {}, onMode: (m) => sent.push(m), asking: false, onAsk: (on) => asks.push(on) });
  const watch = box.querySelectorAll('button').find((b) => b.textContent === 'Watch only');
  assert.ok(watch, 'no Watch only button');
  assert.ok(!box.querySelectorAll('button').some((b) => /off/i.test(b.textContent)), 'off is offered');
  watch.dispatchEvent({ type: 'click' });
  assert.deepEqual(sent, [], 'Watch only sent before the confirm');
  assert.deepEqual(asks, [true]);
  box = detailsBox(details('daisugi', r, null, NOW), { onClose: () => {}, onMode: (m) => sent.push(m), asking: true, onAsk: (on) => asks.push(on) });
  assert.ok(box.querySelectorAll('p').some((p) => p.textContent === 'Watch only? The gate will log the calls of auth fix and stop none. While it watches, it can also change the gate for other agents.'));
  const keep = box.querySelectorAll('button').find((b) => b.textContent === 'Keep enforcing');
  keep.dispatchEvent({ type: 'click' });
  assert.deepEqual(asks, [true, false]);
  assert.deepEqual(sent, []);
  const yes = box.querySelectorAll('button').find((b) => b.textContent === 'Watch only');
  yes.dispatchEvent({ type: 'click' });
  assert.deepEqual(sent, ['watching']);
  assert.deepEqual(asks, [true, false, false]);
  const marked = row({ label: 'auth fix', stack: { loop: 'claude', daisugi: { mode: 'watching', armed: true, marker: 'watching' } } });
  box = detailsBox(details('daisugi', marked, null, NOW), { onClose: () => {}, onMode: (m) => sent.push(m), asking: false, onAsk: (on) => asks.push(on) });
  box.querySelectorAll('button').find((b) => b.textContent === 'Enforce').dispatchEvent({ type: 'click' });
  assert.deepEqual(sent, ['watching', 'enforcing']);
  assert.equal(asks.length, 3, 'Enforce asked');
});

test('when the whole gate watches, the row offers no Watch only and never says enforces', () => {
  // The hook runs in audit: the agent reports watching with no marker.
  const r = row({ label: 'auth fix', stack: { loop: 'claude', daisugi: { mode: 'watching', armed: true } } });
  const c = modeChoice(r);
  assert.equal(c.next, null);
  assert.equal(c.line, 'The gate itself watches every call of auth fix: its hook runs in audit, so there is nothing to lower here.');
  const box = detailsBox(details('daisugi', r, null, NOW), { onClose: () => {}, onMode: () => assert.fail('a mode was set'), asking: false, onAsk: () => {} });
  assert.ok(!box.querySelectorAll('button').some((b) => b.textContent === 'Watch only' || b.textContent === 'Enforce'));
  assert.ok(box.querySelectorAll('p').some((p) => p.textContent === c.line));
  // Enforce after Watch only goes back to the gate's own mode, whatever it is.
  assert.ok(!modeDone('auth fix', 'enforcing').includes('enforces'));
});

test('when the hook reads another gate root, the row says so and offers nothing', () => {
  const facts = { daisugi: { mode: 'enforcing', hook_root: '/elsewhere/gate' } };
  const r = row({ label: 'auth fix' });
  const c = modeChoice(r, facts);
  assert.equal(c.next, null);
  assert.equal(c.line, 'The gate hook reads another gate root, /elsewhere/gate, so a mode set here does nothing.');
  const d = details('daisugi', r, facts, NOW);
  assert.deepEqual(d.mode, c);
  assert.equal(modeChoice(r, { daisugi: { mode: 'enforcing' } }).next, 'watching');
});

test('the model row lists the harness\'s own models for claude and takes a typed name for sprig', () => {
  const c = modelSwap(row({ kind: 'pty' }));
  assert.deepEqual(c.options, MODELS.claude);
  assert.equal(c.free, false);
  assert.equal(c.when, 'The change goes in at the next idle prompt.');
  const sp = modelSwap(row({ harness: 'sprig', kind: 'headless', stack: { loop: 'sprig' } }));
  assert.deepEqual([sp.options.length, sp.free], [0, true]);
  assert.equal(sp.when, 'The change applies from the next turn.');
  // Nothing is built for the rest: they keep the honest note.
  for (const r of [row({ harness: 'codex', stack: { loop: 'codex' } }), row({ kind: 'headless' }), row({ closed: true }), row({ state: 'done' }), null]) {
    assert.equal(modelSwap(r), null);
  }
  assert.equal(details('model', row({ harness: 'codex', stack: { loop: 'codex' } }), null, NOW).note, SWAP_NOTE);
  assert.equal(details('model', row({ kind: 'pty' }), null, NOW).note, c.when);
});

test('a model button sends the name, and sprig\'s box sends what was typed', () => {
  reset();
  const sent = [];
  let box = detailsBox(details('model', row({ kind: 'pty' }), null, NOW), { onClose: () => {}, onSwap: (k, v) => sent.push([k, v]) });
  const btn = box.querySelectorAll('button').filter((b) => MODELS.claude.includes(b.textContent));
  assert.deepEqual(btn.map((b) => b.textContent), MODELS.claude);
  btn[0].dispatchEvent({ type: 'click' });
  assert.deepEqual(sent, [['model', 'opus']]);
  const sp = row({ harness: 'sprig', kind: 'headless', stack: { loop: 'sprig' } });
  box = detailsBox(details('model', sp, null, NOW), { onClose: () => {}, onSwap: (k, v) => sent.push([k, v]) });
  const input = box.querySelectorAll('input')[0];
  const set = box.querySelectorAll('button').find((b) => b.textContent === 'Set');
  set.dispatchEvent({ type: 'click' });
  assert.equal(sent.length, 1, 'an empty name was sent');
  input.value = ' o3 ';
  set.dispatchEvent({ type: 'click' });
  assert.deepEqual(sent[1], ['model', 'o3']);
});

test('the router row asks before it restarts, and offers only the routes it can take', () => {
  reset();
  const gw = { gateway: { url: 'http://127.0.0.1:8787', answers: true } };
  const r = row({ label: 'auth fix', stack: { loop: 'claude', router: 'gateway', daisugi: { mode: 'enforcing' } } });
  assert.deepEqual(routerSwap(r, gw).choices.map((c) => c.next), ['direct']);
  const direct = row({ label: 'auth fix', stack: { loop: 'claude', router: 'direct' } });
  assert.deepEqual(routerSwap(direct, gw).choices.map((c) => c.next), ['gateway']);
  // No gateway known: only direct. Switchyard is the gateway's own setting.
  assert.deepEqual(routerSwap(r, null).choices.map((c) => c.next), ['direct']);
  assert.equal(routerSwap(direct, null), null);
  assert.equal(routerSwap(row({ stack: { loop: 'claude', router: 'switchyard' } }), gw).choices.length, 1);
  assert.equal(routerSwap(row({ harness: 'codex', stack: { loop: 'codex' } }), gw), null);
  assert.equal(routerSwap(row({ closed: true }), gw), null);
  assert.equal(details('router', r, gw, NOW).note, ROUTER_NOTE);
  assert.equal(details('router', row({ harness: 'codex', stack: { loop: 'codex' } }), gw, NOW).note, SWAP_NOTE);
  assert.equal(routerAsk('auth fix'), 'Restarts auth fix and resumes its conversation. A turn in progress ends.');
  const sent = [];
  const asks = [];
  const opts = (asking) => ({ onClose: () => {}, onSwap: (k, v) => sent.push([k, v]), swapAsking: asking, onSwapAsk: (v) => asks.push(v) });
  let box = detailsBox(details('router', r, gw, NOW), opts(''));
  box.querySelectorAll('button').find((b) => b.textContent === 'Direct').dispatchEvent({ type: 'click' });
  assert.deepEqual([sent, asks], [[], ['direct']], 'the click restarted before the question');
  box = detailsBox(details('router', r, gw, NOW), opts('direct'));
  assert.ok(box.querySelectorAll('p').some((p) => p.textContent === 'Restarts auth fix and resumes its conversation. A turn in progress ends.'));
  box.querySelectorAll('button').find((b) => b.textContent === 'Keep').dispatchEvent({ type: 'click' });
  assert.deepEqual([sent, asks], [[], ['direct', '']]);
  box.querySelectorAll('button').find((b) => b.textContent === 'Resume').dispatchEvent({ type: 'click' });
  assert.deepEqual(sent, [['router', 'direct']]);
});

test('the gateway choice is greyed out when the facts say the gateway does not answer', () => {
  reset();
  const direct = row({ label: 'auth fix', stack: { loop: 'claude', router: 'direct' } });
  const down = { gateway: { url: 'http://127.0.0.1:8787', answers: false } };
  const choice = routerSwap(direct, down).choices.find((c) => c.next === 'gateway');
  assert.equal(choice.disabled, true);
  assert.equal(choice.why, 'The gateway does not answer.');
  // Answering, or not known (a gateway off this computer): offered as before.
  for (const g of [true, undefined]) {
    const up = { gateway: { url: 'http://127.0.0.1:8787', answers: g } };
    assert.ok(!routerSwap(direct, up).choices.find((c) => c.next === 'gateway').disabled);
  }
  // Direct stays open, so a pane on a dead gateway can still leave it.
  const onGw = row({ label: 'auth fix', stack: { loop: 'claude', router: 'gateway' } });
  assert.ok(!routerSwap(onGw, down).choices.find((c) => c.next === 'direct').disabled);
  const asks = [];
  const box = detailsBox(details('router', direct, down, NOW), { onClose: () => {}, onSwap: () => {}, swapAsking: '', onSwapAsk: (v) => asks.push(v) });
  const btn = box.querySelectorAll('button').find((b) => b.textContent === 'Through the gateway');
  assert.equal(btn.disabled, true);
  assert.equal(btn.title, 'The gateway does not answer.');
  btn.dispatchEvent({ type: 'click' });
  assert.deepEqual(asks, [], 'a greyed-out choice asked');
});

test('the status lines say when a swap lands', () => {
  assert.equal(modelDone('auth fix', 'opus', 'queued'), 'auth fix switches to opus at its next idle prompt.');
  assert.equal(modelDone('sp', 'o3', 'next turn'), 'sp uses o3 from its next turn.');
  assert.equal(modelDone('auth fix', 'opus', 'now'), 'Asked auth fix to switch to opus.');
  assert.equal(routerDone('auth fix', 'gateway'), 'auth fix restarted and resumed through the gateway. It enforces again.');
  assert.equal(routerDone('auth fix', 'direct'), 'auth fix restarted and resumed direct. It enforces again.');
});

test('the loop row offers the other harnesses and shows the replay cost before it moves', () => {
  reset();
  const r = row({ label: 'auth fix' });
  const sw = loopSwap(r);
  assert.deepEqual(sw.choices.map((c) => c.next), LOOPS.filter((h) => h !== 'claude'));
  assert.ok(!sw.choices.some((c) => c.next === 'claude'), 'the harness it runs on is offered');
  // An ended agent, and a row that names no harness, have nothing to move.
  assert.equal(loopSwap(row({ closed: true })), null);
  assert.equal(loopSwap(row({ state: 'done' })), null);
  assert.equal(loopSwap({ id: 'x', state: 'idle' }), null);
  assert.equal(loopSwap(null), null);
  // A harness whose conversation the server cannot read has no move.
  assert.equal(loopSwap(row({ harness: 'codex' })), null);
  assert.equal(loopSwap(row({ harness: 'sprig', kind: 'pty' })), null);
  assert.ok(loopSwap(row({ harness: 'sprig', kind: 'headless' })));
  const previews = [];
  const sent = [];
  const asks = [];
  const opts = (asking, line) => ({
    onClose: () => {}, onSwap: (k, v) => sent.push([k, v]), swapAsking: asking, swapLine: line,
    onSwapAsk: (v) => asks.push(v), onMovePreview: (to) => previews.push(to),
  });
  let box = detailsBox(details('loop', r, null, NOW), opts('', ''));
  box.querySelectorAll('button').find((b) => b.textContent === 'codex').dispatchEvent({ type: 'click' });
  assert.deepEqual([previews, sent, asks], [['codex'], [], []], 'the click moved before the cost was shown');
  const line = moveLine({ tokens: 38000, trimmed: false }, 'codex');
  assert.equal(line, 'Moves about 38k tokens of conversation to codex. This agent stays.');
  box = detailsBox(details('loop', r, null, NOW), opts('codex', line));
  assert.ok(box.querySelectorAll('p').some((p) => p.textContent === line));
  box.querySelectorAll('button').find((b) => b.textContent === 'Keep').dispatchEvent({ type: 'click' });
  assert.deepEqual([sent, asks], [[], ['']]);
  box.querySelectorAll('button').find((b) => b.textContent === 'Move').dispatchEvent({ type: 'click' });
  assert.deepEqual(sent, [['loop', 'codex']]);
});

test('the replay cost line says when older turns stay behind', () => {
  assert.equal(moveLine({ tokens: 3200, trimmed: true, conversation: 38000 }, 'pi'),
    'Moves about 3.2k tokens of conversation to pi. Older turns stay behind (about 38k in all). This agent stays.');
  assert.equal(moveLine({ tokens: 900, trimmed: true }, 'pi'),
    'Moves about 900 tokens of conversation to pi. Older turns stay behind. This agent stays.');
  assert.equal(moveLine(null, 'pi'), 'Moves about 0 tokens of conversation to pi. This agent stays.');
  assert.equal(moveDone('auth fix', 'codex', { when: 'now' }), 'Started codex with the conversation of auth fix. auth fix stays.');
  assert.equal(moveDone('auth fix', 'codex', { when: 'queued' }),
    'Started codex with the conversation of auth fix. auth fix stays. The brief goes in when codex is ready, or a note says it did not.');
  assert.equal(moveDone('auth fix', 'codex', { when: 'typed' }),
    'Started codex with the conversation of auth fix. auth fix stays. The brief was typed in at once. Check that it landed.');
});

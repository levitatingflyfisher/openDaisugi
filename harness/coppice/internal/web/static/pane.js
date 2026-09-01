import { applyFrame, gridToText, drawGrid, measureAdvance } from './grid.js';
import { stateChip } from './chips.js';
import { mountRecord } from './record.js';
import { cellFor, scrollFollow } from './windows.js';
import { segments, gateMark, light, details, stackBarEl, gateMarkEls, detailsBox } from './stackbar.js';

// One agent on its own: on a phone, the sheet that slides in over the
// overview; on a wider page, the screen a link to a pane opens. It
// watches and never resizes, so attach carries no size, because the same
// pane is open in the operator's terminal and a resize from here would
// change what they are reading. The text is drawn at a readable cell, at
// least 7 px wide, and a terminal wider than the sheet scrolls sideways.
//
// The bottom bar types to the agent. The keyboard's own Enter types the
// text into the agent's input and does not press Enter, so the owner can
// read it there first. The Enter button types the text, if any, and
// presses Enter, in one request. The key drawer sends the keys a phone
// keyboard lacks.

// KEYS are the drawer's keys: the name the server knows, the word on the
// button, and what it does.
export const KEYS = [
  { key: 'esc', label: 'Esc', title: 'Escape' },
  { key: 'tab', label: 'Tab', title: 'Tab' },
  { key: 'left', label: '←', title: 'Left arrow' },
  { key: 'up', label: '↑', title: 'Up arrow' },
  { key: 'down', label: '↓', title: 'Down arrow' },
  { key: 'right', label: '→', title: 'Right arrow' },
  { key: 'ctrl+c', label: 'ctrl-c', title: 'Control C: stop what the agent runs' },
  { key: 'enter', label: 'Enter', title: 'Enter' },
];

export function attachCommand(pane) {
  return { cmd: 'pane.attach', pane: pane.id };
}

// typeCommand types text into the agent's input with no Enter.
export function typeCommand(pane, text) {
  return { cmd: 'pane.send_text', pane: pane.id, text, enter: false };
}

// enterCommand types text, which may be empty, and presses Enter.
export function enterCommand(pane, text) {
  return { cmd: 'pane.send_text', pane: pane.id, text, enter: true };
}

// HEADLESS_NOTE is what a headless agent's sheet and window say: it has
// no terminal to type into, so it takes whole messages.
export const HEADLESS_NOTE = 'This agent takes whole messages. Type below and press Enter.';

// isHeadless is true for a row whose agent runs headless, through its
// adapter. Raw text and keys would reach its stdin or be refused, and
// would skip the rule that typed text never answers an ask.
export function isHeadless(row) {
  return Boolean(row) && row.kind === 'headless';
}

// typeFor is what the keyboard's Enter in the bar sends for row: typed
// text with no Enter, or for a headless agent the whole message. It is
// null when there is nothing to send.
export function typeFor(row, text) {
  if (!text) return null;
  return isHeadless(row) ? { cmd: 'agent.prompt', pane: row.id, text } : typeCommand(row, text);
}

// enterFor is what the Enter button sends for row: the text and Enter in
// one request, a bare Enter with no text, or for a headless agent the
// whole message, and nothing when it is empty.
export function enterFor(row, text) {
  if (isHeadless(row)) return text ? { cmd: 'agent.prompt', pane: row.id, text } : null;
  return enterCommand(row, text);
}

export function keysCommand(pane, key) {
  return { cmd: 'pane.send_keys', pane: pane.id, keys: [key] };
}

// stateOf reads the state off a coppice-server object. Both shapes that
// carry one are flat and use the same field names: a pane.list row and a
// state event both put state, source, harness, ts, and ask straight on the
// object, not nested under a wrapper. Read the nested shape here and
// st.state is undefined for every pane, the whole roster paints unknown,
// and Allow and Deny never appears on the pane the operator just tapped.
export function stateOf(row) {
  if (!row) return { state: 'unknown', source: '', harness: 'unknown', ts: 0, ask: null };
  return {
    state: row.state || 'unknown',
    source: row.source || '',
    harness: row.harness || 'unknown',
    ts: row.ts || 0,
    ask: row.ask || null,
    detail: row.detail || '',
  };
}

// TRUST_RULE is the manifest rule that sees Claude Code's folder trust
// screen. The server names it in a state's detail.
const TRUST_RULE = 'rule=coppice_first_run_trust ';

// trustFor is true when st shows Claude's folder trust screen and no ask.
export function trustFor(st) {
  return Boolean(st) && st.state === 'blocked' && !st.ask && String(st.detail || '').includes(TRUST_RULE);
}

// TRUST_TEXT is the line above the two trust answers.
export const TRUST_TEXT = 'Claude asks to trust this folder.';

// TRUST_YES and TRUST_NO are the two trust answers. Not now presses Esc,
// which ends Claude, and its title says so.
export const TRUST_YES = { label: 'Trust this folder', title: 'Choose "Yes, I trust this folder" on Claude\'s screen.' };
export const TRUST_NO = { label: 'Not now', title: 'Press Esc on Claude\'s screen. This ends Claude.' };

// trustCommand answers the trust screen of pane: yes trusts the folder,
// and no is not now.
export function trustCommand(pane, yes) {
  return { cmd: 'pane.trust', pane, trust: Boolean(yes) };
}

// trustDone is what the page says once an answer went.
export function trustDone(yes) {
  return yes ? 'Trusted the folder. Claude starts.' : 'Not now. Claude ends.';
}

// queuedLine is what the page says when the server queued a prompt until
// the agent is ready, or '' when it sent it at once.
export function queuedLine(res) {
  if (!res || !res.note) return '';
  const note = String(res.note);
  return note.charAt(0).toUpperCase() + note.slice(1) + '. It goes by itself.';
}

// QUESTION_REFUSAL is in the server's refusal of a prompt to a pane that
// waits on a question on its screen. The server adds the fix after it.
export const QUESTION_REFUSAL = ' is waiting on a question on its screen. Answer it first';

// questionMessage is the refusal as a message with a fix: a button that
// shows the pane and its question.
export function questionMessage(text, pane) {
  return { text: String(text || ''), actions: [{ kind: 'open', label: 'Show the question', pane }] };
}

// isQuestionRefusal is true for the server's refusal of a prompt to a
// pane that waits on a question.
export function isQuestionRefusal(e) {
  return Boolean(e) && typeof e.message === 'string' && e.message.includes(QUESTION_REFUSAL);
}

// needsYou is true for a pane the operator must answer now: a blocked
// pane whose ask no foreman holds.
export function needsYou(row) {
  return Boolean(row) && stateOf(row).state === 'blocked' && !row.held;
}

// waitedText is how long a hold has lasted, in the coarsest unit that is
// not zero.
function waitedText(seconds) {
  const s = Math.max(0, Math.floor(seconds));
  if (s < 60) return s + 's';
  if (s < 3600) return Math.floor(s / 60) + 'm';
  return Math.floor(s / 3600) + 'h';
}

// heldLine is the line a held ask shows: the task whose foreman hears it,
// and how long it has waited there when the hold says when it began.
export function heldLine(held, nowSeconds) {
  const task = (held && (held.task_label || held.task)) || 'a task';
  const line = 'waiting on ' + task + "'s foreman";
  if (!held || typeof held.since !== 'number') return line;
  return line + ' · ' + waitedText(nowSeconds - held.since);
}

// askFor decides what the Allow and Deny box shows. Pure, so the decision
// the operator depends on is testable against a real pane.list row rather
// than only through the DOM.
export function askFor(st) {
  if (st.state !== 'blocked' || !st.ask) return { visible: false, summary: '', askId: '', tier: 'permanent', held: false };
  return {
    visible: true,
    summary: st.ask.summary || st.ask.tool || 'blocked',
    askId: st.ask.id || '',
    tier: tierOf(st.ask),
    held: st.ask.holder === 'harness',
  };
}

// HELD_BY_HARNESS is what an ask a harness holds itself says above Deny
// and Allow. The gate has no file for such an ask, so the page answers it
// through coppice-server with agent.allow or agent.deny, as the operator.
export const HELD_BY_HARNESS = 'The harness holds this ask itself. Deny or allow here, and coppice-server answers it.';

// heldTierLine is the tier line of an ask a harness holds: where the
// answer goes, then the same tier rule every ask has.
export function heldTierLine(tier, name) {
  return HELD_BY_HARNESS + ' ' + tierLine(tier, name);
}

// heldAnswer is the request that answers an ask a harness holds: the verb
// and its fields. A permanent allow carries the typed pane name as
// confirm, as the server asks.
export function heldAnswer(pane, askId, decision, confirm) {
  const fields = { pane, ask: askId, reason: decision === 'allow' ? 'allowed from the phone' : 'denied from the phone' };
  if (confirm) fields.confirm = confirm;
  return { cmd: decision === 'allow' ? 'agent.allow' : 'agent.deny', fields };
}

// tierOf is 'undoable' only when the ask says so. A missing or unknown
// tier is 'permanent', the same as on the server.
export function tierOf(ask) {
  return ask && ask.tier === 'undoable' ? 'undoable' : 'permanent';
}

// paneName is what the operator types to allow a permanent ask: the
// pane's label, or its id when it has none. The server uses the same rule.
export function paneName(row) {
  return (row && (row.label || row.id)) || '';
}

// tierLine is the sentence that says how to answer an ask of this tier.
export function tierLine(tier, name) {
  if (tier === 'undoable') return 'Undoable. Deny is the default.';
  return 'This cannot be undone. Deny is the default. To allow, type ' + name + ' and press Enter.';
}

// verdictLine is what the ask bar says under the summary when the ask
// carries the gate's own verdict. It is '' when there is none.
export function verdictLine(ask) {
  const gate = ask && ask.gate;
  if (!gate || typeof gate !== 'object') return '';
  if (gate.verdict === 'deny') return gate.rule === undefined || gate.rule === null ? 'The gate says no' : 'The gate says no, rule ' + gate.rule;
  if (gate.verdict === 'allow') return 'The gate says yes';
  return '';
}

// answerBody is the body the answer route takes. confirm and scope go
// only when set.
export function answerBody(askId, decision, { pane, reason, confirm } = {}) {
  const body = { tool_use_id: askId, decision };
  if (pane) body.pane = pane;
  body.reason = reason || (decision === 'allow' ? 'allowed from the phone' : 'denied from the phone');
  if (confirm) body.confirm = confirm;
  return body;
}

// denyButton is the Deny control. It is always the first control, so a
// tap in the usual place denies.
export function denyButton(onDeny, className = 'deny') {
  const b = document.createElement('button');
  b.className = className;
  b.textContent = 'Deny';
  b.addEventListener('click', (e) => { stop(e); onDeny(); });
  return b;
}

// drafts holds what the operator has typed into a name field, by ask id.
// The floor bar and the phone card are rebuilt on every refresh, and the
// typed text must survive that, but never carry over to another ask.
const drafts = new Map();

// allowControl is Allow on an undoable ask. On a permanent ask it is a
// field for the pane name: Enter with the name allows, and any other text
// allows nothing and says why. A rebuilt field for the same ask keeps the
// typed text, and the focus when the old field had it.
export function allowControl(tier, name, onAllow, status, askId = '') {
  if (tier === 'undoable') {
    const b = document.createElement('button');
    b.className = 'allow';
    b.textContent = 'Allow';
    b.addEventListener('click', (e) => { stop(e); onAllow(''); });
    return b;
  }
  const input = document.createElement('input');
  input.className = 'ask-name';
  input.type = 'text';
  input.placeholder = 'type the pane name';
  input.title = 'Type ' + name + ' and press Enter to allow';
  input.dataset.askId = askId;
  if (askId && drafts.has(askId)) input.value = drafts.get(askId);
  const had = typeof document !== 'undefined' ? document.activeElement : null;
  if (askId && had && had.dataset && had.dataset.askId === askId && had !== input) {
    setTimeout(() => { if (typeof input.focus === 'function') input.focus(); }, 0);
  }
  input.addEventListener('input', () => { if (askId) drafts.set(askId, input.value); });
  input.addEventListener('click', stop);
  input.addEventListener('keydown', (e) => {
    stop(e);
    if (e.key !== 'Enter') return;
    const typed = String(input.value || '').trim();
    if (typed !== name) {
      status('That is not the pane name. Type ' + name + ' to allow.');
      return;
    }
    if (typeof e.preventDefault === 'function') e.preventDefault();
    drafts.delete(askId);
    onAllow(typed);
  });
  return input;
}

function stop(e) {
  if (e && typeof e.stopPropagation === 'function') e.stopPropagation();
}

let current = null;   // { id }
let currentName = ''; // the pane name a permanent allow needs
let grid = null;
let advance = 0;      // the measured glyph advance, once a canvas exists
let follow = {};      // autoTop and lastX from the last cursor follow
let seen = false;     // a pane list has held the open pane
let openSeg = '';     // the stack segment whose details show
let headless = false; // the open agent runs headless and takes whole messages
// outbox chains every send, so the agent gets text and keys in the order
// they were typed even though the server answers each request on its own.
let outbox = Promise.resolve();

const dpr = () => (typeof window !== 'undefined' && Number(window.devicePixelRatio) > 0 ? Number(window.devicePixelRatio) : 1);

// say puts one message on the status line. An error keeps its fix.
const say = (msg) => window.coppice.status(msg);

// repaint draws the grid at a cell that fits the body's width, never below
// the readable size, and keeps the cursor in sight.
function repaint() {
  if (!grid) return;
  const canvas = document.getElementById('grid');
  const wrap = document.getElementById('canvas-wrap');
  const ctx = canvas.getContext('2d');
  if (!advance) advance = measureAdvance(ctx);
  const cell = cellFor(Number(wrap && wrap.clientWidth) || 0, grid.cols, advance, dpr());
  drawGrid(ctx, grid, cell);
  document.getElementById('grid-text').textContent = gridToText(grid);
  if (!wrap) return;
  const at = scrollFollow({
    shownW: Number(wrap.clientWidth) || 0, shownH: Number(wrap.clientHeight) || 0,
    cols: grid.cols, rows: grid.rows, cellW: cell.w, cellH: cell.h, cursor: grid.cursor,
    top: wrap.scrollTop, left: wrap.scrollLeft, autoTop: follow.autoTop, lastX: follow.lastX,
  });
  if (at.top !== (Number(wrap.scrollTop) || 0)) wrap.scrollTop = at.top;
  if (at.left !== (Number(wrap.scrollLeft) || 0)) wrap.scrollLeft = at.left;
  follow = { autoTop: at.autoTop, lastX: at.lastX };
}

// Frames come faster than a screen draws, so the grid draws at most once
// per animation frame. Without requestAnimationFrame it draws at once.
let frameAsked = false;
function schedule() {
  if (typeof requestAnimationFrame !== 'function') {
    repaint();
    return;
  }
  if (frameAsked) return;
  frameAsked = true;
  requestAnimationFrame(() => {
    frameAsked = false;
    repaint();
  });
}

function setAsk(st) {
  const box = document.getElementById('ask');
  const chip = document.getElementById('pane-chip');
  const c = stateChip(st.state);
  chip.textContent = c.word;
  chip.className = 'chip ' + c.cls;
  const dot = document.getElementById('pane-dot');
  if (dot) dot.className = 'dot dot-' + c.word;
  document.getElementById('pane-source').textContent = st.source ? 'source ' + st.source : '';
  const ask = askFor(st);
  // A new ask starts with an empty name field, so text typed for the last
  // ask can never allow this one.
  if (box.dataset.askId !== ask.askId) document.getElementById('ask-name').value = '';
  document.getElementById('ask-summary').textContent = ask.summary;
  const paneId = current ? current.id : currentName;
  const tierEl = document.getElementById('ask-tier');
  tierEl.textContent = !ask.visible ? '' : ask.held ? heldTierLine(ask.tier, currentName || paneId) : tierLine(ask.tier, currentName);
  document.getElementById('allow').hidden = ask.tier !== 'undoable';
  document.getElementById('ask-name').hidden = ask.tier === 'undoable';
  document.getElementById('deny').hidden = false;
  box.dataset.held = ask.held ? '1' : '';
  box.dataset.askId = ask.askId;
  box.dataset.tier = ask.tier;
  box.hidden = !ask.visible;
  const trust = document.getElementById('trust');
  if (trust) trust.hidden = !trustFor(st);
}

// answerTrust answers the trust screen of the open pane.
// Both buttons stay off while one answer is on its way, so a second tap
// cannot send a second answer.
async function answerTrust(yes) {
  if (!current) return;
  const buttons = ['trust-yes', 'trust-no'].map((id) => document.getElementById(id)).filter(Boolean);
  if (buttons.some((b) => b.disabled)) return;
  for (const b of buttons) b.disabled = true;
  try {
    await queue(trustCommand(current.id, yes));
    say(trustDone(yes));
  } catch (e) {
    say(e);
  } finally {
    for (const b of buttons) b.disabled = false;
  }
}

// lastPart is the last part of a directory.
const lastPart = (cwd) => String(cwd || '').split('/').filter(Boolean).pop() || '';

// paintStack draws the stack bar, the gate mark and an open segment's
// details from the pane's row, the way a window's header does.
function paintStack(row) {
  const line = document.getElementById('pane-stack');
  const gate = document.getElementById('pane-gate');
  const box = document.getElementById('pane-details');
  if (!line || !gate || !box) return;
  const facts = typeof window.coppice.facts === 'function' ? window.coppice.facts() : null;
  const segs = segments(row, facts);
  if (!segs.some((s) => s.key === openSeg)) openSeg = '';
  const toggle = (key) => {
    openSeg = !key || openSeg === key ? '' : key;
    paintStack(row);
  };
  const parts = [];
  if (segs.length) parts.push(stackBarEl(segs, { open: openSeg, lit: light(row), onSeg: toggle }));
  else if (row && row.harness && row.harness !== 'unknown') {
    const word = document.createElement('span');
    word.className = 'muted';
    word.textContent = row.harness;
    parts.push(word);
  }
  const cwd = document.createElement('span');
  cwd.className = 'muted cwd';
  cwd.textContent = lastPart(row && row.cwd);
  parts.push(cwd);
  line.replaceChildren(...parts);
  const mark = gateMark(row && row.gate);
  gate.replaceChildren(...(mark ? gateMarkEls(mark, () => toggle('daisugi')) : []));
  const pane = current ? current.id : '';
  const journal = typeof window.coppice.journal === 'function' && pane ? () => window.coppice.journal(pane) : null;
  box.replaceChildren(...(openSeg ? [detailsBox(details(openSeg, row, facts, Date.now() / 1000), { onJournal: journal, onClose: () => toggle(openSeg) })] : []));
  box.hidden = !openSeg;
}

// GONE is what a link to an ask the pane no longer shows says.
export const GONE = 'That ask is gone. The gate timed out, or another client answered it.';

// askMark says what a link to ask wanted finds on a pane whose ask box
// holds shown: 'here' when it is that ask, 'gone' when the pane is known
// and shows another ask or none, and '' when there is nothing to say yet.
export function askMark(wanted, shown, known) {
  if (!wanted) return '';
  if (shown === wanted) return 'here';
  return known ? 'gone' : '';
}

// marked is the link and the answer markAsk last acted on, so a repaint
// neither scrolls again nor says gone twice.
let marked = '';

// markAsk marks the ask box when the route names the ask it shows, and
// brings it into view once. A link to another ask marks nothing and says
// it is gone. known is false before the pane's row has landed.
function markAsk(known) {
  const box = document.getElementById('ask');
  const r = window.coppice.route(location.hash);
  const result = r.screen === 'pane' ? askMark(r.ask, box.dataset.askId, known) : '';
  const classes = String(box.className || '').split(/\s+/).filter((c) => c && c !== 'here');
  if (result === 'here') classes.push('here');
  box.className = classes.join(' ');
  const key = (r.ask || '') + ' ' + result;
  if (!result || key === marked) return;
  // An ask this link already found and that then left, answered here or
  // elsewhere, is not news.
  const found = marked === (r.ask || '') + ' here';
  marked = key;
  if (found) return;
  if (result === 'here') {
    if (typeof box.scrollIntoView === 'function') box.scrollIntoView({ block: 'center' });
  } else {
    say(GONE);
  }
}

// paintPane fills the header, the stack bar and the ask box from a
// pane.list row. info is undefined when the row is not known yet, and
// stateOf reads that the same way it reads a missing row: unknown, no ask.
function paintPane(paneId, info) {
  currentName = paneName(info || { id: paneId });
  paintKind(isHeadless(info));
  document.getElementById('pane-label').textContent = currentName;
  setAsk(stateOf(info));
  paintStack(info || null);
  markAsk(Boolean(info));
}

// paintKind shows the key drawer for an agent with a terminal, and for a
// headless one hides it and says the agent takes whole messages.
function paintKind(on) {
  headless = on;
  const toggle = document.getElementById('keys-toggle');
  const drawer = document.getElementById('keys');
  const note = document.getElementById('pane-note');
  if (toggle) toggle.hidden = on;
  if (drawer && on) {
    drawer.hidden = true;
    if (toggle) toggle.setAttribute('aria-expanded', 'false');
  }
  if (note) {
    note.textContent = on ? HEADLESS_NOTE : '';
    note.hidden = !on;
  }
  const box = document.getElementById('text');
  if (box) box.placeholder = on ? 'Message' : 'Type here';
}

// gone tells the page the open agent has ended, so nobody stays on a dead
// screen. The page takes the owner back to the floor with the line that
// says so.
function gone(paneId) {
  const info = (window.coppice.state.panes || []).find((p) => p && p.id === paneId);
  const label = (info && info.label) || currentName || paneId;
  window.dispatchEvent(new CustomEvent('coppice:gone', { detail: { pane: paneId, label } }));
}

// answer posts one decision for the ask the box shows. An allow on an
// ask that is not undoable needs the pane name typed in the name field,
// and anything else posts nothing.
async function answer(decision) {
  const box = document.getElementById('ask');
  const askId = box.dataset.askId;
  if (!askId) return;
  const body = { tool_use_id: askId, decision, reason: 'answered from the phone' };
  if (current) body.pane = current.id;
  if (decision === 'allow' && box.dataset.tier !== 'undoable') {
    const typed = String(document.getElementById('ask-name').value || '').trim();
    if (!currentName || typed !== currentName) {
      say('That is not the pane name. Type ' + (currentName || 'the pane name') + ' to allow.');
      return;
    }
    body.confirm = typed;
  }
  try {
    if (box.dataset.held) {
      // The harness holds this ask, so coppice-server answers it.
      if (!current) return;
      const req = heldAnswer(current.id, askId, decision, body.confirm);
      await window.coppice.rpc(req.cmd, req.fields);
    } else {
      await window.coppice.api('/api/ask/answer', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
    }
    say(decision === 'allow' ? 'Allowed.' : 'Denied.');
    box.dataset.askId = '';
    box.hidden = true;
  } catch (e) {
    say(e);
  }
}

// queue sends one request after every send before it has been answered.
function queue(req) {
  const { cmd, ...fields } = req;
  const sent = outbox.then(() => window.coppice.rpc(cmd, fields));
  outbox = sent.catch(() => {});
  return sent;
}

// typeText sends what the field holds. With enter it presses Enter after
// the text, or alone when the field is empty; without, it types the text
// and presses nothing. The field clears at once, so a second tap cannot
// send the text twice, and gets the text back if the send fails.
async function typeText(enter) {
  const box = document.getElementById('text');
  const text = String(box.value || '');
  if (!current) return;
  const row = { ...current, kind: headless ? 'headless' : 'pty' };
  const req = enter ? enterFor(row, text) : typeFor(row, text);
  if (!req) return;
  box.value = '';
  try {
    const res = await queue(req);
    const line = queuedLine(res);
    if (line) say(line);
  } catch (e) {
    if (!box.value) box.value = text;
    say(isQuestionRefusal(e) ? questionMessage(e.message, current && current.id) : e);
  }
}

// close detaches whatever pane is open. Leaving a pane attached while the
// operator is on another screen keeps frames streaming to a phone that is
// not showing them, which costs the battery the whole feature exists to
// save. It returns a promise that settles once the detach is answered.
export function close() {
  if (!current) return Promise.resolve();
  const id = current.id;
  current = null;
  grid = null;
  return window.coppice.rpc('pane.detach', { pane: id }).catch(() => {});
}

// detached clears current and the grid the way close does, but sends
// nothing: the socket that would have carried pane.detach is already gone.
// Call this when the socket closes. Without it, a reconnect while parked
// on a pane finds current still set to the pane it was showing, open()'s
// same-pane guard reads that as the same repeat route event it exists to
// ignore, and no pane.attach is ever sent on the new socket. The grid then
// stays frozen on the last frame before the drop until the operator routes
// away and back.
export function detached() {
  current = null;
  grid = null;
}

// release hands the open pane back to the floor. It clears current with
// no detach and returns the pane id and its grid, or null when no pane is
// open. The floor adopts the attach and the server never sees a change.
export function release() {
  if (!current) return null;
  const handed = { id: current.id, grid };
  current = null;
  grid = null;
  return handed;
}

// open shows one pane. seed is the grid the floor's tile held for this
// pane, when the floor hands its attach over: the picture starts from it
// and later delta frames apply to it. The attach is still sent, and the
// server answers already-attached with the pump still running, so no
// detach and attach pair for one pane ever meets on the wire.
export async function open(paneId, seed) {
  if (current && current.id === paneId) return;   // a repeat route event must not blank the grid
  if (current) close();
  current = { id: paneId };
  marked = '';
  grid = seed || null;
  follow = {};
  openSeg = '';
  // Another agent's sends never wait on this one's.
  outbox = Promise.resolve();
  const wrap = document.getElementById('canvas-wrap');
  if (wrap) {
    wrap.scrollTop = 0;
    wrap.scrollLeft = 0;
  }
  const info = (window.coppice.state.panes || []).find((p) => p.id === paneId);
  seen = Boolean(info);
  paintPane(paneId, info);
  repaint();
  const req = attachCommand(current);
  const { cmd, ...fields } = req;
  try {
    await window.coppice.rpc(cmd, fields);
  } catch (e) {
    // A failed attach must not latch current: the socket may still be
    // connecting, and the next coppice:open is the retry. A pane that
    // ended is gone, and the page leaves it.
    if (current && current.id === paneId) current = null;
    if (e && (e.code === 'pane_closed' || e.code === 'no_such_pane')) gone(paneId);
    else say(e);
  }
}

export function onEvent(msg) {
  if (msg.event === 'frame' && current && msg.pane === current.id) {
    grid = applyFrame(grid, msg);
    schedule();
    return;
  }
  if (msg.event === 'state') {
    // A state event is already flat. Wrapping it would break stateOf the
    // same way reading pane.list as nested does.
    const mine = current && msg.pane === current.id;
    if (mine) {
      setAsk(stateOf(msg));
      markAsk(true);
    }
    window.dispatchEvent(new CustomEvent('coppice:state', { detail: msg }));
    if (mine && msg.state === 'done') gone(msg.pane);
  }
}

// holdFocus keeps the focus where it is when a button is pressed, so a
// tap on a drawer key or Enter does not close the phone's keyboard.
function holdFocus(b) {
  b.addEventListener('pointerdown', (e) => { if (e && typeof e.preventDefault === 'function') e.preventDefault(); });
}

export function mountPane() {
  document.getElementById('compose').addEventListener('submit', (e) => {
    if (e && typeof e.preventDefault === 'function') e.preventDefault();
    typeText(false);
  });
  const enter = document.getElementById('send-enter');
  holdFocus(enter);
  enter.addEventListener('click', () => typeText(true));
  document.getElementById('allow').addEventListener('click', () => answer('allow'));
  document.getElementById('deny').addEventListener('click', () => answer('deny'));
  const trustYes = document.getElementById('trust-yes');
  if (trustYes) {
    trustYes.textContent = TRUST_YES.label;
    trustYes.title = TRUST_YES.title;
    trustYes.addEventListener('click', () => answerTrust(true));
  }
  const trustNo = document.getElementById('trust-no');
  if (trustNo) {
    trustNo.textContent = TRUST_NO.label;
    trustNo.title = TRUST_NO.title;
    trustNo.addEventListener('click', () => answerTrust(false));
  }
  document.getElementById('ask-name').addEventListener('keydown', (e) => {
    if (e.key === 'Enter') answer('allow');
  });
  mountRecord();

  const drawer = document.getElementById('keys');
  drawer.replaceChildren(...KEYS.map((k) => {
    const b = document.createElement('button');
    b.type = 'button';
    b.textContent = k.label;
    b.title = k.title;
    b.dataset.key = k.key;
    b.setAttribute('aria-label', k.title);
    holdFocus(b);
    b.addEventListener('click', () => {
      if (!current || headless) return;
      queue(keysCommand(current, k.key)).catch(say);
    });
    return b;
  }));
  const toggle = document.getElementById('keys-toggle');
  toggle.addEventListener('click', () => {
    drawer.hidden = !drawer.hidden;
    toggle.setAttribute('aria-expanded', drawer.hidden ? 'false' : 'true');
  });

  // A new width, from a turned phone, draws the grid at a new cell.
  window.addEventListener('resize', () => { if (current) repaint(); });

  // The route change is app.js's to drive: it decides whether the floor
  // hands this pane over or the pane screen attaches on its own.

  // The socket is not open yet when boot() dispatches the first route
  // event, so a pane reached by a cold deep link never attaches from that
  // event alone. Once the socket opens, retry the attach for whatever pane
  // the hash still names.
  window.addEventListener('coppice:open', () => {
    const r = window.coppice.route(location.hash);
    if (r.screen === 'pane') open(r.pane);
  });

  // A pane reached by a cold deep link has no row to read until the first
  // pane.list lands. Once it does, repaint the header and the ask box from
  // it, which is the only way that pane ever learns it is blocked. A pane
  // a list held that a later list lacks has ended.
  window.addEventListener('coppice:panes', () => {
    if (!current) return;
    const info = (window.coppice.state.panes || []).find((p) => p.id === current.id);
    if (info) {
      seen = true;
      paintPane(current.id, info);
    } else if (seen) {
      gone(current.id);
    }
  });
  // The roster merges a state event into its row after this file saw it,
  // so the stack bar and the gate mark draw from the merged row here.
  window.addEventListener('coppice:state', (e) => {
    const msg = e && e.detail;
    if (!current || !msg || msg.pane !== current.id) return;
    const info = (window.coppice.state.panes || []).find((p) => p.id === current.id);
    if (info) paintStack(info);
  });
}

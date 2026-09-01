import { stateOf, askFor, answerBody, denyButton, allowControl, tierLine, verdictLine, needsYou, heldLine, heldTierLine, heldAnswer } from './pane.js';
import { ORDER, stateChip } from './chips.js';
import { railEntries, countParts, recentLine, rowFromEvent, ROW_TYPE } from './rail.js';
import { MESSAGES } from './messages.js';
import { phoneQueue } from './dock.js';

// unreadable is the error a pane list reply the page cannot read throws.
// It carries its fix.
function unreadable() {
  const m = MESSAGES.unreadable();
  const e = new Error(m.text);
  e.fix = m;
  return e;
}

// The roster: the rail of live agents as a tree, blocked first in each
// group, each with a word as well as a colour, and Recent at its foot.

export { stateChip };

// floor is the mounted floor page, or null before mountFloor runs. A row
// click goes to a tile when the floor can show one, and to the pane
// screen when it cannot.
let floor = null;

export function setFloor(f) { floor = f; }

const canTile = () => floor !== null && floor.canShow();

// EMPTY_TEXT is what the rail says with no pane at all.
const EMPTY_TEXT = 'No agents yet. Press New to start one.';

export function sortPanes(panes) {
  return panes
    .map((p, i) => [p, i])
    .sort((a, b) => {
      const d = (ORDER[a[0].state] ?? 5) - (ORDER[b[0].state] ?? 5);
      return d !== 0 ? d : a[1] - b[1];
    })
    .map((e) => e[0]);
}

export function ageLabel(seconds) {
  const s = Math.max(0, Math.floor(seconds));
  if (s < 60) return s + 's';
  if (s < 3600) return Math.floor(s / 60) + 'm';
  if (s < 86400) return Math.floor(s / 3600) + 'h';
  return Math.floor(s / 86400) + 'd';
}

function make(tag, className, text) {
  const el = document.createElement(tag);
  if (className) el.className = className;
  if (text !== undefined) el.textContent = text;
  return el;
}

// paneHash is the pane screen's address. With an ask id it lands on that
// ask.
export function paneHash(pane, askId) {
  return '#/pane/' + encodeURIComponent(pane) + (askId ? '?ask=' + encodeURIComponent(askId) : '');
}

// onOpen wires a card or a row to open its pane. A click puts the agent in
// a window and gives that window the keys. With no floor the click opens
// the pane screen. A double click on the name renames the agent in place.
// A drag carries the agent to a window.
function onOpen(li, pane, name) {
  li.tabIndex = 0;
  li.dataset.pane = pane.id;
  li.dataset.row = pane.id;
  li.addEventListener('click', (e) => {
    if (canTile()) {
      // The first click of a double click redraws the rail, so the second
      // lands on a new row and names itself by its count.
      if (e && e.detail >= 2 && name && e.target === name) {
        floor.startRename(pane.id, 'row');
        return;
      }
      if (e && e.ctrlKey) floor.openRow(pane.id);
      else floor.fillRow(pane.id);
      return;
    }
    location.hash = paneHash(pane.id);
  });
  li.addEventListener('auxclick', (e) => {
    if (e && e.button === 1 && canTile()) floor.openRow(pane.id);
  });
  if (name) {
    name.addEventListener('dblclick', (e) => {
      if (e && typeof e.stopPropagation === 'function') e.stopPropagation();
      if (canTile() && floor.editing() === null) floor.startRename(pane.id, 'row');
    });
  }
  li.draggable = true;
  li.addEventListener('dragstart', (e) => {
    if (!e || !e.dataTransfer) return;
    e.dataTransfer.effectAllowed = 'move';
    e.dataTransfer.setData(ROW_TYPE, pane.id);
    e.dataTransfer.setData('text/plain', pane.label || pane.id);
  });
  return li;
}

// killButton is the x that asks to stop an agent. Its click stays off the
// row, so it never also puts the agent in a window.
export function killButton(id, label, ask) {
  const b = make('button', 'kill', '×');
  b.type = 'button';
  b.title = 'Stop ' + label;
  if (typeof b.setAttribute === 'function') b.setAttribute('aria-label', 'Stop ' + label);
  b.addEventListener('click', (e) => {
    if (e && typeof e.stopPropagation === 'function') e.stopPropagation();
    ask(id);
  });
  return b;
}

// confirmText is what the kill confirm says.
export function confirmText(label) {
  return 'Stop ' + label + '? Enter stops it, Esc keeps it.';
}

// clearAllText is what the Clear all confirm says for n ended agents.
export function clearAllText(n) {
  if (n === 1) return 'Forget 1 ended agent? Enter forgets it, Esc keeps it.';
  return 'Forget ' + n + ' ended agents? Enter forgets them, Esc keeps them.';
}

// Words a blocked card with no ask shows, by what found the block. Only
// the gate's own block blames the gate.
export const BLOCKED_BY_GATE = 'Blocked. The gate gave no detail.';
export const BLOCKED_OWN_QUESTION = "Waiting on the agent's own question. Open it to answer.";

// blockedLine is what a blocked card with no ask says.
export function blockedLine(source) {
  return source === 'gate' ? BLOCKED_BY_GATE : BLOCKED_OWN_QUESTION;
}

// confirmLine is the one line a kill asks before it stops an agent, with
// a Stop and a Keep for the mouse.
export function confirmLine(label, stop, keep) {
  const box = make('div', 'confirm');
  box.dataset.confirm = '1';
  const btn = (text, cls, fn) => {
    const b = make('button', cls, text);
    b.type = 'button';
    b.addEventListener('click', (e) => {
      if (e && typeof e.stopPropagation === 'function') e.stopPropagation();
      fn();
    });
    return b;
  };
  box.append(make('span', 'confirm-text', confirmText(label)), btn('Stop', 'stop', stop), btn('Keep', 'keep ghost', keep));
  return box;
}

// renameInput is the field of the rename in progress, or null.
let renameInput = null;

// editInPlace puts a field in place of el's text. Enter or leaving the
// field saves a changed name, Esc keeps the old one. end runs first, once,
// and save runs after it with the new name when there is one.
export function editInPlace(el, value, save, end) {
  const input = make('input', 'rename');
  input.type = 'text';
  input.value = value;
  if (typeof input.setAttribute === 'function') input.setAttribute('aria-label', 'New name for ' + value);
  let over = false;
  const finish = (commit) => {
    if (over) return;
    over = true;
    if (renameInput === input) renameInput = null;
    const next = String(input.value || '').trim();
    end();
    if (commit && next && next !== value) save(next);
  };
  input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' || e.key === 'Escape') {
      if (typeof e.preventDefault === 'function') e.preventDefault();
      finish(e.key === 'Enter');
    }
    if (typeof e.stopPropagation === 'function') e.stopPropagation();
  });
  input.addEventListener('blur', () => finish(true));
  const still = (e) => { if (e && typeof e.stopPropagation === 'function') e.stopPropagation(); };
  input.addEventListener('click', still);
  input.addEventListener('dblclick', still);
  input.addEventListener('mousedown', still);
  el.replaceChildren(input);
  renameInput = input;
  return input;
}

// focusRename gives the rename field the keys once it is on the page.
export function focusRename() {
  if (!renameInput) return;
  if (typeof renameInput.focus === 'function') renameInput.focus();
  if (typeof renameInput.select === 'function') renameInput.select();
}

// renaming is the open rename field, or null.
export const renaming = () => renameInput;

// windowMark is the small number a row shows when its agent is in a
// window, or null.
function windowMark(id) {
  const num = floor !== null && typeof floor.windowOf === 'function' ? floor.windowOf(id) : 0;
  return num > 0 ? make('span', 'win', String(num)) : null;
}

// markSelected gives the selected rail row its border while the rail has
// the keys.
function markSelected(li, id) {
  const sel = floor !== null && typeof floor.railSelected === 'function' ? floor.railSelected() : '';
  if (sel && sel === id) {
    li.className += ' selected';
    li.dataset.selected = '1';
    if (typeof li.setAttribute === 'function') li.setAttribute('aria-current', 'true');
  }
  return li;
}

function ageText(pane) {
  return pane.age === null || pane.age === undefined ? '' : ageLabel(pane.age);
}

// harnessOf is the harness a row names, or null when the server does not
// know it.
const harnessOf = (pane) => (pane.harness && pane.harness !== 'unknown' ? make('span', 'muted harness', pane.harness) : null);

// nameOf is a row's name, with the rename field in it while it is renamed.
function nameOf(pane) {
  const name = make('strong', 'label', pane.label || pane.id);
  const ed = floor !== null && typeof floor.editing === 'function' ? floor.editing() : null;
  if (ed && ed.where === 'row' && ed.pane === pane.id) {
    editInPlace(name, pane.label || pane.id, (next) => floor.rename(pane.id, next), () => floor.endRename());
  }
  return name;
}

// tail is the end of a row: its window number and the x that stops it.
function tail(pane) {
  const out = [windowMark(pane.id)];
  if (floor !== null && typeof floor.askKill === 'function') out.push(killButton(pane.id, pane.label || pane.id, floor.askKill));
  return out;
}

// indent moves a line under its group.
function indent(li, depth) {
  if (depth > 0) li.style.marginLeft = (depth * 0.8) + 'rem';
  return li;
}

// askCard is one blocked pane in the queue: who asks, the summary in code,
// the gate line when the gate gave one, and the answer controls.
function askCard(pane) {
  const li = make('li', 'card ask');
  const who = make('div', 'who');
  const name = nameOf(pane);
  who.append(...[make('span', 'dot dot-blocked'), name, harnessOf(pane), make('span', 'muted', ageText(pane)), ...tail(pane)].filter(Boolean));
  li.append(who);
  if (pane.askSummary) li.append(make('code', 'summary', pane.askSummary));
  else li.append(make('p', 'muted', blockedLine(pane.source)));
  const verdict = verdictLine({ gate: pane.gate });
  if (verdict) li.append(make('p', 'verdict', verdict));
  if (pane.askId) li.append(queueControls(pane));
  return markSelected(onOpen(li, pane, name), pane.id);
}

// row is one pane that does not need the operator, in one compact line:
// its state dot, its name, its harness when known, its window number, and
// the x that stops it. The dot's shape and colour say the state, a filled
// dot for work and a ring for idle, and its word is there for a screen
// reader and on hover. A held ask adds the line that says
// whose foreman hears it.
function row(pane) {
  const chip = stateChip(pane.state);
  const li = make('li', 'card row');
  const dot = make('span', 'dot dot-' + chip.word);
  dot.title = chip.word;
  const name = nameOf(pane);
  if (pane.state === 'blocked') li.className += ' amber';
  li.append(...[dot, name, make('span', 'state visually-hidden', chip.word), harnessOf(pane), ...tail(pane)].filter(Boolean));
  if (pane.held) li.append(make('span', 'muted held', pane.held));
  return markSelected(onOpen(li, pane, name), pane.id);
}

// confirmRow is a row whose agent waits for the kill confirm.
function confirmRow(pane) {
  const li = make('li', 'card row confirming');
  li.dataset.row = pane.id;
  li.dataset.pane = pane.id;
  li.append(confirmLine(pane.label || pane.id, () => floor.stopKill(true), () => floor.stopKill(false)));
  return markSelected(li, pane.id);
}

// groupLine is a task or a project: a button that folds it, its name, and
// how many of its agents are in each state.
function groupLine(g) {
  const li = make('li', 'group' + (g.counts.blocked ? ' has-ask' : ''));
  li.dataset.group = g.key;
  const b = make('button', 'fold');
  b.type = 'button';
  const parts = countParts(g.counts);
  const words = parts.length ? parts.map((p) => p.words).join(', ') : 'no agents';
  if (typeof b.setAttribute === 'function') {
    b.setAttribute('aria-expanded', String(!g.folded));
    b.setAttribute('aria-label', g.label + ', ' + words);
  }
  b.title = (g.kind === 'task' ? 'Task ' : 'Project ') + g.label + ': ' + words;
  const counts = make('span', 'counts');
  for (const p of parts) {
    const n = make('span', 'n n-' + p.state);
    if (p.state !== 'blocked') n.append(make('span', 'dot dot-' + p.state));
    n.append(p.text);
    counts.append(n);
  }
  if (!parts.length) counts.append(make('span', 'n muted', 'empty'));
  b.append(make('span', 'caret', g.folded ? '▸' : '▾'), make('span', 'glabel', g.label), counts);
  b.addEventListener('click', (e) => {
    if (e && typeof e.stopPropagation === 'function') e.stopPropagation();
    if (floor !== null && typeof floor.toggleGroup === 'function') floor.toggleGroup(g.key);
  });
  li.append(b);
  return indent(li, g.depth);
}

// entryItem draws one line of the rail tree. On a phone the asks sit in
// the queue at the top, so a blocked agent is one compact row in its group.
function entryItem(e, phone) {
  if (e.type === 'group') return groupLine(e);
  const pane = e.row;
  const pending = floor !== null && typeof floor.killPending === 'function' ? floor.killPending() : '';
  if (pending && pending === pane.id) return indent(confirmRow(pane), e.depth);
  return indent(pane.state === 'blocked' && !phone ? askCard(pane) : row(pane), e.depth);
}

// queueCounts counts the rows by state.
export function queueCounts(rows) {
  const c = { blocked: 0, working: 0, idle: 0, unknown: 0, done: 0 };
  for (const r of rows || []) {
    const s = stateChip(r.state).word;
    c[s] += 1;
  }
  return c;
}

// queueTitle is the head of the queue: how many panes need the operator.
export function queueTitle(counts) {
  const n = counts.blocked || 0;
  return n + (n === 1 ? ' needs you' : ' need you');
}

// queueSub is the line under the title: the working count, then each other
// state that has a pane.
export function queueSub(counts) {
  const parts = [(counts.working || 0) + ' working'];
  for (const s of ['idle', 'unknown', 'done']) if (counts[s]) parts.push(counts[s] + ' ' + s);
  return parts.join(' · ');
}

// queueControls is the answer row on a phone card: Deny, big and first,
// then Look, then Allow on an undoable ask or a field for the pane name on
// a permanent one. Every control keeps its click off the card, so a tap
// answers and never also opens a tile.
function queueControls(pane) {
  const box = document.createElement('div');
  box.className = 'queue tier-' + pane.tier;
  const line = document.createElement('p');
  line.className = 'tier muted';
  line.textContent = pane.askHeld ? heldTierLine(pane.tier, pane.label) : tierLine(pane.tier, pane.label);
  const status = (msg) => window.coppice.status(msg);
  const post = async (decision, confirm) => {
    const body = answerBody(pane.askId, decision, { pane: pane.id, confirm });
    try {
      if (pane.askHeld) {
        // The harness holds this ask, so coppice-server answers it.
        const req = heldAnswer(pane.id, pane.askId, decision, confirm);
        await window.coppice.rpc(req.cmd, req.fields);
      } else {
        await window.coppice.api('/api/ask/answer', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        });
      }
      status(decision === 'deny' ? 'Denied.' : 'Allowed.');
      box.hidden = true;
    } catch (e) {
      status(e.message);
    }
  };
  const look = document.createElement('button');
  look.className = 'look';
  look.textContent = 'Look';
  // On the wide floor Look puts the agent in a window with the keys; with
  // no window it opens the pane screen at the ask.
  look.addEventListener('click', (e) => {
    if (e && typeof e.stopPropagation === 'function') e.stopPropagation();
    if (floor !== null && typeof floor.canShow === 'function' && floor.canShow()) floor.fillRow(pane.id);
    else location.hash = paneHash(pane.id, pane.askId);
  });
  box.append(
    line,
    denyButton(() => post('deny'), 'deny big'),
    look,
    allowControl(pane.tier, pane.label, (confirm) => post('allow', confirm), status, pane.askId),
  );
  return box;
}

// UNKNOWN_ASK is the last ask line before the ring answers, or when it
// cannot.
const UNKNOWN_ASK = 'last ask unknown';

// lastAskLine is the quiet screen's last line. clock is what askClock
// knows: known is false when the ring did not answer, last is the time of
// the newest blocked event the ring held, and from is the time since which
// the ring holds every event. A span the ring did not see is unknown, so
// only an ask inside the unbroken span counts as the last one.
export function lastAskLine(clock, nowSeconds) {
  if (!clock || !clock.known || typeof clock.from !== 'number') return UNKNOWN_ASK;
  // An ask from before from may not be the newest: the span before from
  // was not seen whole.
  if (typeof clock.last === 'number' && clock.last >= clock.from) return 'last ask ' + ageLabel(nowSeconds - clock.last) + ' ago';
  return 'no ask in the last ' + ageLabel(nowSeconds - clock.from);
}

// ASK_OVERLAP is how many seconds before the newest event it holds each
// later read of the ring starts. Each event carries its own reporter's
// time, so an event can land after one stamped later, and a read from the
// newest time alone would never see it.
export const ASK_OVERLAP = 300;

// askClock reads the server's event ring for the newest ask. The first
// read asks for the whole ring, and each later read for the events after
// the newest it holds, less ASK_OVERLAP. A refusal makes the clock unknown
// until the next read answers. A refused token stops the reads, so a phone
// with a stale token never earns a ban from a clock. restart lets the
// reads go again once a new token is in use.
export function askClock(api) {
  let top = 0;
  let last = null;
  let from = null;
  let known = false;
  let stopped = false;
  const read = async () => {
    if (stopped) return;
    try {
      const since = top > 0 ? Math.max(0, top - ASK_OVERLAP) : 0;
      const body = await api('/api/events?since=' + since);
      const events = Array.isArray(body && body.events) ? body.events : [];
      for (const e of events) {
        if (!e || typeof e.ts !== 'number') continue;
        top = Math.max(top, e.ts);
        if (e.state === 'blocked' && (last === null || e.ts > last)) last = e.ts;
      }
      from = body && typeof body.from === 'number' ? body.from : null;
      known = from !== null;
    } catch (e) {
      known = false;
      if (e && e.status === 401) stopped = true;
    }
  };
  const restart = () => { stopped = false; };
  return { read, restart, state: () => ({ known, last, from }) };
}

// quietBlock fills the quiet screen: nothing needs the operator, how many
// panes work, and the last ask line.
function quietBlock(el, counts, askLine) {
  const working = counts.working || 0;
  const title = document.createElement('p');
  title.className = 'quiet-title';
  title.textContent = 'Nothing needs you.';
  const count = document.createElement('p');
  count.textContent = working + ' working. I will ping when one asks.';
  const last = document.createElement('p');
  last.className = 'muted';
  last.textContent = askLine || UNKNOWN_ASK;
  el.replaceChildren(title, count, last);
}

// render draws the rail. With a pane blocked, the head says how many need
// the operator. With none blocked, the quiet screen says so. The agents
// follow as a tree: tasks with their agents under them, then the agents
// with no task by project, blocked first in each group. An empty list is
// hidden, never drawn. opts.askLine is the quiet screen's last ask line.
// With opts.phone the agents that need you sit in a small queue at the top,
// each with its ask, and their rows in the groups stay one line.
// While a row is renamed the rail is not drawn again, so the field keeps
// its keys.
export function render(panes, opts = {}) {
  const phone = Boolean(opts.phone);
  const ed = floor !== null && typeof floor.editing === 'function' ? floor.editing() : null;
  const keepRow = renameInput !== null && ed && ed.where === 'row' ? ed.pane : '';
  const list = document.getElementById('roster');
  const empty = document.getElementById('roster-empty');
  const head = document.getElementById('queue-head');
  const quiet = document.getElementById('quiet');
  const gone = floor !== null && typeof floor.isGone === 'function' ? floor.isGone : () => false;
  const live = (panes || []).filter((p) => !gone(p.id));
  const counts = queueCounts(live);
  const tasks = floor !== null && typeof floor.tasks === 'function' ? floor.tasks() : [];
  const folded = floor !== null && typeof floor.folded === 'function' ? floor.folded() : new Set();
  const entries = railEntries(live, tasks, folded);
  const sig = railSignature(entries, keepRow) + (phone ? '|phone' : '');
  const same = drawn.list === list && drawn.sig === sig;
  if (!same) {
    drawn.list = list;
    drawn.sig = sig;
    const old = keepRow ? Array.from(list.children || []).find((c) => c.dataset && c.dataset.row === keepRow) : null;
    const items = entries.map((e) => (old && e.type === 'row' && e.row.id === keepRow ? old : entryItem(e, phone)));
    placeItems(list, items, keepRow);
    if (!keepRow && renameInput !== null) focusRename();
    const picked = items.find((li) => li.dataset.selected === '1');
    if (picked && typeof picked.scrollIntoView === 'function') picked.scrollIntoView({ block: 'nearest' });
  }
  list.hidden = entries.length === 0;
  renderNeeds(phone ? phoneQueue(live) : []);
  empty.hidden = live.length > 0;
  empty.textContent = EMPTY_TEXT;
  head.hidden = counts.blocked === 0;
  document.getElementById('queue-title').textContent = queueTitle(counts);
  document.getElementById('queue-sub').textContent = queueSub(counts);
  quiet.className = 'quiet';
  quiet.hidden = counts.blocked > 0 || live.length === 0;
  if (!quiet.hidden) quietBlock(quiet, counts, opts.askLine);
}

// drawn is the list the rail last drew into and what it drew, so a draw
// with nothing new keeps the rows, their focus and a click in progress.
const drawn = { list: null, sig: '' };

// needsDrawn is the same for the phone's queue.
const needsDrawn = { list: null, sig: '' };

// renderNeeds draws the phone's queue: one ask card per agent that needs
// you, with Deny, Look and Allow or the name field. A draw with nothing new
// keeps the cards, so a name typed in a field keeps its focus.
function renderNeeds(rows) {
  const box = document.getElementById('needs');
  if (!box) return;
  const sig = JSON.stringify(rows.map((r) => [r.id, r.label, r.harness, r.askSummary, r.askId, r.tier, r.askHeld, r.gate, ageText(r)]));
  box.hidden = rows.length === 0;
  if (needsDrawn.list === box && needsDrawn.sig === sig) return;
  needsDrawn.list = box;
  needsDrawn.sig = sig;
  box.replaceChildren(...rows.map((r) => askCard(r)));
}

// railSignature is everything a rail draw shows, as one string.
function railSignature(entries, keepRow) {
  const f = floor;
  const win = (id) => (f !== null && typeof f.windowOf === 'function' ? f.windowOf(id) : 0);
  const sel = f !== null && typeof f.railSelected === 'function' ? f.railSelected() : '';
  const kill = f !== null && typeof f.killPending === 'function' ? f.killPending() : '';
  const ed = f !== null && typeof f.editing === 'function' ? f.editing() : null;
  return JSON.stringify([sel, kill, ed, keepRow, entries.map((e) => (e.type === 'group'
    ? ['g', e.key, e.label, e.depth, e.folded, e.counts]
    : ['r', e.row.id, e.row.label, e.row.state, e.row.harness, e.row.held, e.row.askSummary, e.row.askId,
      e.row.tier, e.row.askHeld, e.row.gate, ageText(e.row), e.depth, win(e.row.id)]))]);
}

// placeItems puts items in list. A row being renamed stays where it is in
// the page, since a field taken out of the page loses its keys: only the
// rows around it are taken out and put back.
function placeItems(list, items, keepRow) {
  const kids = Array.from(list.children || []);
  const kept = keepRow ? kids.find((c) => c.dataset && c.dataset.row === keepRow && items.includes(c)) : null;
  if (!kept || typeof list.insertBefore !== 'function' || typeof list.removeChild !== 'function') {
    list.replaceChildren(...items);
    return;
  }
  for (const c of kids) if (c !== kept) list.removeChild(c);
  const at = items.indexOf(kept);
  for (const li of items.slice(0, at)) list.insertBefore(li, kept);
  list.append(...items.slice(at + 1));
}

// renderRecent fills the Recent fold with the ended agents, newest first:
// each with its line, Resume and Forget. With none the fold is hidden. The
// fold's open state is the reader's and is kept. act holds resume and
// forget for one record.
export function renderRecent(ended, act, nowSeconds) {
  const box = document.getElementById('recent');
  const list = document.getElementById('recent-list');
  const count = document.getElementById('recent-count');
  if (!box || !list) return;
  const recs = Array.isArray(ended) ? ended.filter((r) => r && r.id) : [];
  box.hidden = recs.length === 0;
  if (count) count.textContent = String(recs.length);
  const btn = (text, cls, fn) => {
    const b = make('button', cls, text);
    b.type = 'button';
    b.addEventListener('click', (e) => {
      if (e && typeof e.stopPropagation === 'function') e.stopPropagation();
      fn();
    });
    return b;
  };
  list.replaceChildren(...recs.map((rec) => {
    const amber = Number.isInteger(rec.exit_code) && rec.exit_code !== 0;
    const li = make('li', 'recent-row' + (amber ? ' amber' : ''));
    li.dataset.recent = rec.id;
    const text = make('div', 'recent-text');
    text.append(make('strong', 'label', rec.label || rec.id), make('span', 'muted', recentLine(rec, nowSeconds)));
    li.append(text, btn('Resume', 'resume', () => act.resume(rec)), btn('Forget', 'forget ghost', () => act.forget(rec)));
    return li;
  }));
}

// paneRows turns a flat pane.list row into what a card needs. The row's ts
// is the merged event's timestamp. A pane the state store has never seen
// has none, and its age stays blank rather than reading as zero seconds
// old. A blocked pane whose ask a foreman holds reads as working, with
// held set to its waiting line, so it sorts and counts as working.
export function paneRows(raw, nowSeconds) {
  return (raw || []).map((p) => {
    const st = stateOf(p);
    const held = st.state === 'blocked' && p.held ? heldLine(p.held, nowSeconds) : '';
    return {
      id: p.id,
      label: p.label || p.id,
      state: held ? 'working' : st.state,
      held,
      source: st.source,
      harness: st.harness,
      askSummary: askFor(st).summary,
      askId: askFor(st).askId,
      tier: askFor(st).tier,
      askHeld: askFor(st).held,
      gate: st.ask && st.ask.gate ? st.ask.gate : null,
      age: st.ts ? Math.max(0, nowSeconds - st.ts) : null,
      task: p.task || '',
      cwd: p.cwd || '',
    };
  });
}

// panesFrom reads the panes list off a pane.list result, and returns null
// when result itself is not a real object or carries no panes array.
// coppice-server always sets panes, to an empty array when there are none,
// so a missing or non-array panes is not "no panes yet"; it is a reply that
// cannot be read, and it must not paint as an empty roster. rpc() resolves
// a reply with no result key at all as {}, which would otherwise look
// exactly like a real, deliberately empty result.
export function panesFrom(result) {
  if (!result || typeof result !== 'object' || Array.isArray(result)) return null;
  if (!Array.isArray(result.panes)) return null;
  return result.panes;
}

// ASK_READ_MS is the shortest time between two reads of the event ring
// for the quiet screen.
const ASK_READ_MS = 5000;

// ENDED_READ_MS is the longest a refresh goes without reading the ended
// list.
const ENDED_READ_MS = 10000;

// phoneNow is true while the page is a phone, and overviewNow while the
// overview is on screen: the floor, or a phone's sheet over it.
const phoneNow = () => typeof window.coppice.phone === 'function' && window.coppice.phone();
const overviewNow = () => (typeof window.coppice.overview === 'function'
  ? window.coppice.overview()
  : window.coppice.route(location.hash).screen === 'roster');

export function mountRoster() {
  let raw = [];
  let ticker = null;
  const clock = askClock((path) => window.coppice.api(path));
  let readAt = -Infinity;
  let reading = false;

  const quietNow = () => raw.length > 0 && !raw.some(needsYou);

  const paint = () => {
    const now = Date.now();
    render(paneRows(raw, now / 1000), { askLine: lastAskLine(clock.state(), now / 1000), phone: phoneNow() });
    // The quiet screen reads the ring for its last ask line, and only while
    // it shows. With no token there is nothing to read with.
    const home = overviewNow();
    if (!home || !quietNow() || reading || !window.coppice.state.token || now - readAt < ASK_READ_MS) return;
    reading = true;
    readAt = now;
    clock.read().finally(() => {
      reading = false;
      render(paneRows(raw, Date.now() / 1000), { askLine: lastAskLine(clock.state(), Date.now() / 1000), phone: phoneNow() });
    });
  };

  // setPanes is the one place window.coppice.state.panes changes. The pane
  // screen listens for coppice:panes to refresh its own header and ask box
  // once a fresh list lands, which is the only way a pane reached by a
  // cold deep link ever learns its real state.
  const setPanes = (panes) => {
    raw = panes;
    window.coppice.state.panes = panes;
    paint();
    window.dispatchEvent(new CustomEvent('coppice:panes'));
  };

  // firstPaint reads the plain HTTP endpoint, which answers before the
  // websocket handshake finishes. A refusal here is a real error and goes
  // to the status line, never to the empty banner.
  const firstPaint = async () => {
    try {
      const result = await window.coppice.api('/api/panes');
      const panes = panesFrom(result);
      if (panes === null) throw unreadable();
      setPanes(panes);
    } catch (e) {
      window.coppice.status(e);
    }
  };

  // refresh asks the live socket. It only runs once the socket is open, so
  // a cold start never blames the operator's token for a handshake that is
  // still in progress.
  const refresh = async () => {
    try {
      const result = await window.coppice.rpc('pane.list');
      const panes = panesFrom(result);
      if (panes === null) throw unreadable();
      setPanes(panes);
    } catch (e) {
      window.coppice.status(e);
      return;
    }
    readTasks();
    if (endedDue || Date.now() - endedAt >= ENDED_READ_MS) readEnded();
  };

  // ended is the last list of ended agents, for Recent. A refused read
  // keeps the last one. The list changes only when an agent ends or a
  // Recent action runs, so a refresh reads it only then, or once it is
  // ENDED_READ_MS old.
  let ended = [];
  let endedDue = true;
  let endedAt = -Infinity;
  const paintRecent = () => renderRecent(ended, recentAct, Date.now() / 1000);
  const readEnded = async () => {
    endedDue = false;
    endedAt = Date.now();
    try {
      const result = await window.coppice.rpc('pane.list', { ended: true });
      const list = panesFrom(result);
      if (list === null) return;
      ended = list;
      paintRecent();
    } catch {
      // Recent keeps its last list.
    }
  };

  // Each Recent action says what it did on the status line and reads the
  // lists again.
  const act = async (cmd, fields, say) => {
    endedDue = true;
    try {
      const result = await window.coppice.rpc(cmd, fields);
      window.coppice.status(say(result || {}));
    } catch (e) {
      window.coppice.status(e.message);
    }
  };
  const name = (rec) => rec.label || rec.id;
  const resumeOne = (rec) => act('pane.resume', { pane: rec.id },
    (r) => (r.resumed === false ? 'Started ' + name(rec) + ' again. Its session could not resume.' : 'Resumed ' + name(rec) + '.'));
  const recentAct = {
    resume: (rec) => resumeOne(rec).then(refresh),
    forget: (rec) => act('pane.forget', { pane: rec.id }, () => 'Forgot ' + name(rec) + '.').then(refresh),
  };
  // resumeAll resumes each ended agent in turn and says how many did, and
  // why the first that failed did. A second click while it runs waits.
  let resuming = false;
  const resumeAll = async () => {
    if (resuming) return;
    resuming = true;
    endedDue = true;
    const list = [...ended];
    let done = 0;
    let why = '';
    try {
      for (const rec of list) {
        try {
          await window.coppice.rpc('pane.resume', { pane: rec.id });
          done += 1;
        } catch (e) {
          if (!why) why = e.message;
        }
      }
    } finally {
      resuming = false;
    }
    if (done === list.length) window.coppice.status('Resumed ' + done + (done === 1 ? ' agent.' : ' agents.'));
    else window.coppice.status('Resumed ' + done + ' of ' + list.length + '. ' + why);
    refresh();
  };
  const forgetAll = () => act('pane.forget', { ended: true },
    (r) => 'Forgot ' + (Number(r.forgot) || 0) + ' ended ' + (Number(r.forgot) === 1 ? 'agent.' : 'agents.')).then(refresh);
  // Clear all asks first, as the TUI does: Forget or Enter forgets them,
  // Keep or Esc keeps them.
  const confirmBox = document.getElementById('recent-confirm');
  const endConfirm = () => {
    if (!confirmBox) return;
    confirmBox.replaceChildren();
    confirmBox.hidden = true;
  };
  if (confirmBox) {
    confirmBox.addEventListener('keydown', (e) => {
      if (e && e.key === 'Escape') endConfirm();
    });
  }
  const clearAll = () => {
    const n = ended.length;
    if (!confirmBox || n === 0) return;
    const btn = (text, cls, fn) => {
      const b = make('button', cls, text);
      b.type = 'button';
      b.addEventListener('click', (e) => {
        if (e && typeof e.stopPropagation === 'function') e.stopPropagation();
        endConfirm();
        fn();
      });
      return b;
    };
    const forget = btn('Forget', 'stop', forgetAll);
    confirmBox.replaceChildren(make('span', 'confirm-text', clearAllText(n)), forget, btn('Keep', 'keep ghost', () => {}));
    confirmBox.hidden = false;
    if (typeof forget.focus === 'function') forget.focus();
  };
  const onClick = (id, fn) => {
    const el = document.getElementById(id);
    if (el) el.addEventListener('click', (e) => {
      if (e && typeof e.stopPropagation === 'function') e.stopPropagation();
      fn();
    });
  };
  onClick('recent-resume-all', resumeAll);
  onClick('recent-clear', clearAll);
  // Opening Recent is how the owner sees an ended agent, so the floor
  // drops its ended lines then.
  const recentBox = document.getElementById('recent');
  if (recentBox) {
    recentBox.addEventListener('toggle', () => {
      if (recentBox.open && floor !== null && typeof floor.seenRecent === 'function') floor.seenRecent();
    });
  }

  // readTasks hands the floor a fresh task list for its stack. A refusal
  // changes nothing: the floor keeps the list it had.
  const readTasks = async () => {
    if (floor === null || typeof floor.setTasks !== 'function') return;
    try {
      const result = await window.coppice.rpc('task.list');
      floor.setTasks(result && result.tasks);
    } catch {
      // The rail keeps its last stack.
    }
  };

  const socketOpen = () => window.coppice.state.ws && window.coppice.state.ws.readyState === WebSocket.OPEN;

  // Nothing else ticks the age text between refreshes. Thirty seconds
  // keeps a card honest without a round trip, and the interval stops the
  // moment the operator leaves the roster.
  const startTicker = () => { if (ticker === null) ticker = setInterval(paint, 30000); };
  const stopTicker = () => {
    if (ticker === null) return;
    clearInterval(ticker);
    ticker = null;
  };

  // A busy pane produces state events faster than a phone should ask for a
  // pane list. Coalesce them: one round trip every half second is plenty
  // for a roster a human is reading.
  let timer = null;
  const soon = () => {
    if (timer) return;
    timer = setTimeout(() => { timer = null; refresh(); }, 500);
  };

  // A new socket may carry a new token, so the clock may read again.
  window.addEventListener('coppice:open', () => { clock.restart(); readAt = -Infinity; });
  window.addEventListener('coppice:open', refresh);
  window.addEventListener('coppice:route', () => {
    if (!overviewNow()) { stopTicker(); return; }
    startTicker();
    // With no token there is nothing to authenticate a request with.
    // connect() already left its own sentence on the status line; asking
    // the plain HTTP endpoint anyway would send "Bearer null" and cost the
    // phone's own address one of its three ban strikes for a mistake that
    // is not a bad token, it is no token at all.
    if (!window.coppice.state.token) return;
    if (socketOpen()) refresh();
    else firstPaint();
  });
  // A state event changes the row it names at once, so the rail and the
  // windows draw from the same merged row. An agent that ended makes the
  // ended list due. A fresh list follows.
  window.addEventListener('coppice:state', (e) => {
    const msg = e && e.detail;
    if (msg && msg.pane) {
      const row = raw.find((p) => p && p.id === msg.pane);
      if (row) {
        Object.assign(row, rowFromEvent(row, msg));
        paint();
      }
      if (msg.state === 'done') endedDue = true;
    }
    soon();
  });
  // The floor asks for this when the rail changes with no new list: a
  // fold, a selection, a kill confirm.
  window.addEventListener('coppice:rail', paint);
  // A kill, a rename or a new agent asks for fresh lists.
  window.addEventListener('coppice:refresh', () => { if (socketOpen()) refresh(); });
  return refresh;
}

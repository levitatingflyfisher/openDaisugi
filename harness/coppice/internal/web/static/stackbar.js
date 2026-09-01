// The stack bar: what each agent runs, as one bar on its window header.
// Its segments run loop, daisugi and its mode, router, model. Colour is
// health only: ok, warn, bad, or off for off and not known. The words are
// the choice. A field the server does not know shows as not known, never
// as a guess. The same file reads the floor facts for the header row.
// Nothing here but the two mount functions touches the page.

import { MESSAGES } from './messages.js';

export const SEGMENTS = ['loop', 'daisugi', 'router', 'model'];

// SWAP_NOTE is what a segment's details say while no verb swaps it.
export const SWAP_NOTE = 'Swap is not built yet.';
// MODE_NOTE is what the daisugi details say while no verb sets the mode.
export const MODE_NOTE = 'Changing the mode from here is not built yet.';

// GLYPH marks a verdict. An ask has its own glyph, apart from the ? of a
// field not known.
const GLYPH = { allow: '✓', deny: '✕', ask: '⧗' };

const str = (v) => (typeof v === 'string' && v !== '' ? v : '');

// fmtTokens is a token count in a few characters: 999, 12.3k, 2.5M.
export function fmtTokens(n) {
  const v = Number(n);
  if (!Number.isFinite(v) || v <= 0) return '0';
  const cut = (x) => String(Number(x.toFixed(1)));
  if (v < 1000) return String(Math.round(v));
  if (v < 1e6) return cut(v / 1000) + 'k';
  return cut(v / 1e6) + 'M';
}

// ago is how long ago a number of seconds was, in words.
export function ago(sec) {
  const s = Math.max(0, Math.round(Number(sec) || 0));
  if (s < 10) return 'just now';
  if (s < 60) return s + ' s ago';
  if (s < 3600) return Math.floor(s / 60) + ' min ago';
  return Math.floor(s / 3600) + ' h ago';
}

// gatewayDown is true when the floor facts say the gateway does not answer.
const gatewayDown = (facts) => Boolean(facts && facts.gateway && facts.gateway.answers === false);

// segments is the bar for one pane row: one entry per segment with its
// key, word, health and why. A row with no stack draws no bar. The daisugi
// step's place names it, so its word is the mode alone. A deny is a
// verdict, not the gate's health, so it does not colour the step.
export function segments(row, facts) {
  const st = row && row.stack;
  if (!st || typeof st !== 'object') return [];
  const ended = row.state === 'done' || row.closed === true;
  const loopWord = str(st.loop);
  const loop = {
    key: 'loop', word: loopWord || 'loop ?',
    health: !loopWord || ended ? 'off' : 'ok',
    why: !loopWord ? 'The harness is not known.' : ended ? 'The agent ended.' : 'The harness runs.',
  };
  const d = st.daisugi && typeof st.daisugi === 'object' ? st.daisugi : null;
  const mode = d ? str(d.mode) : '';
  let daisugi;
  if (!mode) {
    daisugi = { key: 'daisugi', word: 'mode ?', health: 'off', why: 'The gate mode is not known.' };
  } else {
    const disarmed = d.armed === false;
    daisugi = { key: 'daisugi', word: mode + (disarmed ? ' · disarmed' : ''), health: 'ok', why: 'The gate checks each call.' };
    if (mode === 'off') {
      daisugi.health = 'off';
      daisugi.why = 'No gate hook reports for this agent.';
    } else if (disarmed) {
      daisugi.health = 'warn';
      daisugi.why = 'The gate is disarmed, so it checks nothing.';
    } else if (loopWord === 'codex') {
      daisugi.health = 'warn';
      daisugi.why = 'Codex hooks fail open, so this gate is soft.';
    }
  }
  const route = str(st.router);
  const router = { key: 'router', word: route || 'router ?', health: route ? 'ok' : 'off', why: route ? 'Calls go this way.' : 'No base URL is set, so the route is not known.' };
  if ((route === 'gateway' || route === 'switchyard') && gatewayDown(facts)) {
    router.health = 'bad';
    router.why = 'The gateway does not answer.';
  }
  const modelWord = str(st.model);
  const model = { key: 'model', word: modelWord || 'model ?', health: modelWord ? 'ok' : 'off', why: modelWord ? 'The model the agent last used.' : 'No model reported yet.' };
  return [loop, daisugi, router, model];
}

// gateMark is the last verdict as a mark: its glyph, decision and a tip
// with the tool and clause. null when there is no verdict.
export function gateMark(gate) {
  if (!gate || !GLYPH[gate.decision]) return null;
  const tool = str(gate.tool);
  const clause = str(gate.clause);
  const tip = 'Last gate verdict: ' + gate.decision + (tool ? ' ' + tool : '') + (clause ? '. Clause: ' + clause : '');
  return { glyph: GLYPH[gate.decision], decision: gate.decision, tip };
}

// light is the segment where the current turn is: the loop while the
// agent works, daisugi while an ask waits for the owner, or '' when
// neither. A held ask is the foreman's, so the loop keeps the light.
export function light(row) {
  if (!row || !row.stack) return '';
  if (row.state === 'blocked' && row.ask && !row.held) return 'daisugi';
  if (row.state === 'working' || (row.state === 'blocked' && row.held)) return 'loop';
  return '';
}

// details is what an open segment shows below the header: a title, lines
// of [label, value], and a note that says what cannot be done here.
export function details(key, row, facts, now) {
  const st = (row && row.stack) || {};
  if (key === 'loop') {
    return { title: 'Loop', lines: [['harness', str(st.loop) || 'not known'], ['state', str(row && row.state) || 'not known']], note: SWAP_NOTE };
  }
  if (key === 'daisugi') {
    const d = st.daisugi && typeof st.daisugi === 'object' ? st.daisugi : null;
    const lines = [['mode', d && str(d.mode) ? d.mode : 'not known']];
    if (d && typeof d.armed === 'boolean') lines.push(['armed', d.armed ? 'yes' : 'no']);
    const g = row && row.gate;
    if (g && GLYPH[g.decision]) {
      lines.push(['last verdict', g.decision + (str(g.tool) ? ' ' + g.tool : '')]);
      lines.push(['clause', str(g.clause) || 'none']);
      if (Number.isFinite(g.at)) lines.push(['when', ago(now - g.at)]);
    } else {
      lines.push(['last verdict', 'none yet']);
    }
    // An agent no gate hook reports for gets the command that installs one.
    const note = d && d.mode === 'off' ? MESSAGES.gateOff().text : MODE_NOTE;
    return { title: 'daisugi', lines, note };
  }
  if (key === 'router') {
    const lines = [['route', str(st.router) || 'not known']];
    const gw = facts && facts.gateway;
    if (gw && str(gw.url)) {
      lines.push(['gateway', gw.url]);
      lines.push(['answers', gw.answers === true ? 'yes' : gw.answers === false ? 'no' : 'not known']);
    }
    return { title: 'Router', lines, note: SWAP_NOTE };
  }
  const t = row && row.tokens;
  const lines = [['model', str(st.model) || 'not reported yet']];
  if (t && typeof t === 'object') {
    lines.push(['fresh in', fmtTokens(t.fresh)], ['cache read', fmtTokens(t.cache_read)], ['cache write', fmtTokens(t.cache_write)], ['out', fmtTokens(t.out)]);
  } else {
    lines.push(['tokens', 'not reported yet']);
  }
  return { title: 'Model', lines, note: SWAP_NOTE };
}

const sum = (t) => (t && typeof t === 'object'
  ? ['fresh', 'cache_read', 'cache_write', 'out'].reduce((a, k) => a + (Number(t[k]) > 0 ? Number(t[k]) : 0), 0)
  : 0);
const count = (n) => (Number.isInteger(n) && n > 0 ? n : 0);

// liveCounts counts working and needs-you over a live pane list the way
// the rail does: a blocked pane whose ask a foreman holds is working, and
// only a blocked pane no foreman holds needs you.
export function liveCounts(rows) {
  let working = 0;
  let needing = 0;
  for (const r of Array.isArray(rows) ? rows : []) {
    if (!r || r.closed) continue;
    if (r.state === 'working' || (r.state === 'blocked' && r.held)) working += 1;
    else if (r.state === 'blocked') needing += 1;
  }
  return { working, needing };
}

// factsItems is the header row for a floor.facts reply: one item per fact
// with its key, text, health and title. A reply with no daisugi object
// gives nothing. With rows, the page's own live pane list, the working and
// needs-you counts come from the rows, so the header never disagrees with
// the rail beside it between two reads of the facts.
export function factsItems(facts, rows) {
  if (!facts || typeof facts !== 'object' || !facts.daisugi || typeof facts.daisugi !== 'object') return [];
  const d = facts.daisugi;
  const disarmed = d.armed === false;
  const n = { enforcing: count(d.enforcing), watching: count(d.watching), off: count(d.off) };
  const parts = ['enforcing', 'watching', 'off'].filter((k) => n[k] > 0).map((k) => n[k] + ' ' + k);
  const guarded = n.enforcing + n.watching;
  let health = 'ok';
  if (guarded === 0) health = 'off';
  else if (n.off > 0 || disarmed) health = 'warn';
  const items = [{
    key: 'daisugi',
    text: 'daisugi · ' + (parts.length ? parts.join(' · ') : 'no agents') + (disarmed ? ' · disarmed' : ''),
    health,
    title: 'Agents by mode: enforcing ' + n.enforcing + ', watching ' + n.watching + ', off ' + n.off,
  }];
  // No harness has a gate hook and no agent is guarded: say how to turn
  // the gate on, with a button that types the command in a shell.
  if (d.installed === false && guarded === 0) {
    const hint = MESSAGES.gateNotInstalled();
    items.push({ key: 'gate-hint', text: hint.text, health: 'warn', title: hint.text, action: hint.actions[0] });
  }
  const gw = facts.gateway;
  if (!gw || !str(gw.url)) items.push({ key: 'gateway', text: 'no gateway', health: 'off', title: 'No gateway is set in coppice.toml.' });
  else if (gw.answers === true) items.push({ key: 'gateway', text: 'gateway answers', health: 'ok', title: gw.url });
  else if (gw.answers === false) items.push({ key: 'gateway', text: 'gateway silent', health: 'bad', title: gw.url + ' does not answer.' });
  else items.push({ key: 'gateway', text: 'gateway ?', health: 'off', title: gw.url + ' is not on this computer, so it is not checked.' });
  const own = Array.isArray(rows) ? liveCounts(rows) : null;
  const working = own ? own.working : count(facts.working);
  items.push({ key: 'working', text: working + ' working', health: working > 0 ? 'ok' : 'off', title: 'Agents working now' });
  const need = own ? own.needing : count(facts.needing_you);
  items.push({ key: 'needing', text: need + (need === 1 ? ' needs you' : ' need you'), health: need > 0 ? 'warn' : 'off', title: 'Agents waiting on an ask' });
  const t = facts.tokens_today;
  const part = (k) => fmtTokens(t && t[k]);
  items.push({
    key: 'tokens', text: 'tokens today ' + fmtTokens(sum(t)), health: 'plain',
    title: 'fresh in ' + part('fresh') + ', cache read ' + part('cache_read') + ', cache write ' + part('cache_write') + ', out ' + part('out'),
  });
  return items;
}

// make is one element with a class and text.
function make(tag, className, text) {
  const el = document.createElement(tag);
  if (className) el.className = className;
  if (text !== undefined) el.textContent = text;
  return el;
}

const stop = (e) => { if (e && typeof e.stopPropagation === 'function') e.stopPropagation(); };

// stackBarEl is the bar of segments for one agent. open is the segment
// whose details show, lit the segment the light sits on, and onSeg(key)
// runs on a click. A click stays off the header under it.
export function stackBarEl(segs, { open, lit, onSeg }) {
  const bar = make('span', 'stackbar');
  bar.setAttribute('role', 'group');
  bar.setAttribute('aria-label', 'Stack');
  for (const seg of segs) {
    const isOpen = open === seg.key;
    const b = make('button', 'seg seg-' + seg.key + ' h-' + seg.health + (isOpen ? ' open' : '') + (lit === seg.key ? ' lit' : ''), seg.word);
    b.type = 'button';
    b.title = seg.key + ': ' + seg.word + '. ' + seg.why;
    b.setAttribute('aria-label', seg.key + ' ' + seg.word);
    b.setAttribute('aria-expanded', isOpen ? 'true' : 'false');
    b.addEventListener('click', (e) => {
      stop(e);
      onSeg(seg.key);
    });
    bar.append(b);
  }
  return bar;
}

// gateMarkEls is the last verdict's mark and its tip, which names the tool
// and clause. The tip shows on hover as a title, and beside the mark while
// the mark has the keyboard focus. onClick runs on a click.
export function gateMarkEls(mark, onClick) {
  const b = make('button', 'gate gate-' + mark.decision, mark.glyph);
  b.type = 'button';
  b.title = mark.tip;
  b.setAttribute('aria-label', mark.tip);
  b.addEventListener('click', (e) => {
    stop(e);
    onClick();
  });
  return [b, make('span', 'gate-tip', mark.tip)];
}

// detailsBox is an open segment's details: its title, a Journal button on
// the daisugi details when onJournal is given, a close control, its lines
// and its note.
export function detailsBox(d, { onJournal, onClose }) {
  const box = make('div', 'stack-details');
  box.dataset.details = d.title;
  const top = make('div', 'details-top');
  top.append(make('strong', '', d.title));
  if (d.title === 'daisugi' && typeof onJournal === 'function') {
    const j = make('button', 'ghost', 'Journal');
    j.type = 'button';
    j.addEventListener('click', (e) => {
      stop(e);
      onJournal();
    });
    top.append(j);
  }
  const close = make('button', 'ghost close', '×');
  close.type = 'button';
  close.setAttribute('aria-label', 'Close details');
  close.addEventListener('click', (e) => {
    stop(e);
    onClose();
  });
  top.append(close);
  const dl = make('dl');
  for (const [k, v] of d.lines) dl.append(make('dt', '', k), make('dd', '', v));
  box.append(top, dl, make('p', 'muted details-note', d.note));
  box.addEventListener('click', stop);
  return box;
}

// FACTS_MS is how often the header row reads the floor facts.
export const FACTS_MS = 5000;

// mountFacts keeps the header row in el. read asks floor.facts, hands the
// reply to got, calls tick so the rows' own facts are read on the same
// beat, and reads again FACTS_MS after it ends, for as long as
// connected() is true. A refusal leaves one grey item that says the facts
// are not known. later and cancel are the timer, setTimeout by default.
// rows, when given, returns the page's live pane list, and repaint draws
// the row again from it with the last facts. onAction runs the action of
// an item that carries one, as app.js runs a message's fix. hintEl, when
// given, carries the install hint on a phone (see paintHint).
// paintHint shows the install hint among items, if there is one, in
// hintEl, the rail's list header, with a button that runs its action; it
// hides hintEl when there is none. The header row that carries the hint
// is hidden on a phone and on a narrow screen, so the rail carries it
// there.
export function paintHint(hintEl, items, onAction) {
  if (!hintEl) return;
  const it = (items || []).find((x) => x.key === 'gate-hint');
  hintEl.textContent = '';
  hintEl.hidden = !it;
  if (!it) return;
  const words = document.createElement('span');
  words.textContent = it.text;
  hintEl.append(words);
  if (it.action && typeof onAction === 'function') {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'fix';
    b.textContent = it.action.label;
    b.addEventListener('click', () => onAction(it.action));
    hintEl.append(' ', b);
  }
}

export function mountFacts(el, { rpc, connected, got, tick, later, cancel, rows, onAction, hintEl }) {
  let last = null;
  let timer = null;
  const wait = later || ((fn, ms) => setTimeout(fn, ms));
  const stop = () => {
    if (timer !== null) (cancel || clearTimeout)(timer);
    timer = null;
  };
  const paint = (items) => {
    el.textContent = '';
    for (const it of items) {
      const li = document.createElement('li');
      li.className = 'fact fact-' + it.key + ' h-' + it.health;
      li.textContent = it.text;
      if (it.title) li.title = it.title;
      // An item with an action gets a button that runs it, as a message's
      // fix does.
      if (it.action && typeof onAction === 'function') {
        const b = document.createElement('button');
        b.type = 'button';
        b.className = 'ghost fact-fix';
        b.textContent = it.action.label;
        b.addEventListener('click', () => onAction(it.action));
        li.append(' ', b);
      }
      el.append(li);
    }
    el.hidden = items.length === 0;
    paintHint(hintEl, items, onAction);
  };
  const read = async () => {
    stop();
    if (!connected()) return;
    try {
      last = await rpc('floor.facts', {});
      paint(factsItems(last, rows ? rows() : undefined));
      if (got) got(last);
    } catch (e) {
      paint([{ key: 'none', text: 'floor facts not known', health: 'off', title: (e && e.message) || '' }]);
    }
    if (tick) tick();
    stop();
    if (connected()) timer = wait(read, FACTS_MS);
  };
  const repaint = () => {
    if (last) paint(factsItems(last, rows ? rows() : undefined));
  };
  return { read, stop, repaint, facts: () => last };
}

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
// MODE_OFF_NOTE is what the daisugi details say under the mode row: the
// mode is set here, but off never is.
export const MODE_OFF_NOTE = MESSAGES.modeOff().text;

// modeAsk is the one question before an agent goes from enforcing to
// watching.
export const modeAsk = (label) => 'Watch only? The gate will log the calls of ' + label + ' and stop none. While it watches, it can also change the gate for other agents.';

// modeDone is the status line after agent.set_mode. The gate reads the
// marker on the agent's next tool call, so it says when. Enforce takes the
// marker away, and the agent goes back to whatever mode the gate itself
// runs in, which may be watching too, so it never says enforces
// (F11-R-14).
export function modeDone(label, mode) {
  return mode === 'watching'
    ? label + ' watches from its next tool call. The gate logs its calls and stops none.'
    : label + " goes back to the gate's own mode from its next tool call.";
}

// hookRootLine is the mode row's one line when the installed gate hook
// names another gate root than coppice's (F11-R-15).
export const hookRootLine = (root) =>
  'The gate hook reads another gate root, ' + root + ', so a mode set here does nothing.';

// gateWatches is the mode row's one line when the whole gate watches: the
// hook runs in audit, so no agent can be lowered and the row offers no
// button (F11-R-14).
export const gateWatches = (label) =>
  'The gate itself watches every call of ' + label + ': its hook runs in audit, so there is nothing to lower here.';

// modeChoice is the mode row's one button for a pane row: the mode it sets
// (next), its words, and the question it asks first ('' for none). The
// marker the owner set wins over the mode the gate last reported, since
// the gate reads it on the next call. An agent that watches with no marker
// watches because the whole gate does, so the row has a line and no
// button (next null). null when there is nothing to set: no gate hook
// reports for the agent, or the agent ended.
export function modeChoice(row, facts) {
  const st = row && row.stack;
  const d = st && st.daisugi && typeof st.daisugi === 'object' ? st.daisugi : null;
  if (!d || !str(d.mode) || d.mode === 'off' || row.closed === true || row.state === 'done') return null;
  const label = str(row.label) || str(row.id) || 'this agent';
  // The installed hook reads markers from another gate root, so a mode
  // set here does nothing (F11-R-15).
  const hookRoot = facts && facts.daisugi && str(facts.daisugi.hook_root);
  if (hookRoot) return { next: null, line: hookRootLine(hookRoot) };
  if (d.marker === 'watching') return { next: 'enforcing', label: 'Enforce', ask: '' };
  if (d.mode === 'watching') return { next: null, line: gateWatches(label) };
  return { next: 'watching', label: 'Watch only', ask: modeAsk(label) };
}

// MODELS is the harness's own list of models for the model row. Only
// claude has one: its /model takes these names. A headless sprig agent
// takes a typed name, since sprig runs on any provider's model.
export const MODELS = { claude: ['opus', 'sonnet', 'haiku'] };

// ROUTER_NOTE is what the router details say under the swap row.
export const ROUTER_NOTE = 'A router change restarts the agent and resumes its conversation.';

// routerAsk is the one question before a router swap.
export const routerAsk = (label) => 'Restarts ' + label + ' and resumes its conversation. A turn in progress ends.';

// modelDone is the status line after agent.set_model, from the reply's
// when: now, queued or next turn.
export function modelDone(label, model, when) {
  if (when === 'queued') return label + ' switches to ' + model + ' at its next idle prompt.';
  if (when === 'next turn') return label + ' uses ' + model + ' from its next turn.';
  return 'Asked ' + label + ' to switch to ' + model + '.';
}

// routerDone is the status line after agent.set_router. The agent comes
// back as a new window that enforces, since its old mode marker went with
// the old pane.
export const routerDone = (label, router) =>
  label + ' restarted and resumed ' + (router === 'direct' ? 'direct' : 'through the gateway') + '. It enforces again.';

// modelSwap is the model row for a pane row: what the owner may pick, and
// when it lands. null where no swap is built: an ended agent, and every
// harness but claude in a terminal and sprig headless.
export function modelSwap(row) {
  if (!row || row.closed === true || row.state === 'done') return null;
  const harness = str(row.harness) || str(row.stack && row.stack.loop);
  if (harness === 'claude' && str(row.kind) !== 'headless') {
    return { kind: 'model', options: MODELS.claude, free: false, when: 'The change goes in at the next idle prompt.' };
  }
  if (harness === 'sprig' && str(row.kind) === 'headless') {
    return { kind: 'model', options: [], free: true, when: 'The change applies from the next turn.' };
  }
  return null;
}

// routerSwap is the router row for a pane row: the routes it may take
// instead of its own, and the question asked first. Direct needs no
// gateway. The gateway is offered only where the floor knows one.
// Switchyard is a setting of the gateway, so it is not a choice here.
// null where no swap is built: an ended agent, and every harness but claude.
export function routerSwap(row, facts) {
  if (!row || row.closed === true || row.state === 'done') return null;
  if ((str(row.harness) || str(row.stack && row.stack.loop)) !== 'claude') return null;
  const label = str(row.label) || str(row.id) || 'this agent';
  const cur = str(row.stack && row.stack.router);
  const gw = facts && facts.gateway && str(facts.gateway.url);
  const choices = [];
  if (cur !== 'direct') choices.push({ next: 'direct', label: 'Direct' });
  if (gw && cur !== 'gateway' && cur !== 'switchyard') {
    // A gateway the facts say is silent is shown but greyed out: the server
    // refuses the swap before it stops the agent.
    const down = gatewayDown(facts);
    choices.push(down
      ? { next: 'gateway', label: 'Through the gateway', disabled: true, why: 'The gateway does not answer.' }
      : { next: 'gateway', label: 'Through the gateway' });
  }
  return choices.length ? { kind: 'router', choices, ask: routerAsk(label) } : null;
}

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
    if (d && d.marker === 'watching') lines.push(['next call', 'watching']);
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
    const note = d && d.mode === 'off' ? MESSAGES.gateOff().text : MODE_OFF_NOTE;
    return { title: 'daisugi', lines, note, mode: modeChoice(row, facts) };
  }
  if (key === 'router') {
    const lines = [['route', str(st.router) || 'not known']];
    const gw = facts && facts.gateway;
    if (gw && str(gw.url)) {
      lines.push(['gateway', gw.url]);
      lines.push(['answers', gw.answers === true ? 'yes' : gw.answers === false ? 'no' : 'not known']);
    }
    const swap = routerSwap(row, facts);
    return { title: 'Router', lines, note: swap ? ROUTER_NOTE : SWAP_NOTE, swap };
  }
  const t = row && row.tokens;
  const lines = [['model', str(st.model) || 'not reported yet']];
  if (t && typeof t === 'object') {
    lines.push(['fresh in', fmtTokens(t.fresh)], ['cache read', fmtTokens(t.cache_read)], ['cache write', fmtTokens(t.cache_write)], ['out', fmtTokens(t.out)]);
  } else {
    lines.push(['tokens', 'not reported yet']);
  }
  const swap = modelSwap(row);
  return { title: 'Model', lines, note: swap ? swap.when : SWAP_NOTE, swap };
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
  // The phone's tailscale certificate is close to its end: warn, with
  // Renew. The server names it only under 7 days left.
  if (typeof facts.phone_cert_days === 'number' && Number.isFinite(facts.phone_cert_days)) {
    const m = MESSAGES.phoneCert(facts.phone_cert_days);
    items.push({ key: 'phone-cert', text: m.text, health: facts.phone_cert_days < 0 ? 'bad' : 'warn', title: m.text, action: m.actions[0] });
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
  // A saving comes from the gateway's own figure. With none, or one that is
  // not a count, the header names no saving.
  const sv = facts.saved_today;
  if (sv && typeof sv === 'object' && Number.isFinite(sv.tokens) && sv.tokens >= 0) {
    items.push({
      key: 'saved', text: 'saved ' + fmtTokens(sv.tokens), health: 'plain',
      title: 'The gateway reports ' + sv.tokens + ' frontier tokens saved today.',
    });
  }
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
// the daisugi details when onJournal is given, a close control, its lines,
// the mode row when onMode is given, and its note. The mode row is one
// button. Watch only asks once first: onAsk(true) asks the page to draw
// the box again with asking true, which shows the question with Watch
// only and Keep enforcing. onMode(mode) sets the mode.
export function detailsBox(d, { onJournal, onClose, onMode, asking, onAsk, onSwap, swapAsking, onSwapAsk }) {
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
  box.append(top, dl);
  if (d.mode && typeof onMode === 'function') box.append(modeRow(d.mode, { onMode, asking, onAsk }));
  if (d.swap && typeof onSwap === 'function') box.append(swapRow(d.swap, { onSwap, asking: swapAsking, onAsk: onSwapAsk }));
  box.append(make('p', 'muted details-note', d.note));
  box.addEventListener('click', stop);
  return box;
}

// modeRow is the mode row of the daisugi details: one button, or while
// asking, the question and its two answers.
function modeRow(choice, { onMode, asking, onAsk }) {
  const row = make('div', 'mode-row');
  if (!choice.next) {
    row.append(make('p', 'mode-ask', choice.line));
    return row;
  }
  const ask = (on) => { if (typeof onAsk === 'function') onAsk(on); };
  const button = (cls, text, fn) => {
    const b = make('button', cls, text);
    b.type = 'button';
    b.addEventListener('click', (e) => {
      stop(e);
      fn();
    });
    return b;
  };
  if (choice.ask && asking) {
    row.append(
      make('p', 'mode-ask', choice.ask),
      button('deny', choice.label, () => { ask(false); onMode(choice.next); }),
      button('ghost', 'Keep enforcing', () => ask(false)),
    );
    return row;
  }
  row.append(button('ghost mode-set', choice.label, () => (choice.ask ? ask(true) : onMode(choice.next))));
  return row;
}

// swapRow is the swap row of the model and router details. A claude model
// row is one button for each model, and a sprig one a box for a name and a
// Set button: onSwap('model', name). A router row has a button for each
// route. A route asks first, with Resume and Keep: onAsk(next) shows the
// question and onSwap('router', next) restarts the agent.
function swapRow(sw, { onSwap, asking, onAsk }) {
  const row = make('div', 'mode-row swap-row');
  const button = (cls, text, fn) => {
    const b = make('button', cls, text);
    b.type = 'button';
    b.addEventListener('click', (e) => {
      stop(e);
      fn();
    });
    return b;
  };
  if (sw.kind === 'model') {
    for (const name of sw.options) row.append(button('ghost model-set', name, () => onSwap('model', name)));
    if (sw.free) {
      const input = make('input', 'swap-input');
      input.type = 'text';
      input.placeholder = 'model name';
      input.setAttribute('aria-label', 'Model name');
      input.addEventListener('click', stop);
      const go = () => {
        const name = String(input.value || '').trim();
        if (name) onSwap('model', name);
      };
      input.addEventListener('keydown', (e) => {
        stop(e);
        if (e.key === 'Enter') go();
      });
      row.append(input, button('ghost model-set', 'Set', go));
    }
    return row;
  }
  const ask = (v) => { if (typeof onAsk === 'function') onAsk(v); };
  const chosen = sw.choices.find((c) => c.next === asking);
  if (chosen) {
    row.append(
      make('p', 'mode-ask', sw.ask),
      button('deny', 'Resume', () => { ask(''); onSwap('router', chosen.next); }),
      button('ghost', 'Keep', () => ask('')),
    );
    return row;
  }
  for (const c of sw.choices) {
    const b = button('ghost router-set', c.label, () => { if (!c.disabled) ask(c.next); });
    if (c.disabled) {
      b.disabled = true;
      b.title = c.why;
    }
    row.append(b);
  }
  return row;
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
// The phone certificate warning goes there too, for the same reason.
const HINT_KEYS = ['gate-hint', 'phone-cert'];

export function paintHint(hintEl, items, onAction) {
  if (!hintEl) return;
  const its = (items || []).filter((x) => HINT_KEYS.includes(x.key));
  hintEl.textContent = '';
  hintEl.hidden = its.length === 0;
  its.forEach((it, i) => {
    if (i > 0) hintEl.append(' ');
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
  });
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

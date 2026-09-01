// Views and the journal open as an overlay: a panel over part of the
// windows, never a new page. The rail and at least one window stay in
// sight. The colony can pin as a strip above the windows instead. The
// pure rules come first; mountOverlay draws them.

// JOURNAL is the overlay id of the journal. No view id starts with a colon.
export const JOURNAL = ':journal';
// COLONY is the view that can pin as a strip.
export const COLONY = 'colony';
// PIN_KEY is where this browser keeps whether the colony strip is pinned.
export const PIN_KEY = 'coppice.colony.pinned';
// JOURNAL_KEPT is the most entries the journal shows.
export const JOURNAL_KEPT = 200;

// JOURNAL_SOURCE says where the journal's lines come from, since the
// server keeps no history of verdicts.
export const JOURNAL_SOURCE = 'The server keeps no verdict history. This lists the gate reports in the shift log\'s events of the last two hours, and each last verdict this page saw. A verdict made while an agent kept working shows only if this page saw it as the last one.';

// overlayNext is the overlay after one action. state is {open, sel, pane,
// pinned}: open is a view id, JOURNAL, or ''; sel the panes a view was
// opened with; pane the agent the journal shows, or '' for all; pinned
// whether the colony strip shows. Actions: open {id, sel, pane}, close,
// pin, unpin. Pinning the colony closes its overlay, since the strip
// shows it.
export function overlayNext(state, action) {
  const s = { open: '', sel: [], pane: '', pinned: false, ...(state || {}) };
  const a = action || {};
  if (a.type === 'open' && typeof a.id === 'string' && a.id) {
    return { ...s, open: a.id, sel: Array.isArray(a.sel) ? a.sel.filter(Boolean) : [], pane: a.id === JOURNAL && typeof a.pane === 'string' ? a.pane : '' };
  }
  if (a.type === 'close') return { ...s, open: '', sel: [], pane: '' };
  if (a.type === 'pin') return { ...s, pinned: true, ...(s.open === COLONY ? { open: '', sel: [] } : {}) };
  if (a.type === 'unpin') return { ...s, pinned: false };
  return s;
}

// escCloses is true when Esc should close the overlay: one is open, and
// the keys are not in a window, where Esc belongs to the agent.
export function escCloses(state, keys) {
  return Boolean(state && state.open) && keys !== 'window';
}

// verdictOfDetail reads the detail a gate report carries: "verdict=allow"
// or "verdict=deny clause=<clause>". Anything else is null.
export function verdictOfDetail(detail) {
  const d = String(detail || '');
  if (d === 'verdict=allow') return { decision: 'allow', clause: '' };
  const m = d.match(/^verdict=deny(?: clause=(.*))?$/);
  if (m) return { decision: 'deny', clause: m[1] || '' };
  return null;
}

// journalFromEvents is the gate verdicts among state events, newest first:
// an allow or deny a gate report's detail names, and each ask a gate
// report raised. pane, when given, keeps only that agent's.
export function journalFromEvents(events, pane) {
  const out = [];
  for (const e of Array.isArray(events) ? events : []) {
    if (!e || e.event !== 'state' || e.source !== 'gate' || typeof e.pane !== 'string') continue;
    if (pane && e.pane !== pane) continue;
    const ts = Number(e.ts) || 0;
    if (e.state === 'blocked' && e.ask && typeof e.ask === 'object') {
      out.push({ pane: e.pane, ts, decision: 'ask', tool: String(e.ask.tool || ''), clause: '', what: String(e.ask.summary || '') });
      continue;
    }
    const v = verdictOfDetail(e.detail);
    if (v) out.push({ pane: e.pane, ts, decision: v.decision, tool: '', clause: v.clause, what: '' });
  }
  return out.sort((a, b) => b.ts - a.ts);
}

// seeVerdicts adds each row's last verdict to seen, a map of key to entry,
// when this page has not seen it before, and returns seen.
export function seeVerdicts(seen, panes) {
  const map = seen instanceof Map ? seen : new Map();
  for (const p of Array.isArray(panes) ? panes : []) {
    const g = p && p.gate;
    if (!g || !['allow', 'deny', 'ask'].includes(g.decision) || !Number.isFinite(g.at)) continue;
    const key = p.id + '|' + g.at + '|' + g.decision;
    if (!map.has(key)) map.set(key, { pane: p.id, ts: g.at, decision: g.decision, tool: String(g.tool || ''), clause: String(g.clause || ''), what: '' });
  }
  while (map.size > JOURNAL_KEPT) map.delete(map.keys().next().value);
  return map;
}

// SAME_S is how far apart in time an event and a seen verdict can be and
// still be one verdict: the gate's own clock against the server's.
const SAME_S = 3;

// journal merges the verdicts from events with the ones this page saw,
// newest first, at most JOURNAL_KEPT. A seen verdict names its tool, so
// it wins over an event for the same agent, decision and moment.
export function journal(fromEvents, seen, pane) {
  const mine = [...(seen instanceof Map ? seen.values() : [])].filter((e) => !pane || e.pane === pane);
  const kept = (Array.isArray(fromEvents) ? fromEvents : []).filter((e) => (!pane || e.pane === pane) && !mine.some((s) => s.pane === e.pane && s.decision === e.decision && Math.abs(s.ts - e.ts) <= SAME_S));
  return [...kept, ...mine].sort((a, b) => b.ts - a.ts).slice(0, JOURNAL_KEPT);
}

// readPinned reads whether the colony strip is pinned.
export function readPinned(storage) {
  try { return storage.getItem(PIN_KEY) === '1'; } catch { return false; }
}

// savePinned keeps whether the colony strip is pinned.
export function savePinned(storage, on) {
  try { storage.setItem(PIN_KEY, on ? '1' : '0'); } catch { /* storage is a convenience */ }
}

// JOURNAL_MS is how often an open journal reads the shift log's events.
export const JOURNAL_MS = 3000;

// clock is a unix time as HH:MM:SS, local time.
export function clock(ts) {
  const d = new Date(Number(ts) * 1000);
  if (Number.isNaN(d.getTime())) return '';
  const two = (n) => String(n).padStart(2, '0');
  return two(d.getHours()) + ':' + two(d.getMinutes()) + ':' + two(d.getSeconds());
}

const GLYPH = { allow: '✓', deny: '✕', ask: '?' };

function make(tag, className, text) {
  const el = document.createElement(tag);
  if (className) el.className = className;
  if (text !== undefined) el.textContent = text;
  return el;
}

// mountOverlay draws the overlay and the colony strip. d carries the page
// elements (el, title, pin, close, frame, journal, strip, unpin), the two
// view hosts (host, stripHost), api, labelOf(pane), titleOf(view id),
// onFloor(), fixes(message) for an error's buttons, toMessage, storage,
// every and cancel for the journal's timer, place(el) to size the panel,
// and relayout() for when the strip shows or hides.
export function mountOverlay(d) {
  let st = overlayNext(undefined, { type: 'close' });
  st.pinned = readPinned(d.storage);
  const seen = new Map();
  let events = [];
  let top = 0;
  // have holds the pane, time and detail of each kept event, so the ring
  // and the live stream never add one event twice, and two reports at one
  // moment stay two.
  const have = new Set();
  const keyOf = (e) => e.pane + '|' + e.ts + '|' + (e.detail || '');
  const keep = (list) => {
    for (const e of list) {
      const k = keyOf(e);
      if (have.has(k)) continue;
      have.add(k);
      events.push(e);
    }
    if (events.length > 2000) {
      for (const e of events.splice(0, events.length - 2000)) have.delete(keyOf(e));
    }
  };
  let ringErr = null;
  let timer = null;
  const stopTimer = () => {
    if (timer !== null) d.cancel(timer);
    timer = null;
  };

  const readRing = async () => {
    try {
      const body = await d.api('/api/events?since=' + top);
      const fresh = (Array.isArray(body && body.events) ? body.events : []).filter((e) => e && typeof e.ts === 'number' && e.ts > top);
      keep(fresh);
      for (const e of fresh) top = Math.max(top, e.ts);
      ringErr = null;
    } catch (e) {
      ringErr = e;
    }
    if (st.open === JOURNAL) paintJournal();
  };

  const paintJournal = () => {
    const box = d.journal;
    box.textContent = '';
    const bar = make('div', 'journal-bar');
    if (st.pane) {
      bar.append(make('span', '', 'Showing ' + d.labelOf(st.pane) + '.'));
      const all = make('button', 'ghost', 'All agents');
      all.type = 'button';
      all.addEventListener('click', () => { st = { ...st, pane: '' }; render(); });
      bar.append(all);
    } else {
      bar.append(make('span', '', 'Showing every agent.'));
    }
    box.append(bar, make('p', 'muted journal-source', JOURNAL_SOURCE));
    if (ringErr) {
      const m = d.toMessage(ringErr);
      const line = make('p', 'journal-error', 'The shift log\'s events did not load: ' + m.text);
      line.append(...d.fixes(m));
      box.append(line);
    }
    const list = journal(journalFromEvents(events, st.pane), seen, st.pane);
    if (list.length === 0) {
      box.append(make('p', 'muted', 'No gate verdicts yet.'));
      return;
    }
    const ol = make('ol', 'journal-list');
    for (const e of list) {
      const li = make('li', 'journal-row');
      li.append(
        make('span', 'muted when', clock(e.ts)),
        make('span', 'gate-mark gate-' + e.decision, GLYPH[e.decision] || '?'),
        make('span', 'who', d.labelOf(e.pane)),
        make('span', 'what', [e.decision, e.tool, e.clause ? 'clause ' + e.clause : '', e.what].filter(Boolean).join(' · ')),
      );
      ol.append(li);
    }
    box.append(ol);
  };

  const render = () => {
    const open = st.open;
    d.el.hidden = !open;
    d.pin.hidden = open !== COLONY || st.pinned || (typeof d.always === 'function' && d.always());
    if (open === JOURNAL) {
      d.title.textContent = 'Journal';
      d.frame.hidden = true;
      d.journal.hidden = false;
      d.host.hide();
      paintJournal();
      if (timer === null) {
        readRing();
        timer = d.every(readRing, JOURNAL_MS);
      }
    } else {
      stopTimer();
      d.journal.hidden = true;
      d.frame.hidden = !open;
      if (open) {
        d.title.textContent = d.titleOf(open);
        const cur = d.host.current();
        if (!cur || cur.id !== open) d.host.show(open, st.sel);
      } else {
        d.host.hide();
      }
    }
    placeStrip();
    if (open) d.place(d.el);
  };

  const placeStrip = () => {
    // A phone's overview always carries the strip when the server lists
    // the colony; always() says so.
    const always = typeof d.always === 'function' && d.always();
    const on = (st.pinned || always) && d.onFloor();
    const was = !d.strip.hidden;
    d.strip.hidden = !on;
    // The strip takes height from the windows, so they lay out again.
    if (was !== on && typeof d.relayout === 'function') d.relayout();
    if (on) {
      const cur = d.stripHost.current();
      if (!cur) d.stripHost.show(COLONY, []);
    } else {
      d.stripHost.hide();
    }
  };

  // saved is what had the focus before the overlay opened. The overlay
  // takes the focus on its close control, and gives it back on close.
  let saved = null;
  const focusOn = (el) => { if (el && typeof el.focus === 'function') el.focus(); };
  const apply = (action) => {
    const wasOpen = Boolean(st.open);
    st = overlayNext(st, action);
    if (action.type === 'pin' || action.type === 'unpin') savePinned(d.storage, st.pinned);
    if (!wasOpen && st.open) saved = typeof document !== 'undefined' ? document.activeElement || null : null;
    render();
    if (!wasOpen && st.open) focusOn(d.close);
    if (wasOpen && !st.open) {
      const back = saved;
      saved = null;
      focusOn(back);
    }
  };

  d.close.addEventListener('click', () => apply({ type: 'close' }));
  d.pin.addEventListener('click', () => apply({ type: 'pin' }));
  d.unpin.addEventListener('click', () => apply({ type: 'unpin' }));

  return {
    open: (id, sel, pane) => apply({ type: 'open', id, sel, pane }),
    close: () => apply({ type: 'close' }),
    state: () => st,
    // routed follows a change of screen: the overlay closes off the floor,
    // and the strip shows only on it.
    routed: () => {
      if (!d.onFloor() && st.open) apply({ type: 'close' });
      else placeStrip();
    },
    // panesChanged keeps each new last verdict the page saw.
    panesChanged: (panes) => {
      seeVerdicts(seen, panes);
      if (st.open === JOURNAL) paintJournal();
    },
    // onEvent keeps a live state event for the journal.
    onEvent: (msg) => {
      if (!msg || msg.event !== 'state' || typeof msg.ts !== 'number' || msg.ts <= top) return;
      keep([msg]);
      if (st.open === JOURNAL && msg.source === 'gate') paintJournal();
    },
    place: () => { if (st.open) d.place(d.el); },
  };
}

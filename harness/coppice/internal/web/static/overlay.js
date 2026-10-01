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
// JOURNAL_KEPT is the most entries the journal shows: the most the server
// keeps.
export const JOURNAL_KEPT = 500;

// JOURNAL_SOURCE says where the journal's lines come from.
export const JOURNAL_SOURCE = "This lists the verdicts the gate and each agent's loop report, newest first. The journal keeps 100 per agent, 500 in all, oldest first. They stay across a server restart.";

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

// PANELS are the screens that open as a panel over the floor and never
// replace it: the New form and Settings.
export const PANELS = Object.freeze(['new', 'settings']);

// isPanel is true when a route's screen opens as a panel over the floor.
export function isPanel(screen) {
  return PANELS.includes(screen);
}

// panelEsc is true when Esc should close an open panel. Esc inside a
// field closes it too, since a panel has no other use for Esc. It does
// not close while the keys are in a window, where Esc is the agent's.
export function panelEsc(screen, keys) {
  return isPanel(screen) && keys !== 'window';
}

// GATE_KINDS are the decisions a row's class name may carry. Any other
// decision gets the class 'other', so a text from a file never becomes a
// class.
const GATE_KINDS = ['allow', 'deny', 'ask'];

// journalRows are the entries floor.journal returned as rows, newest
// first, at most JOURNAL_KEPT. pane, when given, keeps only that agent's.
// An entry the server did not shape as a verdict is dropped.
export function journalRows(entries, pane) {
  const out = [];
  for (const e of Array.isArray(entries) ? entries : []) {
    if (!e || typeof e.pane !== 'string' || typeof e.decision !== 'string' || !Number.isFinite(e.at)) continue;
    if (pane && e.pane !== pane) continue;
    out.push({ pane: e.pane, ts: e.at, decision: e.decision, kind: GATE_KINDS.includes(e.decision) ? e.decision : 'other', tool: String(e.tool || ''), clause: String(e.clause || ''), mode: String(e.mode || '') });
  }
  return out.sort((a, b) => b.ts - a.ts).slice(0, JOURNAL_KEPT);
}

// readPinned reads whether the colony strip is pinned.
export function readPinned(storage) {
  try { return storage.getItem(PIN_KEY) === '1'; } catch { return false; }
}

// savePinned keeps whether the colony strip is pinned.
export function savePinned(storage, on) {
  try { storage.setItem(PIN_KEY, on ? '1' : '0'); } catch { /* storage is a convenience */ }
}

// JOURNAL_MS is how often an open journal reads floor.journal.
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
// view hosts (host, stripHost), rpc, labelOf(pane), titleOf(view id),
// onFloor(), fixes(message) for an error's buttons, toMessage, storage,
// every and cancel for the journal's timer, stripFits() (false when the strip would take a window) and dots (the one-line form), place(el) to size the panel,
// and relayout() for when the strip shows or hides.
export function mountOverlay(d) {
  let st = overlayNext(undefined, { type: 'close' });
  st.pinned = readPinned(d.storage);
  let rows = [];
  let readErr = null;
  let timer = null;
  let reading = false;
  const stopTimer = () => {
    if (timer !== null) d.cancel(timer);
    timer = null;
  };

  const readJournal = async () => {
    if (reading) return;
    reading = true;
    const pane = st.pane;
    try {
      const body = await d.rpc('floor.journal', pane ? { pane } : {});
      rows = Array.isArray(body && body.entries) ? body.entries : [];
      readErr = null;
    } catch (e) {
      readErr = e;
    }
    reading = false;
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
      all.addEventListener('click', () => { st = { ...st, pane: '' }; rows = []; render(); readJournal(); });
      bar.append(all);
    } else {
      bar.append(make('span', '', 'Showing every agent.'));
    }
    box.append(bar, make('p', 'muted journal-source', JOURNAL_SOURCE));
    if (readErr) {
      const m = d.toMessage(readErr);
      const line = make('p', 'journal-error', 'The journal did not load: ' + m.text);
      line.append(...d.fixes(m));
      box.append(line);
    }
    const list = journalRows(rows, st.pane);
    if (list.length === 0) {
      if (!readErr) box.append(make('p', 'muted', 'No gate verdicts yet.'));
      return;
    }
    const ol = make('ol', 'journal-list');
    for (const e of list) {
      const li = make('li', 'journal-row');
      li.append(
        make('span', 'muted when', clock(e.ts)),
        make('span', 'gate-mark gate-' + e.kind, GLYPH[e.kind] || '?'),
        make('span', 'who', d.labelOf(e.pane)),
        make('span', 'what', [e.decision, e.tool, e.clause ? 'clause ' + e.clause : ''].filter(Boolean).join(' · ')),
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
        readJournal();
        timer = d.every(readJournal, JOURNAL_MS);
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
    const want = (st.pinned || always) && d.onFloor();
    // When the strip would take a window, it shows as one line of dots in
    // the bar instead, which takes no height from the windows.
    const dots = want && typeof d.stripFits === 'function' && !d.stripFits();
    const on = want && !dots;
    const was = !d.strip.hidden;
    d.strip.hidden = !on;
    if (d.dots) d.dots.hidden = !dots;
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
    // onEvent reads the journal again when a gate reports while it shows.
    onEvent: (msg) => {
      if (st.open === JOURNAL && msg && msg.event === 'state' && msg.source === 'gate') readJournal();
    },
    place: () => { if (st.open) d.place(d.el); placeStrip(); },
  };
}

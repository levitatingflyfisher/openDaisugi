// The small library every view shares. A view runs in a sandboxed frame
// and holds no token. The floor page posts it the panes, the tasks, the
// events and the selection. The view may post back four things: open a
// pane, set the selection, watch up to nine panes, and close. Esc in the
// view asks the floor to close it, since the floor never hears a key the
// frame takes. The floor checks
// each against its pane list. Nothing here asks the server for anything.

// WATCH_MAX is how many panes one view may watch at once.
export const WATCH_MAX = 9;

// STATE_COLORS are the five state colours the roster paints.
export const STATE_COLORS = Object.freeze({
  blocked: '#d98b4a',
  working: '#8fb996',
  idle: '#7f8c8a',
  done: '#5d6a68',
  unknown: '#b06868',
});

// ORDER sorts states from the one that needs a person most, as the roster
// sorts them.
const ORDER = { blocked: 0, working: 1, unknown: 2, idle: 3, done: 4 };

// stateColor is the colour of one of the five states. Any other value
// throws: a mark with no state is a lie.
export function stateColor(state) {
  if (typeof state !== 'string' || !Object.prototype.hasOwnProperty.call(STATE_COLORS, state)) {
    throw new Error('no colour for the state ' + JSON.stringify(state));
  }
  return STATE_COLORS[state];
}

// stateWord is state when it is one of the five, else unknown. It reads a
// state the way the roster chip reads it.
export function stateWord(state) {
  return Object.prototype.hasOwnProperty.call(STATE_COLORS, state) ? state : 'unknown';
}

// age is a count of seconds as the roster writes it: 12s, 4m, 3h, 2d.
export function age(seconds) {
  const s = Math.max(0, Math.floor(seconds));
  if (s < 60) return s + 's';
  if (s < 3600) return Math.floor(s / 60) + 'm';
  if (s < 86400) return Math.floor(s / 3600) + 'h';
  return Math.floor(s / 86400) + 'd';
}

// byNeed is a copy of panes sorted as the roster sorts them: blocked
// first, and the list order inside one state.
export function byNeed(panes) {
  return (panes || [])
    .map((p, i) => [p, i])
    .sort((a, b) => {
      const d = (ORDER[a[0].state] ?? 5) - (ORDER[b[0].state] ?? 5);
      return d !== 0 ? d : a[1] - b[1];
    })
    .map((e) => e[0]);
}

// selection reads and writes the sel part of a view hash.
export const selection = {
  read(hash) {
    const h = String(hash || '');
    const at = h.indexOf('?');
    if (at < 0) return [];
    const raw = new URLSearchParams(h.slice(at + 1)).get('sel') || '';
    return raw.split(',').map((s) => s.trim()).filter(Boolean);
  },
  write(hash, ids) {
    const h = String(hash || '');
    const at = h.indexOf('?');
    const base = at < 0 ? h : h.slice(0, at);
    const panes = (ids || []).filter(Boolean);
    return panes.length ? base + '?sel=' + panes.map(encodeURIComponent).join(',') : base;
  },
};

const list = (v) => (Array.isArray(v) ? v : []);

// connect listens for what the floor page posts into win. It returns the
// newest data, three listener lists, and the three posts a view may send.
// from is the time since which events holds every state event, or null
// when the floor does not know it: a view that did not ask for the ring,
// or a ring the server refused.
export function connect(win) {
  const framed = Boolean(win && win.parent && win.parent !== win);
  let data = { panes: [], tasks: [], events: [], sel: [], from: null };
  const onData = [];
  const onFrame = [];
  const onWatch = [];
  if (win && typeof win.addEventListener === 'function') {
    win.addEventListener('message', (e) => {
      if (!framed || !e || e.source !== win.parent) return;
      const m = e.data;
      if (!m || typeof m !== 'object') return;
      if (m.type === 'coppice.data') {
        data = {
          panes: list(m.panes),
          tasks: list(m.tasks),
          events: list(m.events),
          sel: list(m.sel),
          from: typeof m.from === 'number' ? m.from : null,
        };
        for (const fn of onData) fn(data);
      } else if (m.type === 'coppice.frame' && typeof m.pane === 'string') {
        for (const fn of onFrame) fn(m);
      } else if (m.type === 'coppice.watch') {
        const st = { live: list(m.live), refused: list(m.refused) };
        for (const fn of onWatch) fn(st);
      }
    });
  }
  const post = (msg) => { if (framed) win.parent.postMessage(msg, '*'); };
  if (framed && win && typeof win.addEventListener === 'function') {
    win.addEventListener('keydown', (e) => {
      if (e && e.key === 'Escape' && !e.defaultPrevented) post({ type: 'close' });
    });
  }
  return {
    framed,
    panes: () => data.panes,
    tasks: () => data.tasks,
    events: () => data.events,
    sel: () => data.sel,
    from: () => data.from,
    onData: (fn) => { onData.push(fn); },
    onFrame: (fn) => { onFrame.push(fn); },
    onWatch: (fn) => { onWatch.push(fn); },
    openPane: (pane) => post({ type: 'open-pane', pane }),
    setSel: (ids) => post({ type: 'set-sel', sel: list(ids).filter((p) => typeof p === 'string') }),
    watch: (ids) => post({ type: 'watch', panes: list(ids).filter((p) => typeof p === 'string').slice(0, WATCH_MAX) }),
    close: () => post({ type: 'close' }),
  };
}

// boot runs start once the page body exists. A plain import, as in a
// test, does not run it.
export function boot(start) {
  if (typeof document === 'undefined' || !document.body) return;
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', start);
  else start();
}

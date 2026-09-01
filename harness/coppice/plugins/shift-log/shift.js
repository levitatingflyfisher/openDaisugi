// The shift log: where the hour went. Rows are panes and cells are spans
// of time. A cell holds the strongest mark among the state events in its
// span: working when the pane reported working, ask when it was blocked,
// deny when the gate said no. A cell with no event is empty only inside
// the span the server's ring holds without a break. Before that span, or
// with no span known, a cell with no event is unknown, never a guess.

// LEVEL is the mark each kind of event leaves. 0 is no mark.
export const LEVEL = Object.freeze({ working: 1, ask: 2, deny: 3 });

// UNKNOWN is a cell the ring does not cover and no event marked.
export const UNKNOWN = null;

// levelOf is the mark one event leaves. Only the gate's own verdict is a
// deny. An ask whose words hold deny is an ask.
export function levelOf(e) {
  if (!e || e.event !== 'state') return 0;
  if (typeof e.detail === 'string' && /\bverdict=deny\b/.test(e.detail)) return LEVEL.deny;
  if (e.state === 'blocked') return LEVEL.ask;
  if (e.state === 'working') return LEVEL.working;
  return 0;
}

// cells folds events into one row per pane over the minutes before now,
// in cells of cell minutes each, oldest cell first. A row is { pane,
// cells }. Rows come in the order their panes first show up. An event
// with no time, or one outside the window, is left out. from is the time
// since which the events hold every state event, or null when not known.
// A cell that starts before from, with no mark, is UNKNOWN.
export function cells(events, now, minutes = 80, cell = 5, from = now - minutes * 60) {
  const count = Math.max(1, Math.round(minutes / cell));
  const start = now - minutes * 60;
  const span = cell * 60;
  const held = (i) => typeof from === 'number' && start + i * span >= from;
  const blank = () => Array.from({ length: count }, (_, i) => (held(i) ? 0 : UNKNOWN));
  const rows = new Map();
  for (const e of events || []) {
    if (!e || e.event !== 'state' || typeof e.pane !== 'string' || typeof e.ts !== 'number') continue;
    if (e.ts < start || e.ts > now) continue;
    if (!rows.has(e.pane)) rows.set(e.pane, blank());
    const i = Math.min(count - 1, Math.floor((e.ts - start) / span));
    const row = rows.get(e.pane);
    // An event is a fact, so its cell is known even before from.
    row[i] = Math.max(row[i] || 0, levelOf(e));
  }
  return [...rows].map(([pane, marks]) => ({ pane, cells: marks }));
}

// coverText is the status line for a log of rows rows. It says there were
// no state changes only for the span the ring holds.
export function coverText(from, now, minutes, rows) {
  if (typeof from !== 'number') return 'The floor has no history from the server. Cells with no mark are unknown.';
  const held = Math.floor((now - from) / 60);
  if (held >= minutes) return rows ? '' : 'No state changes in the last ' + minutes + ' minutes.';
  const before = 'The server holds nothing before that, so the cells before it are unknown.';
  if (!rows) return 'No state changes in the last ' + held + ' minutes. ' + before;
  return 'The server holds nothing from before ' + held + ' minutes ago, so the cells before it are unknown.';
}

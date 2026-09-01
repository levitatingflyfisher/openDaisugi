// The Herdr-shaped grid: a roster on the left with the state word on each
// row, and a live picture of every pane on the right. The slots follow
// the floor's own rule for the layout all: every pane has one, in roster
// order. The floor shows at most nine panes live to one view, so the
// first nine slots of open panes are watched and the rest say so.
// pane.list no longer sends a closed pane at all by default (an ended one
// moves to Recent, `ended: true`, and an operator-closed one is gone for
// good), so `watched`'s own closed check below is defense in depth, not
// something a real feed triggers day to day: if a closed row ever does
// reach here, it still keeps its roster slot and is never watched - the
// floor would refuse it.
import { byNeed, stateWord, WATCH_MAX } from '../_lib/view.js';

// slots is every pane id in roster order, one slot each.
export function slots(panes) {
  const out = [];
  for (const p of byNeed(panes || [])) {
    if (p && typeof p.id === 'string' && !out.includes(p.id)) out.push(p.id);
  }
  return out;
}

// watched is the ids of the panes the view asks the floor to show live:
// the first open panes in slot order, WATCH_MAX at most.
export function watched(panes) {
  const closed = new Set((panes || []).filter((p) => p && p.closed).map((p) => p.id));
  return slots(panes).filter((id) => !closed.has(id)).slice(0, WATCH_MAX);
}

// shape is the columns and rows of a grid that holds n slots, as square
// as it can be.
export function shape(n) {
  if (!n || n < 1) return { cols: 0, rows: 0 };
  const cols = Math.ceil(Math.sqrt(n));
  return { cols, rows: Math.ceil(n / cols) };
}

// rosterRows is one row per pane in roster order: its id, its label and
// its state word.
export function rosterRows(panes) {
  return byNeed(panes || [])
    .filter((p) => p && typeof p.id === 'string')
    .map((p) => ({ id: p.id, label: p.label || p.id, word: stateWord(p.state) }));
}

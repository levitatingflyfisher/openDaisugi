// The lazybox-shaped inbox: one row per task that has a worktree, the
// rows that need a person first. A row shows the task, its worktree, how
// many commits it is ahead, and its state. ! jumps to the first row that
// needs you. The selected row's neediest pane is shown live beside the
// rows. The row carries no review number: to find one the server would
// ask the network on every poll.
import { byNeed } from '../_lib/view.js';

const ORDER = { blocked: 0, working: 1, unknown: 2, idle: 3, done: 4 };
const RANK = { blocked: 5, working: 4, idle: 3, unknown: 2, done: 1 };

function base(path) {
  const parts = String(path || '').split('/').filter(Boolean);
  return parts.length ? parts[parts.length - 1] : '';
}

// worst is the state among states that needs a person most, or ''.
function worst(states) {
  let out = '';
  let best = 0;
  for (const s of states) {
    if ((RANK[s] || 0) > best) {
      best = RANK[s];
      out = s;
    }
  }
  return out;
}

// rows is the tasks with a worktree, sorted as the roster sorts panes. A
// row is { id, label, worktree, ahead, state, panes }. worktree is the
// last part of the path, ahead is a number or null, and panes holds only
// the panes the list knows.
export function rows(tasks, panes) {
  const known = new Map();
  for (const p of panes || []) if (p && typeof p.id === 'string') known.set(p.id, p);
  const out = [];
  for (const t of tasks || []) {
    if (!t || typeof t.id !== 'string' || !t.worktree) continue;
    const held = (t.panes || []).filter((id) => known.has(id));
    const state = typeof t.state === 'string' && t.state !== ''
      ? t.state
      : worst(held.map((id) => known.get(id).state));
    out.push({
      id: t.id,
      label: t.label || t.id,
      worktree: base(t.worktree),
      ahead: typeof t.ahead === 'number' ? t.ahead : null,
      state,
      panes: held,
    });
  }
  return out
    .map((r, i) => [r, i])
    .sort((a, b) => (ORDER[a[0].state] ?? 5) - (ORDER[b[0].state] ?? 5) || a[1] - b[1])
    .map((e) => e[0]);
}

// jumpToNeed is the index of the first row that needs you, or -1.
export function jumpToNeed(list) {
  return (list || []).findIndex((r) => r && r.state === 'blocked');
}

// focusPane is the id of the row's open pane that needs a person most, or
// ''. A closed pane is never shown live: the floor would refuse it.
export function focusPane(row, panes) {
  const held = new Set((row && row.panes) || []);
  const first = byNeed((panes || []).filter((p) => held.has(p.id) && !p.closed))[0];
  return first ? first.id : '';
}

// selectedRow is the index of the first row that holds a pane in sel, or
// -1. The selection lives on the floor page, so the inbox keeps none.
export function selectedRow(list, sel) {
  const want = new Set(sel || []);
  return (list || []).findIndex((r) => r.panes.some((p) => want.has(p)));
}

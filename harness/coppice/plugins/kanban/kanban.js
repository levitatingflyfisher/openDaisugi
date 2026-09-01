// The kanban: tasks as cards in four columns. The columns are states the
// server owns, so a card moves when its panes change state and never by
// hand. inbox holds a task with nothing in it. needs you holds a task
// with a blocked pane. done holds a task whose panes are all done.
// working holds the rest: a task whose panes run or wait for a prompt,
// and a task whose state the server does not know. A card in working
// whose state is not working says its state.

export const COLUMNS = ['inbox', 'working', 'needs you', 'done'];

// HINT is the line under the columns.
export const HINT = 'columns are states. prompt the pane to move it.';

// RANK orders pane states as the server folds a task state: the state
// that needs a person most wins.
const RANK = { blocked: 5, working: 4, idle: 3, unknown: 2, done: 1, '': 0 };

// worst is the state among states that needs a person most, or ''.
export function worst(states) {
  let out = '';
  let best = -1;
  for (const s of states) {
    const r = RANK[s] ?? 0;
    if (r > best) {
      best = r;
      out = s;
    }
  }
  return out;
}

// columnOf is the column a task state puts a card in.
export function columnOf(state) {
  if (!state) return 'inbox';
  if (state === 'blocked') return 'needs you';
  if (state === 'done') return 'done';
  return 'working';
}

// columns sorts tasks into the four columns. A task's state is the one the
// server gave. With none, it is the worst state of its panes. Each card is
// { id, label, state, panes, ahead }, and panes holds only the panes the
// list knows.
export function columns(tasks, panes) {
  const known = new Map();
  for (const p of panes || []) if (p && typeof p.id === 'string') known.set(p.id, p);
  const out = COLUMNS.map((name) => ({ name, cards: [] }));
  for (const t of tasks || []) {
    if (!t || typeof t.id !== 'string') continue;
    const held = (t.panes || []).filter((id) => known.has(id));
    const state = typeof t.state === 'string' && t.state !== ''
      ? t.state
      : worst(held.map((id) => known.get(id).state || ''));
    const card = { id: t.id, label: t.label || t.id, state, panes: held };
    if (typeof t.ahead === 'number') card.ahead = t.ahead;
    out[COLUMNS.indexOf(columnOf(state))].cards.push(card);
  }
  return out;
}

// The rules of the rail, with no DOM. The rail is a tree: each task with
// its agents under it, then the agents with no task grouped by project.
// With no task and one project it is a flat list. A group folds to one
// line with its state counts. roster.js draws what these return, and the
// floor reads the same order for the arrows, the numbers and the fill.

import { ORDER, stateChip } from './chips.js';

// rowFromEvent is a pane row after one state event. Every field the event
// carries replaces the old one. The hold is the event's own: an event with
// no hold ends it. The detail is the event's own too: it says, for one,
// that the pane shows Claude's trust screen, and a later event with no
// detail ends that.
export function rowFromEvent(old, msg) {
  return {
    ...old, state: msg.state, source: msg.source, harness: msg.harness || old.harness, ts: msg.ts,
    ask: msg.ask || null, held: msg.held || null, detail: msg.detail || '',
  };
}

// ROW_TYPE is the drag data type a rail row carries: the pane id.
export const ROW_TYPE = 'application/x-coppice-pane';

// NO_DIR names the group of agents with no directory.
export const NO_DIR = 'no directory';

// MENU_MAX is the most projects the New menu lists.
export const MENU_MAX = 12;

// baseName is the last segment of a path, or ''.
export function baseName(path) {
  const parts = String(path || '').split('/').filter(Boolean);
  return parts.length ? parts[parts.length - 1] : '';
}

// byState sorts rows blocked first, then working, and keeps the list order
// within a state.
function byState(rows) {
  return rows
    .map((row, i) => [row, i])
    .sort((a, b) => ((ORDER[a[0].state] ?? 5) - (ORDER[b[0].state] ?? 5)) || a[1] - b[1])
    .map((e) => e[0]);
}

// groupCounts counts rows by state word.
export function groupCounts(rows) {
  const c = { blocked: 0, working: 0, idle: 0, unknown: 0, done: 0 };
  for (const row of rows || []) c[stateChip(row && row.state).word] += 1;
  return c;
}

// countParts is what a group line shows for its counts, asks first. text is
// the short form on screen and words the long form a screen reader hears.
export function countParts(counts) {
  const c = counts || {};
  const out = [];
  const n = (s) => Number(c[s]) || 0;
  if (n('blocked')) out.push({ state: 'blocked', n: n('blocked'), text: n('blocked') + ' ask', words: n('blocked') + (n('blocked') === 1 ? ' needs you' : ' need you') });
  for (const s of ['working', 'idle', 'unknown', 'done']) {
    if (n(s)) out.push({ state: s, n: n(s), text: String(n(s)), words: n(s) + ' ' + s });
  }
  return out;
}

// taskTree reads the task list into roots and children, each in list
// order. A task whose parent the list lacks is a root.
function taskTree(tasks) {
  const list = (Array.isArray(tasks) ? tasks : []).filter((t) => t && t.id);
  const ids = new Set(list.map((t) => t.id));
  const kids = new Map();
  const roots = [];
  for (const t of list) {
    if (t.parent && ids.has(t.parent) && t.parent !== t.id) {
      if (!kids.has(t.parent)) kids.set(t.parent, []);
      kids.get(t.parent).push(t);
    } else {
      roots.push(t);
    }
  }
  return { list, ids, kids, roots };
}

// railEntries is the rail from top to bottom: group lines and agent rows,
// each with its depth. folded is a set of group keys. A folded group keeps
// its line and hides everything under it. A group line carries its key,
// label, counts over every agent under it, total and folded.
export function railEntries(rows, tasks, folded) {
  const all = Array.isArray(rows) ? rows.filter((x) => x && x.id) : [];
  const shut = folded instanceof Set ? folded : new Set(folded || []);
  const tree = taskTree(tasks);
  const byTask = new Map();
  const loose = [];
  for (const row of all) {
    if (row.task && tree.ids.has(row.task)) {
      if (!byTask.has(row.task)) byTask.set(row.task, []);
      byTask.get(row.task).push(row);
    } else {
      loose.push(row);
    }
  }
  const out = [];
  const seen = new Set();
  // under is every row of task t and the tasks below it.
  const under = (t, walked = new Set()) => {
    if (walked.has(t.id)) return [];
    walked.add(t.id);
    const mine = [...(byTask.get(t.id) || [])];
    for (const k of tree.kids.get(t.id) || []) mine.push(...under(k, walked));
    return mine;
  };
  const walk = (t, depth) => {
    if (seen.has(t.id)) return;
    seen.add(t.id);
    const key = 'task:' + t.id;
    const inside = under(t);
    const isShut = shut.has(key);
    out.push({ type: 'group', kind: 'task', key, label: t.label || t.id, depth, counts: groupCounts(inside), total: inside.length, folded: isShut });
    if (isShut) return;
    for (const row of byState(byTask.get(t.id) || [])) out.push({ type: 'row', row, depth: depth + 1 });
    for (const k of tree.kids.get(t.id) || []) walk(k, depth + 1);
  };
  for (const t of tree.roots) walk(t, 0);

  const dirs = new Map();
  for (const row of loose) {
    const cwd = String(row.cwd || '');
    if (!dirs.has(cwd)) dirs.set(cwd, []);
    dirs.get(cwd).push(row);
  }
  // With no task and one project there is nothing to group.
  if (tree.list.length === 0 && dirs.size <= 1) {
    for (const row of byState(loose)) out.push({ type: 'row', row, depth: 0 });
    return out;
  }
  const groups = [...dirs.entries()]
    .map(([cwd, list]) => ({ cwd, label: baseName(cwd) || NO_DIR, list }))
    .sort((a, b) => (a.cwd === '') - (b.cwd === '') || a.label.localeCompare(b.label) || a.cwd.localeCompare(b.cwd));
  for (const g of groups) {
    const key = 'dir:' + g.cwd;
    const isShut = shut.has(key);
    out.push({ type: 'group', kind: 'project', key, label: g.label, depth: 0, counts: groupCounts(g.list), total: g.list.length, folded: isShut });
    if (isShut) continue;
    for (const row of byState(g.list)) out.push({ type: 'row', row, depth: 1 });
  }
  return out;
}

// railOrder is the pane ids of the rows the rail shows, top to bottom.
export function railOrder(entries) {
  return (entries || []).filter((e) => e.type === 'row').map((e) => e.row.id);
}

// treeOrder is every agent in tree order, as if no group were folded.
export function treeOrder(rows, tasks) {
  return railOrder(railEntries(rows, tasks, new Set()));
}

// killKey is what a key does while a kill waits for its confirm: 'stop',
// 'keep', or null for a key that leaves the confirm alone. A key an input
// method is still composing is the input method's.
export function killKey(e) {
  if (!e || e.ctrlKey || e.altKey || e.metaKey || e.isComposing) return null;
  if (e.key === 'Enter') return 'stop';
  if (e.key === 'Escape') return 'keep';
  return null;
}

// projectMenu is the New menu's projects: pinned first, each path once,
// at most MENU_MAX. A project with no name takes its path's base name.
export function projectMenu(projects) {
  const seen = new Set();
  const out = [];
  for (const p of Array.isArray(projects) ? projects : []) {
    if (!p || typeof p.path !== 'string' || !p.path || seen.has(p.path)) continue;
    seen.add(p.path);
    out.push({ path: p.path, name: p.name || baseName(p.path), pinned: p.pinned === true });
  }
  const pinned = out.filter((p) => p.pinned);
  return [...pinned, ...out.filter((p) => !p.pinned)].slice(0, MENU_MAX);
}

// dropAction is what a drop on window i does. drag.slot is the window a
// dragged header came from, or null; drag.pane is the agent a dragged rail
// row carries, or ''.
export function dropAction(drag, i) {
  if (!drag) return null;
  if (Number.isInteger(drag.slot)) return drag.slot === i ? null : { kind: 'swap', from: drag.slot, to: i };
  if (drag.pane) return { kind: 'put', pane: drag.pane, window: i };
  return null;
}

// ago is a time span in its largest useful unit.
function ago(seconds) {
  const s = Math.max(0, Math.floor(seconds));
  if (s < 60) return s + 's';
  if (s < 3600) return Math.floor(s / 60) + 'm';
  if (s < 86400) return Math.floor(s / 3600) + 'h';
  return Math.floor(s / 86400) + 'd';
}

// recentLine is the muted line under an ended agent in Recent: its
// project, its exit code when known, and when it ended.
export function recentLine(rec, nowSeconds) {
  const parts = [];
  const dir = baseName(rec && rec.cwd);
  if (dir) parts.push(dir);
  if (rec && Number.isInteger(rec.exit_code)) parts.push(rec.exit_code < 0 ? 'killed' : 'exit ' + rec.exit_code);
  if (rec && typeof rec.ended_at === 'number') parts.push(ago(nowSeconds - rec.ended_at) + ' ago');
  return parts.join(' · ');
}

// The tree view: tasks and their panes as a text tree, the same lines the
// terminal floor draws for its tree word. The view runs in a sandbox and
// holds no token. The floor page posts it the tasks, the panes and the
// selection it read over its own connection. A pane the floor had selected
// is marked. Every mark is a state the socket reports. A click on a pane
// line asks the floor page to open that pane.

// stateMark is the dot and word a tree line ends with.
export function stateMark(state) {
  switch (state) {
    case 'blocked': return '● needs you';
    case 'working': return '● working';
    case 'idle': return '○ idle';
    case 'done': return '· done';
    case '': case undefined: case null: return '';
    default: return '· ' + state;
  }
}

function base(path) {
  const parts = String(path || '').split('/').filter(Boolean);
  return parts.length ? parts[parts.length - 1] : '';
}

// taskText is a task line without its prefix: the label, the worktree's
// base name, the ahead count, and the state mark.
export function taskText(t) {
  const parts = [t.label || t.id];
  if (t.worktree) parts.push(base(t.worktree));
  if (typeof t.ahead === 'number') parts.push('+' + t.ahead);
  const mark = stateMark(t.state);
  if (mark) parts.push(mark);
  return parts.join('  ');
}

// paneText is a pane line without its prefix: the label and the state.
export function paneText(p) {
  const label = p.label || p.id;
  const mark = stateMark(p.state);
  return mark ? label + '  ' + mark : label;
}

// treeLines walks the tasks depth first. Each line is { text, task, pane }:
// a task line names its task, a pane line its pane. Under a task its panes
// come first, then its child tasks.
export function treeLines(tasks, panes) {
  const byId = new Map();
  const children = new Map();
  for (const t of tasks || []) byId.set(t.id, t);
  const roots = [];
  for (const t of tasks || []) {
    if (byId.has(t.parent) && t.parent !== t.id) {
      if (!children.has(t.parent)) children.set(t.parent, []);
      children.get(t.parent).push(t.id);
    } else {
      roots.push(t.id);
    }
  }
  const paneRows = new Map();
  for (const p of panes || []) paneRows.set(p.id, p);
  const seen = new Set();
  const out = [];
  const walk = (id, indent, last) => {
    if (seen.has(id)) return;
    seen.add(id);
    const t = byId.get(id);
    const branch = last ? '└─ ' : '├─ ';
    const deeper = last ? '   ' : '│  ';
    out.push({ text: indent + branch + taskText(t), task: id, pane: '' });
    const here = (t.panes || []).filter((p) => paneRows.has(p)).map((p) => paneRows.get(p));
    const kids = children.get(id) || [];
    here.forEach((p, i) => {
      const b = i === here.length - 1 && kids.length === 0 ? '└─ ' : '├─ ';
      out.push({ text: indent + deeper + b + paneText(p), task: '', pane: p.id });
    });
    kids.forEach((kid, i) => walk(kid, indent + deeper, i === kids.length - 1));
  };
  roots.forEach((id, i) => walk(id, '', i === roots.length - 1));
  return out;
}

// markSelected sets selected on each pane line whose pane is in sel.
export function markSelected(lines, sel) {
  const want = new Set(sel || []);
  return lines.map((l) => ({ ...l, selected: Boolean(l.pane) && want.has(l.pane) }));
}

// WAITING is what the page says until the floor page sends it data.
const WAITING = 'Open this view from the floor page. It shows what the floor page sends it.';

// mountTree draws what the floor page posts into list. post sends one
// message to the floor page.
export function mountTree(status, list, { post }) {
  status.textContent = WAITING;
  const render = (data) => {
    if (!data || data.type !== 'coppice.data') return;
    const tasks = Array.isArray(data.tasks) ? data.tasks : [];
    const panes = Array.isArray(data.panes) ? data.panes : [];
    const lines = markSelected(treeLines(tasks, panes), Array.isArray(data.sel) ? data.sel : []);
    list.textContent = '';
    for (const l of lines) {
      const li = document.createElement('li');
      li.textContent = l.text;
      if (l.selected) li.className = 'sel';
      if (l.pane) {
        li.dataset.pane = l.pane;
        li.addEventListener('click', () => post({ type: 'open-pane', pane: l.pane }));
      }
      list.append(li);
    }
    status.textContent = lines.length ? '' : 'No tasks yet.';
  };
  return { render };
}

function boot() {
  const framed = window.parent && window.parent !== window;
  const tree = mountTree(document.getElementById('tree-status'), document.getElementById('tree-lines'), {
    post: (msg) => { if (framed) window.parent.postMessage(msg, '*'); },
  });
  window.addEventListener('message', (e) => {
    if (framed && e.source === window.parent) tree.render(e.data);
  });
}

// A page boots once its body exists. A plain import, as in a test, does
// not.
if (typeof document !== 'undefined' && document.body) {
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot);
  else boot();
}

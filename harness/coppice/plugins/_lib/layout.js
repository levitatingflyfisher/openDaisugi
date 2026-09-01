// The tidy tree layout that the tree graph and the minimap share. Tasks
// and panes are nodes. A task's panes and child tasks sit one row below
// it, and a parent sits over the middle of its children. x counts leaf
// slots and y counts rows, so two nodes on one row are at least one slot
// apart.

// layout returns { nodes, edges }. Each node is { id, kind, x, y, state,
// label }, kind task or pane. state is the state the socket gave, and ''
// for a task with nothing in it. Each edge is [parent id, child id]. A
// pane that no task holds is a node of its own on the top row.
export function layout(tasks, panes) {
  const byId = new Map();
  for (const t of tasks || []) if (t && typeof t.id === 'string') byId.set(t.id, t);
  const kids = new Map();
  const roots = [];
  for (const t of byId.values()) {
    if (byId.has(t.parent) && t.parent !== t.id) {
      if (!kids.has(t.parent)) kids.set(t.parent, []);
      kids.get(t.parent).push(t.id);
    } else {
      roots.push(t.id);
    }
  }
  const paneById = new Map();
  for (const p of panes || []) if (p && typeof p.id === 'string') paneById.set(p.id, p);
  const nodes = [];
  const edges = [];
  const seen = new Set();
  let next = 0;
  const leaf = (p, y) => {
    seen.add(p.id);
    nodes.push({ id: p.id, kind: 'pane', x: next++, y, state: p.state || '', label: p.label || p.id });
  };
  const walk = (id, y) => {
    seen.add(id);
    const t = byId.get(id);
    const xs = [];
    for (const pid of t.panes || []) {
      const p = paneById.get(pid);
      if (!p || seen.has(pid)) continue;
      leaf(p, y + 1);
      edges.push([id, pid]);
      xs.push(nodes[nodes.length - 1].x);
    }
    for (const kid of kids.get(id) || []) {
      if (seen.has(kid)) continue;
      xs.push(walk(kid, y + 1));
      edges.push([id, kid]);
    }
    const x = xs.length ? (Math.min(...xs) + Math.max(...xs)) / 2 : next++;
    nodes.push({ id, kind: 'task', x, y, state: t.state || '', label: t.label || id });
    return x;
  };
  for (const id of roots) walk(id, 0);
  // Tasks whose parents form a cycle have no root. Each starts its own tree.
  for (const id of byId.keys()) if (!seen.has(id)) walk(id, 0);
  for (const p of paneById.values()) if (!seen.has(p.id)) leaf(p, 0);
  return { nodes, edges };
}

// nearest is the point in points closest to x, y within reach, or null.
// Each point has px and py.
export function nearest(points, x, y, reach) {
  let best = null;
  let bestD = Infinity;
  for (const p of points || []) {
    const d = Math.hypot(p.px - x, p.py - y);
    if (d <= reach && d < bestD) {
      best = p;
      bestD = d;
    }
  }
  return best;
}

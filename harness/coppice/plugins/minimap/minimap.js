// The minimap: the tree shrunk to dots. Every dot is a task or a pane, in
// the colour of the state the socket reports. A task with nothing in it
// has no state and draws as a ring with no fill.
import { layout, nearest } from '../_lib/layout.js';
import { stateColor, stateWord } from '../_lib/view.js';

// MAX_R is the largest dot radius. RING is how far past a dot the ring
// that marks it as selected reaches, stroke included. The margin round the
// map is a dot and its ring, so no dot is clipped at an edge.
const MAX_R = 6;
const RING = 3;

// dots fits the tree into w by h pixels. Each dot is { id, kind, x, y, r,
// color }. color is null for a node with no state.
export function dots(tasks, panes, w, h) {
  const lay = layout(tasks, panes);
  if (!lay.nodes.length) return [];
  const maxX = Math.max(...lay.nodes.map((n) => n.x));
  const maxY = Math.max(...lay.nodes.map((n) => n.y));
  const wide = MAX_R + RING;
  const r = Math.max(2, Math.min(MAX_R, (w - 2 * wide) / Math.max(1, maxX) / 3, (h - 2 * wide) / Math.max(1, maxY) / 3));
  const pad = r + RING;
  const sx = (w - 2 * pad) / Math.max(1, maxX);
  const sy = (h - 2 * pad) / Math.max(1, maxY);
  return lay.nodes.map((n) => ({
    id: n.id,
    kind: n.kind,
    x: maxX === 0 ? w / 2 : pad + n.x * sx,
    y: maxY === 0 ? h / 2 : pad + n.y * sy,
    r,
    color: n.state ? stateColor(stateWord(n.state)) : null,
  }));
}

// hit is the id of the dot under a click at x, y, or ''.
export function hit(ds, x, y) {
  const points = (ds || []).map((d) => ({ ...d, px: d.x, py: d.y }));
  const reach = ds && ds.length ? ds[0].r + 4 : 0;
  const found = nearest(points, x, y, reach);
  return found ? found.id : '';
}

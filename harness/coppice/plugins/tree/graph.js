// The tree as a graph: the pure part. layout places tasks and panes, and
// project turns slots into pixels for a pan. The canvas drawing is in
// graph-view.js.
import { layout, nearest } from '../_lib/layout.js';

export { layout };

// UNIT is the pixel width of one leaf slot and the height of one row.
export const UNIT = 64;

// RADIUS is the pixel radius of one node.
export const RADIUS = 9;

// project places each node of lay in pixels, pan pixels from the top left.
// Each placed node keeps its fields and gains px and py at its centre.
export function project(lay, pan) {
  const off = pan || { x: 0, y: 0 };
  return (lay.nodes || []).map((n) => ({
    ...n,
    px: off.x + n.x * UNIT + UNIT / 2,
    py: off.y + n.y * UNIT + UNIT / 2,
  }));
}

// nodeAt is the placed node under a click at x, y, or null.
export function nodeAt(placed, x, y) {
  return nearest(placed, x, y, RADIUS + 4);
}

// panesOf is the panes a click on node selects: the pane itself, or every
// pane a task node holds.
export function panesOf(node, lay) {
  if (!node) return [];
  if (node.kind === 'pane') return [node.id];
  return (lay.edges || [])
    .filter((e) => e[0] === node.id)
    .map((e) => e[1])
    .filter((id) => (lay.nodes || []).some((n) => n.id === id && n.kind === 'pane'));
}

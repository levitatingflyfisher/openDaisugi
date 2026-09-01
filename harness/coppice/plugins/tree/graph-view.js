// The tree graph on a canvas. The text tree shows first, and the toggle
// switches between the two. Drag pans the graph. A click on a node sets
// the selection to its panes. A double click on a pane opens it.
import { connect, boot, stateColor, stateWord } from '../_lib/view.js';
import { layout, project, nodeAt, panesOf, RADIUS } from './graph.js';

const MUTED = '#98a3a0';
const MARK = '#e8eae7';
// MOVE_PX is how far a press may move and still count as a click.
const MOVE_PX = 4;

export function mountGraph(canvas, toggle, lines, view) {
  let pan = { x: 0, y: 0 };
  let lay = { nodes: [], edges: [] };
  let placed = [];
  let press = null;

  const draw = () => {
    if (canvas.hidden) return;
    canvas.width = canvas.clientWidth || 600;
    canvas.height = canvas.clientHeight || 400;
    const ctx = canvas.getContext('2d');
    ctx.clearRect(0, 0, canvas.width, canvas.height);
    placed = project(lay, pan);
    const at = new Map(placed.map((p) => [p.id, p]));
    ctx.strokeStyle = MUTED;
    ctx.lineWidth = 1;
    for (const [a, b] of lay.edges) {
      const p = at.get(a);
      const q = at.get(b);
      if (!p || !q) continue;
      ctx.beginPath();
      ctx.moveTo(p.px, p.py);
      ctx.lineTo(q.px, q.py);
      ctx.stroke();
    }
    const sel = new Set(view.sel());
    ctx.font = '12px ui-monospace, Menlo, monospace';
    ctx.textBaseline = 'middle';
    for (const p of placed) {
      ctx.beginPath();
      ctx.arc(p.px, p.py, RADIUS, 0, Math.PI * 2);
      if (p.state) {
        ctx.fillStyle = stateColor(stateWord(p.state));
        ctx.fill();
      } else {
        ctx.strokeStyle = MUTED;
        ctx.stroke();
      }
      if (sel.has(p.id)) {
        ctx.strokeStyle = MARK;
        ctx.lineWidth = 2;
        ctx.beginPath();
        ctx.arc(p.px, p.py, RADIUS + 3, 0, Math.PI * 2);
        ctx.stroke();
        ctx.lineWidth = 1;
      }
      ctx.fillStyle = MUTED;
      ctx.fillText(p.label, p.px + RADIUS + 4, p.py);
    }
  };

  const render = () => {
    lay = layout(view.tasks(), view.panes());
    draw();
  };

  const point = (e) => {
    const box = canvas.getBoundingClientRect();
    return { x: e.clientX - box.left, y: e.clientY - box.top };
  };
  canvas.addEventListener('pointerdown', (e) => {
    press = { start: point(e), pan: { ...pan }, moved: false };
  });
  canvas.addEventListener('pointermove', (e) => {
    if (!press) return;
    const p = point(e);
    const dx = p.x - press.start.x;
    const dy = p.y - press.start.y;
    if (Math.hypot(dx, dy) > MOVE_PX) press.moved = true;
    if (press.moved) {
      pan = { x: press.pan.x + dx, y: press.pan.y + dy };
      draw();
    }
  });
  canvas.addEventListener('pointerup', (e) => {
    const was = press;
    press = null;
    if (!was || was.moved) return;
    const p = point(e);
    const node = nodeAt(placed, p.x, p.y);
    if (node) view.setSel(panesOf(node, lay));
  });
  canvas.addEventListener('dblclick', (e) => {
    const p = point(e);
    const node = nodeAt(placed, p.x, p.y);
    if (node && node.kind === 'pane') view.openPane(node.id);
  });
  toggle.addEventListener('click', () => {
    const toGraph = canvas.hidden;
    canvas.hidden = !toGraph;
    lines.hidden = toGraph;
    toggle.textContent = toGraph ? 'Text' : 'Graph';
    draw();
  });
  view.onData(render);
  return { render, draw };
}

boot(() => {
  mountGraph(
    document.getElementById('tree-graph'),
    document.getElementById('tree-toggle'),
    document.getElementById('tree-lines'),
    connect(window),
  );
});

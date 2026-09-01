// The minimap on a canvas. A click on a pane dot asks the floor to open
// that pane. A click on a task dot opens the pane of that task that needs
// a person most.
import { connect, boot, byNeed } from '../_lib/view.js';
import { dots, hit } from './minimap.js';

const RING = '#98a3a0';
const MARK = '#e8eae7';

export function mountMinimap(canvas, view) {
  let ds = [];
  const draw = () => {
    canvas.width = canvas.clientWidth || 200;
    canvas.height = canvas.clientHeight || 80;
    const ctx = canvas.getContext('2d');
    ctx.clearRect(0, 0, canvas.width, canvas.height);
    ds = dots(view.tasks(), view.panes(), canvas.width, canvas.height);
    const sel = new Set(view.sel());
    for (const d of ds) {
      ctx.beginPath();
      ctx.arc(d.x, d.y, d.r, 0, Math.PI * 2);
      if (d.color) {
        ctx.fillStyle = d.color;
        ctx.fill();
      } else {
        ctx.strokeStyle = RING;
        ctx.stroke();
      }
      if (sel.has(d.id)) {
        ctx.strokeStyle = MARK;
        ctx.beginPath();
        ctx.arc(d.x, d.y, d.r + 2, 0, Math.PI * 2);
        ctx.stroke();
      }
    }
  };
  canvas.addEventListener('click', (e) => {
    const box = canvas.getBoundingClientRect();
    const id = hit(ds, e.clientX - box.left, e.clientY - box.top);
    const d = ds.find((x) => x.id === id);
    if (!d) return;
    if (d.kind === 'pane') {
      view.openPane(id);
      return;
    }
    const task = view.tasks().find((t) => t.id === id);
    const held = new Set((task && task.panes) || []);
    const first = byNeed(view.panes().filter((p) => held.has(p.id)))[0];
    if (first) view.openPane(first.id);
  });
  view.onData(draw);
  return { draw };
}

boot(() => mountMinimap(document.getElementById('minimap'), connect(window)));

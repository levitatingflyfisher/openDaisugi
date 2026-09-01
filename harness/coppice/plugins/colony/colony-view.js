// The colony on a canvas. A drag box sets the selection to the foragers
// inside it. A click on one forager selects it, and a double click opens
// it. The floor page acts on the selection. The colony only shows it.
// A field no taller than a strip draws one evenly spaced row with each
// label cut to its slot, and a click there opens the agent.
import { connect, boot, stateColor, stateWord } from '../_lib/view.js';
import { place, inBox, bar, quietFor, foragerAt, RADIUS, stripPlace, fitLabel, isStrip } from './colony.js';

const SEED = 1;
const MUTED = '#98a3a0';
const MARK = '#e8eae7';
const BAR_PX = 28;
const MOVE_PX = 4;
const WAITING = 'Open this view from the floor page. It shows what the floor page sends it.';

export function mountColony(status, canvas, view, now) {
  const clock = now || (() => Date.now() / 1000);
  let pos = [];
  let box = null;
  status.textContent = WAITING;
  let strip = false;
  const draw = () => {
    const w = canvas.clientWidth || 600;
    const h = canvas.clientHeight || 400;
    const dpr = typeof window !== 'undefined' && Number(window.devicePixelRatio) > 0 ? Number(window.devicePixelRatio) : 1;
    canvas.width = Math.round(w * dpr);
    canvas.height = Math.round(h * dpr);
    const ctx = canvas.getContext('2d');
    if (typeof ctx.setTransform === 'function') ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, w, h);
    const panes = view.panes();
    strip = isStrip(canvas.clientHeight);
    pos = strip ? stripPlace(panes, w, h) : place(panes, w, h, SEED);
    const byId = new Map(panes.map((p) => [p.id, p]));
    const sel = new Set(view.sel());
    const t = clock();
    ctx.font = '11px ui-monospace, Menlo, monospace';
    ctx.textBaseline = 'top';
    for (const p of pos) {
      const pane = byId.get(p.id);
      ctx.beginPath();
      ctx.arc(p.x, p.y, RADIUS, 0, Math.PI * 2);
      ctx.fillStyle = stateColor(stateWord(pane.state));
      ctx.fill();
      if (sel.has(p.id)) {
        ctx.strokeStyle = MARK;
        ctx.beginPath();
        ctx.arc(p.x, p.y, RADIUS + 3, 0, Math.PI * 2);
        ctx.stroke();
      }
      const len = bar(quietFor(pane, view.events(), t));
      if (len !== null) {
        ctx.fillStyle = MUTED;
        ctx.fillRect(p.x - BAR_PX / 2, p.y + RADIUS + (strip ? 1 : 3), Math.max(1, len * BAR_PX), 2);
      }
      ctx.fillStyle = MUTED;
      if (strip) {
        const measure = (t) => ctx.measureText(t).width;
        const label = fitLabel(pane.label || p.id, p.slot - 6, measure);
        ctx.fillText(label, p.x - measure(label) / 2, p.y + RADIUS + 4);
      } else {
        ctx.fillText(pane.label || p.id, p.x - BAR_PX / 2, p.y + RADIUS + 7);
      }
    }
    if (box) {
      ctx.strokeStyle = MARK;
      ctx.strokeRect(box.x0, box.y0, box.x1 - box.x0, box.y1 - box.y0);
    }
    status.textContent = panes.length ? '' : 'No panes yet.';
  };
  const point = (e) => {
    const r = canvas.getBoundingClientRect();
    return { x: e.clientX - r.left, y: e.clientY - r.top };
  };
  canvas.addEventListener('pointerdown', (e) => {
    const p = point(e);
    box = { x0: p.x, y0: p.y, x1: p.x, y1: p.y };
  });
  canvas.addEventListener('pointermove', (e) => {
    if (!box) return;
    const p = point(e);
    box.x1 = p.x;
    box.y1 = p.y;
    draw();
  });
  canvas.addEventListener('pointerup', () => {
    const b = box;
    box = null;
    if (!b) return;
    if (strip) {
      // A strip opens the agent in a click; its slot is the target.
      const hit = pos.find((p) => Math.abs(p.x - b.x0) <= p.slot / 2);
      if (hit && Math.hypot(b.x1 - b.x0, b.y1 - b.y0) <= MOVE_PX * 3) view.openPane(hit.id);
      return;
    }
    if (Math.hypot(b.x1 - b.x0, b.y1 - b.y0) <= MOVE_PX) {
      const id = foragerAt(pos, b.x0, b.y0);
      view.setSel(id ? [id] : []);
    } else {
      view.setSel(inBox(pos, b));
    }
    draw();
  });
  canvas.addEventListener('dblclick', (e) => {
    const p = point(e);
    const id = foragerAt(pos, p.x, p.y);
    if (id) view.openPane(id);
  });
  view.onData(draw);
  return { draw };
}

boot(() => mountColony(
  document.getElementById('colony-status'),
  document.getElementById('colony-field'),
  connect(window),
));

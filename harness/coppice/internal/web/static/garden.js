// The garden: an overlay that draws the floor as a daisugi, the cedar that
// is pruned to one broad, low, gnarled trunk with straight shoots rising
// from its limbs. The foreman is the trunk. Each agent is a shoot with a
// cedar crown. An agent that another agent started rises from the side of
// its parent shoot, smaller. A shoot shows what the agent does: the crown
// sways and sparks drift while it works, a lantern hangs from the crown
// and glows when it needs you, the crown stands still when it idles, turns
// autumn amber when it is done, and the stem carries a red notch when the
// gate denied its last call.
//
// The art is drawn on one canvas with the page's own colour tokens. The
// pure parts come first: the layout, the look of one state, the hit test
// and the key. mountGarden draws them.

import { GARDEN } from './overlay.js';

export { GARDEN };

// LABEL_MAX is the most characters of a pane name the picture shows.
export const LABEL_MAX = 14;

// gardenKey is true for the key that opens or closes the garden: plain g,
// with no modifier, while the keys are not in a window and not in a field.
export function gardenKey(e, keys) {
  if (!e || e.key !== 'g' || e.repeat || e.ctrlKey || e.metaKey || e.altKey || e.shiftKey) return false;
  if (keys === 'window') return false;
  const t = e.target;
  return !(t && (['INPUT', 'TEXTAREA', 'SELECT'].includes(t.tagName) || t.isContentEditable));
}

// lookOf is how one pane looks. kind is one of grow, lantern, still, bud
// and notch. sway says the shoot moves, pulse that its light or sparks
// move, loud that it is the one thing meant to catch the eye. A blocked
// pane whose ask a foreman holds reads as working. A notch shows when the
// last gate verdict was a deny and the agent is not working on.
export function lookOf(pane) {
  const p = pane || {};
  let state = String(p.state || 'unknown');
  if (state === 'blocked' && p.held) state = 'working';
  if (state === 'blocked') return { kind: 'lantern', token: 'blocked', sway: false, pulse: true, loud: true };
  const denied = p.gate && p.gate.decision === 'deny';
  if (denied && state !== 'working') return { kind: 'notch', token: 'bad', sway: false, pulse: false, loud: false };
  if (state === 'working') return { kind: 'grow', token: 'working', sway: true, pulse: true, loud: false };
  if (state === 'done') return { kind: 'bud', token: 'done', sway: false, pulse: false, loud: false };
  return { kind: 'still', token: state === 'idle' ? 'idle' : 'unknown', sway: false, pulse: false, loud: false };
}

// shortLabel cuts a pane name to max characters, LABEL_MAX by default. A
// narrow canvas cuts it shorter.
export function shortLabel(label, id, max = LABEL_MAX) {
  const s = String(label || id || '').trim();
  const chars = [...s];
  return chars.length > max ? chars.slice(0, max - 1).join('') + '…' : s;
}

// pillSize is the size of a label pill. Text is monospace at 12 px.
function pillSize(text) {
  return { w: Math.round([...text].length * 7.3 + 16), h: 20 };
}

const overlaps = (a, b) => a.x0 < b.x1 && b.x0 < a.x1 && a.y0 < b.y1 && b.y0 < a.y1;

// trunkTop is the y of the trunk's rounded back at x.
function trunkTop(t, x) {
  const k = (x - t.x) / (t.w / 2);
  return t.y - t.h * Math.sqrt(Math.max(0.12, 1 - k * k));
}

// gardenLayout places the panes on a w by h canvas. foreman is the pane
// that is the trunk, or ''. It returns { w, h, ground, trunk, nodes }.
// trunk is { id, label, x, y, w, h, pill }: x the middle, y the ground, w
// and h its size. id is '' when no foreman shows, and the trunk stands
// bare. A node is { id, label, look, parent, depth, x, y, r, from, base,
// stemTop, f, pill, phase }: x and y are the middle of the crown, r its
// size, base where the stem rises, from where it leaves its parent (the
// same as base on a shoot that rises from a limb), f the share of the
// parent's stem where it leaves, and pill the label's box { x, y, w, h,
// text }. The foreman is not a node. Its children and every agent with no
// parent rise from its limbs.
export function gardenLayout(panes, w, h, foreman) {
  const list = (Array.isArray(panes) ? panes : []).filter((p) => p && typeof p.id === 'string');
  const byId = new Map(list.map((p) => [p.id, p]));
  const kids = new Map();
  const roots = [];
  for (const p of list) {
    const up = p.parent_pane;
    if (typeof up === 'string' && up !== p.id && byId.has(up)) {
      if (!kids.has(up)) kids.set(up, []);
      kids.get(up).push(p.id);
    } else roots.push(p.id);
  }
  // A cycle has no root. Its panes still show, as roots.
  const seen = new Set();
  const mark = (id) => { if (seen.has(id)) return; seen.add(id); for (const k of kids.get(id) || []) mark(k); };
  for (const r of roots) mark(r);
  // The link that closes a cycle is cut, so the walk ends.
  for (const p of list) {
    if (seen.has(p.id)) continue;
    kids.set(p.parent_pane, (kids.get(p.parent_pane) || []).filter((k) => k !== p.id));
    roots.push(p.id);
    mark(p.id);
  }

  const trunkId = roots.includes(foreman) ? foreman : '';
  const shoots = [...(kids.get(trunkId) || []), ...roots.filter((r) => r !== trunkId)];
  const weight = (id) => 1 + 0.9 * (kids.get(id) || []).length;
  const total = shoots.reduce((n, id) => n + weight(id), 0);

  const ground = Math.round(h * 0.9);
  const trunkW = Math.min(w - 24, Math.max(w * 0.6, Math.min(w - 60, total * 74)));
  const trunk = { id: trunkId, label: trunkId ? 'foreman' : '', x: w / 2, y: ground, w: trunkW, h: Math.min(h * 0.2, trunkW * 0.24), pill: null };
  const obstacles = [];
  if (trunkId) {
    const sz = pillSize('foreman');
    trunk.pill = { x: trunk.x, y: ground - trunk.h * 0.3, ...sz, text: 'foreman' };
    obstacles.push(rect(trunk.pill));
  }

  const r0 = Math.max(11, Math.min(h * 0.09, total ? (trunkW / total) * 0.42 : h * 0.09));
  const yTop = (r) => 1.4 * r + h * 0.05;
  const nodes = [];
  const grow = (id, r, parent, depth, from, base, height, f) => {
    const p = byId.get(id);
    const y = Math.max(yTop(r), base.y - height);
    const n = {
      id, label: shortLabel(p.label, p.id, w < 500 ? 9 : LABEL_MAX), look: lookOf(p), parent: parent ? parent.id : '', depth,
      x: base.x, y, r, from, base, stemTop: { x: base.x, y: y + 0.6 * r }, f, pill: null, phase: nodes.length * 1.7,
    };
    nodes.push(n);
    const cs = kids.get(id) || [];
    cs.forEach((cid, i) => {
      const along = 0.4 + 0.2 * ((i % 3) / 2);
      const ay = n.base.y - (n.base.y - n.stemTop.y) * along;
      const side = i % 2 === 0 ? 1 : -1;
      const cr = r * 0.68;
      const cx = Math.max(cr + 8, Math.min(w - cr - 8, n.x + side * (r * 1.55 + Math.floor(i / 2) * r * 1.1)));
      const ch = Math.max(cr * 3, (n.base.y - y) * 0.45);
      const e = Math.min(ch * 0.2, r);
      grow(cid, cr, n, depth + 1, { x: n.x, y: ay }, { x: cx, y: ay - e }, ch, along);
    });
  };
  let at = 0;
  shoots.forEach((id, k) => {
    const wt = weight(id);
    const x = trunk.x - trunkW / 2 + ((at + wt / 2) / total) * trunkW;
    at += wt;
    const tip = { x, y: trunkTop(trunk, x) - h * 0.1 };
    const pattern = [1, 0.74, 0.9, 0.62, 0.82][k % 5];
    const height = Math.max(r0 * 3, (tip.y - yTop(r0)) * pattern);
    grow(id, r0, null, 0, tip, tip, height, 0);
  });

  // Pills sit beside or above a crown, and never over a stem, a crown, a
  // lantern or another pill.
  for (const n of nodes) {
    obstacles.push({ x0: n.x - n.r, y0: n.y - 1.4 * n.r, x1: n.x + n.r, y1: n.y + 1.4 * n.r });
    obstacles.push({ x0: n.x - 4, y0: n.y + 1.4 * n.r, x1: n.x + 4, y1: n.base.y });
    if (n.depth > 0) {
      obstacles.push({ x0: Math.min(n.from.x, n.base.x) - 3, y0: n.base.y - 3, x1: Math.max(n.from.x, n.base.x) + 3, y1: n.from.y + 3 });
    }
    if (n.look.kind === 'lantern') {
      const lh = Math.max(24, n.r * 1.05);
      obstacles.push({ x0: n.x + 0.6 * n.r - lh, y0: n.y + 1.3 * n.r, x1: n.x + 0.6 * n.r + lh, y1: n.y + 1.3 * n.r + 0.5 * n.r + lh + 6 });
    }
  }
  const order = [...nodes].sort((a, b) => a.depth - b.depth || a.x - b.x);
  for (const n of order) {
    const sz = pillSize(n.label);
    const gap = 6;
    const spots = [
      [n.x + n.r + gap + sz.w / 2, n.y - 0.3 * n.r],
      [n.x - n.r - gap - sz.w / 2, n.y - 0.3 * n.r],
      [n.x, n.y - 1.4 * n.r - gap - sz.h / 2],
      [n.x + n.r + gap + sz.w / 2, n.y + 0.9 * n.r],
      [n.x - n.r - gap - sz.w / 2, n.y + 0.9 * n.r],
    ];
    for (let k = 1; k <= 14; k++) {
      for (const dx of [0, 0.6 * sz.w, -0.6 * sz.w, 1.2 * sz.w, -1.2 * sz.w]) spots.push([n.x + dx, n.y - 1.4 * n.r - gap - sz.h / 2 - k * (sz.h + 3)]);
    }
    let pick = null;
    for (const [px, py] of spots) {
      const x = Math.max(sz.w / 2 + 4, Math.min(w - sz.w / 2 - 4, px));
      const y = Math.max(sz.h / 2 + 4, Math.min(ground - 4, py));
      const box = { x0: x - sz.w / 2, y0: y - sz.h / 2, x1: x + sz.w / 2, y1: y + sz.h / 2 };
      if (!obstacles.some((o) => overlaps(box, o))) { pick = { x, y }; break; }
    }
    if (!pick) {
      // No near spot is free: take the free spot on the canvas nearest the crown.
      let best = Infinity;
      for (let y = sz.h / 2 + 4; y <= ground - 4; y += 6) {
        for (let x = sz.w / 2 + 4; x <= w - sz.w / 2 - 4; x += 12) {
          const d = (x - n.x) ** 2 + (y - n.y) ** 2;
          if (d >= best) continue;
          const box = { x0: x - sz.w / 2, y0: y - sz.h / 2, x1: x + sz.w / 2, y1: y + sz.h / 2 };
          if (!obstacles.some((o) => overlaps(box, o))) { best = d; pick = { x, y }; }
        }
      }
    }
    if (!pick) pick = { x: Math.max(sz.w / 2 + 4, Math.min(w - sz.w / 2 - 4, spots[2][0])), y: Math.max(sz.h / 2 + 4, spots[2][1]) };
    n.pill = { x: pick.x, y: pick.y, ...sz, text: n.label };
    obstacles.push(rect(n.pill));
  }
  return { w, h, ground, trunk, nodes };
}

function rect(p) {
  return { x0: p.x - p.w / 2, y0: p.y - p.h / 2, x1: p.x + p.w / 2, y1: p.y + p.h / 2 };
}

// hitNode is the id of the shoot whose crown is nearest to the point x, y,
// within radius or the crown's own size, or ''.
export function hitNode(nodes, x, y, radius) {
  let best = '';
  let bestD = Infinity;
  for (const n of nodes) {
    const reach = Math.max(radius, n.r * 1.3);
    const d = (n.x - x) ** 2 + (n.y - y) ** 2;
    if (d <= reach * reach && d < bestD) { best = n.id; bestD = d; }
  }
  return best;
}

// hitPane is the pane a press at x, y means: a shoot, else the foreman's
// trunk, else ''.
export function hitPane(model, x, y, radius, pills = true) {
  const id = hitNode(model.nodes, x, y, radius);
  if (id) return id;
  // A label and a lantern are part of their shoot.
  for (const n of model.nodes) {
    const p = n.pill;
    if (pills && p && Math.abs(x - p.x) <= p.w / 2 && Math.abs(y - p.y) <= p.h / 2) return n.id;
    if (n.look.kind === 'lantern') {
      const lh = Math.max(24, n.r * 1.05);
      const lx = n.x + 0.6 * n.r;
      const ly = n.y + 1.3 * n.r + 0.5 * n.r + lh / 2;
      if (Math.abs(x - lx) <= lh * 0.7 && Math.abs(y - ly) <= lh * 0.8) return n.id;
    }
  }
  const t = model.trunk;
  if (t.id && Math.abs(x - t.x) <= t.w / 2 && y <= t.y && y >= trunkTop(t, x)) return t.id;
  return '';
}

// animated is true when any shoot moves or pulses.
export function animated(nodes) {
  return nodes.some((n) => n.look.sway || n.look.pulse);
}

// parseColor reads #rgb, #rrggbb or rgb(r, g, b) as [r, g, b], or null.
export function parseColor(s) {
  const t = String(s || '').trim();
  let m = /^#([0-9a-f]{3})$/i.exec(t);
  if (m) return [...m[1]].map((c) => parseInt(c + c, 16));
  m = /^#([0-9a-f]{6})$/i.exec(t);
  if (m) return [0, 2, 4].map((i) => parseInt(m[1].slice(i, i + 2), 16));
  m = /^rgba?\(\s*(\d+)[,\s]+(\d+)[,\s]+(\d+)/i.exec(t);
  if (m) return [Number(m[1]), Number(m[2]), Number(m[3])];
  return null;
}

// mix is colour a toward colour b by t, as an rgb() string. A colour it
// cannot read gives a back.
export function mix(a, b, t) {
  const x = parseColor(a);
  const y = parseColor(b);
  if (!x || !y) return a;
  return 'rgb(' + x.map((v, i) => Math.round(v + (y[i] - v) * t)).join(',') + ')';
}

const FALLBACK = {
  bg: '#12181a', fg: '#e8eae7', muted: '#98a3a0', line: '#26312f',
  blocked: '#d98b4a', working: '#8fb996', idle: '#7f8c8a', done: '#5d6a68', unknown: '#b06868',
  accent: '#5fb3f0', bad: '#e0605a',
};

// pageColors reads the page's colour tokens.
export function pageColors() {
  const out = { ...FALLBACK };
  if (typeof getComputedStyle !== 'function' || typeof document === 'undefined' || !document.documentElement) return out;
  try {
    const cs = getComputedStyle(document.documentElement);
    for (const k of Object.keys(out)) {
      const v = cs.getPropertyValue('--' + k).trim();
      if (v) out[k] = v;
    }
  } catch { /* the fallback palette stays */ }
  return out;
}

// crownColor is the colour of a shoot's crown.
function crownColor(n, c) {
  const k = n.look.kind;
  if (k === 'grow') return mix(c.working, '#6fe08f', 0.4);
  if (k === 'bud') return mix(c.blocked, '#e8c45a', 0.55);
  if (k === 'lantern') return mix(c.working, c.idle, 0.55);
  return mix(c.working, c.idle, 0.75);
}

// trunkPath outlines the trunk's gnarled back: a rounded dome with knobs.
function trunkPath(ctx, t) {
  const l = t.x - t.w / 2;
  const r = t.x + t.w / 2;
  ctx.beginPath();
  ctx.moveTo(l - 6, t.y + 4);
  ctx.bezierCurveTo(l + t.w * 0.02, t.y - t.h * 0.35, l + t.w * 0.1, t.y - t.h * 0.9, l + t.w * 0.26, t.y - t.h * 0.88);
  ctx.bezierCurveTo(l + t.w * 0.34, t.y - t.h * 1.08, l + t.w * 0.44, t.y - t.h * 0.98, t.x, t.y - t.h);
  ctx.bezierCurveTo(r - t.w * 0.42, t.y - t.h * 1.05, r - t.w * 0.32, t.y - t.h * 1.0, r - t.w * 0.25, t.y - t.h * 0.86);
  ctx.bezierCurveTo(r - t.w * 0.1, t.y - t.h * 0.9, r - t.w * 0.02, t.y - t.h * 0.3, r + 6, t.y + 4);
  ctx.closePath();
}

// pill draws one label on a rounded box in the page's background colour.
function pill(ctx, p, c, loud) {
  ctx.beginPath();
  if (typeof ctx.roundRect === 'function') ctx.roundRect(p.x - p.w / 2, p.y - p.h / 2, p.w, p.h, 10);
  else ctx.rect(p.x - p.w / 2, p.y - p.h / 2, p.w, p.h);
  ctx.globalAlpha = 0.9;
  ctx.fillStyle = c.bg;
  ctx.fill();
  ctx.globalAlpha = 1;
  ctx.lineWidth = loud ? 1.5 : 1;
  ctx.strokeStyle = loud ? c.blocked : c.line;
  ctx.stroke();
  ctx.font = (loud ? 'bold ' : '') + '12px ui-monospace, Menlo, monospace';
  ctx.textAlign = 'center';
  ctx.textBaseline = 'middle';
  ctx.fillStyle = loud ? c.fg : c.muted;
  ctx.fillText(p.text, p.x, p.y + 0.5);
}

function crown(ctx, x, y, r, col, c) {
  const step = 0.6 * r;
  ctx.lineJoin = 'round';
  for (let i = 0; i < 4; i++) {
    const half = r * (1 - i * 0.2);
    const base = y + 1.4 * r - i * step;
    const shade = mix(col, c.bg, 0.3 * (1 - i / 3));
    ctx.beginPath();
    ctx.moveTo(x - half, base);
    ctx.lineTo(x + half, base);
    ctx.lineTo(x, base - r);
    ctx.closePath();
    ctx.fillStyle = shade;
    ctx.strokeStyle = shade;
    ctx.lineWidth = Math.max(3, r * 0.3);
    ctx.stroke();
    ctx.fill();
  }
}

// drawGarden paints the model on ctx. o is { w, h, t, colors, hideWords,
// mark, picture }: t is seconds for the motion, or null for a still frame.
// Sparks drift on a moving frame, and stand still in a picture.
export function drawGarden(ctx, model, o) {
  const { w, h, colors: c } = o;
  const t = o.t === null || o.t === undefined ? null : o.t;
  const g = model.ground;
  // The sky.
  const sky = ctx.createLinearGradient(0, 0, 0, g);
  sky.addColorStop(0, mix(c.bg, c.accent, 0.16));
  sky.addColorStop(1, mix(c.bg, c.working, 0.1));
  ctx.fillStyle = sky;
  ctx.fillRect(0, 0, w, h);
  // A far hill, then the mossy ground.
  const moss = mix(c.working, c.bg, 0.62);
  ctx.beginPath();
  ctx.ellipse(w * 0.2, g + 10, w * 0.5, h * 0.09, 0, Math.PI, Math.PI * 2);
  ctx.ellipse(w * 0.85, g + 10, w * 0.45, h * 0.07, 0, Math.PI, Math.PI * 2);
  ctx.fillStyle = mix(c.working, c.bg, 0.8);
  ctx.fill();
  ctx.fillStyle = moss;
  ctx.fillRect(0, g, w, h - g);
  ctx.beginPath();
  ctx.moveTo(0, g);
  for (let x = 0; x <= w; x += 40) ctx.quadraticCurveTo(x + 10, g - 3, x + 20, g - 0.5 + ((x / 40) % 2) * 1.5);
  ctx.lineWidth = 4;
  ctx.lineCap = 'round';
  ctx.strokeStyle = mix(c.working, c.bg, 0.3);
  ctx.stroke();

  // The trunk, with bark strokes, knots and the limbs the shoots rise from.
  const tr = model.trunk;
  const bark = mix(mix(c.muted, c.blocked, 0.28), c.bg, 0.5);
  const dark = mix(bark, c.bg, 0.45);
  const limbs = model.nodes.filter((n) => n.depth === 0);
  ctx.lineCap = 'round';
  for (const n of limbs) {
    ctx.beginPath();
    ctx.moveTo(tr.x + (n.base.x - tr.x) * 0.8, g - tr.h * 0.25);
    ctx.quadraticCurveTo(n.base.x + (n.base.x - tr.x) * 0.02, g - tr.h * 0.6, n.base.x, n.base.y + 2);
    ctx.lineWidth = Math.max(10, n.r * 0.85);
    ctx.strokeStyle = bark;
    ctx.stroke();
  }
  trunkPath(ctx, tr);
  const tg = ctx.createLinearGradient(0, g - tr.h, 0, g);
  tg.addColorStop(0, mix(bark, c.fg, 0.14));
  tg.addColorStop(1, mix(bark, c.bg, 0.25));
  ctx.fillStyle = tg;
  ctx.fill();
  ctx.strokeStyle = dark;
  ctx.lineWidth = 2;
  ctx.stroke();
  ctx.lineWidth = 2;
  ctx.strokeStyle = dark;
  for (let i = 0; i < 12; i++) {
    const bx = tr.x - tr.w * 0.42 + (i / 11) * tr.w * 0.84;
    const top = trunkTop(tr, bx);
    ctx.beginPath();
    ctx.moveTo(bx, top + tr.h * 0.18 + (i % 3) * 3);
    ctx.quadraticCurveTo(bx + (i % 2 ? 5 : -5), (top + g) / 2, bx + (i % 3 - 1) * 3, g - 2);
    ctx.stroke();
  }
  for (const k of [0.3, 0.62, 0.8]) {
    ctx.beginPath();
    ctx.ellipse(tr.x - tr.w / 2 + tr.w * k, g - tr.h * 0.42, 7, 4.5, 0.3, 0, Math.PI * 2);
    ctx.fillStyle = dark;
    ctx.fill();
  }

  // Where each crown is, with sway.
  const at = new Map();
  const sorted = [...model.nodes].sort((a, b) => a.depth - b.depth);
  for (const n of sorted) {
    const par = n.parent ? at.get(n.parent) : null;
    const own = t !== null && n.look.sway ? Math.sin(t * 1.3 + n.phase) * n.r * 0.12 : 0;
    const inherit = par ? par.dx * n.f * n.f : 0;
    at.set(n.id, { bx: n.base.x + inherit, fx: n.from.x + inherit, x: n.x + inherit + own, dx: inherit + own });
  }
  // Stems first, then crowns, so a child draws over its parent.
  for (const n of sorted) {
    const p = at.get(n.id);
    const stem = mix(bark, crownColor(n, c), 0.3);
    ctx.strokeStyle = n.look.kind === 'notch' ? mix(stem, c.bad, 0.3) : stem;
    ctx.lineWidth = Math.max(4, n.r * 0.4);
    ctx.lineCap = 'round';
    if (n.depth > 0) {
      ctx.beginPath();
      ctx.moveTo(p.fx, n.from.y);
      ctx.quadraticCurveTo(p.bx, n.from.y, p.bx, n.base.y);
      ctx.stroke();
    }
    ctx.beginPath();
    ctx.moveTo(p.bx, n.base.y);
    ctx.quadraticCurveTo(p.bx, (n.base.y + n.stemTop.y) / 2, p.x, n.stemTop.y);
    ctx.stroke();
    if (n.look.kind === 'notch') {
      const ny = n.base.y - (n.base.y - n.stemTop.y) * 0.35;
      const nx = p.bx + (p.x - p.bx) * 0.12;
      ctx.fillStyle = c.bad;
      ctx.beginPath();
      ctx.moveTo(nx - 8, ny - 5);
      ctx.lineTo(nx + 8, ny - 5);
      ctx.lineTo(nx, ny + 8);
      ctx.closePath();
      ctx.fill();
    }
  }
  const pulse = t === null ? 0.5 : (Math.sin(t * 2.4) + 1) / 2;
  for (const n of sorted) {
    const p = at.get(n.id);
    crown(ctx, p.x, n.y, n.r, crownColor(n, c), c);
    if (n.look.kind === 'grow' && (t !== null || o.picture)) {
      for (let k = 0; k < 3; k++) {
        const a = t === null ? k * 2.1 + n.phase : t * 0.9 + n.phase + k * 2.1;
        const sx = p.x + Math.cos(a) * n.r * 1.2;
        const sy = n.y - n.r * 0.2 + Math.sin(a * 1.2 + k) * n.r * 0.9;
        ctx.beginPath();
        ctx.arc(sx, sy, Math.max(2, n.r * 0.07), 0, Math.PI * 2);
        ctx.globalAlpha = 0.85;
        ctx.fillStyle = mix(c.fg, c.working, 0.25);
        ctx.fill();
        ctx.globalAlpha = 1;
      }
    }
    if (n.look.kind === 'lantern') {
      const lh = Math.max(24, n.r * 1.05);
      const sw = t === null ? 0 : Math.sin(t * 1.6 + n.phase) * 2.5;
      const ax = p.x + 0.6 * n.r;
      const ay = n.y + 1.3 * n.r;
      const lx = ax + sw;
      const ly = ay + 0.5 * n.r + lh / 2;
      const col = c.blocked;
      const gr = lh * (2.6 + pulse * 0.9);
      const grad = ctx.createRadialGradient(lx, ly, 2, lx, ly, gr);
      const rgb = parseColor(col) || [217, 139, 74];
      grad.addColorStop(0, 'rgba(' + rgb.join(',') + ',0.95)');
      grad.addColorStop(0.45, 'rgba(' + rgb.join(',') + ',0.55)');
      grad.addColorStop(1, 'rgba(' + rgb.join(',') + ',0)');
      ctx.globalAlpha = 1;
      ctx.fillStyle = grad;
      ctx.beginPath();
      ctx.arc(lx, ly, gr, 0, Math.PI * 2);
      ctx.fill();
      ctx.globalAlpha = 1;
      ctx.strokeStyle = c.fg;
      ctx.lineWidth = 2;
      ctx.beginPath();
      ctx.moveTo(ax, ay);
      ctx.lineTo(lx, ly - lh / 2);
      ctx.stroke();
      ctx.fillStyle = col;
      ctx.beginPath();
      if (typeof ctx.roundRect === 'function') ctx.roundRect(lx - lh * 0.35, ly - lh / 2, lh * 0.7, lh, lh * 0.2);
      else ctx.rect(lx - lh * 0.35, ly - lh / 2, lh * 0.7, lh);
      ctx.fill();
      ctx.stroke();
      ctx.fillStyle = mix(c.fg, col, 0.15);
      ctx.beginPath();
      ctx.arc(lx, ly, lh * 0.17, 0, Math.PI * 2);
      ctx.fill();
    }
  }
  if (!o.hideWords) {
    if (tr.pill) pill(ctx, tr.pill, c, false);
    for (const n of sorted) {
      pill(ctx, n.pill, c, n.look.loud);
    }
    if (model.nodes.length === 0) {
      ctx.font = '14px ui-monospace, Menlo, monospace';
      ctx.textAlign = 'center';
      ctx.textBaseline = 'middle';
      ctx.fillStyle = c.muted;
      ctx.fillText('No agents yet. The garden grows when one starts.', w / 2, g * 0.4);
    }
  }
  if (o.mark) {
    ctx.font = '12px ui-monospace, Menlo, monospace';
    ctx.textAlign = 'right';
    ctx.textBaseline = 'alphabetic';
    ctx.fillStyle = mix(c.muted, c.bg, 0.2);
    ctx.fillText('coppice', w - 12, h - 12);
  }
}

// pictureName is the file name of a saved picture, from a date.
export function pictureName(date) {
  const d = date instanceof Date ? date : new Date();
  const two = (n) => String(n).padStart(2, '0');
  return 'coppice-garden-' + d.getFullYear() + '-' + two(d.getMonth() + 1) + '-' + two(d.getDate()) + '.png';
}

function make(tag, className, text) {
  const el = document.createElement(tag);
  if (className) el.className = className;
  if (text !== undefined) el.textContent = text;
  return el;
}

// mountGarden fills root with the canvas and its controls. d carries
// panes() and foreman(), open(pane) for a press on a shoot, close() for Esc
// inside the garden's own controls, expand(on) to make the overlay cover
// the whole floor or not, and optional colors(), reduced(), now(),
// raf(fn), caf(id), download(blob, name) and size(). The garden draws only
// while shown. It draws again when the panes, the size or a toggle change,
// and keeps a frame loop only while a shoot moves.
export function mountGarden(root, d) {
  const bar = make('div', 'garden-bar');
  const save = make('button', 'ghost', 'Save picture');
  save.type = 'button';
  save.title = 'Save the garden as a PNG picture';
  const wide = make('button', 'ghost', 'Expand');
  wide.type = 'button';
  wide.title = 'Make the garden cover the whole floor. Esc closes it.';
  const wordsLabel = make('label', 'garden-words');
  const words = make('input');
  words.type = 'checkbox';
  words.checked = false;
  wordsLabel.append(words, make('span', '', ' Hide words'));
  wordsLabel.title = 'Leave out every name, in the view and in the picture';
  bar.append(save, wide, wordsLabel);
  const canvas = make('canvas', 'garden-canvas');
  canvas.setAttribute('aria-label', 'Garden. One shoot for each agent. Press a shoot to open that agent.');
  root.append(bar, canvas);

  const now = d.now || (() => (typeof performance !== 'undefined' ? performance.now() : Date.now()));
  const raf = d.raf || ((fn) => requestAnimationFrame(fn));
  const caf = d.caf || ((id) => cancelAnimationFrame(id));
  const reduced = d.reduced || (() => typeof matchMedia === 'function' && matchMedia('(prefers-reduced-motion: reduce)').matches);
  const colors = d.colors || pageColors;
  let shown = false;
  let expanded = false;
  let model = gardenLayout([], 600, 400, '');
  let sig = '';
  let size = { w: 0, h: 0, dpr: 1 };
  let frame = null;
  let last = 0;
  let start = null;

  const measure = () => {
    if (d.size) return d.size();
    const w = Math.max(240, canvas.clientWidth || root.clientWidth || 600);
    const h = Math.max(200, canvas.clientHeight || 400);
    return { w, h, dpr: Math.max(1, (typeof window !== 'undefined' && window.devicePixelRatio) || 1) };
  };
  const foremanNow = () => (d.foreman ? d.foreman() : '');
  const paint = (t) => {
    const ctx = canvas.getContext('2d');
    if (!ctx) return;
    size = measure();
    const bw = Math.round(size.w * size.dpr);
    const bh = Math.round(size.h * size.dpr);
    if (canvas.width !== bw) canvas.width = bw;
    if (canvas.height !== bh) canvas.height = bh;
    if (typeof ctx.setTransform === 'function') ctx.setTransform(size.dpr, 0, 0, size.dpr, 0, 0);
    drawGarden(ctx, model, { w: size.w, h: size.h, t, colors: colors(), hideWords: words.checked, mark: true, picture: false });
  };
  const key = () => {
    const sz = measure();
    return JSON.stringify([sz.w, sz.h, words.checked, (d.panes() || []).map((p) => p && [p.id, p.label, p.state, p.parent_pane, p.held, p.gate && p.gate.decision]), foremanNow()]);
  };
  const relayout = () => {
    const sz = measure();
    model = gardenLayout(d.panes(), sz.w, sz.h, foremanNow());
    sig = key();
  };
  const stopLoop = () => { if (frame !== null) caf(frame); frame = null; };
  const tick = () => {
    frame = null;
    if (!shown) return;
    const t = now();
    if (t - last >= 45) { last = t; paint(((t - start) / 1000)); }
    if (animated(model.nodes) && !reduced()) frame = raf(tick);
  };
  const redraw = () => {
    if (!shown) return;
    relayout();
    const move = animated(model.nodes) && !reduced();
    if (start === null) start = now();
    paint(move ? (now() - start) / 1000 : null);
    stopLoop();
    if (move) frame = raf(tick);
  };
  const setWide = (on) => {
    expanded = Boolean(on);
    wide.textContent = expanded ? 'Shrink' : 'Expand';
    if (d.expand) d.expand(expanded);
    redraw();
  };

  // The page's Esc rule leaves a focused field alone, so the garden's own
  // controls close it themselves.
  root.addEventListener('keydown', (e) => {
    if (e && e.key === 'Escape' && d.close) { if (typeof e.preventDefault === 'function') e.preventDefault(); d.close(); }
  });
  words.addEventListener('change', redraw);
  wide.addEventListener('click', () => setWide(!expanded));
  canvas.addEventListener('click', (e) => {
    const r = typeof canvas.getBoundingClientRect === 'function' ? canvas.getBoundingClientRect() : { left: 0, top: 0 };
    const x = (e.clientX === undefined ? e.offsetX : e.clientX - r.left);
    const y = (e.clientY === undefined ? e.offsetY : e.clientY - r.top);
    const id = hitPane(model, x, y, e.pointerType === 'touch' ? 34 : 18, !words.checked);
    if (id) d.open(id);
  });
  save.addEventListener('click', () => {
    // The picture is a still of the current size, drawn at twice the
    // pixels. Sparks stand still in it.
    const out = make('canvas');
    const sz = measure();
    const scale = 2;
    out.width = Math.round(sz.w * scale);
    out.height = Math.round(sz.h * scale);
    const ctx = out.getContext('2d');
    if (!ctx) return;
    if (typeof ctx.setTransform === 'function') ctx.setTransform(scale, 0, 0, scale, 0, 0);
    drawGarden(ctx, gardenLayout(d.panes(), sz.w, sz.h, foremanNow()), { w: sz.w, h: sz.h, t: null, colors: colors(), hideWords: words.checked, mark: true, picture: true });
    out.toBlob((blob) => {
      if (!blob) return;
      const name = pictureName(new Date());
      if (d.download) d.download(blob, name);
      else {
        const url = URL.createObjectURL(blob);
        const a = make('a');
        a.href = url;
        a.download = name;
        document.body.append(a);
        a.click();
        a.remove();
        setTimeout(() => URL.revokeObjectURL(url), 1000);
      }
    }, 'image/png');
  });
  // Reduced motion is followed live: a change of the system setting draws again.
  if (!d.reduced && typeof matchMedia === 'function') {
    const mq = matchMedia('(prefers-reduced-motion: reduce)');
    if (mq && typeof mq.addEventListener === 'function') mq.addEventListener('change', () => { if (shown) redraw(); });
  }
  if (typeof ResizeObserver === 'function') new ResizeObserver(() => { if (shown) redraw(); }).observe(root);
  if (typeof document !== 'undefined' && typeof document.addEventListener === 'function') {
    document.addEventListener('visibilitychange', () => {
      if (document.hidden) stopLoop();
      else redraw();
    });
  }

  return {
    show: () => { shown = true; start = null; redraw(); },
    hide: () => {
      shown = false;
      stopLoop();
      if (expanded) { expanded = false; wide.textContent = 'Expand'; if (d.expand) d.expand(false); }
    },
    // changed is the page's call when the panes change. It draws again only
    // when the picture would differ.
    changed: () => {
      if (!shown) return;
      const s = key();
      if (s !== sig) redraw();
    },
    resized: () => { if (shown) redraw(); },
    model: () => model,
    hideWords: (on) => { words.checked = Boolean(on); redraw(); },
    looping: () => frame !== null,
    expanded: () => expanded,
    canvas,
    save,
    wide,
    words,
  };
}

import test from 'node:test';
import assert from 'node:assert/strict';
import { element, reset } from './browser-stub.mjs';
import {
  GARDEN, gardenLayout, lookOf, hitNode, hitPane, gardenKey, drawGarden, mountGarden, shortLabel, pictureName, animated,
} from '../garden.js';
import { mountOverlay, overlayNext, GARDEN as OVERLAY_GARDEN } from '../overlay.js';

const COLORS = {
  bg: '#12181a', fg: '#e8eae7', muted: '#98a3a0', line: '#26312f',
  blocked: '#d98b4a', working: '#8fb996', idle: '#7f8c8a', done: '#5d6a68', unknown: '#b06868',
  accent: '#5fb3f0', bad: '#e0605a',
};

const PANES = [
  { id: 'f', label: 'foreman', state: 'working' },
  { id: 'a', label: 'auth', state: 'working', parent_pane: 'f' },
  { id: 'b', label: 'docs', state: 'idle', parent_pane: 'f' },
  { id: 'a1', label: 'auth tests', state: 'blocked', parent_pane: 'a' },
  { id: 'x', label: 'loose shell', state: 'done' },
];

// A recording 2D context: every call is kept, and any method is allowed.
function recorder(without = []) {
  const calls = [];
  const grad = { addColorStop() {} };
  return new Proxy({}, {
    get(_t, name) {
      if (name === 'calls') return calls;
      if (without.includes(name)) return undefined;
      if (name === 'createRadialGradient' || name === 'createLinearGradient') return (...args) => { calls.push([name, ...args]); return grad; };
      return (...args) => { calls.push([name, ...args]); };
    },
    set(_t, name, val) { calls.push(['set:' + name, val]); return true; },
  });
}
const texts = (ctx) => ctx.calls.filter((c) => c[0] === 'fillText' || c[0] === 'strokeText').map((c) => c[1]);
const opts = (extra) => ({ w: 600, h: 400, t: null, colors: COLORS, hideWords: false, mark: true, ...extra });

test('the trunk is the foreman, and shoots rise from its limbs, nested ones from their parent', () => {
  const m = gardenLayout(PANES, 800, 500, 'f');
  const at = Object.fromEntries(m.nodes.map((n) => [n.id, n]));
  assert.equal(m.trunk.id, 'f');
  assert.equal(m.trunk.pill.text, 'foreman');
  assert.equal(at.f, undefined, 'the foreman is the trunk, not a shoot');
  assert.ok(m.trunk.w >= 800 * 0.58, 'the trunk spans most of the width');
  assert.equal(at.a.parent, '');
  assert.equal(at.a.depth, 0);
  assert.equal(at.x.depth, 0, 'a loose pane rises from a limb too');
  assert.equal(at.a1.parent, 'a');
  assert.equal(at.a1.depth, 2 - 1);
  assert.ok(at.a1.r < at.a.r, 'a nested shoot is smaller');
  assert.ok(at.a.y < m.ground - m.trunk.h, 'a shoot rises above the trunk');
  assert.equal(at.a1.from.x, at.a.x, 'a nested shoot leaves its parent stem');
  assert.ok(at.a1.from.y < at.a.base.y && at.a1.from.y > at.a.stemTop.y, 'at a point along the parent stem');
  const tallest = Math.min(...m.nodes.map((n) => n.y - 1.4 * n.r));
  assert.ok(tallest < 500 * 0.3, 'the tallest crown reaches high into the canvas');
  for (const n of m.nodes) assert.ok(n.x > 0 && n.x < 800 && n.y - 1.4 * n.r >= 0 && n.y < 500, n.id + ' is on the canvas');
});

test('with no foreman the trunk stands bare, and every parentless pane rises from a limb', () => {
  const flat = [{ id: 'p1', label: 'one', state: 'idle' }, { id: 'p2', label: 'two', state: 'idle' }, { id: 'p3', label: 'three', state: 'idle' }];
  const m = gardenLayout(flat, 600, 400, '');
  assert.equal(m.trunk.id, '');
  assert.equal(m.trunk.pill, null);
  assert.equal(m.nodes.length, 3);
  assert.ok(m.nodes.every((n) => n.depth === 0 && n.parent === ''));
  assert.equal(new Set(m.nodes.map((n) => Math.round(n.x))).size, 3);
});

test('no label box covers a stem, a crown, a lantern or another label, in a crowded garden', () => {
  const many = [{ id: 'f', label: 'foreman', state: 'working' }];
  const states = ['working', 'blocked', 'idle', 'done', 'working', 'idle', 'blocked'];
  for (let i = 0; i < 9; i++) many.push({ id: 'k' + i, label: 'worker number ' + i, state: states[i % states.length], parent_pane: i < 6 ? 'f' : 'k' + (i - 6) });
  for (const [w, h] of [[1100, 700], [600, 450], [390, 420]]) {
    const m = gardenLayout(many, w, h, 'f');
    const boxes = [...m.nodes.map((n) => n.pill), m.trunk.pill].map((p) => ({ id: p.text, x0: p.x - p.w / 2, y0: p.y - p.h / 2, x1: p.x + p.w / 2, y1: p.y + p.h / 2 }));
    for (let i = 0; i < boxes.length; i++) {
      for (let j = i + 1; j < boxes.length; j++) {
        const a = boxes[i];
        const b = boxes[j];
        assert.ok(!(a.x0 < b.x1 && b.x0 < a.x1 && a.y0 < b.y1 && b.y0 < a.y1), a.id + ' and ' + b.id + ' overlap at ' + w);
      }
    }
    for (const n of m.nodes) {
      for (const o of m.nodes) {
        const p = n.pill;
        const hitsStem = p.x - p.w / 2 < o.x + 3 && p.x + p.w / 2 > o.x - 3 && p.y - p.h / 2 < o.base.y && p.y + p.h / 2 > o.y + 1.4 * o.r;
        assert.ok(!hitsStem, n.id + ' label crosses the stem of ' + o.id + ' at ' + w);
      }
    }
  }
});

test('a missing parent, a self parent and a cycle still show every pane', () => {
  const odd = [
    { id: 'a', label: 'a', state: 'idle', parent_pane: 'ghost' },
    { id: 'b', label: 'b', state: 'idle', parent_pane: 'b' },
    { id: 'c', label: 'c', state: 'idle', parent_pane: 'd' },
    { id: 'd', label: 'd', state: 'idle', parent_pane: 'c' },
  ];
  const m = gardenLayout(odd, 600, 400, '');
  assert.deepEqual(m.nodes.map((n) => n.id).sort(), ['a', 'b', 'c', 'd']);
  assert.equal(gardenLayout([], 600, 400, '').nodes.length, 0);
  assert.equal(gardenLayout(null, 600, 400, '').nodes.length, 0);
});

test('each state has its own look, and a lantern is the only loud one', () => {
  assert.equal(lookOf({ state: 'working' }).kind, 'grow');
  assert.equal(lookOf({ state: 'working' }).sway, true);
  const lantern = lookOf({ state: 'blocked' });
  assert.deepEqual([lantern.kind, lantern.loud, lantern.pulse], ['lantern', true, true]);
  assert.equal(lookOf({ state: 'blocked', held: { by: 'f' } }).kind, 'grow', 'a held ask reads as working');
  assert.deepEqual([lookOf({ state: 'idle' }).kind, lookOf({ state: 'idle' }).sway], ['still', false]);
  assert.equal(lookOf({ state: 'done' }).kind, 'bud');
  assert.equal(lookOf({ state: 'idle', gate: { decision: 'deny' } }).kind, 'notch');
  assert.equal(lookOf({ state: 'working', gate: { decision: 'deny' } }).kind, 'grow');
  assert.equal(lookOf({ state: 'blocked', gate: { decision: 'deny' } }).kind, 'lantern');
  assert.equal(lookOf({ state: 'idle', gate: { decision: 'allow' } }).kind, 'still');
  assert.equal(lookOf(null).kind, 'still');
  const loud = ['working', 'idle', 'done', 'unknown'].filter((s) => lookOf({ state: s }).loud);
  assert.deepEqual(loud, []);
});

test('the drawing differs by state: a lantern makes a glow, a notch a closed wedge', () => {
  const draw = (panes) => {
    const ctx = recorder();
    drawGarden(ctx, gardenLayout(panes, 600, 400, ''), opts({ t: 1 }));
    return ctx;
  };
  assert.ok(draw([{ id: 'a', label: 'a', state: 'blocked' }]).calls.some((c) => c[0] === 'createRadialGradient'));
  assert.ok(!draw([{ id: 'a', label: 'a', state: 'idle' }]).calls.some((c) => c[0] === 'createRadialGradient'));
  const red = (panes) => draw(panes).calls.some((c) => c[0] === 'set:fillStyle' && c[1] === COLORS.bad);
  assert.ok(!red([{ id: 'a', label: 'a', state: 'idle' }]));
  assert.ok(red([{ id: 'a', label: 'a', state: 'idle', gate: { decision: 'deny' } }]), 'a denied stem carries a red notch');
  const crowns = (panes) => draw(panes).calls.filter((c) => c[0] === 'set:fillStyle' && /^rgb/.test(String(c[1]))).length;
  assert.notEqual(crowns([{ id: 'a', label: 'a', state: 'working' }]), crowns([{ id: 'a', label: 'a', state: 'idle' }]), 'a working shoot draws sparks');
});

test('hide words removes every label, and the corner mark stays', () => {
  const model = gardenLayout(PANES, 800, 500, 'f');
  const shown = recorder();
  drawGarden(shown, model, opts({ w: 800, h: 500 }));
  for (const n of model.nodes) assert.ok(texts(shown).includes(n.label), n.label + ' is drawn');
  assert.ok(texts(shown).includes('coppice'));
  const hidden = recorder();
  drawGarden(hidden, model, opts({ w: 800, h: 500, hideWords: true }));
  assert.deepEqual(texts(hidden), ['coppice']);
  const empty = recorder();
  drawGarden(empty, gardenLayout([], 800, 500, ''), opts({ w: 800, h: 500, hideWords: true }));
  assert.deepEqual(texts(empty), ['coppice'], 'the empty note is words too');
});

test('a click maps to the shoot under it, and to none on bare ground', () => {
  const m = gardenLayout(PANES, 800, 500, 'f');
  const n = m.nodes.find((x) => x.id === 'a1');
  assert.equal(hitNode(m.nodes, n.x + 3, n.y - 4, 18), 'a1');
  assert.equal(hitNode(m.nodes, 2, 2, 18), '');
  assert.equal(hitNode([], 5, 5, 18), '');
  assert.equal(hitPane(m, m.trunk.x, m.trunk.y - 4, 18), 'f', 'a press on the trunk means the foreman');
  assert.equal(hitPane(m, n.x, n.y, 18), 'a1');
  assert.equal(hitPane(gardenLayout(PANES, 800, 500, ''), 400, 440, 18), '', 'a bare trunk is nobody');
});

test('the key is plain g outside a window and a field', () => {
  assert.equal(gardenKey({ key: 'g' }, 'rail'), true);
  assert.equal(gardenKey({ key: 'g' }, 'window'), false);
  assert.equal(gardenKey({ key: 'g', ctrlKey: true }, 'rail'), false);
  assert.equal(gardenKey({ key: 'G', shiftKey: true }, 'rail'), false);
  assert.equal(gardenKey({ key: 'g', target: { tagName: 'INPUT' } }, 'rail'), false);
  assert.equal(gardenKey({ key: 'h' }, 'rail'), false);
  assert.equal(gardenKey(null, 'rail'), false);
});

test('names are cut short and the file name carries the date', () => {
  assert.equal(shortLabel('a very long agent name indeed', 'x'), 'a very long a…');
  assert.equal(shortLabel('', 'w1:p2'), 'w1:p2');
  assert.equal(pictureName(new Date(2026, 9, 5)), 'coppice-garden-2026-10-05.png');
});

// mounted builds a garden on a fake canvas with a recording context.
function mounted(panes, extra = {}) {
  reset();
  const root = element('garden');
  const frames = [];
  const opened = [];
  let cur = panes;
  const g = mountGarden(root, {
    panes: () => cur,
    foreman: () => 'f',
    open: (p) => opened.push(p),
    colors: () => COLORS,
    size: () => ({ w: 600, h: 400, dpr: 1 }),
    raf: (fn) => { frames.push(fn); return frames.length; },
    caf: () => {},
    reduced: () => false,
    now: () => 0,
    ...extra,
  });
  const ctx = recorder();
  g.canvas.getContext = () => ctx;
  return { g, ctx, frames, opened, set: (p) => { cur = p; } };
}

test('the garden draws only while shown, and again only when the picture would change', () => {
  const m = mounted(PANES);
  m.g.changed();
  assert.equal(m.ctx.calls.length, 0, 'hidden, it draws nothing');
  m.g.show();
  const first = m.ctx.calls.length;
  assert.ok(first > 0);
  m.g.changed();
  assert.equal(m.ctx.calls.length, first, 'the same panes draw nothing again');
  m.set(PANES.map((p) => (p.id === 'b' ? { ...p, state: 'blocked' } : p)));
  m.g.changed();
  assert.ok(m.ctx.calls.length > first);
  const before = m.ctx.calls.length;
  m.g.hide();
  m.set(PANES);
  m.g.changed();
  assert.equal(m.ctx.calls.length, before);
});

test('the frame loop runs while a shoot works, and not when all is still or motion is reduced', () => {
  const working = mounted(PANES);
  working.g.show();
  assert.equal(working.g.looping(), true);
  working.g.hide();
  assert.equal(working.g.looping(), false, 'hidden, the loop stops');
  const calm = mounted([{ id: 'a', label: 'a', state: 'idle' }, { id: 'b', label: 'b', state: 'done' }]);
  calm.g.show();
  assert.equal(calm.g.looping(), false);
  assert.equal(animated(calm.g.model().nodes), false);
  const reduced = mounted(PANES, { reduced: () => true });
  reduced.g.show();
  assert.equal(reduced.g.looping(), false, 'reduced motion draws one still frame');
});

test('a still frame draws every shoot where it stands, and a moving frame sways', () => {
  const model = gardenLayout([{ id: 'a', label: 'a', state: 'working' }], 600, 400, '');
  const heads = (t) => {
    const c = recorder();
    drawGarden(c, model, opts({ t, hideWords: true, mark: false }));
    return c.calls.filter((x) => x[0] === 'quadraticCurveTo').map((x) => x[3]);
  };
  assert.notDeepEqual(heads(null), heads(1.3));
  assert.deepEqual(heads(null), heads(null));
  assert.ok(heads(null).includes(model.nodes[0].x), 'a still shoot stands at its place');
});

test('a click on a shoot opens that pane, and the toggle hides the words', () => {
  const m = mounted(PANES);
  m.g.show();
  const n = m.g.model().nodes.find((x) => x.id === 'b');
  m.g.canvas.dispatchEvent({ type: 'click', clientX: n.x, clientY: n.y });
  assert.deepEqual(m.opened, ['b']);
  m.g.canvas.dispatchEvent({ type: 'click', clientX: 1, clientY: 1 });
  assert.deepEqual(m.opened, ['b'], 'bare ground opens nothing');
  m.ctx.calls.length = 0;
  m.g.words.checked = true;
  m.g.words.dispatchEvent({ type: 'change' });
  assert.deepEqual(texts(m.ctx), ['coppice']);
});

test('save picture makes a PNG named for the date, with the words left out when asked', () => {
  const saved = [];
  const m = mounted(PANES, { download: (blob, name) => saved.push([blob.type, name]) });
  m.g.show();
  m.g.words.checked = true;
  const outs = [];
  const real = document.createElement;
  document.createElement = (tag) => {
    const el = real(tag);
    if (tag === 'canvas') {
      const ctx = recorder();
      el.getContext = () => ctx;
      el.toBlob = (cb, type) => { outs.push({ el, ctx, type }); cb({ size: 3, type }); };
    }
    return el;
  };
  try {
    m.g.save.dispatchEvent({ type: 'click' });
  } finally {
    document.createElement = real;
  }
  assert.equal(outs.length, 1);
  assert.equal(outs[0].type, 'image/png');
  assert.equal(outs[0].el.width, 1200, 'the picture is drawn at twice the size');
  assert.deepEqual(texts(outs[0].ctx), ['coppice'], 'the picture holds no names');
  assert.equal(saved.length, 1);
  assert.equal(saved[0][0], 'image/png');
  assert.match(saved[0][1], /^coppice-garden-\d{4}-\d{2}-\d{2}\.png$/);
});

test('Esc inside the garden closes it, also from its own controls', () => {
  let closed = 0;
  reset();
  const root = element('garden');
  mountGarden(root, { panes: () => PANES, open() {}, close: () => { closed += 1; }, colors: () => COLORS, size: () => ({ w: 600, h: 400, dpr: 1 }) });
  root.dispatchEvent({ type: 'keydown', key: 'Escape' });
  root.dispatchEvent({ type: 'keydown', key: 'a' });
  assert.equal(closed, 1);
});

test('the overlay shows the garden for its id, hides the frame, and hides it again on close', () => {
  reset();
  const el = (id) => element(id);
  const calls = [];
  const garden = { el: element('garden'), show: () => calls.push('show'), hide: () => calls.push('hide') };
  const d = {
    el: el('overlay'), title: el('overlay-title'), pin: el('overlay-pin'), close: el('overlay-close'),
    frame: el('view-frame'), journal: el('journal'), strip: el('strip'), unpin: el('strip-unpin'),
    host: { hide() {}, show() {}, current: () => null },
    stripHost: { hide() {}, show() {}, current: () => null },
    garden, labelOf: (p) => p, titleOf: (i) => i, onFloor: () => true, fixes: () => [], toMessage: (e) => e,
    storage: { getItem: () => null, setItem() {} }, every: () => 1, cancel() {}, place() {},
    rpc: async () => ({}),
  };
  const ov = mountOverlay(d);
  ov.open(GARDEN, []);
  assert.equal(d.title.textContent, 'Garden');
  assert.equal(garden.el.hidden, false);
  assert.equal(d.frame.hidden, true);
  assert.equal(d.journal.hidden, true);
  assert.equal(calls.at(-1), 'show');
  ov.close();
  assert.equal(garden.el.hidden, true);
  assert.equal(calls.at(-1), 'hide');
  assert.equal(overlayNext(undefined, { type: 'open', id: GARDEN }).open, GARDEN);
});

test('the key ignores a held key and an editable target', () => {
  assert.equal(gardenKey({ key: 'g', repeat: true }, 'rail'), false);
  assert.equal(gardenKey({ key: 'g', target: { tagName: 'DIV', isContentEditable: true } }, 'rail'), false);
  assert.equal(gardenKey({ key: 'g', target: { tagName: 'DIV' } }, 'rail'), true);
});

test('a label and a lantern press the same shoot as its crown, unless the words are hidden', () => {
  const m = gardenLayout(PANES, 800, 500, 'f');
  const n = m.nodes.find((x) => x.id === 'a');
  assert.equal(hitPane(m, n.pill.x, n.pill.y, 18), 'a');
  assert.notEqual(hitPane(m, n.pill.x, n.pill.y, 18, false), 'a', 'a hidden label is not a target');
  const lit = m.nodes.find((x) => x.id === 'a1');
  const lh = Math.max(24, lit.r * 1.05);
  assert.equal(hitPane(m, lit.x + 0.6 * lit.r, lit.y + 1.3 * lit.r + 0.5 * lit.r + lh / 2, 3, false), 'a1');
});

test('a browser with no roundRect still draws the lantern and the labels', () => {
  const ctx = recorder(['roundRect']);
  drawGarden(ctx, gardenLayout([{ id: 'a', label: 'a', state: 'blocked' }], 600, 400, ''), opts({ t: 1 }));
  assert.ok(texts(ctx).includes('a'));
  assert.ok(ctx.calls.filter((c) => c[0] === 'rect').length >= 2, 'the lantern and the pill fall back to rect');
});

test('the garden has one overlay id', () => {
  assert.equal(GARDEN, OVERLAY_GARDEN);
});

test('the garden follows the reduced-motion setting while it is shown', () => {
  let reducedNow = false;
  const listeners = [];
  globalThis.matchMedia = () => ({ get matches() { return reducedNow; }, addEventListener: (_t, fn) => listeners.push(fn) });
  try {
    const m = mounted(PANES, { reduced: undefined });
    m.g.show();
    assert.equal(m.g.looping(), true);
    reducedNow = true;
    listeners.forEach((fn) => fn());
    assert.equal(m.g.looping(), false, 'a setting turned on stops the motion');
    reducedNow = false;
    listeners.forEach((fn) => fn());
    assert.equal(m.g.looping(), true);
  } finally {
    delete globalThis.matchMedia;
  }
});

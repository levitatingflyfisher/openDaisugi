import { LAYOUTS, LOCKED, newTiles, fill, open, closeSlot, reset, focused, slotOf, shown } from './tiles.js';
import { stateOf, askFor } from './pane.js';
import { stateChip } from './chips.js';
import { applyFrame, gridToText, drawGrid } from './grid.js';
import { sortPanes, paneRows, setFloor } from './roster.js';

// The floor page: the roster rail plus as many live tiles as the width
// allows. A row click fills the focused tile. A middle click or a ctrl
// click opens a new tile. Every shown tile holds one attach, and a tile
// that loses its pane detaches it. The pane screen keeps its own single
// attach, so app.js calls detachAll when the route leaves the floor and
// paint when it returns.

export const STORE_KEY = 'coppice.floor';
export const DEFAULT_PREFS = Object.freeze({ layout: 'focus', locked: false });

// CELL_PX is the cell height the pane screen draws at, and what a tile
// draws at until it has a measured width.
const CELL_PX = 14;
// GLYPH_WIDTH is drawGrid's glyph width as a share of the cell size.
const GLYPH_WIDTH = 0.6;
const EMPTY_TEXT = 'Empty. Click a row.';

// tileCount is how many tiles a page width holds. A phone gets none: the
// rail is the whole page there, and a row opens the pane screen.
export function tileCount(widthPx) {
  const width = Number(widthPx) || 0;
  if (width < 600) return 0;
  if (width < 900) return 1;
  if (widthPx < 1400) return 2;
  return 3;
}

// cellPxFor is the cell size that fits a grid of cols columns into a
// canvas canvasWidth wide, never below 4. drawGrid paints each glyph six
// tenths of the cell size wide, so the width holds cols glyphs when the
// cell size is the width over cols times that factor. With no measured
// width it is CELL_PX.
export function cellPxFor(canvasWidth, cols) {
  const width = Number(canvasWidth) || 0;
  if (width <= 0 || !cols || cols < 1) return CELL_PX;
  return Math.max(4, Math.floor(width / (cols * GLYPH_WIDTH)));
}

// prefsFrom reads the stored layout and lock. Anything unreadable, or any
// field with a value that is not one of the allowed ones, reads as the
// default.
export function prefsFrom(raw) {
  let parsed = null;
  try { parsed = JSON.parse(raw); } catch { parsed = null; }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return { ...DEFAULT_PREFS };
  return {
    layout: LAYOUTS.includes(parsed.layout) ? parsed.layout : DEFAULT_PREFS.layout,
    locked: typeof parsed.locked === 'boolean' ? parsed.locked : DEFAULT_PREFS.locked,
  };
}

function readPrefs() {
  let raw = null;
  try { raw = localStorage.getItem(STORE_KEY); } catch { raw = null; }
  return prefsFrom(raw);
}

function savePrefs(layout, locked) {
  try { localStorage.setItem(STORE_KEY, JSON.stringify({ layout, locked })); } catch { /* storage is a convenience */ }
}

// lastSegment is the last path segment of a working directory.
export function lastSegment(cwd) {
  const parts = String(cwd || '').split('/').filter(Boolean);
  return parts.length ? parts[parts.length - 1] : '';
}

// verdictLine is what the ask bar says under the summary when the ask
// carries the gate's own verdict. It is '' when there is none.
export function verdictLine(ask) {
  const gate = ask && ask.gate;
  if (!gate || typeof gate !== 'object') return '';
  if (gate.verdict === 'deny') return gate.rule === undefined || gate.rule === null ? 'The gate says no' : 'The gate says no, rule ' + gate.rule;
  if (gate.verdict === 'allow') return 'The gate says yes';
  return '';
}

function make(tag, className, text) {
  const el = document.createElement(tag);
  if (className) el.className = className;
  if (text !== undefined) el.textContent = text;
  return el;
}

// mountFloor reads rpc, api, and status off floorState at each call, not
// once at mount, so the page can swap them after a reconnect.
export function mountFloor(root, floorState) {
  const rpc = (cmd, fields) => floorState.rpc(cmd, fields);
  const api = (path, init) => floorState.api(path, init);
  const status = (msg) => floorState.status(msg);
  const width = Number(floorState.width) || 0;
  const n = tileCount(width);
  const prefs = readPrefs();
  const tiles = newTiles(prefs.layout, n);
  tiles.locked = prefs.locked;

  let rows = new Map();     // pane id to its last flat row
  let order = [];           // pane ids in roster order
  const grids = new Map();  // pane id to its grid
  const attached = new Set();
  const gen = new Map();    // pane id to the number of its current attach
  let views = [];           // one per shown slot

  const controls = make('div');
  controls.id = 'floor-controls';
  const layoutLabel = make('label', '', 'Layout');
  const select = make('select');
  select.id = 'floor-layout';
  for (const name of LAYOUTS) {
    const opt = make('option', '', name);
    opt.value = name;
    select.append(opt);
  }
  select.value = tiles.layout;
  const lock = make('button');
  lock.id = 'floor-lock';
  lock.textContent = tiles.locked ? 'Unlock' : 'Lock';
  controls.append(layoutLabel, select, lock);

  const tilesEl = make('div');
  tilesEl.id = 'tiles';
  tilesEl.dataset.tiles = String(n);

  // With no tile there is no grid and no control to show. The floor still
  // tracks the pane rows so a later paint costs nothing.
  const floorEl = make('div');
  floorEl.id = 'floor';
  floorEl.append(controls, tilesEl);
  if (n > 0) root.append(floorEl);

  const visible = () => !root.hidden;
  const viewOf = (pane) => views.find((v) => v.pane === pane) || null;

  // repaint draws a tile's grid at whatever size the frames carry, scaled
  // so its width fits the tile, and refreshes the text mirror.
  const repaint = (view) => {
    const grid = grids.get(view.pane);
    if (!grid) return;
    drawGrid(view.canvas.getContext('2d'), grid, cellPxFor(view.body.clientWidth, grid.cols));
    view.mirror.textContent = gridToText(grid);
  };

  const setNote = (pane, text) => {
    const view = viewOf(pane);
    if (view) view.note.textContent = text;
  };

  // attach asks for frames for one pane. It carries no size: the same
  // pane is open in the operator's terminal, and a size here would resize
  // it. A refusal drops the pane from the attached set, so the next paint
  // tries again, and the tile itself says why. The status line stays free
  // for the roster's own messages.
  // Each attach carries a number, and a reply acts only when its number
  // is still the pane's current one. A late refusal of an attach that a
  // detach and a new attach already replaced must not drop the live one.
  const bump = (pane) => {
    const g = (gen.get(pane) || 0) + 1;
    gen.set(pane, g);
    return g;
  };

  const attach = (pane) => {
    attached.add(pane);
    const g = bump(pane);
    rpc('pane.attach', { pane })
      .then(() => { if (gen.get(pane) === g) setNote(pane, ''); })
      .catch((e) => {
        if (gen.get(pane) !== g) return;
        attached.delete(pane);
        setNote(pane, e.message);
      });
  };

  const detach = (pane) => {
    attached.delete(pane);
    grids.delete(pane);
    bump(pane);
    rpc('pane.detach', { pane }).catch(() => {});
  };

  // syncAttach makes the attached set equal to the panes the shown tiles
  // hold, and empty while the floor is not the shown screen.
  const syncAttach = () => {
    const want = new Set(visible() ? views.map((v) => v.pane).filter(Boolean) : []);
    for (const pane of [...attached]) if (!want.has(pane)) detach(pane);
    for (const pane of want) if (!attached.has(pane)) attach(pane);
  };

  const denyAsk = async (pane) => {
    const ask = askFor(stateOf(rows.get(pane)));
    if (!ask.visible || !ask.askId) return;
    try {
      await api('/api/ask/answer', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ tool_use_id: ask.askId, decision: 'deny', reason: 'denied from the floor' }),
      });
      status('Denied.');
      const view = viewOf(pane);
      if (view && view.askbar) view.askbar.hidden = true;
    } catch (e) {
      status(e.message);
    }
  };

  const reply = (pane) => { location.hash = '#/pane/' + encodeURIComponent(pane); };

  // renderHead rebuilds a tile's header and ask bar from its row.
  const renderHead = (view) => {
    const row = rows.get(view.pane);
    const st = stateOf(row);
    const chip = stateChip(st.state);
    // The header is the way to the pane screen at every width, where the
    // prompt box, the steer box, and the key drawer live.
    const head = make('div', 'head');
    head.addEventListener('click', () => reply(view.pane));
    head.append(
      make('span', 'dot dot-' + chip.word),
      make('strong', '', (row && row.label) || view.pane),
      make('span', 'muted', st.harness),
      make('span', 'muted', lastSegment(row && row.cwd)),
    );
    const ask = askFor(st);
    let askbar = null;
    if (ask.visible) {
      askbar = make('div', 'askbar');
      askbar.append(make('p', 'summary', ask.summary));
      const verdict = verdictLine(st.ask);
      if (verdict) askbar.append(make('p', 'verdict', verdict));
      const deny = make('button', '', 'Deny');
      deny.addEventListener('click', () => denyAsk(view.pane));
      const replyBtn = make('button', '', 'Reply');
      replyBtn.addEventListener('click', () => reply(view.pane));
      askbar.append(deny, replyBtn);
    }
    view.askbar = askbar;
    view.section.replaceChildren(...[head, askbar, view.body, view.note].filter(Boolean));
  };

  const buildTile = (pane, i) => {
    const section = make('section', 'tile' + (i === tiles.focus ? ' focused' : ''));
    section.dataset.tile = pane;
    if (!pane) {
      section.className += ' empty';
      section.textContent = EMPTY_TEXT;
      return { pane, section };
    }
    const canvas = make('canvas');
    const mirror = make('pre', 'mirror visually-hidden');
    const body = make('div', 'tile-body');
    body.append(canvas, mirror);
    const note = make('p', 'note');
    const view = { pane, section, canvas, mirror, body, note, askbar: null };
    renderHead(view);
    repaint(view);
    return view;
  };

  // renderAll rebuilds every shown tile from the tile set.
  const renderAll = () => {
    views = shown(tiles, n).map(buildTile);
    tilesEl.replaceChildren(...views.map((v) => v.section));
    syncAttach();
  };

  // paint takes a fresh pane list. A pane gone from the list leaves its
  // slot. A pane with no slot takes an empty one, or under all a new one.
  // The focused pane keeps focus, so a refresh never moves it.
  const paint = (panes) => {
    const list = Array.isArray(panes) ? panes : [];
    rows = new Map(list.map((p) => [p.id, p]));
    order = sortPanes(paneRows(list, Date.now() / 1000)).map((r) => r.id);
    for (let i = tiles.slots.length - 1; i >= 0; i--) {
      if (tiles.slots[i] && !rows.has(tiles.slots[i])) closeSlot(tiles, i);
    }
    const keep = focused(tiles);
    for (const id of order) {
      if (slotOf(tiles, id) >= 0) continue;
      if (tiles.layout === 'all' || tiles.slots.includes('')) fill(tiles, id);
    }
    const back = slotOf(tiles, keep);
    tiles.focus = back >= 0 ? back : 0;
    renderAll();
  };

  const onEvent = (msg) => {
    if (!msg || !msg.pane) return;
    if (msg.event === 'frame') {
      const view = viewOf(msg.pane);
      if (!view || !attached.has(msg.pane)) return;
      grids.set(msg.pane, applyFrame(grids.get(msg.pane) || null, msg));
      repaint(view);
      return;
    }
    if (msg.event === 'state' && rows.has(msg.pane)) {
      const old = rows.get(msg.pane);
      rows.set(msg.pane, {
        ...old, state: msg.state, source: msg.source, harness: msg.harness || old.harness, ts: msg.ts, ask: msg.ask || null,
      });
      const view = viewOf(msg.pane);
      if (view) renderHead(view);
    }
  };

  const detachAll = () => {
    for (const pane of [...attached]) detach(pane);
  };

  // release hands one attach to the pane screen. The pane leaves the
  // attached set with no wire message, so the server keeps the pump
  // running for the pane screen's own attach, and the grid goes with it so
  // the pane screen starts from the picture the tile had. It returns null
  // for a pane the floor does not hold.
  const release = (pane) => {
    if (!attached.has(pane)) return null;
    attached.delete(pane);
    bump(pane);
    const grid = grids.get(pane) || null;
    grids.delete(pane);
    return grid;
  };

  // adopt takes an attach back from the pane screen with no wire message
  // and draws the tile from the grid it brings. The next paint detaches
  // the pane when no shown slot holds it.
  const adopt = (pane, grid) => {
    if (!pane) return;
    attached.add(pane);
    bump(pane);
    if (grid) grids.set(pane, grid);
    else grids.delete(pane);
    const view = viewOf(pane);
    if (view) repaint(view);
  };

  // dropped forgets every attach and every grid and sends nothing. The
  // socket that held them is gone, and the next paint attaches again on
  // the new one. Without this the paint after a reconnect would see every
  // shown pane already attached and leave the tiles frozen.
  const dropped = () => {
    for (const pane of attached) bump(pane);
    attached.clear();
    grids.clear();
  };

  const fillRow = (pane) => { fill(tiles, pane); renderAll(); };
  const openRow = (pane) => { open(tiles, pane); renderAll(); };
  const canShow = () => n > 0;

  // A layout change rebuilds the slots from the shown panes in roster
  // order. The lock refuses it, the same as it refuses reset.
  select.addEventListener('change', () => {
    const want = select.value;
    if (!LAYOUTS.includes(want) || want === tiles.layout) return;
    if (tiles.locked) {
      select.value = tiles.layout;
      status(LOCKED);
      return;
    }
    const showing = shown(tiles, n).filter(Boolean);
    const keep = order.filter((id) => showing.includes(id));
    const fresh = newTiles(want, n);
    tiles.layout = fresh.layout;
    tiles.slots = fresh.slots;
    tiles.focus = 0;
    reset(tiles, keep);
    savePrefs(tiles.layout, tiles.locked);
    renderAll();
  });

  lock.addEventListener('click', () => {
    tiles.locked = !tiles.locked;
    lock.textContent = tiles.locked ? 'Unlock' : 'Lock';
    savePrefs(tiles.layout, tiles.locked);
  });

  // n denies the focused tile's ask and r replies to it, only while the
  // floor is the shown screen, a tile can show, and the key was not typed
  // into a field.
  document.addEventListener('keydown', (e) => {
    if (!visible() || !canShow() || e.ctrlKey || e.metaKey || e.altKey) return;
    const tag = e.target && e.target.tagName;
    if (tag === 'INPUT' || tag === 'TEXTAREA') return;
    const pane = focused(tiles);
    if (!pane) return;
    if (e.key === 'n') denyAsk(pane);
    else if (e.key === 'r') reply(pane);
  });

  const floor = {
    paint, onEvent, tiles, detachAll, dropped, release, adopt, fillRow, openRow, canShow,
    gridOf: (pane) => grids.get(pane) || null,
  };
  setFloor(floor);
  paint(floorState.panes);
  return floor;
}

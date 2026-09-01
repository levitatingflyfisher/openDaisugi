import { LAYOUTS, LOCKED, newTiles, reset, swap, focused, slotOf, shown } from './tiles.js';
import { stateOf, askFor, paneName, tierLine, answerBody, denyButton, allowControl, verdictLine, needsYou, heldLine, heldTierLine, heldAnswer, isHeadless, HEADLESS_NOTE, trustFor, trustCommand, trustDone, TRUST_TEXT, TRUST_YES, TRUST_NO } from './pane.js';
import { stateChip } from './chips.js';
import { applyFrame, gridToText, drawGrid, measureAdvance } from './grid.js';
import { paneRows, setFloor, render as renderRail, killButton, confirmLine, editInPlace, focusRename } from './roster.js';
import {
  cellFor, windowCapacity, windowShape, fillOrder, startSelect, endedLine, endedSlot, isEndedSlot, paneOfSlot, livePanes,
  phoneSize, scrollFollow,
} from './windows.js';
import { keyBytes, isPaste, isLeave, routeKey } from './keys.js';
import { message, fixButtons, toMessage } from './messages.js';
import { segments, gateMark, light, details, stackBarEl, gateMarkEls, detailsBox } from './stackbar.js';
import { railEntries, railOrder, treeOrder, killKey, dropAction, rowFromEvent, ROW_TYPE } from './rail.js';

export { verdictLine, keyBytes, isPaste, isLeave, rowFromEvent };

// endedMessage is the line an agent that ended on its own leaves, with
// Resume and Forget. An agent that ended well, or with no known code,
// leaves a line that clears after ENDED_CLEAR_MS, as a window's line does.
export function endedMessage(pane, label, exit) {
  const name = label || pane;
  const m = message(endedLine(label, exit), [
    { kind: 'resume', label: 'Resume', pane, name },
    { kind: 'forget', label: 'Forget', pane, name },
  ]);
  if (!Number.isInteger(exit) || exit === 0) m.clearAfter = ENDED_CLEAR_MS;
  return m;
}

// The floor page: the rail plus as many live windows as fit at a readable
// size. Windows fill with the agents that need you first, then the working
// ones, then the rest. The keys belong to the selected window, whose
// border and header say "typing here", and every key goes to its agent
// until ctrl-space gives the keys to the rail. On the rail the arrows move
// the selected row, Enter puts it in a window, and a number puts it in that
// window. An agent that ends on its own leaves its window with one line
// that says so. Every shown window holds one attach, and a window that
// loses its agent detaches it. The pane screen keeps its own single attach,
// so app.js calls detachAll when the route leaves the floor and paint when
// it returns.

export const STORE_KEY = 'coppice.floor';
export const DEFAULT_PREFS = Object.freeze({ layout: 'focus', locked: false });

// NO_AGENTS is what the one window says when no agent runs.
export const NO_AGENTS = 'No agents running. New starts one.';
// FREE_TEXT is what a free window says while the layout is locked.
export const FREE_TEXT = 'Free. Click a row to put an agent here.';
// NOTES_SHOWN is how many notes a window draws under its header.
export const NOTES_SHOWN = 3;
// TYPING_TEXT marks the header of the window that has the keys.
export const TYPING_TEXT = 'typing here · ctrl-space leaves';
// CLEAR_HINT sits under an ended agent's line.
export const CLEAR_HINT = 'Click to clear.';
// ENDED_CLEAR_MS is how long the line of an agent that ended well stays.
// A line for a non-zero exit stays until it is clicked.
export const ENDED_CLEAR_MS = 10000;

// FIELD_TAGS keep their own keys even while a window types.
const FIELD_TAGS = ['INPUT', 'TEXTAREA', 'SELECT'];

// RAIL_PX, TOP_PX and PAD_PX estimate the windows area from the page size
// when the page has not laid it out yet: the rail, the bar and status line
// above, and the margins.
const RAIL_PX = 240;
const TOP_PX = 70;
const PAD_PX = 16;

// areaFor is the windows area a page of width by height CSS pixels leaves,
// by estimate. Under 900 px the rail sits above the windows.
export function areaFor(width, height) {
  const w = Number(width) || 0;
  const h = Number(height) || 900;
  if (w < 900) return { w: w - PAD_PX, h: h - TOP_PX - PAD_PX };
  return { w: w - RAIL_PX - PAD_PX, h: h - TOP_PX - PAD_PX };
}

// tileCount is how many windows a page holds. A phone gets none: the
// overview is the whole page there, and a row slides its agent in over it.
// Under 900 px the page holds one. From 900 px it holds as many as fit at
// a readable size.
export function tileCount(widthPx, heightPx, advance) {
  const width = Number(widthPx) || 0;
  if (phoneSize(width, heightPx)) return 0;
  if (width < 900) return 1;
  const area = areaFor(width, heightPx);
  const cap = windowCapacity(area.w, area.h, advance);
  return cap.cols * cap.rows;
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

// FOLD_KEY is where this browser keeps the rail groups it folded.
export const FOLD_KEY = 'coppice.rail.folded';

function readFolds() {
  try {
    const list = JSON.parse(localStorage.getItem(FOLD_KEY) || '[]');
    return new Set(Array.isArray(list) ? list.filter((k) => typeof k === 'string') : []);
  } catch {
    return new Set();
  }
}

function saveFolds(set) {
  try { localStorage.setItem(FOLD_KEY, JSON.stringify([...set])); } catch { /* storage is a convenience */ }
}

// hasRowDrag is true while a rail row is dragged over a window.
const hasRowDrag = (e) => {
  const types = e && e.dataTransfer && e.dataTransfer.types;
  return Boolean(types && Array.from(types).includes(ROW_TYPE));
};

// TITLE is the tab title with no pane blocked.
export const TITLE = 'coppice';

// titleFor is the tab title for a pane list: the count of panes that need
// the operator and the name, as 2 · coppice, or the name alone when none
// does. An ask a foreman holds does not count.
export function titleFor(panes) {
  const n = (Array.isArray(panes) ? panes : []).filter(needsYou).length;
  return n > 0 ? n + ' · ' + TITLE : TITLE;
}


// lookingLine is "· alice looking" for the people who have a pane open,
// with me, the name of the person reading this page, left out. It is ""
// when nobody else looks.
export function lookingLine(names, me) {
  const others = (Array.isArray(names) ? names : []).filter((n) => typeof n === 'string' && n !== '' && n !== me);
  return others.length > 0 ? '· ' + others.join(', ') + ' looking' : '';
}

// lastSegment is the last path segment of a working directory.
export function lastSegment(cwd) {
  const parts = String(cwd || '').split('/').filter(Boolean);
  return parts.length ? parts[parts.length - 1] : '';
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
  let pageW = Number(floorState.width) || 0;
  let pageH = Number(floorState.height) || 0;
  const prefs = readPrefs();

  let rows = new Map();     // pane id to its last flat row
  let order = [];           // pane ids in rail order
  const grids = new Map();  // pane id to its grid
  const attached = new Set();
  const failed = new Map(); // pane id to why its attach was refused
  const gen = new Map();    // pane id to the number of its current attach
  const cache = new Map();  // pane id to its window, kept while it shows
  let views = [];           // one per shown window
  let keysIn = 'rail';      // 'window' when the selected window has the keys
  let railSel = '';         // pane id of the selected rail row
  let started = false;      // the keys found their start place
  const ended = new Map();  // pane id to {label, exit, known} once looked up
  const looked = new Set(); // pane ids known gone; they never fill a window
  const asked = new Set();  // pane ids already looked up in the ended list
  const stopping = new Set(); // pane ids the operator is stopping; their end is no ending
  const labels = new Map(); // pane id to its label, kept past its last row
  const notes = new Map();  // pane id to its last notes, oldest first
  let outbox = Promise.resolve();
  let dragFrom = null;      // slot index of the window whose header is dragged
  let tasks = Array.isArray(floorState.tasks) ? floorState.tasks : [];
  const folds = readFolds(); // rail group keys this browser folded
  let killAsk = '';         // pane id whose stop waits for its confirm
  let editing = null;       // {pane, where: 'row'|'head'} while a label is renamed
  let want = '';            // pane id a New made, to put in a window once listed
  let endedShown = false;   // the status line holds an ended line
  let advance = 0;          // measured glyph advance, once a canvas exists
  const openSeg = new Map(); // pane id to the stack segment whose details show

  let n = tileCount(pageW, pageH);
  const tiles = newTiles(prefs.layout, n);
  tiles.locked = prefs.locked;

  // typing is the pane that has the keys, or null while the rail has them.
  const typing = () => {
    if (keysIn !== 'window') return null;
    const slot = focused(tiles);
    return slot && !isEndedSlot(slot) ? slot : null;
  };

  // The rail is drawn by the roster. The floor asks for a redraw when the
  // selection, a fold, a kill confirm or a rename changes it.
  const redrawRail = () => {
    if (typeof floorState.rail === 'function') floorState.rail();
    else renderRail(paneRows(lastList, Date.now() / 1000));
  };
  // refreshLists asks the roster for fresh live and ended lists.
  const refreshLists = () => {
    if (typeof window !== 'undefined' && typeof window.dispatchEvent === 'function') window.dispatchEvent(new CustomEvent('coppice:refresh'));
  };
  // takeTasks keeps a fresh task list, which the roster reads beside each
  // pane list, and draws the rail again when the tree changed. A reply
  // with no list keeps the old one.
  const takeTasks = (list) => {
    if (!Array.isArray(list)) return;
    const shape = (l) => JSON.stringify(l.map((t) => [t && t.id, t && t.label, t && t.parent]));
    const changed = shape(list) !== shape(tasks);
    tasks = list;
    if (changed) redrawRail();
  };
  // toggleGroup folds or opens one rail group. A selected row the fold
  // hides gives the selection to the next row the rail still shows.
  const toggleGroup = (key) => {
    if (folds.has(key)) folds.delete(key);
    else folds.add(key);
    saveFolds(folds);
    if (railSel && !railIds().includes(railSel)) railSel = railIds()[0] || railSel;
    redrawRail();
  };

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

  // With no window there is no grid and no control to show. The floor
  // still tracks the pane rows so a later paint costs nothing. The layout
  // and lock controls sit in the rail footer when the page has one.
  const floorEl = make('div');
  floorEl.id = 'floor';
  const foot = typeof root.querySelector === 'function' ? root.querySelector('#rail-foot') : null;
  if (foot) foot.append(controls);
  floorEl.append(...(foot ? [tilesEl] : [controls, tilesEl]));
  let placed = false;
  const place = () => {
    if (n > 0 && !placed) {
      root.append(floorEl);
      placed = true;
    }
    floorEl.hidden = n === 0;
    if (foot) foot.hidden = n === 0;
  };
  place();

  const visible = () => !root.hidden;
  const viewOf = (pane) => views.find((v) => v.pane === pane) || null;
  const dpr = () => (typeof window !== 'undefined' && Number(window.devicePixelRatio) > 0 ? Number(window.devicePixelRatio) : 1);

  // measured is the windows area as the page laid it out, or null before
  // it has.
  const measured = () => {
    const w = Number(tilesEl.clientWidth) || 0;
    const h = Number(tilesEl.clientHeight) || 0;
    return w > 0 && h > 0 ? { w, h } : null;
  };

  // capacity is the grid of windows the area holds now.
  const capacity = () => {
    if (n <= 1) return { cols: 1, rows: 1 };
    const area = measured() || areaFor(pageW, pageH);
    return windowCapacity(area.w, area.h, advance);
  };

  // repaint draws a window's grid with a cell that fits the window width,
  // never below the readable size, and refreshes the text mirror. A grid
  // wider or taller than the window scrolls, and the cursor row is kept in
  // sight.
  // A window draws again only when a frame came or its cell changed.
  // It follows the cursor down while the reader has not scrolled away, and
  // sideways when the cursor moves past an edge.
  const repaint = (view) => {
    const grid = grids.get(view.pane);
    if (!grid) return;
    const ctx = view.canvas.getContext('2d');
    if (!advance) advance = measureAdvance(ctx);
    const cell = cellFor(Number(view.body.clientWidth) || 0, grid.cols, advance, dpr());
    const key = cell.w + '|' + cell.h + '|' + cell.dpr;
    if (!view.fresh && view.drawn === key) return;
    view.fresh = false;
    view.drawn = key;
    drawGrid(ctx, grid, cell);
    view.mirror.textContent = gridToText(grid);
    followCursor(view, grid, cell);
  };

  const followCursor = (view, grid, cell) => {
    const at = scrollFollow({
      shownW: Number(view.body.clientWidth) || 0, shownH: Number(view.body.clientHeight) || 0,
      cols: grid.cols, rows: grid.rows, cellW: cell.w, cellH: cell.h, cursor: grid.cursor,
      top: view.body.scrollTop, left: view.body.scrollLeft, autoTop: view.autoTop, lastX: view.cursorX,
    });
    if (at.top !== (Number(view.body.scrollTop) || 0)) view.body.scrollTop = at.top;
    if (at.left !== (Number(view.body.scrollLeft) || 0)) view.body.scrollLeft = at.left;
    view.autoTop = at.autoTop;
    view.cursorX = at.lastX;
  };

  // Frames come faster than a screen draws. Each window repaints at most
  // once per animation frame. Without requestAnimationFrame it repaints at
  // once.
  const dirty = new Set();
  let frameAsked = false;
  const flush = () => {
    frameAsked = false;
    for (const pane of dirty) {
      const view = viewOf(pane);
      if (view) repaint(view);
    }
    dirty.clear();
  };
  const schedule = (view) => {
    if (typeof requestAnimationFrame !== 'function') {
      repaint(view);
      return;
    }
    dirty.add(view.pane);
    if (!frameAsked) {
      frameAsked = true;
      requestAnimationFrame(flush);
    }
  };

  // setNote shows why a window has no picture, and a button for each fix
  // the message carries.
  const setNote = (pane, text) => {
    const view = viewOf(pane);
    if (!view) return;
    const m = toMessage(text);
    view.note.textContent = m.text;
    view.noteFix.textContent = '';
    view.noteFix.append(...fixButtons(m, (a) => { if (typeof floorState.fix === 'function') floorState.fix(a); }));
  };

  // attach asks for frames for one pane. It carries no size: the same
  // pane may be open in the operator's terminal, and a size here would
  // resize it. A refusal from the server is not tried again until the
  // agent is put in a window again, and the window says why. A refusal because the pane has
  // ended looks the pane up in the ended list.
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
        // Only a refusal from the server waits. A socket that was not open
        // yet is tried again by the next paint.
        if (e && e.code) failed.set(pane, e.message);
        setNote(pane, e.message);
        if (e && (e.code === 'pane_closed' || e.code === 'no_such_pane') && !stopping.has(pane)) agentGone(pane);
      });
  };

  // detaching holds each detach still on its way, so a view's watch can
  // wait for them before it attaches a pane a window let go.
  const detaching = new Set();
  const detach = (pane) => {
    attached.delete(pane);
    grids.delete(pane);
    bump(pane);
    const p = rpc('pane.detach', { pane }).catch(() => {});
    detaching.add(p);
    p.then(() => detaching.delete(p));
    return p;
  };

  // held is a promise the floor waits for before it attaches or detaches
  // anything: the detaches a view sent as the page left it. A paint while
  // it waits draws the windows and sends nothing.
  let held = null;
  const hold = (p) => {
    const mine = Promise.resolve(p).catch(() => {});
    held = mine;
    mine.then(() => {
      if (held !== mine) return;
      held = null;
      syncAttach();
    });
  };

  // syncAttach makes the attached set equal to the live panes the shown
  // windows hold, and empty while the floor is not the shown screen. A
  // pane whose attach was refused waits to be put in a window again.
  const syncAttach = () => {
    if (held) return;
    const want = new Set(visible() ? views.map((v) => v.pane).filter(Boolean) : []);
    for (const pane of [...attached]) if (!want.has(pane)) detach(pane);
    for (const pane of [...failed.keys()]) if (!want.has(pane)) failed.delete(pane);
    for (const pane of want) if (!attached.has(pane) && !failed.has(pane)) attach(pane);
  };

  // answerAsk posts one answer for the ask the pane shows now. confirm is
  // the typed pane name on a permanent allow.
  const answerAsk = async (pane, decision, confirm) => {
    const ask = askFor(stateOf(rows.get(pane)));
    if (!ask.visible || !ask.askId) return;
    const body = decision === 'deny'
      ? { tool_use_id: ask.askId, decision: 'deny', reason: 'denied from the floor' }
      : answerBody(ask.askId, 'allow', { pane, reason: 'allowed from the floor', confirm });
    try {
      if (ask.held) {
        // The harness holds this ask, so coppice-server answers it.
        const req = heldAnswer(pane, ask.askId, decision, confirm);
        await rpc(req.cmd, req.fields);
      } else {
        await api('/api/ask/answer', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        });
      }
      status(decision === 'deny' ? 'Denied.' : 'Allowed.');
      const view = viewOf(pane);
      if (view && view.askbar) view.askbar.hidden = true;
    } catch (e) {
      status(e.message);
    }
  };
  const denyAsk = (pane) => answerAsk(pane, 'deny');


  // blurField takes focus from a text field, so typing here is true: a
  // field with focus keeps every key.
  const blurField = () => {
    const a = typeof document !== 'undefined' ? document.activeElement : null;
    if (a && (FIELD_TAGS.includes(a.tagName) || a.isContentEditable) && typeof a.blur === 'function') a.blur();
  };

  // takeKeys selects slot i and gives it the keys.
  const takeKeys = (i) => {
    if (!Number.isInteger(i) || i < 0 || i >= Math.max(n, 0) || i >= tiles.slots.length) return;
    tiles.focus = i;
    keysIn = 'window';
    blurField();
    const pane = focused(tiles);
    if (pane) {
      railSel = paneOfSlot(pane);
      failed.delete(pane);
    }
    renderAll();
    redrawRail();
    // A headless agent takes its keys in its message line.
    const view = viewOf(pane);
    if (view && view.promptInput && isHeadless(rows.get(pane))) focusOn(view.promptInput);
  };

  // leave gives the keys to the rail, on the selected window's row.
  const leave = () => {
    keysIn = 'rail';
    const pane = paneOfSlot(focused(tiles));
    if (pane && rows.has(pane)) railSel = pane;
    if (!rows.has(railSel)) railSel = railIds()[0] || '';
    renderAll();
    redrawRail();
  };

  // A browser resets the scroll of an element that leaves the page and
  // comes back, and a redraw moves each window body. scrollOf and scrollTo
  // carry the scroll across.
  const scrollOf = (view) => [Number(view.body.scrollTop) || 0, Number(view.body.scrollLeft) || 0];
  const scrollTo = (view, at) => {
    if (view.body.scrollTop !== at[0]) view.body.scrollTop = at[0];
    if (view.body.scrollLeft !== at[1]) view.body.scrollLeft = at[1];
  };

  // trusting holds the panes whose trust answer is on its way.
  const trusting = new Set();

  // renderHead rebuilds a window's header and ask bar from its row.
  // While its label is renamed a header is not drawn again, so the field
  // keeps its keys.
  const renderHead = (view) => {
    if (view.renaming && editing && editing.where === 'head' && editing.pane === view.pane) return;
    const row = rows.get(view.pane);
    const st = stateOf(row);
    const chip = stateChip(st.state);
    const head = make('div', 'head');
    const labelText = (row && row.label) || view.pane;
    const label = make('strong', 'label', labelText);
    view.renaming = false;
    if (editing && editing.where === 'head' && editing.pane === view.pane) {
      view.renaming = true;
      editInPlace(label, labelText, (next) => rename(view.pane, next), () => { view.renaming = false; endRename(); });
    }
    // A click on the header selects the window and gives it the keys. A
    // double click on the label renames the agent.
    head.addEventListener('click', (e) => {
      if (e && e.detail >= 2 && e.target === label) {
        startRename(view.pane, 'head');
        return;
      }
      takeKeys(view.slot);
    });
    label.addEventListener('dblclick', (e) => {
      if (e && typeof e.stopPropagation === 'function') e.stopPropagation();
      if (editing === null) startRename(view.pane, 'head');
    });
    // The header drags the window onto another window to swap the two. The
    // lock refuses a drag as it starts and says so. The header stays
    // draggable while locked, since a browser fires no dragstart on one
    // that is not, and the refusal would say nothing.
    head.draggable = !view.renaming;
    head.addEventListener('dragstart', (e) => {
      if (tiles.locked) {
        if (e && typeof e.preventDefault === 'function') e.preventDefault();
        status(LOCKED);
        return;
      }
      dragFrom = view.slot;
      if (e && e.dataTransfer) {
        e.dataTransfer.effectAllowed = 'move';
        e.dataTransfer.setData('text/plain', view.pane);
      }
    });
    head.addEventListener('dragend', () => { dragFrom = null; });
    const now = Date.now() / 1000;
    const facts = typeof floorState.facts === 'function' ? floorState.facts() : null;
    const segs = segments(row, facts);
    if (!segs.some((x) => x.key === openSeg.get(view.pane))) openSeg.delete(view.pane);
    head.append(
      make('span', 'num', String(view.slot + 1)),
      make('span', 'dot dot-' + chip.word),
      label,
    );
    const mark = gateMark(row && row.gate);
    if (mark) head.append(...gateEl(view.pane, mark));
    // The second line holds what the agent runs, its state word and its
    // directory, so the stack bar has the width to show its words. With no
    // stack the harness shows as a word, and one the server does not know
    // is left out.
    const line2 = make('div', 'head2');
    if (segs.length) line2.append(stackBar(view.pane, segs, light(row)));
    else if (st.harness && st.harness !== 'unknown') line2.append(make('span', 'muted', st.harness));
    line2.append(make('span', 'state state-' + chip.word, chip.word), make('span', 'muted cwd', lastSegment(row && row.cwd)));
    line2.addEventListener('click', () => takeKeys(view.slot));
    const look = lookingLine(row && row.looking, floorState.me || '');
    if (look) head.append(make('span', 'muted looking', look));
    if (view.pane === typing()) head.append(make('span', 'typing-words', TYPING_TEXT));
    head.append(killButton(view.pane, labelText, askKill));
    const confirm = killAsk === view.pane ? confirmLine(labelText, () => stopKill(true), () => stopKill(false)) : null;
    const segOpen = openSeg.get(view.pane);
    const segPanel = segOpen ? detailsEl(view.pane, details(segOpen, row, facts, now)) : null;
    const ask = askFor(st);
    let askbar = null;
    if (ask.visible) {
      askbar = make('div', 'askbar');
      askbar.append(make('p', 'summary', ask.summary));
      // A held ask can still be answered here. The line says a foreman
      // hears it first.
      if (row && row.held) askbar.append(make('p', 'muted held', heldLine(row.held, Date.now() / 1000)));
      const verdict = verdictLine(st.ask);
      if (verdict) askbar.append(make('p', 'verdict', verdict));
      const name = paneName(row || { id: view.pane });
      // Reply gives this window the keys, so the answer is typed straight
      // into the agent here.
      const replyBtn = make('button', '', 'Reply');
      replyBtn.title = 'Type into this window';
      replyBtn.addEventListener('click', (e) => {
        if (e && typeof e.stopPropagation === 'function') e.stopPropagation();
        takeKeys(view.slot);
      });
      askbar.append(make('p', 'tier tier-' + ask.tier, ask.held ? heldTierLine(ask.tier, name) : tierLine(ask.tier, name)));
      const deny = denyButton(() => denyAsk(view.pane));
      const allow = allowControl(ask.tier, name, (confirm) => answerAsk(view.pane, 'allow', confirm), status, ask.askId);
      askbar.append(deny, allow, replyBtn);
    } else if (trustFor(st)) {
      askbar = make('div', 'askbar trustbar');
      askbar.append(make('p', 'summary', TRUST_TEXT));
      // Both buttons stay off while one answer is on its way, so a
      // second click cannot send a second answer.
      const answerTrust = (yes) => (e) => {
        if (e && typeof e.stopPropagation === 'function') e.stopPropagation();
        if (trusting.has(view.pane)) return;
        trusting.add(view.pane);
        renderHead(view);
        const req = trustCommand(view.pane, yes);
        rpc(req.cmd, { pane: req.pane, trust: req.trust })
          .then(() => status(trustDone(yes)))
          .catch((err) => status(err.message))
          .finally(() => {
            trusting.delete(view.pane);
            const v = viewOf(view.pane);
            if (v) renderHead(v);
          });
      };
      const yes = make('button', 'allow', TRUST_YES.label);
      yes.title = TRUST_YES.title;
      yes.addEventListener('click', answerTrust(true));
      const no = make('button', 'deny', TRUST_NO.label);
      no.title = TRUST_NO.title;
      no.addEventListener('click', answerTrust(false));
      yes.disabled = no.disabled = trusting.has(view.pane);
      askbar.append(yes, no);
    }
    view.askbar = askbar;
    // The notes are the commands the pane ran, most often the foreman's
    // hands. They draw dim under the header.
    const kept = notes.get(view.pane) || [];
    const noteLines = kept.length ? make('div', 'notes') : null;
    for (const text of kept) noteLines.append(make('p', 'note-line muted', text));
    const at = scrollOf(view);
    const line = isHeadless(row) ? messageLine(view) : null;
    const had = line && typeof document !== 'undefined' && document.activeElement === view.promptInput;
    view.section.replaceChildren(...[head, line2, confirm, segPanel, noteLines, askbar, view.body, line, view.note, view.noteFix].filter(Boolean));
    if (had) focusOn(view.promptInput);
    scrollTo(view, at);
  };

  // focusOn gives an element the focus when it can take it.
  const focusOn = (el) => { if (el && typeof el.focus === 'function') el.focus(); };

  // messageLine is the line under a headless agent's window. The agent has
  // no terminal to type into, so the window sends it no keys: Enter in this
  // line sends the whole message, and ctrl-space gives the keys back to
  // the rail. It is made once per window, so typed text survives a redraw.
  const messageLine = (view) => {
    if (view.promptLine) return view.promptLine;
    const line = make('div', 'prompt-line');
    const input = make('input', 'prompt-input');
    input.type = 'text';
    input.placeholder = 'Message to the agent';
    input.setAttribute('aria-label', 'Message to the agent');
    input.addEventListener('click', (e) => { if (e && typeof e.stopPropagation === 'function') e.stopPropagation(); });
    input.addEventListener('focus', () => {
      if (typing() === view.pane) return;
      const at = tiles.slots.indexOf(view.pane);
      if (at >= 0) {
        tiles.focus = at;
        keysIn = 'window';
        railSel = view.pane;
        renderAll();
        redrawRail();
      }
    });
    input.addEventListener('keydown', (e) => {
      if (isLeave(e)) {
        if (typeof e.preventDefault === 'function') e.preventDefault();
        if (typeof input.blur === 'function') input.blur();
        leave();
        return;
      }
      if (e.key !== 'Enter') return;
      if (typeof e.preventDefault === 'function') e.preventDefault();
      const text = String(input.value || '');
      if (!text.trim()) return;
      input.value = '';
      outbox = outbox
        .then(() => rpc('agent.prompt', { pane: view.pane, text }))
        .catch((err) => {
          if (!input.value) input.value = text;
          status(err);
        });
    });
    line.append(make('p', 'muted headless-note', HEADLESS_NOTE), input);
    view.promptLine = line;
    view.promptInput = input;
    return line;
  };

  // stackBar is the bar of segments on a window header. A click opens a
  // segment's details below the header, or closes them.
  const stackBar = (pane, segs, lit) => stackBarEl(segs, { open: openSeg.get(pane), lit, onSeg: (key) => toggleSeg(pane, key) });

  // gateEl is the last verdict's mark and its tip. A click opens the
  // daisugi details, which show the tool and clause too.
  const gateEl = (pane, mark) => gateMarkEls(mark, () => toggleSeg(pane, 'daisugi'));

  // detailsEl is an open segment's details, drawn below the header.
  const detailsEl = (pane, d) => detailsBox(d, {
    onJournal: typeof floorState.journal === 'function' ? () => floorState.journal(pane) : null,
    onClose: () => toggleSeg(pane, openSeg.get(pane)),
  });

  const toggleSeg = (pane, key) => {
    if (!key || openSeg.get(pane) === key) openSeg.delete(pane);
    else openSeg.set(pane, key);
    const view = viewOf(pane);
    if (view) renderHead(view);
  };

  // dropOn makes a window a place a dragged header or rail row can land. A
  // header dropped here swaps the two windows, and the lock refuses it and
  // says so. A row dropped here puts its agent in this window. at is the
  // window's place, or a function that reads it when the drop lands, since
  // a window kept across redraws moves.
  const dropOn = (section, at) => {
    section.addEventListener('dragover', (e) => {
      if ((dragFrom !== null || hasRowDrag(e)) && typeof e.preventDefault === 'function') e.preventDefault();
    });
    section.addEventListener('drop', (e) => {
      if (typeof e.preventDefault === 'function') e.preventDefault();
      const from = dragFrom;
      dragFrom = null;
      const pane = hasRowDrag(e) ? e.dataTransfer.getData(ROW_TYPE) : '';
      const act = dropAction({ slot: from, pane }, typeof at === 'function' ? at() : at);
      if (!act) return;
      if (act.kind === 'put') {
        putRow(act.pane, act.window);
        return;
      }
      const done = swap(tiles, act.from, act.to);
      if (!done.ok) {
        status(done.reason);
        return;
      }
      renderAll();
    });
  };

  const tileClass = (i, pane) => 'tile'
    + (i === tiles.focus ? ' focused' : '')
    + (pane && pane === typing() ? ' typing' : '');

  // endedTile is the window an ended agent left: its one line, amber for
  // a non-zero exit. A click clears it. Until the ended list answers, the
  // window is blank.
  const endedTile = (slot, i) => {
    const pane = paneOfSlot(slot);
    const info = ended.get(pane);
    const amber = info && Number.isInteger(info.exit) && info.exit !== 0;
    const section = make('section', 'tile ended' + (amber ? ' amber' : '') + (i === tiles.focus ? ' focused' : ''));
    section.dataset.tile = '';
    section.dataset.ended = pane;
    dropOn(section, i);
    if (info) {
      section.append(make('p', 'ended-line', endedLine(info.label, info.exit)), make('p', 'muted ended-hint', CLEAR_HINT));
      const fixes = make('p', 'ended-fix');
      fixes.append(...fixButtons(endedMessage(pane, info.label, info.exit), (a) => {
        clearEnded(pane);
        if (typeof floorState.fix === 'function') floorState.fix(a);
      }));
      section.append(fixes);
    }
    section.addEventListener('click', () => clearEnded(pane));
    return { pane: '', slot: i, section };
  };

  const emptyTile = (i, text) => {
    const section = make('section', 'tile empty' + (i === tiles.focus ? ' focused' : ''));
    section.dataset.tile = '';
    dropOn(section, i);
    section.textContent = text;
    return { pane: '', slot: i, section };
  };

  const buildTile = (slot, i) => {
    if (isEndedSlot(slot)) return endedTile(slot, i);
    if (!slot) return emptyTile(i, FREE_TEXT);
    const pane = slot;
    const have = cache.get(pane);
    if (have) {
      have.slot = i;
      have.section.className = tileClass(i, pane);
      renderHead(have);
      return have;
    }
    const section = make('section', tileClass(i, pane));
    section.dataset.tile = pane;
    const canvas = make('canvas');
    const mirror = make('pre', 'mirror visually-hidden');
    const body = make('div', 'tile-body');
    body.append(canvas, mirror);
    const view = { pane, slot: i, section, canvas, mirror, body, note: make('p', 'note'), noteFix: make('p', 'note-fix'), askbar: null };
    // A click on the body selects the window and gives it the keys. The
    // slot is read at click time, since a swap moves the window.
    body.addEventListener('click', () => takeKeys(view.slot));
    dropOn(section, () => view.slot);
    renderHead(view);
    cache.set(pane, view);
    return view;
  };

  // windowSlots is the slots the page shows now: every slot that holds an
  // agent or a line, and the free ones only while the layout is locked,
  // since then nothing moves up into them.
  const windowSlots = () => {
    const all = shown(tiles, n);
    if (tiles.locked) return all;
    let last = -1;
    all.forEach((s, i) => { if (s) last = i; });
    return all.slice(0, last + 1);
  };

  // renderAll rebuilds the shown windows from the slots, reusing each
  // window whose agent stays, so its canvas is not drawn again from
  // nothing. A drag in progress ends here: the header it started from is
  // gone, so its dragend never fires, and a later drop must not swap from
  // it.
  const renderAll = () => {
    const scrolls = new Map([...cache.values()].map((v) => [v, scrollOf(v)]));
    dragFrom = null;
    tilesEl.dataset.locked = String(tiles.locked);
    if (typing() && !visible()) keysIn = 'rail';
    const slots = windowSlots();
    let built;
    if (n === 0) {
      built = [];
    } else if (slots.length === 0) {
      const none = rows.size === 0;
      built = [emptyTile(0, none ? NO_AGENTS : FREE_TEXT)];
    } else {
      built = slots.map(buildTile);
    }
    const livePane = new Set(built.map((v) => v.pane).filter(Boolean));
    for (const pane of [...cache.keys()]) if (!livePane.has(pane)) cache.delete(pane);
    views = built.filter((v) => v.pane);
    const shape = windowShape(Math.max(1, built.length), capacity());
    tilesEl.dataset.tiles = String(built.length);
    tilesEl.dataset.cols = String(shape.cols);
    tilesEl.dataset.rows = String(shape.rows);
    tilesEl.style.gridTemplateColumns = 'repeat(' + shape.cols + ', minmax(0, 1fr))';
    tilesEl.style.gridTemplateRows = 'repeat(' + shape.rows + ', minmax(0, 1fr))';
    const spare = shape.cols * shape.rows - built.length;
    built.forEach((v, i) => {
      v.section.style.gridColumn = i === built.length - 1 && spare > 0 ? 'span ' + (spare + 1) : '';
    });
    // Windows that stay in place are not taken out and put back: a browser
    // takes the focus from a field in one that leaves the page.
    const sections = built.map((v) => v.section);
    const now = Array.from(tilesEl.children || []);
    if (now.length !== sections.length || now.some((s, i) => s !== sections[i])) tilesEl.replaceChildren(...sections);
    for (const [v, at] of scrolls) scrollTo(v, at);
    for (const v of views) schedule(v);
    syncAttach();
    if (typeof floorState.changed === 'function') floorState.changed();
  };

  // railIds is the pane ids the rail shows, in rail order: the tree, with
  // folded groups left out.
  const liveRows = () => paneRows(lastList.filter((p) => p && !looked.has(p.id)), Date.now() / 1000);
  const railIds = () => railOrder(railEntries(liveRows(), tasks, folds));

  // clearEnded takes an ended agent's line off its window, which then
  // takes the next agent or closes up.
  const clearEnded = (pane) => {
    const i = tiles.slots.indexOf(endedSlot(pane));
    if (i < 0) return;
    tiles.slots[i] = '';
    ended.delete(pane);
    paint(lastList);
  };

  // agentGone is called when a pane a window holds leaves the live list,
  // or its attach says it is gone. Its window turns blank at once, and one
  // read of the ended list says why: a pane there ended on its own and its
  // window shows the line; a pane not there was closed, and its window
  // clears. The keys go back to the rail when they were in it.
  const agentGone = (pane, label) => {
    if (label) labels.set(pane, label);
    looked.add(pane);
    if (railSel === pane) railSel = railIds()[0] || '';
    const i = slotOf(tiles, pane);
    if (i >= 0) {
      if (keysIn === 'window' && tiles.focus === i) keysIn = 'rail';
      tiles.slots[i] = endedSlot(pane);
      failed.delete(pane);
    }
    lookUpEnded(pane);
  };

  const lookUpEnded = (pane) => {
    if (asked.has(pane)) return;
    asked.add(pane);
    looked.add(pane);
    const label = labels.get(pane) || (rows.get(pane) && rows.get(pane).label) || pane;
    Promise.resolve()
      .then(() => rpc('pane.list', { ended: true }))
      .then((r) => {
        const list = r && Array.isArray(r.panes) ? r.panes : [];
        const rec = list.find((x) => x && x.id === pane);
        if (!rec) {
          clearEnded(pane);
          return;
        }
        const exit = Number.isInteger(rec.exit_code) ? rec.exit_code : null;
        ended.set(pane, { label: rec.label || label, exit });
        if (tiles.slots.includes(endedSlot(pane))) {
          renderAll();
          if (exit === 0 || exit === null) setTimeout(() => clearEnded(pane), ENDED_CLEAR_MS);
        } else {
          status(endedMessage(pane, rec.label || label, exit));
          endedShown = true;
        }
      })
      .catch(() => clearEnded(pane));
  };

  // fillFree puts agents with no window into free slots, in fill order.
  const fillFree = () => {
    const rowsNow = paneRows(lastList, Date.now() / 1000);
    const byId = new Map(rowsNow.map((r) => [r.id, r]));
    const rail = treeOrder(rowsNow, tasks).map((id) => byId.get(id));
    for (const id of fillOrder(rail)) {
      if (slotOf(tiles, id) >= 0 || looked.has(id)) continue;
      if (tiles.layout === 'all') { tiles.slots.push(id); continue; }
      const free = tiles.slots.indexOf('');
      if (free < 0) break;
      tiles.slots[free] = id;
    }
  };

  // closeUp moves free slots after the ones in use, so no blank window
  // sits between two agents. The selected window keeps its agent. The lock
  // stops it.
  const closeUp = () => {
    if (tiles.locked || tiles.layout === 'all') return;
    const keep = tiles.slots[tiles.focus];
    const used = tiles.slots.filter(Boolean);
    const next = [...used, ...tiles.slots.filter((s) => !s)];
    tiles.slots = next;
    const back = keep ? next.indexOf(keep) : -1;
    tiles.focus = back >= 0 ? back : Math.min(tiles.focus, Math.max(0, used.length - 1));
  };

  // paint takes a fresh live pane list. A pane gone from the list leaves
  // its window through agentGone. Free windows take agents in fill order.
  // The first paint with agents while the floor shows puts the keys on the
  // first agent that needs you, or the first window, or the rail. Later
  // paints never move the keys.
  let lastList = [];
  const paint = (panes) => {
    const list = Array.isArray(panes) ? panes : [];
    lastList = list;
    if (typeof document !== 'undefined') document.title = titleFor(list);
    const before = rows;
    rows = new Map(list.map((p) => [p.id, p]));
    order = treeOrder(paneRows(list, Date.now() / 1000), tasks);
    for (const slot of [...tiles.slots]) {
      if (!slot || isEndedSlot(slot) || rows.has(slot)) continue;
      // An agent the operator stops leaves its window with no line.
      if (stopping.has(slot)) tiles.slots[tiles.slots.indexOf(slot)] = '';
      else agentGone(slot, before.get(slot) && before.get(slot).label);
    }
    fillFree();
    closeUp();
    if (!started && visible() && list.length > 0 && n > 0) {
      started = true;
      const needs = new Set(list.filter(needsYou).map((p) => p.id));
      const at = startSelect(shown(tiles, n), needs);
      if (at >= 0) {
        tiles.focus = at;
        keysIn = 'window';
        railSel = focused(tiles);
        blurField();
      } else {
        keysIn = 'rail';
        railSel = order[0] || '';
      }
    }
    const wasSel = railSel;
    const live = (id) => rows.has(id) && !looked.has(id);
    if (!live(railSel)) {
      const here = paneOfSlot(focused(tiles));
      railSel = live(here) ? here : order.find(live) || '';
    }
    if (killAsk && !live(killAsk)) killAsk = '';
    renderAll();
    // The rail drew before this paint, so a row that just became the
    // selected one needs the rail drawn again.
    if (railSel !== wasSel && !typing()) redrawRail();
    fit();
    // An agent New just made goes in a window with the keys once it lists.
    if (want && live(want) && n > 0) {
      const made = want;
      want = '';
      fillRow(made);
    }
  };

  // fit checks the window count against the area the page really laid
  // out, once it has one, and redraws when they differ.
  const fit = () => {
    const area = measured();
    if (!area || pageW < 900) return;
    const cap = windowCapacity(area.w, area.h, advance);
    if (cap.cols * cap.rows !== n) setSize(pageW, pageH);
  };

  // addNote keeps the last NOTES_SHOWN notes of one pane. A note with no
  // pane is an operator note and has no window.
  const addNote = (msg) => {
    if (!msg || !msg.pane || typeof msg.text !== 'string') return false;
    const kept = [...(notes.get(msg.pane) || []), msg.text].slice(-NOTES_SHOWN);
    notes.set(msg.pane, kept);
    return true;
  };

  const seedNotes = (list) => {
    for (const msg of Array.isArray(list) ? list : []) addNote(msg);
    for (const view of views) if (view.pane) renderHead(view);
  };

  const onEvent = (msg) => {
    if (!msg || !msg.pane) return;
    if (msg.event === 'note') {
      if (!addNote(msg)) return;
      const view = viewOf(msg.pane);
      if (view) renderHead(view);
      return;
    }
    if (msg.event === 'frame') {
      const view = viewOf(msg.pane);
      if (!view || !attached.has(msg.pane)) return;
      grids.set(msg.pane, applyFrame(grids.get(msg.pane) || null, msg));
      view.fresh = true;
      schedule(view);
      return;
    }
    if (msg.event === 'presence' && rows.has(msg.pane)) {
      rows.set(msg.pane, { ...rows.get(msg.pane), looking: Array.isArray(msg.looking) ? msg.looking : [] });
      const view = viewOf(msg.pane);
      if (view) renderHead(view);
      return;
    }
    if (msg.event === 'state' && rows.has(msg.pane)) {
      // A process that ended reads as done. Whether it ended on its own or
      // was closed, only the ended list says, so a window it holds asks.
      if (msg.state === 'done' && msg.source === 'process' && stopping.has(msg.pane)) return;
      if (msg.state === 'done' && msg.source === 'process') {
        const label = rows.get(msg.pane).label;
        if (slotOf(tiles, msg.pane) < 0) {
          labels.set(msg.pane, label);
          lookUpEnded(msg.pane);
        } else {
          agentGone(msg.pane, label);
          renderAll();
          redrawRail();
          return;
        }
      }
      rows.set(msg.pane, rowFromEvent(rows.get(msg.pane), msg));
      const view = viewOf(msg.pane);
      if (view) renderHead(view);
    }
  };

  // detachAll detaches every pane and returns a promise that settles once
  // each detach is answered.
  const detachAll = () => Promise.all([...attached].map(detach));

  // release hands one attach to the pane screen. The pane leaves the
  // attached set with no wire message, so the server keeps the pump
  // running for the pane screen's own attach, and the grid goes with it so
  // the pane screen starts from the picture the window had. It returns
  // null for a pane the floor does not hold.
  const release = (pane) => {
    if (!attached.has(pane)) return null;
    attached.delete(pane);
    bump(pane);
    const grid = grids.get(pane) || null;
    grids.delete(pane);
    return grid;
  };

  // adopt takes an attach back from the pane screen with no wire message
  // and draws the window from the grid it brings. The next paint detaches
  // the pane when no shown slot holds it.
  const adopt = (pane, grid) => {
    if (!pane) return;
    attached.add(pane);
    bump(pane);
    if (grid) grids.set(pane, grid);
    else grids.delete(pane);
    const view = viewOf(pane);
    if (view) {
      view.fresh = true;
      repaint(view);
    }
  };

  // dropped forgets every attach and every grid and sends nothing. The
  // socket that held them is gone, and the next paint attaches again on
  // the new one. Without this the paint after a reconnect would see every
  // shown pane already attached and leave the windows frozen.
  const dropped = () => {
    for (const pane of attached) bump(pane);
    attached.clear();
    failed.clear();
    grids.clear();
  };

  // setSize redraws the floor for a new page size. When the window count
  // changes, the windows keep their agents in order as far as they fit,
  // and every window draws again at its new width.
  const setSize = (widthPx, heightPx) => {
    pageW = Number(widthPx) || 0;
    if (heightPx !== undefined) pageH = Number(heightPx) || 0;
    const area = measured();
    const cap = area && pageW >= 900 ? windowCapacity(area.w, area.h, advance) : null;
    const next = cap ? cap.cols * cap.rows : tileCount(pageW, pageH, advance);
    if (next !== n) {
      n = next;
      place();
      if (tiles.layout === 'focus') {
        // The selected agent keeps a window, first when it would be cut, so
        // a shrink never hands its keys to another agent.
        const size = Math.max(n, 1);
        const cur = focused(tiles);
        let keep = tiles.slots.filter(Boolean);
        if (cur && keep.indexOf(cur) >= size) keep = [cur, ...keep.filter((x) => x !== cur)];
        keep = keep.slice(0, size);
        tiles.slots = Array.from({ length: size }, (_, i) => keep[i] || '');
        const at = cur ? tiles.slots.indexOf(cur) : -1;
        if (at >= 0) {
          tiles.focus = at;
        } else {
          tiles.focus = Math.min(tiles.focus, size - 1);
          if (keysIn === 'window') {
            keysIn = 'rail';
            if (rows.has(paneOfSlot(cur))) railSel = paneOfSlot(cur);
          }
        }
      }
      if (n === 0) keysIn = 'rail';
      paint(lastList);
      return;
    }
    for (const v of views) schedule(v);
  };

  // A click anywhere on the floor page outside every window and every row
  // gives the keys to the rail.
  root.addEventListener('click', (e) => {
    const t = e && e.target;
    if (!typing()) return;
    if (t && typeof t.closest === 'function' && (t.closest('.tile') || t.closest('[data-row]'))) return;
    leave();
  });

  // fillRow puts a row's agent in the selected window, or selects the
  // window that already shows it, and gives that window the keys.
  const fillRow = (pane) => {
    if (!pane || looked.has(pane)) return;
    let i = slotOf(tiles, pane);
    if (i < 0) {
      const free = tiles.slots.indexOf('');
      i = free >= 0 && free < n ? free : Math.min(tiles.focus, Math.max(0, n - 1));
      if (tiles.layout === 'all' && free < 0) { tiles.slots.push(pane); i = tiles.slots.length - 1; }
      else tiles.slots[i] = pane;
    }
    takeKeys(i);
  };
  // openRow is what a ctrl click or a middle click does. With windows
  // sized to fit, a new window is a free one, so it is the same as fillRow.
  const openRow = (pane) => fillRow(pane);

  // putRow puts a row's agent in window number i+1 and gives it the keys.
  // An agent already in another window swaps places with what window i
  // holds. A number past the windows there are fills the first free one.
  const putRow = (pane, i) => {
    if (!pane || !rows.has(pane) || looked.has(pane) || n === 0 || i >= n) return;
    let at = i;
    if (at >= windowSlots().length) {
      at = tiles.slots.indexOf('');
      if (at < 0) return;
    }
    const was = slotOf(tiles, pane);
    if (was >= 0 && was !== at) tiles.slots[was] = tiles.slots[at];
    tiles.slots[at] = pane;
    closeUp();
    takeKeys(slotOf(tiles, pane));
  };

  const labelOf = (pane) => (rows.get(pane) && rows.get(pane).label) || labels.get(pane) || pane;

  // askKill asks once before it stops an agent: its row and its window
  // header show the confirm line, and Enter or Stop stops it, Esc or Keep
  // keeps it.
  const askKill = (pane) => {
    if (!pane || !rows.has(pane) || looked.has(pane)) return;
    if (editing) endRename();
    killAsk = pane;
    railSel = typing() ? railSel : pane;
    renderAll();
    redrawRail();
  };

  // gone takes an agent the operator stopped off the floor at once. Its
  // record is gone too, so no ended line is looked up for it.
  const gone = (pane) => {
    looked.add(pane);
    asked.add(pane);
    ended.delete(pane);
    const i = slotOf(tiles, pane);
    if (i >= 0) {
      if (keysIn === 'window' && tiles.focus === i) keysIn = 'rail';
      tiles.slots[i] = '';
      failed.delete(pane);
    }
    if (railSel === pane) {
      const ids = railIds();
      railSel = ids[0] || '';
    }
    paint(lastList.filter((p) => p && p.id !== pane));
    redrawRail();
  };

  // stopKill ends the confirm: stop true closes the agent, which removes
  // its record, and false keeps it.
  const stopKill = (stop) => {
    const pane = killAsk;
    if (!pane) return;
    killAsk = '';
    if (!stop) {
      renderAll();
      redrawRail();
      return;
    }
    const label = labelOf(pane);
    stopping.add(pane);
    rpc('pane.close', { pane })
      .then(() => {
        status('Stopped ' + label + '.');
        gone(pane);
        refreshLists();
      })
      .catch((e) => {
        stopping.delete(pane);
        status(e.message);
        renderAll();
        redrawRail();
      });
  };

  // startRename opens the label of pane for editing in its rail row or its
  // window header. Only one label is edited at a time.
  const startRename = (pane, where) => {
    if (!pane || !rows.has(pane) || looked.has(pane)) return;
    if (killAsk) killAsk = '';
    editing = { pane, where: where === 'head' && viewOf(pane) ? 'head' : 'row' };
    if (editing.where === 'row') railSel = pane;
    renderAll();
    redrawRail();
    focusRename();
  };

  // endRename closes the label field and draws what it held back.
  const endRename = () => {
    if (!editing) return;
    editing = null;
    for (const v of views) v.renaming = false;
    renderAll();
    redrawRail();
  };

  // rename sends the new label and shows it at once.
  const rename = (pane, label) => {
    // The roster draws from the same row objects as the floor, so the new
    // name is set on them in place and every redraw shows it at once.
    for (const p of lastList) if (p && p.id === pane) p.label = label;
    const row = rows.get(pane);
    if (row) row.label = label;
    const view = viewOf(pane);
    if (view) renderHead(view);
    redrawRail();
    rpc('pane.rename', { pane, label })
      .then(() => refreshLists())
      .catch((e) => {
        status(e.message);
        refreshLists();
      });
  };

  // selectedPane is the agent New starts near: the one typed into, or the
  // selected rail row.
  const selectedPane = () => typing() || (rows.has(railSel) && !looked.has(railSel) ? railSel : '');

  // seenRecent drops every ended line, once the owner opens Recent.
  const seenRecent = () => {
    let any = false;
    tiles.slots = tiles.slots.map((s) => {
      if (!isEndedSlot(s)) return s;
      any = true;
      ended.delete(paneOfSlot(s));
      return '';
    });
    if (endedShown) {
      status('');
      endedShown = false;
    }
    if (any) paint(lastList);
  };

  // sendText sends typed bytes to pane as raw text with no Enter added.
  // Each send waits for the reply to the one before it, so the pane gets
  // the bytes in the order they were typed.
  const sendText = (pane, text) => {
    outbox = outbox
      .then(() => rpc('pane.send_text', { pane, text, enter: false }))
      .catch((e) => status(e.message));
  };
  const canShow = () => n > 0;

  // A layout change rebuilds the slots from the shown panes in rail
  // order. The lock refuses it, the same as it refuses reset.
  select.addEventListener('change', () => {
    const want = select.value;
    if (!LAYOUTS.includes(want) || want === tiles.layout) return;
    if (tiles.locked) {
      select.value = tiles.layout;
      status(LOCKED);
      return;
    }
    const showing = livePanes(shown(tiles, n));
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
    renderAll();
  });

  // moveRail moves the selected row by one, and stops at either end.
  const moveRail = (by) => {
    const ids = railIds();
    if (ids.length === 0) return;
    const at = ids.indexOf(railSel);
    const next = at < 0 ? 0 : Math.min(ids.length - 1, Math.max(0, at + by));
    railSel = ids[next];
    redrawRail();
  };

  // The keys go where routeKey says. They hold only while the floor is
  // the shown screen and a window can show, and a key typed into a field
  // stays with the field.
  // A click anywhere but the confirm line keeps an agent a kill asked
  // about. It listens as the click goes down, so a control that keeps its
  // own click, such as New or its menu, still cancels the kill.
  document.addEventListener('click', (e) => {
    if (!killAsk) return;
    const t = e && e.target;
    if (t && typeof t.closest === 'function' && t.closest('[data-confirm]')) return;
    stopKill(false);
  }, true);

  // killTarget is true for a key the kill confirm may take: one typed on
  // the page itself, on a rail row, or on the confirm's own buttons. A key
  // on any other control is that control's.
  const killTarget = (t) => {
    if (!t || t.tagName === 'BODY' || t.tagName === 'HTML') return true;
    if (typeof t.closest !== 'function') return false;
    return Boolean(t.closest('[data-confirm]') || t.closest('[data-row]'));
  };

  document.addEventListener('keydown', (e) => {
    if (!visible() || !canShow()) return;
    // A kill that waits for its confirm takes Enter and Esc first, so
    // neither reaches an agent. Any other key keeps the agent.
    if (killAsk) {
      const k = killTarget(e.target) ? killKey(e) : null;
      if (k) {
        if (typeof e.preventDefault === 'function') e.preventDefault();
        if (typeof e.stopPropagation === 'function') e.stopPropagation();
        stopKill(k === 'stop');
        return;
      }
      if (!['Shift', 'Control', 'Alt', 'Meta'].includes(e.key)) stopKill(false);
    }
    const tag = e.target && e.target.tagName;
    if (FIELD_TAGS.includes(tag)) return;
    // An open menu keeps its own keys.
    if (e.target && typeof e.target.closest === 'function' && e.target.closest('[role="menu"]')) return;
    const pane = typing();
    if (!pane) {
      // A focused button, link or editable keeps its own keys on the rail.
      const t = e.target;
      if (t && (t.tagName === 'BUTTON' || t.tagName === 'A' || t.isContentEditable)) return;
      // A row with focus, reached by Tab, is the selected row.
      const row = t && typeof t.closest === 'function' ? t.closest('[data-row]') : null;
      if (row && row.dataset && rows.has(row.dataset.row) && !looked.has(row.dataset.row)) railSel = row.dataset.row;
    }
    const act = routeKey(e, pane ? 'window' : 'rail');
    if (!act) return;
    // A headless agent takes no raw keys. The key goes to its message
    // line, which takes the focus before the browser types the key.
    if (act.kind === 'send' && isHeadless(rows.get(pane))) {
      const view = viewOf(pane);
      if (view) focusOn(view.promptInput);
      return;
    }
    if (typeof e.preventDefault === 'function') e.preventDefault();
    if (act.kind === 'send') sendText(pane, act.bytes);
    else if (act.kind === 'leave') leave();
    else if (act.kind === 'move') moveRail(act.by);
    else if (act.kind === 'enter') {
      if (railSel && rows.has(railSel) && !looked.has(railSel)) fillRow(railSel);
      else if (typeof focused(tiles) === 'string' && focused(tiles) && !isEndedSlot(focused(tiles))) takeKeys(tiles.focus);
    } else if (act.kind === 'put') putRow(railSel, act.window);
    else if (act.kind === 'deny' && railSel) denyAsk(railSel);
    else if (act.kind === 'kill' && railSel) askKill(railSel);
    else if (act.kind === 'rename' && railSel) startRename(railSel, 'row');
  });

  // A paste while a window types goes to its agent as one text. Line
  // breaks become carriage returns, as a terminal sends them.
  document.addEventListener('paste', (e) => {
    const pane = typing();
    if (!pane || !visible() || !canShow()) return;
    const tag = e.target && e.target.tagName;
    if (FIELD_TAGS.includes(tag)) return;
    const text = e.clipboardData ? e.clipboardData.getData('text/plain') : '';
    if (typeof e.preventDefault === 'function') e.preventDefault();
    if (isHeadless(rows.get(pane))) {
      const view = viewOf(pane);
      if (view && view.promptInput) {
        view.promptInput.value = String(view.promptInput.value || '') + text;
        focusOn(view.promptInput);
      }
      return;
    }
    if (text) sendText(pane, text.replace(/\r?\n/g, '\r'));
  });

  const floor = {
    paint, onEvent, seedNotes, tiles, detachAll, dropped, release, adopt, fillRow, openRow, putRow, canShow, setSize, hold,
    setWidth: (w) => setSize(w),
    gridOf: (pane) => grids.get(pane) || null,
    setTasks: takeTasks,
    // tasks is the task list the rail tree is built from.
    tasks: () => tasks,
    // folded is the set of rail group keys that are folded.
    folded: () => folds,
    toggleGroup,
    // isGone is true for an agent the floor knows has left.
    isGone: (pane) => looked.has(pane),
    askKill,
    // killPending is the agent whose stop waits for its confirm, or ''.
    killPending: () => killAsk,
    stopKill,
    // editing is {pane, where} while a label is renamed, or null.
    editing: () => editing,
    startRename,
    endRename,
    rename,
    selectedPane,
    // want puts pane in a window with the keys once the list holds it.
    want: (pane) => {
      want = pane || '';
      if (want && rows.has(want)) paint(lastList);
    },
    seenRecent,
    typing,
    // keys is 'window' or 'rail': who has the keys now.
    keys: () => (typing() ? 'window' : 'rail'),
    // railSelected is the pane id of the selected row while the rail has
    // the keys, or ''.
    railSelected: () => (typing() ? '' : railSel),
    // windowOf is the window number that shows pane, 1 first, or 0.
    windowOf: (pane) => {
      const i = windowSlots().indexOf(pane);
      return i >= 0 && canShow() ? i + 1 : 0;
    },
    // selection is the panes the shown windows hold, in slot order.
    selection: () => livePanes(shown(tiles, n)),
    // settled is a promise that settles once every detach on its way is
    // answered.
    settled: () => Promise.all([...detaching]),
    // holding is the panes the windows hold an attach for, or have one on
    // its way.
    holding: () => [...attached],
    // reattach attaches panes again once after settles, each that a window
    // still holds, so a view-only attach that raced the window's own never
    // leaves the window unable to type.
    reattach: (panes, after) => {
      Promise.resolve(after).catch(() => {}).then(() => {
        for (const pane of panes || []) if (attached.has(pane)) attach(pane);
      });
    },
  };
  // A move to a screen with another pixel ratio draws every window again.
  const watchRatio = () => {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return;
    const mq = window.matchMedia('(resolution: ' + dpr() + 'dppx)');
    if (!mq || typeof mq.addEventListener !== 'function') return;
    mq.addEventListener('change', () => {
      for (const v of views) {
        v.fresh = true;
        schedule(v);
      }
      watchRatio();
    }, { once: true });
  };
  watchRatio();
  setFloor(floor);

  paint(floorState.panes);
  return floor;
}

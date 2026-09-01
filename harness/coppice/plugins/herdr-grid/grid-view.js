// The grid on the page. A click on a roster row sets the selection to
// that pane. A click on a tile opens its pane. The view asks the floor to
// watch the first nine open panes and draws each picture the floor posts.
// A slot says when its picture is not live: the pane is closed, the floor
// could not show it, or the picture has not come yet.
import { connect, boot, stateColor } from '../_lib/view.js';
import { drawPicture } from '../_lib/picture.js';
import { slots, watched, shape, rosterRows } from './grid.js';

const WAITING = 'Open this view from the floor page. It shows what the floor page sends it.';
const NOT_LIVE = 'Nine panes at most show live. Click to open this one.';
const CLOSED = 'Closed.';
const REFUSED = 'The floor could not show this pane live.';
const NO_PICTURE = 'Waiting for the picture.';

function make(tag, className, text) {
  const el = document.createElement(tag);
  if (className) el.className = className;
  if (text !== undefined) el.textContent = text;
  return el;
}

export function mountGrid(status, roster, tiles, view) {
  status.textContent = WAITING;
  const pics = new Map();
  const canvases = new Map();
  let asked = '';
  let live = new Set();
  let refused = new Set();
  let seen = false;
  const render = () => {
    seen = true;
    const panes = view.panes();
    const ids = slots(panes);
    const want = watched(panes);
    const key = want.join(',');
    if (key !== asked) {
      asked = key;
      view.watch(want);
    }
    const sel = new Set(view.sel());
    roster.textContent = '';
    for (const row of rosterRows(panes)) {
      const li = make('li', sel.has(row.id) ? 'sel' : '');
      const word = make('span', 'word', row.word);
      word.style.color = stateColor(row.word);
      li.append(make('span', '', row.label), word);
      li.addEventListener('click', () => view.setSel([row.id]));
      roster.append(li);
    }
    const { cols } = shape(ids.length);
    tiles.style.gridTemplateColumns = 'repeat(' + Math.max(1, cols) + ', minmax(0, 1fr))';
    tiles.textContent = '';
    canvases.clear();
    const byId = new Map(panes.map((p) => [p.id, p]));
    for (const id of ids) {
      const pane = byId.get(id) || {};
      const slot = make('section', 'slot');
      slot.dataset.pane = id;
      slot.append(make('span', 'head', pane.label || id));
      if (pane.closed) {
        slot.append(make('p', '', CLOSED));
      } else if (!want.includes(id)) {
        slot.append(make('p', '', NOT_LIVE));
      } else if (refused.has(id)) {
        slot.append(make('p', '', REFUSED));
      } else if (!live.has(id)) {
        slot.append(make('p', '', NO_PICTURE));
      } else {
        const canvas = make('canvas');
        canvases.set(id, canvas);
        slot.append(canvas);
      }
      slot.addEventListener('click', () => view.openPane(id));
      tiles.append(slot);
    }
    for (const [id, canvas] of canvases) drawPicture(canvas, pics.get(id) || null);
    status.textContent = ids.length ? '' : 'No panes yet.';
  };
  const onFrame = (pic) => {
    if (!live.has(pic.pane)) return;
    pics.set(pic.pane, pic);
    const canvas = canvases.get(pic.pane);
    if (canvas) drawPicture(canvas, pic);
  };
  // onWatch takes which panes the floor shows live. A picture of a pane no
  // longer live is dropped, so an old screen never reads as live.
  const onWatch = (st) => {
    live = new Set(st.live);
    refused = new Set(st.refused);
    for (const id of [...pics.keys()]) if (!live.has(id)) pics.delete(id);
    if (seen) render();
  };
  view.onData(render);
  view.onFrame(onFrame);
  view.onWatch(onWatch);
  return { render, onFrame, onWatch };
}

boot(() => mountGrid(
  document.getElementById('grid-status'),
  document.getElementById('grid-roster'),
  document.getElementById('grid-tiles'),
  connect(window),
));

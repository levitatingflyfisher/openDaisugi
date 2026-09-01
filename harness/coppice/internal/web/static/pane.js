import { applyFrame, gridToText, drawGrid } from './grid.js';
import { stateChip } from './chips.js';
import { mountRecord } from './record.js';

// One pane, full width. The phone watches and zooms; it never resizes, so
// attach carries no size, because the same pane is open in the operator's
// terminal and a resize from the phone would change what they are reading.
// Prompt is agent.prompt on a pane of either kind: the server defines that
// verb as send text, and a shell pane answers it too, so there is one
// command and no branch to get wrong.

export const KEYS = ['enter', 'esc', 'ctrl+c', 'tab'];

export function attachCommand(pane) {
  return { cmd: 'pane.attach', pane: pane.id };
}

export function promptCommand(pane, text) {
  return { cmd: 'agent.prompt', pane: pane.id, text };
}

export function steerCommand(pane, text) {
  return { cmd: 'pane.send_text', pane: pane.id, text, enter: true };
}

export function keysCommand(pane, key) {
  return { cmd: 'pane.send_keys', pane: pane.id, keys: [key] };
}

// stateOf reads the state off a coppice-server object. Both shapes that
// carry one are flat and use the same field names: a pane.list row and a
// state event both put state, source, harness, ts, and ask straight on the
// object, not nested under a wrapper. Read the nested shape here and
// st.state is undefined for every pane, the whole roster paints unknown,
// and Allow and Deny never appears on the pane the operator just tapped.
export function stateOf(row) {
  if (!row) return { state: 'unknown', source: '', harness: 'unknown', ts: 0, ask: null };
  return {
    state: row.state || 'unknown',
    source: row.source || '',
    harness: row.harness || 'unknown',
    ts: row.ts || 0,
    ask: row.ask || null,
  };
}

// askFor decides what the Allow and Deny box shows. Pure, so the decision
// the operator depends on is testable against a real pane.list row rather
// than only through the DOM.
export function askFor(st) {
  if (st.state !== 'blocked' || !st.ask) return { visible: false, summary: '', askId: '' };
  return {
    visible: true,
    summary: st.ask.summary || st.ask.tool || 'blocked',
    askId: st.ask.id || '',
  };
}

let current = null;   // { id }
let grid = null;
let scale = 1;
const pointers = new Map();
let pinchStart = 0;
let scaleStart = 1;

function repaint() {
  if (!grid) return;
  const canvas = document.getElementById('grid');
  drawGrid(canvas.getContext('2d'), grid, 14 * scale);
  document.getElementById('grid-text').textContent = gridToText(grid);
}

function setAsk(st) {
  const box = document.getElementById('ask');
  const chip = document.getElementById('pane-chip');
  const c = stateChip(st.state);
  chip.textContent = c.word;
  chip.className = c.cls;
  document.getElementById('pane-source').textContent = st.source ? 'source ' + st.source : '';
  const ask = askFor(st);
  document.getElementById('ask-summary').textContent = ask.summary;
  box.dataset.askId = ask.askId;
  box.hidden = !ask.visible;
}

// paintPane fills the header and the ask box from a pane.list row. info is
// undefined when the row is not known yet, and stateOf reads that the same
// way it reads a missing row: unknown, no ask.
function paintPane(paneId, info) {
  document.getElementById('pane-label').textContent = (info && info.label) || paneId;
  setAsk(stateOf(info));
}

async function answer(decision) {
  const box = document.getElementById('ask');
  const askId = box.dataset.askId;
  if (!askId) return;
  try {
    await window.coppice.api('/api/ask/answer', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ tool_use_id: askId, decision, reason: 'answered from the phone' }),
    });
    window.coppice.status(decision === 'allow' ? 'Allowed.' : 'Denied.');
    box.dataset.askId = '';
    box.hidden = true;
  } catch (e) {
    window.coppice.status(e.message);
  }
}

async function send(build) {
  const box = document.getElementById('text');
  const text = box.value.trim();
  if (!current || !text) return;
  const req = build(current, text);
  const { cmd, ...fields } = req;
  try {
    await window.coppice.rpc(cmd, fields);
    box.value = '';
    window.coppice.status('');
  } catch (e) {
    window.coppice.status(e.message);
  }
}

// close detaches whatever pane is open. Leaving a pane attached while the
// operator is on another screen keeps frames streaming to a phone that is
// not showing them, which costs the battery the whole feature exists to
// save.
export function close() {
  if (!current) return;
  const id = current.id;
  current = null;
  grid = null;
  window.coppice.rpc('pane.detach', { pane: id }).catch(() => {});
}

// detached clears current and the grid the way close does, but sends
// nothing: the socket that would have carried pane.detach is already gone.
// Call this when the socket closes. Without it, a reconnect while parked
// on a pane finds current still set to the pane it was showing, open()'s
// same-pane guard reads that as the same repeat route event it exists to
// ignore, and no pane.attach is ever sent on the new socket. The grid then
// stays frozen on the last frame before the drop until the operator routes
// away and back.
export function detached() {
  current = null;
  grid = null;
}

// release hands the open pane back to the floor. It clears current with
// no detach and returns the pane id and its grid, or null when no pane is
// open. The floor adopts the attach and the server never sees a change.
export function release() {
  if (!current) return null;
  const handed = { id: current.id, grid };
  current = null;
  grid = null;
  return handed;
}

// open shows one pane. seed is the grid the floor's tile held for this
// pane, when the floor hands its attach over: the picture starts from it
// and later delta frames apply to it. The attach is still sent, and the
// server answers already-attached with the pump still running, so no
// detach and attach pair for one pane ever meets on the wire.
export async function open(paneId, seed) {
  if (current && current.id === paneId) return;   // a repeat route event must not blank the grid
  if (current) close();
  current = { id: paneId };
  grid = seed || null;
  scale = 1;
  const info = (window.coppice.state.panes || []).find((p) => p.id === paneId);
  paintPane(paneId, info);
  repaint();
  const req = attachCommand(current);
  const { cmd, ...fields } = req;
  try {
    await window.coppice.rpc(cmd, fields);
  } catch (e) {
    // A failed attach must not latch current: the socket may still be
    // connecting, and the next coppice:open is the retry.
    current = null;
    window.coppice.status(e.message);
  }
}

export function onEvent(msg) {
  if (msg.event === 'frame' && current && msg.pane === current.id) {
    grid = applyFrame(grid, msg);
    repaint();
    return;
  }
  if (msg.event === 'state') {
    // A state event is already flat. Wrapping it would break stateOf the
    // same way reading pane.list as nested does.
    if (current && msg.pane === current.id) setAsk(stateOf(msg));
    window.dispatchEvent(new CustomEvent('coppice:state'));
  }
}

export function mountPane() {
  document.getElementById('prompt').addEventListener('click', () => send(promptCommand));
  document.getElementById('steer').addEventListener('click', () => send(steerCommand));
  document.getElementById('allow').addEventListener('click', () => answer('allow'));
  document.getElementById('deny').addEventListener('click', () => answer('deny'));
  mountRecord();

  const drawer = document.getElementById('keys');
  drawer.replaceChildren(...KEYS.map((key) => {
    const b = document.createElement('button');
    b.textContent = key;
    b.addEventListener('click', () => {
      if (!current) return;
      const { cmd, ...fields } = keysCommand(current, key);
      window.coppice.rpc(cmd, fields).catch((e) => window.coppice.status(e.message));
    });
    return b;
  }));
  document.getElementById('keys-toggle').addEventListener('click', () => { drawer.hidden = !drawer.hidden; });

  // Pinch to zoom the grid. The pane keeps its size on the server; only the
  // picture changes.
  const wrap = document.getElementById('canvas-wrap');
  wrap.addEventListener('pointerdown', (e) => {
    pointers.set(e.pointerId, e);
    if (pointers.size === 2) {
      const [a, b] = [...pointers.values()];
      pinchStart = Math.hypot(a.clientX - b.clientX, a.clientY - b.clientY);
      scaleStart = scale;
    }
  });
  wrap.addEventListener('pointermove', (e) => {
    if (!pointers.has(e.pointerId)) return;
    pointers.set(e.pointerId, e);
    if (pointers.size === 2 && pinchStart > 0) {
      const [a, b] = [...pointers.values()];
      const now = Math.hypot(a.clientX - b.clientX, a.clientY - b.clientY);
      scale = Math.min(3, Math.max(0.4, scaleStart * (now / pinchStart)));
      repaint();
    }
  });
  const drop = (e) => { pointers.delete(e.pointerId); if (pointers.size < 2) pinchStart = 0; };
  wrap.addEventListener('pointerup', drop);
  wrap.addEventListener('pointercancel', drop);

  // The route change is app.js's to drive: it decides whether the floor
  // hands this pane over or the pane screen attaches on its own.

  // The socket is not open yet when boot() dispatches the first route
  // event, so a pane reached by a cold deep link never attaches from that
  // event alone. Once the socket opens, retry the attach for whatever pane
  // the hash still names.
  window.addEventListener('coppice:open', () => {
    const r = window.coppice.route(location.hash);
    if (r.screen === 'pane') open(r.pane);
  });

  // A pane reached by a cold deep link has no row to read until the first
  // pane.list lands. Once it does, repaint the header and the ask box from
  // it, which is the only way that pane ever learns it is blocked.
  window.addEventListener('coppice:panes', () => {
    if (!current) return;
    const info = (window.coppice.state.panes || []).find((p) => p.id === current.id);
    if (info) paintPane(current.id, info);
  });
}

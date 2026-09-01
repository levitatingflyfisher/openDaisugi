// The shell: one websocket, one request map, four screens. Every screen is a
// section in index.html. Routing is the hash, so the back button works and a
// notification can link straight at a pane. The roster route shows the
// floor: the rail of panes plus as many live tiles as the width allows.

import { mountRoster } from './roster.js';
import { mountFloor } from './floor.js';
import { mountPane, onEvent as paneEvent, detached, open as openPane, close as closePane, release as releasePane } from './pane.js';
import { mountNew } from './newpane.js';
import { mountSettings, tokenFromHash, wsUrl } from './settings.js';

export const WS_SUBPROTOCOL = 'daisugi.v1';
export const WS_BEARER_PREFIX = 'daisugi.bearer.';

// A browser cannot set headers on a WebSocket, so the token rides a second
// subprotocol offer. The server negotiates the first name only.
export function bearerProtocols(token) {
  return [WS_SUBPROTOCOL, WS_BEARER_PREFIX + token];
}

// route turns a location hash into a screen and its argument.
export function route(hash) {
  const h = (hash || '').replace(/^#/, '');
  if (h.startsWith('/pane/')) return { screen: 'pane', pane: decodeURIComponent(h.slice('/pane/'.length)) };
  if (h === '/new') return { screen: 'new' };
  if (h === '/settings') return { screen: 'settings' };
  return { screen: 'roster' };
}

// floor is the mounted floor page once boot() has run.
let floor = null;

const state = {
  token: null,
  ws: null,
  seq: 0,
  pending: new Map(),
  panes: [],
  onEvent: null,
};

function status(msg) {
  const el = document.getElementById('status');
  if (el) el.textContent = msg || '';
}

// rpc sends one request and resolves with the reply that echoes its id.
function rpc(cmd, fields = {}) {
  return new Promise((resolve, reject) => {
    if (!state.ws || state.ws.readyState !== WebSocket.OPEN) {
      reject(new Error('Not connected. Check the token in Settings.'));
      return;
    }
    const id = 'w' + ++state.seq;
    const timer = setTimeout(() => {
      if (state.pending.delete(id)) {
        reject(new Error('The server did not answer in 15 s. Check that coppice-server is running, then try again.'));
      }
    }, 15000);
    state.pending.set(id, { resolve, reject, timer });
    state.ws.send(JSON.stringify({ id, cmd, ...fields }));
  });
}

// rejectAllPending fails every rpc call still waiting on a reply. A closed
// socket will never deliver one, and without this a tap made just before
// the drop would sit silent for the full fifteen second timeout instead of
// failing the moment the close event already knows about.
function rejectAllPending(err) {
  for (const { reject, timer } of state.pending.values()) {
    clearTimeout(timer);
    reject(err);
  }
  state.pending.clear();
}

const RETRY_MIN_MS = 2000;
const RETRY_MAX_MS = 30000;
let retryMs = RETRY_MIN_MS;
let giveUp = false;
// A reconnect closes the live socket itself, on purpose, to replace it with
// one that carries the new token. That close must not read as a drop: the
// old token is not wrong, and scheduling a retry for it would race the new
// socket reconnect() is about to open. This flag is how afterClose tells
// the two apart.
let deliberateClose = false;

// The server closes an already open socket for one of two reasons, both
// coppice-server itself, never the phone. Reading the reason here means a
// dead upstream never has to wait on the token probe below: the socket was
// open, so the token was already good.
const UPSTREAM_CLOSE_REASONS = [
  'coppice-server is not reachable',
  'coppice-server closed the connection',
];

// nextRetry is the whole backoff decision, kept pure and apart from any
// timer so it can be tested on its own. reason is 'upstream' for a close
// naming one of the two reasons above, 'rejected' for a token the server no
// longer accepts, 'banned' for an address the server is refusing for a
// minute, or 'unknown' for anything else, including a network the phone
// cannot reach at all. It returns the delay to use now and the delay to
// remember for next time, or a stop marker when no further retry belongs
// on the schedule at all.
export function nextRetry(reason, retryMs) {
  if (reason === 'rejected') return { stop: true };
  if (reason === 'banned') return { delayMs: 60000, retryMs };
  return { delayMs: retryMs, retryMs: Math.min(RETRY_MAX_MS, retryMs * 2) };
}

// probeReason asks the server why the last close might have happened, for
// the one case the close event itself does not say. A handshake the
// server refuses outright reaches the browser as a blank reason, so the
// only way left to tell a bad token from a box that is simply unreachable
// is to ask the API directly. A plain network drop, the phone losing
// signal, also reaches this function, and costs one doomed fetch before
// the catch below turns it into 'unknown'. That fetch is accepted: without
// it there would be no way left to tell a refused handshake from a box
// that is simply out of reach.
async function probeReason() {
  let probe = null;
  try {
    probe = await fetch('/api/token/check', {
      headers: { Authorization: 'Bearer ' + state.token },
    });
  } catch {
    probe = null;   // the box is unreachable, which is a normal retry
  }
  if (probe && probe.status === 401) return 'rejected';
  if (probe && probe.status === 429) return 'banned';
  return 'unknown';
}

// afterClose decides whether reconnecting is worth doing, and what the
// operator should see while it happens. Retrying a rejected token every two
// seconds trips the server's own ban after three tries, and the operator's
// phone then spends a minute at 429 with "Reconnecting." on screen and
// nothing that says why. So ask once what happened, unless the close
// reason already answers that question.
async function afterClose(e) {
  if (deliberateClose) {
    deliberateClose = false;
    return;
  }
  const reason = e && UPSTREAM_CLOSE_REASONS.includes(e.reason) ? 'upstream' : await probeReason();
  const decision = nextRetry(reason, retryMs);
  if (decision.stop) {
    giveUp = true;
    status('That token is not accepted. Run coppice web token and scan the QR again.');
    location.hash = '#/settings';
    show('settings');
    window.dispatchEvent(new CustomEvent('coppice:route'));
    return;
  }
  status(reason === 'banned' ? 'Too many bad tokens. Waiting one minute.' : 'Reconnecting.');
  retryMs = decision.retryMs;
  setTimeout(connect, decision.delayMs);
}

function connect() {
  if (giveUp) return;
  if (state.ws && [WebSocket.CONNECTING, WebSocket.OPEN, WebSocket.CLOSING].includes(state.ws.readyState)) return;
  if (!state.token) { status('No token. Open Settings and paste one.'); return; }
  const ws = new WebSocket(wsUrl(location.origin), bearerProtocols(state.token));
  state.ws = ws;
  ws.addEventListener('open', () => {
    status('');
    rpc('events.subscribe', { panes: '*', kinds: ['state', 'layout'] }).catch(() => {});
    window.dispatchEvent(new CustomEvent('coppice:open'));
  });
  ws.addEventListener('message', (e) => {
    // A message is the one proof the link actually works end to end, not
    // just that the handshake completed. The backoff only means something
    // if it survives past the next handshake, so it resets here and not on
    // open.
    retryMs = RETRY_MIN_MS;
    let msg;
    try { msg = JSON.parse(e.data); } catch { return; }
    if (msg.id && state.pending.has(msg.id)) {
      const { resolve, reject, timer } = state.pending.get(msg.id);
      clearTimeout(timer);
      state.pending.delete(msg.id);
      if (msg.ok === false) reject(new Error((msg.error && msg.error.message) || 'refused'));
      else resolve(msg.result || {});
      return;
    }
    if (state.onEvent) state.onEvent(msg);
  });
  ws.addEventListener('close', (e) => {
    // A deliberate close from reconnect() replaces state.ws with a new
    // socket before this socket's own close event arrives. Checking
    // identity here keeps that late event from nulling the socket, or the
    // pane, that already replaced it.
    if (state.ws === ws) {
      state.ws = null;
      detached();
      if (floor) floor.dropped();
    }
    rejectAllPending(new Error('the connection closed'));
    afterClose(e);
  });
}

// reconnect is what a freshly saved token calls. It replaces the token the
// socket dropped, lifts the stop latch that a rejected token set, and opens
// a new socket right away, so the operator never has to reload the page by
// hand to use a token they just pasted. A socket that is still CONNECTING
// or OPEN under the old token has to be closed first: connect()'s own
// reentry guard refuses to open a second socket while one is live, so
// without this a fresh token saved while still connected would report
// Reconnecting and do nothing.
export function reconnect(token) {
  if (state.ws && [WebSocket.CONNECTING, WebSocket.OPEN].includes(state.ws.readyState)) {
    deliberateClose = true;
    state.ws.close();
    state.ws = null;
    // The old socket's own close event arrives later, asynchronously, by
    // which time state.ws already names the new socket, so its listener's
    // own identity check will skip clearing pane state. Clear it here
    // instead, synchronously, so a pane still open when the operator saves
    // a fresh token re-attaches on the new socket rather than sitting
    // behind the same-pane guard forever. The floor's attaches died with
    // the same socket, so it forgets them here too.
    detached();
    if (floor) floor.dropped();
  }
  state.token = token;
  giveUp = false;
  connect();
}

async function api(path, options = {}) {
  const headers = Object.assign({ Authorization: 'Bearer ' + state.token }, options.headers || {});
  const resp = await fetch(path, Object.assign({}, options, { headers }));
  const body = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(body.error || ('HTTP ' + resp.status));
  return body;
}

function show(name) {
  for (const s of ['roster', 'pane', 'new', 'settings']) {
    document.getElementById('screen-' + s).hidden = s !== name;
  }
  document.getElementById('back').hidden = name === 'roster';
  // A button pointed at the screen it already opened does nothing useful.
  document.getElementById('go-new').hidden = name === 'new';
  document.getElementById('go-settings').hidden = name === 'settings';
}

// window and the DOM are both absent under a plain Node import, so this
// stays out of module scope. Guarding it is what lets route, bearerProtocols
// and nextRetry, the pure functions in this file, be imported and tested on
// their own.
if (typeof window !== 'undefined') {
  window.coppice = { rpc, api, state, show, status, route, connect, reconnect };
}

function boot() {
  const scanned = tokenFromHash(location.hash);
  if (scanned) {
    localStorage.setItem('coppice.token', scanned);
    history.replaceState(null, '', '#/roster');
  }
  state.token = localStorage.getItem('coppice.token');

  document.getElementById('back').addEventListener('click', () => { location.hash = '#/roster'; });
  document.getElementById('go-new').addEventListener('click', () => { location.hash = '#/new'; });
  document.getElementById('go-settings').addEventListener('click', () => { location.hash = '#/settings'; });
  window.addEventListener('hashchange', () => window.dispatchEvent(new CustomEvent('coppice:route')));

  if ('serviceWorker' in navigator) {
    navigator.serviceWorker.register('/sw.js', { type: 'module' }).catch((err) => {
      console.warn('coppice: the service worker did not register', err);
      status('No offline shell. The app still works while you have a signal.');
    });
  }

  // Listeners on coppice:route fire in the order they were added. The
  // screen switch goes first, so every later listener reads the right
  // screen as shown or hidden.
  window.addEventListener('coppice:route', () => {
    const r = route(location.hash);
    show(r.screen);
    document.getElementById('title').textContent =
      { roster: 'Floor', pane: 'Pane', new: 'New pane', settings: 'Settings' }[r.screen];
  });

  mountRoster();
  floor = mountFloor(document.getElementById('screen-roster'), {
    panes: state.panes, width: window.innerWidth, rpc, api, status,
  });
  mountPane();
  // The floor and the pane screen share one socket, and the server runs
  // each request on its own goroutine, so a detach and an attach for one
  // pane may land in either order. The two screens hand an attach over
  // instead: leaving the floor for a pane a tile holds, the floor releases
  // that pane with no message and detaches the rest, and the pane screen
  // opens from the tile's grid; returning, the pane screen releases and the
  // floor adopts before it paints. A route to any other screen closes
  // both.
  window.addEventListener('coppice:route', () => {
    const r = route(location.hash);
    if (r.screen === 'pane') {
      const seed = floor.release(r.pane);
      floor.detachAll();
      openPane(r.pane, seed);
      return;
    }
    if (r.screen === 'roster') {
      const handed = releasePane();
      if (handed) floor.adopt(handed.id, handed.grid);
      floor.paint(state.panes);
      return;
    }
    closePane();
    floor.detachAll();
  });
  mountNew();
  mountSettings();
  window.addEventListener('coppice:panes', () => floor.paint(state.panes));
  state.onEvent = (msg) => {
    paneEvent(msg);
    floor.onEvent(msg);
  };

  connect();
  window.dispatchEvent(new CustomEvent('coppice:route'));
}

if (typeof window !== 'undefined') {
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot);
  else boot();
}

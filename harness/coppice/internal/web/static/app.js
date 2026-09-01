// The shell: one websocket, one request map, four screens. Every screen is a
// section in index.html. Routing is the hash, so the back button works and a
// notification can link straight at a pane. The roster route shows the
// floor: the rail of panes plus as many live tiles as the width allows.
// Views and the journal open over part of the floor, never as a screen of
// their own; an old #/view link opens that overlay.
// On a phone the roster route is the overview, and a pane route slides that
// agent's sheet in over it; the overview stays drawn and live under it.

import { mountRoster } from './roster.js';
import { mountFloor, endedMessage } from './floor.js';
import { mountPane, onEvent as paneEvent, detached, open as openPane, close as closePane, release as releasePane } from './pane.js';
import { mountNew, mountNewButton } from './newpane.js';
import { mountSettings, tokenFromHash, wsUrl } from './settings.js';
import { isPhone, overviewShown, mountSheet, phoneAfter, keyboardInset } from './dock.js';
import { mountViews, mountViewHost, parseView } from './views.js';
import { MESSAGES, toMessage, fixButtons, openShell, shellCwd } from './messages.js';
import { mountFacts } from './stackbar.js';
import { mountOverlay, escCloses, JOURNAL } from './overlay.js';
import { retryVoice } from './record.js';

export const WS_SUBPROTOCOL = 'daisugi.v1';
export const WS_BEARER_PREFIX = 'daisugi.bearer.';

// A browser cannot set headers on a WebSocket, so the token rides a second
// subprotocol offer. The server negotiates the first name only.
export function bearerProtocols(token) {
  return [WS_SUBPROTOCOL, WS_BEARER_PREFIX + token];
}

// route turns a location hash into a screen and its argument. A pane
// route may carry ?ask=<id>, the ask a lock-screen card lands on.
export function route(hash) {
  const h = (hash || '').replace(/^#/, '');
  if (h.startsWith('/pane/')) {
    const rest = h.slice('/pane/'.length);
    const q = rest.indexOf('?');
    // A pane id with a broken escape cannot name a pane, so the route
    // falls back to the roster.
    let pane;
    try {
      pane = decodeURIComponent(q < 0 ? rest : rest.slice(0, q));
    } catch {
      return { screen: 'roster' };
    }
    const r = { screen: 'pane', pane };
    const ask = q < 0 ? '' : new URLSearchParams(rest.slice(q + 1)).get('ask');
    if (ask) r.ask = ask;
    return r;
  }
  if (h === '/new') return { screen: 'new' };
  if (h === '/settings') return { screen: 'settings' };
  if (h.startsWith('/view/') && h.length > '/view/'.length) return parseView(h.slice('/view/'.length));
  return { screen: 'roster' };
}

// floor is the mounted floor page once boot() has run.
let floor = null;
// phone is true while the page is a phone. boot() and a resize set it.
let phone = false;
// viewHostRef is the mounted view host, or null before boot runs. The
// socket's close drops its attaches the way it drops the floor's.
let viewHostRef = null;

const state = {
  token: null,
  ws: null,
  seq: 0,
  pending: new Map(),
  panes: [],
  onEvent: null,
  // me is the name the token carries, "" for the operator's own token.
  me: '',
};

// statusSeq counts status lines, so a timed clear never clears a later one.
let statusSeq = 0;

// status shows one message on the status line, with a button for each fix
// it carries. msg is a message, an error or a string. A string or an
// error that names a command gets a button that types it in a shell.
function status(msg) {
  const m = toMessage(msg);
  const el = document.getElementById('status');
  if (el) el.textContent = m.text;
  const fixes = document.getElementById('status-fix');
  if (fixes) {
    fixes.textContent = '';
    fixes.append(...fixButtons(m, fix));
    fixes.hidden = m.actions.length === 0;
  }
  // A line that clears by itself goes after its time, unless another
  // line took its place first.
  const after = msg && typeof msg === 'object' && Number.isInteger(msg.clearAfter) ? msg.clearAfter : 0;
  if (after > 0 && el) {
    const shown = ++statusSeq;
    setTimeout(() => { if (shown === statusSeq) status(''); }, after);
  } else {
    statusSeq += 1;
  }
}

// setOffline dims the last picture while the page cannot reach the box.
function setOffline(on) {
  const b = typeof document !== 'undefined' ? document.body : null;
  if (b && b.classList && typeof b.classList.toggle === 'function') b.classList.toggle('offline', on);
}

// withFix is an error that carries its message's fix.
function withFix(m) {
  const e = new Error(m.text);
  e.fix = m;
  return e;
}

// rpc sends one request and resolves with the reply that echoes its id.
function rpc(cmd, fields = {}) {
  return new Promise((resolve, reject) => {
    if (!state.ws || state.ws.readyState !== WebSocket.OPEN) {
      reject(withFix(MESSAGES.notConnected()));
      return;
    }
    const id = 'w' + ++state.seq;
    const timer = setTimeout(() => {
      if (state.pending.delete(id)) {
        reject(withFix(MESSAGES.timeout()));
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
    status(MESSAGES.tokenRejected());
    location.hash = '#/settings';
    show('settings');
    window.dispatchEvent(new CustomEvent('coppice:route'));
    return;
  }
  status(reason === 'banned' ? MESSAGES.banned() : MESSAGES.reconnecting());
  setOffline(true);
  retryMs = decision.retryMs;
  setTimeout(connect, decision.delayMs);
}

function connect() {
  if (giveUp) return;
  if (state.ws && [WebSocket.CONNECTING, WebSocket.OPEN, WebSocket.CLOSING].includes(state.ws.readyState)) return;
  if (!state.token) { status(MESSAGES.noToken()); return; }
  const ws = new WebSocket(wsUrl(location.origin), bearerProtocols(state.token));
  state.ws = ws;
  ws.addEventListener('open', () => {
    status('');
    setOffline(false);
    rpc('events.subscribe', { panes: '*', kinds: ['state', 'layout'] }).catch(() => {});
    // Notes come on their own subscribe, so a server that has none still
    // sends state.
    rpc('events.subscribe', { panes: '*', kinds: ['note'] }).catch(() => {});
    // Presence says who else has a pane open. It comes on its own
    // subscribe too.
    rpc('events.subscribe', { panes: '*', kinds: ['presence'] }).catch(() => {});
    rpc('floor.notes', {}).then((r) => { if (floor) floor.seedNotes(r.notes); }).catch(() => {});
    // The page leaves its own name off the tiles it has open.
    api('/api/token/check').then((r) => {
      state.me = r && typeof r.name === 'string' ? r.name : '';
      if (floor) floor.paint(state.panes);
    }).catch(() => {});
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
      if (msg.ok === false) {
        // The code rides along, so a caller can tell a pane that ended
        // from any other refusal.
        const err = new Error((msg.error && msg.error.message) || 'refused');
        err.code = (msg.error && msg.error.code) || 'refused';
        reject(err);
      }
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
      if (viewHostRef) viewHostRef.dropped();
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
    if (viewHostRef) viewHostRef.dropped();
  }
  state.token = token;
  giveUp = false;
  connect();
}

async function api(path, options = {}) {
  const headers = Object.assign({ Authorization: 'Bearer ' + state.token }, options.headers || {});
  const resp = await fetch(path, Object.assign({}, options, { headers }));
  const body = await resp.json().catch(() => ({}));
  if (!resp.ok) {
    const err = new Error(body.error || ('HTTP ' + resp.status));
    err.status = resp.status;
    throw err;
  }
  return body;
}

// sheetPane is the agent whose sheet is open, or '' with none.
let sheetPane = '';

// sheetDialog makes an open sheet a modal dialog: the overview and the
// header under it take no focus and no clicks, and the focus starts on the
// sheet's back control. The edge beside the sheet stays live. On close the
// focus goes back to the agent's row.
function sheetDialog(sheet, paneEl) {
  const setInert = (id, on) => {
    const el = document.getElementById(id);
    if (!el) return;
    el.inert = on;
    if (typeof el.setAttribute === 'function') {
      if (on) el.setAttribute('inert', '');
      else if (typeof el.removeAttribute === 'function') el.removeAttribute('inert');
    }
  };
  const focus = (el) => { if (el && typeof el.focus === 'function') el.focus(); };
  if (paneEl && typeof paneEl.setAttribute === 'function') {
    if (sheet) {
      paneEl.setAttribute('role', 'dialog');
      paneEl.setAttribute('aria-modal', 'true');
      paneEl.setAttribute('aria-labelledby', 'pane-label');
    } else if (typeof paneEl.removeAttribute === 'function') {
      paneEl.removeAttribute('role');
      paneEl.removeAttribute('aria-modal');
      paneEl.removeAttribute('aria-labelledby');
    }
  }
  setInert('screen-roster', sheet);
  setInert('bar', sheet);
  const now = sheet ? route(location.hash).pane || '' : '';
  if (now && now !== sheetPane) focus(document.getElementById('sheet-back'));
  if (!now && sheetPane) {
    const sel = '[data-row="' + String(sheetPane).replace(/["\\]/g, '') + '"]';
    const find = (id) => {
      const el = document.getElementById(id);
      return el && typeof el.querySelector === 'function' ? el.querySelector(sel) : null;
    };
    focus(find('needs') || find('roster'));
  }
  sheetPane = now;
}

// show draws one screen. On a phone a pane is a sheet over the overview,
// so the overview stays drawn under it, and the sheet's own back control
// and the edge of overview beside it take the header's Back.
function show(name) {
  const sheet = phone && name === 'pane';
  for (const s of ['roster', 'pane', 'new', 'settings']) {
    document.getElementById('screen-' + s).hidden = s !== name && !(sheet && s === 'roster');
  }
  const edge = document.getElementById('sheet-edge');
  if (edge) edge.hidden = !sheet;
  const body = typeof document !== 'undefined' ? document.body : null;
  if (body && body.classList && typeof body.classList.toggle === 'function') {
    body.classList.toggle('phone', phone);
    body.classList.toggle('sheet-open', sheet);
  }
  // The sheet covers the status line, so the line moves into the sheet,
  // above its keys and bottom bar, while the sheet is open.
  const row = document.getElementById('status-row');
  const paneEl = document.getElementById('screen-pane');
  if (body && row && paneEl && typeof body.insertBefore === 'function') {
    if (sheet && row.parentNode !== paneEl) paneEl.insertBefore(row, document.getElementById('keys'));
    else if (!sheet && row.parentNode === paneEl) body.insertBefore(row, document.getElementById('screen-roster'));
  }
  sheetDialog(sheet, paneEl);
  document.getElementById('back').hidden = name === 'roster' || sheet;
  // A button pointed at the screen it already opened does nothing useful.
  document.getElementById('go-new').hidden = name === 'new';
  const menuBtn = document.getElementById('new-menu-btn');
  if (menuBtn) menuBtn.hidden = name === 'new';
  document.getElementById('go-settings').hidden = name === 'settings';
}

// window and the DOM are both absent under a plain Node import, so this
// stays out of module scope. Guarding it is what lets route, bearerProtocols
// and nextRetry, the pure functions in this file, be imported and tested on
// their own.
// fix runs one action a message carries. Every kind here does what its
// button says on this page, or says why it cannot.
async function fix(action) {
  const kind = action && action.kind;
  const socketUp = () => Boolean(state.ws && state.ws.readyState === WebSocket.OPEN);
  const refresh = () => window.dispatchEvent(new CustomEvent('coppice:refresh'));
  try {
    if (kind === 'reconnect') {
      retryMs = RETRY_MIN_MS;
      giveUp = false;
      if (socketUp()) {
        status('');
        refresh();
      } else {
        status('Connecting.');
        connect();
      }
    } else if (kind === 'settings') {
      location.hash = '#/settings';
    } else if (kind === 'voice') {
      status(await retryVoice(api));
    } else if (kind === 'open' && action.pane) {
      // Show the pane, so its question is in view.
      const want = '#/pane/' + encodeURIComponent(action.pane);
      if (floor && floor.canShow()) floor.want(action.pane);
      else if (location.hash !== want) location.hash = want;
      const box = document.getElementById('trust');
      const ask = document.getElementById('ask');
      const shown = box && !box.hidden ? box : ask && !ask.hidden ? ask : null;
      if (shown && typeof shown.scrollIntoView === 'function') shown.scrollIntoView({ block: 'center' });
    } else if (kind === 'new') {
      const b = document.getElementById('go-new');
      if (b && typeof b.click === 'function') b.click();
    } else if (kind === 'forget') {
      const r = await rpc('pane.forget', action.pane ? { pane: action.pane } : { ended: true });
      status(action.pane ? 'Forgot ' + (action.name || action.pane) + '.' : 'Forgot ' + (Number(r.forgot) || 0) + ' ended agents.');
      refresh();
    } else if (kind === 'resume') {
      const r = await rpc('pane.resume', { pane: action.pane });
      status(r.resumed === false ? 'Started ' + (action.name || 'it') + ' again. Its session could not resume.' : 'Resumed ' + (action.name || 'it') + '.');
      if (floor && r.pane && floor.canShow()) floor.want(r.pane);
      refresh();
    } else if (kind === 'shell') {
      status(await openShell(action, {
        rpc,
        connected: socketUp,
        cwd: async () => {
          const sel = floor ? floor.selectedPane() : '';
          let projects = [];
          if (!shellCwd(action, state.panes, sel, [])) {
            projects = await rpc('project.list').then((r) => r.projects).catch(() => []);
          }
          return shellCwd(action, state.panes, sel, projects);
        },
        made: (pane) => {
          if (floor && floor.canShow()) {
            floor.want(pane);
            if (route(location.hash).screen !== 'roster') location.hash = '#/roster';
          } else location.hash = '#/pane/' + encodeURIComponent(pane);
          refresh();
        },
      }));
    }
  } catch (e) {
    status(e);
  }
}

// overview is true while the overview is on screen: the floor, or a
// phone's sheet over it.
const overview = () => overviewShown(route(location.hash).screen, phone);

if (typeof window !== 'undefined') {
  window.coppice = { rpc, api, state, show, status, route, connect, reconnect, fix, overview, phone: () => phone };
}

// placeOverlay sizes the overlay to cover every window but the first
// column, so the rail and at least one window stay in sight. With one
// column of windows it leaves the left two fifths.
function placeOverlay(el) {
  const floorEl = document.getElementById('floor');
  if (!el || !floorEl || typeof floorEl.getBoundingClientRect !== 'function') return;
  const f = floorEl.getBoundingClientRect();
  const first = floorEl.querySelector('#tiles > .tile');
  const tilesEl = document.getElementById('tiles');
  const cols = tilesEl ? Number(tilesEl.dataset.cols) || 1 : 1;
  let keep = f.width * 0.4;
  if (first && cols > 1) keep = first.getBoundingClientRect().right - f.left + 4;
  el.style.setProperty('--overlay-w', Math.max(280, Math.round(f.width - keep)) + 'px');
}

function boot() {
  const scanned = tokenFromHash(location.hash);
  if (scanned) {
    localStorage.setItem('coppice.token', scanned);
    history.replaceState(null, '', '#/roster');
  }
  state.token = localStorage.getItem('coppice.token');
  phone = isPhone(window.innerWidth, window.innerHeight);

  document.getElementById('back').addEventListener('click', () => { location.hash = '#/roster'; });
  document.getElementById('go-settings').addEventListener('click', () => { location.hash = '#/settings'; });
  window.addEventListener('hashchange', () => window.dispatchEvent(new CustomEvent('coppice:route')));

  if ('serviceWorker' in navigator) {
    navigator.serviceWorker.register('/sw.js', { type: 'module' }).catch((err) => {
      console.warn('coppice: the service worker did not register', err);
      status('No offline shell. The app still works while you have a signal.');
    });
  }

  // A view route becomes the floor with that view open over it. This
  // listener goes first, so every later one reads the floor's route.
  let viewAsked = null;
  window.addEventListener('coppice:route', () => {
    const r = route(location.hash);
    if (r.screen !== 'view') return;
    viewAsked = r;
    history.replaceState(null, '', '#/roster');
  });
  // Listeners on coppice:route fire in the order they were added. The
  // screen switch goes first, so every later listener reads the right
  // screen as shown or hidden.
  window.addEventListener('coppice:route', () => {
    const r = route(location.hash);
    show(r.screen);
    document.getElementById('title').textContent = phone && r.screen === 'pane' ? 'Floor'
      : { roster: 'Floor', pane: 'Pane', new: 'New pane', settings: 'Settings', view: 'View' }[r.screen];
  });
  // pushed is true while the open sheet came from a tap on this page, so
  // the history holds the overview right under it. Going back then steps
  // back, and a phone's own back gesture does the same. A sheet a link
  // opened cold has nothing under it, so going back replaces it.
  let pushed = false;
  let lastScreen = '';
  window.addEventListener('coppice:route', () => {
    const now = route(location.hash).screen;
    pushed = now === 'pane' && (lastScreen === 'roster' || (lastScreen === 'pane' && pushed));
    lastScreen = now;
  });
  const goBack = () => {
    if (route(location.hash).screen !== 'pane') return;
    if (pushed && typeof history.back === 'function') {
      pushed = false;
      history.back();
      return;
    }
    history.replaceState(null, '', '#/roster');
    window.dispatchEvent(new CustomEvent('coppice:route'));
  };
  window.coppice.back = goBack;

  mountRoster();
  // The header row reads the floor facts every few seconds, and on the
  // same beat asks for a fresh pane list, since a gate report that keeps
  // an agent working changes its row's verdict and tokens but sends no
  // state event.
  const factsEl = document.getElementById('facts');
  let factsKey = '';
  const onFloor = overview;
  const factsRow = factsEl ? mountFacts(factsEl, {
    rpc,
    connected: () => Boolean(state.ws && state.ws.readyState === WebSocket.OPEN) && onFloor(),
    // The windows draw again only when the facts changed, so a quiet floor
    // does not rebuild its headers every beat.
    got: (f) => {
      const key = JSON.stringify(f);
      if (key === factsKey) return;
      factsKey = key;
      if (floor) floor.paint(state.panes);
    },
    tick: () => window.dispatchEvent(new CustomEvent('coppice:refresh')),
    rows: () => state.panes,
    onAction: fix,
    hintEl: document.getElementById('gate-hint'),
  }) : null;
  if (factsRow) {
    window.addEventListener('coppice:open', () => factsRow.read());
    // The rail and the header count from the same list, so they agree the
    // moment a new list lands.
    window.addEventListener('coppice:panes', () => factsRow.repaint());
    window.addEventListener('coppice:route', () => {
      factsEl.hidden = !onFloor() || factsEl.children.length === 0;
      if (onFloor()) factsRow.read();
      else factsRow.stop();
    });
  }
  // The minimap sits at the foot of the rail when the server lists it. It
  // runs in its own sandboxed frame with its own host. A dot click fills a
  // tile, and the minimap never watches a pane: a view-only attach would
  // stop the tiles from typing.
  let minimapOn = false;
  const minimapFrame = document.getElementById('minimap-frame');
  const minimap = mountViewHost(minimapFrame, {
    api,
    panes: () => state.panes,
    go: (hash) => { location.hash = hash; },
    replace: () => {},
    every: (fn, ms) => setInterval(fn, ms),
    cancel: (id) => clearInterval(id),
    sel: () => (floor ? floor.selection() : []),
    select: false,
    watch: false,
    open: (pane) => {
      if (floor && floor.canShow()) {
        floor.fillRow(pane);
        minimap.post();
      } else {
        location.hash = '#/pane/' + encodeURIComponent(pane);
      }
    },
  });
  const placeMinimap = () => {
    const on = minimapOn && overview();
    minimapFrame.hidden = !on;
    if (on) minimap.show('minimap', []);
    else minimap.hide();
  };
  // ringViews are the views whose manifest asks for the event ring, and
  // viewTitles their titles.
  let ringViews = new Set();
  let viewTitles = new Map();
  // colonyOn is true when the server lists the colony, which a phone's
  // overview then carries as a strip.
  let colonyOn = false;
  // overlay is the panel views and the journal open in; it is mounted
  // once the view hosts are.
  let overlay = null;
  const viewList = document.getElementById('view-list');
  const views = mountViews(viewList, {
    api,
    selection: () => (floor ? floor.selection() : []),
    // A view button opens the overlay in place; the address stays the
    // floor's and the history gains nothing.
    go: (hash) => {
      const r = route(hash);
      if (r.screen === 'view' && overlay) overlay.open(r.view, r.sel);
      else location.hash = hash;
    },
    // The minimap has its own place at the foot of the rail, so it gets no
    // button.
    skip: (v) => v.id === 'minimap',
    loaded: (list) => {
      minimapOn = list.some((v) => v.id === 'minimap');
      ringViews = new Set(list.filter((v) => v.ring === true).map((v) => v.id));
      viewTitles = new Map(list.map((v) => [v.id, v.title || v.id]));
      colonyOn = list.some((v) => v.id === 'colony');
      placeMinimap();
      if (overlay) overlay.routed();
      // The journal is the page's own, so it has a button whether or not
      // the server lists any view.
      const li = document.createElement('li');
      const b = document.createElement('button');
      b.className = 'ghost';
      b.dataset.view = JOURNAL;
      b.textContent = 'Journal';
      b.addEventListener('click', () => { if (overlay) overlay.open(JOURNAL, [], floor ? floor.selectedPane() : ''); });
      li.append(b);
      viewList.append(li);
      viewList.hidden = false;
    },
  });
  floor = mountFloor(document.getElementById('screen-roster'), {
    panes: state.panes, width: window.innerWidth, height: window.innerHeight, rpc, api, status, fix,
    facts: () => (factsRow ? factsRow.facts() : null),
    journal: (pane) => { if (overlay) overlay.open(JOURNAL, [], pane); },
    // A change in what the windows hold moves the open view's watch.
    changed: () => { if (viewHostRef) viewHostRef.floorChanged(); },
    get me() { return state.me; },
    // A push or a pop redraws the rail through the roster's own paint.
    rail: () => window.dispatchEvent(new CustomEvent('coppice:rail')),
  });
  mountPane();
  window.coppice.facts = () => (factsRow ? factsRow.facts() : null);
  window.coppice.journal = (pane) => { if (overlay) overlay.open(JOURNAL, [], pane); };
  // The floor and the pane screen share one socket, and the server runs
  // each request on its own goroutine, so a detach and an attach for one
  // pane may land in either order. The two screens hand an attach over
  // instead: leaving the floor for a pane a tile holds, the floor releases
  // that pane with no message and detaches the rest, and the pane screen
  // opens from the tile's grid; returning, the pane screen releases and the
  // floor adopts before it paints. A route to any other screen closes
  // both.
  // A view opens over the floor, so it shares the socket with the
  // windows. Its watch never touches a pane a window holds, and shows that
  // pane from the window's own grid. A pane it opens goes in a window and
  // the overlay closes, so the owner sees it.
  const openFromView = (pane) => {
    if (floor && floor.canShow()) {
      floor.fillRow(pane);
      if (overlay) overlay.close();
    } else {
      location.hash = '#/pane/' + encodeURIComponent(pane);
    }
  };
  const viewHost = mountViewHost(document.getElementById('view-frame'), {
    api,
    rpc,
    ring: (id) => ringViews.has(id),
    panes: () => state.panes,
    go: (hash) => { location.hash = hash; },
    // The selection a view sets stays with the overlay; the address stays
    // the floor's.
    replace: () => {},
    every: (fn, ms) => setInterval(fn, ms),
    cancel: (id) => clearInterval(id),
    held: () => (floor ? floor.holding() : []),
    picture: (pane) => (floor ? floor.gridOf(pane) : null),
    settled: () => (floor ? floor.settled() : Promise.resolve()),
    reattach: (panes, after) => { if (floor) floor.reattach(panes, after); },
    open: openFromView,
    close: () => { if (overlay) overlay.close(); },
  });
  viewHostRef = viewHost;
  // The colony pinned as a strip above the windows: its own host, with no
  // watch and no selection of its own.
  const stripHost = mountViewHost(document.getElementById('strip-frame'), {
    api,
    panes: () => state.panes,
    go: (hash) => { location.hash = hash; },
    replace: () => {},
    every: (fn, ms) => setInterval(fn, ms),
    cancel: (id) => clearInterval(id),
    sel: () => (floor ? floor.selection() : []),
    select: false,
    watch: false,
    open: (pane) => { if (floor && floor.canShow()) floor.fillRow(pane); else location.hash = '#/pane/' + encodeURIComponent(pane); },
  });
  // The strip sits above the windows, inside the floor. On a phone it
  // heads the overview.
  const strip = document.getElementById('strip');
  const floorBox = document.getElementById('floor');
  const tilesBox = document.getElementById('tiles');
  const railBox = document.getElementById('rail');
  const moveStrip = () => {
    if (!strip) return;
    if (phone && railBox && typeof railBox.insertBefore === 'function') {
      if (railBox.children && railBox.children[0] !== strip) railBox.insertBefore(strip, railBox.children[0] || null);
    } else if (floorBox && tilesBox && typeof floorBox.insertBefore === 'function') {
      floorBox.insertBefore(strip, tilesBox);
    }
  };
  moveStrip();
  const overlayEl = document.getElementById('overlay');
  overlay = mountOverlay({
    el: overlayEl,
    title: document.getElementById('overlay-title'),
    pin: document.getElementById('overlay-pin'),
    close: document.getElementById('overlay-close'),
    frame: document.getElementById('view-frame'),
    journal: document.getElementById('journal'),
    strip,
    unpin: document.getElementById('strip-unpin'),
    host: viewHost,
    stripHost,
    api,
    labelOf: (pane) => {
      const p = state.panes.find((x) => x && x.id === pane);
      return (p && p.label) || pane;
    },
    titleOf: (id) => viewTitles.get(id) || id,
    onFloor: overview,
    always: () => phone && colonyOn,
    fixes: (m) => fixButtons(m, fix),
    toMessage,
    storage: localStorage,
    every: (fn, ms) => setInterval(fn, ms),
    cancel: (id) => clearInterval(id),
    place: placeOverlay,
    relayout: () => { if (floor) floor.paint(state.panes); },
  });
  window.addEventListener('message', (e) => viewHost.onMessage(e));
  window.addEventListener('message', (e) => stripHost.onMessage(e));
  window.addEventListener('message', (e) => minimap.onMessage(e));
  window.addEventListener('coppice:panes', () => viewHost.post());
  window.addEventListener('coppice:panes', () => stripHost.post());
  window.addEventListener('coppice:panes', () => minimap.post());
  window.addEventListener('coppice:panes', () => overlay.panesChanged(state.panes));
  window.addEventListener('coppice:route', placeMinimap);
  window.addEventListener('coppice:route', () => overlay.routed());
  window.addEventListener('coppice:route', () => {
    if (!viewAsked) return;
    const v = viewAsked;
    viewAsked = null;
    overlay.open(v.view, v.sel);
  });
  // Esc closes the overlay while the keys are on the rail. In a window Esc
  // is the agent's.
  document.addEventListener('keydown', (e) => {
    if (!e || e.key !== 'Escape' || e.defaultPrevented) return;
    const t = e.target;
    if (t && ['INPUT', 'TEXTAREA', 'SELECT'].includes(t.tagName)) return;
    if (floor && floor.killPending()) return;
    if (!escCloses(overlay.state(), floor ? floor.keys() : 'rail')) return;
    if (typeof e.preventDefault === 'function') e.preventDefault();
    overlay.close();
  });
  const onRoute = (r) => {
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
  };
  // A view that watched panes hands back the wait for its detaches, and
  // the next screen attaches only after they are answered, so a late
  // detach never stops a tile or the pane screen. A newer route that came
  // while it waited wins.
  window.addEventListener('coppice:route', () => {
    const r = route(location.hash);
    const hash = location.hash;
    const gone = r.screen !== 'roster' ? viewHost.hide() : null;
    if (!gone) {
      onRoute(r);
      return;
    }
    // The floor holds every attach until the detaches are answered, so a
    // pane list or a minimap click while it waits sends nothing early.
    floor.hold(gone);
    gone.then(() => { if (location.hash === hash) onRoute(r); });
  });
  mountNew();
  // New starts the default harness near the selected agent, and the new
  // agent takes a window with the keys. With no window it opens on its
  // own screen.
  mountNewButton({
    rpc,
    status,
    near: () => floor.selectedPane(),
    made: (pane) => {
      if (floor.canShow()) {
        floor.want(pane);
        if (route(location.hash).screen !== 'roster') location.hash = '#/roster';
      } else location.hash = '#/pane/' + encodeURIComponent(pane);
      window.dispatchEvent(new CustomEvent('coppice:refresh'));
    },
    go: (hash) => { location.hash = hash; },
  });
  mountSettings();
  window.addEventListener('coppice:panes', () => floor.paint(state.panes));

  // The sheet goes back on a swipe right, a tap on the edge of overview
  // beside it, or its own back control.
  mountSheet(document.getElementById('screen-pane'), {
    edge: document.getElementById('sheet-edge'),
    body: () => document.getElementById('canvas-wrap'),
    back: goBack,
    on: () => phone,
  });
  const sheetBack = document.getElementById('sheet-back');
  if (sheetBack) sheetBack.addEventListener('click', () => { if (phone) goBack(); else location.hash = '#/roster'; });
  // An agent that ends while its sheet or screen is open takes the owner
  // back to the floor, with the line that says so and Resume and Forget.
  window.addEventListener('coppice:gone', (e) => {
    const d = e && e.detail;
    const r = route(location.hash);
    if (!d || r.screen !== 'pane' || r.pane !== d.pane) return;
    status(endedMessage(d.pane, d.label));
    if (phone) goBack();
    else location.hash = '#/roster';
    rpc('pane.list', { ended: true }).then((res) => {
      const rec = (Array.isArray(res && res.panes) ? res.panes : []).find((x) => x && x.id === d.pane);
      if (rec && Number.isInteger(rec.exit_code)) status(endedMessage(d.pane, rec.label || d.label, rec.exit_code));
    }).catch(() => {});
    window.dispatchEvent(new CustomEvent('coppice:refresh'));
  });
  // width and height are the page size the floor and the phone rule read.
  // A resize, from a turned phone or a narrowed window, updates them. A
  // page that stops or starts being a phone draws its screen again; a
  // sheet open when the page grows past a phone moves into a window.
  let width = window.innerWidth;
  let height = window.innerHeight;
  window.addEventListener('resize', () => {
    if (window.innerWidth === width && window.innerHeight === height) return;
    const oldWidth = width;
    width = window.innerWidth;
    height = window.innerHeight;
    const was = phone;
    const a = document.activeElement;
    const typing = Boolean(a && ['INPUT', 'TEXTAREA'].includes(a.tagName));
    phone = phoneAfter(was, { width, height, oldWidth, typing });
    // A keyboard that took height from a wide page leaves its windows be.
    if (phone === isPhone(width, height)) floor.setSize(width, height);
    overlay.place();
    if (was === phone) return;
    moveStrip();
    const r = route(location.hash);
    if (!phone && r.screen === 'pane' && floor.canShow()) {
      history.replaceState(null, '', '#/roster');
      window.dispatchEvent(new CustomEvent('coppice:route'));
      floor.fillRow(r.pane);
      return;
    }
    window.dispatchEvent(new CustomEvent('coppice:route'));
  });
  // A browser that does not resize the page for its on-screen keyboard
  // hides the sheet's bottom bar under it. The visual viewport says how
  // much it hides, and the sheet ends above that.
  const vv = window.visualViewport;
  if (vv && typeof vv.addEventListener === 'function') {
    const inset = () => {
      const root = document.documentElement;
      if (root && root.style && typeof root.style.setProperty === 'function') {
        root.style.setProperty('--kb', keyboardInset(window.innerHeight, vv) + 'px');
      }
    };
    vv.addEventListener('resize', inset);
    vv.addEventListener('scroll', inset);
  }
  // A view open over the overview closes when a sheet slides in, so the
  // sheet is what the owner sees.
  let sheetWas = false;
  window.addEventListener('coppice:route', () => {
    const now = phone && route(location.hash).screen === 'pane';
    if (now && !sheetWas && overlay.state().open) overlay.close();
    sheetWas = now;
  });
  state.onEvent = (msg) => {
    paneEvent(msg);
    floor.onEvent(msg);
    viewHost.onEvent(msg);
    overlay.onEvent(msg);
  };

  connect();
  if (state.token) views.load();
  window.dispatchEvent(new CustomEvent('coppice:route'));
}

if (typeof window !== 'undefined') {
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot);
  else boot();
}

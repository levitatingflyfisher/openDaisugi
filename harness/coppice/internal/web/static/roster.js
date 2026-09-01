import { stateOf, askFor } from './pane.js';
import { ORDER, stateChip } from './chips.js';

// The roster: every pane, blocked first, each with a word as well as a
// colour.

export { stateChip };

// floor is the mounted floor page, or null before mountFloor runs. A row
// click goes to a tile when the floor can show one, and to the pane
// screen when it cannot.
let floor = null;

export function setFloor(f) { floor = f; }

const canTile = () => floor !== null && floor.canShow();

export function sortPanes(panes) {
  return panes
    .map((p, i) => [p, i])
    .sort((a, b) => {
      const d = (ORDER[a[0].state] ?? 5) - (ORDER[b[0].state] ?? 5);
      return d !== 0 ? d : a[1] - b[1];
    })
    .map((e) => e[0]);
}

export function ageLabel(seconds) {
  const s = Math.max(0, Math.floor(seconds));
  if (s < 60) return s + 's';
  if (s < 3600) return Math.floor(s / 60) + 'm';
  if (s < 86400) return Math.floor(s / 3600) + 'h';
  return Math.floor(s / 86400) + 'd';
}

function card(pane) {
  const chip = stateChip(pane.state);
  const li = document.createElement('li');
  li.className = 'card';
  li.tabIndex = 0;
  li.dataset.pane = pane.id;
  li.dataset.row = pane.id;

  const row = document.createElement('div');
  row.className = 'row';
  const label = document.createElement('strong');
  label.textContent = pane.label || pane.id;
  const chipEl = document.createElement('span');
  chipEl.className = chip.cls;
  chipEl.textContent = chip.word;
  const harness = document.createElement('span');
  harness.className = 'muted';
  harness.textContent = pane.harness || 'unknown';
  const age = document.createElement('span');
  age.className = 'muted';
  age.textContent = pane.age === null ? '' : ageLabel(pane.age);
  row.append(label, chipEl, harness, age);
  li.append(row);

  if (pane.state === 'blocked' && pane.askSummary) {
    const ask = document.createElement('p');
    ask.className = 'ask';
    ask.textContent = pane.askSummary;
    li.append(ask);
  }
  // A click fills the focused tile. A ctrl click or a middle click opens a
  // new tile. With no floor the click opens the pane screen.
  li.addEventListener('click', (e) => {
    if (canTile()) {
      if (e && e.ctrlKey) floor.openRow(pane.id);
      else floor.fillRow(pane.id);
      return;
    }
    location.hash = '#/pane/' + encodeURIComponent(pane.id);
  });
  li.addEventListener('auxclick', (e) => {
    if (e && e.button === 1 && canTile()) floor.openRow(pane.id);
  });
  return li;
}

export function render(panes) {
  const list = document.getElementById('roster');
  const empty = document.getElementById('roster-empty');
  list.replaceChildren(...sortPanes(panes).map(card));
  empty.hidden = panes.length > 0;
}

// paneRows turns a flat pane.list row into what a card needs. The row's ts
// is the merged event's timestamp. A pane the state store has never seen
// has none, and its age stays blank rather than reading as zero seconds
// old.
export function paneRows(raw, nowSeconds) {
  return (raw || []).map((p) => {
    const st = stateOf(p);
    return {
      id: p.id,
      label: p.label || p.id,
      state: st.state,
      source: st.source,
      harness: st.harness,
      askSummary: askFor(st).summary,
      age: st.ts ? Math.max(0, nowSeconds - st.ts) : null,
    };
  });
}

// panesFrom reads the panes list off a pane.list result, and returns null
// when result itself is not a real object or carries no panes array.
// coppice-server always sets panes, to an empty array when there are none,
// so a missing or non-array panes is not "no panes yet"; it is a reply that
// cannot be read, and it must not paint as an empty roster. rpc() resolves
// a reply with no result key at all as {}, which would otherwise look
// exactly like a real, deliberately empty result.
export function panesFrom(result) {
  if (!result || typeof result !== 'object' || Array.isArray(result)) return null;
  if (!Array.isArray(result.panes)) return null;
  return result.panes;
}

export function mountRoster() {
  let raw = [];
  let ticker = null;

  const paint = () => { render(paneRows(raw, Date.now() / 1000)); };

  // setPanes is the one place window.coppice.state.panes changes. The pane
  // screen listens for coppice:panes to refresh its own header and ask box
  // once a fresh list lands, which is the only way a pane reached by a
  // cold deep link ever learns its real state.
  const setPanes = (panes) => {
    raw = panes;
    window.coppice.state.panes = panes;
    paint();
    window.dispatchEvent(new CustomEvent('coppice:panes'));
  };

  // firstPaint reads the plain HTTP endpoint, which answers before the
  // websocket handshake finishes. A refusal here is a real error and goes
  // to the status line, never to the empty banner.
  const firstPaint = async () => {
    try {
      const result = await window.coppice.api('/api/panes');
      const panes = panesFrom(result);
      if (panes === null) throw new Error('coppice-server sent a reply the phone could not read. Run coppice server status.');
      setPanes(panes);
    } catch (e) {
      window.coppice.status(e.message);
    }
  };

  // refresh asks the live socket. It only runs once the socket is open, so
  // a cold start never blames the operator's token for a handshake that is
  // still in progress.
  const refresh = async () => {
    try {
      const result = await window.coppice.rpc('pane.list');
      const panes = panesFrom(result);
      if (panes === null) throw new Error('coppice-server sent a reply the phone could not read. Run coppice server status.');
      setPanes(panes);
    } catch (e) {
      window.coppice.status(e.message);
    }
  };

  const socketOpen = () => window.coppice.state.ws && window.coppice.state.ws.readyState === WebSocket.OPEN;

  // Nothing else ticks the age text between refreshes. Thirty seconds
  // keeps a card honest without a round trip, and the interval stops the
  // moment the operator leaves the roster.
  const startTicker = () => { if (ticker === null) ticker = setInterval(paint, 30000); };
  const stopTicker = () => {
    if (ticker === null) return;
    clearInterval(ticker);
    ticker = null;
  };

  // A busy pane produces state events faster than a phone should ask for a
  // pane list. Coalesce them: one round trip every half second is plenty
  // for a roster a human is reading.
  let timer = null;
  const soon = () => {
    if (timer) return;
    timer = setTimeout(() => { timer = null; refresh(); }, 500);
  };

  window.addEventListener('coppice:open', refresh);
  window.addEventListener('coppice:route', () => {
    if (window.coppice.route(location.hash).screen !== 'roster') { stopTicker(); return; }
    startTicker();
    // With no token there is nothing to authenticate a request with.
    // connect() already left its own sentence on the status line; asking
    // the plain HTTP endpoint anyway would send "Bearer null" and cost the
    // phone's own address one of its three ban strikes for a mistake that
    // is not a bad token, it is no token at all.
    if (!window.coppice.state.token) return;
    if (socketOpen()) refresh();
    else firstPaint();
  });
  window.addEventListener('coppice:state', soon);
  return refresh;
}

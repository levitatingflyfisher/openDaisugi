// Views are pages that plugins add to the floor. The rail lists them. A
// view opens over part of the windows in a frame sandboxed with scripts
// only, so it runs in an opaque origin and holds no token. An old
// #/view/<id> link opens the same overlay. The floor page reads the
// panes, the tasks and the server's ring of recent state events with its
// own token and posts them into the frame, with the panes the tiles
// showed as sel, and the newest events it heard. A view may post back
// four things: open a pane, set sel, watch up to nine panes, or close. The
// floor checks each against the pane list and acts on nothing else.

import { mountWatch, WATCH_MAX } from './watch.js';
import { gridToText } from './grid.js';

export { WATCH_MAX };

// eventForView is a state event or a pane row as a view is given it. A
// blocked one whose ask a steward holds reads as working, with held still
// on it, so every view sorts, colours and counts it as the rail does.
export function eventForView(e) {
  if (!e || e.state !== 'blocked' || !e.held) return e;
  return { ...e, state: 'working' };
}

// forView is a pane list as a view is given it. See eventForView.
export function forView(panes) {
  return (Array.isArray(panes) ? panes : []).map(eventForView);
}

// viewHash is the route that opens view id with the panes in sel.
export function viewHash(id, sel) {
  const panes = (sel || []).filter(Boolean);
  const q = panes.length ? '?sel=' + panes.map(encodeURIComponent).join(',') : '';
  return '#/view/' + encodeURIComponent(id) + q;
}

// viewSrc is the frame address for view id. The selection is posted, not
// put in the address, so a new selection does not reload the view.
export function viewSrc(id) {
  return '/plugins/' + encodeURIComponent(id) + '/';
}

// viewAction reads one message a view posted. It is an open of a pane the
// list holds, a new sel cut down to panes the list holds, a watch of at
// most WATCH_MAX panes the list holds, a close, or null for anything else.
//
// A view may also post an answer to a card (answer), a request for a
// card's diff (card-diff) and an answer to a tree ask (tree-answer). Each
// is accepted only for an id in the last list the page sent that view:
// cards for the first two, tree.asks for the third.
export function viewAction(data, panes, cards, tree) {
  if (!data || typeof data !== 'object') return null;
  if (data.type === 'close') return { kind: 'close' };
  const cardIds = new Set((Array.isArray(cards) ? cards : []).map((c) => c && c.choice_id));
  if (data.type === 'answer') {
    if (typeof data.choice_id !== 'string' || !cardIds.has(data.choice_id)) return null;
    if (!['keep', 'drop', 'switch'].includes(data.outcome)) return null;
    const pick = typeof data.pick === 'string' ? data.pick : '';
    if (data.outcome === 'switch') {
      const card = cards.find((c) => c && c.choice_id === data.choice_id);
      const alts = Array.isArray(card && card.alternatives) ? card.alternatives : [];
      if (!pick || !alts.includes(pick)) return null;
    }
    return { kind: 'answer', choice_id: data.choice_id, outcome: data.outcome, pick: data.outcome === 'switch' ? pick : '' };
  }
  if (data.type === 'card-diff') {
    return typeof data.choice_id === 'string' && cardIds.has(data.choice_id)
      ? { kind: 'card-diff', choice_id: data.choice_id } : null;
  }
  if (data.type === 'tree-answer') {
    const asks = Array.isArray(tree && tree.asks) ? tree.asks : [];
    if (typeof data.ask_id !== 'string' || !asks.some((a) => a && a.ask_id === data.ask_id)) return null;
    if (data.decision !== 'allow' && data.decision !== 'deny') return null;
    return { kind: 'tree-answer', ask_id: data.ask_id, decision: data.decision };
  }
  const known = new Set((panes || []).map((p) => p.id));
  if (data.type === 'open-pane' && typeof data.pane === 'string' && known.has(data.pane)) {
    return { kind: 'open', pane: data.pane };
  }
  if (data.type === 'set-sel' && Array.isArray(data.sel)) {
    return { kind: 'sel', sel: data.sel.filter((p) => typeof p === 'string' && known.has(p)) };
  }
  if (data.type === 'watch' && Array.isArray(data.panes)) {
    const panes = [...new Set(data.panes.filter((p) => typeof p === 'string' && known.has(p)))];
    return { kind: 'watch', panes: panes.slice(0, WATCH_MAX) };
  }
  return null;
}

// parseView reads the part of a hash after #/view/: the view id and the
// selected panes.
export function parseView(rest) {
  const at = rest.indexOf('?');
  const id = decodeURIComponent(at < 0 ? rest : rest.slice(0, at));
  const query = at < 0 ? '' : rest.slice(at + 1);
  const raw = new URLSearchParams(query).get('sel') || '';
  const sel = raw.split(',').map((s) => s.trim()).filter(Boolean);
  return { screen: 'view', view: id, sel };
}

function make(tag, text) {
  const el = document.createElement(tag);
  if (text !== undefined) el.textContent = text;
  return el;
}

// mountViews fills list with one button per view the server names. A
// click opens that view with the panes selection() returns. No views, or
// a refusal, leaves the list hidden. loaded, when given, gets the list
// each time it is painted. skip, when given, names the views that get no
// button.
export function mountViews(list, { api, selection, go, loaded, skip }) {
  const paint = (views) => {
    list.textContent = '';
    const shown = skip ? views.filter((v) => !skip(v)) : views;
    for (const v of shown) {
      const li = make('li');
      const b = make('button', v.title || v.id);
      b.className = 'ghost';
      b.dataset.view = v.id;
      b.addEventListener('click', () => go(viewHash(v.id, selection())));
      li.append(b);
      list.append(li);
    }
    list.hidden = shown.length === 0;
    if (loaded) loaded(views);
  };
  list.hidden = true;
  const load = () => api('/api/views')
    .then((body) => paint(Array.isArray(body && body.views) ? body.views : []))
    .catch(() => paint([]));
  return { load, paint };
}

// EVENTS_KEPT is how many of the newest events the floor keeps for views.
export const EVENTS_KEPT = 100;

// VIEW_EVENTS are the event kinds a view is given in its data. A frame is
// not among them. A view gets frames only of the panes it watches, each
// as a picture of its own.
const VIEW_EVENTS = ['state', 'note', 'child', 'layout'];

// RING_SPAN is how many seconds of the server's event ring the floor
// keeps, and RING_MAX how many events at most.
export const RING_SPAN = 7200;
export const RING_MAX = 10000;

// REFRESH_MS is how often the floor sends a shown view fresh data.
export const REFRESH_MS = 3000;

// POST_MS is the shortest time between two posts that events or frames
// cause. Many events in that time make one post.
export const POST_MS = 250;

// EXTRA_MS is how often the floor reads the cards and the tree for a view
// that shows them.
export const EXTRA_MS = 30000;

// STOP_ON are the answers that end the refresh: a token the server no
// longer takes, and an address it has banned for a minute.
const STOP_ON = [401, 429];

// mountViewHost runs the view frame. show opens view id with sel, hide
// blanks the frame and stops the refresh, and onMessage acts on what the
// frame posts. deps carries api, panes, go and replace, every and cancel
// for the refresh timer, and later for the post timer. A host may also
// take open, which gets a pane a view asks to open in place of a route to
// the pane screen; select false, which drops every new selection a view
// sends; sel, which gives the selection each post carries; ring, a
// function that says whether a view id asked for the server's event ring;
// rpc, which a watch needs; and watch false, which drops every watch a
// view sends.
//
// A view that shares the page's socket with the floor's windows takes
// held, a function that gives the panes the windows hold now; picture,
// which gives a held pane's grid; and settled, a promise of the windows'
// detaches on their way. The watch never attaches or detaches a pane a
// window holds: the socket keeps one attach per pane, so either would end
// the window's own. A held pane's picture comes from the window's grid.
// reattach(panes, after) asks the windows to attach panes again once after
// settles, for a view-only attach that raced a window's own.
// close, when given, gets a view's ask to close.
//
// Each data post carries from: the time since which the events hold every
// state event, or null when the floor does not know. A view that did not
// ask for the ring, and a view whose ring was refused, gets null.
export function mountViewHost(frame, deps) {
  let current = null;
  let tasks = [];
  // panes is the newest pane list the floor holds or read for the view.
  let panes = [];
  // events is the newest EVENTS_KEPT events the floor heard, oldest first.
  const events = [];
  // ring is the state events the server's ring gave, oldest first,
  // ringTop the time of the newest of them, and ringFrom the time since
  // which the ring holds every one, or null when not known.
  let ring = [];
  let ringTop = 0;
  let ringFrom = null;
  const later = deps.later || ((fn, ms) => setTimeout(fn, ms));
  let pending = null;
  let dirty = false;
  const frames = new Map();
  // cards, diffs and tree are the data the cards and tree views are given.
  // The floor reads them with its own token, at most every EXTRA_MS, and
  // at once after an answer.
  let cards = [];
  let diffs = {};
  let tree = null;
  let notice = '';
  // readNotice is why the last read of the cards or the tree failed.
  let readNotice = '';
  let extraAt = -Infinity;
  const clock = deps.now || (() => Date.now());
  const wants = (kind) => Boolean(current && typeof deps.wants === 'function' && deps.wants(current.id, kind));
  const wantsRing = () => Boolean(current && typeof deps.ring === 'function' && deps.ring(current.id));
  const win = () => {
    const w = frame.contentWindow;
    return current && w && typeof w.postMessage === 'function' ? w : null;
  };
  const tellWatch = () => {
    const w = win();
    if (!w || !watch) return;
    const st = watch.status();
    const held = heldNow();
    const mine = wanted.filter((p) => held.has(p));
    w.postMessage({ type: 'coppice.watch', view: current.id, live: [...mine, ...st.live.filter((p) => !mine.includes(p))], refused: st.refused }, '*');
  };
  // watch holds the view-only attaches the shown view asked for, and
  // wanted is every pane the view asked to watch, held ones among them.
  const watch = deps.rpc && deps.watch !== false ? mountWatch(deps.rpc, tellWatch) : null;
  let wanted = [];
  const heldNow = () => new Set(typeof deps.held === 'function' ? deps.held() : []);
  const pictureOf = (pane) => {
    const grid = typeof deps.picture === 'function' ? deps.picture(pane) : null;
    return grid ? { pane, cols: grid.cols, rows: grid.rows, lines: gridToText(grid).split('\n') } : null;
  };
  // letGo hands panes to the windows with no detach. A pane whose
  // view-only attach was already on the wire may land after the window's
  // own attach, so the window attaches again once the watch is quiet.
  const letGo = (panes) => {
    const racing = watch.release(panes);
    if (racing.length && typeof deps.reattach === 'function') deps.reattach(racing, watch.quiet());
  };
  // applyWatch gives the watch the wanted panes no window holds, and lets
  // go of the held ones with no detach.
  const applyWatch = () => {
    if (!watch || !current) return;
    const held = heldNow();
    const ids = new Set(known().map((p) => p.id));
    wanted = wanted.filter((p) => ids.has(p));
    letGo([...held]);
    if (typeof deps.settled === 'function') watch.warden(deps.settled());
    watch.set(wanted.filter((p) => !held.has(p)));
  };
  const known = () => {
    const fromFloor = deps.panes();
    return Array.isArray(fromFloor) && fromFloor.length ? fromFloor : panes;
  };
  let timer = null;
  const stop = () => {
    if (timer !== null) deps.cancel(timer);
    timer = null;
  };
  // held is what the view is given as events: with the ring, the ring and
  // then each heard event the ring does not hold, so a state event is not
  // given twice; without it, the heard events alone.
  const held = () => (wantsRing()
    ? [...ring, ...events.filter((e) => e.event !== 'state' || !(e.ts <= ringTop))]
    : [...events]);
  const post = () => {
    dirty = false;
    const w = win();
    if (!w) return;
    // An opaque origin cannot be named as a target, so the message goes to
    // any. It holds only what the view is shown, never the token.
    w.postMessage({
      type: 'coppice.data',
      view: current.id,
      panes: forView(known()),
      tasks,
      sel: deps.sel ? deps.sel() : current.sel,
      events: held().map(eventForView),
      from: wantsRing() ? ringFrom : null,
      ...(wants('cards') ? { cards, diffs } : {}),
      ...(wants('tree') ? { tree } : {}),
      ...(notice || readNotice ? { notice: notice || readNotice } : {}),
    }, '*');
  };
  // flush sends what events and frames left waiting.
  const flush = () => {
    pending = null;
    if (dirty) post();
    const w = win();
    for (const pic of frames.values()) {
      if (w) w.postMessage({ type: 'coppice.frame', view: current.id, ...pic }, '*');
    }
    frames.clear();
  };
  const soon = () => {
    if (pending === null) pending = later(flush, POST_MS);
  };
  // forgetRing drops what the floor held from the ring. The next read
  // starts over, and until it answers the view is told from is unknown.
  const forgetRing = () => {
    ring = [];
    ringTop = 0;
    ringFrom = null;
  };
  // readRing asks the server for the ring events after the newest the
  // floor holds, and the time since which the ring holds every event.
  const readRing = async () => {
    if (!wantsRing()) return;
    try {
      const body = await deps.api('/api/events?since=' + ringTop);
      const fresh = (Array.isArray(body && body.events) ? body.events : [])
        .filter((e) => e && typeof e.ts === 'number' && e.ts > ringTop);
      ring = ring.concat(fresh);
      for (const e of fresh) ringTop = Math.max(ringTop, e.ts);
      ring = ring.filter((e) => e.ts >= ringTop - RING_SPAN).slice(-RING_MAX);
      ringFrom = body && typeof body.from === 'number' ? body.from : null;
    } catch (e) {
      forgetRing();
      throw e;
    }
  };
  // readExtras reads the cards and the tree for a view that asks for
  // them. A refusal keeps what the floor held and says why in notice.
  const readExtras = async (force) => {
    const t = clock();
    if (!force && t - extraAt < EXTRA_MS) return;
    const jobs = [];
    if (wants('cards')) {
      jobs.push(deps.api('/api/cards?sort=' + encodeURIComponent(cardSort))
        .then((b) => { cards = Array.isArray(b && b.cards) ? b.cards : []; readNotice = ''; }));
    }
    if (wants('tree')) {
      jobs.push(deps.api('/api/tree').then((b) => { tree = b && typeof b === 'object' ? b : null; readNotice = ''; }));
    }
    if (!jobs.length) return;
    extraAt = t;
    const done = await Promise.allSettled(jobs);
    for (const r of done) {
      if (r.status === 'rejected') {
        if (STOP_ON.includes(r.reason && r.reason.status)) stop();
        readNotice = (r.reason && r.reason.message) || 'Could not read.';
      }
    }
  };
  let cardSort = 'reversibility';
  const refresh = async () => {
    const [lists, ringRead] = await Promise.allSettled([
      Promise.all([deps.api('/api/tasks'), deps.api('/api/panes')]),
      readRing(),
      readExtras(false),
    ]);
    if (lists.status === 'fulfilled') {
      const [t, p] = lists.value;
      tasks = Array.isArray(t && t.tasks) ? t.tasks : [];
      panes = Array.isArray(p && p.panes) ? p.panes : [];
    }
    for (const r of [lists, ringRead]) {
      if (r.status === 'rejected' && STOP_ON.includes(r.reason && r.reason.status)) stop();
    }
    if (watch && current) {
      watch.refresh(known());
      applyWatch();
    }
    post();
  };
  frame.addEventListener('load', () => { post(); tellWatch(); });
  // show opens view id. A different view than the one shown ends the old
  // watch and starts the ring again. after, when given, is a promise every
  // attach waits for: the detaches the floor sent as the page left it.
  const show = (id, sel, after) => {
    if (current && current.id !== id) {
      if (watch) {
        letGo([...heldNow()]);
        watch.set([]);
      }
      wanted = [];
      forgetRing();
    }
    if (watch && after) watch.warden(after);
    const fresh = !current || current.id !== id;
    current = { id, sel: [...(sel || [])] };
    const src = viewSrc(id);
    if (frame.dataset.src !== src) {
      frame.dataset.src = src;
      frame.src = src;
    }
    frame.title = id;
    stop();
    extraAt = -Infinity;
    if (fresh) { cards = []; tree = null; diffs = {}; notice = ''; readNotice = ''; }
    timer = deps.every(refresh, REFRESH_MS);
    return refresh();
  };
  // hide blanks the frame and ends the watch. It returns a promise that
  // settles once each detach is answered, or null when none is on its
  // way, so the next screen attaches only after the detaches land.
  const hide = () => {
    current = null;
    stop();
    frames.clear();
    dirty = false;
    wanted = [];
    if (watch) letGo([...heldNow()]);
    const gone = watch ? watch.end() : null;
    if (frame.dataset.src) {
      frame.dataset.src = '';
      frame.src = 'about:blank';
    }
    return gone;
  };
  const json = (body) => ({ method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
  const failed = (e) => ((e && e.message) || 'The answer did not go through.');
  const answerCard = async (act) => {
    try {
      const r = await deps.api('/api/cards/answer', json({ choice_id: act.choice_id, outcome: act.outcome, pick: act.pick }));
      notice = act.outcome === 'switch' ? 'Switched to ' + act.pick + '.' : act.outcome === 'drop' ? 'Dropped.' : 'Kept.';
      if (r && r.pruned && typeof r.pruned === 'object' && Array.isArray(r.pruned.removed) && r.pruned.removed.length) {
        notice += ' Removed ' + r.pruned.removed.length + ' branch worktree' + (r.pruned.removed.length === 1 ? '' : 's') + '.';
      }
    } catch (err) {
      notice = failed(err);
    }
    delete diffs[act.choice_id];
    await readExtras(true);
    post();
  };
  const loadDiff = async (id) => {
    try {
      const b = await deps.api('/api/cards/' + encodeURIComponent(id) + '/diff');
      diffs = { ...diffs, [id]: { diffs: b.diffs || {}, notes: b.notes || {} } };
    } catch (err) {
      diffs = { ...diffs, [id]: { diffs: {}, notes: { '': failed(err) } } };
    }
    post();
  };
  const answerAsk = async (act) => {
    try {
      await deps.api('/api/tree/answer', json({ ask_id: act.ask_id, decision: act.decision }));
      notice = act.decision === 'allow' ? 'Allowed. The child is marked not proved.' : 'Denied.';
    } catch (err) {
      notice = failed(err);
    }
    await readExtras(true);
    post();
  };
  const onMessage = (e) => {
    if (!current || !e || e.source !== frame.contentWindow || !frame.contentWindow) return;
    const act = viewAction(e.data, known(), wants('cards') ? cards : [], wants('tree') ? tree : null);
    if (!act) return;
    if (act.kind === 'answer') { answerCard(act); return; }
    if (act.kind === 'card-diff') { loadDiff(act.choice_id); return; }
    if (act.kind === 'tree-answer') { answerAsk(act); return; }
    if (act.kind === 'open') {
      if (deps.open) deps.open(act.pane);
      else deps.go('#/pane/' + encodeURIComponent(act.pane));
      return;
    }
    if (act.kind === 'watch') {
      wanted = act.panes;
      applyWatch();
      // A held pane has a picture already: the window's own grid.
      for (const pane of wanted) {
        const pic = heldNow().has(pane) ? pictureOf(pane) : null;
        if (pic) frames.set(pane, pic);
      }
      if (frames.size) soon();
      tellWatch();
      return;
    }
    if (act.kind === 'close') {
      if (typeof deps.close === 'function') deps.close();
      return;
    }
    if (deps.select === false) return;
    current.sel = act.sel;
    deps.replace(viewHash(current.id, act.sel));
    post();
  };
  // onEvent keeps one event the floor heard. A shown view gets the new
  // list, and a frame of a watched pane as a picture, both at most once
  // each POST_MS.
  const onEvent = (msg) => {
    if (msg && msg.event === 'frame') {
      let pic = current && watch ? watch.frame(msg) : null;
      if (!pic && current && watch && wanted.includes(msg.pane) && heldNow().has(msg.pane)) pic = pictureOf(msg.pane);
      if (!pic) return;
      frames.set(pic.pane, pic);
      soon();
      return;
    }
    if (!msg || !VIEW_EVENTS.includes(msg.event)) return;
    events.push(msg);
    if (events.length > EVENTS_KEPT) events.splice(0, events.length - EVENTS_KEPT);
    if (!current) return;
    dirty = true;
    soon();
  };
  // dropped forgets the watch's attaches when the socket closes. The next
  // refresh makes them again on the new socket.
  const dropped = () => { if (watch) watch.dropped(); };
  // floorChanged is what the page calls when the windows take or give up
  // panes, so the watch follows what they hold.
  const floorChanged = () => {
    if (!current) return;
    applyWatch();
    tellWatch();
  };
  return { show, hide, onMessage, onEvent, post, dropped, flush, floorChanged, current: () => current };
}

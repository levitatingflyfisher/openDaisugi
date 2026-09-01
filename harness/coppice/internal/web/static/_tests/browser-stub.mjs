// A minimal browser environment for driving app.js under Node. A real page
// gives app.js window, document, location, localStorage, navigator, fetch,
// WebSocket, and timers; this supplies one fake of each, plus reset(), so a
// fresh scenario can start from a clean socket list, timer list, and DOM
// inside the same process.

class FakeEventTarget {
  constructor() { this.listeners = {}; }
  addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
  dispatchEvent(evt) { this.fire(evt.type, evt); }
  fire(type, detail) {
    for (const fn of (this.listeners[type] || [])) fn(detail || {});
  }
}

export const sockets = [];
export const timers = [];
export const intervals = [];
const store = new Map();
const elements = new Map();

// FakeElement is one stand-in for every DOM node app.js, roster.js, and
// pane.js touch: a button, a list item, a canvas. It keeps enough of the
// real element surface, properties, event listeners, children, and a 2D
// context stub, that mounting the roster and pane screens does not throw,
// without pulling in a real DOM.
class FakeElement {
  constructor(tag) {
    this.tagName = (tag || 'div').toUpperCase();
    this.id = '';
    this.children = [];
    this.hidden = false;
    this.ownText = '';
    this.className = '';
    this.value = '';
    this.tabIndex = 0;
    this.dataset = {};
    this.listeners = {};
  }
  // textContent reads the way a real node's does: the node's own text
  // followed by every child's, so a test can read a whole tile at once.
  // Setting it replaces the children with that text.
  get textContent() {
    return this.ownText + this.children.map((c) => (c instanceof FakeElement ? c.textContent : String(c))).join('');
  }
  set textContent(v) {
    this.ownText = v === null || v === undefined ? '' : String(v);
    this.children = [];
  }
  addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
  removeEventListener(type, fn) {
    if (!this.listeners[type]) return;
    this.listeners[type] = this.listeners[type].filter((f) => f !== fn);
  }
  dispatchEvent(evt) { for (const fn of (this.listeners[evt.type] || [])) fn(evt); }
  append(...nodes) { this.children.push(...nodes); }
  appendChild(node) { this.children.push(node); return node; }
  replaceChildren(...nodes) { this.children = nodes; }
  // querySelectorAll reads four selector forms, [data-name="value"],
  // .class, #id, and a bare tag, joined by at most one descendant step, and
  // walks children depth first. A lone #id that no descendant carries
  // falls back to the element document.getElementById made for that id.
  querySelectorAll(sel) {
    const steps = String(sel).trim().split(/\s+/);
    let scope = [this];
    for (const step of steps) {
      const found = [];
      for (const s of scope) {
        for (const d of descendants(s)) {
          if (matches(d, step) && !found.includes(d)) found.push(d);
        }
      }
      scope = found;
    }
    if (scope.length === 0 && steps.length === 1 && steps[0].startsWith('#') && elements.has(steps[0].slice(1))) {
      return [elements.get(steps[0].slice(1))];
    }
    return scope;
  }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
  // The pane screen tests drawGrid on its own with a purpose-built fake
  // context. This stub only has to exist so mountPane() does not throw
  // while wiring up the canvas.
  getContext() { return { canvas: { width: 0, height: 0 }, fillRect() {}, fillText() {} }; }
}

const camel = (name) => name.replace(/-([a-z])/g, (_, c) => c.toUpperCase());

function matches(node, step) {
  if (step.startsWith('[')) {
    const m = /^\[data-([\w-]+)="([^"]*)"\]$/.exec(step);
    return m !== null && node.dataset[camel(m[1])] === m[2];
  }
  if (step.startsWith('.')) return String(node.className || '').split(/\s+/).includes(step.slice(1));
  if (step.startsWith('#')) return node.id === step.slice(1);
  return node.tagName === step.toUpperCase();
}

// descendants lists every FakeElement under node, depth first. A scenario
// may put a plain string in children, and that is skipped.
function descendants(node, out = []) {
  for (const c of node.children) {
    if (!(c instanceof FakeElement)) continue;
    out.push(c);
    descendants(c, out);
  }
  return out;
}

function el(id) {
  if (!elements.has(id)) {
    const made = new FakeElement();
    made.id = id;
    elements.set(id, made);
  }
  return elements.get(id);
}

export function element(id) { return el(id); }

export class FakeWebSocket extends FakeEventTarget {
  constructor(url, protocols) {
    super();
    this.url = url;
    this.protocols = protocols;
    this.readyState = FakeWebSocket.CONNECTING;
    sockets.push(this);
  }
  send() {}
  close() {}
  // A real close sequence moves readyState to CLOSED before the close
  // event fires. Modeling that order is what lets connect()'s reentry
  // guard tell a dead socket from a live one.
  simulateOpen() {
    this.readyState = FakeWebSocket.OPEN;
    this.fire('open', {});
  }
  simulateClose(reason) {
    this.readyState = FakeWebSocket.CLOSED;
    this.fire('close', { type: 'close', reason });
  }
}
FakeWebSocket.CONNECTING = 0;
FakeWebSocket.OPEN = 1;
FakeWebSocket.CLOSING = 2;
FakeWebSocket.CLOSED = 3;

let fetchImpl = defaultFetch;

function defaultFetch() {
  return Promise.reject(new Error('no network in this harness'));
}

export function setFetch(fn) { fetchImpl = fn; }

// reset clears every piece of state a scenario could have left behind, so
// the next scenario starts from a socket list, timer list, and DOM with
// nothing in them, even though the process and the stubbed globals stay
// the same across scenarios.
export function reset() {
  sockets.length = 0;
  timers.length = 0;
  intervals.length = 0;
  store.clear();
  elements.clear();
  fetchImpl = defaultFetch;
  global.location.hash = '';
  // window itself is not recreated between scenarios, so the listeners a
  // previous freshApp() registered on it would otherwise still fire
  // alongside the next scenario's own, and either double a call or clear
  // a wrong entry when ids get reused after intervals and timers empty.
  windowTarget.listeners = {};
  windowTarget.innerWidth = 1200;
  documentListeners.length = 0;
}

global.setTimeout = (fn, ms) => {
  const id = timers.length;
  timers.push({ fn, ms, id, cancelled: false });
  return id;
};
global.clearTimeout = (id) => {
  if (timers[id]) timers[id].cancelled = true;
};
// setInterval is tracked separately from setTimeout, never as a real OS
// timer. A real one would keep node --test's event loop alive for the
// whole interval, and the roster's own ticker never gets cleared by any
// scenario that does not route away from the roster.
global.setInterval = (fn, ms) => {
  const id = intervals.length;
  intervals.push({ fn, ms, id, cancelled: false });
  return id;
};
global.clearInterval = (id) => {
  if (intervals[id]) intervals[id].cancelled = true;
};
global.localStorage = {
  getItem: (k) => (store.has(k) ? store.get(k) : null),
  setItem: (k, v) => store.set(k, v),
  removeItem: (k) => store.delete(k),
};
// documentListeners holds what the floor registers on the document itself,
// its keydown handler, so a scenario can press a key.
const documentListeners = [];
global.document = {
  readyState: 'complete',
  getElementById: el,
  addEventListener(type, fn) { documentListeners.push({ type, fn }); },
  dispatchEvent(evt) { for (const l of documentListeners.filter((l) => l.type === evt.type)) l.fn(evt); },
  createElement: (tag) => new FakeElement(tag),
};
global.history = { replaceState() {} };
global.location = { origin: 'https://box.example', hash: '', protocol: 'https:' };
Object.defineProperty(global, 'navigator', { value: {}, configurable: true });
global.WebSocket = FakeWebSocket;
global.fetch = (...args) => fetchImpl(...args);
global.CustomEvent = class FakeCustomEvent { constructor(type) { this.type = type; } };
const windowTarget = new FakeEventTarget();
windowTarget.dispatchEvent = (evt) => windowTarget.fire(evt.type, evt);
// The floor reads the width once at mount. 1200 gives two tiles.
windowTarget.innerWidth = 1200;
global.window = windowTarget;

// A token shaped the way token.go mints one: 32 bytes, base64url, no
// padding, 43 characters.
export const TOKEN = 'A'.repeat(43);

// freshApp imports app.js as a new module instance, with its own retryMs,
// giveUp, and state, distinct from any other scenario's instance. app.js
// has no cache-busting hook of its own, so the query string is what forces
// a second evaluation of the same file.
let n = 0;
export async function freshApp() {
  n += 1;
  return import(`../app.js?scenario=${n}`);
}

// lastSocket is the socket the most recent connect() call created. app.js
// exports nothing that names its own socket, so this is read from the
// stub's own record of every WebSocket app.js constructed.
export function lastSocket() { return sockets[sockets.length - 1]; }

// liveTimers is every scheduled timer not yet cancelled by clearTimeout,
// which is what "no timer scheduled" and "the reconnect timer" both mean
// in the assertions below.
export function liveTimers() { return timers.filter((t) => !t.cancelled); }

// liveIntervals is every setInterval not yet cancelled by clearInterval,
// which is what "the roster ticker is running" and "the roster ticker
// stopped" both mean in the assertions below.
export function liveIntervals() { return intervals.filter((t) => !t.cancelled); }

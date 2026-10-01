// The chat with the foreman, on the phone's home. It shows what the owner
// said to the floor's foreman and what the foremen said back, from
// floor.chat. A messages event for the chat only says the chat changed, so
// the page reads it again. A sentence the owner sends shows at once, is
// marked sent once floor.talk answers, and read once the server marks its
// line read: the foreman's transcript recorded it, so its turn started.
// The page never guesses a read mark. The chat is drawn from the open
// page only:
// nothing here is ever stored on the phone.

// CHAT_LIMIT is how many messages one read asks for.
export const CHAT_LIMIT = 100;
export const CHAT_EMPTY = 'No messages yet. Type a sentence below. It goes to the foreman.';
export const CHAT_NAMED = "The chat is for the owner's own sign-in.";
export const CHAT_REFUSED = "The server will not show the chat to this page. Open the floor with the owner's own sign-in.";

// makeAct is a fresh client act id for one sentence (FM-R-65): a random
// UUID. crypto.randomUUID needs a secure context, which a LAN page may not
// have, so the id is made from getRandomValues, and from Math.random only
// where a page has no crypto at all.
export function makeAct() {
  const c = typeof globalThis !== 'undefined' ? globalThis.crypto : undefined;
  if (c && typeof c.randomUUID === 'function') {
    try { return c.randomUUID(); } catch (e) { /* fall through */ }
  }
  const b = new Uint8Array(16);
  if (c && typeof c.getRandomValues === 'function') c.getRandomValues(b);
  else for (let i = 0; i < 16; i++) b[i] = Math.floor(Math.random() * 256);
  b[6] = (b[6] & 0x0f) | 0x40;
  b[8] = (b[8] & 0x3f) | 0x80;
  const h = Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}

// The act ids of the sentences being sent, one for each sentence text. A
// sentence that fails to send and is sent again keeps its id, so the server
// never logs it twice. Two inputs of the page share the table, so a failed
// sentence in one does not lose its id when the other sends. A sent
// sentence leaves the table (actDone); the next sentence gets a new id.
const acts = new Map();
const MAX_ACTS = 32;
export function actFor(line) {
  let id = acts.get(line);
  if (!id) {
    id = makeAct();
    acts.set(line, id);
    if (acts.size > MAX_ACTS) acts.delete(acts.keys().next().value);
  }
  return id;
}
export function actDone(line) {
  if (line === undefined) acts.clear();
  else acts.delete(line);
}

// cleanTalk is a sentence as the server writes it to the chat: line
// breaks and tabs become spaces, other control characters go, and each run
// of spaces is one space.
export function cleanTalk(text) {
  return String(text || '')
    .replace(/[\t\n\r\v\f]/g, ' ')
    // eslint-disable-next-line no-control-regex
    .replace(/[\u0000-\u001f\u007f]/g, '')
    .split(' ')
    .filter(Boolean)
    .join(' ');
}

const WORD = /[\p{L}\p{N}_-]/u;

// chipParts splits text into plain parts and chips. A chip is a live agent
// the text names by its label or its id, as a whole word. Longer names
// match first, so glean-import is never read as glean.
export function chipParts(text, panes) {
  const s = String(text || '');
  if (!s) return [];
  const names = [];
  for (const p of Array.isArray(panes) ? panes : []) {
    if (!p || !p.id || p.closed) continue;
    const label = p.label || p.id;
    names.push({ name: p.id, pane: p.id, label });
    if (p.label) names.push({ name: p.label, pane: p.id, label });
  }
  names.sort((a, b) => b.name.length - a.name.length);
  const out = [];
  let plain = '';
  let i = 0;
  while (i < s.length) {
    const before = i > 0 ? s[i - 1] : '';
    const hit = before && WORD.test(before) ? null : names.find((n) => {
      if (!s.startsWith(n.name, i)) return false;
      const after = s[i + n.name.length] || '';
      return !(after && WORD.test(after));
    });
    if (hit) {
      if (plain) out.push({ text: plain });
      plain = '';
      out.push({ chip: true, pane: hit.pane, label: hit.label });
      i += hit.name.length;
    } else {
      plain += s[i];
      i += 1;
    }
  }
  if (plain) out.push({ text: plain });
  return out;
}

// chatTime is a message's time as hours and minutes, or '' when the server
// knows no time for it.
export function chatTime(at) {
  const n = Number(at);
  if (!Number.isFinite(n) || n <= 0) return '';
  const d = new Date(n * 1000);
  const two = (v) => String(v).padStart(2, '0');
  return two(d.getHours()) + ':' + two(d.getMinutes());
}

// DESK_LINE is the line under the chat window on the desk. The words go
// to the foreman through floor.talk, not to its terminal.
export const DESK_LINE = 'Tell the foreman. Enter sends.';

// lastReply is the id of the newest message the foreman wrote in words, or
// ''. A tool use is not a reply, and neither is a line the owner wrote.
export function lastReply(messages) {
  const list = Array.isArray(messages) ? messages : [];
  for (let i = list.length - 1; i >= 0; i--) {
    const m = list[i];
    if (m && m.role === 'agent' && !m.tool && m.id !== undefined) return String(m.id);
  }
  return '';
}

const WHO = { owner: 'you', agent: 'foreman', note: '' };
// READING_WHO names the speakers of one agent's transcript.
export const READING_WHO = { owner: 'you', agent: 'agent', note: '' };

// chatRows is what the chat draws: the server's messages in order, each
// owner line marked read when the server's read field is true, else sent,
// then the lines this page sent that the server does not show yet. A local
// line carries before, how many owner lines with its words the server
// showed when it was sent, so an older line with the same words never
// takes its place.
//
// opts: who maps a role to the name drawn for it, and marks false leaves
// the sent and read marks off, as a transcript has none.
export function chatRows(messages, local, opts = {}) {
  const names = opts.who || WHO;
  const msgs = Array.isArray(messages) ? messages.filter((m) => m && typeof m === 'object') : [];
  const rows = msgs.map((m) => {
    let mark = '';
    if (m.role === 'owner' && opts.marks !== false) {
      mark = m.read === true ? 'read' : 'sent';
      // The server says when a line reached a running turn, or waits for its own.
      if (m.delivery === 'delivered mid-turn' || m.delivery === 'queued') mark = m.delivery;
    }
    return {
      key: String(m.id), role: m.role, who: m.role in names ? names[m.role] : String(m.role || ''),
      at: m.at, text: String(m.text || ''), tool: m.tool || '', pane: m.pane || '', mark,
    };
  });
  for (const l of pendingLines(msgs, local)) {
    rows.push({ key: 'local/' + l.id, role: 'owner', who: 'you', at: l.at || 0, text: String(l.text), tool: '', pane: '', mark: l.state });
  }
  return rows;
}

// pendingLines is the local lines the server does not show yet, oldest
// first.
export function pendingLines(messages, local) {
  const shown = new Map();
  for (const m of Array.isArray(messages) ? messages : []) {
    if (!m || m.role !== 'owner') continue;
    const k = cleanTalk(m.text);
    shown.set(k, (shown.get(k) || 0) + 1);
  }
  const used = new Map();
  const out = [];
  for (const l of Array.isArray(local) ? local : []) {
    const k = cleanTalk(l.text);
    const u = used.get(k) || 0;
    if ((shown.get(k) || 0) - (l.before || 0) > u) {
      used.set(k, u + 1);
      continue;
    }
    out.push(l);
  }
  return out;
}

function make(tag, className, text) {
  const el = document.createElement(tag);
  if (className) el.className = className;
  if (text !== undefined) el.textContent = text;
  return el;
}

// rowItem draws one message. A reply's agents are chips that open their
// sheets.
function rowItem(r, panes, open, chips = true, speak = null) {
  const li = make('li', 'msg msg-' + r.role + (r.tool ? ' msg-tool' : ''));
  li.dataset.key = r.key;
  if (r.role === 'note') {
    li.append(make('p', 'msg-text', r.text));
    return li;
  }
  // A tool use is one dim line under the reply, with no head of its own.
  if (!r.tool) {
    const head = make('p', 'msg-head');
    head.append(make('span', 'who', r.who));
    const t = chatTime(r.at);
    if (t) head.append(' ', make('span', 'time muted', t));
    if (r.mark) head.append(' ', make('span', 'mark mark-' + r.mark.replace(/\s+/g, '-'), r.mark));
    // Listen reads this reply aloud.
    if (speak && r.role === 'agent') {
      const b = make('button', 'chip listen', 'Listen');
      b.type = 'button';
      b.title = 'Read this reply aloud';
      b.addEventListener('click', () => speak(r.text));
      head.append(' ', b);
    }
    li.append(head);
  }
  const body = make('p', 'msg-text');
  if (r.tool) body.append(make('span', 'tool', '· ' + r.tool + ' '));
  const parts = r.role === 'agent' && chips ? chipParts(r.text, panes) : [{ text: r.text }];
  for (const part of parts) {
    if (!part.chip) {
      body.append(part.text);
      continue;
    }
    const b = make('button', 'chip chip-agent', part.label + ' ›');
    b.type = 'button';
    b.dataset.pane = part.pane;
    b.title = 'Open ' + part.label;
    b.addEventListener('click', (e) => {
      if (e && typeof e.preventDefault === 'function') e.preventDefault();
      open(part.pane);
    });
    body.append(b);
  }
  li.append(body);
  return li;
}

// mountChat draws the chat into list. opts: rpc; panes() the live pane
// list, for the chips; open(pane) opens an agent's sheet; me() the name
// the sign-in carries, '' for the owner's own; on() whether the chat is on
// screen. It returns read(), sent(text), onEvent(msg) and paint().
//
// The same list draws one agent's transcript for the reading view. Then
// cmd is 'pane.messages' and fields() names the pane; match(msg) says which
// messages events are for this list; who, marks, chips and empty change how
// the lines are drawn; scroller() is the element that scrolls; speak(text),
// when given, puts a Listen button on each reply. seed(list)
// shows messages already read, and reset() empties the list for another
// agent.
// importView turns a floor.import reply into what the import bar shows, or
// null when it shows nothing: {text, buttons: [{label, action}]}. Import is
// the owner's choice (FM-R-71): the offer has a yes and a no, a running
// import has a pause, and a declined or finished one has no bar.
export function importView(r) {
  if (!r || typeof r.state !== 'string') return null;
  const n = (k) => Math.max(0, Math.round(Number(r[k]) || 0));
  const msgs = (k) => n(k) + (n(k) === 1 ? ' message' : ' messages');
  // An accept that found nothing says so (never a silent no-op).
  if (typeof r.note === 'string' && r.note !== '') return { text: r.note[0].toUpperCase() + r.note.slice(1) + '.', buttons: [] };
  if (r.state === 'off' && !r.estimating && n('sessions') > 0) {
    const chats = n('sessions') + (n('sessions') === 1 ? ' past chat' : ' past chats');
    return {
      text: 'Import ' + chats + ' (about ' + msgs('messages') + ')?',
      buttons: [{ label: 'Import', action: 'accept' }, { label: 'No thanks', action: 'decline' }],
    };
  }
  if (r.state === 'running') {
    return { text: 'Importing past chats: ' + n('done') + ' of ' + msgs('total') + '.', buttons: [{ label: 'Pause', action: 'pause' }] };
  }
  if (r.state === 'paused' && n('total') > 0) {
    return { text: 'Import paused: ' + n('done') + ' of ' + msgs('total') + '.', buttons: [{ label: 'Resume', action: 'resume' }] };
  }
  return null;
}

// mountImportBar draws importView into bar. refresh asks the server
// (floor.import) and draws; a button sends its action and draws the answer.
// A server that cannot import (pane mode, a refusal) leaves the bar empty.
export function mountImportBar(bar, rpc) {
  const paint = (r) => {
    const v = importView(r);
    if (!v) {
      bar.hidden = true;
      bar.replaceChildren();
      return;
    }
    bar.hidden = false;
    const kids = [make('span', 'import-text', v.text)];
    for (const b of v.buttons) {
      const btn = make('button', 'ghost import-' + b.action, b.label);
      btn.type = 'button';
      btn.addEventListener('click', (e) => {
        if (e && typeof e.stopPropagation === 'function') e.stopPropagation();
        bar.replaceChildren(make('span', 'import-text muted', 'Working…'));
        Promise.resolve()
          .then(() => rpc('floor.import', { action: b.action }))
          .then(paint)
          .catch(() => refresh());
      });
      kids.push(btn);
    }
    bar.replaceChildren(...kids);
  };
  const refresh = async () => {
    try {
      paint(await rpc('floor.import', { action: 'status' }));
    } catch (e) {
      paint(null);
    }
  };
  if (typeof bar.setAttribute === 'function') bar.setAttribute('role', 'status');
  bar.hidden = true;
  return { refresh, paint };
}

export function mountChat({ list, rpc, panes, open, me, on, cmd = 'floor.chat', fields, match, who, marks, chips = true, empty = CHAT_EMPTY, scroller, speak, importBar }) {
  const imp = importBar ? mountImportBar(importBar, rpc) : null;
  let messages = [];
  let local = [];
  let failed = '';
  let loaded = false;
  let seq = 0;
  let reading = null;
  let again = false;
  let queued = false;
  const live = () => typeof on !== 'function' || on();
  const named = () => Boolean(typeof me === 'function' && me());
  const near = () => {
    const own = typeof scroller === 'function' ? scroller() : null;
    if (own) return (own.scrollHeight || 0) - (own.scrollTop || 0) - (own.clientHeight || 0) < 120;
    const doc = typeof document !== 'undefined' ? document.scrollingElement || document.documentElement : null;
    if (!doc || typeof window === 'undefined') return true;
    return (doc.scrollHeight || 0) - (doc.scrollTop || 0) - (window.innerHeight || 0) < 120;
  };
  // drawn is what the list shows now: each item's signature and element.
  // A paint that only adds messages at the end appends them, so the list
  // announces only the new ones.
  let drawn = [];
  // paint draws the chat. follow scrolls to the newest line even when
  // the owner had scrolled up, as after a send.
  const paint = (follow) => {
    if (!list) return;
    const stay = follow === true || near();
    const agents = panes ? panes() : [];
    const want = [];
    if (named()) {
      want.push({ sig: 'named', make: () => make('li', 'msg msg-empty muted', CHAT_NAMED) });
    } else {
      for (const r of chatRows(messages, local, { who, marks })) {
        const found = r.role === 'agent' && chips ? chipParts(r.text, agents).filter((x) => x.chip).map((x) => x.pane + '=' + x.label) : [];
        want.push({ sig: JSON.stringify([r.key, r.who, r.at, r.mark, r.text, r.tool, found]), make: () => rowItem(r, agents, open, chips, speak) });
      }
      if (failed) want.push({ sig: 'failed ' + failed, make: () => make('li', 'msg msg-error', failed) });
      else if (want.length === 0 && loaded) want.push({ sig: 'empty', make: () => make('li', 'msg msg-empty muted', empty) });
    }
    const prefix = drawn.length <= want.length && drawn.every((d, i) => d.sig === want[i].sig);
    if (prefix) {
      const added = want.slice(drawn.length).map((w) => ({ sig: w.sig, el: w.make() }));
      if (added.length) list.append(...added.map((a) => a.el));
      drawn = drawn.concat(added);
    } else {
      const old = new Map(drawn.map((d) => [d.sig, d.el]));
      drawn = want.map((w) => ({ sig: w.sig, el: old.get(w.sig) || w.make() }));
      list.replaceChildren(...drawn.map((d) => d.el));
    }
    const last = drawn.length ? drawn[drawn.length - 1].el : null;
    if (stay && last && typeof last.scrollIntoView === 'function') last.scrollIntoView({ block: 'end' });
  };
  const readOnce = async () => {
    try {
      const r = await rpc(cmd, { ...(typeof fields === 'function' ? fields() : {}), limit: CHAT_LIMIT });
      messages = Array.isArray(r && r.messages) ? r.messages : [];
      local = pendingLines(messages, local);
      failed = '';
    } catch (e) {
      failed = (e && e.code === 'unauthorized') ? (named() ? CHAT_NAMED : CHAT_REFUSED) : 'Could not read the chat: ' + ((e && e.message) || String(e));
    }
    loaded = true;
    paint();
    if (imp) imp.refresh();
  };
  // read reads the chat when it is on screen. A read asked for while one
  // runs runs once more after it.
  const read = async () => {
    if (!live()) return;
    if (named()) {
      paint();
      return;
    }
    if (reading) {
      again = true;
      return reading;
    }
    reading = (async () => {
      do {
        again = false;
        await readOnce();
      } while (again);
      reading = null;
    })();
    return reading;
  };
  const onEvent = (msg) => {
    const mine = typeof match === 'function' ? match(msg) : msg && msg.chat === true;
    if (!msg || msg.event !== 'messages' || !mine || queued) return;
    queued = true;
    Promise.resolve().then(() => {
      queued = false;
      read();
    });
  };
  // sent shows a sentence at once. ok() marks it sent; fail() takes it
  // away, since the words stay in the bar.
  const sent = (text) => {
    const k = cleanTalk(text);
    const before = messages.filter((m) => m && m.role === 'owner' && cleanTalk(m.text) === k).length;
    const line = { id: ++seq, text: String(text), state: 'sending', before, at: Date.now() / 1000 };
    local.push(line);
    paint(true);
    return {
      ok: () => { line.state = 'sent'; paint(); },
      fail: () => { local = local.filter((l) => l !== line); paint(); },
    };
  };
  // seed shows messages that were read already.
  const seed = (got) => {
    messages = Array.isArray(got) ? got : [];
    failed = '';
    loaded = true;
    paint(true);
  };
  // reset empties the list, for another agent.
  const reset = () => {
    messages = [];
    local = [];
    failed = '';
    loaded = false;
    drawn = [];
    if (list && typeof list.replaceChildren === 'function') list.replaceChildren();
  };
  return { read, sent, onEvent, paint, seed, reset };
}

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

const WHO = { owner: 'you', agent: 'foreman', note: '' };

// chatRows is what the chat draws: the server's messages in order, each
// owner line marked read when the server's read field is true, else sent,
// then the lines this page sent that the server does not show yet. A local
// line carries before, how many owner lines with its words the server
// showed when it was sent, so an older line with the same words never
// takes its place.
export function chatRows(messages, local) {
  const msgs = Array.isArray(messages) ? messages.filter((m) => m && typeof m === 'object') : [];
  const rows = msgs.map((m) => {
    let mark = '';
    if (m.role === 'owner') mark = m.read === true ? 'read' : 'sent';
    return {
      key: String(m.id), role: m.role, who: m.role in WHO ? WHO[m.role] : String(m.role || ''),
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
function rowItem(r, panes, open) {
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
    li.append(head);
  }
  const body = make('p', 'msg-text');
  if (r.tool) body.append(make('span', 'tool', '· ' + r.tool + ' '));
  const parts = r.role === 'agent' ? chipParts(r.text, panes) : [{ text: r.text }];
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
export function mountChat({ list, rpc, panes, open, me, on }) {
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
      for (const r of chatRows(messages, local)) {
        const chips = r.role === 'agent' ? chipParts(r.text, agents).filter((x) => x.chip).map((x) => x.pane + '=' + x.label) : [];
        want.push({ sig: JSON.stringify([r.key, r.who, r.at, r.mark, r.text, r.tool, chips]), make: () => rowItem(r, agents, open) });
      }
      if (failed) want.push({ sig: 'failed ' + failed, make: () => make('li', 'msg msg-error', failed) });
      else if (want.length === 0 && loaded) want.push({ sig: 'empty', make: () => make('li', 'msg msg-empty muted', CHAT_EMPTY) });
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
      const r = await rpc('floor.chat', { limit: CHAT_LIMIT });
      messages = Array.isArray(r && r.messages) ? r.messages : [];
      local = pendingLines(messages, local);
      failed = '';
    } catch (e) {
      failed = (e && e.code === 'unauthorized' && named()) ? CHAT_NAMED : 'Could not read the chat: ' + ((e && e.message) || String(e));
    }
    loaded = true;
    paint();
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
    if (!msg || msg.event !== 'messages' || msg.chat !== true || queued) return;
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
  return { read, sent, onEvent, paint };
}

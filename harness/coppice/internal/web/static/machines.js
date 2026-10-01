// Machines: a read-only panel of the boxes on the tailnet that serve the
// floor. One row for each machine: an online dot, its name, its system, and
// under it the profiles that point at it with how fast each answers. It has
// no button that changes anything: adding a box and the access rules stay
// in Tailscale's own console. The pure rules come first; mountMachines
// draws them over the floor as a panel, which is a bottom sheet on a phone
// (the .panel rules of app.css).

// MACHINES_MS is how often an open panel asks again.
export const MACHINES_MS = 5000;

// NO_PROFILE is the line under a machine that no profile points at.
export const NO_PROFILE = 'No profile points here.';

// KEY_NOT_SET is the note on a profile whose key variable is not set. A
// profile that needs no key shows nothing extra.
export const KEY_NOT_SET = 'Key not set.';

// UNKNOWN is the line when the server does not answer. It names the fix.
export const UNKNOWN = 'The machines are not known. Reconnect to the server, then open Machines again.';

// answerText is how a probe reads: the time of its TCP connect, or that
// nothing answered.
export function answerText(p) {
  if (!p || !p.answers) return 'No answer';
  const ms = Number(p.answers_ms);
  return Number.isFinite(ms) ? (ms < 1 ? 'under 1 ms' : Math.round(ms) + ' ms') : 'Answers';
}

// seenText is the last-seen time of an offline machine, local time, or ''.
export function seenText(iso) {
  const d = new Date(String(iso || ''));
  if (!iso || Number.isNaN(d.getTime())) return '';
  const two = (n) => String(n).padStart(2, '0');
  return 'last seen ' + d.getFullYear() + '-' + two(d.getMonth() + 1) + '-' + two(d.getDate()) + ' ' + two(d.getHours()) + ':' + two(d.getMinutes());
}

// describe turns a floor.machines reply into what the panel draws:
// {line, note, rows}. line is the one sentence a tailscale that is absent
// or off gets, with rows empty. note says the profiles were not known.
// A null reply is the server not answering.
export function describe(reply) {
  if (!reply || typeof reply !== 'object') return { line: UNKNOWN, note: '', rows: [] };
  const line = reply.tailscale === 'ok' ? '' : String(reply.line || UNKNOWN);
  const rows = (Array.isArray(reply.machines) ? reply.machines : []).map((m) => {
    const profiles = (Array.isArray(m.profiles) ? m.profiles : []).map((p) => ({
      name: String(p.name || ''), address: String(p.address || ''), text: answerText(p), ok: Boolean(p.answers), keyMissing: p.key_set === false,
    }));
    const voice = m.voice ? { name: 'voice', address: String(m.voice.address || ''), text: answerText(m.voice), ok: Boolean(m.voice.answers) } : null;
    const speak = m.speak ? { name: 'speech', address: String(m.speak.address || ''), text: answerText(m.speak), ok: Boolean(m.speak.answers) } : null;
    return {
      name: String(m.name || ''), os: String(m.os || ''), online: Boolean(m.online), self: Boolean(m.self),
      seen: m.online ? '' : seenText(m.last_seen), profiles, voice, speak,
    };
  });
  return { line, note: reply.profiles_line ? String(reply.profiles_line) : '', rows };
}

function make(tag, className, text) {
  const el = document.createElement(tag);
  if (className) el.className = className;
  if (text !== undefined) el.textContent = text;
  return el;
}

function probeEl(p) {
  const li = make('li', 'machine-profile ' + (p.ok ? 'up' : 'down'));
  li.append(make('span', 'machine-profile-name', p.name), make('span', 'muted machine-address', p.address), make('span', 'machine-answer', p.text));
  if (p.keyMissing) li.append(make('span', 'machine-key', KEY_NOT_SET));
  return li;
}

// machinesEl draws the rows of describe(...) into list.
export function paintMachines(list, view) {
  list.textContent = '';
  for (const r of view.rows) {
    const li = make('li', 'machine ' + (r.online ? 'up' : 'down'));
    const head = make('div', 'machine-head');
    const dot = make('span', 'dot ' + (r.online ? 'dot-working' : 'dot-idle'));
    dot.setAttribute('role', 'img');
    dot.setAttribute('aria-label', r.online ? 'online' : 'offline');
    head.append(dot, make('strong', 'machine-name', r.name), make('span', 'muted machine-os', [r.os, r.self ? 'this box' : '', r.seen].filter(Boolean).join(' · ')));
    li.append(head);
    const ul = make('ul', 'machine-profiles');
    for (const p of r.profiles) ul.append(probeEl(p));
    if (r.voice) ul.append(probeEl(r.voice));
    if (r.speak) ul.append(probeEl(r.speak));
    if (r.profiles.length === 0 && !r.voice && !r.speak) ul.append(make('li', 'muted machine-none', view.note ? 'Profiles not known.' : NO_PROFILE));
    li.append(ul);
    list.append(li);
  }
}

// mountMachines wires the panel. d carries el (the panel), list (its row
// list), line (the one-line message), note (the profiles note), close (the
// x button), rpc(cmd, fields), connected(), keys() (the focus: 'window'
// keeps Esc for the agent), and the timer, later(fn, ms) and cancel(id),
// setTimeout and clearTimeout by default. It returns {open, close, toggle,
// isOpen, read}. An open panel reads again every MACHINES_MS, one read at a
// time, and stops when it closes.
export function mountMachines(d) {
  const later = d.later || ((fn, ms) => setTimeout(fn, ms));
  const cancel = d.cancel || ((id) => clearTimeout(id));
  let shown = false;
  let timer = null;
  let busy = false;
  const stop = () => { if (timer !== null) { cancel(timer); timer = null; } };
  const read = async () => {
    if (busy) return;
    busy = true;
    stop();
    let view;
    try {
      view = describe(await d.rpc('floor.machines', {}));
    } catch (e) {
      view = describe(null);
      d.line.title = (e && e.message) || '';
    }
    busy = false;
    if (!shown) return;
    d.line.textContent = view.line;
    d.line.hidden = view.line === '';
    d.note.textContent = view.note;
    d.note.hidden = view.note === '';
    paintMachines(d.list, view);
    if (view.rows.length === 0 && view.line === '') {
      d.line.textContent = 'This tailnet lists no machines.';
      d.line.hidden = false;
    }
    stop();
    if (shown && d.connected()) timer = later(read, MACHINES_MS);
  };
  const close = () => {
    if (!shown) return;
    shown = false;
    stop();
    d.el.hidden = true;
  };
  const open = () => {
    if (shown) return;
    shown = true;
    d.el.hidden = false;
    d.line.textContent = 'Reading the tailnet.';
    d.line.hidden = false;
    d.note.hidden = true;
    d.list.textContent = '';
    read();
  };
  d.close.addEventListener('click', close);
  document.addEventListener('keydown', (e) => {
    if (!shown || !e || e.key !== 'Escape' || e.defaultPrevented) return;
    if (d.keys && d.keys() === 'window') return;
    if (typeof e.preventDefault === 'function') e.preventDefault();
    close();
  });
  return { open, close, toggle: () => (shown ? close() : open()), isOpen: () => shown, read };
}

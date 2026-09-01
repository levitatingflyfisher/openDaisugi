import { mountRecord } from './record.js';
import { projectMenu } from './rail.js';
import { toMessage, fixButtons } from './messages.js';

// NO_DEFAULT is what the project menu says when the config names no
// default harness.
export const NO_DEFAULT = 'No default harness is set. Use More to pick a command.';

// mountNewButton wires New and its project menu. New starts the default
// harness near the selected agent in one click. The menu lists the
// projects, pinned first, and starts the default harness in the one
// chosen. More opens the full form. opts: rpc, status, near() the pane to
// start near or '', made(pane) for the new agent, go(hash).
export function mountNewButton(opts) {
  const btn = document.getElementById('go-new');
  const menuBtn = document.getElementById('new-menu-btn');
  const menu = document.getElementById('new-menu');
  let busy = false;
  const start = async (fields, where) => {
    if (busy) return;
    busy = true;
    try {
      const result = await opts.rpc('pane.create', { kind: 'pty', ...fields });
      opts.status('Started ' + (result && result.label ? result.label : 'a new agent') + (where ? ' in ' + where : '') + '.');
      if (result && result.pane) opts.made(result.pane);
    } catch (e) {
      opts.status(e.message);
    } finally {
      busy = false;
    }
  };
  const close = () => {
    if (!menu) return;
    menu.hidden = true;
    if (menuBtn && typeof menuBtn.setAttribute === 'function') menuBtn.setAttribute('aria-expanded', 'false');
  };
  const item = (text, sub, fn) => {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'menu-item';
    if (typeof b.setAttribute === 'function') b.setAttribute('role', 'menuitem');
    const name = document.createElement('span');
    name.className = 'name';
    name.textContent = text;
    b.append(name);
    if (sub) {
      const s = document.createElement('span');
      s.className = 'muted sub';
      s.textContent = sub;
      b.append(s);
    }
    b.addEventListener('click', (e) => {
      if (e && typeof e.stopPropagation === 'function') e.stopPropagation();
      close();
      fn();
    });
    return b;
  };
  const open = async () => {
    if (!menu) return;
    let list = [];
    let harness = '';
    let failed = null;
    try {
      const r = await opts.rpc('project.list');
      list = projectMenu(r && r.projects);
      harness = (r && typeof r.default === 'string') ? r.default : '';
    } catch (e) {
      failed = toMessage(e);
    }
    const items = list.map((p) => item(p.name, (p.pinned ? 'pinned · ' : '') + p.path, () => {
      if (!harness) {
        opts.status(NO_DEFAULT);
        return;
      }
      start({ cwd: p.path, harness }, p.name);
    }));
    if (items.length === 0) {
      const none = document.createElement('p');
      none.className = 'muted menu-note';
      // A refused read says why, with its fix, never that there are no
      // projects.
      none.textContent = failed ? 'Could not read the projects: ' + failed.text : 'No projects yet. New starts near the selected agent.';
      if (failed) none.append(' ', ...fixButtons(failed, (a) => { close(); if (window.coppice && window.coppice.fix) window.coppice.fix(a); }));
      items.push(none);
    }
    items.push(item('More...', 'directory, command, kind', () => opts.go('#/new')));
    menu.replaceChildren(...items);
    menu.hidden = false;
    if (menuBtn && typeof menuBtn.setAttribute === 'function') menuBtn.setAttribute('aria-expanded', 'true');
    const first = menu.querySelector('button');
    if (first && typeof first.focus === 'function') first.focus();
  };
  if (btn) {
    btn.addEventListener('click', (e) => {
      if (e && typeof e.stopPropagation === 'function') e.stopPropagation();
      close();
      const near = opts.near();
      start(near ? { near } : {}, '');
    });
  }
  if (menuBtn) {
    menuBtn.addEventListener('click', (e) => {
      if (e && typeof e.stopPropagation === 'function') e.stopPropagation();
      if (menu && !menu.hidden) close();
      else open();
    });
  }
  if (menu) {
    // Esc closes the menu, and the arrows move between its items.
    menu.addEventListener('keydown', (e) => {
      if (e.key === 'Escape') {
        close();
        if (menuBtn && typeof menuBtn.focus === 'function') menuBtn.focus();
        return;
      }
      if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp') return;
      if (typeof e.preventDefault === 'function') e.preventDefault();
      const all = menu.querySelectorAll('button');
      const at = all.indexOf ? all.indexOf(e.target) : Array.from(all).indexOf(e.target);
      const next = all[(at + (e.key === 'ArrowDown' ? 1 : all.length - 1)) % all.length];
      if (next && typeof next.focus === 'function') next.focus();
    });
  }
  if (typeof document.addEventListener === 'function') document.addEventListener('click', () => { if (menu && !menu.hidden) close(); });
  return { open, close };
}

// Start a pane from the kitchen. The directory is the one field that must
// not have a default: a pane started in the wrong repository does real
// work in the wrong place.

const MAX_RECENT = 10;

export function createCommand(form) {
  const cwd = (form.cwd || '').trim();
  if (!cwd) throw new Error('Pick a directory first.');
  const req = {
    cmd: 'pane.create',
    cwd,
    label: (form.label || '').trim(),
    kind: form.kind === 'headless' ? 'headless' : 'pty',
    env: {},
  };
  if (req.kind === 'headless') {
    req.harness = form.harness;
    req.cmd_argv = [];
  } else {
    const command = (form.command || '').trim();
    if (!command) throw new Error('Type a command first.');
    req.cmd_argv = command.split(/\s+/);
  }
  return req;
}

// recentCwds is the picker's list: the directories panes are running in
// today, then the ones this phone used before.
export function recentCwds(panes, stored) {
  const out = [];
  for (const cwd of [...(panes || []).map((p) => p.cwd), ...(stored || [])]) {
    if (cwd && !out.includes(cwd)) out.push(cwd);
  }
  return out.slice(0, MAX_RECENT);
}

function readStored() {
  try { return JSON.parse(localStorage.getItem('coppice.cwds') || '[]'); } catch { return []; }
}

export function mountNew() {
  mountTell();
  const kind = document.getElementById('new-kind');
  const command = document.getElementById('new-command');
  const commandLabel = document.getElementById('new-command-label');
  const harness = document.getElementById('new-harness');
  const harnessLabel = document.getElementById('new-harness-label');
  const sync = () => {
    const headless = kind.value === 'headless';
    command.hidden = headless;
    commandLabel.hidden = headless;
    harness.hidden = !headless;
    harnessLabel.hidden = !headless;
  };
  kind.addEventListener('change', sync);
  sync();

  window.addEventListener('coppice:route', () => {
    if (window.coppice.route(location.hash).screen !== 'new') return;
    const list = document.getElementById('recent-cwds');
    list.replaceChildren(...recentCwds(window.coppice.state.panes, readStored()).map((cwd) => {
      const o = document.createElement('option');
      o.value = cwd;
      return o;
    }));
  });

  document.getElementById('create').addEventListener('click', async () => {
    let req;
    try {
      req = createCommand({
        cwd: document.getElementById('new-cwd').value,
        label: document.getElementById('new-label').value,
        kind: kind.value,
        command: command.value,
        harness: harness.value,
      });
    } catch (e) {
      window.coppice.status(e.message);
      return;
    }
    const { cmd, ...fields } = req;
    try {
      const result = await window.coppice.rpc(cmd, fields);
      const stored = recentCwds([{ cwd: req.cwd }], readStored());
      localStorage.setItem('coppice.cwds', JSON.stringify(stored));
      location.hash = '#/pane/' + encodeURIComponent(result.pane);
    } catch (e) {
      window.coppice.status(e.message);
    }
  });
}

// HARNESSES are the harness names the tell bar opens by name. The terminal
// floor reads the same list.
export const HARNESSES = ['claude', 'codex', 'pi', 'sprig', 'opencode'];

// VERBS are the terminal floor's plumbing verbs. A line led by one, at
// most PLUMBING_WORDS long, is plumbing. A longer one is a sentence.
const VERBS = ['list', 'read', 'close', 'open', 'swap', 'rotate', 'reset', 'lock', 'unlock', 'layout', 'tree', 'zoom', 'foreman'];
const PLUMBING_WORDS = 4;

// classifyTell tells plumbing from talk the way the terminal floor does. A
// line led by a harness name is plumbing at any length. A line led by a
// verb is plumbing when it has at most four words. Everything else is
// talk.
export function classifyTell(line) {
  const words = String(line || '').trim().split(/\s+/).filter(Boolean);
  if (words.length === 0) return 'empty';
  if (HARNESSES.includes(words[0])) return 'plumbing';
  if (VERBS.includes(words[0]) && words.length <= PLUMBING_WORDS) return 'plumbing';
  return 'talk';
}

// createFor is the pane.create request for a harness. With no directory
// known here it sends none, and the server picks one, as New does.
function createFor(harness, args, cwd) {
  const req = { cmd: 'pane.create', kind: 'pty', harness };
  if (cwd) req.cwd = cwd;
  if (args.length) req.args = args;
  if (!cwd) return { req, say: 'Opening ' + harness + ' where you last worked.' };
  return { req, say: 'Opening ' + harness + ' in ' + cwd + '.' };
}

// findPanes is the live top panes that name matches: the one with that
// id, else every one with that label.
function findPanes(panes, name) {
  const live = (panes || []).filter((p) => p && !p.closed && !p.parent_pane);
  const byId = live.filter((p) => p.id === name);
  return byId.length ? byId : live.filter((p) => p.label === name);
}

// countWord is n as a word for two, and as digits above.
function countWord(n) {
  return n === 2 ? 'Two' : String(n);
}

// plumbingCommand turns a plumbing line into one request, or one line
// that says why it sends nothing. A harness name, or open and a harness
// name, opens that harness in cwd. close closes a pane named by id or
// label. Every other verb lives on the terminal floor.
export function plumbingCommand(line, { panes, cwd }) {
  const words = String(line || '').trim().split(/\s+/).filter(Boolean);
  const [first, ...rest] = words;
  if (HARNESSES.includes(first)) return createFor(first, rest, cwd);
  if (first === 'open') {
    if (!HARNESSES.includes(rest[0])) return { say: 'open needs a harness name, one of ' + HARNESSES.join(' ') + '.' };
    return createFor(rest[0], rest.slice(1), cwd);
  }
  if (first === 'close') {
    if (!rest[0]) return { say: 'close needs a pane name, as in close docs.' };
    const found = findPanes(panes, rest[0]);
    if (found.length === 0) return { say: 'No pane ' + rest[0] + '.' };
    // A label two panes share names neither. Closing the first could stop
    // an agent the operator did not mean.
    if (found.length > 1) {
      return { say: countWord(found.length) + ' panes are called ' + rest[0] + '. Close one by id: ' + found.map((p) => p.id).join(' or ') + '.' };
    }
    const pane = found[0];
    return { req: { cmd: 'pane.close', pane: pane.id }, say: 'Closing ' + (pane.label || pane.id) + '.' };
  }
  return { say: first + ' works on the terminal floor. Here, type a harness name, open, close, or a sentence for the foreman.' };
}

// talkCommand turns a sentence into floor.talk. The server starts the
// floor's foreman when none runs and keeps the words in order, so the
// page, the terminal floor and voice never start two.
export function talkCommand(line) {
  return { req: { cmd: 'floor.talk', text: line } };
}

// talkSay is the one line the status shows for a floor.talk reply: the
// server's note when it has one, else where the words went.
export function talkSay(reply) {
  const r = reply || {};
  const label = r.label || 'the foreman';
  if (r.note) return r.note;
  if (r.started) return label + ' starts and gets its page. Your words go once it is ready.';
  if (r.queued > 1) return 'Sent to ' + label + '. ' + r.queued + ' sentences wait for it, yours last.';
  return 'Sent to ' + label + '.';
}

// tellCwd is the directory a harness name opens in: the last one this
// phone used on New, or '' when it has used none. A pane started in the
// wrong repository does real work in the wrong place, so the running
// panes never supply a guess.
function tellCwd() {
  const stored = readStored();
  const last = Array.isArray(stored) ? stored[0] : '';
  return typeof last === 'string' ? last : '';
}

// mountTell wires the tell bar. Send runs the line: plumbing first, then
// talk to the foreman through floor.talk. A line that sends nothing stays
// in the bar with the reason on the status line. A talk that starts the
// foreman opens its window, since a new harness may ask a question of its
// own first. The microphone only fills the bar.
export function mountTell() {
  const form = document.getElementById('tell');
  const box = document.getElementById('tell-text');
  const status = (msg) => window.coppice.status(msg);
  let busy = false;
  const run = async () => {
    const line = String(box.value || '').trim();
    const kind = classifyTell(line);
    if (kind === 'empty' || busy) return;
    busy = true;
    try {
      let plan;
      if (kind === 'plumbing') {
        plan = plumbingCommand(line, { panes: window.coppice.state.panes, cwd: tellCwd() });
      } else {
        plan = talkCommand(line);
      }
      if (!plan.req) {
        status(plan.say);
        return;
      }
      const { cmd, ...fields } = plan.req;
      const reply = await window.coppice.rpc(cmd, fields);
      box.value = '';
      if (cmd === 'floor.talk') {
        status(talkSay(reply));
        if (reply && reply.started && reply.pane) location.hash = '#/pane/' + encodeURIComponent(reply.pane);
        return;
      }
      status(plan.say);
    } catch (e) {
      status(e);
    } finally {
      busy = false;
    }
  };
  form.addEventListener('submit', (e) => {
    if (e && typeof e.preventDefault === 'function') e.preventDefault();
    run();
  });
  mountRecord('tell-mic', 'tell-text', 'tell bar');
}

// Every error or warning the page shows carries a fix: a button that does
// it here, or words that say why the fix is not here. A message is
// {text, actions, away}. actions is a list of {kind, label, ...}; away is
// true when the words say the fix is on the computer that runs coppice.
//
// Every message text that names a coppice or daisugi command lives in this
// file, in MESSAGES, so one test can hold each of them to that rule. That
// includes a command built by joining strings: write the whole command in
// one string here, never 'coppice ' + verb elsewhere. Every entry carries
// an action, says the fix is elsewhere (away), or clears by itself
// (clears), as a wait does. Text
// from the server goes through fromText, which gives a command it names a
// button that types it in a shell.

// AWAY is the words a message uses when its fix is on the box itself.
export const AWAY = 'on the computer that runs coppice';

// NAMES_COMMAND matches text that names a coppice or daisugi command: the
// tool's name, a space, and a word.
export const NAMES_COMMAND = /\b(?:coppice|daisugi) [a-z][a-z-]*/;

// KNOWN are the commands a shell fix may type. A command a text names is
// cut to the longest of these it starts with, so a sentence that goes on
// after the command never gets typed.
export const KNOWN = [
  'coppice server start', 'coppice server status', 'coppice server stop',
  'coppice web token', 'coppice web serve', 'coppice task list',
  'coppice agent allow', 'coppice agent deny', 'coppice pane list',
  'daisugi voice serve', 'daisugi gateway', 'daisugi gate',
];

// commandIn is the command text names, as a shell fix would type it, or
// '' when it names none. A command not in KNOWN is cut to its first two
// words, the tool and its verb.
export function commandIn(text) {
  const m = String(text || '').match(/\b(?:coppice|daisugi)(?: [a-z][a-z-]*(?![\w:]))+/);
  if (!m) return '';
  const said = m[0];
  let best = '';
  for (const k of KNOWN) {
    if ((said === k || said.startsWith(k + ' ')) && k.length > best.length) best = k;
  }
  return best || said.split(' ').slice(0, 2).join(' ');
}

// TYPED_AS_IS are whole lines a shell fix may type although they hold
// quotes and brackets: the install lines for daisugi's voice extra, which
// the server names when voice needs it. Only these exact lines pass.
export const TYPED_AS_IS = [
  "pip install 'opendaisugi[voice]'",
  "pip install --upgrade 'opendaisugi[voice]'",
];

// typeable is true for a line a shell fix may type: words of letters,
// digits and . _ : - only, with no line break and nothing a shell reads
// as more than words. One trailing space may leave room for a word the
// owner types.
export function typeable(line) {
  if (TYPED_AS_IS.includes(line)) return true;
  return typeof line === 'string' && line.length > 0 && line.length <= 300
    && /^[A-Za-z0-9_.:-]+(?: [A-Za-z0-9_.:-]+)* ?$/.test(line);
}

// shell is the action that opens a shell agent with command typed and not
// run.
export const shell = (command, label) => ({ kind: 'shell', label: label || 'Type it in a shell', command });
export const reconnect = (label) => ({ kind: 'reconnect', label: label || 'Retry now' });
export const settings = () => ({ kind: 'settings', label: 'Open Settings' });

// message builds one message. Only text is required. away says the fix is
// on the computer that runs coppice; clears says the message ends by
// itself.
export function message(text, actions, away, clears) {
  const m = { text: String(text || ''), actions: Array.isArray(actions) ? actions.filter(Boolean) : [] };
  if (away) m.away = true;
  if (clears) m.clears = true;
  return m;
}

// MESSAGES are the page's own messages that name a command, or that other
// files share. Each is a function, so one that names an agent takes it.
export const MESSAGES = {
  tokenRejected: () => message('That token is not accepted. ' + cap(AWAY) + ', run coppice web token and scan the QR again.', [], true),
  tokenShape: () => message('That token is not the right shape. Run coppice web token.', [shell('coppice web token')]),
  noToken: () => message('No token. Open Settings and paste one.', [settings()]),
  notConnected: () => message('Not connected. Check the token in Settings.', [reconnect(), settings()]),
  timeout: () => message('The server did not answer in 15 s. Check that coppice-server is running, then try again.', [reconnect()]),
  reconnecting: () => message('Reconnecting.', [reconnect()]),
  banned: () => message('Too many bad tokens. Waiting one minute.', [], false, true),
  // gateNotInstalled is the floor header's hint when no harness has a
  // daisugi gate hook and no agent is guarded. Its button types the
  // command in a shell and does not run it.
  gateNotInstalled: () => message('daisugi is off: no agent is guarded. Run: daisugi install --gate',
    [shell('daisugi install --gate')]),
  // gateOff is what an agent's daisugi details say when no gate hook
  // reports for it. Setting the mode rewrites each harness's hook
  // settings, so it stays a command the owner runs, named in words.
  gateOff: () => message('No gate hook reports for this agent. ' + cap(AWAY) + ', run daisugi install --gate, then start the agent again.', [], true),
  unreadable: () => message('coppice-server sent a reply the phone could not read. Run coppice server status.', [shell('coppice server status')]),
  // shellOffline is what a shell fix says when there is no connection to
  // start a shell with.
  shellOffline: (command) => message('Not connected, so no shell can start here. ' + cap(AWAY) + ', run ' + command + '.', [reconnect()], true),
  // voiceDown is what the microphone says when the server says voice
  // cannot run. command is the line the server says fixes it, typed in a
  // shell and not run. Retry asks the server to start voice again. A fix
  // that is not on this page, a config to edit and the server to start
  // again, says so, with no Retry.
  voiceDown: (text, command, away) => {
    if (away) return message(String(text || '') + ' Do this ' + AWAY + '.', [], true);
    return message(text, [command ? shell(command, 'Type the fix in a shell') : null, voiceRetry()]);
  },
};

// voiceRetry is the action that asks the server to start voice again.
export const voiceRetry = () => ({ kind: 'voice', label: 'Retry' });

function cap(s) {
  return s.charAt(0).toUpperCase() + s.slice(1);
}

// fromText makes a message of text, most often a server's error, or the
// text of one of the page's own messages that lost its fix on the way.
// A command the text names gets a button that types it in a shell.
export function fromText(text) {
  const t = String(text || '');
  // A caller that shows only an error's message still gets the fix of the
  // page's own message with that text.
  for (const fn of Object.values(MESSAGES)) {
    if (fn.length !== 0) continue;
    const m = fn();
    if (m.text === t) return m;
  }
  const cmd = commandIn(t);
  return cmd ? message(t, [shell(cmd)]) : message(t);
}

// toMessage reads whatever a status call was given: a message, an error,
// or a string. An error may carry its own fix as err.fix.
export function toMessage(x) {
  if (x && typeof x === 'object' && !(x instanceof Error) && typeof x.text === 'string') {
    return message(x.text, x.actions, x.away, x.clears);
  }
  if (x instanceof Error || (x && typeof x === 'object' && typeof x.message === 'string')) {
    return x.fix ? toMessage(x.fix) : fromText(x.message);
  }
  return fromText(x === undefined || x === null ? '' : x);
}

// needsFix is true for a message that names a command and has no action
// and no words saying the fix is elsewhere. No message the page shows may
// be one.
export function needsFix(m) {
  if (!m || !NAMES_COMMAND.test(m.text)) return false;
  if (m.actions && m.actions.length > 0) return false;
  return !(m.away && m.text.toLowerCase().includes(AWAY));
}

// shellCwd is the directory a shell fix starts in: the one the action
// names, else the selected agent's, else the first agent's, else the first
// project's, else ''.
export function shellCwd(action, panes, selected, projects) {
  if (action && typeof action.cwd === 'string' && action.cwd) return action.cwd;
  const list = Array.isArray(panes) ? panes : [];
  const sel = list.find((p) => p && p.id === selected && p.cwd);
  if (sel) return sel.cwd;
  const any = list.find((p) => p && p.cwd);
  if (any) return any.cwd;
  const proj = (Array.isArray(projects) ? projects : []).find((p) => p && typeof p.path === 'string' && p.path);
  return proj ? proj.path : '';
}

// openShell starts a shell agent in cwd and types command into it with no
// Enter, so the owner reads it before it runs. deps: rpc, connected(),
// cwd() a promise of the directory, made(pane). It returns the message to
// show.
export async function openShell(action, deps) {
  const command = action && action.command;
  if (!typeable(command)) return message('That fix holds characters this page will not type. Type it yourself ' + AWAY + '.', [], true);
  if (!deps.connected()) return MESSAGES.shellOffline(command);
  const cwd = await deps.cwd();
  if (!cwd) return message('No directory is known to start a shell in. Start an agent first.', [{ kind: 'new', label: 'New agent' }]);
  const made = await deps.rpc('pane.create', { kind: 'pty', cwd, cmd_argv: ['sh'], label: 'fix' });
  const pane = made && made.pane;
  if (!pane) return message('The shell did not start.', [shell(command)]);
  await deps.rpc('pane.send_text', { pane, text: command, enter: false });
  if (deps.made) deps.made(pane);
  return message('A shell has the command typed. Read it, then press Enter to run it.');
}

// fixButtons makes one button for each action of m. run gets the action
// a click picks.
export function fixButtons(m, run) {
  return (m && Array.isArray(m.actions) ? m.actions : []).map((a) => {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'fix';
    b.textContent = a.label;
    b.addEventListener('click', (e) => {
      if (e && typeof e.stopPropagation === 'function') e.stopPropagation();
      run(a);
    });
    return b;
  });
}

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
  'coppice web token', 'coppice web serve', 'coppice web cert tailscale', 'coppice task list',
  'coppice agent allow', 'coppice agent deny', 'coppice pane list',
  'daisugi voice serve', 'daisugi router serve', 'daisugi warden',
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
export const resumeAll = () => ({ kind: 'resume_all', label: 'Resume all' });
export const showRecent = () => ({ kind: 'show_recent', label: 'Show them' });

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
  // reconnecting is what the page says while it reconnects. A page that
  // cannot reach the box at all says cannotReach instead (app.js).
  reconnecting: () => message('Reconnecting.', [reconnect()]),
  // micNeedsHttps is what the mic says on a page that is not a secure
  // context: the browser gives it no microphone, and the fix is on the box.
  micNeedsHttps: () => message('The mic needs https here. ' + cap(AWAY) + ', run coppice web serve --tls tailscale, then open the address it prints on this phone.', [], true),
  // phoneCert is the header's warning when the phone's tailscale
  // certificate is close to its end. Renew types the command in a shell.
  phoneCert: (days) => message(phoneCertText(days), [shell('coppice web cert tailscale', 'Renew')]),
  // restarted is the one line after a start that found agents which
  // stopped with the old server and can come back.
  restarted: (n) => {
    const k = Number(n) || 0;
    return message(k + (k === 1 ? ' agent' : ' agents') + ' stopped when coppice restarted.', [resumeAll(), showRecent()]);
  },
  banned: () => message('Too many bad tokens. Waiting one minute.', [], false, true),
  // wardenNotInstalled is the floor header's hint when no harness has a
  // daisugi warden hook and no agent is guarded. Its button types the
  // command in a shell and does not run it.
  wardenNotInstalled: () => message('daisugi is off: no agent is guarded. Run: daisugi install',
    [shell('daisugi install')]),
  // wardenOff is what an agent's daisugi details say when no warden hook
  // reports for it. Setting the mode rewrites each harness's hook
  // settings, so it stays a command the owner runs, named in words.
  wardenOff: () => message('No warden hook reports for this agent. ' + cap(AWAY) + ', run daisugi install, then start the agent again.', [], true),
  // modeOff is what the daisugi details say under the mode row: the mode
  // is set there, but turning the warden off stays a typed command.
  modeOff: () => message('Turning the warden off stays a command. ' + cap(AWAY) + ', run daisugi warden disarm.', [], true),
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
  // voiceNoEngine is what the mic says when the voice server runs but has no
  // speech engine. Retry cannot help: the engine is installed on the
  // computer that runs coppice.
  voiceNoEngine: () => message('No speech engine here. ' + cap(AWAY) + ', run scripts/install.sh --voice, then tap Mic again.', [], true),
  // voiceFetching is what the mic says while voice gets its speech model:
  // bytes done and in all, in megabytes (10**6, rounded half up, as
  // daisugi voice status rounds). voiceReady ends it.
  voiceFetching: (done, total) => message('Getting the speech model, ' + megabytes(done) + ' of ' + megabytes(total) + ' MB.', [], false, true),
  voiceReady: () => message('Voice is ready.', [], false, true),
  // clipKept is what the mic says when a clip did not become text. The
  // recording is kept in the page, and run holds what the buttons do:
  // send it again, save the audio, or let it go.
  clipKept: (why, run) => message(String(why || 'The clip did not send.') + ' Your recording is kept.',
    [clipAction('Send again', (run || {}).again), clipAction('Save', (run || {}).save)]),
  // clipPending is what the mic says on a tap to record while an earlier
  // clip still has no text. A new clip must not push that one away.
  clipPending: (run) => message('The last clip has no text yet. Send it again, save it, or discard it, then record.',
    [clipAction('Send again', (run || {}).again), clipAction('Save', (run || {}).save), clipAction('Discard', (run || {}).discard)]),
  // clipRecovered is the one line on a page that loads with a clip kept
  // from before: the page was reloaded or dropped while it sent.
  clipRecovered: (run) => message('A recording from before has no text yet. Send it again, save it, or discard it.',
    [clipAction('Send again', (run || {}).again), clipAction('Save', (run || {}).save), clipAction('Discard', (run || {}).discard)]),
  // clipSaved is the line after Save. The clip stays until the owner lets
  // it go, since a download can fail without an error.
  clipSaved: (run) => message('Saved to your downloads. Discard the recording once you have the file.',
    [clipAction('Send again', (run || {}).again), clipAction('Discard', (run || {}).discard)]),
  // clipGaps is the line when the text is in the box but some of the audio
  // was not heard. The clip stays.
  clipGaps: (n, run, cutS = 0) => message(
    (n === 1 ? '1 part' : n + ' parts') + ' of the recording could not be heard, and the text has '
    + (n === 1 ? 'a gap' : 'gaps') + '.'
    + (cutS > 0 ? ' The last ' + cutS + ' s was past the limit and was not turned into text.' : '')
    + ' Your recording is kept.',
    [clipAction('Send again', (run || {}).again), clipAction('Save', (run || {}).save), clipAction('Discard', (run || {}).discard)]),
  // clipCut is the line when the server read only the first part of a clip
  // that ran past its limit. Sending again would give the same text.
  clipCut: (seconds, run) => message(
    'The last ' + seconds + ' s of the recording was past the limit and was not turned into text. Your recording is kept.',
    [clipAction('Save', (run || {}).save), clipAction('Discard', (run || {}).discard)]),
  // clipLimit is the line when a recording reaches the server's cap.
  clipLimit: (seconds) => message('Reached the ' + Math.round(seconds / 60) + ' minute limit. Sending what was recorded.', [], false, true),
  // clipNearLimit is the warning before the cap.
  clipNearLimit: (left) => message(clockWords(left) + ' left. A clip stops at the limit and is sent.', [], false, true),
  // clipWorking is the line while a long clip is turned into text.
  clipWorking: (seconds) => message('Turning ' + Math.round(seconds) + ' s of speech into text. A long clip takes a while.', [], false, true),
};

// clockWords is a count of seconds in words.
function clockWords(seconds) {
  const s = Math.max(0, Math.round(seconds));
  if (s >= 60) return Math.round(s / 60) + (Math.round(s / 60) === 1 ? ' minute' : ' minutes');
  return s + ' s';
}

// clipAction is an action that runs a function held by the page that made
// it. fix() runs it as it is.
export const clipAction = (label, fn) => ({ kind: 'run', label, run: typeof fn === 'function' ? fn : () => {} });

// megabytes is a byte count in megabytes, rounded half up.
export function megabytes(n) {
  return Math.floor((Number(n) + 500000) / 1000000);
}

// voiceRetry is the action that asks the server to start voice again.
export const voiceRetry = () => ({ kind: 'voice', label: 'Retry' });

// pageHost is the host name the page was loaded from, or '' when there is
// no location to read.
export function pageHost() {
  try {
    return (typeof location !== 'undefined' && location && location.hostname) || '';
  } catch {
    return '';
  }
}

// isOwnBox is true for a host name that is this computer: the page then
// runs on the box itself, so there is no other machine to wake.
function isOwnBox(host) {
  const h = String(host || '').replace(/^\[|\]$/g, '');
  return h === '' || h === 'localhost' || h === '::1' || /^127\./.test(h);
}

// cannotReach is the line the page shows while it cannot reach the box.
// On a phone it says the two fixes that are on the phone: Tailscale on,
// when the page came from a .ts.net name, and the box asleep or off. On
// the box itself it only says it reconnects. Both carry Retry.
export function cannotReach(host) {
  if (isOwnBox(host)) return message('Reconnecting.', [reconnect()]);
  const ts = /\.ts\.net\.?$/i.test(String(host)) ? ' Turn on Tailscale on this phone.' : '';
  return message('Cannot reach the box.' + ts + ' The box may be asleep or off.', [reconnect()]);
}

// phoneCertText is the warning for days left on the phone's certificate.
function phoneCertText(days) {
  const n = Number(days);
  if (!Number.isFinite(n)) return 'The phone certificate runs out soon.';
  if (n < 0) return 'The phone certificate has run out. The phone cannot reach the box until it is renewed.';
  if (n === 0) return 'The phone certificate runs out today.';
  return 'The phone certificate runs out in ' + n + (n === 1 ? ' day.' : ' days.');
}

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

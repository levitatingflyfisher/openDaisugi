import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import {
  MESSAGES, NAMES_COMMAND, AWAY, TYPED_AS_IS, commandIn, typeable, fromText, toMessage, needsFix, shellCwd, openShell,
} from '../messages.js';

const here = dirname(fileURLToPath(import.meta.url));
const staticDir = join(here, '..');

// literals is every string a JS source holds: quoted strings, and each
// text part of a template literal. Comments and regex literals are
// skipped, so a comment that names a command is not a message.
export function literals(src) {
  const out = [];
  let i = 0;
  // prev is the last code character that was not space, which says
  // whether a / starts a regex or divides.
  let prev = '';
  let word = '';
  const regexAfter = (p, w) => p === '' || '(,=:[!&|?{};+-*%<>~^'.includes(p) || ['return', 'typeof', 'case', 'in', 'of', 'void', 'delete'].includes(w);
  const readString = (q) => {
    let s = '';
    i += 1;
    while (i < src.length && src[i] !== q) {
      if (src[i] === '\\') { s += src[i + 1]; i += 2; continue; }
      s += src[i];
      i += 1;
    }
    i += 1;
    return s;
  };
  // readTemplate reads a template from its backtick, and each ${ } in it
  // as code, until its closing backtick.
  const readTemplate = () => {
    i += 1;
    let s = '';
    while (i < src.length && src[i] !== '`') {
      if (src[i] === '\\') { s += src[i + 1]; i += 2; continue; }
      if (src[i] === '$' && src[i + 1] === '{') {
        out.push(s);
        s = '';
        i += 2;
        code(1);
        continue;
      }
      s += src[i];
      i += 1;
    }
    out.push(s);
    i += 1;
  };
  // code reads code until the brace depth drops below its start, or the
  // end.
  const code = (depth) => {
    while (i < src.length) {
      const c = src[i];
      if (c === '/' && src[i + 1] === '/') { while (i < src.length && src[i] !== '\n') i += 1; continue; }
      if (c === '/' && src[i + 1] === '*') { i = src.indexOf('*/', i + 2); i = i < 0 ? src.length : i + 2; continue; }
      if (c === '"' || c === "'") { out.push(readString(c)); prev = 'x'; word = ''; continue; }
      if (c === '`') { readTemplate(); prev = 'x'; word = ''; continue; }
      if (c === '/' && regexAfter(prev, word)) {
        i += 1;
        let inClass = false;
        while (i < src.length && (src[i] !== '/' || inClass)) {
          if (src[i] === '\\') { i += 2; continue; }
          if (src[i] === '[') inClass = true;
          if (src[i] === ']') inClass = false;
          i += 1;
        }
        i += 1;
        prev = 'x';
        word = '';
        continue;
      }
      if (c === '{') depth += 1;
      if (c === '}') {
        depth -= 1;
        if (depth === 0) { i += 1; return; }
      }
      if (/\s/.test(c)) { i += 1; continue; }
      if (/[A-Za-z0-9_$]/.test(c)) word = (/[A-Za-z0-9_$]/.test(prev) ? word : '') + c;
      else word = '';
      prev = c;
      i += 1;
    }
  };
  code(Infinity);
  return out;
}

test('the scanner finds strings and template text and skips comments and regexes', () => {
  const src = [
    "// run coppice web token here",
    "/* coppice server start */",
    "const a = 'one' + \"two\";",
    "const r = /coppice web/;",
    "const t = `head ${x ? 'in' : `deep ${y}`} tail`;",
    "const d = a / 2 / 3;",
  ].join('\n');
  assert.deepEqual(literals(src), ['one', 'two', 'head ', 'in', 'deep ', '', ' tail']);
});

// namesCommand is true for a string that names a command, or that ends in
// the tool's name and a space, the head of a command built by joining
// strings, such as 'Run coppice ' + verb.
const namesCommand = (s) => NAMES_COMMAND.test(s) || /\b(?:coppice|daisugi) $/.test(s);

// PLUGIN_ALLOW holds the plugin strings that may name a command, such as a
// view's own title. A plugin runs in a sandboxed frame with no fix to
// offer, so each one here must say what to do in words.
const PLUGIN_ALLOW = new Set([]);

// The files the scan reads: every client file but messages.js, and every
// plugin script, the shared library among them.
function scanned() {
  const out = readdirSync(staticDir).filter((f) => f.endsWith('.js') && f !== 'messages.js').map((f) => join(staticDir, f));
  const plugins = join(staticDir, '..', '..', '..', 'plugins');
  for (const d of readdirSync(plugins, { withFileTypes: true })) {
    if (!d.isDirectory()) continue;
    for (const f of readdirSync(join(plugins, d.name))) if (f.endsWith('.js')) out.push(join(plugins, d.name, f));
  }
  return out;
}

// A string that names a coppice or daisugi command would be a message with
// no place to carry its fix, so it must move into MESSAGES.
test('no client file but messages.js, and no plugin, holds a string that names a command', () => {
  const files = scanned();
  assert.ok(files.length > 20, 'the scan found too few files: ' + files.length);
  assert.ok(files.some((f) => f.includes('plugins')), 'the scan read no plugin');
  const bad = [];
  for (const f of files) {
    for (const s of literals(readFileSync(f, 'utf8'))) {
      if (namesCommand(s) && !(f.includes('plugins') && PLUGIN_ALLOW.has(s))) bad.push(f + ': ' + JSON.stringify(s));
    }
  }
  assert.deepEqual(bad, [], 'move these into MESSAGES in messages.js with an action');
});

test('the scan catches a command built by joining strings', () => {
  assert.equal(namesCommand('Run coppice '), true);
  assert.equal(namesCommand('coppice-server is down'), false);
});

// Every message the page owns is fixable here, says the fix is elsewhere,
// or clears by itself, as a wait does.
test('every message in MESSAGES has an action, says the fix is elsewhere, or clears by itself', () => {
  const calls = [['w1:p1', 'toolu_1'], ['w1:p1', 'x;y'], ['coppice server status']];
  for (const [name, fn] of Object.entries(MESSAGES)) {
    for (const args of calls) {
      const m = fn(...args);
      assert.ok(m.actions.length > 0 || m.away || m.clears, name + ' has no fix: ' + m.text);
    }
  }
});

test('every message in MESSAGES that names a command carries an action or says the fix is elsewhere', () => {
  const samples = ['w1:p1', 'toolu_1'];
  for (const [name, fn] of Object.entries(MESSAGES)) {
    const m = fn(...samples);
    assert.equal(typeof m.text, 'string', name);
    assert.ok(!needsFix(m), name + ' names a command with no fix: ' + m.text);
    for (const a of m.actions) {
      assert.ok(a.kind && a.label, name + ' has an action with no kind or label');
      if (a.kind === 'shell') assert.ok(typeable(a.command), name + ' types ' + a.command);
    }
    if (m.away) assert.ok(m.text.toLowerCase().includes(AWAY), name + ' says away but not where');
  }
});

test('needsFix flags a command with no action and no words about where', () => {
  assert.equal(needsFix({ text: 'Run coppice web token.', actions: [] }), true);
  assert.equal(needsFix({ text: 'Run coppice web token.', actions: [], away: true }), true);
  assert.equal(needsFix({ text: 'Run coppice web token ' + AWAY + '.', actions: [], away: true }), false);
  assert.equal(needsFix({ text: 'Stopped reader.', actions: [] }), false);
  assert.equal(needsFix({ text: 'coppice-server is down.', actions: [] }), false);
});

test('commandIn cuts a sentence to the command it names', () => {
  assert.equal(commandIn('Run daisugi voice serve, then start coppice web serve again with --voice-url.'), 'daisugi voice serve');
  assert.equal(commandIn('Start coppice web serve again with --voice-url.'), 'coppice web serve');
  assert.equal(commandIn('no task t9. Run: coppice task list'), 'coppice task list');
  assert.equal(commandIn('Answer: coppice agent allow w1:p1 toolu_1'), 'coppice agent allow');
  assert.equal(commandIn('Run coppice frob the widget'), 'coppice frob');
  assert.equal(commandIn('coppice-server did not answer.'), '');
  assert.equal(commandIn(''), '');
});

test('typeable takes plain words and ids and refuses anything a shell reads as more', () => {
  assert.equal(typeable('coppice agent deny w1:p1 toolu_1'), true);
  assert.equal(typeable('coppice agent allow a b --confirm '), true, 'one trailing space leaves room for the name');
  assert.equal(typeable('coppice agent allow a b --confirm  '), false);
  for (const bad of ['', 'a; rm -rf ~', 'a\nb', 'a\rb', 'a | sh', 'a $(x)', 'a `x`', "a 'b'", 'a  b', 'x'.repeat(301)]) {
    assert.equal(typeable(bad), false, JSON.stringify(bad));
  }
});

test('server text that names a command gets a shell fix', () => {
  const m = fromText('coppice-server did not answer. Run coppice server status.');
  assert.deepEqual(m.actions, [{ kind: 'shell', label: 'Type it in a shell', command: 'coppice server status' }]);
  assert.deepEqual(fromText('Stopped reader.').actions, []);
  assert.equal(MESSAGES.voiceOff, undefined, 'no server sends the old voice sentence any more');
});

test('the text alone of each message with no arguments still carries its fixes', () => {
  for (const [name, fn] of Object.entries(MESSAGES)) {
    if (fn.length !== 0) continue;
    const m = fn();
    assert.deepEqual(fromText(m.text), m, name + ' lost its fix when shown as text');
    assert.deepEqual(toMessage(new Error(m.text)), m, name + ' lost its fix when shown as an error');
  }
  assert.deepEqual(fromText('Not connected. Check the token in Settings.').actions.map((a) => a.kind), ['reconnect', 'settings']);
});

test('toMessage reads a message, an error with its own fix, an error and a string', () => {
  assert.equal(toMessage('Allowed.').text, 'Allowed.');
  assert.equal(toMessage(undefined).text, '');
  const e = new Error('The server did not answer in 15 s.');
  e.fix = MESSAGES.timeout();
  assert.equal(toMessage(e).actions[0].kind, 'reconnect');
  assert.equal(toMessage(new Error('Run coppice task list')).actions[0].command, 'coppice task list');
  assert.equal(toMessage(MESSAGES.noToken()).actions[0].kind, 'settings');
});

test('a shell fix starts where the action says, else near the selected agent, else anywhere known', () => {
  const panes = [{ id: 'a', cwd: '/p/a' }, { id: 'b', cwd: '/p/b' }];
  assert.equal(shellCwd({ cwd: '/x' }, panes, 'b', []), '/x');
  assert.equal(shellCwd({}, panes, 'b', []), '/p/b');
  assert.equal(shellCwd({}, panes, '', []), '/p/a');
  assert.equal(shellCwd({}, [], '', [{ path: '/proj' }]), '/proj');
  assert.equal(shellCwd({}, [], '', []), '');
});

test('openShell starts a shell and types the command with no Enter', async () => {
  const calls = [];
  const rpc = async (cmd, f) => { calls.push([cmd, f]); return cmd === 'pane.create' ? { pane: 'w1:p9' } : {}; };
  let made = '';
  const m = await openShell({ kind: 'shell', command: 'coppice server status' },
    { rpc, connected: () => true, cwd: async () => '/p/a', made: (p) => { made = p; } });
  assert.deepEqual(calls, [
    ['pane.create', { kind: 'pty', cwd: '/p/a', cmd_argv: ['sh'], label: 'fix' }],
    ['pane.send_text', { pane: 'w1:p9', text: 'coppice server status', enter: false }],
  ]);
  assert.equal(made, 'w1:p9');
  assert.match(m.text, /press Enter/);
});

test('openShell with no connection starts nothing and says where the fix is', async () => {
  let called = false;
  const m = await openShell({ kind: 'shell', command: 'coppice web token' },
    { rpc: async () => { called = true; }, connected: () => false, cwd: async () => '/p' });
  assert.equal(called, false);
  assert.ok(m.text.toLowerCase().includes(AWAY));
  assert.ok(!needsFix(m));
});

test('openShell refuses a command it will not type', async () => {
  let called = false;
  const m = await openShell({ kind: 'shell', command: 'a; rm -rf ~' },
    { rpc: async () => { called = true; }, connected: () => true, cwd: async () => '/p' });
  assert.equal(called, false);
  assert.deepEqual(m.actions, []);
});

// The failure this names: the voice extra's install line holds quotes and
// brackets, so no shell fix could type it. The exact lines in
// TYPED_AS_IS may be typed; nothing like them may.
test('typeable takes the voice install lines exactly and nothing near them', () => {
  for (const line of TYPED_AS_IS) assert.equal(typeable(line), true, line);
  assert.equal(typeable("pip install 'opendaisugi[voice]'; rm -rf ~"), false);
  assert.equal(typeable("pip install 'other[voice]'"), false);
});

test('voiceDown types the install line, and a fix on the box says so', () => {
  const m = MESSAGES.voiceDown("Voice needs daisugi's voice extra. Install it, then press Retry.",
    "pip install 'opendaisugi[voice]'");
  assert.deepEqual(m.actions.map((a) => [a.kind, a.command]),
    [['shell', "pip install 'opendaisugi[voice]'"], ['voice', undefined]]);
  assert.ok(!needsFix(m));
  const off = MESSAGES.voiceDown('Voice is off in coppice.toml. Remove [voice] enabled = false there and start the coppice server again.', '', true);
  assert.deepEqual(off.actions, []);
  assert.ok(off.text.includes(AWAY), off.text);
  assert.ok(!needsFix(off));
  const plain = MESSAGES.voiceDown('Voice is starting. Wait a moment, then press Retry.', '');
  assert.deepEqual(plain.actions, [{ kind: 'voice', label: 'Retry' }]);
  assert.ok(!needsFix(plain));
});

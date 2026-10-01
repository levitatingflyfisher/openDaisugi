import test from 'node:test';
import assert from 'node:assert/strict';
import { reset, element } from './browser-stub.mjs';
import { cleanTalk, chipParts, chatRows, chatTime, lastReply, DESK_LINE, CHAT_REFUSED, mountChat, CHAT_EMPTY, CHAT_NAMED, READING_WHO } from '../chat.js';

// The chat with the foreman on the phone's home. The page reads floor.chat
// again on each messages event for the chat. A sentence the owner sends
// shows at once, as sent once floor.talk answers, and as read once the
// server marks its line read: the foreman's transcript recorded it. No chat
// text is stored on the phone.

const settle = () => new Promise((r) => setImmediate(r));

const PANES = [
  { id: 'w1:p1', label: 'foreman', cwd: '/f', state: 'idle' },
  { id: 'w1:p2', label: 'glean-import', cwd: '/g', state: 'blocked' },
  { id: 'w1:p3', label: 'glean', cwd: '/g', state: 'working' },
  { id: 'w1:p4', label: 'old', cwd: '/o', state: 'done', closed: true },
];

const owner = (id, text, pane = 'w1:p1', at = 10, read = false) => ({ id, at, role: 'owner', text, pane, read });
const agent = (id, text, pane = 'w1:p1', at = 20, tool) => ({ id, at, role: 'agent', text, pane, ...(tool ? { tool } : {}) });

test('cleanTalk reads a sentence the way the server writes it to the chat', () => {
  assert.equal(cleanTalk('hello\tforeman\u0007'), 'hello foreman');
  assert.equal(cleanTalk('  two\n\nlines  '), 'two lines');
  assert.equal(cleanTalk(''), '');
});

test('a live agent named in a reply is a chip, the longest name first and whole words only', () => {
  const parts = chipParts('Started glean-import and glean, not gleaner or old.', PANES);
  assert.deepEqual(parts, [
    { text: 'Started ' },
    { chip: true, pane: 'w1:p2', label: 'glean-import' },
    { text: ' and ' },
    { chip: true, pane: 'w1:p3', label: 'glean' },
    { text: ', not gleaner or old.' },
  ]);
  assert.deepEqual(chipParts('ask w1:p3 now', PANES), [{ text: 'ask ' }, { chip: true, pane: 'w1:p3', label: 'glean' }, { text: ' now' }]);
  assert.deepEqual(chipParts('nothing here', PANES), [{ text: 'nothing here' }]);
  assert.deepEqual(chipParts('', PANES), []);
});

test('an owner line is read only when the server says so, else sent', () => {
  const rows = chatRows([owner('c1', 'one', 'w1:p1', 10, true), agent('a1', 'ok'), owner('c2', 'two', 'w1:p1', 30)], []);
  assert.deepEqual(rows.map((r) => [r.key, r.who, r.mark]), [['c1', 'you', 'read'], ['a1', 'foreman', ''], ['c2', 'you', 'sent']]);
  // A reply after the line is not a read mark: a sentence queued behind a
  // long turn has replies after it from the turn before.
  const queued = chatRows([owner('c1', 'one'), agent('a1', 'still on the last one')], []);
  assert.equal(queued[0].mark, 'sent');
  // A line with no read field from the server reads as sent.
  const bare = chatRows([{ id: 'c9', at: 1, role: 'owner', text: 'x', pane: 'w1:p1' }], []);
  assert.equal(bare[0].mark, 'sent');
  // A note line has no speaker and no mark.
  const note = chatRows([{ id: 'n', at: 5, role: 'note', text: 'A new foreman started.', pane: 'w1:p1' }], []);
  assert.deepEqual([note[0].who, note[0].mark, note[0].role], ['', '', 'note']);
});

test('a sentence shows at once, and the server copy takes its place', () => {
  const local = [{ id: 1, text: 'hello\tfore', state: 'sending', before: 0 }];
  let rows = chatRows([owner('c0', 'hi')], local);
  assert.deepEqual(rows.map((r) => [r.text, r.mark]), [['hi', 'sent'], ['hello\tfore', 'sending']]);
  local[0].state = 'sent';
  rows = chatRows([owner('c0', 'hi')], local);
  assert.equal(rows[1].mark, 'sent');
  // The server wrote the line: one row, the server's.
  rows = chatRows([owner('c0', 'hi'), owner('c1', 'hello fore')], local);
  assert.deepEqual(rows.map((r) => r.key), ['c0', 'c1']);
  // The same words said twice: the older server line does not take the
  // place of the new one.
  const again = [{ id: 2, text: 'yes', state: 'sent', before: 1 }];
  rows = chatRows([owner('c0', 'yes')], again);
  assert.equal(rows.length, 2);
  rows = chatRows([owner('c0', 'yes'), owner('c3', 'yes', 'w1:p1', 40)], again);
  assert.equal(rows.length, 2);
});

test('a tool use is one dim line, and a time shows as hours and minutes', () => {
  const rows = chatRows([agent('t', 'pytest -q', 'w1:p1', 20, 'Bash')], []);
  assert.equal(rows[0].tool, 'Bash');
  assert.match(chatTime(0), /^$/);
  assert.match(chatTime(1757300000), /^\d\d:\d\d$/);
});

// mount builds the chat with a fake rpc. replies maps a command to what it
// answers, or an Error it throws.
function mount({ chat = { messages: [], more: false }, me = '', on = true } = {}) {
  reset();
  const calls = [];
  const opened = [];
  let reply = chat;
  const c = mountChat({
    list: element('chat-list'),
    rpc: async (cmd, fields) => {
      calls.push({ cmd, ...fields });
      if (reply instanceof Error) throw reply;
      return reply;
    },
    panes: () => PANES,
    open: (pane) => opened.push(pane),
    me: () => me,
    on: () => on,
  });
  return { c, calls, opened, set: (r) => { reply = r; } };
}

const texts = (el) => el.children.map((li) => li.textContent);

test('the chat reads floor.chat and draws each message, a reply with its chips', async () => {
  const { c, calls, opened } = mount({ chat: { messages: [owner('c1', 'start glean', 'w1:p1', 10, true), agent('a1', 'Started glean-import.')], more: false } });
  await c.read();
  assert.deepEqual(calls, [{ cmd: 'floor.chat', limit: 100 }]);
  const list = element('chat-list');
  assert.equal(list.children.length, 2);
  assert.match(list.children[0].textContent, /you.*start glean/);
  assert.match(list.children[0].textContent, /read/);
  const chip = list.children[1].querySelector('.chip-agent');
  assert.ok(chip, 'no chip for glean-import');
  assert.equal(chip.dataset.pane, 'w1:p2');
  chip.dispatchEvent({ type: 'click' });
  assert.deepEqual(opened, ['w1:p2']);
});

test('a new message is added at the end and the drawn ones stay, so only it is announced', async () => {
  const { c, set } = mount({ chat: { messages: [owner('c1', 'one')], more: false } });
  await c.read();
  const first = element('chat-list').children[0];
  set({ messages: [owner('c1', 'one'), owner('c2', 'two', 'w1:p1', 30)], more: false });
  await c.read();
  assert.equal(element('chat-list').children.length, 2);
  assert.equal(element('chat-list').children[0], first, 'the old line was drawn again');
  // A mark that changes, read from the server, draws that line again.
  set({ messages: [owner('c1', 'one', 'w1:p1', 10, true), owner('c2', 'two', 'w1:p1', 30), agent('a1', 'ok', 'w1:p1', 40)], more: false });
  await c.read();
  assert.match(element('chat-list').children[0].textContent, /read/);
});

test('markup in a reply or in an agent label draws as text, never as elements', async () => {
  const evil = '<img src=x onerror=alert(1)>';
  const panes = [...PANES, { id: 'w1:p9', label: '<b>bold</b>', cwd: '/b', state: 'idle' }];
  reset();
  const c = mountChat({
    list: element('chat-list'),
    rpc: async () => ({ messages: [agent('a1', evil + ' and <b>bold</b> ran'), owner('c1', '<script>x()</script>')], more: false }),
    panes: () => panes,
    open: () => {},
    me: () => '',
    on: () => true,
  });
  await c.read();
  const list = element('chat-list');
  assert.ok(list.textContent.includes(evil), list.textContent);
  assert.ok(list.textContent.includes('<script>x()</script>'));
  const chip = list.querySelector('.chip-agent');
  assert.equal(chip.textContent, '<b>bold</b> ›');
  for (const tag of ['img', 'b', 'script']) assert.equal(list.querySelectorAll(tag).length, 0, 'a ' + tag + ' element was made');
});

test('an empty chat says what to do', async () => {
  const { c } = mount();
  await c.read();
  assert.deepEqual(texts(element('chat-list')), [CHAT_EMPTY]);
});

test('a named sign-in gets one line and never reads the chat', async () => {
  const { c, calls } = mount({ me: 'kitchen' });
  await c.read();
  assert.equal(calls.length, 0);
  assert.deepEqual(texts(element('chat-list')), [CHAT_NAMED]);
});

test('a refused read shows the reason in the chat', async () => {
  const { c, set } = mount();
  const e = new Error('cannot read the chat in /d/chat.');
  e.code = 'internal';
  set(e);
  await c.read();
  assert.match(element('chat-list').textContent, /cannot read the chat/);
});

test('a chat that is not on screen is not read', async () => {
  const { c, calls } = mount({ on: false });
  await c.read();
  c.onEvent({ event: 'messages', pane: 'w1:p1', chat: true });
  await settle();
  assert.equal(calls.length, 0);
});

test('a messages event for the chat reads it again, once for a burst; other events do not', async () => {
  const { c, calls } = mount();
  c.onEvent({ event: 'messages', pane: 'w1:p2' });
  c.onEvent({ event: 'state', pane: 'w1:p1' });
  await settle();
  assert.equal(calls.length, 0);
  c.onEvent({ event: 'messages', pane: 'w1:p1', chat: true });
  c.onEvent({ event: 'messages', pane: 'w1:p1', chat: true });
  c.onEvent({ event: 'messages', pane: 'w1:p1', chat: true });
  await settle();
  await settle();
  assert.equal(calls.length, 1);
});

test('a sent line shows sending, then sent, then read when the server says the foreman took it', async () => {
  const { c, set } = mount({ chat: { messages: [], more: false } });
  await c.read();
  const line = c.sent('check the tests');
  assert.match(element('chat-list').textContent, /sending.*check the tests/);
  line.ok();
  assert.match(element('chat-list').textContent, /sent.*check the tests/);
  set({ messages: [owner('c1', 'check the tests')], more: false });
  await c.read();
  assert.equal(element('chat-list').children.length, 1);
  assert.match(element('chat-list').textContent, /sent/);
  set({ messages: [owner('c1', 'check the tests', 'w1:p1', 10, true), agent('a1', 'On it.')], more: false });
  await c.read();
  assert.match(element('chat-list').children[0].textContent, /read/);
});

test('a line floor.talk refused leaves the chat', async () => {
  const { c } = mount();
  await c.read();
  const line = c.sent('fix it');
  line.fail();
  assert.deepEqual(texts(element('chat-list')), [CHAT_EMPTY]);
});

test('no chat text goes to localStorage', async () => {
  const { c, set } = mount();
  const stored = [];
  const real = global.localStorage.setItem;
  global.localStorage.setItem = (k, v) => { stored.push(String(k) + '=' + String(v)); real(k, v); };
  try {
    set({ messages: [owner('c1', 'SECRET-OWNER-WORDS'), agent('a1', 'SECRET-REPLY-WORDS')], more: false });
    await c.read();
    c.sent('SECRET-SENT-WORDS').ok();
    c.onEvent({ event: 'messages', pane: 'w1:p1', chat: true });
    await settle();
    await settle();
  } finally {
    global.localStorage.setItem = real;
  }
  assert.ok(!stored.some((s) => /SECRET/.test(s)), 'chat text was stored: ' + stored.join(' | '));
});

// The reading view draws one agent's transcript with the same list.

test('a transcript list names the agent, leaves off the sent and read marks, and makes no chips', async () => {
  reset();
  const list = element('reading-list');
  const calls = [];
  const rpc = async (cmd, fields) => {
    calls.push({ cmd, ...fields });
    return { messages: [owner('t/1', 'fix test_sync'), agent('t/2', 'I ran w1:p3 and it passed.', 'w1:p9')], source: 'transcript', more: false };
  };
  const chat = mountChat({
    list, rpc, panes: () => PANES, open: () => {}, cmd: 'pane.messages', fields: () => ({ pane: 'w1:p9' }),
    match: (m) => m.pane === 'w1:p9', who: READING_WHO, marks: false, chips: false, empty: 'No messages yet.',
  });
  await chat.read();
  assert.deepEqual(calls, [{ cmd: 'pane.messages', pane: 'w1:p9', limit: 100 }]);
  const heads = list.querySelectorAll('.msg-head').map((h) => h.textContent);
  assert.deepEqual(heads.map((h) => h.replace(/\d\d:\d\d/, '').trim()), ['you', 'agent']);
  assert.equal(list.querySelectorAll('.chip-agent').length, 0, 'a transcript reply made a chip');
  assert.equal(list.querySelectorAll('.mark').length, 0, 'a transcript line carries a mark');
  assert.match(list.textContent, /I ran w1:p3 and it passed\./);
});

test('a transcript list reads again on its own pane\'s messages event only', async () => {
  reset();
  const list = element('reading-list');
  let reads = 0;
  const chat = mountChat({
    list, rpc: async () => { reads += 1; return { messages: [] }; }, panes: () => [], open: () => {},
    cmd: 'pane.messages', fields: () => ({ pane: 'w1:p9' }), match: (m) => m.pane === 'w1:p9',
  });
  chat.onEvent({ event: 'messages', pane: 'w1:p8' });
  await settle();
  assert.equal(reads, 0, 'another pane\'s event read the list');
  chat.onEvent({ event: 'messages', pane: 'w1:p9' });
  await settle();
  assert.equal(reads, 1);
});

test('seed shows messages already read, and reset empties the list for another agent', () => {
  reset();
  const list = element('reading-list');
  const chat = mountChat({ list, rpc: async () => ({ messages: [] }), panes: () => [], open: () => {}, cmd: 'pane.messages', fields: () => ({}), empty: 'None yet.' });
  chat.seed([owner('a', 'hello there')]);
  assert.match(list.textContent, /hello there/);
  chat.reset();
  assert.equal(list.children.length, 0);
  chat.seed([]);
  assert.match(list.textContent, /None yet\./);
});

test('lastReply is the newest reply in words: no tool line, no owner line', () => {
  assert.equal(lastReply([]), '');
  assert.equal(lastReply(undefined), '');
  assert.equal(lastReply([owner('c1', 'hi'), agent('c2', 'Done.'), agent('c3', 'Bash ls', 'w1:p1', 30, 'Bash'), owner('c4', 'more')]), 'c2');
  assert.equal(lastReply([owner('c1', 'hi')]), '');
  assert.match(DESK_LINE, /foreman/);
});

test('a refusal from the server shows its words in the chat, never the empty line', async () => {
  reset();
  const list = element('refused-list');
  const rpc = async () => { const e = new Error('only the operator talks to the floor\'s foreman.'); e.code = 'unauthorized'; throw e; };
  const chat = mountChat({ list, rpc, panes: () => [], open() {}, me: () => '' });
  await chat.read();
  assert.match(list.textContent, new RegExp(CHAT_REFUSED.slice(0, 20)));
  assert.doesNotMatch(list.textContent, /No messages yet/);
});

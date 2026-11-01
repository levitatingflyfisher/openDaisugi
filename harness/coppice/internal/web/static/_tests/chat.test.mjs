import test from 'node:test';
import assert from 'node:assert/strict';
import { reset, element } from './browser-stub.mjs';
import { cleanTalk, chipParts, chatRows, chatTime, lastReply, DESK_LINE, CHAT_REFUSED, mountChat, SHRUNK_WAIT, CHAT_EMPTY, CHAT_NAMED, READING_WHO, makeAct, actFor, actDone } from '../chat.js';

// The chat with the steward on the phone's home. The page reads floor.chat
// again on each messages event for the chat. A sentence the owner sends
// shows at once, as sent once floor.talk answers, and as read once the
// server marks its line read: the steward's transcript recorded it. No chat
// text is stored on the phone.

const settle = () => new Promise((r) => setImmediate(r));

const PANES = [
  { id: 'w1:p1', label: 'steward', cwd: '/f', state: 'idle' },
  { id: 'w1:p2', label: 'glean-import', cwd: '/g', state: 'blocked' },
  { id: 'w1:p3', label: 'glean', cwd: '/g', state: 'working' },
  { id: 'w1:p4', label: 'old', cwd: '/o', state: 'done', closed: true },
];

const owner = (id, text, pane = 'w1:p1', at = 10, read = false) => ({ id, at, role: 'owner', text, pane, read });
const agent = (id, text, pane = 'w1:p1', at = 20, tool) => ({ id, at, role: 'agent', text, pane, ...(tool ? { tool } : {}) });

test('cleanTalk reads a sentence the way the server writes it to the chat', () => {
  assert.equal(cleanTalk('hello\tsteward\u0007'), 'hello steward');
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
  assert.deepEqual(rows.map((r) => [r.key, r.who, r.mark]), [['c1', 'you', 'read'], ['a1', 'steward', ''], ['c2', 'you', 'sent']]);
  // A reply after the line is not a read mark: a sentence queued behind a
  // long turn has replies after it from the turn before.
  const queued = chatRows([owner('c1', 'one'), agent('a1', 'still on the last one')], []);
  assert.equal(queued[0].mark, 'sent');
  // A line with no read field from the server reads as sent.
  const bare = chatRows([{ id: 'c9', at: 1, role: 'owner', text: 'x', pane: 'w1:p1' }], []);
  assert.equal(bare[0].mark, 'sent');
  // A note line has no speaker and no mark.
  const note = chatRows([{ id: 'n', at: 5, role: 'note', text: 'A new steward started.', pane: 'w1:p1' }], []);
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

test('a sent line shows sending, then sent, then read when the server says the steward took it', async () => {
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
  assert.match(DESK_LINE, /steward/);
});

test('a refusal from the server shows its words in the chat, never the empty line', async () => {
  reset();
  const list = element('refused-list');
  const rpc = async () => { const e = new Error('only the operator talks to the floor\'s steward.'); e.code = 'unauthorized'; throw e; };
  const chat = mountChat({ list, rpc, panes: () => [], open() {}, me: () => '' });
  await chat.read();
  assert.match(list.textContent, new RegExp(CHAT_REFUSED.slice(0, 20)));
  assert.doesNotMatch(list.textContent, /No messages yet/);
});

// The client act id (FM-R-65): a UUID per sentence, kept while the same
// sentence is sent again, new for the next one.
test('an act id is a version 4 uuid', () => {
  assert.match(makeAct(), /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
  assert.notEqual(makeAct(), makeAct());
});

test('a sentence keeps its act id until it is done', () => {
  actDone();
  const a = actFor('build it');
  assert.equal(actFor('build it'), a);
  assert.notEqual(actFor('another'), a);
  const b = actFor('another');
  actDone('another');
  assert.notEqual(actFor('another'), b);
  actDone();
});

test('a failed sentence keeps its id when another input sends and finishes', () => {
  actDone();
  const failed = actFor('sentence A');
  const other = actFor('sentence B');
  actDone('sentence B');
  assert.equal(actFor('sentence A'), failed);
  assert.notEqual(actFor('sentence B'), other);
  actDone();
});

test('an owner line shows when it reached a running turn, or waits for its own', () => {
  const rows = chatRows([
    { id: 'l1', role: 'owner', text: 'one', read: true, delivery: 'delivered mid-turn' },
    { id: 'l2', role: 'owner', text: 'two', read: false, delivery: 'queued' },
  ], []);
  assert.deepEqual(rows.map((r) => r.mark), ['delivered mid-turn', 'queued']);
});

test('paint(true) pins the newest line, so a list shown again after a hide lands at the bottom', async () => {
  const { c } = mount({ chat: { messages: [owner('c1', 'one'), agent('a1', 'two')], more: false } });
  await c.read();
  const last = element('chat-list').children[1];
  const seen = [];
  last.scrollIntoView = (o) => seen.push(o);
  c.paint();
  c.paint(true);
  assert.ok(seen.length >= 1 && seen[seen.length - 1].block === 'end', 'the newest line was not scrolled into view');
});

// fakeSpeech is the page's speech with a state the test drives.
function fakeSpeech() {
  const s = { on_: false, tag: '', spoken: [], stops: 0, fns: new Set() };
  const emit = () => { for (const f of s.fns) f(s.on_); };
  s.speaking = () => s.on_;
  s.current = () => (s.on_ ? s.tag : '');
  s.speak = (t, tag) => { s.spoken.push(t); s.on_ = true; s.tag = tag === undefined ? '' : tag; emit(); };
  s.stop = () => { s.stops += 1; s.on_ = false; s.tag = ''; emit(); };
  s.on = (f) => { s.fns.add(f); return () => s.fns.delete(f); };
  s.end = () => { s.on_ = false; emit(); };
  return s;
}

async function listenList(sp, tag = 'chat', id = 'chat-list', fresh = true) {
  if (fresh) reset();
  const c = mountChat({
    list: element(id), tag,
    rpc: async () => ({ messages: [agent('a1', 'first reply'), agent('a2', 'second reply')] }),
    panes: () => [], open: () => {}, me: () => '', on: () => true,
    speech: () => sp,
  });
  await c.read();
  const btn = (i) => element(id).children[i].querySelector('.listen');
  return { c, btn };
}

test('Listen becomes Stop while the reply is read, and a tap on Stop stops the speech', async () => {
  const sp = fakeSpeech();
  const { btn } = await listenList(sp);
  assert.equal(btn(0).textContent, 'Listen');
  btn(0).dispatchEvent({ type: 'click' });
  assert.deepEqual(sp.spoken, ['first reply']);
  assert.equal(btn(0).textContent, 'Stop');
  assert.equal(btn(1).textContent, 'Listen', 'another reply says Stop too');
  btn(0).dispatchEvent({ type: 'click' });
  assert.equal(sp.stops, 1);
  assert.equal(btn(0).textContent, 'Listen');
  assert.deepEqual(sp.spoken, ['first reply'], 'Stop started the speech again');
});

test('the button says Listen again when the speech ends by itself', async () => {
  const sp = fakeSpeech();
  const { btn } = await listenList(sp);
  btn(1).dispatchEvent({ type: 'click' });
  assert.equal(btn(1).textContent, 'Stop');
  sp.end();
  assert.equal(btn(1).textContent, 'Listen');
});

test('a second Listen starts that reply and the first button goes back to Listen', async () => {
  const sp = fakeSpeech();
  const { btn } = await listenList(sp);
  btn(0).dispatchEvent({ type: 'click' });
  btn(1).dispatchEvent({ type: 'click' });
  assert.deepEqual(sp.spoken, ['first reply', 'second reply']);
  assert.equal(btn(0).textContent, 'Listen');
  assert.equal(btn(1).textContent, 'Stop');
});

test('stopListening and reset stop the speech this list started, and no other', async () => {
  const sp = fakeSpeech();
  const { c, btn } = await listenList(sp);
  c.stopListening();
  assert.equal(sp.stops, 0, 'it stopped speech it did not start');
  btn(0).dispatchEvent({ type: 'click' });
  c.stopListening();
  assert.equal(sp.stops, 1);
  assert.equal(btn(0).textContent, 'Listen');
  btn(0).dispatchEvent({ type: 'click' });
  c.reset();
  assert.equal(sp.stops, 2, 'a reset for another agent left the speech running');
});

test('a reply whose speech ended says Listen when other speech starts', async () => {
  const sp = fakeSpeech();
  const { btn } = await listenList(sp);
  btn(0).dispatchEvent({ type: 'click' });
  assert.equal(btn(0).textContent, 'Stop');
  sp.end();
  sp.speak('read aloud text, nobody\'s reply');
  assert.equal(btn(0).textContent, 'Listen', 'an old reply showed Stop for speech it did not start');
  btn(0).dispatchEvent({ type: 'click' });
  assert.deepEqual(sp.spoken.slice(-1), ['first reply'], 'the tap stopped other speech and did not start the reply');
});

test('stopListening leaves speech that another list started', async () => {
  const sp = fakeSpeech();
  const home = await listenList(sp, 'chat');
  sp.speak('a reading reply', 'reading:a1');
  home.c.stopListening();
  assert.equal(sp.stops, 0, 'the home chat stopped the reading view\'s speech');
  assert.equal(sp.speaking(), true);
  sp.stop();
  home.btn(0).dispatchEvent({ type: 'click' });
  home.c.stopListening();
  assert.equal(sp.stops, 2);
});

test('the home chat and the reading list never both say Stop for the same speech', async () => {
  const sp = fakeSpeech();
  const home = await listenList(sp, 'chat');
  const homeBtn = home.btn;
  const reading = await listenList(sp, 'reading', 'reading-list', false);
  homeBtn(0).dispatchEvent({ type: 'click' });
  assert.equal(homeBtn(0).textContent, 'Stop');
  assert.equal(reading.btn(0).textContent, 'Listen');
  reading.btn(0).dispatchEvent({ type: 'click' });
  assert.equal(reading.btn(0).textContent, 'Stop');
  assert.equal(homeBtn(0).textContent, 'Listen');
});

// A reading list the way the phone draws it: a scroller with a height the
// test sets, and an rpc the test answers.
function readingMount(answer, extra = {}) {
  reset();
  const sc = element('reading');
  sc.scrollTop = 0; sc.scrollHeight = 1000; sc.clientHeight = 500;
  const calls = { n: 0 };
  const c = mountChat({
    list: element('reading-list'), scroller: () => sc,
    rpc: async () => { calls.n += 1; return typeof answer === 'function' ? answer(calls.n) : answer; },
    panes: () => [], open: () => {}, cmd: 'pane.messages', who: READING_WHO, marks: false, chips: false, ...extra,
  });
  return { c, sc, list: element('reading-list'), calls };
}
const rows = (n, last) => Array.from({ length: n }, (_, i) => agent('m' + i, i === n - 1 && last ? last : 'row ' + i));

test('a growing last row changes that row only, and the rows above are not touched', async () => {
  let text = 'a';
  const { c, list } = readingMount(() => ({ messages: rows(6, text) }));
  let wiped = 0;
  const orig = list.replaceChildren.bind(list);
  list.replaceChildren = (...n) => { wiped += 1; orig(...n); };
  await c.read();
  const before = [...list.children];
  text = 'ab';
  await c.read();
  text = 'abc';
  await c.read();
  assert.equal(wiped, 0, 'the list was emptied and filled again');
  assert.equal(list.children.length, 6);
  for (let i = 0; i < 5; i++) assert.equal(list.children[i], before[i], 'row ' + i + ' was drawn again');
  assert.notEqual(list.children[5], before[5]);
  assert.match(list.children[5].textContent, /abc/);
});

test('a window that slides drops the old row and keeps the others', async () => {
  let from = 0;
  const { c, list } = readingMount(() => ({ messages: Array.from({ length: 6 }, (_, i) => agent('m' + (i + from), 'row ' + (i + from))) }));
  await c.read();
  const second = list.children[1];
  from = 1;
  await c.read();
  assert.equal(list.children.length, 6);
  assert.equal(list.children[0], second, 'the kept rows were drawn again');
});

test('a read far shorter than the list is held back once, and taken the second time', async () => {
  let n = 8;
  const { c, list, calls } = readingMount(() => ({ messages: rows(n) }), { wait: async () => {} });
  await c.read();
  assert.equal(list.children.length, 8);
  n = 1;
  await c.read();
  assert.equal(calls.n, 3, 'the short read was not asked again');
  assert.equal(list.children.length, 1, 'a short read that held twice was not taken');
  n = 0;
  await c.read();
  assert.match(list.children[0].textContent, /No messages/, 'an empty list was held back');
});

test('a read that began before a reset is dropped', async () => {
  let release;
  const gate = new Promise((r) => { release = r; });
  const { c, list } = readingMount(async () => { await gate; return { messages: rows(3, 'stale') }; });
  const p = c.read();
  c.reset();
  c.seed([agent('n1', 'fresh')]);
  release();
  await p;
  assert.equal(list.children.length >= 1 && /stale/.test(list.textContent), false, 'the stale answer was drawn');
  assert.match(list.textContent, /fresh/);
});

test('a reader at the end stays at the end when the height changes, and a reader who scrolled up does not', async () => {
  const { c, sc } = readingMount({ messages: rows(6) });
  await c.read();
  assert.equal(sc.scrollTop, 1000, 'the first paint did not go to the end');
  sc.scrollHeight = 1300;
  global.window.dispatchEvent({ type: 'resize' });
  assert.equal(sc.scrollTop, 1300, 'a resize left a pinned reader behind');
  sc.scrollTop = 100;
  sc.dispatchEvent({ type: 'scroll' });
  sc.scrollHeight = 1500;
  global.window.dispatchEvent({ type: 'resize' });
  assert.equal(sc.scrollTop, 100, 'a resize moved a reader who scrolled up');
});

test('a paint scrolls the reading list itself and not the page', async () => {
  const { c, list, sc } = readingMount({ messages: rows(3) });
  await c.read();
  const last = list.children[2];
  let page = 0;
  last.scrollIntoView = () => { page += 1; };
  sc.scrollHeight = 1200;
  await c.read();
  assert.equal(page, 0, 'scrollIntoView moves the page');
  assert.equal(sc.scrollTop, 1200);
});

test('the held-back second ask waits SHRUNK_WAIT first', async () => {
  let n = 8;
  const order = [];
  const { c } = readingMount(() => { order.push('ask'); return { messages: rows(n) }; }, { wait: async (ms) => { order.push('wait ' + ms); } });
  await c.read();
  n = 1;
  await c.read();
  assert.deepEqual(order, ['ask', 'ask', 'wait ' + SHRUNK_WAIT, 'ask']);
});

test('a held-back read whose retry fails forgets the hold, so the next short read is held again', async () => {
  let mode = 'full';
  const { c, calls } = readingMount(() => {
    if (mode === 'throw') throw new Error('down');
    return { messages: rows(mode === 'full' ? 8 : 1) };
  }, { wait: async () => { if (mode === 'short') mode = 'throw'; } });
  await c.read();
  mode = 'short';
  await c.read(); // held; the retry throws
  assert.equal(calls.n, 3);
  mode = 'short';
  const before = calls.n;
  await c.read(); // must be held again, not taken
  assert.equal(calls.n - before, 2, 'the hold from the failed retry stayed set');
});

// Rows 100 high (200 when their text says TALL) in a scroller, with their
// rects computed from scrollTop, so an anchor can be measured.
function laidOut(list, sc) {
  sc.getBoundingClientRect = () => ({ top: 0, bottom: sc.clientHeight });
  const kids = [...list.children];
  const tops = [];
  let y = 0;
  for (const el of kids) { tops.push(y); y += /TALL/.test(el.textContent) ? 200 : 100; }
  kids.forEach((el, i) => {
    const h = /TALL/.test(el.textContent) ? 200 : 100;
    el.getBoundingClientRect = () => ({ top: tops[i] - sc.scrollTop, bottom: tops[i] + h - sc.scrollTop });
  });
}

test('a reader who scrolled up is anchored on a row that stays, not one that changes', async () => {
  let third = 'row 3';
  const { c, sc, list } = readingMount(() => ({ messages: rows(6).map((m, i) => (i === 3 ? { ...m, text: third } : m)) }));
  await c.read();
  sc.scrollHeight = 700; sc.clientHeight = 200;
  sc.scrollTop = 350; // rows 3 (changing) and 4 are in view; row 3 is half out
  sc.dispatchEvent({ type: 'scroll' });
  laidOut(list, sc);
  const kept = list.children[4];
  for (const el of list.children) el.parentNode = list; // the stub keeps no parent
  const before = kept.getBoundingClientRect().top;
  third = 'row 3 TALL';
  const insert = list.insertBefore.bind(list);
  list.insertBefore = (el, at) => { insert(el, at); el.parentNode = list; laidOut(list, sc); };
  const remove = list.removeChild.bind(list);
  list.removeChild = (el) => { remove(el); laidOut(list, sc); };
  await c.read();
  assert.equal(list.children[4], kept);
  assert.equal(kept.getBoundingClientRect().top, before, 'the kept row moved under the reader');
});

test('a box that changes height keeps a pinned reader at the end, and a reader who scrolled up in place', async () => {
  const seen = [];
  const keep = global.ResizeObserver;
  global.ResizeObserver = class { constructor(fn) { this.fn = fn; seen.push(this); } observe(el) { this.el = el; } disconnect() {} };
  try {
    const { c, sc } = readingMount({ messages: rows(6) });
    await c.read();
    assert.equal(seen.length, 1, 'no ResizeObserver watches the chat box');
    assert.equal(seen[0].el, sc);
    // The agents area above grows: the box gets shorter and the end moves.
    sc.clientHeight = 300; sc.scrollHeight = 1000;
    seen[0].fn([]);
    assert.equal(sc.scrollTop, 1000, 'a pinned reader was left behind');
    sc.scrollTop = 100;
    sc.dispatchEvent({ type: 'scroll' });
    sc.clientHeight = 200;
    seen[0].fn([]);
    assert.equal(sc.scrollTop, 100, 'a reader who scrolled up was moved');
  } finally {
    if (keep === undefined) delete global.ResizeObserver; else global.ResizeObserver = keep;
  }
});

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { reset, element } from '../../../internal/web/static/_tests/browser-stub.mjs';
import { sortCards, mountCards, decayText } from '../cards.js';

const CARDS = [
  { choice_id: 'ch_b', project: 'zed', opened_at: 100, impact_lines: 3, confidence: 0.9,
    switch: { cost: 'costly', text: 'Undo 2 steps.' }, headline: 'Kept a over b.', why: 'a passed more checks.',
    decays_in_days: 5, alternatives: ['b'], eliminated: ['c'] },
  { choice_id: 'ch_a', project: 'alpha', opened_at: 200, impact_lines: 40, confidence: 0.6,
    switch: { cost: 'cheap', text: 'Nothing to undo.' }, headline: 'Kept x over y.', why: '', decays_in_days: 1,
    alternatives: ['y', 'z'], eliminated: [] },
];
const ids = (cards) => cards.map((c) => c.choice_id);

function mount(post) {
  reset();
  const m = mountCards({
    status: element('s'), list: element('l'), sortEl: Object.assign(element('so'), { value: 'reversibility' }),
    codeEl: Object.assign(element('co'), { checked: false }), post,
  });
  return { m, status: element('s'), list: element('l'), sortEl: element('so'), codeEl: element('co') };
}

test('a card renders its headline, why, switch cost and buttons, no code at level one', () => {
  const sent = [];
  const { m, list } = mount({ answer: (...a) => sent.push(a), diff: () => sent.push(['diff']) });
  m.render({ type: 'coppice.data', cards: CARDS, diffs: {} });
  assert.equal(list.children.length, 2);
  const first = list.children[0];
  assert.equal(first.dataset.choice, 'ch_a', 'cheap to undo comes first');
  const text = list.children[1].textContent;
  assert.match(text, /Kept a over b/);
  assert.match(text, /Switching: Undo 2 steps/);
  assert.match(text, /decays in 5 days/);
  assert.match(text, /c failed a required check/);
  assert.ok(!text.includes('Reading the code'), 'no code at level one');
  assert.equal(sent.length, 0, 'level one asks for no diff');
  const buttons = first.children.filter((c) => c.tagName === 'DIV')[0].children;
  assert.deepEqual(buttons.map((b) => b.textContent), ['Keep', 'Switch to y', 'Switch to z', 'Drop']);
  buttons[1].dispatchEvent({ type: 'click' });
  buttons[3].dispatchEvent({ type: 'click' });
  assert.deepEqual(sent, [['ch_a', 'switch', 'y'], ['ch_a', 'drop']]);
});

test('level two shows the diff text the page posted', () => {
  const sent = [];
  const { m, list, codeEl } = mount({ answer() {}, diff: (id) => sent.push(id) });
  codeEl.checked = true;
  m.render({ type: 'coppice.data', cards: [CARDS[0]], diffs: {} });
  assert.deepEqual(sent, ['ch_b'], 'the view asks for the diff of a card it shows');
  assert.match(list.children[0].textContent, /Reading the code/);
  m.render({ type: 'coppice.data', cards: [CARDS[0]], diffs: { ch_b: { diffs: { a: '-one\n+two\n' }, notes: { b: 'no diff: x' } } } });
  const text = list.children[0].textContent;
  assert.match(text, /\+two/);
  assert.match(text, /b: no diff: x/);
});

test('sort by impact puts the widest diff gap first; the four keys follow the oracle', () => {
  assert.deepEqual(ids(sortCards(CARDS, 'impact')), ['ch_a', 'ch_b']);
  assert.deepEqual(ids(sortCards(CARDS, 'reversibility')), ['ch_a', 'ch_b']);
  assert.deepEqual(ids(sortCards(CARDS, 'date')), ['ch_a', 'ch_b']);
  assert.deepEqual(ids(sortCards(CARDS, 'project')), ['ch_a', 'ch_b']);
  assert.deepEqual(ids(sortCards([CARDS[1], { ...CARDS[0], opened_at: 300 }], 'date')), ['ch_b', 'ch_a']);
  assert.equal(CARDS[0].choice_id, 'ch_b', 'the input is not reordered');
});

test('an empty queue says so, and a floor notice wins', () => {
  const { m, status } = mount({ answer() {}, diff() {} });
  m.render({ type: 'coppice.data', cards: [], diffs: {} });
  assert.equal(status.textContent, 'No open cards.');
  m.render({ type: 'coppice.data', cards: [], notice: 'Dropped.' });
  assert.equal(status.textContent, 'Dropped.');
  assert.equal(decayText(0), 'decays today');
  assert.equal(decayText(1), 'decays in 1 day');
});

test('the cards view holds no token and asks the server for nothing', () => {
  const src = readFileSync(new URL('../cards.js', import.meta.url), 'utf8');
  for (const word of ['localStorage', 'fetch(', 'WebSocket', 'coppice.token']) {
    assert.ok(!src.includes(word), 'cards.js uses ' + word);
  }
});

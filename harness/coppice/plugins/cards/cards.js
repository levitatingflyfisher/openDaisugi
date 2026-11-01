// The cards view: the open review cards from `daisugi rank queue`. A card
// says what an agent kept, why, and what switching to another attempt would
// cost. Level one shows no code. Level two shows the diff. The view runs in
// a sandbox and holds no token: the floor page posts it the cards and the
// diffs, and takes the three answers (Keep, Switch, Drop) back.

import { connect, boot } from '../_lib/view.js';

const COST_RANK = { cheap: 0, costly: 1, follow_up_only: 2 };
const num = (v, d) => (typeof v === 'number' && Number.isFinite(v) ? v : d);

// SORTS are the four keys of the design.
export const SORTS = ['reversibility', 'impact', 'date', 'project'];

// sortCards is a copy of cards in the order of rank.sort_cards.
export function sortCards(cards, by) {
  const c = (a, b) => (a < b ? -1 : a > b ? 1 : 0);
  const cost = (v) => COST_RANK[v.switch && v.switch.cost] ?? 3;
  const key = {
    reversibility: (a, b) => cost(a) - cost(b) || c(num(a.opened_at, 0), num(b.opened_at, 0)) || c(a.choice_id, b.choice_id),
    impact: (a, b) => num(b.impact_lines, 0) - num(a.impact_lines, 0)
      || num(a.confidence, 1) - num(b.confidence, 1) || c(a.choice_id, b.choice_id),
    date: (a, b) => num(b.opened_at, 0) - num(a.opened_at, 0) || c(a.choice_id, b.choice_id),
    project: (a, b) => c(a.project || '', b.project || '') || num(b.opened_at, 0) - num(a.opened_at, 0)
      || c(a.choice_id, b.choice_id),
  }[by] || null;
  return [...(cards || [])].sort(key || (() => 0));
}

// decayText is how long the card stays open.
export function decayText(days) {
  const d = num(days, 0);
  return d <= 0 ? 'decays today' : 'decays in ' + d + (d === 1 ? ' day' : ' days');
}

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

// renderCard builds one card. code is true for level two, diff is the
// {diffs, notes} the floor posted for this card, or undefined.
export function renderCard(card, { code, diff, post }) {
  const li = el('li', 'card');
  li.dataset.choice = card.choice_id;
  li.append(el('h2', '', card.headline || card.choice_id));
  if (card.why) li.append(el('p', '', card.why));
  const sw = card.switch && card.switch.text;
  if (sw) li.append(el('p', 'muted', 'Switching: ' + sw));
  li.append(el('p', 'muted', decayText(card.decays_in_days)));
  if (card.adopt) li.append(el('p', 'muted', 'Take its edits with: ' + card.adopt));
  for (const id of card.eliminated || []) li.append(el('p', 'elim', id + ' failed a required check.'));
  if (code) {
    if (!diff) {
      li.append(el('p', 'muted', 'Reading the code...'));
    } else {
      for (const id of Object.keys(diff.diffs || {})) {
        li.append(el('p', 'muted', id));
        li.append(el('pre', '', diff.diffs[id] || '(no change)'));
      }
      for (const id of Object.keys(diff.notes || {})) li.append(el('p', 'elim', (id ? id + ': ' : '') + diff.notes[id]));
    }
  }
  const row = el('div', 'buttons');
  const keep = el('button', 'keep', 'Keep');
  keep.type = 'button';
  keep.addEventListener('click', () => post.answer(card.choice_id, 'keep'));
  row.append(keep);
  for (const alt of card.alternatives || []) {
    const b = el('button', 'switch', 'Switch to ' + alt);
    b.type = 'button';
    b.dataset.pick = alt;
    b.addEventListener('click', () => post.answer(card.choice_id, 'switch', alt));
    row.append(b);
  }
  const drop = el('button', 'drop', 'Drop');
  drop.type = 'button';
  drop.addEventListener('click', () => post.answer(card.choice_id, 'drop'));
  row.append(drop);
  li.append(row);
  return li;
}

// mountCards draws the cards the floor posted into list.
export function mountCards({ status, list, sortEl, codeEl, post }) {
  let last = null;
  const render = (data) => {
    if (!data || data.type !== 'coppice.data') return;
    last = data;
    const cards = sortCards(Array.isArray(data.cards) ? data.cards : [], sortEl.value);
    const diffs = data.diffs && typeof data.diffs === 'object' ? data.diffs : {};
    list.textContent = '';
    for (const card of cards) {
      if (codeEl.checked && !diffs[card.choice_id]) post.diff(card.choice_id);
      list.append(renderCard(card, { code: codeEl.checked, diff: diffs[card.choice_id], post }));
    }
    status.textContent = data.notice || (cards.length ? '' : 'No open cards.');
  };
  const again = () => { if (last) render(last); };
  sortEl.addEventListener('change', again);
  codeEl.addEventListener('change', again);
  return { render };
}

function start() {
  const c = connect(window);
  const post = { answer: c.answer, diff: c.diff };
  const cards = mountCards({
    status: document.getElementById('cards-status'),
    list: document.getElementById('cards-list'),
    sortEl: document.getElementById('cards-sort'),
    codeEl: document.getElementById('cards-code'),
    post,
  });
  window.addEventListener('message', (e) => {
    if (c.framed && e.source === window.parent) cards.render(e.data);
  });
}

boot(start);

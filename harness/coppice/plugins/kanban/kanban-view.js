// The kanban on the page. A click on a card sets the selection to the
// card's panes. A double click opens the pane of the card that needs a
// person most. The columns are states, so a card has no other action.
import { connect, boot, byNeed, stateColor, stateWord } from '../_lib/view.js';
import { columns, HINT } from './kanban.js';

const WAITING = 'Open this view from the floor page. It shows what the floor page sends it.';

function make(tag, className, text) {
  const el = document.createElement(tag);
  if (className) el.className = className;
  if (text !== undefined) el.textContent = text;
  return el;
}

export function mountKanban(status, board, hint, view) {
  status.textContent = WAITING;
  hint.textContent = HINT;
  const render = () => {
    const cols = columns(view.tasks(), view.panes());
    const sel = new Set(view.sel());
    board.textContent = '';
    for (const col of cols) {
      const box = make('section', 'column');
      box.append(make('h2', '', col.name + ' · ' + col.cards.length));
      for (const card of col.cards) {
        const b = make('button', 'card');
        b.type = 'button';
        if (card.panes.some((p) => sel.has(p))) b.className += ' sel';
        if (card.state) b.style.borderLeftColor = stateColor(stateWord(card.state));
        b.append(card.label);
        const facts = [card.panes.length + (card.panes.length === 1 ? ' pane' : ' panes')];
        if (col.name === 'working' && card.state !== 'working') facts.push(stateWord(card.state));
        if (typeof card.ahead === 'number') facts.push('+' + card.ahead);
        b.append(make('small', '', facts.join('  ')));
        b.addEventListener('click', () => view.setSel(card.panes));
        b.addEventListener('dblclick', () => {
          const held = new Set(card.panes);
          const first = byNeed(view.panes().filter((p) => held.has(p.id)))[0];
          if (first) view.openPane(first.id);
        });
        box.append(b);
      }
      board.append(box);
    }
    status.textContent = cols.some((c) => c.cards.length) ? '' : 'No tasks yet.';
  };
  view.onData(render);
  return { render };
}

boot(() => mountKanban(
  document.getElementById('kanban-status'),
  document.getElementById('kanban-board'),
  document.getElementById('kanban-hint'),
  connect(window),
));

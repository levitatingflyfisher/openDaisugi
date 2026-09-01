// The shift log on the page. Each row is one pane, each cell five
// minutes, oldest on the left. A cell the server's ring does not cover
// and no event marked is drawn as unknown. A click on a row selects its pane, and a
// double click opens it. The page redraws each minute, so the window
// moves with the clock.
import { connect, boot, STATE_COLORS } from '../_lib/view.js';
import { cells, coverText, LEVEL, UNKNOWN } from './shift.js';

// DENY_COLOR marks a cell where the gate said no. A deny is not a state,
// so it has a colour of its own.
export const DENY_COLOR = '#c0392b';

// MARKS are the colour of each level.
export const MARKS = {
  [LEVEL.working]: STATE_COLORS.working,
  [LEVEL.ask]: STATE_COLORS.blocked,
  [LEVEL.deny]: DENY_COLOR,
};

const MINUTES = 80;
const CELL = 5;
const WAITING = 'Open this view from the floor page. It shows what the floor page sends it.';

function make(tag, className, text) {
  const el = document.createElement(tag);
  if (className) el.className = className;
  if (text !== undefined) el.textContent = text;
  return el;
}

function swatch(color, word) {
  const s = make('span', 'swatch');
  s.style.background = color;
  return [s, word];
}

export function mountShift(status, grid, legend, view, now) {
  const clock = now || (() => Date.now() / 1000);
  status.textContent = WAITING;
  legend.textContent = '';
  const unknown = make('span', 'swatch unknown');
  legend.append(...swatch(MARKS[LEVEL.working], 'working'), ...swatch(MARKS[LEVEL.ask], 'ask'),
    ...swatch(MARKS[LEVEL.deny], 'deny'), unknown, 'unknown');
  let seen = false;
  const render = () => {
    const now = clock();
    const from = view.from();
    const rows = cells(view.events(), now, MINUTES, CELL, from);
    const labels = new Map(view.panes().map((p) => [p.id, p.label || p.id]));
    grid.textContent = '';
    const head = make('div', 'shift-row head');
    head.append(make('span', 'who', 'pane'));
    for (let i = 0; i < MINUTES / CELL; i++) {
      const left = MINUTES - i * CELL;
      head.append(make('span', '', i % 4 === 0 ? left + 'm' : ''));
    }
    grid.append(head);
    for (const row of rows) {
      const line = make('div', 'shift-row');
      line.dataset.pane = row.pane;
      const who = make('span', 'who', labels.get(row.pane) || row.pane);
      who.addEventListener('click', () => view.setSel([row.pane]));
      who.addEventListener('dblclick', () => { if (labels.has(row.pane)) view.openPane(row.pane); });
      line.append(who);
      for (const level of row.cells) {
        const c = make('span', level === UNKNOWN ? 'cell unknown' : 'cell');
        if (level) c.style.background = MARKS[level];
        line.append(c);
      }
      grid.append(line);
    }
    status.textContent = seen ? coverText(from, now, MINUTES, rows.length) : WAITING;
  };
  view.onData(() => { seen = true; render(); });
  return { render };
}

boot(() => {
  const shift = mountShift(
    document.getElementById('shift-status'),
    document.getElementById('shift-grid'),
    document.getElementById('shift-legend'),
    connect(window),
  );
  setInterval(shift.render, 60000);
});

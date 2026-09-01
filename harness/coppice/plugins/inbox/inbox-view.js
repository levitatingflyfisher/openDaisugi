// The inbox on the page. A click on a row sets the selection to its
// panes, and the floor sends the selection back, so the page keeps none
// of its own. ! selects the first row that needs you. The view watches
// the selected row's neediest open pane and draws its picture, and says
// when that picture is not live.
import { connect, boot, stateColor, stateWord } from '../_lib/view.js';
import { drawPicture } from '../_lib/picture.js';
import { rows, jumpToNeed, focusPane, selectedRow } from './inbox.js';

const WAITING = 'Open this view from the floor page. It shows what the floor page sends it.';

function make(tag, className, text) {
  const el = document.createElement(tag);
  if (className) el.className = className;
  if (text !== undefined) el.textContent = text;
  return el;
}

export function mountInbox(status, list, head, canvas, view) {
  status.textContent = WAITING;
  let shown = [];
  let live = '';
  let pic = null;
  // shownLive and refused are what the floor last said about the watch.
  let shownLive = false;
  let refused = false;
  const say = () => {
    if (!live) head.textContent = 'Select a row to see its pane live.';
    else if (refused) head.textContent = 'The floor could not show ' + live + ' live.';
    else if (!shownLive) head.textContent = 'Waiting for the picture of ' + live + '.';
    else head.textContent = 'live: ' + live;
  };
  const pick = (i) => { if (shown[i]) view.setSel(shown[i].panes); };
  const render = () => {
    shown = rows(view.tasks(), view.panes());
    const at = selectedRow(shown, view.sel());
    list.textContent = '';
    shown.forEach((r, i) => {
      const li = make('li', i === at ? 'sel' : '');
      const dot = make('span', 'dot');
      if (r.state) dot.style.background = stateColor(stateWord(r.state));
      const ahead = r.ahead === null ? '' : '+' + r.ahead;
      li.append(dot, make('span', '', r.label + '  ' + r.worktree), make('span', 'muted', ahead),
        make('span', 'muted', r.state === 'blocked' ? 'needs you' : r.state));
      li.addEventListener('click', () => pick(i));
      li.addEventListener('dblclick', () => { const p = focusPane(r, view.panes()); if (p) view.openPane(p); });
      list.append(li);
    });
    const want = at >= 0 ? focusPane(shown[at], view.panes()) : '';
    if (want !== live) {
      live = want;
      pic = null;
      shownLive = false;
      refused = false;
      view.watch(want ? [want] : []);
    }
    say();
    drawPicture(canvas, pic);
    status.textContent = shown.length ? '' : 'No task has a worktree yet.';
  };
  const onFrame = (p) => {
    if (p.pane !== live || !shownLive) return;
    pic = p;
    drawPicture(canvas, pic);
  };
  // onWatch takes what the floor says about the watch. A picture that is
  // no longer live is cleared, so an old screen never reads as live.
  const onWatch = (st) => {
    shownLive = Boolean(live) && st.live.includes(live);
    refused = Boolean(live) && st.refused.includes(live);
    if (!shownLive) {
      pic = null;
      drawPicture(canvas, pic);
    }
    say();
  };
  const onKey = (e) => {
    if (e.key !== '!') return;
    const i = jumpToNeed(shown);
    if (i >= 0) pick(i);
  };
  view.onData(render);
  view.onFrame(onFrame);
  view.onWatch(onWatch);
  return { render, onFrame, onKey, onWatch };
}

boot(() => {
  const inbox = mountInbox(
    document.getElementById('inbox-status'),
    document.getElementById('inbox-rows'),
    document.getElementById('inbox-live-head'),
    document.getElementById('inbox-picture'),
    connect(window),
  );
  document.addEventListener('keydown', inbox.onKey);
});

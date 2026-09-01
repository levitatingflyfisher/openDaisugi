import { phoneSize } from './windows.js';

// The phone. Home is the overview: the colony strip, the agents that need
// you in a small queue at the top, and the agent list grouped as the rail
// groups it. A tap on an agent slides its sheet in from the right over most
// of the screen, and the overview stays live under it and in sight at the
// left edge. A swipe right on the sheet, or a tap on that edge, goes back.
// Nothing here navigates away from the page: the sheet is the pane route
// drawn over the overview.

// SWIPE_PX is how far a finger must travel right to count as going back.
export const SWIPE_PX = 60;
// FOLLOW_PX is how far a finger moves before the sheet follows it.
export const FOLLOW_PX = 10;
// SIDEWAYS is how many times longer than its rise a swipe must be.
export const SIDEWAYS = 1.5;

// isPhone is true for a page the phone layout serves: under 600 px wide,
// or a phone on its side.
export function isPhone(widthPx, heightPx) {
  return phoneSize(widthPx, heightPx);
}

// overviewShown is true when the overview is on screen: on the floor, and
// on a phone under an open sheet too.
export function overviewShown(screen, phone) {
  return screen === 'roster' || (Boolean(phone) && screen === 'pane');
}

// canGoBack is true when a finger that started at a place may take the
// sheet back. On the terminal body a swipe right is a scroll while the
// body can still scroll left, so there it counts only from the left end.
export function canGoBack(onBody, bodyLeft) {
  return !onBody || (Number(bodyLeft) || 0) <= 0;
}

// backSwipe is true for a drag of dx by dy pixels that goes back: right,
// at least SWIPE_PX, and SIDEWAYS times longer than it rises or falls,
// from a place canGoBack allows.
export function backSwipe({ dx, dy, onBody, bodyLeft }) {
  if (!canGoBack(onBody, bodyLeft)) return false;
  const x = Number(dx) || 0;
  const y = Math.abs(Number(dy) || 0);
  return x >= SWIPE_PX && x > SIDEWAYS * y;
}

// sheetShift is how far the sheet follows a finger that moved dx by dy: 0
// until the drag is plainly sideways and to the right, then dx.
export function sheetShift({ dx, dy, onBody, bodyLeft }) {
  if (!canGoBack(onBody, bodyLeft)) return 0;
  const x = Number(dx) || 0;
  const y = Math.abs(Number(dy) || 0);
  return x >= FOLLOW_PX && x > SIDEWAYS * y ? x : 0;
}

// phoneAfter is whether the page is a phone after a resize to width by
// height. A page whose width stayed while a text field has the focus only
// lost height to the on-screen keyboard, so it keeps the layout it had and
// the field keeps its place. Growing taller always counts, so a closing
// keyboard never strands a layout.
export function phoneAfter(was, { width, height, oldWidth, typing }) {
  const next = isPhone(width, height);
  if (next && !was && typing && width === oldWidth) return was;
  return next;
}

// keyboardInset is how many pixels at the bottom of a page innerHeight
// tall the visual viewport vv leaves hidden, as an on-screen keyboard
// does on a browser that does not resize the page for it.
export function keyboardInset(innerHeight, vv) {
  if (!vv) return 0;
  const hidden = (Number(innerHeight) || 0) - (Number(vv.height) || 0) - (Number(vv.offsetTop) || 0);
  return Math.max(0, Math.round(hidden));
}

// phoneQueue is the rows that need you, in the order given: the small
// queue at the top of the phone's overview. A held ask reads as working
// and is not in it.
export function phoneQueue(rows) {
  return (Array.isArray(rows) ? rows : []).filter((r) => r && r.state === 'blocked');
}

// mountSheet wires the sheet's gestures. el is the sheet, edge the strip
// of overview left in sight, body() the terminal body, and back() closes
// the sheet. on() says whether the page is a sheet now; a wider page's
// pane screen takes no swipe. A swipe is one finger: a second finger ends
// it.
export function mountSheet(el, { edge, body, back, on }) {
  let start = null;
  const live = () => typeof on !== 'function' || on();
  const onBody = (target) => {
    const b = typeof body === 'function' ? body() : null;
    if (!b || !target) return false;
    return target === b || (typeof b.contains === 'function' && b.contains(target));
  };
  const drag = (x) => {
    if (!el.style) return;
    el.style.transform = x > 0 ? 'translateX(' + Math.round(x) + 'px)' : '';
    el.style.transition = x > 0 ? 'none' : '';
  };
  const at = (e) => {
    const t = e && e.changedTouches && e.changedTouches[0] ? e.changedTouches[0] : (e && e.touches && e.touches[0]) || null;
    return t ? { x: t.clientX, y: t.clientY } : null;
  };
  const gesture = (e) => {
    const p = at(e);
    return p && start ? { dx: p.x - start.x, dy: p.y - start.y, onBody: start.onBody, bodyLeft: start.bodyLeft } : null;
  };
  el.addEventListener('touchstart', (e) => {
    if (!live() || !e || !e.touches || e.touches.length !== 1) {
      start = null;
      drag(0);
      return;
    }
    const b = typeof body === 'function' ? body() : null;
    const p = at(e);
    start = { x: p.x, y: p.y, onBody: onBody(e.target), bodyLeft: b ? Number(b.scrollLeft) || 0 : 0 };
  }, { passive: true });
  el.addEventListener('touchmove', (e) => {
    const g = gesture(e);
    if (g) drag(sheetShift(g));
  }, { passive: true });
  el.addEventListener('touchend', (e) => {
    const g = gesture(e);
    start = null;
    drag(0);
    if (g && backSwipe(g)) back();
  }, { passive: true });
  el.addEventListener('touchcancel', () => {
    start = null;
    drag(0);
  }, { passive: true });
  if (edge) {
    edge.addEventListener('click', (e) => {
      if (e && typeof e.preventDefault === 'function') e.preventDefault();
      back();
    });
  }
}

import test from 'node:test';
import assert from 'node:assert/strict';
import { reset, element } from './browser-stub.mjs';
import {
  isPhone, overviewShown, canGoBack, backSwipe, sheetShift, phoneQueue, mountSheet, SWIPE_PX, phoneAfter, keyboardInset,
} from '../dock.js';
import { paneRows } from '../roster.js';

// The phone: the overview is home, and an agent's sheet slides in over it.
// These scenarios drive dock.js on its own against the fake DOM. The whole
// page at phone size is in phone.test.mjs.

function p(id, state, ask) {
  return { id, label: id, state, harness: 'claude', cwd: '/w/' + id, ts: 1, ask };
}

const PANES = [p('a', 'working'), p('b', 'blocked', { id: 't1', summary: 'ls' }), p('c', 'idle')];

test('a phone is under 600 px wide, or on its side', () => {
  assert.equal(isPhone(390, 844), true);
  assert.equal(isPhone(360, 780), true);
  assert.equal(isPhone(599), true);
  assert.equal(isPhone(844, 390), true);
  assert.equal(isPhone(600, 900), false);
  assert.equal(isPhone(1440, 900), false);
});

test('the overview shows on the floor, and under a sheet on a phone only', () => {
  assert.equal(overviewShown('roster', false), true);
  assert.equal(overviewShown('roster', true), true);
  assert.equal(overviewShown('pane', true), true);
  assert.equal(overviewShown('pane', false), false, 'a wide pane screen kept the floor');
  assert.equal(overviewShown('settings', true), false);
  assert.equal(overviewShown('new', true), false);
});

test('a swipe right goes back once it is long and plainly sideways', () => {
  assert.equal(backSwipe({ dx: SWIPE_PX, dy: 0 }), true);
  assert.equal(backSwipe({ dx: 120, dy: 30 }), true);
  assert.equal(backSwipe({ dx: SWIPE_PX - 1, dy: 0 }), false, 'a short drag went back');
  assert.equal(backSwipe({ dx: 120, dy: 90 }), false, 'a slanted drag went back');
  assert.equal(backSwipe({ dx: -120, dy: 0 }), false, 'a swipe left went back');
  assert.equal(backSwipe({ dx: 120, dy: -30 }), true);
});

test('on the terminal body a swipe right is a scroll until the body is at its left end', () => {
  assert.equal(canGoBack(false, 300), true, 'the sheet header took the body scroll into account');
  assert.equal(canGoBack(true, 0), true);
  assert.equal(canGoBack(true, 40), false);
  assert.equal(backSwipe({ dx: 200, dy: 0, onBody: true, bodyLeft: 40 }), false, 'a sideways scroll went back');
  assert.equal(backSwipe({ dx: 200, dy: 0, onBody: true, bodyLeft: 0 }), true);
});

test('the sheet follows a finger only once the drag is plainly sideways and right', () => {
  assert.equal(sheetShift({ dx: 5, dy: 0 }), 0);
  assert.equal(sheetShift({ dx: 40, dy: 5 }), 40);
  assert.equal(sheetShift({ dx: 40, dy: 40 }), 0);
  assert.equal(sheetShift({ dx: -40, dy: 0 }), 0);
  assert.equal(sheetShift({ dx: 40, dy: 0, onBody: true, bodyLeft: 10 }), 0);
});

test('the queue at the top holds the agents that need you, and no held ask', () => {
  const rows = paneRows([...PANES, { ...p('d', 'blocked', { id: 't2', summary: 'rm' }), held: { task: 't' } }], 10);
  assert.deepEqual(phoneQueue(rows).map((r) => r.id), ['b']);
  assert.deepEqual(phoneQueue(null), []);
});

// touch is one touch event with a single finger at x, y.
function touch(el, type, x, y, target, fingers = 1) {
  const pt = { clientX: x, clientY: y };
  const list = type === 'touchend' ? [] : Array.from({ length: fingers }, () => pt);
  el.dispatchEvent({ type, touches: list, changedTouches: [pt], target: target || el });
}

test('a swipe right on the sheet goes back, and a tap on the edge does too', () => {
  reset();
  const sheet = element('screen-pane');
  const edge = element('sheet-edge');
  const body = element('canvas-wrap');
  let backs = 0;
  mountSheet(sheet, { edge, body: () => body, back: () => { backs += 1; } });
  touch(sheet, 'touchstart', 50, 300);
  touch(sheet, 'touchmove', 90, 305);
  assert.equal(sheet.style.transform, 'translateX(40px)', 'the sheet did not follow the finger');
  touch(sheet, 'touchend', 200, 310);
  assert.equal(backs, 1);
  assert.equal(sheet.style.transform, '', 'the sheet stayed shifted');
  touch(sheet, 'touchstart', 50, 300);
  touch(sheet, 'touchend', 80, 300);
  assert.equal(backs, 1, 'a short drag went back');
  edge.dispatchEvent({ type: 'click' });
  assert.equal(backs, 2);
});

test('a swipe on a terminal body that can scroll left scrolls it, and two fingers never go back', () => {
  reset();
  const sheet = element('screen-pane');
  const body = element('canvas-wrap');
  body.scrollLeft = 120;
  let backs = 0;
  mountSheet(sheet, { edge: null, body: () => body, back: () => { backs += 1; } });
  touch(sheet, 'touchstart', 50, 300, body);
  touch(sheet, 'touchmove', 150, 300, body);
  assert.equal(sheet.style.transform, '', 'the sheet moved under a scroll');
  touch(sheet, 'touchend', 250, 300, body);
  assert.equal(backs, 0, 'a sideways scroll went back');
  body.scrollLeft = 0;
  touch(sheet, 'touchstart', 50, 300, body);
  touch(sheet, 'touchend', 250, 300, body);
  assert.equal(backs, 1, 'a swipe from the left end of the body did not go back');
  touch(sheet, 'touchstart', 50, 300, null, 2);
  touch(sheet, 'touchend', 250, 300);
  assert.equal(backs, 1, 'a pinch went back');
});

test('a page that is no phone takes no swipe', () => {
  reset();
  const sheet = element('screen-pane');
  let phone = false;
  let backs = 0;
  mountSheet(sheet, { edge: null, body: () => null, back: () => { backs += 1; }, on: () => phone });
  touch(sheet, 'touchstart', 50, 300);
  touch(sheet, 'touchmove', 150, 300);
  assert.equal(sheet.style.transform || '', '', 'a wide pane screen moved under a finger');
  touch(sheet, 'touchend', 250, 300);
  assert.equal(backs, 0);
  phone = true;
  touch(sheet, 'touchstart', 50, 300);
  touch(sheet, 'touchend', 250, 300);
  assert.equal(backs, 1);
});

test('a page that only got shorter while a field has the focus keeps its layout, since that is a keyboard', () => {
  assert.equal(phoneAfter(false, { width: 844, height: 390, oldWidth: 844, typing: true }), false, 'a keyboard turned a tablet into a phone');
  assert.equal(phoneAfter(false, { width: 844, height: 390, oldWidth: 844, typing: false }), true);
  assert.equal(phoneAfter(false, { width: 844, height: 390, oldWidth: 1200, typing: true }), true, 'a turn with a field focused kept the old layout');
  assert.equal(phoneAfter(true, { width: 844, height: 900, oldWidth: 844, typing: true }), false, 'a keyboard closing kept the phone layout');
  assert.equal(phoneAfter(true, { width: 390, height: 400, oldWidth: 390, typing: true }), true);
});

test('the keyboard inset is what the visual viewport leaves at the bottom', () => {
  assert.equal(keyboardInset(844, { height: 500, offsetTop: 0 }), 344);
  assert.equal(keyboardInset(844, { height: 844, offsetTop: 0 }), 0);
  assert.equal(keyboardInset(844, { height: 500, offsetTop: 20 }), 324);
  assert.equal(keyboardInset(844, null), 0);
  assert.equal(keyboardInset(844, { height: 900, offsetTop: 0 }), 0);
});

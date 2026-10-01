// web.mjs: the web floor, driven in headless chromium like a person would.
//
//   node web.mjs BASE_URL TOKEN SHOTS_DIR
//
// BASE_URL serves the floor and TOKEN signs in, the way a scanned QR does.
// run.sh has made the panes "e2e shell", "e2e ask" (with a gate ask held on
// it) and "e2e trust" (fake-trust.sh, claude's folder trust screen). Every check
// that fails saves a screenshot named after it and exits 1. After the desk
// checks it starts the floor's foreman, fake-foreman.sh, through rpc.mjs
// on the scratch socket that run.sh exports as $sock, for the phone's chat.
import { chromium } from 'playwright';
import { mkdirSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

// rpcCall sends one command to the scratch server as the operator.
function rpcCall(cmd, fields) {
  if (!process.env.sock) throw new Error('no $sock: run web.mjs through run.sh');
  const rpc = fileURLToPath(new URL('./rpc.mjs', import.meta.url));
  return execFileSync('node', [rpc, process.env.sock, 'call', cmd, JSON.stringify(fields)], { encoding: 'utf8' });
}

const [base, token, shots] = process.argv.slice(2);
if (!shots) {
  console.error('usage: node web.mjs BASE_URL TOKEN SHOTS_DIR');
  process.exit(2);
}
mkdirSync(shots, { recursive: true });

const browser = await chromium.launch();
const context = await browser.newContext({ viewport: { width: 1440, height: 900 } });
const page = await context.newPage();
const pageErrors = [];
page.on('pageerror', (e) => pageErrors.push(String(e)));
page.on('console', (m) => {
  if (m.type() === 'error') pageErrors.push(m.text());
});

let failed = 0;
const shot = (name, p = page) => p.screenshot({ path: path.join(shots, `web-${name}.png`) });
// failPage is the page a failed check's screenshot shows.
let failPage = page;
async function check(name, fn) {
  try {
    await fn();
    console.log(`ok   ${name}`);
  } catch (e) {
    failed++;
    console.log(`FAIL ${name}: ${e && e.message ? e.message.split('\n')[0] : e}`);
    try {
      await shot(`FAIL-${name.replace(/[^a-z0-9]+/gi, '-')}`, failPage);
    } catch {
      // The page may be gone; the failure line above is the evidence.
    }
  }
}

// openTile returns the window that shows the pane labelled label. The
// floor has fewer windows than panes, so when none shows it, a click on
// its roster row puts it in one.
async function openTile(label) {
  const t = page.locator('#tiles section').filter({ hasText: label }).first();
  if (!(await t.count())) await page.locator(`#roster >> text=${label}`).first().click();
  await t.waitFor({ timeout: 15000 });
  return t;
}

await check('the floor loads and signs in', async () => {
  await page.goto(`${base}/#t=${token}`, { waitUntil: 'load', timeout: 20000 });
  await page.waitForSelector('#roster', { timeout: 10000 });
});

await check('the roster lists every pane', async () => {
  for (const label of ['e2e shell', 'e2e ask', 'e2e trust']) {
    await page.locator(`#roster >> text=${label}`).first().waitFor({ timeout: 15000 });
  }
  await shot('roster');
});

await check('a tile shows the pane live and takes typed keys', async () => {
  const t = await openTile('e2e shell');
  await t.click();
  await page.keyboard.type('echo typed-in-a-tile', { delay: 10 });
  await page.keyboard.press('Enter');
  await page.waitForFunction(
    // The command line and its output: the text shows twice.
    () => [...document.querySelectorAll('#tiles section')].some((s) => s.textContent.split('typed-in-a-tile').length > 2),
    null,
    { timeout: 15000 },
  );
  await shot('tile');
});

await check('a gate ask shows its ask bar with Deny and Allow', async () => {
  await openTile('e2e ask');
  const bar = page.locator('.askbar:not(.trustbar)').filter({ hasText: 'git push origin main' }).first();
  await bar.waitFor({ timeout: 15000 });
  await bar.locator('button', { hasText: /deny/i }).first().waitFor({ timeout: 5000 });
  await bar.locator('button', { hasText: /allow/i }).first().waitFor({ timeout: 5000 });
  await shot('ask');
});

await check('the trust screen shows both answers, and Trust this folder answers it', async () => {
  await openTile('e2e trust');
  const bar = page.locator('.askbar.trustbar').first();
  await bar.waitFor({ timeout: 20000 });
  const yes = bar.locator('button', { hasText: 'Trust this folder' });
  await bar.locator('button', { hasText: 'Not now' }).waitFor({ timeout: 5000 });
  await shot('trust');
  await yes.click();
  // coppice moves the cursor down to "Yes, I trust this folder" and presses
  // Enter. fake-trust.sh then draws a prompt box, the pane reads idle and
  // the bar goes. Had the answer landed on "No, exit", the pane would end.
  await bar.waitFor({ state: 'detached', timeout: 20000 });
  const trusted = page.locator('#tiles section').filter({ hasText: 'e2e trust' }).first();
  await page.waitForFunction(
    () => [...document.querySelectorAll('#tiles section')].some((s) => s.textContent.includes('e2e trust') && s.textContent.includes('❯') && !s.textContent.includes('Quick safety check')),
    null,
    { timeout: 15000 },
  );
  if (/ended/.test(await trusted.textContent())) throw new Error('the trust pane ended: the answer was not yes');
  await shot('trusted');
});

await check('at 1440 the chat bar sits at the foot of the rail, above Recent, and no chat shows', async () => {
  const tell = page.locator('#rail #tell');
  await tell.waitFor({ state: 'visible', timeout: 5000 });
  if (await page.locator('#chat').isVisible()) throw new Error('the chat shows on the desk');
  if (await page.locator('#agents-fold').isVisible()) throw new Error('the agents fold shows on the desk');
  const box = await tell.boundingBox();
  const rail = await page.locator('#rail').boundingBox();
  if (box.x + box.width > rail.x + rail.width + 1) throw new Error('the chat bar is not in the rail');
  await shot('desk', page);
});

// The New form and Settings are panels over the floor. The rail and a
// window stay drawn while they are open, Esc and x close them, and More in
// the New menu makes an agent without the floor leaving.
const tileCount = () => page.locator('#tiles section').count();
const floorDrawn = async (what) => {
  if (!(await page.locator('#screen-roster').isVisible())) throw new Error(`the floor is hidden ${what}`);
  if (!(await page.locator('#rail').isVisible())) throw new Error(`the rail is hidden ${what}`);
  if ((await tileCount()) < 1) throw new Error(`no window is drawn ${what}`);
};

await check('More in the New menu opens the form over the floor, and a created agent shows', async () => {
  await page.locator('#new-menu-btn').click();
  await page.locator('#new-menu >> text=More').first().click();
  await page.locator('#screen-new').waitFor({ state: 'visible', timeout: 5000 });
  await floorDrawn('with the New form open');
  const form = await page.locator('#screen-new').boundingBox();
  const rail = await page.locator('#rail').boundingBox();
  if (form.x < rail.x + rail.width - 1) throw new Error('the New form covers the rail');
  await shot('new-panel');
  const dir = path.join(process.env.E2E_DIR || '', 'proj', 'alpha');
  await page.locator('#new-cwd').fill(dir);
  await page.locator('#new-label').fill('e2e more');
  await page.locator('#new-command').fill('bash');
  await page.locator('#create').click();
  await page.locator('#roster >> text=e2e more').first().waitFor({ timeout: 15000 });
  await floorDrawn('after the New form made an agent');
});

await check('Esc and the x close the New form and Settings, and the floor stays drawn', async () => {
  let step = 'start';
  try {
    await page.evaluate(() => { location.hash = '#/roster'; });
    step = 'open Settings';
    await page.locator('#go-settings').click();
    await page.locator('#screen-settings').waitFor({ state: 'visible', timeout: 5000 });
    if (await page.locator('#screen-new').isVisible()) throw new Error('two panels show');
    await floorDrawn('with Settings open');
    step = 'Esc closes Settings';
    await page.keyboard.press('Escape');
    await page.locator('#screen-settings').waitFor({ state: 'hidden', timeout: 5000 });
    step = 'open New';
    await page.locator('#new-menu-btn').click();
    await page.locator('#new-menu >> text=More').first().click();
    await page.locator('#screen-new').waitFor({ state: 'visible', timeout: 5000 });
    step = 'the x closes New';
    await page.locator('#new-close').click();
    await page.locator('#screen-new').waitFor({ state: 'hidden', timeout: 5000 });
    await floorDrawn('after the panels closed');
  } catch (e) {
    throw new Error(`${step}: ${e.message}`);
  }
});

// Machines is a read-only panel: a row for each box on the (fake) tailnet,
// its profiles under it with the time each answers, and no control but x.
await check('Machines lists the boxes and their profiles, read only, and Esc closes it', async () => {
  await page.evaluate(() => { location.hash = '#/roster'; });
  await page.locator('#go-machines').click();
  await page.locator('#machines').waitFor({ state: 'visible', timeout: 5000 });
  await page.locator('#machines-list >> text=nas').first().waitFor({ timeout: 10000 });
  const text = await page.locator('#machines').innerText();
  for (const want of ['box', 'gpu', 'laptop', 'nas', 'desk', 'dead', 'No profile points here.', 'No answer', 'Key not set.', 'last seen']) {
    if (!text.includes(want)) throw new Error(`the panel lacks "${want}": ${text}`);
  }
  if (!/desk[\s\S]*?(\d+ ms|under 1 ms)/.test(text)) throw new Error(`desk shows no answer time: ${text}`);
  const controls = await page.locator('#machines button, #machines input, #machines select').count();
  if (controls !== 1) throw new Error(`the panel has ${controls} controls, not just the x`);
  await floorDrawn('with Machines open');
  await shot('machines');
  await page.keyboard.press('Escape');
  await page.locator('#machines').waitFor({ state: 'hidden', timeout: 5000 });
});

// stripTiles reloads the desk at 1440 by height, with the strip pinned or
// not, and says how many windows it draws.
async function stripTiles(height, pinned) {
  await page.setViewportSize({ width: 1440, height });
  await page.evaluate((on) => { if (on) localStorage.setItem('coppice.colony.pinned', '1'); else localStorage.removeItem('coppice.colony.pinned'); }, pinned);
  await page.reload({ waitUntil: 'load' });
  await page.waitForSelector('#tiles', { timeout: 10000 });
  await page.waitForTimeout(900);
  return tileCount();
}
for (const height of [760, 820, 900, 1000]) {
  await check(`the pinned colony strip never removes a window at 1440 by ${height}`, async () => {
    try {
      const without = await stripTiles(height, false);
      const withStrip = await stripTiles(height, true);
      await shot(`strip-pinned-${height}`);
      if (withStrip < without) throw new Error(`the strip took ${without - withStrip} window(s): ${without} without, ${withStrip} with it`);
    } finally {
      await page.evaluate(() => localStorage.removeItem('coppice.colony.pinned'));
      await page.setViewportSize({ width: 1440, height: 900 });
      await page.reload({ waitUntil: 'load' });
      await page.waitForSelector('#tiles', { timeout: 10000 });
    }
  });
}

// The floor's foreman for the chat: fake-foreman.sh, the claude table. The
// desk checks ran before it, so their windows never held it. No sentence
// goes in first: floor.chat holds the tracked foreman's replies anyway.
await check('the floor starts the fake foreman', async () => {
  rpcCall('floor.foreman', { harness: 'claude' });
});

// The chat is a window on the desk, and takes a window the way an agent
// does: only a free one. The desk is full here, so the foreman's roster row
// says it holds a reply, and a click on the row puts the chat in a window
// with the reply, drawn from floor.chat, and a line of its own. Screen in
// its header shows the foreman's terminal (its canvas), and Chat returns.
await check('with no free window the foreman row shows its reply, and its window draws the chat', async () => {
  const mark = page.locator('#roster .reply');
  await mark.first().waitFor({ timeout: 40000 });
  if (await page.locator('#tiles .chat-window').count()) throw new Error('the chat took a window that was not free');
  // The rail draws again on every beat, so the click goes to the row in the page.
  await page.evaluate(() => document.querySelector('#roster .reply').closest('li').click());
  const win = page.locator('#tiles .chat-window');
  await win.locator('.msg-agent', { hasText: 'I can see e2e shell' }).first().waitFor({ timeout: 10000 });
  await win.locator('.prompt-input').waitFor({ timeout: 5000 });
  if (await win.locator('canvas').count()) throw new Error('the chat window still holds the terminal canvas');
  await shot('desk-chat');
});

await check('Screen in the chat window header shows the foreman terminal, and Chat returns', async () => {
  await page.locator('#tiles .chat-window .screen-toggle').click();
  const term = page.locator('#tiles section:has(.screen-toggle) canvas');
  await term.first().waitFor({ timeout: 5000 });
  if (await page.locator('#tiles .chat-window').count()) throw new Error('the chat window stayed after Screen');
  await page.locator('#tiles .screen-toggle', { hasText: 'Chat' }).click();
  await page.locator('#tiles .chat-window .msg-agent').first().waitFor({ timeout: 5000 });
});

// The phone. Every check below runs on one 390 x 844 page with touch.
const phone = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });
const p = await phone.newPage();
failPage = p;
p.on('pageerror', (e) => pageErrors.push(String(e)));
p.on('console', (m) => {
  if (m.type() === 'error') pageErrors.push(m.text());
});
// speechFrames are the binary frames the phone's socket receives: each is
// one sentence of speech from the fake speech server.
const speechFrames = [];
p.on('websocket', (ws) => ws.on('framereceived', (e) => {
  if (typeof e.payload !== 'string') speechFrames.push(Buffer.from(e.payload));
}));
const sideways = async () => {
  const scroll = await p.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  if (scroll > 1) throw new Error(`the page scrolls sideways by ${scroll}px at 390px`);
};

await check('a phone-sized page shows the roster in its fold', async () => {
  await p.goto(`${base}/#t=${token}`, { waitUntil: 'load', timeout: 20000 });
  await p.locator('#agents-fold').waitFor({ timeout: 15000 });
  await p.waitForFunction(() => /agents/.test(document.getElementById('agents-fold').textContent), null, { timeout: 15000 });
  await p.locator('#agents-fold').click();
  await p.locator('#roster >> text=e2e shell').first().waitFor({ timeout: 15000 });
  await sideways();
  await shot('phone-roster', p);
  await p.locator('#agents-fold').click();
});

await check('the phone chat shows the fake foreman\'s reply', async () => {
  await p.locator('#chat-list .msg-agent', { hasText: 'I can see e2e shell' }).first().waitFor({ timeout: 30000 });
});

await check('home reads facts, needs-you cards, the fold, the chat, with the chat bar docked at the bottom', async () => {
  // Earlier checks add chat lines, and the home may sit scrolled down to
  // its chat; the order is read from the top, so scroll every container
  // of the home back first.
  await p.evaluate(() => {
    for (let el = document.querySelector('#facts'); el; el = el.parentElement) el.scrollTop = 0;
    window.scrollTo(0, 0);
  });
  const top = async (sel) => {
    const b = await p.locator(sel).first().boundingBox();
    if (!b) throw new Error(`${sel} is not on screen`);
    return b.y;
  };
  const order = [['#facts', await top('#facts')], ['#needs', await top('#needs')], ['#agents-fold', await top('#agents-fold')], ['#chat', await top('#chat')]];
  for (let i = 1; i < order.length; i++) {
    if (!(order[i - 1][1] < order[i][1])) throw new Error(`${order[i - 1][0]} is not above ${order[i][0]}: ${JSON.stringify(order)}`);
  }
  const bar = await p.locator('#tell').boundingBox();
  if (Math.abs(bar.y + bar.height - 844) > 2) throw new Error(`the chat bar ends at ${bar.y + bar.height}, not at the bottom`);
  await sideways();
});

await check('a sentence shows in the chat at once as sent, then read', async () => {
  const words = 'please look at the e2e parser';
  await p.locator('#tell-text').fill(words);
  await p.locator('#tell-send').click();
  const line = p.locator('#chat-list .msg-owner', { hasText: words }).last();
  await line.waitFor({ timeout: 5000 });
  await p.waitForFunction((w) => [...document.querySelectorAll('#chat-list .msg-owner')]
    .some((li) => li.textContent.includes(w) && / sent/.test(li.querySelector('.msg-head').textContent)), words, { timeout: 5000, polling: 50 });
  await shot('phone-chat-sent', p);
  await p.waitForFunction((w) => [...document.querySelectorAll('#chat-list .msg-owner')]
    .some((li) => li.textContent.includes(w) && /read/.test(li.querySelector('.msg-head').textContent)), words, { timeout: 30000 });
  await p.locator('#chat-list .msg-agent', { hasText: 'I asked e2e shell' }).first().waitFor({ timeout: 10000 });
  await sideways();
  await shot('phone-chat', p);
});

await check('a typed sentence is answered in text only: the socket carries no speech frame', async () => {
  // The reply row is on screen first, so the check does not pass before the
  // reply could have been spoken; then a moment for any audio to arrive.
  await p.locator('#chat-list .msg-agent', { hasText: 'I asked e2e shell' }).first().waitFor({ timeout: 30000 });
  await p.waitForTimeout(1500);
  if (speechFrames.length) throw new Error(`${speechFrames.length} speech frame(s) came for a typed sentence`);
});

await check('a sentence the mic filled is answered aloud: the speech server\'s audio arrives as one binary frame', async () => {
  const words = 'now look at the e2e lexer';
  // record.js does this when the mic puts words in the bar.
  await p.evaluate(() => window.coppice.speech.markVoiced());
  await p.locator('#tell-text').fill(words);
  await p.locator('#tell-send').click();
  await p.waitForFunction((w) => [...document.querySelectorAll('#chat-list .msg-owner')]
    .some((li) => li.textContent.includes(w) && /read/.test(li.querySelector('.msg-head').textContent)), words, { timeout: 30000 });
  const t0 = Date.now();
  while (speechFrames.length === 0) {
    if (Date.now() - t0 > 30000) {
      // Say what the chat held, so a miss can be read.
      let rows = '';
      try { rows = rpcCall('floor.chat', { limit: 8 }).replace(/\s+/g, ' ').slice(0, 900); } catch { /* the chat could not be read */ }
      const st = await p.evaluate(() => JSON.stringify({ mode: window.coppice.speech.mode(), debug: window.coppice.speech.debug(), status: document.getElementById('status').textContent }));
      throw new Error(`no speech frame came for a voiced sentence: ${st} ${rows}`);
    }
    await p.waitForTimeout(100);
  }
  const f = speechFrames[0];
  const n = f.readUInt32BE(0);
  const head = JSON.parse(f.subarray(4, 4 + n).toString('utf8'));
  if (head.event !== 'speech' || head.type !== 'audio/mpeg' || typeof head.id !== 'string') throw new Error(`header ${JSON.stringify(head)}`);
  const audio = f.subarray(4 + n).toString('utf8');
  if (audio !== 'ID3 fake audio') throw new Error(`audio ${JSON.stringify(audio)}`);
  // Headless chromium cannot play those bytes: the page says so and the
  // browser's voice takes the reply.
  await p.waitForFunction(() => /browser's voice/.test(document.getElementById('status').textContent), null, { timeout: 10000 });
  await shot('phone-speech', p);
});

await check('Settings shows where replies are spoken and holds the Read replies aloud choice', async () => {
  await p.evaluate(() => { location.hash = '#/settings'; });
  await p.locator('#screen-settings').waitFor({ state: 'visible', timeout: 5000 });
  await p.waitForFunction(() => /Speech server http:\/\/127\.0\.0\.1:\d+\/v1\/audio\/speech\. Model kokoro, voice af_heart\./
    .test(document.getElementById('set-speech').textContent), null, { timeout: 10000 });
  await p.locator('#set-read-aloud').check();
  const stored = await p.evaluate(() => localStorage.getItem('coppice.readAloud'));
  if (stored !== '1') throw new Error(`the choice was not kept: ${stored}`);
  await p.locator('#set-read-aloud').uncheck();
  await p.evaluate(() => { location.hash = '#/roster'; });
});

await check('on a phone Settings rises from the bottom over the home, and the x closes it', async () => {
  await p.evaluate(() => { location.hash = '#/settings'; });
  await p.locator('#screen-settings').waitFor({ state: 'visible', timeout: 5000 });
  if (!(await p.locator('#screen-roster').isVisible())) throw new Error('the home is hidden under Settings');
  await p.waitForTimeout(400);
  const box = await p.locator('#screen-settings').boundingBox();
  if (Math.abs(box.y + box.height - 844) > 2) throw new Error(`the sheet ends at ${box.y + box.height}, not at the bottom`);
  if (box.y < 100) throw new Error('the sheet covers the whole home');
  await sideways();
  await shot('phone-settings', p);
  await p.locator('#settings-close').click();
  await p.locator('#screen-settings').waitFor({ state: 'hidden', timeout: 5000 });
});

await check('on a phone Machines rises from the bottom over the home, and the x closes it', async () => {
  await p.evaluate(() => { location.hash = '#/roster'; });
  await p.locator('#go-machines').click();
  await p.locator('#machines').waitFor({ state: 'visible', timeout: 5000 });
  await p.locator('#machines-list >> text=gpu').first().waitFor({ timeout: 10000 });
  await p.waitForTimeout(400);
  const box = await p.locator('#machines').boundingBox();
  if (Math.abs(box.y + box.height - 844) > 2) throw new Error(`the sheet ends at ${box.y + box.height}, not at the bottom`);
  await sideways();
  await shot('phone-machines', p);
  await p.locator('#machines-close').click();
  await p.locator('#machines').waitFor({ state: 'hidden', timeout: 5000 });
});

// swipeRight drags one finger right across the sheet, as a thumb does.
async function swipeRight(page, y) {
  const cdp = await page.context().newCDPSession(page);
  const at = (x) => [{ x, y }];
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: at(80) });
  for (const x of [110, 160, 220, 280]) await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: at(x) });
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
}

// The shell agent is open in a desk window, so the phone shares it.
await check('the desk holds the shell agent, so the phone\'s sheet does not resize it', async () => {
  await openTile('e2e shell');
  await page.waitForTimeout(500);
});

await check('a chip opens the agent\'s sheet, it takes typed words, and a swipe right goes back with no reload', async () => {
  await p.evaluate(() => { window.__noReload = 'kept'; });
  const chip = p.locator('#chat-list .chip-agent', { hasText: 'e2e shell' }).last();
  await chip.click();
  await p.locator('#screen-pane').waitFor({ state: 'visible', timeout: 10000 });
  await p.waitForFunction(() => document.getElementById('pane-label').textContent.includes('e2e shell'), null, { timeout: 10000 });
  await p.locator('#text').fill('echo typed-in-a-sheet');
  await p.locator('#send-enter').click();
  await p.waitForFunction(() => document.getElementById('grid-text').textContent.split('typed-in-a-sheet').length > 2, null, { timeout: 15000 });
  await sideways();
  await p.waitForFunction(() => document.getElementById('view-line').textContent === 'Not resized: open elsewhere, or sized by hand.', null, { timeout: 10000 });
  await shot('phone-sheet', p);
  await swipeRight(p, 300);
  await p.locator('#screen-pane').waitFor({ state: 'hidden', timeout: 10000 });
  if ((await p.evaluate(() => window.__noReload)) !== 'kept') throw new Error('the page reloaded');
  await p.locator('#chat-list .msg-agent').first().waitFor({ timeout: 5000 });
  await sideways();
});

// openSheetOf opens the sheet of the agent labelled label on the phone page.
async function openSheetOf(label) {
  const id = await p.evaluate((l) => window.coppice.state.panes.find((x) => x.label === l).id, label);
  await p.evaluate((i) => { location.hash = '#/pane/' + encodeURIComponent(i); }, id);
  await p.locator('#screen-pane').waitFor({ state: 'visible', timeout: 10000 });
  await p.waitForFunction((l) => document.getElementById('pane-label').textContent.includes(l), label, { timeout: 10000 });
}
async function closeSheet() {
  await swipeRight(p, 300);
  await p.locator('#screen-pane').waitFor({ state: 'hidden', timeout: 10000 });
}

// The desk page closes, so no other client holds the shell agent and the
// phone may fit it to the sheet.
await check('alone, the shell agent is fitted to the phone and draws with no sideways scroll', async () => {
  await page.close();
  await p.waitForTimeout(600);
  await openSheetOf('e2e shell');
  await p.waitForFunction(() => document.getElementById('view-line').textContent === 'Sized to this phone.', null, { timeout: 10000 });
  // The agent redraws at its new size, and the first frame of that size is
  // what the check reads.
  await p.waitForFunction(() => {
    const w = document.getElementById('canvas-wrap');
    return document.getElementById('grid-text').textContent.trim().length > 0 && w.scrollWidth <= w.clientWidth + 1;
  }, null, { timeout: 15000 });
  const wrap = await p.evaluate(() => {
    const w = document.getElementById('canvas-wrap');
    return { scroll: w.scrollWidth, shown: w.clientWidth };
  });
  if (wrap.scroll > wrap.shown + 1) throw new Error(`the screen scrolls sideways: ${wrap.scroll}px in ${wrap.shown}px`);
  await sideways();
  await p.locator('#text').fill('echo fitted-to-phone');
  await p.locator('#send-enter').click();
  await p.waitForFunction(() => document.getElementById('grid-text').textContent.split('fitted-to-phone').length > 2, null, { timeout: 15000 });
  await shot('phone-fit', p);
  await closeSheet();
});

// The fake foreman has a transcript, so its sheet opens on the reading view.
await check('an agent with a transcript opens on the reading view, and the compose bar types into it', async () => {
  await openSheetOf('foreman');
  await p.locator('#reading').waitFor({ state: 'visible', timeout: 10000 });
  if (await p.locator('#canvas-wrap').isVisible()) throw new Error('the screen shows over the reading view');
  const line = await p.locator('#view-line').textContent();
  if (line !== 'From the transcript.') throw new Error(`the line is ${JSON.stringify(line)}`);
  await p.locator('#view-toggle', { hasText: 'Screen' }).waitFor({ timeout: 5000 });
  await p.locator('#reading-list .msg-text', { hasText: 'I can see e2e shell on the floor.' }).first().waitFor({ timeout: 10000 });
  await sideways();
  await shot('phone-reading', p);
  const replies = () => p.evaluate(() => [...document.querySelectorAll('#reading-list .msg-agent .msg-text')].filter((e) => e.textContent.includes('Got it.')).length);
  const before = await replies();
  await p.locator('#text').fill('hello from the reading view');
  await p.locator('#send-enter').click();
  await p.locator('#reading-list .msg-owner', { hasText: 'hello from the reading view' }).first().waitFor({ timeout: 30000 });
  await p.waitForFunction((n) => [...document.querySelectorAll('#reading-list .msg-agent .msg-text')].filter((e) => e.textContent.includes('Got it.')).length > n, before, { timeout: 30000 });
  await sideways();
});

await check('Screen shows the live canvas, and Messages brings the reading view back', async () => {
  await p.locator('#view-toggle', { hasText: 'Screen' }).click();
  await p.locator('#canvas-wrap').waitFor({ state: 'visible', timeout: 10000 });
  await p.locator('#reading').waitFor({ state: 'hidden', timeout: 5000 });
  await p.waitForFunction(() => /\u276f/.test(document.getElementById('grid-text').textContent), null, { timeout: 15000 });
  await shot('phone-reading-screen', p);
  await p.locator('#view-toggle', { hasText: 'Messages' }).click();
  await p.locator('#reading').waitFor({ state: 'visible', timeout: 10000 });
  await closeSheet();
});

// A second trust screen, for the phone: the desk answered the first.
await check('the trust agent\'s bar shows over its view on the phone', async () => {
  const here = path.dirname(fileURLToPath(import.meta.url));
  const screen = path.join(here, '..', 'testdata', 'screens', 'claude', 'blocked-10.txt');
  rpcCall('pane.create', {
    cwd: path.join(process.env.E2E_DIR, 'proj', 'alpha'), kind: 'pty', harness: 'claude', label: 'e2e trust phone',
    cmd_argv: ['bash', path.join(here, 'fake-trust.sh'), screen],
  });
  await p.waitForFunction(() => (window.coppice.state.panes || []).some((x) => x.label === 'e2e trust phone'), null, { timeout: 15000 });
  await openSheetOf('e2e trust phone');
  const bar = p.locator('#trust');
  await bar.waitFor({ state: 'visible', timeout: 20000 });
  await bar.locator('#trust-no').waitFor({ state: 'visible', timeout: 5000 });
  await bar.locator('#trust-yes').waitFor({ state: 'visible', timeout: 5000 });
  if (!(await p.locator('#canvas-wrap').isVisible())) throw new Error('the trust agent shows no view under its bar');
  await sideways();
  await shot('phone-trust', p);
});

await phone.close();

await check('the page raised no script errors', async () => {
  if (pageErrors.length) throw new Error(pageErrors.join(' | '));
});

await browser.close();
console.log(failed ? `${failed} web check(s) failed` : 'web ok');
process.exit(failed ? 1 : 0);

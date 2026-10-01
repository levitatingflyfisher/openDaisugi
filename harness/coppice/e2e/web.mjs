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

// The floor's foreman for the chat: fake-foreman.sh, the claude table. The
// desk checks ran before it, so their windows never held it. No sentence
// goes in first: floor.chat holds the tracked foreman's replies anyway.
await check('the floor starts the fake foreman', async () => {
  rpcCall('floor.foreman', { harness: 'claude' });
});

// The phone. Every check below runs on one 390 x 844 page with touch.
const phone = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });
const p = await phone.newPage();
failPage = p;
p.on('pageerror', (e) => pageErrors.push(String(e)));
p.on('console', (m) => {
  if (m.type() === 'error') pageErrors.push(m.text());
});
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

// swipeRight drags one finger right across the sheet, as a thumb does.
async function swipeRight(page, y) {
  const cdp = await page.context().newCDPSession(page);
  const at = (x) => [{ x, y }];
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: at(80) });
  for (const x of [110, 160, 220, 280]) await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: at(x) });
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
}

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
  await shot('phone-sheet', p);
  await swipeRight(p, 300);
  await p.locator('#screen-pane').waitFor({ state: 'hidden', timeout: 10000 });
  if ((await p.evaluate(() => window.__noReload)) !== 'kept') throw new Error('the page reloaded');
  await p.locator('#chat-list .msg-agent').first().waitFor({ timeout: 5000 });
  await sideways();
});

await phone.close();

await check('the page raised no script errors', async () => {
  if (pageErrors.length) throw new Error(pageErrors.join(' | '));
});

await browser.close();
console.log(failed ? `${failed} web check(s) failed` : 'web ok');
process.exit(failed ? 1 : 0);

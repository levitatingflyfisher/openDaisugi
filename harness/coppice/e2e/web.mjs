// web.mjs: the web floor, driven in headless chromium like a person would.
//
//   node web.mjs BASE_URL TOKEN SHOTS_DIR
//
// BASE_URL serves the floor and TOKEN signs in, the way a scanned QR does.
// run.sh has made the panes "e2e shell", "e2e ask" (with a gate ask held on
// it) and "e2e trust" (fake-trust.sh, claude's folder trust screen). Every check
// that fails saves a screenshot named after it and exits 1.
import { chromium } from 'playwright';
import { mkdirSync } from 'node:fs';
import path from 'node:path';

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
async function check(name, fn) {
  try {
    await fn();
    console.log(`ok   ${name}`);
  } catch (e) {
    failed++;
    console.log(`FAIL ${name}: ${e && e.message ? e.message.split('\n')[0] : e}`);
    try {
      await shot(`FAIL-${name.replace(/[^a-z0-9]+/gi, '-')}`);
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

await check('a phone-sized page shows the roster', async () => {
  const phone = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });
  const p = await phone.newPage();
  p.on('pageerror', (e) => pageErrors.push(String(e)));
  await p.goto(`${base}/#t=${token}`, { waitUntil: 'load', timeout: 20000 });
  await p.locator('#roster >> text=e2e shell').first().waitFor({ timeout: 15000 });
  const scroll = await p.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  if (scroll > 1) throw new Error(`the page scrolls sideways by ${scroll}px at 390px`);
  await shot('phone-roster', p);
  await phone.close();
});

await check('the page raised no script errors', async () => {
  if (pageErrors.length) throw new Error(pageErrors.join(' | '));
});

await browser.close();
console.log(failed ? `${failed} web check(s) failed` : 'web ok');
process.exit(failed ? 1 : 0);

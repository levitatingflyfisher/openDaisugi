// scripts/phone_smoke.mjs
//
// Drives the coppice PWA on a phone-sized viewport and shoots one PNG per
// screen. Copied to ~/.cache/oh-visual-loop by phone_smoke.py, because ES
// module resolution ignores NODE_PATH and playwright lives there.
//
// usage: node phone_smoke.mjs <baseUrl> <token> <outDir> <paneLabel> <newCwd> [--insecure]
import { chromium } from 'playwright';
import { mkdirSync } from 'node:fs';

const [baseUrl, token, outDir, paneLabel, newCwd] = process.argv.slice(2);
const insecure = process.argv.includes('--insecure');
if (!baseUrl || !token || !outDir || !paneLabel || !newCwd) {
  console.error('usage: node phone_smoke.mjs <baseUrl> <token> <outDir> <paneLabel> <newCwd> [--insecure]');
  process.exit(2);
}
mkdirSync(outDir, { recursive: true });

// scale: 'css' asks for one pixel per CSS pixel in the saved PNG, so a
// screenshot is 360x780 even though deviceScaleFactor renders it at twice
// that density first. The extra density is what keeps text and lines
// looking like a real phone screen instead of a blurry downscale.
const SHOT = { scale: 'css' };

const prefix = insecure ? 'https-' : '';
const browser = await chromium.launch();
const context = await browser.newContext({
  viewport: { width: 360, height: 780 },
  deviceScaleFactor: 2,
  ignoreHTTPSErrors: insecure,
});
const page = await context.newPage();
page.on('console', (m) => console.log('[page]', m.type(), m.text()));

// closeBrowser is idempotent and swallows its own errors, so it is safe to
// call from both the normal cleanup path and a failure path without ever
// throwing a second error on top of the first one.
let closed = false;
async function closeBrowser() {
  if (closed) return;
  closed = true;
  try {
    await browser.close();
  } catch (closeErr) {
    console.error('could not close the browser:', closeErr.message);
  }
}

// fail is the one way this script reports a problem. The screenshot is in
// its own try so a page that has already crashed cannot stop fail from
// still closing the browser and exiting.
const fail = async (msg) => {
  try {
    await page.screenshot({ path: `${outDir}/${prefix}failure.png`, ...SHOT });
  } catch (shotErr) {
    console.error('could not save the failure screenshot:', shotErr.message);
  }
  console.error('FAIL:', msg);
  await closeBrowser();
  process.exit(1);
};

async function run() {
  // Sign in the way a scanned QR does.
  await page.goto(`${baseUrl}/#t=${token}`, { waitUntil: 'load', timeout: 20000 });
  await page.waitForTimeout(1500);

  if (!insecure) {
    // ready resolves only for an ACTIVE worker. getRegistration would resolve
    // for one that is still installing, or one whose install rejected because
    // cache.addAll failed, so it would pass on exactly the failure this pass
    // exists to catch.
    const active = await page.evaluate(() => Promise.race([
      navigator.serviceWorker.ready.then(() => true),
      new Promise((r) => setTimeout(() => r(false), 10000)),
    ]));
    if (!active) return fail('the service worker never became active over http://127.0.0.1');
  }

  await page.waitForSelector(`#roster .card:has-text("${paneLabel}")`, { timeout: 20000 })
    .catch(() => fail(`the roster never showed a pane labelled ${paneLabel}`));
  await page.screenshot({ path: `${outDir}/${prefix}roster.png`, ...SHOT });

  if (insecure) {
    console.log('https pass ok');
    return;
  }

  await page.click(`#roster .card:has-text("${paneLabel}")`);
  await page.waitForSelector('#screen-pane:not([hidden])', { timeout: 10000 });
  await page.waitForTimeout(1500);

  // The keys drawer starts on the page with the hidden attribute, and a CSS
  // rule that fails to respect it is a real bug the drawer can catch here
  // instead of only in a screenshot a person has to notice.
  if (!(await page.isHidden('#keys'))) {
    return fail('the keys drawer is visible before it is opened');
  }
  await page.click('#keys-toggle');
  if (!(await page.isVisible('#keys'))) {
    return fail('the keys drawer did not open when its button was tapped');
  }
  await page.click('#keys-toggle');
  if (!(await page.isHidden('#keys'))) {
    return fail('the keys drawer did not close on a second tap');
  }

  await page.fill('#text', 'echo hello-from-phone');
  await page.click('#prompt');

  const seen = await page.waitForFunction(
    () => document.getElementById('grid-text').textContent.includes('hello-from-phone'),
    null, { timeout: 20000 },
  ).then(() => true).catch(() => false);
  if (!seen) return fail('the echo never appeared in the pane grid');

  // Steer is the other primary action on this screen. If the compose row
  // wraps, its buttons can fall below the viewport and a person would have
  // to scroll to find it, on a screen that is supposed to fit a phone.
  const steerBox = await page.locator('#steer').boundingBox();
  if (!steerBox || steerBox.x < 0 || steerBox.y < 0
    || steerBox.x + steerBox.width > 360 || steerBox.y + steerBox.height > 780) {
    return fail('the Steer button is not fully inside the 360x780 viewport');
  }
  await page.screenshot({ path: `${outDir}/pane.png`, ...SHOT });

  await page.goto(`${baseUrl}/#/new`);
  await page.waitForSelector('#screen-new:not([hidden])');
  if (!(await page.isHidden('#new-harness-label'))) {
    return fail('the harness label is visible for a pty pane');
  }
  await page.fill('#new-cwd', newCwd);
  await page.screenshot({ path: `${outDir}/new.png`, ...SHOT });

  await page.goto(`${baseUrl}/#/settings`);
  await page.waitForSelector('#screen-settings:not([hidden])');
  await page.waitForTimeout(300);
  // The token field fills itself from what was scanned. A real bearer token
  // saved into a public repo has no security cost here, since it belongs to
  // a server that is already gone by the time this file is committed, but
  // it churns the picture on every run for no reason. A fixed placeholder
  // keeps the screenshot byte-stable so a real UI change is what shows up
  // in a diff, not a new random token.
  await page.fill('#set-token', 'a-token-goes-here');
  await page.screenshot({ path: `${outDir}/settings.png`, ...SHOT });

  console.log('smoke ok');
}

try {
  await run();
} catch (e) {
  await fail(e && e.message ? e.message : String(e));
} finally {
  await closeBrowser();
}

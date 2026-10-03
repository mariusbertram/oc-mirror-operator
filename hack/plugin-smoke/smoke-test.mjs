// Validate actual federation against origin-console, not a standalone dev server.
// Seed via seed.mjs before running. Screenshots and a JSON report survive failures.
import { chromium } from 'playwright';
import assert from 'node:assert/strict';
import { mkdir, writeFile } from 'node:fs/promises';
import path from 'node:path';

const BASE_URL = process.env.CONSOLE_URL || 'http://localhost:9000';
const ARTIFACTS = process.env.SMOKE_ARTIFACT_DIR || 'artifacts';
const PLUGIN_SIGNAL = /oc-mirror-operator|exposed-\w+Page|React error/i;
const PREFIX = '/oc-mirror/targets/demo-target';
const IMAGESET = `${PREFIX}/imagesets/demo-imageset`;
const CONTENT = `${PREFIX}/namespaces/oc-mirror-demo/imagesets/demo-imageset`;
const visible = (locator) => locator.waitFor({ state: 'visible', timeout: 30_000 });
const text = async (page, value) => visible(page.getByText(value, { exact: false }).first());
const button = (page, name) => page.getByRole('button', { name, exact: true });

async function tabs(page, names) {
  for (const [name, content] of names) {
    const tab = page.getByRole('tab', { name });
    await tab.click();
    assert.equal(await tab.getAttribute('aria-selected'), 'true', `Tab ${name} did not activate`);
    await text(page, content);
    await page.screenshot({
      path: path.join(ARTIFACTS, `tab-${new URL(page.url()).pathname.replace(/\W+/g, '_')}-${content.replace(/\W+/g, '_')}.png`),
      fullPage: true,
    });
  }
}

async function dialog(page, { opener, title, body, action, confirm = false }) {
  await button(page, opener).click();
  const modal = page.getByRole('dialog', { name: title, exact: true });
  await visible(modal);
  await visible(modal.getByRole('heading', { name: title, exact: true }));
  await visible(modal.getByText(body, { exact: false }));
  // Check the visible footer (not merely a title or detached DOM node).
  const footer = modal.locator('[class*="modal-box__footer"]');
  await visible(footer);
  await visible(button(footer, action));
  await visible(button(footer, 'Cancel'));
  await page.screenshot({ path: path.join(ARTIFACTS, `dialog-${title.replace(/\W+/g, '_')}.png`), fullPage: true });
  await button(footer, 'Cancel').click();
  await modal.waitFor({ state: 'hidden' });
  if (confirm) {
    await button(page, opener).click();
    const deleting = action === 'Delete';
    const response = page.waitForResponse((r) => r.request().method() === (deleting ? 'DELETE' : 'PATCH')
      && r.url().endsWith(deleting ? '/demo-imageset' : '/force-resync'));
    await button(modal, action).click();
    assert.equal((await response).status(), 204, `${action} API action failed`);
    await modal.waitFor({ state: 'hidden' });
    await text(page, deleting ? 'ImageSet "demo-imageset" deleted.' : 'Force resync requested.');
  }
}

const PAGES = [
  { path: '/oc-mirror/targets', title: 'Mirror Targets', body: 'demo-target' },
  { path: '/oc-mirror/imagesets', title: 'ImageSets', body: 'demo-imageset' },
  { path: '/oc-mirror/failed', title: 'Failed Images', body: 'Smoke fixture registry unavailable' },
  { path: `${PREFIX}/failures`, title: 'Image Failures — demo-target', body: 'Smoke fixture registry unavailable' },
  {
    path: PREFIX, title: 'demo-target', body: 'registry.example.com/mirror',
    interact: async (page) => {
      await tabs(page, [
        [/^Resources$/, 'IDMS'], [/^Catalogs$/, 'index-v1'],
        [/^Settings$/, 'Registry'], [/^Conditions$/, 'Smoke fixture is mirroring'],
        [/^ImageSets/, 'demo-imageset'],
      ]);
      await dialog(page, {
        opener: 'Delete', title: 'Delete ImageSet?', body: 'This will remove the ImageSet',
        action: 'Delete',
      });
      await button(page, 'Recollect').click();
      await text(page, 'Recollect requested for ImageSet');
      await tabs(page, [[/^Overview$/, 'Mirror progress']]);
    },
  },
  {
    path: IMAGESET, title: 'demo-imageset', body: 'Mirroring progress',
    interact: async (page) => {
      await tabs(page, [
        [/^Catalogs$/, 'Operator catalogs'], [/^Operators/, 'Operator catalogs (spec)'],
        [/^Helm/, 'Helm chart repositories'], [/^Additional Images/, 'Individual images mirrored as-is'],
        [/^Blocked Images/, 'No blocked images configured.'], [/^Overview$/, 'Mirroring progress'],
      ]);
      await dialog(page, {
        opener: 'Force Resync', title: 'Force resync this ImageSet?',
        body: 'Unlike Recollect, this also transfers images already marked as mirrored.',
        action: 'Force Resync', confirm: true,
      });
      await button(page, 'Recollect').click();
      await text(page, 'Recollect requested.');
    },
  },
  {
    path: `${CONTENT}/catalogs/index-v1`, title: 'index-v1', body: 'demo-operator',
    interact: async (page) => {
      await button(page, 'Expand').first().click();
      await text(page, 'stable');
      await button(page, 'Collapse').first().click();
      const remove = page.getByTitle('«Remove all', { exact: true });
      await remove.click();
      await visible(button(page, 'Save changes').first());
      await button(page, 'Cancel').last().click();
      await button(page, 'Save changes').first().waitFor({ state: 'hidden' });
    },
  },
  { path: `${CONTENT}/releases`, title: 'Release Browser — demo-imageset', body: 'Available OCP Channels' },
  // Destructive confirmation runs last, after all pages using this fixture.
  {
    path: PREFIX, title: 'demo-target', body: 'registry.example.com/mirror',
    interact: async (page) => {
      await tabs(page, [[/^ImageSets/, 'demo-imageset']]);
      await dialog(page, {
        opener: 'Delete', title: 'Delete ImageSet?', body: 'This will remove the ImageSet',
        action: 'Delete', confirm: true,
      });
    },
  },
];

async function checkPage(browser, spec) {
  const page = await browser.newPage();
  // Authentication is disabled only in the console shell. The real resource
  // API still requires a caller token for writes; never mock its responses.
  if (process.env.SMOKE_BEARER_TOKEN) {
    await page.route('**/api/proxy/plugin/oc-mirror-operator/resourceapi/**', (route) => route.continue({
      headers: { ...route.request().headers(), 'X-Forwarded-Access-Token': process.env.SMOKE_BEARER_TOKEN },
    }));
  }
  const fatal = [];
  const noise = [];
  page.on('pageerror', (err) => fatal.push(`[pageerror] ${err.message}`));
  page.on('console', (msg) => {
    if (msg.type() === 'error') {
      (PLUGIN_SIGNAL.test(msg.text()) ? fatal : noise).push(`[console.error] ${msg.text()}`);
    }
  });
  page.on('response', (resp) => {
    if (!resp.ok()) {
      (PLUGIN_SIGNAL.test(resp.url()) ? fatal : noise).push(`[response ${resp.status()}] ${resp.url()}`);
    }
  });
  page.on('requestfailed', (req) => {
    if (PLUGIN_SIGNAL.test(req.url())) fatal.push(`[requestfailed] ${req.url()} ${req.failure()?.errorText}`);
  });
  console.log(`Checking ${spec.path}`);
  const screenshot = path.join(ARTIFACTS, `plugin-smoke-${spec.path.replace(/\W+/g, '_')}.png`);
  try {
    // Console watch requests can remain open: networkidle is not a rendering signal.
    await page.goto(`${BASE_URL}${spec.path}`, { waitUntil: 'domcontentloaded', timeout: 60_000 });
    const heading = page.getByRole('heading', { name: spec.title, exact: true });
    // Newer console releases show a first-visit tour in a blocking modal.
    const skipTour = button(page, 'Skip tour');
    await visible(heading.or(skipTour));
    if (await skipTour.isVisible()) await skipTour.click();
    await visible(heading);
    await text(page, spec.body);
    if (spec.interact) await spec.interact(page);
    await page.waitForTimeout(500);
  } catch (err) {
    fatal.push(`[assertion] ${err.stack || err.message}`);
  }
  await page.screenshot({ path: screenshot, fullPage: true }).catch((err) => fatal.push(`[screenshot] ${err.message}`));
  await page.close();
  return { path: spec.path, title: spec.title, fatal, noise, screenshot };
}

async function main() {
  await mkdir(ARTIFACTS, { recursive: true });
  const browser = await chromium.launch();
  const results = [];
  try {
    for (const spec of PAGES) results.push(await checkPage(browser, spec));
  } finally {
    await browser.close();
  }
  await writeFile(path.join(ARTIFACTS, 'results.json'), JSON.stringify({
    consoleVersion: process.env.CONSOLE_VERSION || 'unspecified', baseURL: BASE_URL, results,
  }, null, 2));
  for (const result of results) {
    console.log(`${result.fatal.length ? 'FAIL' : 'OK'}: ${result.title} (${result.noise.length} console-shell errors ignored)`);
    for (const err of result.fatal) console.error(err);
  }
  assert(!results.some((r) => r.fatal.length), 'Plugin smoke test failed; see artifacts/results.json and screenshots');
  console.log('Plugin smoke passed: all eight routes, detail tabs, dialog titles/bodies/footers and actions.');
}

main().catch((err) => {
  console.error(err);
  process.exitCode = 1;
});

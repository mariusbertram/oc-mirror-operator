// Headless-browser smoke test for the OC Mirror console plugin.
//
// Loads each plugin page against a real OpenShift console (bridge) container
// with the plugin registered via BRIDGE_PLUGINS, and fails if the browser
// throws or logs an error that traces back to *our* plugin. This is the
// check that would have caught the "Minified React error #130" regression:
// build-time TypeScript/webpack validation cannot see a runtime crash caused
// by a version mismatch between the plugin's bundled dependencies and what
// the console actually federates at runtime — only an actual browser can.
//
// The bare console shell (running against a plain Kind cluster with none of
// the OpenShift services it optionally integrates with — Thanos,
// AlertManager, GitOps, user-settings ConfigMap storage, OperatorHub, etc.)
// logs a baseline of its own unrelated console.error/failed-response noise
// regardless of the plugin. Those are printed for visibility but don't fail
// the run; only errors traceable to our plugin's own URLs/bundle do.
//
// Usage: node smoke-test.mjs
// Env:   CONSOLE_URL (default http://localhost:9000)

import { chromium } from 'playwright';

const BASE_URL = process.env.CONSOLE_URL || 'http://localhost:9000';
const PLUGIN_NAME = 'oc-mirror-operator';
// Matches our plugin's static-asset/proxy URLs and its own error signatures
// (e.g. "at exposed-MirrorTargetListPage-chunk.js", "React error #130").
const PLUGIN_SIGNAL = new RegExp(`${PLUGIN_NAME}|exposed-\\w+Page|React error`, 'i');

// expectText: text that must appear in the page body — proves the plugin's
// resource-API round trip actually returned our seeded MirrorTarget/ImageSet
// data, not just an empty state that happens not to crash.
const PAGES = [
  { path: '/oc-mirror/targets', name: 'Mirror Targets list', expectText: 'demo-target' },
  { path: '/oc-mirror/imagesets', name: 'ImageSets list', expectText: 'demo-imageset' },
  { path: '/oc-mirror/failed', name: 'Failed Images (all)', expectText: null },
  { path: '/oc-mirror/targets/demo-target', name: 'Mirror Target detail', expectText: 'demo-target' },
];

async function checkPage(browser, { path, name, expectText }) {
  const page = await browser.newPage();
  const fatal = [];
  const noise = [];

  // Uncaught JS exceptions are always fatal, wherever they come from — a
  // healthy page (ours or the console shell's own chrome) shouldn't throw.
  page.on('pageerror', (err) => fatal.push(`[pageerror] ${err.message}`));

  page.on('console', (msg) => {
    if (msg.type() !== 'error') return;
    const text = msg.text();
    (PLUGIN_SIGNAL.test(text) ? fatal : noise).push(`[console.error] ${text}`);
  });

  page.on('response', (resp) => {
    if (resp.ok()) return;
    const url = resp.url();
    const line = `[response ${resp.status()}] ${url}`;
    (PLUGIN_SIGNAL.test(url) ? fatal : noise).push(line);
  });

  const url = `${BASE_URL}${path}`;
  console.log(`\n=== ${name} (${url}) ===`);
  try {
    await page.goto(url, { waitUntil: 'networkidle', timeout: 30_000 });
    // Let any async render/error settle after the network goes idle.
    await page.waitForTimeout(2000);
  } catch (err) {
    fatal.push(`[navigation] ${err.message}`);
  }

  const bodyText = await page.textContent('body').catch(() => '');
  const screenshotPath = `/tmp/plugin-smoke-${path.replace(/\W+/g, '_')}.png`;
  await page.screenshot({ path: screenshotPath, fullPage: true }).catch(() => {});

  const missingText = expectText && !bodyText?.includes(expectText);
  await page.close();

  return { name, path, fatal, noise, missingText, expectText, screenshotPath };
}

async function main() {
  const browser = await chromium.launch();
  const results = [];
  for (const p of PAGES) {
    results.push(await checkPage(browser, p));
  }
  await browser.close();

  let failed = false;
  for (const r of results) {
    if (r.noise.length > 0) {
      console.log(`  (${r.noise.length} unrelated console-shell error(s) ignored — not from the plugin)`);
    }
    if (r.fatal.length > 0) {
      failed = true;
      console.error(`\nFAIL: ${r.name} (${r.path}) — ${r.fatal.length} plugin-related error(s):`);
      for (const e of r.fatal) console.error(`  ${e}`);
      console.error(`  screenshot: ${r.screenshotPath}`);
    } else {
      console.log(`OK: ${r.name} — no plugin-related errors`);
    }
    if (r.missingText) {
      failed = true;
      console.error(`FAIL: ${r.name} (${r.path}) did not contain expected text "${r.expectText}" — seeded data did not round-trip through the plugin's resource API.`);
      console.error(`  screenshot: ${r.screenshotPath}`);
    }
  }

  if (failed) {
    console.error('\nPlugin smoke test FAILED.');
    process.exit(1);
  }
  console.log('\nPlugin smoke test passed — all pages rendered without plugin errors.');
}

main().catch((err) => {
  console.error('Smoke test crashed:', err);
  process.exit(1);
});

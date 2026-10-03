// Validate actual federation against origin-console, not a standalone dev server.
// Seed via seed.mjs before running. Screenshots and a JSON report survive failures.
import { chromium } from 'playwright';
import assert from 'node:assert/strict';
import { mkdir, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const BASE_URL = process.env.CONSOLE_URL || 'http://localhost:9000';
const ARTIFACT_ROOT = process.env.SMOKE_ARTIFACT_DIR || 'artifacts';
let ARTIFACTS;
let themeEvidence;
const PLUGIN_SIGNAL = /oc-mirror-operator|exposed-\w+Page|React error/i;
const PREFIX = '/oc-mirror/targets/demo-target';
const IMAGESET = `${PREFIX}/imagesets/demo-imageset`;
const CONTENT = `${PREFIX}/namespaces/oc-mirror-demo/imagesets/demo-imageset`;
const visible = (locator) => locator.waitFor({ state: 'visible', timeout: 30_000 });
const text = async (page, value) => visible(page.getByText(value, { exact: false }).first());
const button = (page, name) => page.getByRole('button', { name, exact: true });

function reseed() {
  const context = process.env.SMOKE_KUBE_CONTEXT;
  const args = context ? ['--context', context] : [];
  const fixture = (extra = []) => execFileSync(process.execPath,
    [fileURLToPath(new URL('./seed.mjs', import.meta.url)), ...extra]);
  // Resync writes compressed image-state keys; apply alone leaves those keys
  // and annotations behind, making the next theme's fixture inconsistent.
  execFileSync('kubectl', [...args, 'delete', '-f', '-', '--ignore-not-found=true', '--wait=true'],
    { input: fixture(), stdio: ['pipe', 'inherit', 'inherit'] });
  execFileSync('kubectl', [...args, 'apply', '-f', '-'], { input: fixture(), stdio: ['pipe', 'inherit', 'inherit'] });
  execFileSync('kubectl', [...args, 'patch', 'mirrortarget', 'demo-target', '-n', 'oc-mirror-demo',
    '--subresource=status', '--type=merge', '-p', fixture(['--status']).toString().trim()],
  { stdio: 'inherit' });
}

async function themeState(page, theme) {
  const state = await page.evaluate(() => {
    const root = document.documentElement;
    const style = getComputedStyle(root);
    const pf6 = style.getPropertyValue('--pf-t--global--background--color--primary--default').trim();
    const background = pf6 || style.getPropertyValue('--pf-v5-global--BackgroundColor--100').trim();
    const canvas = document.createElement('canvas');
    const ctx = canvas.getContext('2d');
    ctx.fillStyle = background.replace(/rgba\(([^,]+),([^,]+),([^,]+),[^)]+\)/, 'rgb($1,$2,$3)');
    ctx.fillRect(0, 0, 1, 1);
    return {
      classes: root.className, osDark: matchMedia('(prefers-color-scheme: dark)').matches,
      preference: JSON.parse(localStorage.getItem('console-user-settings') || '{}')['console.theme'],
      background, backgroundRGB: [...ctx.getImageData(0, 0, 1, 1).data].slice(0, 3),
      foreground: style.getPropertyValue(pf6 ? '--pf-t--global--text--color--regular' : '--pf-v5-global--Color--100').trim(),
      patternFly: pf6 ? 6 : 5,
    };
  });
  assert.equal(state.preference, theme, 'Console did not persist its authoritative console.theme preference');
  assert.equal(/(?:pf-v[56]-theme-dark|pf-theme-dark)/.test(state.classes), theme === 'dark',
    'Console theme classes disagree with the chosen preference');
  assert.equal(state.osDark, theme !== 'dark', 'The OS must disagree with the explicit Console theme');
  const expectedBackground = theme === 'light' ? [255, 255, 255]
    : state.patternFly === 6 ? [41, 41, 41] : [27, 29, 33];
  assert.deepEqual(state.backgroundRGB, expectedBackground,
    `Unexpected ${theme} PF${state.patternFly} primary background: ${state.background}`);
  return state;
}

async function chooseTheme(context, theme) {
  const page = await context.newPage();
  await page.addLocatorHandler(button(page, 'Skip tour'), async () => button(page, 'Skip tour').click());
  await page.goto(`${BASE_URL}/user-preferences/general`, { waitUntil: 'domcontentloaded' });
  const field = page.locator('[data-test="console.theme field"]');
  await visible(field.or(button(page, 'Skip tour')).first());
  await visible(field);
  await field.getByRole('button').click();
  await page.getByRole('option', { name: theme === 'dark' ? 'Dark' : 'Light', exact: true }).click();
  await page.waitForFunction((selected) =>
    JSON.parse(localStorage.getItem('console-user-settings') || '{}')['console.theme'] === selected, theme);
  await page.reload({ waitUntil: 'domcontentloaded' });
  await visible(field);
  const state = await themeState(page, theme);
  await page.screenshot({ path: path.join(ARTIFACTS, 'console-preference.png'), fullPage: true });
  await page.close();
  return state;
}

async function contrastEvidence(page, scope, label) {
  const count = await scope.count();
  if (count > 1) {
    for (let i = 0; i < count; i++) await contrastEvidence(page, scope.nth(i), `${label} section ${i + 1}`);
    return;
  }
  await page.waitForTimeout(250); // Measure settled PF hover/disabled/tab color transitions.
  const evidence = await scope.evaluate(async (root) => {
    const rgba = (value) => {
      const canvas = document.createElement('canvas');
      const ctx = canvas.getContext('2d');
      ctx.fillStyle = value;
      ctx.fillRect(0, 0, 1, 1);
      return [...ctx.getImageData(0, 0, 1, 1).data].map((v, i) => i === 3 ? v / 255 : v);
    };
    const mix = (front, back) => [...front.slice(0, 3).map((v, i) => v * front[3] + back[i] * (1 - front[3])), 1];
    const html = document.documentElement;
    const htmlStyle = getComputedStyle(html);
    let backdrop;
    // The 4.22 Console uses translucent glass surfaces over a real SVG image.
    // Ignoring that image incorrectly composites dark surfaces over white.
    if (htmlStyle.backgroundImage !== 'none') {
      const url = htmlStyle.backgroundImage.match(/^url\(["']?(.+?)["']?\)$/)?.[1];
      if (!url) throw new Error(`Unsupported Console background: ${htmlStyle.backgroundImage}`);
      const image = new Image();
      image.src = url;
      await image.decode();
      const canvas = document.createElement('canvas');
      canvas.width = html.clientWidth;
      canvas.height = htmlStyle.backgroundAttachment === 'fixed' ? innerHeight : html.scrollHeight;
      const ctx = canvas.getContext('2d');
      let width = image.naturalWidth, height = image.naturalHeight;
      if (['cover', 'contain'].includes(htmlStyle.backgroundSize)) {
        const scale = Math[htmlStyle.backgroundSize === 'cover' ? 'max' : 'min'](canvas.width / width, canvas.height / height);
        width *= scale;
        height *= scale;
      } else if (htmlStyle.backgroundSize !== 'auto') {
        throw new Error(`Unsupported Console background size: ${htmlStyle.backgroundSize}`);
      }
      const positions = htmlStyle.backgroundPosition.split(' ');
      if (!positions.every((v) => v.endsWith('%') || v === '0px')) {
        throw new Error(`Unsupported Console background position: ${htmlStyle.backgroundPosition}`);
      }
      const x = (canvas.width - width) * parseFloat(positions[0]) / 100;
      const y = (canvas.height - height) * parseFloat(positions[1] || positions[0]) / 100;
      ctx.drawImage(image, x, y, width, height);
      backdrop = (element) => {
        const rect = element.getBoundingClientRect();
        const px = Math.max(0, Math.min(canvas.width - 1, Math.floor(rect.x + rect.width / 2)));
        const py = Math.max(0, Math.min(canvas.height - 1, Math.floor(rect.y + rect.height / 2
          + (htmlStyle.backgroundAttachment === 'fixed' ? 0 : scrollY))));
        const pixel = [...ctx.getImageData(px, py, 1, 1).data].map((v, i) => i === 3 ? v / 255 : v);
        return mix(pixel, [255, 255, 255, 1]);
      };
    }
    const background = (element) => {
      const chain = [];
      for (let el = element; el; el = el.parentElement) chain.unshift(el);
      return chain.reduce((bg, el) => {
        let result = mix(rgba(getComputedStyle(el).backgroundColor), bg);
        // PF6 labels paint their colored pill on ::before, behind their text.
        if (el.matches('.pf-v6-c-label__content')) {
          result = mix(rgba(getComputedStyle(el, '::before').backgroundColor), result);
        }
        return result;
      }, backdrop ? backdrop(element) : [255, 255, 255, 1]);
    };
    const luminance = (rgb) => rgb.slice(0, 3).map((v) => {
      const c = v / 255;
      return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
    }).reduce((sum, v, i) => sum + v * [0.2126, 0.7152, 0.0722][i], 0);
    const ratio = (a, b) => {
      const first = luminance(a), second = luminance(b);
      return (Math.max(first, second) + 0.05) / (Math.min(first, second) + 0.05);
    };
    const samples = [];
    for (const el of root.querySelectorAll('*')) {
      if (!(el instanceof HTMLElement) || !el.checkVisibility() || el.closest('[disabled], [aria-disabled="true"]')) continue;
      const directText = [...el.childNodes].filter((n) => n.nodeType === Node.TEXT_NODE).map((n) => n.textContent).join('').trim();
      if (!directText || ['OPTION', 'SCRIPT', 'STYLE'].includes(el.tagName)) continue;
      const style = getComputedStyle(el), bg = background(el);
      const large = parseFloat(style.fontSize) >= 24 || (parseFloat(style.fontSize) >= 18.66 && parseFloat(style.fontWeight) >= 700);
      samples.push({
        text: directText.slice(0, 100), tag: el.tagName, className: el.className,
        foreground: style.color, background: bg.slice(0, 3), border: style.borderColor,
        ratio: ratio(mix(rgba(style.color), bg), bg), minimum: large ? 3 : 4.5,
      });
    }
    const selects = [...root.querySelectorAll('select')].map((el) => {
      const style = getComputedStyle(el);
      return { className: el.className, color: style.color, background: style.backgroundColor, border: style.borderColor,
        ratio: ratio(mix(rgba(style.color), background(el)), background(el)),
        backgroundMatchesToken: rgba(style.backgroundColor).every((v, i) =>
          v === rgba(style.getPropertyValue('--pf-v6-global--BackgroundColor--100'))[i]),
        foregroundMatchesToken: rgba(style.color).every((v, i) =>
          v === rgba(style.getPropertyValue('--pf-v6-global--Color--100'))[i]),
        borderMatchesToken: rgba(style.borderTopColor).every((v, i) =>
          v === rgba(style.getPropertyValue('--pf-v6-global--BorderColor--100'))[i]),
        colorScheme: style.colorScheme, options: [...el.options].map((option) => {
          const s = getComputedStyle(option);
          return { text: option.text, color: s.color, background: s.backgroundColor };
        }) };
    });
    const style = getComputedStyle(root);
    const token = (name) => style.getPropertyValue(name).trim();
    const tokens = {
      background: token('--pf-v6-global--BackgroundColor--100'),
      foreground: token('--pf-v6-global--Color--100'),
      border: token('--pf-v6-global--BorderColor--100'),
    };
    const panes = [...root.querySelectorAll('.mirror-dual-pane')].map((el) => {
      const s = getComputedStyle(el);
      return { background: s.backgroundColor, border: s.borderTopColor,
        backgroundMatchesToken: rgba(s.backgroundColor).every((v, i) => v === rgba(tokens.background)[i]),
        borderMatchesToken: rgba(s.borderTopColor).every((v, i) => v === rgba(tokens.border)[i]),
        decorativeBorderContrast: ratio(rgba(s.borderTopColor), background(el)) };
    });
    const selectedRows = [...root.querySelectorAll('.mirror-dual-row--selected')].map((el) => {
      const shadow = getComputedStyle(el).boxShadow;
      const accent = shadow.match(/rgba?\([^)]+\)/)?.[0];
      return { shadow, accentContrast: accent ? ratio(rgba(accent), background(el)) : 0 };
    });
    return { samples, selects, panes, selectedRows, tokens, background: style.backgroundColor, consoleBackdrop: htmlStyle.backgroundImage };
  });
  themeEvidence.push({ path: new URL(page.url()).pathname, label, ...evidence });
  assert(evidence.samples.length, `${label}: no rendered text measured`);
  const failures = evidence.samples.filter((s) => s.ratio + 0.01 < s.minimum);
  assert.equal(failures.length, 0, `${label}: insufficient text contrast: ${JSON.stringify(failures)}`);
  for (const select of evidence.selects) {
    assert(select.ratio >= 4.5, 'Native select text must have sufficient contrast');
    if (select.className.includes('mirror-native-select')) {
      assert.equal(select.colorScheme, path.basename(ARTIFACTS), 'Native compact select must use the chosen Console color scheme');
      assert(select.backgroundMatchesToken && select.foregroundMatchesToken && select.borderMatchesToken,
        'Native compact select must resolve the real theme background/text/border tokens');
      for (const option of select.options) {
        assert.equal(option.color, select.color, 'Native option text must match its themed select');
        assert.equal(option.background, select.background, 'Native option background must match its themed select');
      }
    }
  }
  for (const pane of evidence.panes) {
    assert(pane.backgroundMatchesToken, 'Catalog/release pane background must resolve to the real theme token');
    assert(pane.borderMatchesToken, 'Catalog/release pane border must resolve to the real theme token');
  }
  for (const row of evidence.selectedRows) {
    assert(row.accentContrast >= 3, 'The meaningful selected-row control indicator needs at least 3:1 contrast');
  }
}

async function toolbarSpacing(page) {
  const section = page.locator('.mirror-filter-toolbar > [class*="toolbar__content-section"]');
  await visible(section);
  const spacing = await section.evaluate((element) => {
    const styles = getComputedStyle(element);
    return { column: parseFloat(styles.columnGap), row: parseFloat(styles.rowGap), wrap: styles.flexWrap };
  });
  assert(spacing.column >= 16, 'Filter toolbar needs at least 16px horizontal spacing');
  assert(spacing.row >= 8, 'Wrapped filter toolbar needs at least 8px vertical spacing');
  assert.equal(spacing.wrap, 'wrap', 'Filter toolbar must allow responsive wrapping');
  const items = section.locator(':scope > [class*="toolbar__item"]');
  const first = await items.nth(0).boundingBox();
  const next = await items.nth(1).boundingBox();
  assert(first && next, 'Filter toolbar controls must be visible');
  if (next.y < first.y + first.height && first.y < next.y + next.height) {
    assert(next.x - first.x - first.width >= 15.5, 'Adjacent filter toolbar controls are too close');
  } else {
    assert(next.y - first.y - first.height >= 7.5, 'Wrapped filter toolbar controls are too close');
  }
}

async function tabs(page, names) {
  for (const [name, content] of names) {
    const tab = page.getByRole('tab', { name });
    await tab.click();
    assert.equal(await tab.getAttribute('aria-selected'), 'true', `Tab ${name} did not activate`);
    await text(page, content);
    await contrastEvidence(page, page.locator('.mirror-ui'), `tab ${content}`);
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
  await contrastEvidence(page, modal, `dialog ${title}`);
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
  { path: '/oc-mirror/targets', title: 'Mirror Targets', body: 'demo-target', toolbar: true },
  { path: '/oc-mirror/imagesets', title: 'ImageSets', body: 'demo-imageset', toolbar: true },
  { path: '/oc-mirror/failed', title: 'Pending & Failed Images', body: 'Smoke fixture registry unavailable', toolbar: true },
  { path: `${PREFIX}/failures`, title: 'Pending & Failed Images — demo-target', body: 'Smoke fixture registry unavailable', toolbar: true },
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
      const rows = page.locator('.mirror-dual-row');
      await rows.first().hover();
      await contrastEvidence(page, page.locator('.mirror-ui'), 'catalog row hover');
      await rows.first().click();
      await rows.last().click();
      await contrastEvidence(page, page.locator('.mirror-ui'), 'catalog rows selected');
      await button(page, 'Expand').last().click();
      await button(page, 'Expand').first().click();
      await text(page, 'stable');
      await contrastEvidence(page, page.locator('.mirror-ui'), 'catalog expanded channels and native version selects');
      const remove = page.getByTitle('«Remove all', { exact: true });
      await remove.hover();
      await page.waitForTimeout(150); // Allow the action's short CSS color transition.
      await contrastEvidence(page, page.locator('.mirror-ui'), 'catalog action hover');
      await page.screenshot({ path: path.join(ARTIFACTS, 'catalog-selected-expanded-hover.png'), fullPage: true });
      await button(page, 'Collapse').first().click();
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

async function checkPage(context, spec, theme) {
  const page = await context.newPage();
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
    await visible(heading.or(skipTour).first());
    if (await skipTour.isVisible()) await skipTour.click();
    await visible(heading);
    await text(page, spec.body);
    await themeState(page, theme);
    await contrastEvidence(page, page.locator('.mirror-ui'), 'page');
    if (spec.toolbar) {
      await toolbarSpacing(page);
      const refresh = button(page, 'Refresh');
      await refresh.hover();
      await contrastEvidence(page, page.locator('.mirror-ui'), 'refresh hover');
      await refresh.click();
      await text(page, spec.body);
      await page.waitForFunction((element) => !element.disabled, await refresh.elementHandle());
      await page.setViewportSize({ width: 760, height: 900 });
      await toolbarSpacing(page);
      await contrastEvidence(page, page.locator('.mirror-ui'), 'narrow filter toolbar');
      await page.screenshot({ path: path.join(ARTIFACTS, `narrow-${spec.path.replace(/\W+/g, '_')}.png`), fullPage: true });
      await page.setViewportSize({ width: 1280, height: 720 });
    }
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
  await mkdir(ARTIFACT_ROOT, { recursive: true });
  const browser = await chromium.launch();
  const results = [];
  try {
    for (const theme of ['light', 'dark']) {
      ARTIFACTS = path.join(ARTIFACT_ROOT, theme);
      themeEvidence = [];
      await mkdir(ARTIFACTS, { recursive: true });
      reseed();
      const context = await browser.newContext({ colorScheme: theme === 'dark' ? 'light' : 'dark' });
      let preference;
      try {
        preference = await chooseTheme(context, theme);
        for (const spec of PAGES) results.push({ theme, ...await checkPage(context, spec, theme) });
      } catch (err) {
        results.push({ theme, title: 'Console preference', fatal: [err.stack || err.message], noise: [] });
      } finally {
        await writeFile(path.join(ARTIFACTS, 'theme-evidence.json'), JSON.stringify({ preference, measurements: themeEvidence,
          nativePopupLimitation: 'Computed select/option colors and color-scheme are checked; OS-rendered popup pixels are not proven by headless Chromium.' }, null, 2));
        await context.close();
      }
    }
  } finally {
    await browser.close();
  }
  await writeFile(path.join(ARTIFACT_ROOT, 'results.json'), JSON.stringify({
    consoleVersion: process.env.CONSOLE_VERSION || 'unspecified', baseURL: BASE_URL, results,
  }, null, 2));
  for (const result of results) {
    console.log(`${result.fatal.length ? 'FAIL' : 'OK'}: ${result.theme} ${result.title} (${result.noise.length} console-shell errors ignored)`);
    for (const err of result.fatal) console.error(err);
  }
  assert(!results.some((r) => r.fatal.length), 'Plugin smoke test failed; see artifacts/results.json and screenshots');
  console.log('Plugin smoke passed: light and dark against opposite OS themes, all eight routes, tabs, contrast, dialogs and actions.');
}

main().catch((err) => {
  console.error(err);
  process.exitCode = 1;
});

// First-visit navigation performance. Every page is a lazy-loaded chunk; if
// the chunks are not prefetched, the first click on each nav tab waits on the
// network before the page appears, which on a slow link reads as the UI
// hanging for seconds. Runs over an emulated high-latency link so that any
// network round trip on the navigation path blows the time budget, while the
// budget stays loose enough for a busy CI runner.
const { test, expect } = require('@playwright/test');
const { attachCapture, fatalOf, gotoSeeded } = require('./helpers');

const LATENCY_MS = 1000;

// Nav tabs present with auth disabled and an empty store.
const tabs = [
  '/configs/',
  '/disks/',
  '/hosts',
  '/users',
  '/log',
  '/scorch',
  '/settings',
  '/experiments',
];

// Resolve once no new resource has loaded for `quietMs`, i.e. once the idle
// prefetch has finished.
async function waitForResourcesQuiet(page, quietMs = 1500, timeoutMs = 60000) {
  const deadline = Date.now() + timeoutMs;
  let last = -1;
  let quietSince = Date.now();
  while (Date.now() < deadline) {
    const count = await page.evaluate(
      () => performance.getEntriesByType('resource').length,
    );
    if (count !== last) {
      last = count;
      quietSince = Date.now();
    } else if (Date.now() - quietSince >= quietMs) {
      return;
    }
    await page.waitForTimeout(250);
  }
  throw new Error('resources never went quiet');
}

test.describe('first-visit navigation', () => {
  test.skip(
    ({ browserName }) => browserName !== 'chromium',
    'network emulation uses the Chrome DevTools Protocol',
  );

  test('nav tabs open without fetching code on click', async ({ page }) => {
    const issues = [];
    attachCapture(page, issues);

    const cdp = await page.context().newCDPSession(page);
    await cdp.send('Network.enable');
    await cdp.send('Network.emulateNetworkConditions', {
      offline: false,
      latency: LATENCY_MS,
      downloadThroughput: (4 * 1024 * 1024) / 8,
      uploadThroughput: (4 * 1024 * 1024) / 8,
    });

    await gotoSeeded(page, '/experiments');
    await page.waitForLoadState('load');
    await waitForResourcesQuiet(page);

    let assetRequests = [];
    page.on('request', (req) => {
      if (new URL(req.url()).pathname.includes('/assets/')) {
        assetRequests.push(req.url());
      }
    });

    const slow = [];
    for (const path of tabs) {
      assetRequests = [];
      const start = Date.now();
      await page.locator(`a.navbar-item[href$="${path}"]`).first().click();
      await page.waitForURL((url) => url.pathname === path);
      const elapsed = Date.now() - start;

      expect(
        assetRequests,
        `opening ${path} should not wait on fetching code`,
      ).toEqual([]);
      if (elapsed >= LATENCY_MS) {
        slow.push(`${path}: ${elapsed} ms`);
      }
    }

    expect(slow, `tabs slower than ${LATENCY_MS} ms`).toEqual([]);
    const fatal = fatalOf(issues);
    expect(fatal, JSON.stringify(fatal, null, 2)).toHaveLength(0);
  });
});

test('static assets are compressed and cached', async ({ page, request }) => {
  await page.goto('/');
  const script = await page
    .locator('script[type="module"][src*="/assets/"]')
    .first()
    .getAttribute('src');

  const resp = await request.get(script, {
    headers: { 'Accept-Encoding': 'gzip' },
  });
  expect(resp.ok()).toBe(true);
  const headers = resp.headers();
  expect(headers['content-encoding']).toBe('gzip');
  expect(headers['cache-control']).toContain('immutable');
});

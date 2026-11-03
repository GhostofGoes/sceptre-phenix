// Retakes the docs images that need a running experiment: the SCORCH runs
// page and the State of Health graph of the scorch-demo experiment that
// live/run-experiment.sh starts (see README.md). It runs against the real
// server, with no stand-in data, and only when DOCS_LIVE_EXPERIMENT names
// the experiment, so `npm run docs-screenshots` without it skips this file.
const { test, expect } = require('@playwright/test');
const { pageDataLoaded } = require('../tests/helpers');
const { shoot, shootRegion } = require('./helpers');

const exp = process.env.DOCS_LIVE_EXPERIMENT;

test.skip(!exp, 'DOCS_LIVE_EXPERIMENT names the running experiment to capture');
// The graph draws, fits and hovers without d3 transitions under reduced motion.
test.use({ reducedMotion: 'reduce' });

// A SCORCH run can wait minutes for its pause components and the break.
const RUN_TIMEOUT = 10 * 60 * 1000;

const api = (path) => `/api/v1/experiments/${exp}${path}`;

async function startRun(request, run) {
  const response = await request.post(api(`/scorch/pipelines/${run}`));
  expect(response.status(), `starting run ${run}`).toBe(204);
}

// The card of one run on the runs page: its header and pipeline.
const runCard = (page, name) =>
  page
    .locator('.scorch-run')
    .filter({ has: page.locator('h2.run-title', { hasText: `Run: ${name}` }) });

// A pipeline node, whose accessible name carries its status.
const node = (scope, label, status) =>
  scope.getByRole('button', { name: `${label}, status ${status}` });

// Both run cards and the legend fit in a tall window, so no capture needs
// to scroll.
async function runsPage(page) {
  await page.setViewportSize({ width: 1600, height: 2200 });
  await page.goto(`/scorch/${exp}`);
  await pageDataLoaded(page);
  await expect(runCard(page, 'baseline')).toBeVisible();
}

// The app reports a run as running until it has stored the run's end, a
// moment after the pipeline's done node turns.
async function scorchIdle(request) {
  await expect
    .poll(
      async () => {
        const response = await request.get(api('/scorch/pipelines'));
        const body = await response.json();
        return body.running === -1 && !body.app_running;
      },
      { timeout: 60000, intervals: [1000] },
    )
    .toBe(true);
}

test.describe('scorch', () => {
  // The runs change the experiment's state, so these build on each other.
  test.describe.configure({ mode: 'serial' });

  test('legend', async ({ page }) => {
    await runsPage(page);
    const rows = page.locator('.columns.is-vcentered');
    await shootRegion(
      page,
      [
        rows.filter({ hasText: 'Uninitialized' }),
        rows.filter({ hasText: 'Break Point' }),
      ],
      'legend',
      12,
    );
  });

  // The baseline run before it ever ran: every node uninitialized.
  test('pipeline', async ({ page }) => {
    await runsPage(page);
    const card = runCard(page, 'baseline');
    await expect(node(card, 'done', 'unknown')).toBeVisible();
    await shoot(card, 'pipeline');
  });

  // One run of baseline gives the stages, both terminals, an output window and
  // the loop view: the run waits at its break point until the terminal exits.
  test('stages, read-write, read-only, output and loop', async ({
    page,
    browser,
    request,
  }) => {
    test.setTimeout(3 * RUN_TIMEOUT);
    await runsPage(page);
    const card = runCard(page, 'baseline');

    await startRun(request, 0);
    await expect(node(card, 'break', 'unstable')).toBeVisible({
      timeout: RUN_TIMEOUT,
    });
    await expect(node(card, 'stop', 'running')).toBeVisible();
    await shoot(card, 'stages');

    // The first client to open the break's terminal may write in it.
    await node(card, 'break', 'unstable').click();
    const terminal = page
      .locator('.modal-card')
      .filter({ hasText: `Terminal (${exp})` });
    await expect(terminal.locator('h2.modal-card-title')).toHaveText(
      `Terminal (${exp})`,
    );
    await terminal.locator('.xterm').click();
    await page.waitForTimeout(2000);
    // The shell starts in a temporary directory with the runner's host name in
    // its prompt and no TERM; the image should show neither.
    await page.keyboard.type(
      "export TERM=xterm PS1='root@phenix:\\w# '; cd /phenix; clear\n",
    );
    await page.waitForTimeout(1000);
    await page.keyboard.type('ls\n');
    await page.waitForTimeout(1000);
    await page.keyboard.type('pwd\n');
    await page.waitForTimeout(2000);
    await shoot(terminal, 'read-write');

    // Every later client, here a second browser context, only watches.
    const { baseURL, viewport, deviceScaleFactor } = test.info().project.use;
    const watcher = await browser.newContext({
      baseURL,
      viewport,
      deviceScaleFactor,
      reducedMotion: 'reduce',
    });
    const watcherPage = await watcher.newPage();
    await runsPage(watcherPage);
    await node(runCard(watcherPage, 'baseline'), 'break', 'unstable').click();
    const readOnly = watcherPage
      .locator('.modal-card')
      .filter({ hasText: '(read-only)' });
    await expect(readOnly.locator('h2.modal-card-title')).toHaveText(
      `Terminal (${exp}) (read-only)`,
    );
    await watcherPage.waitForTimeout(2000);
    await shoot(readOnly, 'read-only');
    await watcher.close();

    // Exiting the terminal lets the run finish its cleanup.
    await terminal.getByRole('button', { name: 'Exit' }).click();
    await expect(node(card, 'done', 'success')).toBeVisible({
      timeout: RUN_TIMEOUT,
    });

    // A finished component's output.
    await node(card, 'snort', 'success').click();
    const output = page
      .locator('.modal-card')
      .filter({ has: page.locator('.output-head') });
    await expect(output.locator('textarea')).not.toHaveValue('', {
      timeout: 30000,
    });
    await shoot(output, 'output');
    await output.getByRole('button', { name: 'Exit' }).click();
    await expect(output).toBeHidden();

    // The loop the run executed, one level down.
    await node(card, 'loop', 'success').click();
    await expect(card.locator('h2.run-title')).toHaveText(/\(loop 1\)/);
    await expect(node(card, 'done', 'success')).toBeVisible();
    await shoot(card, 'loop');
    await card
      .getByRole('button', { name: 'Return to the previous loop' })
      .click();
    await expect(card.locator('h2.run-title')).toHaveText('Run: baseline');
  });

  // The failing run: trafficgen fails in configure, so the run jumps to cleanup.
  test('failed', async ({ page, request }) => {
    test.setTimeout(2 * RUN_TIMEOUT);
    await runsPage(page);
    const card = runCard(page, 'failing');
    await scorchIdle(request);
    await startRun(request, 1);
    await expect(node(card, 'done', 'failure')).toBeVisible({
      timeout: RUN_TIMEOUT,
    });
    await shootRegion(
      page,
      [
        node(card, 'configure', 'failure'),
        node(card, 'trafficgen', 'failure'),
        node(card, 'snort', 'unknown'),
        node(card, 'start', 'unknown'),
      ],
      'failed',
      20,
    );
  });
});

test.describe('state of health', () => {
  test.describe.configure({ timeout: RUN_TIMEOUT + 60000 });

  // The run-experiment script started the experiment through the soh app's
  // post-start checks, so the results are there; the Layered layout draws the
  // same graph every time.
  async function sohPage(page, request) {
    await expect
      .poll(
        async () => {
          const response = await request.get(api('/soh'));
          const body = await response.json();
          return body.soh_initialized && !body.soh_running;
        },
        { timeout: RUN_TIMEOUT, intervals: [5000] },
      )
      .toBe(true);
    await page.addInitScript(() =>
      localStorage.setItem('phenix.soh.layout', 'layered'),
    );
    await page.goto(`/soh/${exp}`);
    await pageDataLoaded(page);
    await expect(
      page.getByRole('button', { name: 'server, status running' }),
    ).toBeVisible();
    await page.waitForTimeout(1500);
  }

  const vm = (page, name, status) =>
    page.getByRole('button', { name: `${name}, status ${status}` });

  const details = (page, name) =>
    page.locator('.modal-card').filter({ hasText: `${name} Details` });

  // The server failed a check, so it is ringed orange; hovering shows its OS.
  test('linux', async ({ page, request }) => {
    await sohPage(page, request);
    const server = vm(page, 'server', 'running');
    await server.hover();
    await page.waitForTimeout(1000);
    const box = await server.boundingBox();
    const graph = await page.locator('.soh-graph-view').boundingBox();
    const pad = 200;
    const left = Math.max(graph.x, box.x - pad);
    const top = Math.max(graph.y, box.y - pad);
    const right = Math.min(graph.x + graph.width, box.x + box.width + pad);
    const bottom = Math.min(graph.y + graph.height, box.y + box.height + pad);
    await page.screenshot({
      path: require('./helpers').image('linux'),
      clip: { x: left, y: top, width: right - left, height: bottom - top },
      animations: 'disabled',
      caret: 'hide',
    });
  });

  test('soh_log_details', async ({ page, request }) => {
    await sohPage(page, request);
    await vm(page, 'server', 'running').click();
    const dialog = details(page, 'server');
    await expect(dialog.getByText('Listeners', { exact: true })).toBeVisible();
    await expect(dialog.getByText('not listening on port')).toBeVisible();
    await shoot(dialog, 'soh_log_details');
  });

  test('soh_details', async ({ page, request }) => {
    await sohPage(page, request);
    await vm(page, 'client2', 'running').click();
    const dialog = details(page, 'client2');
    await expect(dialog.getByText(/CPU Load/)).toBeVisible();
    await shoot(dialog, 'soh_details');
  });

  test('soh_no_details', async ({ page, request }) => {
    await sohPage(page, request);
    await vm(page, 'spare', 'not booted').click();
    const dialog = details(page, 'spare');
    await expect(
      dialog.getByText('There is no state of health data available'),
    ).toBeVisible();
    await shoot(dialog, 'soh_no_details');
  });
});

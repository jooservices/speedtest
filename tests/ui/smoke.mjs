// Run on the host. The fake server and browser use only a disposable loopback port.
import { chromium } from 'playwright';
import assert from 'node:assert/strict';
import { mkdir } from 'node:fs/promises';
import { startServer } from '../ui-fixtures/server.mjs';
import { trackCoverage } from '../lib/v8cov.mjs';

const server = await startServer();
let browser;
try {
  browser = process.env.PW_TEST_CONNECT_WS_ENDPOINT ?
    await chromium.connect(process.env.PW_TEST_CONNECT_WS_ENDPOINT) :
    await chromium.launch({ headless: true, ...(process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE ?
      { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE } : {}) });
} catch (error) { await server.close(); throw error; }
const routes = ['/', '/nodes', '/nodes/node-contract', '/history', '/stats', '/network', '/isps', '/schedules', '/alerts', '/settings'];
const diagnostics = [];
const page = await browser.newPage();
const coverage = await trackCoverage(page);
page.on('pageerror', error => diagnostics.push(error.message));
page.on('console', message => { if (['error', 'warning'].includes(message.type())) diagnostics.push(message.text()); });
const goto = async (route, demo = false) => {
  await page.goto(`${server.origin}/${demo ? '?demo=1' : ''}#${route}`);
  if (!demo) await page.waitForFunction(() => ST.remote.status === 'ready');
  await page.locator('#view h1').waitFor();
  assert.equal(await page.getByText('Something went wrong', { exact: true }).count(), 0);
};
try {
  await mkdir('.tmp/ui-smoke', { recursive: true });
  for (let run = 1; run <= 3; run++) {
    for (const demo of [true, false]) {
      for (const route of routes) {
        const path = demo && route === '/nodes/node-contract' ? '/nodes/n1' : route;
        await goto(path, demo);
        const text = await page.locator('#view').innerText();
        assert.ok(text.trim().length > 20, `empty ${path}`);
      }
      for (const width of [390, 768, 1440]) {
        await page.setViewportSize({ width, height: 900 });
        for (const theme of ['dark', 'light']) {
          for (const route of routes) {
            await goto(demo && route === '/nodes/node-contract' ? '/nodes/n1' : route, demo);
            await page.evaluate(theme => { ST.state.settings.theme = theme; ST.emit('settings'); }, theme);
            assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false,
              `overflow ${route} ${width} ${theme} demo=${demo}`);
            assert.equal(await page.locator('.menu-btn').isVisible(), width <= 900);
            if (route === '/') {
              assert.ok(await page.locator('.chart .axis text').evaluateAll(labels => labels.filter(label => label.textContent === 'Mbps (G = Gbps)').every(label => label.getBBox().x >= 0)));
            }
            if (run === 1) await page.screenshot({ animations: 'disabled', path: `.tmp/ui-smoke/${demo ? 'demo' : 'api'}-${route.replaceAll('/', '_') || 'dashboard'}-${width}-${theme}.png` });
          }
        }
      }
    }
    console.log(`PASS smoke run ${run}: all 10 routes, demo/API, dark/light, 390/768/1440px`);
  }
  await goto('/isps', true);
  await page.getByRole('button', { name: /^Show trend for .* \(AS\d+\)$/ }).first().click();
  await page.locator('#view .grid-2 svg[role="img"]').waitFor();

  await goto('/isps', false);
  const ispSelect = () => page.getByRole('button', { name: 'Show trend for Fixture ISP (AS64500)' });
  await ispSelect().waitFor();
  server.control.empty = true;
  await ispSelect().click();
  await page.getByText('No samples').waitFor();
  server.control.empty = false;
  await ispSelect().click();
  await page.locator('#view .grid-2 svg[aria-label="Fixture ISP download trend"]').waitFor();
  const trendPath = `/api/isps/${server.data.isp.id}/trend`;
  server.control.failPaths.add(trendPath);
  await ispSelect().click();
  await page.getByText('Could not load trend').waitFor();
  server.control.failPaths.delete(trendPath);
  await ispSelect().click();
  await page.locator('#view .grid-2 svg[aria-label="Fixture ISP download trend"]').waitFor();

  server.control.failPaths.add('/api/isps');
  await page.getByRole('link', { name: 'Nodes', exact: true }).click();
  await page.waitForURL('**/#/nodes');
  await page.getByRole('link', { name: 'ISPs', exact: true }).click();
  await page.waitForURL('**/#/isps');
  await page.waitForFunction(() => ST.remote.status === 'ready');
  await page.getByText('Could not load ISPs').waitFor();
  server.control.failPaths.delete('/api/isps');
  await page.getByRole('button', { name: 'Retry', exact: true }).click();
  await ispSelect().waitFor();

  for (const route of routes) {
    // Delay each bootstrap so loading is observable on every route.
    server.control.delay = 50;
    await page.goto(`${server.origin}/?loading=${encodeURIComponent(route)}#${route}`);
    await page.locator('[data-load-state="loading"]').waitFor();
    await page.waitForFunction(() => ST.remote.status === 'ready');
    server.control.delay = 0;
    server.control.status = 500;
    await page.reload();
    await page.getByText('Could not load data', { exact: true }).waitFor();
    server.control.status = 0;
    const retry = page.getByRole('button', { name: 'Retry', exact: true });
    await retry.focus();
    assert.equal(await retry.evaluate(el => getComputedStyle(el).outlineStyle), 'solid');
    await page.keyboard.press('Enter');
    await page.waitForFunction(() => ST.remote.status === 'ready');
    await page.locator('#view h1').waitFor();
    server.control.empty = true;
    await page.reload();
    await page.waitForFunction(() => ST.remote.status === 'ready');
    assert.equal(await page.getByText('Something went wrong', { exact: true }).count(), 0);
    server.control.empty = false;
  }
  // HTTP errors deliberately emit browser resource errors; those are expected only in the error scenarios above.
  const unexpected = diagnostics.filter(message => !message.includes('Failed to load resource: the server responded with a status of 500'));
  assert.deepEqual(unexpected, []);
  server.control.status = 401;
  await page.goto(`${server.origin}/#/history`);
  await page.waitForURL('**/#/signin');
  await page.getByRole('heading', { name: 'Sign in', exact: true }).waitFor();
  await coverage.finish('smoke');
  console.log('PASS loading/error/retry/empty on all routes; 401 redirects to sign-in; no unexpected console warnings/errors');
} finally {
  await browser.close(); await server.close();
}

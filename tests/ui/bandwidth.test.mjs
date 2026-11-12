import { test } from 'node:test';
import assert from 'node:assert/strict';
import { chromium } from 'playwright';
import { startServer } from '../ui-fixtures/server.mjs';

test('incomplete bandwidth advice explains missing measurements when a budget exists', async t => {
  const server = await startServer();
  t.after(server.close);
  server.control.noMeasurements = true;
  const browser = await chromium.launch({ headless: true, ...(process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE ?
    { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE } : {}) });
  t.after(() => browser.close());
  const page = await browser.newPage();
  await page.goto(`${server.origin}/#/nodes/node-contract`);
  await page.waitForFunction(() => ST.remote.status === 'ready');
  await page.getByRole('tab', { name: 'Settings' }).click();
  await page.getByText('A budget is set, but there are no complete byte measurements yet. Run tests that report both download and upload byte counters.').first().waitFor();
  assert.equal(await page.getByText('Enter a monthly budget to get an interval recommendation.').count(), 0);
  assert.ok((await page.locator('#view').innerText()).includes('Unavailable — no complete byte measurements.'));
});

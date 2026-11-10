export function statisticsSuite(T) {
  const { page, stack, assert } = T;
  return {
    async unavailableTrendExplainsRetry() {
      const refuseTrend = route => route.fulfill({ status: 500, contentType: 'application/problem+json', body: JSON.stringify({
        type: '/problems/internal', title: 'Internal Server Error', status: 500, detail: 'Temporary statistics failure.'
      }) });
      await page.route('**/api/stats/trend**', refuseTrend);
      try {
        await page.goto(T.url('#/stats'));
        await page.getByRole('heading', { name: 'Statistics are unavailable' }).waitFor();
        T.expectRefusal(500);
      } finally {
        await page.unroute('**/api/stats/trend**', refuseTrend);
      }
      await page.reload();
      await page.getByRole('heading', { name: 'Statistics' }).waitFor();
      await T.settled();
      await T.expectNoLoadError();
    },

    // The figures on the page are the center's, not a browser calculation.
    async summaryMatchesApi() {
      await T.open('#/stats', 'Statistics');
      await page.getByRole('heading', { name: 'Summary by node' }).waitFor();
      await page.getByRole('columnheader', { name: 'Average' }).waitFor();
      const summary = (await T.api('/api/stats/summary?range=7d')).body;
      const vm1 = summary.data.find(x => x.nodeId === '10000000-0000-4000-8000-000000000001').current.down.mean;
      assert.ok((await T.row('VM1').innerText()).includes(vm1.toFixed(1)), 'the summary shows the center average');
      const before = await page.locator('table').first().innerText();
      await T.tab('Upload');
      await page.getByRole('heading', { name: 'Summary by node' }).waitFor();
      assert.notEqual(await page.locator('table').first().innerText(), before, 'switching to upload changes the figures');
      for (const section of ['Trend', 'Time of day', 'Week at a glance', 'Distribution']) await page.getByRole('heading', { name: section }).waitFor();
      await T.expectNoLoadError();
    },

    // Every metric, node chips, combined total, chart/table, heat-map node and the range.
    async everyControl() {
      await T.open('#/stats', 'Statistics');
      for (const metric of ['Latency', 'Jitter', 'Packet loss', 'Upload', 'Download']) {
        await T.tab(metric);
        await page.getByRole('heading', { name: 'Summary by node' }).waitFor();
        await T.expectNoLoadError();
      }
      const chip = page.locator('.legend button', { hasText: 'VM2' });
      await chip.click();
      assert.equal(await chip.getAttribute('aria-pressed'), null);
      await chip.click();
      assert.notEqual(await chip.getAttribute('aria-pressed'), null);
      await page.getByLabel('Add combined total').check();
      await T.see('Combined total');
      await page.getByLabel('Add combined total').uncheck();
      await T.tab('Table');
      await page.locator('.card').filter({ hasText: 'Trend' }).locator('table').waitFor();
      await T.tab('Chart');
      await T.choose(page.locator('.card').filter({ hasText: 'Week at a glance' }).locator('select'), '10000000-0000-4000-8000-000000000002');
      await page.getByRole('img', { name: 'Heatmap for VM2' }).waitFor();
      await T.choose(page.locator('#topbar select').first(), '24h');
      await T.see('last 24 hours', page.locator('#view'));
      await T.choose(page.locator('#topbar select').first(), '7d');
      await T.expectNoLoadError();
    },
  };
}

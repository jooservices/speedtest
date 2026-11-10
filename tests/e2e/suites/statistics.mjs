export function statisticsSuite(T) {
  const { page, stack, assert } = T;
  return {
    // The figures on the page are the center's, not a browser calculation.
    async summaryMatchesApi() {
      await T.open('#/stats', 'Statistics');
      await page.getByRole('heading', { name: 'Summary by node' }).waitFor();
      await page.getByRole('columnheader', { name: 'Average' }).waitFor();
      const summary = (await T.api('/api/stats/summary?range=7d')).body;
      const vm1 = summary.data.find(x => x.nodeId === '10000000-0000-4000-8000-000000000001').current.down.mean;
      const summaryCard = page.locator('.card').filter({ hasText: 'Summary by node' });
      assert.ok((await summaryCard.getByRole('row').filter({ hasText: 'VM1' }).innerText()).includes(vm1.toFixed(1)), 'the summary shows the center average');
      const before = await summaryCard.locator('table').innerText();
      await T.tab('Upload');
      await page.getByRole('heading', { name: 'Summary by node' }).waitFor();
      assert.notEqual(await page.locator('.card').filter({ hasText: 'Summary by node' }).locator('table').innerText(), before, 'switching to upload changes the figures');
      for (const section of ['Trend', 'Time of day', 'Week at a glance', 'Distribution']) await page.getByRole('heading', { name: section }).waitFor();
      await page.getByRole('heading', { name: 'Bandwidth' }).waitFor();
      await page.getByRole('img', { name: 'Daily bandwidth usage by node and engine' }).waitFor();
      await page.getByRole('columnheader', { name: 'Month to date' }).waitFor();
      const bandwidth = page.locator('.card').filter({ hasText: 'Measured download and upload bytes remain separate by node and engine.' });
      assert.ok(await bandwidth.locator('table').last().locator('tbody tr').count() > 0, 'Statistics shows complete daily bandwidth buckets');
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

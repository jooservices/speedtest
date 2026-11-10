export function dashboardSuite(T) {
  const { page, assert } = T;
  return {
    async showsFleet() {
      await T.open('#/', 'Dashboard');
      await T.see('5 of 5 nodes active');
      for (const label of ['Download vs. expected', 'Upload vs. expected', 'Median latency', 'Tests succeeded', 'IP changes']) await T.see(label);
      for (const label of ['Bandwidth', 'Month to date', 'Projected month end', 'Previous month']) await T.see(label);
      await page.getByRole('columnheader', { name: 'Engine' }).last().waitFor();
      await page.getByRole('img', { name: 'Daily bandwidth usage by node and engine' }).waitFor();
      const bandwidth = page.locator('.card').filter({ hasText: 'Measured download and upload bytes remain separate by node and engine.' });
      assert.ok(await bandwidth.locator('table').last().locator('tbody tr').count() > 0, 'the daily bandwidth table shows numeric buckets');
      for (const name of ['VM1', 'VM2', 'VM3', 'VM4', 'VM5']) await page.locator('.node-card').filter({ hasText: name }).first().waitFor();
      const changes = await page.locator('.stat').filter({ hasText: 'IP changes' }).locator('.stat-value').innerText();
      assert.match(changes, /^[1-9]\d*\s*change/, 'the seeded address changes must be counted, got ' + JSON.stringify(changes));
      await T.expectNoLoadError();
    },

    // The comparison chart: every metric, speed versus % of expected, chart versus table, hiding a node.
    async compareConnections() {
      await T.open('#/', 'Dashboard');
      const card = page.locator('.card').filter({ hasText: 'Compare connections' });
      for (const metric of ['Upload', 'Latency', 'Jitter', 'Loss', 'Download']) {
        await card.getByRole('tab', { name: metric }).click();
        await T.expectNoLoadError();
      }
      await card.getByRole('tab', { name: '% of expected' }).click();
      await card.getByRole('tab', { name: 'Table' }).click();
      await card.locator('table').waitFor();
      assert.ok(await card.locator('table tbody tr').count() > 3, 'the table lists the hourly or daily values');
      await card.getByRole('tab', { name: 'Chart' }).click();
      await card.getByRole('tab', { name: 'Speed' }).click();
      const legend = card.locator('.legend button').first();
      await legend.click();
      assert.ok((await legend.getAttribute('class') || '').includes('off'), 'a clicked node is hidden from the chart');
      await card.locator('.legend button').first().click();
      assert.ok(!((await card.locator('.legend button').first().getAttribute('class')) || '').includes('off'), 'and shown again');
    },

    // Every shortcut on the page leads where it says.
    async shortcuts() {
      await T.open('#/', 'Dashboard');
      await page.getByRole('link', { name: 'All results' }).click();
      await T.heading('Results').waitFor();
      await T.open('#/', 'Dashboard');
      await page.getByRole('link', { name: 'Manage' }).click();
      await T.heading('Alerts').waitFor();
      await T.open('#/', 'Dashboard');
      await page.getByRole('link', { name: 'See history' }).click();
      await T.heading('IP & location').waitFor();
      await T.open('#/', 'Dashboard');
      await page.getByRole('link', { name: /Add a node/ }).first().click();
      await T.heading('Nodes').waitFor();
      await T.open('#/', 'Dashboard');
      await page.getByRole('button', { name: 'Run all tests' }).click();
      await T.dialog(/Run|test/).first().waitFor();
      await T.dismiss();
    },

    // The time range in the top bar drives every page.
    async timeRange() {
      await T.open('#/', 'Dashboard');
      await T.settled();
      await T.choose(page.locator('#topbar select').first(), '24h');
      await T.see('last 24 hours', page.locator('#view'));
      await T.open('#/stats', 'Statistics');
      await T.see('last 24 hours', page.locator('#view'));
      await T.choose(page.locator('#topbar select').first(), '30d');
      await T.see('last 30 days', page.locator('#view'));
      await T.choose(page.locator('#topbar select').first(), '7d');
    },
  };
}

export function dashboardSuite(T) {
  const { page, assert } = T;
  return {
    async showsFleet() {
      await T.open('#/', 'Dashboard');
      await T.see('5 of 5 nodes active');
      for (const label of ['Download vs. expected', 'Upload vs. expected', 'Median latency', 'Tests succeeded', 'IP changes']) await T.see(label);
      for (const name of ['VM1', 'VM2', 'VM3', 'VM4', 'VM5']) await page.locator('.node-card').filter({ hasText: name }).first().waitFor();
      const emptyBaseline = await page.locator('.stat .delta').allInnerTexts();
      assert.ok(emptyBaseline.includes('No previous period'), 'missing history is explained without an empty dash: ' + emptyBaseline.join(', '));
      await T.choose(page.locator('#topbar select').first(), '24h');
      await T.see('last 24 hours', page.locator('#view'));
      await T.settled();
      await T.eventually(async () => (await page.locator('.stat .delta').allInnerTexts()).some(value => /▲|▼|≈ same/.test(value)), 'KPI cards compare this period with the previous period');
      const changes = await page.locator('.stat').filter({ hasText: 'IP changes' }).locator('.stat-value').innerText();
      const expectedChanges = Number(T.db("select count(*) from node_ip_history where ts >= now()-interval '24 hours' and ts < now() and from_ip is not null"));
      assert.equal(Number(changes.match(/^\d+/)?.[0]), expectedChanges, 'the current-period IP change count matches the stored history');
      await T.choose(page.locator('#topbar select').first(), '7d');
      await T.see('last 7 days', page.locator('#view'));
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

    async comparisonRecoversFromFailure() {
      const failSnapshot = route => route.fulfill({ status: 500, contentType: 'application/json', body: JSON.stringify({ type: 'about:blank', title: 'Internal Server Error', status: 500, detail: 'Temporary failure.' }) });
      await page.route('**/api/dashboard**', failSnapshot);
      try {
        await page.goto(T.url('#/'));
        await page.reload();
        await T.heading('Dashboard').waitFor();
        await T.see('5 of 5 nodes active');
        await T.see('Comparison unavailable');
        T.expectRefusal(500);
      } finally {
        await page.unroute('**/api/dashboard**', failSnapshot);
      }
      await page.reload();
      await T.see('5 of 5 nodes active');
      await T.eventually(async () => (await page.locator('.stat .delta').allInnerTexts()).every(value => value !== 'Comparison unavailable' && value !== 'Loading comparison…'), 'the comparison recovers after refresh');
      await T.expectNoLoadError();
    },
  };
}

function demoNode() {
  return { id: 'demo73', name: 'Review fixture', label: 'Backoff review', provider: 'Fixture', plan: 'Test', colorSlot: 1,
    expectedDown: 100, expectedUp: 50, interval: 60, offset: 0, threads: 4, server: 'auto', primaryEngine: 'ookla',
    enabled: true, agent: '2.0.0', note: '', createdAt: Date.now(), profile: { effD: 1, effU: 1, cong: 0, ping: 10, jit: 1, loss: 0 },
    ipModel: { kind: 'static', prefix: '203.0.113', host: 73 }, pref: [1001] };
}

function demoResult(nodeId, ts, status) {
  const failed = status === 'failed';
  return { nodeId, ts, status, engine: 'ookla', failureClass: failed ? 'rate_limited' : 'other',
    error: failed ? 'Too many requests' : null, down: failed ? null : 90, up: failed ? null : 45,
    ping: failed ? null : 10, jitter: failed ? null : 1, loss: failed ? null : 0,
    dLat: null, uLat: null, serverId: failed ? null : 1001, ip: failed ? null : '203.0.113.73',
    threads: 4, bytesDown: failed ? 0 : 100, bytesUp: failed ? 0 : 50, duration: 30, manual: false };
}

async function loadDemoCase(page, origin, statuses) {
  const now = Date.now();
  const extra = statuses.map((status, index) => demoResult('demo73', now - (statuses.length - index) * 60000, status));
  const payload = { nodes: [demoNode()], settings: { range: '24h', timezone: 'UTC' }, extra };
  await page.goto(`${origin}/?demo=1#/stats`);
  await page.evaluate(value => localStorage.setItem('st.demo.v1', JSON.stringify(value)), payload);
  await page.reload();
  await page.getByRole('heading', { name: 'Summary by node' }).waitFor();
}

export function statisticsSuite(T) {
  const { page, assert } = T;
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
      const bandwidth = T.bandwidthCard();
      assert.equal(await bandwidth.count(), 1, 'Statistics has one Bandwidth section');
      const usage = (await T.api('/api/usage?group=day')).body.data;
      const expectedBuckets = new Set(usage.series.map(bucket => [bucket.start, bucket.nodeId, bucket.engine, bucket.ispId || ''].join('|'))).size;
      const dailyRows = bandwidth.locator('table').last().locator('tbody tr');
      assert.equal(await dailyRows.count(), expectedBuckets, 'Statistics shows every available daily bandwidth bucket');
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

    async demoRateLimitExclusions() {
      const origin = new URL(T.url('#/')).origin;
      await loadDemoCase(page, origin, ['ok', 'failed']);
      assert.match(await T.summaryRow('Review fixture').innerText(), /2\s+100\.0%\s+1 rate-limited excluded/);
      await page.setViewportSize({ width: 390, height: 900 });
      assert.ok(await page.getByText('1 rate-limited excluded', { exact: true }).isVisible());
      await page.goto(`${origin}/?demo=1#/nodes/demo73`);
      const mixed = page.locator('.stat').filter({ hasText: 'Success rate' });
      await mixed.waitFor();
      assert.match(await mixed.innerText(), /100\.0%[\s\S]*1 failed; 1 rate-limited excluded of 2/);

      await loadDemoCase(page, origin, ['failed']);
      assert.match(await T.summaryRow('Review fixture').innerText(), /1\s+—\s+1 rate-limited excluded/);
      await page.goto(`${origin}/?demo=1#/nodes/demo73`);
      const allLimited = page.locator('.stat').filter({ hasText: 'Success rate' });
      await allLimited.waitFor();
      assert.match(await allLimited.innerText(), /—[\s\S]*1 failed; 1 rate-limited excluded of 1/);
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false, 'node detail fits at 390 px');
      await page.setViewportSize({ width: 1440, height: 900 });
    },
  };
}

import { mkdirSync } from 'node:fs';

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
      const summaryCard = page.locator('.card').filter({ hasText: 'Summary by node' });
      assert.ok((await summaryCard.getByRole('row').filter({ hasText: 'VM1' }).innerText()).includes(vm1.toFixed(1)), 'the summary shows the center average');
      const before = await summaryCard.locator('table').innerText();
      await T.tab('Upload');
      await page.getByRole('heading', { name: 'Summary by node' }).waitFor();
      assert.notEqual(await page.locator('.card').filter({ hasText: 'Summary by node' }).locator('table').innerText(), before, 'switching to upload changes the figures');
      for (const section of ['Trend', 'Time of day', 'Week at a glance', 'Distribution']) await page.getByRole('heading', { name: section, exact: true }).waitFor();
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
      await page.getByRole('grid', { name: 'Heatmap for VM2' }).waitFor();
      await T.choose(page.locator('#topbar select').first(), '24h');
      await T.see('last 24 hours', page.locator('#view'));
      await T.choose(page.locator('#topbar select').first(), '7d');
      await T.expectNoLoadError();
    },

    async analyticsFiltersEventsAndHeatmap() {
      await T.open('#/stats', 'Statistics');
      await page.getByRole('heading', { name: 'Trend events' }).waitFor();
      const summary = (await T.api('/api/stats/summary?range=7d')).body.data;
      const vm1 = summary.find(x => x.nodeId === '10000000-0000-4000-8000-000000000001');
      const summaryCard = page.locator('.card').filter({ hasText: 'Summary by node' });
      for (const percentile of ['P50', 'P95', 'P99']) await summaryCard.getByRole('columnheader', { name: new RegExp(percentile) }).waitFor();
      const row = summaryCard.getByRole('row').filter({ hasText: 'VM1' });
      for (const value of [vm1.current.down.p50, vm1.current.down.p95, vm1.current.down.p99]) assert.ok((await row.innerText()).includes(value.toFixed(1)));
      const week = await T.api('/api/stats/summary?range=7d');
      const weekNode = week.body.data.find(x => x.nodeId === vm1.nodeId);
      const weekRow = page.locator('.card').filter({ hasText: 'Week over week' }).getByRole('row').filter({ hasText: 'VM1' });
      const weekText = await weekRow.innerText();
      for (const value of [weekNode.current.down.mean, weekNode.previous.down.mean]) {
        if (value == null) assert.match(weekText, /—/);
        else assert.ok(weekText.includes(value.toFixed(1)));
      }
      const eventTypes = await page.locator('.stats-event .tag').allTextContents();
      for (const type of ['ip change', 'isp change', 'alert']) assert.ok(eventTypes.includes(type), `event list omitted ${type}`);
      assert.ok(await page.locator('.card').filter({ hasText: /^Trend/ }).locator('svg line[stroke-dasharray="3 3"]').count() > 0, 'trend chart omitted event markers');
      await page.getByRole('gridcell').first().focus();
      assert.match(await page.getByRole('gridcell').first().getAttribute('aria-label'), /Mon 00:00/);

      const engineRequests = [];
      const collectEngine = request => { if (request.url().includes('/api/stats/')) engineRequests.push(new URL(request.url())); };
      page.on('request', collectEngine);
      const engineSummaryRequest = page.waitForRequest(request => {
        const url = new URL(request.url());
        return url.pathname === '/api/stats/summary' && url.searchParams.get('engine') === 'librespeed';
      });
      await T.choose(page.getByLabel('Engine', { exact: true }), 'librespeed');
      await engineSummaryRequest; await T.settled();
      for (const path of ['/api/stats/summary', '/api/stats/trend', '/api/stats/hour-of-day', '/api/stats/heatmap', '/api/stats/distribution', '/api/stats/events']) {
        assert.ok(engineRequests.some(url => url.pathname === path && url.searchParams.get('engine') === 'librespeed'), `${path} missed the explicit engine filter: ${engineRequests.map(url => url.pathname + url.search).join(', ')}`);
      }
      await T.choose(page.getByLabel('Engine', { exact: true }), ''); await T.settled();
      page.off('request', collectEngine);

      const requests = [];
      const collect = request => { if (request.url().includes('/api/stats/')) requests.push(new URL(request.url())); };
      page.on('request', collect);
      await T.fill('Filter by ISP', 'Carrier B'); await T.press('Apply');
      await page.getByRole('heading', { name: 'Trend events' }).waitFor(); await T.settled();
      const filtered = requests.splice(0);
      const expected = ['/api/stats/summary', '/api/stats/trend', '/api/stats/hour-of-day', '/api/stats/heatmap', '/api/stats/distribution', '/api/stats/events'];
      for (const path of expected) assert.ok(filtered.some(url => url.pathname === path && url.searchParams.get('isp') === 'Carrier B'), `${path} missed the ISP filter`);
      const weekRequest = page.waitForRequest(request => {
        const url = new URL(request.url());
        return url.pathname === '/api/stats/summary' && url.searchParams.get('range') === '7d' && url.searchParams.get('isp') === 'Carrier B';
      });
      await T.choose(page.locator('#topbar select').first(), '24h');
      const independentWeek = new URL((await weekRequest).url());
      assert.equal(independentWeek.searchParams.get('range'), '7d', 'week comparison is independent of the selected page range');
      await T.settled();
      page.off('request', collect);

      await T.fill('Filter by ISP', 'No matching ISP'); await T.press('Apply');
      await T.see('No data in the last 30 days');
      assert.match(await T.row('VM1').innerText(), /0/);
      await T.press('Clear ISP filter');
      await verifyEventCapIsolated(T, page, assert);
    },

    async demoParityAndRetry() {
      let failFirstEventsRequest = true;
      const eventRoute = '**/api/stats/events*';
      await page.route(eventRoute, async route => {
        if (!failFirstEventsRequest) return route.continue();
        failFirstEventsRequest = false;
        return route.fulfill({ status: 500, contentType: 'application/problem+json', body: JSON.stringify({ type: '/problems/test', title: 'Server error', status: 500, detail: 'Temporary fixture error' }) });
      });
      await T.open('#/stats', 'Statistics');
      const rangeSelect = page.locator('#topbar select').first();
      await T.choose(rangeSelect, (await rangeSelect.inputValue()) === '24h' ? '7d' : '24h');
      await T.see('Statistics are unavailable');
      assert.equal(failFirstEventsRequest, false, 'the events endpoint failure was injected');
      await T.press('Retry');
      await page.getByRole('heading', { name: 'Trend events' }).waitFor();
      T.expectRefusal(500);
      await T.expectNoLoadError();
      await page.unroute(eventRoute);

      await page.goto(T.url('#/stats').replace('/#/stats', '/?demo=1#/stats'));
      await T.heading('Statistics').waitFor();
      await page.getByRole('heading', { name: 'Week over week' }).waitFor();
      for (const percentile of ['P50', 'P95', 'P99']) await page.getByRole('columnheader', { name: new RegExp(percentile) }).first().waitFor();
      const demoEvents = await page.locator('.stats-event').evaluateAll(rows => rows.map(row => ({
        type: ['ip_change', 'isp_change', 'alert'].find(type => row.classList.contains(type)),
        ts: Date.parse(row.querySelector('time').dateTime),
      })));
      for (const type of ['ip_change', 'isp_change', 'alert']) assert.ok(demoEvents.some(event => event.type === type), `demo event list omitted ${type}`);
      const trend = page.locator('.card').filter({ has: page.getByRole('heading', { name: 'Trend', exact: true }) });
      const chart = trend.locator('svg');
      const viewBoxWidth = Number((await chart.getAttribute('viewBox')).split(/\s+/)[2]);
      const markers = await chart.locator('line.stats-event-marker').evaluateAll(lines => lines.map(line => ({
        type: line.getAttribute('data-event-type'), ts: Number(line.getAttribute('data-event-ts')),
        x1: Number(line.getAttribute('x1')), x2: Number(line.getAttribute('x2')),
      })));
      assert.deepEqual(markers.map(({ type, ts }) => ({ type, ts })), demoEvents, 'the SVG renders every numeric-epoch demo event with its type and timestamp');
      for (const marker of markers) {
        assert.ok(Number.isFinite(marker.x1) && marker.x1 > 52 && marker.x1 < viewBoxWidth - 14, `event marker has an in-range x coordinate: ${marker.x1}`);
        assert.equal(marker.x1, marker.x2, 'event marker is vertical at its calculated timestamp position');
      }
      const shotDir = '.tmp/e2e/joost-81'; mkdirSync(shotDir, { recursive: true });
      for (const theme of ['dark', 'light']) {
        if ((await page.locator('html').getAttribute('data-theme')) !== theme) await T.press('Toggle theme');
        for (const width of [390, 768, 1440]) {
          await page.setViewportSize({ width, height: 1000 }); await T.settled();
          assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth + 1), `statistics overflows at ${width}px in ${theme} theme`);
          await page.screenshot({ path: `${shotDir}/stats-${theme}-${width}.png`, fullPage: true });
        }
      }
      await page.setViewportSize({ width: 1440, height: 1000 });
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

async function verifyEventCapIsolated(T, page, assert) {
  const cleanup = "DELETE FROM alert_events WHERE title='E2E cap event' AND detail='plain text'";
  T.db(cleanup);
  try {
    T.db(`INSERT INTO alert_events(id,node_id,severity,ts,title,detail,trigger_result_id)
      SELECT gen_random_uuid(),'10000000-0000-4000-8000-000000000001','info',now()-g*interval '1 second','E2E cap event','plain text',NULL
      FROM generate_series(1,501) AS generated(g)`);
    await page.reload(); await T.heading('Statistics').waitFor();
    await T.see('Showing the newest 500 events. Older events are omitted.');
    const eventStatus = page.getByRole('status').filter({ hasText: 'Showing the newest 500 events' });
    const eventList = page.locator('.stats-events');
    assert.equal(await eventStatus.count(), 1);
    assert.equal(await eventStatus.locator('xpath=ancestor::ol').count(), 0, 'the truncation status is outside the list in the accessibility tree');
    assert.equal(await eventList.getByRole('listitem').count(), 500);
    assert.equal(await eventList.locator(':scope > .stats-truncated').count(), 0, 'the ordered list contains only list items');
    await T.expectNoLoadError();
  } finally {
    T.db(cleanup);
  }
  assert.equal(T.db("SELECT count(*) FROM alert_events WHERE title='E2E cap event' AND detail='plain text'"), '0', 'event-cap fixture does not leak into later scenarios');
}

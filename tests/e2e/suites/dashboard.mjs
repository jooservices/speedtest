import { mkdirSync } from 'node:fs';
import { join } from 'node:path';

const KPI_IP = '203.0.113.82';

function refuseManualRun(route) {
  return route.fulfill({ status: 409, contentType: 'application/problem+json', body: JSON.stringify({
    type: '/problems/conflict', title: 'State, unique name or idempotency conflict', status: 409, detail: 'A test is already queued.'
  }) });
}

function seedKpiFixture(T) {
  T.db(`INSERT INTO ip_addresses(ip) VALUES ('203.0.113.81'),('${KPI_IP}') ON CONFLICT DO NOTHING;
    INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,server,ip,threads,bytes_down,bytes_up,duration_seconds,engine)
    SELECT gen_random_uuid(), n.id, now()-make_interval(hours=>v.hours), 'ok', n.expected_down_mbps*v.down_pct/100.0, n.expected_up_mbps*v.up_pct/100.0,
      v.ping, 9, '{"id":9,"name":"JOOST-82 KPI fixture"}'::jsonb, '${KPI_IP}', 4, 1000, 500, 1, 'librespeed'
    FROM nodes n JOIN (VALUES
      ('VM1',60,60,10,1),('VM2',70,65,20,1),('VM3',80,70,30,1),('VM4',90,75,40,1),('VM5',100,80,50,1),
      ('VM1',50,40,20,25),('VM2',55,45,30,25),('VM3',60,50,40,25),('VM4',65,55,50,25),('VM5',70,60,60,25)
    ) AS v(name,down_pct,up_pct,ping,hours) ON n.name=v.name;
    INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,ip,threads,bytes_down,bytes_up,duration_seconds,error,engine)
    SELECT gen_random_uuid(), n.id, now()-make_interval(hours=>v.hours), 'failed', NULL, NULL, NULL, '${KPI_IP}', 4, 0, 0, 1, 'Synthetic KPI failure', 'librespeed'
    FROM nodes n JOIN (VALUES ('VM1',1),('VM2',25),('VM3',26),('VM4',27)) AS v(name,hours) ON n.name=v.name;
    INSERT INTO node_ip_history(id,node_id,ts,from_ip,to_ip)
    SELECT gen_random_uuid(), n.id, now()-make_interval(hours=>v.hours), '203.0.113.81', '${KPI_IP}'
    FROM nodes n CROSS JOIN (VALUES (1),(2),(25),(26),(27),(28)) AS v(hours) WHERE n.name='VM1';`);
}

function cleanupKpiFixture(T) {
  T.db(`DELETE FROM node_ip_history WHERE to_ip='${KPI_IP}'; DELETE FROM results WHERE ip='${KPI_IP}' AND engine='librespeed';
    DELETE FROM ip_addresses WHERE ip IN ('203.0.113.81','${KPI_IP}') AND NOT EXISTS (SELECT 1 FROM results WHERE ip IN ('203.0.113.81','${KPI_IP}'));`);
}

async function delayedComparisonPreservesEdit(T, page, status) {
  let releaseRequest, requestSeen, routeFinished;
  const held = new Promise(resolve => { releaseRequest = resolve; });
  const seen = new Promise(resolve => { requestSeen = resolve; });
  const finished = new Promise(resolve => { routeFinished = resolve; });
  const handler = async route => {
    requestSeen();
    await held;
    if (status === 500) await route.fulfill({ status, contentType: 'application/problem+json', body: JSON.stringify({
      type: '/problems/internal', title: 'Internal Server Error', status, detail: 'Temporary failure.'
    }) });
    else await route.continue();
    routeFinished();
  };
  await page.route('**/api/dashboard**', handler);
  try {
    await page.goto(T.url('#/'));
    await T.heading('Dashboard').waitFor();
    await seen;
    await page.evaluate(() => { location.hash = '#/nodes/10000000-0000-4000-8000-000000000001'; });
    await T.heading(/VM1/).waitFor();
    await T.tab('Settings');
    const download = page.getByLabel('Expected download', { exact: true });
    await download.fill('777');
    await download.focus();
    const response = page.waitForResponse(item => item.url().includes('/api/dashboard'));
    releaseRequest();
    await response;
    await finished;
    await page.waitForTimeout(150);
    if (status === 500) T.expectRefusal(500);
    T.assert.equal(await download.inputValue(), '777', `${status} comparison response keeps the unsaved download value`);
    T.assert.equal(await download.evaluate(el => el === document.activeElement), true, `${status} comparison response keeps keyboard focus`);
  } finally {
    releaseRequest();
    await page.unroute('**/api/dashboard**', handler);
  }
}

async function assertKpi(T, label, value, delta) {
  const card = T.page.locator('.stat').filter({ hasText: label }).first();
  T.assert.equal((await card.locator('.stat-value').innerText()).replace(/\s/g, ''), value.replace(/\s/g, ''), `${label} current value`);
  const change = card.locator('.delta');
  T.assert.equal((await change.innerText()).trim(), delta, `${label} period delta`);
  T.assert.ok((await change.getAttribute('class')).split(/\s+/).includes('up'), `${label} delta uses the good direction`);
}

export function dashboardSuite(T, ctx) {
  const { page, assert } = T;
  const { browser, stack } = ctx;
  return {
    async showsFleet() {
      await T.open('#/', 'Dashboard');
      await T.see('5 of 5 nodes active');
      for (const label of ['Download vs. expected', 'Upload vs. expected', 'Median latency', 'Tests succeeded', 'IP changes']) await T.see(label);
      for (const label of ['Bandwidth', 'Month to date', 'Projected month end', 'Previous month']) await T.see(label);
      await page.getByRole('columnheader', { name: 'Engine' }).last().waitFor();
      await page.getByRole('img', { name: 'Daily bandwidth usage by node and engine' }).waitFor();
      const bandwidth = T.bandwidthCard();
      assert.equal(await bandwidth.count(), 1, 'the dashboard has one Bandwidth section');
      const usage = (await T.api('/api/usage?group=day')).body.data;
      const expectedBuckets = new Set(usage.series.map(bucket => [bucket.start, bucket.nodeId, bucket.engine, bucket.ispId || ''].join('|'))).size;
      const dailyRows = bandwidth.locator('table').last().locator('tbody tr');
      assert.equal(await dailyRows.count(), expectedBuckets, 'the daily bandwidth table shows every available numeric bucket');
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

    async comparesExactKpis() {
      seedKpiFixture(T);
      try {
        await T.open('#/', 'Dashboard');
        await T.choose(page.locator('#topbar select').first(), '24h');
        await T.settled();
        await T.choose(page.getByLabel('Engine', { exact: true }), 'librespeed');
        await T.settled();
        await T.see('Download vs. expected', page.locator('#view'));
        await T.eventually(async () => {
          const cards = page.locator('.grid.cols-5 .stat'), values = await cards.locator('.delta').allInnerTexts();
          return await cards.count() === 5 && values.length === 5 && values.every(value => value !== 'Loading comparison…');
        }, 'the dashboard comparison has loaded');
        await assertKpi(T, 'Download vs. expected', '80%', '▲ 33.3%');
        await assertKpi(T, 'Upload vs. expected', '70%', '▲ 40.0%');
        await assertKpi(T, 'Median latency', '30.0 ms', '▼ 25.0%');
        await assertKpi(T, 'Tests succeeded', '83.3%', '▲ 33.3%');
        await assertKpi(T, 'IP changes', '2 changes', '▼ 50.0%');
        await page.getByRole('link', { name: 'See history' }).waitFor();

        const zeroBaseline = await T.api('/api/dashboard?range=6h&engine=librespeed');
        assert.equal(zeroBaseline.status, 200, 'the fresh dashboard snapshot is available');
        assert.equal(zeroBaseline.body.data.previous.ips, 0, 'the fresh dashboard snapshot sees zero previous IP changes');
        assert.equal(zeroBaseline.body.data.current.ips, 2, 'the fresh dashboard snapshot sees two current IP changes');
        await T.choose(page.locator('#topbar select').first(), '6h');
        await page.reload();
        await T.heading('Dashboard').waitFor();
        await T.settled();
        const ipCard = page.locator('.stat').filter({ hasText: 'IP changes' }).first();
        assert.equal((await ipCard.locator('.stat-value').innerText()).replace(/\s/g, ''), '2changes');
        assert.equal((await ipCard.locator('.delta').innerText()).trim(), 'New this period', 'zero baseline is explained');
      } finally {
        cleanupKpiFixture(T);
        await page.goto(T.url('#/'));
        await T.heading('Dashboard').waitFor();
        await T.choose(page.getByLabel('Engine', { exact: true }), '');
        await T.settled();
        await T.choose(page.locator('#topbar select').first(), '7d');
      }
    },

    async keyboardCardDoesNotCatchRunEnter() {
      await T.open('#/', 'Dashboard');
      const card = page.locator('.node-card').filter({ hasText: 'VM1' }).first();
      await card.focus();
      await page.keyboard.press('Enter');
      await page.getByRole('heading', { name: /VM1/, level: 1 }).waitFor();
      await T.open('#/', 'Dashboard');
      const run = page.locator('.node-card').filter({ hasText: 'VM1' }).first().getByRole('button', { name: 'Run a test on VM1' });
      await page.route('**/api/nodes/*/run', refuseManualRun);
      try {
        await run.focus();
        await page.keyboard.press('Enter');
        const dialog = page.getByRole('dialog');
        await dialog.waitFor();
        await T.see('Not started: the node is paused or already has a test waiting', dialog);
        assert.equal(new URL(page.url()).hash, '#/', 'Enter on Run opens its test dialog without activating the card');
        T.expectRefusal(409);
        await T.dismiss();
      } finally {
        await page.unroute('**/api/nodes/*/run', refuseManualRun);
      }
    },

    async delayedComparisonKeepsNodeEdit() {
      await delayedComparisonPreservesEdit(T, page, 200);
      await delayedComparisonPreservesEdit(T, page, 500);
      await page.goto(T.url('#/'));
      await T.heading('Dashboard').waitFor();
      await T.expectNoLoadError();
    },

    // The comparison chart: every metric, speed versus % of expected, chart versus table, hiding a node.
    async compareConnections() {
      const previousDemo = await page.evaluate(() => localStorage.getItem('st.demo.v1'));
      try {
        await page.goto(T.url('?demo=1#/'));
        await T.heading('Dashboard').waitFor();
        const card = page.locator('.card').filter({ hasText: 'Compare connections' });
        const hit = card.locator('.chart svg rect.hit').first();
        await hit.scrollIntoViewIfNeeded();
        const bounds = await hit.boundingBox();
        await page.mouse.move(bounds.x + bounds.width / 2, bounds.y + bounds.height / 2);
        await card.locator('.tooltip.show').waitFor();
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
      } finally {
        await page.evaluate(value => { if (value == null) localStorage.removeItem('st.demo.v1'); else localStorage.setItem('st.demo.v1', value); }, previousDemo);
        await page.goto(T.url('#/'));
        await T.heading('Dashboard').waitFor();
      }
    },

    async dashboardResultActions() {
      await T.open('#/', 'Dashboard');
      const latest = page.locator('.card').filter({ hasText: 'Latest results' }).first();
      const row = latest.getByRole('row').nth(1), action = row.getByRole('button', { name: /Open result details for/ });
      assert.equal(await action.count(), 1, 'the dashboard result row exposes its named action');
      await action.focus();
      await page.keyboard.press('Enter');
      await page.getByRole('dialog', { name: 'Result details' }).waitFor();
      await page.keyboard.press('Escape');
      await row.locator('td').nth(3).click();
      await page.getByRole('dialog', { name: 'Result details' }).waitFor();
      await page.keyboard.press('Escape');
    },

    async dashboardAlertCanBeAcknowledgedInDemo() {
      const previousDemo = await page.evaluate(() => localStorage.getItem('st.demo.v1'));
      try {
        await page.goto(T.url('?demo=1#/'));
        await T.heading('Dashboard').waitFor();
        const alerts = page.locator('.card').filter({ hasText: 'Open alerts' }).first();
        const acknowledge = alerts.getByRole('button', { name: 'Acknowledge' }).first();
        await acknowledge.waitFor();
        await acknowledge.click();
        await T.toast('Alert acknowledged').waitFor();
      } finally {
        await page.evaluate(value => { if (value == null) localStorage.removeItem('st.demo.v1'); else localStorage.setItem('st.demo.v1', value); }, previousDemo);
        await page.goto(T.url('#/'));
        await T.heading('Dashboard').waitFor();
      }
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

    async rateLimitedCount() {
      const saved = 'joost73_saved_result_times';
      const ids = ['73000000-0000-4000-8000-000000000001', '73000000-0000-4000-8000-000000000002', '73000000-0000-4000-8000-000000000003'];
      const imageDir = new URL('../../../.tmp/e2e/shots/', import.meta.url).pathname;
      mkdirSync(imageDir, { recursive: true });
      T.db(`CREATE TABLE ${saved} AS SELECT id,ts FROM results WHERE ts >= now()-interval '6 hours';
        UPDATE results SET ts=now()-interval '7 hours' WHERE ts >= now()-interval '6 hours';
        INSERT INTO results(id,node_id,ts,status,threads,error,engine,failure_class,bytes_down,bytes_up,duration_seconds)
        SELECT fixture.id,n.id,now()-fixture.age,'failed',4,'Too many requests','ookla','rate_limited',0,0,0
        FROM nodes n CROSS JOIN (VALUES ('${ids[0]}'::uuid,interval '3 minutes'),('${ids[1]}'::uuid,interval '2 minutes'),('${ids[2]}'::uuid,interval '1 minute')) fixture(id,age)
        WHERE n.name='VM1'`);
      try {
        assert.equal(T.db("SELECT count(*) FROM results WHERE ts >= now()-interval '6 hours'"), '3', 'the fixture range contains only the three rate-limited results');
        await T.open('#/', 'Dashboard');
        await T.choose(page.locator('#topbar select').first(), '6h');
        await T.settled();
        await T.expectNoLoadError();
        const success = page.locator('.stat').filter({ hasText: 'Tests succeeded' });
        const hint = success.locator('.stat-hint');
        await hint.waitFor();
        assert.equal(await hint.getAttribute('aria-hidden'), null, 'rate-limit count remains exposed to assistive technology');
        assert.equal((await success.locator('.stat-value').innerText()).trim(), '—%', 'an all-rate-limited range has no success percentage');
        assert.equal((await hint.innerText()).replaceAll(/\s+/g, ' ').trim(), '3 rate-limited tests excluded from success rate · no eligible tests');
        await T.open('#/stats', 'Statistics');
        await T.choose(page.locator('#topbar select').first(), '6h');
        await T.settled();
        assert.match(await T.summaryRow('VM1').innerText(), /3\s+—\s+3 rate-limited excluded/, 'all-rate-limited statistics show the excluded count');
        const nodeId = T.db("SELECT id::text FROM nodes WHERE name='VM1'");
        await T.open('#/nodes/' + nodeId, 'VM1');
        let nodeSuccess = page.locator('.stat').filter({ hasText: 'Success rate' });
        assert.match(await nodeSuccess.innerText(), /—[\s\S]*3 failed; 3 rate-limited excluded of 3/, 'node detail explains an all-rate-limited range');

        T.db(`UPDATE results SET status='ok',down_mbps=100,up_mbps=50,ping_ms=10,error=NULL,failure_class='other',
          server_id=1,server='{"name":"Fixture"}'::jsonb,ip=(SELECT ip FROM ip_addresses LIMIT 1)
          WHERE id='${ids[0]}'; DELETE FROM results WHERE id='${ids[2]}'`);
        // The center shares one dashboard snapshot for 5 seconds, so reload until the KPI reflects the edit.
        const mixedKPI = page.locator('.stat').filter({ hasText: 'Tests succeeded' });
        await T.eventually(async () => {
          await page.reload();
          await page.waitForFunction(() => ST.remote.status === 'ready');
          await T.open('#/', 'Dashboard');
          await T.choose(page.locator('#topbar select').first(), '6h');
          await T.settled();
          return /100\.0%[\s\S]*1 rate-limited test excluded from success rate/.test(await mixedKPI.innerText());
        }, 'the mixed dashboard KPI shows one eligible success and the excluded rate limit', 20000);
        await T.open('#/stats', 'Statistics');
        await T.choose(page.locator('#topbar select').first(), '6h');
        await T.settled();
        assert.match(await T.summaryRow('VM1').innerText(), /2\s+100\.0%\s+1 rate-limited excluded/, 'mixed statistics show one eligible success and the excluded rate limit');
        await T.open('#/nodes/' + nodeId, 'VM1');
        nodeSuccess = page.locator('.stat').filter({ hasText: 'Success rate' });
        assert.match(await nodeSuccess.innerText(), /100\.0%[\s\S]*1 failed; 1 rate-limited excluded of 2/, 'node detail explains the mixed success rate');
        await T.open('#/', 'Dashboard');
        await T.choose(page.locator('#topbar select').first(), '6h');
        await T.settled();
        for (const theme of ['dark', 'light']) {
          await page.evaluate(value => document.documentElement.setAttribute('data-theme', value), theme);
          for (const width of [390, 768, 1440]) {
            await page.setViewportSize({ width, height: 900 });
            await page.waitForTimeout(220);
            assert.equal(await page.locator('#sidebar').evaluate(element => element.classList.contains('open')), false, `mobile navigation stays closed at ${width}px`);
            await page.evaluate(() => new Promise(done => requestAnimationFrame(() => requestAnimationFrame(done))));
            assert.ok(await hint.isVisible(), `rate-limit count hidden at ${width}px in ${theme} theme`);
            const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
            assert.ok(overflow <= 1, `rate-limit card overflows by ${overflow}px at ${width}px in ${theme} theme`);
            await page.screenshot({ path: join(imageDir, `rate-limited-${theme}-${width}.png`) });
          }
        }
        await page.setViewportSize({ width: 390, height: 900 });
        await page.waitForTimeout(220);
        await page.getByRole('button', { name: 'Open menu' }).focus();
        await page.keyboard.press('Enter');
        await page.locator('#sidebar.open').waitFor();
        await page.getByRole('link', { name: 'Dashboard' }).focus();
        await page.keyboard.press('Enter');
        await page.waitForFunction(() => !document.querySelector('#sidebar').classList.contains('open'));
        assert.ok(await hint.isVisible(), 'keyboard navigation leaves the rate-limit explanation visible');

        const touchContext = await browser.newContext({ viewport: { width: 390, height: 900 }, hasTouch: true,
          storageState: await page.context().storageState() });
        try {
          const touchPage = await touchContext.newPage();
          await touchPage.goto(stack.origin);
          const touchHint = touchPage.locator('.stat-hint');
          await touchHint.waitFor();
          assert.match(await touchHint.innerText(), /1 rate-limited test excluded from success rate/);
          await touchPage.getByRole('button', { name: 'Open menu' }).tap();
          await touchPage.locator('#sidebar.open').waitFor();
          await touchPage.getByRole('link', { name: 'Dashboard' }).tap();
          await touchPage.waitForFunction(() => !document.querySelector('#sidebar').classList.contains('open'));
          assert.ok(await touchHint.isVisible(), 'touch navigation leaves the rate-limit explanation visible');
        } finally {
          await touchContext.close();
        }
      } finally {
        T.db(`DELETE FROM results WHERE id IN (${ids.map(id => `'${id}'`).join(',')});
          UPDATE results r SET ts=s.ts FROM ${saved} s WHERE r.id=s.id;
          DROP TABLE IF EXISTS ${saved}`);
        await page.locator('#topbar select').first().selectOption('7d').catch(() => {});
        await page.setViewportSize({ width: 1440, height: 900 });
        await page.evaluate(() => document.documentElement.setAttribute('data-theme', 'dark'));
      }
    },
  };
}

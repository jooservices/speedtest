import { mkdirSync } from 'node:fs';
import { join } from 'node:path';

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
        await page.reload();
        await page.waitForFunction(() => ST.remote.status === 'ready');
        await T.open('#/', 'Dashboard');
        await T.choose(page.locator('#topbar select').first(), '6h');
        await T.settled();
        const mixedKPI = page.locator('.stat').filter({ hasText: 'Tests succeeded' });
        assert.match(await mixedKPI.innerText(), /100\.0%[\s\S]*1 rate-limited test excluded from success rate/);
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

const UPLOAD_NODE_ID = '82000000-0000-4000-8000-000000000091';
const UPLOAD_IP = '203.0.113.91';
const SPARSE_IP = '203.0.113.92';
const ONE_BUCKET_NODE_ID = '82000000-0000-4000-8000-000000000101';
const TWO_BUCKET_NODE_ID = '82000000-0000-4000-8000-000000000102';
const BUCKET_IP = '203.0.113.101';

function refuseManualRun(route) {
  return route.fulfill({ status: 409, contentType: 'application/problem+json', body: JSON.stringify({
    type: '/problems/conflict', title: 'State, unique name or idempotency conflict', status: 409, detail: 'A test is already queued.'
  }) });
}

function seedUploadFixture(T) {
  T.db(`INSERT INTO ip_addresses(ip) VALUES ('${UPLOAD_IP}') ON CONFLICT DO NOTHING;
    INSERT INTO nodes(id,name,label,expected_down_mbps,expected_up_mbps,color_slot,host,agent) VALUES
      ('${UPLOAD_NODE_ID}','Upload fixture','Known expected speeds',400,75,8,'upload-fixture','test');
    INSERT INTO schedules(node_id) VALUES ('${UPLOAD_NODE_ID}');
    INSERT INTO engine_schedules(node_id,engine) VALUES ('${UPLOAD_NODE_ID}','ookla');
    INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,server,ip,threads,bytes_down,bytes_up,duration_seconds,engine) VALUES
      ('82000000-0000-4000-8000-000000000092','${UPLOAD_NODE_ID}',now()-interval '1 hour','ok',120,30,8,9,
       '{"id":9,"name":"Upload fixture server"}'::jsonb,'${UPLOAD_IP}',4,1000,500,1,'ookla');`);
}

function cleanupUploadFixture(T) {
  T.db(`DELETE FROM nodes WHERE id='${UPLOAD_NODE_ID}'; DELETE FROM ip_addresses WHERE ip='${UPLOAD_IP}' AND NOT EXISTS (SELECT 1 FROM results WHERE ip='${UPLOAD_IP}');`);
}

function seedSparseTrendFixture(T) {
  T.db(`INSERT INTO ip_addresses(ip) VALUES ('${SPARSE_IP}') ON CONFLICT DO NOTHING;
    INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,server,ip,threads,bytes_down,bytes_up,duration_seconds,error,engine)
    SELECT gen_random_uuid(), n.id, now()-make_interval(hours=>v.hours), v.status,
      CASE WHEN v.status='failed' THEN NULL ELSE n.expected_down_mbps*(0.4+v.seq/100.0) END,
      CASE WHEN v.status='failed' THEN NULL ELSE n.expected_up_mbps*(0.4+v.seq/100.0) END,
      CASE WHEN v.status='failed' THEN NULL ELSE 8+v.seq END,
      CASE WHEN v.status='failed' THEN NULL ELSE 9 END,
      CASE WHEN v.status='failed' THEN NULL ELSE '{"id":9,"name":"Sparse trend fixture"}'::jsonb END,
      '${SPARSE_IP}',4,1000,500,1,CASE WHEN v.status='failed' THEN 'Synthetic sparse-series failure' ELSE NULL END,'librespeed'
    FROM nodes n JOIN (VALUES
      ('VM1',1,'failed',1),('VM2',2,'ok',1),('VM3',3,'ok',1),('VM3',4,'ok',2),
      ('VM4',5,'ok',1),('VM4',29,'ok',2),('VM4',53,'ok',3),
      ('VM5',2,'ok',1),('VM5',26,'ok',2),('VM5',50,'ok',3),('VM5',74,'ok',4)
    ) AS v(name,hours,status,seq) ON n.name=v.name;`);
}

function cleanupSparseTrendFixture(T) {
  T.db(`DELETE FROM results WHERE ip='${SPARSE_IP}' AND engine='librespeed'; DELETE FROM ip_addresses WHERE ip='${SPARSE_IP}' AND NOT EXISTS (SELECT 1 FROM results WHERE ip='${SPARSE_IP}');`);
}

function seedSparseBuckets(T) {
  T.db(`INSERT INTO ip_addresses(ip) VALUES ('${BUCKET_IP}') ON CONFLICT DO NOTHING;
    INSERT INTO nodes(id,name,label,expected_down_mbps,expected_up_mbps,color_slot,host,agent) VALUES
      ('${ONE_BUCKET_NODE_ID}','One bucket fixture','Three samples',200,100,7,'one-bucket-fixture','test'),
      ('${TWO_BUCKET_NODE_ID}','Two bucket fixture','Five samples',200,100,8,'two-bucket-fixture','test');
    INSERT INTO engine_schedules(node_id,engine) VALUES ('${ONE_BUCKET_NODE_ID}','ookla'),('${TWO_BUCKET_NODE_ID}','ookla');
    WITH base AS (SELECT date_trunc('day', now()) - interval '12 hours' AS anchor), samples AS (
      SELECT '${ONE_BUCKET_NODE_ID}'::uuid AS node_id, anchor - v.hours * interval '1 hour' AS ts, v.seq FROM base CROSS JOIN (VALUES (0,1),(1,2),(2,3)) v(hours,seq)
      UNION ALL
      SELECT '${TWO_BUCKET_NODE_ID}'::uuid, anchor - v.hours * interval '1 hour', v.seq FROM base CROSS JOIN (VALUES (0,1),(1,2),(2,3)) v(hours,seq)
      UNION ALL
      SELECT '${TWO_BUCKET_NODE_ID}'::uuid, anchor - interval '1 day' - v.hours * interval '1 hour', v.seq FROM base CROSS JOIN (VALUES (0,4),(1,5)) v(hours,seq)
    )
    INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,server,ip,threads,bytes_down,bytes_up,duration_seconds,engine)
    SELECT gen_random_uuid(), node_id, ts, 'ok', 100+seq, 50+seq, 10+seq, 9, '{"id":9,"name":"Bucket fixture"}'::jsonb, '${BUCKET_IP}', 4, 1000, 500, 1, 'ookla' FROM samples;`);
}

function cleanupSparseBuckets(T) {
  T.db(`DELETE FROM results WHERE node_id IN ('${ONE_BUCKET_NODE_ID}','${TWO_BUCKET_NODE_ID}');
    DELETE FROM engine_schedules WHERE node_id IN ('${ONE_BUCKET_NODE_ID}','${TWO_BUCKET_NODE_ID}');
    DELETE FROM nodes WHERE id IN ('${ONE_BUCKET_NODE_ID}','${TWO_BUCKET_NODE_ID}');
    DELETE FROM ip_addresses WHERE ip='${BUCKET_IP}' AND NOT EXISTS (SELECT 1 FROM results WHERE ip='${BUCKET_IP}');`);
}

async function checkNodeChartReferences(T, id, name, sparse) {
  await T.open('#/nodes/' + id, new RegExp(name));
  const card = T.page.locator('.card').filter({ hasText: 'Speed over time' }).first(), svg = card.locator('.chart svg').first();
  await svg.waitFor();
  const refs = svg.locator('line[stroke-dasharray="5 4"]');
  const lines = await svg.locator('line').evaluateAll(items => items.map(line => ({ dash: line.getAttribute('stroke-dasharray'), y: line.getAttribute('y1') })));
  T.assert.equal(await refs.count(), 2, `${name} shows separate expected download and upload lines; rendered lines: ${JSON.stringify(lines)}`);
  const y = await refs.evaluateAll(lines => lines.map(line => Number(line.getAttribute('y1'))));
  T.assert.ok(y.every(value => Number.isFinite(value) && value >= 24 && value <= 254), `${name} reference lines stay within the chart plot: ${y}`);
  if (sparse) await T.see('Not enough data yet', card);
  else T.assert.equal(await card.getByText(/Not enough data yet/).count(), 0, `${name} has enough data for a trend`);
}

async function hoverChartValue(page, cardName) {
  const card = page.locator('.card').filter({ hasText: cardName }).first();
  const hit = card.locator('.chart svg rect.hit').first();
  await hit.scrollIntoViewIfNeeded();
  const bounds = await hit.boundingBox();
  for (let hour = 0; hour <= 23; hour++) {
    await page.mouse.move(bounds.x + bounds.width * hour / 23, bounds.y + bounds.height / 2);
    if (await card.locator('.tooltip.show').count()) return;
  }
  throw new Error(`${cardName} chart did not show a tooltip for any plotted time`);
}

let manualRunSequence = 0;

async function runManual(T, name, message) {
  const { page } = T;
  const attempt = ++manualRunSequence;
  await T.open('#/nodes', 'Nodes');
  const row = T.row(name);
  try {
    await row.waitFor({ timeout: 10000 });
  } catch (error) {
    const exists = T.db(`SELECT count(*) FROM nodes WHERE name='${name}'`);
    throw new Error(`node row ${name} did not render at ${page.url()} (database rows: ${exists}): ${error.message}`);
  }
  await row.click();
  await page.getByRole('heading', { name: new RegExp(name), level: 1 }).waitFor();
  await T.press('Run test');
  const dialog = T.dialog('Running speed test');
  await dialog.waitFor();
  try {
    await dialog.getByText(message).waitFor({ timeout: 90000 });
  } catch (error) {
    const recent = T.db(`SELECT status||':'||COALESCE(error,'') FROM agent_commands c JOIN nodes n ON n.id=c.node_id WHERE n.name='${name}' ORDER BY c.created_at DESC LIMIT 1`);
    throw new Error(`manual run ${attempt} expected ${message}; latest command: ${recent}; ${error.message}`);
  }
  await T.press('Close', dialog.locator('.modal-foot'));
}

async function restartAgentAndWait(T, agent, name, scenario = 'rate_limited') {
  const since = T.db(`SELECT last_seen_at::text FROM nodes WHERE name='${name}'`);
  await agent.restart({ scenario });
  await T.eventually(() => T.db(`SELECT (last_seen_at > '${since}'::timestamptz)::int FROM nodes WHERE name='${name}'`) === '1', 'the restarted rate-limited agent checked in');
}

async function verifyRateLimitedAgent(T, agent, name) {
  const { assert } = T;
  const alertsBefore = Number(T.db(`SELECT count(*) FROM alert_events e JOIN alert_rules r ON r.id=e.rule_id JOIN nodes n ON n.id=e.node_id WHERE n.name='${name}' AND r.metric='failed'`));
  T.db("UPDATE alert_rules SET for_tests=1 WHERE metric='failed'");
  try {
    await restartAgentAndWait(T, agent, name);
    await runManual(T, name, /Failed: speedtest: rate_limited/);
    const stored = T.db(`SELECT count(*) FROM results r JOIN nodes n ON n.id=r.node_id WHERE n.name='${name}' AND r.failure_class='rate_limited' AND r.manual`);
    assert.equal(stored, '1', 'the real Ookla error is spooled, ingested and classified');
    const alerts = Number(T.db(`SELECT count(*) FROM alert_events e JOIN nodes n ON n.id=e.node_id JOIN alert_rules r ON r.id=e.rule_id WHERE n.name='${name}' AND r.metric='failed'`));
    assert.equal(alerts, alertsBefore, 'rate-limited ingestion does not trigger a new failure alert');
    await restartAgentAndWait(T, agent, name);
    await runManual(T, name, /Failed: Test deferred: ookla provider rate-limit backoff is active until/);
    assert.equal(T.db(`SELECT count(*) FROM results r JOIN nodes n ON n.id=r.node_id WHERE n.name='${name}' AND r.failure_class='rate_limited' AND r.manual`), '1', 'restart preserves the deadline and prevents a second engine attempt');
    await runManual(T, name, /Failed: Test deferred: ookla provider rate-limit backoff is active until/);
    assert.equal(T.db(`SELECT count(*) FROM results r JOIN nodes n ON n.id=r.node_id WHERE n.name='${name}' AND r.failure_class='rate_limited' AND r.manual`), '1', 'repeated manual requests remain deferred without another rate-limit result');
    const successfulManuals = Number(T.db(`SELECT count(*) FROM results r JOIN nodes n ON n.id=r.node_id WHERE n.name='${name}' AND r.status='ok' AND r.manual`));
    // The deadline is 90 s +/- 20% jitter after the first rate-limited result (72-108 s); the steps above take up to about 60 s on a busy
    // machine, so wait until the latest possible deadline has passed. (The old 30 s window was shorter than the steps on a loaded machine.)
    await new Promise(resolve => setTimeout(resolve, 62_000));
    await agent.restart({ scenario: 'success' });
    await runManual(T, name, /Mbps ↓|Gbps ↓|kbps ↓/);
    assert.equal(Number(T.db(`SELECT count(*) FROM results r JOIN nodes n ON n.id=r.node_id WHERE n.name='${name}' AND r.status='ok' AND r.manual`)), successfulManuals + 1, 'the real agent resumes successful measurements after the deadline');
  } finally {
    T.db("UPDATE alert_rules SET for_tests=2 WHERE metric='failed'");
  }
}

export function nodesSuite(T, ctx) {
  const { page, assert } = T;
  const { stack, startAgent, fetchAgentConfig, submitScheduledResult, submitFailedResult, sendAgentHeartbeat, receiver } = ctx;
  async function pinOoklaAndCheckFallback() {
    await T.press('Refresh server list');
    await T.row('Gateway Central').waitFor({ timeout: 90000 });
    await page.getByRole('columnheader', { name: 'Distance' }).waitFor();
    await assertPickerTableFits(page.locator('.setting').filter({ hasText: 'Search Ookla servers' }).first(), 'Ookla');
    const search = 'Search Ookla servers';
    await T.fill(search, 'Gateway');
    const server = T.row('Gateway Central');
    await server.waitFor();
    await T.fill(search, 'Hanoi');
    await server.waitFor();
    await T.fill(search, 'Ookla ISP');
    await server.waitFor();
    await server.getByRole('button', { name: 'Pin' }).click();
    await T.toast('Ookla server selection saved').waitFor();
    await T.press('Use automatic selection');
    await T.toast('Ookla server selection saved').waitFor();
    await server.getByRole('button', { name: 'Pin' }).click();
    await T.toast('Ookla server selection saved').waitFor();
    assert.equal(T.db("select pinned_server_id from node_engine_servers c join nodes n on n.id=c.node_id where n.name='E2E-1' and c.engine='ookla'"), '54321');
    await T.press('Run test');
    const run = T.dialog('Running speed test');
    await run.getByText(/Mbps ↓|Gbps ↓|kbps ↓/).waitFor({ timeout: 90000 });
    await T.press('Close', run.locator('.modal-foot'));
    const saved = T.db("select r.server_id::text||':'||r.warnings::text from results r join nodes n on n.id=r.node_id where n.name='E2E-1' order by r.ts desc limit 1");
    assert.match(saved, /^12345:.*Pinned server was unreachable/);
    await T.open('#/history', 'Results');
    await T.fill('Search results', 'E2E-1');
    const resultRow = page.getByRole('row').filter({ hasText: 'Different from pinned' }).first();
    await resultRow.waitFor();
    await resultRow.click();
    await page.getByText(/instead of pinned server #54321/).waitFor();
    await page.getByText(/Pinned server was unreachable/).waitFor();
    await page.locator('dd').filter({ hasText: /Singapore Gateway, Singapore \(#12345\)/ }).waitFor();
  }

  async function pinLibreSpeedAndCheckPicker() {
    await T.open('#/nodes', 'Nodes');
    await T.row('E2E-1').click();
    await T.tab('Settings');
    const libre = page.locator('.setting').filter({ hasText: 'LibreSpeed' }).first();
    await libre.getByRole('button', { name: 'Refresh server list' }).click();
    const libreRows = libre.locator('tbody tr');
    await libreRows.nth(1).waitFor({ timeout: 90000 });
    await assertPickerTableFits(libre, 'LibreSpeed');
    assert.match(await libreRows.nth(0).innerText(), /Hanoi/);
    assert.match(await libreRows.nth(1).innerText(), /Singapore/);
    assert.equal(await libre.getByRole('columnheader', { name: 'Distance' }).count(), 0, 'LibreSpeed has no invented distance column');
    await libre.getByRole('row').filter({ hasText: 'Singapore' }).getByRole('button', { name: 'Pin' }).click();
    await T.toast('LibreSpeed server selection saved').waitFor();
    await T.toast('LibreSpeed server selection saved').waitFor({ state: 'hidden' });
    assert.equal(T.db("select pinned_server_id from node_engine_servers c join nodes n on n.id=c.node_id where n.name='E2E-1' and c.engine='librespeed'"), '52');
    const enabled = page.getByRole('checkbox', { name: 'LibreSpeed enabled' });
    if (!(await enabled.isChecked())) await enabled.click();
    await T.press('Save engine schedules');
    await T.toast('Engine schedules saved').waitFor();
    assert.equal(T.db("select s.enabled from engine_schedules s join nodes n on n.id=s.node_id where n.name='E2E-1' and s.engine='librespeed'"), 't');
    return libre;
  }

  async function runLibreSpeedFallbackAndCheckHistory() {
    T.db("update nodes set primary_engine='librespeed',server_id=52,config_revision=config_revision+1 where name='E2E-1'");
    await T.press('Run test');
    const run = T.dialog('Running speed test');
    await run.getByText(/Mbps ↓|Gbps ↓|kbps ↓/).waitFor({ timeout: 90000 });
    await T.press('Close', run.locator('.modal-foot'));
    const saved = T.db("select r.server_id::text||':'||r.pinned_server_id_at_test::text||':'||coalesce(r.server->>'host','')||':'||coalesce(r.server->>'endpoint','')||':'||r.server_selection_known::text||':'||r.warnings::text from results r join nodes n on n.id=r.node_id where n.name='E2E-1' and r.engine='librespeed' and r.manual order by r.ts desc limit 1");
    assert.match(saved, /^51:52:hn\.example\.test:https:\/\/hn\.example\.test\/backend:true:.*Pinned server was unreachable/);
    await T.open('#/history', 'Results');
    await T.fill('Search results', 'E2E-1');
    const resultRow = page.getByRole('row').filter({ hasText: 'Different from pinned' }).first();
    await resultRow.waitFor();
    await resultRow.click();
    await page.getByText(/instead of pinned server #52/).waitFor();
    await page.getByText(/Pinned server was unreachable/).waitFor();
    await page.locator('dd').filter({ hasText: /Hanoi.*#51/ }).waitFor();
  }

  async function checkServerPickerResponsive(libre) {
    const original = page.viewportSize();
    for (const theme of ['dark', 'light']) {
      await page.evaluate(value => document.documentElement.setAttribute('data-theme', value), theme);
      for (const width of [390, 768, 1440]) {
        await page.setViewportSize({ width, height: 900 });
        await libre.scrollIntoViewIfNeeded();
        await page.evaluate(() => window.scrollTo(0, 0));
        await page.evaluate(() => new Promise(done => requestAnimationFrame(() => requestAnimationFrame(done))));
        const overflow = await page.evaluate(() => {
          const wide = [];
          for (const el of document.querySelectorAll('.content *')) {
            const box = el.getBoundingClientRect();
            if (box.width && box.right > document.documentElement.clientWidth + 1 && !el.closest('.table-wrap, .tabs, .seg, pre, .legend')) wide.push(`${el.tagName}.${el.className} ${Math.round(box.left)}-${Math.round(box.right)} width=${Math.round(box.width)} min=${getComputedStyle(el).minWidth}`);
          }
          return { page: document.documentElement.scrollWidth - document.documentElement.clientWidth, x: scrollX, wide: wide.slice(0, 5) };
        });
        assert.ok(overflow.page <= 1, `node server settings overflow at ${width}px (${theme}): ${JSON.stringify(overflow)}`);
        assert.deepEqual(overflow.wide, [], `node server settings elements overflow at ${width}px (${theme})`);
        if (width >= 768) await assertPickerTableFits(libre, 'LibreSpeed');
        if (width <= 900) {
          const menu = await page.locator('.menu-btn').boundingBox();
          assert.ok(menu && menu.x >= 0 && menu.x + menu.width <= width, `mobile menu is outside the viewport at ${width}px (${theme}): ${JSON.stringify(menu)}`);
        }
        if (process.env.E2E_SHOTS) await page.screenshot({ path: `${process.env.E2E_SHOTS}/server-selection-${theme}-${width}.png`, fullPage: true });
      }
    }
    await page.setViewportSize(original);
    await page.evaluate(() => document.documentElement.setAttribute('data-theme', 'dark'));
  }

  async function assertPickerTableFits(section, label) {
    const table = section.locator('.table-wrap').first();
    const size = await table.evaluate(el => ({ client: el.clientWidth, scroll: el.scrollWidth }));
    assert.ok(size.client > 0 && size.scroll <= size.client + 1, `${label} server table clips at desktop width: ${JSON.stringify(size)}`);
  }
  return {
    async publishedUpdate() {
      T.db("update nodes set agent='speedtest-agent/1.0.0 (linux/amd64)' where name='VM3'");
      try {
        await T.open('#/nodes', 'Nodes');
        await T.row('VM3').click();
        await T.tab('Agent');
        await page.getByRole('button', { name: 'Update to v2.0.0' }).waitFor();
        if (process.env.E2E_SHOTS) await page.screenshot({ path: `${process.env.E2E_SHOTS}/published-agent-update.png` });
        await T.press('Update to v2.0.0');
        await T.toast('Update queued').waitFor();
        assert.equal(T.db("select count(*) from agent_commands c join nodes n on n.id=c.node_id where n.name='VM3' and c.type='update_agent' and c.version='2.0.0'"), '1');
      } finally {
        T.db("update nodes set agent='speedtest-agent/2.0.0' where name='VM3'");
      }
    },
    async listSortsAndOpens() {
      await T.open('#/nodes', 'Nodes');
      for (const name of ['VM1', 'VM2', 'VM3', 'VM4', 'VM5']) await T.row(name).waitFor();
      const header = page.getByRole('columnheader', { name: 'Node' });
      await header.click();
      await T.expectNoLoadError();
      await T.row('VM2').focus();
      await page.keyboard.press('Enter');
      await page.getByRole('heading', { name: /VM2/, level: 1 }).waitFor();
      await T.open('#/nodes', 'Nodes');
      assert.equal(await page.getByRole('table').count(), 1, 'the node list keeps table semantics');
      const openNode = T.row('VM1').getByRole('link', { name: /VM1.*Viettel Fast 50/ });
      assert.equal(await openNode.count(), 1, 'the node row exposes its primary action in the accessibility tree');
      await openNode.focus();
      await page.keyboard.press('Enter');
      await page.getByRole('heading', { name: /VM1/, level: 1 }).waitFor();
      await T.open('#/nodes', 'Nodes');
      const run = T.row('VM1').getByRole('button', { name: 'Run a test on VM1' });
      await page.route('**/api/nodes/*/run', refuseManualRun);
      try {
        await run.focus();
        await page.keyboard.press('Enter');
        const dialog = page.getByRole('dialog');
        await dialog.waitFor();
        await T.see('Not started: the node is paused or already has a test waiting', dialog);
        assert.equal(new URL(page.url()).hash, '#/nodes', 'Enter on a row action opens its dialog without opening the row');
        T.expectRefusal(409);
        await T.dismiss();
      } finally {
        await page.unroute('**/api/nodes/*/run', refuseManualRun);
      }
    },
    async listShowsUpload() {
      seedUploadFixture(T);
      try {
        assert.equal(T.db(`SELECT count(*) FROM nodes WHERE id='${UPLOAD_NODE_ID}'`), '1', 'the known-value node fixture was inserted');
        const response = await T.api('/api/nodes'), apiNames = response.body && response.body.data ? response.body.data.map(node => node.name) : [];
        assert.ok(apiNames.includes('Upload fixture'), `the authenticated nodes API includes the fixture: ${apiNames.join(', ')}`);
        await T.open('#/nodes', 'Nodes');
        await page.evaluate(() => window.ST.load());
        await T.settled();
        const names = await page.evaluate(() => window.ST.nodes().map(node => node.name));
        assert.ok(names.includes('Upload fixture'), `the center snapshot includes the seeded upload fixture: ${names.join(', ')}`);
        const row = T.row('Upload fixture');
        await row.waitFor();
        await T.expectCount(page.getByRole('columnheader', { name: /^Upload / }), 1, 'the list has one upload column');
        assert.equal((await row.locator('td').nth(2).innerText()).trim(), '400 Mbps / 75 Mbps', 'the row shows the distinct expected download and upload values');
        assert.equal((await row.locator('td').nth(3).innerText()).trim(), '120 Mbps (30%)', 'download is 120/400 Mbps = 30%');
        assert.equal((await row.locator('td').nth(4).innerText()).trim(), '30 Mbps (40%)', 'upload is 30/75 Mbps = 40%');
        await T.expectNoLoadError();
      } finally {
        cleanupUploadFixture(T);
      }
    },

    async sparseTrendsExplainThreshold() {
      seedSparseTrendFixture(T);
      try {
        await T.open('#/', 'Dashboard');
        await T.choose(page.getByLabel('Engine'), 'librespeed');
        await T.settled();
        const dashboard = page.locator('.card').filter({ hasText: 'Compare connections' }).first();
        const dashboardNote = dashboard.locator('.callout').filter({ hasText: 'Not enough data yet' });
        const noteText = await dashboardNote.innerText();
        for (const expected of ['A download trend', 'VM1 · Viettel Fast 50: 0', 'VM2 · Viettel Pro 600: 1', 'VM3 · VNPT Home 5 Super: 2']) assert.ok(noteText.includes(expected), `dashboard sparse state includes ${expected}: ${noteText}`);
        assert.ok(!noteText.includes('VM4 · VNPT Home 2'), 'three valid measurements do not show a sparse warning');
        for (const name of ['VM1', 'VM2', 'VM3']) await dashboard.locator('.legend button').filter({ hasText: name }).click();
        assert.equal(await dashboard.locator('.callout').filter({ hasText: 'Not enough data yet' }).count(), 0, 'hidden sparse dashboard series do not keep the warning visible');
        await T.choose(page.getByLabel('Engine'), '');
        await T.settled();
        assert.equal(await dashboard.locator('.callout').filter({ hasText: 'Not enough data yet' }).count(), 0, 'the primary-engine filter excludes sparse LibreSpeed measurements');

        await T.open('#/stats', 'Statistics');
        await T.choose(page.getByLabel('Engine'), 'librespeed');
        await T.settled();
        const trend = page.locator('.card').filter({ hasText: 'Trend' }).first();
        const statsNote = trend.locator('.callout').filter({ hasText: 'Not enough data yet' });
        const statsText = await statsNote.innerText();
        for (const expected of ['A download trend', 'VM1: 0 samples in 0 plotted daily buckets', 'VM2: 1 sample in 1 plotted daily bucket', 'VM3: 2 samples in 1 plotted daily bucket']) assert.ok(statsText.includes(expected), `Statistics sparse state includes ${expected}: ${statsText}`);
        assert.ok(!statsText.includes('VM4:'), 'three valid measurements do not show a Statistics warning');
        for (const name of ['VM1', 'VM2', 'VM3']) {
          await page.locator('.legend.mb button').filter({ hasText: name }).click();
          await T.settled();
        }
        assert.equal(await trend.locator('.callout').filter({ hasText: 'Not enough data yet' }).count(), 0, 'unselected Statistics nodes do not keep the warning visible');
        await checkNodeChartReferences(T, '10000000-0000-4000-8000-000000000001', 'VM1', true);
        await checkNodeChartReferences(T, '10000000-0000-4000-8000-000000000003', 'VM3', true);
        await checkNodeChartReferences(T, '10000000-0000-4000-8000-000000000004', 'VM4', false);
      } finally {
        cleanupSparseTrendFixture(T);
        await page.goto(T.url('#/'));
        await page.reload();
        await T.heading('Dashboard').waitFor();
      }
    },
    async sparseWarningsCountBuckets() {
      seedSparseBuckets(T);
      const previousDemo = await page.evaluate(() => localStorage.getItem('st.demo.v1'));
      try {
        await page.goto(T.url('#/'));
        await page.reload();
        await T.heading('Dashboard').waitFor();
        await T.open('#/stats', 'Statistics');
        await page.getByLabel('Time range').selectOption('7d');
        await T.settled();
        const apiTrend = page.locator('.card').filter({ hasText: 'Trend' }).first();
        const apiNote = apiTrend.locator('.callout').filter({ hasText: 'Not enough data yet' });
        const apiText = await apiNote.innerText();
        assert.ok(apiText.includes('One bucket fixture: 3 samples in 1 plotted daily bucket'), `API mode reports raw samples and plotted buckets: ${apiText}`);
        assert.ok(apiText.includes('Two bucket fixture: 5 samples in 2 plotted daily buckets'), `API mode handles two occupied buckets: ${apiText}`);
        await page.getByRole('row').filter({ hasText: 'One bucket fixture' }).locator('td').nth(1).click();
        await page.getByRole('heading', { name: /One bucket fixture/ }).waitFor();
        await T.open('#/stats', 'Statistics');

        await page.goto(T.url('?demo=1#/stats'));
        await page.waitForFunction(() => window.ST && ST.demo);
        await page.evaluate(() => {
          const one = ST.addNode({ name: 'One bucket fixture', label: 'Three samples', expectedDown: 200, expectedUp: 100 });
          const two = ST.addNode({ name: 'Two bucket fixture', label: 'Five samples', expectedDown: 200, expectedUp: 100 });
          const anchor = Date.now() - 12 * 60 * 60 * 1000;
          const add = (node, hours, seq) => ({ id: 'bucket-' + node.id + '-' + seq, nodeId: node.id, ts: anchor - hours * 3600000,
            status: 'ok', down: 100 + seq, up: 50 + seq, ping: 10 + seq, jitter: 1, loss: 0, dLat: 10, uLat: 10,
            serverId: 1001, ip: '198.51.100.101', threads: 4, bytesDown: 1000, bytesUp: 500, duration: 1, engine: 'ookla' });
          ST.state.settings.range = '7d';
          ST.state.extra = [0, 1, 2].map(i => add(one, i, i + 1)).concat([0, 1, 2].map(i => add(two, i, i + 1)), [0, 1].map(i => add(two, 24 + i, i + 4)));
          ST.persist();
        });
        await page.reload();
        await T.heading('Statistics').waitFor();
        await T.settled();
        const demoTrend = page.locator('.card').filter({ hasText: 'Trend' }).first();
        const demoText = await demoTrend.locator('.callout').filter({ hasText: 'Not enough data yet' }).innerText();
        assert.ok(demoText.includes('One bucket fixture: 3 samples in 1 plotted daily bucket'), `demo mode reports raw samples and plotted buckets: ${demoText}`);
        assert.ok(demoText.includes('Two bucket fixture: 5 samples in 2 plotted daily buckets'), `demo mode handles two occupied buckets: ${demoText}`);
        await page.getByLabel('Add combined total').check();
        await T.settled();
        await hoverChartValue(page, 'Trend');
        await hoverChartValue(page, 'Time of day');
        const heatmap = page.locator('.card').filter({ hasText: 'Week at a glance' }).first();
        const cell = heatmap.locator('svg rect[style*="--heat-"]').first();
        await cell.waitFor();
        const bounds = await cell.boundingBox();
        await page.mouse.move(bounds.x + bounds.width / 2, bounds.y + bounds.height / 2);
        await heatmap.locator('.tooltip.show').waitFor();
      } finally {
        await page.evaluate(value => { if (value == null) localStorage.removeItem('st.demo.v1'); else localStorage.setItem('st.demo.v1', value); }, previousDemo);
        cleanupSparseBuckets(T);
        await page.goto(T.url('#/'));
        await T.heading('Dashboard').waitFor();
      }
    },
    async sparseChartShowsReference() {
      const node = '82000000-0000-4000-8000-000000000082';
      T.db(`INSERT INTO nodes(id,name,label,expected_down_mbps,expected_up_mbps,color_slot,host,agent) VALUES ('${node}','Sparse E2E','Two measurements',100,50,7,'sparse-e2e','test')`);
      T.db(`INSERT INTO engine_schedules(node_id,engine) VALUES ('${node}','ookla')`);
      T.db("INSERT INTO ip_addresses(ip) VALUES ('203.0.113.82') ON CONFLICT DO NOTHING");
      T.db(`INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,server,ip,threads,bytes_down,bytes_up,duration_seconds,engine) VALUES
        ('82000000-0000-4000-8000-000000000083','${node}',now()-interval '1 hour','ok',80,40,8,9,'{"id":9,"name":"Test server"}'::jsonb,'203.0.113.82',4,1000,500,1,'ookla'),
        ('82000000-0000-4000-8000-000000000084','${node}',now()-interval '2 hours','ok',82,42,8,9,'{"id":9,"name":"Test server"}'::jsonb,'203.0.113.82',4,1000,500,1,'ookla')`);
      try {
        await page.evaluate(() => window.ST.load());
        await page.goto(T.url('#/nodes/' + node));
        await page.getByRole('heading', { name: /Sparse E2E/ }).waitFor();
        await T.see('Not enough data yet');
        await T.see('Expected download');
        await T.see('Expected upload');
        await T.expectNoLoadError();
      } finally {
        T.db(`DELETE FROM nodes WHERE id='${node}'`);
      }
    },

    // The wizard with the one-line command, a real agent that enrols with the shown key, the first result, a manual run.
    async addWithCommandAndRunTest() {
      // Keep this delivery scenario independent of the migration's quiet-hour default.
      T.db(`UPDATE settings SET value='""' WHERE key IN ('notifyQuietStart','notifyQuietEnd')`);
      T.db(`insert into notification_event_settings(event_key,enabled,severity,channels)
        values ('test.success',true,'info',ARRAY['webhook']),('test.manual_finished',true,'info',ARRAY['webhook'])
        on conflict (event_key) do update set enabled=excluded.enabled,severity=excluded.severity,channels=excluded.channels`);
      await T.open('#/settings', 'Settings');
      await T.tab('Notifications');
      await T.fillAndLeave('URL for Webhook', receiver.url);
      await T.toast('Saved').waitFor();
      await T.settled();
      await page.getByLabel('Enable Webhook').check({ force: true });
      await T.settled();
      const receivedBefore = receiver.bodies.length;
      await T.open('#/nodes', 'Nodes');
      await T.press('Add a node');
      const dialog = T.dialog('Add a node');
      await T.fill('Node name', 'E2E-1', dialog);
      await T.fill('What it measures', 'End to end test line', dialog);
      await T.fill('Expected download', '100', dialog);
      await T.fill('Expected upload', '50', dialog);
      await T.press('Continue', dialog);
      await T.press(/I will run a command/, dialog);
      const command = await dialog.locator('pre, code, .code').first().innerText();
      const token = /--token\s+(\S+)/.exec(command)?.[1];
      assert.ok(token && token.length > 20, 'the wizard shows a real one-time enrolment key: ' + command.slice(0, 80));
      assert.match(command, /install\.sh/);
      await T.press('I ran the command', dialog);
      const agent = await startAgent(stack, { name: 'E2E-1', token, backoffInitial: '90s', backoffMax: '90s' });
      await T.see('E2E-1 is online', dialog, 90000);
      await submitScheduledResult(stack, { name: 'E2E-1' });
      await dialog.getByText(/First result: .* down, .* up, .* ping\./).waitFor({ timeout: 90000 });
      const schedules = T.db("select s.interval_minutes||':'||s.offset_minutes||':'||e.interval_minutes||':'||e.offset_minutes from schedules s join nodes n on n.id=s.node_id join engine_schedules e on e.node_id=n.id and e.engine='ookla' where n.name='E2E-1'");
      assert.equal(schedules, '60:0:60:0', 'both center schedule tables start at 60 minutes and offset 0');
      const agentConfig = await fetchAgentConfig(stack, 'E2E-1');
      assert.equal(agentConfig.schedule.interval, 60, 'the enrolled agent receives the 60-minute schedule');
      assert.equal(agentConfig.schedule.offset, 0, 'the enrolled agent receives offset 0');
      const firstResultManual = T.db("select r.manual from results r join nodes n on n.id=r.node_id where n.name='E2E-1' order by r.ts asc limit 1") === 't';
      const firstEventKey = firstResultManual ? 'test.manual_finished' : 'test.success';
      await T.eventually(() => Number(T.db(`select count(*) from alert_events e join nodes n on n.id=e.node_id where n.name='E2E-1' and e.event_key='${firstEventKey}'`)) > 0, 'the first agent result creates its matching notification event');

      await submitScheduledResult(stack, { name: 'E2E-1' });
      await T.eventually(() => Number(T.db("select count(*) from alert_events e join nodes n on n.id=e.node_id where n.name='E2E-1' and e.event_key='test.success'")) > 0, 'a scheduled successful test creates a notification event');
      const successPayload = JSON.parse(T.db("select e.payload::text from alert_events e join nodes n on n.id=e.node_id where n.name='E2E-1' and e.event_key='test.success' order by e.ts desc limit 1"));
      for (const field of ['download', 'upload', 'ping', 'jitter', 'loss', 'download_limit', 'upload_limit', 'ping_limit', 'jitter_limit', 'loss_limit']) {
        assert.ok(Object.hasOwn(successPayload, field), `success notification includes ${field}`);
      }
      await T.eventually(() => {
        const deliveryStates = T.db("select coalesce(string_agg(d.status||':'||d.attempts,','),'none') from notification_deliveries d join alert_events e on e.id=d.event_id join nodes n on n.id=e.node_id where n.name='E2E-1' and e.event_key='test.success' and d.channel='webhook'");
        const received = receiver.bodies.slice(receivedBefore).some(message => /Test success/.test(message.text || ''));
        return received || `deliveries=${deliveryStates}; receiver=${receiver.bodies.length - receivedBefore}`;
      }, 'success notification reaches the configured webhook', 75000);
      const successMessage = receiver.bodies.slice(receivedBefore).map(message => message.text || '').find(message => message.includes('Test success'));
      for (const metric of ['Download', 'upload', 'ping', 'jitter', 'loss']) assert.ok(successMessage.includes(metric), `webhook message lists ${metric}`);
      await T.press('Open node', dialog);
      await page.getByRole('heading', { name: /E2E-1/, level: 1 }).waitFor();

      await T.press('Run test');
      const run = T.dialog('Running speed test');
      await run.waitFor();
      await run.getByText(/Mbps ↓|Gbps ↓|kbps ↓/).waitFor({ timeout: 90000 });
      await T.press('Close', run.locator('.modal-foot'));
      const stored = T.db("select count(*) from results r join nodes n on n.id=r.node_id where n.name='E2E-1' and r.manual");
      assert.ok(Number(stored) >= 1, 'the manual result is stored on the center: ' + stored);
      await T.eventually(() => Number(T.db("select count(*) from alert_events e join nodes n on n.id=e.node_id where n.name='E2E-1' and e.event_key='test.manual_finished'")) >= 1, 'manual result creates a finished-test event');
      await T.eventually(() => receiver.bodies.slice(receivedBefore).some(message => /Manual test finished/.test(message.text || '')), 'manual completion notification reaches the configured webhook', 75000);

      const nodeID = T.db("select id::text from nodes where name='E2E-1'");
      // Reuse the suite's global failed-test and offline rules instead of creating duplicate alerts.
      const recoveryRule = '20000000-0000-4000-8000-000000000002';
      T.db(`INSERT INTO notification_event_settings(event_key,enabled,severity,channels)
        VALUES ('test.failed',true,'warn',ARRAY['webhook']),('test.recovered',true,'info',ARRAY['webhook']),
          ('node.offline',true,'crit',ARRAY['webhook']),('node.online',true,'info',ARRAY['webhook'])
        ON CONFLICT (event_key) DO UPDATE SET enabled=excluded.enabled,severity=excluded.severity,channels=excluded.channels`);
      const recoveryBefore = receiver.bodies.length;
      await submitFailedResult(stack, { name: 'E2E-1' });
      await submitFailedResult(stack, { name: 'E2E-1' });
      await T.eventually(() => Number(T.db(`select count(*) from alert_events where rule_id='${recoveryRule}' and event_key='test.failed'`)) === 1, 'two failed tests open one failed-test alert');
      await T.eventually(() => receiver.bodies.slice(recoveryBefore).some(message => /Test failed/.test(message.text || '')), 'failed-test notification reaches the webhook', 75000);
      await submitScheduledResult(stack, { name: 'E2E-1' });
      const recoveryResultID = T.db(`select id::text from results where node_id='${nodeID}' and status='ok' order by ts desc limit 1`);
      await T.eventually(() => {
        const count = Number(T.db(`select count(*) from alert_events where node_id='${nodeID}' and trigger_result_id='${recoveryResultID}' and event_key='test.recovered'`));
        return count > 0 || JSON.stringify({ count, state: T.db(`select consecutive_count||':'||active||':'||COALESCE(active_event_id::text,'')||':'||COALESCE(last_evaluated_at::text,'') from alert_state where rule_id='${recoveryRule}' and node_id='${nodeID}'`), results: T.db(`select status||':'||ts::text from results where node_id='${nodeID}' order by ts desc limit 4`) });
      }, 'the next successful test creates a recovery event');
      const recovered = JSON.parse(T.db(`select payload::text from alert_events where node_id='${nodeID}' and trigger_result_id='${recoveryResultID}' and event_key='test.recovered' order by ts desc limit 1`));
      assert.ok(recovered.duration, 'the test recovery includes the failed streak duration');
      await T.eventually(() => receiver.bodies.slice(recoveryBefore).some(message => /Tests recovered/.test(message.text || '')), 'test recovery notification reaches the webhook', 75000);

      await verifyRateLimitedAgent(T, agent, 'E2E-1');
      await agent.stop();
      T.db(`UPDATE engine_schedules SET interval_minutes=5 WHERE node_id='${nodeID}' AND engine='ookla';
        UPDATE schedules SET interval_minutes=5 WHERE node_id='${nodeID}';
        UPDATE nodes SET last_seen_at=now()-interval '16 minutes' WHERE id='${nodeID}'`);
      await T.eventually(() => Number(T.db(`select count(*) from alert_events where node_id='${nodeID}' and event_key='node.offline'`)) === 1, 'three missed intervals open one node-offline event', 45000);
      await T.eventually(() => receiver.bodies.slice(recoveryBefore).some(message => /Node offline/.test(message.text || '')), 'node-offline notification reaches the webhook', 75000);
      await sendAgentHeartbeat(stack, { name: 'E2E-1' });
      await T.eventually(() => Number(T.db(`select count(*) from alert_events where node_id='${nodeID}' and event_key='node.online'`)) === 1, 'the restored heartbeat creates a node-online recovery event', 45000);
      const online = JSON.parse(T.db(`select payload::text from alert_events where node_id='${nodeID}' and event_key='node.online' order by ts desc limit 1`));
      assert.ok(online.duration, 'the node recovery includes its downtime');
      await T.eventually(() => receiver.bodies.slice(recoveryBefore).some(message => /Node back online/.test(message.text || '')), 'node-online notification reaches the webhook', 75000);
      // The next scenario (server discovery) needs this node's agent running: the node-offline steps above stopped it.
      await agent.restart({ scenario: 'success' });

      await T.open('#/history', 'Results');
      await T.selectWith('Any status').selectOption('all');
      await T.fill('Search results', 'E2E-1');
      await page.getByRole('row').nth(1).waitFor();
      // The same line cannot be enrolled twice with the same one-time key.
      const again = await page.request.post(stack.browserOrigin + '/api/agent/enroll', { data: { token, name: 'E2E-copy', agent: 'x' } });
      assert.ok(again.status() >= 400, 'an enrolment key works once, got ' + again.status());
    },

    async discoverPinAndFallback() {
      await T.open('#/nodes', 'Nodes');
      await T.row('E2E-1').click();
      await T.tab('Settings');
      await pinOoklaAndCheckFallback();
      await T.dismiss();
      const libre = await pinLibreSpeedAndCheckPicker();
      await checkServerPickerResponsive(libre);
      await runLibreSpeedFallbackAndCheckHistory();
    },

    async demoNodeDefault() {
      await page.goto(stack.origin + '/?demo=1#/nodes');
      await page.getByRole('heading', { name: 'Nodes' }).waitFor();
      await T.press('Add a node');
      const dialog = T.dialog('Add a node');
      await T.fill('Node name', 'Demo default');
      await T.fill('What it measures', 'Demo default schedule');
      await T.fill('Expected download', '100');
      await T.fill('Expected upload', '50');
      await T.press('Continue', dialog);
      await T.press(/I will run a command/, dialog);
      await T.press('I ran the command', dialog);
      await dialog.getByRole('heading', { name: 'Demo default is online' }).waitFor({ timeout: 20000 });
      assert.ok(await dialog.getByText('It will now test every 60 minutes.').isVisible());
      const schedule = await page.evaluate(() => {
        const node = ST.nodes().find(value => value.name === 'Demo default');
        return node && { interval: node.interval, offset: node.offset };
      });
      assert.deepEqual(schedule, { interval: 60, offset: 0 });
      await page.goto(stack.origin + '/?demo=1#/schedules');
      await T.heading('Schedules').waitFor();
      assert.equal(await T.row('Demo default').locator('select').inputValue(), '60');
      assert.equal(await T.row('Demo default').getByRole('spinbutton').inputValue(), '0');
    },

    // Wizard validation: nothing is created from an incomplete or duplicate form.
    // On a phone the list is cards (no sideways scrolling) and a card still opens the node.
    async cardsOnPhone() {
      const original = page.viewportSize();
      await page.setViewportSize({ width: 390, height: 844 });
      await T.open('#/nodes', 'Nodes');
      await T.settled();
      const overflow = await page.evaluate(() => { const w = document.querySelector('.table-wrap'); return w.scrollWidth - w.clientWidth; });
      assert.ok(overflow <= 1, `the node list scrolls sideways by ${overflow}px`);
      assert.equal(await page.locator('table.data thead').evaluate(e => getComputedStyle(e).position), 'absolute', 'the header row is hidden on phones');
      const card = T.row('VM1');
      for (const label of ['Status', 'Public IP']) {
        const cell = card.locator(`td[data-label="${label}"]`);
        await cell.waitFor();
        assert.equal(await cell.evaluate(e => getComputedStyle(e, '::before').content), `"${label}"`, `each value is captioned with its column (${label})`);
      }
      await card.click();
      await page.getByRole('heading', { name: /VM1/, level: 1 }).waitFor();
      await page.setViewportSize(original);
    },

    async wizardValidation() {
      await T.open('#/nodes', 'Nodes');
      await T.press('Add a node');
      const dialog = T.dialog('Add a node');
      await T.press('Continue', dialog);
      await dialog.getByText(/name/i).first().waitFor();
      await T.fill('Node name', 'VM1', dialog);
      await T.fill('Expected download', '100', dialog);
      await T.fill('Expected upload', '50', dialog);
      await T.press('Continue', dialog);
      await T.see('A node with this name already exists', dialog);
      T.expectRefusal(409);
      await T.press('Cancel', dialog);
      assert.equal(T.db("select count(*) from nodes where name='VM1'"), '1', 'a duplicate name creates nothing');
    },

    // Node detail: overview, settings edit (validation then save), agent tab.
    async detailAndSettings() {
      await T.open('#/nodes', 'Nodes');
      await T.row('VM3').click();
      await page.getByRole('heading', { name: /VM3/, level: 1 }).waitFor();
      await T.see('Expected');
      await T.tab('Settings');
      await T.see('Bandwidth cost advice', page);
      await T.see('per day', page);
      await T.fillAndLeave('Ookla interval', '120', page);
      await T.see('Estimate for every 120 minutes', page);
      await T.fillAndLeave('Ookla interval', '60', page);
      await T.see('Estimate for every 60 minutes', page);
      await T.fill('Node name', '', page);
      await T.press('Save changes');
      await T.toast('Node name cannot be empty').waitFor();
      await T.fill('Node name', 'VM3', page);
      await T.fill('What does it measure?', 'VNPT edited', page);
      await T.fill('Expected download', '0');
      await T.press('Save changes');
      await T.toast('Expected speeds must be above zero').waitFor();
      await T.fill('Expected download', '320');
      await T.press('Save changes');
      await T.toast('Settings saved').waitFor();
      assert.equal(T.db("select expected_down_mbps::int || ' ' || label from nodes where name='VM3'"), '320 VNPT edited');
      // A name that another node already uses is refused.
      await T.fill('Node name', 'VM1');
      await T.press('Save changes');
      await T.toast('Another node already has this name.').waitFor();
      T.expectRefusal(409);
      await T.press('Discard');
      await T.tab('Agent');
      await T.see('Program on the machine');
      await T.tab('Overview');
      await T.expectNoLoadError();
    },

    async pauseAndResume() {
      await T.open('#/nodes', 'Nodes');
      await T.row('VM4').click();
      await page.getByRole('heading', { name: /VM4/, level: 1 }).waitFor();
      await T.press('Pause');
      await T.toast('paused').waitFor();
      await T.settled();
      await T.eventually(() => T.db("select enabled from nodes where name='VM4'") === 'f', 'the pause is stored');
      await T.eventually(async () => await page.getByRole('button', { name: 'Run test' }).isDisabled(), 'a paused node cannot be tested');
      await T.press('Resume');
      await T.toast('resumed').waitFor();
      await T.settled();
      await T.eventually(() => T.db("select enabled from nodes where name='VM4'") === 't', 'the resume is stored');
    },

    // Removing a node from the dashboard only (no SSH): the node and its results disappear.
    async removeFromDashboard(name) {
      await T.open('#/nodes', 'Nodes');
      await T.row(name).click();
      await page.getByRole('heading', { name: new RegExp(name), level: 1 }).waitFor();
      await T.tab('Settings');
      await T.press(new RegExp('^Remove ' + name));
      const removal = T.dialog('Remove ' + name);
      await T.press(/Only remove from the dashboard/, removal);
      await T.see('sudo speedtest-agent uninstall --all', removal);
      await T.press(new RegExp('^Remove ' + name), removal);
      await T.heading('Nodes').waitFor();
      assert.equal(T.db(`select count(*) from nodes where name='${name}'`), '0', 'the node is gone from the center');
    },
  };
}

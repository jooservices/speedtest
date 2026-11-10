const UPLOAD_NODE_ID = '82000000-0000-4000-8000-000000000091';
const UPLOAD_IP = '203.0.113.91';
const SPARSE_IP = '203.0.113.92';

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
      ('VM4',5,'ok',1),('VM4',6,'ok',2),('VM4',7,'ok',3),
      ('VM5',2,'ok',1),('VM5',3,'ok',2),('VM5',4,'ok',3),('VM5',5,'ok',4)
    ) AS v(name,hours,status,seq) ON n.name=v.name;`);
}

function cleanupSparseTrendFixture(T) {
  T.db(`DELETE FROM results WHERE ip='${SPARSE_IP}' AND engine='librespeed'; DELETE FROM ip_addresses WHERE ip='${SPARSE_IP}' AND NOT EXISTS (SELECT 1 FROM results WHERE ip='${SPARSE_IP}');`);
}

async function checkNodeChartReferences(T, id, name, sparse) {
  await T.open('#/nodes/' + id, new RegExp(name));
  const card = T.page.locator('.card').filter({ hasText: 'Speed over time' }).first(), svg = card.locator('.chart svg').first();
  const refs = svg.locator('line[stroke-dasharray="5 4"]');
  const lines = await svg.locator('line').evaluateAll(items => items.map(line => ({ dash: line.getAttribute('stroke-dasharray'), y: line.getAttribute('y1') })));
  T.assert.equal(await refs.count(), 2, `${name} shows separate expected download and upload lines; rendered lines: ${JSON.stringify(lines)}`);
  const y = await refs.evaluateAll(lines => lines.map(line => Number(line.getAttribute('y1'))));
  T.assert.ok(y.every(value => Number.isFinite(value) && value >= 24 && value <= 254), `${name} reference lines stay within the chart plot: ${y}`);
  if (sparse) await T.see('Not enough data yet', card);
  else T.assert.equal(await card.getByText(/Not enough data yet/).count(), 0, `${name} has enough data for a trend`);
}

export function nodesSuite(T, ctx) {
  const { page, assert } = T;
  const { stack, startAgent } = ctx;
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
        for (const expected of ['A download trend', 'VM1: 0', 'VM2: 1', 'VM3: 2']) assert.ok(statsText.includes(expected), `Statistics sparse state includes ${expected}: ${statsText}`);
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
      await startAgent(stack, { name: 'E2E-1', token });
      await T.see('E2E-1 is online', dialog, 90000);
      await dialog.getByText(/First result: .* down, .* up, .* ping\./).waitFor({ timeout: 90000 });
      await T.press('Open node', dialog);
      await page.getByRole('heading', { name: /E2E-1/, level: 1 }).waitFor();

      await T.press('Run test');
      const run = T.dialog('Running speed test');
      await run.waitFor();
      await run.getByText(/Mbps ↓|Gbps ↓|kbps ↓/).waitFor({ timeout: 90000 });
      await T.press('Close', run.locator('.modal-foot'));
      const stored = T.db("select count(*) from results r join nodes n on n.id=r.node_id where n.name='E2E-1' and r.manual");
      assert.ok(Number(stored) >= 1, 'the manual result is stored on the center: ' + stored);
      await T.open('#/history', 'Results');
      await T.fill('Search results', 'E2E-1');
      await page.getByRole('row').nth(1).waitFor();
      // The same line cannot be enrolled twice with the same one-time key.
      const again = await page.request.post(stack.origin + '/api/agent/enroll', { data: { token, name: 'E2E-copy', agent: 'x' } });
      assert.ok(again.status() >= 400, 'an enrolment key works once, got ' + again.status());
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
      assert.equal(T.db("select enabled from nodes where name='VM4'"), 'f');
      await T.eventually(async () => await page.getByRole('button', { name: 'Run test' }).isDisabled(), 'a paused node cannot be tested');
      await T.press('Resume');
      await T.toast('resumed').waitFor();
      await T.settled();
      assert.equal(T.db("select enabled from nodes where name='VM4'"), 't');
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

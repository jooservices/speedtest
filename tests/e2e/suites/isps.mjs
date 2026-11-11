// ISP page, filters and plans (JOOST-74).
const VIETTEL_ISP = '22000000-0000-4000-8000-000000000001';
const VNPT_ISP = '22000000-0000-4000-8000-000000000002';
const E2E_PLAN = '23000000-0000-4000-8000-000000000001';

export function ispsSuite(T) {
  const { page, assert } = T;
  return {
    async listAndCompare() {
      await T.open('#/isps', 'ISPs');
      await T.see('Providers inferred from ASN');
      const list = await T.api('/api/isps?range=30d');
      T.assert(list.status === 200, 'ISP list returns 200');
      T.assert(Array.isArray(list.body.data) && list.body.data.length >= 2, 'deterministic fixtures provide multiple ISP rows');
      const first = list.body.data[0];
      T.assert(first.asn && first.name, 'ISP row has ASN and name');
      await T.choose(page.locator('#topbar select').first(), '6h');
      await T.settled();
      const uiTrend = page.waitForResponse(response => response.url().includes(`/api/isps/${first.id}/trend`));
      await page.getByRole('button', { name: `Show trend for ${first.name} (AS${first.asn})` }).click();
      T.assert((await uiTrend).status() === 200, 'selecting an ISP loads its trend through the UI');
      await T.settled();
      const trend = await T.api(`/api/isps/${first.id}/trend?range=6h`);
      T.assert(trend.status === 200 && trend.body.data.length > 1, 'six-hour trend has hourly buckets');
      const started = Date.now();
      T.assert(trend.body.data.every(point => Date.parse(point.ts) >= started - 6 * 60 * 60 * 1000 - 10000 && Date.parse(point.ts) <= Date.now()), 'six-hour trend timestamps stay inside the rolling window');
      const chart = page.locator(`#view svg[aria-label="${first.name} download trend"]`);
      await chart.waitFor();
      const positions = await chart.evaluate(svg => ({
        width: Number(svg.getAttribute('viewBox').split(' ')[2]),
        x: [...svg.querySelectorAll('circle')].map(circle => Number(circle.getAttribute('cx')))
      }));
      assert.ok(positions.x.length > 1 && positions.x.every(x => x >= 52 && x <= positions.width - 14), 'every trend marker is plotted inside the chart window');
      await page.getByRole('checkbox', { name: `Compare ${first.name}` }).check();
      const second = list.body.data.find(row => row.id !== first.id);
      await page.getByRole('checkbox', { name: `Compare ${second.name}` }).check();
      const comparison = page.locator('#view .grid-2').getByRole('table');
      await comparison.getByRole('cell', { name: `${first.name} (AS${first.asn})`, exact: true }).waitFor();
      await comparison.getByRole('cell', { name: `${second.name} (AS${second.asn})`, exact: true }).waitFor();
      const ids = [first.id, second.id].join(',');
      const cmp = await T.api(`/api/isps/compare?range=30d&isps=${ids}`);
      T.assert(cmp.status === 200 && cmp.body.data.length === 2, 'ISP comparison returns both requested providers');
      const filtered = await T.api(`/api/results?isps=${first.id}&range=30d`);
      T.assert(filtered.status === 200 && filtered.body.data.length > 0 && filtered.body.data.every(row => row.ispId === first.id), 'results ISP predicate returns matching rows');
    },

    async filtersStayPageScopedAndClear() {
      const { data: isps } = (await T.api('/api/isps?range=7d')).body;
      const viettel = isps.find(row => row.id === VIETTEL_ISP);
      const vnpt = isps.find(row => row.id === VNPT_ISP);
      assert.ok(viettel && vnpt, 'fixtures contain Viettel and VNPT');
      await T.open('#/history', 'Results');
      await T.settled();
      const resultISP = page.getByLabel('ISP', { exact: true });
      await resultISP.selectOption(viettel.id);
      const rows = page.locator('#view tbody tr');
      await rows.first().waitFor();
      const resultRows = await rows.allInnerTexts();
      assert.ok(resultRows.length > 0 && resultRows.every(text => !/VM3|VM4/.test(text)), 'Results shows only matching ISP nodes');
      await assertFullExportKeepsAllISPs(T, assert);
      await T.open('#/', 'Dashboard');
      await T.settled();
      assert.equal(await page.locator('#view .node-card').count(), 6, 'dashboard still shows all five nodes and the add-node card');
      assert.ok(await page.evaluate(() => new Set(ST.state.results.map(row => row.ispId).filter(Boolean)).size) >= 3, 'shared results remain unfiltered for dashboard charts');
      await T.open('#/network', 'IP & location');
      await T.settled();
      assert.ok(await page.locator('#view .map-pin-wrap').count() >= 3, 'the map keeps locations across the seeded ISPs');
      await T.open('#/settings', 'Settings');
      await T.tab('Data');
      await assertFullExportKeepsAllISPs(T, assert);

      await T.open('#/history', 'Results');
      await resultISP.selectOption(viettel.id);
      await page.locator('.legend button', { hasText: 'VM1' }).click();
      await page.locator('.legend button', { hasText: 'VM2' }).click();
      await T.see('0 results');
      await T.press('Clear filters');
      assert.equal(await page.getByLabel('ISP', { exact: true }).inputValue(), '', 'Clear filters resets the remote ISP selection');
      assert.ok(await rows.count() > 0, 'Clear filters restores results from every ISP');

      await T.open('#/stats', 'Statistics');
      const filteredSummary = page.waitForResponse(response => {
        const url = new URL(response.url());
        return url.pathname === '/api/stats/summary' && url.searchParams.get('isps') === vnpt.id;
      });
      await page.getByLabel('ISP', { exact: true }).selectOption(vnpt.id);
      const summary = (await (await filteredSummary).json()).data;
      const vm1 = summary.find(row => row.nodeId === '10000000-0000-4000-8000-000000000001');
      const vm3 = summary.find(row => row.nodeId === '10000000-0000-4000-8000-000000000003');
      assert.equal(vm1.current.count, 0, 'Statistics excludes the nonmatching ISP');
      assert.ok(vm3.current.count > 0, 'Statistics includes matching ISP results');

      await T.open('#/alerts', 'Alerts');
      const alertISP = page.getByLabel('ISP', { exact: true });
      await alertISP.selectOption(viettel.id);
      await T.see('All clear');
      await alertISP.selectOption(vnpt.id);
      await T.see('VM3 download is slow');
      assert.equal(await page.getByText('VM4 tests are failing').count(), 1, 'Alerts shows both events for the selected ISP');
      await alertISP.selectOption(viettel.id);
      await T.see('All clear');
      assert.equal(await page.evaluate(() => ST.events().filter(event => !event.acked).length), 2, 'the app-wide alert snapshot remains complete');
      await T.open('#/', 'Dashboard');
      await T.see('VM3 download is slow');
      await T.see('VM4 tests are failing');
      await assertFullExportKeepsAllISPs(T, assert);
    },

    async planDefaultsPreserveOverrides() {
      await T.open('#/nodes', 'Nodes');
      await T.row('VM1').click();
      await page.getByRole('heading', { name: /VM1/ }).waitFor();
      await T.tab('Settings');
      await page.getByLabel('ISP plan', { exact: true }).selectOption(E2E_PLAN);
      await T.press('Save changes');
      await T.toast('Settings saved').waitFor();
      assert.equal(T.db(`select expected_down_mbps::int||'/'||expected_up_mbps::int||':'||expected_down_isp_default||':'||expected_up_isp_default from nodes where id='10000000-0000-4000-8000-000000000001'`), '500/100:true:true', 'plan assignment saves both values as defaults through the node API');

      await updatePlan(page, E2E_PLAN, 600, 120);
      await page.reload();
      await page.getByRole('heading', { name: /VM1/ }).waitFor();
      await T.tab('Settings');
      assert.equal(await page.getByLabel('Expected download').inputValue(), '600');
      assert.equal(await page.getByLabel('Expected upload').inputValue(), '120');
      await page.getByLabel('Expected download').fill('600');
      await page.getByLabel('Expected upload').fill('80');
      await T.press('Save changes');
      await T.toast('Settings saved').waitFor();
      assert.equal(T.db(`select expected_down_mbps::int||'/'||expected_up_mbps::int||':'||expected_down_isp_default||':'||expected_up_isp_default from nodes where id='10000000-0000-4000-8000-000000000001'`), '600/80:false:false', 'equal and different explicit speeds both become overrides');

      await updatePlan(page, E2E_PLAN, 700, 140);
      const preserved = T.db(`select expected_down_mbps::int||'/'||expected_up_mbps::int from nodes where id='10000000-0000-4000-8000-000000000001'`);
      assert.equal(preserved, '600/80', 'plan edit preserves explicit values equal to or different from the previous advertised speeds');

      await updatePlan(page, E2E_PLAN, 500, 100);
      await page.reload();
      await page.getByRole('heading', { name: /VM1/ }).waitFor();
      await T.tab('Settings');
      await page.getByLabel('ISP plan', { exact: true }).selectOption('');
      await page.getByLabel('Expected download').fill('500');
      await page.getByLabel('Expected upload').fill('100');
      await T.press('Save changes');
      await T.toast('Settings saved').waitFor();
      assert.equal(T.db(`select expected_down_mbps::int||'/'||expected_up_mbps::int||':'||COALESCE(isp_plan_id::text,'none') from nodes where id='10000000-0000-4000-8000-000000000001'`), '500/100:none', 'plan scenario restores the seeded node state');
    },
    async monthlyReport() {
      await T.open('#/isps', 'ISPs');
      await T.see('Monthly report');
      const month = new Date(Date.UTC(new Date().getUTCFullYear(), new Date().getUTCMonth() - 1, 1)).toISOString().slice(0, 7);
      const json = await T.api(`/api/reports/monthly?month=${month}`);
      T.assert(json.status === 200 && Array.isArray(json.body.data.rows), 'report JSON returns rows');
      T.assert(json.body.data.month === month && json.body.data.partial === false, 'report covers the requested whole month');
      const csv = await T.api(`/api/reports/monthly?month=${month}&format=csv`);
      T.assert(csv.status === 200 && csv.text.startsWith('month,isp,asn,node,plan'), 'report CSV has the header');
      const pdf = await T.api(`/api/reports/monthly?month=${month}&format=pdf`);
      T.assert(pdf.status === 200 && pdf.text.startsWith('%PDF-1.4'), 'report PDF is a PDF');
      for (const bad of ['month=2999-01', 'month=2026-13', `month=${month}&format=xml`, `month=${month}&engine=nope`]) {
        const res = await T.api(`/api/reports/monthly?${bad}`);
        T.assert(res.status === 422, `report rejects ${bad}`);
        T.expectRefusal(422);
      }
      const csvLink = await T.page.getAttribute('[data-report-download="csv"]', 'href');
      const pdfLink = await T.page.getAttribute('[data-report-download="pdf"]', 'href');
      T.assert(csvLink.includes('format=csv') && pdfLink.includes('format=pdf'), 'download links point at the report API');
      const oldest = await T.page.locator('select.report-month option').last().getAttribute('value');
      await T.choose(T.page.locator('select.report-month'), oldest);
      await T.see('No data for this month');
      const file = await T.download(() => T.page.click('[data-report-download="csv"]'));
      T.assert(file.startsWith('month,isp,asn,node,plan'), 'the CSV download button saves the report');
    },
  };
}

async function assertFullExportKeepsAllISPs(T, assert) {
  await T.open('#/settings', 'Settings');
  await T.tab('Data');
  const body = JSON.parse(await T.download(() => T.press('JSON')));
  const ids = new Set(body.map(row => row.ispId).filter(Boolean));
  assert.ok(ids.size >= 3, 'full JSON export still includes results from all seeded ISPs');
}

async function updatePlan(page, planID, down, up) {
  await page.evaluate(async input => {
    await ST.api.request('/api/isps/22000000-0000-4000-8000-000000000001/plans/' + input.id, {
      method: 'PUT', body: { name: 'E2E Fiber', advertisedDown: input.down, advertisedUp: input.up }
    });
  }, { id: planID, down, up });
}

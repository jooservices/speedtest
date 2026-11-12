export function resultsSuite(T) {
  const { page, assert } = T;
  return {
    async filterOpenExport() {
      await T.open('#/history', 'Results');
      await T.see('1,680 results');
      await page.getByRole('columnheader', { name: 'Data used' }).waitFor();
      const status = T.selectWith('Any status');
      await status.selectOption('failed');
      const failed = await page.locator('text=/\\d[\\d,]* results?/').first().innerText();
      assert.ok(/^(\d+) results?$/.test(failed.replace(/,/g, '')) && Number.parseInt(failed, 10) < 1680, `status filter: ${failed}`);
      await page.getByRole('row').nth(1).click();
      await T.see('— down · — up · — total', page);
      await T.dismiss();
      await status.selectOption('all');
      await T.fill('Search results', 'VM3');
      await T.see('336 results');
      await T.fill('Minimum download', '100000');
      await T.see('0 results');
      await T.fill('Minimum download', '');
      await T.fill('Search results', '');
      await page.getByRole('row').nth(1).click();
      await page.getByRole('dialog').or(page.locator('.drawer')).first().waitFor();
      await T.see('Data used');
      await T.dismiss();
      const text = await T.download(() => T.press('Export CSV'));
      assert.ok(text.startsWith('time,node,status,download_mbps'), 'CSV header');
      assert.ok(text.startsWith('time,node,status,download_mbps,upload_mbps,ping_ms,jitter_ms,packet_loss_pct,bytes_down,bytes_up,bytes_total'), 'byte usage columns');
      assert.ok(text.split('\n').length > 1000, 'the export holds the rows');
    },

    // Column sorting, paging, the node chips and the maximum download filter.
    async sortPageAndChips() {
      await T.open('#/history', 'Results');
      await T.settled();
      const firstCell = () => page.getByRole('row').nth(1).innerText();
      const newest = await firstCell();
      await page.getByRole('columnheader', { name: /Time/ }).click();
      assert.notEqual(await firstCell(), newest, 'sorting by time flips the order');
      await page.getByRole('columnheader', { name: /Download/ }).click();
      await T.expectNoLoadError();
      await page.getByRole('button', { name: /Next|›|»/ }).first().click();
      await T.expectNoLoadError();
      await page.getByRole('button', { name: /Previous|‹|«/ }).first().click();
      const chip = page.locator('.legend button', { hasText: 'VM1' });
      await chip.click();
      assert.ok([null, 'false'].includes(await chip.getAttribute('aria-pressed')), 'a node chip hides that node');
      await chip.click();
      assert.notEqual(await chip.getAttribute('aria-pressed'), 'false');
      await T.fill('Maximum download', '1');
      await T.see('0 results');
      await T.fill('Maximum download', '');
    },

    // Failed rows show a short failure-class reason chip next to Failed.
    async failureClassChip() {
      const fixtures = [
        ['73000000-0000-4000-8000-000000000101', 'rate_limited', 'Rate limited'],
        ['73000000-0000-4000-8000-000000000102', 'no_network', 'No network'],
        ['73000000-0000-4000-8000-000000000103', 'server_unreachable', 'Server unreachable'],
        ['73000000-0000-4000-8000-000000000104', 'engine_missing', 'Engine missing'],
        ['73000000-0000-4000-8000-000000000105', 'timeout', 'Timeout'],
        ['73000000-0000-4000-8000-000000000106', 'other', 'Other'],
      ];
      const ids = fixtures.map(([id]) => `'${id}'`).join(',');
      const status = T.selectWith('Any status');
      const search = page.getByRole('searchbox', { name: 'Search results' });
      try {
        T.db(`DELETE FROM results WHERE id IN (${ids});
          INSERT INTO results(id,node_id,ts,status,threads,error,engine,failure_class,bytes_down,bytes_up,duration_seconds)
          SELECT fixture.id,n.id,now()-fixture.age,'failed',4,'JOOST-73 failure class fixture '||fixture.failure_class,
            'ookla',fixture.failure_class,0,0,0
          FROM nodes n CROSS JOIN (VALUES
            ('${fixtures[0][0]}'::uuid,interval '1 minute','${fixtures[0][1]}'),
            ('${fixtures[1][0]}'::uuid,interval '2 minutes','${fixtures[1][1]}'),
            ('${fixtures[2][0]}'::uuid,interval '3 minutes','${fixtures[2][1]}'),
            ('${fixtures[3][0]}'::uuid,interval '4 minutes','${fixtures[3][1]}'),
            ('${fixtures[4][0]}'::uuid,interval '5 minutes','${fixtures[4][1]}'),
            ('${fixtures[5][0]}'::uuid,interval '6 minutes','${fixtures[5][1]}')
          ) fixture(id,age,failure_class) WHERE n.name='VM1'`);
        await T.eventually(() => T.db(`SELECT count(*) FROM results WHERE id IN (${ids})`) === String(fixtures.length), 'all failure-class fixtures are stored');
        await page.reload();
        await T.open('#/history', 'Results');
        await T.settled();
        await status.selectOption('failed');
        await search.fill('JOOST-73 failure class fixture');
        await T.settled();
        const rows = page.locator('table.data tbody tr');
        assert.equal(await rows.count(), fixtures.length, 'the Results filter shows only the deterministic class fixtures');
        for (let index = 0; index < fixtures.length; index++) {
          const chips = rows.nth(index).locator('.chip');
          const labels = (await chips.allTextContents()).map(label => label.trim());
          assert.deepEqual(labels, ['Failed', fixtures[index][2]], `failureClass ${fixtures[index][1]} maps to the exact Results chip`);
        }
      } finally {
        T.db(`DELETE FROM results WHERE id IN (${ids})`);
        await search.fill('').catch(() => {});
        await status.selectOption('all').catch(() => {});
      }
    },
  };
}

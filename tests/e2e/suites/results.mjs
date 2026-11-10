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
  };
}

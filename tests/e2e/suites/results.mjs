export function resultsSuite(T) {
  const { page, assert } = T;
  return {
    async filterOpenExport() {
      await T.open('#/history', 'Results');
      await T.see('1,680 results');
      const status = T.selectWith('Any status');
      await status.selectOption('failed');
      const reason = page.locator('.result-reason').first();
      await reason.waitFor();
      const reasonText = await reason.innerText();
      assert.ok(reasonText.length <= 32 && reasonText.startsWith('The test server'), 'failed rows show a short reason chip: ' + reasonText);
      const failed = await page.locator('text=/\\d[\\d,]* results?/').first().innerText();
      assert.ok(/^(\d+) results?$/.test(failed.replace(/,/g, '')) && Number.parseInt(failed, 10) < 1680, `status filter: ${failed}`);
      await status.selectOption('all');
      await T.fill('Search results', 'VM3');
      await T.see('336 results');
      await T.fill('Minimum download', '100000');
      await T.see('0 results');
      await T.fill('Minimum download', '');
      await T.fill('Search results', '');
      assert.equal(await page.getByRole('table').count(), 1, 'the results list keeps table semantics');
      const firstResult = page.getByRole('row').nth(1), openResult = firstResult.getByRole('button', { name: /Open result details for/ });
      assert.equal(await openResult.count(), 1, 'the result row exposes its primary action in the accessibility tree');
      await openResult.focus();
      await page.keyboard.press('Enter');
      await page.getByRole('dialog').or(page.locator('.drawer')).first().waitFor();
      await T.dismiss();
      const text = await T.download(() => T.press('Export CSV'));
      assert.ok(text.startsWith('time,node,status,download_mbps'), 'CSV header');
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

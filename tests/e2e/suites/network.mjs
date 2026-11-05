export function networkSuite(T) {
  const { page, assert } = T;
  return {
    async mapLookupAndDetails() {
      await T.open('#/network', 'IP & location');
      await page.getByRole('heading', { name: 'Every address seen' }).waitFor();
      await page.locator('.leaflet-container').waitFor();
      const markers = page.locator('.leaflet-marker-icon');
      await markers.first().waitFor();
      assert.ok(await markers.count() >= 1, 'the map shows the node locations');
      await markers.first().hover();
      await page.locator('.leaflet-tooltip').first().waitFor();
      await page.getByRole('link', { name: 'OpenStreetMap' }).waitFor();
      await page.getByRole('cell', { name: '203.0.113.11' }).first().waitFor();
      await T.fill('IP address to look up', 'not an address');
      await T.press('Look up');
      await T.see('Enter a valid IP address');
      await T.fill('IP address to look up', '203.0.113.11');
      const lookup = page.waitForResponse(r => r.url().includes('/api/ip/lookup?ip=203.0.113.11') && r.status() === 200);
      await T.press('Look up');
      const found = await (await lookup).json();
      assert.equal(found.data.city, 'Hanoi', 'the center answered the lookup');
      await page.locator('.card').filter({ hasText: 'Look up an address' }).getByText('Hanoi').first().waitFor();
      await T.fill('IP address to look up', '2001:db8::1');
      await T.press('Look up');
      await T.see('No location is known for this address.');
      await page.getByRole('cell', { name: '203.0.113.11' }).first().click();
      await page.getByRole('heading', { name: 'IP address details' }).waitFor();
      await T.press('Copy', page.locator('.drawer'));
      await T.dismiss();
    },

    // Per-node view, longer history and the node filter.
    async filterAndHistory() {
      await T.open('#/network', 'IP & location');
      await T.settled();
      const filter = T.selectWith('All nodes');
      await filter.selectOption({ index: 1 });
      await T.expectNoLoadError();
      await filter.selectOption('all');
      const more = page.getByRole('button', { name: /^Show \d+ more/ });
      if (await more.count()) { await more.click(); await T.expectNoLoadError(); }
      assert.ok(await page.getByText(/changed address/).count() >= 1, 'the address changes are listed');
    },
  };
}

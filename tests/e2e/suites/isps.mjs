// ISP page and API filters (JOOST-74).
export function ispsSuite(T) {
  return {
    async listAndCompare() {
      await T.open('#/isps', 'ISPs');
      await T.see('Providers inferred from ASN');
      const list = await T.api('/api/isps?range=30d');
      T.assert(list.status === 200, 'ISP list returns 200');
      T.assert(Array.isArray(list.body.data), 'ISP list data is an array');
      if (list.body.data.length) {
        const first = list.body.data[0];
        T.assert(first.asn && first.name, 'ISP row has asn and name');
        const uiTrend = T.page.waitForResponse(response => response.url().includes(`/api/isps/${first.id}/trend`));
        await T.page.getByRole('button', { name: `Show trend for ${first.name} (AS${first.asn})` }).click();
        T.assert((await uiTrend).status() === 200, 'selecting an ISP loads its trend through the UI');
        await T.settled();
        await T.page.locator(`#view svg[aria-label="${first.name} download trend"]`).waitFor();
        await T.page.getByRole('checkbox', { name: `Compare ${first.name}` }).check();
        const trend = await T.api(`/api/isps/${first.id}/trend?range=30d`);
        T.assert(trend.status === 200, 'ISP trend returns 200');
        if (list.body.data.length > 1) {
          const second = list.body.data[1];
          await T.page.getByRole('checkbox', { name: `Compare ${second.name}` }).check();
          const comparison = T.page.getByRole('table').last();
          await comparison.getByRole('cell', { name: `${first.name} (AS${first.asn})`, exact: true }).waitFor();
          await comparison.getByRole('cell', { name: `${second.name} (AS${second.asn})`, exact: true }).waitFor();
          const ids = list.body.data.slice(0, 2).map((r) => r.id).join(',');
          const cmp = await T.api(`/api/isps/compare?range=30d&isps=${ids}`);
          T.assert(cmp.status === 200 && cmp.body.data.length >= 1, 'ISP compare returns rows');
        }
        const filtered = await T.api(`/api/results?isps=${first.id}&range=30d`);
        T.assert(filtered.status === 200, 'results accept isps filter');
      }
      await T.see('Trend');
      await T.see('Comparison');
    },
  };
}

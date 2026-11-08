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
        const trend = await T.api(`/api/isps/${first.id}/trend?range=30d`);
        T.assert(trend.status === 200, 'ISP trend returns 200');
        if (list.body.data.length > 1) {
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

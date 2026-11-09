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

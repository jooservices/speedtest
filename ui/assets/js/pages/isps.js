/* ISP list, comparison and trend. */
(function () {
  'use strict';
  var ST = window.ST, U = ST.ui, h = U.h, F = ST.fmt;
  var state = { selected: null, compare: [], remote: null, trendRevision: 0, trendController: null };

  function mean(a) { return a.length ? a.reduce(function (s, v) { return s + v; }, 0) / a.length : null; }
  function p95(a) {
    if (!a.length) return null;
    var s = a.slice().sort(function (x, y) { return x - y; });
    return ST.stats.pct(s, 0.95);
  }

  function ispsFromResults() {
    var map = {}, list = [];
    ST.results({ status: 'all', from: Date.now() - ST.ranges[ST.settings().range] }).forEach(function (r) {
      var g = ST.geo(r.ip);
      if (!g || !g.asn) return;
      var key = 'as' + g.asn;
      var row = map[key];
      if (!row) {
        row = map[key] = {
          id: key, asn: g.asn, name: g.isp || g.asnOrg || ('AS' + g.asn), organisation: g.asnOrg || g.isp || '',
          nodes: {}, downs: [], pings: [], ok: 0, total: 0, expected: []
        };
        list.push(row);
      }
      row.total++;
      if (r.status === 'ok' || r.status === 'degraded') {
        row.ok++;
        if (r.down != null) row.downs.push(r.down);
        if (r.ping != null) row.pings.push(r.ping);
        var n = ST.node(r.nodeId);
        if (n) {
          row.nodes[n.id] = true;
          if (n.expectedDown > 0 && r.down != null) row.expected.push(r.down / n.expectedDown * 100);
        }
      }
    });
    return list.map(function (row) {
      return {
        id: row.id, asn: row.asn, name: row.name, organisation: row.organisation,
        nodeCount: Object.keys(row.nodes).length, avgDown: mean(row.downs), p95Down: p95(row.downs),
        pctExpected: mean(row.expected), successRate: row.total ? row.ok / row.total : null, avgPing: mean(row.pings)
      };
    }).sort(function (a, b) { return (b.nodeCount - a.nodeCount) || a.name.localeCompare(b.name); });
  }

  function drawTrend(host, row) {
    state.trendRevision++;
    var revision = state.trendRevision;
    if (state.trendController) state.trendController.abort();
    state.trendController = null;
    U.clear(host);
    if (!row) {
      host.appendChild(U.empty('Select an ISP', 'Choose a row to see its download trend for the current range.'));
      return;
    }
    function paint(points) {
      if (revision !== state.trendRevision || !host.isConnected || !state.selected || state.selected.id !== row.id) return;
      if (!points.length) {
        host.appendChild(U.empty('No samples', 'No successful tests for this ISP in the selected range.'));
        return;
      }
      ST.charts.line(host, {
        series: [{ name: row.name, color: 'var(--accent)', points: points }],
        x: { type: 'time', min: Date.now() - ST.ranges[ST.settings().range], max: Date.now() },
        y: { min: 0, format: F.axisSpeed, unit: F.axisUnit() }, height: 220, label: row.name + ' download trend'
      });
    }
    if (!ST.demo && row.id.indexOf('as') !== 0) {
      var trendPath = '/api/isps/' + encodeURIComponent(row.id) + '/trend?range=' + encodeURIComponent(ST.settings().range) + ST.engineQuery();
      state.trendController = new AbortController();
      ST.api.request(trendPath, { signal: state.trendController.signal }).then(function (res) {
        if (revision !== state.trendRevision || !host.isConnected || !state.selected || state.selected.id !== row.id) return;
        U.clear(host);
        paint((res.data || []).filter(function (p) { return p.down != null; }).map(function (p) { return [Date.parse(p.ts), p.down]; }));
      }).catch(function (err) {
        if (revision !== state.trendRevision || err.name === 'AbortError' || !host.isConnected) return;
        U.clear(host);
        host.appendChild(U.empty('Could not load trend', err.message || 'Try again.'));
      });
      host.appendChild(h('div', { class: 'muted' }, 'Loading trend…'));
      return;
    }
    var from = Date.now() - ST.ranges[ST.settings().range];
    var buckets = {}, order = [], hourly = ST.ranges[ST.settings().range] <= 24 * ST.HOUR;
    ST.results({ from: from, status: 'all' }).forEach(function (r) {
      var g = ST.geo(r.ip);
      if (!g || g.asn !== row.asn || r.status === 'failed' || r.down == null) return;
      var key = hourly ? Math.floor(r.ts / ST.HOUR) : F.parts(r.ts).key;
      if (!buckets[key]) { buckets[key] = { t: 0, n: 0, v: 0 }; order.push(key); }
      buckets[key].t += r.ts; buckets[key].n++; buckets[key].v += r.down;
    });
    paint(order.map(function (k) { return [buckets[k].t / buckets[k].n, buckets[k].v / buckets[k].n]; }));
  }

  function render(root, list) {
    var trendHost = h('div', { class: 'card card-pad' });
    var selectedStatus = h('div', { role: 'status', 'aria-live': 'polite', class: 'muted mb' });
    var compareHost = h('div', { class: 'card card-pad' });
    state.selected = list.filter(function (row) { return state.selected && row.id === state.selected.id; })[0] || null;
    function selectISP(row) { state.selected = row; selectedStatus.textContent = 'Selected ISP: ' + row.name + ' (AS' + row.asn + ')'; drawTrend(trendHost, row); }
    function redrawCompare() {
      U.clear(compareHost);
      var rows = list.filter(function (r) { return state.compare.indexOf(r.id) >= 0; });
      if (rows.length < 2) {
        compareHost.appendChild(U.empty('Compare ISPs', 'Tick at least two ISPs in the list to compare averages in this period.'));
        return;
      }
      compareHost.appendChild(U.table({
        columns: [
          { key: 'name', label: 'ISP', render: function (r) { return r.name + ' (AS' + r.asn + ')'; } },
          { key: 'nodeCount', label: 'Nodes', num: true },
          { key: 'avgDown', label: 'Avg down', num: true, render: function (r) { return F.speed(r.avgDown); } },
          { key: 'p95Down', label: 'p95', num: true, render: function (r) { return F.speed(r.p95Down); } },
          { key: 'pctExpected', label: '% expected', num: true, render: function (r) { return r.pctExpected == null ? '—' : F.num(r.pctExpected, 0) + '%'; } },
          { key: 'successRate', label: 'Success', num: true, render: function (r) { return r.successRate == null ? '—' : F.num(r.successRate * 100, 0) + '%'; } },
          { key: 'avgPing', label: 'Latency', num: true, render: function (r) { return F.ms(r.avgPing); } }
        ],
        rows: rows
      }));
    }
    U.clear(root);
    root.appendChild(ST.engineSelector());
    root.appendChild(U.pageHead('ISPs', 'Providers inferred from ASN data, with speed and reliability in the selected range.'));
    root.appendChild(h('div', { class: 'card' }, U.table({
      columns: [
        { key: 'pick', label: 'Compare', render: function (r) {
          var on = state.compare.indexOf(r.id) >= 0;
          return h('input', { type: 'checkbox', checked: on || null, 'aria-label': 'Compare ' + r.name, onChange: function (e) {
            if (e.target.checked) state.compare.push(r.id); else state.compare = state.compare.filter(function (id) { return id !== r.id; });
            redrawCompare();
          } });
        } },
        { key: 'name', label: 'ISP', render: function (r) { return h('button', { type: 'button', class: 'btn sm', 'aria-label': 'Show trend for ' + r.name + ' (AS' + r.asn + ')', 'aria-pressed': !!(state.selected && state.selected.id === r.id), onClick: function () { selectISP(r); } }, r.name + ' (AS' + r.asn + ')'); } },
        { key: 'nodeCount', label: 'Nodes', num: true },
        { key: 'avgDown', label: 'Average', num: true, render: function (r) { return F.speed(r.avgDown); } },
        { key: 'p95Down', label: 'p95', num: true, render: function (r) { return F.speed(r.p95Down); } },
        { key: 'pctExpected', label: '% of expected', num: true, render: function (r) { return r.pctExpected == null ? '—' : F.num(r.pctExpected, 0) + '%'; } },
        { key: 'successRate', label: 'Success', num: true, render: function (r) { return r.successRate == null ? '—' : F.num(r.successRate * 100, 0) + '%'; } },
        { key: 'avgPing', label: 'Latency', num: true, render: function (r) { return F.ms(r.avgPing); } }
      ],
      rows: list,
      empty: U.empty('No ISP data yet', 'Run tests so public IPs get ASN lookups; ISPs appear automatically.')
    })));
    root.appendChild(h('div', { class: 'grid-2', style: 'margin-top:16px' },
      h('div', null, h('h2', { class: 'section-title' }, 'Trend'), selectedStatus, trendHost),
      h('div', null, h('h2', { class: 'section-title' }, 'Comparison'), compareHost)));
    if (state.selected) selectedStatus.textContent = 'Selected ISP: ' + state.selected.name + ' (AS' + state.selected.asn + ')';
    drawTrend(trendHost, state.selected);
    redrawCompare();
    if (!ST.demo) root.appendChild(reportCard());
  }


  /* ---------- Monthly report (JOOST-79) ---------- */
  function lastMonths(count) {
    var out = [], d = new Date(), y = d.getUTCFullYear(), m = d.getUTCMonth();
    for (var i = 0; i < count; i++) {
      out.push(y + '-' + (m + 1 < 10 ? '0' : '') + (m + 1));
      m -= 1;
      if (m < 0) { m = 11; y -= 1; }
    }
    return out;
  }

  function reportColumns() {
    return [
      { key: 'isp', label: 'ISP', render: function (r) { return h('span', null, r.isp, h('span', { class: 'muted' }, ' AS' + r.asn)); } },
      { key: 'node', label: 'Node' },
      { key: 'plan', label: 'Plan', render: function (r) { return r.plan || '—'; } },
      { key: 'avgDownMbps', label: 'Avg down', num: true, render: function (r) { return F.speed(r.avgDownMbps); } },
      { key: 'p95DownMbps', label: 'p95', num: true, render: function (r) { return F.speed(r.p95DownMbps); } },
      { key: 'deliveredPercent', label: 'Delivered', num: true, render: function (r) { return r.deliveredPercent == null ? '—' : F.num(r.deliveredPercent, 0) + '%'; } },
      { key: 'worstHoursUtc', label: 'Worst hours (UTC)', render: function (r) { return r.worstHoursUtc.length ? r.worstHoursUtc.join(', ') : '—'; } },
      { key: 'failures', label: 'Failures', num: true },
      { key: 'downtimeMinutes', label: 'Downtime (min)', num: true },
      { key: 'ipChanges', label: 'IP changes', num: true }
    ];
  }

  function reportCard() {
    var months = lastMonths(12), month = months[1], host = h('div', { 'data-report-preview': '' });
    var csv = h('a', { class: 'btn', download: '', 'data-report-download': 'csv' }, 'Download CSV');
    var pdf = h('a', { class: 'btn', download: '', 'data-report-download': 'pdf' }, 'Download PDF');
    function path(format) { return '/api/reports/monthly?month=' + encodeURIComponent(month) + '&format=' + format + ST.engineQuery(); }
    function load() {
      csv.setAttribute('href', path('csv'));
      pdf.setAttribute('href', path('pdf'));
      U.clear(host);
      host.appendChild(h('div', { class: 'muted' }, 'Loading report…'));
      ST.api.request(path('json')).then(function (res) {
        var data = res.data || { rows: [] };
        U.clear(host);
        if (data.partial) host.appendChild(h('p', { class: 'muted' }, 'This month is not over yet; figures cover the days so far.'));
        host.appendChild(U.table({ columns: reportColumns(), rows: data.rows, empty: U.empty('No data for this month', 'Pick another month, or wait until tests with a known ISP have run.') }));
      }).catch(function (err) {
        U.clear(host);
        host.appendChild(U.empty('Could not load the report', err.message || 'Retry later.', U.btn('Retry', { onClick: load })));
      });
    }
    var picker = U.select(months, month, function (v) { month = v; load(); }, 'report-month');
    load();
    return h('div', { class: 'card', style: 'margin-top:16px' },
      h('h2', { class: 'section-title' }, 'Monthly report'),
      h('p', { class: 'muted' }, 'Delivered against advertised speed, p95, worst hours, failures, downtime and IP changes per ISP and node. Set SPEEDTEST_MONTHLY_REPORT_EMAIL=true to e-mail the previous month on the first of each month.'),
      h('div', { class: 'row', style: 'gap:8px;align-items:center;margin-bottom:12px' }, U.field('Month', picker), csv, pdf),
      host);
  }

  ST.pages.isps = function (root) {
    if (ST.demo) {
      render(root, ispsFromResults());
      return;
    }
    root.appendChild(U.pageHead('ISPs', 'Providers inferred from ASN data, with speed and reliability in the selected range.'));
    root.appendChild(h('div', { class: 'muted' }, 'Loading ISPs…'));
    var listPath = '/api/isps?range=' + encodeURIComponent(ST.settings().range) + ST.engineQuery();
    ST.api.request(listPath).then(function (res) {
      render(root, res.data || []);
    }).catch(function (err) {
      U.clear(root);
      root.appendChild(U.pageHead('ISPs', 'Providers inferred from ASN data, with speed and reliability in the selected range.'));
      root.appendChild(U.empty('Could not load ISPs', err.message || 'Retry from the menu.', U.btn('Retry', { onClick: function () { ST.refresh(); } })));
    });
  };
})();

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

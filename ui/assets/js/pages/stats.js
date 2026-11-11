/* Statistics: summary table, trend, time-of-day, weekly heatmap and distribution. */
(function () {
  'use strict';
  var ST = window.ST, U = ST.ui, h = U.h, F = ST.fmt, S = ST.stats;
  var q = { nodes: null, metric: 'down', heatNode: null, ispId: '', sum: false, table: false };
  var DAYS = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'];

  function fmtM(m, v) { return v == null ? '—' : F.metric(m, v); }
  function successCell(summary) {
    var note = summary.rateLimited ? h('span', { class: 'hint' }, F.num(summary.rateLimited) + ' rate-limited excluded') : null;
    return h('span', { class: 'stack' }, h('span', null, F.pct(summary.successRate, 1)), note);
  }
  function axisFmt(m) { return ST.metrics[m].unit === 'speed' ? F.axisSpeed : function (v) { return F.num(v, m === 'loss' ? 1 : 0); }; }
  function axisUnit(m) { return ST.metrics[m].unit === 'speed' ? F.axisUnit() : ST.metrics[m].unit === 'ms' ? 'ms' : '%'; }


  /* ---- data: computed in the browser for the demo, fetched from /api/stats/* for the real center ---- */
  var remote = { key: null, data: null, pending: null, error: null };
  var BINS = 12;

  function binsOf(values) {
    if (values.length < 3) return { n: values.length, bins: [] };
    var lo = Math.min.apply(null, values), hi = Math.max.apply(null, values), w = (hi - lo) / BINS || 1, counts = [];
    for (var i = 0; i < BINS; i++) counts.push(0);
    values.forEach(function (x) { counts[Math.min(BINS - 1, Math.floor((x - lo) / w))]++; });
    return { n: values.length, bins: counts.map(function (c, i) { return { from: lo + i * w, to: lo + (i + 1) * w, count: c, percent: c / values.length * 100 }; }) };
  }

  function localSummary(node, from, to) {
    var rows = ST.results({ nodeIds: [node.id], from: from, to: to, ispId: q.ispId }), good = rows.filter(function (r) { return r.status !== 'failed'; });
    var ips = {}; rows.forEach(function (r) { ips[r.ip] = true; });
    var summary = { count: rows.length, ok: good.length, failed: rows.length - good.length, successRate: rows.length ? good.length / rows.length * 100 : null };
    ['down', 'up', 'ping', 'jitter', 'loss'].forEach(function (key) { summary[key] = S.summary(good.map(function (r) { return r[key]; })); });
    summary.ipCount = Object.keys(ips).length;
    summary.downPct = node.expectedDown > 0 && summary.down.mean != null ? summary.down.mean / node.expectedDown * 100 : null;
    summary.upPct = node.expectedUp > 0 && summary.up.mean != null ? summary.up.mean / node.expectedUp * 100 : null;
    summary.stability = summary.down.mean ? Math.max(0, Math.min(100, 100 - summary.down.std / summary.down.mean * 220)) : null;
    return summary;
  }

  function emptySummary() {
    var empty = { n: 0, mean: null, median: null, min: null, max: null, p5: null, p95: null, std: null };
    return { count: 0, ok: 0, failed: 0, successRate: null, down: empty, up: empty, ping: empty, jitter: empty, loss: empty, ipCount: 0, downPct: null, upPct: null, stability: null };
  }

  function localData(sel, m, from, now, hourly, showSum, heatNode) {
    var out = { summary: {}, hours: {}, trend: [], heat: null, dist: {} };
    sel.forEach(function (n) {
      out.summary[n.id] = localSummary(n, from, now);
      var buckets = []; for (var i = 0; i < 24; i++) buckets.push([]);
      ST.results({ nodeIds: [n.id], from: from, status: 'all', ispId: q.ispId }).forEach(function (r) { var v = S.metricValue(r, m); if (v != null) buckets[F.parts(r.ts).hour].push(v); });
      out.hours[n.id] = buckets.map(function (b) { return b.length ? S.mean(b) : null; });
      var map = {}, order = [], values = [];
      ST.results({ nodeIds: [n.id], from: from, status: 'all', ispId: q.ispId }).forEach(function (r) {
        var v = S.metricValue(r, m); if (v == null) return;
        values.push(v);
        var p = F.parts(r.ts), key = hourly ? p.key + 'T' + p.hour : p.key, e = map[key];
        if (!e) { e = map[key] = { t: 0, n: 0, v: 0 }; order.push(key); }
        e.t += r.ts; e.n++; e.v += v;
      });
      out.trend.push({ id: n.id, name: n.name, color: ST.nodeColor(n), points: order.map(function (k) { var e = map[k]; return [e.t / e.n, e.v / e.n, k]; }) });
      out.dist[n.id] = binsOf(values);
    });
    if (showSum) {
      var sumMap = {};
      out.trend.forEach(function (s) { s.points.forEach(function (p) { var e = sumMap[p[2]] || (sumMap[p[2]] = { t: p[0], v: 0 }); e.v += p[1]; }); });
      out.trend.push({ id: 'sum', name: 'Combined total', color: 'var(--text)', dashed: true, points: Object.keys(sumMap).map(function (k) { return [sumMap[k].t, sumMap[k].v]; }).sort(function (a, b) { return a[0] - b[0]; }) });
    }
    var vals = [], counts = []; for (var d = 0; d < 7; d++) { vals.push([]); counts.push([]); for (var hr = 0; hr < 24; hr++) { vals[d].push([]); counts[d].push(0); } }
    ST.results({ nodeIds: [heatNode.id], from: now - 30 * ST.DAY, status: 'all', ispId: q.ispId }).forEach(function (r) { var p = F.parts(r.ts), v = S.metricValue(r, m); if (v != null) vals[p.wd][p.hour].push(v); });
    out.heat = { matrix: vals.map(function (row, di) { return row.map(function (c, hi) { counts[di][hi] = c.length; return c.length ? S.mean(c) : null; }); }), counts: counts };
    return out;
  }

  // The center computes every figure; the browser only draws them. Results are cached per request so a redraw
  // (theme, resize, refresh) does not fetch again.
  function remoteData(sel, m, range, hourly, showSum, heatNode) {
    var ids = sel.map(function (n) { return n.id; }).join(','), zone = ST.timezone();
    var key = [ids, m, range, hourly, showSum, heatNode.id, zone, ST.selectedEngine, q.ispId].join('|');
    if (remote.key === key && Date.now() - remote.at < 60000) return remote.error ? { error: remote.error } : remote.data;
    if (remote.pending !== key || remote.key === key) {
      remote.pending = key;
      var base = 'range=' + encodeURIComponent(range) + '&metric=' + m + '&timezone=' + encodeURIComponent(zone) + '&nodes=' + encodeURIComponent(ids) + (q.ispId ? '&isps=' + encodeURIComponent(q.ispId) : '') + ST.engineQuery();
      Promise.all([
        ST.api.request('/api/stats/summary?range=' + encodeURIComponent(range) + '&timezone=' + encodeURIComponent(zone) + '&nodes=' + encodeURIComponent(ids) + (q.ispId ? '&isps=' + encodeURIComponent(q.ispId) : '') + ST.engineQuery()),
        ST.api.request('/api/stats/trend?' + base + '&bucket=' + (hourly ? 'hour' : 'day') + (showSum ? '&combined=true' : '')),
        ST.api.request('/api/stats/hour-of-day?' + base),
        ST.api.request('/api/stats/heatmap?node=' + encodeURIComponent(heatNode.id) + '&metric=' + m + '&timezone=' + encodeURIComponent(zone) + '&range=' + encodeURIComponent(range) + (q.ispId ? '&isps=' + encodeURIComponent(q.ispId) : '') + ST.engineQuery()),
        ST.api.request('/api/stats/distribution?' + base)
      ]).then(function (r) {
        if (remote.pending !== key) return;
        var data = { summary: {}, hours: {}, trend: [], heat: null, dist: {} };
        r[0].data.forEach(function (x) { data.summary[x.nodeId] = x.current; });
        r[1].data.forEach(function (x) {
          var node = x.nodeId ? ST.node(x.nodeId) : null;
          data.trend.push({ id: x.nodeId || 'sum', name: node ? node.name : 'Combined total', color: node ? ST.nodeColor(node) : 'var(--text)', dashed: !node,
            points: x.points.filter(function (p) { return p.value != null; }).map(function (p) { return [Date.parse(p.ts), p.value, p.ts]; }) });
        });
        r[2].data.forEach(function (x) { data.hours[x.nodeId] = x.hours.map(function (hh) { return hh.value; }); });
        var cells = r[3].data.cells, matrix = [], counts = [];
        for (var d = 0; d < 7; d++) { matrix.push([]); counts.push([]); for (var hr = 0; hr < 24; hr++) { matrix[d].push(null); counts[d].push(0); } }
        cells.forEach(function (c) { matrix[c.weekday][c.hour] = c.value; counts[c.weekday][c.hour] = c.count; });
        data.heat = { matrix: matrix, counts: counts };
        r[4].data.forEach(function (x) { data.dist[x.nodeId] = { n: x.count, bins: x.bins }; });
        remote.key = key; remote.at = Date.now(); remote.data = data; remote.error = null; ST.refresh();
      }, function (error) {
        if (remote.pending !== key) return;
        remote.key = key; remote.at = Date.now(); remote.data = null; remote.error = error; ST.refresh();
      });
    }
    return null;
  }

  ST.pages.stats = function (root) {
    var all = ST.nodes();
    if (!all.length) { root.appendChild(U.pageHead('Statistics')); root.appendChild(U.card(null, null, U.empty('No data yet', 'Add a node to see statistics.'))); return; }
    // Every node is shown until the owner picks some by hand, including nodes added later.
    if (!q.nodes || q.allNodes !== false) q.nodes = all.map(function (n) { return n.id; });
    q.nodes = q.nodes.filter(function (id) { return ST.node(id); });
    if (!q.heatNode || !ST.node(q.heatNode)) q.heatNode = all[0].id;
    var now = Date.now(), range = ST.settings().range, from = now - ST.ranges[range];
    var m = q.metric, sel = q.nodes.map(ST.node);

    root.appendChild(ST.engineSelector());
    root.appendChild(U.pageHead('Statistics', 'How fast, how steady and when — ' + ST.rangeLabels[range].toLowerCase() + '.', [
      U.seg(Object.keys(ST.metrics).map(function (k) { return { value: k, label: ST.metrics[k].label }; }), m, function (v) { q.metric = v; ST.refresh(); })]));

    var nodeBox = h('div', { class: 'legend mb' });
    all.forEach(function (n) {
      var on = q.nodes.indexOf(n.id) >= 0;
      nodeBox.appendChild(h('button', { type: 'button', class: on ? '' : 'off', 'aria-pressed': on, onClick: function () {
        q.allNodes = false; var i = q.nodes.indexOf(n.id); if (i >= 0) { if (q.nodes.length === 1) { U.toast('Keep at least one node selected'); return; } q.nodes.splice(i, 1); } else q.nodes.push(n.id); ST.refresh();
      } }, U.dot(ST.nodeColor(n)), ST.nodeTitle(n)));
    });
    root.appendChild(nodeBox);
    root.appendChild(h('div', { class: 'mb', style: 'max-width:280px' }, U.field('ISP', U.select([{ value: '', label: 'Any ISP' }].concat(ST.ispOptions()), q.ispId, function (v) { q.ispId = v; remote.key = null; ST.refresh(); }))));

    /* ---- summary table ---- */
    var hourly = ST.ranges[range] <= ST.DAY, showSum = q.sum && ST.metrics[m].unit === 'speed' && sel.length > 1;
    var heatNode = ST.node(q.heatNode);
    var data = ST.demo ? localData(sel, m, from, now, hourly, showSum, heatNode) : remoteData(sel, m, range, hourly, showSum, heatNode);
    if (!data) { root.appendChild(U.card(null, null, h('div', { role: 'status', class: 'empty' }, 'Loading statistics…'))); return; }
    if (data.error) { root.appendChild(U.card(null, null, U.empty('Statistics are unavailable', data.error.message || 'The command center could not compute them. Try again.'))); return; }
    var sums = sel.map(function (n) { return { n: n, s: data.summary[n.id] || (q.ispId ? emptySummary() : S.nodeSummary(n.id, from, now)) }; });
    var tableCols = [
      { key: 'n', label: 'Node', render: function (r) { return h('span', { class: 'row' }, U.dot(ST.nodeColor(r.n)), r.n.name, h('span', { class: 'muted' }, r.n.label)); } },
      { key: 'c', label: 'Tests', num: true, render: function (r) { return F.num(r.s.count); } },
      { key: 'ok', label: 'Success', num: true, render: function (r) { return successCell(r.s); } },
      { key: 'avg', label: 'Average', num: true, render: function (r) { return fmtM(m, r.s[m].mean); } },
      { key: 'med', label: 'Median', num: true, render: function (r) { return fmtM(m, r.s[m].median); } },
      { key: 'p5', label: ST.metrics[m].good === 'high' ? 'Slow 5%' : 'Best 5%', num: true, render: function (r) { return fmtM(m, r.s[m].p5); } },
      { key: 'p95', label: ST.metrics[m].good === 'high' ? 'Fast 5%' : 'Worst 5%', num: true, render: function (r) { return fmtM(m, r.s[m].p95); } },
      { key: 'min', label: 'Lowest', num: true, render: function (r) { return fmtM(m, r.s[m].min); } },
      { key: 'max', label: 'Highest', num: true, render: function (r) { return fmtM(m, r.s[m].max); } },
      { key: 'std', label: 'Variation (±)', num: true, render: function (r) { return fmtM(m, r.s[m].std); } }];
    if (m === 'down' || m === 'up') tableCols.push({ key: 'pe', label: 'Of expected', num: true, render: function (r) { return F.pct(m === 'down' ? r.s.downPct : r.s.upPct, 0); } });
    tableCols.push({ key: 'st', label: 'Stability', num: true, render: function (r) { return r.s.stability == null ? '—' : F.num(r.s.stability, 0) + '/100'; } });
    root.appendChild(U.card('Summary by node', 'Slow 5% / Fast 5% show the typical worst and best cases without being thrown off by a single odd test.', h('div', { style: 'margin:0 -18px -18px' }, U.table({ columns: tableCols, rows: sums, onRow: function (r) { location.hash = '#/nodes/' + r.n.id; } }))));

    /* ---- insights ---- */
    var byHour = data.hours;
    var insights = h('div', { class: 'stack' });
    sel.forEach(function (n) {
      var vals = byHour[n.id], valid = vals.map(function (v, i) { return [i, v]; }).filter(function (p) { return p[1] != null; });
      if (valid.length < 4) return;
      var hi = ST.metrics[m].good === 'high', avg = S.mean(valid.map(function (p) { return p[1]; }));
      var sorted = valid.slice().sort(function (a, b) { return hi ? b[1] - a[1] : a[1] - b[1]; });
      var best = sorted[0], worst = sorted[sorted.length - 1], gap = Math.abs(worst[1] - avg) / avg * 100;
      var pad = function (x) { return (x < 10 ? '0' : '') + x + ':00'; };
      insights.appendChild(h('div', { class: 'list-row' }, U.dot(ST.nodeColor(n)), h('div', { class: 'grow' },
        h('b', null, n.name), ' is best around ', h('b', null, pad(best[0])), ' (' + fmtM(m, best[1]) + ') and weakest around ', h('b', null, pad(worst[0])), ' (' + fmtM(m, worst[1]) + ', ' + F.num(gap, 0) + '% ' + (hi ? 'below' : 'above') + ' its average).')));
    });
    if (insights.children.length) root.appendChild(h('div', { class: 'mt' }, U.card('What stands out', 'Based on the average at each hour of the day.', insights)));

    /* ---- trend ---- */
    var trendHost = h('div');
    var trendSeries = data.trend;
    var trendCtl = [];
    if (ST.metrics[m].unit === 'speed' && sel.length > 1) trendCtl.push(h('label', { class: 'check' }, h('input', { type: 'checkbox', checked: q.sum, onChange: function (e) { q.sum = e.target.checked; ST.refresh(); } }), 'Add combined total'));
    trendCtl.push(U.seg([{ value: 'chart', label: 'Chart' }, { value: 'table', label: 'Table' }], q.table ? 'table' : 'chart', function (v) { q.table = v === 'table'; drawTrend(); }));
    function drawTrend() {
      U.clear(trendHost);
      if (q.table) {
        var keys = {}; trendSeries.forEach(function (s) { s.points.forEach(function (p) { keys[p[2] || p[0]] = p[0]; }); });
        var ts = Object.keys(keys).map(function (k) { return { k: k, t: keys[k] }; }).sort(function (a, b) { return b.t - a.t; });
        var look = {}; trendSeries.forEach(function (s) { look[s.id] = {}; s.points.forEach(function (p) { look[s.id][p[2] || p[0]] = p[1]; }); });
        trendHost.appendChild(U.table({ columns: [{ key: 't', label: hourly ? 'Hour' : 'Day', render: function (r) { return hourly ? F.date(r.t) : F.date(r.t, 'dayfull'); } }].concat(trendSeries.map(function (s) {
          return { key: s.id, label: s.name, num: true, render: function (r) { return fmtM(m, look[s.id][r.k]); } };
        })), rows: ts.slice(0, 60) }));
        return;
      }
      ST.charts.line(trendHost, { series: trendSeries, x: { type: 'time', min: from, max: now }, y: { min: 0, format: axisFmt(m), unit: axisUnit(m) }, height: 300, label: ST.metrics[m].label + ' trend', tipValue: function (s, v) { return fmtM(m, v); } });
    }
    root.appendChild(h('div', { class: 'mt' }, U.card('Trend', (hourly ? 'Hourly' : 'Daily') + ' average of ' + ST.metrics[m].label.toLowerCase(), trendHost, trendCtl)));
    drawTrend();

    /* ---- time of day ---- */
    var todHost = h('div');
    root.appendChild(h('div', { class: 'mt' }, U.card('Time of day', 'Average at each hour. Shows when the connection is busiest.', todHost)));
    ST.charts.line(todHost, {
      series: sel.map(function (n) { return { id: n.id, name: n.name, color: ST.nodeColor(n), points: byHour[n.id].map(function (v, i) { return [i, v]; }) }; }),
      x: { type: 'linear', min: 0, max: 23, ticks: [0, 3, 6, 9, 12, 15, 18, 21], format: function (v) { return (v < 10 ? '0' : '') + v + ':00'; } },
      y: { min: 0, format: axisFmt(m), unit: axisUnit(m) }, height: 260, label: 'Average by hour of day', tipValue: function (s, v) { return fmtM(m, v); }
    });

    /* ---- heatmap ---- */
    var heatHost = h('div'), matrix = data.heat.matrix, counts = data.heat.counts;
    root.appendChild(h('div', { class: 'mt' }, U.card('Week at a glance', 'Average ' + ST.metrics[m].label.toLowerCase() + ' for each weekday and hour over the last 30 days. Use the scale below the grid.',
      h('div', null, heatHost, h('div', { class: 'heat-legend', style: 'margin-top:10px' }, h('span', null, ST.metrics[m].good === 'high' ? 'Lowest' : 'Lowest'), h('span', { class: 'ramp' }), h('span', null, ST.metrics[m].good === 'high' ? 'Highest' : 'Highest'))),
      [U.select(all.map(function (n) { return { value: n.id, label: ST.nodeTitle(n) }; }), q.heatNode, function (v) { q.heatNode = v; ST.refresh(); }, 'auto')])));
    ST.charts.heat(heatHost, { rows: DAYS, cols: Array.apply(null, Array(24)).map(function (_, i) { return (i < 10 ? '0' : '') + i; }), values: matrix, counts: counts, name: ST.metrics[m].label, format: function (v) { return fmtM(m, v); }, label: 'Heatmap for ' + heatNode.name });

    /* ---- distribution ---- */
    var dist = h('div', { class: 'grid cols-3' });
    sel.forEach(function (n) {
      var d = data.dist[n.id] || { n: 0, bins: [] }, host = h('div');
      dist.appendChild(h('div', { class: 'card card-pad' }, h('div', { class: 'row', style: 'margin-bottom:4px' }, U.dot(ST.nodeColor(n)), h('b', null, n.name), h('span', { class: 'muted' }, d.n + ' tests')), host));
      if (d.n < 3 || !d.bins.length) { host.appendChild(U.empty('Not enough data', 'Needs a few more tests.')); return; }
      ST.charts.bars(host, { bars: d.bins.map(function (b, i) {
        return { value: b.count, label: i % 3 === 0 ? (ST.metrics[m].unit === 'speed' ? F.speedParts(b.from).v : F.num(b.from, m === 'loss' ? 1 : 0)) : '', tipHead: fmtM(m, b.from) + ' – ' + fmtM(m, b.to), color: ST.nodeColor(n), tip: [{ label: 'Tests', value: String(b.count) + ' (' + F.num(b.percent, 0) + '%)', color: ST.nodeColor(n) }] };
      }), height: 170, format: function (x) { return F.num(x, 0); }, label: 'Distribution for ' + n.name });
    });
    root.appendChild(h('div', { class: 'mt' }, U.card('Distribution', 'How many tests landed in each ' + ST.metrics[m].label.toLowerCase() + ' range. A narrow, tall shape means a steady connection.', dist)));
  };
})();

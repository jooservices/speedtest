/* Statistics: summary table, trend, time-of-day, weekly heatmap and distribution. */
(function () {
  'use strict';
  var ST = window.ST, U = ST.ui, h = U.h, F = ST.fmt, S = ST.stats;
  var q = { nodes: null, metric: 'down', heatNode: null, sum: false, table: false };
  var DAYS = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'];

  function fmtM(m, v) { return v == null ? '—' : F.metric(m, v); }
  function axisFmt(m) { return ST.metrics[m].unit === 'speed' ? F.axisSpeed : function (v) { return F.num(v, m === 'loss' ? 1 : 0); }; }
  function axisUnit(m) { return ST.metrics[m].unit === 'speed' ? F.axisUnit() : ST.metrics[m].unit === 'ms' ? 'ms' : '%'; }

  ST.pages.stats = function (root) {
    var all = ST.nodes();
    if (!all.length) { root.appendChild(U.pageHead('Statistics')); root.appendChild(U.card(null, null, U.empty('No data yet', 'Add a node to see statistics.'))); return; }
    if (!q.nodes) q.nodes = all.map(function (n) { return n.id; });
    q.nodes = q.nodes.filter(function (id) { return ST.node(id); });
    if (!q.heatNode || !ST.node(q.heatNode)) q.heatNode = all[0].id;
    var now = Date.now(), range = ST.settings().range, from = now - ST.ranges[range];
    var m = q.metric, sel = q.nodes.map(ST.node);

    root.appendChild(U.pageHead('Statistics', 'How fast, how steady and when — ' + ST.rangeLabels[range].toLowerCase() + '.', [
      U.seg(Object.keys(ST.metrics).map(function (k) { return { value: k, label: ST.metrics[k].label }; }), m, function (v) { q.metric = v; ST.refresh(); })]));

    var nodeBox = h('div', { class: 'legend mb' });
    all.forEach(function (n) {
      var on = q.nodes.indexOf(n.id) >= 0;
      nodeBox.appendChild(h('button', { type: 'button', class: on ? '' : 'off', 'aria-pressed': on, onClick: function () {
        var i = q.nodes.indexOf(n.id); if (i >= 0) { if (q.nodes.length === 1) { U.toast('Keep at least one node selected'); return; } q.nodes.splice(i, 1); } else q.nodes.push(n.id); ST.refresh();
      } }, U.dot(ST.nodeColor(n)), ST.nodeTitle(n)));
    });
    root.appendChild(nodeBox);

    /* ---- summary table ---- */
    var sums = sel.map(function (n) { return { n: n, s: S.nodeSummary(n.id, from, now) }; });
    var tableCols = [
      { key: 'n', label: 'Node', render: function (r) { return h('span', { class: 'row' }, U.dot(ST.nodeColor(r.n)), r.n.name, h('span', { class: 'muted' }, r.n.label)); } },
      { key: 'c', label: 'Tests', num: true, render: function (r) { return F.num(r.s.count); } },
      { key: 'ok', label: 'Success', num: true, render: function (r) { return F.pct(r.s.successRate, 1); } },
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
    var byHour = {};
    sel.forEach(function (n) {
      var buckets = []; for (var i = 0; i < 24; i++) buckets.push([]);
      ST.results({ nodeIds: [n.id], from: from, status: 'ok' }).forEach(function (r) { var v = S.metricValue(r, m); if (v != null) buckets[F.parts(r.ts).hour].push(v); });
      byHour[n.id] = buckets.map(function (b) { return b.length ? S.mean(b) : null; });
    });
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
    var hourly = ST.ranges[range] <= ST.DAY;
    var trendHost = h('div'), showSum = q.sum && ST.metrics[m].unit === 'speed' && sel.length > 1;
    var trendSeries = sel.map(function (n) {
      var map = {}, order = [];
      ST.results({ nodeIds: [n.id], from: from, status: 'ok' }).forEach(function (r) {
        var v = S.metricValue(r, m); if (v == null) return;
        var p = F.parts(r.ts), key = hourly ? p.key + 'T' + p.hour : p.key, e = map[key];
        if (!e) { e = map[key] = { t: 0, n: 0, v: 0 }; order.push(key); }
        e.t += r.ts; e.n++; e.v += v;
      });
      return { id: n.id, name: n.name, color: ST.nodeColor(n), points: order.map(function (k) { var e = map[k]; return [e.t / e.n, e.v / e.n, k]; }) };
    });
    if (showSum) {
      var sumMap = {};
      trendSeries.forEach(function (s) { s.points.forEach(function (p) { var e = sumMap[p[2]] || (sumMap[p[2]] = { t: p[0], v: 0 }); e.v += p[1]; }); });
      trendSeries.push({ id: 'sum', name: 'Combined total', color: 'var(--text)', dashed: true, points: Object.keys(sumMap).map(function (k) { return [sumMap[k].t, sumMap[k].v]; }).sort(function (a, b) { return a[0] - b[0]; }) });
    }
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
    var heatHost = h('div'), heatNode = ST.node(q.heatNode);
    var vals = [], counts = []; for (var d = 0; d < 7; d++) { vals.push([]); counts.push([]); for (var hr = 0; hr < 24; hr++) { vals[d].push([]); counts[d].push(0); } }
    ST.results({ nodeIds: [heatNode.id], from: now - 30 * ST.DAY, status: 'ok' }).forEach(function (r) { var p = F.parts(r.ts), v = S.metricValue(r, m); if (v != null) vals[p.wd][p.hour].push(v); });
    var matrix = vals.map(function (row, di) { return row.map(function (c, hi) { counts[di][hi] = c.length; return c.length ? S.mean(c) : null; }); });
    root.appendChild(h('div', { class: 'mt' }, U.card('Week at a glance', 'Average ' + ST.metrics[m].label.toLowerCase() + ' for each weekday and hour over the last 30 days. Use the scale below the grid.',
      h('div', null, heatHost, h('div', { class: 'heat-legend', style: 'margin-top:10px' }, h('span', null, ST.metrics[m].good === 'high' ? 'Lowest' : 'Lowest'), h('span', { class: 'ramp' }), h('span', null, ST.metrics[m].good === 'high' ? 'Highest' : 'Highest'))),
      [U.select(all.map(function (n) { return { value: n.id, label: ST.nodeTitle(n) }; }), q.heatNode, function (v) { q.heatNode = v; ST.refresh(); }, 'auto')])));
    ST.charts.heat(heatHost, { rows: DAYS, cols: Array.apply(null, Array(24)).map(function (_, i) { return (i < 10 ? '0' : '') + i; }), values: matrix, counts: counts, name: ST.metrics[m].label, format: function (v) { return fmtM(m, v); }, label: 'Heatmap for ' + heatNode.name });

    /* ---- distribution ---- */
    var dist = h('div', { class: 'grid cols-3' });
    sel.forEach(function (n) {
      var v = ST.results({ nodeIds: [n.id], from: from, status: 'ok' }).map(function (r) { return S.metricValue(r, m); }).filter(function (x) { return x != null; });
      var host = h('div');
      dist.appendChild(h('div', { class: 'card card-pad' }, h('div', { class: 'row', style: 'margin-bottom:4px' }, U.dot(ST.nodeColor(n)), h('b', null, n.name), h('span', { class: 'muted' }, v.length + ' tests')), host));
      if (v.length < 3) { host.appendChild(U.empty('Not enough data', 'Needs a few more tests.')); return; }
      var lo = Math.min.apply(null, v), hi = Math.max.apply(null, v), bins = 12, w = (hi - lo) / bins || 1, counts2 = Array.apply(null, Array(bins)).map(function () { return 0; });
      v.forEach(function (x) { counts2[Math.min(bins - 1, Math.floor((x - lo) / w))]++; });
      ST.charts.bars(host, { bars: counts2.map(function (c, i) {
        var a = lo + i * w, b = a + w;
        return { value: c, label: i % 3 === 0 ? (ST.metrics[m].unit === 'speed' ? F.speedParts(a).v : F.num(a, m === 'loss' ? 1 : 0)) : '', tipHead: fmtM(m, a) + ' – ' + fmtM(m, b), color: ST.nodeColor(n), tip: [{ label: 'Tests', value: String(c) + ' (' + F.num(c / v.length * 100, 0) + '%)', color: ST.nodeColor(n) }] };
      }), height: 170, format: function (x) { return F.num(x, 0); }, label: 'Distribution for ' + n.name });
    });
    root.appendChild(h('div', { class: 'mt' }, U.card('Distribution', 'How many tests landed in each ' + ST.metrics[m].label.toLowerCase() + ' range. A narrow, tall shape means a steady connection.', dist)));
  };
})();

/* Statistics: summary table, trend, time-of-day, weekly heatmap and distribution. */
(function () {
  'use strict';
  var ST = (/** @type {any} */ (window)).ST, U = ST.ui, h = U.h, F = ST.fmt, S = ST.stats;
  var q = { nodes: null, metric: 'down', heatNode: null, sum: false, table: false, isp: '', ispDraft: '', ispId: '' };
  var DAYS = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'];

  function fmtM(m, v) { return v == null ? '—' : F.metric(m, v); }
  function successCell(summary) {
    var note = summary.rateLimited ? h('span', { class: 'hint' }, F.num(summary.rateLimited) + ' rate-limited excluded') : null;
    return h('span', { class: 'stack' }, h('span', null, F.pct(summary.successRate, 1)), note);
  }
  function axisFmt(m) { return ST.metrics[m].unit === 'speed' ? F.axisSpeed : function (v) { return F.num(v, m === 'loss' ? 1 : 0); }; }
  function axisUnit(m) { return ST.metrics[m].unit === 'speed' ? F.axisUnit() : ST.metrics[m].unit === 'ms' ? 'ms' : '%'; }
  function sparseTrendNotice(metric, nodes, data, hourly, showSum) {
    var groups = nodes.map(function (n) { return { id: n.id, name: n.name, samples: data.dist[n.id] ? data.dist[n.id].n : 0 }; });
    if (showSum && nodes.length > 1) groups.push({ id: 'sum', name: 'Combined total', samples: groups.reduce(function (total, x) { return total + x.samples; }, 0) });
    var sparse = groups.map(function (x) {
      var series = data.trend.filter(function (s) { return s.id === x.id; })[0];
      x.buckets = series ? series.points.filter(function (p) { return p[1] != null; }).length : 0;
      return x;
    }).filter(function (x) { return x.buckets < 3; });
    if (!sparse.length) return null;
    var bucket = hourly ? 'hourly bucket' : 'daily bucket';
    return U.callout('Not enough data yet. A ' + ST.metrics[metric].label.toLowerCase() + ' trend needs at least three valid ' + (hourly ? 'hourly buckets. ' : 'daily buckets. ') +
      sparse.map(function (x) { return x.name + ': ' + x.samples + (x.samples === 1 ? ' sample' : ' samples') + ' in ' + x.buckets + ' plotted ' + bucket + (x.buckets === 1 ? '' : 's'); }).join('; ') + '.', 'info', 'info');
  }


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

  function ispMatches(result, isp, ispId) {
    var effective = S.effectiveISP(result);
    return (!isp || (effective && effective.toLowerCase() === isp.toLowerCase())) && (!ispId || ST.resultISP(result) === ispId);
  }

  function localEvents(sel, from, to, isp, ispId) {
    var events = [], selected = {};
    sel.forEach(function (node) {
      selected[node.id] = node;
      var engine = ST.selectedEngine || node.primaryEngine || 'ookla';
      var rows = ST.results({ nodeIds: [node.id], engine: engine }).slice().sort(function (a, b) { return a.ts - b.ts || String(a.id).localeCompare(String(b.id)); });
      S.ipChanges(rows, engine, from, to).forEach(function (change) {
        if (ispMatches(change.result, isp, ispId)) events.push({ id: 'ip-' + change.result.id, nodeId: node.id, nodeName: node.name,
          ts: change.result.ts, type: 'ip_change', label: 'IP ' + change.from + ' → ' + change.result.ip });
      });
      var previousISP = null;
      rows.forEach(function (result) {
        var effective = S.effectiveISP(result);
        if (result.ts >= from && result.ts < to) {
          if (previousISP && effective && previousISP.toLowerCase() !== effective.toLowerCase() && ispMatches(result, isp, ispId)) events.push({ id: 'isp-' + result.id, nodeId: node.id, nodeName: node.name, ts: result.ts, type: 'isp_change', label: 'ISP ' + previousISP + ' → ' + effective });
        }
        if (effective) previousISP = effective;
      });
    });
    ST.events().forEach(function (event) {
      var node = selected[event.nodeId];
      if (!node || event.ts < from || event.ts >= to) return;
      var linkedID = event.triggerResultId || event.resultId, linked = null;
      if (linkedID) linked = (ST.state.results || []).filter(function (result) { return result.id === linkedID; })[0] || null;
      var engine = event.engine || (linked && linked.engine) || null;
      if (linked && engine && engine !== (ST.selectedEngine || node.primaryEngine || 'ookla')) return;
      if (linked && !ispMatches(linked, isp, ispId)) return;
      if (!linked && (isp || ispId)) return;
      events.push({ id: event.id, nodeId: node.id, nodeName: node.name, ts: event.ts, type: 'alert',
        label: event.title + (event.detail ? ': ' + event.detail : '') + (linked ? '' : ' (engine unknown)') });
    });
    events.sort(function (a, b) { return b.ts - a.ts || a.type.localeCompare(b.type) || a.id.localeCompare(b.id); });
    return { items: events.slice(0, 500), truncated: events.length > 500 };
  }

  function localData(sel, m, from, now, hourly, showSum, heatNode, isp, ispId) {
    var out = { summary: {}, week: {}, hours: {}, trend: [], heat: null, dist: {}, events: localEvents(sel, from, now, isp, ispId) };
    sel.forEach(function (n) {
      out.summary[n.id] = S.nodeSummary(n.id, from, now, isp, ispId);
      out.week[n.id] = { current: S.nodeSummary(n.id, now - 7 * ST.DAY, now, isp, ispId)[m].mean,
        previous: S.nodeSummary(n.id, now - 14 * ST.DAY, now - 7 * ST.DAY, isp, ispId)[m].mean };
      var buckets = []; for (var i = 0; i < 24; i++) buckets.push([]);
      ST.results({ nodeIds: [n.id], from: from, status: 'ok', isp: isp, ispId: ispId }).forEach(function (r) { var v = S.metricValue(r, m); if (v != null) buckets[F.parts(r.ts).hour].push(v); });
      out.hours[n.id] = buckets.map(function (b) { return b.length ? S.mean(b) : null; });
      var map = {}, order = [], values = [];
      ST.results({ nodeIds: [n.id], from: from, status: 'ok', isp: isp, ispId: ispId }).forEach(function (r) {
        var v = S.metricValue(r, m); if (v == null) return;
        values.push(v);
        var p = F.parts(r.ts), key = hourly ? p.key + 'T' + p.hour : p.key, e = map[key];
        if (!e) { e = map[key] = { t: 0, n: 0, v: 0 }; order.push(key); }
        e.t += r.ts; e.n++; e.v += v;
      });
      out.trend.push({ id: n.id, name: n.name, color: ST.nodeColor(n), points: order.map(function (k) { var e = map[k]; return [e.t / e.n, e.v / e.n, k, e.n]; }) });
      out.dist[n.id] = binsOf(values);
    });
    if (showSum) {
      var sumMap = {};
      out.trend.forEach(function (s) { s.points.forEach(function (p) { var e = sumMap[p[2]] || (sumMap[p[2]] = { t: p[0], v: 0, n: 0 }); e.v += p[1]; e.n += p[3] || 0; }); });
      out.trend.push({ id: 'sum', name: 'Combined total', color: 'var(--text)', dashed: true, points: Object.keys(sumMap).map(function (k) { return [sumMap[k].t, sumMap[k].v, k, sumMap[k].n]; }).sort(function (a, b) { return a[0] - b[0]; }) });
    }
    var vals = [], counts = []; for (var d = 0; d < 7; d++) { vals.push([]); counts.push([]); for (var hr = 0; hr < 24; hr++) { vals[d].push([]); counts[d].push(0); } }
    ST.results({ nodeIds: [heatNode.id], from: now - 30 * ST.DAY, status: 'ok', isp: isp, ispId: ispId }).forEach(function (r) { var p = F.parts(r.ts), v = S.metricValue(r, m); if (v != null) vals[p.wd][p.hour].push(v); });
    out.heat = { matrix: vals.map(function (row, di) { return row.map(function (c, hi) { counts[di][hi] = c.length; return c.length ? S.mean(c) : null; }); }), counts: counts };
    return out;
  }

  // The center computes every figure; the browser only draws them. Results are cached per request so a redraw
  // (theme, resize, refresh) does not fetch again.
  function remoteData(sel, m, range, hourly, showSum, heatNode, isp, ispId) {
    var ids = sel.map(function (n) { return n.id; }).join(','), zone = ST.timezone();
    var key = [ids, m, range, hourly, showSum, heatNode.id, zone, ST.selectedEngine, isp, ispId].join('|');
    if (remote.key === key && Date.now() - remote.at < 60000) return remote.error ? { error: remote.error } : remote.data;
    if (remote.pending !== key || remote.key === key) {
      remote.pending = key;
      var ispQuery = isp ? '&isp=' + encodeURIComponent(isp) : '';
      var ispIDQuery = ispId ? '&isps=' + encodeURIComponent(ispId) : '';
      var base = 'range=' + encodeURIComponent(range) + '&metric=' + m + '&timezone=' + encodeURIComponent(zone) + '&nodes=' + encodeURIComponent(ids) + ST.engineQuery() + ispQuery + ispIDQuery;
      Promise.all([
        ST.api.request('/api/stats/summary?range=' + encodeURIComponent(range) + '&timezone=' + encodeURIComponent(zone) + '&nodes=' + encodeURIComponent(ids) + ST.engineQuery() + ispQuery + ispIDQuery),
        ST.api.request('/api/stats/trend?' + base + '&bucket=' + (hourly ? 'hour' : 'day') + (showSum ? '&combined=true' : '')),
        ST.api.request('/api/stats/hour-of-day?' + base),
        ST.api.request('/api/stats/heatmap?node=' + encodeURIComponent(heatNode.id) + '&metric=' + m + '&timezone=' + encodeURIComponent(zone) + ST.engineQuery() + ispQuery + ispIDQuery),
        ST.api.request('/api/stats/distribution?' + base),
        ST.api.request('/api/stats/summary?range=7d&nodes=' + encodeURIComponent(ids) + ST.engineQuery() + ispQuery + ispIDQuery),
        ST.api.request('/api/stats/events?range=' + encodeURIComponent(range) + '&nodes=' + encodeURIComponent(ids) + ST.engineQuery() + ispQuery)
      ]).then(function (r) {
        if (remote.pending !== key) return;
        var data = { summary: {}, week: {}, hours: {}, trend: [], heat: null, dist: {}, events: r[6].data };
        r[0].data.forEach(function (x) { data.summary[x.nodeId] = x.current; });
        r[5].data.forEach(function (x) { data.week[x.nodeId] = { current: x.current[m].mean, previous: x.previous[m].mean }; });
        r[1].data.forEach(function (x) {
          var node = x.nodeId ? ST.node(x.nodeId) : null;
          data.trend.push({ id: x.nodeId || 'sum', name: node ? node.name : 'Combined total', color: node ? ST.nodeColor(node) : 'var(--text)', dashed: !node,
            points: x.points.filter(function (p) { return p.value != null; }).map(function (p) { return [Date.parse(p.ts), p.value, p.ts, p.count]; }) });
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
    ST.bandwidth.panel(root);
    root.appendChild(h('div', { class: 'mb', style: 'max-width:280px' }, U.field('ISP', U.select([{ value: '', label: 'Any ISP' }].concat(ST.ispOptions()), q.ispId, function (v) { q.ispId = v; remote.key = null; ST.refresh(); }))));

    /* ---- summary table ---- */
    var hourly = ST.ranges[range] <= ST.DAY, showSum = q.sum && ST.metrics[m].unit === 'speed' && sel.length > 1;
    var heatNode = ST.node(q.heatNode);
    var data = ST.demo ? localData(sel, m, from, now, hourly, showSum, heatNode, q.isp, q.ispId) : remoteData(sel, m, range, hourly, showSum, heatNode, q.isp, q.ispId);
    if (!data) { root.appendChild(U.card(null, null, h('div', { role: 'status', class: 'empty' }, 'Loading statistics…'))); return; }
    if (data.error) {
      var retry = U.empty('Statistics are unavailable', data.error.message || 'The command center could not compute them.');
      retry.appendChild(U.btn('Retry', { variant: 'primary', onClick: function () { remote.key = null; remote.pending = null; remote.error = null; ST.refresh(); } }));
      root.appendChild(U.card(null, null, retry)); return;
    }
    var filter = h('div', { class: 'stats-filter' }, U.field('Filter by ISP', U.input(q.ispDraft, function (v) { q.ispDraft = v; }, { type: 'text', maxlength: 200, placeholder: 'Exact ISP name', 'aria-label': 'Filter by ISP' }), 'Matches result engine ISP, then cached IP organisation. Unknown ISPs are excluded when a filter is active.'),
      U.btn('Apply', { variant: 'primary', onClick: function () { q.isp = q.ispDraft.trim(); remote.key = null; ST.refresh(); } }));
    if (q.isp) filter.appendChild(U.btn('Clear ISP filter', { onClick: function () { q.isp = ''; q.ispDraft = ''; remote.key = null; ST.refresh(); } }));
    root.appendChild(filter);
    var sums = sel.map(function (n) { return { n: n, s: data.summary[n.id] || S.nodeSummary(n.id, from, now, q.isp, q.ispId) }; });
    var tableCols = [
      { key: 'n', label: 'Node', render: function (r) { return h('span', { class: 'row' }, U.dot(ST.nodeColor(r.n)), r.n.name, h('span', { class: 'muted' }, r.n.label)); } },
      { key: 'c', label: 'Tests', num: true, render: function (r) { return F.num(r.s.count); } },
      { key: 'ok', label: 'Success', num: true, render: function (r) { return successCell(r.s); } },
      { key: 'avg', label: 'Average', num: true, render: function (r) { return fmtM(m, r.s[m].mean); } },
      { key: 'p50', label: 'P50 (' + axisUnit(m) + ')', num: true, render: function (r) { return fmtM(m, r.s[m].p50 == null ? r.s[m].median : r.s[m].p50); } },
      { key: 'p95', label: 'P95 (' + axisUnit(m) + ')', num: true, render: function (r) { return fmtM(m, r.s[m].p95); } },
      { key: 'p99', label: 'P99 (' + axisUnit(m) + ')', num: true, render: function (r) { return fmtM(m, r.s[m].p99); } },
      { key: 'p5', label: ST.metrics[m].good === 'high' ? 'Slow 5%' : 'Best 5%', num: true, render: function (r) { return fmtM(m, r.s[m].p5); } },
      { key: 'min', label: 'Lowest', num: true, render: function (r) { return fmtM(m, r.s[m].min); } },
      { key: 'max', label: 'Highest', num: true, render: function (r) { return fmtM(m, r.s[m].max); } },
      { key: 'std', label: 'Variation (±)', num: true, render: function (r) { return fmtM(m, r.s[m].std); } }];
    if (m === 'down' || m === 'up') tableCols.push({ key: 'pe', label: 'Of expected', num: true, render: function (r) { return F.pct(m === 'down' ? r.s.downPct : r.s.upPct, 0); } });
    tableCols.push({ key: 'st', label: 'Stability', num: true, render: function (r) { return r.s.stability == null ? '—' : F.num(r.s.stability, 0) + '/100'; } });
    root.appendChild(U.card('Summary by node', 'P50 is the median; p50, p95 and p99 use linear interpolation of nonfailed results.', h('div', { style: 'margin:0 -18px -18px' }, U.table({ columns: tableCols, rows: sums, onRow: function (r) { location.hash = '#/nodes/' + r.n.id; }, rowAction: {
      column: 'n', href: function (r) { return '#/nodes/' + r.n.id; }
    } }))));

    var weekRows = sel.map(function (n) { return { n: n, w: data.week[n.id] || { current: null, previous: null } }; });
    root.appendChild(h('div', { class: 'mt' }, U.card('Week over week', 'Selected metric mean for the latest seven days versus the preceding seven days, independent of the page range.', U.table({ columns: [
      { key: 'node', label: 'Node', render: function (r) { return r.n.name; } },
      { key: 'previous', label: 'Previous 7 days', num: true, render: function (r) { return fmtM(m, r.w.previous); } },
      { key: 'current', label: 'Latest 7 days', num: true, render: function (r) { return fmtM(m, r.w.current); } },
      { key: 'change', label: 'Change', num: true, render: function (r) { var change = S.weekChange(r.w.current, r.w.previous); return change == null ? '—' : (change > 0 ? '+' : '') + F.num(change, 1) + '%'; } }
    ], rows: weekRows }))));

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
      var notice = sparseTrendNotice(m, sel, data, hourly, showSum), chartHost = h('div');
      if (notice) trendHost.appendChild(notice);
      trendHost.appendChild(chartHost);
      ST.charts.line(chartHost, { series: trendSeries, events: data.events.items, x: { type: 'time', min: from, max: now }, y: { min: 0, format: axisFmt(m), unit: axisUnit(m) }, height: 300, label: ST.metrics[m].label + ' trend', tipValue: function (s, v) { return fmtM(m, v); } });
    }
    root.appendChild(h('div', { class: 'mt' }, U.card('Trend', (hourly ? 'Hourly' : 'Daily') + ' average of ' + ST.metrics[m].label.toLowerCase(), trendHost, trendCtl)));
    drawTrend();
    var eventRows = data.events.items.map(function (event) {
      var node = ST.node(event.nodeId), type = event.type.replace('_', ' '), eventTS = typeof event.ts === 'number' ? event.ts : Date.parse(event.ts);
      return h('li', { class: 'stats-event ' + event.type }, h('time', { dateTime: new Date(eventTS).toISOString() }, F.date(eventTS, 'full')),
        h('b', null, node ? node.name : event.nodeId), h('span', { class: 'tag' }, type), h('span', { class: 'stats-event-label' }, event.label));
    });
    var eventContent = h('div', { class: 'stats-events-content' });
    if (data.events.truncated) eventContent.appendChild(h('p', { class: 'stats-truncated', role: 'status' }, 'Showing the newest 500 events. Older events are omitted.'));
    eventContent.appendChild(eventRows.length ? h('ol', { class: 'stats-events' }, eventRows) : U.empty('No events in this range', 'IP changes, ISP changes and alert openings will appear here.'));
    root.appendChild(h('div', { class: 'mt' }, U.card('Trend events', 'IP and known ISP changes and alert openings in ' + ST.timezone() + '.', eventContent)));

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
    var heatNodeSelect = U.select(all.map(function (n) { return { value: n.id, label: ST.nodeTitle(n) }; }), q.heatNode, function (v) { q.heatNode = v; ST.refresh(); }, 'auto');
    heatNodeSelect.setAttribute('aria-label', 'Heatmap node');
    var heatCount = counts.reduce(function (total, row) { return total + row.reduce(function (sum, count) { return sum + count; }, 0); }, 0);
    var heatContent = h('div', null, heatCount ? null : U.empty('No data in the last 30 days', 'No successful tests have values for this metric in the selected time zone.'), heatHost,
      h('p', { class: 'muted stats-zone' }, 'Rolling 30 days · ' + ST.metrics[m].label + ' (' + axisUnit(m) + ') · ' + ST.timezone()),
      h('div', { class: 'heat-legend', style: 'margin-top:10px' }, h('span', null, 'Lowest'), h('span', { class: 'ramp' }), h('span', null, 'Highest')));
    root.appendChild(h('div', { class: 'mt' }, U.card('Week at a glance', 'Average ' + ST.metrics[m].label.toLowerCase() + ' for each weekday and hour over the rolling 30-day window.',
      h('div', null, heatContent),
      [heatNodeSelect])));
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

/* Dashboard: fleet summary, node cards, comparison chart, recent results and alerts. */
(function () {
  'use strict';
  var ST = (/** @type {any} */ (window)).ST, U = ST.ui, h = U.h, F = ST.fmt, S = ST.stats;
  var state = { metric: 'down', mode: 'abs', hidden: {} };
  var dashboardComparisons = {}, dashboardRequests = {}, dashboardRevision = 0, activeComparisonDisplay = null;

  ST.on('settings', function () { dashboardComparisons = {}; dashboardRequests = {}; dashboardRevision++; });
  window.addEventListener('hashchange', function () { dashboardRequests = {}; dashboardRevision++; activeComparisonDisplay = null; });

  function dashboardIsActive() {
    var path = (location.hash || '#/').slice(1).split('?')[0] || '/';
    return path === '/' && !ST.demo;
  }

  function displayComparison(key, data, error) {
    var display = activeComparisonDisplay;
    if (!dashboardIsActive() || !display || display.key !== key) return;
    Object.keys(display.fields).forEach(function (name) {
      var field = display.fields[name];
      var nextDelta = error ? h('span', { class: 'delta flat', title: 'Refresh the page to retry' }, 'Comparison unavailable') : null;
      if (!error) {
        var current = data.current[name], previous = data.previous[name];
        field.value.textContent = field.format(current);
        nextDelta = U.deltaTag(current, previous, field.goodHigh);
      }
      field.delta.replaceWith(nextDelta);
      field.delta = nextDelta;
    });
  }

  function comparison(range) {
    if (ST.demo) return null;
    var key = range + '|' + ST.selectedEngine;
    if (dashboardComparisons[key]) return Object.assign({ key: key }, dashboardComparisons[key]);
    if (dashboardRequests[key] || ST.remote.status !== 'ready') return { key: key, pending: true };
    var revision = dashboardRevision;
    var request = {};
    dashboardRequests[key] = request;
    ST.api.request('/api/dashboard?range=' + encodeURIComponent(range) + ST.engineQuery()).then(function (response) {
      if (dashboardRequests[key] !== request || revision !== dashboardRevision || !dashboardIsActive()) return;
      delete dashboardRequests[key];
      dashboardComparisons[key] = { data: response.data };
      displayComparison(key, response.data, null);
    }, function (error) {
      if (dashboardRequests[key] !== request || revision !== dashboardRevision || !dashboardIsActive()) return;
      delete dashboardRequests[key];
      dashboardComparisons[key] = { error: error };
      displayComparison(key, null, error);
    });
    return { key: key, pending: true };
  }

  ST.sevInfo = function (sev) {
    return { crit: { cls: 'crit', icon: 'alert', label: 'Critical' }, warn: { cls: 'warn', icon: 'alert', label: 'Warning' }, info: { cls: 'info', icon: 'info', label: 'Info' } }[sev] || { cls: 'info', icon: 'info', label: sev };
  };

  ST.pages.dashboard = function (root) {
    var s = ST.settings(), P = ST.ranges[s.range], now = Date.now(), from = now - P, prevFrom = now - 2 * P;
    var nodes = ST.nodes();
    root.appendChild(ST.engineSelector());
    root.appendChild(U.pageHead('Dashboard', 'How every connection is performing — ' + ST.rangeLabels[s.range].toLowerCase() + '.'));

    if (!nodes.length) {
      root.appendChild(U.card(null, null, U.empty('No nodes yet', 'Add your first node to start measuring a connection.', h('a', { class: 'btn primary', href: '#/nodes' }, 'Add a node'))));
      return;
    }

    /* ---- fleet tiles ---- */
    function fleet(a, b) {
      var sums = nodes.map(function (n) { return S.nodeSummary(n.id, a, b); });
      var pcts = sums.map(function (x) { return x.downPct; }).filter(function (v) { return v != null; });
      var upc = sums.map(function (x) { return x.upPct; }).filter(function (v) { return v != null; });
      var allOk = S.sum(sums.map(function (x) { return x.ok; })), allN = S.sum(sums.map(function (x) { return x.count; }));
      var rateLimited = S.sum(sums.map(function (x) { return x.rateLimited || 0; }));
      var eligible = allN - rateLimited;
      var pings = ST.results({ from: a, to: b, status: 'ok' }).map(function (r) { return r.ping; });
      var ips = S.sum(nodes.map(function (n) { return ST.ipHistory(n.id).changes.filter(function (c) { return c.ts >= a && c.ts <= b && !(n.ipModel && n.ipModel.kind === 'mix'); }).length; }));
      return { down: S.mean(pcts), up: S.mean(upc), ping: S.summary(pings).median, success: eligible ? allOk / eligible * 100 : null, rateLimited: rateLimited, ips: ips, count: allN };
    }
    var cur = fleet(from, now), prev = fleet(prevFrom, from), comparisonState = comparison(s.range);
    if (comparisonState && comparisonState.data) {
      cur = comparisonState.data.current;
      prev = comparisonState.data.previous;
    }
    function delta(value, previous, goodHigh) {
      if (comparisonState && comparisonState.pending) return h('span', { class: 'delta flat', role: 'status' }, 'Loading comparison…');
      if (comparisonState && comparisonState.error) return h('span', { class: 'delta flat', title: 'Refresh the page to retry' }, 'Comparison unavailable');
      return U.deltaTag(value, previous, goodHigh);
    }
    var tiles = h('div', { class: 'grid cols-5 mb' });
    var comparisonDisplay = { key: comparisonState && comparisonState.key, fields: {} };
    activeComparisonDisplay = comparisonDisplay;
    function tile(label, icon, value, unit, delta, hint, field, format, goodHigh, detail) {
      var valueText = h('span', null, value), deltaContent = delta || h('span', { class: 'delta flat' }, ' ');
      var deltaValue = deltaContent.classList && deltaContent.classList.contains('delta') ? deltaContent : deltaContent.querySelector('.delta');
      if (field && comparisonDisplay.key) comparisonDisplay.fields[field] = { value: valueText, delta: deltaValue, format: format, goodHigh: goodHigh };
      return h('div', { class: 'card stat', title: hint }, h('div', { class: 'stat-label' }, U.icon(icon, 15), label),
        h('div', { class: 'stat-value tabnum' }, valueText, unit ? h('small', null, unit) : null), h('div', { style: 'margin-top:2px' }, deltaContent),
        detail ? h('div', { class: 'stat-hint' }, detail) : null);
    }
    tiles.appendChild(tile('Download vs. expected', 'down', cur.down == null ? '—' : F.num(cur.down, 0), '%', delta(cur.down, prev.down, true), 'Average of each node’s download compared with the speed you expect from it', 'down', function (v) { return v == null ? '—' : F.num(v, 0); }, true));
    tiles.appendChild(tile('Upload vs. expected', 'up', cur.up == null ? '—' : F.num(cur.up, 0), '%', delta(cur.up, prev.up, true), 'Average of each node’s upload compared with the speed you expect from it', 'up', function (v) { return v == null ? '—' : F.num(v, 0); }, true));
    tiles.appendChild(tile('Median latency', 'activity', cur.ping == null ? '—' : F.num(cur.ping, 1), 'ms', delta(cur.ping, prev.ping, false), null, 'ping', function (v) { return v == null ? '—' : F.num(v, 1); }, false));
    var limitedText = F.num(cur.rateLimited || 0) + ' rate-limited ' + (cur.rateLimited === 1 ? 'test' : 'tests') + ' excluded from success rate';
    if (cur.rateLimited > 0 && cur.count - cur.rateLimited === 0) limitedText += ' · no eligible tests';
    tiles.appendChild(tile('Tests succeeded', 'check', cur.success == null ? '—' : F.num(cur.success, 1), '%', delta(cur.success, prev.success, true), F.num(cur.count) + ' tests in this range', 'success', function (v) { return v == null ? '—' : F.num(v, 1); }, true, limitedText));
    tiles.appendChild(tile('IP changes', 'swap', String(cur.ips), cur.ips === 1 ? 'change' : 'changes',
      h('span', { class: 'row wrap', style: 'gap:8px' }, delta(cur.ips, prev.ips, false), h('a', { href: '#/network', class: 'hint' }, 'See history')), null, 'ips', String, false));
    root.appendChild(tiles);

    /* ---- node cards ---- */
    var cards = h('div', { class: 'grid cols-3 mb' });
    nodes.forEach(function (n) {
      var last = ST.latest(n.id), ok = ST.latestOk(n.id), sum = S.nodeSummary(n.id, from, now), color = ST.nodeColor(n);
      var down24 = ST.results({ nodeIds: [n.id], from: now - 24 * ST.HOUR }).map(function (r) { return r.down; });
      var geo = last ? ST.geo(last.ip) : null;
      var failedNow = last && last.status === 'failed';
      var pct = ok ? ok.down / n.expectedDown * 100 : null;
      var statusChip = !n.enabled ? U.chip('Paused', '', 'pause') : failedNow ? U.chip('Last test failed', 'crit', 'x') : (last && last.status === 'degraded') ? U.chip('Degraded', 'warn', 'alert') : U.chip('Online', 'good', 'check');
      var card = h('article', { class: 'card node-card', tabindex: 0, role: 'link', onClick: function (e) { if (e.target.closest('button')) return; location.hash = '#/nodes/' + n.id; },
        onKeydown: function (e) { if (e.key === 'Enter' && e.target === e.currentTarget) location.hash = '#/nodes/' + n.id; } },
        h('div', { class: 'node-top' },
          h('div', null, h('div', { class: 'node-name' }, U.dot(color), n.name, h('span', { style: 'font-weight:500' }, n.label)), h('div', { class: 'node-meta' }, 'Expected ' + F.speed(n.expectedDown) + ' down · ' + F.speed(n.expectedUp) + ' up')),
          statusChip),
        h('div', { class: 'speeds' },
          h('div', null, h('div', { class: 'speed-big tabnum' }, F.speedParts(ok && ok.down).v, h('small', null, F.speedParts(ok && ok.down).u)), h('div', { class: 'speed-cap' }, U.icon('down', 12), 'Download')),
          h('div', null, h('div', { class: 'speed-big tabnum' }, F.speedParts(ok && ok.up).v, h('small', null, F.speedParts(ok && ok.up).u)), h('div', { class: 'speed-cap' }, U.icon('up', 12), 'Upload')),
          h('div', null, h('div', { class: 'speed-big tabnum' }, ok ? F.num(ok.ping, 0) : '—', h('small', null, 'ms')), h('div', { class: 'speed-cap' }, 'Ping'))),
        h('div', null, U.meter(ok && ok.down, n.expectedDown, color),
          h('div', { class: 'meter-cap' }, h('span', null, pct == null ? 'No data' : F.num(pct, 0) + '% of expected'), h('span', null, 'Avg ' + F.speed(sum.down.mean)))),
        C_spark(down24, color),
        h('div', { class: 'node-foot' },
          h('span', { class: 'row', style: 'gap:5px;min-width:0' }, U.icon('pin', 13), h('span', { class: 'mono', style: 'overflow:hidden;text-overflow:ellipsis' }, last ? last.ip : '—'), geo ? h('span', { class: 'muted' }, geo.city) : null),
          h('span', { class: 'row', style: 'gap:6px' }, h('span', { class: 'muted' }, last ? F.rel(last.ts) : 'never'),
            U.btn('', { icon: 'play', size: 'sm', title: 'Run a test on ' + n.name, onClick: function () { U.runTestDialog([n.id]); } }))));
      cards.appendChild(card);
    });
    var addCard = h('a', { class: 'card node-card', href: '#/nodes', style: 'place-items:center;align-content:center;text-align:center;color:var(--text-2);text-decoration:none;min-height:220px;border-style:dashed' },
      h('div', { class: 'ev-ico info', style: 'width:38px;height:38px;margin:0 auto 8px' }, U.icon('plus', 20)), h('b', null, 'Add a node'), h('span', { class: 'muted' }, 'Measure another connection'));
    cards.appendChild(addCard);
    root.appendChild(cards);

    ST.bandwidth.panel(root);

    /* ---- comparison chart ---- */
    var chartHost = h('div'), sparseHost = h('div'), legendHost = h('div'), tableMode = false;
    var metricSeg = U.seg([{ value: 'down', label: 'Download' }, { value: 'up', label: 'Upload' }, { value: 'ping', label: 'Latency' }, { value: 'jitter', label: 'Jitter' }, { value: 'loss', label: 'Loss' }], state.metric, function (v) { state.metric = v; drawChart(); modeHost.style.display = (v === 'down' || v === 'up') ? '' : 'none'; });
    var modeHost = h('div', { style: (state.metric === 'down' || state.metric === 'up') ? '' : 'display:none' }, U.seg([{ value: 'abs', label: 'Speed' }, { value: 'pct', label: '% of expected' }], state.mode, function (v) { state.mode = v; drawChart(); }));
    var viewSeg = U.seg([{ value: 'chart', label: 'Chart' }, { value: 'table', label: 'Table' }], 'chart', function (v) { tableMode = v === 'table'; drawChart(); });
    function drawChart() {
      var m = state.metric, pctMode = state.mode === 'pct' && (m === 'down' || m === 'up');
      var series = nodes.map(function (n) {
        var pts = ST.results({ nodeIds: [n.id], from: from }).map(function (r) {
          var v = r.status === 'failed' ? null : pctMode ? S.metricValue(r, m + '_pct') : S.metricValue(r, m);
          return [r.ts, v];
        });
        return { id: n.id, name: n.name + ' · ' + n.label, color: ST.nodeColor(n), points: pts, hidden: !!state.hidden[n.id] };
      });
      U.clear(legendHost); legendHost.appendChild(U.legend(series, state.hidden, function (id) { state.hidden[id] = !state.hidden[id]; drawChart(); }));
      U.clear(sparseHost);
      U.clear(chartHost);
      if (tableMode) {
        var rows = series.filter(function (x) { return !x.hidden; }).map(function (x) {
          var v = x.points.map(function (p) { return p[1]; }).filter(function (q) { return q != null; }), sm = S.summary(v);
          return { name: x.name, color: x.color, sm: sm };
        });
        var fmt = function (v) { return v == null ? '—' : pctMode ? F.num(v, 0) + '%' : F.metric(m, v); };
        chartHost.appendChild(U.table({ columns: [
          { key: 'name', label: 'Node', render: function (r) { return h('span', { class: 'row' }, U.dot(r.color), r.name); } },
          { key: 'n', label: 'Tests', num: true, render: function (r) { return r.sm.n; } },
          { key: 'mean', label: 'Average', num: true, render: function (r) { return fmt(r.sm.mean); } },
          { key: 'min', label: 'Lowest', num: true, render: function (r) { return fmt(r.sm.min); } },
          { key: 'max', label: 'Highest', num: true, render: function (r) { return fmt(r.sm.max); } }], rows: rows }));
        return;
      }
      var sparse = series.filter(function (x) { return !x.hidden; }).map(function (x) {
        return { name: x.name, count: x.points.filter(function (p) { return p[1] != null; }).length };
      }).filter(function (x) { return x.count < 3; });
      if (sparse.length) sparseHost.appendChild(U.callout('Not enough data yet. A ' + ST.metrics[m].label.toLowerCase() + ' trend needs at least three valid measurements. ' +
        sparse.map(function (x) { return x.name + ': ' + x.count; }).join('; ') + '.', 'info', 'info'));
      var host = h('div'); chartHost.appendChild(host);
      var yfmt = pctMode ? function (v) { return F.num(v, 0) + '%'; } : (ST.metrics[m].unit === 'speed' ? F.axisSpeed : function (v) { return F.num(v, m === 'loss' ? 1 : 0); });
      ST.charts.line(host, {
        series: series, x: { type: 'time', min: from, max: now }, y: { format: yfmt, min: 0, unit: pctMode ? '% of expected' : (ST.metrics[m].unit === 'speed' ? F.axisUnit() : ST.metrics[m].unit === 'ms' ? 'ms' : '%') }, height: 320, label: ST.metrics[m].label + ' over time',
        refLines: pctMode ? [{ v: 100, label: 'Expected', color: 'var(--text-2)' }] : null,
        tipValue: function (sr, v) { return pctMode ? F.num(v, 1) + '% of expected' : F.metric(m, v); }
      });
    }
    var chartCard = U.card('Compare connections', 'Hover for exact values. Click a node to hide or show it. Use “% of expected” to compare nodes with different plans fairly.', h('div', null, h('div', { class: 'row wrap between mb' }, legendHost), sparseHost, chartHost),
      [metricSeg, modeHost, viewSeg]);
    root.appendChild(chartCard);
    drawChart();

    /* ---- recent results + alerts ---- */
    var recent = ST.results({}).slice(-9).reverse();
    var resTable = U.table({
      columns: [
        { key: 'ts', label: 'Time', render: function (r) { return F.date(r.ts); } },
        { key: 'node', label: 'Node', render: function (r) { var n = ST.node(r.nodeId); return h('span', { class: 'row' }, U.dot(ST.nodeColor(n)), n.name); } },
        { key: 'down', label: 'Down', num: true, render: function (r) { return F.speed(r.down); } },
        { key: 'up', label: 'Up', num: true, render: function (r) { return F.speed(r.up); } },
        { key: 'ping', label: 'Ping', num: true, render: function (r) { return F.ms(r.ping); } },
        { key: 'st', label: 'Status', render: U.resultStatus }],
      rows: recent, onRow: function (r) { ST.openResult(r); }, rowAction: {
        column: 'ts', label: function (r) { var n = ST.node(r.nodeId); return 'Open result details for ' + (n ? n.name : 'unknown node') + ' at ' + F.date(r.ts); },
        onClick: function (r) { ST.openResult(r); }
      }
    });
    var evs = ST.events().filter(function (e) { return !e.acked; }).slice(0, 6);
    var evList = h('div');
    if (!evs.length) evList.appendChild(U.empty('All clear', 'No open alerts right now.'));
    evs.forEach(function (e) {
      var si = ST.sevInfo(e.severity), n = ST.node(e.nodeId);
      evList.appendChild(h('div', { class: 'list-row' }, h('div', { class: 'ev-ico ' + si.cls }, U.icon(si.icon, 15)),
        h('div', { class: 'grow' }, h('div', { style: 'font-weight:600' }, e.title), h('div', { class: 'muted' }, (n ? n.name : '') + ' · ' + F.rel(e.ts) + (e.resolvedAt ? ' · resolved' : ''))),
        U.btn('', { icon: 'check', size: 'sm', title: 'Acknowledge', onClick: function () { ST.ack(e.id); U.toast('Alert acknowledged'); } })));
    });
    root.appendChild(h('div', { class: 'grid cols-main mt' },
      U.card('Latest results', null, h('div', { style: 'margin:-8px -18px -18px' }, resTable), [h('a', { class: 'btn sm', href: '#/history' }, 'All results')]),
      U.card('Open alerts', null, evList, [h('a', { class: 'btn sm', href: '#/alerts' }, 'Manage')])));
  };

  function C_spark(values, color) { return h('div', { 'aria-hidden': 'true' }, ST.charts.spark(values, color)); }

  /* Result detail drawer, shared with the Results page. */
  ST.openResult = function (r) {
    var n = ST.node(r.nodeId), srv = r.server || ST.server(r.serverId), geo = ST.geo(r.ip);
    var usedServerID = r.serverId == null && r.server ? r.server.id : r.serverId;
    var differsFromPin = r.pinnedServerId != null && usedServerID != null && Number(r.pinnedServerId) !== Number(usedServerID);
    var json = JSON.stringify({ id: r.id, time: new Date(r.ts).toISOString(), node: n && n.name, status: r.status, download_mbps: r.down, upload_mbps: r.up, ping_ms: r.ping, jitter_ms: r.jitter, packet_loss_pct: r.loss, bytes_down: r.bytesDown, bytes_up: r.bytesUp, bytes_total: F.bytesTotal(r.bytesDown, r.bytesUp), loaded_latency_download_ms: r.dLat, loaded_latency_upload_ms: r.uLat, server: srv.name + ' - ' + srv.city, public_ip: r.ip, threads: r.threads }, null, 2);
    var body = h('div', { class: 'stack' },
      h('div', { class: 'row between' }, h('span', { class: 'row' }, U.dot(ST.nodeColor(n)), h('b', null, ST.nodeTitle(n))), U.statusChip(r.status)),
      r.status === 'failed' ? U.callout(r.error || 'The test could not finish.', 'crit', 'alert') : null,
      r.warnings && r.warnings.length ? U.callout(r.warnings.join(' '), 'warn', 'alert') : null,
      differsFromPin ? U.callout('This result used server #' + usedServerID + ' instead of pinned server #' + r.pinnedServerId + '.', 'warn', 'alert') : null,
      r.status !== 'failed' ? h('div', { class: 'grid cols-3' },
        h('div', { class: 'card card-pad' }, h('div', { class: 'muted' }, 'Download'), h('div', { class: 'speed-big tabnum' }, F.speedParts(r.down).v, h('small', null, F.speedParts(r.down).u)), h('div', { class: 'hint' }, F.num(r.down / n.expectedDown * 100, 0) + '% of expected')),
        h('div', { class: 'card card-pad' }, h('div', { class: 'muted' }, 'Upload'), h('div', { class: 'speed-big tabnum' }, F.speedParts(r.up).v, h('small', null, F.speedParts(r.up).u)), h('div', { class: 'hint' }, F.num(r.up / n.expectedUp * 100, 0) + '% of expected')),
        h('div', { class: 'card card-pad' }, h('div', { class: 'muted' }, 'Ping'), h('div', { class: 'speed-big tabnum' }, F.num(r.ping, 1), h('small', null, 'ms')), h('div', { class: 'hint' }, 'Jitter ' + F.ms(r.jitter)))) : null,
      U.kv([
        ['Time', F.date(r.ts, 'full')], ['Packet loss', r.status === 'failed' ? '—' : F.num(r.loss, 1) + '%'],
        ['Latency under load', r.status === 'failed' ? '—' : 'Down ' + F.ms(r.dLat) + ' · Up ' + F.ms(r.uLat)],
        ['Data used', F.bytes(r.bytesDown) + ' down · ' + F.bytes(r.bytesUp) + ' up · ' + F.bytes(F.bytesTotal(r.bytesDown, r.bytesUp)) + ' total'], ['Duration', F.num(r.duration, 0) + ' s'], ['Connections', r.threads],
        ['Test server', srv.name + ', ' + srv.city + ' (#' + srv.id + ')'], ['Public IP', h('span', { class: 'mono' }, r.ip)],
        ['Location', geo ? geo.city + ', ' + geo.country + ' · ' + geo.isp + ' (AS' + geo.asn + ')' : 'Unknown'], ['Started by', r.manual ? 'You (manual test)' : 'Schedule']]),
      h('div', null, h('div', { class: 'label', style: 'margin-bottom:6px' }, 'Raw result'), U.code(json)));
    U.drawer({ title: 'Result details', body: body });
  };
})();

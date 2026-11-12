/* Shared bandwidth summaries, budgets and interval cost advice. */
(function () {
  'use strict';
  var ST = (/** @type {any} */ (window)).ST, U = ST.ui, h = U.h, F = ST.fmt;
  var usageCache = {}, adviceCache = {}, adviceDrafts = {}, budgets = { items: null, etag: null, pending: null, error: null }, demoBudgets = ST.demo ? ST.demoBandwidthBudgets() : [], usageGroup = 'day', usageISP = '';
  var engines = ['ookla', 'librespeed'];

  function utcMonth(now, offset) {
    var d = new Date(now);
    return Date.UTC(d.getUTCFullYear(), d.getUTCMonth() + offset, 1);
  }
  function bucketStart(ts, group) {
    var d = new Date(ts), start;
    if (group === 'month') return Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), 1);
    if (group === 'week') {
      start = Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate());
      return start - ((d.getUTCDay() + 6) % 7) * 86400000;
    }
    return Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate());
  }
  function utcBucketLabel(ts, group) {
    var d = new Date(ts), year = String(d.getUTCFullYear()), month = String(d.getUTCMonth() + 1).padStart(2, '0');
    return group === 'month' ? year + '-' + month + ' UTC' : year + '-' + month + '-' + String(d.getUTCDate()).padStart(2, '0') + ' UTC';
  }
  function ispName(id) {
    var matches = ST.isps().filter(function (isp) { return isp.id === id; });
    return matches.length ? matches[0].name : id || 'Unassigned';
  }
  function emptyBytes() { return { bytesDown: 0, bytesUp: 0, totalBytes: 0 }; }
  function addBytes(target, down, up) {
    target.bytesDown += Number(down) || 0;
    target.bytesUp += Number(up) || 0;
    target.totalBytes = target.bytesDown + target.bytesUp;
  }
  function localUsage(group, now) {
    var month = utcMonth(now, 0), previous = utcMonth(now, -1), from = now - 365 * 86400000;
    var totals = emptyBytes(), mtd = emptyBytes(), prior = emptyBytes(), budgetMtd = emptyBytes(), by = {}, mtdByNode = {}, series = {}, results = [];
    ST.nodes().forEach(function (node) {
      (ST.selectedEngine ? [ST.selectedEngine] : engines).forEach(function (engine) {
        results = results.concat(ST.results({ nodeIds: [node.id], engine: engine, from: from, to: now }));
      });
    });
    results.forEach(function (r) {
      var resultISP = ST.resultISP(r) || '';
      if (r.bytesDown == null && r.bytesUp == null) return;
      var down = r.bytesDown || 0, up = r.bytesUp || 0, engine = r.engine || 'ookla', key = r.nodeId + '|' + engine + '|' + resultISP;
      if (r.ts >= month) {
        addBytes(budgetMtd, down, up);
        mtdByNode[r.nodeId] = mtdByNode[r.nodeId] || emptyBytes();
        addBytes(mtdByNode[r.nodeId], down, up);
      }
      if (usageISP && resultISP !== usageISP) return;
      by[key] = by[key] || { nodeId: r.nodeId, nodeName: (ST.node(r.nodeId) || {}).name || r.nodeId, engine: r.engine || 'ookla', bytesDown: 0, bytesUp: 0, totalBytes: 0,
        ispId: resultISP, ispName: ispName(resultISP), monthToDate: emptyBytes(), projectedMonthEnd: emptyBytes(), previousMonth: emptyBytes() };
      addBytes(totals, down, up);
      if (r.ts >= month) {
        addBytes(mtd, down, up);
        addBytes(by[key].monthToDate, down, up);
      }
      else if (r.ts >= previous && r.ts < month) { addBytes(prior, down, up); addBytes(by[key].previousMonth, down, up); }
      addBytes(by[key], down, up);
      var start = bucketStart(r.ts, group), skey = key + '|' + start;
      series[skey] = series[skey] || { nodeId: r.nodeId, engine: engine, ispId: resultISP, ispName: ispName(resultISP), start: new Date(start).toISOString(), bytesDown: 0, bytesUp: 0, totalBytes: 0 };
      addBytes(series[skey], down, up);
    });
    var elapsed = Math.max(now - month, 1), monthDays = (utcMonth(now, 1) - month) / 86400000;
    function projected(value) { return Math.round(value * monthDays * 86400000 / elapsed); }
    Object.keys(by).forEach(function (key) {
      by[key].projectedMonthEnd = { bytesDown: projected(by[key].monthToDate.bytesDown), bytesUp: projected(by[key].monthToDate.bytesUp), totalBytes: projected(by[key].monthToDate.totalBytes) };
    });
    var statuses = demoBudgets.map(function (b) {
      var used = b.scopeKey === 'global' ? budgetMtd.totalBytes : (mtdByNode[b.scopeKey] || emptyBytes()).totalBytes;
      var ratio = used / b.monthlyBytes;
      return { scopeKey: b.scopeKey, monthlyBytes: b.monthlyBytes, usedBytes: used, ratio: ratio, alert: ratio >= 0.8, action: b.action, atMaximumInterval: false };
    });
    return { totals: totals, monthToDate: mtd, projectedMonthEnd: { bytesDown: projected(mtd.bytesDown), bytesUp: projected(mtd.bytesUp), totalBytes: projected(mtd.totalBytes) }, previousMonth: prior,
      comparison: { currentMonthToDateBytes: mtd.totalBytes, previousMonthBytes: prior.totalBytes, projectedMonthEndBytes: projected(mtd.totalBytes), changePercent: prior.totalBytes ? (projected(mtd.totalBytes) - prior.totalBytes) / prior.totalBytes * 100 : null },
      byNodeEngine: Object.keys(by).map(function (k) { return by[k]; }), series: Object.keys(series).map(function (k) { return series[k]; }), budgets: statuses };
  }
  function usage(group) {
    var now = Date.now();
    if (ST.demo) return { data: localUsage('day', now) };
    var from = new Date(now - 365 * 86400000).toISOString(), to = new Date(now).toISOString();
    var key = [ST.selectedEngine, usageISP, Math.floor(now / 60000)].join('|');
    Object.keys(usageCache).forEach(function (cached) { if (cached !== key) delete usageCache[cached]; });
    if (usageCache[key]) return usageCache[key];
    var result = usageCache[key] = /** @type {any} */ ({ loading: true });
    var path = '/api/usage?from=' + encodeURIComponent(from) + '&to=' + encodeURIComponent(to) + '&group=day' + ST.engineQuery();
    if (usageISP) path += '&isp=' + encodeURIComponent(usageISP);
    ST.api.request(path)
      .then(function (response) { result.loading = false; result.data = response.data; ST.refresh(); }, function (error) { result.loading = false; result.error = error; ST.refresh(); });
    return result;
  }
  function moneylessComparison(value) { return value == null ? 'No previous month data' : (value > 0 ? '+' : '') + F.num(value, 1) + '% vs. previous month'; }
  function nodeName(id) { var n = ST.node(id); return n ? n.name : id || 'All nodes'; }

  function groupedSeries(series, group) {
    var buckets = {};
    (series || []).forEach(function (item) {
      var start = bucketStart(Date.parse(item.start), group), key = item.nodeId + '|' + item.engine + '|' + (item.ispId || '') + '|' + start;
      buckets[key] = buckets[key] || { nodeId: item.nodeId, engine: item.engine, ispId: item.ispId || '', ispName: item.ispName || 'Unassigned', start: new Date(start).toISOString(), bytesDown: 0, bytesUp: 0, totalBytes: 0 };
      addBytes(buckets[key], item.bytesDown, item.bytesUp);
    });
    return Object.keys(buckets).map(function (key) { return buckets[key]; }).sort(function (a, b) { return b.start.localeCompare(a.start) || a.nodeId.localeCompare(b.nodeId) || a.engine.localeCompare(b.engine) || a.ispId.localeCompare(b.ispId); });
  }

  function dailyChart(host, series) {
    var grouped = {};
    (series || []).forEach(function (item) {
      var key = item.nodeId + '|' + item.engine + '|' + (item.ispId || ''), node = ST.node(item.nodeId);
      grouped[key] = grouped[key] || { name: nodeName(item.nodeId) + ' · ' + item.engine + ' · ' + (item.ispName || 'Unassigned'), color: node ? ST.nodeColor(node) : 'var(--accent)', dashed: item.engine === 'librespeed', points: [] };
      grouped[key].points.push([Date.parse(item.start), item.totalBytes]);
    });
    var lines = Object.keys(grouped).sort().map(function (key) { return grouped[key]; });
    var now = Date.now();
    ST.charts.line(host, { series: lines, x: { type: 'time', min: now - 365 * 86400000, max: now }, y: { min: 0, format: F.bytes, unit: 'bytes per day' },
      height: 300, label: 'Daily bandwidth usage by node and engine', tipValue: function (_, value) { return F.bytes(value); } });
  }

  function adviceDraft(nodeId, engine, interval) {
    var key = nodeId + '|' + engine;
    if (!adviceDrafts[key]) adviceDrafts[key] = { interval: interval, budgetGiB: '', touched: false };
    else if (!adviceDrafts[key].touched) adviceDrafts[key].interval = interval;
    return adviceDrafts[key];
  }

  function recommendInDemo(avg, budget) {
    if (!(avg >= 0) || !(budget > 0)) return { interval: null, status: 'unavailable' };
    var needed = Math.ceil(avg * 30 * 24 * 60 / budget);
    return needed > 1440 ? { interval: null, status: 'infeasible' } : { interval: Math.max(5, needed), status: 'feasible' };
  }

  ST.bandwidth = {};
  ST.bandwidth.draftInterval = function (nodeId, engine, fallback) { return adviceDraft(nodeId, engine, fallback).interval; };
  ST.bandwidth.setDraftInterval = function (nodeId, engine, interval) {
    var draft = adviceDraft(nodeId, engine, interval);
    draft.interval = interval; draft.touched = true;
  };
  ST.bandwidth.clearDrafts = function (nodeId) {
    Object.keys(adviceDrafts).forEach(function (key) { if (key.indexOf(nodeId + '|') === 0) delete adviceDrafts[key]; });
    adviceCache = {};
  };
  ST.bandwidth.panel = function (root) {
    var group = usageGroup, state = usage(group), host = h('div', { class: 'stack' });
    if (state.loading) host.appendChild(h('div', { role: 'status', class: 'empty' }, 'Loading bandwidth usage…'));
    else if (state.error) host.appendChild(U.empty('Bandwidth usage is unavailable', state.error.message || 'Retry the page to reload usage.'));
    else {
      var data = state.data, cards = h('div', { class: 'grid cols-4' });
      [ ['Selected range', data.totals.totalBytes], ['Month to date', data.monthToDate.totalBytes], ['Projected month end', data.projectedMonthEnd.totalBytes], ['Previous month', data.previousMonth.totalBytes] ].forEach(function (x) {
        cards.appendChild(h('div', { class: 'card stat' }, h('div', { class: 'stat-label' }, x[0]), h('div', { class: 'stat-value tabnum' }, F.bytes(x[1]))));
      });
      host.appendChild(cards);
      host.appendChild(h('div', { class: 'hint' }, moneylessComparison(data.comparison.changePercent)));
      (data.budgets || []).filter(function (b) { return b.alert; }).forEach(function (b) {
        var atLimit = b.ratio >= 1;
        var policy = atLimit ? (b.action === 'pause' ? 'Tests are paused until the budget resets.' : b.atMaximumInterval ? 'Schedules already at 24 hours are paused; shorter schedules are widened.' : 'The test interval is widened until the budget resets.') : 'The configured schedule policy applies when the budget is reached.';
        host.appendChild(U.callout((atLimit ? 'Monthly bandwidth budget reached' : 'Monthly bandwidth budget is at least 80% used') + ' · ' + (b.scopeKey === 'global' ? 'Global' : nodeName(b.nodeId || b.scopeKey)) + ': ' + F.bytes(b.usedBytes) + ' of ' + F.bytes(b.monthlyBytes) + '. ' + policy, atLimit ? 'crit' : 'warn', 'alert'));
      });
      var rows = data.byNodeEngine || [];
      host.appendChild(U.table({ columns: [
        { key: 'node', label: 'Node', render: function (r) { return nodeName(r.nodeId); } },
        { key: 'engine', label: 'Engine' },
        { key: 'isp', label: 'ISP', render: function (r) { return r.ispName || 'Unassigned'; } },
        { key: 'down', label: 'Download', num: true, render: function (r) { return F.bytes(r.bytesDown); } },
        { key: 'up', label: 'Upload', num: true, render: function (r) { return F.bytes(r.bytesUp); } },
        { key: 'total', label: 'Range total', num: true, render: function (r) { return F.bytes(r.totalBytes); } },
        { key: 'mtd', label: 'Month to date', num: true, render: function (r) { return F.bytes(r.monthToDate.totalBytes); } },
        { key: 'projected', label: 'Projected month end', num: true, render: function (r) { return F.bytes(r.projectedMonthEnd.totalBytes); } },
        { key: 'previous', label: 'Previous month', num: true, render: function (r) { return F.bytes(r.previousMonth.totalBytes); } }], rows: rows }));
      var chartHost = h('div'), legend = h('div', { class: 'row wrap gap-sm mb' });
      (data.series || []).reduce(function (seen, item) {
        var key = item.nodeId + '|' + item.engine + '|' + (item.ispId || '');
        if (seen[key]) return seen;
        seen[key] = true;
        var node = ST.node(item.nodeId);
        legend.appendChild(h('span', { class: 'row gap-sm' }, U.dot(node ? ST.nodeColor(node) : 'var(--accent)'), h('span', null, nodeName(item.nodeId) + ' · ' + item.engine + ' · ' + (item.ispName || 'Unassigned'))));
        return seen;
      }, {});
      host.appendChild(U.card('Daily usage', 'Download and upload bytes summed per day for each node and engine. Hover or touch the chart for exact values.', h('div', null, legend, chartHost)));
      dailyChart(chartHost, data.series || []);
      var series = groupedSeries(data.series || [], group);
      host.appendChild(U.table({ columns: [
        { key: 'start', label: group === 'month' ? 'Month (UTC)' : group === 'week' ? 'Week starting (UTC)' : 'Day (UTC)', render: function (r) { return utcBucketLabel(Date.parse(r.start), group); } },
        { key: 'node', label: 'Node', render: function (r) { return nodeName(r.nodeId); } }, { key: 'engine', label: 'Engine' }, { key: 'isp', label: 'ISP', render: function (r) { return r.ispName || 'Unassigned'; } },
        { key: 'down', label: 'Down', num: true, render: function (r) { return F.bytes(r.bytesDown); } },
        { key: 'up', label: 'Up', num: true, render: function (r) { return F.bytes(r.bytesUp); } },
        { key: 'total', label: 'Total', num: true, render: function (r) { return F.bytes(r.totalBytes); } }], rows: series }));
    }
    var groupSelect = U.select([{ value: 'day', label: 'By day' }, { value: 'week', label: 'By week' }, { value: 'month', label: 'By month' }], group, function (v) { usageGroup = v; ST.refresh(); });
    var ispSelect = U.select([{ value: '', label: 'Any ISP' }].concat(ST.ispOptions()), usageISP, function (value) { usageISP = value; usageCache = {}; ST.refresh(); });
    groupSelect.setAttribute('aria-label', 'Bandwidth period');
    ispSelect.setAttribute('aria-label', 'Bandwidth ISP');
    root.appendChild(U.card('Bandwidth', 'Measured download and upload bytes remain separate by node, engine and recorded ISP. Calendar buckets use UTC.', host, [groupSelect, ispSelect]));
  };

  function readBudgets(done) {
    if (ST.demo) { done(demoBudgets, null); return; }
    if (budgets.error) return;
    if (budgets.items) { done(budgets.items, budgets.etag); return; }
    if (budgets.pending) return;
    budgets.pending = ST.api.request('/api/usage/budgets');
    budgets.pending.then(function (response) {
      budgets.items = response.data; budgets.etag = response.etag; budgets.pending = null; budgets.error = null; ST.refresh();
    }, function (error) { budgets.error = error; budgets.pending = null; ST.refresh(); });
  }
  function retryBudgets() {
    budgets.error = null; budgets.items = null; budgets.etag = null; budgets.pending = null;
    readBudgets(); ST.refresh();
  }
  function budgetAction(scope, method, body) {
    var current = ST.demo ? Promise.resolve({ data: demoBudgets, etag: 'demo' }) : ST.api.request('/api/usage/budgets/' + encodeURIComponent(scope), { method: method, body: body, ifMatch: budgets.etag });
    current.then(function (response) {
      if (ST.demo) {
        if (method === 'DELETE') demoBudgets = demoBudgets.filter(function (b) { return b.scopeKey !== scope; });
        else { demoBudgets = demoBudgets.filter(function (b) { return b.scopeKey !== scope; }); demoBudgets.push(Object.assign({}, body, { scopeKey: scope, nodeId: scope === 'global' ? '' : scope, revision: 1 })); }
        ST.setDemoBandwidthBudgets(demoBudgets);
      } else if (method === 'DELETE') { budgets.items = null; budgets.etag = null; }
      else { budgets.items = response.data || budgets.items; budgets.etag = response.etag || budgets.etag; }
      usageCache = {}; adviceCache = {};
      U.toast(method === 'DELETE' ? 'Budget removed' : 'Bandwidth budget saved'); ST.refresh();
    }, function (error) { U.toast(error.status === 412 ? 'Budgets changed elsewhere. Reload and try again.' : error.message, 'error'); });
  }
  ST.bandwidth.budgets = function (root) {
    var host = h('div', { class: 'stack' });
    root.appendChild(U.card('Monthly bandwidth budgets', 'Set an optional global or per-node cap. At 80% the dashboard shows a warning; at 100% the selected schedule policy is applied until next month.', host));
    readBudgets(function () {});
    if (budgets.error) { host.appendChild(U.empty('Budgets are unavailable', budgets.error.message || 'Check the connection, then retry.', U.btn('Retry budgets', { onClick: retryBudgets }))); return; }
    var items = ST.demo ? demoBudgets : budgets.items;
    if (!items) { host.appendChild(h('div', { role: 'status', class: 'empty' }, 'Loading budgets…')); return; }
    var target = U.select([{ value: 'global', label: 'Global' }].concat(ST.nodes().map(function (n) { return { value: n.id, label: n.name }; })), 'global', function () {});
    var amount = U.input('', function () {}, { type: 'number', min: '0.001', step: '0.001', required: true, 'aria-label': 'Monthly budget (GiB)' });
    var action = U.select([{ value: 'widen', label: 'Widen test interval' }, { value: 'pause', label: 'Pause tests' }], 'widen', function () {});
    var multiplier = U.input('2', function () {}, { type: 'number', min: 2, max: 10, step: 1, 'aria-label': 'Interval multiplier' });
    var form = h('form', { class: 'row wrap' }, U.field('Scope', target), U.field('Monthly budget (GiB)', amount), U.field('At 100%', action), U.field('Widen by', multiplier), h('button', { type: 'submit', class: 'btn primary' }, 'Save budget'));
    form.addEventListener('submit', function (event) {
      event.preventDefault(); var bytes = Math.round(Number(amount.value) * 1024 * 1024 * 1024), scope = target.value, kind = action.value;
      if (!(bytes > 0) || kind === 'widen' && (+multiplier.value < 2 || +multiplier.value > 10)) { U.toast('Enter a valid monthly budget and interval multiplier.', 'error'); return; }
      budgetAction(scope, 'PUT', { monthlyBytes: bytes, action: kind, widenMultiplier: kind === 'widen' ? +multiplier.value : 2 });
    });
    host.appendChild(form);
    if (!items.length) host.appendChild(U.empty('No budgets configured', 'Budgets are optional and do not change test schedules until a limit is reached.'));
    items.forEach(function (b) {
      var label = b.scopeKey === 'global' ? 'Global' : nodeName(b.nodeId || b.scopeKey);
      host.appendChild(h('div', { class: 'list-row' }, h('div', { class: 'grow' }, h('b', null, label), h('div', { class: 'muted' }, F.bytes(b.monthlyBytes) + ' per month · ' + (b.action === 'pause' ? 'pause at limit' : 'widen ×' + b.widenMultiplier))),
        U.btn('Remove', { size: 'sm', variant: 'ghost', onClick: function () { budgetAction(b.scopeKey, 'DELETE'); } })));
    });
  };
  function configuredBudget(nodeId) {
    var items = ST.demo ? demoBudgets : budgets.items || [], scoped = items.filter(function (b) { return b.scopeKey === nodeId; })[0];
    if (scoped) return Number(scoped.monthlyBytes) || 0;
    var global = items.filter(function (b) { return b.scopeKey === 'global'; })[0];
    return global ? Number(global.monthlyBytes) || 0 : 0;
  }
  // Demo mode: a node budget is shared by the engines of that node, the global budget by every node and engine. The advice for one engine gets the
  // budget minus what the OTHER engines and nodes of the shared scope already consume at their current interval (as the center does).
  function sharedReserve(node, engine) {
    var items = demoBudgets, nodeBudget = items.filter(function (b) { return b.scopeKey === node.id; })[0];
    var scope = nodeBudget ? [node] : ST.nodes(), since = Date.now() - 30 * 86400000, reserved = 0, complete = true;
    scope.forEach(function (other) {
      ['ookla', 'librespeed'].forEach(function (name) {
        if (other.id === node.id && name === engine) return;
        var values = ST.results({ nodeIds: [other.id], engine: name, from: since }).filter(function (r) {
          return (r.status === 'ok' || r.status === 'degraded') && r.bytesDown != null && r.bytesUp != null;
        }).map(function (r) { return r.bytesDown + r.bytesUp; });
        if (!values.length) { if (other.primaryEngine === name) complete = false; return; }
        reserved += ST.stats.mean(values) * 30 * 1440 / (other.interval || 60);
      });
    });
    return { bytes: reserved, complete: complete };
  }
  ST.bandwidth.advice = function (node, engine, interval, budgetBytes) {
    var key = [node.id, engine, interval, budgetBytes || 0].join('|');
    if (adviceCache[key]) return adviceCache[key];
    var result = adviceCache[key] = /** @type {any} */ ({ loading: true });
    if (ST.demo) {
      var since = Date.now() - 30 * 86400000, values = ST.results({ nodeIds: [node.id], engine: engine, from: since }).filter(function (r) {
        return (r.status === 'ok' || r.status === 'degraded') && r.bytesDown != null && r.bytesUp != null;
      }).map(function (r) { return r.bytesDown + r.bytesUp; });
      var shared = sharedReserve(node, engine), total = budgetBytes || configuredBudget(node.id), budget = total > 0 ? Math.max(0, total - shared.bytes) : 0;
      var available = values.length > 0, avg = available ? ST.stats.mean(values) : null;
      var perDay = available ? avg * 1440 / interval : null, perMonth = available ? perDay * 30 : null;
      var recommendation = available ? recommendInDemo(avg, budget) : { interval: null, status: 'unavailable' };
      result.loading = false; result.data = { averageBytesPerTest: avg, estimatedBytesPerDay: perDay, estimatedBytesPerMonth: perMonth, budgetBytes: total,
        measurementAvailable: available, sharedMeasurementsAvailable: shared.complete, sharedReservedBytes: shared.bytes, recommendedIntervalMinutes: recommendation.interval, recommendationStatus: recommendation.status };
    } else {
      var path = '/api/usage/advice?node=' + encodeURIComponent(node.id) + '&engine=' + engine + '&interval=' + interval;
      if (budgetBytes > 0) path += '&budgetBytes=' + encodeURIComponent(budgetBytes);
      ST.api.request(path)
        .then(function (response) { result.loading = false; result.data = response.data; if (result.refresh) result.refresh(); }, function (error) { result.loading = false; result.error = error; if (result.refresh) result.refresh(); });
    }
    return result;
  };
  ST.bandwidth.advicePanel = function (root, onlyNodeId, scheduleDrafts) {
    var nodes = onlyNodeId ? [ST.node(onlyNodeId)].filter(Boolean) : ST.nodes(), rows = [];
    nodes.forEach(function (node) {
      var scheduled = scheduleDrafts || ST.engineSchedules(node), enginesForNode = onlyNodeId ? scheduled.map(function (x) { return x.engine; }) : [ST.selectedEngine || node.primaryEngine || 'ookla'];
      enginesForNode.forEach(function (engine) {
        var schedule = scheduled.filter(function (x) { return x.engine === engine; })[0];
        var interval = schedule ? schedule.interval : node.interval || 60, draft = adviceDraft(node.id, engine, interval);
        rows.push({ node: node, engine: engine, draft: draft });
      });
    });
    root.appendChild(U.card('Bandwidth cost advice', 'Estimated from recent measurements for the chosen engine and interval.', h('div', { class: 'stack' },
      rows.map(function (r) {
        return adviceRow(r.node, r.engine, r.draft);
      })
    )));
  };

  function adviceRow(node, engine, draft) {
    var row = h('div', { class: 'card stack' }), detail = h('p', { class: 'hint' }), recommendation = h('div');
    var intervalInput = U.input(draft.interval, function (value) { draft.interval = value; }, { type: 'number', min: 5, max: 1440, step: 1, 'aria-label': node.name + ' ' + engine + ' proposed interval in minutes', onBlur: function () { ST.bandwidth.setDraftInterval(node.id, engine, intervalInput.value); refreshEstimate(); } });
    var budgetInput = U.input(draft.budgetGiB, function (value) { draft.budgetGiB = value; }, { type: 'number', min: 0.001, step: 0.001, 'aria-label': node.name + ' ' + engine + ' proposed monthly budget in GiB', onBlur: refreshEstimate });
    var controls = h('div', { class: 'row wrap' }, U.field('Interval (minutes)', intervalInput), U.field('Budget (GiB per month)', budgetInput));
    function refreshEstimate() {
      var interval = Math.max(5, Math.min(1440, Math.round(Number(draft.interval) || 60)));
      var budget = Math.max(0, Math.round(Number(draft.budgetGiB) * 1024 * 1024 * 1024) || 0);
      var state = ST.bandwidth.advice(node, engine, interval, budget);
      var text = state.loading ? 'Calculating estimate…' : state.error ? 'Advice is unavailable. Check the connection and try again.' : !state.data.measurementAvailable ? 'Unavailable — no complete byte measurements.' : F.bytes(state.data.estimatedBytesPerDay) + ' per day · ' + F.bytes(state.data.estimatedBytesPerMonth) + ' per month';
      detail.textContent = 'Estimate for every ' + interval + ' minutes: ' + text;
      U.clear(recommendation);
      var message = adviceRecommendation(state);
      if (message) recommendation.appendChild(h('p', { class: 'hint', role: 'status' }, message));
      state.refresh = refreshEstimate;
    }
    row.appendChild(h('div', { class: 'row gap-sm' }, U.dot(ST.nodeColor(node)), h('b', null, node.name + ' · ' + engine)));
    row.appendChild(controls);
    row.appendChild(detail);
    row.appendChild(recommendation);
    refreshEstimate();
    return row;
  }

  function adviceRecommendation(state) {
    if (state.loading || state.error || !state.data) return '';
    if (!state.data.measurementAvailable) return state.data.budgetBytes > 0 ? 'A budget is set, but there are no complete byte measurements yet. Run tests that report both download and upload byte counters.' : 'Run tests with both byte counters to get estimates and interval advice.';
    if (!state.data.sharedMeasurementsAvailable) return 'Some enabled schedules sharing this budget lack complete byte measurements, so interval advice is unavailable.';
    if (state.data.recommendationStatus === 'feasible' && state.data.recommendedIntervalMinutes) return 'Recommended minimum interval for this budget: ' + state.data.recommendedIntervalMinutes + ' minutes.';
    if (state.data.recommendationStatus === 'infeasible') return 'This budget cannot be met even at a 24-hour interval. Increase the budget or reduce test volume.';
    return 'Enter a monthly budget to get an interval recommendation.';
  }
})();

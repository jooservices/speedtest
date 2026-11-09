/* State, persistence (localStorage), formatting, queries, statistics, alert evaluation. */
(function () {
  'use strict';
  var ST = window.ST;
  var KEY = 'st.demo.v1';
  ST.demo = new URLSearchParams(location.search).get('demo') === '1';
  var MIN = 60000, HOUR = 3600000, DAY = 86400000;
  ST.MIN = MIN; ST.HOUR = HOUR; ST.DAY = DAY;
  ST.ranges = { '6h': 6 * HOUR, '24h': DAY, '7d': 7 * DAY, '30d': 30 * DAY };
  ST.rangeLabels = { '6h': 'Last 6 hours', '24h': 'Last 24 hours', '7d': 'Last 7 days', '30d': 'Last 30 days' };
  ST.palette = [null, 's1', 's2', 's3', 's4', 's5', 's6', 's7', 's8'];

  /* ---------- Tiny event bus ---------- */
  var handlers = {};
  ST.on = function (ev, fn) { (handlers[ev] = handlers[ev] || []).push(fn); return function () { handlers[ev] = handlers[ev].filter(function (f) { return f !== fn; }); }; };
  ST.emit = function (ev, data) { (handlers[ev] || []).slice().forEach(function (f) { try { f(data); } catch (e) { console.error(e); } }); };

  /* ---------- Settings ---------- */
  var defaultSettings = function () {
    return {
      theme: 'dark', range: '7d', units: 'auto', timezone: 'browser', hour12: false, decimals: 1,
      retentionDays: 365, geoProvider: 'geolite2', geoFallback: 'ip-api', geoKey: '', geoAutoUpdate: true,
      engine: 'ookla', defaultThreads: 4, serverMode: 'auto', maxConcurrent: 1, stagger: true, quietStart: '', quietEnd: '', notifyQuietStart: '', notifyQuietEnd: '', notifyQuietTimezone: 'UTC', notifyQuietCritical: true,
      notify: { email: { on: true, to: 'ops@example.com' }, telegram: { on: false, to: '' }, slack: { on: false, to: '' }, webhook: { on: false, to: '' } },
      enrollToken: 'st_enroll_' + Math.random().toString(36).slice(2, 10) + Math.random().toString(36).slice(2, 10),
      centerUrl: 'https://speedtest.example.com'
    };
  };

  var defaultRules = function () {
    return [
      { id: 'r1', name: 'Download far below expected', metric: 'down_pct', op: '<', value: 60, forTests: 3, nodes: 'all', severity: 'warn', channels: ['email'], enabled: true },
      { id: 'r2', name: 'Tests are failing', metric: 'failed', op: '>=', value: 1, forTests: 3, nodes: 'all', severity: 'crit', channels: ['email'], enabled: true },
      { id: 'r3', name: 'High latency', metric: 'ping', op: '>', value: 60, forTests: 3, nodes: 'all', severity: 'warn', channels: ['email'], enabled: true },
      { id: 'r4', name: 'Packet loss', metric: 'loss', op: '>', value: 2, forTests: 2, nodes: 'all', severity: 'warn', channels: ['email'], enabled: true },
      { id: 'r5', name: 'Public IP address changed', metric: 'ip_changed', op: '=', value: 1, forTests: 1, nodes: ['n1', 'n2', 'n3', 'n4'], severity: 'info', channels: ['email'], enabled: true }
    ];
  };

  var state = ST.state = { nodes: [], results: [], settings: defaultSettings(), rules: defaultRules(), acked: {}, events: [], now: Date.now() };

  function load() {
    var saved = {};
    try { saved = JSON.parse(localStorage.getItem(KEY) || '{}'); } catch (e) { saved = {}; }
    state.now = Date.now();
    state.nodes = saved.nodes || ST.seedNodes();
    state.settings = Object.assign(defaultSettings(), saved.settings || {});
    state.settings.notify = Object.assign(defaultSettings().notify, (saved.settings || {}).notify || {});
    state.rules = saved.rules || defaultRules();
    state.acked = saved.acked || {};
    state.extra = saved.extra || [];
    var seedNodes = ST.seedNodes();
    var seeded = ST.buildSeedResults(seedNodes, state.now);
    var alive = {}; state.nodes.forEach(function (n) { alive[n.id] = 1; });
    state.results = seeded.filter(function (r) { return alive[r.nodeId]; }).concat(state.extra.filter(function (r) { return alive[r.nodeId]; }));
    state.results.sort(function (a, b) { return a.ts - b.ts; });
    reindex();
    ST.recomputeEvents();
  }
  function persist() {
    if (!ST.demo) return;
    try {
      localStorage.setItem(KEY, JSON.stringify({ nodes: state.nodes, settings: state.settings, rules: state.rules, acked: state.acked, extra: state.extra }));
    } catch (e) { /* storage full or disabled: demo keeps working in memory */ }
  }
  ST.persist = persist;
  ST.resetDemo = function () { if (!ST.demo) return; try { localStorage.removeItem(KEY); } catch (e) {} location.reload(); };

  var byNode = {};
  function reindex() {
    byNode = {};
    state.nodes.forEach(function (n) { byNode[n.id] = []; });
    state.results.forEach(function (r) { if (byNode[r.nodeId]) byNode[r.nodeId].push(r); });
  }

  /* ---------- Settings / nodes API ---------- */
  ST.settings = function () { return state.settings; };
  ST.timezone = function () {
    var zone = state.settings.timezone;
    return zone && zone !== 'browser' ? zone : (Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC');
  };
  ST.setSettings = function (patch, silent) { Object.assign(state.settings, patch); persist(); if (!silent) ST.emit('settings'); };
  ST.nodes = function () { return state.nodes; };
  ST.node = function (id) { return state.nodes.filter(function (n) { return n.id === id; })[0]; };
  ST.nodeColor = function (node) { return 'var(--' + ST.palette[((node.colorSlot - 1) % 8) + 1] + ')'; };
  ST.nodeTitle = function (n) { return n.name + ' · ' + n.label; };
  ST.updateNode = function (id, patch) { var n = ST.node(id); if (!n) return; Object.assign(n, patch); persist(); ST.emit('data'); };
  ST.addNode = function (n) {
    var used = state.nodes.map(function (x) { return x.colorSlot; });
    var slot = 1; while (used.indexOf(slot) >= 0) slot++;
    var id = 'n' + (Math.max.apply(null, state.nodes.map(function (x) { return +x.id.slice(1); }).concat([0])) + 1);
    var node = Object.assign({
      id: id, colorSlot: slot, interval: 30, offset: state.nodes.length * 3 % 30, threads: 4, server: 'auto', enabled: true,
      agent: '1.4.2', note: '', createdAt: Date.now(), pref: [1001, 2001],
      profile: { effD: .88, effU: .86, cong: .15, ping: 13, jit: 2, loss: .05 },
      ipModel: { kind: 'dynamic', prefix: '198.51.100', segSlots: 7 * 48, off: 3 + state.nodes.length * 17 }
    }, n, { id: id, colorSlot: slot });
    state.nodes.push(node); byNode[id] = []; persist(); ST.emit('data'); return node;
  };
  ST.removeNode = function (id) {
    state.nodes = state.nodes.filter(function (n) { return n.id !== id; });
    state.results = state.results.filter(function (r) { return r.nodeId !== id; });
    state.extra = state.extra.filter(function (r) { return r.nodeId !== id; });
    reindex(); ST.recomputeEvents(); persist(); ST.emit('data');
  };
  ST.rules = function () { return state.rules; };
  ST.saveRule = function (rule) {
    var i = state.rules.findIndex(function (r) { return r.id === rule.id; });
    if (i < 0) { rule.id = 'r' + Date.now().toString(36); state.rules.push(rule); } else state.rules[i] = rule;
    persist(); ST.recomputeEvents(); ST.emit('data');
  };
  ST.deleteRule = function (id) { state.rules = state.rules.filter(function (r) { return r.id !== id; }); persist(); ST.recomputeEvents(); ST.emit('data'); };

  ST.selectedEngine = '';
  ST.engineQuery = function () { return ST.selectedEngine ? '&engine=' + encodeURIComponent(ST.selectedEngine) : ''; };
  ST.engineSelector = function () {
    return ST.ui.field('Engine', ST.ui.select([{ value: '', label: 'Primary engine' }, { value: 'ookla', label: 'Ookla' }, { value: 'librespeed', label: 'LibreSpeed' }], ST.selectedEngine, function (value) {
      ST.selectedEngine = value;
      if (ST.demo) ST.refresh(); else ST.load();
    }));
  };
  ST.engineSchedules = function (node) { return node.engineSchedules || [{ engine: 'ookla', interval: node.interval, offset: node.offset || 0, enabled: node.enabled }]; };
  ST.saveEngineSchedules = function (id, engines) { var n = ST.node(id); n.engineSchedules = engines; persist(); ST.emit('data'); return Promise.resolve(); };

  /* ---------- Queries ---------- */
  ST.now = function () { return Date.now(); };
  ST.nodeResults = function (id) { return ST.results({nodeIds:[id]}); };
  function effectiveISP(result) {
    var value = String(result.engineISP || '').trim();
    var geo = result.geo || (ST.geo && ST.geo(result.ip)) || {};
    if (!value) value = String(geo.organisation || geo.asnOrg || geo.isp || '').trim();
    return value || null;
  }
  ST.results = function (o) {
    o = o || {};
    var ids = o.nodeIds || state.nodes.map(function (n) { return n.id; });
    var from = o.from == null ? 0 : o.from, to = o.to == null ? Infinity : o.to, out = [];
    ids.forEach(function (id) {
      var arr = byNode[id] || [];
      for (var i = 0; i < arr.length; i++) {
        var r = arr[i];
        if (r.ts < from || r.ts > to) continue;
        if ((r.engine || 'ookla') !== (o.engine || ST.selectedEngine || (ST.node(id) || {}).primaryEngine || 'ookla')) continue;
        if (o.isp && (!effectiveISP(r) || effectiveISP(r).toLowerCase() !== o.isp.toLowerCase())) continue;
        if (o.status && o.status !== 'all' && r.status !== o.status) continue;
        out.push(r);
      }
    });
    out.sort(function (a, b) { return a.ts - b.ts; });
    return out;
  };
  ST.rangeFrom = function (key) { return Date.now() - ST.ranges[key || state.settings.range]; };
  ST.latest = function (id) { var a = ST.nodeResults(id); for (var i = a.length - 1; i >= 0; i--) return a[i]; return null; };
  ST.latestOk = function (id) { var a = ST.nodeResults(id); for (var i = a.length - 1; i >= 0; i--) if (a[i].status !== 'failed') return a[i]; return null; };
  ST.server = function (id) { return ST.servers.filter(function (s) { return s.id === id; })[0] || { id: id, name: 'Unknown server', city: '', sponsor: '' }; };

  /* ---------- Statistics ---------- */
  var S = ST.stats = {};
  S.mean = function (a) { return a.length ? a.reduce(function (x, y) { return x + y; }, 0) / a.length : null; };
  S.sum = function (a) { return a.reduce(function (x, y) { return x + y; }, 0); };
  S.pct = function (sorted, p) {
    if (!sorted.length) return null;
    var i = (sorted.length - 1) * p, lo = Math.floor(i), hi = Math.ceil(i);
    return sorted[lo] + (sorted[hi] - sorted[lo]) * (i - lo);
  };
  S.effectiveISP = effectiveISP;
  S.weekChange = function (current, previous) {
    return current == null || previous == null || previous === 0 ? null : (current - previous) / previous * 100;
  };
  S.std = function (a) { if (a.length < 2) return 0; var m = S.mean(a); return Math.sqrt(S.mean(a.map(function (x) { return (x - m) * (x - m); }))); };
  S.summary = function (values) {
    var v = values.filter(function (x) { return x != null; }).sort(function (a, b) { return a - b; });
    if (!v.length) return { n: 0, mean: null, median: null, min: null, max: null, p5: null, p50: null, p95: null, p99: null, std: null };
    var p50 = S.pct(v, .5);
    return { n: v.length, mean: S.mean(v), median: p50, min: v[0], max: v[v.length - 1], p5: S.pct(v, .05), p50: p50, p95: S.pct(v, .95), p99: S.pct(v, .99), std: S.std(v) };
  };
  S.metricValue = function (r, m) {
    if (r.status === 'failed') return null;
    var n = ST.node(r.nodeId);
    switch (m) {
      case 'down': return r.down; case 'up': return r.up; case 'ping': return r.ping; case 'jitter': return r.jitter; case 'loss': return r.loss;
      case 'down_pct': return n ? r.down / n.expectedDown * 100 : null;
      case 'up_pct': return n ? r.up / n.expectedUp * 100 : null;
    }
    return null;
  };
  ST.metrics = {
    down: { label: 'Download', short: 'Down', unit: 'speed', good: 'high' },
    up: { label: 'Upload', short: 'Up', unit: 'speed', good: 'high' },
    ping: { label: 'Latency', short: 'Ping', unit: 'ms', good: 'low' },
    jitter: { label: 'Jitter', short: 'Jitter', unit: 'ms', good: 'low' },
    loss: { label: 'Packet loss', short: 'Loss', unit: '%', good: 'low' }
  };
  S.nodeSummary = function (id, from, to, isp) {
    var rs = ST.results({ nodeIds: [id], from: from, to: to, isp: isp }), ok = rs.filter(function (r) { return r.status !== 'failed'; }), n = ST.node(id);
    var ips = {}; rs.forEach(function (r) { ips[r.ip] = 1; });
    var sum = {
      count: rs.length, ok: ok.length, failed: rs.length - ok.length, successRate: rs.length ? ok.length / rs.length * 100 : null,
      down: S.summary(ok.map(function (r) { return r.down; })), up: S.summary(ok.map(function (r) { return r.up; })),
      ping: S.summary(ok.map(function (r) { return r.ping; })), jitter: S.summary(ok.map(function (r) { return r.jitter; })),
      loss: S.summary(ok.map(function (r) { return r.loss; })), ipCount: Object.keys(ips).length
    };
    sum.downPct = n && sum.down.mean != null ? sum.down.mean / n.expectedDown * 100 : null;
    sum.upPct = n && sum.up.mean != null ? sum.up.mean / n.expectedUp * 100 : null;
    // Stability: coefficient of variation of download, mapped to 0-100 (lower variation = more stable).
    sum.stability = sum.down.mean ? Math.max(0, Math.min(100, 100 - (sum.down.std / sum.down.mean) * 220)) : null;
    return sum;
  };

  /* ---------- IP history ---------- */
  ST.ipHistory = function (id) {
    var rs = ST.nodeResults(id), map = {}, order = [], changes = [], prev = null;
    rs.forEach(function (r) {
      var e = map[r.ip];
      if (!e) { e = map[r.ip] = { ip: r.ip, first: r.ts, last: r.ts, tests: 0, down: [], up: [] }; order.push(e); }
      e.last = r.ts; e.tests++;
      if (r.status !== 'failed') { e.down.push(r.down); e.up.push(r.up); }
      if (prev && prev !== r.ip) changes.push({ ts: r.ts, from: prev, to: r.ip });
      prev = r.ip;
    });
    order.forEach(function (e) { e.avgDown = S.mean(e.down); e.avgUp = S.mean(e.up); e.geo = ST.geo(e.ip); });
    return { ips: order, changes: changes };
  };

  /* ---------- Alert evaluation ---------- */
  function ruleMatch(rule, r, node, prevIp) {
    switch (rule.metric) {
      case 'failed': return r.status === 'failed';
      case 'ip_changed': return prevIp != null && prevIp !== r.ip;
      default:
        if (r.status === 'failed') return false;
        var v = S.metricValue(r, rule.metric);
        if (v == null) return false;
        return rule.op === '<' ? v < rule.value : rule.op === '>' ? v > rule.value : rule.op === '>=' ? v >= rule.value : rule.op === '<=' ? v <= rule.value : v === rule.value;
    }
  }
  ST.ruleText = function (rule) {
    var m = { down_pct: 'Download', up_pct: 'Upload', ping: 'Latency', jitter: 'Jitter', loss: 'Packet loss', failed: 'Failed tests', ip_changed: 'Public IP changes' };
    var u = { down_pct: '% of expected', up_pct: '% of expected', ping: ' ms', jitter: ' ms', loss: '%' };
    if (rule.metric === 'ip_changed') return 'Public IP address changes';
    if (rule.metric === 'failed') return rule.forTests + ' failed tests in a row';
    return m[rule.metric] + ' ' + rule.op + ' ' + rule.value + (u[rule.metric] || '') + ' for ' + rule.forTests + ' tests in a row';
  };
  ST.recomputeEvents = function () {
    var events = [];
    state.rules.forEach(function (rule) {
      if (!rule.enabled) return;
      state.nodes.forEach(function (node) {
        if (rule.nodes !== 'all' && rule.nodes.indexOf(node.id) < 0) return;
        var streak = 0, open = null, prevIp = null;
        (byNode[node.id] || []).forEach(function (r) {
          var hit = ruleMatch(rule, r, node, prevIp);
          prevIp = r.ip;
          if (hit) {
            streak++;
            if (streak === rule.forTests) {
              open = {
                id: rule.id + ':' + node.id + ':' + r.ts, ruleId: rule.id, nodeId: node.id, severity: rule.severity, ts: r.ts, resolvedAt: null,
                title: rule.name,
                detail: rule.metric === 'ip_changed' ? 'Now ' + r.ip + (ST.geo(r.ip) ? ' (' + ST.geo(r.ip).city + ')' : '') : ST.ruleText(rule),
                triggerResultId: r.id,
                engine: r.engine || node.primaryEngine || 'ookla'
              };
              events.push(open);
            }
          } else {
            if (open && rule.metric !== 'ip_changed') { open.resolvedAt = r.ts; }
            if (rule.metric === 'ip_changed') { /* one-shot */ }
            streak = 0; open = null;
          }
          if (rule.metric === 'ip_changed' && hit) { open = null; streak = 0; }
        });
      });
    });
    events.sort(function (a, b) { return b.ts - a.ts; });
    events.forEach(function (e) { e.acked = !!state.acked[e.id]; });
    state.events = events;
  };
  ST.events = function () { return state.events; };
  ST.openEventCount = function () { return state.events.filter(function (e) { return !e.acked && e.severity !== 'info' && (!e.resolvedAt || Date.now() - e.resolvedAt < 3 * DAY); }).length; };
  ST.ack = function (id, on) { if (on === false) delete state.acked[id]; else state.acked[id] = 1; persist(); ST.recomputeEvents(); ST.emit('data'); };
  ST.ackAll = function () { state.events.forEach(function (e) { state.acked[e.id] = 1; }); persist(); ST.recomputeEvents(); ST.emit('data'); };

  /* ---------- Formatting ---------- */
  var F = ST.fmt = {};
  function tz() { var t = state.settings.timezone; return t === 'browser' ? undefined : t; }
  var dtCache = {};
  function dtf(opts) {
    var k = JSON.stringify(opts) + state.settings.timezone + state.settings.hour12;
    return dtCache[k] || (dtCache[k] = new Intl.DateTimeFormat('en-GB', Object.assign({ timeZone: tz(), hour12: state.settings.hour12 }, opts)));
  }
  F.date = function (ts, kind) {
    if (ts == null) return '—';
    switch (kind) {
      case 'day': return dtf({ day: '2-digit', month: 'short' }).format(ts);
      case 'time': return dtf({ hour: '2-digit', minute: '2-digit' }).format(ts);
      case 'full': return dtf({ day: '2-digit', month: 'short', year: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit' }).format(ts);
      case 'dayfull': return dtf({ weekday: 'short', day: '2-digit', month: 'short' }).format(ts);
      default: return dtf({ day: '2-digit', month: 'short', hour: '2-digit', minute: '2-digit' }).format(ts);
    }
  };
  var partsFmt = {};
  F.parts = function (ts) {
    var k = state.settings.timezone;
    var f = partsFmt[k] || (partsFmt[k] = new Intl.DateTimeFormat('en-US', { timeZone: tz(), hour: 'numeric', hourCycle: 'h23', weekday: 'short', year: 'numeric', month: '2-digit', day: '2-digit' }));
    var o = {}; f.formatToParts(ts).forEach(function (p) { o[p.type] = p.value; });
    var wd = { Mon: 0, Tue: 1, Wed: 2, Thu: 3, Fri: 4, Sat: 5, Sun: 6 }[o.weekday];
    return { hour: +o.hour % 24, wd: wd, key: o.year + '-' + o.month + '-' + o.day };
  };
  F.rel = function (ts) {
    if (ts == null) return '—';
    var s = Math.round((Date.now() - ts) / 1000);
    if (s < 0) { s = -s; return 'in ' + F.dur(s * 1000); }
    if (s < 45) return 'just now';
    if (s < 3600) return Math.round(s / 60) + ' min ago';
    if (s < 86400) return Math.round(s / 3600) + ' h ago';
    return Math.round(s / 86400) + ' d ago';
  };
  F.dur = function (ms) {
    var s = Math.round(ms / 1000);
    if (s < 60) return s + 's';
    if (s < 3600) return Math.floor(s / 60) + 'm ' + (s % 60 ? (s % 60) + 's' : '');
    if (s < 86400) return Math.floor(s / 3600) + 'h ' + Math.floor((s % 3600) / 60) + 'm';
    return Math.floor(s / 86400) + 'd ' + Math.floor((s % 86400) / 3600) + 'h';
  };
  F.num = function (v, d) { return v == null ? '—' : Number(v).toLocaleString('en-US', { minimumFractionDigits: d == null ? 0 : d, maximumFractionDigits: d == null ? 0 : d }); };
  F.speedParts = function (mbps) {
    if (mbps == null) return { v: '—', u: '' };
    var u = state.settings.units, d = state.settings.decimals;
    var whole = function (x) { return Math.abs(x - Math.round(x)) < 1e-9; };
    if (u === 'MBps') return { v: F.num(mbps / 8, whole(mbps / 8) ? 0 : d), u: 'MB/s' };
    if (u === 'Gbps' || (u === 'auto' && mbps >= 1000)) return { v: F.num(mbps / 1000, whole(mbps / 1000) ? 0 : 2), u: 'Gbps' };
    return { v: F.num(mbps, whole(mbps) ? 0 : d), u: 'Mbps' };
  };
  F.axisSpeed = function (mbps) {
    var u = state.settings.units;
    if (u === 'MBps') return F.num(mbps / 8, mbps / 8 >= 100 ? 0 : 1);
    if (u === 'Gbps') return F.num(mbps / 1000, 1);
    if (mbps >= 1000) return F.num(mbps / 1000, mbps % 1000 ? 1 : 0) + 'G';
    return F.num(mbps, 0);
  };
  F.axisUnit = function () { var u = ST.settings().units; return u === 'MBps' ? 'MB/s' : u === 'Gbps' ? 'Gbps' : u === 'Mbps' ? 'Mbps' : 'Mbps (G = Gbps)'; };
  F.speed = function (mbps) { var p = F.speedParts(mbps); return mbps == null ? '—' : p.v + ' ' + p.u; };
  F.ms = function (v) { return v == null ? '—' : F.num(v, 1) + ' ms'; };
  F.pct = function (v, d) { return v == null ? '—' : F.num(v, d == null ? 0 : d) + '%'; };
  F.bytes = function (b) { if (!b) return '—'; var u = ['B', 'KB', 'MB', 'GB']; var i = 0; while (b >= 1024 && i < 3) { b /= 1024; i++; } return F.num(b, i ? 1 : 0) + ' ' + u[i]; };
  F.metric = function (m, v) {
    if (v == null) return '—';
    var info = ST.metrics[m]; if (!info) return F.num(v, 1);
    return info.unit === 'speed' ? F.speed(v) : info.unit === 'ms' ? F.ms(v) : F.num(v, 1) + '%';
  };

  /* ---------- Running a test (simulated) ---------- */
  ST.runTest = function (nodeId, onProgress) {
    var node = ST.node(nodeId);
    return new Promise(function (resolve) {
      var byId = {}; state.nodes.forEach(function (n) { byId[n.id] = n; });
      var now = Date.now(), slot = Math.floor(now / ST.util.SLOT);
      var rnd = ST.util.rng(nodeId + ':manual:' + now);
      var res = ST.genSample(node, now, rnd, -1, byId, slot);
      if (res.status === 'failed' && rnd() < 0.8) { res.status = 'ok'; res = ST.genSample(Object.assign({}, node), now, function () { return 0.5 + (rnd() - .5) * .3; }, -1, byId, slot); }
      res.id = 'rm_' + now + '_' + nodeId; res.manual = true;
      var srv = ST.server(res.serverId);
      var phases = [
        { name: 'Finding the best server', ms: 900 },
        { name: 'Measuring latency', ms: 1100, value: res.ping, unit: 'ms' },
        { name: 'Testing download', ms: 3200, value: res.down, unit: 'speed' },
        { name: 'Testing upload', ms: 2800, value: res.up, unit: 'speed' },
        { name: 'Saving result', ms: 500 }
      ];
      var total = phases.reduce(function (a, p) { return a + p.ms; }, 0), elapsed = 0, i = 0;
      (function step() {
        if (i >= phases.length) {
          state.extra.push(res); state.results.push(res); state.results.sort(function (a, b) { return a.ts - b.ts; });
          reindex(); ST.recomputeEvents(); persist(); ST.emit('data'); resolve(res); return;
        }
        var p = phases[i], t0 = Date.now();
        (function tick() {
          var f = Math.min(1, (Date.now() - t0) / p.ms);
          var val = p.value != null ? p.value * (p.unit === 'ms' ? (1.8 - .8 * f) : (0.35 + 0.65 * f) * (1 + (Math.random() - .5) * .08)) : null;
          if (onProgress) onProgress({ phase: p.name, index: i, count: phases.length, value: val, unit: p.unit, pct: (elapsed + p.ms * f) / total * 100, server: srv });
          if (f < 1 && document.body) setTimeout(tick, 90); else { elapsed += p.ms; i++; step(); }
        })();
      })();
    });
  };

  ST.csv = function (rows) {
    var head = ['time', 'node', 'status', 'download_mbps', 'upload_mbps', 'ping_ms', 'jitter_ms', 'packet_loss_pct', 'loaded_latency_down_ms', 'loaded_latency_up_ms', 'server', 'public_ip', 'error', 'engine'];
    var esc = function (v) { v = v == null ? '' : String(v); return /[",\n]/.test(v) ? '"' + v.replace(/"/g, '""') + '"' : v; };
    return [head.join(',')].concat(rows.map(function (r) {
      var n = ST.node(r.nodeId), s = ST.server(r.serverId);
      return [new Date(r.ts).toISOString(), n ? n.name + ' ' + n.label : r.nodeId, r.status, r.down, r.up, r.ping, r.jitter, r.loss, r.dLat, r.uLat, s.name + ' - ' + s.city, r.ip, r.error, r.engine || 'ookla'].map(esc).join(',');
    })).join('\n');
  };

  /* API-backed snapshot; reads retain the preview API for existing page renderers.
     Server-specific page queries and workflows are composed by JOOST-38/39/40/41. */
  function initRemote() {
    state.rules = []; state.extra = [];
    state.settings.geoKey = ''; state.settings.enrollToken = '';
    state.settings.centerUrl = location.origin;
    state.settings.notify = { email: { on: false, to: '' }, telegram: { on: false, to: '' },
      slack: { on: false, to: '' }, webhook: { on: false, to: '' } };
    ST.servers = [];
    var geos = {}, addresses = [], changes = [], schedules = { rules: {}, nodes: [], clashes: [] }, controller, revision = 0;
    var API = ST.api;
    ST.remote = { status: 'idle', error: null };
    ST.geo = function (ip) { return geos[ip] || null; };
    ST.recomputeEvents = function () {}; // Events are evaluated by the center.
    ST.ipHistory = function (id) {
      return { ips: addresses.filter(function (a) { return a.nodeId === id; }),
        changes: changes.filter(function (c) { return c.nodeId === id; }) };
    };
    function timestamp(value) { return value == null ? null : Date.parse(value); }
    function result(r) { return Object.assign({}, r, { ts: timestamp(r.ts) }); }
    function remember(r) {
      if (r.server && !ST.servers.some(function (s) { return s.id === r.server.id; })) ST.servers.push(r.server);
      if (r.geo) geos[r.ip] = r.geo;
    }
    ST.cancelLoad = function () {
      revision++;
      if (controller) controller.abort();
      ST.remote.status = 'idle';
    };
    ST.load = async function () {
      ST.cancelLoad();
      var current = revision;
      controller = new AbortController();
      var options = { signal: controller.signal };
      ST.remote = { status: 'loading', error: null };
      ST.emit('remote');
      try {
        var session = await API.session(options);
        if (!session.data.user) { API.signIn(); throw new Error('Sign in to view your nodes.'); }
        var settings = await API.request('/api/settings', options);
        var range = settings.data.range;
        var snapshot = await Promise.all([
          API.list('/api/nodes', options),
          API.list('/api/results?range=' + encodeURIComponent(range) + ST.engineQuery(), options),
          API.list('/api/alerts/rules', options),
          // state=all: acknowledged alerts stay visible under "All" and can be reopened (the center's default is open only).
          API.list('/api/alerts/events?state=all&range=' + encodeURIComponent(range), options),
          API.list('/api/ip/addresses', options),
          API.list('/api/ip/history', options),
          API.request('/api/schedules', options)
        ]);
        if (current !== revision) return;
        geos = {}; ST.servers = [];
        state.settings = Object.assign({}, state.settings, settings.data, { geoKey: '', enrollToken: '' });
        state.nodes = snapshot[0].map(function (n) {
          if (n.geo && n.currentIp) geos[n.currentIp] = n.geo;
          return Object.assign({}, n, { agent: String(n.agent || '').replace(/^speedtest-agent\//, ''), createdAt: timestamp(n.createdAt), lastSeen: timestamp(n.lastSeen),
            latest: n.latest && result(n.latest), latestGood: n.latestGood && result(n.latestGood) });
        });
        state.results = snapshot[1].map(result).sort(function (a, b) { return a.ts - b.ts; });
        state.results.forEach(remember);
        state.rules = snapshot[2];
        state.events = snapshot[3].map(function (e) { return Object.assign({}, e, { ts: timestamp(e.ts), resolvedAt: timestamp(e.resolvedAt) }); });
        addresses = snapshot[4].map(function (a) {
          if (a.geo) geos[a.ip] = a.geo;
          return Object.assign({}, a, { first: timestamp(a.first), last: timestamp(a.last) });
        });
        changes = snapshot[5].map(function (c) { return Object.assign({}, c, { ts: timestamp(c.ts) }); });
        schedules = snapshot[6].data;
        reindex();
        ST.remote = { status: 'ready', error: null };
        ST.emit('settings');
      } catch (error) {
        if (current !== revision) return;
        controller.abort();
        ST.remote = { status: 'error', error: error };
      }
      if (current === revision) ST.emit('remote');
    };
    // These methods must never mutate a local simulation in real mode. Return
    // promises so page workflow tickets can await server confirmation.
    function mutation(path, method, body, extra) {
      return API.request(path, Object.assign({ method: method, body: body }, extra)).then(async function (response) {
        await ST.load();
        return response.data;
      });
    }
    // Read the current ETag, write with it, and try once more with a fresh one when the center says the
    // resource changed in between (for example by the owner's own previous change still being applied).
    var writeChains = {};
    function conditional(path, send) {
      function attempt(again) {
        return API.request(path).then(send).catch(function (error) {
          if (again && error && error.status === 412) return attempt(false);
          throw error;
        });
      }
      // Writes to one resource go one after the other: two quick changes (a select changed twice) must not use the
      // same ETag, which would make the second one fail with 412.
      var turn = (writeChains[path] || Promise.resolve()).catch(function () {}).then(function () { return attempt(true); });
      writeChains[path] = turn;
      return turn;
    }
    function editable(path, method, body, contentType) {
      return conditional(path, function (response) {
        return mutation(path, method, body, { ifMatch: response.etag, contentType: contentType });
      });
    }
    ST.updateNode = function (id, patch) {
      return editable('/api/nodes/' + encodeURIComponent(id), 'PATCH', patch, 'application/merge-patch+json');
    };
    ST.addNode = function (node) { return mutation('/api/nodes', 'POST', node, { idempotencyKey: API.key() }); };
    ST.removeNode = function (id) {
      var path = '/api/nodes/' + encodeURIComponent(id);
      return API.request(path).then(function (response) {
        return mutation(path + '?confirm=true', 'DELETE', undefined, { ifMatch: response.etag });
      });
    };
    ST.saveRule = function (rule) {
      var input = Object.assign({}, rule); delete input.id;
      if (rule.id) return editable('/api/alerts/rules/' + encodeURIComponent(rule.id), 'PUT', input);
      return mutation('/api/alerts/rules', 'POST', input, { idempotencyKey: API.key() });
    };
    ST.deleteRule = function (id) { return editable('/api/alerts/rules/' + encodeURIComponent(id), 'DELETE'); };
    ST.ack = function (id, on) {
      var path = '/api/alerts/events/' + encodeURIComponent(id);
      return conditional(path, function (response) {
        return mutation(path + '/ack', 'PUT', { acked: on !== false }, { ifMatch: response.etag });
      });
    };
    ST.ackAll = function () { return mutation('/api/alerts/acknowledgements', 'POST', undefined, { idempotencyKey: API.key() }); };
    ST.setSettings = function (patch) {
      return conditional('/api/settings', function (response) {
        var input = Object.assign({}, response.data, patch);
        delete input.geoKeyConfigured; delete input.enrollKeyConfigured;
        delete input.enrollToken;
        if (!input.geoKey) delete input.geoKey;
        Object.keys(input.notify).forEach(function (key) {
          input.notify[key] = Object.assign({}, input.notify[key]);
          delete input.notify[key].configured;
        });
        return mutation('/api/settings', 'PUT', input, { ifMatch: response.etag });
      });
    };
    // Schedules are computed by the center: next runs, data usage and clashes come from /api/schedules.
    ST.scheduleInfo = function () { return schedules; };
    ST.engineSchedules = function (node) {
      return schedules.nodes.filter(function (x) { return x.nodeId === node.id; }).map(function (x) { return Object.assign({ engine: x.engine || 'ookla' }, x.config); });
    };
    ST.saveEngineSchedules = function (id, engines) { return ST.saveSchedule(id, { engines: engines }); };
    // Two-factor sign-in (authenticator app): status, start setup, confirm, turn off.
    ST.twoFactorStatus = function () { return API.request('/api/auth/2fa').then(function (r) { return r.data; }); };
    ST.twoFactorSetup = function (currentPassword) { return API.request('/api/auth/2fa/setup', { method: 'POST', body: { currentPassword: currentPassword } }).then(function (r) { return r.data; }); };
    ST.twoFactorEnable = function (code) { return API.request('/api/auth/2fa/enable', { method: 'POST', body: { code: code } }).then(function (r) { return r.data.recoveryCodes; }); };
    ST.twoFactorDisable = function (currentPassword, code) { return API.request('/api/auth/2fa/disable', { method: 'POST', body: { currentPassword: currentPassword, code: code } }); };
    // Changes the owner's password; the center rotates this session, so the owner stays signed in.
    ST.changePassword = function (currentPassword, newPassword) {
      return API.request('/api/auth/password', { method: 'POST', body: { currentPassword: currentPassword, newPassword: newPassword } });
    };
    ST.saveSchedule = function (id, input) {
      var path = '/api/schedules/' + encodeURIComponent(id);
      return conditional(path, function (response) { return mutation(path, 'PUT', input, { ifMatch: response.etag }); });
    };
    ST.saveScheduleRules = function (patch) {
      return conditional('/api/schedules', function (response) {
        // Start from the rules this ETag belongs to, so a stale local copy can never overwrite newer values.
        var current = response.data.rules;
        var rules = Object.assign({ stagger: current.stagger, maxConcurrent: current.maxConcurrent,
          quietStart: current.quiet.quietStart, quietEnd: current.quiet.quietEnd }, patch);
        return mutation('/api/schedules/rules', 'PUT', { stagger: !!rules.stagger, maxConcurrent: +rules.maxConcurrent,
          quiet: { quietStart: rules.quietStart || '', quietEnd: rules.quietEnd || '', timezone: ST.timezone() } }, { ifMatch: response.etag });
      });
    };
    ST.staggerSchedules = function () {
      return conditional('/api/schedules', function (response) { return mutation('/api/schedules/stagger', 'POST', undefined, { ifMatch: response.etag }); });
    };
    ST.rotateEnrollmentKey = function () {
      // No reload here: the page shows the new key once, and a reload would redraw the page and discard it.
      return API.request('/api/settings/enrolment-key', { method: 'POST' }).then(function (response) { state.settings.enrollKeyConfigured = true; return response.data; });
    };
    // A test message is a delivery the center sends; wait until it is sent or has failed.
    ST.testNotification = function (channel) {
      return API.request('/api/notifications/test/' + encodeURIComponent(channel), { method: 'POST', idempotencyKey: API.key() }).then(function (queued) {
        var started = Date.now(), path = '/api/notifications/deliveries/' + encodeURIComponent(queued.data.id);
        return new Promise(function (resolve, reject) {
          (function poll() {
            API.request(path).then(function (reply) {
              var d = reply.data;
              if (d.status === 'sent' || d.status === 'failed') { resolve(d); return; }
              if (Date.now() - started > 30000) { resolve({ status: 'failed', error: 'no answer from the center in time' }); return; }
              setTimeout(poll, 1000);
            }, reject);
          })();
        });
      });
    };
    // Follows an installation or removal job. Progress arrives as server-sent events; if the stream drops after the job has
    // ended, its final state is read instead. onEvent receives {id, message, pct, status}; the last call has a terminal status.
    ST.followInstallation = function (id, onEvent) {
      var finished = false, source;
      function finish() { finished = true; if (source) source.close(); }
      function handle(raw) {
        var event; try { event = JSON.parse(raw); } catch (e) { return; }
        onEvent(event);
        if (event.status === 'succeeded' || event.status === 'failed') finish();
      }
      source = new EventSource('/api/installations/' + encodeURIComponent(id) + '/events');
      source.addEventListener('progress', function (e) { handle(e.data); });
      source.onerror = function () {
        if (finished) return;
        API.request('/api/installations/' + encodeURIComponent(id)).then(function (reply) {
          var job = reply.data;
          if (!finished && (job.status === 'succeeded' || job.status === 'failed')) {
            onEvent({ id: 'final', status: job.status, message: job.error || (job.status === 'succeeded' ? 'Done.' : 'It did not finish.'), pct: 100 });
            finish();
          }
        }, function () {});
      };
      return { close: finish };
    };
    // Probes the machine (no credential is sent), then returns the fingerprint to confirm.
    ST.probeMachine = function (host, port) {
      return API.request('/api/installer/probes', { method: 'POST', idempotencyKey: API.key(), body: { host: host, port: port } }).then(function (reply) { return reply.data; });
    };
    ST.startInstallation = function (nodeId, kind, probe, ssh) {
      return API.request('/api/nodes/' + encodeURIComponent(nodeId) + '/' + kind, { method: 'POST', idempotencyKey: API.key(), body: { probeId: probe.id, fingerprint: probe.fingerprint, ssh: ssh } }).then(function (reply) { return reply.data; });
    };
    ST.updateAgent = function (id, version) {
      return mutation('/api/nodes/' + encodeURIComponent(id) + '/commands', 'POST', { type: 'update_agent', version: version }, { idempotencyKey: API.key() });
    };
    // A manual test is a command the node picks up; poll its progress, then read the result it produced.
    ST.runTest = function (id, onProgress) {
      var base = '/api/nodes/' + encodeURIComponent(id);
      return API.request(base + '/run', { method: 'POST', idempotencyKey: API.key() }).then(function (queued) {
        var command = queued.data, started = Date.now();
        return new Promise(function (resolve, reject) {
          (function poll() {
            API.request(base + '/commands/' + encodeURIComponent(command.id)).then(function (reply) {
              var c = reply.data;
              if (onProgress) onProgress({ phase: c.phase || (c.status === 'pending' ? 'Waiting for the node to pick this up' : 'Running the test'), pct: c.pct || 0, value: null, unit: 'speed', index: 0 });
              if (c.status === 'succeeded' || c.status === 'failed') {
                ST.load();
                if (c.status === 'succeeded' && c.resultId) {
                  // The result travels from the node after the command is acknowledged, so reload once more when it is in.
                  API.request('/api/results/' + encodeURIComponent(c.resultId)).then(function (r) { ST.load(); resolve(result(r.data)); }, reject);
                } else resolve({ status: 'failed', error: c.error || 'The test did not finish.' });
                return;
              }
              if (Date.now() - started > 5 * 60 * 1000) { resolve({ status: 'failed', error: 'The node did not answer in time. Check that it is online.' }); return; }
              setTimeout(poll, 1500);
            }, reject);
          })();
        });
      });
    };
  }

  if (ST.demo) load();
  else {
    initRemote();
    // Keep the numbers fresh without disturbing the owner: only while the tab is visible, nothing is being typed and no dialog is open.
    ST.refreshWhenIdle = function () {
      var active = document.activeElement, typing = active && /^(INPUT|TEXTAREA|SELECT)$/.test(active.tagName);
      if (document.visibilityState === 'visible' && ST.remote.status === 'ready' && !typing && !document.querySelector('.overlay, .drawer-wrap')) return ST.load();
      return null;
    };
    if (typeof setInterval === 'function') setInterval(ST.refreshWhenIdle, 60000);
  }
})();

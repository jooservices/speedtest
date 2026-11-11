/* Deterministic demo data: 5 nodes, 30 days of results, IP history and a sample geolocation "database".
   Everything is generated in the browser — there is no backend in this preview. */
(function () {
  'use strict';
  var ST = (window.ST = window.ST || {});

  function hash32(str) {
    var h = 2166136261;
    for (var i = 0; i < str.length; i++) { h ^= str.charCodeAt(i); h = Math.imul(h, 16777619); }
    return h >>> 0;
  }
  function mulberry(a) {
    return function () {
      a |= 0; a = (a + 0x6D2B79F5) | 0;
      var t = Math.imul(a ^ (a >>> 15), 1 | a);
      t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
      return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
    };
  }
  function rng(key) { return mulberry(hash32(String(key))); }
  function gauss(r) { var u = 0, v = 0; while (u === 0) u = r(); while (v === 0) v = r(); return Math.sqrt(-2 * Math.log(u)) * Math.cos(2 * Math.PI * v); }
  function clamp(v, a, b) { return Math.max(a, Math.min(b, v)); }

  var SLOT = 30 * 60 * 1000;
  var DAYS = 30;

  ST.util = { hash32: hash32, rng: rng, gauss: gauss, clamp: clamp, SLOT: SLOT };

  /* ---------- Speed test servers (sample pool) ---------- */
  ST.servers = [
    { id: 1001, name: 'Viettel Network', city: 'Hà Nội', sponsor: 'Viettel Group' },
    { id: 1002, name: 'Viettel Network', city: 'TP. Hồ Chí Minh', sponsor: 'Viettel Group' },
    { id: 2001, name: 'VNPT', city: 'Hà Nội', sponsor: 'VNPT Corp' },
    { id: 2002, name: 'VNPT', city: 'TP. Hồ Chí Minh', sponsor: 'VNPT Corp' },
    { id: 3001, name: 'FPT Telecom', city: 'Hà Nội', sponsor: 'FPT Telecom' },
    { id: 3002, name: 'CMC Telecom', city: 'Hà Nội', sponsor: 'CMC Telecom' },
    { id: 4001, name: 'Mobifone', city: 'TP. Hồ Chí Minh', sponsor: 'Mobifone' }
  ];

  /* ---------- Sample geolocation "database" (stands in for GeoLite2 / ip-api) ---------- */
  var ISPS = {
    viettel: { asn: 7552, org: 'Viettel Group', isp: 'Viettel', cities: [
      ['Hà Nội', 'Hà Nội', 21.0285, 105.8542], ['TP. Hồ Chí Minh', 'TP. Hồ Chí Minh', 10.8231, 106.6297], ['Đà Nẵng', 'Đà Nẵng', 16.0544, 108.2022]] },
    vnpt: { asn: 45899, org: 'VNPT Corp', isp: 'VNPT', cities: [
      ['Hà Nội', 'Hà Nội', 21.0285, 105.8542], ['TP. Hồ Chí Minh', 'TP. Hồ Chí Minh', 10.8231, 106.6297],
      ['Hải Phòng', 'Hải Phòng', 20.8449, 106.6881], ['Cần Thơ', 'Cần Thơ', 10.0452, 105.7469]] }
  };
  // Sample addresses use reserved documentation ranges only.
  var PREFIX_ISP = { '203.0.113': 'viettel', '198.51.100': 'viettel', '192.0.2': 'vnpt', '198.18.4': 'vnpt' };
  var geoCache = {};
  ST.geo = function (ip) {
    if (geoCache[ip]) return geoCache[ip];
    var p = ip.split('.').slice(0, 3).join('.');
    var key = PREFIX_ISP[p];
    if (!key) return (geoCache[ip] = null);
    var isp = ISPS[key], h = hash32('geo' + ip), r = rng('geoj' + ip);
    var c = isp.cities[h % isp.cities.length];
    var g = {
      ip: ip, version: 4, asn: isp.asn, asnOrg: isp.org, isp: isp.isp,
      country: 'Việt Nam', countryCode: 'VN', region: c[1], city: c[0],
      lat: +(c[2] + (r() - 0.5) * 0.12).toFixed(4), lon: +(c[3] + (r() - 0.5) * 0.12).toFixed(4),
      timezone: 'Asia/Ho_Chi_Minh', radiusKm: [20, 50, 100][h % 3], connection: 'Fiber / broadband',
      source: h % 11 === 0 ? 'ip-api.com (fallback)' : 'GeoLite2 City + ASN'
    };
    return (geoCache[ip] = g);
  };

  /* ---------- Nodes ---------- */
  // "profile" is hidden simulation input only. The real product never needs to know what a node is attached to.
  function seedNodes() {
    return [
      { id: 'n1', name: 'VM1', label: 'Viettel Fast 50', provider: 'Viettel', plan: 'Fast 50', colorSlot: 1,
        expectedDown: 300, expectedUp: 300, interval: 30, offset: 0, threads: 4, server: 'auto', enabled: true,
        host: '192.168.10.31', agent: '1.4.2', note: '', createdAt: 0,
        profile: { effD: 0.94, effU: 0.9, cong: 0.12, ping: 12, jit: 1.6, loss: 0.02 },
        ipModel: { kind: 'dynamic', prefix: '203.0.113', segSlots: 6 * 48, off: 70 }, pref: [1001, 1002] },
      { id: 'n2', name: 'VM2', label: 'Viettel Pro 600', provider: 'Viettel', plan: 'Pro 600', colorSlot: 2,
        expectedDown: 1000, expectedUp: 1000, interval: 30, offset: 3, threads: 6, server: 'auto', enabled: true,
        host: '192.168.10.32', agent: '1.4.2', note: 'Static address.', createdAt: 0,
        profile: { effD: 0.93, effU: 0.91, cong: 0.08, ping: 9, jit: 1.1, loss: 0.01 },
        ipModel: { kind: 'static', prefix: '198.51.100', host: 42 }, pref: [1001, 1002] },
      { id: 'n3', name: 'VM3', label: 'VNPT Home 5 Super', provider: 'VNPT', plan: 'Home 5 Super', colorSlot: 3,
        expectedDown: 1000, expectedUp: 1000, interval: 30, offset: 6, threads: 6, server: 'auto', enabled: true,
        host: '192.168.10.33', agent: '1.4.2', note: '', createdAt: 0,
        profile: { effD: 0.86, effU: 0.84, cong: 0.22, ping: 14, jit: 2.4, loss: 0.08, bad: [9 * 48, 5 * 48, 0.62] },
        ipModel: { kind: 'dynamic', prefix: '192.0.2', segSlots: 4 * 48, off: 31 }, pref: [2001, 2002] },
      { id: 'n4', name: 'VM4', label: 'VNPT Home 2', provider: 'VNPT', plan: 'Home 2', colorSlot: 4,
        expectedDown: 1000, expectedUp: 1000, interval: 30, offset: 9, threads: 6, server: 'auto', enabled: true,
        host: '192.168.10.34', agent: '1.4.1', note: '', createdAt: 0,
        profile: { effD: 0.79, effU: 0.8, cong: 0.3, ping: 17, jit: 3.1, loss: 0.2 },
        ipModel: { kind: 'dynamic', prefix: '198.18.4', segSlots: 9 * 48, off: 205 }, pref: [2001, 2002] },
      { id: 'n5', name: 'VM5', label: 'All connections (load balanced)', provider: 'Load balancer', plan: 'Combined', colorSlot: 5,
        expectedDown: 3300, expectedUp: 3300, interval: 30, offset: 15, threads: 16, server: 'auto', enabled: true,
        host: '192.168.10.35', agent: '1.4.2', note: 'Runs through the gateway load balancer.', createdAt: 0,
        profile: { effD: 0.72, effU: 0.7, cong: 0.18, ping: 11, jit: 1.8, loss: 0.03 },
        ipModel: { kind: 'mix', from: ['n1', 'n2', 'n3', 'n4'], weights: [0.1, 0.35, 0.3, 0.25] }, pref: [1001, 2001, 3001, 1002] }
    ];
  }
  ST.seedNodes = seedNodes;

  // Outage windows, expressed as "slots before the newest slot".
  var OUTAGES = { n1: [[1100, 1099]], n3: [[300, 297]], n4: [[700, 694], [90, 89]] };

  function ipForNode(node, slot, byId) {
    var m = node.ipModel;
    if (!m) return '198.51.100.9';
    if (m.kind === 'static') return m.prefix + '.' + m.host;
    if (m.kind === 'dynamic') {
      var seg = Math.floor((slot + m.off) / m.segSlots);
      return m.prefix + '.' + (10 + (hash32(node.id + ':' + seg) % 230));
    }
    if (m.kind === 'mix') {
      var r = rng('mix' + node.id + slot)(), acc = 0, pick = m.from[0];
      for (var i = 0; i < m.from.length; i++) { acc += m.weights[i]; if (r <= acc) { pick = m.from[i]; break; } }
      var src = byId[pick];
      return src ? ipForNode(src, slot, byId) : '198.51.100.9';
    }
    return '198.51.100.9';
  }
  ST.ipForNode = ipForNode;

  /* Generate one sample. `rnd` is a 0..1 generator, `ts` epoch ms, `slotsFromNow` for outage/degradation windows. */
  ST.genSample = function (node, ts, rnd, slotsFromNow, byId, slot) {
    var p = node.profile || { effD: .85, effU: .85, cong: .15, ping: 14, jit: 2, loss: .05 };
    var d = new Date(ts + 7 * 3600000); // Vietnam wall clock for the simulated daily pattern
    var hr = d.getUTCHours() + d.getUTCMinutes() / 60, wd = d.getUTCDay();
    var evening = Math.exp(-Math.pow((hr - 20.5) / 2.3, 2) / 2);
    var lunch = 0.25 * Math.exp(-Math.pow((hr - 12.5) / 1.2, 2) / 2);
    var weekend = (wd === 0 || wd === 6) ? 1.25 : 1;
    var cg = clamp(p.cong * weekend * (evening + lunch), 0, 0.9);
    var degrade = 1;
    if (p.bad && slotsFromNow <= p.bad[0] && slotsFromNow >= p.bad[1]) degrade = p.bad[2];
    var res = { nodeId: node.id, ts: ts };
    var out = (OUTAGES[node.id] || []).some(function (w) { return slotsFromNow <= w[0] && slotsFromNow >= w[1]; });
    var randomFail = rnd() < 0.008;
    var sid = (node.pref || [1001])[Math.floor(rnd() * (node.pref || [1001]).length)];
    if (rnd() < 0.06) sid = ST.servers[Math.floor(rnd() * ST.servers.length)].id;
    res.serverId = sid;
    res.threads = node.threads || 4;
    res.ip = byId ? ipForNode(node, slot, byId) : '198.51.100.9';
    if (out || randomFail) {
      res.status = 'failed';
      if (out) {
        res.error = rnd() < .5 ? 'No route to test server' : 'Link down (no response from gateway)';
        res.failureClass = 'no_network';
      } else if (rnd() < 0.2) {
        res.error = 'Too many requests';
        res.failureClass = 'rate_limited';
      } else {
        res.error = 'Timed out while contacting the test server';
        res.failureClass = 'timeout';
      }
      res.down = res.up = res.ping = res.jitter = res.loss = res.dLat = res.uLat = null;
      res.bytesDown = res.bytesUp = 0; res.duration = 30;
      return res;
    }
    res.failureClass = 'other';
    var noise = 1 + gauss(rnd) * 0.035;
    var down = node.expectedDown * p.effD * (1 - cg) * degrade * noise;
    var up = node.expectedUp * p.effU * (1 - cg * 0.45) * degrade * (1 + gauss(rnd) * 0.04);
    res.down = +clamp(down, 1, node.expectedDown * 1.03).toFixed(2);
    res.up = +clamp(up, 1, node.expectedUp * 1.03).toFixed(2);
    res.ping = +clamp(p.ping + cg * 9 + Math.abs(gauss(rnd)) * p.jit * 1.4, 2, 400).toFixed(1);
    res.jitter = +clamp(p.jit * (1 + cg * 1.5) * (0.6 + rnd() * 0.9), 0.1, 80).toFixed(1);
    res.loss = rnd() < p.loss ? +(rnd() * (cg > .15 ? 3 : 1.2)).toFixed(1) : 0;
    res.dLat = +(res.ping + 18 + rnd() * 55 + cg * 60).toFixed(1);
    res.uLat = +(res.ping + 10 + rnd() * 35 + cg * 25).toFixed(1);
    res.bytesDown = Math.round(res.down * 1e6 / 8 * (10 + rnd() * 3));
    res.bytesUp = Math.round(res.up * 1e6 / 8 * (8 + rnd() * 3));
    res.duration = +(26 + rnd() * 9).toFixed(1);
    res.status = res.loss >= 2 || res.down < node.expectedDown * 0.4 ? 'degraded' : 'ok';
    return res;
  };

  ST.buildSeedResults = function (nodes, now) {
    var byId = {}; nodes.forEach(function (n) { byId[n.id] = n; });
    var last = Math.floor(now / SLOT), out = [], id = 0;
    var total = DAYS * 48;
    for (var s = last - total + 1; s <= last; s++) {
      for (var i = 0; i < nodes.length; i++) {
        var n = nodes[i];
        var ts = s * SLOT + (n.offset || 0) * 60000;
        if (ts > now) continue;
        var rnd = rng(n.id + ':' + s);
        var r = ST.genSample(n, ts, rnd, last - s, byId, s);
        r.id = 'r' + (++id) + '_' + n.id;
        out.push(r);
      }
    }
    out.sort(function (a, b) { return a.ts - b.ts; });
    return out;
  };
})();

/* IP & location: public IP history per node, geolocation, map and lookup. */
(function () {
  'use strict';
  var ST = window.ST, U = ST.ui, h = U.h, F = ST.fmt, S = ST.stats;
  var q = { more: false, node: 'all', lookup: '', lookupRes: null };

  function geoLine(g) { return g ? g.city + ', ' + g.country : 'Unknown location'; }

  function openIp(entry, node) {
    var g = entry.geo;
    U.drawer({ title: 'IP address details', body: h('div', { class: 'stack' },
      h('div', { class: 'row between' }, h('span', { class: 'mono', style: 'font-size:18px;font-weight:650' }, entry.ip), U.btn('Copy', { size: 'sm', icon: 'copy', onClick: function () { U.copy(entry.ip, 'IP copied'); } })),
      h('div', { class: 'row' }, U.dot(ST.nodeColor(node)), node.name + ' · ' + node.label),
      U.kv([['First seen', F.date(entry.first, 'full')], ['Last seen', F.date(entry.last, 'full')], ['Held for', F.dur(entry.last - entry.first)], ['Tests on this address', F.num(entry.tests)],
        ['Average download', F.speed(entry.avgDown)], ['Average upload', F.speed(entry.avgUp)]]),
      g ? h('div', null, h('h3', { style: 'margin:6px 0' }, 'Location'), U.kv([['City', g.city], ['Region', g.region], ['Country', g.country + ' (' + g.countryCode + ')'], ['Coordinates', g.lat + ', ' + g.lon],
        ['Accuracy', 'About ' + g.radiusKm + ' km'], ['Time zone', g.timezone], ['Provider', g.isp], ['Network', 'AS' + g.asn + ' · ' + g.asnOrg], ['Connection', g.connection], ['Looked up with', g.source]]))
        : U.callout('No location data is available for this address.', 'warn', 'alert')) });
  }

  ST.pages.network = function (root) {
    var nodes = ST.nodes();
    root.appendChild(U.pageHead('IP & location', 'Which public address each node uses, where it is, and how it changes over time.'));
    if (!nodes.length) { root.appendChild(U.card(null, null, U.empty('No data yet', 'Add a node to track its public IP address.'))); return; }
    if (q.node !== 'all' && !ST.node(q.node)) q.node = 'all';
    var sel = q.node === 'all' ? nodes : [ST.node(q.node)];
    root.appendChild(h('div', { class: 'row wrap mb' }, h('span', { class: 'muted' }, 'Show'), U.select([{ value: 'all', label: 'All nodes' }].concat(nodes.map(function (n) { return { value: n.id, label: ST.nodeTitle(n) }; })), q.node, function (v) { q.node = v; ST.refresh(); }, 'auto')));

    var hist = {}, entries = [], changes = [];
    sel.forEach(function (n) {
      hist[n.id] = ST.ipHistory(n.id);
      hist[n.id].ips.forEach(function (e) { entries.push({ e: e, n: n }); });
      if (!(n.ipModel && n.ipModel.kind === 'mix')) hist[n.id].changes.forEach(function (c) { changes.push({ c: c, n: n }); });
    });
    changes.sort(function (a, b) { return b.c.ts - a.c.ts; });

    /* ---- map + timeline ---- */
    var mapHost = h('div');
    var cities = {};
    sel.forEach(function (n) {
      var cur = ST.latest(n.id); if (!cur) return;
      var g = ST.geo(cur.ip); if (!g) return;
      var c = cities[g.city] || (cities[g.city] = { lat: g.lat, lon: g.lon, label: g.city, colors: [], nodes: [], ips: [] });
      c.colors.push(ST.nodeColor(n)); c.nodes.push(n.name); c.ips.push(cur.ip);
    });
    var pins = Object.keys(cities).map(function (k) { var c = cities[k]; return { lat: c.lat, lon: c.lon, label: c.label, colors: c.colors, names: c.nodes.join(', '), tip: c.nodes.map(function (nm, i) { return { label: nm, value: c.ips[i], color: c.colors[i] }; }) }; });

    var tl = h('div', { class: 'timeline' }), shown = q.more ? 60 : 8;
    changes.slice(0, shown).forEach(function (x) {
      var g1 = ST.geo(x.c.from), g2 = ST.geo(x.c.to);
      tl.appendChild(h('div', { class: 'tl-item' }, h('span', { class: 'tl-dot', style: { background: ST.nodeColor(x.n) } }),
        h('div', null, h('div', { class: 'tl-title' }, x.n.name + ' changed address'), h('div', { class: 'mono' }, x.c.from + ' → ' + x.c.to),
          h('div', { class: 'muted' }, F.date(x.c.ts, 'full') + (g2 ? ' · now in ' + g2.city : '') + (g1 && g2 && g1.city !== g2.city ? ' (was ' + g1.city + ')' : '')))));
    });
    if (!changes.length) tl.appendChild(U.empty('No address changes', 'Every node kept the same public IP over the whole period.'));
    if (changes.length > shown) tl.appendChild(U.btn('Show ' + (changes.length - shown) + ' more', { size: 'sm', onClick: function () { q.more = true; ST.refresh(); } }));
    var mixNote = sel.some(function (n) { return n.ipModel && n.ipModel.kind === 'mix'; });
    root.appendChild(h('div', { class: 'grid cols-main', style: 'align-items:start' },
      U.card('Where the connections are', 'Current location of each node.', mapHost),
      U.card('Address changes', changes.length + ' changes recorded', h('div', null, tl, mixNote ? h('p', { class: 'hint' }, 'Nodes behind a load balancer switch address constantly, so they are listed in the table below instead.') : null))));
    ST.charts.map(mapHost, { pins: pins });
    if (!pins.length) mapHost.appendChild(h('div', { style: 'position:absolute;inset:0;display:grid;place-items:center' }, U.empty('No location data', 'Run a test to see where this node is.')));

    /* ---- IP table ---- */
    entries.sort(function (a, b) { return b.e.last - a.e.last; });
    root.appendChild(h('div', { class: 'mt' }, U.card('Every address seen', 'Click a row for the full location record.', h('div', { style: 'margin:-8px -18px -18px' }, U.table({
      columns: [
        { key: 'ip', label: 'Address', render: function (r) { return h('span', { class: 'mono' }, r.e.ip); } },
        { key: 'node', label: 'Node', render: function (r) { return h('span', { class: 'row' }, U.dot(ST.nodeColor(r.n)), r.n.name); } },
        { key: 'loc', label: 'Location', render: function (r) { return geoLine(r.e.geo); } },
        { key: 'isp', label: 'Provider', render: function (r) { return r.e.geo ? r.e.geo.isp + ' · AS' + r.e.geo.asn : '—'; } },
        { key: 'first', label: 'First seen', render: function (r) { return F.date(r.e.first); } },
        { key: 'last', label: 'Last seen', render: function (r) { return F.rel(r.e.last); } },
        { key: 'tests', label: 'Tests', num: true, render: function (r) { return F.num(r.e.tests); } },
        { key: 'avg', label: 'Avg download', num: true, render: function (r) { return F.speed(r.e.avgDown); } }],
      rows: entries, onRow: function (r) { openIp(r.e, r.n); } })))));

    /* ---- lookup + provider ---- */
    var resBox = h('div', { class: 'mt' });
    function doLookup(v) {
      U.clear(resBox);
      v = v.trim();
      if (!/^(\d{1,3}\.){3}\d{1,3}$/.test(v) || v.split('.').some(function (p) { return +p > 255; })) { resBox.appendChild(U.callout('Enter a valid IPv4 address, for example 203.0.113.57.', 'crit', 'alert')); return; }
      var g = ST.geo(v);
      resBox.appendChild(g ? h('div', { class: 'card card-pad' }, h('div', { class: 'row between' }, h('b', { class: 'mono' }, g.ip), U.chip(g.source, 'info')),
        U.kv([['Location', geoLine(g) + ' (' + g.lat + ', ' + g.lon + ')'], ['Provider', g.isp + ' · AS' + g.asn], ['Accuracy', 'About ' + g.radiusKm + ' km']]))
        : U.callout('This preview only knows the sample addresses used by the demo nodes. The real service looks up any public address.', 'warn', 'info'));
    }
    var inp = U.input(q.lookup, function (v) { q.lookup = v; }, { placeholder: '203.0.113.57', 'aria-label': 'IP address to look up', onKeydown: function (e) { if (e.key === 'Enter') doLookup(q.lookup); } });
    var known = {}; nodes.forEach(function (n) { hist[n.id] = hist[n.id] || ST.ipHistory(n.id); hist[n.id].ips.forEach(function (e) { known[e.ip] = 1; }); });
    var providerCard = U.card('Location service', 'Addresses are looked up once, then remembered, so a repeated address never costs another lookup.', h('div', { class: 'stack' },
      U.kv([['Primary', 'MaxMind GeoLite2 City + ASN (offline database)'], ['Database updated', '28 Oct 2026'], ['Fallback', 'ip-api.com (used when the database has no answer)'], ['Addresses remembered', F.num(Object.keys(known).length)]]),
      h('div', { class: 'row wrap' },
        U.btn('Update database', { icon: 'refresh', onClick: function (e) { var b = e.currentTarget; b.disabled = true; setTimeout(function () { b.disabled = false; U.toast('Location database is up to date'); }, 1100); } }),
        U.btn('Refresh all locations', { onClick: function (e) { var b = e.currentTarget; b.disabled = true; setTimeout(function () { b.disabled = false; U.toast('Re-checked ' + Object.keys(known).length + ' addresses'); }, 1300); } })),
      h('div', null, h('div', { class: 'label', style: 'margin-bottom:6px' }, 'Look up an address'), h('div', { class: 'row' }, inp, U.btn('Look up', { icon: 'search', onClick: function () { doLookup(q.lookup); } })), resBox)));
    root.appendChild(h('div', { class: 'mt' }, providerCard));
    if (q.lookup) doLookup(q.lookup);
  };
})();

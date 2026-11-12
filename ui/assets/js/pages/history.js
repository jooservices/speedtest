/* Results: filter, sort, paginate, export. */
(function () {
  'use strict';
  var ST = (/** @type {any} */ (window)).ST, U = ST.ui, h = U.h, F = ST.fmt;
  var q = { nodes: null, allNodes: true, status: 'all', ispId: '', text: '', minDown: '', maxDown: '', range: null, sort: 'ts', dir: 'desc', page: 1, size: 25 };

  ST.pages.history = function (root) {
    var nodes = ST.nodes();
    // Until the owner picks nodes by hand every node is shown, including nodes added after this page was first opened.
    if (!q.nodes || q.allNodes) q.nodes = nodes.map(function (n) { return n.id; });
    q.nodes = q.nodes.filter(function (id) { return ST.node(id); });
    var rangeKey = q.range || ST.settings().range;
    var listHost = h('div'), countHost = h('span', { class: 'muted' });
    var statusSelect, ispSelect, minDownInput, maxDownInput, searchInput;

    function filtered() {
      var from = Date.now() - ST.ranges[rangeKey];
      var rows = ST.results({ nodeIds: q.nodes, from: from, status: q.status, ispId: q.ispId });
      var text = q.text.trim().toLowerCase(), minD = q.minDown === '' ? null : +q.minDown, maxD = q.maxDown === '' ? null : +q.maxDown;
      rows = rows.filter(function (r) {
        if (minD != null && (r.down == null || r.down < minD)) return false;
        if (maxD != null && (r.down == null || r.down > maxD)) return false;
        if (text) {
          var s = ST.server(r.serverId), g = ST.geo(r.ip), n = ST.node(r.nodeId);
          var hay = [r.ip, s.name, s.city, g && g.city, g && g.isp, n.name, n.label, r.error].join(' ').toLowerCase();
          if (hay.indexOf(text) < 0) return false;
        }
        return true;
      });
      var dir = q.dir === 'asc' ? 1 : -1, key = q.sort;
      rows.sort(function (a, b) {
        var av, bv;
        if (key === 'node') { av = ST.node(a.nodeId).name; bv = ST.node(b.nodeId).name; }
        else if (key === 'server') { av = ST.server(a.serverId).city; bv = ST.server(b.serverId).city; }
        else { av = a[key]; bv = b[key]; }
        if (av == null && bv == null) return 0; if (av == null) return 1; if (bv == null) return -1;
        return (av < bv ? -1 : av > bv ? 1 : 0) * dir;
      });
      return rows;
    }

    function draw() {
      var rows = filtered(), pages = Math.max(1, Math.ceil(rows.length / q.size));
      if (q.page > pages) q.page = pages;
      var slice = rows.slice((q.page - 1) * q.size, q.page * q.size);
      countHost.textContent = F.num(rows.length) + ' results';
      U.clear(listHost);
      listHost.appendChild(U.table({
        columns: [
          { key: 'ts', label: 'Time', sort: true, render: function (r) { return F.date(r.ts); } },
          { key: 'node', label: 'Node', sort: true, render: function (r) { var n = ST.node(r.nodeId); return h('span', { class: 'row' }, U.dot(ST.nodeColor(n)), n.name); } },
          { key: 'down', label: 'Download', sort: true, num: true, render: function (r) { return F.speed(r.down); } },
          { key: 'up', label: 'Upload', sort: true, num: true, render: function (r) { return F.speed(r.up); } },
          { key: 'ping', label: 'Ping', sort: true, num: true, render: function (r) { return F.ms(r.ping); } },
          { key: 'jitter', label: 'Jitter', sort: true, num: true, render: function (r) { return F.ms(r.jitter); } },
          { key: 'loss', label: 'Loss', sort: true, num: true, render: function (r) { return r.loss == null ? '—' : F.num(r.loss, 1) + '%'; } },
          { key: 'server', label: 'Server', sort: true, render: function (r) {
            if (r.serverId == null && !r.server) return '—';
            var s = r.server || ST.server(r.serverId), used = r.serverId == null && r.server ? r.server.id : r.serverId;
            var differs = r.pinnedServerId != null && used != null && Number(r.pinnedServerId) !== Number(used);
            return h('span', { class: 'row wrap' }, s.name + (s.city ? ', ' + s.city : ''), differs ? U.chip('Different from pinned', 'warn', 'alert') : null);
          } },
          { key: 'ip', label: 'Public IP', sort: true, render: function (r) { return h('span', { class: 'mono' }, r.ip); } },
          { key: 'status', label: 'Status', sort: true, render: function (r) {
            var chips = h('span', { class: 'row', style: 'gap:6px' }, U.statusChip(r.status));
            var reason = U.failureChip(r);
            if (reason) chips.appendChild(reason);
            return chips;
          } }],
        rows: slice, sortKey: q.sort, sortDir: q.dir, onSort: function (k) { if (q.sort === k) q.dir = q.dir === 'asc' ? 'desc' : 'asc'; else { q.sort = k; q.dir = k === 'ts' ? 'desc' : 'asc'; } draw(); },
        onRow: function (r) { ST.openResult(r); },
        empty: U.empty('No results match', 'Try a wider time range or clear some filters.', U.btn('Clear filters', { onClick: function () {
          q.allNodes = true; q.nodes = nodes.map(function (n) { return n.id; }); q.status = 'all'; q.ispId = ''; q.text = ''; q.minDown = ''; q.maxDown = ''; q.page = 1;
          if (statusSelect) statusSelect.value = 'all';
          if (ispSelect) ispSelect.value = '';
          if (minDownInput) minDownInput.value = '';
          if (maxDownInput) maxDownInput.value = '';
          if (searchInput) searchInput.value = '';
          Array.prototype.forEach.call(nodeBox.querySelectorAll('button'), function (button) { button.classList.remove('off'); button.setAttribute('aria-pressed', 'true'); });
          draw();
        } }))
      }));
      var pager = h('div', { class: 'pager' },
        h('span', { class: 'row' }, 'Rows per page', U.select([10, 25, 50, 100], q.size, function (v) { q.size = +v; q.page = 1; draw(); }, 'auto')),
        h('span', { class: 'muted tabnum' }, rows.length ? ((q.page - 1) * q.size + 1) + '–' + Math.min(rows.length, q.page * q.size) + ' of ' + F.num(rows.length) : '0 of 0'),
        h('span', { class: 'row' },
          U.btn('', { icon: 'chevron', size: 'sm', title: 'Previous page', disabled: q.page <= 1, onClick: function () { q.page--; draw(); } }),
          h('span', { class: 'tabnum' }, 'Page ' + q.page + ' of ' + pages),
          U.btn('', { icon: 'chevron', size: 'sm', title: 'Next page', disabled: q.page >= pages, onClick: function () { q.page++; draw(); } })));
      pager.querySelectorAll('button')[0].firstChild.style.transform = 'rotate(180deg)';
      listHost.appendChild(pager);
    }

    root.appendChild(ST.engineSelector());
    root.appendChild(U.pageHead('Results', 'Every speed test, newest first. Click a row for all the details.', [
      U.btn('Export CSV', { icon: 'download', onClick: function () { var rows = filtered(); U.download('speedtest-results-' + new Date().toISOString().slice(0, 10) + '.csv', ST.csv(rows), 'text/csv'); U.toast('Exported ' + rows.length + ' results'); } }),
      U.btn('Run tests', { icon: 'play', variant: 'primary', onClick: function () { U.runTestDialog(nodes.filter(function (n) { return n.enabled; }).map(function (n) { return n.id; })); } })]));

    /* filter bar */
    var nodeBox = h('div', { class: 'legend' });
    nodes.forEach(function (n) {
      var on = q.nodes.indexOf(n.id) >= 0;
      var b = h('button', { type: 'button', class: on ? '' : 'off', 'aria-pressed': on, onClick: function () {
        q.allNodes = false; var i = q.nodes.indexOf(n.id); if (i >= 0) q.nodes.splice(i, 1); else q.nodes.push(n.id);
        b.classList.toggle('off', q.nodes.indexOf(n.id) < 0); b.setAttribute('aria-pressed', q.nodes.indexOf(n.id) >= 0); q.page = 1; draw();
      } }, U.dot(ST.nodeColor(n)), n.name);
      nodeBox.appendChild(b);
    });
    var filters = h('div', { class: 'card card-pad mb stack' },
      h('div', { class: 'row wrap between' },
        h('div', { class: 'row wrap' },
          U.rangeSelect(rangeKey, function (v) { q.range = v; q.page = 1; draw(); }),
          (statusSelect = U.select([{ value: 'all', label: 'Any status' }, { value: 'ok', label: 'OK' }, { value: 'degraded', label: 'Degraded' }, { value: 'failed', label: 'Failed' }], q.status, function (v) { q.status = v; q.page = 1; draw(); }, 'auto')),
          h('div', { style: 'min-width:190px' }, U.field('ISP', (ispSelect = U.select([{ value: '', label: 'Any ISP' }].concat(ST.ispOptions()), q.ispId, function (v) {
            q.ispId = v; q.page = 1; draw();
          }, 'auto')))),
          h('div', { style: 'width:190px' }, U.suffix((minDownInput = U.input(q.minDown, function (v) { q.minDown = v; q.page = 1; draw(); }, { type: 'number', min: 0, placeholder: 'Min download', 'aria-label': 'Minimum download' })), 'Mbps')),
          h('div', { style: 'width:190px' }, U.suffix((maxDownInput = U.input(q.maxDown, function (v) { q.maxDown = v; q.page = 1; draw(); }, { type: 'number', min: 0, placeholder: 'Max download', 'aria-label': 'Maximum download' })), 'Mbps')),
          h('div', { style: 'width:280px;position:relative' }, (searchInput = U.input(q.text, function (v) { q.text = v; q.page = 1; draw(); }, { type: 'search', placeholder: 'Search IP, server, city, error', 'aria-label': 'Search results' })))),
        countHost),
      nodeBox);
    root.appendChild(filters);
    root.appendChild(h('div', { class: 'card' }, listHost));
    draw();
  };
})();

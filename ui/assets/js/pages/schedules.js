/* Schedules: per-node interval and offset, avoid-overlap option, upcoming runs, data usage estimate. */
(function () {
  'use strict';
  var ST = (/** @type {any} */ (window)).ST, U = ST.ui, h = U.h, F = ST.fmt, S = ST.stats;
  var INTERVALS = [5, 10, 15, 30, 60, 120, 240, 360, 720, 1440];
  function ivLabel(m) { return m < 60 ? 'Every ' + m + ' minutes' : m === 60 ? 'Every hour' : m < 1440 ? 'Every ' + m / 60 + ' hours' : 'Once a day'; }

  function nextRun(n, from) {
    var iv = n.interval * 60000, off = (n.offset || 0) * 60000;
    return Math.ceil((from - off) / iv) * iv + off;
  }

  // The half-typed pause window survives a redraw (a reload of the data); it resets when the center's values change.
  var quietDraft = null;

  function failed(error) { U.toast(error.message, 'error'); }

  ST.pages.schedules = function (root) {
    var nodes = ST.nodes(), s = ST.settings(), real = !ST.demo, info = real ? ST.scheduleInfo() : null;
    if (real) s = Object.assign({}, s, { stagger: info.rules.stagger, maxConcurrent: info.rules.maxConcurrent, quietStart: info.rules.quiet.quietStart, quietEnd: info.rules.quiet.quietEnd });
    if (!quietDraft || quietDraft.serverStart !== s.quietStart || quietDraft.serverEnd !== s.quietEnd) quietDraft = { start: s.quietStart, end: s.quietEnd, serverStart: s.quietStart, serverEnd: s.quietEnd };
    var byNode = {};
    if (real) info.nodes.forEach(function (x) { byNode[x.nodeId] = x; });
    root.appendChild(U.pageHead('Schedules', 'Decide how often each node measures its connection.', [U.btn('Run all now', { icon: 'play', onClick: function () { U.runTestDialog(nodes.filter(function (n) { return n.enabled; }).map(function (n) { return n.id; })); } })]));
    if (!nodes.length) { root.appendChild(U.card(null, null, U.empty('No nodes yet', 'Add a node to schedule tests.'))); return; }

    /* ---- global rules ---- */
    function settingRow(title, desc, ctl) { return h('div', { class: 'setting' }, h('div', null, h('div', { style: 'font-weight:600' }, title), h('div', { class: 'hint' }, desc)), h('div', { class: 's-ctl' }, ctl)); }
    root.appendChild(U.card('Rules for all nodes', null, h('div', null,
      settingRow('Keep tests from overlapping', 'Shifts each node by a few minutes so two tests never compete for the same bandwidth. Recommended when one node measures a combination of the others.', U.toggle(s.stagger, function (v) {
        if (real) { ST.saveScheduleRules({ stagger: v }).then(function () { U.toast(v ? 'Tests are now spread out' : 'Tests may overlap'); }, failed); return; }
        ST.setSettings({ stagger: v }, true);
        if (v) autoStagger(); else ST.emit('data');
        U.toast(v ? 'Tests are now spread out' : 'Tests may overlap');
      }, 'Keep tests from overlapping')),
      settingRow('Maximum tests at the same time', 'Used when overlapping is allowed.', U.select([1, 2, 3, 4, 5].map(function (x) { return { value: x, label: String(x) }; }), s.maxConcurrent, function (v) { if (real) { ST.saveScheduleRules({ maxConcurrent: +v }).then(function () { U.toast('Saved'); }, failed); return; } ST.setSettings({ maxConcurrent: +v }, true); U.toast('Saved'); })),
      settingRow('Pause tests during these hours', 'Leave empty to test around the clock.', h('div', { class: 'row' },
        U.input(quietDraft.start, function (v) { if (!real) ST.setSettings({ quietStart: v }, true); }, { type: 'time', 'aria-label': 'Pause from', onChange: function (e) { quiet(e.target.value, undefined); } }), h('span', { class: 'muted' }, 'to'),
        U.input(quietDraft.end, function (v) { if (!real) ST.setSettings({ quietEnd: v }, true); }, { type: 'time', 'aria-label': 'Pause until', onChange: function (e) { quiet(undefined, e.target.value); } }))))));
    // Both ends must be empty or both set; the center refuses anything else, so wait until the pair is complete.
    function quiet(start, end) {
      if (!real) return;
      if (start !== undefined) quietDraft.start = start;
      if (end !== undefined) quietDraft.end = end;
      var nextStart = quietDraft.start, nextEnd = quietDraft.end;
      if (!!nextStart !== !!nextEnd) return;
      ST.saveScheduleRules({ quietStart: nextStart, quietEnd: nextEnd }).then(function () { U.toast('Saved'); }, failed);
    }

    function autoStagger() {
      if (real) { ST.staggerSchedules().then(function () { U.toast('Start times spread out'); }, failed); return; }
      var step = Math.max(1, Math.floor(30 / nodes.length));
      nodes.forEach(function (n, i) { n.offset = (i * Math.max(step, 3)) % Math.max(1, n.interval); });
      ST.persist(); ST.emit('data');
    }

    /* ---- per node ---- */
    var countdowns = [];
    var rows = nodes.map(function (n) { return { n: n }; });
    var table = U.table({
      columns: [
        { key: 'n', label: 'Node', render: function (r) { return h('div', null, h('div', { class: 'row', style: 'font-weight:650' }, U.dot(ST.nodeColor(r.n)), r.n.name), h('div', { class: 'muted' }, r.n.label)); } },
        { key: 'iv', label: 'How often', render: function (r) { return U.select(INTERVALS.map(function (m) { return { value: m, label: ivLabel(m) }; }), r.n.interval, function (v) { if (real) { ST.saveSchedule(r.n.id, { interval: +v, offset: Math.min(r.n.offset || 0, +v - 1), enabled: byNode[r.n.id] ? byNode[r.n.id].config.enabled : true }).then(function () { U.toast(r.n.name + ': ' + ivLabel(+v).toLowerCase()); }, failed); return; } ST.updateNode(r.n.id, { interval: +v, offset: Math.min(r.n.offset || 0, +v - 1) }); U.toast(r.n.name + ': ' + ivLabel(+v).toLowerCase()); }, 'auto'); } },
        { key: 'off', label: 'Starts at minute', render: function (r) { return h('div', { style: 'width:90px' }, U.input(r.n.offset || 0, function (v) { var x = Math.max(0, Math.min(r.n.interval - 1, Math.round(+v) || 0)); if (!real) { r.n.offset = x; ST.persist(); } }, { type: 'number', min: 0, max: r.n.interval - 1, 'aria-label': 'Offset in minutes for ' + r.n.name, onChange: function (e) {
          if (!real) { ST.emit('data'); return; }
          var x = Math.max(0, Math.min(r.n.interval - 1, Math.round(+e.target.value) || 0));
          ST.saveSchedule(r.n.id, { interval: r.n.interval, offset: x, enabled: byNode[r.n.id] ? byNode[r.n.id].config.enabled : true }).then(function () { U.toast(r.n.name + ' starts at minute ' + x); }, failed);
        } })); } },
        { key: 'next', label: 'Next test', render: function (r) { var span = h('span', { class: 'tabnum' }); countdowns.push({ span: span, node: r.n }); return span; } },
        { key: 'on', label: 'Active', render: function (r) { return U.toggle(r.n.enabled, function (v) { ST.updateNode(r.n.id, { enabled: v }); U.toast(r.n.name + (v ? ' resumed' : ' paused')); }, 'Active: ' + r.n.name); } },
        { key: 'run', label: '', render: function (r) { return U.btn('Run now', { size: 'sm', icon: 'play', disabled: !r.n.enabled, onClick: function () { U.runTestDialog([r.n.id]); } }); } }],
      rows: rows
    });
    root.appendChild(h('div', { class: 'mt' }, U.card('Per node', 'Changes apply right away.', h('div', { style: 'margin:-8px -18px -18px' }, table), [U.btn('Spread out evenly', { size: 'sm', onClick: function () { autoStagger(); U.toast('Start times spread out'); } })])));

    /* ---- overlap check ---- */
    var active = nodes.filter(function (n) { return n.enabled; }), clashes = [];
    if (real) {
      info.clashes.forEach(function (c) { var a = ST.node(c.nodes[0]), b = ST.node(c.nodes[1]); if (a && b) clashes.push(a.name + ' and ' + b.name); });
    } else for (var i = 0; i < active.length; i++) for (var j = i + 1; j < active.length; j++) {
      var a = active[i], b = active[j], g = Math.abs(((a.offset || 0) % 60) - ((b.offset || 0) % 60));
      if (g < 3 && (a.interval <= 60 || b.interval <= 60)) clashes.push(a.name + ' and ' + b.name);
    }
    if (clashes.length) root.appendChild(h('div', { class: 'mt' }, U.callout('These nodes start within 3 minutes of each other and may measure at the same time: ' + clashes.join('; ') + '. Use “Spread out evenly” to fix it.', 'warn', 'alert')));

    /* ---- upcoming + data usage ---- */
    var now = Date.now(), up = [];
    if (real) info.nodes.forEach(function (x) { var n = ST.node(x.nodeId); if (n) x.upcoming.forEach(function (t) { up.push({ n: n, t: Date.parse(t) }); }); });
    else active.forEach(function (n) { var t = nextRun(n, now); for (var k = 0; k < 8; k++) { up.push({ n: n, t: t }); t += n.interval * 60000; } });
    up.sort(function (a, b) { return a.t - b.t; });
    var upList = h('div');
    up.slice(0, 10).forEach(function (u) { upList.appendChild(h('div', { class: 'list-row' }, U.dot(ST.nodeColor(u.n)), h('div', { class: 'grow' }, h('b', null, u.n.name), ' ', h('span', { class: 'muted' }, u.n.label)), h('span', { class: 'tabnum' }, F.date(u.t, 'time')), h('span', { class: 'muted', style: 'width:84px;text-align:right' }, F.rel(u.t)))); });
    if (!up.length) upList.appendChild(U.empty('Nothing scheduled', 'All nodes are paused.'));

    var usageRows = nodes.map(function (n) {
      if (real) return { n: n, perDay: byNode[n.id] ? byNode[n.id].bytesPerDay : 0 };
      var rs = ST.nodeResults(n.id).filter(function (r) { return r.status !== 'failed'; }).slice(-200);
      var avg = rs.length ? S.mean(rs.map(function (r) { return r.bytesDown + r.bytesUp; })) : 0;
      return { n: n, perDay: n.enabled ? avg * (1440 / n.interval) : 0 };
    });
    var usageTable = U.table({ columns: [
      { key: 'n', label: 'Node', render: function (r) { return h('span', { class: 'row' }, U.dot(ST.nodeColor(r.n)), r.n.name); } },
      { key: 'd', label: 'Per day', num: true, render: function (r) { return F.bytes(r.perDay); } },
      { key: 'm', label: 'Per month', num: true, render: function (r) { return F.bytes(r.perDay * 30); } }], rows: usageRows });
    root.appendChild(h('div', { class: 'grid cols-2 mt' },
      U.card('Coming up', 'The next tests across all nodes.', upList),
      U.card('Data used by tests', 'Estimated from recent results. Shorter intervals use more data — check this if your plan has a data cap.', h('div', { style: 'margin:-8px -18px -18px' }, usageTable))));
    ST.bandwidth.advicePanel(root);

    function tickCountdowns() {
      var t = Date.now();
      countdowns.forEach(function (c) {
        var next = real ? (byNode[c.node.id] && byNode[c.node.id].nextRun ? Date.parse(byNode[c.node.id].nextRun) : null) : (c.node.enabled ? nextRun(c.node, t) : null);
        c.span.textContent = next == null ? 'Paused' : next <= t ? 'Now' : F.date(next, 'time') + ' · ' + F.dur(next - t);
      });
    }
    tickCountdowns();
    var timer = setInterval(tickCountdowns, 1000);
    return function () { clearInterval(timer); };
  };
})();

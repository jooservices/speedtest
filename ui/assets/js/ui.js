/* DOM helpers and shared components. Text is always inserted with textContent (no innerHTML) so data cannot inject markup. */
(function () {
  'use strict';
  var ST = window.ST;
  ST.pages = ST.pages || {};
  var U = (ST.ui = {});

  U.h = function (tag, props) {
    var el = document.createElement(tag);
    props = props || {};
    Object.keys(props).forEach(function (k) {
      var v = props[k];
      if (v == null || v === false) return;
      if (k === 'class') el.className = v;
      else if (k === 'style') { if (typeof v === 'string') el.style.cssText = v; else Object.assign(el.style, v); }
      else if (k === 'dataset') Object.keys(v).forEach(function (d) { el.dataset[d] = v[d]; });
      else if (k.slice(0, 2) === 'on' && typeof v === 'function') el.addEventListener(k.slice(2).toLowerCase(), v);
      else if (k === 'value') el.value = v;
      else if (k === 'checked' || k === 'disabled' || k === 'selected' || k === 'readOnly') { if (v) el[k] = true; }
      else el.setAttribute(k, v === true ? '' : v);
    });
    for (var i = 2; i < arguments.length; i++) append(el, arguments[i]);
    return el;
  };
  function append(el, c) {
    if (c == null || c === false) return;
    if (Array.isArray(c)) c.forEach(function (x) { append(el, x); });
    else if (c instanceof Node) el.appendChild(c);
    else el.appendChild(document.createTextNode(String(c)));
  }
  var h = U.h;
  var NS = 'http://www.w3.org/2000/svg';
  U.svg = function (tag, attrs) {
    var el = document.createElementNS(NS, tag);
    Object.keys(attrs || {}).forEach(function (k) { if (attrs[k] != null) el.setAttribute(k, attrs[k]); });
    for (var i = 2; i < arguments.length; i++) append(el, arguments[i]);
    return el;
  };
  U.clear = function (el) { while (el.firstChild) el.removeChild(el.firstChild); return el; };

  /* ---------- Icons (24px stroke set) ---------- */
  var ICONS = {
    gauge: 'M4 18a8 8 0 1 1 16 0M12 18l4-5',
    dashboard: 'M3 3h7v9H3zM14 3h7v5h-7zM14 12h7v9h-7zM3 16h7v5H3z',
    server: 'M4 4h16v6H4zM4 14h16v6H4zM8 7h.01M8 17h.01',
    list: 'M8 6h13M8 12h13M8 18h13M3 6h.01M3 12h.01M3 18h.01',
    chart: 'M4 20V10M10 20V4M16 20v-7M22 20H2',
    globe: 'M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM3 12h18M12 3a14 14 0 0 1 0 18M12 3a14 14 0 0 0 0 18',
    clock: 'M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM12 7v5l3 2',
    bell: 'M18 8a6 6 0 1 0-12 0c0 7-3 9-3 9h18s-3-2-3-9M13.7 21a2 2 0 0 1-3.4 0',
    settings: 'M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM19.4 15a1.7 1.7 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.7 1.7 0 0 0-1.8-.3 1.7 1.7 0 0 0-1 1.5V21a2 2 0 1 1-4 0v-.1a1.7 1.7 0 0 0-1.1-1.5 1.7 1.7 0 0 0-1.8.3l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1a1.7 1.7 0 0 0 .3-1.8 1.7 1.7 0 0 0-1.5-1H3a2 2 0 1 1 0-4h.1a1.7 1.7 0 0 0 1.5-1.1 1.7 1.7 0 0 0-.3-1.8l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1a1.7 1.7 0 0 0 1.8.3H9a1.7 1.7 0 0 0 1-1.5V3a2 2 0 1 1 4 0v.1a1.7 1.7 0 0 0 1 1.5 1.7 1.7 0 0 0 1.8-.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.7 1.7 0 0 0-.3 1.8V9a1.7 1.7 0 0 0 1.5 1H21a2 2 0 1 1 0 4h-.1a1.7 1.7 0 0 0-1.5 1z',
    play: 'M6 4l14 8-14 8z',
    pause: 'M7 4v16M17 4v16',
    plus: 'M12 5v14M5 12h14',
    trash: 'M3 6h18M8 6V4h8v2M19 6l-1 14H6L5 6M10 11v6M14 11v6',
    down: 'M12 4v14M6 12l6 6 6-6',
    up: 'M12 20V6M6 12l6-6 6 6',
    activity: 'M22 12h-4l-3 9L9 3l-3 9H2',
    check: 'M5 12l5 5 9-10',
    x: 'M6 6l12 12M18 6L6 18',
    alert: 'M12 3l10 18H2zM12 10v5M12 18h.01',
    info: 'M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM12 11v5M12 8h.01',
    search: 'M11 18a7 7 0 1 0 0-14 7 7 0 0 0 0 14zM21 21l-5-5',
    chevron: 'M9 6l6 6-6 6',
    chevdown: 'M6 9l6 6 6-6',
    sun: 'M12 16a4 4 0 1 0 0-8 4 4 0 0 0 0 8zM12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4',
    moon: 'M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z',
    refresh: 'M21 12a9 9 0 0 1-15.5 6.2L3 16M3 12a9 9 0 0 1 15.5-6.2L21 8M21 3v5h-5M3 21v-5h5',
    copy: 'M9 9h11v11H9zM5 15H4V4h11v1',
    pin: 'M12 21s7-6 7-12a7 7 0 1 0-14 0c0 6 7 12 7 12zM12 11a2 2 0 1 0 0-4 2 2 0 0 0 0 4z',
    menu: 'M3 6h18M3 12h18M3 18h18',
    edit: 'M12 20h9M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4z',
    terminal: 'M4 17l6-5-6-5M12 19h8',
    shield: 'M12 3l8 3v6c0 5-3.5 8-8 9-4.5-1-8-4-8-9V6z',
    link: 'M10 14a5 5 0 0 0 7 0l3-3a5 5 0 0 0-7-7l-1 1M14 10a5 5 0 0 0-7 0l-3 3a5 5 0 0 0 7 7l1-1',
    file: 'M14 3H6v18h12V7zM14 3v4h4',
    layers: 'M12 3l9 5-9 5-9-5zM3 13l9 5 9-5',
    wifi: 'M5 12.5a10 10 0 0 1 14 0M8.5 16a5 5 0 0 1 7 0M12 20h.01',
    swap: 'M7 4L3 8l4 4M3 8h14M17 20l4-4-4-4M21 16H7'
  };
  U.icon = function (name, size, extra) {
    var s = size || 18;
    var svg = U.svg('svg', { width: s, height: s, viewBox: '0 0 24 24', fill: 'none', stroke: 'currentColor', 'stroke-width': 2, 'stroke-linecap': 'round', 'stroke-linejoin': 'round', 'aria-hidden': 'true', class: extra || null });
    svg.appendChild(U.svg('path', { d: ICONS[name] || ICONS.info }));
    return svg;
  };

  /* ---------- Controls ---------- */
  U.btn = function (label, opts) {
    opts = opts || {};
    var el = h('button', { type: 'button', class: 'btn ' + (opts.variant || '') + (opts.size ? ' ' + opts.size : '') + (opts.icon && !label ? ' icon' : ''), onClick: opts.onClick, disabled: opts.disabled, title: opts.title, 'aria-label': opts.aria || (label ? null : opts.title) });
    if (opts.icon) el.appendChild(U.icon(opts.icon, opts.size === 'sm' ? 15 : 16));
    if (label) el.appendChild(document.createTextNode(label));
    return el;
  };
  U.select = function (options, value, onChange, cls) {
    var el = h('select', { class: 'select ' + (cls || ''), onChange: function () { onChange && onChange(el.value); } },
      options.map(function (o) { o = typeof o === 'object' ? o : { value: o, label: o }; return h('option', { value: o.value, selected: String(o.value) === String(value) }, o.label); }));
    el.value = value;
    return el;
  };
  U.input = function (value, onInput, attrs) {
    var el = h('input', Object.assign({ class: 'input', value: value == null ? '' : value, onInput: function () { onInput && onInput(el.value); } }, attrs || {}));
    return el;
  };
  U.toggle = function (checked, onChange, label) {
    var inp = h('input', { type: 'checkbox', checked: !!checked, 'aria-label': label || 'Toggle', onChange: function () { onChange && onChange(inp.checked); } });
    return h('span', { class: 'switch' }, inp, h('i'));
  };
  U.field = function (label, control, hint, id) {
    var wrap = h('div', { class: 'field' });
    if (label) wrap.appendChild(h('label', null, label));
    wrap.appendChild(control);
    if (hint) wrap.appendChild(h('span', { class: 'hint' }, hint));
    return wrap;
  };
  U.suffix = function (control, text) { return h('div', { class: 'input-suffix' }, control, h('span', null, text)); };
  U.seg = function (items, active, onChange) {
    var wrap = h('div', { class: 'seg', role: 'tablist' });
    items.forEach(function (it) {
      it = typeof it === 'object' ? it : { value: it, label: it };
      var b = h('button', { type: 'button', role: 'tab', class: String(it.value) === String(active) ? 'on' : '', 'aria-selected': String(it.value) === String(active), onClick: function () {
        Array.prototype.forEach.call(wrap.children, function (c) { c.classList.remove('on'); c.setAttribute('aria-selected', 'false'); });
        b.classList.add('on'); b.setAttribute('aria-selected', 'true'); onChange(it.value);
      } }, it.label);
      wrap.appendChild(b);
    });
    return wrap;
  };
  U.tabs = function (items, active, onChange) {
    var wrap = h('div', { class: 'tabs', role: 'tablist' });
    items.forEach(function (it) {
      var b = h('button', { type: 'button', role: 'tab', class: it.value === active ? 'on' : '', onClick: function () { onChange(it.value); } }, it.label, it.count != null ? h('span', { class: 'muted', style: 'margin-left:6px' }, it.count) : null);
      wrap.appendChild(b);
    });
    return wrap;
  };
  U.chip = function (text, kind, icon) { return h('span', { class: 'chip ' + (kind || '') }, icon ? U.icon(icon, 12) : null, text); };
  U.statusChip = function (s) {
    if (s === 'ok') return U.chip('OK', 'good', 'check');
    if (s === 'degraded') return U.chip('Degraded', 'warn', 'alert');
    if (s === 'failed') return U.chip('Failed', 'crit', 'x');
    return U.chip(s);
  };
  U.dot = function (color) { return h('span', { class: 'dot', style: { background: color } }); };
  U.nodeTag = function (n) { return h('span', { class: 'row', style: 'gap:7px' }, U.dot(ST.nodeColor(n)), h('span', null, n.name), h('span', { class: 'muted' }, n.label)); };
  U.empty = function (title, text, action) { return h('div', { class: 'empty' }, h('h3', null, title), h('p', null, text), action ? h('div', { style: 'margin-top:12px' }, action) : null); };
  U.callout = function (text, kind, icon) { return h('div', { class: 'callout ' + (kind || '') }, U.icon(icon || 'info', 18), h('div', null, text)); };

  U.pageHead = function (title, sub, actions, crumb) {
    return h('div', { class: 'page-head' },
      h('div', null, crumb || null, h('h1', null, title), sub ? h('p', null, sub) : null),
      actions ? h('div', { class: 'page-actions' }, actions) : null);
  };
  U.card = function (title, sub, body, actions) {
    return h('section', { class: 'card' },
      (title || actions) ? h('div', { class: 'card-head' }, h('div', null, title ? h('h2', null, title) : null, sub ? h('div', { class: 'sub' }, sub) : null), actions ? h('div', { class: 'row wrap' }, actions) : null) : null,
      h('div', { class: 'card-body' }, body));
  };

  /* ---------- Data table ---------- */
  U.table = function (o) {
    var wrap = h('div', { class: 'table-wrap' }), t = h('table', { class: 'data' });
    var thead = h('thead'), tr = h('tr');
    o.columns.forEach(function (c) {
      var th = h('th', { class: (c.num ? 'num ' : '') + (c.sort ? 'sortable' : ''), scope: 'col', 'aria-sort': o.sortKey === c.key ? (o.sortDir === 'asc' ? 'ascending' : 'descending') : null,
        onClick: c.sort && o.onSort ? function () { o.onSort(c.key); } : null }, c.label);
      if (c.sort && o.sortKey === c.key) th.appendChild(h('span', { style: 'margin-left:4px' }, o.sortDir === 'asc' ? '▲' : '▼'));
      tr.appendChild(th);
    });
    thead.appendChild(tr); t.appendChild(thead);
    var tb = h('tbody');
    o.rows.forEach(function (r) {
      var row = h('tr', { class: o.onRow ? 'clickable' : '', tabindex: o.onRow ? 0 : null, onClick: o.onRow ? function () { o.onRow(r); } : null,
        onKeydown: o.onRow ? function (e) { if (e.key === 'Enter') o.onRow(r); } : null });
      o.columns.forEach(function (c) { row.appendChild(h('td', { class: c.num ? 'num' : '' }, c.render ? c.render(r) : r[c.key])); });
      tb.appendChild(row);
    });
    t.appendChild(tb); wrap.appendChild(t);
    if (!o.rows.length) wrap.appendChild(o.empty || U.empty('Nothing to show', 'No rows match the current filters.'));
    return wrap;
  };

  /* ---------- Overlays ---------- */
  U.toast = function (msg, kind) {
    var box = document.getElementById('toasts');
    var t = h('div', { class: 'toast ' + (kind || ''), role: 'status' }, U.icon(kind === 'error' ? 'alert' : 'check', 16), msg);
    box.appendChild(t);
    setTimeout(function () { t.style.opacity = 0; t.style.transition = 'opacity .25s'; setTimeout(function () { t.remove(); }, 260); }, 3200);
  };
  var openLayers = [];
  document.addEventListener('keydown', function (e) { if (e.key === 'Escape' && openLayers.length) openLayers[openLayers.length - 1].close(); });
  U.modal = function (o) {
    var layer = { close: close };
    var overlay = h('div', { class: 'overlay', onMousedown: function (e) { if (e.target === overlay && !o.sticky) close(); } });
    var foot = o.actions ? h('div', { class: 'modal-foot' }, o.actions(layer)) : null;
    var body = h('div', { class: 'modal-body' }, typeof o.body === 'function' ? o.body(layer) : o.body);
    var box = h('div', { class: 'modal ' + (o.wide ? 'wide' : ''), role: 'dialog', 'aria-modal': 'true', 'aria-label': o.title },
      h('div', { class: 'modal-head' }, h('h2', null, o.title), U.btn('', { icon: 'x', variant: 'ghost', size: 'sm', title: 'Close', onClick: function () { close(); } })), body, foot);
    overlay.appendChild(box); document.body.appendChild(overlay); openLayers.push(layer);
    var first = box.querySelector('input,select,textarea,button.primary'); if (first) setTimeout(function () { first.focus(); }, 30);
    function close() {
      if (!overlay.parentNode) return;
      overlay.remove(); openLayers = openLayers.filter(function (l) { return l !== layer; }); if (o.onClose) o.onClose();
    }
    layer.el = box; layer.body = body; layer.foot = foot; layer.setFoot = function (nodes) { U.clear(foot); append(foot, nodes); };
    return layer;
  };
  U.confirm = function (o) {
    return new Promise(function (resolve) {
      var done = false;
      U.modal({
        title: o.title, body: h('p', null, o.text), sticky: false,
        onClose: function () { if (!done) resolve(false); },
        actions: function (l) { return [U.btn('Cancel', { onClick: function () { done = true; l.close(); resolve(false); } }), U.btn(o.confirm || 'Confirm', { variant: o.danger ? 'danger solid' : 'primary', onClick: function () { done = true; l.close(); resolve(true); } })]; }
      });
    });
  };
  U.drawer = function (o) {
    var wrap = h('div', { class: 'drawer-wrap', onMousedown: function (e) { if (e.target === wrap) close(); } });
    var layer = { close: close };
    wrap.appendChild(h('aside', { class: 'drawer', role: 'dialog', 'aria-modal': 'true', 'aria-label': o.title },
      h('div', { class: 'drawer-head' }, h('h2', null, o.title), U.btn('', { icon: 'x', variant: 'ghost', size: 'sm', title: 'Close', onClick: function () { close(); } })),
      h('div', { class: 'drawer-body' }, o.body)));
    document.body.appendChild(wrap); openLayers.push(layer);
    function close() { if (!wrap.parentNode) return; wrap.remove(); openLayers = openLayers.filter(function (l) { return l !== layer; }); }
    return layer;
  };
  U.copy = function (text, msg) {
    var done = function () { U.toast(msg || 'Copied to clipboard'); };
    if (navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(text).then(done, fallback); else fallback();
    function fallback() { var ta = h('textarea', { style: 'position:fixed;opacity:0' }); ta.value = text; document.body.appendChild(ta); ta.select(); try { document.execCommand('copy'); } catch (e) {} ta.remove(); done(); }
  };
  U.download = function (name, text, mime) {
    var blob = new Blob([text], { type: mime || 'text/plain' }), url = URL.createObjectURL(blob);
    var a = h('a', { href: url, download: name }); document.body.appendChild(a); a.click(); a.remove(); setTimeout(function () { URL.revokeObjectURL(url); }, 500);
  };
  U.code = function (text) {
    return h('div', { class: 'code-copy' }, h('pre', { class: 'code' }, text), U.btn('Copy', { size: 'sm', icon: 'copy', onClick: function () { U.copy(text); } }));
  };
  U.kv = function (pairs) {
    var dl = h('dl', { class: 'kv' });
    pairs.forEach(function (p) { if (!p) return; dl.appendChild(h('dt', null, p[0])); dl.appendChild(h('dd', null, p[1])); });
    return dl;
  };

  /* ---------- Domain components ---------- */
  U.meter = function (value, expected, color) {
    var max = Math.max(expected * 1.1, value || 0), pct = value == null ? 0 : Math.min(100, value / max * 100);
    return h('div', { class: 'meter', role: 'img', 'aria-label': value == null ? 'No data' : Math.round(value / expected * 100) + '% of expected speed' },
      h('i', { style: { width: pct + '%', background: color } }), h('b', { style: { left: (expected / max * 100) + '%' }, title: 'Expected ' + ST.fmt.speed(expected) }));
  };
  U.deltaTag = function (cur, prev, goodHigh) {
    if (cur == null || prev == null || prev === 0) return h('span', { class: 'delta flat' }, '—');
    var d = (cur - prev) / Math.abs(prev) * 100, flat = Math.abs(d) < 1;
    var good = goodHigh ? d > 0 : d < 0;
    return h('span', { class: 'delta ' + (flat ? 'flat' : good ? 'up' : 'down') }, flat ? '≈ same' : (d > 0 ? '▲ ' : '▼ ') + Math.abs(d).toFixed(1) + '%');
  };
  U.rangeSelect = function (value, onChange) {
    return U.select(Object.keys(ST.ranges).map(function (k) { return { value: k, label: ST.rangeLabels[k] }; }), value, onChange, 'auto');
  };
  U.legend = function (items, hidden, onToggle) {
    var wrap = h('div', { class: 'legend' });
    items.forEach(function (it) {
      var b = h('button', { type: 'button', class: hidden[it.id] ? 'off' : '', 'aria-pressed': !hidden[it.id], title: 'Show or hide ' + it.name, onClick: function () { onToggle(it.id); } }, U.dot(it.color), it.name);
      wrap.appendChild(b);
    });
    return wrap;
  };

  /* ---------- Test runner dialog ---------- */
  U.runTestDialog = function (nodeIds) {
    var queue = nodeIds.slice(), nodes = nodeIds.map(ST.node).filter(Boolean);
    if (!nodes.length) return;
    var title = h('div', { class: 'row', style: 'gap:8px' }), phase = h('div', { class: 'muted' }), big = h('div', { class: 'live-num tabnum' }, '—'), prog = h('i', { style: 'width:0%' });
    var list = h('ul', { class: 'checklist', style: 'margin-top:14px' });
    var items = {};
    nodes.forEach(function (n) {
      var ic = h('span', { style: 'width:16px;display:inline-grid' }, U.icon('clock', 14));
      items[n.id] = { ic: ic, li: h('li', null, ic, ST.nodeTitle(n), h('span', { class: 'muted', style: 'margin-left:auto' }, 'Waiting')) };
      list.appendChild(items[n.id].li);
    });
    var cancelled = false;
    var layer = U.modal({
      title: nodes.length > 1 ? 'Running tests on ' + nodes.length + ' nodes' : 'Running speed test', sticky: true, onClose: function () { cancelled = true; },
      body: h('div', null, title, h('div', { style: 'margin:10px 0 6px' }, big), phase, h('div', { class: 'progress', style: 'margin-top:12px' }, prog), list,
        nodes.length > 1 ? h('p', { class: 'hint', style: 'margin-top:10px' }, 'Tests run one after another so they do not compete for the same connection.') : null),
      actions: function (l) { return [U.btn('Close', { onClick: function () { l.close(); } })]; }
    });
    (function next() {
      if (!queue.length || cancelled) { if (!cancelled) { phase.textContent = 'All tests finished.'; big.textContent = '✓'; prog.style.width = '100%'; } return; }
      var id = queue.shift(), node = ST.node(id), it = items[id];
      U.clear(title); title.appendChild(U.dot(ST.nodeColor(node))); title.appendChild(h('b', null, ST.nodeTitle(node)));
      it.li.className = 'run'; U.clear(it.ic); it.ic.appendChild(h('span', { class: 'spinner' })); it.li.lastChild.textContent = 'Testing…';
      ST.runTest(id, function (p) {
        if (cancelled) return;
        phase.textContent = p.phase + (p.server && p.index === 0 ? '' : p.server ? ' · ' + p.server.name + ', ' + p.server.city : '');
        prog.style.width = p.pct + '%';
        big.textContent = p.value == null ? '…' : p.unit === 'ms' ? ST.fmt.ms(p.value) : ST.fmt.speed(p.value);
      }).then(function (res) {
        it.li.className = 'ok'; U.clear(it.ic); it.ic.appendChild(U.icon(res.status === 'failed' ? 'x' : 'check', 14));
        it.li.lastChild.textContent = res.status === 'failed' ? 'Failed' + (res.error ? ': ' + res.error : '') : ST.fmt.speed(res.down) + ' ↓  ' + ST.fmt.speed(res.up) + ' ↑';
        if (!cancelled) next();
      }, function (error) {
        // The test could not even be queued, for example because the node is paused or already has a test waiting.
        it.li.className = 'ok'; U.clear(it.ic); it.ic.appendChild(U.icon('x', 14));
        it.li.lastChild.textContent = error.status === 409 ? 'Not started: the node is paused or already has a test waiting' : 'Not started: ' + error.message;
        if (!cancelled) next();
      });
    })();
  };
})();

/* Small dependency-free SVG chart kit: line/area, bar, heatmap, sparkline and an illustrative map. */
(function () {
  'use strict';
  var ST = (/** @type {any} */ (window)).ST, U = ST.ui, h = U.h, svg = U.svg;
  var C = /** @type {any} */ (ST.charts = {});

  function niceStep(span, count) {
    var raw = span / Math.max(1, count), mag = Math.pow(10, Math.floor(Math.log10(raw))), n = raw / mag;
    return (n < 1.5 ? 1 : n < 3 ? 2 : n < 7 ? 5 : 10) * mag;
  }
  function niceTicks(min, max, count) {
    if (max <= min) max = min + 1;
    var step = niceStep(max - min, count), t = [], start = Math.ceil(min / step) * step;
    for (var v = start; v <= max + step * 1e-6; v += step) t.push(+v.toFixed(10));
    return { ticks: t, step: step };
  }
  var TIME_STEPS = [5 * 60e3, 15 * 60e3, 30 * 60e3, 3600e3, 2 * 3600e3, 3 * 3600e3, 6 * 3600e3, 12 * 3600e3, 86400e3, 2 * 86400e3, 7 * 86400e3];
  function timeTicks(min, max, count) {
    var span = max - min, step = TIME_STEPS[TIME_STEPS.length - 1];
    for (var i = 0; i < TIME_STEPS.length; i++) if (span / TIME_STEPS[i] <= count) { step = TIME_STEPS[i]; break; }
    var off = new Date().getTimezoneOffset() * -60000; // align to local midnight for day-based steps
    var t = [], start = step >= 86400e3 ? Math.ceil((min + off) / step) * step - off : Math.ceil(min / step) * step;
    for (var v = start; v <= max; v += step) t.push(v);
    return { ticks: t, step: step };
  }
  function timeLabel(v, step) { return step >= 86400e3 ? ST.fmt.date(v, 'day') : ST.fmt.date(v, 'time'); }

  function makeTooltip(host) {
    var tip = h('div', { class: 'tooltip', role: 'presentation' });
    host.appendChild(tip);
    return {
      el: tip,
      show: function (x, y, head, rows) {
        U.clear(tip);
        tip.appendChild(h('div', { class: 't-head' }, head));
        rows.forEach(function (r) {
          tip.appendChild(h('div', { class: 't-row' }, h('span', null, r.color ? U.dot(r.color) : null, r.label), h('b', null, r.value)));
        });
        var w = host.clientWidth, tw = tip.offsetWidth || 160;
        tip.style.left = Math.max(tw / 2, Math.min(w - tw / 2, x)) + 'px';
        tip.style.top = Math.max(tip.offsetHeight + 4, y) + 'px';
        tip.classList.add('show');
      },
      hide: function () { tip.classList.remove('show'); }
    };
  }

  function observe(host, fn) {
    var last = 0, raf = 0;
    if (window.ResizeObserver) {
      var ro = new ResizeObserver(function () {
        var w = host.clientWidth; if (w === last || !w) return; last = w;
        cancelAnimationFrame(raf); raf = requestAnimationFrame(fn);
      });
      ro.observe(host);
      host._ro = ro;
    }
  }

  /* ---------- Line / area chart ---------- */
  C.line = function (host, o) {
    var height = o.height || 300, M = { l: 52, r: 14, t: (o.y && o.y.unit) ? 24 : 12, b: 26 };
    U.clear(host); host.classList.add('chart');
    var tip = null;
    function draw() {
      U.clear(host);
      var W = host.clientWidth || 600, plotW = W - M.l - M.r, plotH = height - M.t - M.b;
      var series = o.series.filter(function (s) { return !s.hidden; });
      var xmin = o.x.min, xmax = o.x.max, ymin = Infinity, ymax = -Infinity;
      var N = Math.max(10, Math.floor(plotW / 3)), bw = (xmax - xmin) / N;
      var drawn = series.map(function (s) {
        var pts = s.points.filter(function (p) { return p[1] != null; });
        if (o.x.type !== 'time' || pts.length <= N * 1.2) return { s: s, pts: pts };
        var buckets = {};
        pts.forEach(function (p) { var b = Math.floor((p[0] - xmin) / bw); var e = buckets[b] || (buckets[b] = { x: 0, y: 0, n: 0 }); e.x += p[0]; e.y += p[1]; e.n++; });
        return { s: s, pts: Object.keys(buckets).map(Number).sort(function (a, b) { return a - b; }).map(function (k) { var e = buckets[k]; return [e.x / e.n, e.y / e.n]; }) };
      });
      drawn.forEach(function (d) { d.pts.forEach(function (p) { if (p[1] < ymin) ymin = p[1]; if (p[1] > ymax) ymax = p[1]; }); });
      (o.refLines || []).forEach(function (r) { if (r.v > ymax) ymax = r.v; });
      if (!isFinite(ymin)) { ymin = 0; ymax = 1; }
      if (o.y.min != null) ymin = o.y.min; else ymin = Math.min(0, ymin);
      if (o.y.max != null) ymax = o.y.max; else ymax = ymax * 1.08;
      var yt = niceTicks(ymin, ymax, Math.max(3, Math.floor(plotH / 48)));
      ymax = Math.max(ymax, yt.ticks[yt.ticks.length - 1]);
      var X = function (v) { return M.l + (v - xmin) / (xmax - xmin || 1) * plotW; };
      var Y = function (v) { return M.t + plotH - (v - ymin) / (ymax - ymin || 1) * plotH; };
      var root = svg('svg', { viewBox: '0 0 ' + W + ' ' + height, width: W, height: height, role: 'img', 'aria-label': o.label || 'Line chart' });
      var g = svg('g', { class: 'grid' });
      yt.ticks.forEach(function (t) { g.appendChild(svg('line', { x1: M.l, x2: W - M.r, y1: Y(t), y2: Y(t) })); });
      root.appendChild(g);
      var ax = svg('g', { class: 'axis' });
      yt.ticks.forEach(function (t) { ax.appendChild(svg('text', { x: M.l - 8, y: Y(t) + 4, 'text-anchor': 'end' }, o.y.format ? o.y.format(t) : t)); });
      var xt;
      if (o.x.type === 'time') xt = timeTicks(xmin, xmax, Math.max(3, Math.floor(plotW / 90)));
      else xt = { ticks: o.x.ticks || [], step: 0 };
      xt.ticks.forEach(function (t) {
        ax.appendChild(svg('text', { x: X(t), y: height - 6, 'text-anchor': 'middle' }, o.x.type === 'time' ? timeLabel(t, xt.step) : (o.x.format ? o.x.format(t) : t)));
      });
      if (o.y.unit) ax.appendChild(svg('text', { x: M.l, y: 10, 'text-anchor': 'start' }, o.y.unit));
      root.appendChild(ax);
      root.appendChild(svg('line', { class: 'axis-line', x1: M.l, x2: W - M.r, y1: M.t + plotH, y2: M.t + plotH }));
      (o.refLines || []).forEach(function (r) {
        root.appendChild(svg('line', { x1: M.l, x2: W - M.r, y1: Y(r.v), y2: Y(r.v), stroke: r.color || 'var(--muted)', 'stroke-width': 1.2, 'stroke-dasharray': '5 4' }));
        root.appendChild(svg('text', { x: W - M.r - 4, y: Y(r.v) - 5, 'text-anchor': 'end', fill: 'var(--text-2)', 'font-size': 11.5 }, r.label));
      });
      drawn.forEach(function (d) {
        if (!d.pts.length) return;
        var path = d.pts.map(function (p, i) { return (i ? 'L' : 'M') + X(p[0]).toFixed(1) + ' ' + Y(p[1]).toFixed(1); }).join('');
        if (o.area && drawn.length === 1) root.appendChild(svg('path', { d: path + 'L' + X(d.pts[d.pts.length - 1][0]).toFixed(1) + ' ' + (M.t + plotH) + 'L' + X(d.pts[0][0]).toFixed(1) + ' ' + (M.t + plotH) + 'Z', fill: d.s.color, opacity: .12 }));
        root.appendChild(svg('path', { d: path, fill: 'none', stroke: d.s.color, 'stroke-width': 2, 'stroke-linejoin': 'round', 'stroke-linecap': 'round', 'stroke-dasharray': d.s.dashed ? '6 4' : null }));
        if (d.pts.length <= 60) d.pts.forEach(function (p) { root.appendChild(svg('circle', { cx: X(p[0]), cy: Y(p[1]), r: 3, fill: d.s.color, stroke: 'var(--surface)', 'stroke-width': 2 })); });
      });
      (o.events || []).forEach(function (event) {
        var timestamp = typeof event.ts === 'number' ? event.ts : Date.parse(event.ts);
        var x = X(timestamp);
        if (!isFinite(x) || x < M.l || x > W - M.r) return;
        var color = event.type === 'alert' ? 'var(--crit)' : event.type === 'isp_change' ? 'var(--warn)' : 'var(--accent)';
        root.appendChild(svg('line', { class: 'stats-event-marker', 'data-event-type': event.type, 'data-event-ts': timestamp,
          x1: x, x2: x, y1: M.t, y2: M.t + plotH, stroke: color, 'stroke-width': 1, 'stroke-dasharray': '3 3', opacity: .7, 'pointer-events': 'none' }));
        root.appendChild(svg('circle', { cx: x, cy: M.t + 5, r: 3.5, fill: color, 'pointer-events': 'none' }));
      });
      var cross = svg('line', { y1: M.t, y2: M.t + plotH, stroke: 'var(--line-strong)', 'stroke-width': 1, opacity: 0 });
      var dots = svg('g');
      root.appendChild(cross); root.appendChild(dots);
      var hit = svg('rect', { class: 'hit', x: M.l, y: M.t, width: plotW, height: plotH });
      root.appendChild(hit);
      host.appendChild(root);
      tip = makeTooltip(host);
      if (!drawn.some(function (d) { return d.pts.length; })) {
        root.appendChild(svg('text', { x: W / 2, y: height / 2, 'text-anchor': 'middle', fill: 'var(--muted)' }, o.emptyText || 'No data in this range'));
      }
      function nearest(pts, xv) {
        var lo = 0, hi = pts.length - 1;
        while (hi - lo > 1) { var mid = (lo + hi) >> 1; if (pts[mid][0] < xv) lo = mid; else hi = mid; }
        return Math.abs(pts[lo][0] - xv) <= Math.abs(pts[hi][0] - xv) ? pts[lo] : pts[hi];
      }
      function move(e) {
        var r = root.getBoundingClientRect(), px = (e.clientX - r.left) * (W / r.width);
        var xv = xmin + (px - M.l) / plotW * (xmax - xmin), rows = [], cx = null;
        U.clear(dots);
        drawn.forEach(function (d) {
          if (!d.pts.length) return;
          var p = nearest(d.pts, xv), tol = o.x.type === 'time' ? Math.max(bw * 4, (xmax - xmin) / 40) : (xmax - xmin) / 8;
          if (Math.abs(p[0] - xv) > tol) return;
          rows.push({ color: d.s.color, label: d.s.name, value: o.tipValue ? o.tipValue(d.s, p[1]) : (o.y.format ? o.y.format(p[1]) : p[1]), x: p[0] });
          dots.appendChild(svg('circle', { cx: X(p[0]), cy: Y(p[1]), r: 4.5, fill: d.s.color, stroke: 'var(--surface)', 'stroke-width': 2 }));
          if (cx == null) cx = p[0];
        });
        if (!rows.length) { tip.hide(); cross.setAttribute('opacity', 0); return; }
        rows.sort(function (a, b) { return 0; });
        var cxp = X(cx);
        cross.setAttribute('x1', cxp); cross.setAttribute('x2', cxp); cross.setAttribute('opacity', 1);
        tip.show(cxp, M.t + 6 + 40, o.x.type === 'time' ? ST.fmt.date(cx, 'full') : (o.x.format ? o.x.format(cx) : cx), rows);
      }
      hit.addEventListener('mousemove', move);
      hit.addEventListener('touchmove', function (e) { if (e.touches[0]) move(e.touches[0]); }, { passive: true });
      hit.addEventListener('mouseleave', function () { tip.hide(); cross.setAttribute('opacity', 0); U.clear(dots); });
    }
    draw(); observe(host, draw);
    return { redraw: draw };
  };

  /* ---------- Bar chart (categories) ---------- */
  C.bars = function (host, o) {
    var height = o.height || 220, M = { l: 52, r: 10, t: 10, b: 30 };
    U.clear(host); host.classList.add('chart');
    function draw() {
      U.clear(host);
      var W = host.clientWidth || 500, plotW = W - M.l - M.r, plotH = height - M.t - M.b;
      var vals = o.bars.map(function (b) { return b.value || 0; }), ymax = o.max != null ? o.max : Math.max.apply(null, vals.concat([1])) * 1.1;
      var yt = niceTicks(0, ymax, 4); ymax = Math.max(ymax, yt.ticks[yt.ticks.length - 1]);
      var Y = function (v) { return M.t + plotH - v / ymax * plotH; };
      var root = svg('svg', { viewBox: '0 0 ' + W + ' ' + height, width: W, height: height, role: 'img', 'aria-label': o.label || 'Bar chart' });
      var g = svg('g', { class: 'grid' }), ax = svg('g', { class: 'axis' });
      yt.ticks.forEach(function (t) { g.appendChild(svg('line', { x1: M.l, x2: W - M.r, y1: Y(t), y2: Y(t) })); ax.appendChild(svg('text', { x: M.l - 8, y: Y(t) + 4, 'text-anchor': 'end' }, o.format ? o.format(t) : t)); });
      root.appendChild(g); root.appendChild(ax);
      var n = o.bars.length, slot = plotW / n, bw = Math.max(2, Math.min(40, slot - 2));
      var tip;
      var every = Math.ceil(n / Math.max(1, Math.floor(plotW / 54)));
      o.bars.forEach(function (b, i) {
        var x = M.l + slot * i + (slot - bw) / 2, y = Y(b.value || 0), hh = M.t + plotH - y;
        var r = svg('rect', { x: x, y: y, width: bw, height: Math.max(0, hh), rx: Math.min(4, bw / 2), fill: b.color || o.color || 'var(--s1)', opacity: b.dim ? .35 : 1 });
        root.appendChild(r);
        if (i % every === 0) ax.appendChild(svg('text', { x: x + bw / 2, y: height - 10, 'text-anchor': 'middle' }, b.label));
        var hit = svg('rect', { class: 'hit', x: M.l + slot * i, y: M.t, width: slot, height: plotH });
        hit.addEventListener('mousemove', function () { r.setAttribute('opacity', .8); tip.show(x + bw / 2, y, b.tipHead || b.label, b.tip || [{ label: o.name || 'Value', value: o.format ? o.format(b.value) : b.value, color: b.color || o.color }]); });
        hit.addEventListener('mouseleave', function () { r.setAttribute('opacity', b.dim ? .35 : 1); tip.hide(); });
        root.appendChild(hit);
      });
      root.appendChild(svg('line', { class: 'axis-line', x1: M.l, x2: W - M.r, y1: M.t + plotH, y2: M.t + plotH }));
      if (o.xTitle) root.appendChild(svg('text', { x: M.l + plotW / 2, y: height - 0, 'text-anchor': 'middle', fill: 'var(--muted)', 'font-size': 11.5 }, o.xTitle));
      host.appendChild(root); tip = makeTooltip(host);
    }
    draw(); observe(host, draw);
    return { redraw: draw };
  };

  /* ---------- Heatmap (weekday x hour) ---------- */
  C.heat = function (host, o) {
    U.clear(host); host.classList.add('chart');
    var rows = o.rows, cols = o.cols, cellH = 26, M = { l: 44, t: 4, b: 22, r: 4 };
    function draw() {
      U.clear(host);
      var W = host.clientWidth || 600, cw = (W - M.l - M.r) / cols.length, H = M.t + rows.length * cellH + M.b;
      var vals = []; o.values.forEach(function (r) { r.forEach(function (v) { if (v != null) vals.push(v); }); });
      var min = Math.min.apply(null, vals), max = Math.max.apply(null, vals);
      if (!vals.length) { min = 0; max = 1; }
      var root = svg('svg', { viewBox: '0 0 ' + W + ' ' + H, width: W, height: H, role: 'grid', 'aria-label': o.label || 'Heatmap' });
      var ax = svg('g', { class: 'axis' });
      rows.forEach(function (r, ri) { ax.appendChild(svg('text', { x: M.l - 8, y: M.t + ri * cellH + cellH / 2 + 4, 'text-anchor': 'end' }, r)); });
      cols.forEach(function (c, ci) { if (ci % 3 === 0) ax.appendChild(svg('text', { x: M.l + ci * cw + cw / 2, y: H - 6, 'text-anchor': 'middle' }, c)); });
      root.appendChild(ax);
      var tip;
      rows.forEach(function (r, ri) {
        cols.forEach(function (c, ci) {
          var v = o.values[ri][ci], idx = v == null ? -1 : Math.min(6, Math.floor((v - min) / (max - min || 1) * 6.999));
          if (o.invert && idx >= 0) idx = 6 - idx;
          var count = o.counts ? o.counts[ri][ci] : 0;
          var detail = r + ' ' + c + ':00, ' + (v == null ? 'no data' : o.name + ' ' + o.format(v)) + ', ' + count + ' tests';
          var rect = svg('rect', { x: M.l + ci * cw + 1, y: M.t + ri * cellH + 1, width: Math.max(1, cw - 2), height: cellH - 2, rx: 3, tabindex: 0, role: 'gridcell', 'aria-label': detail, style: idx < 0 ? 'fill:var(--surface-3)' : 'fill:var(--heat-' + idx + ')' });
          function show() { tip.show(M.l + ci * cw + cw / 2, M.t + ri * cellH, r + ' · ' + c + ':00', [{ label: o.name, value: v == null ? 'No data' : o.format(v) }, { label: 'Tests', value: String(count) }]); }
          rect.addEventListener('mousemove', show);
          rect.addEventListener('focus', show);
          rect.addEventListener('mouseleave', function () { tip.hide(); });
          rect.addEventListener('blur', function () { tip.hide(); });
          root.appendChild(rect);
        });
      });
      host.appendChild(root); tip = makeTooltip(host);
    }
    draw(); observe(host, draw);
    return { redraw: draw };
  };

  /* ---------- Sparkline ---------- */
  C.spark = function (values, color, opts) {
    opts = opts || {};
    var v = values.filter(function (x) { return x != null; });
    var W = 120, H = 34, el = svg('svg', { class: 'spark', viewBox: '0 0 ' + W + ' ' + H, preserveAspectRatio: 'none', 'aria-hidden': 'true' });
    if (v.length < 2) return el;
    var min = Math.min.apply(null, v), max = Math.max.apply(null, v), lo = opts.min != null ? opts.min : min, hi = opts.max != null ? opts.max : max;
    if (hi === lo) hi = lo + 1;
    var step = W / (v.length - 1);
    var pts = v.map(function (x, i) { return [i * step, H - 3 - (x - lo) / (hi - lo) * (H - 6)]; });
    var d = pts.map(function (p, i) { return (i ? 'L' : 'M') + p[0].toFixed(1) + ' ' + p[1].toFixed(1); }).join('');
    el.appendChild(svg('path', { d: d + 'L' + W + ' ' + H + 'L0 ' + H + 'Z', fill: color, opacity: .12 }));
    el.appendChild(svg('path', { d: d, fill: 'none', stroke: color, 'stroke-width': 1.6, 'vector-effect': 'non-scaling-stroke', 'stroke-linejoin': 'round' }));
    return el;
  };

  /* ---------- Illustrative map of Vietnam ---------- */
  var OUTLINE = [[102.15,22.40],[102.70,22.75],[103.30,22.70],[104.00,22.80],[104.60,23.10],[105.40,23.30],[106.10,22.95],[106.75,22.85],[107.35,22.00],[108.05,21.50],[107.40,21.00],[106.80,20.65],[106.35,20.25],[106.10,19.75],[105.70,19.05],[105.80,18.55],[106.35,18.00],[106.85,17.40],[107.20,16.85],[107.65,16.40],[108.20,16.10],[108.70,15.55],[109.00,15.10],[109.25,14.20],[109.25,13.50],[109.30,12.60],[109.20,11.80],[108.90,11.30],[108.15,10.85],[107.55,10.45],[107.05,10.35],[106.75,10.40],[106.60,9.90],[106.20,9.55],[105.65,9.05],[105.15,8.65],[104.80,8.62],[104.80,9.20],[105.05,9.80],[104.85,10.15],[104.45,10.40],[104.95,10.70],[105.30,11.25],[105.85,11.60],[106.15,11.15],[106.60,11.55],[106.90,12.05],[107.50,12.35],[107.55,13.40],[107.60,14.15],[107.55,14.90],[107.15,15.40],[106.65,15.85],[106.55,16.50],[105.95,17.20],[105.45,17.95],[104.70,18.45],[104.15,19.10],[104.55,19.70],[103.90,20.35],[103.00,21.15],[102.30,21.75]];
  /* Real map (Leaflet, vendored). Tiles come from the map-tiles meta tag; empty means a plain background. */
  function leafletMap(host, o) {
    if (host._map) { host._map.remove(); host._map = null; } // release the previous map's listeners before redrawing
    U.clear(host); host.classList.add('map', 'map-live');
    var el = h('div', { class: 'map-canvas', role: 'region', 'aria-label': 'Map of connection locations' });
    host.appendChild(el);
    var pins = (o.pins || []).filter(function (p) { return Number.isFinite(+p.lat) && Number.isFinite(+p.lon); });
    var map = (/** @type {any} */ (window)).L.map(el, { zoomControl: true, attributionControl: true, worldCopyJump: false, minZoom: 2 });
    var meta = document.querySelector('meta[name="map-tiles"]'), url = meta ? meta.getAttribute('content') : '';
    if (url) (/** @type {any} */ (window)).L.tileLayer(url, { maxZoom: 18, attribution: '© <a href="https://www.openstreetmap.org/copyright" rel="noopener noreferrer" target="_blank">OpenStreetMap</a> contributors' }).addTo(map);
    map.attributionControl.setPrefix(false); // the OpenStreetMap credit stays; the Leaflet prefix only crowds the corner
    var bounds = [];
    pins.forEach(function (pin) {
      var colors = pin.colors && pin.colors.length ? pin.colors : ['var(--accent)'];
      var dot = h('span', { class: 'map-pin' }, colors.map(function (c) { return h('i', { style: { background: c } }); }));
      var icon = (/** @type {any} */ (window)).L.divIcon({ html: dot.outerHTML, className: 'map-pin-wrap', iconSize: [14 + colors.length * 6, 20], iconAnchor: [(14 + colors.length * 6) / 2, 10] });
      var marker = (/** @type {any} */ (window)).L.marker([pin.lat, pin.lon], { icon: icon, keyboard: true, title: pin.label + ': ' + pin.names, alt: pin.label }).addTo(map);
      var body = h('div', { class: 'map-tip' }, h('b', null, pin.label), (pin.tip || []).map(function (t) { return h('div', null, h('span', { class: 'dot', style: { background: t.color } }), ' ' + t.label + ' ', h('span', { class: 'mono' }, t.value)); }));
      marker.bindTooltip(body, { direction: 'top', offset: [0, -8] });
      marker.on('keypress', function () { marker.openTooltip(); });
      marker.on('focus', function () { marker.openTooltip(); });
      bounds.push([pin.lat, pin.lon]);
    });
    if (bounds.length === 1) map.setView(bounds[0], 9);
    else if (bounds.length) map.fitBounds(bounds, { paddingTopLeft: [40, 40], paddingBottomRight: [40, 60], maxZoom: 9 });
    else map.setView([16.2, 106.2], 5);
    host._map = map;
    return map;
  }
  C.map = function (host, o) {
    if (!ST.demo && (/** @type {any} */ (window)).L) return leafletMap(host, o);
    U.clear(host); host.classList.add('map');
    var W = 360, H = 620, lon0 = 101.5, lon1 = 110.2, lat0 = 8, lat1 = 23.8;
    var P = function (lat, lon) { return [(lon - lon0) / (lon1 - lon0) * W, (lat1 - lat) / (lat1 - lat0) * H]; };
    var root = svg('svg', { viewBox: '0 0 ' + W + ' ' + H, role: 'img', 'aria-label': 'Map of connection locations', style: 'max-height:560px;margin:0 auto' });
    for (var lat = 10; lat <= 22; lat += 4) { var y = P(lat, lon0)[1]; root.appendChild(svg('line', { class: 'graticule', x1: 0, x2: W, y1: y, y2: y })); root.appendChild(svg('text', { x: 6, y: y - 3, fill: 'var(--muted)', 'font-size': 10 }, lat + '°N')); }
    for (var lon = 103; lon <= 109; lon += 2) { var x = P(lat0, lon)[0]; root.appendChild(svg('line', { class: 'graticule', x1: x, x2: x, y1: 0, y2: H })); root.appendChild(svg('text', { x: x + 3, y: H - 6, fill: 'var(--muted)', 'font-size': 10 }, lon + '°E')); }
    root.appendChild(svg('path', { class: 'land', d: OUTLINE.map(function (p, i) { var q = P(p[1], p[0]); return (i ? 'L' : 'M') + q[0].toFixed(1) + ' ' + q[1].toFixed(1); }).join('') + 'Z' }));
    host.appendChild(root);
    var tip = makeTooltip(host);
    (o.pins || []).forEach(function (pin) {
      var q = P(pin.lat, pin.lon), colors = pin.colors && pin.colors.length ? pin.colors : ['var(--accent)'], n = colors.length;
      var grp = svg('g', { tabindex: 0, role: 'button', 'aria-label': pin.label + ': ' + pin.names });
      grp.appendChild(svg('circle', { cx: q[0], cy: q[1], r: 8 + n * 2.5, fill: 'var(--accent)', opacity: .12 }));
      colors.forEach(function (c, i) {
        var a = (i / n) * Math.PI * 2 - Math.PI / 2, rr = n === 1 ? 0 : 7;
        grp.appendChild(svg('circle', { cx: q[0] + Math.cos(a) * rr, cy: q[1] + Math.sin(a) * rr, r: 5.5, fill: c, stroke: 'var(--surface)', 'stroke-width': 2 }));
      });
      grp.appendChild(svg('text', { class: 'pin-label', x: q[0] + 18, y: q[1] + 4 }, pin.label));
      function show() { tip.show(q[0] / W * host.clientWidth * (root.getBoundingClientRect().width / host.clientWidth) + (host.clientWidth - root.getBoundingClientRect().width) / 2, q[1] / H * root.getBoundingClientRect().height, pin.label, pin.tip || []); }
      grp.addEventListener('mouseenter', show); grp.addEventListener('focus', show);
      grp.addEventListener('mouseleave', function () { tip.hide(); }); grp.addEventListener('blur', function () { tip.hide(); });
      root.appendChild(grp);
    });
    host.appendChild(h('div', { class: 'tag' }, 'Illustrative map. The production build uses real map tiles.'));
  };
})();

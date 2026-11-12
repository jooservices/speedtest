/* Alerts: rules and events. */
(function () {
  'use strict';
  // Real mode returns promises that settle when the center has confirmed the change; demo mode returns nothing.
  function friendly(error) {
    if (error.status === 412) return 'This was changed elsewhere. Reload the page and try again.';
    if (error.status === 409) return 'This conflicts with the current state. Reload the page and try again.';
    if (error.status === 422) return 'Some values are not valid. Check them and try again.';
    return error.message;
  }
  function settle(result, ok, fail) {
    if (result && typeof result.then === 'function') return result.then(ok, fail || function (error) { (/** @type {any} */ (window)).ST.ui.toast(friendly(error), 'error'); });
    return ok(result);
  }
  var ST = (/** @type {any} */ (window)).ST, U = ST.ui, h = U.h, F = ST.fmt;
  var q = { tab: 'events', show: 'open', sev: 'all', ispId: '' };
  var METRICS = [
    { value: 'down_pct', label: 'Download (% of expected)', unit: '%', ops: ['<'] },
    { value: 'up_pct', label: 'Upload (% of expected)', unit: '%', ops: ['<'] },
    { value: 'ping', label: 'Latency', unit: 'ms', ops: ['>'] },
    { value: 'jitter', label: 'Jitter', unit: 'ms', ops: ['>'] },
    { value: 'loss', label: 'Packet loss', unit: '%', ops: ['>'] },
    { value: 'failed', label: 'Test fails', unit: '', ops: [] },
    { value: 'ip_changed', label: 'Public IP changes', unit: '', ops: [] }];
  var SEV = [{ value: 'info', label: 'Info' }, { value: 'warn', label: 'Warning' }, { value: 'crit', label: 'Critical' }];

  function ruleDialog(rule) {
    var isNew = !rule;
    var r = rule ? JSON.parse(JSON.stringify(rule)) : { id: '', name: '', metric: 'down_pct', op: '<', value: 50, forTests: 3, nodes: 'all', severity: 'warn', channels: ['email'], enabled: true };
    var body = h('div', { class: 'stack' }), err = h('div');
    function draw() {
      U.clear(body);
      var m = METRICS.filter(function (x) { return x.value === r.metric; })[0];
      body.appendChild(U.field('Name', U.input(r.name, function (v) { r.name = v; }, { placeholder: 'e.g. Evening slowdown', maxlength: 60 })));
      body.appendChild(U.field('Alert me when', U.select(METRICS, r.metric, function (v) { r.metric = v; var mm = METRICS.filter(function (x) { return x.value === v; })[0]; r.op = mm.ops[0] || '='; if (v === 'ping') r.value = 60; if (v === 'loss') r.value = 2; if (v === 'jitter') r.value = 20; if (v.indexOf('_pct') > 0) r.value = 50; if (v === 'failed' || v === 'ip_changed') r.forTests = v === 'failed' ? 3 : 1; draw(); })));
      if (m.ops.length) body.appendChild(h('div', { class: 'field-row' },
        U.field('Threshold', U.suffix(U.input(r.value, function (v) { r.value = +v; }, { type: 'number', min: 0, step: 'any' }), m.unit === '%' ? '%' : 'ms')),
        U.field('For this many tests in a row', U.input(r.forTests, function (v) { r.forTests = Math.max(1, Math.round(+v) || 1); }, { type: 'number', min: 1, max: 20 }), 'Avoids alerts from one odd result.')));
      else if (r.metric === 'failed') body.appendChild(U.field('Number of failed tests in a row', U.input(r.forTests, function (v) { r.forTests = Math.max(1, Math.round(+v) || 1); }, { type: 'number', min: 1, max: 20 })));
      var all = r.nodes === 'all';
      body.appendChild(U.field('Applies to', h('div', { class: 'stack', style: 'gap:8px' },
        h('label', { class: 'check' }, h('input', { type: 'radio', name: 'scope', checked: all, onChange: function () { r.nodes = 'all'; draw(); } }), 'All nodes'),
        h('label', { class: 'check' }, h('input', { type: 'radio', name: 'scope', checked: !all, onChange: function () { r.nodes = ST.nodes().map(function (n) { return n.id; }); draw(); } }), 'Only some nodes'),
        !all ? h('div', { class: 'legend', style: 'margin-left:24px' }, ST.nodes().map(function (n) {
          var on = r.nodes.indexOf(n.id) >= 0;
          return h('button', { type: 'button', class: on ? '' : 'off', 'aria-pressed': on, onClick: function (e) { var i = r.nodes.indexOf(n.id); if (i >= 0) r.nodes.splice(i, 1); else r.nodes.push(n.id); e.currentTarget.classList.toggle('off', i >= 0); e.currentTarget.setAttribute('aria-pressed', i < 0); } }, U.dot(ST.nodeColor(n)), n.name);
        })) : null)));
      body.appendChild(h('div', { class: 'field-row' },
        U.field('Importance', U.select(SEV, r.severity, function (v) { r.severity = v; })),
        U.field('Notify by', h('div', { class: 'row wrap', style: 'gap:12px;height:34px' }, ['email', 'telegram', 'slack', 'webhook'].map(function (c) {
          return h('label', { class: 'check' }, h('input', { type: 'checkbox', checked: r.channels.indexOf(c) >= 0, onChange: function (e) { var i = r.channels.indexOf(c); if (e.target.checked && i < 0) r.channels.push(c); if (!e.target.checked && i >= 0) r.channels.splice(i, 1); } }), c.charAt(0).toUpperCase() + c.slice(1));
        })))));
      body.appendChild(err);
    }
    draw();
    U.modal({ title: isNew ? 'New alert rule' : 'Edit alert rule', wide: true, body: body, actions: function (l) { return [U.btn('Cancel', { onClick: function () { l.close(); } }), U.btn(isNew ? 'Create rule' : 'Save rule', { variant: 'primary', onClick: function () {
      U.clear(err);
      var problem = !r.name.trim() ? 'Give the rule a name.' : r.nodes !== 'all' && !r.nodes.length ? 'Choose at least one node.' : !r.channels.length ? 'Choose at least one way to be notified.' : (METRICS.filter(function (x) { return x.value === r.metric; })[0].ops.length && !(r.value >= 0)) ? 'Enter a threshold.' : null;
      if (problem) { err.appendChild(U.callout(problem, 'crit', 'alert')); return; }
      r.name = r.name.trim();
      settle(ST.saveRule(r), function () { l.close(); U.toast(isNew ? 'Rule created' : 'Rule saved'); }, function (error) { err.appendChild(U.callout(friendly(error), 'crit', 'alert')); });
    } })]; } });
  }

  ST.pages.alerts = function (root) {
    var events = ST.events(), rules = ST.rules(), open = events.filter(function (e) { return !e.acked; });
    root.appendChild(U.pageHead('Alerts', 'Get told when a connection slows down, drops, or changes address.', [
      q.tab === 'rules' ? U.btn('New rule', { icon: 'plus', variant: 'primary', onClick: function () { ruleDialog(null); } }) : U.btn('Acknowledge all', { icon: 'check', disabled: !open.length, onClick: function () { settle(ST.ackAll(), function () { U.toast('All alerts acknowledged'); }); } })]));
    root.appendChild(U.tabs([{ value: 'events', label: 'Activity', count: open.length }, { value: 'rules', label: 'Rules', count: rules.length }], q.tab, function (v) { q.tab = v; ST.refresh(); }));

    if (q.tab === 'events') {
      var filt = h('div', { class: 'row wrap mb' },
        U.seg([{ value: 'open', label: 'Open' }, { value: 'all', label: 'All' }], q.show, function (v) { q.show = v; ST.refresh(); }),
        U.select([{ value: 'all', label: 'Any importance' }].concat(SEV), q.sev, function (v) { q.sev = v; ST.refresh(); }, 'auto'),
        h('div', { style: 'min-width:190px' }, U.field('ISP', U.select([{ value: '', label: 'Any ISP' }].concat(ST.ispOptions()), q.ispId, function (v) {
          q.ispId = v;
          ST.refresh();
        }, 'auto'))));
      root.appendChild(filt);
      var list = events.filter(function (e) { return (!q.ispId || e.ispId === q.ispId) && (q.show === 'all' || !e.acked) && (q.sev === 'all' || e.severity === q.sev); });
      var card = h('div', { class: 'card card-pad' });
      if (!list.length) card.appendChild(U.empty(q.show === 'open' ? 'All clear' : 'No alerts', q.show === 'open' ? 'No open alerts. Switch to “All” to see past ones.' : 'Nothing has triggered a rule in this period.'));
      list.slice(0, 60).forEach(function (e) {
        var si = ST.sevInfo(e.severity), n = ST.node(e.nodeId);
        card.appendChild(h('div', { class: 'list-row' },
          h('div', { class: 'ev-ico ' + si.cls }, U.icon(si.icon, 15)),
          h('div', { class: 'grow' },
            h('div', { class: 'row wrap', style: 'gap:8px' }, h('b', null, e.title), U.chip(si.label, si.cls), e.resolvedAt ? U.chip('Resolved', 'good', 'check') : (e.severity !== 'info' ? U.chip('Ongoing', 'warn') : null)),
            h('div', { class: 'muted' }, (n ? n.name + ' · ' : '') + F.date(e.ts, 'full') + (e.resolvedAt ? ' · lasted ' + F.dur(e.resolvedAt - e.ts) : '')),
            h('div', { class: 'text-2' }, e.detail)),
          e.acked ? U.btn('Reopen', { size: 'sm', onClick: function () { settle(ST.ack(e.id, false), function () {}); } }) : U.btn('Acknowledge', { size: 'sm', icon: 'check', onClick: function () { settle(ST.ack(e.id), function () { U.toast('Alert acknowledged'); }); } })));
      });
      if (list.length > 60) card.appendChild(h('p', { class: 'hint', style: 'padding-top:10px' }, 'Showing the 60 most recent of ' + list.length + '.'));
      root.appendChild(card);
    } else {
      var rc = h('div', { class: 'card card-pad' });
      if (!rules.length) rc.appendChild(U.empty('No rules', 'Create a rule to be alerted automatically.', U.btn('New rule', { icon: 'plus', variant: 'primary', onClick: function () { ruleDialog(null); } })));
      rules.forEach(function (r) {
        var si = ST.sevInfo(r.severity), scope = r.nodes === 'all' ? 'All nodes' : r.nodes.map(function (id) { var n = ST.node(id); return n ? n.name : id; }).join(', ');
        rc.appendChild(h('div', { class: 'list-row' }, U.toggle(r.enabled, function (v) { r.enabled = v; settle(ST.saveRule(r), function () { U.toast(r.name + (v ? ' turned on' : ' turned off')); }); }, 'Enable ' + r.name),
          h('div', { class: 'grow' }, h('div', { class: 'row wrap', style: 'gap:8px' }, h('b', null, r.name), U.chip(si.label, si.cls)), h('div', { class: 'text-2' }, ST.ruleText(r)), h('div', { class: 'muted' }, scope + ' · ' + r.channels.join(', '))),
          U.btn('Edit', { size: 'sm', icon: 'edit', onClick: function () { ruleDialog(r); } }),
          U.btn('', { size: 'sm', icon: 'trash', title: 'Delete ' + r.name, onClick: function () { U.confirm({ title: 'Delete this rule?', text: '“' + r.name + '” stops alerting. Past alerts from it disappear.', confirm: 'Delete rule', danger: true }).then(function (ok) { if (ok) settle(ST.deleteRule(r.id), function () { U.toast('Rule deleted'); }); }); } })));
      });
      root.appendChild(rc);
      root.appendChild(h('div', { class: 'mt' }, U.callout(ST.demo ? 'Rules are checked against every new result. Alerts in this preview are worked out from the sample data, so editing a rule changes the Activity tab immediately.' : 'Rules are checked against every new result as it arrives. Alerts open when the streak is reached and close by themselves when it breaks.', null, 'info')));
    }
  };
})();

/* Owner-facing, secret-free notification delivery history. */
(function () {
  'use strict';
  var ST = (/** @type {any} */ (window)).ST, F = ST.fmt;
  var PAGE_SIZE = 10;
  var CHANNELS = { email: 'Email', telegram: 'Telegram', slack: 'Slack', webhook: 'Webhook' };
  var STATUSES = {
    pending: { label: 'Pending', kind: 'warn' },
    sent: { label: 'Sent', kind: 'good' },
    failed: { label: 'Failed', kind: 'crit' }
  };

  function deliveryTime(value) {
    var timestamp = typeof value === 'string' ? Date.parse(value) : NaN;
    return Number.isFinite(timestamp) ? F.date(timestamp, 'full') : '—';
  }
  function channelName(value) { return CHANNELS[value] || 'Other'; }
  function attemptCount(value) { return Number.isInteger(value) && value >= 0 ? F.num(value) : '—'; }
  function failureDetail(delivery) {
    if (!delivery || delivery.status !== 'failed') return '—';
    return typeof delivery.error === 'string' && delivery.error.trim() ? delivery.error.trim().slice(0, 240) : 'No safe failure detail available.';
  }
  function rowView(delivery) {
    delivery = delivery || {};
    var status = STATUSES[delivery.status] || { label: 'Unknown', kind: 'warn' };
    return {
      when: deliveryTime(delivery.createdAt),
      channel: channelName(delivery.channel),
      statusLabel: status.label,
      statusKind: status.kind,
      attempts: attemptCount(delivery.attempts),
      detail: failureDetail(delivery)
    };
  }

  function mount(root) {
    var U = ST.ui, h = U.h;
    var state = { page: 1, status: '', channel: '', rows: [], pagination: { current_page: 1, per_page: PAGE_SIZE, total: 0, last_page: 1 }, loading: false, error: false, reload: false };
    var results = h('div', { id: 'delivery-history-results', 'aria-busy': 'true', 'data-testid': 'delivery-history-results' });
    var pageStatus = h('span', { class: 'hint', role: 'status', 'aria-live': 'polite', 'aria-atomic': 'true' });
    var previous = U.btn('Previous', { size: 'sm', aria: 'Previous delivery history page', onClick: function () { changePage(state.page - 1); } });
    var next = U.btn('Next', { size: 'sm', aria: 'Next delivery history page', onClick: function () { changePage(state.page + 1); } });
    var statusSelect = U.select([
      { value: '', label: 'All statuses' }, { value: 'pending', label: 'Pending' },
      { value: 'sent', label: 'Sent' }, { value: 'failed', label: 'Failed' }
    ], state.status, changeStatus);
    var channelSelect = U.select([
      { value: '', label: 'All channels' }, { value: 'email', label: 'Email' },
      { value: 'telegram', label: 'Telegram' }, { value: 'slack', label: 'Slack' },
      { value: 'webhook', label: 'Webhook' }
    ], state.channel, changeChannel);
    statusSelect.setAttribute('aria-controls', results.id);
    channelSelect.setAttribute('aria-controls', results.id);

    root.appendChild(h('div', { class: 'stack', 'data-testid': 'delivery-history' },
      h('div', null, h('h3', { style: 'margin:0 0 4px;font-size:1rem' }, 'Delivery history'),
        h('p', { class: 'hint', style: 'margin:0' }, 'Newest deliveries first. Messages, provider responses and destination credentials are never shown.')),
      h('div', { class: 'row wrap', style: 'gap:12px' },
        U.field('Status', statusSelect), U.field('Channel', channelSelect)),
      h('div', { class: 'stack', style: 'gap:8px' }, results),
      h('nav', { class: 'row between wrap', 'aria-label': 'Delivery history pagination' }, previous, pageStatus, next)));

    function render() {
      U.clear(results);
      results.setAttribute('aria-busy', state.loading ? 'true' : 'false');
      previous.disabled = state.loading || state.page <= 1;
      next.disabled = state.loading || state.page >= state.pagination.last_page;
      pageStatus.textContent = state.loading
        ? ''
        : 'Page ' + state.page + ' of ' + state.pagination.last_page + ' · ' + F.count(state.pagination.total) + ' deliveries';
      if (state.loading) {
        results.appendChild(h('p', { class: 'hint', role: 'status', 'aria-live': 'polite' }, 'Loading delivery history…'));
        return;
      }
      if (state.error) {
        results.appendChild(h('div', { class: 'stack', role: 'alert' },
          U.callout('Could not load delivery history. Check your connection and try again.', 'warn', 'info'),
          U.btn('Retry', { size: 'sm', icon: 'refresh', onClick: load })));
        return;
      }
      if (!state.rows.length) {
        var filtered = Boolean(state.status || state.channel);
        results.appendChild(U.empty(
          filtered ? 'No deliveries match these filters' : 'No deliveries recorded yet',
          filtered ? 'Change or clear the filters to see other deliveries.' : 'Notification deliveries will appear here after the next attempt.',
          filtered ? U.btn('Clear filters', { size: 'sm', onClick: clearFilters }) : null));
        return;
      }
      var table = U.table({
        cards: true,
        columns: [
          { key: 'when', label: 'When' }, { key: 'channel', label: 'Channel' },
          { key: 'statusLabel', label: 'Status', render: function (delivery) { return U.chip(delivery.statusLabel, delivery.statusKind); } },
          { key: 'attempts', label: 'Attempts', num: true }, { key: 'detail', label: 'Failure detail' }
        ],
        rows: state.rows.map(rowView),
        empty: U.empty('No deliveries on this page', 'Choose another page to see more deliveries.')
      });
      table.setAttribute('role', 'region');
      table.setAttribute('aria-label', 'Delivery history table');
      table.setAttribute('tabindex', '0');
      results.appendChild(table);
    }
    function request() {
      return ST.api.deliveries.list({ page: state.page, size: PAGE_SIZE, status: state.status, channel: state.channel });
    }
    function load() {
      if (state.loading) { state.reload = true; render(); return Promise.resolve(); }
      state.loading = true;
      state.error = false;
      render();
      return request().then(function (response) {
        if (state.reload) return;
        state.rows = response.data;
        state.pagination = response.meta.pagination;
      }, function () {
        if (!state.reload) state.error = true;
      }).then(function () {
        state.loading = false;
        if (state.reload) {
          state.reload = false;
          return load();
        }
        render();
      });
    }
    function changePage(page) {
      if (state.loading || page < 1 || page > state.pagination.last_page) return;
      state.page = page;
      load();
    }
    function changeStatus(value) {
      state.status = value;
      state.page = 1;
      load();
    }
    function changeChannel(value) {
      state.channel = value;
      state.page = 1;
      load();
    }
    function clearFilters() {
      state.status = '';
      state.channel = '';
      state.page = 1;
      statusSelect.value = '';
      channelSelect.value = '';
      statusSelect.focus();
      load();
    }

    load();
    return { refresh: load };
  }

  ST.deliveryHistory = { mount: mount, rowView: rowView };
})();

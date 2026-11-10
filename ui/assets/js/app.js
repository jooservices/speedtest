/* App shell: sidebar, top bar, hash router, theme. */
(function () {
  'use strict';
  var ST = window.ST, U = ST.ui, h = U.h;
  ST.pages = ST.pages || {};

  var NAV = [
    { path: '/', label: 'Dashboard', icon: 'dashboard' },
    { path: '/nodes', label: 'Nodes', icon: 'server' },
    { path: '/history', label: 'Results', icon: 'list' },
    { path: '/stats', label: 'Statistics', icon: 'chart' },
    { path: '/network', label: 'IP & location', icon: 'globe' },
    { path: '/schedules', label: 'Schedules', icon: 'clock' },
    { path: '/alerts', label: 'Alerts', icon: 'bell', badge: true },
    { path: '/settings', label: 'Settings', icon: 'settings' }
  ];

  function applyTheme() {
    var t = ST.settings().theme === 'light' ? 'light' : 'dark';
    document.documentElement.setAttribute('data-theme', t);
  }
  function savePreferences(patch) {
    Promise.resolve(ST.setSettings(patch)).catch(function (error) { U.toast(error.message, 'error'); });
  }
  ST.toggleTheme = function () { savePreferences({ theme: ST.settings().theme === 'light' ? 'dark' : 'light' }); };

  function route() {
    var raw = (location.hash || '#/').slice(1).split('?')[0] || '/';
    var parts = raw.split('/').filter(Boolean);
    if (!parts.length) return { page: 'dashboard', path: '/', params: {} };
    switch (parts[0]) {
      case 'signin': return { page: 'signin', path: '/signin', params: {} };
      case 'nodes': return parts[1] ? { page: 'nodeDetail', path: '/nodes', params: { id: parts[1] } } : { page: 'nodes', path: '/nodes', params: {} };
      case 'history': return { page: 'history', path: '/history', params: {} };
      case 'stats': return { page: 'stats', path: '/stats', params: {} };
      case 'network': return { page: 'network', path: '/network', params: {} };
      case 'schedules': return { page: 'schedules', path: '/schedules', params: {} };
      case 'alerts': return { page: 'alerts', path: '/alerts', params: {} };
      case 'settings': return { page: 'settings', path: '/settings', params: {} };
    }
    return { page: 'notFound', path: '', params: {} };
  }

  var cleanup = null, current = null;

  function renderSidebar() {
    var sb = document.getElementById('sidebar'); U.clear(sb);
    var open = ST.openEventCount(), settings = ST.settings(), nodes = ST.nodes();
    sb.appendChild(h('div', { class: 'brand' },
      h('div', { class: 'brand-mark' }, U.icon('gauge', 18)),
      h('div', null, h('div', { class: 'brand-name' }, 'Speedtest'), h('div', { class: 'brand-sub' }, 'Network monitor'))));
    var nav = h('nav', { class: 'nav' });
    NAV.forEach(function (n) {
      var active = current && (current.path === n.path);
      nav.appendChild(h('a', { href: '#' + n.path, class: active ? 'active' : '', 'aria-current': active ? 'page' : null, onClick: closeMenu },
        U.icon(n.icon, 18), n.label, n.badge && open ? h('span', { class: 'badge', 'aria-label': open + ' open alerts' }, open) : null));
    });
    sb.appendChild(nav);
    if (ST.demo) sb.appendChild(h('div', { class: 'sidebar-foot' },
      h('div', { class: 'demo-note' }, h('b', null, 'Interface preview. '), 'All data on screen is sample data generated in your browser; nothing is measured yet.'),
      h('button', { class: 'btn sm', type: 'button', onClick: function () {
        U.confirm({ title: 'Reset demo data?', text: 'This clears everything you changed in this preview (nodes, rules, settings, manual tests) and restores the sample data.', confirm: 'Reset', danger: true }).then(function (ok) { if (ok) ST.resetDemo(); });
      } }, U.icon('refresh', 14), 'Reset demo data')));
    var actions = h('div', { class: 'sidebar-actions' }), dark = settings.theme !== 'light';
    actions.appendChild(h('div', { class: 'chip good', role: 'status', title: 'Nodes reporting' }, U.icon('activity', 12), nodes.filter(function (n) { return n.enabled; }).length + ' of ' + nodes.length + ' nodes active'));
    actions.appendChild(U.btn(dark ? 'Light theme' : 'Dark theme', { icon: dark ? 'sun' : 'moon', variant: 'ghost sidebar-action', title: dark ? 'Switch to light theme' : 'Switch to dark theme', onClick: ST.toggleTheme }));
    if (!ST.demo) actions.appendChild(U.btn('Sign out', { icon: 'logout', variant: 'ghost sidebar-action', onClick: function () { ST.signOut(); } }));
    sb.appendChild(actions);
  }

  function renderTopbar() {
    var tb = document.getElementById('topbar'); U.clear(tb);
    var s = ST.settings(), nodes = ST.nodes();
    tb.appendChild(U.btn('', { icon: 'menu', variant: 'ghost menu-btn', title: 'Open menu', onClick: function () { document.getElementById('sidebar').classList.toggle('open'); } }));
    tb.appendChild(h('span', { class: 'row muted', style: 'gap:6px' }, U.icon('clock', 15), h('span', { class: 'hide-phone' }, 'Time range')));
    tb.appendChild(U.rangeSelect(s.range, function (v) { savePreferences({ range: v }); }));
    tb.appendChild(h('div', { class: 'spacer' }));
    tb.appendChild(U.btn('Run all tests', { icon: 'play', variant: 'primary', onClick: function () {
      var ids = nodes.filter(function (n) { return n.enabled; }).map(function (n) { return n.id; });
      if (!ids.length) { U.toast('No active nodes to test', 'error'); return; }
      U.runTestDialog(ids);
    } }));
  }
  function closeMenu() { document.getElementById('sidebar').classList.remove('open'); }

  function renderPage() {
    current = route();
    var view = document.getElementById('view');
    var shell = view.closest('.app');
    if (shell) shell.classList.toggle('signed-out', !ST.demo && current.page === 'signin');
    if (cleanup) { try { cleanup(); } catch (e) {} cleanup = null; }
    var scroll = window.scrollY;
    U.clear(view);
    var fn = ST.pages[current.page];
    var remote = ST.remote;
    var blocked = !ST.demo && current.page !== 'notFound' && current.page !== 'signin' && remote.status !== 'ready';
    try {
      if (!ST.demo && current.page === 'signin') {
        cleanup = ST.pages.signin(view);
      } else if (blocked) {
        var failed = remote.status === 'error';
        view.appendChild(h('div', { role: failed ? 'alert' : 'status', 'aria-live': 'polite', 'data-load-state': remote.status },
          U.empty(failed ? 'Could not load data' : 'Loading data',
            failed ? remote.error.message : 'Loading this view from the command center…',
            failed ? U.btn('Retry', { icon: 'refresh', onClick: function () { ST.load(); } }) : null)));
      } else cleanup = fn ? fn(view, current.params) : null;
    } catch (e) {
      console.error(e);
      view.appendChild(U.empty('Something went wrong', 'This view could not be drawn: ' + e.message));
    }
    if (!fn && current.page !== 'signin') view.appendChild(U.empty('Page not found', 'There is no page at this address.', h('a', { class: 'btn', href: '#/' }, 'Back to the dashboard')));
    document.title = (current.page === 'dashboard' ? 'Dashboard' : (current.page.replace(/([A-Z])/g, ' $1').replace(/^./, function (c) { return c.toUpperCase(); }))) + ' · Speedtest';
    renderSidebar(); renderTopbar();
    return scroll;
  }

  var lastPageKey = '';
  function refresh(keepScroll) {
    var r = route(), key = r.page + JSON.stringify(r.params);
    var y = window.scrollY;
    renderPage();
    if (keepScroll && key === lastPageKey) window.scrollTo(0, y); else window.scrollTo(0, 0);
    lastPageKey = key;
  }

  window.addEventListener('hashchange', function () {
    if (!ST.demo && route().page === 'signin' && ST.remote.status === 'loading') ST.cancelLoad();
    closeMenu(); refresh(false); document.getElementById('view').focus({ preventScroll: true });
    if (!ST.demo && route().page !== 'signin' && ST.remote.status === 'idle') ST.load();
  });
  ST.on('settings', function () { applyTheme(); refresh(true); });
  ST.on('data', function () { refresh(true); });
  ST.on('remote', function () { refresh(true); });
  ST.refresh = function () { refresh(true); };

  applyTheme();
  refresh(false);
  if (!ST.demo && route().page !== 'signin') ST.load();
})();

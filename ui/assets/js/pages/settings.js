/* Settings: general, speed test engine, location service, notifications, data, access. */
(function () {
  'use strict';
  var ST = window.ST, U = ST.ui, h = U.h, F = ST.fmt;
  var tab = 'general';

  function row(title, desc, ctl) { return h('div', { class: 'setting' }, h('div', null, h('div', { style: 'font-weight:600' }, title), desc ? h('div', { class: 'hint' }, desc) : null), h('div', { class: 's-ctl' }, ctl)); }
  var real = !ST.demo, pending = {};
  function saved() { if (!real) U.toast('Saved'); }
  function fail(error) {
    U.toast(error.status === 412 ? 'Settings were changed elsewhere. Reload the page and try again.' : error.status === 422 ? invalidMessage(error) : error.message, 'error');
  }
  function invalidMessage(error) {
    var fields = error.errors && typeof error.errors === 'object' ? Object.keys(error.errors) : [];
    return fields.length ? 'Check these fields: ' + fields.join(', ') + '.' : 'Some values are not valid. Check them and try again.';
  }
  // Demo mode keeps its instant local edits. With a real center nothing is saved until the owner finishes typing (the
  // change event) or picks a value, because every save is a request that the center validates.
  var activityFilter = '';
  function set(patch, silent) {
    if (!real) return ST.setSettings(patch, silent);
    Object.assign(pending, patch);
    if (silent) return Promise.resolve();
    return flush();
  }
  function flush() {
    var patch = pending; pending = {};
    if (!Object.keys(patch).length) return Promise.resolve();
    return Promise.resolve(ST.setSettings(patch)).then(function () { U.toast('Saved'); }, function (error) { fail(error); });
  }

  ST.pages.settings = function (root) {
    var s = ST.settings();
    root.appendChild(U.pageHead('Settings', 'Preferences for this dashboard and for every node.'));
    root.appendChild(U.tabs([
      { value: 'general', label: 'General' }, { value: 'engine', label: 'Speed test' }, { value: 'geo', label: 'Location' },
      { value: 'notify', label: 'Notifications' }, { value: 'data', label: 'Data' }, { value: 'access', label: 'Access' }].concat(real ? [{ value: 'activity', label: 'Activity log' }] : []), tab, function (v) { tab = v; ST.refresh(); }));
    var body = h('div', { class: 'card card-pad', style: 'max-width:860px' });
    if (real) body.addEventListener('change', function () { flush(); });
    root.appendChild(body);

    // ---- Real-mode tabs: secrets are write-only, tests go through the center, exports are downloads of the API. ----
    function realNotify() {
      var channels = [
        { key: 'email', name: 'Email', to: 'Address', toHint: 'ops@example.com', secret: 'SMTP password', secretHint: 'Only if your mail server asks for one', needsTo: true },
        { key: 'telegram', name: 'Telegram', to: 'Chat ID', toHint: '-1001234567890', secret: 'Bot token', secretHint: 'From @BotFather', needsTo: true },
        { key: 'slack', name: 'Slack', secret: 'Webhook URL', secretHint: 'https://hooks.slack.com/services/…' },
        { key: 'webhook', name: 'Webhook', secret: 'URL', secretHint: 'https://example.com/hook' }];
      // Quiet hours for messages: both ends or neither, so a half-typed window is kept here and saved when complete.
      var qh = { start: s.notifyQuietStart || '', end: s.notifyQuietEnd || '' };
      function saveQuiet() {
        if (!!qh.start !== !!qh.end) return;
        set({ notifyQuietStart: qh.start, notifyQuietEnd: qh.end, notifyQuietTimezone: ST.timezone() });
      }
      body.appendChild(row('Quiet hours', 'Alert messages wait until the quiet hours end. Test messages are always sent.', h('div', { class: 'stack', style: 'gap:8px' },
        h('div', { class: 'row' },
          U.input(qh.start, function (v) { qh.start = v; }, { type: 'time', 'aria-label': 'Hold messages from', onChange: saveQuiet }), h('span', { class: 'muted' }, 'to'),
          U.input(qh.end, function (v) { qh.end = v; }, { type: 'time', 'aria-label': 'Hold messages until', onChange: saveQuiet })),
        h('label', { class: 'check' }, h('input', { type: 'checkbox', checked: s.notifyQuietCritical !== false, 'aria-label': 'Critical alerts are still delivered', onChange: function (e) { set({ notifyQuietCritical: e.target.checked }); } }), 'Critical alerts are still delivered'))));
      channels.forEach(function (c) {
        var cfg = s.notify[c.key] || { on: false, to: '', configured: false };
        var controls = [h('div', { style: 'text-align:right' }, U.toggle(cfg.on, function (v) {
          set({ notify: withChannel(c.key, { on: v }) });
        }, 'Enable ' + c.name))];
        if (c.needsTo) controls.push(U.input(cfg.to, function (v) { set({ notify: withChannel(c.key, { to: v }) }, true); }, { placeholder: c.toHint, 'aria-label': c.to + ' for ' + c.name }));
        controls.push(U.input('', function (v) { set({ notify: withChannel(c.key, { secret: v }) }, true); },
          { type: 'password', autocomplete: 'off', placeholder: cfg.configured ? 'Saved — type to replace' : c.secretHint, 'aria-label': c.secret + ' for ' + c.name }));
        controls.push(h('div', { class: 'row', style: 'justify-content:flex-end;gap:8px' },
          cfg.configured || cfg.to ? h('span', { class: 'hint' }, cfg.configured ? U.icon('check', 12) : null, cfg.configured ? ' Credentials saved' : '') : null,
          U.btn('Send a test', { size: 'sm', onClick: function () { sendTest(c); } })));
        body.appendChild(row(c.name, null, h('div', { class: 'stack', style: 'gap:8px' }, controls)));
      });
      body.appendChild(h('p', { class: 'hint', style: 'margin-top:12px' }, 'Secrets are stored encrypted and are never shown again. Leave a secret empty to keep the saved one.'));
    }
    function withChannel(key, patch) {
      var notify = JSON.parse(JSON.stringify(pending.notify || s.notify));
      Object.keys(notify).forEach(function (k) { delete notify[k].configured; });
      notify[key] = Object.assign({}, notify[key], patch);
      return notify;
    }
    function sendTest(c) {
      flush().then(function () { return ST.testNotification(c.key); }).then(function (delivery) {
        if (delivery.status === 'sent') U.toast('Test message sent to ' + c.name);
        else U.toast('The test to ' + c.name + ' did not go through' + (delivery.error ? ': ' + delivery.error : '.'), 'error');
      }, function (error) {
        U.toast(error.status === 422 ? c.name + ' is not set up yet. Turn it on and fill in its details first.' : error.message, 'error');
      });
    }
    function realData() {
      body.appendChild(row('Keep results for', 'Older results are removed automatically every night.', U.select([{ value: 30, label: '30 days' }, { value: 90, label: '90 days' }, { value: 365, label: '1 year' }, { value: 1095, label: '3 years' }, { value: 0, label: 'Forever' }], s.retentionDays, function (v) { set({ retentionDays: +v }); })));
      function download(path, name) { var a = h('a', { href: path, download: name }); document.body.appendChild(a); a.click(); a.remove(); }
      body.appendChild(row('Export all results', 'Everything in one file, for spreadsheets or backups.', h('div', { class: 'row', style: 'justify-content:flex-end' },
        U.btn('CSV', { icon: 'download', onClick: function () { download('/api/export/results.csv', 'speedtest-all.csv'); } }),
        U.btn('JSON', { icon: 'download', onClick: function () { download('/api/export/results.json', 'speedtest-all.json'); } }))));
    }
    function passwordDialog() {
      var cur = '', next = '', again = '', box = h('div'), layer;
      function problem(msg) { U.clear(box); if (msg) box.appendChild(U.callout(msg, 'crit', 'alert')); }
      layer = U.modal({ title: 'Change password',
        body: h('div', { class: 'stack' },
          U.field('Current password', U.input('', function (v) { cur = v; }, { type: 'password', autocomplete: 'current-password' })),
          U.field('New password', U.input('', function (v) { next = v; }, { type: 'password', autocomplete: 'new-password' }), 'Use a long, unique passphrase (at least 12 characters).'),
          U.field('New password again', U.input('', function (v) { again = v; }, { type: 'password', autocomplete: 'new-password' })), box),
        actions: function (l) {
          return [U.btn('Cancel', { onClick: l.close }), U.btn('Change password', { variant: 'primary', onClick: function (e) {
            if (!cur) return problem('Enter your current password.');
            if (next.length < 12) return problem('The new password needs at least 12 characters.');
            if (next !== again) return problem('The two new passwords are not the same.');
            var button = e.currentTarget; button.disabled = true; problem(null);
            ST.changePassword(cur, next).then(function () { l.close(); U.toast('Password changed'); }, function (error) {
              button.disabled = false;
              problem(error.status === 401 || error.status === 403 ? 'The current password is not right.' : error.status === 422 ? 'That password is not allowed. Choose a longer or less common one.' : error.message);
            });
          } })];
        } });
    }
    // Two-factor sign-in: an authenticator app makes a 6-digit code at sign-in; ten one-time recovery codes are the way back in.
    function twoFactorBox() {
      var box = h('div');
      function reload() {
        ST.twoFactorStatus().then(function (status) {
          U.clear(box);
          if (!status.available) { box.appendChild(h('div', { class: 'hint' }, 'Not available: the center needs an APP_KEY to protect the secret.')); return; }
          box.appendChild(h('div', { class: 'hint', style: 'margin-bottom:6px' }, status.enabled ? 'On. Recovery codes left: ' + status.recoveryRemaining + '.' : 'Off.'));
          box.appendChild(U.btn(status.enabled ? 'Turn off…' : 'Turn on…', { size: 'sm', onClick: status.enabled ? turnOffDialog : turnOnDialog }));
        }, function (error) { U.clear(box); box.appendChild(h('div', { class: 'hint' }, 'Could not read the status: ' + error.message)); });
      }
      function explainTwoFactor(error, subject) {
        if (error.status === 429) return 'Too many attempts. Wait a minute and try again.';
        if (error.status === 401 || error.status === 403) return subject === 'code' ? 'That code is not right, or it was already used.' : 'The current password is not right.';
        if (error.status === 409) return 'Two-factor sign-in is already in that state. Close this window and look again.';
        return error.message;
      }
      function turnOnDialog() {
        var pw = '', code = '', note = h('div'), layer;
        function problem(msg) { U.clear(note); if (msg) note.appendChild(U.callout(msg, 'crit', 'alert')); }
        function step(content, buttons) { U.clear(layer.body); layer.body.appendChild(content); layer.setFoot(buttons); }
        function recovery(codes) {
          var text = codes.join('\n');
          step(h('div', { class: 'stack' },
            U.callout('Two-factor sign-in is on. Save these recovery codes now: each works once if you lose your phone, and they are not shown again.', 'warn'),
            h('pre', { class: 'recovery-codes', 'aria-label': 'Recovery codes', style: 'font-family:var(--mono,monospace);user-select:all;margin:0' }, text)),
          [U.btn('Copy codes', { icon: 'copy', onClick: function () { if (navigator.clipboard) navigator.clipboard.writeText(text).then(function () { U.toast('Codes copied'); }); } }),
            U.btn('I saved them', { variant: 'primary', onClick: function () { layer.close(); reload(); } })]);
        }
        function confirmCode(setup) {
          step(h('div', { class: 'stack' },
            h('p', { class: 'muted', style: 'margin:0' }, 'Scan this code with an authenticator app (Google Authenticator, Authy, 1Password…), or enter the key by hand.'),
            h('img', { src: setup.qr, alt: 'QR code for the authenticator app', width: 180, height: 180, style: 'background:#fff;padding:6px;border-radius:6px;align-self:flex-start' }),
            h('div', { class: 'hint' }, 'Key: ', h('code', { 'data-testid': 'two-factor-key', style: 'user-select:all' }, setup.secret)),
            h('a', { href: setup.uri, class: 'hint' }, 'Open in an authenticator app on this device'),
            U.field('Code from the app', U.input('', function (v) { code = v; }, { type: 'text', inputmode: 'numeric', autocomplete: 'one-time-code', maxlength: 8 }), 'Six digits.'), note),
          [U.btn('Cancel', { onClick: layer.close }), U.btn('Turn on', { variant: 'primary', onClick: function (e) {
            if (!code.trim()) return problem('Enter the 6-digit code from the app.');
            var button = e.currentTarget; button.disabled = true; problem(null);
            ST.twoFactorEnable(code.trim()).then(recovery, function (error) { button.disabled = false; problem(explainTwoFactor(error, 'code')); });
          } })]);
        }
        layer = U.modal({ title: 'Turn on two-factor sign-in', body: h('div'), onClose: reload, actions: function () { return []; } });
        step(h('div', { class: 'stack' },
          h('p', { class: 'muted', style: 'margin:0' }, 'Confirm it is you first.'),
          U.field('Current password', U.input('', function (v) { pw = v; }, { type: 'password', autocomplete: 'current-password' })), note),
        [U.btn('Cancel', { onClick: layer.close }), U.btn('Continue', { variant: 'primary', onClick: function (e) {
          if (!pw) return problem('Enter your current password.');
          var button = e.currentTarget; button.disabled = true; problem(null);
          ST.twoFactorSetup(pw).then(confirmCode, function (error) { button.disabled = false; problem(explainTwoFactor(error, 'password')); });
        } })]);
      }
      function turnOffDialog() {
        var pw = '', code = '', note = h('div'), layer;
        function problem(msg) { U.clear(note); if (msg) note.appendChild(U.callout(msg, 'crit', 'alert')); }
        layer = U.modal({ title: 'Turn off two-factor sign-in',
          body: h('div', { class: 'stack' },
            U.field('Current password', U.input('', function (v) { pw = v; }, { type: 'password', autocomplete: 'current-password' })),
            U.field('Code from the app, or a recovery code', U.input('', function (v) { code = v; }, { type: 'text', autocomplete: 'one-time-code', maxlength: 32 })), note),
          actions: function (l) {
            return [U.btn('Cancel', { onClick: l.close }), U.btn('Turn off', { variant: 'danger solid', onClick: function (e) {
              if (!pw || !code.trim()) return problem('Enter your password and a code.');
              var button = e.currentTarget; button.disabled = true; problem(null);
              ST.twoFactorDisable(pw, code.trim()).then(function () { l.close(); U.toast('Two-factor sign-in is off'); reload(); }, function (error) {
                button.disabled = false; problem(explainTwoFactor(error, error.status === 401 && /code/i.test(error.message) ? 'code' : 'password'));
              });
            } })];
          } });
      }
      reload();
      return box;
    }
    function realAccess() {
      var keyBox = h('div');
      body.appendChild(row('Password', 'Change the password you sign in with. Other sessions are signed out.', U.btn('Change password…', { size: 'sm', onClick: passwordDialog })));
      body.appendChild(row('Two-factor sign-in', 'Ask for a code from an authenticator app after the password.', twoFactorBox()));
      body.appendChild(row('Dashboard address', 'Nodes report to this address.', U.input(s.centerUrl, function (v) { set({ centerUrl: v }, true); }, { 'aria-label': 'Dashboard address', placeholder: location.origin })));
      body.appendChild(h('div', { class: 'setting', style: 'align-items:flex-start' }, h('div', null, h('div', { style: 'font-weight:600' }, 'Enrolment key'),
        h('div', { class: 'hint' }, 'A new key is shown once, here. Treat it like a password. Machines that already joined are not affected.')),
        h('div', { class: 's-ctl' }, keyBox, h('div', { class: 'hint', style: 'margin-bottom:6px' }, s.enrollKeyConfigured ? 'A key is set.' : 'No key has been generated yet.'),
          h('div', { style: 'margin-top:8px' }, U.btn('Generate a new key', { size: 'sm', icon: 'refresh', onClick: function () {
            U.confirm({ title: 'Generate a new enrolment key?', text: 'The old key stops working for new machines. Nodes that already joined are not affected.', confirm: 'Generate' }).then(function (ok) {
              if (!ok) return;
              ST.rotateEnrollmentKey().then(function (issued) {
                U.clear(keyBox); keyBox.appendChild(U.code(issued.enrollToken)); keyBox.appendChild(h('p', { class: 'hint' }, 'Copy it now. It will not be shown again.'));
              }, fail);
            });
          } })))));
    }

    if (tab === 'general') {
      var tzs = [{ value: 'browser', label: 'This device (' + Intl.DateTimeFormat().resolvedOptions().timeZone + ')' }, { value: 'UTC', label: 'UTC' }, { value: 'Asia/Ho_Chi_Minh', label: 'Ho Chi Minh City' }, { value: 'Asia/Singapore', label: 'Singapore' }, { value: 'Europe/London', label: 'London' }, { value: 'America/New_York', label: 'New York' }];
      body.appendChild(row('Theme', 'Dark is the default. You can also use the sun/moon button at the top.', U.seg([{ value: 'dark', label: 'Dark' }, { value: 'light', label: 'Light' }], s.theme === 'light' ? 'light' : 'dark', function (v) { set({ theme: v }); })));
      body.appendChild(row('Speed units', 'How speeds are shown everywhere.', U.select([{ value: 'auto', label: 'Automatic (Mbps / Gbps)' }, { value: 'Mbps', label: 'Always Mbps' }, { value: 'Gbps', label: 'Always Gbps' }, { value: 'MBps', label: 'Megabytes per second (MB/s)' }], s.units, function (v) { set({ units: v }); saved(); })));
      body.appendChild(row('Decimal places', null, U.select([0, 1, 2].map(function (x) { return { value: x, label: String(x) }; }), s.decimals, function (v) { set({ decimals: +v }); saved(); })));
      body.appendChild(row('Time zone', 'Used for charts, tables and the week heatmap.', U.select(tzs, s.timezone, function (v) { set({ timezone: v }); saved(); })));
      body.appendChild(row('12-hour clock', null, U.toggle(s.hour12, function (v) { set({ hour12: v }); saved(); }, '12-hour clock')));
      body.appendChild(row('Default time range', 'What the dashboard shows when you open it.', U.rangeSelect(s.range, function (v) { set({ range: v }); saved(); })));
    }

    if (tab === 'engine') {
      body.appendChild(row('Speed test program', 'The tool each node runs to measure speed.', U.select([{ value: 'ookla', label: 'Ookla Speedtest' }, { value: 'librespeed', label: 'LibreSpeed (self-hosted servers)' }, { value: 'cloudflare', label: 'Cloudflare speed test' }], s.engine, function (v) { set({ engine: v }, true); saved(); })));
      body.appendChild(row('Server choice', 'Automatic picks the closest server for each test.', U.select([{ value: 'auto', label: 'Closest automatically' }, { value: 'pinned', label: 'Use the server set on each node' }], s.serverMode, function (v) { set({ serverMode: v }, true); saved(); })));
      body.appendChild(row('Parallel connections', 'More connections fill very fast links. Applies to new nodes.', U.select([1, 2, 4, 6, 8, 16].map(function (x) { return { value: x, label: String(x) }; }), s.defaultThreads, function (v) { set({ defaultThreads: +v }, true); saved(); })));
      body.appendChild(U.callout('Tests use real bandwidth. On a connection with a data cap, lengthen the interval on the Schedules page.', null, 'info'));
    }

    if (tab === 'geo') {
      body.appendChild(row('Location database', 'Looks up addresses on the server itself — free, private and fast.', U.select([{ value: 'geolite2', label: 'MaxMind GeoLite2 (recommended)' }, { value: 'ipinfo', label: 'IPinfo Lite' }, { value: 'ip-api', label: 'ip-api.com' }], s.geoProvider, function (v) { set({ geoProvider: v }, true); saved(); })));
      body.appendChild(row('Backup service', 'Used when the database has no answer.', U.select([{ value: 'none', label: 'None' }, { value: 'ip-api', label: 'ip-api.com (free)' }, { value: 'ipinfo', label: 'IPinfo (free tier)' }], s.geoFallback, function (v) { set({ geoFallback: v }, true); saved(); })));
      body.appendChild(row('MaxMind license key', 'Free to get from maxmind.com. Needed to download database updates.', U.input(s.geoKey, function (v) { set({ geoKey: v }, true); }, { type: 'password', placeholder: 'Paste key', autocomplete: 'off', 'aria-label': 'MaxMind license key' })));
      body.appendChild(row('Update the database weekly', null, U.toggle(s.geoAutoUpdate, function (v) { set({ geoAutoUpdate: v }, true); saved(); }, 'Weekly update')));
    }

    if (tab === 'notify' && real) realNotify();
    else if (tab === 'notify') {
      var ch = [['email', 'Email', 'Address', 'ops@example.com'], ['telegram', 'Telegram', 'Chat ID', '-1001234567890'], ['slack', 'Slack', 'Webhook URL', 'https://hooks.slack.com/…'], ['webhook', 'Webhook', 'URL', 'https://example.com/hook']];
      ch.forEach(function (c) {
        var cfg = s.notify[c[0]];
        body.appendChild(row(c[1], null, h('div', { class: 'stack', style: 'gap:8px' }, h('div', { style: 'text-align:right' }, U.toggle(cfg.on, function (v) { cfg.on = v; set({ notify: s.notify }, true); saved(); }, 'Enable ' + c[1])),
          U.input(cfg.to, function (v) { cfg.to = v; set({ notify: s.notify }, true); }, { placeholder: c[3], 'aria-label': c[2] + ' for ' + c[1] }),
          U.btn('Send a test', { size: 'sm', onClick: function () { if (!cfg.to.trim()) { U.toast('Enter the ' + c[2].toLowerCase() + ' first', 'error'); return; } U.toast('Test message sent to ' + c[1]); } }))));
      });
    }

    if (tab === 'data' && real) realData();
    else if (tab === 'data') {
      body.appendChild(row('Keep results for', 'Older results are removed automatically.', U.select([{ value: 30, label: '30 days' }, { value: 90, label: '90 days' }, { value: 365, label: '1 year' }, { value: 1095, label: '3 years' }, { value: 0, label: 'Forever' }], s.retentionDays, function (v) { set({ retentionDays: +v }, true); saved(); })));
      body.appendChild(row('Export all results', 'Everything in one file, for spreadsheets or backups.', h('div', { class: 'row', style: 'justify-content:flex-end' },
        U.btn('CSV', { icon: 'download', onClick: function () { U.download('speedtest-all.csv', ST.csv(ST.results({})), 'text/csv'); U.toast('Exported'); } }),
        U.btn('JSON', { icon: 'download', onClick: function () { U.download('speedtest-all.json', JSON.stringify(ST.results({}), null, 1), 'application/json'); U.toast('Exported'); } }))));
      body.appendChild(row('Reset the demo', 'Clears everything you changed in this preview.', U.btn('Reset demo data', { variant: 'danger', icon: 'refresh', onClick: function () {
        U.confirm({ title: 'Reset demo data?', text: 'Nodes, rules, settings and manual tests you created here are cleared and the sample data returns.', confirm: 'Reset', danger: true }).then(function (ok) { if (ok) ST.resetDemo(); });
      } })));
    }

    // The log of owner changes: who changed what and when, newest first, with a filter and "Show more".
    function realActivity() {
      var kinds = [{ value: '', label: 'Everything' }, { value: 'node.', label: 'Nodes' }, { value: 'schedule.', label: 'Schedules' }, { value: 'alert.', label: 'Alerts' },
        { value: 'settings.', label: 'Settings' }, { value: 'notification.', label: 'Notifications' }, { value: 'auth.', label: 'Password' }];
      var host = h('div'), more = h('div', { class: 'mt' }), rows = [], page = 0, total = 0, loading = false;
      function draw() {
        U.clear(host); U.clear(more);
        host.appendChild(U.table({ columns: [
          { key: 'ts', label: 'When', render: function (r) { return F.date(Date.parse(r.ts), 'full'); } },
          { key: 'actor', label: 'Who', render: function (r) { return r.actor; } },
          { key: 'summary', label: 'What', render: function (r) { return r.summary; } },
          { key: 'target', label: 'Item', render: function (r) { return h('span', { class: 'mono' }, r.targetId ? r.targetId.slice(0, 8) : '—'); } },
          { key: 'remote', label: 'From', render: function (r) { return h('span', { class: 'mono' }, r.remote || '—'); } }],
          rows: rows, empty: U.empty('No changes recorded yet', 'Changes you make from now on appear here.') }));
        if (rows.length < total) more.appendChild(U.btn('Show more', { size: 'sm', onClick: function () { load(); } }));
        else if (rows.length) more.appendChild(h('p', { class: 'hint' }, rows.length + ' changes shown.'));
      }
      function load() {
        if (loading) return; loading = true; page++;
        ST.api.request('/api/audit?page=' + page + '&size=50' + (activityFilter ? '&action=' + encodeURIComponent(activityFilter) : '')).then(function (res) {
          rows = rows.concat(res.data); total = res.meta.pagination.total; loading = false; draw();
        }, function (error) { loading = false; page--; U.toast(error.message, 'error'); });
      }
      body.appendChild(row('Show', 'Every change made from this dashboard, newest first. Passwords, keys and message contents are never recorded.',
        U.select(kinds, activityFilter, function (v) { activityFilter = v; ST.refresh(); })));
      body.appendChild(host); body.appendChild(more);
      draw(); load();
    }
    if (tab === 'activity' && real) realActivity();
    if (tab === 'access' && real) realAccess();
    else if (tab === 'access') {
      var tokenBox = h('div');
      function drawToken() { U.clear(tokenBox); tokenBox.appendChild(U.code(ST.settings().enrollToken)); }
      body.appendChild(row('Dashboard address', 'Nodes report to this address.', U.input(s.centerUrl, function (v) { set({ centerUrl: v }, true); }, { 'aria-label': 'Dashboard address' })));
      body.appendChild(h('div', { class: 'setting', style: 'align-items:flex-start' }, h('div', null, h('div', { style: 'font-weight:600' }, 'Enrolment key'), h('div', { class: 'hint' }, 'Lets a new machine join. Treat it like a password. Changing it does not disconnect existing nodes.')),
        h('div', { class: 's-ctl' }, tokenBox, h('div', { style: 'margin-top:8px' }, U.btn('Generate a new key', { size: 'sm', icon: 'refresh', onClick: function () {
          U.confirm({ title: 'Generate a new enrolment key?', text: 'The old key stops working for new machines. Nodes that already joined are not affected.', confirm: 'Generate' }).then(function (ok) { if (!ok) return; set({ enrollToken: 'st_enroll_' + Math.random().toString(36).slice(2, 10) + Math.random().toString(36).slice(2, 10) }, true); drawToken(); U.toast('New key created'); });
        } })))));
      drawToken();
      body.appendChild(U.callout('Sign-in and user accounts arrive with the real application. This preview has no login.', null, 'info'));
    }
  };
})();

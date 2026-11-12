/* Nodes: list, add-node wizard (guided install), node detail with settings and agent controls. */
(function () {
  'use strict';
  var ST = (/** @type {any} */ (window)).ST, U = ST.ui, h = U.h, F = ST.fmt, S = ST.stats;
  var detailTab = {}, nodeSettingDrafts = {}, engineScheduleDrafts = {};

  /* ---------- List ---------- */
  ST.pages.nodes = function (root) {
    var nodes = ST.nodes(), now = Date.now();
    root.appendChild(U.pageHead('Nodes', 'A node is a machine that measures one internet connection. Add as many as you need.',
      [U.btn('Add a node', { icon: 'plus', variant: 'primary', onClick: function () { openWizard(); } })]));
    if (!nodes.length) {
      root.appendChild(U.card(null, null, U.empty('No nodes yet', 'Add a node and the app installs a small program on that machine for you.', U.btn('Add your first node', { icon: 'plus', variant: 'primary', onClick: openWizard }))));
      return;
    }
    var rows = nodes.map(function (n) { var last = ST.latest(n.id); return { n: n, last: last, sum: S.nodeSummary(n.id, now - ST.ranges[ST.settings().range], now) }; });
    root.appendChild(h('div', { class: 'card' }, U.table({
      columns: [
        { key: 'name', label: 'Node', render: function (r) { return h('div', null, h('div', { class: 'row', style: 'font-weight:650' }, U.dot(ST.nodeColor(r.n)), r.n.name), h('div', { class: 'muted' }, r.n.label)); } },
        { key: 'status', label: 'Status', render: function (r) { return !r.n.enabled ? U.chip('Paused', '', 'pause') : r.last && r.last.status === 'failed' ? U.chip('Last test failed', 'crit', 'x') : U.chip('Online', 'good', 'check'); } },
        { key: 'exp', label: 'Expected', num: true, render: function (r) { return F.speed(r.n.expectedDown) + ' / ' + F.speed(r.n.expectedUp); } },
        { key: 'avg', label: 'Average ' + ST.settings().range, num: true, render: function (r) { return r.sum.down.mean == null ? '—' : F.speed(r.sum.down.mean) + ' (' + F.num(r.sum.downPct, 0) + '%)'; } },
        { key: 'ping', label: 'Latency', num: true, render: function (r) { return F.ms(r.sum.ping.median); } },
        { key: 'ip', label: 'Public IP', render: function (r) { return h('span', { class: 'mono' }, r.last ? r.last.ip : '—'); } },
        { key: 'seen', label: 'Last test', render: function (r) { return r.last ? F.rel(r.last.ts) : 'Never'; } },
        { key: 'agent', label: 'Agent', render: function (r) { return h('span', { class: 'mono' }, r.n.agent ? 'v' + r.n.agent : '—'); } },
        { key: 'act', label: '', render: function (r) { return h('span', { class: 'row', style: 'justify-content:flex-end' }, U.btn('', { icon: 'play', size: 'sm', title: 'Run a test on ' + r.n.name, onClick: function (e) { e.stopPropagation(); U.runTestDialog([r.n.id]); } })); } }],
      rows: rows, cards: true, onRow: function (r) { location.hash = '#/nodes/' + r.n.id; }
    })));
    root.appendChild(h('div', { class: 'mt' }, U.callout('Each node works on its own. It only needs to know how fast its connection should be; it does not need to know what kind of connection it is attached to.', null, 'info')));
  };

  /* ---------- Add-node wizard ---------- */
  function validIp(v) { return /^(\d{1,3}\.){3}\d{1,3}$/.test(v) && v.split('.').every(function (p) { return +p <= 255; }); }
  function validHost(v) { return validIp(v) || /^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$/i.test(v); }

  function openWizard() {
    var d = { name: '', label: '', down: '', up: '', method: 'ssh', host: '', port: '22', user: '', auth: 'password', secret: '', sudoSecret: '', iface: '' };
    var step = 0, body = h('div'), steps = h('div', { class: 'steps', 'aria-hidden': 'true' }, [0, 1, 2, 3].map(function () { return h('span'); }));
    var errBox = h('div');
    var timers = [], layer;
    var token = ST.settings().enrollToken, real = !ST.demo, enrolled = null;

    function setSteps() { Array.prototype.forEach.call(steps.children, function (s, i) { s.className = i <= step ? 'done' : ''; }); }
    function error(msg) { U.clear(errBox); if (msg) errBox.appendChild(U.callout(msg, 'crit', 'alert')); }

    function s0() {
      var name = U.input(d.name, function (v) { d.name = v; }, { placeholder: 'e.g. VM6', maxlength: 40, 'aria-label': 'Node name' });
      var label = U.input(d.label, function (v) { d.label = v; }, { placeholder: 'e.g. Home fiber 500', maxlength: 60, 'aria-label': 'What it measures' });
      var down = U.input(d.down, function (v) { d.down = v; }, { type: 'number', min: 1, placeholder: '1000', inputmode: 'decimal', 'aria-label': 'Expected download' });
      var up = U.input(d.up, function (v) { d.up = v; }, { type: 'number', min: 1, placeholder: '1000', inputmode: 'decimal', 'aria-label': 'Expected upload' });
      return h('div', { class: 'stack' },
        U.field('Node name', name, 'A short name you will recognise.'),
        U.field('What does it measure?', label, 'Optional. For example the provider and plan, or “All connections”.'),
        h('div', { class: 'field-row' }, U.field('Expected download', U.suffix(down, 'Mbps')), U.field('Expected upload', U.suffix(up, 'Mbps'))),
        h('span', { class: 'hint' }, 'The expected speed is the figure this node is compared against. Use what you actually get, not only what the plan promises.'));
    }
    var probe = null, installation = null;
    function confirmView() {
      return h('div', { class: 'stack' },
        h('p', null, 'The machine ', h('b', null, d.host.trim() + ':' + d.port), ' introduced itself with this fingerprint. Continue only if it is the machine you meant.'),
        U.code(probe.fingerprint),
        h('span', { class: 'hint' }, 'Nothing has been sent to it yet except this introduction. The installer stops if the machine ever shows a different fingerprint. If you can, compare it with the output of ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub on the machine.'));
    }
    function s1() {
      if (real && d.method === 'ssh' && probe) return confirmView();
      var wrap = h('div', { class: 'stack' });
      var sshForm = h('div', { class: 'stack' });
      function drawForm() {
        U.clear(sshForm);
        if (d.method === 'ssh') {
          sshForm.appendChild(h('div', { class: 'field-row' },
            U.field('Machine address', U.input(d.host, function (v) { d.host = v; }, { placeholder: '192.168.10.36', 'aria-label': 'Machine address' }), 'IP address or host name of the machine.'),
            U.field('Port', U.input(d.port, function (v) { d.port = v; }, { type: 'number', 'aria-label': 'Port' }))));
          sshForm.appendChild(U.field('User name', U.input(d.user, function (v) { d.user = v; }, { placeholder: 'ubuntu', autocomplete: 'off', 'aria-label': 'User name' }), 'Needs permission to install a service (sudo).'));
          sshForm.appendChild(U.field('Sign in with', U.seg([{ value: 'password', label: 'Password' }, { value: 'key', label: 'Private key' }], d.auth, function (v) { d.auth = v; d.secret = ''; drawForm(); })));
          sshForm.appendChild(d.auth === 'password' ? U.field('Password', U.input(d.secret, function (v) { d.secret = v; }, { type: 'password', autocomplete: 'new-password', 'aria-label': 'Password' }), 'Used once for the installation and never stored.')
            : U.field('Private key', h('textarea', { class: 'input', rows: 4, placeholder: '-----BEGIN OPENSSH PRIVATE KEY-----', 'aria-label': 'Private key', onInput: function (e) { d.secret = e.target.value; } }), 'Used once for the installation and never stored.'));
        } else {
          var cmd = 'curl -fsSL ' + (ST.settings().centerUrl || location.origin) + '/install.sh | sudo sh -s -- \\\n  --token ' + token + ' \\\n  --name "' + (d.name || 'node') + '"';
          sshForm.appendChild(h('p', null, 'Open a terminal on the machine and paste this single command. The node appears here by itself when it finishes.'));
          sshForm.appendChild(U.code(cmd));
          sshForm.appendChild(h('span', { class: 'hint' }, 'The command contains a one-time enrolment key. Do not share it.'));
        }
        if (d.method === 'ssh' && real) sshForm.appendChild(U.field('Sudo password (only if it differs)', U.input(d.sudoSecret, function (v) { d.sudoSecret = v; }, { type: 'password', autocomplete: 'new-password', 'aria-label': 'Sudo password' }), 'Leave empty to use the sign-in password, or when sudo needs none. Never stored.'));
        sshForm.appendChild(h('details', null, h('summary', { style: 'cursor:pointer;font-weight:600' }, 'Advanced'),
          h('div', { style: 'margin-top:10px' }, U.field('Network interface', U.input(d.iface, function (v) { d.iface = v; }, { placeholder: 'Automatic', 'aria-label': 'Network interface' }), 'Leave empty unless the machine has more than one network connection and the test must use a specific one.'))));
      }
      wrap.appendChild(h('div', { class: 'grid cols-2' },
        h('button', { type: 'button', class: 'choice ' + (d.method === 'ssh' ? 'on' : ''), onClick: function (e) { d.method = 'ssh'; var btns = wrap.querySelectorAll('.choice'); btns[0].classList.add('on'); btns[1].classList.remove('on'); drawForm(); if (layer) layer.setFoot(footer()); } }, h('b', null, U.icon('terminal', 16), 'Install for me'), h('span', { class: 'muted' }, 'We connect to the machine and set everything up.')),
        h('button', { type: 'button', class: 'choice ' + (d.method === 'manual' ? 'on' : ''), onClick: function () { d.method = 'manual'; var btns = wrap.querySelectorAll('.choice'); btns[1].classList.add('on'); btns[0].classList.remove('on'); drawForm(); if (layer) layer.setFoot(footer()); } }, h('b', null, U.icon('copy', 16), 'I will run a command'), h('span', { class: 'muted' }, 'Copy one line and paste it on the machine.'))));
      wrap.appendChild(sshForm); drawForm();
      return wrap;
    }
    var checklist, resultBox;
    var tasks = ['Connecting to the machine', 'Checking the system', 'Downloading the speed test program', 'Installing it as a background service', 'Registering with this dashboard', 'Running the first speed test'];
    function s2() {
      checklist = h('ul', { class: 'checklist' });
      (real ? ['Waiting for the machine to check in'] : tasks).forEach(function (t, i) { if (d.method === 'manual' && i === 0) t = 'Waiting for the machine to check in'; checklist.appendChild(h('li', { id: 'ck' + i }, h('span', { style: 'width:16px;display:inline-grid' }, U.icon('clock', 14)), t)); });
      resultBox = h('div', { style: 'margin-top:14px' });
      return h('div', null, h('p', { class: 'muted', style: 'margin-bottom:12px' }, 'This usually takes under a minute. You can leave this window open.'), checklist, resultBox);
    }
    function waitForCheckIn() {
      var started = Date.now(), li = checklist.children[0];
      li.className = 'run'; U.clear(li.firstChild); li.firstChild.appendChild(h('span', { class: 'spinner' }));
      (function poll() {
        ST.api.request('/api/nodes/' + encodeURIComponent(created.id)).then(function (response) {
          if (response.data.lastSeen) {
            li.className = 'ok'; U.clear(li.firstChild); li.firstChild.appendChild(U.icon('check', 14));
            ST.load().then(function () { step = 3; render(); });
            return;
          }
          if (Date.now() - started > 10 * 60 * 1000) {
            U.clear(resultBox); resultBox.appendChild(U.callout('The machine has not checked in yet. Make sure the command finished without errors, then close this window: the node appears in the list by itself when it connects.', 'warn', 'alert'));
            return;
          }
          timers.push(setTimeout(poll, 3000));
        }, function () { timers.push(setTimeout(poll, 5000)); });
      })();
    }
    function watchInstall() {
      var bar = h('div', { class: 'progress', style: 'margin:10px 0' }, h('i', { style: 'width:0%' }));
      var log = h('ul', { class: 'checklist' });
      U.clear(checklist.parentNode); checklist = log;
      resultBox = h('div', { style: 'margin-top:14px' });
      var host = body.querySelector('.install-host') || body;
      var wrap = h('div', { class: 'install-host' }, h('p', { class: 'muted', style: 'margin-bottom:12px' }, 'Installing on ' + d.host.trim() + '. This usually takes under a minute. You can leave this window open.'), bar, log, resultBox);
      U.clear(body); body.appendChild(steps); body.appendChild(h('h3', { style: 'margin-bottom:10px' }, 'Setting things up')); body.appendChild(wrap); body.appendChild(errBox);
      var shown = {}, finished = false, source;
      function line(event) {
        bar.firstChild.style.width = Math.max(0, Math.min(100, event.pct || 0)) + '%';
        if (shown[event.id] || event.status === 'failed') return; shown[event.id] = true; // a failure is explained once, below the list
        var last = log.lastChild; if (last && last.className === 'run') { last.className = 'ok'; U.clear(last.firstChild); last.firstChild.appendChild(U.icon('check', 14)); }
        var li = h('li', { class: event.status === 'failed' ? 'bad' : event.status === 'succeeded' ? 'ok' : 'run' }, h('span', { style: 'width:16px;display:inline-grid' }, event.status === 'failed' ? U.icon('x', 14) : event.status === 'succeeded' ? U.icon('check', 14) : h('span', { class: 'spinner' })), event.message);
        log.appendChild(li);
      }
      function done(event) {
        if (finished) return; finished = true;
        if (event.status === 'succeeded') { ST.load().then(function () { step = 3; render(); }); return; }
        U.clear(resultBox);
        resultBox.appendChild(U.callout(event.message || 'The installation did not finish.', 'crit', 'alert'));
        resultBox.appendChild(h('p', { class: 'hint', style: 'margin-top:8px' }, 'Nothing was left half-installed. Go back, fix the problem and try again.'));
        layer.setFoot([U.btn('Back', { onClick: function () { probe = null; installation = null; step = 1; render(); } })]);
      }
      source = ST.followInstallation(installation.id, function (event) { line(event); if (event.status === 'succeeded' || event.status === 'failed') done(event); });
      timers.push(function () { if (source) source.close(); });
    }
    function runInstall() {
      if (real && d.method === 'ssh') { watchInstall(); return; }
      if (real) { waitForCheckIn(); return; }
      var i = 0;
      (function next() {
        if (i > 0) { var prev = checklist.children[i - 1]; prev.className = 'ok'; U.clear(prev.firstChild); prev.firstChild.appendChild(U.icon('check', 14)); }
        if (i >= tasks.length) { finish(); return; }
        var li = checklist.children[i]; li.className = 'run'; U.clear(li.firstChild); li.firstChild.appendChild(h('span', { class: 'spinner' }));
        i++; timers.push(setTimeout(next, i === 6 ? 2600 : 650 + Math.random() * 700));
      })();
    }
    var created = null;
    function finish() {
      created = ST.addNode({ name: d.name.trim(), label: d.label.trim() || 'Unnamed connection', provider: '', plan: '', expectedDown: +d.down, expectedUp: +d.up, host: d.host || '192.168.10.' + (40 + ST.nodes().length) });
      // Give the new node a first result immediately.
      var byId = {}; ST.nodes().forEach(function (n) { byId[n.id] = n; });
      ST.runTest(created.id).then(function (res) {
        step = 3; render();
      });
      // runTest emits data events that refresh the page behind the dialog; keep the dialog as-is until the result is in.
    }
    function firstResult(res) {
      return res && res.status !== 'failed' ? 'First result: ' + F.speed(res.down) + ' down, ' + F.speed(res.up) + ' up, ' + F.ms(res.ping) + ' ping.' : null;
    }
    var firstRun = null;
    function s3() {
      var res = ST.latest(created.id);
      var line = h('p', { class: 'text-2', 'aria-live': 'polite' });
      if (real && !res) {
        // The node is up; run its first test right away so the owner sees a real result before closing the window.
        line.textContent = 'Running the first speed test…';
        if (!firstRun) firstRun = ST.runTest(created.id);
        firstRun.then(function (result) {
          line.textContent = firstResult(result) || 'The first test did not finish' + (result.error ? ': ' + result.error : '') + '. You can run it again from the node page.';
        }, function () { line.textContent = 'The node is connected. Its first scheduled test will run shortly.'; });
      } else line.textContent = firstResult(res) || 'The node is connected. Its first scheduled test will run shortly.';
      return h('div', { class: 'stack', style: 'text-align:center;padding:6px 0' },
        h('div', { class: 'ev-ico good', style: 'width:48px;height:48px;margin:0 auto;border-radius:14px' }, U.icon('check', 26)),
        h('h2', null, created.name + ' is online'),
        line,
        h('p', { class: 'hint' }, 'It will now test every ' + (ST.node(created.id) || created).interval + ' minutes. You can change that under Schedules.'));
    }

    function validate() {
      if (step === 0) {
        if (!d.name.trim()) return 'Give the node a name.';
        if (ST.nodes().some(function (n) { return n.name.toLowerCase() === d.name.trim().toLowerCase(); })) return 'A node with this name already exists. Choose another name.';
        if (!(+d.down > 0) || !(+d.up > 0)) return 'Enter the expected download and upload speed in Mbps.';
      }
      if (step === 1 && d.method === 'ssh') {
        if (!validHost(d.host.trim())) return 'Enter a valid IP address or host name for the machine.';
        if (!(+d.port >= 1 && +d.port <= 65535)) return 'The port must be between 1 and 65535.';
        if (!d.user.trim()) return 'Enter the user name to sign in with.';
        if (!d.secret.trim()) return d.auth === 'password' ? 'Enter the password.' : 'Paste the private key.';
      }
      return null;
    }
    // In real mode the node is created on the center as soon as it is described: that is what issues its one-time enrolment key.
    function createNode(button) {
      button.disabled = true; error(null);
      ST.addNode({ name: d.name.trim(), label: d.label.trim(), expectedDown: +d.down, expectedUp: +d.up }).then(function (data) {
        enrolled = data; created = data.node; token = data.token;
        step++; render();
      }, function (failure) {
        button.disabled = false;
        error(failure.status === 409 ? 'A node with this name already exists. Choose another name.' : failure.message);
      });
    }
    var back, next;
    function render() {
      U.clear(body); setSteps(); error(null);
      body.appendChild(steps);
      var titles = ['Describe the node', 'Connect to the machine', 'Setting things up', 'Done'];
      body.appendChild(h('h3', { style: 'margin-bottom:10px' }, titles[step]));
      body.appendChild([s0, s1, s2, s3][step]());
      body.appendChild(errBox);
      if (layer) {
        layer.setFoot(footer());
      }
      if (step === 2) runInstall();
    }
    function checkMachine(button) {
      button.disabled = true; error(null);
      ST.probeMachine(d.host.trim(), +d.port).then(function (reply) { probe = reply; render(); }, function (failure) {
        button.disabled = false;
        error(failure.status === 0 ? failure.message : 'The dashboard could not reach ' + d.host.trim() + ':' + d.port + ' over SSH. Check the address and that SSH is running there.');
      });
    }
    function startInstall(button) {
      button.disabled = true; error(null);
      var ssh = { host: d.host.trim(), port: +d.port, user: d.user.trim(), auth: d.auth, secret: d.secret };
      if (d.sudoSecret) ssh.sudoPassword = d.sudoSecret;
      ST.startInstallation(created.id, 'installations', probe, ssh).then(function (reply) {
        d.secret = ''; d.sudoSecret = ''; installation = reply; step = 2; render();
      }, function (failure) {
        button.disabled = false;
        error(failure.status === 409 ? 'An installation is already running on this node, or the confirmation expired. Go back and check the machine again.' : failure.message);
      });
    }
    function footer() {
      if (step === 2) return [U.btn('Cancel', { onClick: function () { layer.close(); } })];
      if (real && step === 1 && d.method === 'ssh') {
        return [U.btn('Back', { onClick: function () { if (probe) { probe = null; render(); } else { step--; render(); } } }),
          probe ? U.btn('Trust it and install', { variant: 'primary', onClick: function (event) { startInstall(event.currentTarget); } })
            : U.btn('Check the machine', { variant: 'primary', onClick: function (event) { var e = validate(); if (e) { error(e); return; } checkMachine(event.currentTarget); } })];
      }
      if (step === 3) return [U.btn('Add another', { onClick: function () { layer.close(); setTimeout(openWizard, 50); } }), U.btn('Open node', { variant: 'primary', onClick: function () { layer.close(); location.hash = '#/nodes/' + created.id; } })];
      return [step > 0 ? U.btn('Back', { onClick: function () { step--; render(); } }) : U.btn('Cancel', { onClick: function () { layer.close(); } }),
        U.btn(step === 1 ? (d.method === 'ssh' ? 'Install' : 'I ran the command') : 'Continue', { variant: 'primary', onClick: function (event) {
          var e = validate(); if (e) { error(e); return; }
          if (real && step === 0 && !enrolled) { createNode(event.currentTarget); return; }
          step++; render();
        } })];
    }
    layer = U.modal({ title: 'Add a node', body: body, sticky: true, onClose: function () { timers.forEach(function (t) { if (typeof t === 'function') t(); else clearTimeout(t); }); }, actions: function () { return footer(); } });
    render();
  }
  ST.openAddNode = openWizard;

  /* ---------- Remove a node (real mode): optionally uninstall the program from the machine first ---------- */
  var UNINSTALL_COMMAND = 'sudo speedtest-agent uninstall --all';
  function removeNodeDialog(n) {
    var d = { method: 'machine', host: n.host || '', port: '22', user: '', auth: 'password', secret: '', sudo: '' };
    var probe = null, jobId = null, follow = null;
    var body = h('div', { class: 'stack' }), errBox = h('div'), layer;
    function error(msg) { U.clear(errBox); if (msg) errBox.appendChild(U.callout(msg, 'crit', 'alert')); }
    function deleteNode(done) {
      Promise.resolve(ST.removeNode(n.id)).then(function () { layer.close(); location.hash = '#/nodes'; U.toast(n.name + ' removed'); if (done) done(); }, function (failure) {
        error(failure.status === 412 ? 'This node was changed elsewhere. Reload the page and try again.' : failure.message);
      });
    }
    function field(label, control, hint) { return U.field(label, control, hint); }
    function draw() {
      U.clear(body); error(null);
      body.appendChild(h('p', null, 'This deletes ' + n.name + ' and all of its ' + ST.nodeResults(n.id).length + ' results from the dashboard. It cannot be undone.'));
      body.appendChild(h('div', { class: 'grid cols-2' },
        h('button', { type: 'button', class: 'choice ' + (d.method === 'machine' ? 'on' : ''), onClick: function () { d.method = 'machine'; probe = null; draw(); layer.setFoot(footer()); } }, h('b', null, U.icon('terminal', 16), 'Also remove the program'), h('span', { class: 'muted' }, 'We connect to the machine and remove it for you.')),
        h('button', { type: 'button', class: 'choice ' + (d.method === 'dashboard' ? 'on' : ''), onClick: function () { d.method = 'dashboard'; probe = null; draw(); layer.setFoot(footer()); } }, h('b', null, U.icon('copy', 16), 'Only remove from the dashboard'), h('span', { class: 'muted' }, 'The program stays on the machine until you remove it.'))));
      if (d.method === 'dashboard') {
        body.appendChild(h('p', null, 'To remove the program from the machine yourself, run this one line there:'));
        body.appendChild(U.code(UNINSTALL_COMMAND));
        body.appendChild(h('span', { class: 'hint' }, 'It stops the service and deletes the program, its settings and its saved results from that machine. Nothing else is touched.'));
      } else if (probe) {
        body.appendChild(h('p', null, 'The machine ', h('b', null, d.host.trim() + ':' + d.port), ' introduced itself with this fingerprint. Continue only if it is the machine you meant.'));
        body.appendChild(U.code(probe.fingerprint));
        body.appendChild(h('span', { class: 'hint' }, 'The removal stops if the machine ever shows a different fingerprint.'));
      } else {
        body.appendChild(h('div', { class: 'field-row' },
          field('Machine address', U.input(d.host, function (v) { d.host = v; }, { placeholder: '192.168.10.36', 'aria-label': 'Machine address' })),
          field('Port', U.input(d.port, function (v) { d.port = v; }, { type: 'number', 'aria-label': 'Port' }))));
        body.appendChild(field('User name', U.input(d.user, function (v) { d.user = v; }, { placeholder: 'ubuntu', autocomplete: 'off', 'aria-label': 'User name' }), 'Needs permission to use sudo.'));
        body.appendChild(field('Sign in with', U.seg([{ value: 'password', label: 'Password' }, { value: 'key', label: 'Private key' }], d.auth, function (v) { d.auth = v; d.secret = ''; draw(); })));
        body.appendChild(d.auth === 'password' ? field('Password', U.input(d.secret, function (v) { d.secret = v; }, { type: 'password', autocomplete: 'new-password', 'aria-label': 'Password' }), 'Used once and never stored.')
          : field('Private key', h('textarea', { class: 'input', rows: 4, placeholder: '-----BEGIN OPENSSH PRIVATE KEY-----', 'aria-label': 'Private key', onInput: function (e) { d.secret = e.target.value; } }), 'Used once and never stored.'));
        body.appendChild(field('Sudo password (only if it differs)', U.input(d.sudo, function (v) { d.sudo = v; }, { type: 'password', autocomplete: 'new-password', 'aria-label': 'Sudo password' })));
      }
      body.appendChild(errBox);
    }
    function validate() {
      if (!validHost(d.host.trim())) return 'Enter a valid IP address or host name for the machine.';
      if (!(+d.port >= 1 && +d.port <= 65535)) return 'The port must be between 1 and 65535.';
      if (!d.user.trim()) return 'Enter the user name to sign in with.';
      if (!d.secret.trim()) return d.auth === 'password' ? 'Enter the password.' : 'Paste the private key.';
      return null;
    }
    function progress() {
      U.clear(body); error(null);
      var bar = h('div', { class: 'progress', style: 'margin:10px 0' }, h('i', { style: 'width:0%' })), log = h('ul', { class: 'checklist' }), result = h('div');
      body.appendChild(h('p', { class: 'muted' }, 'Removing the program from ' + d.host.trim() + '. This usually takes a few seconds.')); body.appendChild(bar); body.appendChild(log); body.appendChild(result); body.appendChild(errBox);
      layer.setFoot([U.btn('Cancel', { onClick: function () { if (follow) follow.close(); layer.close(); } })]);
      var shown = {};
      follow = ST.followInstallation(jobId, function (event) {
        bar.firstChild.style.width = Math.max(0, Math.min(100, event.pct || 0)) + '%';
        if (event.status === 'failed') {
          result.appendChild(U.callout(event.message || 'The program could not be removed.', 'crit', 'alert'));
          result.appendChild(h('p', { class: 'hint', style: 'margin-top:8px' }, 'You can try again, or remove the node from the dashboard and run ' + UNINSTALL_COMMAND + ' on the machine yourself.'));
          layer.setFoot([U.btn('Try again', { onClick: function () { probe = null; jobId = null; draw(); layer.setFoot(footer()); } }),
            U.btn('Remove from the dashboard anyway', { variant: 'danger', onClick: function () { deleteNode(); } })]);
          return;
        }
        if (!shown[event.id]) { shown[event.id] = true; log.appendChild(h('li', { class: event.status === 'succeeded' ? 'ok' : 'run' }, h('span', { style: 'width:16px;display:inline-grid' }, U.icon('check', 14)), event.message)); }
        if (event.status === 'succeeded') deleteNode();
      });
    }
    function footer() {
      if (d.method === 'dashboard') {
        return [U.btn('Cancel', { onClick: function () { layer.close(); } }), U.btn('Remove ' + n.name, { variant: 'danger', icon: 'trash', onClick: function (e) { e.currentTarget.disabled = true; deleteNode(); } })];
      }
      if (probe) {
        return [U.btn('Back', { onClick: function () { probe = null; draw(); layer.setFoot(footer()); } }),
          U.btn('Trust it and remove', { variant: 'danger', onClick: function (e) {
            var button = e.currentTarget; button.disabled = true; error(null);
            var ssh = { host: d.host.trim(), port: +d.port, user: d.user.trim(), auth: d.auth, secret: d.secret };
            if (d.sudo) ssh.sudoPassword = d.sudo;
            ST.startInstallation(n.id, 'uninstallations', probe, ssh).then(function (job) { d.secret = ''; d.sudo = ''; jobId = job.id; progress(); }, function (failure) {
              button.disabled = false; error(failure.status === 409 ? 'A job is already running on this node, or the confirmation expired. Go back and check the machine again.' : failure.message);
            });
          } })];
      }
      return [U.btn('Cancel', { onClick: function () { layer.close(); } }), U.btn('Check the machine', { variant: 'primary', onClick: function (e) {
        var problem = validate(); if (problem) { error(problem); return; }
        var button = e.currentTarget; button.disabled = true; error(null);
        ST.probeMachine(d.host.trim(), +d.port).then(function (found) { probe = found; draw(); layer.setFoot(footer()); }, function (failure) {
          button.disabled = false; error(failure.status === 0 ? failure.message : 'The dashboard could not reach ' + d.host.trim() + ':' + d.port + ' over SSH. Check the address and that SSH is running there.');
        });
      } })];
    }
    layer = U.modal({ title: 'Remove ' + n.name, body: body, sticky: true, onClose: function () { if (follow) follow.close(); }, actions: function () { return footer(); } });
    draw();
  }

  /* ---------- Detail ---------- */
  ST.pages.nodeDetail = function (root, params) {
    var n = ST.node(params.id);
    if (!n) { root.appendChild(U.empty('Node not found', 'This node does not exist or was removed.', h('a', { class: 'btn', href: '#/nodes' }, 'Back to nodes'))); return; }
    var tab = detailTab[n.id] || 'overview', now = Date.now(), from = now - ST.ranges[ST.settings().range];
    var last = ST.latest(n.id);
    root.appendChild(U.pageHead(n.name + ' · ' + n.label, 'Expected ' + F.speed(n.expectedDown) + ' down / ' + F.speed(n.expectedUp) + ' up',
      [U.btn(n.enabled ? 'Pause' : 'Resume', { icon: n.enabled ? 'pause' : 'play', onClick: function () { ST.updateNode(n.id, { enabled: !n.enabled }); U.toast(n.name + (n.enabled ? ' paused' : ' resumed')); } }),
        U.btn('Run test', { icon: 'play', variant: 'primary', disabled: !n.enabled, title: n.enabled ? null : 'Resume the node first', onClick: function () { U.runTestDialog([n.id]); } })],
      h('div', { class: 'crumb' }, h('a', { href: '#/nodes' }, 'Nodes'), ' / ', n.name)));
    root.appendChild(U.tabs([{ value: 'overview', label: 'Overview' }, { value: 'settings', label: 'Settings' }, { value: 'agent', label: 'Agent' }], tab, function (v) { detailTab[n.id] = v; ST.refresh(); }));

    if (tab === 'overview') {
      var sum = S.nodeSummary(n.id, from, now), prev = S.nodeSummary(n.id, from - (now - from), from);
      var tiles = h('div', { class: 'grid cols-5 mb' });
      [['Average download', F.speed(sum.down.mean), U.deltaTag(sum.down.mean, prev.down.mean, true), F.num(sum.downPct, 0) + '% of expected'],
        ['Average upload', F.speed(sum.up.mean), U.deltaTag(sum.up.mean, prev.up.mean, true), F.num(sum.upPct, 0) + '% of expected'],
        ['Median latency', F.ms(sum.ping.median), U.deltaTag(sum.ping.median, prev.ping.median, false), 'Jitter ' + F.ms(sum.jitter.mean)],
        ['Success rate', F.pct(sum.successRate, 1), U.deltaTag(sum.successRate, prev.successRate, true), sum.failed + ' failed; ' + sum.rateLimited + ' rate-limited excluded of ' + sum.count],
        ['Stability', sum.stability == null ? '—' : F.num(sum.stability, 0) + '/100', null, 'Steadiness of download speed']].forEach(function (t) {
        tiles.appendChild(h('div', { class: 'card stat' }, h('div', { class: 'stat-label' }, t[0]), h('div', { class: 'stat-value tabnum', style: 'font-size:22px' }, t[1]), h('div', { class: 'row', style: 'gap:6px' }, t[2], h('span', { class: 'hint' }, t[3]))));
      });
      root.appendChild(tiles);
      var chartHost = h('div');
      var rs = ST.results({ nodeIds: [n.id], from: from });
      root.appendChild(U.card('Speed over time', ST.rangeLabels[ST.settings().range], chartHost));
      ST.charts.line(chartHost, {
        series: [{ id: 'd', name: 'Download', color: 'var(--s1)', points: rs.map(function (r) { return [r.ts, r.status === 'failed' ? null : r.down]; }) },
          { id: 'u', name: 'Upload', color: 'var(--s2)', points: rs.map(function (r) { return [r.ts, r.status === 'failed' ? null : r.up]; }) }],
        x: { type: 'time', min: from, max: now }, y: { min: 0, format: F.axisSpeed, unit: F.axisUnit() }, height: 280, label: 'Download and upload for ' + n.name,
        refLines: [{ v: n.expectedDown, label: 'Expected download', color: 'var(--text-2)' }], tipValue: function (s, v) { return F.speed(v); }
      });
      var recent = rs.slice(-12).reverse();
      root.appendChild(h('div', { class: 'mt' }, U.card('Latest results', null, h('div', { style: 'margin:-8px -18px -18px' }, U.table({
        columns: [
          { key: 'ts', label: 'Time', render: function (r) { return F.date(r.ts); } },
          { key: 'down', label: 'Download', num: true, render: function (r) { return F.speed(r.down); } },
          { key: 'up', label: 'Upload', num: true, render: function (r) { return F.speed(r.up); } },
          { key: 'ping', label: 'Ping', num: true, render: function (r) { return F.ms(r.ping); } },
          { key: 'ip', label: 'Public IP', render: function (r) { return h('span', { class: 'mono' }, r.ip); } },
          { key: 'st', label: 'Status', render: function (r) { return U.statusChip(r.status); } }],
        rows: recent, onRow: function (r) { ST.openResult(r); } })),
      [h('a', { class: 'btn sm', href: '#/history' }, 'All results')])));
    }

    if (tab === 'settings') {
      var d = nodeSettingDrafts[n.id] || (nodeSettingDrafts[n.id] = { name: n.name, label: n.label, down: n.expectedDown, up: n.expectedUp, threads: n.threads, server: n.server, note: n.note || '', ispPlanId: n.ispPlanId || '', downTouched: false, upTouched: false });
      var serverOpts = [{ value: 'auto', label: 'Choose the closest automatically' }].concat(ST.servers.map(function (s) { return { value: String(s.id), label: s.name + ' — ' + s.city }; }));
      var downInput = U.input(d.down, function (v) { d.down = v; d.downTouched = true; }, { type: 'number', min: 1 });
      var upInput = U.input(d.up, function (v) { d.up = v; d.upTouched = true; }, { type: 'number', min: 1 });
      var planField = h('div');
      function renderPlanField(plans) {
        var options = [{ value: '', label: 'No ISP plan' }].concat(plans.map(function (p) { return { value: p.id, label: p.name + ' — ' + F.speed(p.advertisedDown) + ' / ' + F.speed(p.advertisedUp) }; }));
        U.clear(planField);
        planField.appendChild(U.field('ISP plan', U.select(options, d.ispPlanId, function (value) {
          d.ispPlanId = value;
          var selectedPlan = plans.filter(function (p) { return p.id === value; })[0];
          if (selectedPlan) {
            if (!d.downTouched) { d.down = selectedPlan.advertisedDown; downInput.value = d.down; }
            if (!d.upTouched) { d.up = selectedPlan.advertisedUp; upInput.value = d.up; }
          }
        }, 'auto'), n.isp ? 'Choose a plan for ' + n.isp.name + '. Advertised speeds fill expected speeds unless you override them.' : 'A plan can be assigned after this node reports an ISP.'));
      }
      if (n.isp && n.isp.id && !ST.demo && ST.listISPPlans) {
        planField.appendChild(h('div', { role: 'status', class: 'hint' }, 'Loading ISP plans…'));
        ST.listISPPlans(n.isp.id).then(function (plans) { if (planField.isConnected) renderPlanField(plans); }, function (error) {
          if (planField.isConnected) { U.clear(planField); planField.appendChild(U.callout('Could not load ISP plans: ' + error.message, 'warn', 'alert')); }
        });
      }
      var form = h('div', { class: 'stack', style: 'max-width:640px' },
        h('div', { class: 'field-row' }, U.field('Node name', U.input(d.name, function (v) { d.name = v; })), U.field('What does it measure?', U.input(d.label, function (v) { d.label = v; }))),
        n.isp && n.isp.id && !ST.demo ? planField : null,
        h('div', { class: 'field-row' }, U.field('Expected download', U.suffix(downInput, 'Mbps')), U.field('Expected upload', U.suffix(upInput, 'Mbps'))),
        h('div', { class: 'field-row' }, ST.demo ? U.field('Test server', U.select(serverOpts, d.server, function (v) { d.server = v; }), 'Which speed test server this node talks to.') : null,
          U.field('Parallel connections', U.select([1, 2, 4, 6, 8, 12, 16, 32].map(function (x) { return { value: x, label: String(x) }; }), d.threads, function (v) { d.threads = +v; }), 'Number of simultaneous download connections.')),
        U.field('Notes', h('textarea', { class: 'input', rows: 3, onInput: function (e) { d.note = e.target.value; } }, d.note)),
        h('div', { class: 'row' }, U.btn('Save changes', { variant: 'primary', onClick: function () {
          if (!d.name.trim()) { U.toast('Node name cannot be empty', 'error'); return; }
          if (!(+d.down > 0) || !(+d.up > 0)) { U.toast('Expected speeds must be above zero', 'error'); return; }
          var patch = { name: d.name.trim(), label: d.label.trim(), ispPlanId: d.ispPlanId || null, expectedDown: +d.down, expectedUp: +d.up, threads: d.threads, note: d.note };
          if (!d.downTouched) delete patch.expectedDown;
          if (!d.upTouched) delete patch.expectedUp;
          if (ST.demo) patch.server = d.server;
          Promise.resolve(ST.updateNode(n.id, patch)).then(function () { delete nodeSettingDrafts[n.id]; U.toast('Settings saved'); ST.refresh(); }, function (error) {
            U.toast(error.status === 409 ? 'Another node already has this name.' : error.status === 412 ? 'This node was changed elsewhere. Reload and try again.' : error.message, 'error');
          });
        } }), U.btn('Discard', { onClick: function () { delete nodeSettingDrafts[n.id]; delete engineScheduleDrafts[n.id]; if (ST.bandwidth) ST.bandwidth.clearDrafts(n.id); ST.refresh(); } })));
      root.appendChild(U.card('Node settings', null, form));
      var available = n.engines || ['ookla', 'librespeed'], engineRows = engineScheduleDrafts[n.id] || (engineScheduleDrafts[n.id] = ST.engineSchedules(n).map(function (e) { return Object.assign({}, e); }));
      var engineForm = h('div', { class: 'stack', style: 'max-width:640px' });
      var adviceHost = h('div');
      var refreshAdvice = function () { U.clear(adviceHost); ST.bandwidth.advicePanel(adviceHost, n.id, engineRows); };
      ['ookla', 'librespeed'].forEach(function (name) {
        var current = engineRows.filter(function (e) { return e.engine === name; })[0];
        var installed = available.indexOf(name) !== -1;
        var draft = Object.assign({ engine: name, interval: 60, offset: 0, enabled: false }, current || {});
        if (ST.bandwidth) draft.interval = ST.bandwidth.draftInterval(n.id, name, draft.interval);
        if (!current && installed) engineRows.push(draft); else if (current) { current.interval = draft.interval; draft = current; }
        var label = name === 'ookla' ? 'Ookla' : 'LibreSpeed';
        engineForm.appendChild(h('div', { class: 'setting engine-setting' },
          h('div', null, h('strong', null, label), h('p', { class: 'hint' }, installed ? 'Installed on this node' : 'Not installed. Repair the agent installation, then wait for its heartbeat.')),
            installed ? h('div', { class: 'row wrap' }, U.field(label + ' interval', U.input(draft.interval, function (v) { draft.interval = +v; if (ST.bandwidth) ST.bandwidth.setDraftInterval(n.id, name, +v); }, { type: 'number', min: 5, max: 1440, 'aria-label': label + ' interval', onBlur: function () { if (ST.bandwidth) { ST.bandwidth.setDraftInterval(n.id, name, draft.interval); refreshAdvice(); } } }), 'minutes'),
            U.toggle(draft.enabled, function (v) { draft.enabled = v; }, label + ' enabled')) : null,
          installed && !ST.demo ? serverPicker(n.id, name, label) : null));
      });
      var saveEngines = U.btn('Save engine schedules', { variant: 'primary', onClick: function () {
        if (engineRows.some(function (e) { return !Number.isInteger(+e.interval) || +e.interval < 5 || +e.interval > 1440; })) { U.toast('Intervals must be whole minutes between 5 and 1440.', 'error'); return; }
        saveEngines.disabled = true;
        ST.saveEngineSchedules(n.id, engineRows).then(function () { delete engineScheduleDrafts[n.id]; if (ST.bandwidth) ST.bandwidth.clearDrafts(n.id); U.toast('Engine schedules saved'); ST.refresh(); }, function (error) { U.toast(error.status === 412 ? 'Schedules changed elsewhere. Reload and try again.' : error.message, 'error'); }).finally(function () { saveEngines.disabled = false; });
      } });
      engineForm.appendChild(h('p', { class: 'hint' }, 'Engines run one at a time. When nodes share a public IP, use a longer Ookla interval to avoid its rate limit.'));
      engineForm.appendChild(saveEngines);
      root.appendChild(U.card('Test engines', 'Each engine has its own schedule. Results remain separate.', engineForm));
      refreshAdvice();
      root.appendChild(adviceHost);
      root.appendChild(h('div', { class: 'mt' }, U.card('Remove this node', 'Stops testing and removes the program from the machine.', h('div', null,
        h('p', { class: 'text-2', style: 'margin-bottom:10px' }, 'The results already collected for this node are deleted from the dashboard.'),
        U.btn('Remove ' + n.name, { variant: 'danger', icon: 'trash', onClick: function () {
          if (!ST.demo) { removeNodeDialog(n); return; }
          U.confirm({ title: 'Remove ' + n.name + '?', text: ST.demo ? 'This deletes ' + ST.nodeResults(n.id).length + ' results and uninstalls the program from the machine. This cannot be undone.' : 'This deletes the node and all of its results from the dashboard. This cannot be undone. The program on the machine stops reporting; remove it there as well if you no longer need it.', confirm: 'Remove node', danger: true }).then(function (ok) {
            if (!ok) return;
            Promise.resolve(ST.removeNode(n.id)).then(function () { location.hash = '#/nodes'; U.toast(n.name + ' removed'); }, function (error) { U.toast(error.status === 412 ? 'This node was changed elsewhere. Reload and try again.' : error.message, 'error'); });
          });
        } })))));
    }

    if (tab === 'agent' && !ST.demo) {
      var stateChip = !n.enabled ? U.chip('Paused', '', 'pause') : n.status === 'offline' ? U.chip('Not reporting', 'warn', 'alert') : U.chip('Running', 'good', 'check');
      root.appendChild(U.card('Program on the machine', null, h('div', { class: 'stack' },
        U.kv([['Machine', h('span', { class: 'mono' }, n.host || '—')], ['Program version', n.agent ? 'v' + n.agent : '—'], ['Last check-in', n.lastSeen ? F.rel(n.lastSeen) : 'Never'], ['Status', stateChip]]),
        n.updateVersion ? h('div', { class: 'row wrap' }, U.btn('Update to v' + n.updateVersion, { icon: 'refresh', onClick: function () {
          Promise.resolve(ST.updateAgent(n.id, n.updateVersion)).then(function () { U.toast('Update queued: the program restarts when it is installed'); }, function (error) { U.toast(error.message, 'error'); });
        } })) : h('p', { class: 'hint' }, 'The program is up to date.'))));
    } else if (tab === 'agent') {
      var logs = ['info  agent started (v' + n.agent + ')', 'info  connected to ' + ST.settings().centerUrl, 'info  schedule received: every ' + n.interval + ' min, offset +' + n.offset + ' min', last ? 'info  last test ' + (last.status === 'failed' ? 'failed: ' + last.error : 'finished: ' + F.speed(last.down) + ' down, ' + F.speed(last.up) + ' up') : 'info  waiting for first test', 'info  heartbeat ok'];
      var logBox = h('pre', { class: 'code', style: 'min-height:120px' }, logs.join('\n'));
      var appendLog = function (line) { logBox.textContent += '\n' + line; logBox.scrollTop = logBox.scrollHeight; };
      root.appendChild(U.card('Program on the machine', null, h('div', { class: 'stack' },
        U.kv([['Machine', h('span', { class: 'mono' }, n.host || '—')], ['Program version', 'v' + n.agent + (n.agent !== '1.4.2' ? ' (update available: v1.4.2)' : ' (latest)')], ['Last check-in', last ? F.rel(last.ts) : 'Never'], ['Status', n.enabled ? U.chip('Running', 'good', 'check') : U.chip('Paused', '', 'pause')]]),
        h('div', { class: 'row wrap' },
          U.btn('Update program', { icon: 'refresh', disabled: n.agent === '1.4.2', onClick: function () { appendLog('info  downloading v1.4.2 …'); setTimeout(function () { appendLog('info  restarting'); }, 700); setTimeout(function () { ST.updateNode(n.id, { agent: '1.4.2' }); U.toast('Program updated to v1.4.2'); }, 1500); } }),
          U.btn('Restart', { icon: 'refresh', onClick: function () { appendLog('info  restart requested'); setTimeout(function () { appendLog('info  agent started (v' + n.agent + ')'); U.toast('Restarted'); }, 800); } }),
          U.btn('Reinstall', { onClick: function () { appendLog('info  reinstalling …'); setTimeout(function () { appendLog('info  done'); U.toast('Reinstalled'); }, 1200); } })),
        h('div', null, h('div', { class: 'label', style: 'margin-bottom:6px' }, 'Recent activity'), logBox))));
    }
  };

  function serverPicker(nodeId, engineName, label) {
    var query = '', catalog = null, error = '', busy = false;
    var list = h('div'), status = h('span', { class: 'hint' }, 'Loading server list…');
    var search = U.input('', function (value) { query = value.toLowerCase().trim(); drawRows(); }, { type: 'search', placeholder: 'Search name, city or ISP', 'aria-label': 'Search ' + label + ' servers' });
    var refresh = U.btn('Refresh server list', { icon: 'refresh', onClick: refreshList });
    var automatic = U.btn('Use automatic selection', { onClick: function () { select('auto'); } });
    var controls = h('div', { class: 'row wrap' }, refresh, automatic, status);
    var root = h('div', { class: 'stack server-picker', style: 'width:100%' }, U.field('Search ' + label + ' servers', search, 'Search by server name, city or ISP.'), controls, list);

    function refreshList() {
      if (busy) return;
      busy = true; error = ''; refresh.disabled = true; refresh.textContent = 'Finding servers…';
      ST.refreshServerCatalog(nodeId, engineName).then(function (found) { catalog = found; })
        .catch(function (failure) { error = failure.message; })
        .finally(function () { busy = false; refresh.disabled = false; refresh.textContent = 'Refresh server list'; drawRows(); });
    }
    function select(serverId) {
      if (!catalog || busy) return;
      busy = true; automatic.disabled = true; error = '';
      ST.selectServer(nodeId, engineName, serverId, catalog.etag).then(function (selection) { catalog = selection; U.toast(label + ' server selection saved'); })
        .catch(function (failure) { error = failure.status === 412 ? 'Node settings changed elsewhere. Reload this page and try again.' : failure.message; U.toast(error, 'error'); })
        .finally(function () { busy = false; automatic.disabled = false; drawRows(); });
    }
    function drawRows() {
      U.clear(list);
      if (error) { status.textContent = 'Server list unavailable'; list.appendChild(U.callout(error + ' Try refreshing the list.', 'warn', 'alert')); return; }
      if (!catalog) { status.textContent = 'Loading server list…'; return; }
      var rows = (catalog.servers || []).filter(function (server) {
        return [server.name, server.city, server.isp, server.distanceKm].join(' ').toLowerCase().indexOf(query) >= 0;
      });
      if (engineName === 'ookla') rows.sort(function (a, b) { return (a.distanceKm == null ? Infinity : a.distanceKm) - (b.distanceKm == null ? Infinity : b.distanceKm) || a.name.localeCompare(b.name); });
      status.textContent = rows.length + ' servers' + (catalog.refreshedAt ? ' · updated ' + F.rel(Date.parse(catalog.refreshedAt)) : ' · not refreshed yet');
      if (!rows.length) { list.appendChild(U.empty(catalog.servers.length ? 'No servers match' : 'No server list yet', catalog.servers.length ? 'Try a shorter search.' : 'Refresh the list from the node first.')); return; }
      var columns = [
        { key: 'name', label: 'Server', render: function (server) { return server.name + ' (#' + server.id + ')'; } },
        { key: 'city', label: 'City', render: function (server) { return server.city || '—'; } },
        { key: 'isp', label: 'ISP', render: function (server) { return server.isp || '—'; } }
      ];
      if (engineName === 'ookla') columns.push({ key: 'distance', label: 'Distance', num: true, render: function (server) { return server.distanceKm == null ? '—' : F.num(server.distanceKm, 1) + ' km'; } });
      columns.push({ key: 'select', label: '', render: function (server) { return U.btn(Number(catalog.serverId) === server.id ? 'Pinned' : 'Pin', { size: 'sm', disabled: busy || Number(catalog.serverId) === server.id, onClick: function () { select(server.id); } }); } });
      list.appendChild(U.table({ columns: columns, rows: rows, cards: true }));
    }
    ST.serverCatalog(nodeId, engineName).then(function (found) { catalog = found; drawRows(); })
      .catch(function (failure) { error = failure.message; drawRows(); });
    return root;
  }
})();

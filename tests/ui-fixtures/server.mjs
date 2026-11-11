// Isolated contract-shaped API; never connects to PostgreSQL or real agents.
import http from 'node:http';
import { readFile } from 'node:fs/promises';
import { resolve, extname } from 'node:path';

export function fixture() {
  const ts = new Date().toISOString();
  const node = { id: 'node-contract', name: 'Contract node', label: 'Test connection', provider: 'Fixture', plan: '',
    expectedDown: 100, expectedUp: 50, threads: 4, server: 'auto', note: '', enabled: true, binding: { iface: '', sourceIp: null }, autoUpdate: false,
    colorSlot: 1, interval: 30, offset: 0, host: '', agent: 'test', createdAt: ts, lastSeen: ts, status: 'online',
    latest: null, latestGood: null, currentIp: '203.0.113.9', geo: null, isp: null, updateVersion: null };
  const result = { id: 'a4bbcf7b-8938-4e90-8128-59047d35b087', nodeId: node.id, ts,
    serverId: 1, threads: 4, ip: '203.0.113.9', status: 'ok', error: null, down: 90, up: 40, ping: 0,
    jitter: null, loss: null, dLat: null, uLat: null, bytesDown: 1000, bytesUp: 500, duration: 1,
    manual: false, raw: {}, server: { id: 1, name: 'Fixture server', city: '', sponsor: '' }, geo: null };
  node.latest = result; node.latestGood = result;
  const isp = { id: '9c96b615-8275-4df5-96e6-1ba67e5a16c7', asn: 64500, organisation: 'Fixture Network',
    name: 'Fixture ISP', nodeCount: 1, avgDown: 90, p95Down: 90, pctExpected: 90, successRate: 1, avgPing: 10 };
  const notify = Object.fromEntries(['email', 'telegram', 'slack', 'webhook'].map(key => [key, { on: false, to: '', configured: false }]));
  const settings = { theme: 'dark', range: '7d', units: 'auto', timezone: 'browser', hour12: false, decimals: 1,
    retentionDays: 365, geoProvider: 'geolite2', geoFallback: 'ip-api', geoAutoUpdate: true, engine: 'ookla',
    defaultThreads: 4, serverMode: 'auto', maxConcurrent: 1, stagger: true, quietStart: '', quietEnd: '', notifyQuietStart: '', notifyQuietEnd: '', notifyQuietTimezone: 'UTC', notifyQuietCritical: true,
    centerUrl: 'http://localhost', geoKeyConfigured: false, enrollKeyConfigured: false, notify };
  const rule = { id: 'rule-fixture', name: 'Slow connection', metric: 'down_pct', op: '<', value: 60, forTests: 3,
    nodes: 'all', severity: 'warn', channels: ['email'], enabled: true };
  const event = { id: 'event-fixture', ruleId: rule.id, nodeId: node.id, severity: 'warn', ts, resolvedAt: null,
    title: rule.name, detail: 'Fixture alert', acked: false };
  const schedules = { rules: { stagger: true, maxConcurrent: 1, quiet: { quietStart: '', quietEnd: '', timezone: 'UTC' } },
    nodes: [{ nodeId: node.id, config: { interval: 30, offset: 0, enabled: true }, nextRun: ts, upcoming: [ts], bytesPerDay: 72000, bytesPerMonth: 2160000 }],
    clashes: [] };
  return { node, result, isp, settings, rule, event, schedules };
}
const ts0 = () => new Date().toISOString();
export const envelope = (data, meta = {}, code = 200) => ({ success: true, code, message: 'OK', data, meta, errors: null });
export const problem = status => ({ type: `/problems/test-${status}`, title: `HTTP ${status}`, status,
  detail: status === 403 ? 'CSRF token invalid. Retry the request.' : `Fixture failure ${status}. Retry the request.` });

export async function startServer() {
  const data = fixture();
  const control = { status: 0, empty: false, delay: 0, requests: [], responses: [], session: true,
    failWrites: 0, failWriteStatus: 412, failPaths: new Set() };
  const server = http.createServer(async (req, res) => {
    const url = new URL(req.url, 'http://localhost');
    if (url.pathname.startsWith('/api/')) {
      const chunks = [];
      for await (const chunk of req) chunks.push(chunk);
      const body = Buffer.concat(chunks).toString();
      control.requests.push({ path: url.pathname, query: url.searchParams, headers: req.headers, method: req.method,
        body: body ? JSON.parse(body) : undefined });
      if (control.delay) await new Promise(resolve => setTimeout(resolve, control.delay));
      const send = (status, value) => {
        control.responses.push({ path: url.pathname, method: req.method.toLowerCase(), status, value });
        res.writeHead(status, { 'Content-Type': status >= 400 ? 'application/problem+json' : 'application/json',
          'Cache-Control': 'no-store', ETag: '"fixture-v1"', ...(status === 429 ? { 'Retry-After': '2' } : {}) });
        res.end(JSON.stringify(value));
      };
      if (control.status) { send(control.status, problem(control.status)); return; }
      if (control.failPaths.has(url.pathname)) { send(500, problem(500)); return; }
      // failWrites: the next N writes are refused with failWriteStatus (a stale ETag by default).
      if (req.method !== 'GET' && control.failWrites > 0) { control.failWrites--; send(control.failWriteStatus, problem(control.failWriteStatus)); return; }
      if (req.method === 'POST' && url.pathname === '/api/auth/password') { control.responses.push({ path: url.pathname, method: 'post', status: 204, value: null }); res.writeHead(204, { 'Cache-Control': 'no-store' }); res.end(); return; }
      const installId = '9b0f2c3e-1111-4222-8333-444455556666';
      if (url.pathname === `/api/installations/${installId}/events`) {
        control.responses.push({ path: url.pathname, method: 'get', status: 200, value: null });
        res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-store' });
        const events = control.installFails
          ? [['Connecting to the machine', 10, 'running'], ['Checking the system', 30, 'running'], ['Sudo is not allowed for this account.', 30, 'failed']]
          : [['Connecting to the machine', 10, 'running'], ['Downloading the program', 50, 'running'], ['Waiting for the first heartbeat', 90, 'running'], ['Installed and reporting', 100, 'succeeded']];
        events.forEach(([message, pct, status], i) => setTimeout(() => res.write(`id: ${i + 1}\nevent: progress\ndata: ${JSON.stringify({ id: String(i + 1), ts: new Date().toISOString(), step: 'step' + i, message, pct, status })}\n\n`), 80 * (i + 1)));
        setTimeout(() => res.end(), 80 * events.length + 200);
        return;
      }
      if (req.method === 'GET' && url.pathname === `/api/installations/${installId}`) {
        send(200, envelope({ id: installId, nodeId: data.node.id, status: control.installFails ? 'failed' : 'succeeded', step: 'done', error: control.installFails ? 'Sudo is not allowed for this account.' : null })); return;
      }
      if (url.pathname === '/api/auth/me') {
        send(200, envelope({ user: control.session ? { id: 'owner', email: 'owner@example.com' } : null,
          csrfToken: 'fixture-csrf', expiresAt: new Date(Date.now() + 60000).toISOString() })); return;
      }
      if (req.method !== 'GET') {
        if (req.headers['x-csrf-token'] !== 'fixture-csrf') { send(403, problem(403)); return; }
        const input = body ? JSON.parse(body) : {};
        if (url.pathname.startsWith('/api/notifications/')) {
          const parts = url.pathname.split('/').slice(3);
          if (req.method === 'PATCH' && parts.length === 2 && parts[0] === 'events') {
            send(200, envelope({ key: parts[1], enabled: input.enabled, severity: input.severity || 'info', channels: input.channels || ['telegram'] })); return;
          }
          if (req.method === 'PUT' && parts.length === 4 && parts[0] === 'events' && parts[2] === 'nodes') {
            send(200, envelope({ enabled: input.enabled })); return;
          }
          if (parts[0] === 'templates' && parts.length >= 3) {
            const [key, channel, action] = parts.slice(1);
            const template = { eventKey: key, channel, body: 'Hello {node}', isDefault: action === 'resets' };
            if (req.method === 'PUT' && parts.length === 3) { send(200, envelope({ ...template, body: input.body, isDefault: false })); return; }
            if (req.method === 'POST' && action === 'previews') { send(200, envelope({ channel, body: 'Hello Viettel Fast 50', sample: { node: 'Viettel Fast 50' } })); return; }
            if (req.method === 'POST' && action === 'resets') { send(200, envelope(template)); return; }
          }
        }
        if (['PUT', 'PATCH', 'DELETE'].includes(req.method) && !req.headers['if-match'] && !url.pathname.startsWith('/api/notifications/')) { send(428, problem(428)); return; }
        if (url.pathname === '/api/installer/probes') {
          send(201, envelope({ id: 'c0ffee00-0000-4000-8000-000000000001', fingerprint: 'SHA256:fixtureFingerprint0123456789', expiresAt: new Date(Date.now() + 600000).toISOString() }, {}, 201)); return;
        }
        if (url.pathname.endsWith('/installations') || url.pathname.endsWith('/uninstallations')) {
          control.lastInstall = input; control.lastInstallKind = url.pathname.endsWith('/uninstallations') ? 'uninstall' : 'install'; // kept for assertions
          control.responses.push({ path: url.pathname, method: 'post', status: 202, value: null });
          res.writeHead(202, { 'Content-Type': 'application/json', Location: '/api/installations/9b0f2c3e-1111-4222-8333-444455556666' });
          res.end(JSON.stringify(envelope({ id: '9b0f2c3e-1111-4222-8333-444455556666', nodeId: data.node.id, status: 'pending', step: 'queued', error: null }, {}, 202))); return;
        }
        const created = url.pathname === '/api/nodes' || url.pathname === '/api/alerts/rules';
        const queued = url.pathname.endsWith('/run') || url.pathname.endsWith('/commands') || url.pathname.startsWith('/api/notifications/test/') || url.pathname === '/api/alerts/acknowledgements';
        if ((created || queued) && !req.headers['idempotency-key']) { send(400, problem(400)); return; }
        if (req.method === 'DELETE') {
          if (url.pathname.startsWith('/api/nodes/') && url.searchParams.get('confirm') !== 'true') { send(422, problem(422)); return; }
          res.writeHead(204); res.end(); return;
        }
        let output, status = 200;
        if (url.pathname === '/api/settings') {
          output = { ...data.settings, ...input, notify: data.settings.notify };
          delete output.geoKey;
        }
        else if (url.pathname === '/api/nodes') {
          status = 201;
          output = { node: { ...data.node, ...input }, token: 'fixture-issuance-only', expiresAt: new Date(Date.now() + 60000).toISOString() };
        } else if (url.pathname === '/api/alerts/rules') { status = 201; output = { ...data.rule, ...input }; }
        else if (url.pathname === '/api/alerts/acknowledgements') { status = 202; output = { id: 'job-fixture', status: 'pending', error: null }; }
        else if (url.pathname.endsWith('/run')) {
          status = 202; output = { id: 'command-fixture', nodeId: data.node.id, type: 'run_now', version: null, status: 'pending',
            createdAt: new Date().toISOString(), ackedAt: null, phase: '', pct: 0, resultId: null, error: null };
        } else if (url.pathname.endsWith('/commands')) {
          status = 202; output = { id: 'command-update', nodeId: data.node.id, type: input.type, version: input.version ?? null, status: 'pending',
            createdAt: new Date().toISOString(), ackedAt: null, phase: '', pct: 0, resultId: null, error: null };
        } else if (url.pathname === '/api/schedules/rules') output = data.schedules.rules;
        else if (url.pathname === '/api/schedules/stagger') output = data.schedules;
        else if (url.pathname.startsWith('/api/schedules/')) output = { ...data.schedules.nodes[0], config: input };
        else if (url.pathname === '/api/settings/enrolment-key') { status = 201; output = { enrollToken: 'st_enroll_fixture_once', createdAt: new Date().toISOString() }; }
        else if (url.pathname.startsWith('/api/notifications/test/')) {
          status = 202; output = { id: 'd1e7a000-0000-4000-8000-000000000001', eventId: null, channel: url.pathname.split('/').pop(), status: 'pending', attempts: 0, createdAt: new Date().toISOString(), lastAttempt: null, error: null };
        } else if (url.pathname.endsWith('/ack')) output = { ...data.event, ...input };
        else if (url.pathname.startsWith('/api/alerts/rules/')) output = { ...data.rule, ...input };
        else if (url.pathname.startsWith('/api/nodes/')) output = { ...data.node, ...input };
        else { send(404, problem(404)); return; }
        send(status, envelope(output, {}, status)); return;
      }
      if (url.pathname.startsWith('/api/stats/')) {
        const id = data.node.id, summary = { n: 1, mean: 90, median: 90, min: 90, max: 90, p5: 90, p95: 90, std: 0 };
        const node = { count: 1, ok: 1, failed: 0, successRate: 100, down: summary, up: summary, ping: summary, jitter: summary, loss: summary, ipCount: 1, downPct: 90, upPct: 80, stability: 100 };
        const day = new Date().toISOString().slice(0, 10) + 'T00:00:00Z', kind = url.pathname.split('/').pop();
        const body = kind === 'summary' ? [{ nodeId: id, current: node, previous: node }]
          : kind === 'trend' ? [{ nodeId: id, name: data.node.name, metric: 'down', unit: 'Mbps', points: [{ ts: day, value: 90, count: 1 }], summary }]
          : kind === 'hour-of-day' ? [{ nodeId: id, hours: Array.from({ length: 24 }, (_, hour) => ({ hour, value: hour === 3 ? 90 : null, count: hour === 3 ? 1 : 0 })) }]
          : kind === 'heatmap' ? { nodeId: id, metric: 'down', timezone: 'UTC', cells: Array.from({ length: 168 }, (_, i) => ({ weekday: Math.floor(i / 24), hour: i % 24, value: i === 3 ? 90 : null, count: i === 3 ? 1 : 0 })) }
          : [{ nodeId: id, metric: 'down', count: 1, bins: [{ from: 90, to: 91, count: 1, percent: 100 }] }];
        send(200, envelope(body)); return;
      }
      if (url.pathname === '/api/isps') { send(200, envelope(control.empty ? [] : [data.isp])); return; }
      if (url.pathname === '/api/isps/compare') { send(200, envelope(control.empty ? [] : [data.isp])); return; }
      if (url.pathname.endsWith('/trend') && url.pathname.startsWith('/api/isps/')) {
        send(200, envelope(control.empty ? [] : [{ ts: ts0(), down: 90, up: 40, ping: 10, n: 1 }])); return;
      }
      if (url.pathname.startsWith('/api/isps/')) { send(200, envelope(data.isp)); return; }
      if (url.pathname === '/api/settings') { send(200, envelope(data.settings)); return; }
      if (url.pathname === '/api/notifications/events') {
        send(200, envelope([{ key: 'test.success', title: 'Test success', enabled: true, severity: 'info', channels: ['telegram'], nodeScoped: true }])); return;
      }
      if (/^\/api\/notifications\/events\/[^/]+\/nodes\/[^/]+$/.test(url.pathname)) {
        send(200, envelope({ enabled: true })); return;
      }
      if (url.pathname.startsWith('/api/notifications/templates/')) {
        const [, , , key, channel] = url.pathname.split('/');
        send(200, envelope({ eventKey: key, channel, body: 'Hello {node}', isDefault: true })); return;
      }
      if (url.pathname === '/api/schedules') { send(200, envelope(data.schedules)); return; }
      if (url.pathname.startsWith('/api/schedules/')) { send(200, envelope(data.schedules.nodes[0])); return; }
      if (/\/commands\/[^/]+$/.test(url.pathname)) {
        if (control.pending > 0) {
          control.pending--;
          send(200, envelope({ id: 'command-fixture', nodeId: data.node.id, type: 'run_now', version: null, status: 'running', createdAt: ts0(), ackedAt: null, phase: 'Testing download', pct: 40, resultId: null, error: null })); return;
        }
        const failed = control.commandFails;
        send(200, envelope({ id: 'command-fixture', nodeId: data.node.id, type: 'run_now', version: null, status: failed ? 'failed' : 'succeeded', createdAt: ts0(),
          ackedAt: ts0(), phase: failed ? '' : 'done', pct: failed ? 0 : 100, resultId: failed ? null : data.result.id, error: failed ? 'speedtest: network is down' : null })); return;
      }
      if (url.pathname.startsWith('/api/notifications/deliveries/')) {
        const waiting = control.pending > 0 && control.pending--;
        send(200, envelope({ id: url.pathname.split('/').pop(), eventId: null, channel: 'webhook', status: waiting ? 'pending' : control.deliveryFails ? 'failed' : 'sent', attempts: 1, createdAt: ts0(), lastAttempt: ts0(), error: control.deliveryFails ? 'the receiver answered 500' : null })); return;
      }
      if (url.pathname.startsWith('/api/results/')) { send(200, envelope(data.result)); return; }
      if (url.pathname.startsWith('/api/nodes/')) { send(200, envelope(data.node)); return; }
      if (url.pathname.startsWith('/api/alerts/rules/')) { send(200, envelope(data.rule)); return; }
      if (url.pathname.startsWith('/api/alerts/events/')) { send(200, envelope(data.event)); return; }
      const lists = {
        '/api/nodes': [data.node], '/api/results': [data.result], '/api/isps': [data.isp], '/api/alerts/rules': [], '/api/alerts/events': [],
        '/api/audit': [{ id: '7d1c0000-0000-4000-8000-000000000001', ts: '2026-11-05T05:00:00Z', actor: 'owner@example.com', action: 'settings.update', targetType: 'settings', targetId: '', summary: 'Changed settings', remote: '127.0.0.1' }],
        '/api/ip/addresses': [{ nodeId: data.node.id, ip: data.result.ip, first: data.result.ts, last: data.result.ts,
          tests: 1, avgDown: 90, avgUp: 40, geo: null }], '/api/ip/history': []
      };
      if (url.pathname in lists) {
        const rows = control.empty ? [] : lists[url.pathname];
        const page = Number(url.searchParams.get('page') || 1), size = Number(url.searchParams.get('size') || 25);
        send(200, envelope(rows.slice((page - 1) * size, page * size), { pagination: {
          current_page: page, per_page: size, total: rows.length, last_page: Math.max(1, Math.ceil(rows.length / size)) } })); return;
      }
      send(404, problem(404)); return;
    }
    const file = resolve('ui', '.' + decodeURIComponent(url.pathname === '/' ? '/index.html' : url.pathname));
    if (!file.startsWith(resolve('ui') + '/')) { res.writeHead(404); res.end(); return; }
    try {
      const content = await readFile(file);
      res.writeHead(200, { 'Content-Type': ({ '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css' })[extname(file)] || 'application/octet-stream' });
      res.end(content);
    } catch { res.writeHead(404); res.end(); }
  });
  const port = process.env.UI_TEST_PORT === undefined ? 0 : Number(process.env.UI_TEST_PORT);
  if (!Number.isInteger(port) || port < 0 || port > 65535) throw new Error('UI_TEST_PORT must be between 0 and 65535.');
  await new Promise(resolve => server.listen(port, '127.0.0.1', resolve));
  return { origin: `http://127.0.0.1:${server.address().port}`, control, data,
    close: () => new Promise(resolve => { server.closeAllConnections(); server.close(resolve); }) };
}

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { runtime } from './runtime.mjs';
import { startServer, envelope, problem } from '../ui-fixtures/server.mjs';

async function setup(t) {
  const server = await startServer(); t.after(server.close);
  return { ...server, ...runtime(server.origin) };
}
test('real store loads contract data, RFC timestamps, server details and null/zero measurements', async t => {
  const { ST, data, storage } = await setup(t);
  assert.equal(ST.demo, false); assert.equal(ST.nodes().length, 0); assert.equal(ST.rules().length, 0);
  assert.equal(ST.geo('203.0.113.9'), null);
  await ST.load();
  assert.equal(ST.remote.status, 'ready');
  assert.equal(ST.node(data.node.id).createdAt, Date.parse(data.node.createdAt));
  const result = ST.results()[0];
  assert.equal(result.ts, Date.parse(data.result.ts)); assert.equal(result.ping, 0); assert.equal(result.jitter, null);
  assert.equal(ST.latest(data.node.id).id, data.result.id);
  assert.equal(ST.server(1).name, 'Fixture server');
  assert.equal(ST.ipHistory(data.node.id).ips[0].first, result.ts);
  assert.equal(ST.events().length, 0); ST.recomputeEvents();
  assert.equal(ST.settings().geoKey, ''); assert.equal(ST.settings().enrollToken, '');
  ST.persist(); ST.resetDemo(); assert.equal(storage.size, 0);
});
test('empty responses produce a ready empty store; failures expose retry state without demo fallback', async t => {
  const { ST, control } = await setup(t);
  control.status = 500;
  await ST.load(); assert.equal(ST.remote.status, 'error'); assert.equal(ST.nodes().length, 0);
  control.status = 0; control.empty = true;
  await ST.load(); assert.equal(ST.remote.status, 'ready'); assert.equal(ST.results().length, 0);
});
test('anonymous sessions redirect without querying protected data', async t => {
  const { ST, context, control } = await setup(t); control.session = false;
  await ST.load();
  assert.equal(context.location.hash, '#/signin'); assert.equal(ST.remote.status, 'error');
  assert.equal(control.requests.length, 1);
});
test('superseded and cancelled loads cannot overwrite the current snapshot', async t => {
  const { ST, control } = await setup(t);
  control.delay = 50;
  const first = ST.load(); const second = ST.load(); await Promise.all([first, second]);
  assert.equal(ST.remote.status, 'ready');
  const third = ST.load(); ST.cancelLoad(); await third;
  assert.equal(ST.remote.status, 'idle');
});
test('demo mode seeds and persists locally without making API calls', () => {
  const { ST, storage } = runtime('http://localhost', { demo: true, fetch: () => { throw Error('demo fetched API'); } });
  assert.equal(ST.nodes().length, 5); assert.ok(ST.results().length > 5000);
  ST.updateNode('n1', { name: 'Local demo' });
  assert.equal(ST.node('n1').name, 'Local demo'); assert.ok(storage.has('st.demo.v1'));
});
test('demo bandwidth budgets survive reload and reset with the rest of demo state', () => {
  const first = runtime('http://localhost', { demo: true });
  const budget = { scopeKey: 'global', monthlyBytes: 4096, action: 'pause', widenMultiplier: 2 };
  first.ST.setDemoBandwidthBudgets([budget]);
  assert.equal(JSON.stringify(first.ST.demoBandwidthBudgets()), JSON.stringify([budget]));
  const reloaded = runtime('http://localhost', { demo: true, storage: first.storage });
  assert.equal(JSON.stringify(reloaded.ST.demoBandwidthBudgets()), JSON.stringify([budget]));
  reloaded.ST.resetDemo();
  const reset = runtime('http://localhost', { demo: true, storage: first.storage });
  assert.equal(JSON.stringify(reset.ST.demoBandwidthBudgets()), '[]');
});
test('percentiles and week changes use interpolated values and preserve undefined cases', () => {
  const { ST } = runtime('http://localhost', { demo: true });
  const summary = ST.stats.summary([10, 20, 30, 40, 50]);
  assert.equal(summary.p50, 30); assert.equal(summary.median, summary.p50);
  assert.equal(summary.p95, 48); assert.equal(summary.p99, 49.6);
  assert.equal(ST.stats.summary([]).p50, null); assert.equal(ST.stats.summary([]).p99, null);
  assert.equal(ST.stats.summary([7]).p50, 7); assert.equal(ST.stats.summary([7]).p99, 7);
  assert.equal(ST.stats.weekChange(120, 100), 20);
  assert.equal(ST.stats.weekChange(100, 0), null); assert.equal(ST.stats.weekChange(null, 100), null);
});
test('effective ISP prefers the trimmed engine value and falls back to cached IP organisation', () => {
  const { ST } = runtime('http://localhost', { demo: true });
  assert.equal(ST.stats.effectiveISP({ engineISP: '  Engine ISP  ', ip: '203.0.113.9' }), 'Engine ISP');
  assert.equal(ST.stats.effectiveISP({ engineISP: ' ', ip: '203.0.113.9' }), 'Viettel Group');
  assert.equal(ST.stats.effectiveISP({ ip: '192.0.2.99' }), 'VNPT Corp');
  assert.equal(ST.stats.effectiveISP({ status: 'failed', failureClass: 'rate_limited', ip: null }), null);
  assert.equal(ST.stats.effectiveISP({ ip: '203.0.114.9' }), null);
  const filtered = ST.results({ nodeIds: ['n1'], isp: 'viettel group' });
  assert.ok(filtered.length > 0);
  assert.ok(filtered.every(result => ST.stats.effectiveISP(result).toLowerCase() === 'viettel group'));
  assert.equal(ST.results({ nodeIds: ['n1'], isp: 'unknown isp' }).length, 0);
});
test('demo IP marks compare known results within one engine across nulls and range edges', () => {
  const { ST } = runtime('http://localhost', { demo: true });
  const interleaved = [
    { id: 'a', ts: 1, engine: 'ookla', ip: '203.0.113.1' },
    { id: 'b', ts: 2, engine: 'librespeed', ip: '203.0.113.2' },
    { id: 'c', ts: 3, engine: 'ookla', ip: '203.0.113.1' },
    { id: 'd', ts: 4, engine: 'ookla', ip: '203.0.113.1' },
    { id: 'e', ts: 5, engine: 'librespeed', ip: '203.0.113.2' },
    { id: 'f', ts: 6, engine: 'ookla', ip: '203.0.113.2' },
  ];
  const interleavedChanges = Array.from(ST.stats.ipChanges(interleaved, 'ookla', 0, 7), change => [change.from, change.result.ip]);
  assert.equal(JSON.stringify(interleavedChanges), JSON.stringify([['203.0.113.1', '203.0.113.2']]));
  const boundaryAndNull = [
    { id: 'before', ts: 0, engine: 'ookla', ip: '203.0.113.3' },
    { id: 'start', ts: 1, engine: 'ookla', ip: '203.0.113.4' },
    { id: 'null', ts: 2, engine: 'ookla', ip: null },
    { id: 'after-null', ts: 3, engine: 'ookla', ip: '203.0.113.5' },
    { id: 'end', ts: 5, engine: 'ookla', ip: '203.0.113.6' },
  ];
  const boundaryChanges = Array.from(ST.stats.ipChanges(boundaryAndNull, 'ookla', 1, 5), change => [change.result.id, change.from, change.result.ip]);
  assert.equal(JSON.stringify(boundaryChanges), JSON.stringify([
    ['start', '203.0.113.3', '203.0.113.4'],
    ['after-null', '203.0.113.4', '203.0.113.5'],
  ]));
});
test('engine selector updates the selected engine and refreshes demo data', () => {
  const { ST } = runtime('http://localhost', { demo: true });
  let refreshes = 0;
  ST.ui = { field: (label, control) => ({ label, control }), select: (options, value, onChange) => ({ options, value, onChange }) };
  ST.refresh = () => { refreshes++; };
  const field = ST.engineSelector();
  field.control.onChange('librespeed');
  assert.equal(field.label, 'Engine');
  assert.equal(ST.selectedEngine, 'librespeed');
  assert.equal(ST.engineQuery(), '&engine=librespeed');
  assert.equal(refreshes, 1);
});
test('engine schedules use the primary engine for legacy nodes', () => {
  const { ST } = runtime('http://localhost', { demo: true });
  const [schedule] = ST.engineSchedules({ interval: 60, enabled: true });
  assert.equal(schedule.engine, 'ookla'); assert.equal(schedule.interval, 60);
  assert.equal(schedule.offset, 0); assert.equal(schedule.enabled, true);
});
test('remote engine schedules are filtered by node and include the default engine', async t => {
  const { ST, data } = await setup(t);
  await ST.load();
  const [schedule] = ST.engineSchedules({ id: data.node.id });
  assert.equal(schedule.engine, 'ookla'); assert.equal(schedule.interval, 30);
  assert.equal(ST.engineSchedules({ id: 'missing' }).length, 0);
});
test('duration formatting includes days', () => {
  const { ST } = runtime('http://localhost', { demo: true });
  assert.equal(ST.fmt.dur(90_000_000), '1d 1h');
});
test('new demo nodes use a 60-minute interval and zero offset', () => {
  const { ST } = runtime('http://localhost', { demo: true });
  const node = ST.addNode({ name: 'New demo node', expectedDown: 100, expectedUp: 50 });
  assert.equal(node.interval, 60);
  assert.equal(node.offset, 0);
});
test('settings writes use CSRF and ETag and never echo configured flags or blank secrets', async t => {
  const { ST, control } = await setup(t);
  await ST.load();
  await ST.setSettings({ theme: 'light' });
  const req = control.requests.find(r => r.method === 'PUT');
  assert.equal(req.headers['if-match'], '"fixture-v1"'); assert.equal(req.body.theme, 'light');
  assert.equal('geoKeyConfigured' in req.body, false); assert.equal('geoKey' in req.body, false);
  assert.equal('configured' in req.body.notify.email, false);
});
test('remote public mutation methods call the API instead of modifying seeded data', async t => {
  const { ST, control, data } = await setup(t);
  await ST.load();
  await ST.updateNode('opaque/id', { name: 'Remote' });
  await ST.addNode({ name: 'New', expectedDown: 100, expectedUp: 50 });
  await ST.removeNode('opaque/id');
  await ST.saveRule({ ...data.rule, id: '', name: 'New rule' }); await ST.saveRule({ ...data.rule, id: 'rule/id', name: 'Rule' });
  await ST.deleteRule('rule/id'); await ST.ack('event/id'); await ST.ack('event/id', false);
  await ST.ackAll(); await ST.runTest('opaque/id');
  const mutations = control.requests.filter(r => r.method !== 'GET');
  assert.equal(mutations.length, 10);
  assert.ok(mutations.some(r => r.path === '/api/nodes/opaque%2Fid' && r.method === 'PATCH' &&
    r.headers['content-type'] === 'application/merge-patch+json' && r.headers['if-match'] === '"fixture-v1"'));
  assert.ok(mutations.find(r => r.path === '/api/alerts/acknowledgements').headers['idempotency-key']);
  assert.equal(ST.node('node-contract').name, 'Contract node');
});
test('remote snapshots use server events, IP changes and geolocation without recomputing demo rules', async t => {
  const { ST, data } = await setup(t);
  const list = ST.api.list;
  const ts = data.result.ts;
  const geo = { ip: data.result.ip, version: 4, source: 'fixture', city: 'Fixture city' };
  ST.api.list = async (path, options) => {
    const rows = await list(path, options);
    if (path === '/api/nodes') return [Object.assign({}, data.node, { geo, latest: null, latestGood: null, lastSeen: null })];
    if (path.startsWith('/api/results')) return [Object.assign({}, data.result, { geo }), data.result];
    if (path.startsWith('/api/alerts/events')) return [{ id: 'event', nodeId: data.node.id, ts, resolvedAt: null, acked: false }];
    if (path === '/api/ip/addresses') return rows.map(row => ({ ...row, geo }));
    if (path === '/api/ip/history') return [{ nodeId: data.node.id, ts, from: '203.0.113.8', to: data.result.ip }];
    return rows;
  };
  await ST.load();
  assert.equal(ST.remote.status, 'ready');
  assert.equal(ST.events()[0].ts, Date.parse(ts)); assert.equal(ST.events()[0].resolvedAt, null);
  ST.recomputeEvents(); assert.equal(ST.events().length, 1);
  assert.equal(ST.geo(data.result.ip).city, 'Fixture city');
  assert.equal(ST.ipHistory(data.node.id).changes[0].ts, Date.parse(ts));
  assert.equal(ST.ipHistory('missing').changes.length, 0);
  assert.equal(ST.node(data.node.id).lastSeen, null);
});
test('out-of-order snapshots are ignored even when a transport does not honor abort', async t => {
  const { ST } = await setup(t);
  const list = ST.api.list;
  let release;
  let delayed = false;
  ST.api.list = async (path, options) => {
    if (path === '/api/nodes' && !delayed) {
      delayed = true;
      await new Promise(resolve => { release = resolve; });
      return [{ id: 'stale' }];
    }
    return list(path, options);
  };
  const first = ST.load();
  while (!release) await new Promise(resolve => setTimeout(resolve, 2));
  await ST.load(); release(); await first;
  assert.equal(ST.nodes()[0].id, 'node-contract');
});
test('demo public queries, statistics, formatting and mutations retain their behavior', () => {
  const { ST } = runtime('http://localhost', { demo: true });
  ST.setSettings({ timezone: 'UTC', hour12: false });
  assert.equal(ST.stats.mean([1, 3]), 2); assert.equal(ST.stats.mean([]), null);
  assert.equal(ST.stats.sum([1, 3]), 4); assert.equal(ST.stats.pct([], .5), null);
  assert.equal(ST.stats.pct([1, 3], .5), 2); assert.equal(ST.stats.std([1, 3]), 1);
  assert.equal(ST.stats.std([]), 0); assert.equal(ST.stats.summary([]).mean, null);
  assert.equal(ST.stats.summary([1, null, 3]).median, 2);
  const sample = { nodeId: 'n1', status: 'ok', down: 150, up: 75, ping: 0, jitter: null, loss: 0 };
  for (const metric of ['down', 'up', 'ping', 'jitter', 'loss', 'down_pct', 'up_pct', 'unknown']) ST.stats.metricValue(sample, metric);
  assert.equal(ST.stats.metricValue(sample, 'down_pct'), 50);
  assert.equal(ST.stats.metricValue({ ...sample, status: 'failed' }, 'down'), null);
  assert.equal(ST.stats.metricValue({ ...sample, nodeId: 'missing' }, 'down_pct'), null);
  assert.equal(ST.stats.metricValue({ ...sample, nodeId: 'missing' }, 'up_pct'), null);
  assert.ok(ST.stats.nodeSummary('n1', 0, Date.now()).count > 0);
  assert.equal(ST.stats.nodeSummary('missing', 0, 0).count, 0);
  assert.ok(ST.nodeResults('n1').length); assert.equal(ST.nodeResults('missing').length, 0);
  assert.equal(ST.latest('missing'), null); assert.equal(ST.latestOk('missing'), null);
  assert.ok(ST.latestOk('n1')); assert.ok(ST.rangeFrom('6h') < ST.now());
  assert.ok(ST.results({ nodeIds: ['n1'], from: 0, to: 1 }).length === 0);
  assert.ok(ST.results({ nodeIds: ['n1'], status: 'failed' }).every(r => r.status === 'failed'));
  assert.ok(ST.ipHistory('n1').ips.length); assert.ok(ST.openEventCount() >= 0);
  ST.ack(ST.events()[0].id); ST.ack(ST.events()[0].id, false); ST.ackAll();
  const rule = { name: 'Temporary', metric: 'ping', op: '>', value: 0, forTests: 1, nodes: ['n1'], severity: 'info', enabled: true };
  ST.saveRule(rule); assert.ok(rule.id); ST.saveRule({ ...rule, enabled: false }); ST.deleteRule(rule.id);
  for (const metric of ['failed', 'ip_changed', 'ping']) assert.equal(typeof ST.ruleText({ ...rule, metric }), 'string');
  ST.updateNode('missing', {});
  const node = ST.addNode({ name: 'Extra', expectedDown: 100, expectedUp: 100 });
  assert.equal(ST.node(node.id).name, 'Extra'); assert.ok(ST.nodeColor(node).startsWith('var('));
  assert.ok(ST.nodeTitle(node).startsWith('Extra')); ST.removeNode(node.id); assert.equal(ST.node(node.id), undefined);
  const F = ST.fmt;
  for (const kind of ['day', 'time', 'full', 'dayfull', undefined]) assert.equal(typeof F.date(Date.now(), kind), 'string');
  assert.equal(F.date(null), '—'); assert.equal(F.parts(Date.parse('2026-11-02T00:00:00Z')).wd, 0);
  assert.equal(F.rel(null), '—');
  for (const delta of [-100000, 0, 100000, 4000000, 100000000]) assert.equal(typeof F.rel(Date.now() - delta), 'string');
  for (const duration of [1000, 100000, 10000000, 100000000]) assert.equal(typeof F.dur(duration), 'string');
  assert.equal(F.num(null), '—'); assert.equal(F.num(1.5, 1), '1.5');
  for (const units of ['auto', 'Mbps', 'MBps', 'Gbps']) {
    ST.setSettings({ units }, true);
    for (const value of [null, 8, 9, 999, 1000, 1100]) { F.speedParts(value); F.speed(value); F.axisSpeed(value || 0); }
    assert.ok(F.axisUnit());
  }
  assert.equal(F.ms(null), '—'); assert.equal(F.ms(0), '0.0 ms'); assert.equal(F.pct(null), '—'); F.pct(1, 2);
  assert.equal(F.bytes(null), '—'); assert.equal(F.bytes(0), '0 B'); F.bytes(1); F.bytes(4096);
  assert.equal(F.bytesTotal(null, 10), null); assert.equal(F.bytesTotal(20, 10), 30);
  for (const metric of ['down', 'ping', 'loss', 'unknown']) { F.metric(metric, 1); F.metric(metric, null); }
  const csv = ST.csv([{ ...ST.results()[0], bytesDown: 100, bytesUp: 50, error: 'quoted,"value"\nnext' }]);
  assert.ok(csv.startsWith('time,node,status,download_mbps,upload_mbps,ping_ms,jitter_ms,packet_loss_pct,bytes_down,bytes_up,bytes_total'));
  assert.ok(csv.includes(',100,50,150,'));
  const missingBytes = ST.csv([{ ...ST.results()[0], bytesDown: null, bytesUp: null }]).split('\n')[1].split(',');
  assert.deepEqual(missingBytes.slice(8, 11), ['', '', ''], 'missing counters keep the total empty');
  assert.ok(csv.includes('"quoted,""value""\nnext"'));
  assert.ok(ST.csv([{ ...ST.results()[0], nodeId: 'missing', serverId: -1 }]).includes('missing'));
});
test('demo manual test reports progress and inserts a result without network access', async () => {
  const { ST, context } = runtime('http://localhost', { demo: true });
  const { default: vm } = await import('node:vm');
  vm.runInContext('var testClock = Date.now(); Date.now = function () { return testClock; };', context);
  context.setTimeout = fn => { context.testClock += 1000; fn(); };
  const before = ST.results().length;
  const progress = [];
  const result = await ST.runTest('n1', value => progress.push(value));
  assert.equal(ST.results().length, before + 1); assert.equal(result.manual, true);
  assert.ok(progress.some(value => value.phase === 'Testing download'));
  assert.equal(ST.latest('n1').id, result.id);
});

test('demo rate-limited results preserve failed and numeric alert streaks', async () => {
  const { ST, context } = runtime('http://localhost', { demo: true });
  const { default: vm } = await import('node:vm');
  vm.runInContext('var testClock = Date.now(); Date.now = function () { return testClock; };', context);
  context.setTimeout = fn => { context.testClock += 1000; fn(); };
  ST.util.rng = () => () => 0.99;
  const node = ST.addNode({ name: 'Rate-limit fixture', expectedDown: 100, expectedUp: 50 });
  const failedRule = { id: 'demo-rate-failed', name: 'Failed streak', metric: 'failed', op: '>=', value: 1, forTests: 2, nodes: [node.id], severity: 'warn', enabled: true };
  const pingRule = { id: 'demo-rate-ping', name: 'Ping streak', metric: 'ping', op: '>', value: 50, forTests: 2, nodes: [node.id], severity: 'warn', enabled: true };
  ST.saveRule(failedRule); ST.saveRule(pingRule);
  let nextSample;
  ST.genSample = (selected, ts) => ({ ...nextSample, nodeId: selected.id, ts });
  const run = async sample => { nextSample = sample; return ST.runTest(node.id); };
  const ping = { status: 'ok', failureClass: 'other', serverId: ST.servers[0].id, ip: '203.0.113.73', down: 100, up: 50, ping: 100, jitter: 1, dLat: 5, uLat: 5, loss: 0, bytesDown: 100, bytesUp: 50, duration: 1 };
  const limited = { status: 'failed', failureClass: 'rate_limited', error: 'Too many requests', serverId: ST.servers[0].id, ip: '', down: null, up: null, ping: null, jitter: null, dLat: null, uLat: null, loss: null, bytesDown: 0, bytesUp: 0, duration: 1 };
  const failure = { ...limited, failureClass: 'other', error: 'fixture failure' };
  await run(ping); await run(limited); await run(ping); await run(limited);
  assert.ok(ST.events().some(event => event.ruleId === pingRule.id && !event.resolvedAt), 'rate limit between and after high ping results preserves the numeric alert');
  await run(failure); await run(ping);
  assert.ok(ST.events().some(event => event.ruleId === pingRule.id && !event.resolvedAt), 'a failed measurement does not recover a numeric alert');
  await run(failure); await run(limited); await run(failure); await run(limited);
  assert.ok(ST.events().some(event => event.ruleId === failedRule.id && !event.resolvedAt), 'rate limit between and after failures preserves the failed-test alert');
});

test('remote schedules, rules, stagger, key rotation, agent update, manual tests and notification tests use the API', async t => {
  const { ST, control, data } = await setup(t);
  await ST.load();
  assert.equal(ST.scheduleInfo().rules.maxConcurrent, 1);
  assert.equal(ST.scheduleInfo().nodes[0].nodeId, data.node.id);
  control.requests.length = 0;
  assert.equal(JSON.stringify(ST.engineSchedules(data.node)), JSON.stringify([{ engine: 'ookla', interval: 30, offset: 0, enabled: true }]));
  assert.equal(ST.engineSchedules({ id: 'unconfigured' }).length, 0);
  await ST.saveSchedule(data.node.id, { interval: 60, offset: 5, enabled: true });
  const put = control.requests.find(r => r.method === 'PUT' && r.path === '/api/schedules/' + data.node.id);
  assert.deepEqual(put.body, { interval: 60, offset: 5, enabled: true }); assert.equal(put.headers['if-match'], '"fixture-v1"');
  assert.equal(control.requests.filter(r => r.path === '/api/schedules').length, 1, 'refreshes schedule data after save');
  assert.equal(control.requests.some(r => r.path.startsWith('/api/results')), false, 'does not reload result history for a schedule edit');
  await ST.saveScheduleRules({ maxConcurrent: 3, quietStart: '23:00', quietEnd: '05:00' });
  const rules = control.requests.find(r => r.path === '/api/schedules/rules' && r.method === 'PUT').body;
  assert.equal(rules.maxConcurrent, 3); assert.equal(rules.stagger, true); assert.equal(rules.quiet.quietStart, '23:00');
  assert.match(rules.quiet.timezone, /^[A-Za-z_]+(\/[A-Za-z_+-]+)*$/, 'the quiet-hours zone is always an IANA name');
  await ST.saveScheduleRules({ stagger: false });
  assert.equal(control.requests.filter(r => r.path === '/api/schedules/rules').at(-1).body.stagger, false);
  await ST.staggerSchedules();
  assert.equal(control.requests.find(r => r.path === '/api/schedules/stagger').method, 'POST');
  assert.equal(await ST.updateAgent(data.node.id, '2.0.0').then(c => c.version), '2.0.0');
  assert.deepEqual(control.requests.find(r => r.path.endsWith('/commands')).body, { type: 'update_agent', version: '2.0.0' });
  assert.equal((await ST.rotateEnrollmentKey()).enrollToken, 'st_enroll_fixture_once');
  assert.equal(ST.settings().enrollKeyConfigured, true);
  assert.equal((await ST.testNotification('webhook')).status, 'sent');
  control.deliveryFails = true;
  assert.equal((await ST.testNotification('webhook')).status, 'failed');
});

test('remote server catalogs poll discovery, preserve errors, and pin with the catalog ETag', async t => {
  const { ST, context, data, control } = await setup(t);
  await ST.load();
  assert.deepEqual(JSON.parse(JSON.stringify(ST.engineSchedules(data.node))),
    [{ engine: 'ookla', interval: 30, offset: 0, enabled: true }]);
  await ST.saveEngineSchedules(data.node.id, [{ engine: 'librespeed', interval: 60, offset: 2, enabled: true }]);
  let commandStatus = 'running';
  let commandError = '';
  let holdCommand = false;
  let commandPolls = 0;
  const request = ST.api.request;
  const calls = [];
  ST.api.request = async (path, options = {}) => {
    calls.push({ path, options });
    if (path.includes('/servers?engine=') && options.method === 'POST') return { data: { id: 'command/id' } };
    if (path.includes('/servers?engine=')) return { data: { servers: [], pinnedServerId: 'auto' }, etag: '"catalog-v1"' };
    if (path.endsWith('/commands/command%2Fid')) {
      if (commandStatus === 'running' && !holdCommand && ++commandPolls > 1) commandStatus = 'succeeded';
      return { data: { status: commandStatus, error: commandError } };
    }
    if (path.endsWith('/server-selection')) return { data: { engine: 'librespeed', serverId: calls.filter(call => call.path.endsWith('/server-selection')).length === 1 ? 17 : 'auto' }, etag: calls.filter(call => call.path.endsWith('/server-selection')).length === 1 ? '"catalog-v2"' : '"catalog-v3"' };
    return request(path, options);
  };
  context.setTimeout = fn => { fn(); return 0; };
  const catalog = await ST.serverCatalog('node/contract', 'libre speed');
  assert.equal(catalog.etag, '"catalog-v1"');
  await ST.refreshServerCatalog('node/contract', 'libre speed');
  assert.ok(calls.some(call => call.path === '/api/nodes/node%2Fcontract/commands/command%2Fid'));
  const selected = await ST.selectServer(data.node.id, 'librespeed', 17, '"catalog-v1"');
  assert.equal(selected.serverId, 17);
  assert.equal(selected.etag, '"catalog-v2"');
  const automatic = await ST.selectServer(data.node.id, 'librespeed', 'auto', selected.etag);
  assert.equal(automatic.serverId, 'auto');
  assert.equal(automatic.etag, '"catalog-v3"');
  assert.deepEqual(JSON.parse(JSON.stringify(calls.find(call => call.path.endsWith('/server-selection')).options.body)),
    { engine: 'librespeed', serverId: 17 });
  commandStatus = 'failed';
  await assert.rejects(ST.refreshServerCatalog(data.node.id, 'librespeed'), /could not retrieve its server list/);
  commandError = 'server list unavailable';
  await assert.rejects(ST.refreshServerCatalog(data.node.id, 'librespeed'), /server list unavailable/);
  commandStatus = 'running'; commandError = ''; holdCommand = true; commandPolls = 0;
  const vm = await import('node:vm');
  vm.runInContext('var serverClock = 0; Date.now = function () { serverClock += 200000; return serverClock; };', context);
  await assert.rejects(ST.refreshServerCatalog(data.node.id, 'librespeed'), /discovery is still running/);
});

test('remote engine schedule queries select the node and default legacy entries to Ookla', async t => {
  const { ST, data } = await setup(t);
  await ST.load();
  const rows = ST.engineSchedules(ST.node(data.node.id));
  assert.equal(rows.length, 1);
  assert.equal(rows[0].engine, 'ookla'); assert.equal(rows[0].interval, 30);
  assert.equal(rows[0].offset, 0); assert.equal(rows[0].enabled, true);
  assert.equal(ST.engineSchedules({ id: 'missing' }).length, 0);
});

test('notification catalog and template methods use their REST endpoints', async t => {
  const { ST, control } = await setup(t);
  await ST.load();
  const events = await ST.notificationEvents();
  assert.equal(events[0].key, 'test.success');
  await ST.updateNotificationEvent('test.success', { enabled: false, severity: 'warn', channels: ['webhook'] });
  assert.equal(await ST.notificationNodeEnabled('test.success', 'node/contract'), true);
  assert.equal((await ST.setNotificationNodeEnabled('test.success', 'node/contract', false)).enabled, false);
  await ST.notificationTemplate('test.success', 'telegram');
  await ST.saveNotificationTemplate('test.success', 'telegram', 'Hello {node}');
  const preview = await ST.previewNotificationTemplate('test.success', 'telegram', 'Hello {node}');
  assert.equal(preview.body, 'Hello Viettel Fast 50');
  const reset = await ST.resetNotificationTemplate('test.success', 'telegram');
  assert.equal(reset.body, 'Hello {node}');
  const calls = control.requests.filter(r => r.path.startsWith('/api/notifications/'));
  assert.deepEqual(calls.map(r => `${r.method} ${r.path}`), [
    'GET /api/notifications/events',
    'PATCH /api/notifications/events/test.success',
    'GET /api/notifications/events/test.success/nodes/node%2Fcontract',
    'PUT /api/notifications/events/test.success/nodes/node%2Fcontract',
    'GET /api/notifications/templates/test.success/telegram',
    'PUT /api/notifications/templates/test.success/telegram',
    'POST /api/notifications/templates/test.success/telegram/previews',
    'POST /api/notifications/templates/test.success/telegram/resets',
  ]);
  assert.deepEqual(calls[3].body, { enabled: false });
  assert.deepEqual(calls[5].body, { body: 'Hello {node}' });
  assert.deepEqual(calls[6].body, { body: 'Hello {node}' });
  assert.equal(calls[1].headers['x-csrf-token'], 'fixture-csrf');
});

test('a manual test follows the command to its result and reports a failed command plainly', async t => {
  const { ST, control, data } = await setup(t);
  await ST.load();
  const progress = [];
  const result = await ST.runTest(data.node.id, p => progress.push(p));
  assert.equal(result.id, data.result.id); assert.equal(result.ts, Date.parse(data.result.ts));
  assert.equal(progress.at(-1).pct, 100); assert.equal(progress.at(-1).phase, 'done');
  control.commandFails = true;
  const failed = await ST.runTest(data.node.id);
  assert.deepEqual({ status: failed.status, error: failed.error }, { status: 'failed', error: 'speedtest: network is down' });
});

test('polling waits for pending commands and deliveries and gives up after its time limit', async t => {
  const { ST, control, context, data } = await setup(t);
  await ST.load();
  const real = context.setTimeout; context.setTimeout = fn => real(fn, 1);
  control.pending = 2;
  const seen = [];
  const result = await ST.runTest(data.node.id, p => seen.push(p.phase));
  assert.equal(result.id, data.result.id); assert.ok(seen.includes('Testing download'));
  control.pending = 2;
  assert.equal((await ST.testNotification('webhook')).status, 'sent');
  const vm = await import('node:vm');
  vm.runInContext('var clockNow = 1000000; Date.now = function () { clockNow += 400000; return clockNow; };', context);
  control.pending = 99;
  assert.match((await ST.runTest(data.node.id)).error, /did not answer in time/);
  control.pending = 99;
  assert.match((await ST.testNotification('webhook')).error, /no answer/);
});

test('the idle refresh reloads only when nothing is being typed, no dialog is open and the tab is visible', async t => {
  const { ST, context, control } = await setup(t);
  await ST.load();
  const reloads = () => control.requests.filter(r => r.path === '/api/settings').length;
  const set = state => { context.document = { body: true, visibilityState: 'visible', activeElement: null, querySelector: () => null, ...state }; };
  let before = reloads();
  set({}); await ST.refreshWhenIdle(); assert.equal(reloads(), before + 1, 'idle page reloads');
  before = reloads();
  set({ activeElement: { tagName: 'INPUT' } }); assert.equal(ST.refreshWhenIdle(), null); assert.equal(reloads(), before, 'typing');
  set({ querySelector: () => ({}) }); assert.equal(ST.refreshWhenIdle(), null); assert.equal(reloads(), before, 'dialog open');
  set({ visibilityState: 'hidden' }); assert.equal(ST.refreshWhenIdle(), null); assert.equal(reloads(), before, 'hidden tab');
});

test('installation helpers probe, start, follow progress and fall back to the final state when the stream drops', async t => {
  const { ST, control, context, data } = await setup(t);
  await ST.load();
  const probe = await ST.probeMachine('192.0.2.7', 22);
  assert.equal(probe.fingerprint, 'SHA256:fixtureFingerprint0123456789');
  assert.deepEqual(control.requests.find(r => r.path === '/api/installer/probes').body, { host: '192.0.2.7', port: 22 });
  const job = await ST.startInstallation(data.node.id, 'uninstallations', probe, { host: '192.0.2.7', port: 22, user: 'u', auth: 'password', secret: 's' });
  assert.equal(job.status, 'pending');
  assert.ok(control.requests.some(r => r.path.endsWith('/uninstallations') && r.headers['idempotency-key']));

  const sources = [];
  context.EventSource = class { constructor(url) { this.url = url; this.listeners = {}; sources.push(this); } addEventListener(name, fn) { this.listeners[name] = fn; } close() { this.closed = true; } };
  const seen = [];
  const follow = ST.followInstallation(job.id, event => seen.push(event));
  const source = sources[0];
  assert.equal(source.url, '/api/installations/' + job.id + '/events');
  source.listeners.progress({ data: 'not json' });
  source.listeners.progress({ data: JSON.stringify({ id: '1', status: 'running', message: 'Working', pct: 20 }) });
  source.listeners.progress({ data: JSON.stringify({ id: '2', status: 'succeeded', message: 'Done', pct: 100 }) });
  assert.deepEqual(seen.map(e => e.id), ['1', '2']); assert.equal(source.closed, true);
  source.onerror(); // after the end the error handler stays quiet
  await new Promise(r => setTimeout(r, 20));
  assert.equal(seen.length, 2);
  follow.close();

  // A dropped stream: the final state is read from the job instead.
  const dropped = [];
  ST.followInstallation(job.id, event => dropped.push(event));
  sources[1].onerror();
  await new Promise(r => setTimeout(r, 50));
  assert.equal(dropped[0].status, 'succeeded'); assert.equal(dropped[0].id, 'final'); assert.equal(sources[1].closed, true);
  control.installFails = true;
  const failed = [];
  ST.followInstallation(job.id, event => failed.push(event));
  sources[2].onerror();
  await new Promise(r => setTimeout(r, 50));
  assert.equal(failed[0].status, 'failed'); assert.match(failed[0].message, /Sudo is not allowed/);
  // A job that is still running leaves the stream to reconnect by itself.
  control.status = 500;
  const quiet = [];
  ST.followInstallation(job.id, event => quiet.push(event));
  sources[3].onerror();
  await new Promise(r => setTimeout(r, 50));
  assert.equal(quiet.length, 0);
});

test('quick successive writes to one resource run one after the other, each with a fresh ETag', async t => {
  const { ST, control } = await setup(t);
  await ST.load();
  control.requests.length = 0;
  await Promise.all([ST.setSettings({ range: '24h' }), ST.setSettings({ range: '30d' }), ST.setSettings({ range: '7d' })]);
  const steps = control.requests.filter(r => r.path === '/api/settings').map(r => r.method);
  assert.equal(steps.filter(m => m === 'PUT').length, 3);
  // Every write is preceded by its own read of the ETag and the next read only starts after the previous write.
  assert.equal(steps[0], 'GET');
  for (let i = 0; i < steps.length; i++) if (steps[i] === 'PUT') assert.equal(steps[i - 1], 'GET', 'a write directly follows its own ETag read: ' + steps.join(','));
});

test('a write refused with 412 is retried once with a fresh ETag, and a second refusal is reported', async t => {
  const { ST, control } = await setup(t);
  await ST.load();
  control.requests.length = 0; control.failWrites = 1;
  await ST.setSettings({ range: '30d' });
  assert.equal(control.requests.filter(r => r.path === '/api/settings' && r.method === 'PUT').length, 2, 'one refused write and one retry');
  control.failWrites = 2;
  await assert.rejects(ST.setSettings({ range: '24h' }), error => error.status === 412);
  control.failWrites = 1; control.failWriteStatus = 409;
  await assert.rejects(ST.setSettings({ range: '7d' }), error => error.status === 409, 'only 412 is retried');
});

test('changing the password posts both passwords to the center', async t => {
  const { ST, control } = await setup(t);
  await ST.load();
  await ST.changePassword('current one', 'a much longer new passphrase');
  const call = control.requests.filter(r => r.path === '/api/auth/password' && r.method === 'POST').at(-1);
  assert.deepEqual(call.body, { currentPassword: 'current one', newPassword: 'a much longer new passphrase' });
});
test('ISP plans are requested for a known ISP and never requested without one', async t => {
  const { ST, control, data } = await setup(t);
  await ST.load();
  assert.equal((await ST.listISPPlans('')).length, 0); assert.equal((await ST.listISPPlans(undefined)).length, 0);
  assert.equal(control.requests.some(r => r.path.endsWith('/plans')), false);
  const plans = await ST.listISPPlans(data.isp.id);
  assert.equal(control.requests.at(-1).path, '/api/isps/' + encodeURIComponent(data.isp.id) + '/plans');
  assert.equal(plans.id, data.isp.id);
});
test('a schedule save still resolves when the schedule list cannot refresh, and reports the remote error', async t => {
  const { ST, control, data } = await setup(t);
  await ST.load();
  const seen = [];
  ST.on('remote', () => seen.push(ST.remote));
  control.failPaths.add('/api/schedules');
  const saved = await ST.saveSchedule(data.node.id, { interval: 45, offset: 1, enabled: true });
  assert.equal(saved.config.interval, 45);
  assert.equal(control.requests.some(r => r.method === 'PUT' && r.path === '/api/schedules/' + data.node.id), true);
  assert.equal(ST.remote.status, 'error'); assert.equal(ST.remote.error.status, 500);
  assert.equal(seen.length, 1); assert.equal(seen[0].status, 'error');
});
// A fake command center for the event-template test flow: the test is accepted, then each poll gets the next status.
function templateTestCenter(pollStatuses) {
  const requests = [];
  const fetch = async (url, init = {}) => {
    const method = init.method || 'GET', headers = new Headers(init.headers);
    requests.push({ method, path: url.pathname, idempotencyKey: headers.get('Idempotency-Key'), csrf: headers.get('X-CSRF-Token') });
    if (url.pathname === '/api/auth/me') return Response.json(envelope({ user: { id: 'owner' }, csrfToken: 'fake-csrf', expiresAt: null }));
    if (method === 'POST') return Response.json(envelope({ id: 'delivery-1', eventId: null, channel: 'webhook', status: 'pending', attempts: 0, createdAt: null, lastAttempt: null, error: null }, {}, 202), { status: 202 });
    if (!pollStatuses.length) throw new Error('unexpected extra poll');
    const next = pollStatuses.shift();
    if (next === 'error') return Response.json(problem(500), { status: 500 });
    return Response.json(envelope({ id: 'delivery-1', eventId: null, channel: 'webhook', status: next, attempts: 1, createdAt: null, lastAttempt: null, error: next === 'failed' ? 'the receiver answered 500' : null }));
  };
  return { fetch, requests };
}
test('an event template test is queued, then polled one second apart until the delivery is sent', async () => {
  const center = templateTestCenter(['pending', 'sent']);
  const { ST, context } = runtime('http://localhost', { fetch: center.fetch });
  const waits = [];
  context.setTimeout = (fn, ms) => { waits.push(ms); fn(); return 0; };
  const delivery = await ST.sendEventTemplateTest('test.success', 'telegram');
  assert.equal(delivery.status, 'sent');
  const queued = center.requests.find(r => r.method === 'POST');
  assert.equal(queued.path, '/api/notifications/events/test.success/tests/telegram');
  assert.equal(queued.csrf, 'fake-csrf'); assert.ok(queued.idempotencyKey);
  assert.deepEqual(center.requests.filter(r => r.path.startsWith('/api/notifications/deliveries/')).map(r => r.path),
    ['/api/notifications/deliveries/delivery-1', '/api/notifications/deliveries/delivery-1']);
  assert.deepEqual(waits, [1000]);
});
test('an event template test resolves with a failed delivery and rejects when a poll fails', async () => {
  const failing = templateTestCenter(['failed']);
  const failed = runtime('http://localhost', { fetch: failing.fetch });
  failed.context.setTimeout = fn => { fn(); return 0; };
  const delivery = await failed.ST.sendEventTemplateTest('test.success', 'webhook');
  assert.deepEqual({ status: delivery.status, error: delivery.error }, { status: 'failed', error: 'the receiver answered 500' });
  const broken = templateTestCenter(['error']);
  const { ST, context } = runtime('http://localhost', { fetch: broken.fetch });
  context.setTimeout = fn => { fn(); return 0; };
  await assert.rejects(ST.sendEventTemplateTest('test.success', 'webhook'), error => error.status === 500);
});
test('an event template test that the center never answers fails after 30 seconds without waiting in real time', async () => {
  const center = templateTestCenter(['pending', 'pending']);
  const { ST, context } = runtime('http://localhost', { fetch: center.fetch });
  const vm = await import('node:vm');
  const waits = [];
  context.setTimeout = (fn, ms) => { waits.push(ms); fn(); return 0; };
  vm.runInContext('var templateClock = 0; Date.now = function () { templateClock += 16000; return templateClock; };', context);
  const delivery = await ST.sendEventTemplateTest('test.success', 'webhook');
  assert.deepEqual({ status: delivery.status, error: delivery.error }, { status: 'failed', error: 'no answer from the center in time' });
  assert.equal(center.requests.filter(r => r.path.startsWith('/api/notifications/deliveries/')).length, 2);
  assert.deepEqual(waits, [1000]);
});

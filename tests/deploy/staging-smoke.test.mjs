import test from 'node:test';
import assert from 'node:assert/strict';
import { smoke, SmokeError } from '../../scripts/staging-smoke.mjs';

function server({ resultStatus = 'ok', health = 204, csrf = 'c1', nodes = [{ id: 7, status: 'online' }], loginStatus = 200, runStatus = () => 202 } = {}) {
  let time = 1_000_000;
  const seen = [];
  let signedIn = false;
  const reply = (status, body = {}, cookies = []) => ({ status, headers: { getSetCookie: () => cookies }, json: async () => body });
  const fetchImpl = async (url, init = {}) => {
    const path = new URL(url).pathname + new URL(url).search;
    seen.push({ path, method: init.method ?? 'GET', headers: init.headers });
    if (path === '/healthz') return reply(health);
    if (path === '/readyz') return reply(204);
    if (path === '/api/v1/auth/me') {
      const signed = signedIn;
      return reply(signed ? 200 : 401, { data: csrf ? { csrfToken: signed ? 'c2' : csrf } : {} }, signed ? [] : ['sid=abc; Path=/']);
    }
    if (path === '/api/v1/auth/login') { signedIn = loginStatus === 200; return reply(loginStatus, {}, ['sid=def; Path=/']); }
    if (path.startsWith('/api/v1/nodes?')) return reply(200, { data: nodes });
    if (path.endsWith('/run')) return reply(runStatus(path));
    if (path.startsWith('/api/v1/results')) return reply(200, { data: [{ id: 'r1', ts: new Date(time + 1).toISOString(), status: resultStatus }] });
    return reply(404);
  };
  return { fetchImpl, seen, now: () => time, sleep: async (ms) => { time += ms; } };
}
const base = { baseUrl: 'http://x', email: 'o@x', password: 'secret', timeoutMs: 10_000 };

test('a healthy center with a completing measurement passes and the sign-in carries the CSRF token and the origin', async () => {
  const s = server();
  const out = await smoke({ ...base, ...s });
  assert.deepEqual(out, { node: 7, result: 'r1' });
  const login = s.seen.find((x) => x.path === '/api/v1/auth/login');
  assert.equal(login.headers['x-csrf-token'], 'c1');
  assert.equal(login.headers.origin, 'http://x');
  assert.match(login.headers.cookie, /sid=abc/);
  const run = s.seen.find((x) => x.path.endsWith('/run'));
  assert.equal(run.headers['x-csrf-token'], 'c2', 'the run uses the CSRF token of the signed-in session');
});

test('a busy node is skipped and the first node that accepts the run is measured', async () => {
  const s = server({ nodes: [{ id: 1, status: 'online' }, { id: 2, status: 'online' }], runStatus: (path) => (path.includes('/nodes/1/') ? 409 : 202) });
  const out = await smoke({ ...base, ...s });
  assert.equal(out.node, 2);
  assert.ok(s.seen.some((x) => x.path.includes('nodes=2')), 'the result is read for the node that ran');
});

test('when every node is busy the smoke fails', async () => {
  await assert.rejects(smoke({ ...base, ...server({ runStatus: () => 409 }) }), /every node is busy/);
});

test('a refused run that is not a conflict fails the smoke', async () => {
  await assert.rejects(smoke({ ...base, ...server({ runStatus: () => 500 }) }), /run: HTTP 500/);
});

test('a failing health check fails the smoke', async () => {
  await assert.rejects(smoke({ ...base, ...server({ health: 503 }) }), (e) => e instanceof SmokeError && /health: HTTP 503/.test(e.message));
});

test('a session without a CSRF token fails the smoke', async () => {
  await assert.rejects(smoke({ ...base, ...server({ csrf: '' }) }), /no CSRF token/);
});

test('a refused sign-in fails the smoke and never prints the password', async () => {
  await assert.rejects(smoke({ ...base, ...server({ loginStatus: 401 }) }), (e) => /sign in: HTTP 401/.test(e.message) && !e.message.includes('secret'));
});

test('a center without an online node fails the smoke', async () => {
  await assert.rejects(smoke({ ...base, ...server({ nodes: [] }) }), /none online/);
  await assert.rejects(smoke({ ...base, ...server({ nodes: [{ id: 3, status: 'offline' }] }) }), /none online/);
});

test('an offline node is never asked to run', async () => {
  const s = server({ nodes: [{ id: 1, status: 'offline' }, { id: 2, status: 'online' }] });
  await smoke({ ...base, ...s });
  assert.ok(!s.seen.some((x) => x.path.includes('/nodes/1/run')));
});

test('a measurement that never reports ok ends in a timeout failure, not a late success', async () => {
  await assert.rejects(smoke({ ...base, ...server({ resultStatus: 'failed' }), timeoutMs: 9000 }), /no ok result within 9 s/);
});

test('a signed-in session without a CSRF token fails the smoke', async () => {
  const s = server();
  const original = s.fetchImpl;
  s.fetchImpl = async (url, init) => {
    const response = await original(url, init);
    if (new URL(url).pathname === '/api/v1/auth/me' && /sid=def/.test(init.headers.cookie)) response.json = async () => ({ data: {} });
    return response;
  };
  await assert.rejects(smoke({ ...base, ...s }), /signed-in session: no CSRF token/);
});

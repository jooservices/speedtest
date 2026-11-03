import { test } from 'node:test';
import assert from 'node:assert/strict';
import { runtime } from './runtime.mjs';
import { startServer, envelope } from '../ui-fixtures/server.mjs';

async function setup(t) {
  const server = await startServer(); t.after(server.close);
  return { ...server, ...runtime(server.origin, { apiOnly: true }) };
}
test('JSON, session cookie policy, CSRF, ETag and stable idempotency key', async t => {
  const { ST, control } = await setup(t);
  const response = await ST.api.request('/api/settings');
  assert.equal(response.data.decimals, 1); assert.equal(response.etag, '"fixture-v1"');
  const key = ST.api.key();
  await ST.api.request('/api/nodes', { method: 'POST', body: { name: 'test', expectedDown: 100, expectedUp: 50 }, idempotencyKey: key, ifMatch: '"v1"' });
  const req = control.requests.at(-1);
  assert.equal(req.headers['x-csrf-token'], 'fixture-csrf');
  assert.equal(req.headers['idempotency-key'], key); assert.equal(req.headers['if-match'], '"v1"');
  assert.deepEqual(req.body, { name: 'test', expectedDown: 100, expectedUp: 50 });
  assert.equal(control.requests.at(-2).path, '/api/auth/me');
});
test('errors preserve RFC problem status, fields and rate limit without replaying writes', async t => {
  const { ST, control } = await setup(t);
  await ST.api.session();
  for (const status of [403, 429, 422, 500, 401]) {
    control.status = status;
    await assert.rejects(ST.api.request('/api/settings'), error => error.status === status && error.type === `/problems/test-${status}` &&
      (status !== 429 || error.retryAfter === '2'));
  }
});
test('401 redirects to sign-in and clears cache and CSRF', async t => {
  const { ST, control, context } = await setup(t);
  await ST.api.request('/api/settings', { cacheMs: 5000 });
  control.status = 401;
  await assert.rejects(ST.api.request('/api/results'));
  assert.equal(context.location.hash, '#/signin');
  control.status = 0;
  await ST.api.request('/api/settings', { cacheMs: 5000 });
  assert.equal(control.requests.filter(r => r.path === '/api/settings').length, 2);
});
test('short bounded cache is opt-in, isolated and invalidated by writes', async t => {
  const { ST, control } = await setup(t);
  const first = await ST.api.request('/api/settings', { cacheMs: 5000 });
  first.data.decimals = 99;
  assert.equal((await ST.api.request('/api/settings', { cacheMs: 5000 })).data.decimals, 1);
  assert.equal(control.requests.length, 1);
  await ST.api.request('/api/settings', { method: 'PUT', body: {}, ifMatch: '"fixture-v1"' });
  await ST.api.request('/api/settings', { cacheMs: 5000 });
  assert.equal(control.requests.filter(r => r.path === '/api/settings' && r.method === 'GET').length, 2);
  await ST.api.session(); await ST.api.session();
  assert.equal(control.requests.filter(r => r.path === '/api/auth/me').length, 3);
});
test('cancel requests and reject an already aborted cached read', async t => {
  const { ST, control } = await setup(t);
  await ST.api.request('/api/settings', { cacheMs: 5000 });
  const controller = new AbortController(); controller.abort();
  await assert.rejects(ST.api.request('/api/settings', { signal: controller.signal, cacheMs: 5000 }), { name: 'AbortError' });
  control.delay = 100;
  const second = new AbortController();
  const request = ST.api.request('/api/results', { signal: second.signal }); second.abort();
  await assert.rejects(request, { name: 'AbortError' });
});
test('transport failures, bad JSON, envelopes, no-content and unsafe paths', async () => {
  for (const path of ['https://evil.example/api/settings', '//evil.example/api/x', '/other', '/api/\\evil', '/api/x#fragment']) {
    await assert.rejects(runtime('http://localhost', { apiOnly: true }).ST.api.request(path), /Invalid API path/);
  }
  for (const [fetcher, message] of [
    [() => Promise.reject(new Error('private transport details')), /Cannot reach/],
    [() => new Response('broken', { status: 200 }), /invalid JSON/],
    [() => Response.json({ data: [] }), /invalid response/]
  ]) {
    await assert.rejects(runtime('http://localhost', { apiOnly: true, fetch: fetcher }).ST.api.request('/api/test'), message);
  }
  const { ST } = runtime('http://localhost', { apiOnly: true, fetch: () => new Response(null, { status: 204 }) });
  assert.equal((await ST.api.request('/api/test')).data, null);
});
test('collections follow all pages with cancellation, reject invalid or unbounded pagination', async () => {
  const calls = [];
  const { ST } = runtime('http://localhost', { apiOnly: true, fetch: url => {
    const page = Number(url.searchParams.get('page')); calls.push(page);
    return Response.json(envelope([page], { pagination: { current_page: page, last_page: 2 } }));
  } });
  assert.deepEqual(Array.from(await ST.api.list('/api/results?range=7d')), [1, 2]);
  assert.deepEqual(calls, [1, 2]);
  for (const pagination of [null, { current_page: 9, last_page: 1 }, { current_page: 1, last_page: 1001 }]) {
    const { ST } = runtime('http://localhost', { apiOnly: true, fetch: () => Response.json(envelope([], { pagination })) });
    await assert.rejects(ST.api.list('/api/results'), /pagination|Too much/);
  }
});
test('cache expiry, eviction and clearing during an in-flight GET cannot reuse stale data', async () => {
  let calls = 0, release;
  const { ST } = runtime('http://localhost', { apiOnly: true, fetch: async url => {
    calls++;
    if (url.pathname === '/api/delayed') await new Promise(resolve => { release = resolve; });
    return Response.json(envelope({ calls }));
  } });
  await ST.api.request('/api/expiring', { cacheMs: 1 });
  await new Promise(resolve => setTimeout(resolve, 5));
  await ST.api.request('/api/expiring', { cacheMs: 1 }); assert.equal(calls, 2);
  for (let i = 0; i < 101; i++) await ST.api.request('/api/item' + i, { cacheMs: 99999 });
  const before = calls;
  await ST.api.request('/api/item0', { cacheMs: 99999 }); assert.equal(calls, before + 1);
  const delayed = ST.api.request('/api/delayed', { cacheMs: 5000 });
  ST.api.clear(); release(); await delayed;
  const pending = ST.api.request('/api/delayed', { cacheMs: 5000 }); release(); await pending;
  assert.equal(calls, before + 3);
});
test('no-content writes clear cache, preserve fetch security options and support custom media types', async () => {
  const calls = [];
  const { ST } = runtime('http://localhost', { apiOnly: true, fetch: (url, options) => {
    calls.push(options);
    if (url.pathname === '/api/auth/me') return Response.json(envelope({ csrfToken: 'test' }));
    if (options.method === 'DELETE') return new Response(null, { status: 204 });
    return Response.json(envelope({}));
  } });
  await ST.api.request('/api/item', { cacheMs: 5000 });
  await ST.api.request('/api/item', { method: 'DELETE' });
  await ST.api.request('/api/item', { cacheMs: 5000 });
  assert.equal(calls.length, 4);
  await ST.api.request('/api/item', { method: 'POST' });
  assert.equal(calls.length, 6);
  assert.ok(calls.every(options => options.credentials === 'same-origin' && options.cache === 'no-store'));
});
test('problem bodies may be missing details; 403 refreshes CSRF before the next write', async t => {
  for (const [body, message] of [[{ title: 'Forbidden' }, 'Forbidden'], [null, 'Request failed. Please retry.']]) {
    const { ST } = runtime('http://localhost', { apiOnly: true, fetch: () => Response.json(body, { status: 403 }) });
    await assert.rejects(ST.api.request('/api/item'), error => error.message === message);
  }
  const { ST, control } = await setup(t);
  await ST.api.session(); control.status = 403;
  await assert.rejects(ST.api.request('/api/settings', { method: 'PUT', body: {} }));
  control.status = 0;
  await ST.api.request('/api/settings', { method: 'PUT', body: {}, ifMatch: '"fixture-v1"' });
  assert.equal(control.requests.filter(r => r.path === '/api/auth/me').length, 2);
});

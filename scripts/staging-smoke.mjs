// Proof that a deployed staging center runs the project: it answers, the owner can sign in and one measurement completes.
// Usage: SMOKE_URL=http://127.0.0.1:18090 SMOKE_EMAIL=... SMOKE_PASSWORD=... node scripts/staging-smoke.mjs
// Exit 0 only when every check passed. The password is read from the environment and is never printed.
import { pathToFileURL } from 'node:url';

const API = '/api/v1';
const POLL_MS = 3000;

export class SmokeError extends Error {}

function jarOf(response, jar) {
  for (const line of response.headers.getSetCookie?.() ?? []) {
    const [name, value] = line.split(';')[0].split('=');
    jar.set(name, value);
  }
}

const cookieHeader = (jar) => [...jar].map(([k, v]) => `${k}=${v}`).join('; ');

async function expect(response, ok, what) {
  if (!ok.includes(response.status)) throw new SmokeError(`${what}: HTTP ${response.status}`);
  return response;
}

export async function smoke({ baseUrl, email, password, timeoutMs, fetchImpl = fetch, now = Date.now, sleep = (ms) => new Promise((r) => setTimeout(r, ms)) }) {
  const jar = new Map();
  const call = async (path, init = {}) => {
    const response = await fetchImpl(baseUrl + path, { ...init, headers: { cookie: cookieHeader(jar), ...(init.headers ?? {}) } });
    jarOf(response, jar);
    return response;
  };
  await expect(await call('/healthz'), [200, 204], 'health');
  await expect(await call('/readyz'), [200, 204], 'ready');
  const me = await expect(await call(`${API}/auth/me`), [200, 401], 'session');
  const csrf = (await me.json()).data?.csrfToken;
  if (!csrf) throw new SmokeError('session: no CSRF token');
  const loginHeaders = { 'content-type': 'application/json', 'x-csrf-token': csrf, origin: baseUrl };
  await expect(await call(`${API}/auth/login`, { method: 'POST', headers: loginHeaders, body: JSON.stringify({ email, password }) }), [200], 'sign in');
  // Signing in rotates the session and the CSRF token, so the token of the signed-in session is read again.
  const signedIn = await expect(await call(`${API}/auth/me`), [200], 'signed-in session');
  const csrfAfter = (await signedIn.json()).data?.csrfToken;
  if (!csrfAfter) throw new SmokeError('signed-in session: no CSRF token');
  const headers = { 'content-type': 'application/json', 'x-csrf-token': csrfAfter, origin: baseUrl };
  const nodes = await (await expect(await call(`${API}/nodes?page=1&size=100`), [200], 'nodes')).json();
  const online = (nodes.data ?? []).filter((n) => n.status === 'online');
  if (!online.length) throw new SmokeError('nodes: none online');
  // A node that is busy answers 409 and its measurement would not be ours, so the first node that accepts the run is used.
  const started = now();
  let node = null;
  for (const candidate of online) {
    const run = await call(`${API}/nodes/${candidate.id}/run`, { method: 'POST', headers: { ...headers, 'idempotency-key': `smoke-${started}-${candidate.id}` } });
    if (run.status === 202) { node = candidate; break; }
    if (run.status !== 409) throw new SmokeError(`run: HTTP ${run.status}`);
  }
  if (!node) throw new SmokeError('run: every node is busy');
  while (now() - started < timeoutMs) {
    const list = await (await expect(await call(`${API}/results?page=1&size=1&sort=ts&dir=desc&nodes=${node.id}`), [200], 'results')).json();
    const latest = list.data?.[0];
    if (latest && Date.parse(latest.ts) >= started && latest.status === 'ok') return { node: node.id, result: latest.id };
    await sleep(POLL_MS);
  }
  throw new SmokeError(`measurement: no ok result within ${Math.round(timeoutMs / 1000)} s`);
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) {
  const { SMOKE_URL, SMOKE_EMAIL, SMOKE_PASSWORD, SMOKE_TIMEOUT_S = '300' } = process.env;
  if (!SMOKE_URL || !SMOKE_EMAIL || !SMOKE_PASSWORD) {
    console.error('smoke: SMOKE_URL, SMOKE_EMAIL and SMOKE_PASSWORD are required');
    process.exit(2);
  }
  smoke({ baseUrl: SMOKE_URL, email: SMOKE_EMAIL, password: SMOKE_PASSWORD, timeoutMs: Number(SMOKE_TIMEOUT_S) * 1000 })
    .then((out) => console.log(`smoke: ok (node ${out.node}, result ${out.result})`))
    .catch((error) => { console.error(`smoke: FAILED: ${error.message}`); process.exit(1); });
}

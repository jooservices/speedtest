// End-to-end harness: a real command center on a throw-away worktree database, real agents driven by a fake speed test.
import { createHash, randomUUID } from 'node:crypto';
import { spawn, spawnSync } from 'node:child_process';
import { existsSync, mkdirSync, readFileSync, writeFileSync, rmSync } from 'node:fs';
import { createServer } from 'node:net';
import { resolve, join } from 'node:path';

export const root = resolve(new URL('../..', import.meta.url).pathname);
const out = join(root, '.tmp', 'e2e');
const processes = [];

export function parseEnv(file) {
  const values = {};
  for (const line of readFileSync(file, 'utf8').split('\n')) {
    const match = /^\s*(\w+)\s*=(.*)$/.exec(line);
    if (match && !line.trim().startsWith('#')) values[match[1]] = match[2].replace(/^["']|["']$/g, '');
  }
  return values;
}

function run(command, args, options = {}) {
  const result = spawnSync(command, args, { cwd: root, encoding: 'utf8', ...options });
  if (result.status !== 0) throw new Error(`${command} ${args.join(' ')} failed:\n${result.stdout}\n${result.stderr}`.replaceAll(/password=\S+/gi, 'password=***'));
  return result.stdout;
}

function freePort(requested) {
  return new Promise((resolvePort, reject) => {
    const server = createServer();
    server.listen(requested || 0, '127.0.0.1', () => { const { port } = server.address(); server.close(() => resolvePort(port)); });
    server.on('error', reject);
  });
}

export async function e2ePort(configured = process.env.E2E_HTTP_PORT) {
  if (!configured) return freePort();
  const port = Number(configured);
  if (!Number.isInteger(port) || port < 1024 || port > 65535) {
    throw new Error('E2E_HTTP_PORT must be an integer between 1024 and 65535');
  }
  return port;
}

// The harness drops everything in the database it is given, so it runs only when APP_ENV says local or test.
function database(env) {
  const config = { ...env, ...process.env };
  const appEnv = (config.APP_ENV || 'local').trim().toLowerCase();
  if (appEnv !== 'local' && appEnv !== 'test') throw new Error(`refusing to reset the database when APP_ENV is "${appEnv}"`);
  const name = config.SPEEDTEST_DB_NAME || '';
  const pg = { PGHOST: config.SPEEDTEST_DB_HOST, PGPORT: config.SPEEDTEST_DB_PORT, PGUSER: config.SPEEDTEST_DB_USER, PGPASSWORD: config.SPEEDTEST_DB_PASSWORD, PGDATABASE: name };
  return { name, pg: { ...process.env, ...pg } };
}

export function sql(db, text) { return run('psql', ['-v', 'ON_ERROR_STOP=1', '-q', '-c', text], { env: db.pg }); }
export function sqlFile(db, file) { return run('psql', ['-v', 'ON_ERROR_STOP=1', '-q', '-f', file], { env: db.pg }); }
export function query(db, text) { return run('psql', ['-Atc', text], { env: db.pg }).trim(); }

export async function startStack({ seed = true } = {}) {
  const env = parseEnv(join(root, '.env'));
  const db = database(env);
  mkdirSync(join(out, 'bin'), { recursive: true });
  run('go', ['build', '-o', join(out, 'bin', 'center'), './cmd/center']);
  run('go', ['build', '-o', join(out, 'bin', 'agent'), './cmd/agent']);
  run('go', ['build', '-o', join(out, 'bin', 'fake-speedtest'), './internal/agent/testdata/fake-speedtest']);
  sql(db, 'DROP SCHEMA public CASCADE; CREATE SCHEMA public;');
  const port = await e2ePort();
  const releaseDir = join(out, 'release');
  mkdirSync(releaseDir, { recursive: true });
  const artifact = 'speedtest-agent-linux-amd64';
  const binary = 'e2e-release-artifact';
  writeFileSync(join(releaseDir, artifact), binary);
  writeFileSync(join(releaseDir, 'release.json'), JSON.stringify({ version: '2.0.0', commit: 'e2e', builtAt: new Date().toISOString(), files: { [artifact]: createHash('sha256').update(binary).digest('hex') } }));
  const centerEnv = { ...env, ...process.env, APP_ENV: 'local', APP_KEY: 'MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=', SPEEDTEST_HTTP_ADDR: `127.0.0.1:${port}`, // gitleaks:allow (fixed test-only key)
    SPEEDTEST_PUBLIC_URL: `http://127.0.0.1:${port}`, SPEEDTEST_RELEASE_DIR: releaseDir, SPEEDTEST_ADMIN_EMAIL: '', SPEEDTEST_LOGIN_MAX_ATTEMPTS: '100', SPEEDTEST_GEO_DBIP_AUTO_UPDATE: 'false' };
  run('go', ['run', './internal/center/db/cmd/migrate', 'up'], { env: centerEnv });
  run(join(out, 'bin', 'center'), ['seed-admin'], { env: centerEnv });
  if (seed) sqlFile(db, join(root, 'tests', 'e2e', 'seed.sql'));
  const log = join(out, 'center.log');
  writeFileSync(log, '');
  const center = spawn(join(out, 'bin', 'center'), ['-ui-dir', join(root, 'ui')], { cwd: root, env: centerEnv, stdio: ['ignore', 'ignore', 'ignore'], detached: false });
  processes.push(center);
  const origin = `http://127.0.0.1:${port}`;
  const browserOrigin = process.env.E2E_BROWSER_ORIGIN || origin;
  for (let i = 0; i < 100; i++) {
    try { if ((await fetch(origin + '/readyz')).ok) break; } catch { /* not up yet */ }
    await new Promise(r => setTimeout(r, 100));
    if (i === 99) throw new Error('the center did not become ready');
  }
  return { origin, browserOrigin, db, env: centerEnv, pid: center.pid };
}

// Starts a real agent for an enrolment token and waits until the center has seen it.
export async function startAgent(stack, { name, token, scenario = 'success', backoffInitial, backoffMax }) {
  const dir = join(out, 'agents', name.replaceAll(/[^A-Za-z0-9]/g, '_'));
  rmSync(dir, { recursive: true, force: true });
  mkdirSync(dir, { recursive: true });
  const env = { ...process.env, SPEEDTEST_AGENT_CONFIG: join(dir, 'config.yaml'), SPEEDTEST_AGENT_CREDENTIAL: join(dir, 'credential'), SPEEDTEST_AGENT_CENTER: stack.origin,
    SPEEDTEST_OOKLA_BINARY: join(out, 'bin', 'fake-speedtest'), SPEEDTEST_OOKLA_ARGS: JSON.stringify(['--scenario', scenario]),
    // Capability discovery only needs LookPath; reuse the Ookla fake so LibreSpeed is reported as installed.
    SPEEDTEST_LIBRESPEED_BINARY: join(out, 'bin', 'fake-speedtest'),
    SPEEDTEST_LIBRESPEED_CONCURRENT: '4', SPEEDTEST_LIBRESPEED_TIMEOUT: '60s',
    SPEEDTEST_SPOOL_DIR: join(dir, 'spool'),
    SPEEDTEST_AGENT_UPDATE_DIR: join(dir, 'update'), SPEEDTEST_AGENT_INBOX_DIR: join(dir, 'inbox') };
  if (backoffInitial) env.SPEEDTEST_RATE_LIMIT_BACKOFF_INITIAL = backoffInitial;
  if (backoffMax) env.SPEEDTEST_RATE_LIMIT_BACKOFF_MAX = backoffMax;
  run(join(out, 'bin', 'agent'), ['enroll', '--center', stack.origin, '--token', token, '--name', name], { env });
  const spawnAgent = nextEnv => {
    const child = spawn(join(out, 'bin', 'agent'), ['run'], { cwd: dir, env: nextEnv, stdio: 'ignore' });
    processes.push(child);
    return child;
  };
  let childProcess = spawnAgent(env);
  const stopProcess = async () => {
    if (childProcess.exitCode == null && childProcess.signalCode == null) {
      await new Promise(resolveExit => { childProcess.once('exit', resolveExit); childProcess.kill('SIGTERM'); });
    }
  };
  return {
    async restart(options = {}) {
      await stopProcess();
      const nextEnv = { ...env };
      if (options.scenario) nextEnv.SPEEDTEST_OOKLA_ARGS = JSON.stringify(['--scenario', options.scenario]);
      childProcess = spawnAgent(nextEnv);
    },
    stop: stopProcess,
  };
}

export async function fetchAgentConfig(stack, name) {
  const safeName = name.replaceAll(/[^A-Za-z0-9]/g, '_');
  const credential = readFileSync(join(out, 'agents', safeName, 'credential'), 'utf8').trim();
  const response = await fetch(stack.origin + '/api/agent/config', { headers: { Authorization: `Bearer ${credential}` } });
  if (!response.ok) throw new Error(`agent config returned HTTP ${response.status}`);
  const body = await response.json();
  return body.data || body;
}

function agentCredential(name) {
  const dir = join(out, 'agents', name.replaceAll(/[^A-Za-z0-9]/g, '_'));
  return readFileSync(join(dir, 'credential'), 'utf8').trim();
}

async function submitAgentResult(stack, name, result) {
  const response = await fetch(stack.origin + '/api/agent/results', {
    method: 'POST',
    headers: { Authorization: `Bearer ${agentCredential(name)}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ results: [result] }),
  });
  const body = await response.json();
  if (!response.ok || body.data?.[0]?.outcome !== 'accepted') {
    throw new Error(`scheduled test result was rejected (${response.status})`);
  }
}

// The first run after enrolment may be a manual run requested by the wizard.
export async function submitScheduledResult(stack, { name }) {
  await submitAgentResult(stack, name, {
      id: randomUUID(), ts: new Date().toISOString(), status: 'ok',
      down: 123.4, up: 56.7, ping: 3, jitter: 4.2, dLat: null, uLat: null, loss: 0.3,
      serverId: 1, ip: '203.0.113.10', threads: 4, bytesDown: 1024, bytesUp: 512,
      duration: 2.3, error: null, manual: false, raw: {}, warnings: [],
      server: { id: 1, name: 'E2E server', city: 'Hanoi', sponsor: 'E2E ISP', host: 'e2e.example' },
  });
}

export async function submitFailedResult(stack, { name }) {
  await submitAgentResult(stack, name, {
    id: randomUUID(), ts: new Date().toISOString(), status: 'failed',
    down: null, up: null, ping: null, jitter: null, dLat: null, uLat: null, loss: null,
    serverId: null, ip: null, threads: 1, bytesDown: 0, bytesUp: 0,
    duration: 2.3, error: 'E2E simulated failure', manual: false, raw: {}, warnings: [],
  });
}

export async function sendAgentHeartbeat(stack, { name }) {
  const response = await fetch(stack.origin + '/api/agent/heartbeat', {
    method: 'POST',
    headers: { Authorization: `Bearer ${agentCredential(name)}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ agent: 'speedtest-agent/2.0.0 (linux/amd64)', uptime: 60, lastTest: null, engines: ['ookla'] }),
  });
  if (!response.ok) throw new Error(`agent heartbeat was rejected (${response.status})`);
}

export function stopAll() {
  for (const p of processes.splice(0)) { try { p.kill('SIGTERM'); } catch { /* already gone */ } }
}
process.on('exit', stopAll);

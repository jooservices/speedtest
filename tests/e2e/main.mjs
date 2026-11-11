// Browser end-to-end scenarios against a real command center, real agents and a seeded database.
// Run with `make e2e` (needs a worktree .env, psql, Go and a Chromium; see docs/e2e.md).
import { chromium } from 'playwright';
import { mkdirSync } from 'node:fs';
import { join } from 'node:path';
import http from 'node:http';
import { startStack, startAgent, fetchAgentConfig, stopAll, query, root } from './lib.mjs';
import { coreSteps } from './steps/core.mjs';
import { enginesSuite } from './suites/engines.mjs';
import { authSuite } from './suites/auth.mjs';
import { dashboardSuite } from './suites/dashboard.mjs';
import { nodesSuite } from './suites/nodes.mjs';
import { resultsSuite } from './suites/results.mjs';
import { statisticsSuite } from './suites/statistics.mjs';
import { networkSuite } from './suites/network.mjs';
import { schedulesSuite } from './suites/schedules.mjs';
import { alertsSuite } from './suites/alerts.mjs';
import { settingsSuite } from './suites/settings.mjs';
import { qualitySuite } from './suites/quality.mjs';
import { scenarios as buildScenarios } from './scenarios/index.mjs';
import { recordContract } from './contract.mjs';
import { trackCoverage } from '../lib/v8cov.mjs';

const shots = join(root, '.tmp', 'e2e', 'shots');
mkdirSync(shots, { recursive: true });
const stack = await startStack();
const browser = process.env.PW_TEST_CONNECT_WS_ENDPOINT ? await chromium.connect(process.env.PW_TEST_CONNECT_WS_ENDPOINT)
  : await chromium.launch({ headless: true, ...(process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE } : {}) });
const context = await browser.newContext({ viewport: { width: 1440, height: 900 }, acceptDownloads: true });
const page = await context.newPage();
const coverage = await trackCoverage(page);
const problems = [];
// Map tiles come from the internet in production; the suite answers them locally with a blank tile.
const BLANK_TILE = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==', 'base64');
await context.route('https://tile.openstreetmap.org/**', route => route.fulfill({ status: 200, contentType: 'image/png', body: BLANK_TILE }));
const contractRecorder = recordContract(page, stack.origin);
page.on('console', m => { if (m.type() === 'error' || m.type() === 'warning') problems.push(`${m.type()}: ${m.text()}`); });
page.on('pageerror', e => problems.push(`pageerror: ${e.message}`));

// A local receiver for webhook test messages.
const receiver = { bodies: [], url: '' };
const receiverServer = http.createServer((req, res) => { const chunks = []; req.on('data', c => chunks.push(c)); req.on('end', () => { try { receiver.bodies.push(JSON.parse(Buffer.concat(chunks).toString())); } catch { receiver.bodies.push({ raw: true }); } res.writeHead(200); res.end('ok'); }); });
await new Promise(resolve => receiverServer.listen(0, '127.0.0.1', resolve));
receiver.url = `http://127.0.0.1:${receiverServer.address().port}/hook`;
const ctx = { page, stack, startAgent, fetchAgentConfig, query, problems, receiver };
const T = coreSteps(ctx);
const S = { engines: enginesSuite(T, ctx), auth: authSuite(T, ctx), dashboard: dashboardSuite(T, ctx), nodes: nodesSuite(T, ctx), results: resultsSuite(T, ctx), statistics: statisticsSuite(T, ctx),
  network: networkSuite(T, ctx), schedules: schedulesSuite(T, ctx), alerts: alertsSuite(T, ctx), settings: settingsSuite(T, ctx), quality: qualitySuite(T, ctx) };
const scenarios = buildScenarios({ T, S });
if (process.env.E2E_TRACE) page.on('response', r => {  if (r.url().includes('/api/') && r.request().method() !== 'GET') console.log('  ', r.request().method(), r.url().replace(stack.origin, ''), r.status(), process.env.E2E_BODY ? (r.request().postData() || '') : ''); });
let failed = 0;
const only = process.env.E2E_ONLY;
// E2E_ONLY filters by scenario id, area or words of the title (comma separated); sign-in always runs first.
const wanted = only ? only.split(',').map(x => x.trim().toLowerCase()) : null;
const results = [];
for (const sc of scenarios) {
  if (wanted && sc.id !== 'SC-AUTH-01' && !wanted.some(w => sc.id.toLowerCase() === w || sc.area === w || sc.title.toLowerCase().includes(w))) continue;
  problems.length = 0;
  // A failed scenario can leave a dialog open; start every scenario from a clean page.
  await page.evaluate(() => document.querySelectorAll('.overlay, .drawer-wrap').forEach(e => e.remove())).catch(() => {});
  const started = Date.now();
  try {
    await sc.run();
    if (problems.length) throw new Error('console problems: ' + problems.join(' | '));
    console.log(`PASS ${sc.id} ${sc.title} (${((Date.now() - started) / 1000).toFixed(1)}s)`);
    results.push({ ...sc, ok: true });
  } catch (error) {
    failed++;
    await page.screenshot({ path: join(shots, `FAIL-${sc.id}.png`) }).catch(() => {});
    console.log(`FAIL ${sc.id} ${sc.title}: ${error.message}`);
    results.push({ ...sc, ok: false });
  }
}
console.log(`scenarios: ${results.filter(r => r.ok).length}/${results.length} passed`);
await coverage.finish('e2e');
const contractResult = contractRecorder.check();
console.log(`contract: ${contractResult.responses} responses checked against docs/openapi.yaml, ${contractResult.violations.length} violations`);
for (const v of contractResult.violations) console.log('  CONTRACT ' + v);
if (contractResult.violations.length && !only) failed++;
await browser.close();
receiverServer.close();
stopAll();
void query;
process.exit(failed ? 1 : 0);
